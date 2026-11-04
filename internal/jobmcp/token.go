package jobmcp

import (
	"crypto/rand"
	"encoding/base64"
	"strings"

	"github.com/forecrew/forecrew/internal/mcp"
)

// Path is where forecrewd serves the gateway.
const Path = "/job-mcp"

// TokenPrefix starts every job MCP token.
const TokenPrefix = "fcj_"

// NewToken returns a fresh job MCP token, fcj_ and 43 base64url
// characters (256 random bits), and the SHA-256 under which it is stored.
// The token itself is never stored.
func NewToken() (token string, hash []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (it crashes the program instead)
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, mcp.HashToken(token)
}

// validToken reports whether s has the shape of a job MCP token, so other
// bearer tokens are refused without a database lookup.
func validToken(s string) bool {
	body, ok := strings.CutPrefix(s, TokenPrefix)
	if !ok || len(body) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(body)
	return err == nil
}
