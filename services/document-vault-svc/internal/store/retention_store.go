package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/document-vault-svc/internal/domain"
)

// ── Retention Rule Versions ──────────────────────────────────────────────────

const retentionRuleColumns = `
	retention_rule_version_id, tenant_id, record_class, jurisdiction_selector, legal_basis_ref, purpose_ref,
	trigger_type, duration_days, disposition_action, status, created_by_principal_id, created_at,
	approved_by_principal_id, approved_at, superseded_by_version_id
`

func scanRetentionRule(row pgx.Row, rr *domain.RetentionRuleVersion) error {
	return row.Scan(&rr.RetentionRuleVersionID, &rr.TenantID, &rr.RecordClass, &rr.JurisdictionSelector, &rr.LegalBasisRef, &rr.PurposeRef,
		&rr.TriggerType, &rr.DurationDays, &rr.DispositionAction, &rr.Status, &rr.CreatedByPrincipalID, &rr.CreatedAt,
		&rr.ApprovedByPrincipalID, &rr.ApprovedAt, &rr.SupersededByVersionID)
}

func (s *PgStore) CreateRetentionRuleVersion(ctx context.Context, p domain.CreateRetentionRuleVersionParams) (*domain.RetentionRuleVersion, error) {
	if !domain.RecordClass(p.RecordClass).Valid() {
		return nil, domain.ErrInvalidRecordClass
	}
	if p.JurisdictionSelector == "" {
		return nil, domain.ErrJurisdictionScopeRequired
	}
	if !domain.RetentionTriggerType(p.TriggerType).Valid() {
		return nil, domain.ErrInvalidTriggerType
	}
	if !domain.DispositionAction(p.DispositionAction).Valid() {
		return nil, domain.ErrInvalidDispositionAction
	}
	if p.DurationDays <= 0 {
		return nil, fmt.Errorf("duration_days must be positive")
	}

	var out domain.RetentionRuleVersion
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO retention_rule_versions (tenant_id, record_class, jurisdiction_selector, legal_basis_ref,
				purpose_ref, trigger_type, duration_days, disposition_action, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+retentionRuleColumns,
			tenantID, p.RecordClass, p.JurisdictionSelector, p.LegalBasisRef, p.PurposeRef,
			p.TriggerType, p.DurationDays, p.DispositionAction, p.CreatedByPrincipalID,
		)
		return scanRetentionRule(row, &out)
	})
	if err != nil {
		return nil, fmt.Errorf("document store unavailable: %w", mapPgError(err))
	}
	return &out, nil
}

func (s *PgStore) GetRetentionRuleVersion(ctx context.Context, retentionRuleVersionID string) (*domain.RetentionRuleVersion, error) {
	var out domain.RetentionRuleVersion
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+retentionRuleColumns+` FROM retention_rule_versions WHERE retention_rule_version_id = $1 AND tenant_id::text = $2`,
			retentionRuleVersionID, tenantID)
		if err := scanRetentionRule(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRetentionRuleNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func loadRetentionRuleForUpdate(ctx context.Context, tx pgx.Tx, tenantID, id string) (*domain.RetentionRuleVersion, error) {
	var rr domain.RetentionRuleVersion
	row := tx.QueryRow(ctx, `SELECT `+retentionRuleColumns+` FROM retention_rule_versions WHERE retention_rule_version_id = $1 AND tenant_id::text = $2 FOR UPDATE`,
		id, tenantID)
	if err := scanRetentionRule(row, &rr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRetentionRuleNotFound
		}
		return nil, fmt.Errorf("document store unavailable: %w", mapPgError(err))
	}
	return &rr, nil
}

// ApproveRetentionRuleVersion enforces the maker-checker gate: the
// approver must differ from the rule's own creator (migration
// 000010's own CHECK constraint is the backstop; this is the
// friendlier pre-check).
func (s *PgStore) ApproveRetentionRuleVersion(ctx context.Context, p domain.ApproveRetentionRuleVersionParams) (*domain.RetentionRuleVersion, error) {
	var out domain.RetentionRuleVersion
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		rr, err := loadRetentionRuleForUpdate(ctx, tx, tenantID, p.RetentionRuleVersionID)
		if err != nil {
			return err
		}
		if rr.Status != domain.RetentionRuleDraft {
			return domain.ErrRetentionRuleNotDraft
		}
		if rr.CreatedByPrincipalID == p.ApprovedByPrincipalID {
			return domain.ErrRetentionRuleSelfApproval
		}
		row := tx.QueryRow(ctx, `
			UPDATE retention_rule_versions SET status = 'APPROVED', approved_by_principal_id = $2, approved_at = now()
			WHERE retention_rule_version_id = $1 RETURNING `+retentionRuleColumns,
			p.RetentionRuleVersionID, p.ApprovedByPrincipalID,
		)
		return scanRetentionRule(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) ActivateRetentionRuleVersion(ctx context.Context, retentionRuleVersionID string) (*domain.RetentionRuleVersion, error) {
	var out domain.RetentionRuleVersion
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		rr, err := loadRetentionRuleForUpdate(ctx, tx, tenantID, retentionRuleVersionID)
		if err != nil {
			return err
		}
		if rr.Status != domain.RetentionRuleApproved {
			return domain.ErrRetentionRuleNotApproved
		}
		row := tx.QueryRow(ctx, `UPDATE retention_rule_versions SET status = 'ACTIVE' WHERE retention_rule_version_id = $1 RETURNING `+retentionRuleColumns,
			retentionRuleVersionID)
		return scanRetentionRule(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Record Retention States ──────────────────────────────────────────────────

const retentionStateColumns = `
	retention_state_id, tenant_id, record_id, retention_rule_version_id, trigger_date, due_at, state,
	review_notes, reviewed_by_principal_id, reviewed_at, approved_for_disposition_by_principal_id,
	approved_for_disposition_at, created_by_principal_id, created_at
`

func scanRetentionState(row pgx.Row, st *domain.RecordRetentionState) error {
	return row.Scan(&st.RetentionStateID, &st.TenantID, &st.RecordID, &st.RetentionRuleVersionID, &st.TriggerDate, &st.DueAt, &st.State,
		&st.ReviewNotes, &st.ReviewedByPrincipalID, &st.ReviewedAt, &st.ApprovedForDispositionByPrincipalID,
		&st.ApprovedForDispositionAt, &st.CreatedByPrincipalID, &st.CreatedAt)
}

// BindRetentionRule binds a record to an ACTIVE retention rule version
// whose record_class/jurisdiction_selector match the record's own. If
// TriggerDate is supplied, the state starts ACTIVE immediately with
// due_at computed; otherwise it starts WAITING_FOR_TRIGGER.
func (s *PgStore) BindRetentionRule(ctx context.Context, p domain.BindRetentionRuleParams) (*domain.RecordRetentionState, error) {
	var out domain.RecordRetentionState
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var recordClass, jurisdictionScope string
		if err := tx.QueryRow(ctx, `SELECT record_class, jurisdiction_scope FROM records WHERE record_id = $1 AND tenant_id::text = $2`,
			p.RecordID, tenantID).Scan(&recordClass, &jurisdictionScope); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRecordNotFound
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}

		rule, err := loadRetentionRuleForUpdate(ctx, tx, tenantID, p.RetentionRuleVersionID)
		if err != nil {
			return err
		}
		if rule.Status != domain.RetentionRuleActive {
			return domain.ErrRetentionRuleNotActive
		}
		if string(rule.RecordClass) != recordClass || rule.JurisdictionSelector != jurisdictionScope {
			return domain.ErrRetentionRuleMismatch
		}

		var state domain.RetentionState = domain.RetentionWaitingForTrigger
		var dueAt *time.Time
		if p.TriggerDate != nil {
			state = domain.RetentionActive
			d := p.TriggerDate.AddDate(0, 0, rule.DurationDays)
			dueAt = &d
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO record_retention_states (tenant_id, record_id, retention_rule_version_id, trigger_date, due_at, state, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+retentionStateColumns,
			tenantID, p.RecordID, p.RetentionRuleVersionID, p.TriggerDate, dueAt, state, p.CreatedByPrincipalID,
		)
		if err := scanRetentionState(row, &out); err != nil {
			if isUniqueViolationOn(err, "record_retention_states_record_id_key") {
				return domain.ErrRecordAlreadyBound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) GetRecordRetentionState(ctx context.Context, retentionStateID string) (*domain.RecordRetentionState, error) {
	var out domain.RecordRetentionState
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+retentionStateColumns+` FROM record_retention_states WHERE retention_state_id = $1 AND tenant_id::text = $2`,
			retentionStateID, tenantID)
		if err := scanRetentionState(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRetentionStateNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) GetRetentionStateByRecord(ctx context.Context, recordID string) (*domain.RecordRetentionState, error) {
	var out domain.RecordRetentionState
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+retentionStateColumns+` FROM record_retention_states WHERE record_id = $1 AND tenant_id::text = $2`,
			recordID, tenantID)
		if err := scanRetentionState(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRetentionStateNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func loadRetentionStateForUpdate(ctx context.Context, tx pgx.Tx, tenantID, id string) (*domain.RecordRetentionState, error) {
	var st domain.RecordRetentionState
	row := tx.QueryRow(ctx, `SELECT `+retentionStateColumns+` FROM record_retention_states WHERE retention_state_id = $1 AND tenant_id::text = $2 FOR UPDATE`,
		id, tenantID)
	if err := scanRetentionState(row, &st); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRetentionStateNotFound
		}
		return nil, fmt.Errorf("document store unavailable: %w", mapPgError(err))
	}
	return &st, nil
}

func (s *PgStore) RecordTriggerEvent(ctx context.Context, p domain.RecordTriggerEventParams) (*domain.RecordRetentionState, error) {
	var out domain.RecordRetentionState
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		st, err := loadRetentionStateForUpdate(ctx, tx, tenantID, p.RetentionStateID)
		if err != nil {
			return err
		}
		if st.State != domain.RetentionWaitingForTrigger {
			return domain.ErrRetentionStateNotWaiting
		}
		rule, err := loadRetentionRuleForUpdate(ctx, tx, tenantID, st.RetentionRuleVersionID)
		if err != nil {
			return err
		}
		dueAt := p.TriggerDate.AddDate(0, 0, rule.DurationDays)
		row := tx.QueryRow(ctx, `
			UPDATE record_retention_states SET trigger_date = $2, due_at = $3, state = 'ACTIVE'
			WHERE retention_state_id = $1 RETURNING `+retentionStateColumns,
			p.RetentionStateID, p.TriggerDate, dueAt,
		)
		return scanRetentionState(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// EvaluateDue moves ACTIVE -> DUE only if AsOf is at or after due_at —
// retention can never be declared due early.
func (s *PgStore) EvaluateDue(ctx context.Context, p domain.EvaluateDueParams) (*domain.RecordRetentionState, error) {
	var out domain.RecordRetentionState
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		st, err := loadRetentionStateForUpdate(ctx, tx, tenantID, p.RetentionStateID)
		if err != nil {
			return err
		}
		if st.State != domain.RetentionActive {
			return domain.ErrRetentionStateNotActive
		}
		if st.DueAt == nil || p.AsOf.Before(*st.DueAt) {
			return domain.ErrNotYetDue
		}
		row := tx.QueryRow(ctx, `UPDATE record_retention_states SET state = 'DUE' WHERE retention_state_id = $1 RETURNING `+retentionStateColumns,
			p.RetentionStateID)
		return scanRetentionState(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) FlagForReview(ctx context.Context, p domain.FlagForReviewParams) (*domain.RecordRetentionState, error) {
	var out domain.RecordRetentionState
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		st, err := loadRetentionStateForUpdate(ctx, tx, tenantID, p.RetentionStateID)
		if err != nil {
			return err
		}
		if st.State != domain.RetentionDue {
			return domain.ErrRetentionStateNotDue
		}
		row := tx.QueryRow(ctx, `
			UPDATE record_retention_states SET state = 'REVIEW_REQUIRED', review_notes = $2, reviewed_by_principal_id = $3, reviewed_at = now()
			WHERE retention_state_id = $1 RETURNING `+retentionStateColumns,
			p.RetentionStateID, p.ReviewNotes, p.ReviewedByPrincipalID,
		)
		return scanRetentionState(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ApproveForDisposition is DRC-03's single most important gate:
// REVIEW_REQUIRED -> APPROVED_FOR_DISPOSITION is refused outright if
// any active legal hold target covers this record (DRC-I10/I12). This
// is the dry-run boundary — no command anywhere in this package moves
// a record past this state.
func (s *PgStore) ApproveForDisposition(ctx context.Context, p domain.ApproveForDispositionParams) (*domain.RecordRetentionState, error) {
	var out domain.RecordRetentionState
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		st, err := loadRetentionStateForUpdate(ctx, tx, tenantID, p.RetentionStateID)
		if err != nil {
			return err
		}
		if st.State != domain.RetentionReviewRequired {
			return domain.ErrRetentionStateNotReviewRequired
		}

		var underHold bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM legal_hold_targets
			WHERE record_id = $1 AND tenant_id::text = $2 AND applied_at IS NOT NULL AND released_at IS NULL)`,
			st.RecordID, tenantID).Scan(&underHold); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if underHold {
			return domain.ErrRecordUnderLegalHold
		}

		row := tx.QueryRow(ctx, `
			UPDATE record_retention_states SET state = 'APPROVED_FOR_DISPOSITION',
				approved_for_disposition_by_principal_id = $2, approved_for_disposition_at = now()
			WHERE retention_state_id = $1 RETURNING `+retentionStateColumns,
			p.RetentionStateID, p.ApprovedByPrincipalID,
		)
		return scanRetentionState(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Legal Holds ──────────────────────────────────────────────────────────────

const legalHoldColumns = `
	hold_id, tenant_id, matter_ref, authority_ref, hold_reason_code, status, issued_by_principal_id, issued_at,
	activated_by_principal_id, activated_at, release_reason, released_by_principal_id,
	release_approved_by_principal_id, released_at
`

func scanLegalHold(row pgx.Row, h *domain.LegalHold) error {
	return row.Scan(&h.HoldID, &h.TenantID, &h.MatterRef, &h.AuthorityRef, &h.HoldReasonCode, &h.Status, &h.IssuedByPrincipalID, &h.IssuedAt,
		&h.ActivatedByPrincipalID, &h.ActivatedAt, &h.ReleaseReason, &h.ReleasedByPrincipalID,
		&h.ReleaseApprovedByPrincipalID, &h.ReleasedAt)
}

const legalHoldTargetColumns = `hold_target_id, tenant_id, hold_id, record_id, applied_at, released_at, created_at`

func scanLegalHoldTarget(row pgx.Row, t *domain.LegalHoldTarget) error {
	return row.Scan(&t.HoldTargetID, &t.TenantID, &t.HoldID, &t.RecordID, &t.AppliedAt, &t.ReleasedAt, &t.CreatedAt)
}

// CreateLegalHold opens a DRAFT hold with an explicit set of target
// records — §5.5's scope_selector is deliberately simplified to a
// caller-supplied record_id list in this wave; no dynamic scope-query
// resolver is built.
func (s *PgStore) CreateLegalHold(ctx context.Context, p domain.CreateLegalHoldParams) (*domain.LegalHold, error) {
	if len(p.RecordIDs) == 0 {
		return nil, domain.ErrNoRecordIDsForHold
	}
	var out domain.LegalHold
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		for _, recordID := range p.RecordIDs {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM records WHERE record_id = $1 AND tenant_id::text = $2)`,
				recordID, tenantID).Scan(&exists); err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			if !exists {
				return domain.ErrRecordNotFound
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO legal_holds (tenant_id, matter_ref, authority_ref, hold_reason_code, issued_by_principal_id)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+legalHoldColumns,
			tenantID, p.MatterRef, p.AuthorityRef, p.HoldReasonCode, p.IssuedByPrincipalID,
		)
		if err := scanLegalHold(row, &out); err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}

		for _, recordID := range p.RecordIDs {
			if _, err := tx.Exec(ctx, `INSERT INTO legal_hold_targets (tenant_id, hold_id, record_id) VALUES ($1, $2, $3)`,
				tenantID, out.HoldID, recordID); err != nil {
				return fmt.Errorf("document store unavailable: %w", mapPgError(err))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) GetLegalHold(ctx context.Context, holdID string) (*domain.LegalHold, error) {
	var out domain.LegalHold
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+legalHoldColumns+` FROM legal_holds WHERE hold_id = $1 AND tenant_id::text = $2`, holdID, tenantID)
		if err := scanLegalHold(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrLegalHoldNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func loadLegalHoldForUpdate(ctx context.Context, tx pgx.Tx, tenantID, id string) (*domain.LegalHold, error) {
	var h domain.LegalHold
	row := tx.QueryRow(ctx, `SELECT `+legalHoldColumns+` FROM legal_holds WHERE hold_id = $1 AND tenant_id::text = $2 FOR UPDATE`, id, tenantID)
	if err := scanLegalHold(row, &h); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrLegalHoldNotFound
		}
		return nil, fmt.Errorf("document store unavailable: %w", mapPgError(err))
	}
	return &h, nil
}

// ActivateLegalHold moves DRAFT -> ACTIVE and applies every existing
// target in the same transaction.
func (s *PgStore) ActivateLegalHold(ctx context.Context, p domain.ActivateLegalHoldParams) (*domain.LegalHold, error) {
	var out domain.LegalHold
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		h, err := loadLegalHoldForUpdate(ctx, tx, tenantID, p.HoldID)
		if err != nil {
			return err
		}
		if h.Status != domain.LegalHoldDraft {
			return domain.ErrLegalHoldNotDraft
		}
		if _, err := tx.Exec(ctx, `UPDATE legal_hold_targets SET applied_at = now() WHERE hold_id = $1 AND tenant_id::text = $2 AND applied_at IS NULL`,
			p.HoldID, tenantID); err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		row := tx.QueryRow(ctx, `
			UPDATE legal_holds SET status = 'ACTIVE', activated_by_principal_id = $2, activated_at = now()
			WHERE hold_id = $1 RETURNING `+legalHoldColumns,
			p.HoldID, p.ActivatedByPrincipalID,
		)
		return scanLegalHold(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AddLegalHoldTarget adds a new target to an already-ACTIVE hold,
// applying it immediately (there is no asynchronous apply pipeline in
// this wave, so a newly added target is never left in a pending
// state).
func (s *PgStore) AddLegalHoldTarget(ctx context.Context, p domain.AddLegalHoldTargetParams) (*domain.LegalHoldTarget, error) {
	var out domain.LegalHoldTarget
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		h, err := loadLegalHoldForUpdate(ctx, tx, tenantID, p.HoldID)
		if err != nil {
			return err
		}
		var recordExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM records WHERE record_id = $1 AND tenant_id::text = $2)`,
			p.RecordID, tenantID).Scan(&recordExists); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if !recordExists {
			return domain.ErrRecordNotFound
		}

		appliedNow := h.Status == domain.LegalHoldActive
		row := tx.QueryRow(ctx, `
			INSERT INTO legal_hold_targets (tenant_id, hold_id, record_id, applied_at)
			VALUES ($1, $2, $3, CASE WHEN $4 THEN now() ELSE NULL END)
			RETURNING `+legalHoldTargetColumns,
			tenantID, p.HoldID, p.RecordID, appliedNow,
		)
		if err := scanLegalHoldTarget(row, &out); err != nil {
			if isUniqueViolationOn(err, "legal_hold_targets_unique") {
				return domain.ErrLegalHoldTargetExists
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ReleaseLegalHold moves ACTIVE -> RELEASED and releases every
// still-applied target in the same transaction. Enforces ZS-SVC-S-001
// §5.5's maker-checker gate: the approver must differ from the
// principal executing the release (migration 000011's own CHECK
// constraint is the backstop; this is the friendlier pre-check — same
// shape as ApproveRetentionRuleVersion's self-approval guard above).
func (s *PgStore) ReleaseLegalHold(ctx context.Context, p domain.ReleaseLegalHoldParams) (*domain.LegalHold, error) {
	var out domain.LegalHold
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		h, err := loadLegalHoldForUpdate(ctx, tx, tenantID, p.HoldID)
		if err != nil {
			return err
		}
		if h.Status != domain.LegalHoldActive {
			return domain.ErrLegalHoldNotActive
		}
		if p.ReleasedByPrincipalID == p.ReleaseApprovedByPrincipalID {
			return domain.ErrLegalHoldSelfRelease
		}
		if _, err := tx.Exec(ctx, `
			UPDATE legal_hold_targets SET released_at = now()
			WHERE hold_id = $1 AND tenant_id::text = $2 AND applied_at IS NOT NULL AND released_at IS NULL`,
			p.HoldID, tenantID); err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		row := tx.QueryRow(ctx, `
			UPDATE legal_holds SET status = 'RELEASED', release_reason = $2, released_by_principal_id = $3,
				release_approved_by_principal_id = $4, released_at = now()
			WHERE hold_id = $1 RETURNING `+legalHoldColumns,
			p.HoldID, p.ReleaseReason, p.ReleasedByPrincipalID, p.ReleaseApprovedByPrincipalID,
		)
		return scanLegalHold(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) ListLegalHoldTargets(ctx context.Context, holdID string) ([]domain.LegalHoldTarget, error) {
	var out []domain.LegalHoldTarget
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, err := tx.Query(ctx, `SELECT `+legalHoldTargetColumns+` FROM legal_hold_targets WHERE hold_id = $1 AND tenant_id::text = $2 ORDER BY created_at`,
			holdID, tenantID)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.LegalHoldTarget
			if err := scanLegalHoldTarget(rows, &t); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
