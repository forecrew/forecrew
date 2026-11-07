package roles

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/models"
)

type fixedModels struct {
	d   models.Decision
	err error
	got models.Request
}

func (f *fixedModels) Decide(_ context.Context, _ uuid.UUID, r models.Request) (models.Decision, error) {
	f.got = r
	return f.d, f.err
}

func TestCatalogResolveWithModels(t *testing.T) {
	c, err := New(forecrew.Files, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	project := domain.NewID()
	errDown := errors.New("down")
	tests := []struct {
		name    string
		d       models.Decision
		err     error
		runtime string
		model   string
		wantErr error
	}{
		{"connection", models.Decision{Adapter: config.AdapterCodex, Model: "org-model", Tier: config.TierBest}, nil, config.AdapterCodex, "org-model", nil},
		{"waits", models.Decision{Waiting: "no connection"}, nil, domain.RuntimeNone, "", nil},
		{"never configured", models.Decision{Unset: true, Waiting: "x"}, nil, "", "", ErrNotConfigured},
		{"no model", models.Decision{Adapter: config.AdapterCodex}, nil, "", "", ErrNotConfigured},
		{"error", models.Decision{}, errDown, "", "", errDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &fixedModels{d: tt.d, err: tt.err}
			r, err := c.WithModels(m).Resolve(t.Context(), domain.NewID(), project, "developer")
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || r.Runtime != tt.runtime || r.Model != tt.model || m.got.ProjectID != project || m.got.Role != "developer" || m.got.Pool != r.Pool {
				t.Fatalf("role = %+v, %v, request %+v", r, err, m.got)
			}
		})
	}
}

func TestCatalogToolbox(t *testing.T) {
	c, err := New(forecrew.Files, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if box, known := c.Toolbox("developer"); box || !known {
		t.Fatalf("developer = %v %v", box, known)
	}
	if _, known := c.Toolbox("ghost"); known {
		t.Fatal("ghost is known")
	}
	for _, name := range c.Names() {
		if d, _ := c.Definition(name); d.Toolbox != nil {
			if box, _ := c.Toolbox(name); !box {
				t.Fatalf("%s has a toolbox", name)
			}
		}
	}
}

func TestCatalogWritesAndModelNeeds(t *testing.T) {
	c, err := New(forecrew.Files, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Writes("developer") || c.Writes("reviewer") || !c.Writes("ghost") {
		t.Fatal("writes")
	}
	needs := c.ModelNeeds()
	if len(needs) != len(c.Names()) {
		t.Fatalf("needs = %v", needs)
	}
	for _, n := range needs {
		if n.Writes != c.Writes(n.Name) || !n.Tier.Valid() {
			t.Errorf("need %+v", n)
		}
	}
}
