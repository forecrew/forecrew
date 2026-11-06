package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommands(t *testing.T) {
	none := func(string) string { return "" }
	tests := []struct {
		args []string
		code int
		out  string
	}{
		{nil, 2, "usage"},
		{[]string{"nonsense"}, 2, "unknown command"},
		{[]string{"version"}, 0, "forecrew-fleet-agent dev"},
		{[]string{"help"}, 0, "usage"},
		{[]string{"doctor", "--nonsense"}, 2, ""},
		{[]string{"doctor", "--kind", "vmware"}, 1, "not a kind"},
		{[]string{"run", "--nonsense"}, 2, ""},
		{[]string{"run", "--config", filepath.Join(t.TempDir(), "none.yaml")}, 1, "read configuration"},
	}
	for _, tt := range tests {
		var out, errOut bytes.Buffer
		if code := run(t.Context(), tt.args, &out, &errOut, none); code != tt.code || !strings.Contains(out.String()+errOut.String(), tt.out) {
			t.Errorf("%v: code %d, %q, %q", tt.args, code, out.String(), errOut.String())
		}
	}
}

func TestRunWithoutProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte("server_url: http://127.0.0.1:1\nkind: tart\ntoken_file: t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"run", "--config", path}, &bytes.Buffer{}, &errOut, func(string) string { return "" }); code != 1 || !strings.Contains(errOut.String(), "no provider of kind tart") {
		t.Fatalf("code %d, %q", code, errOut.String())
	}
}
