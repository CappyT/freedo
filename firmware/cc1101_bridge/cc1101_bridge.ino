// CC1101 <-> USB serial bridge for ESP32 DevKit (WROOM-32, 30-pin), firmware v2.
//
// Wiring (CC1101 module -> ESP32 devkit):
//   Vcc  -> 3V3      GND -> GND
//   SCLK -> D18(GPIO18)   MOSI -> D23(GPIO23)   MISO -> D21(GPIO21)
//   CSN  -> D5 (GPIO5)    GDO0 -> D25(GPIO25)   GDO2 -> D26(GPIO26)
//
// The serial protocol and every status code stay identical to v1, except that
// the RECV-shaped blobs of RECV, of AUTOACK and of the CSLQUEUE inbound record
// carry FREQEST as one trailing byte after the LQI/CRC byte. See README.md in
// this folder for the wire format and the v1 differences.
//
// Structure:
//   serialTask (core 0) parses requests and writes every response byte.
//   radioTask  (core 1) owns SPI and all CC1101 state. SPI is never touched
//              anywhere else.
//   qCmd moves requests to the radio task, qOut moves response bytes back.
//   The GDO0 interrupt timestamps the end of a frame, the GDO2 interrupt drains
//   the RX FIFO of a frame longer than the 64-byte FIFO.
//   One CSL state machine (IDLE/RX/WAKE/ACK/FOLLOW) runs every transmit job:
//   the per-neighbour queue, the synchronous CSLTX and the Router Advertisement
//   burst all schedule a job on it, so no path can strobe the radio on top of a
//   transaction that is in flight.
//   While the host keeps a pair window open (CMD_PAIRWIN), the idle drain and
//   CMD_AUTOACK answer an Enhanced Beacon Request with an Enhanced Beacon. The
//   CSL state machine never does.

#include <Arduino.h>
#include <SPI.h>
#include <atomic>
#include <string.h>

#include "esp_task_wdt.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/task.h"
#include "mbedtls/aes.h"

// ---------------------------------------------------------------- board wiring

static const int PIN_SCK = 18;
static const int PIN_MISO = 21;  // moved off GPIO19 (suspected dead input)
static const int PIN_MOSI = 23;
static const int PIN_CS = 5;
static const int PIN_GDO0 = 25;
static const int PIN_GDO2 = 26;

// --------------------------------------------------------------- serial layout

// Firmware before v2.10 used 921600. A CH340 adapter on a shared USB 2.0 full-speed bus lost
// bytes of replies of 80 to 120 bytes at that rate. The host driver still opens an old bridge
// at 921600. See README.md.
static const uint32_t SERIAL_BAUD = 230400;
static const size_t SERIAL_RX_BUF = 2048;
static const size_t SERIAL_TX_BUF = 2048;

// PING banner. The level is the serial protocol level. A release build adds the release
// version with -DFIRMWARE_BUILD=<version> (unquoted), a local build reads "dev".
#ifndef FIRMWARE_BUILD
#define FIRMWARE_BUILD dev
#endif
#define BANNER_STR_(x) #x
#define BANNER_STR(x) BANNER_STR_(x)
static const char BANNER[] = "CC1101BRIDGE v2.10 (" BANNER_STR(FIRMWARE_BUILD) ")";

static const uint8_t REQ_MAGIC = 0xA5;
static const uint8_t RSP_MAGIC = 0x5A;

enum : uint8_t {
  CMD_PING = 0x01,
  CMD_XFER = 0x02,
  CMD_GDO = 0x03,
  CMD_RESET = 0x04,
  CMD_STROBE = 0x05,
  CMD_RECV = 0x06,
  CMD_STREAM = 0x07,
  CMD_TXTRAIN = 0x08,
  CMD_RESPOND = 0x09,
  CMD_AUTOACK = 0x0A,
  CMD_RABURST = 0x0B,
  CMD_CSLTX = 0x0C,
  CMD_CSLQUEUE = 0x0D,
  CMD_PAIRWIN = 0x0E
};

// Status bytes of the response header. The host maps every one of them.
enum : uint8_t {
  ST_OK = 0x00,
  ST_NOT_READY = 0x10,
  ST_NO_FRAME = 0x11,
  ST_ACK_SKIPPED = 0x12,
  ST_ACK_FAILED = 0x13,
  ST_INPUT = 0x14,
  ST_TXFAIL = 0x15,
  ST_NOACK = 0x16,
  ST_BADACK = 0x17,
  ST_NOANCHOR = 0x18,
  ST_QUEUE_FULL = 0x19,
  ST_NEIGHBOR_FULL = 0x1A,
  ST_BAD_CMD = 0xE0,
  ST_BAD_LEN = 0xE1,
  ST_BAD_PAYLOAD = 0xE2,
  ST_BAD_ARGS = 0xE3
};

// Streaming markers (CMD_STREAM, CMD_RESPOND). One frame is
//   5A F1 <n:1> <n bytes> <freqest:1> <xor8>
// with xor8 over the n bytes and freqest. End of window: 5A F2 00. Heartbeat:
// 5A F3 00, so the host tells a live-but-quiet link from a dead one and never
// times out on a silent CSL gap. freqest is the FREQEST register, the residual
// carrier offset of this packet, which the host folds back into FSCTRL0.
static const uint8_t STREAM_FRAME = 0xF1;
static const uint8_t STREAM_END = 0xF2;
static const uint8_t STREAM_ALIVE = 0xF3;
static const uint32_t STREAM_HEARTBEAT_MS = 500;
static const uint32_t STREAM_DEFAULT_MS = 10000;

static const size_t MAX_PAYLOAD = 1024;
static const uint32_t SERIAL_INTERBYTE_MS = 1000;
static const uint32_t SERIAL_REQUEST_MS = 3000;  // total budget for one request

// ------------------------------------------------------------- CC1101 register

static const uint8_t CC_IOCFG2 = 0x00;
static const uint8_t CC_IOCFG0 = 0x02;
static const uint8_t CC_FREQEST = 0x32;
static const uint8_t CC_MARCSTATE = 0x35;
static const uint8_t CC_RXBYTES = 0x3B;
static const uint8_t CC_TXBYTES = 0x3A;
static const uint8_t CC_RXFIFO = 0x3F;
static const uint8_t CC_TXFIFO = 0x3F;
static const uint8_t CC_SRES = 0x30;
static const uint8_t CC_SRX = 0x34;
static const uint8_t CC_STX = 0x35;
static const uint8_t CC_SIDLE = 0x36;
static const uint8_t CC_SFRX = 0x3A;
static const uint8_t CC_SFTX = 0x3B;
static const uint8_t READ_SINGLE = 0x80;
static const uint8_t READ_BURST = 0xC0;
static const uint8_t WRITE_BURST = 0x40;

// Datasheet table 41: 0x2F drives the GDO pin low. After SRES, IOCFG0 is 0x3F,
// a CLK_XOSC/192 clock (135 kHz) that fires the GDO0 CHANGE interrupt 270k
// times a second. The pins stay low until the host loads its register set.
static const uint8_t GDO_CFG_LOW = 0x2F;
static const uint8_t GDO_QUIET_TRIES = 4;

static const uint8_t MARC_STATE_MASK = 0x1F;
static const uint8_t MARC_TX = 0x13;
static const uint8_t MARC_TX_END = 0x14;
static const uint8_t MARC_TXFIFO_UNDER = 0x16;
static const uint8_t FIFO_BYTES_MASK = 0x7F;
static const uint8_t FIFO_ERROR_BIT = 0x80;
static const uint8_t CRC_OK_BIT = 0x80;

static const uint8_t FIFO_SIZE = 64;
static const uint8_t MAX_TX_FRAME = 140;  // longest sub-frame payload of a train
static const uint8_t MAX_RX_RAW = 1 + MAX_TX_FRAME + 2;
static const uint8_t MAX_TRAIN = 48;  // sub-frames per CMD_RESPOND response
static const uint8_t MAX_MATCH = 64;
static const uint32_t TX_TMO_MS = 100;

// The chip needs a settle time between CS low and the first clock edge. The slow
// profile is the safe default, the fast profile keeps the wake-up burst tight.
static const uint32_t SPI_SLOW_HZ = 1000000;
static const uint32_t SPI_FAST_HZ = 6000000;
static const uint16_t SPI_SLOW_SETTLE_US = 150;
static const uint16_t SPI_FAST_SETTLE_US = 1;
// TI CC1101 errata: a status register read while the radio is active can return
// a value in transition, so two equal reads in a row are required.
static const uint8_t STATUS_READ_TRIES = 4;

// ------------------------------------------------------------------ CSL timing

static const uint32_t CSL_PERIOD_US = 1000000;
static const uint32_t CSL_UNIT_US = 320;  // one CSL phase unit, 10 symbols
static const uint32_t CSL_SAMPLE_GUARD_US = 40000;
static const uint32_t CSL_BOOTSTRAP_US = 1000000;
static const uint8_t CSL_WAKE_CAP = 253;
static const uint16_t CSL_ACK_TMO_MAX_MS = 1000;
static const uint16_t CSL_FOLLOW_TMO_MS = 500;
static const uint8_t CSL_FOLLOW_MAX = 6;
static const uint8_t CSL_WAKE_BUDGET = 4;  // wake frames per state machine step
static const uint32_t CSL_GUARD_MARGIN_MS = 400;
static const uint8_t CSL_ANCHOR_MISS_LIMIT = 4;
static const uint16_t RA_PERIOD_UNITS = 3125;  // 1 s of CSL phase units
static const uint32_t RA_PERIOD_US = 1000000;
static const uint8_t RA_WAKE_CAP = 253;

static const uint8_t CSL_ANCHORS = 16;
// The guard grows with the time since the last refresh, for the clock error of
// two crystals at 20 and 10 ppm. At half a period the train covers every sample
// instant, as the bootstrap train does. There is no drift estimate: the
// commercial one divides by 60 periods samples stamped to 32 us, and a stamp
// error of a few ms here, divided the same way, moves a projection of some
// hundred periods off the sample.
static const uint64_t CSL_GUARD_PPM = 30;
static const uint32_t CSL_GUARD_MAX_US = CSL_PERIOD_US / 2;

static const uint8_t CSL_NEIGHBORS = 16;
static const uint8_t CSL_NEIGHBOR_DEPTH = 32;  // one fragmented datagram batch
static const uint8_t CSL_JOB_POOL = 128;
static const uint8_t CSL_JOB_NONE = 0xFF;
static const uint8_t CSL_COMPLETE_DEPTH = 8;
static const uint8_t CSL_INBOUND_DEPTH = 32;

// A batch of CSL_NEIGHBOR_DEPTH jobs for one neighbour has to be admitted while
// other neighbours still hold work, so the shared pool keeps room for several
// full batches at once.
static_assert(CSL_JOB_POOL >= 4 * CSL_NEIGHBOR_DEPTH, "job pool too small for a full batch");
static_assert(CSL_JOB_POOL < CSL_JOB_NONE, "job index does not fit the pool sentinel");

// ----------------------------------------------------------------- pair window

// Frame sizes of tado/beacon.go. The CCM* of the link takes the nonce from
// frame[0:13] and the aad from frame[0:16]; the rest up to the 4-byte MIC is
// ciphertext.
static const uint8_t EBR_LEN = 27;
static const uint8_t EB_LEN = 32;
static const uint8_t CCM_NONCE_LEN = 13;
static const uint8_t CCM_AAD_LEN = 16;
static const uint8_t CCM_MIC_LEN = 4;
static const uint8_t MAC_CMD_BEACON_REQ = 0x07;
static const uint8_t SEC_CTRL = 0x04;
static const uint8_t PAIRWIN_OPEN_LEN = 2 + 16 + 8 + 1 + 4 + 1;
static const uint16_t PAIRWIN_MAX_S = 600;
// The commercial beacon starts 2.4 to 7.4 ms after the request ends
// (the pairing capture), after its 124-tick listen of 3.968 ms.
static const uint32_t EB_GAP_US = 3968;

// ------------------------------------------------------------- task parameters

static const uint32_t RADIO_STACK = 8192;
static const uint32_t SERIAL_STACK = 4096;
static const UBaseType_t RADIO_PRIO = 3;
static const UBaseType_t SERIAL_PRIO = 3;
static const int RADIO_CORE = 1;
static const int SERIAL_CORE = 0;
static const uint32_t WDT_TIMEOUT_MS = 2000;

static const uint8_t CMD_POOL = 2;
static const uint8_t OUT_POOL = 4;
static const size_t OUT_BUF_MAX = 4 + MAX_PAYLOAD;

static const uint32_t RADIO_IDLE_TICK_MS = 8;
static const uint32_t RX_ASSEMBLE_TICK_MS = 5;  // FIFO fills in ~10 ms at 50 kBaud
// Before the first byte of a frame the assembler is idle, so a lost GDO2 edge
// leaves this tick as the only pump. The 64-byte FIFO overflows ~11 ms after the
// frame starts, so the tick has to stay well below that.
static const uint32_t RX_HOUSEKEEP_TICK_MS = 8;
// The longest frame takes 23 ms on air at 50 kBaud. Past this the assembler
// holds the wreck of an aborted packet and has to start over.
static const uint32_t RX_STALE_MS = 60;
static const uint32_t HOST_SPI_HOLDOFF_MS = 500;  // host drives the radio itself

// ------------------------------------------------------------ shared structures

struct CmdMsg {
  uint8_t cmd;
  uint16_t len;
  uint8_t payload[MAX_PAYLOAD];
};

struct OutMsg {
  uint16_t len;
  uint8_t data[OUT_BUF_MAX];
};

struct CslFollow {
  uint8_t raw[CSL_FOLLOW_MAX][MAX_RX_RAW];
  uint8_t len[CSL_FOLLOW_MAX];
  uint8_t count;
  uint8_t dropped;
};

struct CslJob {
  uint16_t id;
  uint16_t ackTmoMs;
  uint16_t phase;
  uint8_t flags;
  uint8_t kind;
  uint8_t shortAddr[2];
  uint8_t dataLen;
  uint8_t data[MAX_TX_FRAME];
  uint8_t next;
};

struct CslNeighbor {
  bool used;
  uint8_t shortAddr[2];
  uint8_t head;
  uint8_t tail;
  uint8_t count;
};

struct CslAnchor {
  bool valid;
  uint8_t shortAddr[2];
  uint8_t misses;
  uint64_t sampleUs;
  uint64_t refreshUs;
};

struct CslCompletion {
  uint16_t id;
  uint8_t flags;
  uint8_t status;
  uint8_t sent;
  uint8_t ackLen;
  uint8_t ack[MAX_RX_RAW];
  CslFollow follow;
};

struct CslInbound {
  uint8_t raw[MAX_RX_RAW];
  uint8_t len;
  uint8_t freqest;
};

enum : uint8_t { JOB_QUEUED = 0, JOB_SYNC_CSL = 1, JOB_RA = 2 };

enum : uint8_t {
  CSL_IDLE = 0,
  CSL_RX = 1,
  CSL_WAKE = 2,
  CSL_ACK = 3,
  CSL_FOLLOW = 4
};

enum : uint8_t {
  OUT_OK = 0,
  OUT_TXFAIL = 1,
  OUT_NOACK = 2,
  OUT_BADACK = 3,
  OUT_INPUT = 4,
  OUT_NOANCHOR = 5
};

enum : uint8_t { CSLQ_ENQUEUE = 0, CSLQ_POLL = 1, CSLQ_FLUSH = 2 };
enum : uint8_t { CSL_FLAG_BOOTSTRAP = 1, CSL_FLAG_FOLLOW = 2 };
enum : uint8_t { AUTOACK_NONE = 0, AUTOACK_SENT = 1, AUTOACK_FAILED = 2 };

struct CslTxn {
  bool active;
  bool outcomeReady;
  uint8_t state;
  uint8_t outcome;
  uint8_t sent;
  bool sawInvalid;
  bool targetStored;
  CslJob job;
  uint64_t startUs;
  uint64_t wakeEndUs;
  uint64_t windowEndUs;
  uint64_t guardUs;
  uint8_t ack[MAX_RX_RAW];
  uint8_t ackLen;
  CslFollow follow;
};

struct RxAssembler {
  uint8_t buf[MAX_RX_RAW];
  uint8_t freqest;
  int16_t pktLen;
  int16_t total;
  uint8_t got;
  uint64_t startUs;
};

// ------------------------------------------------------------ task plumbing

static QueueHandle_t qCmd;      // serialTask -> radioTask, one request each
static QueueHandle_t qCmdFree;  // free CmdMsg slots
static QueueHandle_t qOut;      // radioTask -> serialTask, bytes to put on the wire
static QueueHandle_t qOutFree;  // free OutMsg slots

static CmdMsg cmdPool[CMD_POOL];
static OutMsg outPool[OUT_POOL];

static TaskHandle_t radioTaskHandle = nullptr;

// The host aborts a running stream by sending any byte. serialTask counts every
// byte it reads, the stream loop compares against the count it started with.
static std::atomic<uint32_t> hostByteSeq(0);
// Bumped by both GDO interrupts. The radio task pumps the FIFO when it moves.
static std::atomic<uint32_t> gdoSeq(0);
// Low 32 bits of the end-of-frame instant. A 32-bit store is atomic on this
// core, a 64-bit one is not, and the high word is rebuilt from the clock.
static std::atomic<uint32_t> gdo0StampLo(0);

static const uint32_t NOTIFY_RADIO = 0x1;
static const uint32_t NOTIFY_ALL_BITS = 0xFFFFFFFFUL;

// ------------------------------------------------------- radio task owned state

static SPISettings spiSlow(SPI_SLOW_HZ, MSBFIRST, SPI_MODE0);
static SPISettings spiFast(SPI_FAST_HZ, MSBFIRST, SPI_MODE0);
static SPISettings *spiActive = &spiSlow;
static uint16_t spiSettleUs = SPI_SLOW_SETTLE_US;

static RxAssembler rx;
static uint32_t gdoSeenSeq = 0;
static uint64_t lastPumpUs = 0;
static uint64_t hostSpiHoldoffUs = 0;

static CslJob cslJobs[CSL_JOB_POOL];
static uint8_t cslJobFree;
static uint8_t cslJobFreeCount;
static CslNeighbor cslNeighbors[CSL_NEIGHBORS];
static uint8_t cslCursor;
static CslAnchor cslAnchors[CSL_ANCHORS];
static CslCompletion cslComplete[CSL_COMPLETE_DEPTH];
static uint8_t cslCompleteHead, cslCompleteTail, cslCompleteCount;
static CslInbound cslInbound[CSL_INBOUND_DEPTH];
static uint8_t cslInboundHead, cslInboundTail, cslInboundCount;
static CslTxn txn;
static bool exclusiveWaiting;

// Last coordinator seen on a request. An uplink caught between transactions is
// answered with it, the same Enhanced ACK the host would send one poll later.
// A passive receive, a flush or a reset forgets it: a host that stops
// transmitting must not leave the bridge answering on its behalf.
static uint8_t coordEUI[8];
static bool coordKnown;
static uint16_t coordPhase;

// The pair window the host opened with CMD_PAIRWIN. The beacons take seq and
// frame counter from the block the host reserved for them.
struct PairWindow {
  bool active;
  uint64_t endUs;
  mbedtls_aes_context aes;
  uint8_t coord[8];
  uint8_t seq;
  uint32_t ctr;
  uint8_t left;
  uint8_t sent;
};
static PairWindow pairWin;

static uint8_t respBuf[MAX_PAYLOAD];

// ----------------------------------------------------------------- small tools

static inline uint64_t nowUs() {
  return (uint64_t)esp_timer_get_time();
}

// Rebuild a full 64-bit instant from the low word an interrupt stored. Exact for
// anything the clock passed in the last wrap period (about 71 minutes).
static uint64_t stampFromLo(uint32_t lo) {
  uint64_t now = nowUs();
  uint64_t full = (now & ~0xFFFFFFFFULL) | (uint64_t)lo;
  if (full > now) full -= 0x100000000ULL;
  return full;
}

static inline void csLow() {
  digitalWrite(PIN_CS, LOW);
}

static inline void csHigh() {
  digitalWrite(PIN_CS, HIGH);
}

static uint16_t macFCS(const uint8_t *p, size_t n) {
  uint16_t crc = 0;
  for (size_t i = 0; i < n; i++) {
    crc ^= p[i];
    for (uint8_t bit = 0; bit < 8; bit++) {
      crc = (crc & 1) ? ((crc >> 1) ^ 0x8408) : (crc >> 1);
    }
  }
  return crc;
}

// ------------------------------------------------------------------ interrupts

static void IRAM_ATTR gdo0Isr() {
  // IOCFG0 = 0x07 asserts at the end of a CRC-OK packet, so the rising level is
  // the end of frame instant. Read the level, an edge count can miss a pulse.
  if (digitalRead(PIN_GDO0)) {
    gdo0StampLo.store((uint32_t)esp_timer_get_time(), std::memory_order_relaxed);
  }
  gdoSeq.fetch_add(1, std::memory_order_relaxed);
  if (radioTaskHandle != nullptr) {
    BaseType_t woken = pdFALSE;
    xTaskNotifyFromISR(radioTaskHandle, NOTIFY_RADIO, eSetBits, &woken);
    if (woken) portYIELD_FROM_ISR();
  }
}

static void IRAM_ATTR gdo2Isr() {
  // IOCFG2 = 0x00 asserts at the RX FIFO threshold, which is the only warning
  // for a frame longer than the 64-byte FIFO.
  gdoSeq.fetch_add(1, std::memory_order_relaxed);
  if (radioTaskHandle != nullptr) {
    BaseType_t woken = pdFALSE;
    xTaskNotifyFromISR(radioTaskHandle, NOTIFY_RADIO, eSetBits, &woken);
    if (woken) portYIELD_FROM_ISR();
  }
}

// --------------------------------------------------------------- SPI primitives

static void spiUseFast(bool fast) {
  spiActive = fast ? &spiFast : &spiSlow;
  spiSettleUs = fast ? SPI_FAST_SETTLE_US : SPI_SLOW_SETTLE_US;
}

// Full-duplex transfer of n bytes in place, framed by CS. Do not read MISO to
// poll the chip-ready line: the ESP32 SPI peripheral owns that matrix input.
static bool spiXfer(uint8_t *p, size_t n) {
  SPI.beginTransaction(*spiActive);
  csLow();
  delayMicroseconds(spiSettleUs);
  for (size_t i = 0; i < n; i++) p[i] = SPI.transfer(p[i]);
  csHigh();
  SPI.endTransaction();
  return true;
}

static uint8_t ccStatusRaw(uint8_t addr) {
  uint8_t b[2] = {(uint8_t)(addr | READ_BURST), 0x00};
  spiXfer(b, 2);
  return b[1];
}

// TI CC1101 errata: RXBYTES, TXBYTES and MARCSTATE read while the radio is
// active can return a value in transition. Accept two equal reads in a row.
static uint8_t ccStatus(uint8_t addr) {
  uint8_t prev = ccStatusRaw(addr);
  for (uint8_t i = 0; i < STATUS_READ_TRIES; i++) {
    uint8_t cur = ccStatusRaw(addr);
    if (cur == prev) return cur;
    prev = cur;
  }
  return prev;
}

static void ccStrobe(uint8_t addr) {
  uint8_t b[1] = {addr};
  spiXfer(b, 1);
}

static void ccWriteReg(uint8_t addr, uint8_t val) {
  uint8_t b[2] = {addr, val};
  spiXfer(b, 2);
}

static uint8_t ccReadReg(uint8_t addr) {
  uint8_t b[2] = {(uint8_t)(addr | READ_SINGLE), 0x00};
  spiXfer(b, 2);
  return b[1];
}

static void ccReadFifo(uint8_t *dst, uint8_t n) {
  if (n > FIFO_SIZE) n = FIFO_SIZE;
  uint8_t b[1 + FIFO_SIZE];
  b[0] = CC_RXFIFO | READ_BURST;
  memset(b + 1, 0, n);
  spiXfer(b, 1 + n);
  memcpy(dst, b + 1, n);
}

static void ccWriteFifo(const uint8_t *src, uint8_t n) {
  if (n > FIFO_SIZE) n = FIFO_SIZE;
  uint8_t b[1 + FIFO_SIZE];
  b[0] = CC_TXFIFO | WRITE_BURST;
  memcpy(b + 1, src, n);
  spiXfer(b, 1 + n);
}

static void ccArmRx() {
  ccStrobe(CC_SIDLE);
  ccStrobe(CC_SFRX);
  ccStrobe(CC_SRX);
}

// Manual power-on reset, CC1101 datasheet 19.1.2, then quiet GDO pins.
static void manualReset() {
  spiUseFast(false);
  csHigh();
  delayMicroseconds(5);
  csLow();
  delayMicroseconds(10);
  csHigh();
  delayMicroseconds(45);
  SPI.beginTransaction(spiSlow);
  csLow();
  delayMicroseconds(SPI_SLOW_SETTLE_US);
  SPI.transfer(CC_SRES);
  delayMicroseconds(SPI_SLOW_SETTLE_US);
  csHigh();
  SPI.endTransaction();
  // The reset ends when SO goes low, but the SPI peripheral owns MISO. A read
  // back of both registers confirms that the chip took the writes.
  for (uint8_t i = 0; i < GDO_QUIET_TRIES; i++) {
    ccWriteReg(CC_IOCFG0, GDO_CFG_LOW);
    ccWriteReg(CC_IOCFG2, GDO_CFG_LOW);
    if (ccReadReg(CC_IOCFG0) == GDO_CFG_LOW && ccReadReg(CC_IOCFG2) == GDO_CFG_LOW) break;
  }
}

// ------------------------------------------------------------------ frame sends

// Wait for the packet to leave the chip. The Tado register set has MCSM1
// TXOFF_MODE=RX, so the state machine lands in RX and never shows IDLE: TX is
// over once it is out of TX/TX_END with a drained FIFO.
static bool ccTxWait(uint32_t tmoMs) {
  uint64_t deadline = nowUs() + (uint64_t)tmoMs * 1000ULL;
  while (nowUs() < deadline) {
    esp_task_wdt_reset();
    uint8_t m = ccStatus(CC_MARCSTATE) & MARC_STATE_MASK;
    if (m == MARC_TXFIFO_UNDER) {
      ccStrobe(CC_SFTX);
      return false;
    }
    if (m != MARC_TX && m != MARC_TX_END && (ccStatus(CC_TXBYTES) & FIFO_BYTES_MASK) == 0) {
      return true;
    }
  }
  return false;
}

static void rxReset();

// A failed TX leaves the chip in IDLE, or in TX after a timeout. Put it in RX
// with an empty RX FIFO, where TXOFF_MODE=RX leaves a TX that ends well.
static bool ccTxFailed() {
  ccArmRx();
  rxReset();
  return false;
}

// Transmit one variable-length frame ([len][payload], the chip appends the CRC).
// Streams the FIFO when the frame does not fit in one burst.
static bool ccSendFrame(const uint8_t *payload, uint8_t n) {
  if (n == 0 || n > MAX_TX_FRAME) return false;
  uint8_t data[1 + MAX_TX_FRAME];
  data[0] = n;
  memcpy(data + 1, payload, n);
  uint16_t total = 1 + (uint16_t)n;
  // SIDLE keeps a frame cut off mid-air in the RX FIFO, and TXOFF_MODE=RX puts
  // the reply to this frame behind it, so flush now while the chip is idle.
  // One access carries all three strobes (datasheet 10.4, RX to IDLE ~0.1 us).
  uint8_t prep[3] = {CC_SIDLE, CC_SFRX, CC_SFTX};
  spiXfer(prep, sizeof(prep));
  rxReset();
  uint8_t first = (total > FIFO_SIZE) ? FIFO_SIZE : (uint8_t)total;
  ccWriteFifo(data, first);
  uint16_t idx = first;
  ccStrobe(CC_STX);
  uint64_t deadline = nowUs() + (uint64_t)TX_TMO_MS * 1000ULL;
  while (idx < total) {
    esp_task_wdt_reset();
    if (nowUs() > deadline) {
      ccStrobe(CC_SIDLE);
      ccStrobe(CC_SFTX);
      return ccTxFailed();
    }
    uint8_t tb = ccStatus(CC_TXBYTES);
    if (tb & FIFO_ERROR_BIT) {  // TXFIFO_UNDERFLOW: SFTX is the only way out (15.4)
      ccStrobe(CC_SFTX);
      return ccTxFailed();
    }
    // A read above 64 is a corrupt one (datasheet 10.3): read again.
    uint8_t used = tb & FIFO_BYTES_MASK;
    if (used >= FIFO_SIZE) continue;
    uint16_t chunk = total - idx;
    if (chunk > (uint16_t)(FIFO_SIZE - used)) chunk = FIFO_SIZE - used;
    ccWriteFifo(data + idx, (uint8_t)chunk);
    idx += chunk;
  }
  if (!ccTxWait(TX_TMO_MS)) return ccTxFailed();
  return true;
}

// Transmit `count` packed [len][bytes] sub-frames back-to-back, with no serial
// round-trip between them: the gap is SPI plus state machine time, not the
// ~2.5 ms USB serial latency a host-driven burst pays. Returns the frames sent.
static uint8_t ccSendTrain(const uint8_t *p, size_t len, uint8_t count) {
  uint8_t sent = 0;
  size_t off = 0;
  for (uint8_t i = 0; i < count; i++) {
    if (off >= len) break;
    uint8_t n = p[off];
    if (n == 0 || n > MAX_TX_FRAME || off + 1 + (size_t)n > len) break;
    if (!ccSendFrame(p + off + 1, n)) break;
    off += 1 + (size_t)n;
    sent++;
  }
  return sent;
}

// ------------------------------------------------------------------- auto-ACK

static uint8_t ccAutoAck(const uint8_t *raw, size_t rawLen, const uint8_t *coordinator, uint16_t phase) {
  if (rawLen < 19 || !(raw[rawLen - 1] & CRC_OK_BIT)) return AUTOACK_NONE;
  uint8_t macLen = raw[0];
  if (macLen < 16 || rawLen != (size_t)1 + macLen + 2) return AUTOACK_NONE;
  const uint8_t *mac = raw + 1;
  if (!((mac[0] == 0x69 || mac[0] == 0x79) && mac[1] == 0xEC) || memcmp(mac + 3, coordinator, 8)) {
    return AUTOACK_NONE;
  }
  uint8_t ack[29] = {0x42, 0xEE, mac[2]};
  memcpy(ack + 3, mac + 11, 5);
  ack[8] = 0xC5;
  ack[9] = 0x1B;
  ack[10] = 0x00;
  memcpy(ack + 11, coordinator, 8);
  ack[19] = 0x04;
  ack[20] = 0x0D;
  ack[21] = (uint8_t)phase;
  ack[22] = (uint8_t)(phase >> 8);
  ack[23] = 0x35;
  ack[24] = 0x0C;
  ack[25] = 0x80;
  ack[26] = 0x3F;
  uint16_t fcs = macFCS(ack, 27);
  ack[27] = (uint8_t)fcs;
  ack[28] = (uint8_t)(fcs >> 8);
  return ccSendFrame(ack, sizeof(ack)) ? AUTOACK_SENT : AUTOACK_FAILED;
}

static void coordFromData(const uint8_t *data, uint8_t *out) {
  memcpy(out, data + 11, 5);
  out[5] = 0xC5;
  out[6] = 0x1B;
  out[7] = 0x00;
}

static void coordRemember(const uint8_t *eui, uint16_t phase) {
  // A CSLTX without the follow flag carries no phase. Keep the last one known
  // for this coordinator instead of advertising phase 0.
  if (phase == 0 && coordKnown && coordPhase != 0 && memcmp(coordEUI, eui, 8) == 0) {
    phase = coordPhase;
  }
  memcpy(coordEUI, eui, 8);
  coordPhase = phase;
  coordKnown = true;
}

// --------------------------------------------------------------- RX assembling

static void rxReset() {
  rx.pktLen = -1;
  rx.total = -1;
  rx.got = 0;
  rx.startUs = 0;
  rx.freqest = 0;
}

static bool rxAssembling() {
  return rx.pktLen >= 0;
}

static void rxFlush() {
  ccArmRx();
  rxReset();
}

// Drain the RX FIFO into the assembler. Returns true when a whole raw frame
// (length byte, payload, RSSI, LQI/CRC) is ready. Datasheet 10.5: a read that
// empties the RX FIFO while a byte arrives duplicates that byte, so one byte
// stays behind until the FIFO holds the rest of the packet. RXBYTES alone tells
// that: GDO0 can still be high from the packet before. The shortest packet is
// 4 bytes with its status bytes, so the length byte always has one behind it.
// FREQEST is captured at that instant, because any strobe or transmission that
// follows takes the demodulator off this packet and scatters the reading.
static bool rxPump() {
  lastPumpUs = nowUs();
  if (rx.pktLen >= 0 && lastPumpUs - rx.startUs > (uint64_t)RX_STALE_MS * 1000ULL) {
    rxFlush();  // the packet stopped mid-air, do not glue the next one onto it
  }
  for (;;) {
    uint8_t rb = ccStatus(CC_RXBYTES);
    if (rb & FIFO_ERROR_BIT) {  // overflow, drop the partial frame and re-arm
      rxFlush();
      return false;
    }
    uint8_t n = rb & FIFO_BYTES_MASK;
    if (n > FIFO_SIZE) n = FIFO_SIZE;
    if (rx.pktLen < 0) {
      if (n >= 2) {
        ccReadFifo(rx.buf, 1);
        rx.got = 1;
        rx.pktLen = rx.buf[0];
        rx.total = 1 + rx.pktLen + 2;
        rx.startUs = nowUs();
        if (rx.pktLen < 1 || rx.pktLen > MAX_TX_FRAME) {
          rxFlush();
          return false;
        }
        continue;
      }
      return false;
    }
    int remaining = rx.total - (int)rx.got;
    if (remaining <= 0) return true;  // already complete, FREQEST already taken
    uint8_t toRead = 0;
    if (n >= remaining) {
      toRead = (uint8_t)remaining;
    } else if (n > 1) {
      toRead = n - 1;  // still receiving, leave one byte behind
    }
    if (toRead == 0) return false;
    ccReadFifo(rx.buf + rx.got, toRead);
    rx.got += toRead;
    if ((int)rx.got >= rx.total) {
      rx.freqest = ccStatusRaw(CC_FREQEST);
      return true;
    }
  }
}

// Hand the assembled frame to the caller, with the FREQEST of that frame.
static void rxTake(uint8_t *dst, size_t *len, uint64_t *stampUs, uint8_t *freqest) {
  *len = rx.got;
  memcpy(dst, rx.buf, rx.got);
  if (stampUs != nullptr) *stampUs = stampFromLo(gdo0StampLo.load(std::memory_order_relaxed));
  if (freqest != nullptr) *freqest = rx.freqest;
  rxReset();
}

static bool rxCrcOk(const uint8_t *raw, size_t len) {
  return len >= 3 && (raw[len - 1] & CRC_OK_BIT);
}

static bool rxDuePump(uint64_t now) {
  uint32_t seq = gdoSeq.load(std::memory_order_relaxed);
  if (seq != gdoSeenSeq) {
    gdoSeenSeq = seq;
    return true;
  }
  uint32_t tickMs = rxAssembling() ? RX_ASSEMBLE_TICK_MS : RX_HOUSEKEEP_TICK_MS;
  return now - lastPumpUs >= (uint64_t)tickMs * 1000ULL;
}

static void radioWait(uint32_t ms);

// Wait for one complete frame on an already armed receiver.
static bool rxWaitFrame(uint32_t tmoMs, uint8_t *dst, size_t *len, uint64_t *stampUs,
                        uint8_t *freqest) {
  uint64_t deadline = nowUs() + (uint64_t)tmoMs * 1000ULL;
  for (;;) {
    uint64_t now = nowUs();
    if (rxDuePump(now) && rxPump()) {
      rxTake(dst, len, stampUs, freqest);
      return true;
    }
    now = nowUs();
    if (now >= deadline) return false;
    uint64_t left = (deadline - now) / 1000ULL;
    uint32_t tickMs = rxAssembling() ? RX_ASSEMBLE_TICK_MS : RX_HOUSEKEEP_TICK_MS;
    radioWait(left < tickMs ? (uint32_t)left : tickMs);
    esp_task_wdt_reset();
  }
}

// ------------------------------------------------------------ pair window TX

static void pairAes(const uint8_t *in, uint8_t *out) {
  mbedtls_aes_crypt_ecb(&pairWin.aes, MBEDTLS_AES_ENCRYPT, in, out);
}

// macFCS for other lengths. A shared macFCS loses the constant length 27 of
// the Enhanced ACK, which changes the ACK code and adds a memw to spiXfer; with
// this copy the ACK and CSL paths compile as in v2.8. noipa keeps the compiler
// from merging the two copies.
static __attribute__((noipa)) uint16_t pairFCS(const uint8_t *p, size_t n) {
  uint16_t crc = 0;
  for (size_t i = 0; i < n; i++) {
    crc ^= p[i];
    for (uint8_t bit = 0; bit < 8; bit++) {
      crc = (crc & 1) ? ((crc >> 1) ^ 0x8408) : (crc >> 1);
    }
  }
  return crc;
}

// XOR the CCM* keystream (counter blocks A1, A2, ...) over n bytes at p.
static void ccmCrypt(const uint8_t *frame, uint8_t *p, uint8_t n) {
  uint8_t a[16], s[16];
  a[0] = 0x01;  // L - 1
  memcpy(a + 1, frame, CCM_NONCE_LEN);
  for (uint8_t off = 0, i = 1; off < n; off += 16, i++) {
    a[14] = 0;
    a[15] = i;
    pairAes(a, s);
    for (uint8_t j = 0; j < 16 && off + j < n; j++) p[off + j] ^= s[j];
  }
}

// The CCM* MIC (M=4, L=2) over the aad frame[0:16] and n plaintext bytes.
static void ccmMic(const uint8_t *frame, const uint8_t *pt, uint8_t n, uint8_t *mic) {
  uint8_t x[16], b[16];
  b[0] = 0x49;  // Adata, M=4, L=2
  memcpy(b + 1, frame, CCM_NONCE_LEN);
  b[14] = 0;
  b[15] = n;
  pairAes(b, x);
  uint8_t aad[32] = {0x00, CCM_AAD_LEN};
  memcpy(aad + 2, frame, CCM_AAD_LEN);
  for (uint8_t off = 0; off < sizeof(aad); off += 16) {
    for (uint8_t j = 0; j < 16; j++) b[j] = x[j] ^ aad[off + j];
    pairAes(b, x);
  }
  for (uint8_t off = 0; off < n; off += 16) {
    for (uint8_t j = 0; j < 16; j++) b[j] = x[j] ^ ((off + j < n) ? pt[off + j] : 0);
    pairAes(b, x);
  }
  b[0] = 0x01;
  memcpy(b + 1, frame, CCM_NONCE_LEN);
  b[14] = 0;
  b[15] = 0;
  uint8_t s0[16];
  pairAes(b, s0);
  for (uint8_t j = 0; j < CCM_MIC_LEN; j++) mic[j] = x[j] ^ s0[j];
}

// Answer an Enhanced Beacon Request of a joining device with an Enhanced
// Beacon (tado/beacon.go). The commercial bridge answers with no wake train,
// 2.4 to 7.4 ms after the request, while the device still listens; a round
// trip through the host is slower than that. raw is the received blob that
// was taken last, so the last GDO0 stamp is its end.
static void pairAnswer(const uint8_t *raw, size_t rawLen) {
  if (!pairWin.active) return;
  uint64_t now = nowUs();
  if (now >= pairWin.endUs) {
    pairWin.active = false;
    return;
  }
  uint64_t stampUs = stampFromLo(gdo0StampLo.load(std::memory_order_relaxed));
  if (pairWin.left == 0 || rawLen != (size_t)1 + EBR_LEN + 2 || raw[0] != EBR_LEN || !rxCrcOk(raw, rawLen)) return;
  const uint8_t *req = raw + 1;
  if (req[0] != 0x0B || req[1] != 0xE8 || req[15] != SEC_CTRL) return;
  if (memcmp(req + 7, pairWin.coord, 5) == 0) return;  // our own address is no joiner
  // Sealed part: counter(4) command(1) FCS(2).
  uint8_t pt[EBR_LEN - CCM_AAD_LEN - CCM_MIC_LEN];
  memcpy(pt, req + CCM_AAD_LEN, sizeof(pt));
  ccmCrypt(req, pt, sizeof(pt));
  uint8_t mic[CCM_MIC_LEN];
  ccmMic(req, pt, sizeof(pt), mic);
  if (memcmp(mic, req + EBR_LEN - CCM_MIC_LEN, CCM_MIC_LEN) != 0 || pt[4] != MAC_CMD_BEACON_REQ) return;
  uint8_t fcsIn[CCM_AAD_LEN + 5];
  memcpy(fcsIn, req, CCM_AAD_LEN);
  memcpy(fcsIn + CCM_AAD_LEN, pt, 5);
  uint16_t fcs = pairFCS(fcsIn, sizeof(fcsIn));
  if (pt[5] != (uint8_t)fcs || pt[6] != (uint8_t)(fcs >> 8)) return;

  uint8_t eb[EB_LEN] = {0x18, 0xEC, pairWin.seq, 0xCD, 0xAB};
  memcpy(eb + 5, req + 7, 8);
  memcpy(eb + 13, pairWin.coord, 8);
  eb[21] = SEC_CTRL;
  eb[22] = (uint8_t)pairWin.ctr;
  eb[23] = (uint8_t)(pairWin.ctr >> 8);
  eb[24] = (uint8_t)(pairWin.ctr >> 16);
  eb[25] = (uint8_t)(pairWin.ctr >> 24);
  fcs = pairFCS(eb, 26);
  eb[26] = (uint8_t)fcs;
  eb[27] = (uint8_t)(fcs >> 8);
  ccmMic(eb, eb + CCM_AAD_LEN, EB_LEN - CCM_AAD_LEN - CCM_MIC_LEN, eb + EB_LEN - CCM_MIC_LEN);
  ccmCrypt(eb, eb + CCM_AAD_LEN, EB_LEN - CCM_AAD_LEN - CCM_MIC_LEN);
  pairWin.seq++;
  pairWin.ctr++;
  pairWin.left--;
  // A stamp from an earlier frame lies in the past and waits for nothing.
  uint64_t at = stampUs + EB_GAP_US;
  while (nowUs() < at) {
  }
  if (ccSendFrame(eb, EB_LEN) && pairWin.sent != 0xFF) pairWin.sent++;
}

static void pairClose() {
  pairWin.active = false;
  pairWin.sent = 0;
}

// ------------------------------------------------------------- serial out queue

static OutMsg *outAcquire() {
  OutMsg *m = nullptr;
  while (xQueueReceive(qOutFree, &m, pdMS_TO_TICKS(100)) != pdTRUE) {
    esp_task_wdt_reset();
  }
  return m;
}

static void outPost(OutMsg *m) {
  while (xQueueSend(qOut, &m, pdMS_TO_TICKS(100)) != pdTRUE) {
    esp_task_wdt_reset();
  }
}

static void sendResp(uint8_t status, const uint8_t *p, size_t n) {
  if (n > MAX_PAYLOAD) n = MAX_PAYLOAD;
  OutMsg *m = outAcquire();
  m->data[0] = RSP_MAGIC;
  m->data[1] = status;
  m->data[2] = (uint8_t)(n >> 8);
  m->data[3] = (uint8_t)(n & 0xFF);
  if (n) memcpy(m->data + 4, p, n);
  m->len = (uint16_t)(4 + n);
  outPost(m);
}

static void sendStreamMarker(uint8_t marker) {
  OutMsg *m = outAcquire();
  m->data[0] = RSP_MAGIC;
  m->data[1] = marker;
  m->data[2] = 0x00;
  m->len = 3;
  outPost(m);
}

// One received frame on the stream: 5A F1 <n> <n bytes> <freqest> <xor8>.
static void sendStreamFrame(const uint8_t *p, uint8_t n, uint8_t freqest) {
  OutMsg *m = outAcquire();
  m->data[0] = RSP_MAGIC;
  m->data[1] = STREAM_FRAME;
  m->data[2] = n;
  memcpy(m->data + 3, p, n);
  m->data[3 + n] = freqest;
  uint8_t x = freqest;
  for (uint8_t i = 0; i < n; i++) x ^= p[i];
  m->data[4 + n] = x;
  m->len = (uint16_t)(5 + n);
  outPost(m);
}

// ------------------------------------------------------------------- CSL anchors

// An anchor has no age limit: only CSL_ANCHOR_MISS_LIMIT misses in a row drop
// it, as in the commercial bridge. The guard grows with its age instead.
static int8_t anchorIndex(const uint8_t *shortAddr) {
  for (uint8_t i = 0; i < CSL_ANCHORS; i++) {
    CslAnchor *a = &cslAnchors[i];
    if (!a->valid) continue;
    if (a->shortAddr[0] != shortAddr[0] || a->shortAddr[1] != shortAddr[1]) continue;
    return (int8_t)i;
  }
  return -1;
}

static void anchorStore(const uint8_t *shortAddr, const uint8_t *ack, uint64_t stampUs) {
  uint16_t phase = (uint16_t)ack[21] | ((uint16_t)ack[22] << 8);
  int8_t idx = anchorIndex(shortAddr);
  if (idx < 0) {
    int8_t pick = -1;
    for (uint8_t i = 0; i < CSL_ANCHORS; i++) {
      if (!cslAnchors[i].valid) {
        pick = (int8_t)i;
        break;
      }
    }
    if (pick < 0) {  // evict the least recently refreshed, never a fixed slot
      pick = 0;
      for (uint8_t i = 1; i < CSL_ANCHORS; i++) {
        if (cslAnchors[i].refreshUs < cslAnchors[pick].refreshUs) pick = (int8_t)i;
      }
    }
    idx = pick;
  }
  CslAnchor *a = &cslAnchors[idx];
  a->shortAddr[0] = shortAddr[0];
  a->shortAddr[1] = shortAddr[1];
  // A missed GDO0 edge leaves a stale timestamp.
  uint64_t now = nowUs();
  if (stampUs > now || now - stampUs > CSL_PERIOD_US) stampUs = now;
  // The phase field counts back to a sample the peer woke on, and every later
  // sample is one CSL period apart, so only the phase modulo one period counts.
  // A phase of up to 21 s can exceed the uptime: the anchor then moves one
  // period forward, instead of an underflow to a sample that never comes.
  uint64_t back = ((uint64_t)phase * CSL_UNIT_US) % CSL_PERIOD_US;
  a->sampleUs = (stampUs >= back) ? stampUs - back : stampUs + CSL_PERIOD_US - back;
  a->refreshUs = stampUs;
  a->misses = 0;
  a->valid = true;
}

static void anchorMiss(const uint8_t *shortAddr) {
  int8_t idx = anchorIndex(shortAddr);
  if (idx < 0) return;
  if (++cslAnchors[idx].misses >= CSL_ANCHOR_MISS_LIMIT) cslAnchors[idx].valid = false;
}

// A bootstrap job skips an anchor that has missed once. A device that rebooted
// samples at a new phase, and its uplinks carry no CSL IE that would show it.
static int8_t anchorForJob(const CslJob *job) {
  int8_t idx = anchorIndex(job->shortAddr);
  if (idx >= 0 && (job->flags & CSL_FLAG_BOOTSTRAP) && cslAnchors[idx].misses != 0) return -1;
  return idx;
}

// Half the wake train around a sample: the base guard plus the clock error that
// can build up since the last refresh.
static uint32_t anchorGuardUs(const CslAnchor *a, uint64_t sampleUs) {
  uint64_t age = (sampleUs > a->refreshUs) ? sampleUs - a->refreshUs : 0;
  uint64_t guard = CSL_SAMPLE_GUARD_US + age * CSL_GUARD_PPM / 1000000ULL;
  return (guard > CSL_GUARD_MAX_US) ? CSL_GUARD_MAX_US : (uint32_t)guard;
}

// The first sample that leaves the whole guard before it. A sample closer than
// the guard would get a train that starts late, and a peer whose clock drifted
// early would sample before it: the job waits one more period instead.
static uint64_t anchorNextSample(int8_t idx, uint64_t now, uint32_t *guardUs) {
  const CslAnchor *a = &cslAnchors[idx];
  uint64_t sample = a->sampleUs;
  uint64_t earliest = now + CSL_SAMPLE_GUARD_US;
  if (sample < earliest) sample += ((earliest - sample + CSL_PERIOD_US - 1) / CSL_PERIOD_US) * CSL_PERIOD_US;
  // The guard is below half a period, so this takes at most one more period.
  for (;;) {
    uint32_t guard = anchorGuardUs(a, sample);
    if (sample >= now + guard) {
      *guardUs = guard;
      return sample;
    }
    sample += CSL_PERIOD_US;
  }
}

// ------------------------------------------------------------- CSL job storage

static void cslPoolInit() {
  for (uint8_t i = 0; i < CSL_JOB_POOL; i++) {
    cslJobs[i].next = (i + 1 < CSL_JOB_POOL) ? (uint8_t)(i + 1) : CSL_JOB_NONE;
  }
  cslJobFree = 0;
  cslJobFreeCount = CSL_JOB_POOL;
  for (uint8_t i = 0; i < CSL_NEIGHBORS; i++) {
    cslNeighbors[i].used = false;
    cslNeighbors[i].head = CSL_JOB_NONE;
    cslNeighbors[i].tail = CSL_JOB_NONE;
    cslNeighbors[i].count = 0;
  }
}

static uint8_t cslJobAlloc() {
  if (cslJobFree == CSL_JOB_NONE) return CSL_JOB_NONE;
  uint8_t idx = cslJobFree;
  cslJobFree = cslJobs[idx].next;
  cslJobFreeCount--;
  cslJobs[idx].next = CSL_JOB_NONE;
  return idx;
}

static void cslJobRelease(uint8_t idx) {
  cslJobs[idx].next = cslJobFree;
  cslJobFree = idx;
  cslJobFreeCount++;
}

static CslNeighbor *cslNeighborFind(const uint8_t *shortAddr, bool create) {
  CslNeighbor *freeSlot = nullptr;
  for (uint8_t i = 0; i < CSL_NEIGHBORS; i++) {
    CslNeighbor *e = &cslNeighbors[i];
    if (e->used && e->shortAddr[0] == shortAddr[0] && e->shortAddr[1] == shortAddr[1]) return e;
    if (!e->used && freeSlot == nullptr) freeSlot = e;
  }
  if (!create || freeSlot == nullptr) return nullptr;
  freeSlot->used = true;
  freeSlot->shortAddr[0] = shortAddr[0];
  freeSlot->shortAddr[1] = shortAddr[1];
  freeSlot->head = CSL_JOB_NONE;
  freeSlot->tail = CSL_JOB_NONE;
  freeSlot->count = 0;
  return freeSlot;
}

static void cslNeighborPush(CslNeighbor *e, uint8_t jobIdx) {
  cslJobs[jobIdx].next = CSL_JOB_NONE;
  if (e->tail == CSL_JOB_NONE) e->head = jobIdx;
  else cslJobs[e->tail].next = jobIdx;
  e->tail = jobIdx;
  e->count++;
}

static uint8_t cslNeighborPop(CslNeighbor *e) {
  uint8_t idx = e->head;
  if (idx == CSL_JOB_NONE) return CSL_JOB_NONE;
  e->head = cslJobs[idx].next;
  if (e->head == CSL_JOB_NONE) e->tail = CSL_JOB_NONE;
  e->count--;
  if (e->count == 0) e->used = false;
  return idx;
}

static void cslQueueClear() {
  for (uint8_t i = 0; i < CSL_NEIGHBORS; i++) {
    CslNeighbor *e = &cslNeighbors[i];
    if (e->used) {
      while (e->head != CSL_JOB_NONE) {
        uint8_t idx = e->head;
        e->head = cslJobs[idx].next;
        cslJobRelease(idx);
      }
    }
    e->used = false;
    e->head = CSL_JOB_NONE;
    e->tail = CSL_JOB_NONE;
    e->count = 0;
  }
  memset(cslComplete, 0, sizeof(cslComplete));
  memset(cslInbound, 0, sizeof(cslInbound));
  cslCompleteHead = cslCompleteTail = cslCompleteCount = 0;
  cslInboundHead = cslInboundTail = cslInboundCount = 0;
  // A synchronous CSLTX or a Router Advertisement still owes the host a reply,
  // so only a queued transaction is cancelled here.
  if (!txn.active || txn.job.kind == JOB_QUEUED) memset(&txn, 0, sizeof(txn));
}

// -------------------------------------------------------------- CSL validation

static bool cslAckValid(const uint8_t *raw, size_t rawLen, const uint8_t *data) {
  if (rawLen != 32 || raw[0] != 29) return false;
  const uint8_t *ack = raw + 1;
  if (ack[0] != 0x42 || ack[1] != 0xEE || ack[2] != data[2]) return false;
  if (memcmp(ack + 3, data + 11, 5) || ack[8] != 0xC5 || ack[9] != 0x1B || ack[10] != 0x00) return false;
  if (memcmp(ack + 11, data + 3, 8)) return false;
  uint16_t fcs = macFCS(ack, 27);
  return ack[27] == (uint8_t)fcs && ack[28] == (uint8_t)(fcs >> 8) && (raw[31] & CRC_OK_BIT);
}

static bool cslFollowMatch(const uint8_t *raw, size_t rawLen, const uint8_t *data) {
  if (rawLen < 19 || !(raw[rawLen - 1] & CRC_OK_BIT)) return false;
  if (rawLen != (size_t)1 + raw[0] + 2 || raw[0] < 16) return false;
  const uint8_t *mac = raw + 1;
  return (mac[0] == 0x69 || mac[0] == 0x79) && mac[1] == 0xEC &&
         memcmp(mac + 3, data + 11, 5) == 0 && mac[8] == 0xC5 && mac[9] == 0x1B &&
         mac[10] == 0x00 && mac[11] == data[3] && mac[12] == data[4];
}

// A full ring gets no Enhanced ACK: the device retransmits an uplink it sees
// unacknowledged, and an acknowledged uplink the ring cannot hold is lost.
static void cslStoreInbound(const uint8_t *raw, size_t rawLen, const uint8_t *coordinator,
                            uint16_t phase, uint8_t freqest) {
  if (rawLen > MAX_RX_RAW || cslInboundCount == CSL_INBOUND_DEPTH) return;
  if (coordinator != nullptr) ccAutoAck(raw, rawLen, coordinator, phase);
  if (!rxCrcOk(raw, rawLen) || raw[1] == 0x25) return;
  CslInbound *slot = &cslInbound[cslInboundTail];
  memcpy(slot->raw, raw, rawLen);
  slot->len = (uint8_t)rawLen;
  slot->freqest = freqest;
  cslInboundTail = (uint8_t)((cslInboundTail + 1) % CSL_INBOUND_DEPTH);
  cslInboundCount++;
}

static void cslStoreInboundForJob(const uint8_t *raw, size_t rawLen, const CslJob *job,
                                  uint8_t freqest) {
  uint8_t coordinator[8];
  coordFromData(job->data, coordinator);
  cslStoreInbound(raw, rawLen, coordinator, job->phase, freqest);
}

// ------------------------------------------------------------ CSL state machine

static uint8_t cslWireStatus(uint8_t outcome) {
  if (outcome == OUT_OK) return ST_OK;
  if (outcome == OUT_INPUT) return ST_INPUT;
  if (outcome == OUT_NOACK) return ST_NOACK;
  if (outcome == OUT_BADACK) return ST_BADACK;
  if (outcome == OUT_NOANCHOR) return ST_NOANCHOR;
  return ST_TXFAIL;
}

static void cslSetOutcome(uint8_t outcome) {
  txn.outcome = outcome;
  txn.outcomeReady = true;
}

static size_t cslAppendFollowBody(uint8_t *out, size_t off, const uint8_t *ack, uint8_t ackLen,
                                  const CslFollow *follow) {
  out[off++] = ackLen;
  memcpy(out + off, ack, ackLen);
  off += ackLen;
  out[off++] = follow->count;
  for (uint8_t i = 0; i < follow->count; i++) {
    out[off++] = follow->len[i];
    memcpy(out + off, follow->raw[i], follow->len[i]);
    off += follow->len[i];
  }
  out[off++] = follow->dropped;
  return off;
}

// Reply to a synchronous CMD_CSLTX.
static void cslReplySync() {
  uint8_t status = cslWireStatus(txn.outcome);
  if (txn.outcome != OUT_OK) {
    respBuf[0] = txn.sent;
    sendResp(status, respBuf, 1);
    return;
  }
  respBuf[0] = txn.sent;
  if ((txn.job.flags & CSL_FLAG_FOLLOW) == 0) {
    memcpy(respBuf + 1, txn.ack, txn.ackLen);
    sendResp(ST_OK, respBuf, (size_t)txn.ackLen + 1);
    return;
  }
  size_t off = cslAppendFollowBody(respBuf, 1, txn.ack, txn.ackLen, &txn.follow);
  sendResp(ST_OK, respBuf, off);
}

static void cslReplyRA() {
  respBuf[0] = txn.sent;
  sendResp(cslWireStatus(txn.outcome), respBuf, 1);
}

// Park a queued outcome in the completion ring. Returns false while the ring is
// full, in which case the transaction keeps its outcome until a poll drains one.
static bool cslPark() {
  if (cslCompleteCount == CSL_COMPLETE_DEPTH) return false;
  CslCompletion *done = &cslComplete[cslCompleteTail];
  memset(done, 0, sizeof(*done));
  done->id = txn.job.id;
  done->flags = txn.job.flags;
  done->status = cslWireStatus(txn.outcome);
  done->sent = txn.sent;
  done->ackLen = txn.ackLen;
  if (done->ackLen) memcpy(done->ack, txn.ack, done->ackLen);
  done->follow = txn.follow;
  cslCompleteTail = (uint8_t)((cslCompleteTail + 1) % CSL_COMPLETE_DEPTH);
  cslCompleteCount++;
  return true;
}

// Deliver a decided outcome. A parked OK never turns into a failure later: the
// state machine looks at the outcome before it looks at the transaction guard.
static void cslDeliver() {
  spiUseFast(false);
  if (txn.job.kind == JOB_QUEUED) {
    if (!cslPark()) return;
  } else if (txn.job.kind == JOB_SYNC_CSL) {
    cslReplySync();
  } else {
    cslReplyRA();
  }
  memset(&txn, 0, sizeof(txn));
  ccArmRx();
  rxReset();
}

static void cslCompleteNow(uint8_t outcome) {
  cslSetOutcome(outcome);
  cslDeliver();
}

static bool cslSendWake(const CslJob *job, uint16_t rendez) {
  uint8_t dst0 = job->shortAddr[0];
  uint8_t dst1 = job->shortAddr[1];
  if (job->kind == JOB_RA) {  // the Router Advertisement wakes every neighbour
    dst0 = 0xFF;
    dst1 = 0xFF;
  }
  uint8_t wake[12] = {0x25, job->data[2], 0xCD, 0xAB, dst0, dst1, 0x82, 0x0E,
                      (uint8_t)rendez, (uint8_t)(rendez >> 8), 0x80, 0x3F};
  spiUseFast(true);
  bool ok = ccSendFrame(wake, sizeof(wake));
  spiUseFast(job->kind == JOB_RA);
  return ok;
}

static void cslSendData() {
  if (txn.sent == 0 || !cslSendWake(&txn.job, 0)) {
    cslCompleteNow(OUT_TXFAIL);
    return;
  }
  // ccSendFrame clears what the wake burst left in the RX FIFO before the Data
  // frame goes out, so the Enhanced ACK lands in an empty FIFO.
  spiUseFast(txn.job.kind == JOB_RA);
  bool ok = ccSendFrame(txn.job.data, txn.job.dataLen);
  spiUseFast(false);
  if (!ok) {
    cslCompleteNow(OUT_TXFAIL);
    return;
  }
  txn.sent += 2;
  if (txn.job.kind == JOB_RA) {
    cslCompleteNow(OUT_OK);
    return;
  }
  rxReset();
  txn.windowEndUs = nowUs() + (uint64_t)txn.job.ackTmoMs * 1000ULL;
  txn.state = CSL_ACK;
}

// Take an uplink that lands in the window before the burst. A Router
// Advertisement carries no peer address, so it never answers one.
static bool cslDrainWindow(uint64_t now) {
  if (txn.job.kind == JOB_RA) return false;
  if (!rxDuePump(now) || !rxPump()) return false;
  uint8_t raw[MAX_RX_RAW];
  size_t rawLen = 0;
  uint8_t freqest = 0;
  rxTake(raw, &rawLen, nullptr, &freqest);
  cslStoreInboundForJob(raw, rawLen, &txn.job, freqest);
  return true;
}

static void cslStepRx(uint64_t now) {
  if (now >= txn.wakeEndUs) {
    cslSendData();
    return;
  }
  if (cslDrainWindow(now)) return;
  if (now >= txn.startUs) txn.state = CSL_WAKE;
}

static void cslStepWake(uint64_t now) {
  // The wake window closes on time even with an uplink waiting in the FIFO:
  // the Data frame has to land on the peer's sample.
  if (now >= txn.wakeEndUs) {
    cslSendData();
    return;
  }
  if (cslDrainWindow(now)) return;
  uint8_t cap = (txn.job.kind == JOB_RA) ? RA_WAKE_CAP : CSL_WAKE_CAP;
  for (uint8_t i = 0; i < CSL_WAKE_BUDGET && txn.sent < cap; i++) {
    int64_t delta = (int64_t)(txn.wakeEndUs - nowUs());
    if (delta < (int64_t)CSL_UNIT_US) break;
    uint32_t rendez = (uint32_t)delta / CSL_UNIT_US;
    if (txn.job.kind == JOB_RA) {
      // The first burst frame always announces a whole period, the rest count down.
      if (txn.sent == 0) rendez = RA_PERIOD_UNITS;
      else if (rendez > RA_PERIOD_UNITS) rendez = RA_PERIOD_UNITS;
    }
    if (!cslSendWake(&txn.job, (uint16_t)rendez)) {
      cslCompleteNow(OUT_TXFAIL);
      return;
    }
    txn.sent++;
  }
  if (nowUs() < txn.wakeEndUs) return;
  cslSendData();
}

static void cslStepAck(uint64_t now) {
  if (now >= txn.windowEndUs) {
    anchorMiss(txn.job.shortAddr);
    cslCompleteNow(txn.sawInvalid ? OUT_BADACK : OUT_NOACK);
    return;
  }
  if (!rxDuePump(now) || !rxPump()) return;
  uint8_t raw[MAX_RX_RAW];
  size_t rawLen = 0;
  uint64_t stampUs = 0;
  uint8_t freqest = 0;
  rxTake(raw, &rawLen, &stampUs, &freqest);
  if (!cslAckValid(raw, rawLen, txn.job.data)) {
    // Only a frame that can be a damaged Enhanced ACK turns NOACK into BADACK:
    // a CRC failure or the Enhanced ACK frame control. An uplink of another
    // node says nothing about this Data frame.
    if (!rxCrcOk(raw, rawLen) || (raw[1] == 0x42 && raw[2] == 0xEE)) txn.sawInvalid = true;
    // No auto-ACK in this window: its TX would cover the Enhanced ACK of the
    // target. The other node retransmits an uplink it sees unacknowledged.
    cslStoreInbound(raw, rawLen, nullptr, 0, freqest);
    return;
  }
  memcpy(txn.ack, raw, rawLen);
  txn.ackLen = (uint8_t)rawLen;
  // A GDO0 edge from before the Data frame is not the edge of this ACK: an
  // anchor placed on it misses by up to a period.
  if (stampUs < txn.windowEndUs - (uint64_t)txn.job.ackTmoMs * 1000ULL) stampUs = nowUs();
  anchorStore(txn.job.shortAddr, raw + 1, stampUs);
  if ((txn.job.flags & CSL_FLAG_FOLLOW) == 0) {
    cslCompleteNow(OUT_OK);
    return;
  }
  txn.windowEndUs = nowUs() + (uint64_t)CSL_FOLLOW_TMO_MS * 1000ULL;
  txn.state = CSL_FOLLOW;
}

static void cslStepFollow(uint64_t now) {
  if (now >= txn.windowEndUs) {
    cslCompleteNow(OUT_OK);
    return;
  }
  if (!rxDuePump(now) || !rxPump()) return;
  uint8_t raw[MAX_RX_RAW];
  size_t rawLen = 0;
  uint8_t freqest = 0;
  rxTake(raw, &rawLen, nullptr, &freqest);
  if (!rxCrcOk(raw, rawLen)) {  // a corrupted frame must not take a follow slot
    if (txn.follow.dropped != 0xFF) txn.follow.dropped++;
    return;
  }
  bool match = cslFollowMatch(raw, rawLen, txn.job.data);
  // Hold one slot for the addressed peer until its own answer arrives.
  bool keep = match ? txn.follow.count < CSL_FOLLOW_MAX
                    : txn.follow.count < (txn.targetStored ? CSL_FOLLOW_MAX : CSL_FOLLOW_MAX - 1);
  if (keep) {
    memcpy(txn.follow.raw[txn.follow.count], raw, rawLen);
    txn.follow.len[txn.follow.count++] = (uint8_t)rawLen;
    if (match) txn.targetStored = true;
    uint8_t coordinator[8];
    coordFromData(txn.job.data, coordinator);
    ccAutoAck(raw, rawLen, coordinator, txn.job.phase);
    return;
  }
  if (txn.follow.dropped != 0xFF) txn.follow.dropped++;
  cslStoreInboundForJob(raw, rawLen, &txn.job, freqest);
}

// Set up the timing of one job and open its receive window.
static void cslStart(const CslJob *job) {
  memset(&txn, 0, sizeof(txn));
  txn.active = true;
  txn.job = *job;
  uint64_t now = nowUs();
  uint64_t followUs = (job->flags & CSL_FLAG_FOLLOW) ? (uint64_t)CSL_FOLLOW_TMO_MS * 1000ULL : 0;
  if (job->kind == JOB_RA) {
    txn.startUs = now;
    txn.wakeEndUs = now + RA_PERIOD_US;
    spiUseFast(true);
  } else {
    int8_t idx = anchorForJob(job);
    if (idx >= 0) {
      uint32_t guardUs = 0;
      uint64_t sample = anchorNextSample(idx, now, &guardUs);
      txn.startUs = sample - guardUs;
      txn.wakeEndUs = sample + guardUs;
    } else {
      txn.startUs = now;
      txn.wakeEndUs = now + CSL_BOOTSTRAP_US;
    }
    ccArmRx();
    rxReset();
  }
  // The guard follows the parameters of this job, so a legitimate worst case
  // never trips it: whole wake window, ACK timeout, follow window and margin.
  txn.guardUs = txn.wakeEndUs + (uint64_t)job->ackTmoMs * 1000ULL + followUs +
                (uint64_t)CSL_GUARD_MARGIN_MS * 1000ULL;
  txn.state = (now < txn.startUs) ? CSL_RX : CSL_WAKE;
}

static bool cslCanSchedule() {
  return !txn.active && !exclusiveWaiting && cslCompleteCount < CSL_COMPLETE_DEPTH;
}

// Pick the neighbour whose wake window opens first, round-robin on a tie.
static bool cslScheduleNext() {
  CslNeighbor *best = nullptr;
  uint64_t bestStart = 0;
  uint64_t now = nowUs();
  for (uint8_t step = 0; step < CSL_NEIGHBORS; step++) {
    uint8_t i = (uint8_t)((cslCursor + step) % CSL_NEIGHBORS);
    CslNeighbor *e = &cslNeighbors[i];
    if (!e->used || e->count == 0) continue;
    CslJob *job = &cslJobs[e->head];
    int8_t idx = anchorForJob(job);
    uint64_t start = now;
    if (idx >= 0) {
      uint32_t guardUs = 0;
      start = anchorNextSample(idx, now, &guardUs) - guardUs;
    } else if ((job->flags & CSL_FLAG_BOOTSTRAP) == 0) {
      memset(&txn, 0, sizeof(txn));
      txn.active = true;
      txn.job = *job;
      cslJobRelease(cslNeighborPop(e));
      cslCursor = (uint8_t)((i + 1) % CSL_NEIGHBORS);
      cslCompleteNow(OUT_NOANCHOR);
      return true;
    }
    if (best == nullptr || start < bestStart) {
      best = e;
      bestStart = start;
    }
  }
  if (best == nullptr) return false;
  uint8_t jobIdx = cslNeighborPop(best);
  cslCursor = (uint8_t)(((best - cslNeighbors) + 1) % CSL_NEIGHBORS);
  cslStart(&cslJobs[jobIdx]);
  cslJobRelease(jobIdx);
  return true;
}

static void cslStep() {
  if (!txn.active) {
    if (cslCanSchedule()) cslScheduleNext();
    return;
  }
  if (txn.outcomeReady) {  // outcome first, the guard must never rewrite it
    cslDeliver();
    return;
  }
  uint64_t now = nowUs();
  if (now >= txn.guardUs) {
    cslCompleteNow(OUT_TXFAIL);
    return;
  }
  switch (txn.state) {
    case CSL_RX: cslStepRx(now); break;
    case CSL_WAKE: cslStepWake(now); break;
    case CSL_ACK: cslStepAck(now); break;
    default: cslStepFollow(now); break;
  }
}

// ------------------------------------------------------------------- idle drain

// Between transactions the chip stays in RX because MCSM1 has TXOFF_MODE=RX, so
// frames pile up in the 64-byte FIFO until it overflows. Drain them on the GDO
// interrupt. While the host transmits, answer and store them for its next
// CSLQUEUE poll, otherwise throw them away.
static void idleRxService() {
  uint64_t now = nowUs();
  if (now < hostSpiHoldoffUs) return;  // the host is driving the radio itself
  if (!rxDuePump(now)) return;
  if (!rxPump()) return;
  uint8_t raw[MAX_RX_RAW];
  size_t rawLen = 0;
  uint8_t freqest = 0;
  rxTake(raw, &rawLen, nullptr, &freqest);
  if (!rxCrcOk(raw, rawLen)) {
    ccArmRx();  // a bad CRC leaves the receiver out of step with the air
    return;
  }
  if (!coordKnown) return;
  pairAnswer(raw, rawLen);
  cslStoreInbound(raw, rawLen, coordEUI, coordPhase, freqest);
}

// ---------------------------------------------------------------- stream window

// Continuous RX for durMs: keep the radio armed, drain the FIFO on-chip and push
// every complete frame to the host as it arrives. RX is re-armed only on FIFO
// overflow, so back-to-back frames such as a CSL wake-up burst are all captured.
//
// With matchLen or respCount set it also acts as the auto-responder: a frame
// whose payload matches `match` at matchOff triggers the packed response train
// before the frame is reported. matchOff counts from the first payload byte, the
// same bytes the host sees. matchLen 0 responds to every frame. The match does
// not check the CRC, so a corrupted frame can trigger the train.
static void streamWindow(uint32_t durMs, uint8_t matchOff, const uint8_t *match, uint8_t matchLen,
                         const uint8_t *resp, size_t respLen, uint8_t respCount) {
  uint32_t baseSeq = hostByteSeq.load(std::memory_order_relaxed);
  coordKnown = false;
  ccArmRx();
  rxReset();
  uint64_t now = nowUs();
  uint64_t end = now + (uint64_t)durMs * 1000ULL;
  uint64_t lastTx = now;
  while ((now = nowUs()) < end) {
    esp_task_wdt_reset();
    if (hostByteSeq.load(std::memory_order_relaxed) != baseSeq) break;
    if (now - lastTx >= (uint64_t)STREAM_HEARTBEAT_MS * 1000ULL) {
      sendStreamMarker(STREAM_ALIVE);
      lastTx = nowUs();
      continue;
    }
    if (rxDuePump(now) && rxPump()) {
      uint8_t raw[MAX_RX_RAW];
      size_t rawLen = 0;
      uint8_t freqest = 0;
      rxTake(raw, &rawLen, nullptr, &freqest);
      uint8_t pktLen = raw[0];
      // Respond before reporting: the peer's RX window closes long before a host
      // round-trip could answer. TX drops out of RX, so re-arm afterwards.
      if (respCount && (matchLen == 0 || ((size_t)matchOff + matchLen <= (size_t)pktLen &&
                                          memcmp(raw + 1 + matchOff, match, matchLen) == 0))) {
        ccSendTrain(resp, respLen, respCount);
        ccArmRx();
        rxReset();
      }
      sendStreamFrame(raw, (uint8_t)rawLen, freqest);
      lastTx = nowUs();
      continue;
    }
    uint64_t left = (end - now) / 1000ULL;
    uint32_t tickMs = rxAssembling() ? RX_ASSEMBLE_TICK_MS : RX_HOUSEKEEP_TICK_MS;
    if (left < tickMs) tickMs = (uint32_t)left;
    radioWait(tickMs);
  }
  ccStrobe(CC_SIDLE);
  sendStreamMarker(STREAM_END);
}

// -------------------------------------------------------------- CSLTX requests

// Shape of one CSLTX record: ackTmoHi ackTmoLo flags dst0 dst1 [phaseLo phaseHi]
// followed by the Data frame. The phase pair is present only with the follow flag.
static bool cslParseTx(const uint8_t *tx, size_t txLen, CslJob *job, uint16_t recordPhase) {
  if (txLen < 21) return false;
  uint16_t ackTmoMs = ((uint16_t)tx[0] << 8) | tx[1];
  uint8_t flags = tx[2];
  bool waitFollow = (flags & CSL_FLAG_FOLLOW) != 0;
  size_t off = waitFollow ? 7 : 5;
  if ((flags & ~(CSL_FLAG_BOOTSTRAP | CSL_FLAG_FOLLOW)) != 0) return false;
  if (txLen < off + 16) return false;
  const uint8_t *data = tx + off;
  size_t dataLen = txLen - off;
  if (ackTmoMs == 0 || ackTmoMs > CSL_ACK_TMO_MAX_MS || dataLen > MAX_TX_FRAME) return false;
  if (!((data[0] == 0x69 || data[0] == 0x79) && data[1] == 0xEC)) return false;
  if (tx[3] != data[4] || tx[4] != data[3]) return false;
  job->ackTmoMs = ackTmoMs;
  job->flags = flags;
  job->shortAddr[0] = tx[3];
  job->shortAddr[1] = tx[4];
  job->phase = recordPhase;
  job->dataLen = (uint8_t)dataLen;
  memcpy(job->data, data, dataLen);
  return true;
}

static void cmdCSLTx(const uint8_t *p, uint16_t len) {
  uint8_t sent = 0;
  CslJob job;
  memset(&job, 0, sizeof(job));
  job.kind = JOB_SYNC_CSL;
  // Only a follow request carries the coordinator phase, otherwise it stays 0.
  uint16_t phase = 0;
  if (len >= 7 && (p[2] & CSL_FLAG_FOLLOW)) phase = (uint16_t)p[5] | ((uint16_t)p[6] << 8);
  if (!cslParseTx(p, len, &job, phase)) {
    sendResp(ST_INPUT, &sent, 1);
    return;
  }
  uint8_t coordinator[8];
  coordFromData(job.data, coordinator);
  coordRemember(coordinator, job.phase);
  if (anchorIndex(job.shortAddr) < 0 && (job.flags & CSL_FLAG_BOOTSTRAP) == 0) {
    sendResp(ST_NOANCHOR, &sent, 1);
    return;
  }
  cslStart(&job);
}

// A pair window sends the advertisement twice, the first copy with Frame
// Pending (0x59E8).
static void cmdRABurst(const uint8_t *p, uint16_t len) {
  uint8_t sent = 0;
  if (len < 3 || len > MAX_TX_FRAME || (p[0] != 0x49 && p[0] != 0x59) || p[1] != 0xE8) {
    sendResp(ST_INPUT, &sent, 1);
    return;
  }
  CslJob job;
  memset(&job, 0, sizeof(job));
  job.kind = JOB_RA;
  job.dataLen = (uint8_t)len;
  memcpy(job.data, p, len);
  cslStart(&job);
}

// Request: durHi durLo seconds, then for a window that opens key[16]
// coordinator[8] seq counter[4 LE] count. A zero duration closes the window.
// The reply carries the beacons sent in the window the request ends.
static void cmdPairWin(const uint8_t *p, uint16_t len) {
  uint8_t sent = pairWin.sent;
  uint16_t durS = (len >= 2) ? (uint16_t)(((uint16_t)p[0] << 8) | p[1]) : 0;
  if (len == 2 && durS == 0) {
    pairClose();
    sendResp(ST_OK, &sent, 1);
    return;
  }
  if (len != PAIRWIN_OPEN_LEN || durS == 0 || durS > PAIRWIN_MAX_S || p[31] == 0) {
    sendResp(ST_INPUT, &sent, 1);
    return;
  }
  pairClose();
  mbedtls_aes_setkey_enc(&pairWin.aes, p + 2, 128);
  memcpy(pairWin.coord, p + 18, 8);
  pairWin.seq = p[26];
  pairWin.ctr = (uint32_t)p[27] | ((uint32_t)p[28] << 8) | ((uint32_t)p[29] << 16) | ((uint32_t)p[30] << 24);
  pairWin.left = p[31];
  pairWin.endUs = nowUs() + (uint64_t)durS * 1000000ULL;
  pairWin.active = true;
  sendResp(ST_OK, &sent, 1);
}

// -------------------------------------------------------------- CSLQUEUE command

static void cmdQueueEnqueue(const uint8_t *p, uint16_t len) {
  if (len < 2 || p[1] == 0 || p[1] > CSL_NEIGHBOR_DEPTH) {
    sendResp(ST_INPUT, nullptr, 0);
    return;
  }
  uint8_t count = p[1];
  size_t pos = 2;
  uint8_t shortAddr[2] = {0, 0};
  CslJob probe;
  for (uint8_t i = 0; i < count; i++) {
    if (pos + 6 > len) {
      sendResp(ST_INPUT, nullptr, 0);
      return;
    }
    size_t txLen = ((size_t)p[pos + 2] << 8) | p[pos + 3];
    if (pos + 6 + txLen > len) {
      sendResp(ST_INPUT, nullptr, 0);
      return;
    }
    uint16_t recordPhase = (uint16_t)p[pos + 4] | ((uint16_t)p[pos + 5] << 8);
    memset(&probe, 0, sizeof(probe));
    if (!cslParseTx(p + pos + 6, txLen, &probe, recordPhase)) {
      sendResp(ST_INPUT, nullptr, 0);
      return;
    }
    if (i == 0) {
      shortAddr[0] = probe.shortAddr[0];
      shortAddr[1] = probe.shortAddr[1];
    } else if (shortAddr[0] != probe.shortAddr[0] || shortAddr[1] != probe.shortAddr[1]) {
      sendResp(ST_INPUT, nullptr, 0);
      return;
    }
    pos += 6 + txLen;
  }
  if (pos != len) {
    sendResp(ST_INPUT, nullptr, 0);
    return;
  }
  CslNeighbor *entry = cslNeighborFind(shortAddr, false);
  if (entry == nullptr) entry = cslNeighborFind(shortAddr, true);
  if (entry == nullptr) {
    sendResp(ST_NEIGHBOR_FULL, nullptr, 0);
    return;
  }
  if ((uint16_t)entry->count + count > CSL_NEIGHBOR_DEPTH || cslJobFreeCount < count) {
    if (entry->count == 0) entry->used = false;
    sendResp(ST_QUEUE_FULL, nullptr, 0);
    return;
  }
  pos = 2;
  for (uint8_t i = 0; i < count; i++) {
    size_t txLen = ((size_t)p[pos + 2] << 8) | p[pos + 3];
    uint16_t recordPhase = (uint16_t)p[pos + 4] | ((uint16_t)p[pos + 5] << 8);
    uint8_t idx = cslJobAlloc();
    CslJob *job = &cslJobs[idx];
    memset(job, 0, sizeof(*job));
    job->kind = JOB_QUEUED;
    job->id = ((uint16_t)p[pos] << 8) | p[pos + 1];
    cslParseTx(p + pos + 6, txLen, job, recordPhase);
    cslNeighborPush(entry, idx);
    pos += 6 + txLen;
  }
  uint8_t coordinator[8];
  coordFromData(cslJobs[entry->tail].data, coordinator);
  coordRemember(coordinator, cslJobs[entry->tail].phase);
  sendResp(ST_OK, &count, 1);
}

static void cmdQueuePoll() {
  if (cslCompleteCount != 0) {
    CslCompletion *done = &cslComplete[cslCompleteHead];
    size_t out = 0;
    respBuf[out++] = 0;
    respBuf[out++] = (uint8_t)(done->id >> 8);
    respBuf[out++] = (uint8_t)done->id;
    respBuf[out++] = done->flags;
    respBuf[out++] = done->sent;
    if (done->status == ST_OK) {
      if ((done->flags & CSL_FLAG_FOLLOW) == 0) {
        memcpy(respBuf + out, done->ack, done->ackLen);
        out += done->ackLen;
      } else {
        out = cslAppendFollowBody(respBuf, out, done->ack, done->ackLen, &done->follow);
      }
    }
    uint8_t status = done->status;
    cslCompleteHead = (uint8_t)((cslCompleteHead + 1) % CSL_COMPLETE_DEPTH);
    cslCompleteCount--;
    sendResp(status, respBuf, out);
    return;
  }
  if (cslInboundCount != 0) {
    CslInbound *inbound = &cslInbound[cslInboundHead];
    respBuf[0] = 1;
    memcpy(respBuf + 1, inbound->raw, inbound->len);
    uint8_t n = inbound->len;
    respBuf[1 + n] = inbound->freqest;
    cslInboundHead = (uint8_t)((cslInboundHead + 1) % CSL_INBOUND_DEPTH);
    cslInboundCount--;
    sendResp(ST_OK, respBuf, (size_t)n + 2);
    return;
  }
  sendResp(ST_OK, nullptr, 0);
}

static void cmdQueue(const uint8_t *p, uint16_t len) {
  if (len < 1) {
    sendResp(ST_INPUT, nullptr, 0);
    return;
  }
  if (p[0] == CSLQ_ENQUEUE) {
    cmdQueueEnqueue(p, len);
    return;
  }
  if (p[0] == CSLQ_POLL) {
    if (len != 1) sendResp(ST_INPUT, nullptr, 0);
    else cmdQueuePoll();
    return;
  }
  if (p[0] == CSLQ_FLUSH) {
    if (len != 1) {
      sendResp(ST_INPUT, nullptr, 0);
      return;
    }
    cslQueueClear();
    coordKnown = false;
    pairClose();
    if (!txn.active) {  // a job that still owes a reply keeps the receiver
      ccArmRx();
      rxReset();
    }
    sendResp(ST_OK, nullptr, 0);
    return;
  }
  sendResp(ST_INPUT, nullptr, 0);
}

// ------------------------------------------------------------ command execution

// Only PING, GDO and CSLQUEUE run while a transaction is in flight. Everything
// else waits for the state machine, so no host strobe lands mid transaction.
static bool cmdNeedsRadio(uint8_t cmd) {
  return cmd != CMD_PING && cmd != CMD_GDO && cmd != CMD_CSLQUEUE;
}

static void execCommand(CmdMsg *msg) {
  uint8_t *p = msg->payload;
  uint16_t len = msg->len;
  switch (msg->cmd) {
    case CMD_PING: {
      const char *s = BANNER;
      sendResp(ST_OK, (const uint8_t *)s, strlen(s));
      break;
    }
    case CMD_XFER: {
      hostSpiHoldoffUs = nowUs() + (uint64_t)HOST_SPI_HOLDOFF_MS * 1000ULL;
      bool ready = spiXfer(p, len);
      sendResp(ready ? ST_OK : ST_NOT_READY, p, len);
      break;
    }
    case CMD_STROBE: {
      if (len < 1) {
        sendResp(ST_BAD_ARGS, nullptr, 0);
        break;
      }
      hostSpiHoldoffUs = nowUs() + (uint64_t)HOST_SPI_HOLDOFF_MS * 1000ULL;
      spiXfer(p, 1);
      sendResp(ST_OK, p, 1);
      break;
    }
    case CMD_GDO: {
      uint8_t g = (digitalRead(PIN_GDO0) ? 1 : 0) | (digitalRead(PIN_GDO2) ? 2 : 0);
      sendResp(ST_OK, &g, 1);
      break;
    }
    case CMD_RESET: {
      hostSpiHoldoffUs = nowUs() + (uint64_t)HOST_SPI_HOLDOFF_MS * 1000ULL;
      coordKnown = false;
      pairClose();
      // The host loads its register set next. A job a crashed session left in
      // the queue would transmit on a chip that is half configured.
      cslQueueClear();
      manualReset();
      rxReset();
      sendResp(ST_OK, nullptr, 0);
      break;
    }
    case CMD_RECV: {
      uint32_t tmo = (len >= 2) ? (((uint32_t)p[0] << 8) | p[1]) : 2000;
      coordKnown = false;
      ccArmRx();
      rxReset();
      uint8_t pkt[MAX_RX_RAW + 1];
      size_t plen = 0;
      uint8_t freqest = 0;
      bool got = rxWaitFrame(tmo, pkt, &plen, nullptr, &freqest);
      // The radio stays in RX (MCSM1 RXOFF_MODE=RX) and the idle drain takes
      // over the FIFO and any frame that already started.
      if (!got) {
        sendResp(ST_NO_FRAME, nullptr, 0);
        break;
      }
      pkt[plen++] = freqest;
      sendResp(ST_OK, pkt, plen);
      break;
    }
    case CMD_AUTOACK: {
      if (len != 12) {
        sendResp(ST_BAD_ARGS, nullptr, 0);
        break;
      }
      uint32_t tmo = ((uint32_t)p[0] << 8) | p[1];
      uint16_t phase = (uint16_t)p[10] | ((uint16_t)p[11] << 8);
      coordRemember(p + 2, phase);
      ccArmRx();
      rxReset();
      uint8_t pkt[MAX_RX_RAW + 1];
      size_t plen = 0;
      uint8_t freqest = 0;
      bool got = rxWaitFrame(tmo, pkt, &plen, nullptr, &freqest);
      // The radio stays in RX: RXOFF_MODE=RX after a frame, TXOFF_MODE=RX after
      // the Enhanced ACK, and a failed ACK re-arms RX. Until the next host
      // window the idle drain answers and stores the uplinks addressed to the
      // coordinator this request set, a following fragment included.
      if (!got) {
        sendResp(ST_NO_FRAME, nullptr, 0);
        break;
      }
      uint8_t outcome = ccAutoAck(pkt, plen, p + 2, phase);
      if (outcome == AUTOACK_NONE) pairAnswer(pkt, plen);
      pkt[plen++] = freqest;
      if (outcome == AUTOACK_SENT) sendResp(ST_OK, pkt, plen);
      else if (outcome == AUTOACK_FAILED) sendResp(ST_ACK_FAILED, pkt, plen);
      else sendResp(ST_ACK_SKIPPED, pkt, plen);
      break;
    }
    case CMD_STREAM: {
      // Streams frames directly (5A F1 ...) then an end marker (5A F2 00), with
      // no response header. The host reads this window with its stream reader.
      uint32_t dur = (len >= 2) ? (((uint32_t)p[0] << 8) | p[1]) : STREAM_DEFAULT_MS;
      streamWindow(dur, 0, nullptr, 0, nullptr, 0, 0);
      break;
    }
    case CMD_RESPOND: {
      // Request: durHi durLo matchOff matchLen match[matchLen] respCount, then
      // respCount sub-frames [len:1][bytes:len]. Output is the CMD_STREAM stream.
      if (len < 5) {
        sendResp(ST_BAD_ARGS, nullptr, 0);
        break;
      }
      uint32_t dur = ((uint32_t)p[0] << 8) | p[1];
      uint8_t matchOff = p[2];
      uint8_t matchLen = p[3];
      if (matchLen > MAX_MATCH || (size_t)5 + matchLen > len) {
        sendResp(ST_BAD_ARGS, nullptr, 0);
        break;
      }
      uint8_t respCount = p[4 + matchLen];
      if (respCount > MAX_TRAIN) {
        sendResp(ST_BAD_ARGS, nullptr, 0);
        break;
      }
      streamWindow(dur, matchOff, p + 4, matchLen, p + 5 + matchLen,
                   len - (5 + (size_t)matchLen), respCount);
      break;
    }
    case CMD_TXTRAIN: {
      // Request: count[1] then count sub-frames [len:1][bytes:len]. Each one is
      // a complete on-air frame. Response payload is the frames actually sent.
      if (len < 1) {
        sendResp(ST_BAD_ARGS, nullptr, 0);
        break;
      }
      uint8_t sent = ccSendTrain(p + 1, len - 1, p[0]);
      sendResp(ST_OK, &sent, 1);
      break;
    }
    case CMD_RABURST: cmdRABurst(p, len); break;
    case CMD_CSLTX: cmdCSLTx(p, len); break;
    case CMD_CSLQUEUE: cmdQueue(p, len); break;
    case CMD_PAIRWIN: cmdPairWin(p, len); break;
    default: sendResp(ST_BAD_CMD, nullptr, 0); break;
  }
}

// ------------------------------------------------------------------- radio task

static void radioWait(uint32_t ms) {
  uint32_t bits = 0;
  xTaskNotifyWait(0, NOTIFY_ALL_BITS, &bits, pdMS_TO_TICKS(ms));
}

static uint32_t radioWaitMs() {
  if (txn.active) {
    // The outcome is decided but the completion ring is full: wait for a poll.
    if (txn.outcomeReady) return RADIO_IDLE_TICK_MS;
    if (txn.state == CSL_WAKE) return 0;
    uint64_t next = (txn.state == CSL_RX) ? txn.startUs : txn.windowEndUs;
    uint64_t now = nowUs();
    uint32_t cap = RADIO_IDLE_TICK_MS;
    if (next > now) {
      uint64_t left = (next - now) / 1000ULL;
      if (left < cap) cap = (uint32_t)left;
    } else {
      cap = 0;
    }
    if (rxAssembling() && cap > RX_ASSEMBLE_TICK_MS) cap = RX_ASSEMBLE_TICK_MS;
    return cap;
  }
  if (cslCanSchedule()) {
    for (uint8_t i = 0; i < CSL_NEIGHBORS; i++) {
      if (cslNeighbors[i].used && cslNeighbors[i].count != 0) return 0;
    }
  }
  return rxAssembling() ? RX_ASSEMBLE_TICK_MS : RADIO_IDLE_TICK_MS;
}

static void radioTask(void *) {
  esp_task_wdt_add(nullptr);
  rxReset();  // the chip stays where the reset left it until the host sets it up
  for (;;) {
    esp_task_wdt_reset();
    CmdMsg *msg = nullptr;
    bool have = xQueuePeek(qCmd, &msg, 0) == pdTRUE;
    exclusiveWaiting = have && cmdNeedsRadio(msg->cmd);
    cslStep();
    if (have && (!exclusiveWaiting || !txn.active)) {
      xQueueReceive(qCmd, &msg, 0);
      exclusiveWaiting = false;
      execCommand(msg);
      xQueueSend(qCmdFree, &msg, portMAX_DELAY);
      continue;
    }
    if (!txn.active) idleRxService();
    radioWait(radioWaitMs());
  }
}

// ------------------------------------------------------------------ serial task

// A host that stops reading fills the 2048-byte TX buffer, and a blocking
// Serial.write there would let the 2 s task watchdog reboot the bridge.
static void serialWriteAll(const uint8_t *p, size_t n) {
  size_t off = 0;
  while (off < n) {
    esp_task_wdt_reset();
    int room = Serial.availableForWrite();
    if (room <= 0) {
      vTaskDelay(1);
      continue;
    }
    size_t chunk = n - off;
    if (chunk > (size_t)room) chunk = (size_t)room;
    Serial.write(p + off, chunk);
    off += chunk;
  }
}

static void serialDrainOut() {
  OutMsg *m = nullptr;
  while (xQueueReceive(qOut, &m, 0) == pdTRUE) {
    serialWriteAll(m->data, m->len);
    xQueueSend(qOutFree, &m, portMAX_DELAY);
  }
}

// Parser errors answer from the serial task itself. Taking an out buffer here
// would deadlock: the serial task is the only consumer that frees one.
static void serialReject(uint8_t status) {
  serialDrainOut();
  uint8_t hdr[4] = {RSP_MAGIC, status, 0x00, 0x00};
  serialWriteAll(hdr, 4);
}

// Read one byte, servicing the outbound queue while the host is quiet. Returns
// -1 once the deadline passes.
static int serialReadByte(uint64_t deadlineUs) {
  static uint64_t lastNotifyUs = 0;
  for (;;) {
    int c = Serial.read();
    if (c >= 0) {
      hostByteSeq.fetch_add(1, std::memory_order_relaxed);
      // The radio task only needs to know the host spoke, so one wake per
      // millisecond is enough and a 1 kByte payload does not flood it.
      uint64_t t = nowUs();
      if (radioTaskHandle != nullptr && t - lastNotifyUs >= 1000) {
        lastNotifyUs = t;
        xTaskNotify(radioTaskHandle, NOTIFY_RADIO, eSetBits);
      }
      return c;
    }
    serialDrainOut();
    if (nowUs() >= deadlineUs) return -1;
    esp_task_wdt_reset();
    OutMsg *m = nullptr;
    if (xQueueReceive(qOut, &m, 1) == pdTRUE) {
      serialWriteAll(m->data, m->len);
      xQueueSend(qOutFree, &m, portMAX_DELAY);
    }
  }
}

// Read n bytes with an inter-byte timeout and a total budget for the request.
static bool serialReadN(uint8_t *p, size_t n, uint64_t budgetUs) {
  uint64_t total = nowUs() + budgetUs;
  for (size_t i = 0; i < n; i++) {
    uint64_t byteDeadline = nowUs() + (uint64_t)SERIAL_INTERBYTE_MS * 1000ULL;
    if (byteDeadline > total) byteDeadline = total;
    int c = serialReadByte(byteDeadline);
    if (c < 0) return false;
    p[i] = (uint8_t)c;
  }
  return true;
}

static CmdMsg *cmdAcquire() {
  CmdMsg *m = nullptr;
  while (xQueueReceive(qCmdFree, &m, 0) != pdTRUE) {
    serialDrainOut();
    esp_task_wdt_reset();
    vTaskDelay(1);
  }
  return m;
}

static void serialTask(void *) {
  esp_task_wdt_add(nullptr);
  for (;;) {
    esp_task_wdt_reset();
    serialDrainOut();
    int c = serialReadByte(nowUs() + 10000ULL);
    if (c < 0) continue;
    if ((uint8_t)c != REQ_MAGIC) continue;  // resync: drop stray bytes until magic

    uint8_t head[3];
    if (!serialReadN(head, 3, (uint64_t)SERIAL_REQUEST_MS * 1000ULL)) continue;
    size_t len = ((size_t)head[1] << 8) | head[2];
    if (len > MAX_PAYLOAD) {
      serialReject(ST_BAD_LEN);
      continue;
    }
    CmdMsg *msg = cmdAcquire();
    msg->cmd = head[0];
    msg->len = (uint16_t)len;
    if (len && !serialReadN(msg->payload, len, (uint64_t)SERIAL_REQUEST_MS * 1000ULL)) {
      xQueueSend(qCmdFree, &msg, portMAX_DELAY);
      serialReject(ST_BAD_PAYLOAD);
      continue;
    }
    xQueueSend(qCmd, &msg, portMAX_DELAY);
    if (radioTaskHandle != nullptr) xTaskNotify(radioTaskHandle, NOTIFY_RADIO, eSetBits);
  }
}

// ------------------------------------------------------------------------ setup

static void queuesInit() {
  qCmd = xQueueCreate(CMD_POOL, sizeof(CmdMsg *));
  qCmdFree = xQueueCreate(CMD_POOL, sizeof(CmdMsg *));
  qOut = xQueueCreate(OUT_POOL, sizeof(OutMsg *));
  qOutFree = xQueueCreate(OUT_POOL, sizeof(OutMsg *));
  for (uint8_t i = 0; i < CMD_POOL; i++) {
    CmdMsg *m = &cmdPool[i];
    xQueueSend(qCmdFree, &m, 0);
  }
  for (uint8_t i = 0; i < OUT_POOL; i++) {
    OutMsg *m = &outPool[i];
    xQueueSend(qOutFree, &m, 0);
  }
}

static void wdtInit() {
  // idle_core_mask 0: the radio task busy-waits on core 1 during a transaction,
  // so only the two sketch tasks are watched.
  esp_task_wdt_config_t cfg = {};
  cfg.timeout_ms = WDT_TIMEOUT_MS;
  cfg.idle_core_mask = 0;
  cfg.trigger_panic = true;
  if (esp_task_wdt_reconfigure(&cfg) != ESP_OK) esp_task_wdt_init(&cfg);
}

void setup() {
  Serial.setRxBufferSize(SERIAL_RX_BUF);
  Serial.setTxBufferSize(SERIAL_TX_BUF);
  Serial.begin(SERIAL_BAUD);

  pinMode(PIN_CS, OUTPUT);
  csHigh();
  pinMode(PIN_GDO0, INPUT);
  pinMode(PIN_GDO2, INPUT);
  SPI.begin(PIN_SCK, PIN_MISO, PIN_MOSI, PIN_CS);  // MISO owned only by SPI
  manualReset();

  cslPoolInit();
  cslQueueClear();
  rxReset();
  mbedtls_aes_init(&pairWin.aes);
  wdtInit();
  queuesInit();

  xTaskCreatePinnedToCore(radioTask, "radio", RADIO_STACK, nullptr, RADIO_PRIO, &radioTaskHandle,
                          RADIO_CORE);
  xTaskCreatePinnedToCore(serialTask, "serial", SERIAL_STACK, nullptr, SERIAL_PRIO, nullptr,
                          SERIAL_CORE);

  attachInterrupt(digitalPinToInterrupt(PIN_GDO0), gdo0Isr, CHANGE);
  attachInterrupt(digitalPinToInterrupt(PIN_GDO2), gdo2Isr, CHANGE);
}

void loop() {
  vTaskDelay(pdMS_TO_TICKS(1000));
}
