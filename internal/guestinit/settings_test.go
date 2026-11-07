package guestinit

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestEncodeDecode(t *testing.T) {
	in := Settings{ServerURL: "https://x.example.com", Token: "fcb_abc", ServerAddrs: []string{"192.0.2.1"}, CAPEM: "pem"}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw)%sectorBytes != 0 {
		t.Errorf("size %d is not whole sectors", len(raw))
	}
	out, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if out.ServerURL != in.ServerURL || out.Token != in.Token || out.ServerAddrs[0] != "192.0.2.1" || out.CAPEM != "pem" {
		t.Errorf("out = %+v", out)
	}
}

func TestEncodeBounded(t *testing.T) {
	if _, err := Encode(Settings{CAPEM: strings.Repeat("x", MaxConfigBytes)}); err == nil {
		t.Error("no error for settings that do not fit")
	}
}

func TestDecodeRefuses(t *testing.T) {
	body := func(s string) string { return fmt.Sprintf("%s\n%d\n%s", magic, len(s), s) }
	for name, raw := range map[string]string{
		"empty":      "",
		"wiped":      strings.Repeat("\x00", 4096),
		"no length":  magic + "\n",
		"bad length": magic + "\nabc\n{}",
		"too long":   magic + "\n99999999\n{}",
		"cut short":  magic + "\n100\n{}",
		"not json":   body("fcb_secret"),
		"no token":   body(`{"server_url":"http://x","server_addrs":["192.0.2.1"]}`),
		"no address": body(`{"server_url":"http://x","token":"t"}`),
	} {
		_, err := Decode(strings.NewReader(raw))
		if err == nil {
			t.Errorf("%s: no error", name)
		} else if strings.Contains(err.Error(), "fcb_secret") {
			t.Errorf("%s: the error quotes the disk: %v", name, err)
		}
	}
}

func TestSettingsHideTheToken(t *testing.T) {
	v := Settings{ServerURL: "http://x", Token: "fcb_abc"}.LogValue()
	if v.Kind() != slog.KindString || strings.Contains(v.String(), "fcb_abc") {
		t.Errorf("log value = %v", v)
	}
}

func TestParseCmdline(t *testing.T) {
	got := parseCmdline("console=ttyS0 forecrew.ip=10.0.0.2/30 rw forecrew.halt=reboot init=/x -- forecrew.ip=9.9.9.9/32\n")
	if len(got) != 2 || got["ip"] != "10.0.0.2/30" || got["halt"] != "reboot" {
		t.Errorf("got = %v", got)
	}
}
