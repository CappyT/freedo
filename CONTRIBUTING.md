# Contributing

This file explains how to build freedo, which checks a change must pass, and how to contribute a
capture for a device that freedo does not support.

The directory layout is in [`AGENTS.md`](AGENTS.md). The radio facts are in
[`docs/PROTOCOL.md`](docs/PROTOCOL.md). The HTTP routes are in [`docs/API.md`](docs/API.md).

## Build and test

You need the Go version in `go.mod`.

```sh
go build ./...
go vet ./...
go test ./...
```

The three commands are the gate. A change passes when all three succeed. CI runs them, and
it also runs the Home Assistant tests, compiles the bridge firmware and builds both container
images.

The dashboard needs Node 22.17 or later. The three Go commands work without Node, because
`internal/web/dist` holds only a placeholder. A change to `dashboard/` passes these checks, and CI also
runs `npm run prefix-check`:

```sh
cd dashboard
npm ci
npm run check
npm test
```

See [`dashboard/README.md`](dashboard/README.md) for the build and the prefix check.

The Home Assistant tests need Python and a test plugin:

```sh
uv run --no-project --python 3.14 --with pytest-homeassistant-custom-component pytest homeassistant/tests
```

`homeassistant/tests/test_reports.py` checks the YAML examples in
`custom_components/freedo/README.md`. Keep those examples valid.

The bridge firmware builds with `arduino-cli`. See the
[firmware README](firmware/cc1101_bridge/README.md).

### Version stamp

The build produces two binaries: `freedo` (the daemon) and `freedo-dev` (bench and
development tools, not in the images). To stamp a build:

```sh
go build -ldflags "\
  -X freedo/internal/buildinfo.Version=$(git describe --tags --always --dirty) \
  -X freedo/internal/buildinfo.Commit=$(git rev-parse --short HEAD) \
  -X freedo/internal/buildinfo.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/...
```

`--version` prints the stamp. A build without `-ldflags` reads the revision and the date from
the Go toolchain.

A release has one version. The tag `vX.Y.Z`, `version` in `addon/config.yaml` and `version` in
`custom_components/freedo/manifest.json` all say `X.Y.Z`. The release workflow stops when they
differ.

## Rules for changes

- Write code, comments and documentation in English.
- Keep a change small and focused. Add a test that fails when the logic breaks.
- Use synthetic fixtures in automated tests. A fixture holds invented identifiers and an
  invented network key. Never put a real serial number, EUI, home id, network key, token or
  address in the repository.
- Reduce a regression to a small fixture in `tado/testdata` when you can.
- Source reliability, from best to worst: physical captures, device firmware, the code,
  the documents. When two sources disagree, the better source wins. Fix the worse one.
- A document states the current state and the reason for it, not the history. Git records
  the history.

### Commits

Commits follow Conventional Commits. The whole message is lowercase. The subject is
imperative and has no trailing period. The message has a subject line only, with no body.
Keep each commit atomic.

```text
fix(runner): bump chart version
feat: add oidc client
```

## Developer tools

`freedo-dev` (built from `cmd/freedo-dev`, not part of the images) holds the bench and
development commands. Run `freedo-dev help` for the list. Every radio command takes
`-p <port>` (default `/dev/ttyUSB0`). Every command that decrypts, names a device or transmits
reads the installation from the database (`--state`), and never creates one.

- Offline: `decrypt`, `frame`, `dissect` and `nodes`.
- Radio: `ping`, `config`, `regs`, `autotune`, `rx`, `tx` and `setpoint`.
- Emulator: `emulate`, `emulate --replay <file>` and `coldstart`. Without `--tx` they transmit
  nothing.
- Firmware: `ota` checks an image against `d/fw/state` and prints the Block1 plan. With `--tx` it
  hands the image to the daemon.

### Radio calibration

Stop the daemon, then run the calibration once for each chip or installation, in receive only:

```sh
go run ./cmd/freedo-dev autotune -p /dev/ttyUSB0 --state freedo-state.db
```

The command measures the frequency offset of the received frames and stores `FSCTRL0` in the
database. The flag `-dry` prints the value and stores nothing. The commercial bridge can be on
or off during the measurement.

## Design notes

### Persistence

The daemon keeps two stores in one SQLite database.

- The installation is the source of truth. It holds the network key, the devices, the zones and
  the circuits. A write moves `install_rev`. Every write goes through `mutateInstall`: one
  transaction applies the rows, rebuilds the installation, validates it and raises the
  revision.
- The desired state is the cache that faces the devices. A save moves `save_seq` and leaves
  `install_rev` alone. The engine numbers its snapshots, so an old snapshot never overwrites a
  newer one.
- At start the daemon restores the desired state first, then loads the installation. The
  installation wins over a stored `d/config` document.
- Reference columns hold the wire EUI, never a serial number or a label.

### Radio

The Go daemon holds the application model. The ESP32 firmware runs the operations with tight
timing: it answers at the MAC layer within the required window and keeps the CSL queue. The
daemon must never block the receive loop. The firmware limits every queue and every wait.
A restart keeps the counters and the correlations that are still valid.

## Contributing a capture for an unsupported device

freedo supports the valves and wall thermostats of the V3+ line. For another device, a capture
is the only way to add support. A raw capture is not safe to share: the MAC header is in clear
text and holds the EUI of every device, and a capture of a pairing can leak the network key.

Do this, in order:

1. Record with `freedo-dev rx -p /dev/ttyUSB0 > capture.log`. Keep this file private.
2. Note what you did and when: the model of the device, the firmware version it reports, the
   action (for example "set 21 C in the app at 14:03:10"), and the action that came before.
3. Decode the frames with `freedo-dev decrypt` and `freedo-dev dissect` on your machine.
4. Replace every serial number, EUI, home id and token in the decoded text with invented values.
   Share the decoded and redacted text, not the raw hexadecimal frames.
5. If you want to submit a test vector, build the frames again under the synthetic fixture key
   (see `tado/fixture_install_test.go`) with the invented values.
6. Open an issue with the "Device support" template and attach the redacted text and your notes.

Never share the database, the bootstrap file, a network key, an API token or the unhandled log
without redaction. If you are not sure whether a file is safe, ask in the issue first and do
not attach it.

## Hardware and firmware

The radio board design in [`hardware/radio`](hardware/radio/README.md) is not tested. The bridge
firmware ran on one board only. Reviews and pull requests are welcome for both.
