// Command freedo is the local Tado coordinator daemon: it emulates the bridge
// on the 868 MHz link and serves the HTTP get/set API. The bench and
// development tools live in the separate freedo-dev binary.
package main

import (
	"os"
	// The container image has no zoneinfo: options.timezone and TZ need it for 02b2.
	_ "time/tzdata"

	"freedo/internal/daemon"
)

func main() { daemon.Main(os.Args[1:]) }
