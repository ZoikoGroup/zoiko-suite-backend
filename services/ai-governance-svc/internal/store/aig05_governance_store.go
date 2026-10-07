package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/ai-governance-svc/internal/domain"
)

// AIG-05 Evaluation, Monitoring, Incident & Change Governance Service
// (ZS-SVC-X-001 §8). Platform-wide, no tenant_id — same convention as
// AIG-02's ai_model_releases, which every row here references or sits
// alongside. No withTenant wrapping: these methods use s.pool
// directly, exactly like RegisterModelRelease/ApproveRelease etc.

const evaluationColumns = `evaluation_id, model_release_id, use_case_id, dimension, dataset_version, metrics,
	thresholds, result, defects, evaluated_by_principal_id, evaluated_at`

func scanEvaluation(row pgx.Row) (*domain.AIEvaluation, error) {
	var e domain.AIEvaluation
	var dimension, result string
	var useCaseID *string
	var metrics, thresholds, defects []byte
	if err := row.Scan(&e.EvaluationID, &e.ModelReleaseID, &useCaseID, &dimension, &e.DatasetVersion, &metrics,
		&thresholds, &result, &defects, &e.EvaluatedByPrincipalID, &e.EvaluatedAt); err != nil {
		return nil, err
	}
	e.Dimension = domain.EvaluationDimension(dimension)
	e.Result = domain.EvaluationResult(result)
	if useCaseID != nil {
		e.UseCaseID = *useCaseID
	}
	e.Metrics = unmarshalJSONMap(metrics)
	e.Thresholds = unmarshalJSONMap(thresholds)
	e.Defects = unmarshalStrings(defects)
	return &e, nil
}

// CreateEvaluation records one dimension's evaluation run against a
// model release — immutable release evidence (§8). use_case_id, when
// given, is accepted as a caller-attested reference (not validated
// against ai_use_cases) since that table is tenant-scoped under RLS
// and this service's evaluation/incident surface is deliberately
// platform-wide, same reasoning AIG-02 already applies to its own
// cross-references.
func (s *PgStore) CreateEvaluation(ctx context.Context, req domain.CreateEvaluationRequest, actor string) (*domain.AIEvaluation, error) {
	if !domain.EvaluationDimension(req.Dimension).Valid() {
		return nil, domain.ErrInvalidEvaluationDimension
	}
	if domain.EvaluationResult(req.Result) != domain.EvaluationPass && domain.EvaluationResult(req.Result) != domain.EvaluationFail {
		return nil, domain.ErrInvalidEvaluationResult
	}
	if req.ModelReleaseID == "" || req.DatasetVersion == "" {
		return nil, fmt.Errorf("model_release_id and dataset_version are required")
	}

	metrics, err := marshalJSON(req.Metrics)
	if err != nil {
		return nil, fmt.Errorf("marshal metrics: %w", err)
	}
	thresholds, err := marshalJSON(req.Thresholds)
	if err != nil {
		return nil, fmt.Errorf("marshal thresholds: %w", err)
	}
	defects, err := marshalStrings(req.Defects)
	if err != nil {
		return nil, fmt.Errorf("marshal defects: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := loadModelReleaseForUpdate(ctx, tx, req.ModelReleaseID); err != nil {
		return nil, err
	}

	var useCaseIDPtr *string
	if req.UseCaseID != "" {
		useCaseIDPtr = &req.UseCaseID
	}

	e, err := scanEvaluation(tx.QueryRow(ctx, `
		INSERT INTO ai_evaluations (model_release_id, use_case_id, dimension, dataset_version, metrics,
			thresholds, result, defects, evaluated_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+evaluationColumns,
		req.ModelReleaseID, useCaseIDPtr, req.Dimension, req.DatasetVersion, metrics, thresholds, req.Result, defects, actor))
	if err != nil {
		return nil, fmt.Errorf("insert ai evaluation: %w", err)
	}
	return e, tx.Commit(ctx)
}

func (s *PgStore) GetEvaluation(ctx context.Context, evaluationID string) (*domain.AIEvaluation, error) {
	e, err := scanEvaluation(s.pool.QueryRow(ctx, `SELECT `+evaluationColumns+` FROM ai_evaluations WHERE evaluation_id = $1`, evaluationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEvaluationNotFound
	}
	return e, err
}

const incidentColumns = `incident_id, severity, model_release_id, use_case_id, exception_case_ref, incident_state,
	description, containment_action, root_cause, corrective_actions, closure_evidence, reported_by_principal_id,
	reported_at, contained_at, resolved_at, closed_at, closed_by_principal_id`

func scanIncident(row pgx.Row) (*domain.AIIncident, error) {
	var in domain.AIIncident
	var severity, state string
	var modelReleaseID, useCaseID, exceptionCaseRef, containmentAction, rootCause, correctiveActions, closureEvidence, closedBy *string
	if err := row.Scan(&in.IncidentID, &severity, &modelReleaseID, &useCaseID, &exceptionCaseRef, &state,
		&in.Description, &containmentAction, &rootCause, &correctiveActions, &closureEvidence, &in.ReportedByPrincipalID,
		&in.ReportedAt, &in.ContainedAt, &in.ResolvedAt, &in.ClosedAt, &closedBy); err != nil {
		return nil, err
	}
	in.Severity = domain.IncidentSeverity(severity)
	in.IncidentState = domain.IncidentState(state)
	if modelReleaseID != nil {
		in.ModelReleaseID = *modelReleaseID
	}
	if useCaseID != nil {
		in.UseCaseID = *useCaseID
	}
	if exceptionCaseRef != nil {
		in.ExceptionCaseRef = *exceptionCaseRef
	}
	if containmentAction != nil {
		in.ContainmentAction = *containmentAction
	}
	if rootCause != nil {
		in.RootCause = *rootCause
	}
	if correctiveActions != nil {
		in.CorrectiveActions = *correctiveActions
	}
	if closureEvidence != nil {
		in.ClosureEvidence = *closureEvidence
	}
	if closedBy != nil {
		in.ClosedByPrincipalID = *closedBy
	}
	return &in, nil
}

// ReportIncident files a new AI incident. For AI-P0, the referenced
// model release (if any, and if currently ACTIVE or RESTRICTED) is
// quarantined in the SAME transaction as the incident is recorded;
// for AI-P1, it is restricted (if currently ACTIVE) — the doc's
// "immediate kill switch" and "restrict affected release" requirements
// enforced structurally, not as a follow-up manual step a caller could
// forget. An already-quarantined/restricted release is left alone
// (no error) rather than treated as a conflict — filing a second P0
// against an already-contained release is a legitimate, idempotent
// action.
func (s *PgStore) ReportIncident(ctx context.Context, req domain.ReportIncidentRequest, actor string) (*domain.AIIncident, error) {
	if !domain.IncidentSeverity(req.Severity).Valid() {
		return nil, domain.ErrInvalidIncidentSeverity
	}
	if req.ModelReleaseID == "" && req.UseCaseID == "" {
		return nil, domain.ErrIncidentMissingScope
	}
	if req.Description == "" {
		return nil, fmt.Errorf("description is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var release *domain.AIModelRelease
	if req.ModelReleaseID != "" {
		release, err = loadModelReleaseForUpdate(ctx, tx, req.ModelReleaseID)
		if err != nil {
			return nil, err
		}
	}

	var modelReleaseIDPtr, useCaseIDPtr, exceptionCaseRefPtr *string
	if req.ModelReleaseID != "" {
		modelReleaseIDPtr = &req.ModelReleaseID
	}
	if req.UseCaseID != "" {
		useCaseIDPtr = &req.UseCaseID
	}
	if req.ExceptionCaseRef != "" {
		exceptionCaseRefPtr = &req.ExceptionCaseRef
	}

	in, err := scanIncident(tx.QueryRow(ctx, `
		INSERT INTO ai_incidents (severity, model_release_id, use_case_id, exception_case_ref, description, reported_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+incidentColumns,
		req.Severity, modelReleaseIDPtr, useCaseIDPtr, exceptionCaseRefPtr, req.Description, actor))
	if err != nil {
		return nil, fmt.Errorf("insert ai incident: %w", err)
	}

	if release != nil {
		reason := fmt.Sprintf("auto-%s on incident %s", severityAction(in.Severity), in.IncidentID)
		switch {
		case in.Severity == domain.SeverityAIP0 && (release.ReleaseState == domain.ReleaseActive || release.ReleaseState == domain.ReleaseRestricted):
			if _, err := tx.Exec(ctx, `UPDATE ai_model_releases SET release_state = 'QUARANTINED', status_reason = $2 WHERE model_release_id = $1`,
				req.ModelReleaseID, reason); err != nil {
				return nil, fmt.Errorf("auto-quarantine release on AI-P0 incident: %w", err)
			}
		case in.Severity == domain.SeverityAIP1 && release.ReleaseState == domain.ReleaseActive:
			if _, err := tx.Exec(ctx, `UPDATE ai_model_releases SET release_state = 'RESTRICTED', status_reason = $2 WHERE model_release_id = $1`,
				req.ModelReleaseID, reason); err != nil {
				return nil, fmt.Errorf("auto-restrict release on AI-P1 incident: %w", err)
			}
		}
	}

	return in, tx.Commit(ctx)
}

func severityAction(s domain.IncidentSeverity) string {
	if s == domain.SeverityAIP0 {
		return "quarantine"
	}
	return "restrict"
}

func (s *PgStore) GetIncident(ctx context.Context, incidentID string) (*domain.AIIncident, error) {
	in, err := scanIncident(s.pool.QueryRow(ctx, `SELECT `+incidentColumns+` FROM ai_incidents WHERE incident_id = $1`, incidentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIncidentNotFound
	}
	return in, err
}

func loadIncidentForUpdate(ctx context.Context, tx pgx.Tx, incidentID string) (*domain.AIIncident, error) {
	in, err := scanIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM ai_incidents WHERE incident_id = $1 FOR UPDATE`, incidentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIncidentNotFound
	}
	return in, err
}

func (s *PgStore) ContainIncident(ctx context.Context, incidentID string, req domain.ContainIncidentRequest) (*domain.AIIncident, error) {
	if req.ContainmentAction == "" {
		return nil, fmt.Errorf("containment_action is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	in, err := loadIncidentForUpdate(ctx, tx, incidentID)
	if err != nil {
		return nil, err
	}
	if in.IncidentState != domain.IncidentOpen {
		return nil, domain.ErrIncidentNotOpen
	}
	updated, err := scanIncident(tx.QueryRow(ctx, `
		UPDATE ai_incidents SET incident_state = 'CONTAINED', containment_action = $2, contained_at = now()
		WHERE incident_id = $1 RETURNING `+incidentColumns,
		incidentID, req.ContainmentAction))
	if err != nil {
		return nil, fmt.Errorf("contain ai incident: %w", err)
	}
	return updated, tx.Commit(ctx)
}

func (s *PgStore) ResolveIncident(ctx context.Context, incidentID string, req domain.ResolveIncidentRequest) (*domain.AIIncident, error) {
	if req.RootCause == "" || req.CorrectiveActions == "" {
		return nil, fmt.Errorf("root_cause and corrective_actions are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	in, err := loadIncidentForUpdate(ctx, tx, incidentID)
	if err != nil {
		return nil, err
	}
	if in.IncidentState != domain.IncidentContained {
		return nil, domain.ErrIncidentNotContained
	}
	updated, err := scanIncident(tx.QueryRow(ctx, `
		UPDATE ai_incidents SET incident_state = 'RESOLVED', root_cause = $2, corrective_actions = $3, resolved_at = now()
		WHERE incident_id = $1 RETURNING `+incidentColumns,
		incidentID, req.RootCause, req.CorrectiveActions))
	if err != nil {
		return nil, fmt.Errorf("resolve ai incident: %w", err)
	}
	return updated, tx.Commit(ctx)
}

func (s *PgStore) CloseIncident(ctx context.Context, incidentID string, req domain.CloseIncidentRequest, actor string) (*domain.AIIncident, error) {
	if req.ClosureEvidence == "" {
		return nil, fmt.Errorf("closure_evidence is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	in, err := loadIncidentForUpdate(ctx, tx, incidentID)
	if err != nil {
		return nil, err
	}
	if in.IncidentState != domain.IncidentResolved {
		return nil, domain.ErrIncidentNotResolved
	}
	updated, err := scanIncident(tx.QueryRow(ctx, `
		UPDATE ai_incidents SET incident_state = 'CLOSED', closure_evidence = $2, closed_at = now(), closed_by_principal_id = $3
		WHERE incident_id = $1 RETURNING `+incidentColumns,
		incidentID, req.ClosureEvidence, actor))
	if err != nil {
		return nil, fmt.Errorf("close ai incident: %w", err)
	}
	return updated, tx.Commit(ctx)
}

// ReactivateRelease is AIG-05's governed reactivation gate (§8.6): a
// QUARANTINED release may only return to ACTIVE once root cause is
// documented (a RESOLVED/CLOSED incident naming this release) and a
// PASS evaluation has been recorded for it AFTER that incident was
// reported — proving the corrective change was actually re-evaluated,
// not just asserted fixed. This is a new transition migration 000006
// added to the AIG-02 trigger specifically for this gate.
func (s *PgStore) ReactivateRelease(ctx context.Context, modelReleaseID string, req domain.ReactivateReleaseRequest, actor string) (*domain.AIModelRelease, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	release, err := loadModelReleaseForUpdate(ctx, tx, modelReleaseID)
	if err != nil {
		return nil, err
	}
	if release.ReleaseState != domain.ReleaseQuarantined {
		return nil, domain.ErrInvalidReleaseTransition
	}

	incident, err := scanIncident(tx.QueryRow(ctx, `SELECT `+incidentColumns+` FROM ai_incidents WHERE incident_id = $1`, req.IncidentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrReactivationIncidentNotFound
	}
	if err != nil {
		return nil, err
	}
	if incident.ModelReleaseID != modelReleaseID {
		return nil, domain.ErrReactivationIncidentScopeMismatch
	}
	if (incident.IncidentState != domain.IncidentResolved && incident.IncidentState != domain.IncidentClosed) || incident.RootCause == "" {
		return nil, domain.ErrReactivationIncidentNotClosedOrResolved
	}

	var passingReevaluationExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM ai_evaluations WHERE model_release_id = $1 AND result = 'PASS' AND evaluated_at > $2)`,
		modelReleaseID, incident.ReportedAt).Scan(&passingReevaluationExists); err != nil {
		return nil, fmt.Errorf("check for passing re-evaluation: %w", err)
	}
	if !passingReevaluationExists {
		return nil, domain.ErrReactivationNoPassingReevaluation
	}

	var reasonPtr *string
	if req.Reason != "" {
		reasonPtr = &req.Reason
	}
	updated, err := scanModelRelease(tx.QueryRow(ctx, `
		UPDATE ai_model_releases SET release_state = 'ACTIVE', status_reason = $2 WHERE model_release_id = $1
		RETURNING `+modelReleaseColumns,
		modelReleaseID, reasonPtr))
	if err != nil {
		return nil, fmt.Errorf("reactivate ai model release: %w", err)
	}
	return updated, tx.Commit(ctx)
}
