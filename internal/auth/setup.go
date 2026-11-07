package auth

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// A setup link makes the first Owner of an organization on a self-hosted
// server, in place of forecrew user add. The operator creates it on the
// server (forecrew setup-link) and opens it in a browser: the token travels
// in the URL fragment, which no server log or Referer header sees, and the
// setup page posts it to POST /auth/setup, which puts it in a short-lived
// cookie for the sign-in. Whoever signs in next through an instance
// connection becomes the Owner and the link is spent: it works once, lasts at
// most MaxSetupLinkTTL and is stored only as its SHA-256.
const (
	// PrefixSetupLink starts a setup link's token.
	PrefixSetupLink = "fcu_"
	// DefaultSetupLinkTTL and MaxSetupLinkTTL bound how long a link lives.
	DefaultSetupLinkTTL = time.Hour
	MaxSetupLinkTTL     = 24 * time.Hour

	setupCookie    = "fc_setup"
	setupCookieTTL = 15 * time.Minute
	// maxSetupBody bounds the request of POST /auth/setup.
	maxSetupBody = 1 << 10
)

// ErrSetupTTL means the lifetime asked for is out of range.
var ErrSetupTTL = errors.New("the setup link lifetime must be between a minute and 24 hours")

// SetupURL is the link of a setup token. The token is in the fragment so it
// never reaches an access log.
func SetupURL(baseURL, token string) string {
	return strings.TrimRight(baseURL, "/") + "/setup-link#" + token
}

// CreateSetupLink stores a link that makes an Owner of the organization and
// returns its token, which exists nowhere else: callers print it once and
// never log or audit it.
func CreateSetupLink(ctx context.Context, st store.Store, accountID uuid.UUID, createdBy string, ttl time.Duration, now time.Time) (string, store.SetupLink, error) {
	if ttl < time.Minute || ttl > MaxSetupLinkTTL {
		return "", store.SetupLink{}, ErrSetupTTL
	}
	token, err := NewToken(PrefixSetupLink)
	if err != nil {
		return "", store.SetupLink{}, err
	}
	link := store.SetupLink{AccountID: accountID, TokenHash: mcp.HashToken(token), ExpiresAt: now.Add(ttl), CreatedBy: createdBy}
	return token, link, st.SetupLinks().Create(ctx, &link)
}

// setupLink returns the pending link of a token, or the refusal of a sign-in
// that cannot use it.
func (s *Service) setupLink(ctx context.Context, st store.Store, token string) (store.SetupLink, error) {
	link, err := st.SetupLinks().GetByTokenHash(ctx, mcp.HashToken(token))
	if errors.Is(err, store.ErrNotFound) || (err == nil && !link.Pending(s.cfg.Now())) {
		return store.SetupLink{}, refusal(reasonSetupLink)
	}
	return link, err
}

// setupOrganization handles POST /auth/setup: it checks that the token is a
// pending link, without using it, and keeps it in a cookie for the sign-in.
// Every token that does not work gets the same answer.
func (s *Service) setupOrganization(w http.ResponseWriter, r *http.Request) {
	NoStore(w)
	// Only a script of this site sends JSON: a form of another site cannot, so
	// it cannot plant the cookie.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "send application/json", http.StatusUnsupportedMediaType)
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSetupBody)).Decode(&in); err != nil || in.Token == "" {
		writeSetupError(w)
		return
	}
	link, err := s.setupLink(r.Context(), s.cfg.Store, in.Token)
	if err != nil {
		writeSetupError(w)
		return
	}
	// Not worth a cookie once the organization has its first Owner; the
	// sign-in checks again under the lock.
	if open, err := s.cfg.Store.SetupLinks().FirstOwnerOpen(r.Context(), link.AccountID); err != nil || !open {
		writeSetupError(w)
		return
	}
	account, err := s.cfg.Store.Accounts().Get(r.Context(), link.AccountID)
	if err != nil {
		writeSetupError(w)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: setupCookie, Value: in.Token, Path: "/auth/", MaxAge: int(setupCookieTTL.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"organization": account.Name})
}

// writeSetupError is the one answer to every token that does not work; it
// also drops a cookie the browser may still hold from an earlier link.
func writeSetupError(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: setupCookie, Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"This setup link has expired or was already used. Create a new one on the server with forecrew setup-link."}` + "\n"))
}

// linkOfSignIn returns the pending setup link a sign-in carries, nil for none
// or when an invitation already says what the sign-in is for.
func (s *Service) linkOfSignIn(ctx context.Context, st store.Store, inv *domain.Invitation, f flow) (*store.SetupLink, error) {
	if inv != nil || f.Setup == "" {
		return nil, nil
	}
	link, err := s.setupLink(ctx, st, f.Setup)
	return &link, err
}

// afterSetup hands the tokens that waited for an Owner to the one a setup
// link just made.
func (s *Service) afterSetup(ctx context.Context, f flow) {
	if f.Setup == "" {
		return
	}
	if err := AssignTokens(ctx, s.cfg.Store); err != nil {
		s.cfg.Logger.WarnContext(ctx, "assign tokens to the first owner", "error", err)
	}
}

// claimSetupLink spends the link, when there is one, for the user, who becomes
// the first Owner of its organization. A link only ever makes the first
// Owner: with the organization's row locked, it is refused when the
// organization has an Owner or an active member by now, whether another link,
// forecrew user add or an invitation got there first, and a member who signs
// in with a link they carry is not promoted. Joining takes the same lock (Join),
// so the check and the join cannot be raced.
func claimSetupLink(ctx context.Context, tx store.Store, conn domain.Connection, link *store.SetupLink, userID uuid.UUID) error {
	if link == nil {
		return nil
	}
	if conn.AccountID != uuid.Nil() && conn.AccountID != link.AccountID {
		return refusal(reasonSetupLink) // an organization's connection only opens that organization
	}
	if err := tx.Accounts().Lock(ctx, link.AccountID); err != nil {
		return err
	}
	if open, err := tx.SetupLinks().FirstOwnerOpen(ctx, link.AccountID); err != nil {
		return err
	} else if !open {
		return refusal(reasonSetupLink)
	}
	if _, err := tx.SetupLinks().Use(ctx, link.AccountID, link.ID, userID); errors.Is(err, store.ErrConflict) {
		return refusal(reasonSetupLink)
	} else if err != nil {
		return err
	}
	owner, err := tx.Roles().GetByName(ctx, link.AccountID, domain.RoleScopeOrganization, domain.RoleOwner)
	if err != nil {
		return err
	}
	by := domain.UserActor(link.AccountID, userID, domain.Credential{})
	if err := Join(ctx, tx, link.AccountID, userID, domain.SourceManual, []domain.Grant{{RoleID: owner.ID}}, by); err != nil {
		return err
	}
	return audit(ctx, tx, by, "setup_link.used", "setup_link", link.ID, nil)
}
