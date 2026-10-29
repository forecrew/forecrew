package runtime

import (
	"strings"
	"testing"
)

func TestRedactor(t *testing.T) {
	r := NewRedactor(map[string]string{
		"API_KEY": "super-secret-value",
		"SHORT":   "abc",
		"LONGER":  "super-secret-value-extended",
		"QUOTED":  `pa"ss\word`,
	})
	tests := []struct {
		name, in, want string
	}{
		{"env value", "key is super-secret-value ok", "key is [REDACTED] ok"},
		{"longest first", "x super-secret-value-extended y", "x [REDACTED] y"},
		{"short values kept", "abc stays", "abc stays"},
		{"anthropic key", "sk-ant-api03-AbCdEfGh12345", "[REDACTED]"},
		{"github token", "token ghs_" + strings.Repeat("a", 36), "token [REDACTED]"},
		{"github pat", "github_pat_" + strings.Repeat("B", 30), "[REDACTED]"},
		{"forecrew token", "fcw_" + strings.Repeat("z", 43), "[REDACTED]"},
		{"bearer", "Authorization: Bearer abcdefghijklmnop1234", "Authorization: [REDACTED]"},
		{"private key", "a -----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY----- b", "a [REDACTED] b"},
		{"json escaped", `{"k":"pa\"ss\\word"}`, `{"k":"[REDACTED]"}`},
		{"raw quoted", `pa"ss\word`, "[REDACTED]"},
		{"clean", "nothing to hide", "nothing to hide"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.String(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
