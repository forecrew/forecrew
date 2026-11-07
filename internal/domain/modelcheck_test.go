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
	c := ModelConnection{ID: NewID(), AccountID: NewID(), Adapter: "opencode", Vendor: "deepseek", KeyEnv: "DEEPSEEK_API_KEY", BaseURL: "https://x"}
	p := NewID()
	a := AuthorizationOf(c, p, "user:ana")
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
	for name, change := range map[string]func(*ModelConnection){
		"vendor":  func(x *ModelConnection) { x.Vendor = "moonshot" },
		"key_env": func(x *ModelConnection) { x.KeyEnv = "MOONSHOT_API_KEY" },
		"adapter": func(x *ModelConnection) { x.Adapter = "codex" },
	} {
		x := c
		change(&x)
		if a.Covers(p, x) || !MovedEndpoint(c, x) {
			t.Errorf("%s: still covered", name)
		}
	}
	x := c
	x.Models, x.Enabled = map[string]ModelInfo{"m": {}}, true
	if MovedEndpoint(c, x) {
		t.Error("models and enabled moved the endpoint")
	}
}
