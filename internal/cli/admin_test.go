package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

type fakeDB struct {
	st         *testkit.Store
	migrations int
	migrateErr error
	closed     int
}

func (d *fakeDB) Store() store.Store { return d.st }
func (d *fakeDB) Migrate(context.Context) error {
	d.migrations++
	return d.migrateErr
}
func (d *fakeDB) Close() { d.closed++ }

type adminEnv struct {
	db      *fakeDB
	account domain.Account
	env     Env
	openErr error
	vars    map[string]string
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	e := &adminEnv{db: &fakeDB{st: testkit.NewStore(nil)}, account: domain.Account{Name: "default"}}
	if err := e.db.st.Accounts().Create(t.Context(), &e.account); err != nil {
		t.Fatal(err)
	}
	e.vars = map[string]string{config.EnvDatabaseURL: "postgres://localhost/forecrew"}
	e.env = Env{
		Getenv: func(k string) string { return e.vars[k] },
		Open: func(context.Context, string) (Database, error) {
			if e.openErr != nil {
				return nil, e.openErr
			}
			return e.db, nil
		},
	}
	return e
}

func (e *adminEnv) run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = RunWith(t.Context(), args, &out, &errOut, "test", e.env)
	return out.String(), errOut.String(), code
}

func TestMigrateUp(t *testing.T) {
	e := newAdminEnv(t)
	out, _, code := e.run(t, "migrate", "up")
	if code != 0 || e.db.migrations != 1 || e.db.closed != 1 || !strings.Contains(out, "up to date") {
		t.Fatalf("code %d, migrations %d, closed %d, out %q", code, e.db.migrations, e.db.closed, out)
	}
	e.db.migrateErr = errors.New("locked")
	if _, errOut, code := e.run(t, "migrate", "up"); code != 1 || !strings.Contains(errOut, "locked") {
		t.Errorf("migrate error: code %d, %q", code, errOut)
	}
}

func TestDatabaseErrors(t *testing.T) {
	e := newAdminEnv(t)
	delete(e.vars, config.EnvDatabaseURL)
	if _, errOut, code := e.run(t, "migrate", "up"); code != 1 || !strings.Contains(errOut, "FORECREW_DATABASE_URL is not set") {
		t.Errorf("no url: code %d, %q", code, errOut)
	}
	e.vars[config.EnvDatabaseURL] = "x"
	e.openErr = errors.New("connection refused")
	if _, errOut, code := e.run(t, "token", "create", "laptop"); code != 1 || !strings.Contains(errOut, "connection refused") {
		t.Errorf("open error: code %d, %q", code, errOut)
	}
}

const validConfig = `
server:
  public_base_url: https://forecrew.example.com
github:
  app_id: 1
runtimes:
  claude: {adapter: claude-code, vendor: anthropic}
model_tiers:
  fast: {anthropic: haiku}
  strong: {anthropic: sonnet}
  best: {anthropic: opus}
roles:
  developer: {runtime: claude, tier: strong}
`

func TestConfigValidate(t *testing.T) {
	e := newAdminEnv(t)
	e.vars[config.EnvGitHubPrivateKey] = "key"
	e.vars[config.EnvGitHubWebhookSecret] = "secret"
	dir := t.TempDir()
	good, bad := filepath.Join(dir, "good.yaml"), filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(good, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("server: {listen: ':1'}\nnope: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, errOut, code := e.run(t, "config", "validate", "--config", good); code != 0 || !strings.Contains(out, "is valid") {
		t.Errorf("good config: code %d, %q, %q", code, out, errOut)
	}
	if _, errOut, code := e.run(t, "config", "validate", "--config", bad); code != 1 || errOut == "" {
		t.Errorf("bad config: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "config", "validate", "--config", filepath.Join(dir, "missing.yaml")); code != 1 || !strings.Contains(errOut, "read config") {
		t.Errorf("missing config: code %d, %q", code, errOut)
	}
}

func TestTokenCreate(t *testing.T) {
	e := newAdminEnv(t)
	out, errOut, code := e.run(t, "token", "create", "laptop")
	secret := strings.TrimSpace(out)
	if code != 0 || !strings.HasPrefix(secret, "fcm_") || len(secret) != len("fcm_")+43 || !strings.Contains(errOut, "not stored") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	tok, err := e.db.st.MCPTokens().GetByTokenHash(t.Context(), mcp.HashToken(secret))
	if err != nil || tok.Name != "laptop" || tok.AccountID != e.account.ID {
		t.Fatalf("stored token = %+v, %v", tok, err)
	}
	if strings.Contains(errOut, secret) {
		t.Error("stderr repeats the secret")
	}
	out2, _, _ := e.run(t, "token", "create", "phone")
	if strings.TrimSpace(out2) == secret {
		t.Error("two tokens are equal")
	}
	if _, errOut, code := e.run(t, "token", "create", "laptop"); code != 1 || !strings.Contains(errOut, `a token named "laptop" already exists`) {
		t.Errorf("duplicate: code %d, %q", code, errOut)
	}
	if _, _, code := e.run(t, "token", "create"); code != 1 {
		t.Error("name is required")
	}
}

func TestWorkerAddAndRevoke(t *testing.T) {
	e := newAdminEnv(t)
	ctx := t.Context()
	out, _, code := e.run(t, "worker", "add", "build-1")
	secret := strings.TrimSpace(out)
	if code != 0 || !strings.HasPrefix(secret, "fcw_") || len(secret) != len("fcw_")+43 {
		t.Fatalf("code %d, stdout %q", code, out)
	}
	w, err := e.db.st.Workers().GetByTokenHash(ctx, mcp.HashToken(secret))
	if err != nil || w.Name != "build-1" || w.AccountID != e.account.ID || w.Revoked() {
		t.Fatalf("stored worker = %+v, %v", w, err)
	}
	if _, errOut, code := e.run(t, "worker", "add", "build-1"); code != 1 || !strings.Contains(errOut, `a worker named "build-1" already exists`) {
		t.Errorf("duplicate: code %d, %q", code, errOut)
	}

	out, _, code = e.run(t, "worker", "revoke", "build-1")
	if code != 0 || !strings.Contains(out, "worker build-1 revoked") {
		t.Fatalf("revoke: code %d, %q", code, out)
	}
	if w, _ := e.db.st.Workers().Get(ctx, e.account.ID, w.ID); !w.Revoked() {
		t.Error("worker is not revoked")
	}
	if _, errOut, code := e.run(t, "worker", "revoke", "build-1"); code != 1 || !strings.Contains(errOut, "already revoked") {
		t.Errorf("revoking twice: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "worker", "revoke", "ghost"); code != 1 || !strings.Contains(errOut, `worker "ghost" does not exist or is already revoked`) {
		t.Errorf("unknown worker: code %d, %q", code, errOut)
	}
}

// poolEnv is an admin env whose forecrew.yaml (the returned path) defines
// pool gpu.
func poolEnv(t *testing.T) (*adminEnv, string) {
	t.Helper()
	e := newAdminEnv(t)
	e.vars[config.EnvGitHubPrivateKey] = "key"
	e.vars[config.EnvGitHubWebhookSecret] = "secret"
	cfg := filepath.Join(t.TempDir(), "forecrew.yaml")
	gpu := validConfig + "pools:\n  gpu: {kinds: [command], credentials: {repo_token: read}}\n"
	if err := os.WriteFile(cfg, []byte(gpu), 0o600); err != nil {
		t.Fatal(err)
	}
	return e, cfg
}

// pool returns the pool of the worker called name, or "" when there is none.
func (e *adminEnv) pool(t *testing.T, name string) string {
	t.Helper()
	list, err := e.db.st.Workers().List(t.Context(), e.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		if w.Name == name {
			return w.Pool
		}
	}
	return ""
}

// TestWorkerAddPool: workers join a default pool without forecrew.yaml or a
// pool forecrew.yaml defines.
func TestWorkerAddPool(t *testing.T) {
	e, cfg := poolEnv(t)
	tests := []struct {
		args []string
		name string
		want string
	}{
		{[]string{"box"}, "box", domain.PoolBuild},
		{[]string{"ctl", "--pool", "control", "--config", "/missing.yaml"}, "ctl", domain.PoolControl},
		{[]string{"gpu-1", "--pool", "gpu", "--config", cfg}, "gpu-1", "gpu"},
	}
	for _, tt := range tests {
		if _, errOut, code := e.run(t, append([]string{"worker", "add"}, tt.args...)...); code != 0 || e.pool(t, tt.name) != tt.want {
			t.Errorf("%v: code %d, %q, pool %q, want %q", tt.args, code, errOut, e.pool(t, tt.name), tt.want)
		}
	}
}

// TestWorkerPoolRefused: unknown pools and actions are refused, and nothing
// is created or printed.
func TestWorkerPoolRefused(t *testing.T) {
	e, cfg := poolEnv(t)
	e.run(t, "worker", "add", "box")
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"worker", "add", "x", "--pool", "actions"}, "pool actions has no workers"},
		{[]string{"worker", "add", "x", "--pool", "nope", "--config", cfg}, "unknown pool nope"},
		{[]string{"worker", "add", "x", "--pool", "nope", "--config", "/missing.yaml"}, "the pools of /missing.yaml cannot be read"},
		{[]string{"worker", "add", "x", "--pool", "Bad Name"}, `pool "Bad Name" is not a valid pool name`},
		{[]string{"worker", "move", "box", "--pool", "nope", "--config", cfg}, "unknown pool nope"},
		{[]string{"worker", "move", "box"}, `required flag(s) "pool" not set`},
		{[]string{"worker", "move", "ghost", "--pool", "control"}, `worker "ghost" does not exist or is revoked`},
	}
	for _, tt := range tests {
		if out, errOut, code := e.run(t, tt.args...); code != 1 || out != "" || !strings.Contains(errOut, tt.want) {
			t.Errorf("%v: code %d, stdout %q, stderr %q, want %q", tt.args, code, out, errOut, tt.want)
		}
	}
	if e.pool(t, "x") != "" || e.pool(t, "box") != domain.PoolBuild {
		t.Error("a refused command changed a worker")
	}
}

// TestWorkerMove: move changes the stored pool of an active worker.
func TestWorkerMove(t *testing.T) {
	e, cfg := poolEnv(t)
	e.run(t, "worker", "add", "box")
	out, errOut, code := e.run(t, "worker", "move", "box", "--pool", "gpu", "--config", cfg)
	if code != 0 || !strings.Contains(out, "worker box moved to pool gpu") || e.pool(t, "box") != "gpu" {
		t.Fatalf("move: code %d, %q, %q, pool %q", code, out, errOut, e.pool(t, "box"))
	}
	e.vars["USER"] = "ops"
	e.run(t, "worker", "move", "box", "--pool", "control")
	entries, err := e.db.st.Audit().List(t.Context(), e.account.ID, store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var moves []string
	for _, a := range entries {
		if a.Action == "worker.pool_changed" && a.SubjectType == "worker" {
			moves = append(moves, a.Actor+" "+string(a.Data))
		}
	}
	slices.Sort(moves)
	want := []string{`user:admin {"from":"build","name":"box","to":"gpu"}`, `user:ops {"from":"gpu","name":"box","to":"control"}`}
	if !slices.Equal(moves, want) {
		t.Errorf("audit = %v, want %v", moves, want)
	}
	e.run(t, "worker", "revoke", "box")
	if _, errOut, code := e.run(t, "worker", "move", "box", "--pool", "build"); code != 1 || !strings.Contains(errOut, "is revoked") {
		t.Errorf("move revoked: code %d, %q", code, errOut)
	}
}

func TestAccountSelection(t *testing.T) {
	e := newAdminEnv(t)
	ctx := t.Context()
	second := domain.Account{Name: "second"}
	if err := e.db.st.Accounts().Create(ctx, &second); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := e.run(t, "token", "create", "a"); code != 1 || !strings.Contains(errOut, "several accounts") {
		t.Errorf("ambiguous: code %d, %q", code, errOut)
	}
	if _, _, code := e.run(t, "token", "create", "a", "--account", second.ID.String()); code != 0 {
		t.Error("explicit account failed")
	}
	toks, _ := e.db.st.MCPTokens().List(ctx, second.ID)
	if len(toks) != 1 {
		t.Errorf("second account has %d tokens", len(toks))
	}
	if _, errOut, code := e.run(t, "token", "create", "b", "--account", "nope"); code != 1 || !strings.Contains(errOut, "not a UUID") {
		t.Errorf("bad account: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run(t, "worker", "revoke", "b", "--account", domain.NewID().String()); code != 1 || !strings.Contains(errOut, "does not exist") {
		t.Errorf("unknown account: code %d, %q", code, errOut)
	}

	empty := newAdminEnv(t)
	empty.db.st = testkit.NewStore(nil)
	if _, errOut, code := empty.run(t, "token", "create", "a"); code != 1 || !strings.Contains(errOut, "no account yet") {
		t.Errorf("no account: code %d, %q", code, errOut)
	}
}

func TestTokenListAndRevoke(t *testing.T) {
	e := newAdminEnv(t)
	ctx := t.Context()
	secret, _, _ := e.run(t, "token", "create", "laptop")
	e.run(t, "token", "create", "phone")
	tok, err := e.db.st.MCPTokens().GetByTokenHash(ctx, mcp.HashToken(strings.TrimSpace(secret)))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.st.MCPTokens().Touch(ctx, e.account.ID, tok.ID); err != nil {
		t.Fatal(err)
	}

	out, _, code := e.run(t, "token", "list")
	if code != 0 || !strings.Contains(out, "laptop") || !strings.Contains(out, "phone") || strings.Contains(out, "revoked") {
		t.Fatalf("list: code %d, %q", code, out)
	}
	if strings.Contains(out, strings.TrimSpace(secret)) {
		t.Error("list shows a secret")
	}

	// By name, then the ID of the other one.
	if out, _, code = e.run(t, "token", "revoke", "laptop"); code != 0 || !strings.Contains(out, "token laptop revoked") {
		t.Fatalf("revoke by name: code %d, %q", code, out)
	}
	if got, _ := e.db.st.MCPTokens().GetByTokenHash(ctx, mcp.HashToken(strings.TrimSpace(secret))); got.RevokedAt.IsZero() {
		t.Error("token is not revoked")
	}
	toks, _ := e.db.st.MCPTokens().List(ctx, e.account.ID)
	var phone domain.MCPToken
	for _, x := range toks {
		if x.Name == "phone" {
			phone = x
		}
	}
	if _, _, code = e.run(t, "token", "revoke", phone.ID.String()); code != 0 {
		t.Error("revoke by ID failed")
	}
	if out, _, _ = e.run(t, "token", "list"); strings.Count(out, "revoked") != 2 {
		t.Errorf("list after revoke: %q", out)
	}

	for _, arg := range []string{"laptop", "ghost"} {
		if _, errOut, code := e.run(t, "token", "revoke", arg); code != 1 || !strings.Contains(errOut, "does not exist or is already revoked") {
			t.Errorf("revoke %s: code %d, %q", arg, code, errOut)
		}
	}
	if _, _, code := e.run(t, "token", "revoke"); code != 1 {
		t.Error("a name is required")
	}

}

// The name of the Actions provider's worker row cannot be added, and a row
// of that name cannot be moved out of its pool.
func TestProviderWorkerNameIsReserved(t *testing.T) {
	e, cfg := poolEnv(t)
	if out, errOut, code := e.run(t, "worker", "add", "forecrewd-actions"); code != 1 || out != "" || !strings.Contains(errOut, "is reserved") {
		t.Errorf("add: code %d, %q, %q", code, out, errOut)
	}
	row := domain.Worker{AccountID: e.account.ID, Name: "forecrewd-actions", Pool: domain.PoolActions, TokenHash: []byte("x")}
	if err := e.db.st.Workers().Create(t.Context(), &row); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := e.run(t, "worker", "move", "forecrewd-actions", "--pool", "control", "--config", cfg); code != 1 || !strings.Contains(errOut, "is reserved") {
		t.Errorf("move: code %d, %q", code, errOut)
	}
	if e.pool(t, "forecrewd-actions") != domain.PoolActions {
		t.Error("the provider's worker was moved")
	}
}
