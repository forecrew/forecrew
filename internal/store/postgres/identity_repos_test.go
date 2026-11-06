//go:build integration

package postgres

import (
	"testing"

	"github.com/forecrew/forecrew/internal/testkit"
)

func TestIdentityStoreSuite(t *testing.T) {
	f := newFixture(t)
	testkit.IdentityStoreSuite(t, f.st, f.account)
}
