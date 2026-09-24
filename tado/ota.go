package tado

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
)

// Over-the-air update of a valve or thermostat nRF52 (POST d/fw, RFC 7959
// Block1). Read in the valve firmware, block handler FUN_0001e90c:
//   - blocks are 128 bytes; block 0 shorter -> 2.31 asking for 128, longer -> 4.13;
//   - a repeat of the previous block is re-acked, any other gap -> 4.08;
//   - intermediate blocks -> 2.31, the last one -> 2.04 once the CRC verifies;
//   - block 0 opens with a 20-byte header, the image bytes follow it.
//
// The image is the raw application linked at 0xa000. The device checks the
// CRC-16 over the whole 0x73000 slot window, the tail past the image read as
// erased flash (0xFF). Nothing on the device checks the image is built for its
// hardware: that check is ours.
const (
	OTABlockSize  = 128
	OTAHeaderLen  = 20
	OTASlotSize   = 0x73000
	OTALinkBase   = 0xa000
	otaFlashEnd   = OTALinkBase + OTASlotSize // 0x7d000
	otaRAMStart   = 0x20000000
	otaRAMEnd     = 0x20010000 // nRF52832, 64 KiB
	otaTagSize    = 0x01a8
	otaTagUnused  = 0x01a9
	otaTagCRC     = 0x01aa
	otaTagVersion = 0x01ab
	// OTADeviceType is the 01a0 value of d/fw/state. The valves and the wall
	// thermostat all report 8, so it does not tell them apart: the kind check
	// rests on the installation and on the declared image kind.
	OTADeviceType = 8
	otaBlockSZX   = 3 // 16 << 3 = 128 bytes
	coapOptBlock1 = 27
)

// CRC16CCITTFalse is CRC-16/CCITT-FALSE: poly 0x1021, init 0xFFFF, no
// reflection, no final xor.
func CRC16CCITTFalse(data []byte) uint16 {
	return crc16CCITTUpdate(0xFFFF, data)
}

func crc16CCITTUpdate(crc uint16, data []byte) uint16 {
	for _, b := range data {
		crc ^= uint16(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// OTASlotCRC is the CRC the device computes over the slot window: the image,
// then 0xFF up to OTASlotSize.
func OTASlotCRC(image []byte) uint16 {
	crc := crc16CCITTUpdate(0xFFFF, image)
	pad := [256]byte{}
	for i := range pad {
		pad[i] = 0xFF
	}
	for left := OTASlotSize - len(image); left > 0; {
		n := min(left, len(pad))
		crc = crc16CCITTUpdate(crc, pad[:n])
		left -= n
	}
	return crc
}

// ValidateOTAImage checks size and the Cortex-M vector table of an application
// linked at 0xa000: initial SP inside nRF52832 RAM, reset handler inside the
// slot and a Thumb address.
func ValidateOTAImage(image []byte) error {
	if len(image) == 0 || len(image) > OTASlotSize {
		return fmt.Errorf("tado: ota image is %d bytes, want 1..%#x", len(image), OTASlotSize)
	}
	if len(image) < 8 {
		return fmt.Errorf("tado: ota image is %d bytes, too short for a vector table", len(image))
	}
	sp := binary.LittleEndian.Uint32(image[0:4])
	reset := binary.LittleEndian.Uint32(image[4:8])
	if sp < otaRAMStart || sp > otaRAMEnd {
		return fmt.Errorf("tado: ota image initial SP %#08x is outside RAM %#x..%#x", sp, otaRAMStart, otaRAMEnd)
	}
	if reset&1 == 0 {
		return fmt.Errorf("tado: ota image reset vector %#08x is not a Thumb address", reset)
	}
	if addr := reset &^ 1; addr < OTALinkBase || addr >= otaFlashEnd {
		return fmt.Errorf("tado: ota image reset vector %#08x is outside %#x..%#x: not linked at %#x", reset, OTALinkBase, otaFlashEnd, OTALinkBase)
	}
	return nil
}

// OTAImage is a validated image ready to send.
type OTAImage struct {
	Image   []byte
	Version FwVersion
	CRC     uint16
}

// NewOTAImage validates the image and computes its slot CRC.
func NewOTAImage(image []byte, v FwVersion) (*OTAImage, error) {
	if err := ValidateOTAImage(image); err != nil {
		return nil, err
	}
	return &OTAImage{Image: image, Version: v, CRC: OTASlotCRC(image)}, nil
}

// Header is the 20-byte block 0 header. 01a8 carries its 4-byte size with no
// length byte; the other three are regular TLVs.
func (o *OTAImage) Header() []byte {
	h := make([]byte, 0, OTAHeaderLen)
	h = binary.BigEndian.AppendUint16(h, otaTagSize)
	h = binary.BigEndian.AppendUint32(h, uint32(len(o.Image)))
	h = binary.BigEndian.AppendUint16(h, otaTagUnused)
	h = append(h, 1, 0)
	h = binary.BigEndian.AppendUint16(h, otaTagCRC)
	h = append(h, 2)
	h = binary.BigEndian.AppendUint16(h, o.CRC)
	h = binary.BigEndian.AppendUint16(h, otaTagVersion)
	h = append(h, 2)
	return binary.BigEndian.AppendUint16(h, uint16(o.Version))
}

// Blocks splits header and image into Block1 payloads of OTABlockSize bytes;
// only the last one is shorter.
func (o *OTAImage) Blocks() [][]byte {
	body := append(o.Header(), o.Image...)
	var out [][]byte
	for off := 0; off < len(body); off += OTABlockSize {
		out = append(out, body[off:min(off+OTABlockSize, len(body))])
	}
	return out
}

// OTAPlan is a checked update of one device.
type OTAPlan struct {
	Image   *OTAImage
	Running FwVersion
	Blocks  int
}

// PlanOTA refuses what the device would refuse or a downgrade the bootloader
// would never activate: an unconfirmed running image, a version below the
// running one, an image that fails validation.
func PlanOTA(state *FwState, image []byte, v FwVersion) (*OTAPlan, error) {
	if state == nil {
		return nil, errors.New("tado: ota: no fw/state")
	}
	if !state.Confirmed() {
		return nil, fmt.Errorf("tado: ota: running image %s is not confirmed (%s): the device refuses to begin", state.Running.Version, state.Running.Flags)
	}
	if v < state.Running.Version {
		return nil, fmt.Errorf("tado: ota: version %s is below running %s: the bootloader never activates it", v, state.Running.Version)
	}
	img, err := NewOTAImage(image, v)
	if err != nil {
		return nil, err
	}
	return &OTAPlan{Image: img, Running: state.Running.Version, Blocks: len(img.Blocks())}, nil
}

// PlanDeviceOTA is PlanOTA for one declared device: only a valve or a wall
// thermostat takes the update, the image must be built for that kind, and the
// fw/state must carry OTADeviceType.
func PlanDeviceOTA(d *Device, imageKind string, state *FwState, image []byte, v FwVersion) (*OTAPlan, error) {
	if d.Kind != KindValve && d.Kind != KindThermostat {
		return nil, fmt.Errorf("%s is a %s: only valves and thermostats take this update", d.Ref(), d.Kind)
	}
	if DeviceKind(imageKind) != d.Kind {
		return nil, fmt.Errorf("the image is for a %s and %s is a %s", imageKind, d.Ref(), d.Kind)
	}
	if state == nil {
		return nil, errors.New("tado: ota: no fw/state")
	}
	if state.DeviceType != OTADeviceType {
		return nil, fmt.Errorf("fw/state device type %d, want %d", state.DeviceType, OTADeviceType)
	}
	return PlanOTA(state, image, v)
}

// OTAReply is the device's answer to one POST d/fw.
type OTAReply struct {
	Code string // e.g. "2.31"
	// Block1 is the block number the answer echoes in its Block1 option, -1
	// when it carries none.
	Block1 int
}

// OTATransport carries one Block1 exchange at a time to the device.
type OTATransport interface {
	// PostBlock sends POST d/fw with Block1 (num, more, SZX 128). An error means
	// no answer arrived, or ctx ended.
	PostBlock(ctx context.Context, num int, more bool, data []byte) (OTAReply, error)
	// Abort sends DELETE d/fw.
	Abort() error
}

// ErrOTAUnverified ends an upload whose last block was answered 4.12 after a
// resend, or not at all. The device may have taken the image and rebooted
// before its answer reached us: only its fw/state after the reboot tells, so
// the upload is not aborted.
var ErrOTAUnverified = errors.New("tado: ota: the last block got no clear answer: verify the fw/state after the reboot")

// SendOTA sends the blocks with one block in flight. A block without response
// is sent again as is, up to retries extra times: the device re-acks a repeat.
// Any unexpected code, a wrong Block1 echo, exhausted retries or the end of ctx
// aborts the upload with DELETE d/fw, except on the last block: see
// ErrOTAUnverified.
func SendOTA(ctx context.Context, t OTATransport, blocks [][]byte, retries int, progress func(num int)) error {
	if len(blocks) == 0 {
		return errors.New("tado: ota: no blocks")
	}
	for num, data := range blocks {
		more := num < len(blocks)-1
		want := "2.04"
		if more {
			want = "2.31"
		}
		reply, tries, err := postWithRetry(ctx, t, num, more, data, retries)
		if !more && ctx.Err() == nil && (err != nil || reply.Code == "4.12" && tries > 1) {
			if err == nil {
				err = fmt.Errorf("answered 4.12 on try %d", tries)
			}
			return fmt.Errorf("%w: block %d: %w", ErrOTAUnverified, num, err)
		}
		if err == nil && reply.Code != want {
			err = fmt.Errorf("tado: ota: block %d answered %s, want %s", num, reply.Code, want)
		}
		if err == nil && reply.Block1 >= 0 && reply.Block1 != num {
			err = fmt.Errorf("tado: ota: block %d answered %s echoing block %d", num, reply.Code, reply.Block1)
		}
		if err != nil {
			if abortErr := t.Abort(); abortErr != nil {
				return errors.Join(err, fmt.Errorf("tado: ota: abort: %w", abortErr))
			}
			return err
		}
		if progress != nil {
			progress(num)
		}
	}
	return nil
}

func postWithRetry(ctx context.Context, t OTATransport, num int, more bool, data []byte, retries int) (OTAReply, int, error) {
	var err error
	tries := 0
	for range retries + 1 {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return OTAReply{}, tries, ctxErr
		}
		tries++
		var reply OTAReply
		if reply, err = t.PostBlock(ctx, num, more, data); err == nil {
			return reply, tries, nil
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return OTAReply{}, tries, ctxErr
	}
	return OTAReply{}, tries, fmt.Errorf("tado: ota: block %d: no response after %d tries: %w", num, tries, err)
}

// BuildFwBlockCoAP builds one POST d/fw carrying Block1 (num, more, 128 bytes).
func BuildFwBlockCoAP(num int, more bool, data []byte, mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	if num < 0 || num >= 1<<20 {
		return nil, fmt.Errorf("tado: ota block %d out of range", num)
	}
	if len(data) == 0 || len(data) > OTABlockSize || more && len(data) != OTABlockSize {
		return nil, fmt.Errorf("tado: ota block %d is %d bytes", num, len(data))
	}
	block := uint32(num)<<4 | otaBlockSZX
	if more {
		block |= 8
	}
	return devicePost(deviceReq{
		Path:    []string{"d", "fw"},
		Options: []CoAPOption{optUriPort, optOctets, {Num: coapOptBlock1, Val: encodeCoAPUint(block)}},
		Payload: data,
		MID:     mid, Token: token, SrcEUI: srcEUI, DstEUI: dstEUI,
	})
}

// BuildFwAbortCoAP builds the DELETE d/fw that drops an upload in progress.
func BuildFwAbortCoAP(mid uint16, token, srcEUI, dstEUI []byte) ([]byte, error) {
	return deviceRequest(deviceReq{
		Path:    []string{"d", "fw"},
		Options: []CoAPOption{optUriPort},
		MID:     mid, Token: token, SrcEUI: srcEUI, DstEUI: dstEUI,
	}, "0.04")
}

// block1Echo returns the block number of a Block1 option, -1 when m has none.
func block1Echo(m *CoAPMessage) int {
	o, ok := m.Option(coapOptBlock1)
	if !ok || len(o.Val) > 3 {
		return -1
	}
	return int(decodeCoAPUint(o.Val) >> 4)
}

// VerifyOTA checks the fw/state a device reports after the reboot: the new
// version runs and is confirmed. The device confirms only after the
// coordinator answers its fw/state PUT with a 2.xx.
func VerifyOTA(after *FwState, want FwVersion) error {
	if after == nil {
		return errors.New("tado: ota verify: no fw/state")
	}
	if after.Running.Version != want {
		return fmt.Errorf("tado: ota verify: running %s, want %s", after.Running.Version, want)
	}
	if !after.Confirmed() {
		return fmt.Errorf("tado: ota verify: running %s is not confirmed (%s)", want, after.Running.Flags)
	}
	return nil
}
