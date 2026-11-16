package store

// Console registers (API-4) and the KPI summary. Every query runs inside the
// caller's tenant scope (withRLS), so row-level security, not these WHERE
// clauses, decides what a tenant can see; the filters only narrow it.
// Ordering is newest first with the id as the tie-break, which keeps an
// offset cursor stable between pages.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/policy-svc/internal/domain"
)

// where accumulates "column = $n" conditions and their arguments.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, arg any) {
	w.args = append(w.args, arg)
	w.conds = append(w.conds, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(w.args))))
}

func (w *where) addRaw(cond string) { w.conds = append(w.conds, cond) }

func (w *where) eq(column, value string) {
	if value != "" {
		w.add(column+" = ?", value)
	}
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// page appends LIMIT and OFFSET and returns the clause.
func (w *where) page(q domain.ListQuery) string {
	w.args = append(w.args, q.Limit)
	limit := "$" + strconv.Itoa(len(w.args))
	w.args = append(w.args, q.Offset)
	return " LIMIT " + limit + " OFFSET $" + strconv.Itoa(len(w.args))
}

func listRows[T any](ctx context.Context, s *PgStore, op, sql string, args []any, scan func(pgx.Row) (T, error)) ([]T, error) {
	var out []T
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scan(rows)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.domainError(op, err)
	}
	return out, nil
}

// ListPolicyFamilies is the policy register: the platform families and the
// caller tenant's own. Filters: policy_type, scope (TENANT | GLOBAL),
// policy_status.
func (s *PgStore) ListPolicyFamilies(ctx context.Context, q domain.ListQuery) ([]*domain.Policy, error) {
	w := &where{}
	w.eq("policy_type", q.Filter("policy_type"))
	w.eq("policy_status", q.Filter("policy_status"))
	switch q.Filter("scope") {
	case domain.ScopeTypeTenant:
		w.addRaw("tenant_id IS NOT NULL")
	case domain.ScopeTypeGlobal:
		w.addRaw("tenant_id IS NULL")
	}
	sql := `SELECT ` + policyColumns + ` FROM policies` + w.sql() + ` ORDER BY created_at DESC, policy_id DESC`
	sql += w.page(q)
	return listRows(ctx, s, "ListPolicyFamilies", sql, w.args, scanPolicy)
}

// ListPolicyVersions is the version register across every visible family.
// Filters: policy_id, policy_type, version_status, legal_entity_id.
func (s *PgStore) ListPolicyVersions(ctx context.Context, q domain.ListQuery) ([]*domain.PolicyVersion, error) {
	w := &where{}
	w.eq("v.policy_id", q.Filter("policy_id"))
	w.eq("p.policy_type", q.Filter("policy_type"))
	w.eq("v.version_status", q.Filter("version_status"))
	w.eq("v.legal_entity_id", q.Filter("legal_entity_id"))
	sql := `SELECT ` + versionColumns("v") + ` FROM policy_versions v JOIN policies p ON p.policy_id = v.policy_id` +
		w.sql() + ` ORDER BY v.created_at DESC, v.policy_version_id DESC`
	sql += w.page(q)
	return listRows(ctx, s, "ListPolicyVersions", sql, w.args, func(row pgx.Row) (*domain.PolicyVersion, error) {
		return scanPolicyVersion(row)
	})
}

// ListEvaluations is the caller tenant's evaluation evidence register.
// Filters: policy_type, result, legal_entity_id, decision_id, policy_version_id.
func (s *PgStore) ListEvaluations(ctx context.Context, q domain.ListQuery) ([]*domain.Evaluation, error) {
	w := &where{}
	w.add("e.tenant_id = ?::uuid", contextTenantID(ctx))
	w.eq("e.policy_type", q.Filter("policy_type"))
	w.eq("e.result", q.Filter("result"))
	w.eq("e.legal_entity_id", q.Filter("legal_entity_id"))
	w.eq("e.decision_id", q.Filter("decision_id"))
	w.eq("e.policy_version_id", q.Filter("policy_version_id"))
	sql := `SELECT ` + evaluationColumns + w.sql() + ` ORDER BY e.evaluated_at DESC, e.evaluation_id DESC`
	sql += w.page(q)
	return listRows(ctx, s, "ListEvaluations", sql, w.args, scanEvaluation)
}

// ListControlTestDefinitions is the caller tenant's control-test register.
// Filters: control_ref, design_status.
func (s *PgStore) ListControlTestDefinitions(ctx context.Context, q domain.ListQuery) ([]*domain.ControlTestDefinition, error) {
	w := &where{}
	w.eq("control_ref", q.Filter("control_ref"))
	w.eq("design_status", q.Filter("design_status"))
	sql := `SELECT ` + controlTestDefinitionColumns + ` FROM control_test_definitions` + w.sql() +
		` ORDER BY created_at DESC, control_test_definition_id DESC`
	sql += w.page(q)
	return listRows(ctx, s, "ListControlTestDefinitions", sql, w.args, scanControlTestDefinition)
}

// ListAttestations is the caller tenant's attestation register.
// Filters: subject_ref, attestation_status, signer_principal_id.
func (s *PgStore) ListAttestations(ctx context.Context, q domain.ListQuery) ([]*domain.Attestation, error) {
	w := &where{}
	w.eq("subject_ref", q.Filter("subject_ref"))
	w.eq("attestation_status", q.Filter("attestation_status"))
	w.eq("signer_principal_id", q.Filter("signer_principal_id"))
	sql := `SELECT ` + attestationColumns + ` FROM attestations` + w.sql() +
		` ORDER BY created_at DESC, attestation_id DESC`
	sql += w.page(q)
	return listRows(ctx, s, "ListAttestations", sql, w.args, scanAttestation)
}

// PolicySummary counts the console's KPI row in one read-only transaction.
func (s *PgStore) PolicySummary(ctx context.Context) (*domain.PolicySummary, error) {
	tenantID := contextTenantID(ctx)
	sum := &domain.PolicySummary{TenantID: tenantID, AsOf: s.now()}
	since := sum.AsOf.Add(-24 * time.Hour)
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM policies;`).Scan(&sum.PolicyFamilies); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE version_status = 'DRAFT'),
				COUNT(*) FILTER (WHERE version_status = 'REVIEW'),
				COUNT(*) FILTER (WHERE version_status = 'APPROVED'),
				COUNT(*) FILTER (WHERE version_status = 'SCHEDULED'),
				COUNT(*) FILTER (WHERE version_status = 'ACTIVE'),
				COUNT(*) FILTER (WHERE version_status = 'SUSPENDED'),
				COUNT(*) FILTER (WHERE version_status = 'SUSPENDED' AND suspension_reviewed_at IS NULL)
			FROM policy_versions;`).Scan(&sum.VersionsDraft, &sum.VersionsInReview, &sum.VersionsApproved,
			&sum.VersionsScheduled, &sum.VersionsActive, &sum.VersionsSuspended, &sum.SuspensionsAwaitReview); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE evaluated_at >= $2),
				COUNT(*) FILTER (WHERE evaluated_at >= $2 AND result = 'APPROVAL_REQUIRED')
			FROM policy_evaluations WHERE tenant_id = $1::uuid;`, tenantID, since).
			Scan(&sum.Evaluations24h, &sum.EvaluationsApproval24h); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE delivered_at IS NULL AND failed_at IS NULL),
				COUNT(*) FILTER (WHERE failed_at IS NOT NULL)
			FROM policy_evidence_outbox WHERE tenant_id = $1::uuid;`, tenantID).
			Scan(&sum.EvidencePending, &sum.EvidenceFailed); err != nil {
			return err
		}
		// A control test is failing when its latest execution did not find
		// the control effective.
		if err := tx.QueryRow(ctx, `
			SELECT
				(SELECT COUNT(*) FROM control_test_definitions),
				(SELECT COUNT(*) FROM (
					SELECT DISTINCT ON (control_test_definition_id) result
					FROM control_test_executions
					ORDER BY control_test_definition_id, period_end DESC, created_at DESC
				) latest WHERE latest.result <> 'EFFECTIVE');`).
			Scan(&sum.ControlTestDefinitions, &sum.ControlTestsFailing); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE attestation_status = 'ACTIVE'),
				COUNT(*) FILTER (WHERE attestation_status = 'REVOKED')
			FROM attestations;`).Scan(&sum.AttestationsActive, &sum.AttestationsRevoked)
	})
	if err != nil {
		return nil, s.domainError("PolicySummary", err)
	}
	return sum, nil
}
