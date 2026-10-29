//go:build integration

package testkit

import "testing"

func TestNewDatabase(t *testing.T) {
	a, b := NewDatabase(t), NewDatabase(t)
	var nameA, nameB string
	if err := a.QueryRow(t.Context(), "SELECT current_database()").Scan(&nameA); err != nil {
		t.Fatal(err)
	}
	if err := b.QueryRow(t.Context(), "SELECT current_database()").Scan(&nameB); err != nil {
		t.Fatal(err)
	}
	if nameA == nameB || nameA[:3] != "fc_" {
		t.Fatalf("databases = %q, %q", nameA, nameB)
	}
}
