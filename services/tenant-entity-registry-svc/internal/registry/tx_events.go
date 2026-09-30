package registry

import (
	"context"

	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

// EventBuilder renders the event for a write from the write's own result,
// inside the write's transaction. The store calls it with what the write
// produced (the created or updated row, or an events.StatusChange) and
// enqueues the record in event_outbox before commit — so the fact and its
// event commit together, and an UPDATE's event carries the version the
// UPDATE actually produced.
//
// It replaces `go s.events.PublishX(ctx, …)` after commit, which lost every
// event whose process died between commit and publish and, until 28 Sep 2026,
// most events outright (the request context was cancelled under it).
//
// A builder returns (nil, nil) for a result it does not recognise, so a
// service method that performs more than one write can attach a builder
// without it firing for the wrong one.
type EventBuilder func(result any) (*outbox.Record, error)

type eventBuilderKey struct{}

// WithEvent attaches the event builder for the write this context is about to
// perform.
func WithEvent(ctx context.Context, b EventBuilder) context.Context {
	return context.WithValue(ctx, eventBuilderKey{}, b)
}

// EventFor returns the builder attached to ctx, or nil.
func EventFor(ctx context.Context) EventBuilder {
	b, _ := ctx.Value(eventBuilderKey{}).(EventBuilder)
	return b
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
