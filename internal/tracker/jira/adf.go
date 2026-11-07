package jira

import (
	"encoding/json"
	"fmt"
)

// adfToMarkdown converts an Atlassian Document Format value, as Jira Cloud
// sends it, to Markdown. A null or empty value is "".
func adfToMarkdown(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil { // a plain string, as some fields are
		return s, nil
	}
	var doc node
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("read a document: %w", err)
	}
	return toMarkdown(&doc), nil
}

// markdownToADF converts Markdown to an ADF document for Jira Cloud.
func markdownToADF(md string) json.RawMessage {
	raw, err := json.Marshal(fromMarkdown(md))
	if err != nil {
		panic("jira: a document does not encode: " + err.Error()) // nodes are plain data
	}
	return raw
}
