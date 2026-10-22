package store

import "time"

// SetClock replaces the store's clock, so tests can move time past a
// scheduled activation instead of sleeping until it.
func (s *PgStore) SetClock(now func() time.Time) { s.now = now }
