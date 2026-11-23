package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/org-structure-svc/internal/events"
	svcmiddleware "zoiko.io/org-structure-svc/internal/middleware"
	"zoiko.io/org-structure-svc/internal/outbox"
)

// emit writes an ORG-05 event to the outbox INSIDE the caller's transaction, so
// the event lands if and only if the change it attests lands. The tenant is the
// transaction's; actor and correlation id come from the request context unless
// the event already names them.
func emit(ctx context.Context, tx pgx.Tx, tenantID string, ev events.OrgEvent) error {
	ev.TenantID = tenantID
	if ev.Actor == "" {
		ev.Actor = svcmiddleware.ActorFromContext(ctx)
	}
	if ev.CorrelationID == "" {
		ev.CorrelationID = svcmiddleware.CorrelationFromContext(ctx)
	}
	if ev.RecordedAt.IsZero() {
		ev.RecordedAt = time.Now().UTC()
	}
	rec, err := events.BuildOrgEvent(ev)
	if err != nil {
		return err
	}
	if err := outbox.EnqueueTx(ctx, tx, rec); err != nil {
		return fmt.Errorf("emit %s: %w", ev.Name, err)
	}
	return nil
}

// today is the recorded date, used as effective_at for events about something
// with no business date of its own (a status change).
func today() string { return time.Now().UTC().Format("2006-01-02") }

// tracingID is the id an event is traced by: the request's correlation id when
// there is one, else the fallback. A create request's body also has a
// "correlation_id", but that is the client's idempotency key, a different thing.
func tracingID(ctx context.Context, fallback string) string {
	if id := svcmiddleware.CorrelationFromContext(ctx); id != "" {
		return id
	}
	return fallback
}
