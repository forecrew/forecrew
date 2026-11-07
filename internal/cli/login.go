package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/spf13/cobra"

	"github.com/forecrew/forecrew/internal/api"
)

// loginTimeout is how long forecrew login waits for the browser.
const loginTimeout = 5 * time.Minute

// randomString returns n random bytes as base64url.
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// secureServer refuses to send credentials over plain http to another
// machine.
func secureServer(server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("bad server URL %q", server)
	}
	if u.Scheme == "http" && !slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname()) {
		return fmt.Errorf("refusing to log in over plain http to %s: use an https URL", u.Host)
	}
	return nil
}

// loginFlags are those of the commands that sign in.
type loginFlags struct {
	server    string
	org       string
	noBrowser bool
}

func (f *loginFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.server, "server", "", "base URL of forecrewd; default "+EnvURL)
	cmd.Flags().StringVar(&f.org, "org", "", "slug of the organization to log in to; your current one in the browser by default")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the address instead of opening a browser")
}

// loginCommand signs in through the browser and keeps a personal token.
func loginCommand(r *remote) *cobra.Command {
	var f loginFlags
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in through your browser and keep a personal token for this machine",
		Long: "Opens your browser to allow this machine, then keeps a personal token named after it in the OS keychain, " +
			"or in a file only you can read. On a machine without a browser, open the address on another one and paste the code it shows.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return r.login(cmd, f) },
	}
	f.bind(cmd)
	return cmd
}

// listenForCallback serves the redirect of the browser on a free port of
// 127.0.0.1 and returns its address and where the code arrives.
func listenForCallback(ctx context.Context, state string) (addr string, got <-chan string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, fmt.Errorf("listen on the loopback address: %w", err)
	}
	ch := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 || q.Get("code") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "<!doctype html><title>Forecrew</title><p>That was not the login in progress. Start again with forecrew login.")
			return
		}
		_, _ = io.WriteString(w, "<!doctype html><title>Forecrew</title><p>You can close this window and go back to the terminal.")
		select {
		case ch <- q.Get("code"):
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), ch, func() { _ = srv.Close() }, nil
}

// pasted reads a code, or the whole address the browser was sent to, from in.
func pasted(in io.Reader) <-chan string {
	ch := make(chan string, 1)
	if in == nil {
		return ch
	}
	go func() {
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if u, err := url.Parse(line); err == nil && u.Query().Get("code") != "" {
				line = u.Query().Get("code")
			}
			if line != "" {
				ch <- line
				return
			}
		}
	}()
	return ch
}

// login runs the loopback login with PKCE and stores the token.
func (r *remote) login(cmd *cobra.Command, f loginFlags) error {
	ctx := cmd.Context()
	server := strings.TrimRight(cmpOr(f.server, r.url), "/")
	if err := secureServer(server); err != nil {
		return err
	}
	creds, ok := newCredStore(r.env)
	if !ok {
		return errors.New("there is no config directory to keep the login in")
	}
	verifier, err := randomString(32)
	if err != nil {
		return err
	}
	state, err := randomString(16)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(verifier))
	addr, got, stop, err := listenForCallback(ctx, state)
	if err != nil {
		return err
	}
	defer stop()
	host := "this machine"
	if r.env.Hostname != nil {
		if h, err := r.env.Hostname(); err == nil && h != "" {
			host = h
		}
	}
	q := url.Values{"redirect_uri": {"http://" + addr + "/callback"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "host": {host}}
	if f.org != "" {
		q.Set("organization", f.org)
	}
	authURL := server + "/cli-login?" + q.Encode()
	opened := false
	if !f.noBrowser && r.env.OpenBrowser != nil {
		opened = r.env.OpenBrowser(authURL) == nil
	}
	if opened {
		cmd.Println("Your browser should open to allow this machine. If it does not, open:")
	} else {
		cmd.Println("Open this address in your browser to allow this machine:")
	}
	cmd.Println(authURL)
	cmd.Println("Waiting. On a machine without a browser, paste the code the page shows and press Enter.")

	code, err := waitForCode(ctx, got, pasted(r.env.Stdin))
	if err != nil {
		return err
	}
	out, err := r.exchange(ctx, server, api.CLITokenInput{Code: code, CodeVerifier: verifier, Name: "cli-" + host})
	if err != nil {
		return err
	}
	if err := creds.keep(server, out); err != nil {
		return fmt.Errorf("keep the login: %w", err)
	}
	cmd.Printf("Logged in to %s as %s on %s, with the token %s.\n", server, safeText(out.User.Name), safeText(out.Account.Name), out.Token.Name)
	return nil
}

// waitForCode waits for the code from the browser, pasted, or the timeout.
func waitForCode(ctx context.Context, got, paste <-chan string) (string, error) {
	select {
	case code := <-got:
		return code, nil
	case code := <-paste:
		return code, nil
	case <-time.After(loginTimeout):
		return "", errors.New("timed out waiting for the browser: run forecrew login again")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// exchange redeems the code of a login for a token.
func (r *remote) exchange(ctx context.Context, server string, in api.CLITokenInput) (api.CLIToken, error) {
	var out api.CLIToken
	body, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/api/v1/cli/token", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := r.env.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("call forecrewd: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return out, failure(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return out, fmt.Errorf("decode answer: %w", err)
	}
	return out, nil
}

// logoutCommand revokes the token of the login and forgets it.
func logoutCommand(r *remote) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Revoke the token of your login to the current organization and forget it",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return r.logout(cmd, all) },
	}
	cmd.Flags().BoolVar(&all, "all", false, "log out of every organization")
	return cmd
}

// revokeLogin revokes the token of org on the server; a token the server
// does not know any more is already revoked.
func (r *remote) revokeLogin(cmd *cobra.Command, creds *credStore, st stored, org string) error {
	o, ok := st.Orgs[org]
	if !ok {
		return nil
	}
	token, err := creds.token(st, org)
	if err != nil {
		return nil // nothing to revoke with: the secret is gone already
	}
	one := *r
	one.token = token
	one.url, one.urlSet = st.Server, true
	err = one.call(cmd.Context(), http.MethodPost, "/tokens/"+url.PathEscape(o.TokenID)+"/revoke", nil, &struct{}{})
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusNotFound) {
		return nil
	}
	return err
}

// whoamiCommand shows who you are logged in as.
func whoamiCommand(r *remote) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who you are, the organization and your roles",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var me api.Session
			if err := r.call(cmd.Context(), http.MethodGet, "/me", nil, &me); err != nil {
				return err
			}
			rows := [][]string{{"Server:", r.serverOf()}}
			if me.User != nil {
				rows = append(rows, []string{"User:", safeText(me.User.Name) + " " + safeText(me.User.Email)})
			}
			if me.Token != "" {
				rows = append(rows, []string{"Token:", me.Token})
			}
			rows = append(rows, []string{"Organization:", safeText(me.Account.Name) + " (" + me.Account.Slug + ")"})
			roles := make([]string, len(me.Roles))
			for i, x := range me.Roles {
				roles[i] = x.Role
				if x.ProjectID != uuid.Nil() {
					roles[i] += " (project)"
				}
			}
			rows = append(rows, []string{"Roles:", dash(strings.Join(roles, ", "))})
			table(cmd.OutOrStdout(), rows)
			return nil
		},
	}
}

// serverOf says which server a call goes to.
func (r *remote) serverOf() string {
	base, _, err := r.target()
	if err != nil {
		return r.url
	}
	return base
}

// orgSwitchCommand makes another organization the one the CLI works in.
func orgSwitchCommand(r *remote) *cobra.Command {
	var f loginFlags
	cmd := &cobra.Command{
		Use:   "switch <organization>",
		Short: "Work in another organization: use your login to it, or log in to it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			creds, ok := newCredStore(r.env)
			if !ok {
				return errors.New("there is no config directory to keep the login in")
			}
			st, err := creds.load()
			if err != nil {
				return err
			}
			if _, have := st.Orgs[args[0]]; have && st.Server != "" {
				st.Current = args[0]
				if err := creds.save(st); err != nil {
					return err
				}
				cmd.Printf("Working in %s.\n", args[0])
				return nil
			}
			if st.Server == "" {
				return errors.New("you are not logged in: run forecrew login")
			}
			f.org, f.server = args[0], st.Server
			return r.login(cmd, f)
		},
	}
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the address instead of opening a browser")
	return cmd
}

// logout revokes the tokens of the current organization, or of all, and
// forgets them.
func (r *remote) logout(cmd *cobra.Command, all bool) error {
	creds, ok := newCredStore(r.env)
	if !ok {
		return errors.New("there is no config directory: nothing to log out of")
	}
	st, err := creds.load()
	if err != nil {
		return err
	}
	orgs := []string{st.Current}
	if all {
		orgs = slices.Sorted(maps.Keys(st.Orgs))
	}
	if len(orgs) == 0 || orgs[0] == "" {
		return errors.New("you are not logged in")
	}
	var failed []string
	for _, org := range orgs {
		if err := r.revokeLogin(cmd, creds, st, org); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", org, err))
		}
		creds.drop(&st, org)
		cmd.Printf("Logged out of %s.\n", org)
	}
	if st.Current == "" && len(st.Orgs) > 0 {
		st.Current = slices.Sorted(maps.Keys(st.Orgs))[0]
	}
	if err := creds.save(st); err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("forgotten here, but not revoked on the server (revoke it with forecrew tokens revoke): %s", strings.Join(failed, "; "))
	}
	return nil
}

// keep stores the token of a login and makes its organization the current one.
func (c *credStore) keep(server string, out api.CLIToken) error {
	st, err := c.load()
	if err != nil {
		return err
	}
	if st.Server != server { // another server: the old logins are no use here
		for org := range st.Orgs {
			c.drop(&st, org)
		}
		st = stored{Server: server, Orgs: map[string]storedOrg{}}
	}
	c.put(&st, out.Account.Slug, storedOrg{TokenID: out.Token.ID.String(), TokenName: out.Token.Name, User: out.User.Name}, out.Secret)
	st.Current = out.Account.Slug
	return c.save(st)
}
