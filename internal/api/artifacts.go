package api

import (
	"context"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// ArtifactOpener opens the content of an artifact; *tools.Service
// implements it. Errors are those of mcp.ToolService.
type ArtifactOpener interface {
	OpenArtifact(ctx context.Context, by domain.Actor, id string) (domain.Artifact, io.ReadCloser, error)
}

// inlineTypes are the only content types a browser may display from an
// artifact, with the Content-Type sent for them. Everything else, HTML and
// SVG included, is a download: an artifact is whatever a job produced and
// must never run as a page of forecrewd's origin.
var inlineTypes = map[string]string{
	"text/plain":       "text/plain; charset=utf-8",
	"image/png":        "image/png",
	"image/jpeg":       "image/jpeg",
	"application/json": "application/json",
}

// artifactHeaders sets the headers of an artifact download: inline for the
// safe types, an attachment of unknown type for the rest.
func artifactHeaders(h http.Header, a domain.Artifact) {
	disposition, contentType := "attachment", "application/octet-stream"
	if mt, _, err := mime.ParseMediaType(a.ContentType); err == nil {
		if served, ok := inlineTypes[mt]; ok {
			disposition, contentType = "inline", served
		}
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": path.Base(a.Name)}))
	h.Set("Content-Length", strconv.FormatInt(a.Size, 10))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, no-cache")
}

// artifact streams an artifact to the client.
func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	by, ok := mcp.ActorFromContext(r.Context())
	if !ok {
		s.fail(w, r, errNotAuthenticated, http.StatusUnauthorized, "unauthorized")
		return
	}
	a, body, err := s.cfg.Artifacts.OpenArtifact(r.Context(), by, r.PathValue("id"))
	if err != nil {
		s.failTool(w, r, by, err)
		return
	}
	defer func() { _ = body.Close() }()
	artifactHeaders(w.Header(), a)
	if _, err := io.Copy(w, body); err != nil {
		// The status line is out; all that is left is to cut the connection.
		s.cfg.Logger.WarnContext(r.Context(), "stream artifact", "artifact_id", a.ID, "error", err)
		panic(http.ErrAbortHandler)
	}
}
