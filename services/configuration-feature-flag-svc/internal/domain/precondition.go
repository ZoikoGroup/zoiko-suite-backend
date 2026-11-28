package domain

import (
	"context"
	"fmt"
)

// Precondition is the caller's API-3 version precondition (If-Match, or
// If-None-Match: * where a route allows "nothing exists yet").
//
// The handler only reads the header and puts it on the request context. The
// store compares it inside the write transaction, after the row is locked, so
// two writers holding the same ETag cannot both succeed: the second sees the
// version the first one wrote.
type Precondition struct {
	// Version is the version from If-Match: the one the caller last read.
	Version int64
	// Any is If-Match: *, which holds whenever the aggregate exists.
	Any bool
	// NoneMatch is If-None-Match: *, which holds only while nothing exists.
	NoneMatch bool
}

type preconditionKey struct{}

// WithPrecondition returns ctx carrying p for the store to check.
func WithPrecondition(ctx context.Context, p Precondition) context.Context {
	return context.WithValue(ctx, preconditionKey{}, p)
}

// PreconditionFrom returns the precondition ctx carries, if any.
func PreconditionFrom(ctx context.Context) (Precondition, bool) {
	p, ok := ctx.Value(preconditionKey{}).(Precondition)
	return p, ok
}

// StaleVersionError refuses a write whose precondition does not hold. Current
// is the aggregate's version now; 0 means nothing exists yet.
type StaleVersionError struct {
	Current int64
}

func (e *StaleVersionError) Error() string {
	return fmt.Sprintf("stale_version: the aggregate's current version is %d", e.Current)
}

// CheckPrecondition compares the precondition ctx carries with current, the
// aggregate's version read under the row lock (0 when nothing exists).
//
// A ctx without a precondition passes: the routes that need one refuse a
// request without it (428) before the store is called, and the store's own
// callers (sweeps, tests) have no caller ETag to compare.
func CheckPrecondition(ctx context.Context, current int64) error {
	p, ok := PreconditionFrom(ctx)
	if !ok {
		return nil
	}
	switch {
	case p.NoneMatch:
		if current == 0 {
			return nil
		}
	case p.Any:
		if current > 0 {
			return nil
		}
	default:
		if current == p.Version {
			return nil
		}
	}
	return &StaleVersionError{Current: current}
}
