package domain

import "uuid"

// ActorKind says who performed an action.
type ActorKind string

// Actor kinds.
const (
	ActorUser   ActorKind = "user"   // a person in the web UI or CLI
	ActorGitHub ActorKind = "github" // a GitHub login commanding @forecrew
	ActorMCP    ActorKind = "mcp"    // an MCP or API token, Name is the token name
	ActorWorker ActorKind = "worker" // a worker daemon, Name is the worker name
	ActorSystem ActorKind = "system" // forecrewd itself (workflow engine, reaper)
)

// Actor identifies who performs an action and in which account.
type Actor struct {
	AccountID uuid.UUID `json:"account_id"`
	Kind      ActorKind `json:"kind"`
	Name      string    `json:"name"`
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
