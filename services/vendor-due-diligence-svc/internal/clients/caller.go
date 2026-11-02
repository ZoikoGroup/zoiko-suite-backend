package clients

import "context"

// Caller is the identity and scope of the request on whose behalf this
// service pushes a due-diligence outcome to counterparty-management-svc.
// counterparty-management-svc authorizes that write against a real principal
// and enforces the full request envelope (legal entity, purpose, idempotency),
// so the push must carry the initiating caller's context — sending only
// X-Tenant-Id made every push fail with 401 and the counterparty record
// never reflected the check.
type Caller struct {
	PrincipalID   string
	LegalEntityID string
	CorrelationID string
	// CheckID scopes the Idempotency-Key, so a retried push for the same
	// check replays rather than re-applies.
	CheckID string
}

type callerKey struct{}

// WithCaller returns ctx carrying c for the counterparty client to read.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

func callerFrom(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}
