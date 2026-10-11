# freedo add-on

Runs the freedo daemon, a local coordinator for tado 868 MHz heating devices, on Home Assistant. The
daemon talks to an ESP32 + CC1101 radio bridge on a USB port and serves a local HTTP API. The freedo
integration for Home Assistant uses that API.

The add-on starts in dry-run and sends nothing on air until you turn on Transmit. freedo is
experimental. Options, data and backups are in the add-on documentation.
