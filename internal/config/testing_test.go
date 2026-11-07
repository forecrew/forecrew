package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParseTesting(t *testing.T) {
	c, err := Parse([]byte(validYAML), env(nil))
	if err != nil || len(c.Testing.Projects) != 0 {
		t.Fatalf("absent: %v, %+v", err, c.Testing)
	}
	c, err = Parse([]byte(validYAML+"testing:\n  projects: [app, web]\n"), env(nil))
	if err != nil || !slices.Equal(c.Testing.Projects, []string{"app", "web"}) {
		t.Fatalf("Parse = %v, %+v", err, c.Testing)
	}
}

func TestParseTestingInvalid(t *testing.T) {
	tests := []struct{ name, block, want string }{
		{"empty name", "projects: ['']", `testing.projects: "" must be a project name`},
		{"spaces", "projects: [' app']", `testing.projects: " app" must be a project name`},
		{"twice", "projects: [app, app]", `testing.projects: "app" is listed twice`},
		{"unknown key", "all: true", "field all not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(validYAML+"testing:\n  "+tt.block+"\n"), env(nil))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse = %v, want ErrInvalid with %q", err, tt.want)
			}
		})
	}
}

func TestParseGoalV3(t *testing.T) {
	c, err := Parse([]byte(validYAML), env(nil))
	if err != nil || len(c.GoalV3.Projects) != 0 {
		t.Fatalf("absent: %v, %+v", err, c.GoalV3)
	}
	c, err = Parse([]byte(validYAML+"goal_v3:\n  projects: [app]\n"), env(nil))
	if err != nil || !slices.Equal(c.GoalV3.Projects, []string{"app"}) {
		t.Fatalf("Parse = %v, %+v", err, c.GoalV3)
	}
	for _, block := range []string{"projects: ['']", "projects: [app, app]"} {
		_, err := Parse([]byte(validYAML+"goal_v3:\n  "+block+"\n"), env(nil))
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "goal_v3.projects") {
			t.Errorf("%s: Parse = %v", block, err)
		}
	}
}
