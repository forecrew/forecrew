package workerapi

import (
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
)

func TestNewDefaults(t *testing.T) {
	s := New(Config{})
	if g, err := s.cfg.Grant(t.Context(), domain.Job{}); err != nil || g.Connection != "" || g.Env != nil || g.Endpoint != nil {
		t.Fatalf("grant = %+v, %v", g, err)
	}
	if err := s.cfg.Settle(t.Context(), uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	if s.cfg.PollWait != PollWait || s.cfg.PollRecheck != PollRecheck || len(s.cfg.Pools) != len(config.DefaultPools()) {
		t.Fatalf("config = %+v", s.cfg)
	}
}
