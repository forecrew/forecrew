package aws

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLedger(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	at := func() time.Time { return now }
	l, err := openLedger(dir, at)
	must(t, err)

	must(t, l.start("i-1", 2)) // 2 per minute
	now = now.Add(10 * time.Minute)
	if got := l.spent(); got != 20 {
		t.Fatalf("spent = %v", got)
	}
	must(t, l.start("i-1", 99)) // a retry keeps the first start and price
	if got := l.spent(); got != 20 {
		t.Fatalf("spent after a repeated start = %v", got)
	}
	must(t, l.end("i-1", "i-unknown"))
	now = now.Add(time.Hour)
	if got := l.spent(); got != 20 {
		t.Fatalf("an ended machine costs nothing more: %v", got)
	}

	// A new process reads the file.
	l2, err := openLedger(dir, at)
	must(t, err)
	if got := l2.spent(); got != 20 {
		t.Fatalf("reloaded spent = %v", got)
	}
	info, err := os.Stat(filepath.Join(dir, "spend.json"))
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode())
	}

	// onlyOpen ends what is gone; adopt counts what the ledger lost.
	must(t, l2.adopt("i-2", now.Add(-5*time.Minute), 1))
	must(t, l2.adopt("i-2", now.Add(-50*time.Minute), 1)) // known already
	if got := l2.spent(); got != 25 {
		t.Fatalf("with an adopted machine: %v", got)
	}
	must(t, l2.onlyOpen(map[string]bool{}))
	now = now.Add(time.Hour)
	if got := l2.spent(); got != 25 {
		t.Fatalf("after it was gone: %v", got)
	}
}

func TestLedgerNewMonth(t *testing.T) {
	now := time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)
	l, err := openLedger(t.TempDir(), func() time.Time { return now })
	must(t, err)
	must(t, l.start("i-1", 1))
	must(t, l.start("i-2", 1))
	must(t, l.end("i-2"))
	now = now.Add(2 * time.Hour) // 01:00 on 1 December
	// Only the hour since midnight counts, and last month's closed cost is gone.
	if got := l.spent(); got != 60 {
		t.Fatalf("spent = %v", got)
	}
}

func TestLedgerFiles(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "spend.json"), []byte("{"), 0o600))
	if _, err := openLedger(dir, time.Now); err == nil {
		t.Fatal("a broken ledger was read")
	}
	must(t, os.WriteFile(filepath.Join(dir, "spend.json"), []byte(`{"month":"2026-11","closed":3}`), 0o600))
	l, err := openLedger(dir, time.Now) // no open map in the file
	must(t, err)
	must(t, l.start("i-1", 1))
	if _, err := openLedger(filepath.Join(dir, "spend.json", "x"), time.Now); err == nil {
		t.Fatal("read through a file")
	}
	// A state directory that cannot be written stops a new machine: it must not run unmetered.
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	ro := t.TempDir()
	l3, err := openLedger(ro, time.Now)
	must(t, err)
	must(t, os.Chmod(ro, 0o500))
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if err := l3.start("i-1", 1); err == nil {
		t.Fatal("started without a place to write")
	}
}
