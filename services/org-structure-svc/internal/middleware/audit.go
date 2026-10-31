package middleware

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// Who did it and under which correlation id, carried to the store so the
// events it writes in the same transaction as a change can attest both
// (ZS-SVC-I-001 s7: actor and correlation_id are part of every event).

type actorCtxKey struct{}
type correlationCtxKey struct{}

func WithActor(ctx context.Context, actorID string) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, actorID)
}

func ActorFromContext(ctx context.Context) string {
	v, _ := ctx.Value(actorCtxKey{}).(string)
	return v
}

func WithCorrelation(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationCtxKey{}, correlationID)
}

func CorrelationFromContext(ctx context.Context) string {
	v, _ := ctx.Value(correlationCtxKey{}).(string)
	return v
}

// AuditContext puts the caller (X-Principal-Id) and the correlation id
// (X-Correlation-ID, or a fresh one) on the request context. It trusts nothing:
// each handler still requires and authorizes the principal itself; this only
// carries the already-supplied values to the layer that writes the event.
func AuditContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := r.Header.Get("X-Correlation-ID")
		if cid == "" {
			cid = uuid.NewString()
		}
		ctx := WithCorrelation(r.Context(), cid)
		if actor := r.Header.Get("X-Principal-Id"); actor != "" {
			ctx = WithActor(ctx, actor)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
