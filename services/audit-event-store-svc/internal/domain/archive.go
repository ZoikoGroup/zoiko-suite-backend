// Package domain holds AUD-10's archive/verify types — the half of the
// Audit Trail / Data Export capability that lives inside this service,
// operating directly on its own tamper-evident hash chain. The
// approve/redact/deliver half lives in reporting-orchestration-svc.
package domain

import (
	"errors"
	"time"
)

// Archive is a permanent claim that sequence numbers [FromSequence,
// ToSequence] formed an unbroken hash chain, as of CreatedAt, with digest
// ArchiveDigest. It is never mutated after creation — see migration
// 000004's reject_archive_mutation trigger.
//
// TenantID is the tenant on whose behalf the archive was created. An archive
// spans a range of the GLOBAL, cross-tenant chain, so TenantID is not a
// property of the archived rows — it is the ownership boundary that decides
// which tenant may read this archive record and its verification history
// (migration 000005; RLS on audit_archives is tenant-only).
type Archive struct {
	ArchiveID            string    `json:"archive_id"`
	TenantID             string    `json:"tenant_id"`
	FromSequence         int64     `json:"from_sequence"`
	ToSequence           int64     `json:"to_sequence"`
	EventCount           int64     `json:"event_count"`
	ArchiveDigest        string    `json:"archive_digest"`
	FirstEventHash       string    `json:"first_event_hash"`
	LastEventHash        string    `json:"last_event_hash"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	CreatedAt            time.Time `json:"created_at"`
}

type VerificationResult string

const (
	VerificationVerified VerificationResult = "VERIFIED"
	VerificationDiverged VerificationResult = "DIVERGED"
)

// ArchiveVerification is one VerifyArchive call's outcome — a permanent,
// append-only record in its own right (an archive can be re-verified many
// times over its life; each attempt is kept, never overwritten).
//
// TenantID is the tenant that ran the verification, and is also the boundary
// that scopes the history read (migration 000005).
type ArchiveVerification struct {
	VerificationID         string             `json:"verification_id"`
	ArchiveID              string             `json:"archive_id"`
	TenantID               string             `json:"tenant_id"`
	VerifiedByPrincipalID  string             `json:"verified_by_principal_id"`
	VerifiedAt             time.Time          `json:"verified_at"`
	Result                 VerificationResult `json:"result"`
	FirstDivergentSequence *int64             `json:"first_divergent_sequence,omitempty"`
}

// ── params ───────────────────────────────────────────────────────────────────

type CreateArchiveParams struct {
	FromSequence, ToSequence int64
	TenantID                 string
	CreatedByPrincipalID     string
	// Idem, when non-nil, makes the call idempotent (migration 000007). It is
	// nil when the request carried no Idempotency-Key.
	Idem *Idempotency
}

// Idempotency carries POST /v1/archives' Idempotency-Key into the store. The
// store claims it in the same transaction as the archive row, so a retry —
// sequential or concurrent — receives the original archive instead of
// creating a second permanent record. Fingerprint binds the acting principal
// and the request, so a key reused for a different request, or by a different
// principal, is ErrIdempotencyMismatch. Replayed is set by the store.
type Idempotency struct {
	Key         string
	Endpoint    string
	Fingerprint string
	Replayed    bool
}

type VerifyArchiveParams struct {
	ArchiveID             string
	TenantID              string
	VerifiedByPrincipalID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrArchiveNotFound = errors.New("archive not found")

	// ErrIdempotencyMismatch — the Idempotency-Key was already used for a
	// different request (or by a different principal).
	ErrIdempotencyMismatch = errors.New("this Idempotency-Key was already used for a different request")
	ErrInvalidRange        = errors.New("to_sequence must be greater than or equal to from_sequence")
	ErrEmptyRange          = errors.New("no events exist in the requested sequence range")

	// ErrTenantRequired means an archive operation was reached without a
	// resolved tenant context. Archive records are tenant-owned and
	// tenant-read (migration 000005), so a missing tenant must fail closed
	// rather than fall through to an unscoped query. The HTTP layer maps this
	// to 401 (INV-01: missing tenant scope is an authentication failure).
	ErrTenantRequired = errors.New("tenant context is required for archive operations")

	// ErrChainBroken means a break was found — either a hash-link mismatch or
	// a sequence-number gap — inside the requested range at archive-creation
	// time. No archive row is written; there is nothing to reproduce here,
	// the caller must investigate the chain itself before archiving it.
	ErrChainBroken = errors.New("hash chain is broken within the requested sequence range: archive not created")
)
