package middleware

import "context"

type correlationCtxKey struct{}

// WithCorrelation carries the request's correlation id to the store, which stamps it on the
// events it writes in the same transaction as a change.
func WithCorrelation(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationCtxKey{}, correlationID)
}

// CorrelationFromContext returns the correlation id set by WithCorrelation, or "".
func CorrelationFromContext(ctx context.Context) string {
	v, _ := ctx.Value(correlationCtxKey{}).(string)
	return v
}
