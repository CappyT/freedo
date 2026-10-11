# freedo

freedo is a local coordinator for tado V3+ 868 MHz heating devices. A Go daemon talks over a serial
link to an ESP32 with a CC1101 radio, keeps the state of devices, zones and circuits, and serves a
local HTTP API. A Home Assistant integration uses that API. The project is experimental and
safety relevant: it controls heating.

Read `README.md` for scope and status, `docs/PROTOCOL.md` for the radio facts, `docs/API.md` for the
routes and `CONTRIBUTING.md` for the rules of a change.

## Layout

- `cmd/freedo`: the daemon. `cmd/freedo-dev`: bench and development tools (not in the images).
- `internal/daemon`: engine, HTTP API, desired state in SQLite, firmware transfer.
- `internal/install`: SQLite schema, installation, radio calibration.
- `internal/radio`: live radio, replay and mock. `internal/cli`: flags, bootstrap file, replay.
- `internal/buildinfo`: version stamp.
- `tado`: codec, CoAP, TLV, state and coordinator logic. `tado/testdata`: small synthetic fixtures.
- `cc1101`: serial link to the ESP32 bridge.
- `custom_components/freedo`: Home Assistant integration. `homeassistant`: blueprint and Python tests.
- `addon`: Home Assistant add-on. `repository.yaml`, `hacs.json`: store metadata.
- `firmware/cc1101_bridge`: Arduino sketch of the ESP32 + CC1101 bridge.
- `hardware/radio`: KiCad design of the radio board.
- `.github/workflows`: CI, validation and release.

## Gates

Run from the repository root. A change passes when all three succeed.

```sh
go build ./...
go vet ./...
go test ./...
```

## Constraints

- Use synthetic fixtures in automated tests. Never commit a real serial number, EUI, home id,
  network key, API token or address.
- Do not read, print or commit `freedo.yaml`, `freedo-state.db` (and its `-wal` and `-shm` files),
  `freedo_api_token`, capture logs or `bench/`. The database holds the network key.
- Source reliability, best first: physical captures, device firmware, the code, the documents.
  When they disagree, trust the better source and fix the worse one.
- One coordinator per radio network. The daemon starts in dry-run and transmits only with `--tx`.
  Test radio changes in replay first.
- Keep setpoint limits (5 to 25 C for a room, 30 to 65 C for hot water) and the other input checks.
  Do not weaken them.
- Keep the working tree as you find it. Do not delete or rewrite files that your task does not touch.

## Style

- Write code, comments and documentation in English.
- A document states the current state and the reason for it, not the history.
- A code comment explains a non-obvious reason, never what the code does.

## Commits

Conventional Commits, all lowercase, imperative, no trailing period, subject line only:
`fix(runner): bump chart version`. Keep commits atomic.
