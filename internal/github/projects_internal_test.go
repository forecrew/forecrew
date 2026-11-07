package github

import (
	"net/url"
	"testing"
)

func TestGraphQLURL(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.github.com/":     "https://api.github.com/graphql",
		"https://ghe.example/api/v3/": "https://ghe.example/api/graphql",
		"https://ghe.example/api/v3":  "https://ghe.example/api/graphql",
		"http://127.0.0.1:8080/":      "http://127.0.0.1:8080/graphql",
	} {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		if got := (&Client{base: u}).graphqlURL(); got != want {
			t.Errorf("graphqlURL(%q) = %q, want %q", base, got, want)
		}
	}
}
