//go:build !linux

package main

import (
	"fmt"
	"io"
)

func guestInit(stderr io.Writer, _ string) int {
	fmt.Fprintln(stderr, "forecrew-worker: guest-init runs only as PID 1 of a Linux microVM")
	return 1
}
