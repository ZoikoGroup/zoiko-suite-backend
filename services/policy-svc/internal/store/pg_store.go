// Package store provides the PostgreSQL implementation of the policy
// read and write model.
//
// This package is the ONLY layer that touches the database directly.
// No SQL appears in handlers or domain packages.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/domain"
	svcmiddleware "zoiko.io/policy-svc/internal/middleware"
)

// nilScopeUUID is the sentinel used in COALESCE() to make uniqueness
// constraints work across nullable tenant_id/legal_entity_id columns.
const nilScopeUUID = "00000000-0000-0000-0000-000000000000"

// SchedulerActor is recorded as the actor of a transition the activation
// scheduler performs at a version's effective time.
const SchedulerActor = "system:policy-scheduler"

// contextTenantID returns the request's tenant scope, but only if it parses
// as a UUID: handing a non-UUID to a uuid comparison dies in the driver
// rather than reading as an empty scope.
func contextTenantID(ctx context.Context) string {
	if t := svcmiddleware.TenantFromContext(ctx); t != "" {
		if _, err := uuid.Parse(t); err == nil {
			return t
		}
	}
	return ""
}

// derefOrEmpty turns a nullable tenant into withRLS's argument: nil (global
// scope) becomes "", which only ever satisfies a policy's "tenant_id IS NULL"
// branch and never grants access to a real tenant's row.
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// withRLS runs fn inside a transaction with app.tenant_id set for the
// transaction, so every table's tenant_isolation_policy has a value to
// enforce against.
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

// isUniqueViolation reports whether err is a Postgres unique_violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// domainError passes the domain's own sentinel errors through and wraps
// anything else as ErrStoreUnavailable.
func (s *PgStore) domainError(op string, err error) error {
	for _, known := range []error{
		domain.ErrPolicyNotFound, domain.ErrPolicyVersionNotFound, domain.ErrInvalidTransition,
		domain.ErrConflict, domain.ErrApprovalRequired, domain.ErrSoDConflict,
		domain.ErrConcurrencyConflict, domain.ErrPolicyConflicted, domain.ErrIdempotencyMismatch,
		domain.ErrInvalidScope, domain.ErrEvaluationNotFound,
		domain.ErrControlTestDefinitionNotFound, domain.ErrAttestationNotFound,
		domain.ErrInvalidAttestationTransition,
	} {
		if errors.Is(err, known) {
			return err
		}
	}
	s.log.Error("pg "+op+" failed", zap.Error(err))
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}

// PgStore implements the policy store against a PostgreSQL cluster.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger

	// now is the single clock for lifecycle timestamps. Resolution compares
	// them with the request's knowledge time, which the handler takes from
	// the same process clock, so a version activated a moment ago is never
	// "not yet known" because the database's clock ran ahead.
	now func() time.Time
}

// New returns an open PgStore. Caller must call pool.Close() when done.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// ── policies ─────────────────────────────────────────────────────────────────

const policyColumns = `
	policy_id,
	policy_code,
	policy_name,
	policy_type,
	tenant_id,
	policy_status,
	versioning_mode,
	created_at,
	created_by_principal_id`

func scanPolicy(row pgx.Row) (*domain.Policy, error) {
	p := &domain.Policy{}
	err := row.Scan(
		&p.PolicyID,
		&p.PolicyCode,
		&p.PolicyName,
		&p.PolicyType,
		&p.TenantID,
		&p.PolicyStatus,
		&p.VersioningMode,
		&p.CreatedAt,
		&p.CreatedByPrincipalID,
	)
	return p, err
}

// FindPolicyByID looks up a policy family visible to the caller's tenant: a
// platform family, or one the tenant owns.
func (s *PgStore) FindPolicyByID(ctx context.Context, policyID string) (*domain.Policy, error) {
	var p *domain.Policy
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		var scanErr error
		p, scanErr = scanPolicy(tx.QueryRow(ctx, `SELECT `+policyColumns+` FROM policies WHERE policy_id = $1;`, policyID))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPolicyNotFound
	}
	if err != nil {
		return nil, s.domainError("FindPolicyByID", err)
	}
	return p, nil
}

// CreatePolicy inserts a policy family, or replays the existing one when the
// same owner already has that policy_code with identical attributes.
// policy.created is enqueued in the same transaction.
func (s *PgStore) CreatePolicy(ctx context.Context, params domain.CreatePolicyParams) (*domain.Policy, bool, error) {
	callerSuppliedID := params.PolicyID != ""
	if !callerSuppliedID {
		params.PolicyID = uuid.New().String()
	}

	var result *domain.Policy
	var created bool
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		p, err := scanPolicy(tx.QueryRow(ctx, `
			INSERT INTO policies (policy_id, policy_code, policy_name, policy_type, tenant_id, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT DO NOTHING
			RETURNING `+policyColumns+`;`,
			params.PolicyID, params.PolicyCode, params.PolicyName, params.PolicyType, params.TenantID, params.CreatedByPrincipalID,
		))
		if err == nil {
			result, created = p, true
			return s.enqueue(ctx, tx, policyEvent("policy.created", p, params.Meta))
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		existing, err := scanPolicy(tx.QueryRow(ctx, `
			SELECT `+policyColumns+` FROM policies
			WHERE COALESCE(tenant_id, '`+nilScopeUUID+`'::UUID) = COALESCE($1::uuid, '`+nilScopeUUID+`'::UUID)
			  AND policy_code = $2;`, params.TenantID, params.PolicyCode))
		if errors.Is(err, pgx.ErrNoRows) {
			// The conflict was on a caller-supplied policy_id that belongs to
			// some other family.
			return domain.ErrConflict
		}
		if err != nil {
			return err
		}
		if existing.PolicyName != params.PolicyName || existing.PolicyType != params.PolicyType ||
			(callerSuppliedID && existing.PolicyID != params.PolicyID) {
			return domain.ErrConflict
		}
		result = existing
		return nil
	})
	if err != nil {
		return nil, false, s.domainError("CreatePolicy", err)
	}
	return result, created, nil
}

// ── policy_versions ──────────────────────────────────────────────────────────

// versionColumns is the SELECT list for policy_versions, optionally qualified
// with a table alias. Order must match scanVersion.
func versionColumns(alias string) string {
	a := ""
	if alias != "" {
		a = alias + "."
	}
	return a + `policy_version_id, ` + a + `policy_id, ` + a + `tenant_id, ` + a + `legal_entity_id, ` +
		a + `rule_payload, ` + a + `effective_from, ` + a + `effective_to, ` + a + `version_status, ` +
		a + `version_number, ` + a + `source, COALESCE(` + a + `rationale, ''), ` + a + `artifact_digest, ` +
		a + `known_from, ` + a + `activated_by_principal_id, ` + a + `activated_at, ` + a + `row_version, ` +
		a + `submitted_by_principal_id, ` + a + `submitted_at, ` + a + `approved_by_principal_id, ` +
		a + `approved_at, ` + a + `approval_reference, ` + a + `withdrawn_at, ` + a + `superseded_at, ` +
		a + `superseded_by_version_id, ` + a + `suspended_by_principal_id, ` +
		a + `suspension_reviewed_by_principal_id, ` + a + `suspension_reviewed_at, ` +
		a + `suspension_review_outcome, ` + a + `created_at, ` + a + `created_by_principal_id`
}

var policyVersionColumns = versionColumns("")

func versionDest(v *domain.PolicyVersion) []any {
	return []any{
		&v.PolicyVersionID, &v.PolicyID, &v.TenantID, &v.LegalEntityID,
		&v.RulePayload, &v.EffectiveFrom, &v.EffectiveTo, &v.VersionStatus,
		&v.VersionNumber, &v.Source, &v.Rationale, &v.ArtifactDigest,
		&v.KnownFrom, &v.ActivatedByPrincipalID, &v.ActivatedAt, &v.RowVersion,
		&v.SubmittedByPrincipalID, &v.SubmittedAt, &v.ApprovedByPrincipalID,
		&v.ApprovedAt, &v.ApprovalReference, &v.WithdrawnAt, &v.SupersededAt,
		&v.SupersededByVersionID, &v.SuspendedByPrincipalID, &v.SuspensionReviewedByPrincipalID,
		&v.SuspensionReviewedAt, &v.SuspensionReviewOutcome, &v.CreatedAt, &v.CreatedByPrincipalID,
	}
}

// scanPolicyVersion scans one policyVersionColumns row, plus any extra
// trailing columns into extra.
func scanPolicyVersion(row pgx.Row, extra ...any) (*domain.PolicyVersion, error) {
	v := &domain.PolicyVersion{}
	if err := row.Scan(append(versionDest(v), extra...)...); err != nil {
		return nil, err
	}
	v.ScopeType = domain.DeriveScopeType(v.TenantID, v.LegalEntityID)
	return v, nil
}

// FindPolicyVersionByID looks up a version visible to the caller's tenant:
// global, or the tenant's own. An unusable scope sees global versions only.
func (s *PgStore) FindPolicyVersionByID(ctx context.Context, policyVersionID string) (*domain.PolicyVersion, error) {
	tenantID := contextTenantID(ctx)
	var tenantParam *string
	if tenantID != "" {
		tenantParam = &tenantID
	}
	var v *domain.PolicyVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		v, scanErr = scanPolicyVersion(tx.QueryRow(ctx, `
			SELECT `+policyVersionColumns+`
			FROM policy_versions
			WHERE policy_version_id = $1
			  AND (tenant_id IS NULL OR tenant_id = $2::uuid);`, policyVersionID, tenantParam))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPolicyVersionNotFound
	}
	if err != nil {
		return nil, s.domainError("FindPolicyVersionByID", err)
	}
	return v, nil
}

// sameVersionContent reports whether a stored version is what params would
// have created — the test for replaying an idempotent create.
func sameVersionContent(v *domain.PolicyVersion, p domain.CreatePolicyVersionParams) bool {
	eqPtr := func(a, b *string) bool { return derefOrEmpty(a) == derefOrEmpty(b) }
	eqTime := func(a, b *time.Time) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return a.Equal(*b)
	}
	return v.PolicyID == p.PolicyID &&
		eqPtr(v.TenantID, p.TenantID) &&
		eqPtr(v.LegalEntityID, p.LegalEntityID) &&
		jsonEqual(v.RulePayload, p.RulePayload) &&
		v.EffectiveFrom.Equal(p.EffectiveFrom) &&
		eqTime(v.EffectiveTo, p.EffectiveTo) &&
		v.Source == p.Source &&
		v.Rationale == p.Rationale
}

// CreatePolicyVersion inserts a new DRAFT version, numbered next in its
// policy and tenant scope. A retry carrying the same Idempotency-Key (or the
// same caller-supplied policy_version_id) and the same content replays the
// stored version; different content is ErrIdempotencyMismatch.
//
// A tenant-owned family only takes versions in its owner's scope; a platform
// family takes global versions and tenant-scoped overrides.
func (s *PgStore) CreatePolicyVersion(ctx context.Context, params domain.CreatePolicyVersionParams) (*domain.PolicyVersion, bool, error) {
	if params.PolicyVersionID == "" {
		params.PolicyVersionID = uuid.New().String()
	}
	if len(params.RulePayload) == 0 {
		params.RulePayload = []byte(`{}`)
	}
	if params.Source == "" {
		params.Source = "internal"
	}
	var requestKey *string
	if params.RequestKey != "" {
		requestKey = &params.RequestKey
	}

	var result *domain.PolicyVersion
	var created bool
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		// FOR UPDATE serialises version numbering per family.
		family, err := scanPolicy(tx.QueryRow(ctx, `SELECT `+policyColumns+` FROM policies WHERE policy_id = $1 FOR UPDATE;`, params.PolicyID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPolicyNotFound
		}
		if err != nil {
			return err
		}
		if family.TenantID != nil && derefOrEmpty(params.TenantID) != *family.TenantID {
			return domain.ErrInvalidScope
		}

		replay := func(existing *domain.PolicyVersion) error {
			if !sameVersionContent(existing, params) {
				return domain.ErrIdempotencyMismatch
			}
			result = existing
			return nil
		}
		if requestKey != nil {
			existing, err := scanPolicyVersion(tx.QueryRow(ctx, `
				SELECT `+policyVersionColumns+` FROM policy_versions
				WHERE policy_id = $1 AND create_request_key = $2;`, params.PolicyID, params.RequestKey))
			if err == nil {
				return replay(existing)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		existing, err := scanPolicyVersion(tx.QueryRow(ctx, `
			SELECT `+policyVersionColumns+` FROM policy_versions WHERE policy_version_id = $1;`, params.PolicyVersionID))
		if err == nil {
			return replay(existing)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var next int
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(version_number), 0) + 1 FROM policy_versions
			WHERE policy_id = $1
			  AND COALESCE(tenant_id, '`+nilScopeUUID+`'::UUID) = COALESCE($2::uuid, '`+nilScopeUUID+`'::UUID);`,
			params.PolicyID, params.TenantID).Scan(&next); err != nil {
			return err
		}

		// artifact_digest is derived by trigger from the stored payload; the
		// placeholder here never survives the insert.
		v, err := scanPolicyVersion(tx.QueryRow(ctx, `
			INSERT INTO policy_versions (
				policy_version_id, policy_id, tenant_id, legal_entity_id, rule_payload,
				effective_from, effective_to, version_status, version_number, source, rationale,
				artifact_digest, known_from, created_by_principal_id, scope_type, create_request_key
			) VALUES ($1, $2, $3, $4, $5, $6, $7, 'DRAFT', $8, $9, $10, 'pending', $11, $12, $13, $14)
			RETURNING `+policyVersionColumns+`;`,
			params.PolicyVersionID, params.PolicyID, params.TenantID, params.LegalEntityID, params.RulePayload,
			params.EffectiveFrom, params.EffectiveTo, next, params.Source, nullableString(params.Rationale),
			s.now(), params.CreatedByPrincipalID, domain.DeriveScopeType(params.TenantID, params.LegalEntityID), requestKey,
		))
		if err != nil {
			if isUniqueViolation(err) {
				// A caller-supplied id already used in a scope this tenant
				// cannot see.
				return domain.ErrConflict
			}
			return err
		}
		if err := s.insertTransition(ctx, tx, v, nil, domain.StatusDraft, params.CreatedByPrincipalID, "", "", params.Meta.CorrelationID); err != nil {
			return err
		}
		result, created = v, true
		return s.enqueue(ctx, tx, versionEvent("policy.updated", v, params.CreatedByPrincipalID, params.Meta, nil))
	})
	if err != nil {
		return nil, false, s.domainError("CreatePolicyVersion", err)
	}
	return result, created, nil
}

// jsonEqual compares two JSON payloads structurally rather than byte-for-
// byte: JSONB re-serialises with its own whitespace and key order.
func jsonEqual(a, b []byte) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// ListVersionHistory returns every version of a policy visible to the
// caller's tenant (global versions plus that tenant's own), newest first.
func (s *PgStore) ListVersionHistory(ctx context.Context, policyID string) ([]*domain.PolicyVersion, error) {
	if _, err := s.FindPolicyByID(ctx, policyID); err != nil {
		return nil, err
	}

	tenantID := contextTenantID(ctx)
	var tenantParam *string
	if tenantID != "" {
		tenantParam = &tenantID
	}
	var results []*domain.PolicyVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+policyVersionColumns+`
			FROM policy_versions
			WHERE policy_id = $1
			  AND (tenant_id IS NULL OR tenant_id = $2::uuid)
			ORDER BY effective_from DESC, created_at DESC;`, policyID, tenantParam)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, scanErr := scanPolicyVersion(rows)
			if scanErr != nil {
				return scanErr
			}
			results = append(results, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.domainError("ListVersionHistory", err)
	}
	return results, nil
}

// ListTransitions returns a version's lifecycle history, oldest first.
func (s *PgStore) ListTransitions(ctx context.Context, policyVersionID string) ([]*domain.VersionTransition, error) {
	if _, err := s.FindPolicyVersionByID(ctx, policyVersionID); err != nil {
		return nil, err
	}
	var out []*domain.VersionTransition
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT transition_id, policy_version_id, from_status, to_status, actor_id,
			       reason, approval_reference, correlation_id, occurred_at
			FROM policy_version_transitions
			WHERE policy_version_id = $1
			ORDER BY transition_id;`, policyVersionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t := &domain.VersionTransition{}
			if err := rows.Scan(&t.TransitionID, &t.PolicyVersionID, &t.FromStatus, &t.ToStatus, &t.ActorID,
				&t.Reason, &t.ApprovalReference, &t.CorrelationID, &t.OccurredAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.domainError("ListTransitions", err)
	}
	return out, nil
}

// ── lifecycle (GOV-05 commands) ──────────────────────────────────────────────

// stamp holds the lifecycle columns one transition sets. A nil field leaves
// the column as it is.
type stamp struct {
	submittedBy, approvedBy, approvalRef, activatedBy *string
	submittedAt, approvedAt, activatedAt, withdrawnAt *time.Time
	suspendedBy, reviewedBy, reviewOutcome            *string
	reviewedAt                                        *time.Time
}

func (s *PgStore) setStatus(ctx context.Context, tx pgx.Tx, id, status string, st stamp) (*domain.PolicyVersion, error) {
	return scanPolicyVersion(tx.QueryRow(ctx, `
		UPDATE policy_versions SET
			version_status            = $2,
			row_version               = row_version + 1,
			submitted_by_principal_id = COALESCE($3::text, submitted_by_principal_id),
			submitted_at              = COALESCE($4::timestamptz, submitted_at),
			approved_by_principal_id  = COALESCE($5::text, approved_by_principal_id),
			approved_at               = COALESCE($6::timestamptz, approved_at),
			approval_reference        = COALESCE($7::text, approval_reference),
			activated_by_principal_id = COALESCE($8::text, activated_by_principal_id),
			activated_at              = COALESCE($9::timestamptz, activated_at),
			withdrawn_at              = COALESCE(withdrawn_at, $10::timestamptz),
			suspended_by_principal_id = COALESCE($11::text, suspended_by_principal_id),
			suspension_reviewed_by_principal_id = COALESCE($12::text, suspension_reviewed_by_principal_id),
			suspension_reviewed_at    = COALESCE($13::timestamptz, suspension_reviewed_at),
			suspension_review_outcome = COALESCE($14::text, suspension_review_outcome)
		WHERE policy_version_id = $1
		RETURNING `+policyVersionColumns+`;`,
		id, status, st.submittedBy, st.submittedAt, st.approvedBy, st.approvedAt, st.approvalRef,
		st.activatedBy, st.activatedAt, st.withdrawnAt, st.suspendedBy, st.reviewedBy, st.reviewedAt, st.reviewOutcome))
}

func (s *PgStore) insertTransition(ctx context.Context, tx pgx.Tx, v *domain.PolicyVersion, from *string, to, actor, reason, approvalRef, correlationID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO policy_version_transitions (
			policy_version_id, tenant_id, from_status, to_status, actor_id,
			reason, approval_reference, correlation_id, occurred_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);`,
		v.PolicyVersionID, v.TenantID, from, to, actor,
		nullableString(reason), nullableString(approvalRef), nullableString(correlationID), s.now())
	return err
}

// lockVersion reads a version and its family's policy type FOR UPDATE.
func lockVersion(ctx context.Context, tx pgx.Tx, id string) (*domain.PolicyVersion, string, error) {
	var policyType string
	v, err := scanPolicyVersion(tx.QueryRow(ctx, `
		SELECT `+versionColumns("pv")+`, p.policy_type
		FROM policy_versions pv
		JOIN policies p ON p.policy_id = pv.policy_id
		WHERE pv.policy_version_id = $1
		FOR UPDATE OF pv;`, id), &policyType)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", domain.ErrPolicyVersionNotFound
	}
	return v, policyType, err
}

// Transition applies one GOV-05 lifecycle command atomically: the status
// change, its side effects (supersession, scheduling), the transition log
// row and the outbox events commit together or not at all.
//
// A command that has already taken effect (activating an ACTIVE version,
// suspending a SUSPENDED one, ...) is an idempotent no-op with
// Transitioned=false, whatever expected_version says. Otherwise a supplied
// expected_version must equal the stored row_version.
func (s *PgStore) Transition(ctx context.Context, cmd domain.TransitionCommand) (*domain.TransitionResult, error) {
	var out *domain.TransitionResult
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		v, policyType, err := lockVersion(ctx, tx, cmd.PolicyVersionID)
		if err != nil {
			return err
		}
		out, err = s.apply(ctx, tx, v, policyType, cmd)
		return err
	})
	if err != nil {
		return nil, s.domainError("Transition", err)
	}
	return out, nil
}

func (s *PgStore) apply(ctx context.Context, tx pgx.Tx, v *domain.PolicyVersion, policyType string, cmd domain.TransitionCommand) (*domain.TransitionResult, error) {
	now := s.now()
	noop := &domain.TransitionResult{Version: v}
	expected := func() error {
		if cmd.ExpectedVersion != nil && *cmd.ExpectedVersion != v.RowVersion {
			return domain.ErrConcurrencyConflict
		}
		return nil
	}
	from := v.VersionStatus
	record := func(next *domain.PolicyVersion, eventType string, extra map[string]any) error {
		if err := s.insertTransition(ctx, tx, next, &from, next.VersionStatus, cmd.ActorID, cmd.Reason, cmd.ApprovalReference, cmd.Meta.CorrelationID); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, versionEvent(eventType, next, cmd.ActorID, cmd.Meta, extra))
	}

	switch cmd.Command {
	case domain.CommandSubmitReview:
		if from == domain.StatusReview {
			return noop, nil
		}
		if from != domain.StatusDraft {
			return nil, domain.ErrInvalidTransition
		}
		if err := expected(); err != nil {
			return nil, err
		}
		next, err := s.setStatus(ctx, tx, v.PolicyVersionID, domain.StatusReview, stamp{submittedBy: &cmd.ActorID, submittedAt: &now})
		if err != nil {
			return nil, err
		}
		return &domain.TransitionResult{Version: next, Transitioned: true}, record(next, "policy.version.submitted", nil)

	case domain.CommandApprove:
		if from == domain.StatusApproved {
			return noop, nil
		}
		if from != domain.StatusReview {
			return nil, domain.ErrInvalidTransition
		}
		// GOV-05 SoD: the author cannot be the approver.
		if cmd.ActorID == v.CreatedByPrincipalID {
			return nil, domain.ErrSoDConflict
		}
		if err := expected(); err != nil {
			return nil, err
		}
		st := stamp{approvedBy: &cmd.ActorID, approvedAt: &now}
		if cmd.ApprovalReference != "" {
			st.approvalRef = &cmd.ApprovalReference
		}
		next, err := s.setStatus(ctx, tx, v.PolicyVersionID, domain.StatusApproved, st)
		if err != nil {
			return nil, err
		}
		return &domain.TransitionResult{Version: next, Transitioned: true}, record(next, "policy.version.approved", nil)

	case domain.CommandActivate, domain.CommandSchedule:
		future := v.EffectiveFrom.After(now)
		switch {
		case from == domain.StatusActive:
			return noop, nil
		case from == domain.StatusScheduled && (cmd.Command == domain.CommandSchedule || future):
			return noop, nil
		case from == domain.StatusDraft || from == domain.StatusReview:
			return nil, domain.ErrApprovalRequired
		case from != domain.StatusApproved && from != domain.StatusScheduled:
			return nil, domain.ErrInvalidTransition
		}
		if cmd.Command == domain.CommandSchedule && !future {
			return nil, domain.ErrInvalidTransition
		}
		// The approver is independent of the author (APPROVE); the activator
		// must be too, so no single principal can put their own rule live.
		if cmd.ActorID == v.CreatedByPrincipalID {
			return nil, domain.ErrSoDConflict
		}
		if err := expected(); err != nil {
			return nil, err
		}
		if from == domain.StatusApproved {
			if err := s.guardActivation(ctx, tx, v, policyType); err != nil {
				return nil, err
			}
		}
		if from == domain.StatusApproved && future {
			next, err := s.setStatus(ctx, tx, v.PolicyVersionID, domain.StatusScheduled, stamp{activatedBy: &cmd.ActorID, activatedAt: &now})
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO policy_activation_schedule (policy_version_id, tenant_id, due_at, scheduled_at)
				VALUES ($1, $2, $3, $4);`, next.PolicyVersionID, next.TenantID, next.EffectiveFrom, now); err != nil {
				return nil, err
			}
			return &domain.TransitionResult{Version: next, Transitioned: true}, record(next, "policy.version.scheduled", nil)
		}
		return s.activateNow(ctx, tx, v, cmd.ActorID, cmd.Reason, cmd.Meta)

	case domain.CommandSuspend:
		if from == domain.StatusSuspended {
			return noop, nil
		}
		if from != domain.StatusActive && from != domain.StatusScheduled {
			return nil, domain.ErrInvalidTransition
		}
		if err := expected(); err != nil {
			return nil, err
		}
		next, err := s.setStatus(ctx, tx, v.PolicyVersionID, domain.StatusSuspended, stamp{withdrawnAt: &now, suspendedBy: &cmd.ActorID})
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM policy_activation_schedule WHERE policy_version_id = $1;`, v.PolicyVersionID); err != nil {
			return nil, err
		}
		return &domain.TransitionResult{Version: next, Transitioned: true},
			record(next, "policy.version.suspended", map[string]any{"reason": cmd.Reason})

	case domain.CommandReviewSuspension:
		if v.SuspensionReviewedAt != nil {
			return noop, nil
		}
		if from != domain.StatusSuspended {
			return nil, domain.ErrInvalidTransition
		}
		// The review is independent of the emergency action it reviews.
		if v.SuspendedByPrincipalID != nil && cmd.ActorID == *v.SuspendedByPrincipalID {
			return nil, domain.ErrSoDConflict
		}
		if err := expected(); err != nil {
			return nil, err
		}
		next, err := s.setStatus(ctx, tx, v.PolicyVersionID, domain.StatusSuspended,
			stamp{reviewedBy: &cmd.ActorID, reviewedAt: &now, reviewOutcome: &cmd.Outcome})
		if err != nil {
			return nil, err
		}
		cmd.Reason = "suspension review " + cmd.Outcome + ": " + cmd.Reason
		return &domain.TransitionResult{Version: next, Transitioned: true},
			record(next, "policy.version.suspension_reviewed", map[string]any{"outcome": cmd.Outcome})

	case domain.CommandRetire:
		if from == domain.StatusRetired {
			return noop, nil
		}
		if err := expected(); err != nil {
			return nil, err
		}
		st := stamp{}
		// Retiring a version that is (or was about to be) in force withdraws
		// it. A superseded version already stopped governing at its
		// successor's effective time; retiring it is archival and must not
		// change what historical replay resolves.
		if from == domain.StatusActive || from == domain.StatusScheduled || from == domain.StatusSuspended {
			st.withdrawnAt = &now
		}
		next, err := s.setStatus(ctx, tx, v.PolicyVersionID, domain.StatusRetired, st)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM policy_activation_schedule WHERE policy_version_id = $1;`, v.PolicyVersionID); err != nil {
			return nil, err
		}
		return &domain.TransitionResult{Version: next, Transitioned: true},
			record(next, "policy.rule.retired", map[string]any{"reason": cmd.Reason, "previous_status": from})
	}
	return nil, domain.ErrInvalidTransition
}

// guardActivation blocks an activation that would leave two distinct policies
// of one type governing the same scope over overlapping effective windows
// (GOV-05 negative path #2, GCP domain negative path #7), and one that would
// race a scheduled activation already pending in the version's own scope.
//
// The advisory lock serialises every activation of the policy type, so two
// concurrent activations cannot both pass the check.
func (s *PgStore) guardActivation(ctx context.Context, tx pgx.Tx, v *domain.PolicyVersion, policyType string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1));`, "policy-svc:policy_type:"+policyType); err != nil {
		return err
	}
	var conflicting string
	err := tx.QueryRow(ctx, `
		SELECT pv.policy_version_id
		FROM policy_versions pv
		JOIN policies p ON p.policy_id = pv.policy_id
		WHERE p.policy_type = $1
		  AND pv.policy_id <> $2
		  AND pv.tenant_id IS NOT DISTINCT FROM $3::uuid
		  AND pv.legal_entity_id IS NOT DISTINCT FROM $4::uuid
		  AND pv.version_status IN ('ACTIVE', 'SCHEDULED')
		  AND pv.withdrawn_at IS NULL
		  AND pv.effective_from < COALESCE($6::timestamptz, 'infinity'::timestamptz)
		  AND COALESCE(pv.effective_to, 'infinity'::timestamptz) > $5
		LIMIT 1;`,
		policyType, v.PolicyID, v.TenantID, v.LegalEntityID, v.EffectiveFrom, v.EffectiveTo).Scan(&conflicting)
	if err == nil {
		return fmt.Errorf("%w: version %s of another %s policy governs the same scope and period", domain.ErrPolicyConflicted, conflicting, policyType)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	var pending string
	err = tx.QueryRow(ctx, `
		SELECT policy_version_id FROM policy_versions
		WHERE policy_id = $1
		  AND tenant_id IS NOT DISTINCT FROM $2::uuid
		  AND legal_entity_id IS NOT DISTINCT FROM $3::uuid
		  AND version_status = 'SCHEDULED'
		  AND policy_version_id <> $4
		LIMIT 1;`, v.PolicyID, v.TenantID, v.LegalEntityID, v.PolicyVersionID).Scan(&pending)
	if err == nil {
		return fmt.Errorf("%w: version %s is already scheduled in this scope; suspend or retire it first", domain.ErrConcurrencyConflict, pending)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// activateNow makes v ACTIVE and supersedes the version it replaces in the
// same (policy, tenant, legal entity) scope. The predecessor stops governing
// at the instant v took over: v's effective_from, or the moment v's
// activation was recorded if that is later.
func (s *PgStore) activateNow(ctx context.Context, tx pgx.Tx, v *domain.PolicyVersion, actor, reason string, meta domain.EventMeta) (*domain.TransitionResult, error) {
	now := s.now()
	st := stamp{}
	if v.VersionStatus == domain.StatusApproved {
		st.activatedBy, st.activatedAt = &actor, &now
	}
	takeover := v.EffectiveFrom
	recorded := now
	if v.ActivatedAt != nil {
		recorded = *v.ActivatedAt
	}
	if recorded.After(takeover) {
		takeover = recorded
	}

	rows, err := tx.Query(ctx, `
		UPDATE policy_versions
		SET version_status = 'SUPERSEDED',
		    superseded_at = $5,
		    superseded_by_version_id = $4,
		    row_version = row_version + 1
		WHERE policy_id = $1
		  AND tenant_id IS NOT DISTINCT FROM $2::uuid
		  AND legal_entity_id IS NOT DISTINCT FROM $3::uuid
		  AND version_status = 'ACTIVE'
		  AND policy_version_id <> $4
		RETURNING `+policyVersionColumns+`;`,
		v.PolicyID, v.TenantID, v.LegalEntityID, v.PolicyVersionID, takeover)
	if err != nil {
		return nil, err
	}
	var superseded []*domain.PolicyVersion
	for rows.Next() {
		old, scanErr := scanPolicyVersion(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		superseded = append(superseded, old)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	from := v.VersionStatus
	next, err := s.setStatus(ctx, tx, v.PolicyVersionID, domain.StatusActive, st)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM policy_activation_schedule WHERE policy_version_id = $1;`, v.PolicyVersionID); err != nil {
		return nil, err
	}
	if err := s.insertTransition(ctx, tx, next, &from, domain.StatusActive, actor, reason, "", meta.CorrelationID); err != nil {
		return nil, err
	}
	if err := s.enqueue(ctx, tx, versionEvent("policy.version.activated", next, actor, meta, map[string]any{
		"superseded_version_ids": versionIDs(superseded),
	})); err != nil {
		return nil, err
	}
	active := domain.StatusActive
	for _, old := range superseded {
		if err := s.insertTransition(ctx, tx, old, &active, domain.StatusSuperseded, actor, "superseded by "+next.PolicyVersionID, "", meta.CorrelationID); err != nil {
			return nil, err
		}
		if err := s.enqueue(ctx, tx, versionEvent("policy.rule.retired", old, actor, meta, map[string]any{
			"superseded_by_version_id": next.PolicyVersionID,
			"previous_status":          domain.StatusActive,
		})); err != nil {
			return nil, err
		}
	}
	return &domain.TransitionResult{Version: next, Superseded: superseded, Transitioned: true}, nil
}

func versionIDs(vs []*domain.PolicyVersion) []string {
	ids := make([]string, 0, len(vs))
	for _, v := range vs {
		ids = append(ids, v.PolicyVersionID)
	}
	return ids
}

// PromoteDueActivations activates every scheduled version whose effective
// time has arrived (V-001 §18.1 step 7: "at activation, atomically switch
// binding"). Resolution already selects a scheduled version from its
// effective_from without waiting for this; promotion brings the version's
// status, its predecessor's status and the events in line.
//
// Safe to run on every instance: each row is claimed with SKIP LOCKED.
func (s *PgStore) PromoteDueActivations(ctx context.Context, limit int) (int, error) {
	type due struct {
		id     string
		tenant *string
	}
	rows, err := s.pool.Query(ctx, `
		SELECT policy_version_id, tenant_id FROM policy_activation_schedule
		WHERE due_at <= $1 ORDER BY due_at LIMIT $2;`, s.now(), limit)
	if err != nil {
		return 0, s.domainError("PromoteDueActivations", err)
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.tenant); err != nil {
			rows.Close()
			return 0, s.domainError("PromoteDueActivations", err)
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, s.domainError("PromoteDueActivations", err)
	}

	promoted := 0
	for _, d := range list {
		done, err := s.promote(ctx, d.id, d.tenant)
		if err != nil {
			s.log.Error("scheduled activation failed", zap.String("policy_version_id", d.id), zap.Error(err))
			continue
		}
		if done {
			promoted++
		}
	}
	return promoted, nil
}

func (s *PgStore) promote(ctx context.Context, id string, tenant *string) (bool, error) {
	promoted := false
	err := s.withRLS(ctx, derefOrEmpty(tenant), func(tx pgx.Tx) error {
		var claimed string
		err := tx.QueryRow(ctx, `
			SELECT policy_version_id FROM policy_activation_schedule
			WHERE policy_version_id = $1 AND due_at <= $2
			FOR UPDATE SKIP LOCKED;`, id, s.now()).Scan(&claimed)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // another instance has it, or it was cancelled
		}
		if err != nil {
			return err
		}
		v, _, err := lockVersion(ctx, tx, id)
		if err != nil {
			return err
		}
		if v.VersionStatus != domain.StatusScheduled {
			_, err := tx.Exec(ctx, `DELETE FROM policy_activation_schedule WHERE policy_version_id = $1;`, id)
			return err
		}
		meta := domain.EventMeta{CorrelationID: "scheduled-activation:" + id, IdempotencyKey: "scheduled-activation:" + id}
		if _, err := s.activateNow(ctx, tx, v, SchedulerActor, "scheduled activation reached effective_from", meta); err != nil {
			return err
		}
		promoted = true
		return nil
	})
	return promoted, err
}

// ── resolution support ───────────────────────────────────────────────────────

// ResolutionCandidates returns every version of policyType that was ever
// bound (activated or scheduled) and whose scope is compatible with the
// request: its tenant and legal entity are either unset or equal to the
// request's. Which of them governs at a given time is decided by package
// resolve, not here, so the same candidates serve every as_of / known_at.
func (s *PgStore) ResolutionCandidates(ctx context.Context, policyType string, tenantID, legalEntityID *string) ([]*domain.ApplicablePolicyVersion, error) {
	var out []*domain.ApplicablePolicyVersion
	err := s.withRLS(ctx, derefOrEmpty(tenantID), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+versionColumns("pv")+`, p.policy_code
			FROM policy_versions pv
			JOIN policies p ON p.policy_id = pv.policy_id
			WHERE p.policy_type = $1
			  AND pv.activated_at IS NOT NULL
			  AND (pv.tenant_id IS NULL OR pv.tenant_id = $2::uuid)
			  AND (pv.legal_entity_id IS NULL OR pv.legal_entity_id = $3::uuid)
			ORDER BY pv.activated_at, pv.policy_version_id;`, policyType, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a := &domain.ApplicablePolicyVersion{}
			v, scanErr := scanPolicyVersion(rows, &a.PolicyCode)
			if scanErr != nil {
				return scanErr
			}
			a.PolicyVersion = *v
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.domainError("ResolutionCandidates", err)
	}
	return out, nil
}

// ResolutionWatermark returns a counter that changes whenever any version's
// lifecycle changes. A cached candidate set is valid while it is unchanged.
func (s *PgStore) ResolutionWatermark(ctx context.Context) (int64, error) {
	var wm int64
	if err := s.pool.QueryRow(ctx, `SELECT version FROM policy_resolution_watermark WHERE singleton;`).Scan(&wm); err != nil {
		return 0, s.domainError("ResolutionWatermark", err)
	}
	return wm, nil
}
