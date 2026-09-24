package main

import (
	"bufio"
	"encoding/hex"
	"os"
	"testing"
)

// loadFrames reads at most n on-air frames from a capture file.
func loadFrames(t *testing.T, path string, n int) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	var out [][]byte
	for sc.Scan() && len(out) < n {
		h, ok := replayHex(sc.Text())
		if !ok {
			continue
		}
		fr, err := hex.DecodeString(h)
		if err != nil || len(fr) < 2 {
			continue
		}
		out = append(out, fr)
	}
	return out
}
