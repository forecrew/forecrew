package domain

import (
	"fmt"
	"slices"
	"strings"
	"uuid"
)

// ActorKind says who performed an action.
type ActorKind string

// Actor kinds.
const (
	// ActorUser is a person (docs/contracts/identity.md); Name is the
	// user's ID. Audit entries from before migration 00005 and the server
	// CLI also hold user:<operator name>.
	ActorUser ActorKind = "user"
	// ActorServiceAccount is a service account; Name is its ID.
	ActorServiceAccount ActorKind = "service_account"
	ActorGitHub         ActorKind = "github" // a GitHub login commanding @forecrew
	// ActorMCP is an MCP or API token without an owner, Name is the token
	// name. Tokens from before migration 00005 act as it until they are
	// assigned to an owner; existing mcp:<name> audit entries stay readable.
	ActorMCP    ActorKind = "mcp"
	ActorWorker ActorKind = "worker" // a worker daemon, Name is the worker name
	ActorSystem ActorKind = "system" // forecrewd itself (workflow engine, reaper)
)

// ActorKinds returns every actor kind.
func ActorKinds() []ActorKind {
	return []ActorKind{ActorUser, ActorServiceAccount, ActorGitHub, ActorMCP, ActorWorker, ActorSystem}
}

// CredentialKind is what an actor authenticated with.
type CredentialKind string

// Credential kinds.
const (
	CredentialSession    CredentialKind = "session"     // Session.ID
	CredentialToken      CredentialKind = "token"       // MCPToken.ID
	CredentialOAuthGrant CredentialKind = "oauth_grant" // OAuthGrant.ID of the access token
)

// CredentialKinds returns every credential kind.
func CredentialKinds() []CredentialKind {
	return []CredentialKind{CredentialSession, CredentialToken, CredentialOAuthGrant}
}

// Credential is the session, token or OAuth grant a request was made
// with. It is recorded with the actor in the audit log; it never holds the
// secret, only the row's ID.
type Credential struct {
	Kind CredentialKind `json:"kind"`
	ID   uuid.UUID      `json:"id"`
}

// String formats the credential as "kind:id", the form stored in the audit
// log; empty for the zero credential.
func (c Credential) String() string {
	if c.Kind == "" {
		return ""
	}
	return string(c.Kind) + ":" + c.ID.String()
}

// Actor identifies who performs an action and in which account.
type Actor struct {
	AccountID uuid.UUID `json:"account_id"`
	Kind      ActorKind `json:"kind"`
	Name      string    `json:"name"`
	// Credential is set for users and service accounts acting through a
	// request; zero otherwise.
	Credential Credential `json:"credential,omitzero"`
}

// String formats the actor as "kind:name", the form stored in the audit log.
func (a Actor) String() string {
	return string(a.Kind) + ":" + a.Name
}

// ForecrewdName is the name of the system actor of forecrewd itself.
const ForecrewdName = "forecrewd"

// SystemActor returns the system actor called name in an account, for
// changes forecrewd makes itself.
func SystemActor(accountID uuid.UUID, name string) Actor {
	return Actor{AccountID: accountID, Kind: ActorSystem, Name: name}
}

// UserActor returns the actor of a user in an organization, acting with
// cred.
func UserActor(accountID, userID uuid.UUID, cred Credential) Actor {
	return Actor{AccountID: accountID, Kind: ActorUser, Name: userID.String(), Credential: cred}
}

// ServiceAccountActor returns the actor of a service account, acting with
// a token.
func ServiceAccountActor(accountID, serviceAccountID, tokenID uuid.UUID) Actor {
	return Actor{AccountID: accountID, Kind: ActorServiceAccount, Name: serviceAccountID.String(),
		Credential: Credential{Kind: CredentialToken, ID: tokenID}}
}

// SubjectID returns the user or service account ID of the actor, and
// false for other actors and for legacy user:<name> actors.
func (a Actor) SubjectID() (uuid.UUID, bool) {
	if a.Kind != ActorUser && a.Kind != ActorServiceAccount {
		return uuid.Nil(), false
	}
	id, err := uuid.Parse(a.Name)
	return id, err == nil
}

// ParseActor parses an audit log actor "kind:name" (Actor.String()),
// including the forms of before migration 00005 (mcp:<token name>,
// user:<operator name>). The account and credential are not part of it.
func ParseActor(s string) (Actor, error) {
	kind, name, ok := strings.Cut(s, ":")
	if !ok || name == "" || !slices.Contains(ActorKinds(), ActorKind(kind)) {
		return Actor{}, fmt.Errorf("actor %q: %w", s, ErrInvalidRecord)
	}
	a := Actor{Kind: ActorKind(kind), Name: name}
	if a.Kind == ActorServiceAccount {
		if _, ok := a.SubjectID(); !ok {
			return Actor{}, fmt.Errorf("actor %q: service account is not an ID: %w", s, ErrInvalidRecord)
		}
	}
	return a, nil
}
