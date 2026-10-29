package store

import "testing"

func TestName(t *testing.T) {
	if Name() == "" {
		t.Fatal("empty name")
	}
}
