// Package store is the PgStore persistence layer for
// reconciliation-engine-svc (DATA-07, ZS-SVC-N-001 §4). Every method
// runs inside one transaction that first declares app.tenant_id for
// RLS, then performs the write — tenant scoping is enforced by the
// database, never an application-level WHERE clause the caller could
// get wrong. Populations and their items are frozen the moment
// StartRun creates them; match results, exceptions and certifications
// are append-only evidence, never edited after the fact.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
	"zoiko.io/reconciliation-engine-svc/internal/outbox"
)

// Store is the DATA-07 persistence contract.
type Store interface {
	DefineReconciliation(ctx context.Context, tenantID string, req domain.DefineReconciliationRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationDefinition, error)
	CreateRuleVersion(ctx context.Context, tenantID, definitionID string, req domain.CreateRuleVersionRequest, actor string, claim domain.IdempotencyClaim) (*domain.RuleVersion, error)
	ApproveRuleVersion(ctx context.Context, tenantID, definitionID string, version int64, approver string, claim domain.IdempotencyClaim) (*domain.RuleVersion, error)
	ListRuleVersions(ctx context.Context, tenantID, definitionID string) ([]domain.RuleVersion, error)
	GetDefinition(ctx context.Context, tenantID, definitionID string) (*domain.ReconciliationDefinition, error)
	GetRuleVersion(ctx context.Context, tenantID, definitionID string, version int64) (*domain.RuleVersion, error)
	CheckIdempotency(ctx context.Context, tenantID string, claim domain.IdempotencyClaim) error
	StartRun(ctx context.Context, tenantID string, req domain.StartRunRequest, expectedDefinitionVersion int64,
		pops []domain.FrozenPopulation, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error)
	RecordFailedRun(ctx context.Context, tenantID string, req domain.StartRunRequest, expectedDefinitionVersion int64,
		failure domain.RunFailure, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error)
	Match(ctx context.Context, tenantID, runID string, req domain.MatchRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error)
	RaiseException(ctx context.Context, tenantID, runID string, req domain.RaiseExceptionRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationException, error)
	Reperform(ctx context.Context, tenantID, runID string, pops []domain.FrozenPopulation, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error)
	Certify(ctx context.Context, tenantID, runID string, actor string, authority *domain.AuthoritySnapshot, claim domain.IdempotencyClaim) (*domain.Certification, error)
	SupersedeRun(ctx context.Context, tenantID, runID string, req domain.SupersedeRunRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error)

	GetRun(ctx context.Context, tenantID, runID string) (*domain.ReconciliationRun, error)
	GetPopulationSnapshots(ctx context.Context, tenantID, runID string) ([]domain.PopulationSnapshot, error)
	GetMatchResults(ctx context.Context, tenantID, runID string) ([]domain.MatchResult, error)
	GetExceptions(ctx context.Context, tenantID, runID string) ([]domain.ReconciliationException, error)
	GetCertification(ctx context.Context, tenantID, runID string) (*domain.Certification, error)
	GetTransitions(ctx context.Context, tenantID, runID string) ([]domain.RunTransition, error)

	AssignException(ctx context.Context, tenantID, exceptionID string, req domain.AssignExceptionRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationException, error)
	TransitionException(ctx context.Context, tenantID, exceptionID string, req domain.TransitionExceptionRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationException, error)
	GetException(ctx context.Context, tenantID, exceptionID string) (*domain.ReconciliationException, error)
	GetExceptionTransitions(ctx context.Context, tenantID, exceptionID string) ([]domain.ExceptionTransition, error)
	GetAssessment(ctx context.Context, tenantID, runID string) (*domain.RunAssessment, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

var _ Store = (*PgStore)(nil)

func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("tenant_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return mapErr(err)
	}
	return tx.Commit(ctx)
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	return fmt.Errorf("reconciliation-engine-svc: %s", pgErr.Message)
}

func claimIdempotency(ctx context.Context, tx pgx.Tx, c domain.IdempotencyClaim) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, owner_scope, principal_id, idempotency_key, operation, request_sha256, resource_id)
		VALUES (current_setting('app.tenant_id', true), $1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, owner_scope, principal_id, idempotency_key) DO NOTHING`,
		c.OwnerScope, c.PrincipalID, c.Key, c.Operation, c.RequestSHA256, c.ResourceID)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var reqHash, resourceID string
	if err := tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id FROM idempotency_keys
		WHERE tenant_id = current_setting('app.tenant_id', true) AND owner_scope = $1 AND principal_id = $2 AND idempotency_key = $3`,
		c.OwnerScope, c.PrincipalID, c.Key).Scan(&reqHash, &resourceID); err != nil {
		return fmt.Errorf("read idempotency claim: %w", err)
	}
	if reqHash != c.RequestSHA256 {
		return domain.ErrIdempotencyKeyReused
	}
	return &domain.IdempotentReplayError{ResourceID: resourceID}
}

func newID(prefix string) string {
	return prefix + uuid.NewString()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// ── ReconciliationDefinition ────────────────────────────────────────────────

// encodePolicies / decodePolicies carry the governance policies (A4, A7) in
// and out of their JSONB columns; NULL (a row written before 000007) decodes
// to nil.
func encodePolicies(mat *domain.MaterialityPolicy, exPol *domain.ExceptionPolicy, profile *domain.CertificationProfile) ([]byte, []byte, []byte, error) {
	if mat.Currencies == nil {
		mat = &domain.MaterialityPolicy{Currencies: map[string]int64{}}
	}
	m, err := json.Marshal(mat)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode materiality: %w", err)
	}
	e, err := json.Marshal(exPol)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode exception policy: %w", err)
	}
	p, err := json.Marshal(profile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode certification profile: %w", err)
	}
	return m, e, p, nil
}

func decodePolicies(mat, exPol, profile []byte, matOut **domain.MaterialityPolicy, exOut **domain.ExceptionPolicy,
	profOut **domain.CertificationProfile) error {
	if len(mat) > 0 {
		*matOut = &domain.MaterialityPolicy{}
		if err := json.Unmarshal(mat, *matOut); err != nil {
			return err
		}
	}
	if len(exPol) > 0 {
		*exOut = &domain.ExceptionPolicy{}
		if err := json.Unmarshal(exPol, *exOut); err != nil {
			return err
		}
	}
	if len(profile) > 0 {
		*profOut = &domain.CertificationProfile{}
		if err := json.Unmarshal(profile, *profOut); err != nil {
			return err
		}
	}
	return nil
}

// ── ReconciliationRun ────────────────────────────────────────────────────────

const runColumns = `run_id, tenant_id, legal_entity_id::text, definition_id, definition_version, COALESCE(rules_sha256, ''), status, result_state,
	certification_state, failure_code, failure_reason, period_id, scope,
	amount_scale, match_key_fields, balance_dimensions, tolerance_policy, materiality_policy, exception_policy, certification_profile,
	supersedes_run_id, superseded_by_run_id,
	superseded_reason, superseded_by, superseded_at, started_at, started_by, certified_at, certified_by`

func scanRun(row pgx.Row) (*domain.ReconciliationRun, error) {
	var r domain.ReconciliationRun
	var certifiedBy, entity, period *string
	var scale *int
	var scope, tol, mat, exPol, profile []byte
	if err := row.Scan(&r.RunID, &r.TenantID, &entity, &r.DefinitionID, &r.DefinitionVersion, &r.RulesSHA256, &r.Status, &r.ResultState,
		&r.CertificationState, &r.FailureCode, &r.FailureReason, &period, &scope,
		&scale, &r.MatchKeyFields, &r.BalanceDimensions, &tol, &mat, &exPol, &profile, &r.SupersedesRunID, &r.SupersededByRunID,
		&r.SupersededReason, &r.SupersededBy, &r.SupersededAt, &r.StartedAt, &r.StartedBy, &r.CertifiedAt, &certifiedBy); err != nil {
		return nil, err
	}
	if certifiedBy != nil {
		r.CertifiedBy = *certifiedBy
	}
	if err := decodePolicies(mat, exPol, profile, &r.Materiality, &r.ExceptionPolicy, &r.CertificationProfile); err != nil {
		return nil, fmt.Errorf("decode run policies: %w", err)
	}
	if entity != nil {
		r.LegalEntityID = *entity
	}
	if period != nil {
		r.PeriodID = *period
	}
	if scale != nil {
		r.AmountScale = *scale
	}
	if len(tol) > 0 {
		r.Tolerance = &domain.TolerancePolicy{}
		if err := json.Unmarshal(tol, r.Tolerance); err != nil {
			return nil, fmt.Errorf("decode run tolerance: %w", err)
		}
	}
	if len(scope) > 0 {
		if err := json.Unmarshal(scope, &r.Scope); err != nil {
			return nil, fmt.Errorf("decode run scope: %w", err)
		}
		if len(r.Scope) == 0 {
			r.Scope = nil
		}
	}
	return &r, nil
}

func loadRunForUpdate(ctx context.Context, tx pgx.Tx, tenantID, runID string) (*domain.ReconciliationRun, error) {
	r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM reconciliation_runs WHERE tenant_id = $1 AND run_id = $2 FOR UPDATE`,
		tenantID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRunNotFound
	}
	return r, err
}

func (s *PgStore) GetRun(ctx context.Context, tenantID, runID string) (*domain.ReconciliationRun, error) {
	var out *domain.ReconciliationRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM reconciliation_runs WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRunNotFound
		}
		if err != nil {
			return err
		}
		// What the result rests on — including every accepted risk — is part of
		// reading the run (DQC-05: accepted risk is never silent).
		a, err := assessRun(ctx, tx, r)
		if err != nil {
			return err
		}
		r.Assessment = &a
		out = r
		return nil
	})
	return out, err
}

// CheckIdempotency reports, without claiming anything, whether claim's key
// was already used: an IdempotentReplayError for the same request, or
// ErrIdempotencyKeyReused for a different one. StartRun calls it before
// reading any source, so a retry of a run that already started replays it
// instead of re-reading populations that may since have moved.
func (s *PgStore) CheckIdempotency(ctx context.Context, tenantID string, c domain.IdempotencyClaim) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var reqHash, resourceID string
		err := tx.QueryRow(ctx, `
			SELECT request_sha256, resource_id FROM idempotency_keys
			WHERE tenant_id = $1 AND owner_scope = $2 AND principal_id = $3 AND idempotency_key = $4`,
			tenantID, c.OwnerScope, c.PrincipalID, c.Key).Scan(&reqHash, &resourceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read idempotency claim: %w", err)
		}
		if reqHash != c.RequestSHA256 {
			return domain.ErrIdempotencyKeyReused
		}
		return &domain.IdempotentReplayError{ResourceID: resourceID}
	})
}

// StartRun freezes the two populations the caller read from their sources —
// the "frozen populations" core control — and copies the definition's rules
// onto the run itself, so nothing about a definition edited later can ever
// reach back into this run. The populations were read for definition version
// expectedVersion; if the definition changed meanwhile, the populations may
// not be the ones it now names, and the run is refused (ErrDefinitionChanged).
func (s *PgStore) StartRun(ctx context.Context, tenantID string, req domain.StartRunRequest, expectedVersion int64,
	pops []domain.FrozenPopulation, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ReconciliationRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		created, err := createRun(ctx, tx, tenantID, req, expectedVersion, actor, &claim, func(def *domain.ReconciliationDefinition) error {
			return checkPopulationsMatchDefinition(def, pops)
		})
		if err != nil {
			return err
		}
		for _, pop := range pops {
			if err := freezePopulation(ctx, tx, tenantID, created.run.RunID, created.def.AmountScale, 1, pop); err != nil {
				return err
			}
		}
		out = created.run
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_run", AggregateID: out.RunID,
			EventType: "CONTROL.ReconciliationStarted", TenantID: &tenantID, Payload: out})
	})
	return out, err
}

// RecordFailedRun records a run whose populations could not be proven
// complete and reproducible (A6): the run exists, pinned to the definition
// version it was attempted against, with no frozen population, lifecycle
// Failed, result INDETERMINATE (never a MATCH — INV-05, scenario 09) and the
// failure's code and reason. A retry under the same Idempotency-Key replays
// this run; a new attempt is a new run, which may later supersede this one.
func (s *PgStore) RecordFailedRun(ctx context.Context, tenantID string, req domain.StartRunRequest, expectedVersion int64,
	failure domain.RunFailure, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if err := failure.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ReconciliationRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		created, err := createRun(ctx, tx, tenantID, req, expectedVersion, actor, &claim, nil)
		if err != nil {
			return err
		}
		failed, err := advanceRun(ctx, tx, tenantID, created.run, runAdvance{
			To: domain.RunFailed, Result: domain.ResultIndeterminate, Cert: domain.CertUncertified,
			Reason: "populations could not be proven complete: " + string(failure.Code), Actor: actor, Failure: &failure})
		if err != nil {
			return err
		}
		out = failed
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_run", AggregateID: out.RunID,
			EventType: "CONTROL.ReconciliationFailed", TenantID: &tenantID, Payload: out})
	})
	return out, err
}

type createdRun struct {
	def *domain.ReconciliationDefinition
	run *domain.ReconciliationRun
}

// createRun inserts a Planned run pinned to the definition's effective rule
// version — its rules copied onto the run, its digest pinned beside them —
// under a share lock on the definition, after claiming the idempotency key.
// The run is refused if the definition changed since its populations were
// read for expectedVersion (ErrDefinitionChanged); check, when given, vets the
// definition before anything is written.
func createRun(ctx context.Context, tx pgx.Tx, tenantID string, req domain.StartRunRequest, expectedVersion int64, actor string,
	claim *domain.IdempotencyClaim, check func(*domain.ReconciliationDefinition) error) (*createdRun, error) {
	def, err := loadDefinition(ctx, tx, tenantID, req.DefinitionID, true)
	if err != nil {
		return nil, err
	}
	if err := def.Runnable(); err != nil {
		return nil, err
	}
	if def.Version != expectedVersion {
		return nil, domain.ErrDefinitionChanged
	}
	if check != nil {
		if err := check(def); err != nil {
			return nil, err
		}
	}

	runID := newID(domain.PrefixRun)
	claim.ResourceID = runID
	if err := claimIdempotency(ctx, tx, *claim); err != nil {
		return nil, err
	}

	scope, err := json.Marshal(req.Scope)
	if err != nil {
		return nil, fmt.Errorf("encode run scope: %w", err)
	}
	if req.Scope == nil {
		scope = []byte("{}")
	}
	var period *string
	if req.PeriodID != "" {
		period = &req.PeriodID
	}
	tol, err := json.Marshal(def.Tolerance)
	if err != nil {
		return nil, fmt.Errorf("encode run tolerance: %w", err)
	}
	dims := def.BalanceDimensions
	if dims == nil {
		dims = []string{}
	}
	mat, exPol, profile, err := encodePolicies(def.Materiality, def.ExceptionPolicy, def.CertificationProfile)
	if err != nil {
		return nil, err
	}
	run, err := scanRun(tx.QueryRow(ctx, `
		INSERT INTO reconciliation_runs (run_id, tenant_id, legal_entity_id, definition_id, definition_version, status,
			period_id, scope, amount_scale, match_key_fields, balance_dimensions, tolerance_policy,
			materiality_policy, exception_policy, certification_profile, rules_sha256, started_by)
		VALUES ($1, $2, $3, $4, $5, 'Planned', $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING `+runColumns,
		runID, tenantID, def.LegalEntityID, def.DefinitionID, def.Version, period, scope, def.AmountScale,
		def.MatchKeyFields, dims, tol, mat, exPol, profile, def.RulesSHA256, actor))
	if err != nil {
		return nil, fmt.Errorf("create reconciliation run: %w", err)
	}
	return &createdRun{def: def, run: run}, nil
}

// ── Run state movement ──────────────────────────────────────────────────────

// runAdvance is one governed movement of a run's three state dimensions.
type runAdvance struct {
	To     domain.RunStatus
	Result domain.ResultState
	Cert   domain.CertificationState
	Reason string
	Actor  string

	CertifiedBy  *string
	Supersession *supersession
	Failure      *domain.RunFailure
	// AcceptedRiskCertifiable: the run's VARIANCE is only accepted risk its
	// profile permits (domain.Assess), so it may be ELIGIBLE / CERTIFIED.
	AcceptedRiskCertifiable bool
}

type supersession struct {
	ByRunID, Reason, By string
}

// advanceRun is the only way a run's status, result or certification changes
// (the pattern of financial-control-svc's advanceTx). It validates the three
// dimensions' edges and the invariants between them, writes the row, and
// appends one transition per dimension that changed. The lifecycle trigger
// enforces the same rules again in the database.
func advanceRun(ctx context.Context, tx pgx.Tx, tenantID string, cur *domain.ReconciliationRun, a runAdvance) (*domain.ReconciliationRun, error) {
	if err := domain.ValidateResultTransition(cur.Status, cur.ResultState, a.Result); err != nil {
		return nil, err
	}
	if err := domain.ValidateCertificationTransition(cur.CertificationState, a.Cert); err != nil {
		return nil, err
	}
	if err := domain.ValidateLifecycleTransition(cur.Status, a.To, a.Result, a.Cert, a.AcceptedRiskCertifiable); err != nil {
		return nil, err
	}
	if (a.To == domain.RunFailed) != (a.Failure != nil) {
		return nil, fmt.Errorf("%w: a run fails with, and only with, a failure code", domain.ErrInvalidTransition)
	}
	var supBy, supReason, supActor, failCode, failReason *string
	if a.Supersession != nil {
		supBy, supReason, supActor = &a.Supersession.ByRunID, &a.Supersession.Reason, &a.Supersession.By
	}
	if a.Failure != nil {
		code := string(a.Failure.Code)
		failCode, failReason = &code, &a.Failure.Reason
	}
	updated, err := scanRun(tx.QueryRow(ctx, `
		UPDATE reconciliation_runs SET status = $2, result_state = $3, certification_state = $4,
			certified_by = COALESCE($5, certified_by),
			superseded_by_run_id = COALESCE($6, superseded_by_run_id), superseded_reason = COALESCE($7, superseded_reason),
			superseded_by = COALESCE($8, superseded_by),
			failure_code = COALESCE($9, failure_code), failure_reason = COALESCE($10, failure_reason)
		WHERE tenant_id = $11 AND run_id = $1 RETURNING `+runColumns,
		cur.RunID, string(a.To), string(a.Result), string(a.Cert), a.CertifiedBy, supBy, supReason, supActor,
		failCode, failReason, tenantID))
	if err != nil {
		return nil, fmt.Errorf("advance reconciliation run: %w", err)
	}
	for _, d := range []struct{ dim, from, to string }{
		{"LIFECYCLE", string(cur.Status), string(updated.Status)},
		{"RESULT", string(cur.ResultState), string(updated.ResultState)},
		{"CERTIFICATION", string(cur.CertificationState), string(updated.CertificationState)},
	} {
		if d.from == d.to {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO reconciliation_run_transitions (tenant_id, run_id, dimension, from_state, to_state, reason, actor)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, tenantID, cur.RunID, d.dim, d.from, d.to, a.Reason, a.Actor); err != nil {
			return nil, fmt.Errorf("record run transition: %w", err)
		}
	}
	return updated, nil
}

// settleResult re-evaluates an open run's result from its exceptions — MATCH
// with none unresolved, VARIANCE otherwise (an accepted risk is unresolved:
// INV-19) — sets the certification state its assessment implies, and moves
// the run to `to`. A run asked to be Running while an exception is still being
// worked is ExceptionsOpen instead.
func settleResult(ctx context.Context, tx pgx.Tx, tenantID string, run *domain.ReconciliationRun, to domain.RunStatus,
	reason, actor string) (*domain.ReconciliationRun, error) {
	a, err := assessRun(ctx, tx, run)
	if err != nil {
		return nil, err
	}
	working := 0
	for st, n := range a.Unresolved {
		if st.Working() {
			working += n
		}
	}
	if to == domain.RunRunning && working > 0 {
		to = domain.RunExceptionsOpen
	}
	return advanceRun(ctx, tx, tenantID, run, runAdvance{To: to, Result: a.ResultState, Cert: domain.CertificationFor(a),
		Reason:                  fmt.Sprintf("%s: %d exception(s) being worked, %d accepted as risk", reason, working, len(a.AcceptedRiskIDs)),
		Actor:                   actor,
		AcceptedRiskCertifiable: a.Certifiable && a.ResultState == domain.ResultVariance})
}

// checkPopulationsMatchDefinition refuses populations that are not exactly the
// definition's two sides, each read from the system its spec names.
func checkPopulationsMatchDefinition(def *domain.ReconciliationDefinition, pops []domain.FrozenPopulation) error {
	if len(pops) != len(def.Populations) {
		return fmt.Errorf("%w: %d populations supplied, the definition names %d", domain.ErrInvalidArgument, len(pops), len(def.Populations))
	}
	want := map[string]string{}
	for _, sp := range def.Populations {
		want[sp.Side] = sp.System
	}
	for _, p := range pops {
		sys, ok := want[p.Side]
		if !ok || sys != p.SourceSystem {
			return fmt.Errorf("%w: population %q/%q is not one the definition names", domain.ErrInvalidArgument, p.Side, p.SourceSystem)
		}
		delete(want, p.Side)
	}
	return nil
}

// freezePopulation writes one side's snapshot (one generation of it) and its
// items. The snapshot's control totals are computed here from the items
// themselves, with overflow checks (A14): a total that does not fit an int64
// refuses the run instead of wrapping. The row count must equal the source's
// declared row count (the table's CHECK enforces the same).
func freezePopulation(ctx context.Context, tx pgx.Tx, tenantID, runID string, scale, generation int, pop domain.FrozenPopulation) error {
	if strings.TrimSpace(pop.Watermark) == "" {
		return fmt.Errorf("%w: population %q has no source watermark", domain.ErrInvalidArgument, pop.Side)
	}
	if pop.DeclaredTotals == nil || pop.DeclaredRowCount != int64(len(pop.Items)) {
		return fmt.Errorf("%w: population %q does not carry declared totals for %d rows", domain.ErrInvalidArgument, pop.Side, len(pop.Items))
	}
	// The overall total is computed only so that its overflow is refused (A14);
	// it adds currencies together, so it is never stored (A2).
	perCcy, _, err := domain.ControlTotals(pop.Items)
	if err != nil {
		return fmt.Errorf("population %q: %w", pop.Side, err)
	}
	controlJSON, err := json.Marshal(perCcy)
	if err != nil {
		return fmt.Errorf("encode control totals: %w", err)
	}
	declaredJSON, err := json.Marshal(pop.DeclaredTotals)
	if err != nil {
		return fmt.Errorf("encode declared totals: %w", err)
	}

	snapshotID := newID(domain.PrefixSnapshot)
	if _, err := tx.Exec(ctx, `
		INSERT INTO population_snapshots (snapshot_id, tenant_id, run_id, side, generation, source_system, spec_ref, source_watermark,
			item_count, amount_scale, control_totals, declared_row_count, declared_totals, content_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		snapshotID, tenantID, runID, pop.Side, generation, pop.SourceSystem, pop.SpecRef, pop.Watermark,
		len(pop.Items), scale, controlJSON, pop.DeclaredRowCount, declaredJSON, domain.HashPopulation(pop.Items)); err != nil {
		return fmt.Errorf("freeze population snapshot: %w", err)
	}

	for _, it := range pop.Items {
		dims, err := json.Marshal(it.Dimensions)
		if err != nil {
			return fmt.Errorf("marshal item dimensions: %w", err)
		}
		if it.Dimensions == nil {
			dims = []byte("{}")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO population_items (item_id, tenant_id, snapshot_id, source_record_id, ref_id, amount_minor_units, currency, occurred_at, dimensions)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			newID(domain.PrefixItem), tenantID, snapshotID, it.SourceRecordID, it.RefID, it.AmountMinorUnits, it.Currency, it.OccurredAt, dims); err != nil {
			return fmt.Errorf("freeze population item: %w", err)
		}
	}
	return nil
}

// ── Match ─────────────────────────────────────────────────────────────────────

const matchItemSelect = `SELECT pi.item_id, ps.side, COALESCE(pi.source_record_id, ''), pi.ref_id, pi.amount_minor_units,
		COALESCE(pi.currency, ''), pi.occurred_at, pi.dimensions
	FROM population_items pi JOIN population_snapshots ps ON ps.snapshot_id = pi.snapshot_id`

func scanMatchItems(rows pgx.Rows) ([]domain.MatchItem, error) {
	defer rows.Close()
	var out []domain.MatchItem
	for rows.Next() {
		var it domain.MatchItem
		var dims []byte
		if err := rows.Scan(&it.ItemID, &it.Side, &it.SourceRecordID, &it.RefID, &it.Amount, &it.Currency, &it.OccurredAt, &dims); err != nil {
			return nil, err
		}
		if len(dims) > 0 {
			if err := json.Unmarshal(dims, &it.Dimensions); err != nil {
				return nil, fmt.Errorf("decode item dimensions: %w", err)
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func splitBySide(items []domain.MatchItem, sideA string) (a, b []domain.MatchItem) {
	for _, it := range items {
		if it.Side == sideA {
			a = append(a, it)
		} else {
			b = append(b, it)
		}
	}
	return a, b
}

// runSides returns the run's two population sides, ordered by label. They
// come from the frozen snapshots of the run's LATEST generation (A8: a
// reperformance adds a generation; matching and totals are always about the
// current one), not from the unprocessed items: an EMPTY population is still
// a side (B2; ZS-SVC-Z-001 NP-27).
func runSides(ctx context.Context, tx pgx.Tx, runID string) (string, string, error) {
	rows, err := tx.Query(ctx, `SELECT side FROM population_snapshots WHERE run_id = $1
		AND generation = (SELECT MAX(generation) FROM population_snapshots WHERE run_id = $1) ORDER BY side`, runID)
	if err != nil {
		return "", "", fmt.Errorf("load population sides: %w", err)
	}
	defer rows.Close()
	var sides []string
	for rows.Next() {
		var side string
		if err := rows.Scan(&side); err != nil {
			return "", "", err
		}
		sides = append(sides, side)
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	sort.Strings(sides)
	if len(sides) != 2 {
		return "", "", fmt.Errorf("reconciliation run %s has %d populations; exactly two are required", runID, len(sides))
	}
	return sides[0], sides[1], nil
}

func insertItemException(ctx context.Context, tx pgx.Tx, tenantID string, run *domain.ReconciliationRun, actor string, e domain.ItemException) error {
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		return fmt.Errorf("encode exception detail: %w", err)
	}
	cat, assertion := domain.RuleClassification(e.Detail.Rule)
	g, err := governanceFor(run, e.Item.Currency, domain.ItemExposure(e), cat, assertion)
	if err != nil {
		return err
	}
	args := append([]any{newID(domain.PrefixException), tenantID, run.RunID, e.Item.ItemID, e.Item.Side, e.Item.RefID,
		e.Item.Currency, e.Reason, e.Item.Amount, detail, actor}, g.values()...)
	if _, err := tx.Exec(ctx, `
		INSERT INTO reconciliation_exceptions (exception_id, tenant_id, run_id, kind, item_id, side, ref_id, currency, reason,
			amount_minor_units, detail, raised_by, `+governanceInsertColumns+`)
		VALUES ($1, $2, $3, 'ITEM', $4, $5, $6, $7, $8, $9, $10, $11, `+governancePlaceholders(12)+`)`, args...); err != nil {
		return fmt.Errorf("raise auto exception: %w", err)
	}
	return nil
}

func insertBalanceException(ctx context.Context, tx pgx.Tx, tenantID string, run *domain.ReconciliationRun, actor, sideA, sideB string, v domain.BalanceVariance) error {
	scope := "population"
	if v.Dimension != "" {
		scope = fmt.Sprintf("%s=%q", v.Dimension, v.DimensionValue)
	}
	reason := fmt.Sprintf("%s %s total differs between %s (%d) and %s (%d) by %d, tolerance %d",
		v.Currency, scope, sideA, v.TotalA, sideB, v.TotalB, v.Difference, v.Allowed)
	diff, allowed := v.Difference, v.Allowed
	detail, err := json.Marshal(domain.ExceptionDetail{Rule: domain.RuleBalance, Currency: v.Currency, DifferenceMinor: &diff,
		AllowedMinor: &allowed, SideTotals: map[string]int64{sideA: v.TotalA, sideB: v.TotalB}})
	if err != nil {
		return fmt.Errorf("encode balance detail: %w", err)
	}
	cat, assertion := domain.RuleClassification(domain.RuleBalance)
	g, err := governanceFor(run, v.Currency, domain.ExposureOf(v.Difference), cat, assertion)
	if err != nil {
		return err
	}
	args := append([]any{newID(domain.PrefixException), tenantID, run.RunID, v.Currency, v.Dimension, v.DimensionValue, reason,
		v.Difference, detail, actor}, g.values()...)
	// Raised once per run, currency and bucket however often matching is re-run.
	if _, err := tx.Exec(ctx, `
		INSERT INTO reconciliation_exceptions (exception_id, tenant_id, run_id, kind, currency, dimension, dimension_value, reason,
			amount_minor_units, detail, raised_by, `+governanceInsertColumns+`)
		VALUES ($1, $2, $3, 'BALANCE', $4, $5, $6, $7, $8, $9, $10, `+governancePlaceholders(11)+`)
		ON CONFLICT (run_id, currency, dimension, dimension_value) WHERE kind = 'BALANCE' DO NOTHING`, args...); err != nil {
		return fmt.Errorf("raise balance exception: %w", err)
	}
	return nil
}

// runAutoMatch pairs the run's still-unprocessed items of its LATEST
// generation (neither matched nor excepted; a reperformance supersedes the
// earlier generation, whose matches and exceptions stand as history) on the
// run's pinned match key, under its pinned tolerance policy
// (domain.AutoMatch), then checks per-currency balance for the whole
// population and every pinned balance dimension (domain.CheckBalances) over
// the same generation's items. Matching by key — never by aggregate totals —
// is what makes "equal totals with different composition still surface
// exceptions" true; the balance check is what stops a balanced aggregate
// hiding an unbalanced sub-dimension.
func runAutoMatch(ctx context.Context, tx pgx.Tx, tenantID, actor string, run *domain.ReconciliationRun) error {
	if run.Tolerance == nil || len(run.MatchKeyFields) == 0 || run.ExceptionPolicy == nil || run.Materiality == nil ||
		run.CertificationProfile == nil {
		return domain.ErrLegacyRecord
	}
	sideA, sideB, err := runSides(ctx, tx, run.RunID)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, matchItemSelect+`
		WHERE ps.run_id = $1
			AND ps.generation = (SELECT MAX(generation) FROM population_snapshots WHERE run_id = $1)
			AND NOT EXISTS (SELECT 1 FROM match_results mr WHERE mr.item_a_id = pi.item_id OR mr.item_b_id = pi.item_id)
			AND NOT EXISTS (SELECT 1 FROM reconciliation_exceptions re WHERE re.item_id = pi.item_id)`, run.RunID)
	if err != nil {
		return fmt.Errorf("load unprocessed items: %w", err)
	}
	pending, err := scanMatchItems(rows)
	if err != nil {
		return err
	}
	a, b := splitBySide(pending, sideA)
	pairs, excs := domain.AutoMatch(sideA, sideB, a, b, run.MatchKeyFields, *run.Tolerance)
	for _, p := range pairs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO match_results (match_result_id, tenant_id, run_id, item_a_id, item_b_id, ref_id, manual, matched_by)
			VALUES ($1, $2, $3, $4, $5, $6, FALSE, $7)`,
			newID(domain.PrefixMatchResult), tenantID, run.RunID, p.A.ItemID, p.B.ItemID, p.A.RefID, actor); err != nil {
			return fmt.Errorf("insert auto match: %w", err)
		}
	}
	for _, e := range excs {
		if err := insertItemException(ctx, tx, tenantID, run, actor, e); err != nil {
			return err
		}
	}

	rows, err = tx.Query(ctx, matchItemSelect+`
		WHERE ps.run_id = $1
			AND ps.generation = (SELECT MAX(generation) FROM population_snapshots WHERE run_id = $1)`, run.RunID)
	if err != nil {
		return fmt.Errorf("load run items: %w", err)
	}
	all, err := scanMatchItems(rows)
	if err != nil {
		return err
	}
	allA, allB := splitBySide(all, sideA)
	variances, err := domain.CheckBalances(sideA, sideB, allA, allB, run.BalanceDimensions, *run.Tolerance)
	if err != nil {
		return err
	}
	for _, v := range variances {
		if err := insertBalanceException(ctx, tx, tenantID, run, actor, sideA, sideB, v); err != nil {
			return err
		}
	}
	return nil
}

func openRunStatuses() map[domain.RunStatus]bool {
	return map[domain.RunStatus]bool{
		domain.RunPlanned: true, domain.RunRunning: true, domain.RunExceptionsOpen: true, domain.RunReperformed: true,
	}
}

// Match runs the automatic matching pass over any still-unprocessed
// items, then applies every caller-supplied manual match — each of
// which must carry a non-empty reason and is stamped with the calling
// principal as its authority (domain.MatchRequest.Validate already
// enforces the reason is present; the authority is this handler's own
// authenticated actor, never a caller-suppliable field). A manual
// match resolves any exception it was raised against.
func (s *PgStore) Match(ctx context.Context, tenantID, runID string, req domain.MatchRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ReconciliationRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRunForUpdate(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		if !openRunStatuses()[run.Status] {
			return domain.ErrRunNotOpen
		}

		claim.ResourceID = runID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		if run.Status == domain.RunPlanned {
			if run, err = advanceRun(ctx, tx, tenantID, run, runAdvance{To: domain.RunRunning, Result: run.ResultState,
				Cert: run.CertificationState, Reason: "matching started", Actor: actor}); err != nil {
				return err
			}
		}

		if err := runAutoMatch(ctx, tx, tenantID, actor, run); err != nil {
			return err
		}

		for _, m := range req.ManualMatches {
			if err := applyManualMatch(ctx, tx, tenantID, runID, actor, *run.Tolerance, m); err != nil {
				return err
			}
		}

		updated, err := settleResult(ctx, tx, tenantID, run, domain.RunRunning, "matched", actor)
		if err != nil {
			return err
		}
		eventType := "CONTROL.ReconciliationCompleted"
		if updated.ResultState == domain.ResultVariance {
			eventType = "CONTROL.ExceptionRaised"
		}
		out = updated
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_run", AggregateID: runID,
			EventType: eventType, TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

func applyManualMatch(ctx context.Context, tx pgx.Tx, tenantID, runID, actor string, pol domain.TolerancePolicy, m domain.ManualMatchInput) error {
	var pair [2]domain.MatchItem
	for i, itemID := range []string{m.ItemAID, m.ItemBID} {
		rows, err := tx.Query(ctx, matchItemSelect+`
			WHERE ps.run_id = $1 AND pi.item_id = $2
				AND ps.generation = (SELECT MAX(generation) FROM population_snapshots WHERE run_id = $1)`, runID, itemID)
		if err != nil {
			return fmt.Errorf("check manual match item: %w", err)
		}
		got, err := scanMatchItems(rows)
		if err != nil {
			return fmt.Errorf("check manual match item: %w", err)
		}
		if len(got) != 1 {
			return domain.ErrItemNotFound
		}
		pair[i] = got[0]
	}
	if err := domain.ValidateManualMatch(pair[0], pair[1], pol); err != nil {
		return err
	}
	var alreadyMatched bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM match_results WHERE item_a_id IN ($1, $2) OR item_b_id IN ($1, $2))`,
		m.ItemAID, m.ItemBID).Scan(&alreadyMatched); err != nil {
		return fmt.Errorf("check existing match: %w", err)
	}
	if alreadyMatched {
		return fmt.Errorf("item is already matched")
	}

	refID := pair[0].RefID

	// The evidence beside the operator's reason (A12, §10 "reason and
	// evidence"): both frozen items, their generation, and the comparison under
	// the pinned tolerance that let the pairing through. Recorded with the match,
	// never reconstructed later.
	var generation int
	if err := tx.QueryRow(ctx, `SELECT MAX(generation) FROM population_snapshots WHERE run_id = $1`, runID).Scan(&generation); err != nil {
		return fmt.Errorf("read snapshot generation: %w", err)
	}
	evidence, err := json.Marshal(domain.NewManualMatchEvidence(pair[0], pair[1], generation, pol))
	if err != nil {
		return fmt.Errorf("encode manual match evidence: %w", err)
	}

	mrID := newID(domain.PrefixMatchResult)
	if _, err := tx.Exec(ctx, `
		INSERT INTO match_results (match_result_id, tenant_id, run_id, item_a_id, item_b_id, ref_id, manual, reason, matched_by, evidence)
		VALUES ($1, $2, $3, $4, $5, $6, TRUE, $7, $8, $9)`,
		mrID, tenantID, runID, m.ItemAID, m.ItemBID, refID, m.Reason, actor, evidence); err != nil {
		return fmt.Errorf("insert manual match: %w", err)
	}

	return closeExceptionsForMatch(ctx, tx, tenantID, mrID, actor, m.Reason, m.ItemAID, m.ItemBID)
}

// ── RaiseException ───────────────────────────────────────────────────────────

func (s *PgStore) RaiseException(ctx context.Context, tenantID, runID string, req domain.RaiseExceptionRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationException, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ReconciliationException
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRunForUpdate(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		if !openRunStatuses()[run.Status] {
			return domain.ErrRunNotOpen
		}
		// No result is recorded before the run is evaluated: a Planned run has
		// none to turn into a VARIANCE.
		if run.Status == domain.RunPlanned {
			return domain.ErrRunNotEvaluated
		}

		rows, err := tx.Query(ctx, matchItemSelect+`
			WHERE ps.run_id = $1 AND pi.item_id = $2
				AND ps.generation = (SELECT MAX(generation) FROM population_snapshots WHERE run_id = $1)`, runID, req.ItemID)
		if err != nil {
			return fmt.Errorf("load item for exception: %w", err)
		}
		items, err := scanMatchItems(rows)
		if err != nil {
			return fmt.Errorf("load item for exception: %w", err)
		}
		if len(items) != 1 {
			return domain.ErrItemNotFound
		}
		it := items[0]

		claim.ResourceID = runID + "|" + req.ItemID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		g, err := governanceFor(run, it.Currency, domain.ExposureOf(it.Amount), req.Category, req.Assertion)
		if err != nil {
			return err
		}
		exID := newID(domain.PrefixException)
		args := append([]any{exID, tenantID, runID, it.ItemID, it.Side, it.RefID, it.Currency, req.Reason, it.Amount, actor}, g.values()...)
		got, err := scanException(tx.QueryRow(ctx, `
			INSERT INTO reconciliation_exceptions (exception_id, tenant_id, run_id, kind, item_id, side, ref_id, currency, reason,
				amount_minor_units, detail, raised_by, `+governanceInsertColumns+`)
			VALUES ($1, $2, $3, 'ITEM', $4, $5, $6, $7, $8, $9, '{"rule":"RAISED_MANUALLY"}', $10, `+governancePlaceholders(11)+`)
			RETURNING `+exceptionColumns, args...))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return fmt.Errorf("an exception has already been raised for this item")
			}
			return fmt.Errorf("raise exception: %w", err)
		}

		if _, err := settleResult(ctx, tx, tenantID, run, domain.RunRunning, "exception raised", actor); err != nil {
			return err
		}

		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_exception", AggregateID: got.ExceptionID,
			EventType: "CONTROL.ExceptionRaised", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetExceptions(ctx context.Context, tenantID, runID string) ([]domain.ReconciliationException, error) {
	var out []domain.ReconciliationException
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+exceptionColumns+` FROM reconciliation_exceptions WHERE tenant_id = $1 AND run_id = $2 ORDER BY raised_at`,
			tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		now := time.Now()
		for rows.Next() {
			e, err := scanException(rows)
			if err != nil {
				return err
			}
			e.Derive(now)
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}

// ── Reperform ────────────────────────────────────────────────────────────────

// nextSnapshotGeneration is the generation a re-read freezes as: one past the
// run's latest. StartRun writes generation 1.
func nextSnapshotGeneration(ctx context.Context, tx pgx.Tx, runID string) (int, error) {
	var next int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM population_snapshots WHERE run_id = $1`,
		runID).Scan(&next); err != nil {
		return 0, fmt.Errorf("read next snapshot generation: %w", err)
	}
	return next, nil
}

// snapshotFacts is the material identity of one frozen side, as the re-read
// note compares it across generations.
type snapshotFacts struct {
	watermark string
	rows      int64
	hash      string
	totals    map[string]string
}

// reperformanceNote describes what the re-read frozen as generation changed
// relative to the generation it supersedes. A change in the pulled content —
// the hash of its items, the source's declared row count or declared totals —
// is never silent (A8): the note is carried in the Reperformed move's reason,
// append-only history exposed at GET /runs/{id}/transitions. Unchanged content
// is named as such, so "read again, same content" is distinguishable from "not
// read again"; a moved watermark with unchanged content is mentioned on the
// same principle.
func reperformanceNote(ctx context.Context, tx pgx.Tx, runID string, generation int) (string, error) {
	if generation <= 1 {
		return "", nil
	}
	rows, err := tx.Query(ctx, `SELECT side, generation, source_watermark, declared_row_count, content_hash, declared_totals
		FROM population_snapshots WHERE run_id = $1 AND generation IN ($2, $3) ORDER BY side, generation`,
		runID, generation-1, generation)
	if err != nil {
		return "", fmt.Errorf("load snapshot generations: %w", err)
	}
	defer rows.Close()
	prev := map[string]snapshotFacts{}
	cur := map[string]snapshotFacts{}
	for rows.Next() {
		var side string
		var gen int
		var watermark *string
		var declaredRows *int64
		var hash string
		var totalsJSON []byte
		if err := rows.Scan(&side, &gen, &watermark, &declaredRows, &hash, &totalsJSON); err != nil {
			return "", err
		}
		f := snapshotFacts{hash: hash}
		if watermark != nil {
			f.watermark = *watermark
		}
		if declaredRows != nil {
			f.rows = *declaredRows
		}
		if len(totalsJSON) > 0 {
			if err := json.Unmarshal(totalsJSON, &f.totals); err != nil {
				return "", fmt.Errorf("decode declared totals: %w", err)
			}
		}
		if gen == generation {
			cur[side] = f
		} else {
			prev[side] = f
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	sides := make([]string, 0, len(cur))
	for side := range cur {
		sides = append(sides, side)
	}
	sort.Strings(sides)
	var changed, moved []string
	for _, side := range sides {
		p, c := prev[side], cur[side]
		var attrs []string
		if p.hash != c.hash {
			attrs = append(attrs, fmt.Sprintf("content hash %s→%s", shortHash(p.hash), shortHash(c.hash)))
		}
		if p.rows != c.rows {
			attrs = append(attrs, fmt.Sprintf("declared row count %d→%d", p.rows, c.rows))
		}
		if !sameDeclaredTotals(p.totals, c.totals) {
			attrs = append(attrs, "declared totals "+declaredTotalsDiff(p.totals, c.totals))
		}
		switch {
		case len(attrs) > 0:
			changed = append(changed, side+" "+strings.Join(attrs, ", "))
		case p.watermark != c.watermark:
			moved = append(moved, side)
		}
	}
	unchanged := fmt.Sprintf("populations unchanged from generation %d", generation-1)
	if len(changed) > 0 {
		return fmt.Sprintf("populations changed since generation %d: %s", generation-1, strings.Join(changed, "; ")), nil
	}
	if len(moved) > 0 {
		return fmt.Sprintf("%s; source watermarks moved for %s", unchanged, strings.Join(moved, ", ")), nil
	}
	return unchanged, nil
}

func shortHash(h string) string {
	if len(h) <= 8 {
		return h
	}
	return h[:8]
}

func sameDeclaredTotals(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for c, v := range a {
		if b[c] != v {
			return false
		}
	}
	return true
}

// declaredTotalsDiff names each currency whose declared total changed, old
// value before new ("USD 100.00→105.00"); an absent side prints as "none".
func declaredTotalsDiff(a, b map[string]string) string {
	ccys := map[string]bool{}
	for c := range a {
		ccys[c] = true
	}
	for c := range b {
		ccys[c] = true
	}
	sorted := make([]string, 0, len(ccys))
	for c := range ccys {
		sorted = append(sorted, c)
	}
	sort.Strings(sorted)
	var out []string
	orNone := func(v string) string {
		if v == "" {
			return "none"
		}
		return v
	}
	for _, ccy := range sorted {
		if a[ccy] != b[ccy] {
			out = append(out, fmt.Sprintf("%s %s→%s", ccy, orNone(a[ccy]), orNone(b[ccy])))
		}
	}
	return strings.Join(out, ", ")
}

// Reperform re-reads both of the run's populations and re-tests the run
// against them (A8). The caller passes populations read through the same
// control-population path StartRun uses (source.BuildPopulations); they are
// verified against the rules the RUN pinned — never the definition's current
// rule version, which may since have moved — and frozen as the run's next
// snapshot generation, leaving every earlier generation immutable beside it.
// The match pass then runs over the new generation (its matches and
// exceptions are the run's current ones; earlier ones stand as history), and
// the run moves to Reperformed, the doc's own distinct lifecycle stage
// between ExceptionsOpen and Certified/Failed. Any material difference the
// re-read found — item content hash, declared row count or declared totals
// against the generation it supersedes — is recorded in the transition's
// reason, never silently absorbed. Certify accepts a run from either Running
// or Reperformed, but only once zero exceptions remain Open.
func (s *PgStore) Reperform(ctx context.Context, tenantID, runID string, pops []domain.FrozenPopulation, actor string,
	claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	var out *domain.ReconciliationRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRunForUpdate(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		if run.Status != domain.RunExceptionsOpen && run.Status != domain.RunRunning {
			return domain.ErrRunNotOpen
		}
		if run.Tolerance == nil || run.RulesSHA256 == "" {
			return domain.ErrLegacyRecord
		}

		claim.ResourceID = runID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		version, err := loadRuleVersion(ctx, tx, tenantID, run.DefinitionID, run.DefinitionVersion)
		if err != nil {
			return err
		}
		if version.RulesSHA256 != run.RulesSHA256 {
			return fmt.Errorf("run %s pinned rules digest %s but rule version %d of %s is sealed as %s",
				runID, run.RulesSHA256, run.DefinitionVersion, run.DefinitionID, version.RulesSHA256)
		}
		if err := checkPopulationsMatchDefinition(&domain.ReconciliationDefinition{Populations: version.Rules.Populations}, pops); err != nil {
			return err
		}

		generation, err := nextSnapshotGeneration(ctx, tx, runID)
		if err != nil {
			return err
		}
		for _, pop := range pops {
			if err := freezePopulation(ctx, tx, tenantID, runID, version.Rules.AmountScale, generation, pop); err != nil {
				return err
			}
		}
		note, err := reperformanceNote(ctx, tx, runID, generation)
		if err != nil {
			return err
		}

		if err := runAutoMatch(ctx, tx, tenantID, actor, run); err != nil {
			return err
		}

		reason := fmt.Sprintf("reperformed against generation %d (%s)", generation, note)
		updated, err := settleResult(ctx, tx, tenantID, run, domain.RunReperformed, reason, actor)
		if err != nil {
			return err
		}
		out = updated
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_run", AggregateID: runID,
			EventType: "CONTROL.ReconciliationCompleted", TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

// ── Certify ──────────────────────────────────────────────────────────────────

const certificationColumns = `certification_id, tenant_id, run_id, definition_version, matched_count, exception_count,
	total_asserted_minor_units, control_totals, result_state, assessment, authority_snapshot, populations, manual_matches,
	manifest_sha256, sealed_at, sealed_by`

func scanCertification(row pgx.Row) (*domain.Certification, error) {
	var c domain.Certification
	var totals, assessment, authority, populations, manual []byte
	if err := row.Scan(&c.CertificationID, &c.TenantID, &c.RunID, &c.DefinitionVersion, &c.MatchedCount, &c.ExceptionCount,
		&c.TotalAssertedMinor, &totals, &c.ResultState, &assessment, &authority, &populations, &manual,
		&c.ManifestSHA256, &c.SealedAt, &c.SealedBy); err != nil {
		return nil, err
	}
	// Certifications sealed before migration 000010 have none of the three; none
	// is reconstructed.
	if len(authority) > 0 {
		c.Authority = &domain.AuthoritySnapshot{}
		if err := json.Unmarshal(authority, c.Authority); err != nil {
			return nil, fmt.Errorf("decode certification authority: %w", err)
		}
	}
	if len(populations) > 0 {
		if err := json.Unmarshal(populations, &c.Populations); err != nil {
			return nil, fmt.Errorf("decode certification populations: %w", err)
		}
	}
	if len(manual) > 0 {
		if err := json.Unmarshal(manual, &c.ManualMatches); err != nil {
			return nil, fmt.Errorf("decode certification manual matches: %w", err)
		}
	}
	if len(assessment) > 0 {
		c.Assessment = &domain.RunAssessment{}
		if err := json.Unmarshal(assessment, c.Assessment); err != nil {
			return nil, fmt.Errorf("decode certification assessment: %w", err)
		}
	}
	if len(totals) > 0 {
		if err := json.Unmarshal(totals, &c.ControlTotals); err != nil {
			return nil, fmt.Errorf("decode certification control totals: %w", err)
		}
	}
	return &c, nil
}

type certificationManifest struct {
	RunID                string                            `json:"run_id"`
	DefinitionVersion    int64                             `json:"definition_version"`
	RulesSHA256          string                            `json:"rules_sha256"`
	AmountScale          int                               `json:"amount_scale"`
	Tolerance            *domain.TolerancePolicy           `json:"tolerance"`
	MatchKeyFields       []string                          `json:"match_key_fields"`
	BalanceDimensions    []string                          `json:"balance_dimensions"`
	Materiality          *domain.MaterialityPolicy         `json:"materiality"`
	CertificationProfile *domain.CertificationProfile      `json:"certification_profile"`
	ResultState          domain.ResultState                `json:"result_state"`
	Assessment           domain.RunAssessment              `json:"assessment"`
	MatchedCount         int64                             `json:"matched_count"`
	ExceptionCount       int64                             `json:"exception_count"`
	ControlTotals        map[string][]domain.CurrencyTotal `json:"control_totals"`
	// A12: the populations certified (latest generation, by content hash), every
	// manual match with its reason and evidence, and the certifier with the
	// authorization decision that let them certify.
	Populations   []domain.CertifiedPopulation  `json:"populations"`
	ManualMatches []domain.CertifiedManualMatch `json:"manual_matches"`
	Certifier     string                        `json:"certifier"`
	Authority     domain.AuthoritySnapshot      `json:"authority_snapshot"`
	SealedAt      time.Time                     `json:"sealed_at"`
}

// sideControlTotals returns each side's per-currency control totals, of the
// run's latest snapshot generation (A8: an earlier generation is history and
// must never be added to the totals being certified). They are never added
// together across sides or currencies (A2).
func sideControlTotals(ctx context.Context, tx pgx.Tx, runID string) (map[string][]domain.CurrencyTotal, error) {
	rows, err := tx.Query(ctx, `SELECT side, control_totals FROM population_snapshots WHERE run_id = $1
		AND generation = (SELECT MAX(generation) FROM population_snapshots WHERE run_id = $1) ORDER BY side`, runID)
	if err != nil {
		return nil, fmt.Errorf("load control totals: %w", err)
	}
	defer rows.Close()
	out := map[string][]domain.CurrencyTotal{}
	for rows.Next() {
		var side string
		var raw []byte
		if err := rows.Scan(&side, &raw); err != nil {
			return nil, err
		}
		var t []domain.CurrencyTotal
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &t); err != nil {
				return nil, fmt.Errorf("decode control totals: %w", err)
			}
		}
		if t == nil {
			t = []domain.CurrencyTotal{}
		}
		out[side] = t
	}
	return out, rows.Err()
}

// certifiedPopulations is every side of the run's latest snapshot generation,
// the generation certified (A8, A12), by snapshot id and content hash.
func certifiedPopulations(ctx context.Context, tx pgx.Tx, runID string) ([]domain.CertifiedPopulation, error) {
	rows, err := tx.Query(ctx, `SELECT `+snapshotColumns+` FROM population_snapshots WHERE run_id = $1
		AND generation = (SELECT MAX(generation) FROM population_snapshots WHERE run_id = $1) ORDER BY side`, runID)
	if err != nil {
		return nil, fmt.Errorf("load certified populations: %w", err)
	}
	defer rows.Close()
	out := []domain.CertifiedPopulation{}
	for rows.Next() {
		p, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, domain.CertifiedPopulation{SnapshotID: p.SnapshotID, Side: p.Side, Generation: p.Generation,
			SourceSystem: p.SourceSystem, SpecRef: p.SpecRef, SourceWatermark: p.SourceWatermark, ItemCount: p.ItemCount,
			DeclaredRowCount: p.DeclaredRowCount, DeclaredTotals: p.DeclaredTotals, ContentHash: p.ContentHash})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: the run has no frozen population to certify", domain.ErrRunNotEligible)
	}
	return out, nil
}

// certifiedManualMatches is every manual match of the run, of any generation
// (an earlier generation's match closed exceptions the assessment counts), with
// its reason, its recorded evidence and the exceptions it resolved.
func certifiedManualMatches(ctx context.Context, tx pgx.Tx, runID string) ([]domain.CertifiedManualMatch, error) {
	rows, err := tx.Query(ctx, `
		SELECT m.match_result_id, m.item_a_id, m.item_b_id, m.reason, m.matched_by, m.matched_at, m.evidence,
			COALESCE(array_agg(e.exception_id ORDER BY e.exception_id) FILTER (WHERE e.exception_id IS NOT NULL), '{}')
		FROM match_results m
		LEFT JOIN reconciliation_exceptions e ON e.resolved_by_match_id = m.match_result_id
		WHERE m.run_id = $1 AND m.manual
		GROUP BY m.match_result_id
		ORDER BY m.matched_at, m.match_result_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("load manual matches: %w", err)
	}
	defer rows.Close()
	out := []domain.CertifiedManualMatch{}
	for rows.Next() {
		var m domain.CertifiedManualMatch
		var evidence []byte
		if err := rows.Scan(&m.MatchResultID, &m.ItemAID, &m.ItemBID, &m.Reason, &m.MatchedBy, &m.MatchedAt, &evidence,
			&m.ResolvedExceptionIDs); err != nil {
			return nil, err
		}
		if len(evidence) > 0 {
			m.Evidence = &domain.ManualMatchEvidence{}
			if err := json.Unmarshal(evidence, m.Evidence); err != nil {
				return nil, fmt.Errorf("decode manual match evidence: %w", err)
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// runPreparers is everyone who prepared the run: who started it, who matched
// in it (auto or manual — matched_by is the principal who ran the command),
// and who moved it out of Planned or into Reperformed. Raising an exception or
// accepting a risk is a finding about the work, not preparing it, so those
// actors are not preparers.
func runPreparers(ctx context.Context, tx pgx.Tx, run *domain.ReconciliationRun) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT started_by FROM reconciliation_runs WHERE run_id = $1
		UNION SELECT matched_by FROM match_results WHERE run_id = $1
		UNION SELECT actor FROM reconciliation_run_transitions
			WHERE run_id = $1 AND dimension = 'LIFECYCLE' AND (from_state = $2 OR to_state = $3)`,
		run.RunID, string(domain.RunPlanned), string(domain.RunReperformed))
	if err != nil {
		return nil, fmt.Errorf("load run preparers: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Certify seals an immutable Certification — only legal from Running or
// Reperformed, and only once zero exceptions remain Open (the doc's
// own "certified runs immutable" core control starts here; the
// reconciliation_runs trigger then enforces it for the rest of the
// run's life). This pass does not model a separate "expected
// assertions" mismatch concept, so Certify never auto-transitions a run
// to Failed on its own — it either certifies or refuses with a clear
// reason, leaving Failed reachable only through an explicit future
// extension rather than an invented, ungoverned auto-fail path.
//
// The certification binds what it certified (A12): the content hash of every
// side of the generation certified, every manual match with its reason and
// evidence, and the certifier's authority — authorization-svc's live decision,
// which the handler obtains and which must be the certifier's own and
// traceable, or nothing is certified.
func (s *PgStore) Certify(ctx context.Context, tenantID, runID string, actor string, authority *domain.AuthoritySnapshot,
	claim domain.IdempotencyClaim) (*domain.Certification, error) {
	if !authority.Traceable(actor) {
		return nil, domain.ErrAuthorityUntraceable
	}
	var out *domain.Certification
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRunForUpdate(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		if run.Status == domain.RunPlanned {
			return domain.ErrRunNotEvaluated
		}
		if !openRunStatuses()[run.Status] {
			return domain.ErrRunNotOpen
		}
		// Re-assessed under the run's lock: a MATCH, or a VARIANCE that is only
		// accepted risk the pinned profile permits and that is immaterial in
		// aggregate (domain.Assess). An exception still being worked blocks.
		assessment, err := assessRun(ctx, tx, run)
		if err != nil {
			return err
		}
		for st, n := range assessment.Unresolved {
			if st.Working() && n > 0 {
				return domain.ErrOpenExceptionsRemain
			}
		}
		if !assessment.Certifiable || run.CertificationState != domain.CertEligible {
			return fmt.Errorf("%w: %s", domain.ErrRunNotEligible, strings.Join(assessment.CertificationBlocks, "; "))
		}
		// Maker-checker, under the run's lock so no preparation can land between
		// the check and the seal (the handler also requires RECONCILIATION_CERTIFY).
		preparers, err := runPreparers(ctx, tx, run)
		if err != nil {
			return err
		}
		if err := domain.CheckCertifierIndependence(actor, preparers); err != nil {
			return err
		}
		assessmentJSON, err := json.Marshal(assessment)
		if err != nil {
			return fmt.Errorf("encode assessment: %w", err)
		}

		claim.ResourceID = runID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		var matchedCount, exceptionCount int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM match_results WHERE run_id = $1`, runID).Scan(&matchedCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_exceptions WHERE run_id = $1`, runID).Scan(&exceptionCount); err != nil {
			return err
		}
		totals, err := sideControlTotals(ctx, tx, runID)
		if err != nil {
			return err
		}
		totalsJSON, err := json.Marshal(totals)
		if err != nil {
			return fmt.Errorf("encode control totals: %w", err)
		}
		populations, err := certifiedPopulations(ctx, tx, runID)
		if err != nil {
			return err
		}
		manual, err := certifiedManualMatches(ctx, tx, runID)
		if err != nil {
			return err
		}
		authorityJSON, err := json.Marshal(authority)
		if err != nil {
			return fmt.Errorf("encode certification authority: %w", err)
		}
		populationsJSON, err := json.Marshal(populations)
		if err != nil {
			return fmt.Errorf("encode certified populations: %w", err)
		}
		manualJSON, err := json.Marshal(manual)
		if err != nil {
			return fmt.Errorf("encode manual matches: %w", err)
		}

		sealedAt := time.Now().UTC()
		manifest, err := json.Marshal(certificationManifest{
			RunID: runID, DefinitionVersion: run.DefinitionVersion, RulesSHA256: run.RulesSHA256, AmountScale: run.AmountScale, Tolerance: run.Tolerance,
			MatchKeyFields: run.MatchKeyFields, BalanceDimensions: run.BalanceDimensions,
			Materiality: run.Materiality, CertificationProfile: run.CertificationProfile, ResultState: run.ResultState,
			Assessment:   assessment,
			MatchedCount: matchedCount, ExceptionCount: exceptionCount, ControlTotals: totals,
			Populations: populations, ManualMatches: manual, Certifier: actor, Authority: *authority, SealedAt: sealedAt,
		})
		if err != nil {
			return fmt.Errorf("marshal certification manifest: %w", err)
		}

		certID := newID(domain.PrefixCertification)
		cert, err := scanCertification(tx.QueryRow(ctx, `
			INSERT INTO certifications (certification_id, tenant_id, run_id, definition_version, matched_count,
				exception_count, control_totals, result_state, assessment, authority_snapshot, populations, manual_matches,
				manifest_sha256, sealed_at, sealed_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			RETURNING `+certificationColumns,
			certID, tenantID, runID, run.DefinitionVersion, matchedCount, exceptionCount, totalsJSON, string(run.ResultState),
			assessmentJSON, authorityJSON, populationsJSON, manualJSON, sha256Hex(manifest), sealedAt, actor))
		if err != nil {
			return fmt.Errorf("seal certification: %w", err)
		}

		reason := "certified"
		if run.ResultState == domain.ResultVariance {
			reason = fmt.Sprintf("certified as VARIANCE with %d accepted risk(s)", len(assessment.AcceptedRiskIDs))
		}
		if _, err := advanceRun(ctx, tx, tenantID, run, runAdvance{To: domain.RunCertified, Result: run.ResultState,
			Cert: domain.CertCertified, Reason: reason, Actor: actor, CertifiedBy: &actor,
			AcceptedRiskCertifiable: run.ResultState == domain.ResultVariance}); err != nil {
			return err
		}

		out = cert
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "certification", AggregateID: cert.CertificationID,
			EventType: "CONTROL.Certified", TenantID: &tenantID, Payload: cert})
	})
	return out, err
}

func (s *PgStore) GetCertification(ctx context.Context, tenantID, runID string) (*domain.Certification, error) {
	var out *domain.Certification
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		c, err := scanCertification(tx.QueryRow(ctx, `SELECT `+certificationColumns+` FROM certifications WHERE tenant_id = $1 AND run_id = $2`,
			tenantID, runID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRunNotFound
		}
		out = c
		return err
	})
	return out, err
}

// ── SupersedeRun ─────────────────────────────────────────────────────────────

// SupersedeRun marks a Certified or Failed run as superseded by a
// replacement run, without altering its own sealed evidence — the same
// append-only correction pattern used for lineage edges and ingestion
// runs elsewhere in DATA-0x. If remediation needs to happen, it happens
// in the owning domain; this only records that this run's result has
// been superseded, by what, and by whom.
//
// The replacement must reconcile the same thing (same definition) and must
// itself have reached a terminal state (Certified or Failed): a result is
// never superseded by one that does not exist yet (A13). A replacement
// supersedes at most one run. Both runs are locked in run_id order so two
// concurrent supersessions cannot deadlock on each other.
func (s *PgStore) SupersedeRun(ctx context.Context, tenantID, runID string, req domain.SupersedeRunRequest, actor string, claim domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if req.NewRunID == runID {
		return nil, domain.ErrSupersedeSelf
	}
	var out *domain.ReconciliationRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		locked := map[string]*domain.ReconciliationRun{}
		ids := []string{runID, req.NewRunID}
		sort.Strings(ids)
		for _, id := range ids {
			r, err := loadRunForUpdate(ctx, tx, tenantID, id)
			if err != nil {
				return err
			}
			locked[id] = r
		}
		run, replacement := locked[runID], locked[req.NewRunID]
		if run.Status != domain.RunCertified && run.Status != domain.RunFailed {
			return domain.ErrRunNotOpen
		}
		if replacement.DefinitionID != run.DefinitionID {
			return domain.ErrSupersedeDifferentDefinition
		}
		if replacement.Status != domain.RunCertified && replacement.Status != domain.RunFailed {
			return domain.ErrReplacementNotTerminal
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reconciliation_runs WHERE tenant_id = $1 AND superseded_by_run_id = $2)`,
			tenantID, req.NewRunID).Scan(&used); err != nil {
			return fmt.Errorf("check replacement run: %w", err)
		}
		if used {
			return domain.ErrReplacementAlreadyUsed
		}

		claim.ResourceID = runID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		// The run's own result stays in its transition history and, if it was
		// certified, in its sealed certification; only its current state moves.
		cert := run.CertificationState
		if cert == domain.CertCertified {
			cert = domain.CertSuperseded
		}
		updated, err := advanceRun(ctx, tx, tenantID, run, runAdvance{To: domain.RunSuperseded, Result: domain.ResultSuperseded,
			Cert: cert, Reason: "superseded by " + req.NewRunID + ": " + req.Reason, Actor: actor,
			Supersession: &supersession{ByRunID: req.NewRunID, Reason: req.Reason, By: actor}})
		if err != nil {
			return err
		}
		out = updated
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "reconciliation_run", AggregateID: runID,
			EventType: "CONTROL.ReconciliationSuperseded", TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

// ── Read surfaces ────────────────────────────────────────────────────────────

const snapshotColumns = `snapshot_id, tenant_id, run_id, side, generation, source_system, spec_ref, source_watermark, item_count, amount_scale,
	total_amount_minor_units, control_totals, declared_row_count, declared_totals, content_hash, created_at`

func scanSnapshot(row pgx.Row) (*domain.PopulationSnapshot, error) {
	var p domain.PopulationSnapshot
	var specRef, watermark *string
	var control, declared []byte
	if err := row.Scan(&p.SnapshotID, &p.TenantID, &p.RunID, &p.Side, &p.Generation, &p.SourceSystem, &specRef, &watermark, &p.ItemCount,
		&p.AmountScale, &p.TotalAmountMinorUnits, &control, &p.DeclaredRowCount, &declared, &p.ContentHash, &p.CreatedAt); err != nil {
		return nil, err
	}
	if specRef != nil {
		p.SpecRef = *specRef
	}
	if watermark != nil {
		p.SourceWatermark = *watermark
	}
	if len(control) > 0 {
		if err := json.Unmarshal(control, &p.ControlTotals); err != nil {
			return nil, fmt.Errorf("decode control totals: %w", err)
		}
	}
	if len(declared) > 0 {
		if err := json.Unmarshal(declared, &p.DeclaredTotals); err != nil {
			return nil, fmt.Errorf("decode declared totals: %w", err)
		}
	}
	return &p, nil
}

// GetPopulationSnapshots returns every frozen generation of both sides —
// a reperformance freezes a new generation and the earlier ones stay (A8),
// so the run's paper trail is the whole series — ordered by side and
// generation.
func (s *PgStore) GetPopulationSnapshots(ctx context.Context, tenantID, runID string) ([]domain.PopulationSnapshot, error) {
	var out []domain.PopulationSnapshot
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+snapshotColumns+` FROM population_snapshots WHERE tenant_id = $1 AND run_id = $2 ORDER BY side, generation`,
			tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanSnapshot(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	return out, err
}

const matchResultColumns = `match_result_id, tenant_id, run_id, item_a_id, item_b_id, ref_id, manual, reason, matched_by, matched_at, evidence`

func scanMatchResult(row pgx.Row) (*domain.MatchResult, error) {
	var m domain.MatchResult
	var reason *string
	var evidence []byte
	if err := row.Scan(&m.MatchResultID, &m.TenantID, &m.RunID, &m.ItemAID, &m.ItemBID, &m.RefID, &m.Manual, &reason,
		&m.MatchedBy, &m.MatchedAt, &evidence); err != nil {
		return nil, err
	}
	if reason != nil {
		m.Reason = *reason
	}
	if len(evidence) > 0 {
		m.Evidence = &domain.ManualMatchEvidence{}
		if err := json.Unmarshal(evidence, m.Evidence); err != nil {
			return nil, fmt.Errorf("decode manual match evidence: %w", err)
		}
	}
	return &m, nil
}

func (s *PgStore) GetMatchResults(ctx context.Context, tenantID, runID string) ([]domain.MatchResult, error) {
	var out []domain.MatchResult
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+matchResultColumns+` FROM match_results WHERE tenant_id = $1 AND run_id = $2 ORDER BY matched_at`,
			tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMatchResult(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) GetTransitions(ctx context.Context, tenantID, runID string) ([]domain.RunTransition, error) {
	var out []domain.RunTransition
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT transition_id, run_id, dimension, from_state, to_state, reason, actor, occurred_at
			FROM reconciliation_run_transitions WHERE tenant_id = $1 AND run_id = $2 ORDER BY transition_id`, tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.RunTransition
			if err := rows.Scan(&t.TransitionID, &t.RunID, &t.Dimension, &t.FromState, &t.ToState, &t.Reason, &t.Actor,
				&t.OccurredAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}
