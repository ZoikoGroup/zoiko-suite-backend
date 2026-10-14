package ar05

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Transport is the wire to one e-invoice network/provider. THERE IS NO REAL IMPLEMENTATION IN THIS PLATFORM: the
// default is UnconfiguredTransports. A real adapter must:
//
//  1. Submit: send the artifact bytes to the network using ExternalAttemptID as the provider-side client reference /
//     idempotency key (so a re-delivery of the same attempt is deduplicated by the network), and map the answer to a
//     SubmitResult. It MUST return ErrNotSent (wrapped is fine) ONLY when it can guarantee nothing reached the
//     provider (e.g. refused connection before any bytes were written). ANY other failure - timeout, reset, 5xx,
//     unparseable answer - must be returned as an ordinary error: the service treats it as an AMBIGUOUS outcome
//     (PENDING_UNKNOWN), never as a failure to retry blindly.
//  2. QueryStatus: look the document up by ExternalAttemptID (and ProviderRef when known) and return an AUTHORITATIVE
//     state. ProviderNotReceived must mean "the network confirms it never received this attempt"; if the network
//     cannot say so, return ProviderUnknown (or an error). Only ProviderNotReceived ever allows a re-send.
//  3. Name: a stable provider id; it keys the callback secret (AR_EINVOICE_CALLBACK_SECRET_<PROVIDER>) and appears on
//     every attempt and acknowledgement.
//  4. Sign provider callbacks per callback.go (HMAC-SHA256 over timestamp.nonce.body).
//
// Secrets, endpoints and certificates are the adapter's own concern (secret-vault); they are never in a pack.
type Transport interface {
	Name() string
	Submit(ctx context.Context, req SubmitRequest) (*SubmitResult, error)
	QueryStatus(ctx context.Context, req QueryRequest) (*StatusResult, error)
}

// ErrNotSent marks a Submit failure after which NOTHING reached the provider (safe to retry without reconciliation).
var ErrNotSent = errors.New("the document was not sent")

// NoProviderError: no transport is configured for the profile. Typed so the caller refuses cleanly (503
// EINVOICE_PROVIDER_NOT_CONFIGURED); it is never converted into a success or into a PENDING_UNKNOWN state.
type NoProviderError struct{ Network, Profile string }

func (e *NoProviderError) Error() string {
	return fmt.Sprintf("no e-invoice provider is configured for network %q (profile %s)", e.Network, e.Profile)
}

// TransportResolver selects the transport for a profile.
type TransportResolver interface {
	For(p *Profile) (Transport, error)
}

// UnconfiguredTransports is the default: every profile answers NoProviderError.
type UnconfiguredTransports struct{}

func (UnconfiguredTransports) For(p *Profile) (Transport, error) {
	return nil, &NoProviderError{Network: p.Network, Profile: p.ProfileCode}
}

// NetworkTransports maps an upper-cased network (profile channel) to its transport.
type NetworkTransports map[string]Transport

func (n NetworkTransports) For(p *Profile) (Transport, error) {
	if t, ok := n[strings.ToUpper(p.Network)]; ok && t != nil {
		return t, nil
	}
	return nil, &NoProviderError{Network: p.Network, Profile: p.ProfileCode}
}

// SubmitRequest is one transmission attempt.
type SubmitRequest struct {
	ExternalAttemptID string // durable attempt id: the provider-side idempotency key
	SubmissionID      string
	TenantID          string
	LegalEntityID     string
	CorrelationID     string
	ProfileCode       string
	ProfileVersion    string
	Network           string
	Syntax            string
	Mode              string
	DocumentType      string
	DocumentNumber    string
	EndpointID        string
	ParticipantID     string
	Purpose           string
	ContentType       string
	Artifact          []byte
	ArtifactHash      string
}

// Submit dispositions.
const (
	DispAcknowledged = "ACKNOWLEDGED" // received; the final answer comes later (callback / query)
	DispAccepted     = "ACCEPTED"     // synchronous acceptance (clearance); needs a receipt
	DispRejected     = "REJECTED"     // synchronous, definite rejection
)

// SubmitResult is the provider's synchronous answer.
type SubmitResult struct {
	Disposition    string
	ProviderRef    string
	ReceiptID      string
	ReasonCode     string
	Message        string
	ResponseDigest string // sha256 hex of the raw response (evidence)
}

// QueryRequest asks the provider what happened to an attempt.
type QueryRequest struct {
	ExternalAttemptID string
	ProviderRef       string
	SubmissionID      string
	TenantID          string
	CorrelationID     string
	ProfileCode       string
	Network           string
}

// Provider-side states a query can answer.
const (
	ProviderReceived    = "RECEIVED"
	ProviderAccepted    = "ACCEPTED"
	ProviderRejected    = "REJECTED"
	ProviderNotReceived = "NOT_RECEIVED"
	ProviderUnknown     = "UNKNOWN"
)

// StatusResult is the provider's authoritative answer.
type StatusResult struct {
	State          string
	ProviderRef    string
	ReceiptID      string
	ReasonCode     string
	Message        string
	ResponseDigest string
}
