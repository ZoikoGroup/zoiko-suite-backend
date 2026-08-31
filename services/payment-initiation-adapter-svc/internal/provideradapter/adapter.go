// Package provideradapter defines BNK-06's pluggable boundary to a real
// bank/PSP — see internal/domain's package doc for the full architectural
// reasoning. Client is the interface a real integration (a specific bank's
// API, a SWIFT gateway, a payment processor's SDK) would implement.
// StubProviderAdapter is a real, deterministic stand-in used until a real
// provider is connected — clearly labeled, never disguised as one.
package provideradapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// SubmitRequest is everything a real provider integration would need to
// actually transmit the payment.
type SubmitRequest struct {
	IdempotencyKey   string
	PayerAccountRef  string
	PayeeRef         string
	Amount           float64
	Currency         string
	PaymentReference string
}

type Outcome string

const (
	OutcomeSubmitted Outcome = "SUBMITTED"
	OutcomeTimeout   Outcome = "TIMEOUT"  // -> caller should treat as PENDING_UNKNOWN
	OutcomeRejected  Outcome = "REJECTED" // stable, non-retryable
)

type SubmitResult struct {
	Outcome           Outcome
	ProviderRequestID string
	ResponseRef       string
	RejectionReason   string
}

// Client is BNK-06's real dependency boundary. Idempotent: calling Submit
// twice with the same IdempotencyKey must never create two distinct
// provider-side payments — a real provider integration would rely on the
// provider's own idempotency-key support (as this stub does, internally).
type Client interface {
	Submit(ctx context.Context, req SubmitRequest) (*SubmitResult, error)
}

// StubProviderAdapter is NOT a real bank connection. It never calls
// anything external. It exists so this service's own orchestration,
// durability and idempotency logic can be built and tested for real, while
// being completely honest that no actual money movement happens here. Its
// outcome is deterministic, derived from the request itself (via a
// caller-supplied "simulate" prefix convention on PaymentReference), not
// random — so tests can exercise every real outcome the state machine
// needs to handle (Submitted / PendingUnknown-via-timeout / Rejected).
type StubProviderAdapter struct {
	seen map[string]SubmitResult
}

func NewStubProviderAdapter() *StubProviderAdapter {
	return &StubProviderAdapter{seen: map[string]SubmitResult{}}
}

func (s *StubProviderAdapter) Submit(_ context.Context, req SubmitRequest) (*SubmitResult, error) {
	// Real idempotency semantics for the stub itself: a repeat call with
	// the same idempotency key returns the same result, never a new one —
	// the same guarantee a real provider's own idempotency-key support
	// would give.
	if cached, ok := s.seen[req.IdempotencyKey]; ok {
		result := cached
		return &result, nil
	}

	var result SubmitResult
	switch {
	case strings.HasPrefix(req.PaymentReference, "SIMULATE_TIMEOUT"):
		result = SubmitResult{Outcome: OutcomeTimeout}
	case strings.HasPrefix(req.PaymentReference, "SIMULATE_REJECT"):
		result = SubmitResult{Outcome: OutcomeRejected, RejectionReason: "simulated provider rejection: invalid payee reference"}
	default:
		result = SubmitResult{
			Outcome:           OutcomeSubmitted,
			ProviderRequestID: "stub-req-" + fingerprint(req.IdempotencyKey),
			ResponseRef:       "stub-resp-" + fingerprint(req.IdempotencyKey+req.PaymentReference),
		}
	}
	s.seen[req.IdempotencyKey] = result
	return &result, nil
}

func fingerprint(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
