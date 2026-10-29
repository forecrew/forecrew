// Package testkit holds one in-memory fake per Forecrew interface, for tests
// of every package. Fakes follow the documented contract of the interface
// they implement; extend them when a contract grows, never fork them.
package testkit
