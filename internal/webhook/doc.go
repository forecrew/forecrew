// Package webhook turns GitHub events into workflow signals: the HTTP
// handler for POST /webhooks/github, the mapping of events to signals and
// gate decisions, the @forecrew commands and the poller that recovers missed
// events. Signals carry the domain's semantic dedupe keys, so a fact seen by
// both the handler and the poller is stored once.
package webhook
