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
type Archive struct {
	ArchiveID            string    `json:"archive_id"`
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
type ArchiveVerification struct {
	VerificationID         string             `json:"verification_id"`
	ArchiveID              string             `json:"archive_id"`
	VerifiedByPrincipalID  string             `json:"verified_by_principal_id"`
	VerifiedAt             time.Time          `json:"verified_at"`
	Result                 VerificationResult `json:"result"`
	FirstDivergentSequence *int64             `json:"first_divergent_sequence,omitempty"`
}

// ── params ───────────────────────────────────────────────────────────────────

type CreateArchiveParams struct {
	FromSequence, ToSequence int64
	CreatedByPrincipalID     string
}

type VerifyArchiveParams struct {
	ArchiveID             string
	VerifiedByPrincipalID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrArchiveNotFound = errors.New("archive not found")
	ErrInvalidRange    = errors.New("to_sequence must be greater than or equal to from_sequence")
	ErrEmptyRange      = errors.New("no events exist in the requested sequence range")

	// ErrChainBroken means a break was found — either a hash-link mismatch or
	// a sequence-number gap — inside the requested range at archive-creation
	// time. No archive row is written; there is nothing to reproduce here,
	// the caller must investigate the chain itself before archiving it.
	ErrChainBroken = errors.New("hash chain is broken within the requested sequence range: archive not created")
)
