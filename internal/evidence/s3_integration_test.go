//go:build integration

package evidence_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/testkit"
)

// The S3 store against a real Garage, started with the Compose stack's own
// init script.
func TestS3Garage(t *testing.T) {
	cfg := testkit.NewGarage(t)
	s, err := evidence.NewS3(cfg)
	must(t, err)
	ctx := t.Context()
	key := evidence.JobKey(domain.NewID(), domain.NewID(), "reports/unit.xml")

	// A body larger than one part.
	big := make([]byte, 20<<20+123)
	_, err = rand.Read(big)
	must(t, err)
	for _, name := range []string{"first", "overwrite"} {
		must(t, s.Put(ctx, key, bytes.NewReader(big), int64(len(big)), "application/octet-stream"))
		rc, err := s.Get(ctx, key)
		must(t, err)
		got, err := io.ReadAll(rc)
		must(t, err)
		must(t, rc.Close())
		if !bytes.Equal(got, big) {
			t.Fatalf("%s: read back %d bytes, differs from the %d written", name, len(got), len(big))
		}
	}

	// A reader that fails halfway stores nothing under a new key.
	other := evidence.JobKey(domain.NewID(), domain.NewID(), "broken.log")
	boom := errors.New("boom")
	if err := s.Put(ctx, other, io.MultiReader(bytes.NewReader(big[:1000]), errReader{boom}), 5000, ""); err == nil {
		t.Fatal("a failing reader was accepted")
	}
	if _, err := s.Get(ctx, other); !errors.Is(err, evidence.ErrNotFound) {
		t.Fatalf("object after a failed put: %v", err)
	}

	// Overwriting replaces; deleting is idempotent.
	must(t, s.Put(ctx, key, bytes.NewReader([]byte("v2")), 2, "text/plain"))
	rc, err := s.Get(ctx, key)
	must(t, err)
	if got, _ := io.ReadAll(rc); string(got) != "v2" {
		t.Fatalf("after overwrite: %q", got)
	}
	_ = rc.Close()
	must(t, s.Delete(ctx, key))
	must(t, s.Delete(ctx, key))
	if _, err := s.Get(ctx, key); !errors.Is(err, evidence.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}

	// Wrong credentials are refused, and the error does not carry them.
	bad := *cfg
	bad.SecretAccessKey = "0000000000000000000000000000000000000000000000000000000000000000"
	b, err := evidence.NewS3(&bad)
	must(t, err)
	err = b.Put(ctx, key, bytes.NewReader([]byte("x")), 1, "")
	if err == nil || bytes.Contains([]byte(err.Error()), []byte(bad.SecretAccessKey)) || bytes.Contains([]byte(err.Error()), []byte(cfg.SecretAccessKey)) {
		t.Fatalf("put with a wrong secret: %v", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
