package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/middleware"
)

// FindingStore is AUD-08's own persistence contract — kept separate from
// the existing Store interface above so this file's scope is
// self-contained, same pattern as AUD-05/06's composed store interfaces
// in the other services this build touched.
type FindingStore interface {
	CreateFinding(ctx context.Context, p domain.CreateFindingParams) (*domain.AuditFinding, error)
	GetFinding(ctx context.Context, findingID string) (*domain.AuditFinding, error)
	AccumulateMisstatement(ctx context.Context, p domain.AccumulateMisstatementParams) (*domain.MisstatementRecord, error)
	GetMisstatementSummary(ctx context.Context, findingID string) ([]domain.MisstatementRecord, error)
	RecordMaterialityEvaluation(ctx context.Context, p domain.RecordMaterialityEvaluationParams) (*domain.MaterialityEvaluation, error)
	CommunicateFinding(ctx context.Context, p domain.CommunicateFindingParams) (*domain.AuditFinding, error)
	RecordManagementResponse(ctx context.Context, p domain.RecordManagementResponseParams) (*domain.ManagementResponse, error)
	LinkRemediation(ctx context.Context, p domain.LinkRemediationParams) (*domain.RemediationEvidence, error)
	RecordScopeLimitation(ctx context.Context, p domain.RecordScopeLimitationParams) (*domain.ScopeLimitation, error)
	ResolveScopeLimitation(ctx context.Context, p domain.ResolveScopeLimitationParams) (*domain.ScopeLimitation, error)
	ListOpenScopeLimitations(ctx context.Context, tenantID, findingID string) ([]domain.ScopeLimitation, error)
	CloseFinding(ctx context.Context, p domain.CloseFindingParams) (*domain.AuditFinding, error)
	ReopenFinding(ctx context.Context, p domain.ReopenFindingParams) (*domain.AuditFinding, error)
}

// findingSetRLS mirrors setRLS above but uses SELECT set_config with a
// bind parameter rather than SET LOCAL app.tenant_id = $1 — Postgres does
// not accept a bind parameter inside SET, only inside SELECT/set_config,
// so this is the working form (the same defect class documented in
// evidence-requirements-svc's own store package doc, avoided here rather
// than inherited into new code).
func (s *PgStore) findingSetRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	return err
}

const findingColumns = `finding_id, exception_case_id, tenant_id, legal_entity_id, engagement_id, finding_type,
	requires_remediation_evidence, status, reopened_count, created_by_principal_id, created_at, closed_at`

func scanFinding(row pgx.Row) (*domain.AuditFinding, error) {
	f := &domain.AuditFinding{}
	err := row.Scan(&f.FindingID, &f.ExceptionCaseID, &f.TenantID, &f.LegalEntityID, &f.EngagementID, &f.FindingType,
		&f.RequiresRemediationEvidence, &f.Status, &f.ReopenedCount, &f.CreatedByPrincipalID, &f.CreatedAt, &f.ClosedAt)
	return f, err
}

func (s *PgStore) CreateFinding(ctx context.Context, p domain.CreateFindingParams) (*domain.AuditFinding, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	findingID := "finding-" + uuid.New().String()
	row := tx.QueryRow(ctx, `INSERT INTO audit_findings (finding_id, exception_case_id, tenant_id, legal_entity_id, engagement_id, finding_type, requires_remediation_evidence, created_by_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+findingColumns,
		findingID, p.ExceptionCaseID, p.TenantID, p.LegalEntityID, p.EngagementID, string(p.FindingType), p.RequiresRemediationEvidence, p.CreatedByPrincipalID)
	f, err := scanFinding(row)
	if err != nil {
		return nil, fmt.Errorf("insert audit finding: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *PgStore) GetFinding(ctx context.Context, findingID string) (*domain.AuditFinding, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	f, err := scanFinding(tx.QueryRow(ctx, `SELECT `+findingColumns+` FROM audit_findings WHERE finding_id=$1 AND tenant_id=$2`, findingID, middleware.GetTenantID(ctx)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFindingNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *PgStore) AccumulateMisstatement(ctx context.Context, p domain.AccumulateMisstatementParams) (*domain.MisstatementRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	id := "misstatement-" + uuid.New().String()
	row := tx.QueryRow(ctx, `INSERT INTO misstatement_records (misstatement_id, finding_id, tenant_id, amount, corrects_misstatement_id, recorded_by_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING misstatement_id, finding_id, amount, corrects_misstatement_id, is_corrected, recorded_by_principal_id, recorded_at`,
		id, p.FindingID, p.TenantID, p.Amount, p.CorrectsMisstatementID, p.RecordedByPrincipalID)
	m := &domain.MisstatementRecord{}
	if err := row.Scan(&m.MisstatementID, &m.FindingID, &m.Amount, &m.CorrectsMisstatementID, &m.IsCorrected, &m.RecordedByPrincipalID, &m.RecordedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *PgStore) GetMisstatementSummary(ctx context.Context, findingID string) ([]domain.MisstatementRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	// is_corrected is derived here, not read from the stored column: the
	// table is unconditionally append-only (AUD-NEG-027), so there is no
	// UPDATE path that could ever set it after INSERT. A row counts as
	// corrected if some OTHER row's corrects_misstatement_id points at it
	// — AUD-NEG-028's own accumulation-completeness query relies on this
	// same derivation to know which misstatements remain uncorrected.
	rows, err := tx.Query(ctx, `SELECT mr.misstatement_id, mr.finding_id, mr.amount, mr.corrects_misstatement_id,
			EXISTS(SELECT 1 FROM misstatement_records c WHERE c.corrects_misstatement_id = mr.misstatement_id) AS is_corrected,
			mr.recorded_by_principal_id, mr.recorded_at
		FROM misstatement_records mr WHERE mr.finding_id=$1 ORDER BY mr.recorded_at`, findingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MisstatementRecord
	for rows.Next() {
		var m domain.MisstatementRecord
		if err := rows.Scan(&m.MisstatementID, &m.FindingID, &m.Amount, &m.CorrectsMisstatementID, &m.IsCorrected, &m.RecordedByPrincipalID, &m.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PgStore) RecordMaterialityEvaluation(ctx context.Context, p domain.RecordMaterialityEvaluationParams) (*domain.MaterialityEvaluation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	var nextVersion int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM materiality_evaluations WHERE finding_id=$1`, p.FindingID).Scan(&nextVersion); err != nil {
		return nil, err
	}
	id := "matassess-" + uuid.New().String()
	row := tx.QueryRow(ctx, `INSERT INTO materiality_evaluations (evaluation_id, finding_id, tenant_id, version, is_material, qualitative_notes, evaluated_by_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING evaluation_id, finding_id, version, is_material, qualitative_notes, evaluated_by_principal_id, evaluated_at`,
		id, p.FindingID, p.TenantID, nextVersion, p.IsMaterial, p.QualitativeNotes, p.EvaluatedByPrincipalID)
	e := &domain.MaterialityEvaluation{}
	if err := row.Scan(&e.EvaluationID, &e.FindingID, &e.Version, &e.IsMaterial, &e.QualitativeNotes, &e.EvaluatedByPrincipalID, &e.EvaluatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *PgStore) CommunicateFinding(ctx context.Context, p domain.CommunicateFindingParams) (*domain.AuditFinding, error) {
	return s.transitionFinding(ctx, p.FindingID, p.TenantID, []domain.FindingStatus{domain.FindingIdentified, domain.FindingEvaluating}, domain.FindingCommunicated)
}

func (s *PgStore) transitionFinding(ctx context.Context, findingID, tenantID string, from []domain.FindingStatus, to domain.FindingStatus) (*domain.AuditFinding, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	fromStrs := make([]string, len(from))
	for i, f := range from {
		fromStrs[i] = string(f)
	}
	row := tx.QueryRow(ctx, `UPDATE audit_findings SET status=$1 WHERE finding_id=$2 AND tenant_id=$3 AND status = ANY($4) RETURNING `+findingColumns,
		string(to), findingID, tenantID, fromStrs)
	f, err := scanFinding(row)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_findings WHERE finding_id=$1 AND tenant_id=$2)`, findingID, tenantID).Scan(&exists); chkErr != nil {
			return nil, chkErr
		}
		if !exists {
			return nil, domain.ErrFindingNotFound
		}
		return nil, domain.ErrFindingInvalidState
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *PgStore) RecordManagementResponse(ctx context.Context, p domain.RecordManagementResponseParams) (*domain.ManagementResponse, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	id := "mgmtresp-" + uuid.New().String()
	row := tx.QueryRow(ctx, `INSERT INTO management_responses (response_id, finding_id, tenant_id, response_text, remediation_plan, responded_by_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING response_id, finding_id, response_text, remediation_plan, responded_by_principal_id, responded_at`,
		id, p.FindingID, p.TenantID, p.ResponseText, p.RemediationPlan, p.RespondedByPrincipalID)
	m := &domain.ManagementResponse{}
	if err := row.Scan(&m.ResponseID, &m.FindingID, &m.ResponseText, &m.RemediationPlan, &m.RespondedByPrincipalID, &m.RespondedAt); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_findings SET status=$1 WHERE finding_id=$2 AND status=$3`, domain.FindingManagementResponse, p.FindingID, domain.FindingCommunicated); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *PgStore) LinkRemediation(ctx context.Context, p domain.LinkRemediationParams) (*domain.RemediationEvidence, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	id := "remediation-" + uuid.New().String()
	row := tx.QueryRow(ctx, `INSERT INTO remediation_evidence (remediation_id, finding_id, tenant_id, evidence_ref, reperformed, recorded_by_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING remediation_id, finding_id, evidence_ref, reperformed, recorded_by_principal_id, recorded_at`,
		id, p.FindingID, p.TenantID, p.EvidenceRef, p.Reperformed, p.RecordedByPrincipalID)
	r := &domain.RemediationEvidence{}
	if err := row.Scan(&r.RemediationID, &r.FindingID, &r.EvidenceRef, &r.Reperformed, &r.RecordedByPrincipalID, &r.RecordedAt); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_findings SET status=$1 WHERE finding_id=$2 AND status=$3`, domain.FindingRemediationTesting, p.FindingID, domain.FindingManagementResponse); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *PgStore) RecordScopeLimitation(ctx context.Context, p domain.RecordScopeLimitationParams) (*domain.ScopeLimitation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	id := "scopelimit-" + uuid.New().String()
	row := tx.QueryRow(ctx, `INSERT INTO scope_limitations (limitation_id, finding_id, tenant_id, description, recorded_by_principal_id)
		VALUES ($1,$2,$3,$4,$5) RETURNING limitation_id, finding_id, description, still_impacts_report, recorded_by_principal_id, recorded_at, resolved_at`,
		id, p.FindingID, p.TenantID, p.Description, p.RecordedByPrincipalID)
	l := &domain.ScopeLimitation{}
	if err := row.Scan(&l.LimitationID, &l.FindingID, &l.Description, &l.StillImpactsReport, &l.RecordedByPrincipalID, &l.RecordedAt, &l.ResolvedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return l, nil
}

// ResolveScopeLimitation is the ONLY column this table's own trigger
// permits changing — see migration 000002's reject_scope_limitation_mutation.
func (s *PgStore) ResolveScopeLimitation(ctx context.Context, p domain.ResolveScopeLimitationParams) (*domain.ScopeLimitation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	row := tx.QueryRow(ctx, `UPDATE scope_limitations SET resolved_at=$1 WHERE limitation_id=$2 AND tenant_id=$3 AND resolved_at IS NULL
		RETURNING limitation_id, finding_id, description, still_impacts_report, recorded_by_principal_id, recorded_at, resolved_at`,
		now, p.LimitationID, p.TenantID)
	l := &domain.ScopeLimitation{}
	err = row.Scan(&l.LimitationID, &l.FindingID, &l.Description, &l.StillImpactsReport, &l.RecordedByPrincipalID, &l.RecordedAt, &l.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrScopeLimitationNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return l, nil
}

// ListOpenScopeLimitations is the real, independent completion-gate query
// — see the domain package doc's own AUD-NEG-029 explanation.
func (s *PgStore) ListOpenScopeLimitations(ctx context.Context, tenantID, findingID string) ([]domain.ScopeLimitation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT limitation_id, finding_id, description, still_impacts_report, recorded_by_principal_id, recorded_at, resolved_at
		FROM scope_limitations WHERE finding_id=$1 AND still_impacts_report=true AND resolved_at IS NULL`, findingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ScopeLimitation
	for rows.Next() {
		var l domain.ScopeLimitation
		if err := rows.Scan(&l.LimitationID, &l.FindingID, &l.Description, &l.StillImpactsReport, &l.RecordedByPrincipalID, &l.RecordedAt, &l.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// CloseFinding is the real, DB-enforced proof of AUD-NEG-026 "finding is
// closed with no closure evidence where required -> block closure": the
// CAS predicate below only succeeds when either evidence is not required
// or at least one remediation_evidence row exists.
func (s *PgStore) CloseFinding(ctx context.Context, p domain.CloseFindingParams) (*domain.AuditFinding, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `UPDATE audit_findings SET status=$1, closed_at=now() WHERE finding_id=$2 AND tenant_id=$3 AND status <> $1
		AND (requires_remediation_evidence = false OR EXISTS(SELECT 1 FROM remediation_evidence WHERE finding_id=$2))
		RETURNING `+findingColumns, domain.FindingClosed, p.FindingID, p.TenantID)
	f, err := scanFinding(row)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists, hasEvidence bool
		if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_findings WHERE finding_id=$1 AND tenant_id=$2)`, p.FindingID, p.TenantID).Scan(&exists); chkErr != nil {
			return nil, chkErr
		}
		if !exists {
			return nil, domain.ErrFindingNotFound
		}
		if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM remediation_evidence WHERE finding_id=$1)`, p.FindingID).Scan(&hasEvidence); chkErr != nil {
			return nil, chkErr
		}
		if !hasEvidence {
			return nil, domain.ErrClosureEvidenceRequired
		}
		return nil, domain.ErrFindingInvalidState
	}
	if err != nil {
		return nil, err
	}
	id := "closure-" + uuid.New().String()
	if _, err := tx.Exec(ctx, `INSERT INTO finding_closure_assessments (assessment_id, finding_id, tenant_id, closed_by_principal_id, closure_notes) VALUES ($1,$2,$3,$4,$5)`,
		id, p.FindingID, p.TenantID, p.ClosedByPrincipalID, p.ClosureNotes); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

// ReopenFinding refuses if the actor is the same principal who closed it
// — reuses ErrSelfApprovalNotAllowed, the same posture as every other
// maker/checker pair in this build.
func (s *PgStore) ReopenFinding(ctx context.Context, p domain.ReopenFindingParams) (*domain.AuditFinding, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.findingSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	var closedBy string
	if err := tx.QueryRow(ctx, `SELECT closed_by_principal_id FROM finding_closure_assessments WHERE finding_id=$1 ORDER BY closed_at DESC LIMIT 1`, p.FindingID).Scan(&closedBy); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if closedBy != "" && closedBy == p.ActorPrincipalID {
		return nil, domain.ErrSelfApprovalNotAllowed
	}
	row := tx.QueryRow(ctx, `UPDATE audit_findings SET status=$1, reopened_count=reopened_count+1, closed_at=NULL WHERE finding_id=$2 AND tenant_id=$3 AND status=$4 RETURNING `+findingColumns,
		domain.FindingReopened, p.FindingID, p.TenantID, domain.FindingClosed)
	f, err := scanFinding(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFindingInvalidState
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return f, nil
}
