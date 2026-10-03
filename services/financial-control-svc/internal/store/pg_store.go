// Package store is financial-control-svc's PostgreSQL persistence layer.
//
// Every operation runs in withRLS (sets app.tenant_id) AND filters explicitly
// by tenant_id in its own SQL: this platform's pool connects as a Postgres
// superuser, which bypasses row-level security, so RLS alone is not isolation.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/events"
	"zoiko.io/financial-control-svc/internal/outbox"
)

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore { return &PgStore{pool: pool, log: log} }

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return domain.ErrTenantScopeMissing
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return mapPgError(fmt.Errorf("set_config app.tenant_id: %w", err))
	}
	if err := fn(tx); err != nil {
		return mapPgError(err)
	}
	return mapPgError(tx.Commit(ctx))
}

// mapPgError turns caller mistakes into domain errors so they answer 4xx, not 503.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "22P02", "22007", "22008", "22001":
			return domain.ErrInvalidIdentifier
		case "23505":
			return domain.ErrDuplicate
		case "23514", "23503":
			return fmt.Errorf("%w: %s", domain.ErrInvalidArgument, pgErr.Message)
		}
	}
	return err
}

// ─── Control definitions ─────────────────────────────────────────────────────

const defColumns = `control_definition_id, tenant_id, control_code, name, domain, control_type, assertions,
	risk_tier, frequency, close_gating, scope, owner_role, reviewer_role, certifier_role,
	source_spec, target_spec, evidence_policy, policy_refs, created_by, created_at, correlation_id`

func scanDef(row pgx.Row, d *domain.ControlDefinition) error {
	return row.Scan(&d.ControlDefinitionID, &d.TenantID, &d.ControlCode, &d.Name, &d.Domain, &d.ControlType,
		&d.Assertions, &d.RiskTier, &d.Frequency, &d.CloseGating, &d.Scope, &d.OwnerRole, &d.ReviewerRole,
		&d.CertifierRole, &d.SourceSpec, &d.TargetSpec, &d.EvidencePolicy, &d.PolicyRefs, &d.CreatedBy,
		&d.CreatedAt, &d.CorrelationID)
}

func orEmptyObject(b json.RawMessage) json.RawMessage {
	if len(b) == 0 || string(b) == "null" {
		return json.RawMessage(`{}`)
	}
	return b
}

// CreateDefinition inserts a definition and its rule version 1 atomically.
// The first version is created UNAPPROVED; a different principal must approve
// it before any run can pin it (Invariant: independent approval for key controls).
func (s *PgStore) CreateDefinition(ctx context.Context, tenantID, actor, correlationID string, req domain.CreateControlDefinitionRequest) (*domain.ControlDefinition, error) {
	digest, err := domain.DigestLogic(req.InitialLogic)
	if err != nil {
		return nil, err
	}
	var out domain.ControlDefinition
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO control_definitions (tenant_id, control_code, name, domain, control_type, assertions,
				risk_tier, frequency, close_gating, scope, owner_role, reviewer_role, certifier_role,
				source_spec, target_spec, evidence_policy, policy_refs, created_by, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
			RETURNING `+defColumns,
			tenantID, req.ControlCode, req.Name, req.Domain, req.ControlType, req.Assertions,
			req.RiskTier, req.Frequency, req.CloseGating, orEmptyObject(req.Scope), req.OwnerRole,
			req.ReviewerRole, req.CertifierRole, orEmptyObject(req.SourceSpec), orEmptyObject(req.TargetSpec),
			orEmptyObject(req.EvidencePolicy), nonNilStrings(req.PolicyRefs), actor, correlationID)
		if err := scanDef(row, &out); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO control_rule_versions (tenant_id, control_definition_id, rule_version, logic_digest,
				logic, test_pack_version, effective_from, created_by)
			VALUES ($1,$2,1,$3,$4,$5,$6::date,$7)`,
			tenantID, out.ControlDefinitionID, digest, req.InitialLogic, req.InitialTestPack, req.InitialEffectiveFrom, actor)
		return err
	})
	if err != nil {
		return nil, err
	}
	v := 1
	out.LatestRuleVersion = &v
	return &out, nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *PgStore) GetDefinition(ctx context.Context, tenantID, id string) (*domain.ControlDefinition, error) {
	var d domain.ControlDefinition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := scanDef(tx.QueryRow(ctx, `SELECT `+defColumns+` FROM control_definitions
			WHERE tenant_id = $1 AND control_definition_id = $2`, tenantID, id), &d); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		var v *int
		if err := tx.QueryRow(ctx, `SELECT MAX(rule_version) FROM control_rule_versions
			WHERE tenant_id = $1 AND control_definition_id = $2`, tenantID, id).Scan(&v); err != nil {
			return err
		}
		d.LatestRuleVersion = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *PgStore) ListDefinitions(ctx context.Context, tenantID string, limit int) ([]domain.ControlDefinition, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []domain.ControlDefinition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+defColumns+` FROM control_definitions
			WHERE tenant_id = $1 ORDER BY control_code LIMIT $2`, tenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.ControlDefinition
			if err := scanDef(rows, &d); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// ─── Rule versions ───────────────────────────────────────────────────────────

const ruleColumns = `rule_version_id, tenant_id, control_definition_id, rule_version, logic_digest, logic,
	test_pack_version, to_char(effective_from,'YYYY-MM-DD'), to_char(effective_to,'YYYY-MM-DD'),
	approved_by, approved_at, created_by, created_at`

func scanRule(row pgx.Row, v *domain.ControlRuleVersion) error {
	return row.Scan(&v.RuleVersionID, &v.TenantID, &v.ControlDefinitionID, &v.RuleVersion, &v.LogicDigest,
		&v.Logic, &v.TestPackVersion, &v.EffectiveFrom, &v.EffectiveTo, &v.ApprovedBy, &v.ApprovedAt,
		&v.CreatedBy, &v.CreatedAt)
}

func (s *PgStore) CreateRuleVersion(ctx context.Context, tenantID, definitionID, actor string, req domain.CreateRuleVersionRequest) (*domain.ControlRuleVersion, error) {
	digest, err := domain.DigestLogic(req.Logic)
	if err != nil {
		return nil, err
	}
	var out domain.ControlRuleVersion
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM control_definitions
			WHERE tenant_id = $1 AND control_definition_id = $2)`, tenantID, definitionID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrNotFound
		}
		// Serialise version numbering per definition.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM control_definitions
			WHERE tenant_id = $1 AND control_definition_id = $2 FOR UPDATE`, tenantID, definitionID); err != nil {
			return err
		}
		return scanRule(tx.QueryRow(ctx, `
			INSERT INTO control_rule_versions (tenant_id, control_definition_id, rule_version, logic_digest,
				logic, test_pack_version, effective_from, created_by)
			SELECT $1::uuid, $2::uuid, COALESCE(MAX(rule_version),0)+1, $3::varchar, $4::jsonb, $5::varchar, $6::date, $7::varchar
			FROM control_rule_versions WHERE tenant_id = $1 AND control_definition_id = $2
			RETURNING `+ruleColumns,
			tenantID, definitionID, digest, req.Logic, req.TestPackVersion, req.EffectiveFrom, actor), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ApproveRuleVersion records the independent approval. The approver must not
// be the creator (also enforced by a CHECK constraint).
func (s *PgStore) ApproveRuleVersion(ctx context.Context, tenantID, definitionID string, version int, approver string) (*domain.ControlRuleVersion, error) {
	var out domain.ControlRuleVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var creator string
		var approved *string
		err := tx.QueryRow(ctx, `SELECT created_by, approved_by FROM control_rule_versions
			WHERE tenant_id = $1 AND control_definition_id = $2 AND rule_version = $3 FOR UPDATE`,
			tenantID, definitionID, version).Scan(&creator, &approved)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		if approver == creator {
			return domain.ErrSelfApproval
		}
		if approved != nil {
			return fmt.Errorf("%w: rule version already approved", domain.ErrInvalidTransition)
		}
		return scanRule(tx.QueryRow(ctx, `UPDATE control_rule_versions SET approved_by = $4, approved_at = now()
			WHERE tenant_id = $1 AND control_definition_id = $2 AND rule_version = $3
			RETURNING `+ruleColumns, tenantID, definitionID, version, approver), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) ListRuleVersions(ctx context.Context, tenantID, definitionID string) ([]domain.ControlRuleVersion, error) {
	var out []domain.ControlRuleVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+ruleColumns+` FROM control_rule_versions
			WHERE tenant_id = $1 AND control_definition_id = $2 ORDER BY rule_version`, tenantID, definitionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v domain.ControlRuleVersion
			if err := scanRule(rows, &v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// ─── Tolerance & materiality policies ────────────────────────────────────────

const tolColumns = `tolerance_id, tenant_id, legal_entity_id, metric, tolerance_version, absolute_tolerance::text,
	percentage_tolerance::text, percentage_base, date_tolerance_days, date_basis, currency, permitted_contexts,
	rationale, to_char(effective_from,'YYYY-MM-DD'), approved_by, created_by, created_at`

func scanTol(row pgx.Row, t *domain.TolerancePolicy) error {
	return row.Scan(&t.ToleranceID, &t.TenantID, &t.LegalEntityID, &t.Metric, &t.ToleranceVersion,
		&t.AbsoluteTolerance, &t.PercentageTolerance, &t.PercentageBase, &t.DateToleranceDays, &t.DateBasis,
		&t.Currency, &t.PermittedContexts, &t.Rationale, &t.EffectiveFrom, &t.ApprovedBy, &t.CreatedBy, &t.CreatedAt)
}

func defaultDecimal(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// CreateTolerancePolicy appends the next version for (entity, metric). Existing
// versions are never edited — widening is a NEW version that only future runs
// can pin (Invariant 4, scenario 04).
func (s *PgStore) CreateTolerancePolicy(ctx context.Context, tenantID, actor string, req domain.CreateTolerancePolicyRequest) (*domain.TolerancePolicy, error) {
	if req.DateBasis == "" {
		req.DateBasis = "CALENDAR"
	}
	var out domain.TolerancePolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanTol(tx.QueryRow(ctx, `
			INSERT INTO tolerance_policies (tenant_id, legal_entity_id, metric, tolerance_version, absolute_tolerance,
				percentage_tolerance, percentage_base, date_tolerance_days, date_basis, currency, permitted_contexts,
				rationale, effective_from, approved_by, created_by)
			SELECT $1::uuid,$2::uuid,$3::varchar, COALESCE(MAX(tolerance_version),0)+1, $4::numeric,$5::numeric,$6::varchar,$7::int,$8::varchar,$9::varchar,$10::text[],$11::text,$12::date,$13::varchar,$14::varchar
			FROM tolerance_policies WHERE tenant_id = $1 AND legal_entity_id = $2 AND metric = $3
			RETURNING `+tolColumns,
			tenantID, req.LegalEntityID, req.Metric, defaultDecimal(req.AbsoluteTolerance),
			defaultDecimal(req.PercentageTolerance), req.PercentageBase, req.DateToleranceDays, req.DateBasis,
			req.Currency, nonNilStrings(req.PermittedContexts), req.Rationale, req.EffectiveFrom, req.ApprovedBy, actor), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) ListTolerancePolicies(ctx context.Context, tenantID, legalEntityID, metric string) ([]domain.TolerancePolicy, error) {
	var out []domain.TolerancePolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+tolColumns+` FROM tolerance_policies
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND ($3 = '' OR metric = $3)
			ORDER BY metric, tolerance_version`, tenantID, legalEntityID, metric)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.TolerancePolicy
			if err := scanTol(rows, &t); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

const matColumns = `materiality_id, tenant_id, legal_entity_id, reporting_basis, materiality_version,
	amount_threshold::text, aggregate_threshold::text, currency, qualitative_triggers, aggregation_basis,
	to_char(effective_from,'YYYY-MM-DD'), approved_by, created_by, created_at`

func scanMat(row pgx.Row, m *domain.MaterialityPolicy) error {
	return row.Scan(&m.MaterialityID, &m.TenantID, &m.LegalEntityID, &m.ReportingBasis, &m.MaterialityVersion,
		&m.AmountThreshold, &m.AggregateThreshold, &m.Currency, &m.QualitativeTriggers, &m.AggregationBasis,
		&m.EffectiveFrom, &m.ApprovedBy, &m.CreatedBy, &m.CreatedAt)
}

func (s *PgStore) CreateMaterialityPolicy(ctx context.Context, tenantID, actor string, req domain.CreateMaterialityPolicyRequest) (*domain.MaterialityPolicy, error) {
	if req.AggregationBasis == "" {
		req.AggregationBasis = "ENTITY_PERIOD"
	}
	var out domain.MaterialityPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanMat(tx.QueryRow(ctx, `
			INSERT INTO materiality_policies (tenant_id, legal_entity_id, reporting_basis, materiality_version,
				amount_threshold, aggregate_threshold, currency, qualitative_triggers, aggregation_basis,
				effective_from, approved_by, created_by)
			SELECT $1::uuid,$2::uuid,$3::varchar, COALESCE(MAX(materiality_version),0)+1, $4::numeric,$5::numeric,$6::varchar,$7::text[],$8::varchar,$9::date,$10::varchar,$11::varchar
			FROM materiality_policies WHERE tenant_id = $1 AND legal_entity_id = $2 AND reporting_basis = $3
			RETURNING `+matColumns,
			tenantID, req.LegalEntityID, req.ReportingBasis, defaultDecimal(req.AmountThreshold),
			defaultDecimal(req.AggregateThreshold), req.Currency, nonNilStrings(req.QualitativeTriggers),
			req.AggregationBasis, req.EffectiveFrom, req.ApprovedBy, actor), &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ─── Control runs ────────────────────────────────────────────────────────────

const runColumns = `run_id, tenant_id, legal_entity_id, control_definition_id, rule_version, rule_digest,
	tolerance_id, materiality_id, period_id, scope, trigger_type, trigger_reason, prior_run_id,
	superseded_by_run_id, lifecycle_state, result_state, certification_state, version, started_at,
	completed_at, created_by, created_at, updated_at, correlation_id`

func scanRun(row pgx.Row, r *domain.ControlRun) error {
	return row.Scan(&r.RunID, &r.TenantID, &r.LegalEntityID, &r.ControlDefinitionID, &r.RuleVersion,
		&r.RuleDigest, &r.ToleranceID, &r.MaterialityID, &r.PeriodID, &r.Scope, &r.TriggerType,
		&r.TriggerReason, &r.PriorRunID, &r.SupersededByRunID, &r.LifecycleState, &r.ResultState,
		&r.CertificationState, &r.Version, &r.StartedAt, &r.CompletedAt, &r.CreatedBy, &r.CreatedAt,
		&r.UpdatedAt, &r.CorrelationID)
}

// CreateRun creates a governed run, PINNING the latest approved effective rule
// version and the latest effective tolerance/materiality policy. Idempotent on
// (tenant, idempotencyKey): a replay returns the original run (created=false)
// and a replay with a different body is a conflict, never a second run.
func (s *PgStore) CreateRun(ctx context.Context, tenantID, actor, correlationID, idempotencyKey string, req domain.CreateRunRequest) (*domain.ControlRun, bool, error) {
	var out domain.ControlRun
	created := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// Idempotent replay.
		err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
			WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, idempotencyKey), &out)
		if err == nil {
			if out.ControlDefinitionID != req.ControlDefinitionID || out.LegalEntityID != req.LegalEntityID ||
				out.PeriodID != req.PeriodID || out.TriggerType != req.TriggerType {
				return fmt.Errorf("%w: idempotency key reused with a different request", domain.ErrConflict)
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var code, tier string
		if err := tx.QueryRow(ctx, `SELECT control_code, risk_tier FROM control_definitions
			WHERE tenant_id = $1 AND control_definition_id = $2`, tenantID, req.ControlDefinitionID).Scan(&code, &tier); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}

		// Pin the rule version: latest APPROVED version effective today.
		var ruleVersion int
		var digest string
		if err := tx.QueryRow(ctx, `SELECT rule_version, logic_digest FROM control_rule_versions
			WHERE tenant_id = $1 AND control_definition_id = $2 AND approved_by IS NOT NULL
			  AND effective_from <= CURRENT_DATE AND (effective_to IS NULL OR effective_to >= CURRENT_DATE)
			ORDER BY rule_version DESC LIMIT 1`, tenantID, req.ControlDefinitionID).Scan(&ruleVersion, &digest); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: no approved, effective rule version exists for this control", domain.ErrInvalidArgument)
			}
			return err
		}

		// Pin tolerance/materiality. A named id must belong to this entity; otherwise the
		// latest effective policy is resolved server-side. The caller never supplies values.
		var tolID, matID *string
		if req.ToleranceID != "" {
			var id string
			if err := tx.QueryRow(ctx, `SELECT tolerance_id FROM tolerance_policies
				WHERE tenant_id = $1 AND legal_entity_id = $2 AND tolerance_id = $3`,
				tenantID, req.LegalEntityID, req.ToleranceID).Scan(&id); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("%w: tolerance_id not found for this legal entity", domain.ErrInvalidArgument)
				}
				return err
			}
			tolID = &id
		} else {
			var id string
			err := tx.QueryRow(ctx, `SELECT tolerance_id FROM tolerance_policies
				WHERE tenant_id = $1 AND legal_entity_id = $2 AND metric = $3 AND effective_from <= CURRENT_DATE
				ORDER BY tolerance_version DESC LIMIT 1`, tenantID, req.LegalEntityID, code).Scan(&id)
			if err == nil {
				tolID = &id
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if req.MaterialityID != "" {
			var id string
			if err := tx.QueryRow(ctx, `SELECT materiality_id FROM materiality_policies
				WHERE tenant_id = $1 AND legal_entity_id = $2 AND materiality_id = $3`,
				tenantID, req.LegalEntityID, req.MaterialityID).Scan(&id); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("%w: materiality_id not found for this legal entity", domain.ErrInvalidArgument)
				}
				return err
			}
			matID = &id
		} else {
			var id string
			err := tx.QueryRow(ctx, `SELECT materiality_id FROM materiality_policies
				WHERE tenant_id = $1 AND legal_entity_id = $2 AND effective_from <= CURRENT_DATE
				ORDER BY materiality_version DESC LIMIT 1`, tenantID, req.LegalEntityID).Scan(&id)
			if err == nil {
				matID = &id
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}

		var prior *string
		if req.PriorRunID != "" {
			var id string
			if err := tx.QueryRow(ctx, `SELECT run_id FROM control_runs
				WHERE tenant_id = $1 AND run_id = $2 AND control_definition_id = $3`,
				tenantID, req.PriorRunID, req.ControlDefinitionID).Scan(&id); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("%w: prior_run_id is not a run of this control", domain.ErrInvalidArgument)
				}
				return err
			}
			prior = &id
		}

		// Key controls require certification; others are monitored (NOT_REQUIRED).
		cert := domain.CertNotRequired
		if tier == "KEY" {
			cert = domain.CertPending
		}

		if err := scanRun(tx.QueryRow(ctx, `
			INSERT INTO control_runs (tenant_id, legal_entity_id, control_definition_id, rule_version, rule_digest,
				tolerance_id, materiality_id, period_id, scope, trigger_type, trigger_reason, prior_run_id,
				certification_state, created_by, idempotency_key, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			RETURNING `+runColumns,
			tenantID, req.LegalEntityID, req.ControlDefinitionID, ruleVersion, digest, tolID, matID, req.PeriodID,
			orEmptyObject(req.Scope), req.TriggerType, req.Reason, prior, string(cert), actor, idempotencyKey,
			correlationID), &out); err != nil {
			return err
		}
		created = true

		if err := insertTransition(ctx, tx, out.TenantID, out.RunID, "LIFECYCLE", "", string(domain.LifecycleScheduled),
			req.Reason, actor, correlationID); err != nil {
			return err
		}
		if prior != nil {
			if err := supersedePrior(ctx, tx, tenantID, *prior, out, actor, correlationID, req.Reason); err != nil {
				return err
			}
		}
		return emit(ctx, tx, out, events.RunCreated, actor, correlationID, map[string]any{
			"run_id": out.RunID, "control_definition_id": out.ControlDefinitionID, "control_code": code,
			"rule_version": out.RuleVersion, "rule_digest": out.RuleDigest, "tolerance_id": out.ToleranceID,
			"materiality_id": out.MaterialityID, "period_id": out.PeriodID, "trigger_type": out.TriggerType,
			"prior_run_id": out.PriorRunID, "lifecycle_state": out.LifecycleState,
		})
	})
	if err != nil {
		return nil, false, err
	}
	return &out, created, nil
}

func (s *PgStore) GetRun(ctx context.Context, tenantID, runID string) (*domain.ControlRun, error) {
	var r domain.ControlRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
			WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID), &r); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *PgStore) ListRuns(ctx context.Context, tenantID string, f domain.ListRunsFilter) ([]domain.ControlRun, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	var out []domain.ControlRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// Keyset pagination: a run inserted mid-scan cannot shift pages.
		rows, err := tx.Query(ctx, `SELECT `+runColumns+` FROM control_runs
			WHERE tenant_id = $1
			  AND ($2 = '' OR control_definition_id::text = $2)
			  AND ($3 = '' OR legal_entity_id::text = $3)
			  AND ($4 = '' OR period_id = $4)
			  AND ($5 = '' OR lifecycle_state = $5)
			  AND ($6::timestamptz IS NULL OR (created_at, run_id) < ($6::timestamptz, NULLIF($7,'')::uuid))
			ORDER BY created_at DESC, run_id DESC LIMIT $8`,
			tenantID, f.ControlDefinitionID, f.LegalEntityID, f.PeriodID, f.LifecycleState,
			f.AfterCreatedAt, f.AfterRunID, f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.ControlRun
			if err := scanRun(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) ListTransitions(ctx context.Context, tenantID, runID string) ([]domain.Transition, error) {
	var out []domain.Transition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT transition_id, run_id, dimension, from_state, to_state, reason,
			actor_id, correlation_id, occurred_at FROM control_run_transitions
			WHERE tenant_id = $1 AND run_id = $2 ORDER BY transition_id`, tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.Transition
			if err := rows.Scan(&t.TransitionID, &t.RunID, &t.Dimension, &t.FromState, &t.ToState, &t.Reason,
				&t.ActorID, &t.CorrelationID, &t.OccurredAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// AdvanceParams describes one atomic movement across the run's state dimensions.
type AdvanceParams struct {
	ExpectedVersion int
	ToLifecycle     domain.LifecycleState
	ToResult        *domain.ResultState
	ToCert          *domain.CertificationState
	Reason          string
	Actor           string
	CorrelationID   string
}

// AdvanceRun moves a run using optimistic concurrency (expected_version) and
// validates every dimension against ZS-STATE-001-style edge tables. It writes
// the new state, one append-only transition row per changed dimension, and the
// matching outbox event in ONE transaction. There is no other way to change a
// run's state.
func (s *PgStore) AdvanceRun(ctx context.Context, tenantID, runID string, p AdvanceParams) (*domain.ControlRun, error) {
	var out *domain.ControlRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = advanceTx(ctx, tx, tenantID, runID, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SkipVersionCheck is passed as ExpectedVersion by engine-internal movements
// (freeze, execute) that run under the row lock and are validated by the edge
// tables instead of a caller ETag.
const SkipVersionCheck = -1

// advanceTx is AdvanceRun inside a caller-owned transaction, so a population
// freeze or execution result commits atomically with its state movement.
func advanceTx(ctx context.Context, tx pgx.Tx, tenantID, runID string, p AdvanceParams) (*domain.ControlRun, error) {
	var cur, out domain.ControlRun
	if err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM control_runs
		WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`, tenantID, runID), &cur); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	if p.ExpectedVersion != SkipVersionCheck && cur.Version != p.ExpectedVersion {
		return nil, fmt.Errorf("%w: run is at version %d, caller expected %d", domain.ErrConflict, cur.Version, p.ExpectedVersion)
	}

	newResult, newCert := cur.ResultState, cur.CertificationState
	if p.ToResult != nil {
		if p.ToLifecycle == domain.LifecycleFailed {
			// A control that could not run can fail from ANY pre-terminal state (e.g. the
			// source is down while still PREPARING). The only results such a run may carry
			// are Fail or Indeterminate — never Pass (scenario 09).
			if *p.ToResult != domain.ResultFail && *p.ToResult != domain.ResultIndeterminate {
				return nil, fmt.Errorf("%w: a failed run cannot carry result %s", domain.ErrInvalidTransition, *p.ToResult)
			}
		} else if err := domain.ValidateResultTransition(cur.LifecycleState, cur.ResultState, *p.ToResult); err != nil {
			return nil, err
		}
		newResult = *p.ToResult
	}
	if p.ToCert != nil {
		if err := domain.ValidateCertificationTransition(cur.CertificationState, *p.ToCert); err != nil {
			return nil, err
		}
		newCert = *p.ToCert
	}
	if err := domain.ValidateLifecycleTransition(cur.LifecycleState, p.ToLifecycle, newResult, newCert); err != nil {
		return nil, err
	}

	terminal := domain.TerminalLifecycle(p.ToLifecycle) || p.ToLifecycle == domain.LifecycleCertified
	if err := scanRun(tx.QueryRow(ctx, `
		UPDATE control_runs SET lifecycle_state = $3::varchar, result_state = $4::varchar, certification_state = $5::varchar,
			version = version + 1, updated_at = now(),
			started_at = COALESCE(started_at, CASE WHEN $3::varchar = 'PREPARING' THEN now() END),
			completed_at = CASE WHEN $6 THEN now() ELSE completed_at END
		WHERE tenant_id = $1 AND run_id = $2
		RETURNING `+runColumns, tenantID, runID, string(p.ToLifecycle), string(newResult), string(newCert), terminal), &out); err != nil {
		return nil, err
	}

	dims := []struct{ name, from, to string }{
		{"LIFECYCLE", string(cur.LifecycleState), string(out.LifecycleState)},
		{"RESULT", string(cur.ResultState), string(out.ResultState)},
		{"CERTIFICATION", string(cur.CertificationState), string(out.CertificationState)},
	}
	for _, d := range dims {
		if d.from == d.to {
			continue
		}
		if err := insertTransition(ctx, tx, tenantID, runID, d.name, d.from, d.to, p.Reason, p.Actor, p.CorrelationID); err != nil {
			return nil, err
		}
	}
	for _, et := range EventsFor(cur, out) {
		if err := emit(ctx, tx, out, et, p.Actor, p.CorrelationID, map[string]any{
			"run_id": out.RunID, "control_definition_id": out.ControlDefinitionID,
			"lifecycle_state": out.LifecycleState, "result_state": out.ResultState,
			"certification_state": out.CertificationState, "version": out.Version, "reason": p.Reason,
		}); err != nil {
			return nil, err
		}
	}
	return &out, nil
}

func insertTransition(ctx context.Context, tx pgx.Tx, tenantID, runID, dimension, from, to, reason, actor, correlationID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO control_run_transitions
		(tenant_id, run_id, dimension, from_state, to_state, reason, actor_id, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, tenantID, runID, dimension, from, to, reason, actor, correlationID)
	return err
}

func emit(ctx context.Context, tx pgx.Tx, r domain.ControlRun, eventType, actor, correlationID string, payload map[string]any) error {
	payload["occurred_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	a, c := actor, correlationID
	return outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: "control_run", AggregateID: r.RunID, EventType: eventType,
		TenantID: r.TenantID, LegalEntityID: r.LegalEntityID, ActorID: &a, CorrelationID: &c, Payload: payload,
	})
}
