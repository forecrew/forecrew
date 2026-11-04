package gitsha

import (
	"strings"
	"testing"
)

func TestKinds(t *testing.T) {
	tests := []struct {
		s                  string
		full, short, looks bool
	}{
		{strings.Repeat("a", 40), true, false, true},
		{strings.Repeat("a", 64), true, false, true},
		{strings.Repeat("a", 7), false, true, true},
		{strings.Repeat("a", 39), false, true, true},
		{strings.Repeat("a", 6), false, false, false},
		{strings.Repeat("a", 41), false, false, false},
		{strings.Repeat("a", 65), false, false, false},
		{strings.Repeat("A", 40), false, false, false},
		{strings.Repeat("g", 40), false, false, false},
		{"main", false, false, false},
		{"deadbeef", false, true, true},
		{"", false, false, false},
	}
	for _, tt := range tests {
		if IsFull(tt.s) != tt.full || IsShort(tt.s) != tt.short || LooksLikeCommit(tt.s) != tt.looks {
			t.Errorf("%q: full %v short %v looks %v", tt.s, IsFull(tt.s), IsShort(tt.s), LooksLikeCommit(tt.s))
		}
	}
}
