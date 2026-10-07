package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/middleware"
)

// DeadlineStore is BIZ-08's own persistence contract — kept separate
// from the existing Store/FindingStore/TaskStore interfaces, same
// composition pattern used for BIZ-05's own TaskStore.
type DeadlineStore interface {
	CreateDeadline(ctx context.Context, p domain.CreateDeadlineParams) (*domain.Deadline, error)
	// MirrorAuthoritativeDeadline returns the superseded deadline's id
	// when this call superseded a prior live mirror, so the caller can
	// publish DeadlineSuperseded against it.
	MirrorAuthoritativeDeadline(ctx context.Context, p domain.MirrorAuthoritativeDeadlineParams) (d *domain.Deadline, supersededDeadlineID string, err error)
	GetDeadline(ctx context.Context, deadlineID string) (*domain.Deadline, error)
	GetSourceDeadline(ctx context.Context, tenantID, deadlineID string) (*domain.DeadlineSourceInfo, error)
	AssignOwner(ctx context.Context, p domain.AssignOwnerParams) (*domain.Deadline, error)
	CompleteDeadline(ctx context.Context, p domain.CompleteDeadlineParams) (*domain.Deadline, error)

	Recalculate(ctx context.Context, p domain.RecalculateParams) (*domain.Deadline, error)
	Waive(ctx context.Context, p domain.WaiveParams) (*domain.Deadline, error)
	CancelDeadline(ctx context.Context, p domain.CancelDeadlineParams) (*domain.Deadline, error)
	Escalate(ctx context.Context, p domain.EscalateParams) (*domain.DeadlineEscalation, error)
	// ListUpcoming/ListOverdue return the matching deadlines plus the ids
	// among them that were JUST observed crossing their threshold for the
	// first time by this very call — the caller publishes
	// DeadlineDueSoon/DeadlineOverdue for exactly those ids. See migration
	// 000005's own doc comment for why this read-triggers-write shape
	// exists instead of a background clock service.
	ListUpcoming(ctx context.Context, p domain.ListUpcomingParams) (deadlines []domain.Deadline, justNotifiedIDs []string, err error)
	ListOverdue(ctx context.Context, p domain.ListOverdueParams) (deadlines []domain.Deadline, justNotifiedIDs []string, err error)
	ExplainCalculation(ctx context.Context, tenantID, deadlineID string) (*domain.ExplainCalculationResult, error)
}

func (s *PgStore) deadlineSetRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	return err
}

const deadlineColumns = `deadline_id, tenant_id, legal_entity_id, title, linked_object_type, linked_object_id,
	due_at, owner_principal_id, status, source_type, source_ref, source_version, calc_rule, calc_inputs,
	completed_at, completed_by_principal_id, waived_at, waived_by_principal_id, waiver_reason,
	cancelled_at, cancelled_by_principal_id, cancel_reason, superseded_at, superseded_by_deadline_id,
	due_soon_notified_at, overdue_notified_at,
	created_by, created_at, updated_at`

func scanDeadline(row pgx.Row) (*domain.Deadline, error) {
	d := &domain.Deadline{}
	var supersededByDeadlineID *string
	err := row.Scan(&d.DeadlineID, &d.TenantID, &d.LegalEntityID, &d.Title, &d.LinkedObjectType, &d.LinkedObjectID,
		&d.DueAt, &d.OwnerPrincipalID, &d.Status, &d.SourceType, &d.SourceRef, &d.SourceVersion, &d.CalcRule, &d.CalcInputs,
		&d.CompletedAt, &d.CompletedByPrincipalID, &d.WaivedAt, &d.WaivedByPrincipalID, &d.WaiverReason,
		&d.CancelledAt, &d.CancelledByPrincipalID, &d.CancelReason, &d.SupersededAt, &supersededByDeadlineID,
		&d.DueSoonNotifiedAt, &d.OverdueNotifiedAt,
		&d.CreatedBy, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if supersededByDeadlineID != nil {
		d.SupersededByDeadlineID = *supersededByDeadlineID
	}
	return d, nil
}

// CreateDeadline — BIZ-08's own CreateDeadline command, for a deadline
// BIZ-08 calculates and owns outright. Lands SCHEDULED with no source
// lock.
func (s *PgStore) CreateDeadline(ctx context.Context, p domain.CreateDeadlineParams) (*domain.Deadline, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.deadlineSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	deadlineID := "deadline-" + uuid.New().String()
	row := tx.QueryRow(ctx, `INSERT INTO deadlines (
			deadline_id, tenant_id, legal_entity_id, title, linked_object_type, linked_object_id,
			due_at, owner_principal_id, calc_rule, calc_inputs, created_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+deadlineColumns,
		deadlineID, p.TenantID, p.LegalEntityID, p.Title, p.LinkedObjectType, p.LinkedObjectID,
		p.DueAt, p.OwnerPrincipalID, p.CalcRule, p.CalcInputs, p.CreatedByPrincipalID)
	d, err := scanDeadline(row)
	if err != nil {
		return nil, fmt.Errorf("insert deadline: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// MirrorAuthoritativeDeadline — BIZ-08's own MirrorAuthoritativeDeadline
// command. See domain.MirrorAuthoritativeDeadlineParams's own doc
// comment for the full supersede-vs-conflict decision this makes when a
// live mirror for the same source already exists.
func (s *PgStore) MirrorAuthoritativeDeadline(ctx context.Context, p domain.MirrorAuthoritativeDeadlineParams) (*domain.Deadline, string, error) {
	if p.SourceType == "" || p.SourceRef == "" || p.SourceVersion == "" {
		return nil, "", domain.ErrDeadlineSourceRequired
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.deadlineSetRLS(ctx, tx); err != nil {
		return nil, "", err
	}

	existing, err := scanDeadline(tx.QueryRow(ctx, `
		SELECT `+deadlineColumns+` FROM deadlines
		WHERE tenant_id=$1 AND source_type=$2 AND source_ref=$3 AND status != 'SUPERSEDED'
		FOR UPDATE
	`, p.TenantID, p.SourceType, p.SourceRef))
	hasExisting := true
	if errors.Is(err, pgx.ErrNoRows) {
		hasExisting = false
	} else if err != nil {
		return nil, "", err
	}

	if hasExisting {
		if existing.Status != domain.DeadlineStatusScheduled {
			// A human already acted on the prior mirror locally
			// (completed/waived/cancelled) — silently moving its due
			// date out from under that action would rewrite what was
			// actually decided against. Refuse instead.
			return nil, "", domain.ErrDeadlineSourceConflict
		}
		if existing.SourceVersion == p.SourceVersion {
			// Same version already mirrored and untouched — nothing to
			// do; return the existing row rather than creating a
			// pointless duplicate.
			if err := tx.Commit(ctx); err != nil {
				return nil, "", err
			}
			return existing, "", nil
		}
	}

	deadlineID := "deadline-" + uuid.New().String()

	// The old row must be marked SUPERSEDED BEFORE the new row is
	// inserted: idx_deadlines_live_source is a partial unique index on
	// (tenant_id, source_type, source_ref) for any non-SUPERSEDED row, so
	// inserting the replacement first would momentarily violate it while
	// both rows are still "live".
	var supersededID string
	if hasExisting {
		if _, err := tx.Exec(ctx, `
			UPDATE deadlines SET status='SUPERSEDED', superseded_at=now(), superseded_by_deadline_id=$3, updated_at=now()
			WHERE deadline_id=$1 AND tenant_id=$2
		`, existing.DeadlineID, p.TenantID, deadlineID); err != nil {
			return nil, "", err
		}
		supersededID = existing.DeadlineID
	}

	row := tx.QueryRow(ctx, `INSERT INTO deadlines (
			deadline_id, tenant_id, legal_entity_id, title, linked_object_type, linked_object_id,
			due_at, owner_principal_id, source_type, source_ref, source_version, created_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING `+deadlineColumns,
		deadlineID, p.TenantID, p.LegalEntityID, p.Title, p.LinkedObjectType, p.LinkedObjectID,
		p.DueAt, p.OwnerPrincipalID, p.SourceType, p.SourceRef, p.SourceVersion, p.CreatedByPrincipalID)
	d, err := scanDeadline(row)
	if err != nil {
		return nil, "", fmt.Errorf("insert deadline: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return d, supersededID, nil
}

func (s *PgStore) GetDeadline(ctx context.Context, deadlineID string) (*domain.Deadline, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.deadlineSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	d, err := scanDeadline(tx.QueryRow(ctx, `SELECT `+deadlineColumns+` FROM deadlines WHERE deadline_id=$1 AND tenant_id=$2`,
		deadlineID, middleware.GetTenantID(ctx)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDeadlineNotFound
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// GetSourceDeadline — BIZ-08's own GetSourceDeadline query. See
// domain.DeadlineSourceInfo's own doc comment on why this is a
// passthrough rather than a live TAX/LEG lookup.
func (s *PgStore) GetSourceDeadline(ctx context.Context, tenantID, deadlineID string) (*domain.DeadlineSourceInfo, error) {
	d, err := s.GetDeadline(ctx, deadlineID)
	if err != nil {
		return nil, err
	}
	info := &domain.DeadlineSourceInfo{
		DeadlineID: d.DeadlineID, SourceType: d.SourceType, SourceRef: d.SourceRef, SourceVersion: d.SourceVersion,
		DueAt: d.DueAt, Mirrored: d.SourceType != "",
	}
	if info.Mirrored {
		info.Note = "BIZ-08 does not own the authoritative source; this is the source lock recorded at mirror time, not a live TAX/LEG lookup."
	} else {
		info.Note = "this deadline has no authoritative source — BIZ-08 owns it outright."
	}
	return info, nil
}

// AssignOwner — BIZ-08's own AssignOwner command. Valid from SCHEDULED
// only.
func (s *PgStore) AssignOwner(ctx context.Context, p domain.AssignOwnerParams) (*domain.Deadline, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.deadlineSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanDeadline(tx.QueryRow(ctx, `SELECT `+deadlineColumns+` FROM deadlines WHERE deadline_id=$1 AND tenant_id=$2 FOR UPDATE`, p.DeadlineID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDeadlineNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.DeadlineStatusScheduled {
		return nil, domain.ErrDeadlineInvalidState
	}
	d, err := scanDeadline(tx.QueryRow(ctx, `
		UPDATE deadlines SET owner_principal_id=$3, updated_at=now()
		WHERE deadline_id=$1 AND tenant_id=$2 RETURNING `+deadlineColumns,
		p.DeadlineID, p.TenantID, p.OwnerPrincipalID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// CompleteDeadline — BIZ-08's own Complete command. Valid from SCHEDULED
// only.
func (s *PgStore) CompleteDeadline(ctx context.Context, p domain.CompleteDeadlineParams) (*domain.Deadline, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.deadlineSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanDeadline(tx.QueryRow(ctx, `SELECT `+deadlineColumns+` FROM deadlines WHERE deadline_id=$1 AND tenant_id=$2 FOR UPDATE`, p.DeadlineID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDeadlineNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.DeadlineStatusScheduled {
		return nil, domain.ErrDeadlineInvalidState
	}
	d, err := scanDeadline(tx.QueryRow(ctx, `
		UPDATE deadlines SET status='COMPLETED', completed_at=now(), completed_by_principal_id=$3, updated_at=now()
		WHERE deadline_id=$1 AND tenant_id=$2 RETURNING `+deadlineColumns,
		p.DeadlineID, p.TenantID, p.ActorPrincipalID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Recalculate — BIZ-08's own Recalculate command. Refuses outright on a
// mirrored deadline (SourceType set) — the real enforcement of the doc's
// own prohibited anti-pattern; see ErrCannotRecalculateMirroredDeadline's
// own doc comment. Resets the notification-tracking columns since the
// due date materially changed.
func (s *PgStore) Recalculate(ctx context.Context, p domain.RecalculateParams) (*domain.Deadline, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.deadlineSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanDeadline(tx.QueryRow(ctx, `SELECT `+deadlineColumns+` FROM deadlines WHERE deadline_id=$1 AND tenant_id=$2 FOR UPDATE`, p.DeadlineID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDeadlineNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.SourceType != "" {
		return nil, domain.ErrCannotRecalculateMirroredDeadline
	}
	if current.Status != domain.DeadlineStatusScheduled {
		return nil, domain.ErrDeadlineInvalidState
	}
	d, err := scanDeadline(tx.QueryRow(ctx, `
		UPDATE deadlines SET due_at=$3, calc_rule=$4, calc_inputs=$5,
			due_soon_notified_at=NULL, overdue_notified_at=NULL, updated_at=now()
		WHERE deadline_id=$1 AND tenant_id=$2 RETURNING `+deadlineColumns,
		p.DeadlineID, p.TenantID, p.DueAt, p.CalcRule, p.CalcInputs))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Waive — BIZ-08's own Waive command. Valid from SCHEDULED only.
func (s *PgStore) Waive(ctx context.Context, p domain.WaiveParams) (*domain.Deadline, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.deadlineSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanDeadline(tx.QueryRow(ctx, `SELECT `+deadlineColumns+` FROM deadlines WHERE deadline_id=$1 AND tenant_id=$2 FOR UPDATE`, p.DeadlineID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDeadlineNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.DeadlineStatusScheduled {
		return nil, domain.ErrDeadlineInvalidState
	}
	d, err := scanDeadline(tx.QueryRow(ctx, `
		UPDATE deadlines SET status='WAIVED', waived_at=now(), waived_by_principal_id=$3, waiver_reason=$4, updated_at=now()
		WHERE deadline_id=$1 AND tenant_id=$2 RETURNING `+deadlineColumns,
		p.DeadlineID, p.TenantID, p.ActorPrincipalID, p.Reason))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// CancelDeadline — BIZ-08's own Cancel command. Valid from SCHEDULED
// only.
func (s *PgStore) CancelDeadline(ctx context.Context, p domain.CancelDeadlineParams) (*domain.Deadline, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.deadlineSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanDeadline(tx.QueryRow(ctx, `SELECT `+deadlineColumns+` FROM deadlines WHERE deadline_id=$1 AND tenant_id=$2 FOR UPDATE`, p.DeadlineID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDeadlineNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.DeadlineStatusScheduled {
		return nil, domain.ErrDeadlineInvalidState
	}
	d, err := scanDeadline(tx.QueryRow(ctx, `
		UPDATE deadlines SET status='CANCELLED', cancelled_at=now(), cancelled_by_principal_id=$3, cancel_reason=$4, updated_at=now()
		WHERE deadline_id=$1 AND tenant_id=$2 RETURNING `+deadlineColumns,
		p.DeadlineID, p.TenantID, p.ActorPrincipalID, p.Reason))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Escalate — BIZ-08's own Escalate command. Deliberately does not touch
// deadlines.status — see domain.EscalateParams's own doc comment. Valid
// against any deadline that still exists and has not reached a terminal
// state (escalating a COMPLETED/WAIVED/CANCELLED/SUPERSEDED deadline
// makes no sense — there is nothing left to escalate).
func (s *PgStore) Escalate(ctx context.Context, p domain.EscalateParams) (*domain.DeadlineEscalation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.deadlineSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanDeadline(tx.QueryRow(ctx, `SELECT `+deadlineColumns+` FROM deadlines WHERE deadline_id=$1 AND tenant_id=$2 FOR UPDATE`, p.DeadlineID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDeadlineNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.DeadlineStatusScheduled {
		return nil, domain.ErrDeadlineInvalidState
	}
	escalationID := "deadlineesc-" + uuid.New().String()
	row := tx.QueryRow(ctx, `INSERT INTO deadline_escalations (escalation_id, deadline_id, tenant_id, escalated_to_role, reason, escalated_by_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING escalation_id, deadline_id, tenant_id, escalated_to_role, reason, escalated_at, escalated_by_principal_id`,
		escalationID, p.DeadlineID, p.TenantID, p.EscalatedToRole, p.Reason, p.ActorPrincipalID)
	esc := &domain.DeadlineEscalation{}
	if err := row.Scan(&esc.EscalationID, &esc.DeadlineID, &esc.TenantID, &esc.EscalatedToRole, &esc.Reason, &esc.EscalatedAt, &esc.EscalatedByPrincipalID); err != nil {
		return nil, fmt.Errorf("insert deadline escalation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return esc, nil
}

// ListUpcoming — BIZ-08's own ListUpcoming query. SCHEDULED deadlines
// due within DeadlineDueSoonWindow. Any matching row not yet marked
// due_soon_notified_at is stamped in the same transaction and its id is
// returned in justNotifiedIDs — see migration 000005's own doc comment.
func (s *PgStore) ListUpcoming(ctx context.Context, p domain.ListUpcomingParams) ([]domain.Deadline, []string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.deadlineSetRLS(ctx, tx); err != nil {
		return nil, nil, err
	}
	query := `SELECT ` + deadlineColumns + ` FROM deadlines
		WHERE tenant_id=$1 AND status='SCHEDULED' AND due_at > now() AND due_at <= now() + $2::interval`
	args := []any{p.TenantID, fmt.Sprintf("%d seconds", int(domain.DeadlineDueSoonWindow.Seconds()))}
	if p.LegalEntityID != "" {
		args = append(args, p.LegalEntityID)
		query += fmt.Sprintf(" AND legal_entity_id=$%d", len(args))
	}
	if p.OwnerPrincipalID != "" {
		args = append(args, p.OwnerPrincipalID)
		query += fmt.Sprintf(" AND owner_principal_id=$%d", len(args))
	}
	query += " ORDER BY due_at ASC FOR UPDATE"

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	var out []domain.Deadline
	var justNotifiedIDs []string
	for rows.Next() {
		d, err := scanDeadline(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		out = append(out, *d)
		if d.DueSoonNotifiedAt == nil {
			justNotifiedIDs = append(justNotifiedIDs, d.DeadlineID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows.Close()

	for i := range out {
		if out[i].DueSoonNotifiedAt != nil {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE deadlines SET due_soon_notified_at=now() WHERE deadline_id=$1 AND tenant_id=$2`, out[i].DeadlineID, p.TenantID); err != nil {
			return nil, nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return out, justNotifiedIDs, nil
}

// ListOverdue — BIZ-08's own ListOverdue query. SCHEDULED deadlines past
// due_at. Same first-observation notification mechanism as ListUpcoming,
// using overdue_notified_at.
func (s *PgStore) ListOverdue(ctx context.Context, p domain.ListOverdueParams) ([]domain.Deadline, []string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.deadlineSetRLS(ctx, tx); err != nil {
		return nil, nil, err
	}
	query := `SELECT ` + deadlineColumns + ` FROM deadlines WHERE tenant_id=$1 AND status='SCHEDULED' AND due_at <= now()`
	args := []any{p.TenantID}
	if p.LegalEntityID != "" {
		args = append(args, p.LegalEntityID)
		query += fmt.Sprintf(" AND legal_entity_id=$%d", len(args))
	}
	if p.OwnerPrincipalID != "" {
		args = append(args, p.OwnerPrincipalID)
		query += fmt.Sprintf(" AND owner_principal_id=$%d", len(args))
	}
	query += " ORDER BY due_at ASC FOR UPDATE"

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	var out []domain.Deadline
	var justNotifiedIDs []string
	for rows.Next() {
		d, err := scanDeadline(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		out = append(out, *d)
		if d.OverdueNotifiedAt == nil {
			justNotifiedIDs = append(justNotifiedIDs, d.DeadlineID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows.Close()

	for i := range out {
		if out[i].OverdueNotifiedAt != nil {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE deadlines SET overdue_notified_at=now() WHERE deadline_id=$1 AND tenant_id=$2`, out[i].DeadlineID, p.TenantID); err != nil {
			return nil, nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return out, justNotifiedIDs, nil
}

// ExplainCalculation — BIZ-08's own ExplainCalculation query.
func (s *PgStore) ExplainCalculation(ctx context.Context, tenantID, deadlineID string) (*domain.ExplainCalculationResult, error) {
	d, err := s.GetDeadline(ctx, deadlineID)
	if err != nil {
		return nil, err
	}
	res := &domain.ExplainCalculationResult{DeadlineID: d.DeadlineID, DueAt: d.DueAt, Mirrored: d.SourceType != ""}
	if res.Mirrored {
		res.Note = "this deadline was never calculated locally — its due date was mirrored from an authoritative source; see GetSourceDeadline for the source lock."
	} else {
		res.CalcRule, res.CalcInputs = d.CalcRule, d.CalcInputs
		res.Note = "this deadline is calculated and owned by BIZ-08 directly."
	}
	return res, nil
}
