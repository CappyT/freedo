package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"freedo/internal/cli"
	"freedo/tado"
)

func cmdOTA(args []string) {
	fs := flag.NewFlagSet("ota", flag.ExitOnError)
	configFlag(fs)
	stateFlag(fs)
	fwState := fs.String("fw-state", "", `the device's d/fw/state body, hex (the "result" of GET /api/nodes/{id}/fw)`)
	imagePath := fs.String("image", "", "raw application image linked at 0xa000")
	version := fs.String("version", "", "version to write into the header, major.minor")
	imageKind := fs.String("image-kind", "", "device kind the image is built for: valve or thermostat")
	doTx := fs.Bool("tx", false, "hand the image to the running daemon, which sends it; without it the plan is only printed")
	api := fs.String("api", "http://"+cli.DefaultHTTPListen, "daemon API base URL, for --tx")
	tokenFile := fs.String("api-token-file", "", "file holding the daemon API bearer token, for --tx (else $"+apiTokenEnv+")")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `freedo-dev ota - plan or start an over-the-air firmware update of one valve or thermostat

Without --tx it checks the device's fw/state and the image, then prints the
header, the slot CRC and the Block1 plan. It refuses an unconfirmed running
image, a version below the running one, an image that fails validation, and a
kind mismatch.

With --tx it posts the image to the running daemon (POST /api/nodes/{id}/fw).
The daemon owns the radio: it runs the same refusals against the fw/state it
read last, sends the blocks and verifies the reboot. Follow the transfer with
GET /api/nodes/{id}/fw, stop it with DELETE /api/nodes/{id}/fw.

usage:
  freedo-dev ota <device> --fw-state <hex> --image <file> --version <M.m> --image-kind <kind>
  freedo-dev ota <device> --image <file> --version <M.m> --image-kind <kind> --tx [--api <url>]

flags:
`)
		fs.PrintDefaults()
	}
	pos := parseFlags(fs, args)
	if len(pos) != 1 {
		die("ota wants exactly one device (serial, short address, wire EUI or room)")
	}
	if *imagePath == "" || *version == "" || *imageKind == "" || (*fwState == "" && !*doTx) {
		die("ota needs --image, --version, --image-kind, and --fw-state without --tx")
	}
	image, err := os.ReadFile(*imagePath)
	check(err)
	if *doTx {
		token, err := otaAPIToken(*tokenFile)
		check(err)
		if err := txOTA(os.Stdout, http.DefaultClient, *api, token, pos[0], image, *version, *imageKind); err != nil {
			die("ota --tx: %v", err)
		}
		return
	}
	in := mustInstall(fs)
	plan, err := planOTA(in, pos[0], *fwState, image, *version, *imageKind)
	if err != nil {
		die("ota: %v", err)
	}
	printOTAPlan(os.Stdout, plan)
	fmt.Println("dry-run: nothing sent")
}

const apiTokenEnv = "FREEDO_API_TOKEN"

func otaAPIToken(file string) (string, error) {
	if file == "" {
		return strings.TrimSpace(os.Getenv(apiTokenEnv)), nil
	}
	b, err := os.ReadFile(file)
	return strings.TrimSpace(string(b)), err
}

// txOTA starts the transfer on the daemon and prints its answer.
func txOTA(w io.Writer, client *http.Client, api, token, device string, image []byte, version, imageKind string) error {
	body, err := json.Marshal(map[string]any{"confirm": true, "version": version, "imageKind": imageKind, "image": image})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(api, "/")+"/api/nodes/"+url.PathEscape(device)+"/fw", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("daemon answered %s: %s", resp.Status, bytes.TrimSpace(answer))
	}
	fmt.Fprintf(w, "transfer started: %s\nfollow it with GET /api/nodes/%s/fw\n", bytes.TrimSpace(answer), url.PathEscape(device))
	return nil
}

type otaPlan struct {
	device *tado.Device
	state  *tado.FwState
	*tado.OTAPlan
}

func planOTA(in *tado.Install, target, fwStateHex string, image []byte, version, imageKind string) (*otaPlan, error) {
	d, err := in.Resolve(target)
	if err != nil {
		return nil, err
	}
	body, err := hex.DecodeString(strings.TrimSpace(fwStateHex))
	if err != nil {
		return nil, fmt.Errorf("--fw-state: %w", err)
	}
	state, err := tado.ParseFwState(body)
	if err != nil {
		return nil, err
	}
	v, err := tado.ParseFwVersion(version)
	if err != nil {
		return nil, err
	}
	p, err := tado.PlanDeviceOTA(d, imageKind, state, image, v)
	if err != nil {
		return nil, err
	}
	return &otaPlan{device: d, state: state, OTAPlan: p}, nil
}

func printOTAPlan(w io.Writer, p *otaPlan) {
	s := p.state
	slot := func(name string, sl tado.FwSlot) {
		if sl.Present {
			fmt.Fprintf(w, "  %-9s %-7s %s\n", name, sl.Version, sl.Flags)
		}
	}
	fmt.Fprintf(w, "device    %s  kind=%s  fw type=%d  build=%s\n", p.device.Ref(), p.device.Kind, s.DeviceType, s.BuildHash)
	fmt.Fprintln(w, "slots")
	slot("running", s.Running)
	slot("ext 0x10", s.Ext10)
	slot("ext 0x11", s.Ext11)
	img := p.Image
	fmt.Fprintf(w, "image     %d bytes (%#x), %s -> %s\n", len(img.Image), len(img.Image), p.Running, img.Version)
	fmt.Fprintf(w, "header    %s\n", hex.EncodeToString(img.Header()))
	fmt.Fprintf(w, "  01a8 size    %d\n  01a9 unused  0\n  01aa crc16   %#04x (CCITT-FALSE over %#x bytes, 0xFF tail)\n  01ab version %#04x (%s)\n",
		len(img.Image), img.CRC, tado.OTASlotSize, uint16(img.Version), img.Version)
	fmt.Fprintf(w, "blocks    %d x %d bytes (last %d)\n", p.Blocks, tado.OTABlockSize, (tado.OTAHeaderLen+len(img.Image)-1)%tado.OTABlockSize+1)
	// Each 128-byte block rides in a datagram too long for one 127-byte frame:
	// two 6LoWPAN fragments down, one piggybacked response up.
	fmt.Fprintf(w, "round trips %d confirmable POST d/fw, ~%d downlink frames\n", p.Blocks, 2*p.Blocks)
}
