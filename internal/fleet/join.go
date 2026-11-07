package fleet

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// joinAlphabet is Crockford's base32: no I, L, O or U, so a code read aloud or
// copied by hand survives.
const joinAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// joinCodeBytes is the randomness of a join code: 40 bits, 8 characters
// (fleet-agent-api.md, Joining).
const joinCodeBytes = 5

// Why a join is turned down.
var (
	// ErrJoinRefused means an unknown, used or expired code; the three look
	// the same from outside.
	ErrJoinRefused = errors.New("unknown, used or expired join code")
	// ErrJoinClaimed means another agent holds the code.
	ErrJoinClaimed = errors.New("join code claimed by another agent")
)

// NewProviderToken returns a provider token and its hash.
func NewProviderToken() (token string, hash []byte) { return newToken(prefixProviderToken) }

// NewJoinCode returns a code from a cryptographic source, as in K7PQ-2M4X,
// and its hash.
func NewJoinCode() (code string, hash []byte) {
	b := make([]byte, joinCodeBytes)
	_, _ = rand.Read(b) // never fails
	var bits uint64
	for _, x := range b {
		bits = bits<<8 | uint64(x)
	}
	var s strings.Builder
	for i := range 8 {
		if i == 4 {
			s.WriteByte('-')
		}
		s.WriteByte(joinAlphabet[bits>>(35-5*i)&31])
	}
	return s.String(), HashJoinCode(s.String())
}

// HashJoinCode hashes a code as a person may have typed it: in any case, with
// or without its dash, and with the letters base32 leaves out read as the
// digits they look like.
func HashJoinCode(code string) []byte {
	code = strings.NewReplacer("-", "", " ", "", "O", "0", "I", "1", "L", "1").Replace(strings.ToUpper(code))
	h := sha256.Sum256([]byte(code))
	return h[:]
}

// FactsHash names what an agent reported, so that a person confirms what they
// read and not what it says a moment later. 128 bits: the agent picks the text,
// so it could search for two reports with one short hash.
func FactsHash(kind domain.ProviderKind, facts domain.ProviderCapabilities) string {
	raw, _ := json.Marshal(facts) // plain strings and numbers
	sum := sha256.Sum256(append([]byte(string(kind)+"\x00"), raw...))
	return hex.EncodeToString(sum[:16])
}

// JoinRequest is one call of an agent with a join code.
type JoinRequest struct {
	Code  string
	Claim string
	Kind  domain.ProviderKind
	Facts domain.ProviderCapabilities
}

// Join answers an agent with a code (fleet-agent-api.md, POST /fleet/v1/join):
// it records what the agent found and it waits, with an empty token, until a
// person has confirmed. The next call then gets the provider token, once. The
// token is a secret.
func Join(ctx context.Context, st store.Store, now time.Time, req JoinRequest) (token string, err error) {
	c, err := st.ProviderJoinCodes().GetByCodeHash(ctx, HashJoinCode(req.Code))
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrJoinRefused
	}
	if err != nil {
		return "", fmt.Errorf("find join code: %w", err)
	}
	if !c.Usable(now) {
		return "", ErrJoinRefused
	}
	claim := sha256.Sum256([]byte(req.Claim))
	if len(c.ClaimHash) != 0 && subtle.ConstantTimeCompare(c.ClaimHash, claim[:]) != 1 {
		return "", ErrJoinClaimed
	}
	if c.ProviderID == uuid.Nil() {
		return "", claimCode(ctx, st, c, claim[:], req)
	}
	return handOver(ctx, st, c)
}

// claimCode records the agent's facts. Losing a race for the first claim is
// the same as coming second; a confirmation in between is seen by the next
// call.
func claimCode(ctx context.Context, st store.Store, c domain.ProviderJoinCode, claim []byte, req JoinRequest) error {
	err := st.ProviderJoinCodes().Claim(ctx, c.ID, claim, req.Kind, req.Facts)
	if !errors.Is(err, store.ErrConflict) {
		return err
	}
	now, err := st.ProviderJoinCodes().Get(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("find join code: %w", err)
	}
	if bytes.Equal(now.ClaimHash, claim) {
		return nil
	}
	return ErrJoinClaimed
}

// handOver makes the token of a confirmed code. Only its hash is stored, and
// marking the code used in the same transaction means it is made once.
func handOver(ctx context.Context, st store.Store, c domain.ProviderJoinCode) (string, error) {
	token, hash := NewProviderToken()
	err := st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.ProviderJoinCodes().MarkUsed(ctx, c.ID); err != nil {
			return err
		}
		return tx.CapacityProviders().SetToken(ctx, c.ProviderID, hash)
	})
	// Used by a call a moment ago, or a provider revoked since it was confirmed.
	if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
		return "", ErrJoinRefused
	}
	if err != nil {
		return "", fmt.Errorf("hand over provider token: %w", err)
	}
	return token, nil
}
