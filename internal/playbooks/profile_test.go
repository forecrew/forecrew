package playbooks_test

import (
	"maps"
	"strings"
	"testing"
)

func TestValidateProfile(t *testing.T) {
	valid := map[string]any{"version": 1, "name": "app", "summary": "An app."}
	invalid := map[string]any{"version": 2, "name": "app", "summary": "An app."}
	integration := map[string]any{
		"tool": "jira", "uses": []any{"tracker"}, "confidence": "high", "status": "proposed",
		"evidence": []any{map[string]any{"kind": "autolink", "location": "github:autolinks", "count": 1}},
	}
	withIntegration := map[string]any{"version": 1, "name": "app", "summary": "An app.", "integrations": []any{integration}}
	badTool := maps.Clone(integration)
	badTool["tool"] = "github-issues"
	withBadTool := map[string]any{"version": 1, "name": "app", "summary": "An app.", "integrations": []any{badTool}}
	tests := []struct {
		name      string
		with      map[string]any
		wantValid bool
		wantErr   string
	}{
		{"valid", map[string]any{"profile": valid}, true, ""},
		{"valid on the last attempt", map[string]any{"profile": valid, "attempt": 2, "attempts": 2}, true, ""},
		{"invalid, can retry", map[string]any{"profile": invalid, "attempt": 1, "attempts": 2}, false, ""},
		{"invalid, no attempt limit", map[string]any{"profile": invalid}, false, ""},
		{"invalid on the last attempt", map[string]any{"profile": invalid, "attempt": 2, "attempts": 2}, false, "still invalid after 2 attempts"},
		{"integrations", map[string]any{"profile": withIntegration}, true, ""},
		{"integration of an unknown tool", map[string]any{"profile": withBadTool}, false, ""},
		{"no profile", map[string]any{"profile": nil}, false, ""},
		{"nothing to check", map[string]any{}, true, ""},
		{"unknown key", map[string]any{"profile": valid, "strict": true}, false, "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t)
			out, err := v.call(t, "profile.validate", tt.with)
			if tt.wantErr != "" {
				wantErr(t, err, tt.wantErr)
				return
			}
			must(t, err)
			m := toMap(t, out)
			if m["valid"] != tt.wantValid {
				t.Fatalf("output = %v, want valid %v", m, tt.wantValid)
			}
			if msg, _ := m["errors"].(string); (msg == "") != tt.wantValid {
				t.Errorf("errors = %q", msg)
			}
		})
	}
}

func TestValidateProfileKeepsErrors(t *testing.T) {
	v := newEnv(t)
	_, err := v.call(t, "profile.validate", map[string]any{
		"profile": map[string]any{"version": 1, "summary": "x"}, "attempt": 2, "attempts": 2,
	})
	wantErr(t, err, "name")
}

func TestProfileFeedback(t *testing.T) {
	tests := []struct {
		name   string
		human  string
		errors string
		want   []string
	}{
		{"nothing", "", "", nil},
		{"blank", "  \n", " ", nil},
		{"reviewer", "Add the web component.", "", []string{"reviewer asked", "Add the web component."}},
		{"validator", "", "name is missing", []string{"does not match the profile schema", "name is missing"}},
		{"both", "Shorter.", "name is missing", []string{"Shorter.", "name is missing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t)
			out, err := v.call(t, "profile.feedback", map[string]any{"human": tt.human, "errors": tt.errors})
			must(t, err)
			got, _ := out.(string)
			if len(tt.want) == 0 && got != "" {
				t.Errorf("feedback = %q, want empty", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("feedback = %q, want it to contain %q", got, w)
				}
			}
		})
	}
	_, err := newEnv(t).call(t, "profile.feedback", map[string]any{"other": 1})
	wantErr(t, err, "unknown field")
}

func TestValidateProfileFile(t *testing.T) {
	const good = `{"version": 1, "name": "shop", "summary": "x"}`
	tests := []struct {
		name    string
		file    string // empty means no file
		with    map[string]any
		want    string // in the errors; empty means valid
		wantErr string
	}{
		{"valid file", good, nil, "", ""},
		{"invalid file", `{"version": 7, "name": "shop", "summary": "x"}`, nil, ".forecrew/profile.yaml at abc", ""},
		{"missing file", "", nil, "does not exist at abc", ""},
		{"invalid file and output", `{"version": 7}`, map[string]any{"profile": map[string]any{"version": 2}}, "the profile in your answer", ""},
		{"invalid file, last attempt", `{"version": 7}`, map[string]any{"attempt": 2, "attempts": 2}, "", "still invalid after 2 attempts"},
		{"invalid file, single attempt", `{"version": 7}`, map[string]any{"attempt": 1, "attempts": 1}, "", "still invalid after 1 attempts: .forecrew/profile.yaml at abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t)
			if tt.file != "" {
				must(t, v.tracker.SetFile("acme/app", "abc", ".forecrew/profile.yaml", []byte(tt.file)))
			}
			with := map[string]any{"repo": "acme/app", "ref": "abc"}
			maps.Copy(with, tt.with)
			out, err := v.call(t, "profile.validate", with)
			if tt.wantErr != "" {
				wantErr(t, err, tt.wantErr)
				return
			}
			must(t, err)
			m := toMap(t, out)
			if msg, _ := m["errors"].(string); m["valid"] != (tt.want == "") || !strings.Contains(msg, tt.want) {
				t.Errorf("output = %v, want errors containing %q", m, tt.want)
			}
		})
	}
}

func TestValidateProfileFileReadError(t *testing.T) {
	v := newEnv(t)
	v.wrapped = &failingFiles{Tracker: v.tracker, n: 1}
	v.register()
	_, err := v.call(t, "profile.validate", map[string]any{"repo": "acme/app", "ref": "abc"})
	wantErr(t, err, "connection reset")
}
