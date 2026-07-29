// Package store provides the PostgreSQL implementation of
// evidence-requirements-svc's persistence layer.
//
// Two things in here are deliberate reactions to live platform defects, and
// changing either of them will reintroduce a real bug:
//
//  1. Tenant scope is set with SELECT set_config('app.tenant_id', $1, true),
//     NOT with SET LOCAL app.tenant_id = $1. Postgres does not accept bind
//     parameters in SET, so the latter raises a syntax error; because the
//     error is returned and checked, every enclosing write aborts. Twelve
//     services currently ship that form (all ten Phase 5 services plus
//     offboarding-severance-svc and workforce-compliance-svc) and their DB
//     layers cannot write at all.
//
//  2. Every method ALSO filters explicitly by tenant_id in its own SQL
//     rather than relying on the Row-Level Security policy alone. The
//     policies in the migration are real and correctly written, but this
//     pool connects as a Postgres superuser (DB_USER=postgres, same as every
//     other service here), and superusers unconditionally bypass RLS
//     regardless of policy. RLS is defense-in-depth; the explicit filter is
//     the actual guarantee. general-ledger-svc and tenant-entity-registry-svc
//     both learned this through genuine CI failures.
//
// internal/store/tenant_isolation_test.go proves both hold.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func tenantFromCtxOrFallback(ctx context.Context, fallback string) string {
	if t := svcmiddleware.TenantFromContext(ctx); t != "" {
		return t
	}
	return fallback
}

// requirementColumns is the shared SELECT list, kept in one place so the
// scan order below can never drift from it.
const requirementColumns = `
	evidence_requirement_id, tenant_id, legal_entity_id, domain_code, action_type,
	evidence_type, requirement_payload, effective_from, effective_to,
	created_at, created_by_principal_id, correlation_id`

func scanRequirement(row pgx.Row) (*domain.EvidenceRequirement, error) {
	var r domain.EvidenceRequirement
	var payload []byte
	if err := row.Scan(
		&r.EvidenceRequirementID, &r.TenantID, &r.LegalEntityID, &r.DomainCode, &r.ActionType,
		&r.EvidenceType, &payload, &r.EffectiveFrom, &r.EffectiveTo,
		&r.CreatedAt, &r.CreatedByPrincipalID, &r.CorrelationID,
	); err != nil {
		return nil, err
	}
	r.RequirementPayload = json.RawMessage(payload)
	return &r, nil
}

// CreateRequirement inserts an evidence requirement. Idempotent on
// (tenant_id, correlation_id): if a row already exists for that pair, r is
// overwritten with the EXISTING row's values and created=false is returned.
//
// The unique constraint is real and enforced by the database — most
// Finance/Workforce/Legal services on this platform store correlation_id
// without constraining it, so their retries mint duplicates.
func (s *PgStore) CreateRequirement(ctx context.Context, r *domain.EvidenceRequirement) (created bool, err error) {
	tenantID := tenantFromCtxOrFallback(ctx, r.TenantID)

	payload := r.RequirementPayload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			INSERT INTO evidence_requirements (
				evidence_requirement_id, tenant_id, legal_entity_id, domain_code, action_type,
				evidence_type, requirement_payload, effective_from, effective_to,
				created_at, created_by_principal_id, correlation_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULL,$9,$10,$11)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, r.EvidenceRequirementID, r.TenantID, r.LegalEntityID, r.DomainCode, r.ActionType,
			r.EvidenceType, []byte(payload), r.EffectiveFrom,
			now, r.CreatedByPrincipalID, r.CorrelationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			r.RequirementPayload = payload
			r.CreatedAt = now
			r.EffectiveTo = nil
			return nil
		}

		// Conflict: a requirement for this (tenant_id, correlation_id)
		// already exists — this is a retry. Return the existing row as-is so
		// the handler can respond idempotently.
		existing, err := scanRequirement(tx.QueryRow(ctx,
			`SELECT `+requirementColumns+`
			 FROM evidence_requirements WHERE tenant_id = $1 AND correlation_id = $2`,
			r.TenantID, r.CorrelationID))
		if err != nil {
			return err
		}
		*r = *existing
		created = false
		return nil
	})
	return created, err
}

// GetRequirement returns (nil, nil) if not found — including when the
// caller's tenant scope doesn't match the requirement's tenant (explicit
// filter, not RLS-only — see package doc).
func (s *PgStore) GetRequirement(ctx context.Context, requirementID string) (*domain.EvidenceRequirement, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

	var out *domain.EvidenceRequirement
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		r, err := scanRequirement(tx.QueryRow(ctx,
			`SELECT `+requirementColumns+`
			 FROM evidence_requirements
			 WHERE evidence_requirement_id = $1 AND tenant_id = $2`,
			requirementID, tenantID))
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListRequirements returns catalog rows matching filter. TenantID is
// required; the rest are optional. A zero AsOf means no effective-date
// filter at all — retired requirements are included, which is what an
// auditor reviewing history needs.
func (s *PgStore) ListRequirements(ctx context.Context, filter domain.ListRequirementsFilter) ([]domain.EvidenceRequirement, error) {
	var asOf *time.Time
	if !filter.AsOf.IsZero() {
		t := filter.AsOf
		asOf = &t
	}

	var out []domain.EvidenceRequirement
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+requirementColumns+`
			FROM evidence_requirements
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR domain_code = $3)
			  AND ($4 = '' OR action_type = $4)
			  AND ($5::timestamptz IS NULL OR (
			        effective_from <= $5 AND (effective_to IS NULL OR effective_to > $5)))
			ORDER BY domain_code, action_type, evidence_type, effective_from DESC
		`, filter.TenantID, filter.LegalEntityID, filter.DomainCode, filter.ActionType, asOf)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRequirement(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// EffectiveRequirements returns the requirements in force for one action at
// asOf: those scoped to legalEntityID, plus those with a NULL
// legal_entity_id (tenant-wide). This is the read the evaluator depends on.
func (s *PgStore) EffectiveRequirements(ctx context.Context, tenantID, legalEntityID, domainCode, actionType string, asOf time.Time) ([]domain.EvidenceRequirement, error) {
	var out []domain.EvidenceRequirement
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+requirementColumns+`
			FROM evidence_requirements
			WHERE tenant_id = $1
			  AND domain_code = $2
			  AND action_type = $3
			  AND (legal_entity_id IS NULL OR legal_entity_id::text = $4)
			  AND effective_from <= $5
			  AND (effective_to IS NULL OR effective_to > $5)
			ORDER BY evidence_type
		`, tenantID, domainCode, actionType, legalEntityID, asOf)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRequirement(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// EndDateRequirement retires a requirement by setting effective_to. It never
// deletes and never sets a flag (doctrine: no soft-delete on material
// objects; effective end-dating only).
//
// The UPDATE carries its own guard — WHERE effective_to IS NULL AND
// tenant_id = $N — so the state check, the transition, and the tenant scope
// are one atomic statement. Returns domain.ErrAlreadyRetired if the row
// exists but is already retired, and domain.ErrRequirementNotFound if it
// does not exist within this tenant. Never a silent no-op:
// invoice-approval-svc's non-atomic read-then-write is the anti-pattern
// here, and it can double-apply under concurrency.
func (s *PgStore) EndDateRequirement(ctx context.Context, tenantID, requirementID string, effectiveTo time.Time, reason, actorPrincipalID string) (*domain.EvidenceRequirement, error) {
	var out *domain.EvidenceRequirement
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		r, err := scanRequirement(tx.QueryRow(ctx, `
			UPDATE evidence_requirements
			SET effective_to = $1, retired_by_principal_id = $2, retired_reason = $3
			WHERE evidence_requirement_id = $4 AND tenant_id = $5 AND effective_to IS NULL
			RETURNING `+requirementColumns,
			effectiveTo, actorPrincipalID, reason, requirementID, tenantID))
		if err == nil {
			out = r
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// Zero rows updated. Distinguish "already retired" from "not found
		// in this tenant" so the caller gets a truthful error instead of a
		// generic one. Same transaction, so the answer is consistent with
		// the UPDATE that just ran.
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM evidence_requirements
				WHERE evidence_requirement_id = $1 AND tenant_id = $2)
		`, requirementID, tenantID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return domain.ErrAlreadyRetired
		}
		return domain.ErrRequirementNotFound
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordEvaluation appends an evaluation record. Idempotent on
// (tenant_id, correlation_id): a replayed evaluation returns the ORIGINAL
// determination with created=false, and the handler must not republish its
// Kafka event in that case.
func (s *PgStore) RecordEvaluation(ctx context.Context, e *domain.EvidenceEvaluation) (created bool, err error) {
	tenantID := tenantFromCtxOrFallback(ctx, e.TenantID)

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			INSERT INTO evidence_evaluations (
				evaluation_id, tenant_id, legal_entity_id, domain_code, action_type,
				outcome, unmet_payload, present_artifacts_payload,
				evaluated_at, evaluated_for_principal_id, correlation_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, e.EvaluationID, e.TenantID, e.LegalEntityID, e.DomainCode, e.ActionType,
			string(e.Outcome), []byte(e.UnmetPayload), []byte(e.PresentArtifactsPayload),
			now, e.EvaluatedForPrincipalID, e.CorrelationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			e.EvaluatedAt = now
			return nil
		}

		existing, err := scanEvaluation(tx.QueryRow(ctx,
			`SELECT `+evaluationColumns+`
			 FROM evidence_evaluations WHERE tenant_id = $1 AND correlation_id = $2`,
			e.TenantID, e.CorrelationID))
		if err != nil {
			return err
		}
		*e = *existing
		created = false
		return nil
	})
	return created, err
}

const evaluationColumns = `
	evaluation_id, tenant_id, legal_entity_id, domain_code, action_type,
	outcome, unmet_payload, present_artifacts_payload,
	evaluated_at, evaluated_for_principal_id, correlation_id`

func scanEvaluation(row pgx.Row) (*domain.EvidenceEvaluation, error) {
	var e domain.EvidenceEvaluation
	var outcome string
	var unmet, present []byte
	if err := row.Scan(
		&e.EvaluationID, &e.TenantID, &e.LegalEntityID, &e.DomainCode, &e.ActionType,
		&outcome, &unmet, &present,
		&e.EvaluatedAt, &e.EvaluatedForPrincipalID, &e.CorrelationID,
	); err != nil {
		return nil, err
	}
	e.Outcome = domain.Outcome(outcome)
	e.UnmetPayload = json.RawMessage(unmet)
	e.PresentArtifactsPayload = json.RawMessage(present)
	return &e, nil
}

// GetEvaluation returns (nil, nil) if not found within the caller's tenant
// scope. Past determinations are retrievable because the determination is
// itself evidence (03-microservices.md §17.6).
func (s *PgStore) GetEvaluation(ctx context.Context, evaluationID string) (*domain.EvidenceEvaluation, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

	var out *domain.EvidenceEvaluation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		e, err := scanEvaluation(tx.QueryRow(ctx,
			`SELECT `+evaluationColumns+`
			 FROM evidence_evaluations
			 WHERE evaluation_id = $1 AND tenant_id = $2`,
			evaluationID, tenantID))
		if err != nil {
			return err
		}
		out = e
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}
