package jira

import (
	"encoding/json"
	"strings"
	"testing"
)

// doc is Markdown with everything the converters keep.
const doc = "# Export\n\n" +
	"Export *everything*, with **bold**, `code`, ~~old~~ and a [link](https://acme.test/x).\n" +
	"A second line, snake_case and 2 * 3.\n\n" +
	"- one\n- two\n  - nested\n- three\n\n" +
	"1. first\n2. second\n\n" +
	"> quoted\n\n" +
	"```go\nfmt.Println(\"*not bold*\")\n```\n\n" +
	"---\n\n" +
	"End with [**strong link**](https://acme.test/y)."

func TestMarkdownRoundTrips(t *testing.T) {
	for name, conv := range map[string]func(string) string{
		"adf": func(s string) string {
			md, err := adfToMarkdown(markdownToADF(s))
			if err != nil {
				t.Fatal(err)
			}
			return md
		},
		"wiki": func(s string) string { return wikiToMarkdown(markdownToWiki(s)) },
	} {
		for _, in := range []string{doc, "Export *everything*.", "plain", "", "a_b and *c* and \\*x\\* and 2 * 3"} {
			if got := conv(in); got != in {
				t.Errorf("%s round trip of %q:\n%s", name, in, got)
			}
		}
	}
}

func TestWikiToMarkdown(t *testing.T) {
	cases := map[string]string{
		"h2. Title":                              "## Title",
		"*bold* _em_ -gone- +under+ {{c}}":       "**bold** *em* ~~gone~~ under `c`",
		"[Docs|https://d.test] [https://u.test]": "[Docs](https://d.test) [https://u.test](https://u.test)",
		"[~JIRAUSER10100] asked":                 "@JIRAUSER10100 asked",
		"* a\n** b\n# c":                         "- a\n  - b\n\n1. c",
		"{code:java}\nint x;\n{code}":            "```java\nint x;\n```",
		"{noformat}\n*raw*\n{noformat}":          "```\n*raw*\n```",
		"bq. said":                               "> said",
		"{quote}\nq\n{quote}":                    "> q",
		"line one\\\\line two":                   "line one\nline two",
		"{color:red}red{color} text":             "red text",
		"!screen.png|thumbnail! see":             "\\[image: screen.png] see",
		"||h1||h2||\n|a|b|":                      "| h1 | h2 |\n| --- | --- |\n| a | b |",
		"{panel:title=x}\ninside\n{panel}":       "inside",
		"??cite??":                               "cite",
		"[#anchor]":                              "#anchor",
		"e-mail and a - b":                       "e-mail and a - b",
	}
	for in, want := range cases {
		if got := wikiToMarkdown(in); got != want {
			t.Errorf("wikiToMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestADFKeepsUnknownNodes(t *testing.T) {
	raw := `{"type":"doc","version":1,"content":[
		{"type":"panel","attrs":{"panelType":"info"},"content":[{"type":"paragraph","content":[{"type":"text","text":"in a panel"}]}]},
		{"type":"paragraph","content":[
			{"type":"text","text":"hi "},{"type":"mention","attrs":{"id":"5b10","text":"@Ann"}},
			{"type":"text","text":" "},{"type":"emoji","attrs":{"shortName":":smile:","text":"😄"}},
			{"type":"text","text":" "},{"type":"inlineCard","attrs":{"url":"https://card.test"}},
			{"type":"text","text":" "},{"type":"status","attrs":{"text":"DONE"}},
			{"type":"text","text":" "},{"type":"futureThing","content":[{"type":"text","text":"kept"}]}
		]},
		{"type":"mediaSingle","content":[{"type":"media","attrs":{"id":"abc","type":"file"}}]},
		{"type":"expand","attrs":{"title":"More"},"content":[{"type":"paragraph","content":[{"type":"text","text":"hidden"}]}]},
		{"type":"table","content":[
			{"type":"tableRow","content":[{"type":"tableHeader","content":[{"type":"paragraph","content":[{"type":"text","text":"h"}]}]}]},
			{"type":"tableRow","content":[{"type":"tableCell","content":[{"type":"paragraph","content":[{"type":"text","text":"c"}]}]}]}
		]},
		{"type":"heading","attrs":{"level":3},"content":[{"type":"text","text":"T"}]},
		{"type":"orderedList","attrs":{"order":3},"content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"third"}]}]}]}
	]}`
	got, err := adfToMarkdown(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"in a panel", "hi @Ann 😄 https://card.test DONE kept", "abc", "hidden", "| h |", "| c |", "### T", "3. third"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if md, err := adfToMarkdown(json.RawMessage(`"plain text"`)); err != nil || md != "plain text" {
		t.Errorf("string = %q, %v", md, err)
	}
	if md, err := adfToMarkdown(json.RawMessage(`null`)); err != nil || md != "" {
		t.Errorf("null = %q, %v", md, err)
	}
	if _, err := adfToMarkdown(json.RawMessage(`[1]`)); err == nil {
		t.Error("a broken document read")
	}
}

func TestMarkdownToWiki(t *testing.T) {
	got := markdownToWiki("Ask [Ann](https://a.test): *why* {braces} | bars!\n\n- a\n  1. b")
	want := "Ask [Ann|https://a.test]: _why_ \\{braces\\} \\| bars\\!\n\n* a\n*# b"
	if got != want {
		t.Errorf("markdownToWiki = %q, want %q", got, want)
	}
}
