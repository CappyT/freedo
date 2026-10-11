package cc1101

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// At 250000 baud or less the ESP32 core clocks the UART from a 1 MHz tick, and
// a CH340 adapter garbled 28 of 100 replies at 230400. The sketch has to name
// the APB clock before it opens the port.
func TestSketchClocksTheUARTFromAPB(t *testing.T) {
	sketch, err := os.ReadFile("../firmware/cc1101_bridge/cc1101_bridge.ino")
	if err != nil {
		t.Fatal(err)
	}
	text := string(sketch)
	clock := strings.Index(text, "Serial.setClockSource(UART_CLK_SRC_APB)")
	begin := strings.Index(text, "Serial.begin(SERIAL_BAUD)")
	if clock < 0 || begin < 0 || clock > begin {
		t.Fatalf("setClockSource at %d, begin at %d: the APB clock has to come first", clock, begin)
	}
	if want := fmt.Sprintf("SERIAL_BAUD = %d;", DefaultBaud); !strings.Contains(text, want) {
		t.Fatalf("the sketch does not say %q: the host and the firmware disagree on the rate", want)
	}
}
