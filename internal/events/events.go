// Package events holds the event contracts shared between modules. It imports
// no module, so producers and consumers depend on it instead of on each other.
package events

import "time"

// SchemaV1 is the current version of Event. Bump it when a field changes
// meaning; consumers switch on Version.
const SchemaV1 = 1

// Event is a fact published by a module through the outbox.
type Event struct {
	ID      string    // unique per producer, used by consumers for dedup
	Source  string    // producing module, e.g. "ledger"
	Type    string    // e.g. "deposit", "settle"
	Version int       // schema version, see SchemaV1
	Key     string    // idempotency key of the command that produced it
	Seq     uint64    // producer sequence number
	TraceID string    `json:",omitempty"`
	Time    time.Time // commit time
}
