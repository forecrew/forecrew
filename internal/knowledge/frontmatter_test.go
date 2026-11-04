package knowledge

import "testing"

func TestSplitFrontmatter(t *testing.T) {
	tests := []struct {
		name, in, front, body string
		hasFront              bool
	}{
		{"none", "# T\n", "", "# T\n", false},
		{"empty", "---\n---\nbody\n", "", "body\n", true},
		{"crlf", "---\r\ntitle: x\r\n---\r\nbody\r\n", "title: x\n", "body\n", true},
		{"at end", "---\ntitle: x\n---", "title: x\n", "", true},
	}
	for _, tt := range tests {
		front, body, err := splitFrontmatter([]byte(tt.in))
		if err != nil || (front != nil) != tt.hasFront || string(front) != tt.front || string(body) != tt.body {
			t.Errorf("%s: front %q, body %q, %v", tt.name, front, body, err)
		}
	}
}
