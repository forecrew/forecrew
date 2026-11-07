//go:build enterprise

package main

// The enterprise build links ee/ in: it registers itself at init.
import _ "github.com/forecrew/forecrew/ee"
