package fleetagent

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
