package domain

import "errors"

// Sentinel errors returned by the domain functions. Callers match them with
// errors.Is; the returned errors wrap them with details.
var (
	// ErrInvalidTransition means a state change is not in the transition table.
	ErrInvalidTransition = errors.New("invalid state transition")
	// ErrInvalidRef means a human ref could not be parsed.
	ErrInvalidRef = errors.New("invalid ref")
	// ErrInvalidSignal means a signal payload is missing fields or has an unknown kind.
	ErrInvalidSignal = errors.New("invalid signal")
	// ErrInvalidDecision means a gate decision is not allowed for the gate kind.
	ErrInvalidDecision = errors.New("invalid gate decision")
	// ErrInvalidSpec means a job spec, environment or toolbox breaks the
	// rules of docs/contracts.
	ErrInvalidSpec = errors.New("invalid spec")
)
