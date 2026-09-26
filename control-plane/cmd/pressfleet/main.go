// Command pressfleet is the PressFleet control plane: API server, background
// workers, metrics collector, and embedded dashboard, plus the operator CLI
// (init, doctor).
package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: pressfleet version")
	os.Exit(2)
}
