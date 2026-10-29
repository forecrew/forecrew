package roles

import "testing"

func TestReadOnly(t *testing.T) {
	if !(Role{}).ReadOnly() {
		t.Error("role without write paths is not read-only")
	}
	if (Role{WritePaths: []string{"**", "!.github/**"}}).ReadOnly() {
		t.Error("role with write paths is read-only")
	}
}
