package domain

import (
	"errors"
	"time"
)

// UploadReservation is ZS-SVC-S-001 §3.4's reservation: "Client reserves an
// upload with tenant, intended purpose, expected size, declared content type
// and idempotency key." Completion validates the bytes against it and, only if
// every gate passes, commits an immutable version.
//
// CommitStatus is the subset of §3.3's commit_status this service produces:
// RESERVED until completion, then exactly one of COMMITTED, FAILED or
// QUARANTINED. UPLOADING and VALIDATING are not separate states because
// completion validates synchronously in one request.
type UploadReservation struct {
	ReservationID       string     `json:"reservation_id"`
	TenantID            string     `json:"tenant_id"`
	DocumentID          string     `json:"document_id"`
	ReservedBy          string     `json:"reserved_by_principal_id"`
	Purpose             string     `json:"purpose"`
	ExpectedSizeBytes   int64      `json:"expected_size_bytes"`
	DeclaredContentType string     `json:"declared_content_type"`
	CommitStatus        string     `json:"commit_status"`
	FailureReason       string     `json:"failure_reason,omitempty"`
	CommittedVersionID  *string    `json:"committed_version_id,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	CompletedAt         *time.Time `json:"completed_at,omitempty"`
}

const (
	ReservationReserved    = "RESERVED"
	ReservationCommitted   = "COMMITTED"
	ReservationFailed      = "FAILED"
	ReservationQuarantined = "QUARANTINED"
)

// CreateUploadReservationParams: RequestHash fingerprints the request so the same
// Idempotency-Key with a different request is refused rather than answered with
// someone else's reservation.
type CreateUploadReservationParams struct {
	DocumentID          string
	ReservedByPrincipal string
	Purpose             string
	ExpectedSizeBytes   int64
	DeclaredContentType string
	IdempotencyKey      string
	RequestHash         string
}

var (
	ErrUploadReservationNotFound = errors.New("upload reservation not found")
	// ErrUploadReservationClosed — the reservation already reached FAILED or
	// QUARANTINED. It is terminal: reserve again to retry.
	ErrUploadReservationClosed = errors.New("upload reservation is closed")
)
