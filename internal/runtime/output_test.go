package runtime

import (
	"errors"
	"strings"
	"testing"
)

const testSchema = `{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}},"additionalProperties":false}`

func TestExtractOutput(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		schema  string
		want    string
		wantErr string
	}{
		{"fenced", "done\n```json\n{\"summary\":\"ok\"}\n```\n", testSchema, `{"summary":"ok"}`, ""},
		{"last block wins", "```json\n{\"summary\":1}\n```\nfix\n```json\n{\"summary\":\"b\"}\n```", testSchema, `{"summary":"b"}`, ""},
		{"bare json", ` {"summary":"ok"} `, testSchema, `{"summary":"ok"}`, ""},
		{"free text", "anything", "", "", ""},
		{"missing", "I finished the work.", testSchema, "", "no JSON block"},
		{"broken json", "```json\n{\"summary\": \n```", testSchema, "", "not valid JSON"},
		{"wrong type", "```json\n{\"summary\":3}\n```", testSchema, "", "summary"},
		{"missing field", "```json\n{}\n```", testSchema, "", "summary"},
		{"extra field", "```json\n{\"summary\":\"a\",\"x\":1}\n```", testSchema, "", "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractOutput(tt.text, []byte(tt.schema))
			if tt.wantErr != "" {
				var ioe *InvalidOutputError
				if !errors.Is(err, ErrInvalidOutput) || !errors.As(err, &ioe) {
					t.Fatalf("err = %v, want InvalidOutputError", err)
				}
				if !strings.Contains(ioe.Message, tt.wantErr) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("message %q does not contain %q", ioe.Message, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestExtractOutputBadSchema(t *testing.T) {
	for _, s := range []string{`{`, `{"type":"nope"}`} {
		_, err := ExtractOutput("```json\n{}\n```", []byte(s))
		if err == nil || errors.Is(err, ErrInvalidOutput) {
			t.Errorf("schema %s: err = %v, want a schema error", s, err)
		}
	}
}
