// Package guestinit is PID 1 of a Forecrew microVM. It is a static program for
// one reason: the root disk may be built from a repo's Dockerfile, so its
// /bin/sh, its git and its dynamic linker are the repo's code. Nothing of the
// image runs until the machine's one-time bootstrap token has been traded for a
// worker token and wiped from the config disk (docs/images.md, Inside the
// guest).
package guestinit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
)

const (
	// magic starts a config disk.
	magic = "FORECREW-CONFIG-1"
	// MaxConfigBytes bounds what a config disk holds.
	MaxConfigBytes = 1 << 20
	// sectorBytes is what a config disk's size is a multiple of.
	sectorBytes = 4096
)

// Settings is what a provider writes on a machine's config disk. Everything the
// exchange needs is here and not in the image: the image's CA bundle, hosts file
// and proxy settings are the repo's to choose, and would hand it the token.
type Settings struct {
	ServerURL string `json:"server_url"`
	// Token is the machine's bootstrap token. It is a secret.
	Token string `json:"token"`
	// ServerAddrs are the IP addresses of the server, as the host resolved
	// them.
	ServerAddrs []string `json:"server_addrs"`
	// CAPEM is what the server's certificate must chain to.
	CAPEM string `json:"ca_pem,omitempty"`
}

// LogValue hides the token.
func (s Settings) LogValue() slog.Value { return slog.StringValue("settings(" + s.ServerURL + ")") }

// Encode returns the content of a config disk: a line with the magic, a line
// with the length, and the settings as JSON, zero-padded to whole sectors.
func Encode(s Settings) ([]byte, error) {
	body, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	out := fmt.Appendf(nil, "%s\n%d\n", magic, len(body))
	out = append(out, body...)
	if len(out) > MaxConfigBytes {
		return nil, fmt.Errorf("the settings take %d bytes, more than a config disk's %d", len(out), MaxConfigBytes)
	}
	return append(out, make([]byte, (sectorBytes-len(out)%sectorBytes)%sectorBytes)...), nil
}

// Decode reads what Encode wrote.
func Decode(r io.Reader) (Settings, error) {
	b := bufio.NewReader(io.LimitReader(r, MaxConfigBytes))
	first, err := b.ReadString('\n')
	if err != nil || strings.TrimSuffix(first, "\n") != magic {
		return Settings{}, errors.New("not a config disk")
	}
	second, err := b.ReadString('\n')
	if err != nil {
		return Settings{}, errors.New("config disk: no length")
	}
	n, err := strconv.Atoi(strings.TrimSuffix(second, "\n"))
	if err != nil || n <= 0 || n > MaxConfigBytes {
		return Settings{}, errors.New("config disk: bad length")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(b, body); err != nil {
		return Settings{}, errors.New("config disk: cut short")
	}
	var s Settings
	if err := json.Unmarshal(body, &s); err != nil {
		return Settings{}, errors.New("config disk: bad settings") // the error could quote the token
	}
	if s.ServerURL == "" || s.Token == "" || len(s.ServerAddrs) == 0 {
		return Settings{}, errors.New("config disk: the server's URL and addresses and the token are required")
	}
	return s, nil
}

// parseCmdline returns the forecrew.* settings of a kernel command line.
func parseCmdline(line string) map[string]string {
	out := map[string]string{}
	for word := range strings.FieldsSeq(line) {
		if word == "--" {
			break // what follows is PID 1's own arguments
		}
		if k, v, ok := strings.Cut(word, "="); ok && strings.HasPrefix(k, "forecrew.") {
			out[strings.TrimPrefix(k, "forecrew.")] = v
		}
	}
	return out
}
