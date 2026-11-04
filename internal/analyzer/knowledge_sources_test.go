package analyzer

import (
	"errors"
	"reflect"
	"testing"
)

func TestKnowledgeSourcesOf(t *testing.T) {
	files := []string{"CODEOWNERS", "CONTRIBUTING.md", "doc/adr/0001.md", "docs/decisions/0002.md", "src/a.go"}
	read := func(name string) ([]byte, error) {
		if name == ".adr-dir" {
			return []byte("doc/adr\n"), nil
		}
		return nil, errors.New("unexpected read " + name)
	}
	got, err := KnowledgeSourcesOf(append(files, ".adr-dir"), read)
	if err != nil {
		t.Fatal(err)
	}
	want := []KnowledgeSource{
		{Kind: "adr", Path: "doc/adr", Section: "architecture"},
		{Kind: "adr", Path: "docs/decisions", Section: "architecture"},
		{Kind: "contributing", Path: "CONTRIBUTING.md", Section: "conventions"},
		{Kind: "codeowners", Path: "CODEOWNERS", Section: "owners"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sources = %+v, want %+v", got, want)
	}
	if _, err := KnowledgeSourcesOf([]string{".adr-dir"}, func(string) ([]byte, error) { return nil, errors.New("boom") }); err == nil {
		t.Error("a failing read was ignored")
	}
}
