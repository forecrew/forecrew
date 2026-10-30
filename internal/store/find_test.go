package store_test

import (
	"errors"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/store"
)

type item struct {
	id      uuid.UUID
	name    string
	revoked bool
}

func info(i item) (uuid.UUID, string, bool) { return i.id, i.name, i.revoked }

func TestFindActive(t *testing.T) {
	old, cur := item{uuid.New(), "laptop", true}, item{uuid.New(), "laptop", false}
	other := item{uuid.New(), "ci", false}
	items := []item{old, cur, other}
	tests := []struct {
		key  string
		want item
		err  error
	}{
		{"laptop", cur, nil},
		{cur.id.String(), cur, nil},
		{"ci", other, nil},
		{old.id.String(), item{}, store.ErrNotFound},
		{"ghost", item{}, store.ErrNotFound},
	}
	for _, tt := range tests {
		got, err := store.FindActive(tt.key, items, info)
		if !errors.Is(err, tt.err) || got != tt.want {
			t.Errorf("FindActive(%q) = %v, %v; want %v, %v", tt.key, got, err, tt.want, tt.err)
		}
	}
}
