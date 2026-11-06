// Package secrets is the secret store (docs/contracts/config-resources.md):
// values encrypted in Postgres under a key from the environment. Values go
// in and are read by the server only; no API gives one back.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// KeySize is the size of the key in bytes (AES-256).
const KeySize = 32

// MaxValueSize is the largest secret accepted.
const MaxValueSize = 64 << 10

// Errors of the store.
var (
	// ErrNotFound means the store holds no such secret.
	ErrNotFound = errors.New("secret not found")
	// ErrInvalid means a bad name, value or key.
	ErrInvalid = errors.New("invalid secret")
	// ErrCorrupt means a value does not decrypt: another key, or a row
	// moved to another name or organization.
	ErrCorrupt = errors.New("secret does not decrypt")
)

// Store keeps secrets by organization and name; the zero account ID is the
// scope of the whole server. A Vault implementation can stand in for
// Encrypted.
type Store interface {
	// Put creates or replaces a value, as by.
	Put(ctx context.Context, by domain.Actor, accountID uuid.UUID, name string, value []byte) error
	// Has reports whether a value exists.
	Has(ctx context.Context, accountID uuid.UUID, name string) (bool, error)
	// Get returns a value for the server's own use.
	Get(ctx context.Context, accountID uuid.UUID, name string) ([]byte, error)
	Delete(ctx context.Context, accountID uuid.UUID, name string) error
}

// Encrypted is the Store of values encrypted in the database.
type Encrypted struct {
	repo store.SecretRepo
	aead cipher.AEAD
}

var _ Store = (*Encrypted)(nil)

// ParseKey decodes the value of FORECREW_SECRET_KEY. The error never holds
// the value.
func ParseKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != KeySize {
		return nil, fmt.Errorf("the key must be %d bytes in base64: %w", KeySize, ErrInvalid)
	}
	return key, nil
}

// New returns a store over repo under key.
func New(repo store.SecretRepo, key []byte) (*Encrypted, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("the key must be %d bytes: %w", KeySize, ErrInvalid)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	return &Encrypted{repo: repo, aead: aead}, nil
}

// additional binds a value to its scope and name, so a row copied to
// another organization does not decrypt.
func additional(accountID uuid.UUID, name string) []byte {
	return []byte(accountID.String() + "/" + name)
}

func validName(name string) error {
	if _, ok := domain.StoreSecretName(domain.SecretRefStore + name); !ok {
		return fmt.Errorf("name %q: %w", name, ErrInvalid)
	}
	return nil
}

// Put implements Store.
func (e *Encrypted) Put(ctx context.Context, by domain.Actor, accountID uuid.UUID, name string, value []byte) error {
	if err := validName(name); err != nil {
		return err
	}
	if len(value) == 0 || len(value) > MaxValueSize {
		return fmt.Errorf("a value is 1 to %d bytes: %w", MaxValueSize, ErrInvalid)
	}
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	return e.repo.Put(ctx, &domain.StoredSecret{
		AccountID: accountID, Name: name, Nonce: nonce, UpdatedBy: by.String(),
		Ciphertext: e.aead.Seal(nil, nonce, value, additional(accountID, name)),
	})
}

// Has implements Store.
func (e *Encrypted) Has(ctx context.Context, accountID uuid.UUID, name string) (bool, error) {
	_, err := e.repo.Get(ctx, accountID, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Get implements Store.
func (e *Encrypted) Get(ctx context.Context, accountID uuid.UUID, name string) ([]byte, error) {
	row, err := e.repo.Get(ctx, accountID, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	value, err := e.aead.Open(nil, row.Nonce, row.Ciphertext, additional(accountID, name))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, ErrCorrupt)
	}
	return value, nil
}

// Delete implements Store. Deleting what is not there succeeds.
func (e *Encrypted) Delete(ctx context.Context, accountID uuid.UUID, name string) error {
	if err := e.repo.Delete(ctx, accountID, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// Resolve reads a "store:name" reference in the scope of accountID, and
// fails with ErrInvalid for any other reference.
func Resolve(ctx context.Context, s Store, accountID uuid.UUID, ref string) ([]byte, error) {
	name, ok := domain.StoreSecretName(ref)
	if !ok {
		return nil, fmt.Errorf("not a store reference: %w", ErrInvalid)
	}
	return s.Get(ctx, accountID, name)
}
