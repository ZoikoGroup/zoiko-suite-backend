// Control test / attestation persistence — doc7 §E3, §E6, §I3.
//
// Every query runs inside the caller's tenant scope (withRLS): executions and
// attestations are evidence about one tenant's controls and are visible only
// to it, and a definition is visible to its owner, or to everyone when it is
// a platform catalogue entry (tenant_id NULL). Migration 000007 enforces the
// same with FORCE ROW LEVEL SECURITY.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	tenant_id,
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
		&d.TenantID,
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

// CreateControlTestDefinition inserts a definition owned by the caller's
// tenant, or returns the existing one on a (tenant, test_code) dedup match —
// the same idempotent-creation pattern as PgStore.CreatePolicy.
func (s *PgStore) CreateControlTestDefinition(ctx context.Context, params domain.CreateControlTestDefinitionParams) (*domain.ControlTestDefinition, bool, error) {
	if params.ControlTestDefinitionID == "" {
		params.ControlTestDefinitionID = uuid.New().String()
	}
	if params.TestFrequency == "" {
		params.TestFrequency = "AD_HOC"
	}

	var out *domain.ControlTestDefinition
	var created bool
	err := s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		d, err := scanControlTestDefinition(tx.QueryRow(ctx, `
			INSERT INTO control_test_definitions (
				control_test_definition_id, tenant_id, control_ref, test_code, title, methodology,
				sample_approach, test_frequency, created_by_principal_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT DO NOTHING
			RETURNING `+controlTestDefinitionColumns+`;`,
			params.ControlTestDefinitionID, params.TenantID, params.ControlRef, params.TestCode, params.Title, params.Methodology,
			nullableString(params.SampleApproach), params.TestFrequency, params.CreatedByPrincipalID,
		))
		if err == nil {
			out, created = d, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		existing, err := scanControlTestDefinition(tx.QueryRow(ctx, `
			SELECT `+controlTestDefinitionColumns+`
			FROM control_test_definitions
			WHERE tenant_id = $1::uuid AND test_code = $2;`, params.TenantID, params.TestCode))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrConflict // the caller-supplied id belongs to another definition
		}
		if err != nil {
			return err
		}
		if existing.ControlRef != params.ControlRef || existing.Methodology != params.Methodology {
			return domain.ErrConflict
		}
		out = existing
		return nil
	})
	if err != nil {
		return nil, false, s.domainError("CreateControlTestDefinition", err)
	}
	return out, created, nil
}

// FindControlTestDefinitionByID looks up a definition visible to the caller.
func (s *PgStore) FindControlTestDefinitionByID(ctx context.Context, id string) (*domain.ControlTestDefinition, error) {
	var d *domain.ControlTestDefinition
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		var scanErr error
		d, scanErr = scanControlTestDefinition(tx.QueryRow(ctx, `
			SELECT `+controlTestDefinitionColumns+`
			FROM control_test_definitions
			WHERE control_test_definition_id = $1;`, id))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrControlTestDefinitionNotFound
	}
	if err != nil {
		return nil, s.domainError("FindControlTestDefinitionByID", err)
	}
	return d, nil
}

// ── control_test_executions ──────────────────────────────────────────────────

const controlTestExecutionColumns = `
	control_test_execution_id,
	tenant_id,
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
		&e.TenantID,
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

// CreateControlTestExecution records a test run in the caller's tenant.
// Append-only — no dedup key, since a genuine re-test of the same period is a
// legitimate second execution, not a duplicate.
func (s *PgStore) CreateControlTestExecution(ctx context.Context, params domain.CreateControlTestExecutionParams) (*domain.ControlTestExecution, error) {
	if params.ControlTestExecutionID == "" {
		params.ControlTestExecutionID = uuid.New().String()
	}
	evidenceRefs, err := marshalStringSlice(params.EvidenceRefs)
	if err != nil {
		return nil, s.domainError("CreateControlTestExecution", err)
	}

	var out *domain.ControlTestExecution
	err = s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		var visible bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM control_test_definitions WHERE control_test_definition_id = $1);`,
			params.ControlTestDefinitionID).Scan(&visible); err != nil {
			return err
		}
		if !visible {
			return domain.ErrControlTestDefinitionNotFound
		}
		var scanErr error
		out, scanErr = scanControlTestExecution(tx.QueryRow(ctx, `
			INSERT INTO control_test_executions (
				control_test_execution_id, tenant_id, control_test_definition_id, period_start, period_end,
				population_description, sample_description, procedure_notes, evidence_refs,
				tester_principal_id, result, exceptions_noted, reviewer_principal_id,
				created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			RETURNING `+controlTestExecutionColumns+`;`,
			params.ControlTestExecutionID, params.TenantID, params.ControlTestDefinitionID, params.PeriodStart, params.PeriodEnd,
			nullableString(params.PopulationDescription), nullableString(params.SampleDescription), nullableString(params.ProcedureNotes), evidenceRefs,
			params.TesterPrincipalID, params.Result, nullableString(params.ExceptionsNoted), nullableString(params.ReviewerPrincipalID),
			params.CreatedByPrincipalID,
		))
		if isUniqueViolation(scanErr) {
			return domain.ErrConflict
		}
		return scanErr
	})
	if err != nil {
		return nil, s.domainError("CreateControlTestExecution", err)
	}
	return out, nil
}

// ListControlTestExecutions returns the caller's executions of a definition,
// most-recent period first.
func (s *PgStore) ListControlTestExecutions(ctx context.Context, controlTestDefinitionID string) ([]*domain.ControlTestExecution, error) {
	var out []*domain.ControlTestExecution
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+controlTestExecutionColumns+`
			FROM control_test_executions
			WHERE control_test_definition_id = $1
			ORDER BY period_end DESC, created_at DESC;`, controlTestDefinitionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, scanErr := scanControlTestExecution(rows)
			if scanErr != nil {
				return scanErr
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.domainError("ListControlTestExecutions", err)
	}
	return out, nil
}

// ResolveControlEffectiveness composes doc7 §E3's two independent
// dimensions for the caller's tenant: DESIGN_STATUS (a visible, non-retired
// definition exists for control_ref) and OPERATING_EFFECTIVENESS (the most
// recent of the tenant's own executions for that control_ref). Never
// collapses them into one value.
func (s *PgStore) ResolveControlEffectiveness(ctx context.Context, controlRef string) (*domain.ControlEffectiveness, error) {
	result := &domain.ControlEffectiveness{
		ControlRef:             controlRef,
		DesignStatus:           "NOT_TESTED",
		OperatingEffectiveness: "NO_EXECUTIONS_RECORDED",
	}
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		var designed bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM control_test_definitions
				WHERE control_ref = $1 AND design_status = 'DESIGNED'
			);`, controlRef).Scan(&designed); err != nil {
			return err
		}
		if designed {
			result.DesignStatus = "TESTED"
		}

		var executionID, execResult string
		var periodEnd time.Time
		err := tx.QueryRow(ctx, `
			SELECT ce.control_test_execution_id, ce.result, ce.period_end
			FROM control_test_executions ce
			JOIN control_test_definitions cd ON cd.control_test_definition_id = ce.control_test_definition_id
			WHERE cd.control_ref = $1
			ORDER BY ce.period_end DESC, ce.created_at DESC
			LIMIT 1;`, controlRef).Scan(&executionID, &execResult, &periodEnd)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result.OperatingEffectiveness = execResult
		result.LatestExecutionID = &executionID
		result.AsOf = &periodEnd
		return nil
	})
	if err != nil {
		return nil, s.domainError("ResolveControlEffectiveness", err)
	}
	return result, nil
}

// ── attestations ──────────────────────────────────────────────────────────────

const attestationColumns = `
	attestation_id,
	tenant_id,
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
		&a.TenantID,
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

// CreateAttestation records a signed assertion in the caller's tenant.
// Append-only creation — challenge/revocation is a status transition.
func (s *PgStore) CreateAttestation(ctx context.Context, params domain.CreateAttestationParams) (*domain.Attestation, error) {
	if params.AttestationID == "" {
		params.AttestationID = uuid.New().String()
	}
	evidenceRefs, err := marshalStringSlice(params.EvidenceRefs)
	if err != nil {
		return nil, s.domainError("CreateAttestation", err)
	}

	var out *domain.Attestation
	err = s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		var scanErr error
		out, scanErr = scanAttestation(tx.QueryRow(ctx, `
			INSERT INTO attestations (
				attestation_id, tenant_id, statement, statement_version, subject_ref, period_start, period_end,
				signer_principal_id, signer_role, evidence_refs, expires_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING `+attestationColumns+`;`,
			params.AttestationID, params.TenantID, params.Statement, params.StatementVersion, params.SubjectRef, params.PeriodStart, params.PeriodEnd,
			params.SignerPrincipalID, params.SignerRole, evidenceRefs, params.ExpiresAt, params.CreatedByPrincipalID,
		))
		if isUniqueViolation(scanErr) {
			return domain.ErrConflict
		}
		return scanErr
	})
	if err != nil {
		return nil, s.domainError("CreateAttestation", err)
	}
	return out, nil
}

// FindAttestationByID looks up one of the caller's attestations.
func (s *PgStore) FindAttestationByID(ctx context.Context, id string) (*domain.Attestation, error) {
	var a *domain.Attestation
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		var scanErr error
		a, scanErr = scanAttestation(tx.QueryRow(ctx, `
			SELECT `+attestationColumns+` FROM attestations WHERE attestation_id = $1;`, id))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAttestationNotFound
	}
	if err != nil {
		return nil, s.domainError("FindAttestationByID", err)
	}
	return a, nil
}

// RevokeAttestation transitions one of the caller's attestations to REVOKED
// — legal only from ACTIVE or CHALLENGED.
func (s *PgStore) RevokeAttestation(ctx context.Context, attestationID, reason string) (*domain.Attestation, error) {
	var a *domain.Attestation
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		var scanErr error
		a, scanErr = scanAttestation(tx.QueryRow(ctx, `
			UPDATE attestations
			SET attestation_status = 'REVOKED', revocation_reason = $1
			WHERE attestation_id = $2 AND attestation_status = ANY($3::text[])
			RETURNING `+attestationColumns+`;`,
			nullableString(reason), attestationID, []string{"ACTIVE", "CHALLENGED"}))
		if !errors.Is(scanErr, pgx.ErrNoRows) {
			return scanErr
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attestations WHERE attestation_id = $1);`, attestationID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrAttestationNotFound
		}
		return domain.ErrInvalidAttestationTransition
	})
	if err != nil {
		return nil, s.domainError("RevokeAttestation", err)
	}
	return a, nil
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
