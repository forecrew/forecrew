// Package enterprise is the seam between open core and the enterprise build
// (docs/enterprise.md). The ee/ directory, compiled in with the build tag
// enterprise, registers itself here; open core asks for the features and
// never imports ee/.
package enterprise

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/forecrew/forecrew/internal/auth"
	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// Deps is what the enterprise features work with.
type Deps struct {
	Store     store.Store
	Authz     *authz.Engine
	Gates     gates.Service
	PublicURL string
	Logger    *slog.Logger
	Now       func() time.Time
}

// Features are what the enterprise build adds. In the open-core build every
// field is nil and each user falls back to what open core does.
type Features struct {
	Tools  mcp.EnterpriseTools
	SignIn auth.Policy
	SCIM   http.Handler              // serves /scim/v2/
	Run    func(ctx context.Context) // background work, until ctx ends
}

var build func(Deps) Features

// Register makes the enterprise features available. ee/ calls it from init.
func Register(f func(Deps) Features) { build = f }

// Load returns the features of this build.
func Load(d Deps) Features {
	if build == nil {
		return Features{}
	}
	return build(d)
}
