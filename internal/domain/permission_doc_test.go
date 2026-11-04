package domain

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestPermissionsDoc keeps docs/contracts/permissions.md in step with the
// catalog and the built-in roles: the catalog table lists every permission
// with its class and scope, and the role table marks exactly the
// permissions of each built-in role.
func TestPermissionsDoc(t *testing.T) {
	data, err := os.ReadFile("../../docs/contracts/permissions.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	catalog := section(t, doc, "## Catalog")
	roles := section(t, doc, "## Built-in roles")
	builtIn := BuiltInRoles()
	for _, p := range Permissions() {
		row, ok := tableRow(catalog, p)
		if !ok {
			t.Errorf("catalog table lacks %s", p)
			continue
		}
		where := "organization"
		if p.ProjectScoped() {
			where = "project"
		}
		if len(row) < 3 || row[1] != string(p.Scope()) || row[2] != where {
			t.Errorf("catalog row of %s = %v, want class %s on %s", p, row, p.Scope(), where)
		}
		row, ok = tableRow(roles, p)
		if !ok || len(row) != len(builtIn)+1 {
			t.Errorf("role table row of %s = %v", p, row)
			continue
		}
		for i, r := range builtIn {
			if got := row[i+1] == "x"; got != r.Has(p) {
				t.Errorf("role table: %s %s has %s = %v, want %v", r.Scope, r.Name, p, got, r.Has(p))
			}
		}
	}
}

// section returns the text from heading to the next heading of its level.
func section(t *testing.T, doc, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(doc, "\n"+heading+"\n")
	if !ok {
		t.Fatalf("no %q", heading)
	}
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// tableRow returns the trimmed cells of the table row that starts with the
// permission in backquotes.
func tableRow(text string, p Permission) ([]string, bool) {
	for line := range strings.SplitSeq(text, "\n") {
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) > 1 && cells[0] == "`"+string(p)+"`" {
			return slices.Clip(cells), true
		}
	}
	return nil, false
}
