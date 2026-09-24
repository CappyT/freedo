package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// otaFwState is a synthetic d/fw/state: type 8, running 215.1 with the given
// flags nibble, ext 0x10 204.1 image+confirmed.
func otaFwState(runFlags byte) string {
	return fmt.Sprintf("01a00108"+"003a0235c1"+"003b01%02x"+"0035023301"+"0036010a"+"021007"+"61626331323334", runFlags)
}

func otaImage() []byte {
	img := make([]byte, 1000)
	binary.LittleEndian.PutUint32(img[0:], 0x2000fff0)
	binary.LittleEndian.PutUint32(img[4:], 0x0000b001)
	return img
}

func TestPlanOTAPrintsThePlan(t *testing.T) {
	p, err := planOTA(fixtureInstall, "a602", otaFwState(0x0e), otaImage(), "215.2", "valve")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printOTAPlan(&out, p)
	for _, want := range []string{"kind=valve", "running   215.1   image+active+confirmed", "ext 0x10  204.1   image+confirmed",
		"215.1 -> 215.2", "header    01a8000003e801a9010001aa02", "01ab version 0x35c2", "blocks    8 x 128 bytes (last 124)", "round trips 8"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, out.String())
		}
	}
}

func TestPlanOTARefuses(t *testing.T) {
	bad := otaImage()
	binary.LittleEndian.PutUint32(bad[4:], 0x00001001)
	for name, tc := range map[string]struct {
		dev, fw, ver, kind string
		img                []byte
		want               string
	}{
		"unconfirmed":      {"a602", otaFwState(0x06), "215.2", "valve", otaImage(), "not confirmed"},
		"downgrade":        {"a602", otaFwState(0x0e), "215.0", "valve", otaImage(), "below running"},
		"bad image":        {"a602", otaFwState(0x0e), "215.2", "valve", bad, "reset vector"},
		"kind mismatch":    {"a205", otaFwState(0x0e), "215.2", "valve", otaImage(), "is a thermostat"},
		"unknown kind":     {"a602", otaFwState(0x0e), "215.2", "bridge", otaImage(), "image is for a bridge"},
		"bad fw hex":       {"a602", "zz", "215.2", "valve", otaImage(), "--fw-state"},
		"no running slot":  {"a602", "01a00108", "215.2", "valve", otaImage(), "running slot missing"},
		"other fw type":    {"a602", "01a00109" + otaFwState(0x0e)[8:], "215.2", "valve", otaImage(), "device type 9"},
		"bad version text": {"a602", otaFwState(0x0e), "215", "valve", otaImage(), "major.minor"},
	} {
		if _, err := planOTA(fixtureInstall, tc.dev, tc.fw, tc.img, tc.ver, tc.kind); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
}

func TestTxOTAPostsToTheDaemon(t *testing.T) {
	var got struct {
		Confirm   bool   `json:"confirm"`
		Version   string `json:"version"`
		ImageKind string `json:"imageKind"`
		Image     []byte `json:"image"`
	}
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"state":"sending"}`)
	}))
	defer srv.Close()
	var out bytes.Buffer
	if err := txOTA(&out, srv.Client(), srv.URL+"/", "tok", "a602", otaImage(), "215.2", "valve"); err != nil {
		t.Fatal(err)
	}
	if path != "POST /api/nodes/a602/fw" || auth != "Bearer tok" || !got.Confirm || got.Version != "215.2" ||
		got.ImageKind != "valve" || !bytes.Equal(got.Image, otaImage()) || !strings.Contains(out.String(), "sending") {
		t.Fatalf("path %q auth %q body %+v out %q", path, auth, got, out.String())
	}
}

func TestTxOTAReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"running image 215.1 is not confirmed"}`)
	}))
	defer srv.Close()
	err := txOTA(io.Discard, srv.Client(), srv.URL, "", "a602", otaImage(), "215.2", "valve")
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("err %v", err)
	}
}
