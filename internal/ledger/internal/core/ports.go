package core

// Store persists committed transactions in order. Append must be durable
// before it returns: the ledger only updates memory after a successful Append.
type Store interface {
	Append(Tx) error
	Load() ([]Tx, error)
}

// Metrics receives ledger measurements. Labels are bounded: op and result.
type Metrics interface {
	CommandDone(op, result string)
	IdempotencyConflict()
	EntriesWritten(n int)
}
