package fleetagent

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolveAllow(t *testing.T) {
	got, err := ResolveAllow(t.Context(), net.DefaultResolver, []string{"10.0.0.5/8", "192.0.2.1", "2001:db8::/32", "::1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, p := range got {
		s = append(s, p.String())
	}
	if !strings.HasPrefix(strings.Join(s, " "), "10.0.0.0/8 192.0.2.1/32 127.0.0.1/32") {
		t.Errorf("resolved = %v", s)
	}
	if _, err := ResolveAllow(t.Context(), net.DefaultResolver, []string{"nothing.invalid"}); err == nil {
		t.Error("a name that does not resolve was accepted")
	}
}

func TestLoadTrustedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.pub")
	if _, err := LoadTrustedKeys(path); err == nil {
		t.Error("no key file: no error")
	}
	_ = os.WriteFile(path, []byte("# none\n"), 0o600)
	if _, err := LoadTrustedKeys(path); err == nil || !strings.Contains(err.Error(), "lists none") {
		t.Errorf("no keys: %v", err)
	}
	_ = os.WriteFile(path, []byte("junk\n"), 0o600)
	if _, err := LoadTrustedKeys(path); err == nil {
		t.Error("junk keys were read")
	}
}

func TestResolveAvailable(t *testing.T) {
	got, missed := ResolveAvailable(t.Context(), net.DefaultResolver, []string{"10.0.0.5/8", "192.0.2.1", "2001:db8::/32", "::1", "nothing.invalid", "localhost"})
	var s []string
	for _, p := range got {
		s = append(s, p.String())
	}
	if !strings.HasPrefix(strings.Join(s, " "), "10.0.0.0/8 192.0.2.1/32 127.0.0.1/32") {
		t.Errorf("resolved = %v", s)
	}
	if len(missed) != 1 || missed[0] != "nothing.invalid" {
		t.Errorf("missed = %v", missed)
	}
}

// A long list is looked up at most resolveAtOnce names at a time, and a name
// the resolver never answers gives up after resolveTimeout.
func TestResolveAvailableBounded(t *testing.T) {
	was, wasLookup := resolveTimeout, lookup
	resolveTimeout = 100 * time.Millisecond
	t.Cleanup(func() { resolveTimeout, lookup = was, wasLookup })
	var mu sync.Mutex
	now, most := 0, 0
	lookup = func(ctx context.Context, _ *net.Resolver, _ string) ([]netip.Addr, error) {
		mu.Lock()
		now++
		most = max(most, now)
		mu.Unlock()
		<-ctx.Done() // a resolver that never answers
		mu.Lock()
		now--
		mu.Unlock()
		return nil, ctx.Err()
	}
	var names []string
	for i := range 40 {
		names = append(names, fmt.Sprintf("host%d.example.com", i))
	}
	start := time.Now()
	nets, missed := ResolveAvailable(t.Context(), net.DefaultResolver, append(names, "10.0.0.0/8"))
	if len(missed) != 40 || len(nets) != 1 {
		t.Fatalf("nets %v, missed %d", nets, len(missed))
	}
	mu.Lock()
	defer mu.Unlock()
	if most != resolveAtOnce {
		t.Fatalf("%d lookups at once, want %d", most, resolveAtOnce)
	}
	// 40 names, 8 at a time, 100 ms each: about half a second, not forever.
	if d := time.Since(start); d < 400*time.Millisecond || d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
}
