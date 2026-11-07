package domain

import (
	"strings"
	"testing"
	"uuid"
)

func TestLevelOfAbilities(t *testing.T) {
	all := map[string]bool{}
	for _, a := range Abilities() {
		all[a] = true
	}
	if LevelOf(all) != LevelCode {
		t.Fatal("all abilities")
	}
	read := map[string]bool{AbilityRead: true, AbilityMCP: true, AbilityJSON: true, AbilityEdit: true}
	if LevelOf(read) != LevelRead {
		t.Fatal("read")
	}
	if LevelOf(map[string]bool{AbilityEdit: true, AbilityCommand: true}) != LevelNone {
		t.Fatal("none")
	}
}

func TestModelCheckSpecValidates(t *testing.T) {
	ok := JobSpec{Kind: JobKindModelCheck, ModelCheck: &ModelCheckSpec{Connection: "c"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []JobSpec{
		{Kind: JobKindModelCheck},
		{Kind: JobKindModelCheck, ModelCheck: &ModelCheckSpec{Connection: "c"}, Prompt: "do"},
		{Kind: "other"},
	} {
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "model_check") {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if !slicesHas(JobKinds(), JobKindModelCheck) {
		t.Fatal("kinds")
	}
}

func slicesHas(kinds []JobKind, k JobKind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

func TestAuthorizationCovers(t *testing.T) {
	c := ModelConnection{ID: NewID(), AccountID: NewID(), Adapter: "claude-code", BaseURL: "https://x"}
	p := NewID()
	a := ModelAuthorization{AccountID: c.AccountID, ProjectID: p, ConnectionID: c.ID, Adapter: c.Adapter, BaseURL: c.BaseURL}
	all := a
	all.ProjectID = uuid.Nil()
	moved := c
	moved.BaseURL = "https://y"
	other := c
	other.AccountID = NewID()
	switch {
	case !a.Covers(p, c), a.Covers(NewID(), c), !all.Covers(NewID(), c):
		t.Fatal("projects")
	case a.Covers(p, moved), a.Covers(p, other):
		t.Fatal("another endpoint or organization")
	}
}
