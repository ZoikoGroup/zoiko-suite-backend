package store

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"zoiko.io/migration-integrity-svc/internal/domain"
)

// DispositionEntry implements Store. Steps, all in one transaction:
//
//  1. claim the Idempotency-Key;
//  2. lock the job row; If-Match must name its current version (412);
//  3. lock the entry row (404 when it is not on this job);
//  4. re-check the state machine under the locks (409);
//  5. write the decision and its evidence onto the entry;
//  6. bump the job's version (the job's ETag covers its entries);
//  7. enqueue audit_entry_remediated or audit_entry_waived;
//  8. record the response for replay.
//
// The identity rules (segregation of duties, MQ0 owner-only, MQ0 not
// waivable) depend only on immutable facts and are checked by the handler
// before the call (domain.CheckDispositionRules).
func (s *PgStore) DispositionEntry(ctx context.Context, tenantID string, cmd DispositionCommand, claim IdempotencyClaim) (*domain.AuditEntry, int, error) {
	var decided *domain.AuditEntry
	var newVersion int
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		// Step 1.
		if err := claimIdempotencyKey(ctx, tx, tenantID, claim); err != nil {
			return err
		}
		// Step 2.
		job, err := readJob(ctx, tx, tenantID, cmd.JobID, true)
		if err != nil {
			return err
		}
		if cmd.ExpectedVersion != 0 && cmd.ExpectedVersion != job.Version {
			return ErrVersionMismatch
		}
		// Step 3.
		entry, err := lockEntry(ctx, tx, tenantID, cmd.JobID, cmd.EntryID)
		if err != nil {
			return err
		}
		// Step 4.
		if err := domain.CanDisposition(job.Status, entry.Status); err != nil {
			return err
		}
		// Step 5.
		now := time.Now().UTC()
		applyDecision(entry, cmd, now)
		if err := updateEntryDecision(ctx, tx, entry); err != nil {
			return err
		}
		// Step 6.
		newVersion = job.Version + 1
		if _, err := tx.Exec(ctx, `UPDATE migration_jobs SET version = $3, updated_at = $4
			WHERE id = $1 AND tenant_id = $2`, job.ID, tenantID, newVersion, now); err != nil {
			return fmt.Errorf("bump migration_job version: %w", err)
		}
		// Step 7.
		if err := s.enqueueEvent(ctx, tx, tenantID, cmd.ActorID, cmd.CorrelationID, dispositionEvent(job, entry)); err != nil {
			return err
		}
		// Step 8.
		decided = entry
		return completeIdempotencyKey(ctx, tx, tenantID, claimWithStatus(claim, http.StatusOK), entry)
	})
	if err != nil {
		return nil, 0, err
	}
	return decided, newVersion, nil
}

func lockEntry(ctx context.Context, tx pgx.Tx, tenantID, jobID, entryID string) (*domain.AuditEntry, error) {
	entry, err := scanEntry(tx.QueryRow(ctx, `SELECT `+entryColumns+` FROM migration_audit_entries
		WHERE id = $1 AND job_id = $2 AND tenant_id = $3 FOR UPDATE`, entryID, jobID, tenantID))
	if isNoRows(err) {
		return nil, ErrEntryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read audit_entry: %w", err)
	}
	return entry, nil
}

// applyDecision sets the decision fields on the in-memory entry; the same
// values are then written and returned, so response and row agree.
func applyDecision(entry *domain.AuditEntry, cmd DispositionCommand, now time.Time) {
	entry.Status = cmd.TargetStatus
	entry.IsRemediated = cmd.TargetStatus == domain.EntryStatusRemediated
	entry.RemediationReason = cmd.Reason
	entry.RemediationEvidenceRef = cmd.EvidenceRef
	entry.RemediatedBy = cmd.ActorID
	entry.RemediatedAt = &now
	entry.WaiverExpiresAt = nil
	if cmd.TargetStatus == domain.EntryStatusWaived && cmd.WaiverExpiresAt != nil {
		expires := cmd.WaiverExpiresAt.UTC()
		entry.WaiverExpiresAt = &expires
	}
}

func updateEntryDecision(ctx context.Context, tx pgx.Tx, entry *domain.AuditEntry) error {
	_, err := tx.Exec(ctx, `
		UPDATE migration_audit_entries
		   SET status = $3, is_remediated = $4, remediation_reason = $5, remediation_evidence_ref = $6,
		       remediated_by = $7, remediated_at = $8, waiver_expires_at = $9
		 WHERE id = $1 AND tenant_id = $2 AND status = 'QUARANTINED'`,
		entry.ID, entry.TenantID, string(entry.Status), entry.IsRemediated, entry.RemediationReason,
		entry.RemediationEvidenceRef, entry.RemediatedBy, entry.RemediatedAt, entry.WaiverExpiresAt)
	if err != nil {
		return fmt.Errorf("record audit_entry decision: %w", err)
	}
	return nil
}

// dispositionEvent carries identifiers, classification and the decision —
// never the offending source value (ZS-MIG-001 §25: errors use object IDs and
// reason codes, not raw record content).
func dispositionEvent(job *domain.MigrationJob, entry *domain.AuditEntry) migrationEvent {
	fact, legacyType := "remediated", "migration.audit_entry_remediated"
	if entry.Status == domain.EntryStatusWaived {
		fact, legacyType = "waived", "migration.audit_entry_waived"
	}
	return migrationEvent{
		TypeSegment: "audit-entry", Fact: fact, LegacyType: legacyType,
		AggregateType: aggregateAuditEntry, AggregateID: entry.ID, LegalEntityID: job.LegalEntityID,
		Data: map[string]any{
			"job_id":   job.ID,
			"entry_id": entry.ID,
			// is_remediated was in the event before the WAIVED disposition
			// existed; consumers on schema 1.0.0 still read it, so it stays
			// beside status.
			"is_remediated":            entry.IsRemediated,
			"record_ref":               entry.RecordRef,
			"violation_type":           string(entry.ViolationType),
			"severity":                 string(entry.Severity),
			"reason_family":            string(entry.ReasonFamily),
			"status":                   string(entry.Status),
			"remediation_reason":       entry.RemediationReason,
			"remediation_evidence_ref": entry.RemediationEvidenceRef,
			"remediated_by":            entry.RemediatedBy,
			"remediated_at":            entry.RemediatedAt,
			"waiver_expires_at":        entry.WaiverExpiresAt,
		},
	}
}
