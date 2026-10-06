// Package store provides the PostgreSQL implementation of
// obligation-tracking-svc's persistence layer.
//
// Every method wraps its work in setRLS, which sets app.tenant_id on the
// transaction, but that was NOT sufficient on its own: this pool connects as
// a Postgres superuser (same pattern as every other service on this
// platform), and Postgres superusers unconditionally bypass Row Level
// Security — ENABLE ROW LEVEL SECURITY is also skipped for a table's owner
// unless FORCE ROW LEVEL SECURITY is set, and here the owner is postgres
// too. This store additionally carried no explicit tenant_id predicate of
// its own in any query. Migration 000002 adds FORCE; the explicit
// predicates below are the belt to that policy's braces, the same
// defence-in-depth already applied to board-resolutions-svc,
// contract-lifecycle-svc and clause-template-svc after the identical bug
// was found live in each.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/obligation-tracking-svc/internal/domain"
	"zoiko.io/obligation-tracking-svc/internal/middleware"
)

type Store interface {
	CreateObligation(ctx context.Context, o *domain.Obligation) error
	GetObligation(ctx context.Context, id string) (*domain.Obligation, error)
	ListObligations(ctx context.Context, legalEntityID, status, sourceType string) ([]domain.Obligation, error)
	// UpdateObligation edits non-protected fields; only valid while the
	// obligation is CANDIDATE or PLANNED.
	UpdateObligation(ctx context.Context, o *domain.Obligation) error

	ValidateExtractedObligation(ctx context.Context, id, validatedBy string) (*domain.Obligation, error)
	Schedule(ctx context.Context, id string, req *domain.ScheduleObligationRequest) (*domain.Obligation, error)
	MarkDue(ctx context.Context, id, markedBy string) (*domain.Obligation, error)
	MarkInProgress(ctx context.Context, id, markedBy string) (*domain.Obligation, error)
	Complete(ctx context.Context, id, satisfiedBy string) (*domain.Obligation, error)
	Waive(ctx context.Context, id string, req *domain.WaiveObligationRequest) (*domain.Obligation, error)
	RecordBreach(ctx context.Context, id string, req *domain.RecordBreachRequest) (*domain.Obligation, error)
	Dispute(ctx context.Context, id string, req *domain.DisputeObligationRequest) (*domain.Obligation, error)
	Supersede(ctx context.Context, id, supersededBy string) (*domain.Obligation, error)
}

// effectiveDateColumns is the SELECT fragment for date columns that MUST be
// read as text — due_date/effective_from/effective_to are DATE columns, but
// domain.Obligation declares them as string / *string because the API
// contract is a plain "YYYY-MM-DD". Same asymmetric read/write bug already
// fixed in board-resolutions-svc, contract-lifecycle-svc and
// clause-template-svc's stores (pgx encodes a Go string into a DATE
// parameter fine; it cannot decode a DATE into a *string on a plain
// SELECT). TO_CHAR rather than ::TEXT so the format does not depend on the
// session's DateStyle.
const effectiveDateColumns = `TO_CHAR(due_date, 'YYYY-MM-DD'), TO_CHAR(effective_from, 'YYYY-MM-DD'), TO_CHAR(effective_to, 'YYYY-MM-DD')`

const obligationColumns = `obligation_id, tenant_id, legal_entity_id, source_type, source_id, title, COALESCE(description,''),
	       obligation_type, risk_level, status, ` + effectiveDateColumns + `, COALESCE(assigned_to,''),
	       extracted_by_ai, validated_at, validated_by,
	       source_clause_id, COALESCE(trigger_description,''), COALESCE(calculation_method,''), scheduled_at, scheduled_by,
	       due_marked_at, due_marked_by, in_progress_at, in_progress_by,
	       satisfied_at, satisfied_by, waived_at, waived_by, waiver_authority_reference,
	       breached_at, breached_by, breach_note, disputed_at, disputed_by, dispute_reason, superseded_by,
	       created_by, created_at, updated_at`

func scanObligation(row pgx.Row) (*domain.Obligation, error) {
	var o domain.Obligation
	var otype, risk, status string
	err := row.Scan(
		&o.ObligationID, &o.TenantID, &o.LegalEntityID, &o.SourceType, &o.SourceID, &o.Title, &o.Description,
		&otype, &risk, &status, &o.DueDate, &o.EffectiveFrom, &o.EffectiveTo, &o.AssignedTo,
		&o.ExtractedByAI, &o.ValidatedAt, &o.ValidatedBy,
		&o.SourceClauseID, &o.TriggerDescription, &o.CalculationMethod, &o.ScheduledAt, &o.ScheduledBy,
		&o.DueMarkedAt, &o.DueMarkedBy, &o.InProgressAt, &o.InProgressBy,
		&o.SatisfiedAt, &o.SatisfiedBy, &o.WaivedAt, &o.WaivedBy, &o.WaiverAuthorityReference,
		&o.BreachedAt, &o.BreachedBy, &o.BreachNote, &o.DisputedAt, &o.DisputedBy, &o.DisputeReason, &o.SupersededBy,
		&o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrObligationNotFound
		}
		return nil, err
	}
	o.ObligationType = domain.ObligationType(otype)
	o.RiskLevel = domain.RiskLevel(risk)
	o.Status = domain.ObligationStatus(status)
	return &o, nil
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// setRLS installs the caller's tenant for the transaction and refuses an
// empty one rather than defaulting it — see middleware/tenant.go's doc
// comment for why a missing tenant must fail, not pool into a shared bucket.
func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) (string, error) {
	tenantID := middleware.GetTenantID(ctx)
	if tenantID == "" {
		return "", domain.ErrTenantMissing
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return "", err
	}
	return tenantID, nil
}

func (s *PgStore) CreateObligation(ctx context.Context, o *domain.Obligation) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return err
	}

	if o.ObligationID == "" {
		o.ObligationID = "obg-" + uuid.New().String()
	}
	o.TenantID = tenantID
	now := time.Now().UTC()
	o.CreatedAt = now
	o.UpdatedAt = now
	if o.RiskLevel == "" {
		o.RiskLevel = domain.RiskLevelMedium
	}
	// AI extraction cannot activate an obligation without validation
	// (LEG-07 §9.1): an extracted obligation starts as a candidate no
	// matter what status the caller asked for; a manually-entered one
	// starts at PLANNED directly, since there is nothing to validate.
	if o.ExtractedByAI {
		o.Status = domain.ObligationStatusCandidate
	} else {
		o.Status = domain.ObligationStatusPlanned
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO obligations
			(obligation_id, tenant_id, legal_entity_id, source_type, source_id, title, description,
			 obligation_type, risk_level, status, due_date, assigned_to, extracted_by_ai,
			 effective_from, effective_to, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		o.ObligationID, o.TenantID, o.LegalEntityID, o.SourceType, o.SourceID, o.Title, o.Description,
		string(o.ObligationType), string(o.RiskLevel), string(o.Status), o.DueDate, o.AssignedTo, o.ExtractedByAI,
		o.EffectiveFrom, o.EffectiveTo, o.CreatedBy, o.CreatedAt, o.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert obligation: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetObligation(ctx context.Context, id string) (*domain.Obligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	o, err := scanObligation(tx.QueryRow(ctx,
		`SELECT `+obligationColumns+` FROM obligations WHERE obligation_id = $1 AND tenant_id = $2`, id, tenantID))
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	return o, nil
}

// lockObligation reads an obligation FOR UPDATE inside an open transaction
// — the row lock that makes a read-then-write lifecycle transition atomic.
func (s *PgStore) lockObligation(ctx context.Context, tx pgx.Tx, id, tenantID string) (*domain.Obligation, error) {
	return scanObligation(tx.QueryRow(ctx,
		`SELECT `+obligationColumns+` FROM obligations WHERE obligation_id = $1 AND tenant_id = $2 FOR UPDATE`, id, tenantID))
}

func (s *PgStore) ListObligations(ctx context.Context, legalEntityID, status, sourceType string) ([]domain.Obligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT `+obligationColumns+`
		FROM obligations
		WHERE tenant_id = $1
		  AND ($2 = '' OR legal_entity_id = $2)
		  AND ($3 = '' OR status = $3)
		  AND ($4 = '' OR source_type = $4)
		ORDER BY due_date ASC, created_at DESC`, tenantID, legalEntityID, status, sourceType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Obligation
	for rows.Next() {
		o, err := scanObligation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

// UpdateObligation may only act while the obligation is CANDIDATE or
// PLANNED — every later transition goes through its own named command.
func (s *PgStore) UpdateObligation(ctx context.Context, o *domain.Obligation) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return err
	}

	current, err := s.lockObligation(ctx, tx, o.ObligationID, tenantID)
	if err != nil {
		return err
	}
	if current.Status != domain.ObligationStatusCandidate && current.Status != domain.ObligationStatusPlanned {
		return domain.ErrWrongStatus
	}

	o.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE obligations
		SET title=$1, description=$2, obligation_type=$3, risk_level=$4, due_date=$5,
		    assigned_to=$6, effective_to=$7, updated_at=$8
		WHERE obligation_id=$9 AND tenant_id=$10`,
		o.Title, o.Description, string(o.ObligationType), string(o.RiskLevel), o.DueDate,
		o.AssignedTo, o.EffectiveTo, o.UpdatedAt, o.ObligationID, tenantID,
	)
	if err != nil {
		return fmt.Errorf("update obligation: %w", err)
	}

	return tx.Commit(ctx)
}

// ValidateExtractedObligation moves CANDIDATE -> PLANNED. LEG-07 §9.1: "AI
// extraction cannot activate obligation without validation" — this is that
// gate.
func (s *PgStore) ValidateExtractedObligation(ctx context.Context, id, validatedBy string) (*domain.Obligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	o, err := s.lockObligation(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if o.Status != domain.ObligationStatusCandidate {
		return nil, domain.ErrWrongStatus
	}

	now := time.Now().UTC()
	o.Status = domain.ObligationStatusPlanned
	o.ValidatedBy = &validatedBy
	o.ValidatedAt = &now
	o.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE obligations SET status=$1, validated_by=$2, validated_at=$3, updated_at=$4
		WHERE obligation_id=$5 AND tenant_id=$6`,
		string(o.Status), o.ValidatedBy, o.ValidatedAt, o.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// Schedule moves PLANNED -> ACTIVE and certifies the due date's provenance.
// LEG-07 §9: "trigger/date ambiguity blocks due-date certification" —
// enforced by requiring both a trigger description and a calculation
// method before the certification succeeds.
func (s *PgStore) Schedule(ctx context.Context, id string, req *domain.ScheduleObligationRequest) (*domain.Obligation, error) {
	if req.TriggerDescription == "" || req.CalculationMethod == "" {
		return nil, domain.ErrAmbiguousDueDateBasis
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	o, err := s.lockObligation(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if o.Status != domain.ObligationStatusPlanned {
		return nil, domain.ErrWrongStatus
	}

	now := time.Now().UTC()
	o.Status = domain.ObligationStatusActive
	o.SourceClauseID = req.SourceClauseID
	o.TriggerDescription = req.TriggerDescription
	o.CalculationMethod = req.CalculationMethod
	o.ScheduledBy = &req.ScheduledBy
	o.ScheduledAt = &now
	o.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE obligations
		SET status=$1, source_clause_id=$2, trigger_description=$3, calculation_method=$4,
		    scheduled_by=$5, scheduled_at=$6, updated_at=$7
		WHERE obligation_id=$8 AND tenant_id=$9`,
		string(o.Status), o.SourceClauseID, o.TriggerDescription, o.CalculationMethod,
		o.ScheduledBy, o.ScheduledAt, o.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// MarkDue moves ACTIVE -> DUE. Not a named LEG-07 command (the spec names
// only the state), but left unreachable it would repeat the dead-enum-value
// anti-pattern already found and fixed elsewhere in this codebase — kept as
// a minimal, caller-triggered transition until a due-date scheduler exists.
func (s *PgStore) MarkDue(ctx context.Context, id, markedBy string) (*domain.Obligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	o, err := s.lockObligation(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if o.Status != domain.ObligationStatusActive {
		return nil, domain.ErrWrongStatus
	}

	now := time.Now().UTC()
	o.Status = domain.ObligationStatusDue
	o.DueMarkedBy = &markedBy
	o.DueMarkedAt = &now
	o.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE obligations SET status=$1, due_marked_by=$2, due_marked_at=$3, updated_at=$4
		WHERE obligation_id=$5 AND tenant_id=$6`,
		string(o.Status), o.DueMarkedBy, o.DueMarkedAt, o.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// MarkInProgress moves DUE -> IN_PROGRESS. Same status as MarkDue: not a
// named command, kept reachable for the same reason.
func (s *PgStore) MarkInProgress(ctx context.Context, id, markedBy string) (*domain.Obligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	o, err := s.lockObligation(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if o.Status != domain.ObligationStatusDue {
		return nil, domain.ErrWrongStatus
	}

	now := time.Now().UTC()
	o.Status = domain.ObligationStatusInProgress
	o.InProgressBy = &markedBy
	o.InProgressAt = &now
	o.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE obligations SET status=$1, in_progress_by=$2, in_progress_at=$3, updated_at=$4
		WHERE obligation_id=$5 AND tenant_id=$6`,
		string(o.Status), o.InProgressBy, o.InProgressAt, o.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// terminalTransition is the shared shape of Complete/Waive/RecordBreach/
// Dispute: all five require the obligation to be ACTIVE, DUE or IN_PROGRESS
// (work has actually begun or is due) and all are refused once the
// obligation has already reached any final status.
func (s *PgStore) lockActionable(ctx context.Context, tx pgx.Tx, id, tenantID string) (*domain.Obligation, error) {
	o, err := s.lockObligation(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}
	switch o.Status {
	case domain.ObligationStatusActive, domain.ObligationStatusDue, domain.ObligationStatusInProgress:
		return o, nil
	default:
		return nil, domain.ErrWrongStatus
	}
}

func (s *PgStore) Complete(ctx context.Context, id, satisfiedBy string) (*domain.Obligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	o, err := s.lockActionable(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	o.Status = domain.ObligationStatusSatisfied
	o.SatisfiedBy = &satisfiedBy
	o.SatisfiedAt = &now
	o.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE obligations SET status=$1, satisfied_by=$2, satisfied_at=$3, updated_at=$4
		WHERE obligation_id=$5 AND tenant_id=$6`,
		string(o.Status), o.SatisfiedBy, o.SatisfiedAt, o.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// Waive is distinct from satisfaction and requires documented authority
// (LEG-07 §9.1) — enforced by requiring waiver_authority_reference non-empty.
func (s *PgStore) Waive(ctx context.Context, id string, req *domain.WaiveObligationRequest) (*domain.Obligation, error) {
	if req.WaiverAuthorityReference == "" {
		return nil, domain.ErrWaiverAuthorityRequired
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	o, err := s.lockActionable(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	o.Status = domain.ObligationStatusWaived
	o.WaivedBy = &req.WaivedBy
	o.WaivedAt = &now
	o.WaiverAuthorityReference = &req.WaiverAuthorityReference
	o.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE obligations SET status=$1, waived_by=$2, waived_at=$3, waiver_authority_reference=$4, updated_at=$5
		WHERE obligation_id=$6 AND tenant_id=$7`,
		string(o.Status), o.WaivedBy, o.WaivedAt, o.WaiverAuthorityReference, o.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// RecordBreach records the fact of breach only — LEG-07 §9.1: no automatic
// financial accrual, payment or legal remedy. Target domains act separately.
func (s *PgStore) RecordBreach(ctx context.Context, id string, req *domain.RecordBreachRequest) (*domain.Obligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	o, err := s.lockActionable(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	o.Status = domain.ObligationStatusBreached
	o.BreachedBy = &req.BreachedBy
	o.BreachedAt = &now
	if req.BreachNote != "" {
		o.BreachNote = &req.BreachNote
	}
	o.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE obligations SET status=$1, breached_by=$2, breached_at=$3, breach_note=$4, updated_at=$5
		WHERE obligation_id=$6 AND tenant_id=$7`,
		string(o.Status), o.BreachedBy, o.BreachedAt, o.BreachNote, o.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

func (s *PgStore) Dispute(ctx context.Context, id string, req *domain.DisputeObligationRequest) (*domain.Obligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	o, err := s.lockActionable(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	o.Status = domain.ObligationStatusDisputed
	o.DisputedBy = &req.DisputedBy
	o.DisputedAt = &now
	o.DisputeReason = &req.DisputeReason
	o.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE obligations SET status=$1, disputed_by=$2, disputed_at=$3, dispute_reason=$4, updated_at=$5
		WHERE obligation_id=$6 AND tenant_id=$7`,
		string(o.Status), o.DisputedBy, o.DisputedAt, o.DisputeReason, o.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// Supersede may act from any non-final status — a later obligation can
// replace an earlier one before it ever reaches a terminal outcome of its
// own (e.g. a contract amendment restates the obligation entirely).
func (s *PgStore) Supersede(ctx context.Context, id, supersededBy string) (*domain.Obligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tenantID, err := s.setRLS(ctx, tx)
	if err != nil {
		return nil, err
	}

	o, err := s.lockObligation(ctx, tx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if o.Status.IsFinal() {
		return nil, domain.ErrWrongStatus
	}

	now := time.Now().UTC()
	o.Status = domain.ObligationStatusSuperseded
	o.SupersededBy = &supersededBy
	o.UpdatedAt = now

	if _, err := tx.Exec(ctx, `
		UPDATE obligations SET status=$1, superseded_by=$2, updated_at=$3
		WHERE obligation_id=$4 AND tenant_id=$5`,
		string(o.Status), o.SupersededBy, o.UpdatedAt, id, tenantID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}
