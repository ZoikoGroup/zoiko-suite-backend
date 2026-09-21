package indexer

import "github.com/google/uuid"

// newUUID is a one-line indirection so the tombstone id generator can be
// stubbed in tests without pulling uuid into every test's imports.
func newUUID() string { return uuid.NewString() }
