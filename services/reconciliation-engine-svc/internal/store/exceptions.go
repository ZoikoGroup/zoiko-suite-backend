package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
	"zoiko.io/reconciliation-engine-svc/internal/outbox"
)

// Exception governance (A7) and materiality (A4). See domain/governance.go
// for the rules; this file persists them.

const exceptionColumns = `exception_id, tenant_id, run_id, kind, COALESCE(item_id, ''), COALESCE(side, ''), COALESCE(ref_id, ''),
	COALESCE(currency, ''), dimension, dimension_value, reason, amount_minor_units, detail, status,
	raised_at, raised_by, resolved_at, resolved_by_match_id, category, assertion, severity, materiality, exposure_minor_units,
	owner_role, owner_principal, due_at, authority_ref, evidence_ref, root_cause_code, root_cause_note, accepted_by, accepted_at`

func scanException(row pgx.Row) (*domain.ReconciliationException, error) {
	var e domain.ReconciliationException
	var detail []byte
	if err := row.Scan(&e.ExceptionID, &e.TenantID, &e.RunID, &e.Kind, &e.ItemID, &e.Side, &e.RefID, &e.Currency, &e.Dimension,
		&e.DimensionValue, &e.Reason, &e.AmountMinorUnits, &detail, &e.Status, &e.RaisedAt, &e.RaisedBy, &e.ResolvedAt,
		&e.ResolvedByMatchID, &e.Category, &e.Assertion, &e.Severity, &e.Materiality, &e.ExposureMinor,
		&e.OwnerRole, &e.Owner, &e.DueAt, &e.AuthorityRef, &e.EvidenceRef, &e.RootCauseCode, &e.RootCauseNote,
		&e.AcceptedBy, &e.AcceptedAt); err != nil {
		return nil, err
	}
	if len(detail) > 0 {
		e.Detail = json.RawMessage(detail)
	}
	return &e, nil
}

// governance is what every new exception is raised with: its classification,
// the severity and materiality its exposure implies under the run's pinned
// materiality, its owner role and its SLA. A run pinned before migration 000007
// has no policy to derive them from, and raises nothing (ErrLegacyRecord).
type governance struct {
	category, assertion string
	severity            domain.Severity
	materiality         domain.MaterialityClass
	exposure            int64
	ownerRole           string
	slaDays             int
}

func governanceFor(run *domain.ReconciliationRun, currency string, exposure int64, category, assertion string) (governance, error) {
	if run.Materiality == nil || run.ExceptionPolicy == nil || run.CertificationProfile == nil {
		return governance{}, domain.ErrLegacyRecord
	}
	sev, mat := run.Materiality.DeriveSeverity(currency, exposure)
	return governance{category: category, assertion: assertion, severity: sev, materiality: mat, exposure: exposure,
		ownerRole: run.ExceptionPolicy.OwnerRole, slaDays: run.ExceptionPolicy.SLADays[sev]}, nil
}

// governanceValues are the trailing INSERT values for governance g, in the
// order of governanceInsertColumns; due_at is computed by the database from
// its own clock, the same clock that stamps raised_at.
const governanceInsertColumns = `category, assertion, severity, materiality, exposure_minor_units, owner_role, due_at`

func governancePlaceholders(first int) string {
	return fmt.Sprintf("$%d, $%d, $%d, $%d, $%d, $%d, NOW() + make_interval(days => $%d)",
		first, first+1, first+2, first+3, first+4, first+5, first+6)
}

func (g governance) values() []any {
	return []any{g.category, g.assertion, string(g.severity), string(g.materiality), g.exposure, g.ownerRole, g.slaDays}
}

// ── Assessment ──────────────────────────────────────────────────────────────

// loadExposures reads every exception of a run as an aggregate input. A
// legacy exception (raised before 000007) has no exposure; its absolute amount
// stands in, which is what it put at risk.
func loadExposures(ctx context.Context, tx pgx.Tx, runID string) ([]domain.Exposure, error) {
	rows, err := tx.Query(ctx, `
		SELECT exception_id, kind, status, COALESCE(currency, ''), COALESCE(item_id, ''),
			COALESCE(detail ->> 'counterpart_item_id', ''), exposure_minor_units, amount_minor_units
		FROM reconciliation_exceptions WHERE run_id = $1 ORDER BY exception_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("load exceptions for assessment: %w", err)
	}
	defer rows.Close()
	var out []domain.Exposure
	for rows.Next() {
		var e domain.Exposure
		var exposure *int64
		var amount int64
		if err := rows.Scan(&e.ExceptionID, &e.Kind, &e.Status, &e.Currency, &e.ItemID, &e.Counterpart, &exposure, &amount); err != nil {
			return nil, err
		}
		if exposure != nil {
			e.Minor = *exposure
		} else {
			e.Minor = domain.ExposureOf(amount)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// assessRun is the run's assessment under its pinned policies. A run pinned
// before 000007 has no materiality (everything UNASSESSED) and no profile (no
// accepted risk is permitted).
func assessRun(ctx context.Context, tx pgx.Tx, run *domain.ReconciliationRun) (domain.RunAssessment, error) {
	exps, err := loadExposures(ctx, tx, run.RunID)
	if err != nil {
		return domain.RunAssessment{}, err
	}
	var mat domain.MaterialityPolicy
	if run.Materiality != nil {
		mat = *run.Materiality
	}
	var profile domain.CertificationProfile
	if run.CertificationProfile != nil {
		profile = *run.CertificationProfile
	}
	return domain.Assess(exps, mat, profile), nil
}

// ── Exception lifecycle ─────────────────────────────────────────────────────

func insertExceptionTransition(ctx context.Context, tx pgx.Tx, tenantID, exceptionID string, from, to domain.ExceptionStatus,
	owner *string, reason string, req *domain.TransitionExceptionRequest, actor string) error {
	var authority, evidence, rcCode, rcNote *string
	if req != nil {
		authority, evidence, rcCode, rcNote = nonEmpty(req.AuthorityRef), nonEmpty(req.EvidenceRef), nonEmpty(req.RootCauseCode),
			nonEmpty(req.RootCauseNote)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO reconciliation_exception_transitions (tenant_id, exception_id, from_state, to_state, owner_principal, reason,
			authority_ref, evidence_ref, root_cause_code, root_cause_note, actor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		tenantID, exceptionID, string(from), string(to), owner, reason, authority, evidence, rcCode, rcNote, actor); err != nil {
		return fmt.Errorf("record exception transition: %w", err)
	}
	return nil
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// loadExceptionRun returns the exception's run id, so the run can be locked
// before the exception (the same order as Match: run, then exceptions).
func loadExceptionRun(ctx context.Context, tx pgx.Tx, tenantID, exceptionID string) (string, error) {
	var runID string
	err := tx.QueryRow(ctx, `SELECT run_id FROM reconciliation_exceptions WHERE tenant_id = $1 AND exception_id = $2`,
		tenantID, exceptionID).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrExceptionNotFound
	}
	return runID, err
}

// lockException locks the exception's run, then the exception. The run must be
// open and evaluated: an exception of a certified, failed or superseded run is
// evidence, not work.
func lockException(ctx context.Context, tx pgx.Tx, tenantID, exceptionID string) (*domain.ReconciliationRun, *domain.ReconciliationException, error) {
	runID, err := loadExceptionRun(ctx, tx, tenantID, exceptionID)
	if err != nil {
		return nil, nil, err
	}
	run, err := loadRunForUpdate(ctx, tx, tenantID, runID)
	if err != nil {
		return nil, nil, err
	}
	if !openRunStatuses()[run.Status] {
		return nil, nil, domain.ErrRunNotOpen
	}
	e, err := scanException(tx.QueryRow(ctx, `SELECT `+exceptionColumns+` FROM reconciliation_exceptions
		WHERE tenant_id = $1 AND exception_id = $2 FOR UPDATE`, tenantID, exceptionID))
	if err != nil {
		return nil, nil, err
	}
	return run, e, nil
}

// AssignException names the principal who owns an exception still being
// worked (§21 "Owner / reviewer"). Reassigning is recorded like any move.
func (s *PgStore) AssignException(ctx context.Context, tenantID, exceptionID string, req domain.AssignExceptionRequest, actor string,
	claim domain.IdempotencyClaim) (*domain.ReconciliationException, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ReconciliationException
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, e, err := lockException(ctx, tx, tenantID, exceptionID)
		if err != nil {
			return err
		}
		if !e.Status.Working() {
			return fmt.Errorf("%w: a %s exception is final and cannot be reassigned", domain.ErrInvalidTransition, e.Status)
		}
		claim.ResourceID = exceptionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := scanException(tx.QueryRow(ctx, `UPDATE reconciliation_exceptions SET owner_principal = $2
			WHERE exception_id = $1 RETURNING `+exceptionColumns, exceptionID, req.OwnerPrincipal))
		if err != nil {
			return fmt.Errorf("assign exception: %w", err)
		}
		if err := insertExceptionTransition(ctx, tx, tenantID, exceptionID, e.Status, e.Status, &req.OwnerPrincipal,
			"assigned: "+req.Reason, nil, actor); err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_exception", AggregateID: exceptionID,
			EventType: "CONTROL.ExceptionUpdated", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

// TransitionException moves an exception to INVESTIGATING, REMEDIATED or
// ACCEPTED_RISK, then re-settles its run's result in the same transaction.
//
//   - INVESTIGATING and REMEDIATED are the assigned owner's work; REMEDIATED
//     names the correction made in the owning domain (evidence_ref) and leaves
//     the exception unresolved: this run's populations are frozen, so only a
//     new run can re-test it (DQC-05 "Re-test").
//   - ACCEPTED_RISK needs a named authority (authority_ref), is granted by
//     someone other than the owner and the raiser (the handler also requires
//     the RECONCILIATION_ACCEPT_RISK permission), is final, and leaves the
//     exception unresolved: the run's result stays VARIANCE (INV-19).
//   - A BLOCKER needs a root cause to be remediated or accepted.
func (s *PgStore) TransitionException(ctx context.Context, tenantID, exceptionID string, req domain.TransitionExceptionRequest,
	actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationException, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ReconciliationException
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, e, err := lockException(ctx, tx, tenantID, exceptionID)
		if err != nil {
			return err
		}
		if err := domain.ValidateExceptionTransition(e.Status, req.ToState); err != nil {
			return err
		}
		if err := domain.CheckIndependence(req.ToState, actor, e.Owner, e.RaisedBy); err != nil {
			return err
		}
		sev := domain.SeverityBlocker // an exception raised before 000007 has no severity: treat it as significant
		if e.Severity != nil {
			sev = *e.Severity
		}
		if err := req.CheckRootCause(sev); err != nil {
			return err
		}
		claim.ResourceID = exceptionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		var acceptedBy *string
		if req.ToState == domain.ExceptionAcceptedRisk {
			acceptedBy = &actor
		}
		got, err := scanException(tx.QueryRow(ctx, `
			UPDATE reconciliation_exceptions SET status = $2,
				authority_ref = COALESCE($3, authority_ref), evidence_ref = COALESCE($4, evidence_ref),
				root_cause_code = COALESCE($5, root_cause_code), root_cause_note = COALESCE($6, root_cause_note),
				accepted_by = COALESCE($7, accepted_by)
			WHERE exception_id = $1 RETURNING `+exceptionColumns,
			exceptionID, string(req.ToState), nonEmpty(req.AuthorityRef), nonEmpty(req.EvidenceRef), nonEmpty(req.RootCauseCode),
			nonEmpty(req.RootCauseNote), acceptedBy))
		if err != nil {
			return fmt.Errorf("transition exception: %w", err)
		}
		if err := insertExceptionTransition(ctx, tx, tenantID, exceptionID, e.Status, req.ToState, e.Owner, req.Reason, &req, actor); err != nil {
			return err
		}
		if _, err := settleResult(ctx, tx, tenantID, run, domain.RunRunning,
			fmt.Sprintf("exception %s %s", exceptionID, req.ToState), actor); err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_exception", AggregateID: exceptionID,
			EventType: "CONTROL.ExceptionUpdated", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

// closeExceptionsForMatch closes the working exceptions of the two items a
// manual match reconciles, recording each closure. An item whose exception
// was accepted as risk cannot be manually matched: acceptance is final, and
// matching the item afterwards would leave an accepted risk standing against
// an item that no longer differs.
func closeExceptionsForMatch(ctx context.Context, tx pgx.Tx, tenantID, matchID, actor, reason string, itemIDs ...string) error {
	rows, err := tx.Query(ctx, `SELECT exception_id, status, owner_principal FROM reconciliation_exceptions
		WHERE item_id = ANY($1) AND status <> 'CLOSED' ORDER BY exception_id FOR UPDATE`, itemIDs)
	if err != nil {
		return fmt.Errorf("load exceptions for manual match: %w", err)
	}
	type ex struct {
		id     string
		status domain.ExceptionStatus
		owner  *string
	}
	var found []ex
	for rows.Next() {
		var e ex
		if err := rows.Scan(&e.id, &e.status, &e.owner); err != nil {
			rows.Close()
			return err
		}
		found = append(found, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range found {
		if e.status == domain.ExceptionAcceptedRisk {
			return fmt.Errorf("%w: exception %s was accepted as risk; its item can no longer be matched", domain.ErrInvalidTransition, e.id)
		}
	}
	for _, e := range found {
		if _, err := tx.Exec(ctx, `UPDATE reconciliation_exceptions SET status = 'CLOSED', resolved_by_match_id = $2
			WHERE exception_id = $1`, e.id, matchID); err != nil {
			return fmt.Errorf("close exception for manual match: %w", err)
		}
		if err := insertExceptionTransition(ctx, tx, tenantID, e.id, e.status, domain.ExceptionClosed, e.owner,
			"closed by manual match "+matchID+": "+reason, nil, actor); err != nil {
			return err
		}
	}
	return nil
}

// ── Reads ───────────────────────────────────────────────────────────────────

func (s *PgStore) GetException(ctx context.Context, tenantID, exceptionID string) (*domain.ReconciliationException, error) {
	var out *domain.ReconciliationException
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		e, err := scanException(tx.QueryRow(ctx, `SELECT `+exceptionColumns+` FROM reconciliation_exceptions
			WHERE tenant_id = $1 AND exception_id = $2`, tenantID, exceptionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrExceptionNotFound
		}
		if err != nil {
			return err
		}
		e.Derive(time.Now())
		out = e
		return nil
	})
	return out, err
}

func (s *PgStore) GetExceptionTransitions(ctx context.Context, tenantID, exceptionID string) ([]domain.ExceptionTransition, error) {
	var out []domain.ExceptionTransition
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT transition_id, exception_id, from_state, to_state, owner_principal, reason, authority_ref, evidence_ref,
				root_cause_code, root_cause_note, actor, occurred_at
			FROM reconciliation_exception_transitions WHERE tenant_id = $1 AND exception_id = $2 ORDER BY transition_id`,
			tenantID, exceptionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.ExceptionTransition
			if err := rows.Scan(&t.TransitionID, &t.ExceptionID, &t.FromState, &t.ToState, &t.OwnerPrincipal, &t.Reason,
				&t.AuthorityRef, &t.EvidenceRef, &t.RootCauseCode, &t.RootCauseNote, &t.Actor, &t.OccurredAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// GetAssessment is the run's assessment, derived now from its exceptions.
func (s *PgStore) GetAssessment(ctx context.Context, tenantID, runID string) (*domain.RunAssessment, error) {
	var out *domain.RunAssessment
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM reconciliation_runs WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRunNotFound
		}
		if err != nil {
			return err
		}
		a, err := assessRun(ctx, tx, r)
		out = &a
		return err
	})
	return out, err
}
