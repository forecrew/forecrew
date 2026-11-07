package oauth_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/oauth"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestDefaultFetchReachesPublicAddressesOnly is the server as forecrewd
// builds it, without a test client: a client ID that names this machine or a
// private network is refused before any connection is made.
func TestDefaultFetchReachesPublicAddressesOnly(t *testing.T) {
	var hits atomic.Int32
	local := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(local.Close)
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "A", Slug: "a"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	u := addMember(t, st, acc.ID, "ana")
	exp := time.Now().Add(time.Hour)
	must(t, st.Sessions().Create(t.Context(), &domain.Session{TokenHash: mcp.HashToken(cookie), UserID: u.ID, IdleExpiresAt: exp, ExpiresAt: exp}))
	logger := slog.New(slog.DiscardHandler)
	h := oauth.New(oauth.Config{Store: st, Auth: auth.New(auth.Config{Store: st, BaseURL: "https://f.example", Logger: logger}), BaseURL: "https://f.example", Logger: logger}).Handler()

	for _, id := range []string{
		local.URL + "/client.json",
		"https://localhost/client.json", "https://127.0.0.1:1/client.json", "https://[::1]/client.json", "https://[::ffff:127.0.0.1]/client.json",
		"https://0.0.0.0/client.json",
		"https://169.254.169.254/latest/meta-data/client.json", "https://10.0.0.1/client.json", "https://192.168.1.1/client.json",
		"https://100.64.0.1/client.json", "https://[fd00::1]/client.json", "https://[fe80::1]/client.json",
	} {
		t.Run(id, func(t *testing.T) {
			q := url.Values{"client_id": {id}, "redirect_uri": {callback}, "response_type": {"code"}, "code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"}}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
			req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: cookie})
			rec := httptest.NewRecorder()
			start := time.Now()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not registered") {
				t.Errorf("status %d", rec.Code)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("took %v: it tried to connect", d)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the local server was reached %d times", n)
	}
}
