package netguard

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func TestPublicAndLocal(t *testing.T) {
	for addr, want := range map[string][2]bool{ // public, local
		"8.8.8.8":         {true, false},
		"127.0.0.1":       {false, true},
		"10.1.2.3":        {false, true},
		"192.168.1.1":     {false, true},
		"169.254.169.254": {false, false},
		"100.64.0.1":      {false, false},
		"::1":             {false, true},
		"fe80::1":         {false, false},
		"2606:4700::1":    {true, false},
	} {
		a := netip.MustParseAddr(addr)
		if Public(a) != want[0] || Local(a) != want[1] {
			t.Errorf("%s: public %v local %v, want %v", addr, Public(a), Local(a), want)
		}
		if Allowed(a, false) != want[0] || (want[0] || want[1]) && !Allowed(a, true) {
			t.Errorf("%s: Allowed wrong", addr)
		}
	}
	if Public(netip.Addr{}) {
		t.Error("invalid address is public")
	}
}

func TestDial(t *testing.T) {
	for _, tt := range []struct {
		addr  string
		local bool
		ok    bool
	}{
		{"8.8.8.8:443", false, true},
		{"127.0.0.1:11434", false, false},
		{"127.0.0.1:11434", true, true},
		{"169.254.169.254:80", true, false},
		{"nonsense", true, false},
	} {
		err := Dial(tt.local)("tcp", tt.addr, nil)
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, ErrRefused)) {
			t.Errorf("Dial(%v)(%s) = %v", tt.local, tt.addr, err)
		}
	}
	if GuardDial("tcp", "10.0.0.1:80", nil) == nil {
		t.Error("GuardDial allowed a private address")
	}
	if PublicTransport() == nil || Transport(true).Proxy != nil {
		t.Error("transport")
	}
}

func TestAllowHost(t *testing.T) {
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "public.test":
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		case "mixed.test":
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, nil
		case "none.test":
			return nil, nil
		}
		return nil, errors.New("no such host")
	}
	ctx := context.Background()
	if err := AllowHost(ctx, lookup, "public.test"); err != nil {
		t.Error(err)
	}
	if err := AllowHost(ctx, lookup, "mixed.test"); !errors.Is(err, ErrRefused) {
		t.Errorf("mixed: %v", err)
	}
	if err := AllowHostLocal(ctx, lookup, "mixed.test", true); err != nil {
		t.Errorf("mixed local: %v", err)
	}
	if err := AllowHostLocal(ctx, lookup, "169.254.169.254", true); !errors.Is(err, ErrRefused) {
		t.Errorf("metadata: %v", err)
	}
	for _, h := range []string{"none.test", "missing.test"} {
		if err := AllowHost(ctx, lookup, h); err == nil || errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v", h, err)
		}
	}
	if _, err := LookupHost(ctx, "127.0.0.1"); err != nil {
		t.Error(err)
	}
}

// TestLocalModeAllowsTheOperatorsNetworks: with local, everything an
// operator may run a service on is reached, and link-local and metadata
// addresses never are; without it, only public addresses.
func TestLocalModeAllowsTheOperatorsNetworks(t *testing.T) {
	for addr, want := range map[string][2]bool{ // default, local
		"8.8.8.8":                {true, true},
		"10.0.0.1":               {false, true},
		"127.0.0.1":              {false, true},
		"100.101.102.103":        {false, true}, // Tailscale
		"198.18.0.1":             {false, true},
		"192.0.0.9":              {false, true},
		"240.0.0.1":              {false, true},
		"fd7a:115c:a1e0::1":      {false, true}, // Tailscale IPv6
		"169.254.169.254":        {false, false},
		"169.254.1.1":            {false, false},
		"fe80::1":                {false, false},
		"100.100.100.200":        {false, false},
		"fd00:ec2::254":          {false, false},
		"::ffff:169.254.169.254": {false, false},
		"224.0.0.1":              {false, false},
		"0.0.0.0":                {false, false},
		"255.255.255.255":        {false, false},
	} {
		a := netip.MustParseAddr(addr)
		if Allowed(a, false) != want[0] || Allowed(a, true) != want[1] {
			t.Errorf("%s: default %v local %v, want %v", addr, Allowed(a, false), Allowed(a, true), want)
		}
	}
	if Allowed(netip.Addr{}, true) {
		t.Error("the zero address")
	}
}
