//go:build enterprise

package ee

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// The SCIM 2.0 server (RFC 7643, RFC 7644) is the subset identity providers
// use, written here instead of taken from a library: Okta and Entra ID need
// eq filters, pagination and PATCH with their quirks, and the libraries bring
// a schema engine and a filter grammar for far more than that, while the rules
// that matter (a token reaches only what it provisioned) are ours anyway.

const (
	scimUserSchema     = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimGroupSchema    = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimListSchema     = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimErrorSchema    = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimMaxBody        = 1 << 20
	scimDefaultCount   = 100
	scimMaxCount       = 200
	scimActor          = "scim"
	scimContentType    = "application/scim+json"
	scimDocumentFormat = "2006-01-02T15:04:05Z"
)

//go:embed scim_schemas.json
var scimSchemas []byte

//go:embed scim_resource_types.json
var scimResourceTypes []byte

//go:embed scim_service_provider.json
var scimServiceProvider []byte

// scimError is an answer in the error format of RFC 7644, 3.12.
type scimError struct {
	status int
	kind   string // scimType
	detail string
}

func (e *scimError) Error() string { return e.detail }

func scimBad(kind, format string, args ...any) error {
	return &scimError{http.StatusBadRequest, kind, fmt.Sprintf(format, args...)}
}

func scimNotFound(what string) error {
	return &scimError{http.StatusNotFound, "", what + " not found"}
}

func scimTaken(format string, args ...any) error {
	return &scimError{http.StatusConflict, "uniqueness", fmt.Sprintf(format, args...)}
}

// scimStoreErr answers the store errors a client can act on.
func scimStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrLastOwner):
		return &scimError{http.StatusConflict, "mutability", "this is the last Owner of the organization"}
	case errors.Is(err, store.ErrConflict):
		return scimTaken("the value is taken")
	}
	return err
}

// scimCall is an authenticated request to one organization.
type scimCall struct {
	account uuid.UUID
	by      domain.Actor
	base    string // the organization's SCIM URL
	token   string // name of the token, for the audit log
}

type scimHandler func(http.ResponseWriter, *http.Request, scimCall) error

// SCIM serves /scim/v2/<org-slug>/.
func (s *Service) SCIM() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h scimHandler) { mux.Handle(pattern, s.scimAuth(h)) }
	raw := func(doc []byte) scimHandler {
		return func(w http.ResponseWriter, _ *http.Request, _ scimCall) error {
			w.Header().Set("Content-Type", scimContentType)
			_, err := w.Write(doc)
			return err
		}
	}
	route("GET /scim/v2/{org}/ServiceProviderConfig", raw(scimServiceProvider))
	route("GET /scim/v2/{org}/Schemas", listDocuments(scimSchemas))
	route("GET /scim/v2/{org}/Schemas/{id}", oneDocument(scimSchemas))
	route("GET /scim/v2/{org}/ResourceTypes", listDocuments(scimResourceTypes))
	route("GET /scim/v2/{org}/ResourceTypes/{id}", oneDocument(scimResourceTypes))
	route("GET /scim/v2/{org}/Users", s.listUsers)
	route("POST /scim/v2/{org}/Users", s.createUser)
	route("GET /scim/v2/{org}/Users/{id}", s.getUser)
	route("PUT /scim/v2/{org}/Users/{id}", s.replaceUser)
	route("PATCH /scim/v2/{org}/Users/{id}", s.patchUser)
	route("DELETE /scim/v2/{org}/Users/{id}", s.deleteUser)
	route("GET /scim/v2/{org}/Groups", s.listGroups)
	route("POST /scim/v2/{org}/Groups", s.createGroup)
	route("GET /scim/v2/{org}/Groups/{id}", s.getGroup)
	route("PUT /scim/v2/{org}/Groups/{id}", s.replaceGroup)
	route("PATCH /scim/v2/{org}/Groups/{id}", s.patchGroup)
	route("DELETE /scim/v2/{org}/Groups/{id}", s.deleteGroup)
	mux.HandleFunc("/scim/v2/", func(w http.ResponseWriter, _ *http.Request) {
		writeSCIMError(w, &scimError{http.StatusNotFound, "", "no such endpoint"})
	})
	return mux
}

// scimAuth finds the organization of the path and checks the bearer token
// against it. An unknown organization answers like a bad token, so the
// endpoint does not tell which slugs exist.
func (s *Service) scimAuth(next scimHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := s.scimCaller(r)
		if err == nil {
			err = next(w, r, c)
		}
		if err == nil {
			return
		}
		e, ok := errors.AsType[*scimError](err)
		if !ok {
			s.log.ErrorContext(r.Context(), "scim call failed", "method", r.Method, "path", r.URL.Path, "error", err)
			e = &scimError{http.StatusInternalServerError, "", "internal error, try again later"}
		}
		if e.status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}
		writeSCIMError(w, e)
	})
}

func (s *Service) scimCaller(r *http.Request) (scimCall, error) {
	denied := &scimError{http.StatusUnauthorized, "", "invalid or revoked token"}
	scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return scimCall{}, denied
	}
	ctx := r.Context()
	t, err := s.st.SCIMTokens().GetByTokenHash(ctx, mcp.HashToken(strings.TrimSpace(secret)))
	if errors.Is(err, store.ErrNotFound) || (err == nil && !t.RevokedAt.IsZero()) {
		return scimCall{}, denied
	}
	if err != nil {
		return scimCall{}, err
	}
	a, err := s.st.Accounts().GetBySlug(ctx, r.PathValue("org"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && a.ID != t.AccountID) {
		return scimCall{}, denied
	}
	if err != nil {
		return scimCall{}, err
	}
	if err := s.st.SCIMTokens().Touch(ctx, a.ID, t.ID); err != nil {
		s.log.WarnContext(ctx, "touch scim token", "account_id", a.ID, "error", err)
	}
	return scimCall{account: a.ID, by: domain.SystemActor(a.ID, scimActor), base: s.public + "/scim/v2/" + url.PathEscape(a.Slug), token: t.Name}, nil
}

func writeSCIM(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", scimContentType)
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

func writeSCIMError(w http.ResponseWriter, e *scimError) {
	body := map[string]any{"schemas": []string{scimErrorSchema}, "status": strconv.Itoa(e.status), "detail": e.detail}
	if e.kind != "" {
		body["scimType"] = e.kind
	}
	_ = writeSCIM(w, e.status, body)
}

// readSCIM decodes the body; attributes Forecrew does not keep are ignored.
func readSCIM(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, scimMaxBody))
	if err != nil {
		return &scimError{http.StatusRequestEntityTooLarge, "", "the body is too large"}
	}
	if err := json.Unmarshal(body, v); err != nil {
		return scimBad("invalidSyntax", "the body is not valid: %v", err)
	}
	return nil
}

// scimList is the ListResponse of RFC 7644, 3.4.2.
type scimList struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    any      `json:"Resources"`
}

// page cuts items by startIndex and count, which RFC 7644, 3.4.2.4 says to
// read leniently: below 1 is 1, below 0 is 0.
func page[T any](r *http.Request, items []T) scimList {
	start, count := 1, scimDefaultCount
	if v, err := strconv.Atoi(r.URL.Query().Get("startIndex")); err == nil && v > 1 {
		start = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("count")); err == nil {
		count = min(max(v, 0), scimMaxCount)
	}
	from := min(start-1, len(items))
	to := min(from+count, len(items))
	got := items[from:to]
	if got == nil {
		got = []T{}
	}
	return scimList{Schemas: []string{scimListSchema}, TotalResults: len(items), StartIndex: start, ItemsPerPage: len(got), Resources: got}
}

func listDocuments(doc []byte) scimHandler {
	return func(w http.ResponseWriter, r *http.Request, _ scimCall) error {
		var all []json.RawMessage
		if err := json.Unmarshal(doc, &all); err != nil {
			return err
		}
		return writeSCIM(w, http.StatusOK, page(r, all))
	}
}

func oneDocument(doc []byte) scimHandler {
	return func(w http.ResponseWriter, r *http.Request, _ scimCall) error {
		var all []map[string]any
		if err := json.Unmarshal(doc, &all); err != nil {
			return err
		}
		i := slices.IndexFunc(all, func(d map[string]any) bool { return d["id"] == r.PathValue("id") })
		if i < 0 {
			return scimNotFound("document")
		}
		return writeSCIM(w, http.StatusOK, all[i])
	}
}

var scimFilterRE = regexp.MustCompile(`(?i)^\s*([a-z.]+)\s+eq\s+"((?:[^"\\]|\\.)*)"\s*$`)

// scimFilter is one "attribute eq value", the only filter identity providers
// send on these resources. Attribute is lowercase; empty means no filter.
type scimFilter struct{ attribute, value string }

// parseFilter reads the filter of the request; allowed are the attributes
// that may be filtered on, in lowercase.
func parseFilter(r *http.Request, allowed ...string) (scimFilter, error) {
	raw := r.URL.Query().Get("filter")
	if raw == "" {
		return scimFilter{}, nil
	}
	m := scimFilterRE.FindStringSubmatch(raw)
	if m == nil || !slices.Contains(allowed, strings.ToLower(m[1])) {
		return scimFilter{}, scimBad("invalidFilter", "the filter must be one of %s eq \"value\"", strings.Join(allowed, ", "))
	}
	var value string
	if err := json.Unmarshal([]byte(`"`+m[2]+`"`), &value); err != nil {
		return scimFilter{}, scimBad("invalidFilter", "the filter value is not a string")
	}
	return scimFilter{strings.ToLower(m[1]), value}, nil
}

// scimMeta is the common meta attribute.
type scimMeta struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created"`
	LastModified string `json:"lastModified"`
	Location     string `json:"location"`
}

func metaOf(kind, location string, created, modified time.Time) *scimMeta {
	return &scimMeta{ResourceType: kind, Created: created.UTC().Format(scimDocumentFormat), LastModified: modified.UTC().Format(scimDocumentFormat), Location: location}
}

// scimBool is a boolean that Entra ID may send as the string "False".
type scimBool bool

// UnmarshalJSON implements json.Unmarshaler.
func (b *scimBool) UnmarshalJSON(data []byte) error {
	v, err := strconv.ParseBool(strings.ToLower(strings.Trim(string(data), `"`)))
	if err != nil {
		return fmt.Errorf("%s is not a boolean", data)
	}
	*b = scimBool(v)
	return nil
}

// patchOp is one operation of a PatchOp. Okta writes op in lowercase, Entra
// ID capitalised.
type patchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// patchBody is the body of a PATCH.
type patchBody struct {
	Operations []patchOp `json:"Operations"`
}

// read decodes the operations and normalizes op and the path.
func (p *patchBody) read(w http.ResponseWriter, r *http.Request, urn string) error {
	if err := readSCIM(w, r, p); err != nil {
		return err
	}
	if len(p.Operations) == 0 {
		return scimBad("invalidSyntax", "a PATCH needs Operations")
	}
	for i := range p.Operations {
		o := &p.Operations[i]
		o.Op = strings.ToLower(o.Op)
		if !slices.Contains([]string{"add", "replace", "remove"}, o.Op) {
			return scimBad("invalidSyntax", "unknown op %q", o.Op)
		}
		if len(o.Path) > len(urn) && strings.EqualFold(o.Path[:len(urn)+1], urn+":") {
			o.Path = o.Path[len(urn)+1:]
		}
	}
	return nil
}

// byCreated orders resources so that pages stay stable.
func byCreated[T any](items []T, created func(T) time.Time, id func(T) uuid.UUID) {
	slices.SortFunc(items, func(a, b T) int {
		return cmp.Or(created(a).Compare(created(b)), cmp.Compare(id(a).String(), id(b).String()))
	})
}

// inTx runs fn in a transaction.
func (s *Service) inTx(ctx context.Context, fn func(ctx context.Context, tx store.Store) error) error {
	return scimStoreErr(s.st.WithTx(ctx, fn))
}
