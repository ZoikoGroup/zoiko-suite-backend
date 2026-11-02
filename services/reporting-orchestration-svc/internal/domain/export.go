// AUD-10 Audit Trail / Data Export — the export/redact/deliver half.
//
// The archive/verify half lives in audit-event-store-svc, which owns the
// hash chain. This package never re-derives that chain; it only cites the
// real archive record audit-event-store-svc produced (internal/archivestore).
// Building a package therefore cannot fabricate content: if the referenced
// archive cannot be fetched, the export FAILS rather than inventing a
// manifest.
//
// State model (Audit & Assurance spec §3.10, AUD-10):
//
//	REQUESTED ──approve──> APPROVED ──build──> BUILDING ──manifest ok──> VALIDATING
//	BUILDING ──build retried after the lease expired──> BUILDING (re-claimed)
//	                                              │                         │
//	                                              └──archive fetch fails──> FAILED (terminal)
//	VALIDATING ──seal──> SEALED ──deliver──> DELIVERED
//	SEALED / DELIVERED ──revoke (reason required)──> REVOKED (terminal)
//
// Redactions may be recorded only before the package is sealed
// (REQUESTED, APPROVED, VALIDATING). Sealed packages are immutable.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"time"
)

type ExportStatus string

const (
	ExportRequested  ExportStatus = "REQUESTED"
	ExportApproved   ExportStatus = "APPROVED"
	ExportBuilding   ExportStatus = "BUILDING"
	ExportValidating ExportStatus = "VALIDATING"
	ExportSealed     ExportStatus = "SEALED"
	ExportDelivered  ExportStatus = "DELIVERED"
	ExportRevoked    ExportStatus = "REVOKED"
	ExportFailed     ExportStatus = "FAILED"
)

// allowedExportTransitions is the single source of truth for the export
// lifecycle. The store's compare-and-swap UPDATEs use the same "from" states.
var allowedExportTransitions = map[ExportStatus][]ExportStatus{
	ExportRequested:  {ExportApproved},
	ExportApproved:   {ExportBuilding},
	ExportBuilding:   {ExportBuilding, ExportValidating, ExportFailed},
	ExportValidating: {ExportSealed},
	ExportSealed:     {ExportDelivered, ExportRevoked},
	ExportDelivered:  {ExportRevoked},
	ExportRevoked:    {},
	ExportFailed:     {},
}

// CanTransitionExport reports whether the lifecycle allows from -> to.
func CanTransitionExport(from, to ExportStatus) bool {
	for _, allowed := range allowedExportTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// RedactableExportStates are the states in which a redaction decision may be
// recorded: everything before the seal. A sealed package is immutable.
var RedactableExportStates = []ExportStatus{ExportRequested, ExportApproved, ExportValidating}

// IsRedactable reports whether a redaction may be recorded in status.
func IsRedactable(status ExportStatus) bool {
	for _, allowed := range RedactableExportStates {
		if allowed == status {
			return true
		}
	}
	return false
}

// sha256HexPattern matches a lowercase or uppercase 64-hex-digit digest.
var sha256HexPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// IsSHA256Hex reports whether digest looks like a SHA-256 hex digest.
func IsSHA256Hex(digest string) bool {
	return sha256HexPattern.MatchString(digest)
}

// AuditExportRequest tracks one export of an audit archive end to end.
// ArchiveID is a plain string reference into audit-event-store-svc (a
// different service and database), validated live at build time.
type AuditExportRequest struct {
	ExportID               string       `json:"export_id"`
	TenantID               string       `json:"tenant_id"`
	LegalEntityID          string       `json:"legal_entity_id"`
	ArchiveID              string       `json:"archive_id"`
	Purpose                string       `json:"purpose"`
	Status                 ExportStatus `json:"status"`
	Version                int          `json:"version"`
	RequestedByPrincipalID string       `json:"requested_by_principal_id"`
	ApprovedByPrincipalID  *string      `json:"approved_by_principal_id,omitempty"`
	SealedByPrincipalID    *string      `json:"sealed_by_principal_id,omitempty"`
	DeliveredToPrincipalID *string      `json:"delivered_to_principal_id,omitempty"`
	RevokedByPrincipalID   *string      `json:"revoked_by_principal_id,omitempty"`
	RevocationReason       string       `json:"revocation_reason,omitempty"`
	SealDigest             string       `json:"seal_digest,omitempty"`
	FailureReason          string       `json:"failure_reason,omitempty"`
	CreatedAt              time.Time    `json:"created_at"`
	ApprovedAt             *time.Time   `json:"approved_at,omitempty"`
	SealedAt               *time.Time   `json:"sealed_at,omitempty"`
	DeliveredAt            *time.Time   `json:"delivered_at,omitempty"`
	RevokedAt              *time.Time   `json:"revoked_at,omitempty"`
	BuildStartedAt         *time.Time   `json:"build_started_at,omitempty"`
	BuildAttempts          int          `json:"build_attempts"`
}

// ExportManifest is one artifact entry in a built package. Append-only; its
// Sha256 is always a digest fetched from the upstream archive.
type ExportManifest struct {
	ManifestID   string    `json:"manifest_id"`
	ExportID     string    `json:"export_id"`
	ArtifactName string    `json:"artifact_name"`
	Sha256       string    `json:"sha256"`
	RecordedAt   time.Time `json:"recorded_at"`
}

// RedactionDecision records what was redacted from a package and why.
type RedactionDecision struct {
	DecisionID           string    `json:"decision_id"`
	ExportID             string    `json:"export_id"`
	FieldOrScope         string    `json:"field_or_scope"`
	Reason               string    `json:"reason"`
	DecidedByPrincipalID string    `json:"decided_by_principal_id"`
	DecidedAt            time.Time `json:"decided_at"`
}

// DeliveryReceipt records one delivery of a sealed package.
type DeliveryReceipt struct {
	ReceiptID              string    `json:"receipt_id"`
	ExportID               string    `json:"export_id"`
	DeliveredToPrincipalID string    `json:"delivered_to_principal_id"`
	DeliveryChannel        string    `json:"delivery_channel"`
	DeliveredAt            time.Time `json:"delivered_at"`
}

// PackageManifestView is the response of GET /v1/audit-exports/{id}/manifest.
type PackageManifestView struct {
	ExportID          string              `json:"export_id"`
	Status            ExportStatus        `json:"status"`
	Entries           []ExportManifest    `json:"entries"`
	Redactions        []RedactionDecision `json:"redactions"`
	ComputedDigest    string              `json:"computed_digest"`
	SealDigest        string              `json:"seal_digest,omitempty"`
	IntegrityVerified *bool               `json:"integrity_verified,omitempty"`
}

// ComputePackageDigest is the package seal: a SHA-256 over a canonical JSON
// document listing the export, its archive, every manifest entry and every
// redaction decision, each sorted by a stable key. The same inputs always
// produce the same digest, so a sealed package can be re-verified at any
// time by recomputing it (GET .../manifest reports integrity_verified).
func ComputePackageDigest(export *AuditExportRequest, entries []ExportManifest, redactions []RedactionDecision) string {
	type canonicalEntry struct {
		ArtifactName string `json:"artifact_name"`
		Sha256       string `json:"sha256"`
	}
	type canonicalRedaction struct {
		FieldOrScope string `json:"field_or_scope"`
		Reason       string `json:"reason"`
		DecidedBy    string `json:"decided_by"`
	}
	type canonicalPackage struct {
		ExportID   string               `json:"export_id"`
		TenantID   string               `json:"tenant_id"`
		ArchiveID  string               `json:"archive_id"`
		Purpose    string               `json:"purpose"`
		Entries    []canonicalEntry     `json:"entries"`
		Redactions []canonicalRedaction `json:"redactions"`
	}

	canonical := canonicalPackage{
		ExportID:   export.ExportID,
		TenantID:   export.TenantID,
		ArchiveID:  export.ArchiveID,
		Purpose:    export.Purpose,
		Entries:    []canonicalEntry{},
		Redactions: []canonicalRedaction{},
	}
	for _, entry := range entries {
		canonical.Entries = append(canonical.Entries, canonicalEntry{ArtifactName: entry.ArtifactName, Sha256: entry.Sha256})
	}
	for _, redaction := range redactions {
		canonical.Redactions = append(canonical.Redactions, canonicalRedaction{
			FieldOrScope: redaction.FieldOrScope, Reason: redaction.Reason, DecidedBy: redaction.DecidedByPrincipalID,
		})
	}
	sort.Slice(canonical.Entries, func(i, j int) bool {
		if canonical.Entries[i].ArtifactName != canonical.Entries[j].ArtifactName {
			return canonical.Entries[i].ArtifactName < canonical.Entries[j].ArtifactName
		}
		return canonical.Entries[i].Sha256 < canonical.Entries[j].Sha256
	})
	sort.Slice(canonical.Redactions, func(i, j int) bool {
		a, b := canonical.Redactions[i], canonical.Redactions[j]
		if a.FieldOrScope != b.FieldOrScope {
			return a.FieldOrScope < b.FieldOrScope
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		return a.DecidedBy < b.DecidedBy
	})

	document, _ := json.Marshal(canonical) // struct of strings: cannot fail
	sum := sha256.Sum256(document)
	return hex.EncodeToString(sum[:])
}

// ── store parameters ─────────────────────────────────────────────────────────
//
// ExpectedVersion is the If-Match value; 0 means "no precondition".

type CreateExportRequestParams struct {
	TenantID, LegalEntityID, ArchiveID, Purpose, RequestedByPrincipalID string
	Event                                                               OutboxEvent
}

type ApproveExportParams struct {
	ExportID, TenantID, ApprovedByPrincipalID string
	ExpectedVersion                           int
	Event                                     OutboxEvent
}

type StartBuildParams struct {
	ExportID, TenantID string
	ExpectedVersion    int
}

type CompleteBuildParams struct {
	ExportID, TenantID, ArtifactName, Sha256 string
	Event                                    OutboxEvent
}

type FailBuildParams struct {
	ExportID, TenantID, Reason string
	Event                      OutboxEvent
}

type RecordRedactionParams struct {
	ExportID, TenantID, FieldOrScope, Reason, DecidedByPrincipalID string
	ExpectedVersion                                                int
	Event                                                          OutboxEvent
}

type SealExportParams struct {
	ExportID, TenantID, SealedByPrincipalID string
	ExpectedVersion                         int
	Event                                   OutboxEvent
}

type DeliverExportParams struct {
	ExportID, TenantID, DeliveredToPrincipalID, DeliveryChannel string
	ExpectedVersion                                             int
	Event                                                       OutboxEvent
}

type RevokeExportParams struct {
	ExportID, TenantID, RevokedByPrincipalID, Reason string
	ExpectedVersion                                  int
	Event                                            OutboxEvent
}
