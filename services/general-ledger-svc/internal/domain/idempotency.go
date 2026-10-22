package domain

// Idempotency-Key errors for ACC-01's replay-protected commands.
//
// Kept in their own file rather than appended to types.go's error block so a
// concurrent change to the account types does not collide with this one; the
// sentinel is still a plain package-level errorString like every other domain
// error.

// ErrIdempotencyKeyConflict is returned when an Idempotency-Key was already
// claimed by a different command, or by the same command on a different
// account. The same key must replay the same command, never execute a new one.
var ErrIdempotencyKeyConflict = errorString("idempotency key already used for a different command or account")
