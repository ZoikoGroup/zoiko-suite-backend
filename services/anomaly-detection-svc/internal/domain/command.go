package domain

// CommandContext carries everything the store needs to execute one write
// command, resolved by the handler from the verified request envelope.
//
//   - TenantID / LegalEntityID / PrincipalID / CorrelationID come from the
//     X-Tenant-Id, X-Legal-Entity-Id, X-Principal-Id and X-Correlation-ID
//     headers. They are never read from a request body.
//   - Idempotency is the replay-protection claim (Idempotency-Key header).
//   - ExpectedVersion is the parsed If-Match header; nil when the command
//     creates a new resource and therefore has no prior version.
type CommandContext struct {
	TenantID      string
	LegalEntityID string
	PrincipalID   string
	CorrelationID string

	Idempotency IdempotencyClaim

	ExpectedVersion *int
}

// IdempotencyClaim is one row of the idempotency_keys table.
//
// Storage key (Global API contract §9.1):
//
//	tenant_id + principal_id + route_template + idempotency_key
//
// RequestSHA256 is a hash of method, concrete path and body. Same key with
// the same hash is a replay (the original resource is returned); same key
// with a different hash is idempotency_key_reuse_conflict (409).
type IdempotencyClaim struct {
	RouteTemplate string
	Key           string
	RequestSHA256 string
}

// CommandResult wraps the resource a command produced together with whether
// it was produced now or replayed from an earlier request with the same
// Idempotency-Key. On replay the handler sets Idempotent-Replayed: true.
type CommandResult[T any] struct {
	Resource *T
	Replayed bool
}
