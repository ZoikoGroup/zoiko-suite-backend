package store

// Store methods for migration 000008: onboarding idempotency,
// FailedProvisioning, entity verification and non-destructive merge.
//
// Same two rules as pg_store_org.go: every write is version- or state-guarded
// (zero rows is a conflict, never a silent no-op), and every approval that
// releases a write is decided in the same transaction as that write.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// ResolveOnboardingKey reads tenant_onboarding_keys, which has no RLS by
// design (see migration 000008): a replay does not know its tenant.
func (s *PgStore) ResolveOnboardingKey(ctx context.Context, key string) (string, string, error) {
	var tenantID, fp string
	err := s.pool.QueryRow(ctx, `
		SELECT tenant_id, request_fingerprint
		  FROM tenant_onboarding_keys WHERE external_customer_key = $1`, key).Scan(&tenantID, &fp)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return tenantID, fp, err
}

// insertApprovalTx is CreateApprovalRequest inside an existing transaction.
func insertApprovalTx(ctx context.Context, tx pgx.Tx, a *domain.ApprovalRequest) error {
	if _, err := tx.Exec(ctx, `
		UPDATE approval_requests
		   SET status = 'EXPIRED', decided_at = NOW(),
		       decision_note = 'expired before a decision was made'
		 WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3
		   AND status = 'PENDING' AND expires_at <= NOW()`,
		a.TenantID, string(a.SubjectType), a.SubjectID); err != nil {
		return fmt.Errorf("expire stale approvals: %w", err)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO approval_requests (
			approval_request_id, tenant_id, subject_type, subject_id, command_name,
			payload, expected_version, payload_fingerprint, reason,
			requested_by_principal_id, requested_at, expires_at, status, correlation_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'PENDING',$13)`,
		a.ApprovalRequestID, a.TenantID, string(a.SubjectType), a.SubjectID, a.CommandName,
		[]byte(a.Payload), a.ExpectedVersion, a.PayloadFingerprint, a.Reason,
		a.RequestedByPrincipalID, a.RequestedAt, a.ExpiresAt, a.CorrelationID)
	if isUniqueViolation(err) {
		return registry.ErrApprovalPending
	}
	return err
}

// CompleteProvisioning files the creation approval and enqueues tenant.created
// atomically; for RetryProvisioning it also moves the tenant back to
// ONBOARDING and records the command.
func (s *PgStore) CompleteProvisioning(ctx context.Context, p registry.ProvisioningCompletion) error {
	tid := tenantFromCtxOrFallback(ctx, p.TenantID)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		if p.FromFailed {
			var onboardRef, extKey *string
			err := tx.QueryRow(ctx, `
				UPDATE tenants
				   SET lifecycle_state = 'ONBOARDING',
				       provisioning_failure_reason = NULL, provisioning_failed_at = NULL,
				       record_version = record_version + 1,
				       updated_at = $1, updated_by_principal_id = $2
				 WHERE tenant_id = $3 AND tenant_id = $4
				   AND lifecycle_state = 'FAILED_PROVISIONING' AND record_version = $5
				RETURNING onboarding_request_ref, external_customer_key`,
				now, p.ActorID, p.TenantID, tid, p.ExpectedVersion).Scan(&onboardRef, &extKey)
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrConflict
			}
			if err != nil {
				return fmt.Errorf("retry provisioning update: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO tenant_lifecycle_history (
					lifecycle_event_id, tenant_id, from_state, to_state,
					command_name, reason, actor_principal_id, correlation_id, occurred_at,
					onboarding_request_ref, external_customer_key
				) VALUES (gen_random_uuid(), $1, 'FAILED_PROVISIONING', 'ONBOARDING', $2, $3, $4, $5, $6, $7, $8)`,
				p.TenantID, string(domain.TenantCommandRetryProvisioning), p.Reason, p.ActorID,
				nullableString(p.CorrelationID), now, onboardRef, extKey); err != nil {
				return fmt.Errorf("retry provisioning history: %w", err)
			}
		}
		if p.Approval != nil {
			if err := insertApprovalTx(ctx, tx, p.Approval); err != nil {
				return err
			}
		}
		return s.enqueue(ctx, tx, p.Event)
	})
}

// MarkProvisioningFailed records a partial provisioning failure.
func (s *PgStore) MarkProvisioningFailed(ctx context.Context, tenantID, reason, actorID string) error {
	tid := tenantFromCtxOrFallback(ctx, tenantID)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		var onboardRef, extKey *string
		err := tx.QueryRow(ctx, `
			UPDATE tenants
			   SET lifecycle_state = 'FAILED_PROVISIONING',
			       provisioning_failure_reason = $1, provisioning_failed_at = $2,
			       record_version = record_version + 1, updated_at = $2
			 WHERE tenant_id = $3 AND tenant_id = $4 AND lifecycle_state = 'ONBOARDING'
			RETURNING onboarding_request_ref, external_customer_key`,
			reason, now, tenantID, tid).Scan(&onboardRef, &extKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrConflict
		}
		if err != nil {
			return fmt.Errorf("mark provisioning failed: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO tenant_lifecycle_history (
				lifecycle_event_id, tenant_id, from_state, to_state,
				command_name, reason, actor_principal_id, occurred_at,
				onboarding_request_ref, external_customer_key
			) VALUES (gen_random_uuid(), $1, 'ONBOARDING', 'FAILED_PROVISIONING', $2, $3, $4, $5, $6, $7)`,
			tenantID, string(domain.TenantCommandCreate), "provisioning step failed: "+reason, actorID, now,
			onboardRef, extKey)
		return err
	})
}

// abandonProvisioningTx is AbandonProvisioning's compensating cleanup, run in
// the same transaction as the lifecycle move: nothing the half-provisioned
// tenant was given stays live. Nothing is deleted.
func abandonProvisioningTx(ctx context.Context, tx pgx.Tx, tenantID, actorID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE tenant_host_bindings SET active_flag = FALSE WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("deactivate host bindings: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE data_residency_policies
		   SET active_flag = FALSE, updated_at = $1, updated_by_principal_id = $2
		 WHERE tenant_id = $3`, now, actorID, tenantID); err != nil {
		return fmt.Errorf("deactivate residency policies: %w", err)
	}
	return nil
}

// VerifyLegalEntity applies an approved DRAFT → VERIFIED.
func (s *PgStore) VerifyLegalEntity(ctx context.Context, v registry.EntityVerification, ev *outbox.Record) error {
	tid := tenantFromCtxOrFallback(ctx, v.Decision.TenantID)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := decideApprovalTx(ctx, tx, tid, v.Decision, domain.ApprovalApproved); err != nil {
			return err
		}
		now := time.Now().UTC()
		ct, err := tx.Exec(ctx, `
			UPDATE legal_entities
			   SET entity_status = 'VERIFIED',
			       verified_by_principal_id = $1, verified_at = $2,
			       verification_evidence_ref = $3, verification_approval_request_id = $4,
			       record_version = record_version + 1,
			       updated_at = $2, updated_by_principal_id = $1
			 WHERE legal_entity_id = $5 AND tenant_id = $6
			   AND entity_status = 'DRAFT' AND record_version = $7`,
			v.VerifiedBy, now, nullableString(v.EvidenceRef), v.Decision.ApprovalRequestID,
			v.LegalEntityID, tid, v.ExpectedVersion)
		if err != nil {
			return fmt.Errorf("verify entity: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return registry.ErrConflict
		}
		return s.enqueue(ctx, tx, ev)
	})
}

// ActivateLegalEntity applies VERIFIED → ACTIVE.
func (s *PgStore) ActivateLegalEntity(ctx context.Context, legalEntityID, actorID string, expectedVersion int64, ev *outbox.Record) error {
	tid := domain.TenantFromContext(ctx)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
			UPDATE legal_entities
			   SET entity_status = 'ACTIVE', record_version = record_version + 1,
			       updated_at = $1, updated_by_principal_id = $2
			 WHERE legal_entity_id = $3 AND tenant_id = $4
			   AND entity_status = 'VERIFIED' AND record_version = $5`,
			time.Now().UTC(), actorID, legalEntityID, tid, expectedVersion)
		if err != nil {
			return fmt.Errorf("activate entity: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return registry.ErrConflict
		}
		return s.enqueue(ctx, tx, ev)
	})
}

// MergeEntities applies an approved non-destructive merge.
func (s *PgStore) MergeEntities(ctx context.Context, m *domain.EntityMergeRecord, d domain.ApprovalDecision, expectedVersion int64, ev *outbox.Record) error {
	tid := tenantFromCtxOrFallback(ctx, m.TenantID)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := decideApprovalTx(ctx, tx, tid, d, domain.ApprovalApproved); err != nil {
			return err
		}
		// The survivor, locked: it must still be an ACTIVE, unmerged entity
		// in this tenant when the merge lands, not just when it was proposed.
		var survStatus string
		var survMerged *string
		err := tx.QueryRow(ctx, `
			SELECT entity_status, merged_into_legal_entity_id FROM legal_entities
			 WHERE legal_entity_id = $1 AND tenant_id = $2 FOR UPDATE`,
			m.SurvivorLegalEntityID, tid).Scan(&survStatus, &survMerged)
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrNotFound
		}
		if err != nil {
			return err
		}
		if survMerged != nil {
			return fmt.Errorf("%w: survivor is itself merged into another entity", registry.ErrConflict)
		}
		if survStatus != string(domain.EntityStatusActive) {
			return fmt.Errorf("%w: survivor is %s, not ACTIVE", registry.ErrConflict, survStatus)
		}

		now := time.Now().UTC()
		var prior string
		err = tx.QueryRow(ctx, `
			UPDATE legal_entities e
			   SET entity_status = 'DORMANT', merged_into_legal_entity_id = $1, merged_at = $2,
			       record_version = e.record_version + 1,
			       updated_at = $2, updated_by_principal_id = $3
			  FROM (SELECT legal_entity_id, entity_status AS prior FROM legal_entities
			         WHERE legal_entity_id = $4 AND tenant_id = $5 FOR UPDATE) o
			 WHERE e.legal_entity_id = o.legal_entity_id
			   AND e.entity_status IN ('ACTIVE', 'DORMANT', 'SUSPENDED')
			   AND e.merged_into_legal_entity_id IS NULL
			   AND e.record_version = $6
			RETURNING o.prior`,
			m.SurvivorLegalEntityID, now, m.MergedByPrincipalID,
			m.DuplicateLegalEntityID, tid, expectedVersion).Scan(&prior)
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrConflict
		}
		if err != nil {
			return fmt.Errorf("merge duplicate: %w", err)
		}
		m.PriorEntityStatus = domain.EntityStatus(prior)
		m.MergedAt = now
		if _, err := tx.Exec(ctx, `
			INSERT INTO entity_merge_records (
				merge_record_id, tenant_id, duplicate_legal_entity_id, survivor_legal_entity_id,
				prior_entity_status, reason, evidence_ref,
				merged_by_principal_id, merge_approved_by_principal_id, merge_approval_request_id, merged_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			m.MergeRecordID, m.TenantID, m.DuplicateLegalEntityID, m.SurvivorLegalEntityID,
			prior, m.Reason, m.EvidenceRef,
			m.MergedByPrincipalID, m.MergeApprovedByPrincipalID, m.MergeApprovalRequestID, now); err != nil {
			return fmt.Errorf("merge record: %w", err)
		}
		return s.enqueue(ctx, tx, ev)
	})
}

// UnmergeEntity reverses a merge: the duplicate returns to the status it had
// before, and the merge record is completed — never deleted.
func (s *PgStore) UnmergeEntity(ctx context.Context, duplicateID, unmergedBy, reason string, d domain.ApprovalDecision, expectedVersion int64, ev *outbox.Record) error {
	tid := tenantFromCtxOrFallback(ctx, d.TenantID)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := decideApprovalTx(ctx, tx, tid, d, domain.ApprovalApproved); err != nil {
			return err
		}
		var recordID, prior string
		err := tx.QueryRow(ctx, `
			SELECT merge_record_id, prior_entity_status FROM entity_merge_records
			 WHERE duplicate_legal_entity_id = $1 AND tenant_id = $2 AND unmerged_at IS NULL
			 FOR UPDATE`, duplicateID, tid).Scan(&recordID, &prior)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: entity is not merged", registry.ErrConflict)
		}
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		ct, err := tx.Exec(ctx, `
			UPDATE legal_entities
			   SET entity_status = $1, merged_into_legal_entity_id = NULL, merged_at = NULL,
			       record_version = record_version + 1,
			       updated_at = $2, updated_by_principal_id = $3
			 WHERE legal_entity_id = $4 AND tenant_id = $5
			   AND merged_into_legal_entity_id IS NOT NULL AND record_version = $6`,
			prior, now, unmergedBy, duplicateID, tid, expectedVersion)
		if err != nil {
			return fmt.Errorf("unmerge entity: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return registry.ErrConflict
		}
		if _, err := tx.Exec(ctx, `
			UPDATE entity_merge_records
			   SET unmerged_by_principal_id = $1, unmerge_approved_by_principal_id = $2,
			       unmerge_approval_request_id = $3, unmerged_at = $4, unmerge_reason = $5
			 WHERE merge_record_id = $6`,
			unmergedBy, d.DecidedByPrincipalID, d.ApprovalRequestID, now, reason, recordID); err != nil {
			return fmt.Errorf("complete merge record: %w", err)
		}
		return s.enqueue(ctx, tx, ev)
	})
}

// ListEntityMergeRecords returns every merge an entity took part in, as
// duplicate or survivor, newest first.
func (s *PgStore) ListEntityMergeRecords(ctx context.Context, legalEntityID string) ([]*domain.EntityMergeRecord, error) {
	tid := domain.TenantFromContext(ctx)
	out := []*domain.EntityMergeRecord{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT merge_record_id, tenant_id, duplicate_legal_entity_id, survivor_legal_entity_id,
			       prior_entity_status, reason, evidence_ref,
			       merged_by_principal_id, merge_approved_by_principal_id, merge_approval_request_id, merged_at,
			       unmerged_by_principal_id, unmerge_approved_by_principal_id, unmerge_approval_request_id,
			       unmerged_at, unmerge_reason
			  FROM entity_merge_records
			 WHERE tenant_id = $1 AND (duplicate_legal_entity_id = $2 OR survivor_legal_entity_id = $2)
			 ORDER BY merged_at DESC`, tid, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.EntityMergeRecord
			if err := rows.Scan(&m.MergeRecordID, &m.TenantID, &m.DuplicateLegalEntityID, &m.SurvivorLegalEntityID,
				&m.PriorEntityStatus, &m.Reason, &m.EvidenceRef,
				&m.MergedByPrincipalID, &m.MergeApprovedByPrincipalID, &m.MergeApprovalRequestID, &m.MergedAt,
				&m.UnmergedByPrincipalID, &m.UnmergeApprovedByPrincipalID, &m.UnmergeApprovalRequestID,
				&m.UnmergedAt, &m.UnmergeReason); err != nil {
				return err
			}
			out = append(out, &m)
		}
		return rows.Err()
	})
	return out, err
}
