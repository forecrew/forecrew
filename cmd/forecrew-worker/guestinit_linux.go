package main

import (
	"fmt"
	"io"
	"os"

	"github.com/forecrew/forecrew/internal/guestinit"
)

// guestInit runs PID 1 of a microVM, and nothing else: anywhere but as PID 1 it
// would mount over the machine it runs on.
func guestInit(stderr io.Writer, version string) int {
	if os.Getpid() != 1 {
		fmt.Fprintln(stderr, "forecrew-worker: guest-init runs only as PID 1 of a microVM")
		return 1
	}
	guestinit.Main(version)
	return 0
}
