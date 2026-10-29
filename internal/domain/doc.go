// Package domain holds the Forecrew entities, their states and the allowed
// state transitions. It is the shared contract of every other package and
// has no dependencies outside the standard library.
//
// Every project-scoped entity carries an AccountID. IDs are UUIDv7 (see
// NewID); entities that people refer to also carry a per-account Number that
// forms a human ref such as GOAL-3 (see Ref). Times are UTC.
package domain
