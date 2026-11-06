package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

// keychainService is the service name of the entries in the OS keychain.
const keychainService = "forecrew"

// Keyring is the OS keychain. The real one is go-keyring's, which fails where
// there is none (a server without a desktop session): the token then goes in
// the credentials file.
type Keyring interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
	Delete(service, user string) error
}

// ErrNoSecret means the keychain has no such entry.
var ErrNoSecret = errors.New("no such secret")

type osKeyring struct{}

func (osKeyring) Get(service, user string) (string, error) {
	v, err := keyring.Get(service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNoSecret
	}
	return v, err
}
func (osKeyring) Set(service, user, secret string) error { return keyring.Set(service, user, secret) }
func (osKeyring) Delete(service, user string) error {
	if err := keyring.Delete(service, user); !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

// storedOrg is what the CLI keeps of its login to one organization.
type storedOrg struct {
	TokenID   string `json:"token_id"`
	TokenName string `json:"token_name"`
	User      string `json:"user"`
	// Token is the secret, only when the keychain was not usable.
	Token string `json:"token,omitempty"`
}

// stored is the credentials file: the server the CLI logged in to, the
// organization it uses and its token for each organization it logged in to.
type stored struct {
	Server  string               `json:"server"`
	Current string               `json:"current"`
	Orgs    map[string]storedOrg `json:"orgs"`
}

// credStore keeps the logins of the CLI: a file of mode 0600 in the user's
// config directory, with the tokens in the OS keychain where there is one.
type credStore struct {
	path    string
	keyring Keyring
}

// newCredStore returns the store of env; ok is false when the environment
// has no config directory, and then nothing is kept.
func newCredStore(env Env) (*credStore, bool) {
	if env.ConfigDir == nil {
		return nil, false
	}
	dir, err := env.ConfigDir()
	if err != nil || dir == "" {
		return nil, false
	}
	return &credStore{path: filepath.Join(dir, "forecrew", "credentials.json"), keyring: env.Keyring}, true
}

func (c *credStore) load() (stored, error) {
	var s stored
	b, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return stored{Orgs: map[string]storedOrg{}}, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("read %s: %w", c.path, err)
	}
	if s.Orgs == nil {
		s.Orgs = map[string]storedOrg{}
	}
	return s, nil
}

// save writes the file with mode 0600, replacing it whole.
func (c *credStore) save(s stored) error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".credentials-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}

func keychainUser(server, org string) string { return server + "#" + org }

// put keeps the token of org, in the keychain when it works, else in the file.
func (c *credStore) put(s *stored, org string, o storedOrg, token string) {
	o.Token = ""
	if c.keyring == nil || c.keyring.Set(keychainService, keychainUser(s.Server, org), token) != nil {
		o.Token = token
	}
	s.Orgs[org] = o
}

// token returns the secret of org.
func (c *credStore) token(s stored, org string) (string, error) {
	o, ok := s.Orgs[org]
	if !ok {
		return "", ErrNoSecret
	}
	if o.Token != "" {
		return o.Token, nil
	}
	if c.keyring == nil {
		return "", ErrNoSecret
	}
	return c.keyring.Get(keychainService, keychainUser(s.Server, org))
}

// drop forgets org.
func (c *credStore) drop(s *stored, org string) {
	if c.keyring != nil {
		_ = c.keyring.Delete(keychainService, keychainUser(s.Server, org))
	}
	delete(s.Orgs, org)
	if s.Current == org {
		s.Current = ""
	}
}
