// Control test / attestation persistence — doc7 §E3, §E6, §I3.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/domain"
)

func marshalStringSlice(v []string) ([]byte, error) {
	if v == nil {
		v = []string{}
	}
	return json.Marshal(v)
}

func unmarshalStringSlice(data []byte) []string {
	var out []string
	_ = json.Unmarshal(data, &out)
	return out
}

// ── control_test_definitions ─────────────────────────────────────────────────

const controlTestDefinitionColumns = `
	control_test_definition_id,
	control_ref,
	test_code,
	title,
	methodology,
	sample_approach,
	test_frequency,
	design_status,
	created_at,
	created_by_principal_id`

func scanControlTestDefinition(row pgx.Row) (*domain.ControlTestDefinition, error) {
	d := &domain.ControlTestDefinition{}
	var sampleApproach *string
	err := row.Scan(
		&d.ControlTestDefinitionID,
		&d.ControlRef,
		&d.TestCode,
		&d.Title,
		&d.Methodology,
		&sampleApproach,
		&d.TestFrequency,
		&d.DesignStatus,
		&d.CreatedAt,
		&d.CreatedByPrincipalID,
	)
	if sampleApproach != nil {
		d.SampleApproach = *sampleApproach
	}
	return d, err
}

// CreateControlTestDefinition inserts a new definition or returns the
// existing one on a test_code dedup match — same idempotent-creation
// pattern as PgStore.CreatePolicy.
func (s *PgStore) CreateControlTestDefinition(ctx context.Context, params domain.CreateControlTestDefinitionParams) (*domain.ControlTestDefinition, bool, error) {
	if params.ControlTestDefinitionID == "" {
		params.ControlTestDefinitionID = uuid.New().String()
	}
	if params.TestFrequency == "" {
		params.TestFrequency = "AD_HOC"
	}

	const query = `
		INSERT INTO control_test_definitions (
			control_test_definition_id, control_ref, test_code, title, methodology,
			sample_approach, test_frequency, created_by_principal_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (test_code)
		DO NOTHING
		RETURNING ` + controlTestDefinitionColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.ControlTestDefinitionID, params.ControlRef, params.TestCode, params.Title, params.Methodology,
		nullableString(params.SampleApproach), params.TestFrequency, params.CreatedByPrincipalID,
	)

	d, err := scanControlTestDefinition(row)
	if err == nil {
		return d, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreateControlTestDefinition failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	const lookupQuery = `
		SELECT ` + controlTestDefinitionColumns + `
		FROM control_test_definitions
		WHERE test_code = $1;`

	row = s.pool.QueryRow(ctx, lookupQuery, params.TestCode)
	d, err = scanControlTestDefinition(row)
	if err != nil {
		s.log.Error("pg CreateControlTestDefinition lookup existing failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if d.ControlRef != params.ControlRef || d.Methodology != params.Methodology {
		s.log.Warn("control test definition dedup match but attribute mismatch (409 conflict)",
			zap.String("existing_id", d.ControlTestDefinitionID),
		)
		return nil, false, domain.ErrConflict
	}
	return d, false, nil
}

// FindControlTestDefinitionByID looks up a definition by its UUID primary key.
func (s *PgStore) FindControlTestDefinitionByID(ctx context.Context, id string) (*domain.ControlTestDefinition, error) {
	const query = `
		SELECT ` + controlTestDefinitionColumns + `
		FROM control_test_definitions
		WHERE control_test_definition_id = $1;`

	row := s.pool.QueryRow(ctx, query, id)
	d, err := scanControlTestDefinition(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrControlTestDefinitionNotFound
		}
		s.log.Error("pg FindControlTestDefinitionByID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

// ── control_test_executions ──────────────────────────────────────────────────

const controlTestExecutionColumns = `
	control_test_execution_id,
	control_test_definition_id,
	period_start,
	period_end,
	population_description,
	sample_description,
	procedure_notes,
	evidence_refs,
	tester_principal_id,
	result,
	exceptions_noted,
	reviewer_principal_id,
	reviewed_at,
	created_at,
	created_by_principal_id`

func scanControlTestExecution(row pgx.Row) (*domain.ControlTestExecution, error) {
	e := &domain.ControlTestExecution{}
	var population, sample, procedure, exceptions *string
	var evidenceRefs []byte
	err := row.Scan(
		&e.ControlTestExecutionID,
		&e.ControlTestDefinitionID,
		&e.PeriodStart,
		&e.PeriodEnd,
		&population,
		&sample,
		&procedure,
		&evidenceRefs,
		&e.TesterPrincipalID,
		&e.Result,
		&exceptions,
		&e.ReviewerPrincipalID,
		&e.ReviewedAt,
		&e.CreatedAt,
		&e.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, err
	}
	if population != nil {
		e.PopulationDescription = *population
	}
	if sample != nil {
		e.SampleDescription = *sample
	}
	if procedure != nil {
		e.ProcedureNotes = *procedure
	}
	if exceptions != nil {
		e.ExceptionsNoted = *exceptions
	}
	e.EvidenceRefs = unmarshalStringSlice(evidenceRefs)
	return e, nil
}

// CreateControlTestExecution records a new test run. Append-only — no
// idempotent dedup key, since a genuine re-test of the same period is a
// legitimate second execution, not a duplicate (unlike definitions, which
// dedup on test_code).
func (s *PgStore) CreateControlTestExecution(ctx context.Context, params domain.CreateControlTestExecutionParams) (*domain.ControlTestExecution, error) {
	if _, err := s.FindControlTestDefinitionByID(ctx, params.ControlTestDefinitionID); err != nil {
		return nil, err
	}
	if params.ControlTestExecutionID == "" {
		params.ControlTestExecutionID = uuid.New().String()
	}
	evidenceRefs, err := marshalStringSlice(params.EvidenceRefs)
	if err != nil {
		return nil, fmt.Errorf("marshal evidence_refs: %w", err)
	}

	const query = `
		INSERT INTO control_test_executions (
			control_test_execution_id, control_test_definition_id, period_start, period_end,
			population_description, sample_description, procedure_notes, evidence_refs,
			tester_principal_id, result, exceptions_noted, reviewer_principal_id,
			created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING ` + controlTestExecutionColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.ControlTestExecutionID, params.ControlTestDefinitionID, params.PeriodStart, params.PeriodEnd,
		nullableString(params.PopulationDescription), nullableString(params.SampleDescription), nullableString(params.ProcedureNotes), evidenceRefs,
		params.TesterPrincipalID, params.Result, nullableString(params.ExceptionsNoted), nullableString(params.ReviewerPrincipalID),
		params.CreatedByPrincipalID,
	)
	e, err := scanControlTestExecution(row)
	if err != nil {
		s.log.Error("pg CreateControlTestExecution failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return e, nil
}

// ListControlTestExecutions returns every execution for a definition,
// most-recent period first.
func (s *PgStore) ListControlTestExecutions(ctx context.Context, controlTestDefinitionID string) ([]*domain.ControlTestExecution, error) {
	const query = `
		SELECT ` + controlTestExecutionColumns + `
		FROM control_test_executions
		WHERE control_test_definition_id = $1
		ORDER BY period_end DESC, created_at DESC;`

	rows, err := s.pool.Query(ctx, query, controlTestDefinitionID)
	if err != nil {
		s.log.Error("pg ListControlTestExecutions failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var out []*domain.ControlTestExecution
	for rows.Next() {
		e, scanErr := scanControlTestExecution(rows)
		if scanErr != nil {
			s.log.Error("pg ListControlTestExecutions scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ResolveControlEffectiveness composes doc7 §E3's two independent
// dimensions: DESIGN_STATUS (a non-retired control_test_definitions row
// exists for control_ref) and OPERATING_EFFECTIVENESS (the most recent
// control_test_executions.result across every definition for that
// control_ref). Never collapses them into one value.
func (s *PgStore) ResolveControlEffectiveness(ctx context.Context, controlRef string) (*domain.ControlEffectiveness, error) {
	const designQuery = `
		SELECT EXISTS (
			SELECT 1 FROM control_test_definitions
			WHERE control_ref = $1 AND design_status = 'DESIGNED'
		);`
	var designed bool
	if err := s.pool.QueryRow(ctx, designQuery, controlRef).Scan(&designed); err != nil {
		s.log.Error("pg ResolveControlEffectiveness design lookup failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	result := &domain.ControlEffectiveness{
		ControlRef:             controlRef,
		DesignStatus:           "NOT_TESTED",
		OperatingEffectiveness: "NO_EXECUTIONS_RECORDED",
	}
	if designed {
		result.DesignStatus = "TESTED"
	}

	const latestExecutionQuery = `
		SELECT ce.control_test_execution_id, ce.result, ce.period_end
		FROM control_test_executions ce
		JOIN control_test_definitions cd ON cd.control_test_definition_id = ce.control_test_definition_id
		WHERE cd.control_ref = $1
		ORDER BY ce.period_end DESC, ce.created_at DESC
		LIMIT 1;`
	var executionID, execResult string
	var periodEnd time.Time
	row := s.pool.QueryRow(ctx, latestExecutionQuery, controlRef)
	err := row.Scan(&executionID, &execResult, &periodEnd)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		s.log.Error("pg ResolveControlEffectiveness execution lookup failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	result.OperatingEffectiveness = execResult
	result.LatestExecutionID = &executionID
	result.AsOf = &periodEnd
	return result, nil
}

// ── attestations ──────────────────────────────────────────────────────────────

const attestationColumns = `
	attestation_id,
	statement,
	statement_version,
	subject_ref,
	period_start,
	period_end,
	signer_principal_id,
	signer_role,
	signed_at,
	evidence_refs,
	expires_at,
	attestation_status,
	revocation_reason,
	created_at,
	created_by_principal_id`

func scanAttestation(row pgx.Row) (*domain.Attestation, error) {
	a := &domain.Attestation{}
	var evidenceRefs []byte
	var revocationReason *string
	err := row.Scan(
		&a.AttestationID,
		&a.Statement,
		&a.StatementVersion,
		&a.SubjectRef,
		&a.PeriodStart,
		&a.PeriodEnd,
		&a.SignerPrincipalID,
		&a.SignerRole,
		&a.SignedAt,
		&evidenceRefs,
		&a.ExpiresAt,
		&a.AttestationStatus,
		&revocationReason,
		&a.CreatedAt,
		&a.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, err
	}
	a.EvidenceRefs = unmarshalStringSlice(evidenceRefs)
	if revocationReason != nil {
		a.RevocationReason = *revocationReason
	}
	return a, nil
}

// CreateAttestation records a new signed assertion. Append-only creation —
// challenge/revocation is a separate status transition, never a delete.
func (s *PgStore) CreateAttestation(ctx context.Context, params domain.CreateAttestationParams) (*domain.Attestation, error) {
	if params.AttestationID == "" {
		params.AttestationID = uuid.New().String()
	}
	evidenceRefs, err := marshalStringSlice(params.EvidenceRefs)
	if err != nil {
		return nil, fmt.Errorf("marshal evidence_refs: %w", err)
	}

	const query = `
		INSERT INTO attestations (
			attestation_id, statement, statement_version, subject_ref, period_start, period_end,
			signer_principal_id, signer_role, evidence_refs, expires_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING ` + attestationColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.AttestationID, params.Statement, params.StatementVersion, params.SubjectRef, params.PeriodStart, params.PeriodEnd,
		params.SignerPrincipalID, params.SignerRole, evidenceRefs, params.ExpiresAt, params.CreatedByPrincipalID,
	)
	a, err := scanAttestation(row)
	if err != nil {
		s.log.Error("pg CreateAttestation failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// FindAttestationByID looks up an attestation by its UUID primary key.
func (s *PgStore) FindAttestationByID(ctx context.Context, id string) (*domain.Attestation, error) {
	const query = `
		SELECT ` + attestationColumns + `
		FROM attestations
		WHERE attestation_id = $1;`

	row := s.pool.QueryRow(ctx, query, id)
	a, err := scanAttestation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrAttestationNotFound
		}
		s.log.Error("pg FindAttestationByID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// RevokeAttestation transitions an attestation to REVOKED — legal only from
// ACTIVE or CHALLENGED, same atomic UPDATE...WHERE-status-IN pattern as
// transitionVersionStatus.
func (s *PgStore) RevokeAttestation(ctx context.Context, attestationID, reason string) (*domain.Attestation, error) {
	const query = `
		UPDATE attestations
		SET attestation_status = 'REVOKED', revocation_reason = $1
		WHERE attestation_id = $2 AND attestation_status = ANY($3::text[])
		RETURNING ` + attestationColumns + `;`

	row := s.pool.QueryRow(ctx, query, nullableString(reason), attestationID, []string{"ACTIVE", "CHALLENGED"})
	a, err := scanAttestation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, findErr := s.FindAttestationByID(ctx, attestationID); findErr != nil {
				return nil, findErr
			}
			return nil, domain.ErrInvalidAttestationTransition
		}
		s.log.Error("pg RevokeAttestation failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
