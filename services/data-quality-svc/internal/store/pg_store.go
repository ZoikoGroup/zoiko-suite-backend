// Package store is the PgStore persistence layer for data-quality-svc
// (DATA-02, ZS-SVC-N-001 §4). Every method runs inside one transaction
// that first declares app.tenant_id for RLS, then performs the write.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/outbox"
)

// Store is the DATA-02 persistence contract.
type Store interface {
	PublishRuleSetVersion(ctx context.Context, tenantID, ruleSetName string, rules []domain.RuleDefinition, actor string, claim domain.IdempotencyClaim) (*domain.DQRuleSetVersion, error)
	GetRuleSetVersion(ctx context.Context, tenantID, versionID string) (*domain.DQRuleSetVersion, error)

	RunDQ(ctx context.Context, tenantID string, req domain.RunDQRequest, actor string, claim domain.IdempotencyClaim) (*domain.DQRun, error)
	RaiseIssue(ctx context.Context, tenantID string, req domain.RaiseIssueRequest, actor string, claim domain.IdempotencyClaim) (*domain.DQIssue, error)
	AssignIssue(ctx context.Context, tenantID string, req domain.AssignIssueRequest, claim domain.IdempotencyClaim) (*domain.DQIssue, error)
	Reperform(ctx context.Context, tenantID, oldRunID string, req domain.RunDQRequest, actor string, claim domain.IdempotencyClaim) (*domain.DQRun, error)
	CertifyDQ(ctx context.Context, tenantID, runID, actor string, claim domain.IdempotencyClaim) (*domain.DQCertification, error)

	GetRun(ctx context.Context, tenantID, runID string) (*domain.DQRun, error)
	GetResults(ctx context.Context, tenantID, runID string) ([]domain.DQResult, error)
	GetIssues(ctx context.Context, tenantID, runID string) ([]domain.DQIssue, error)
	GetCertification(ctx context.Context, tenantID, runID string) (*domain.DQCertification, error)
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
	switch {
	case strings.Contains(pgErr.Message, "terminal"):
		return fmt.Errorf("%w: %s", domain.ErrRunAlreadyCertified, pgErr.Message)
	case strings.Contains(pgErr.Message, "immutable"), strings.Contains(pgErr.Message, "cannot be deleted"),
		strings.Contains(pgErr.Message, "may only"), strings.Contains(pgErr.Message, "is already"):
		return fmt.Errorf("%w: %s", domain.ErrImmutableViolation, pgErr.Message)
	}
	return err
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

// ── Rule sets / versions ─────────────────────────────────────────────────────

// PublishRuleSetVersion finds-or-creates the named rule set, then always
// creates a NEW version row — an existing published version is never
// edited in place (the doc's own named acceptance test: "DQ rule-set
// change creates a new run/version").
func (s *PgStore) PublishRuleSetVersion(ctx context.Context, tenantID, ruleSetName string, rules []domain.RuleDefinition, actor string, claim domain.IdempotencyClaim) (*domain.DQRuleSetVersion, error) {
	if ruleSetName == "" {
		return nil, fmt.Errorf("ruleset name is required")
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("at least one rule is required")
	}
	var out *domain.DQRuleSetVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		ruleSetID := newID(domain.PrefixRuleSet)
		got, err := tx.Exec(ctx, `
			INSERT INTO dq_rule_sets (ruleset_id, tenant_id, name, created_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id, name) DO NOTHING`,
			ruleSetID, tenantID, ruleSetName, actor)
		if err != nil {
			return fmt.Errorf("find-or-create rule set: %w", err)
		}
		if got.RowsAffected() == 0 {
			if err := tx.QueryRow(ctx, `SELECT ruleset_id FROM dq_rule_sets WHERE tenant_id = $1 AND name = $2`,
				tenantID, ruleSetName).Scan(&ruleSetID); err != nil {
				return fmt.Errorf("look up rule set: %w", err)
			}
		}

		versionID := newID(domain.PrefixRuleSetVersion)
		claim.ResourceID = versionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		var nextVersion int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_number), 0) + 1 FROM dq_rule_set_versions WHERE tenant_id = $1 AND ruleset_id = $2`,
			tenantID, ruleSetID).Scan(&nextVersion); err != nil {
			return fmt.Errorf("compute next version number: %w", err)
		}

		rulesJSON, err := json.Marshal(rules)
		if err != nil {
			return err
		}
		var v domain.DQRuleSetVersion
		if err := tx.QueryRow(ctx, `
			INSERT INTO dq_rule_set_versions (version_id, tenant_id, ruleset_id, version_number, rules, published_by)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6)
			RETURNING version_id, tenant_id, ruleset_id, version_number, published_at, published_by`,
			versionID, tenantID, ruleSetID, nextVersion, rulesJSON, actor,
		).Scan(&v.VersionID, &v.TenantID, &v.RuleSetID, &v.VersionNumber, &v.PublishedAt, &v.PublishedBy); err != nil {
			return fmt.Errorf("insert rule set version: %w", err)
		}
		v.Rules = rules
		out = &v
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_rule_set_version", AggregateID: v.VersionID,
			EventType: "DATA.DQRuleSetVersionPublished", TenantID: &tenantID, Payload: v})
	})
	return out, err
}

func (s *PgStore) GetRuleSetVersion(ctx context.Context, tenantID, versionID string) (*domain.DQRuleSetVersion, error) {
	var out *domain.DQRuleSetVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		v, err := loadRuleSetVersion(ctx, tx, versionID)
		out = v
		return err
	})
	return out, err
}

func loadRuleSetVersion(ctx context.Context, tx pgx.Tx, versionID string) (*domain.DQRuleSetVersion, error) {
	var v domain.DQRuleSetVersion
	var rulesJSON []byte
	err := tx.QueryRow(ctx, `SELECT version_id, tenant_id, ruleset_id, version_number, rules, published_at, published_by
		FROM dq_rule_set_versions WHERE version_id = $1`, versionID,
	).Scan(&v.VersionID, &v.TenantID, &v.RuleSetID, &v.VersionNumber, &rulesJSON, &v.PublishedAt, &v.PublishedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRuleSetVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rulesJSON, &v.Rules); err != nil {
		return nil, err
	}
	return &v, nil
}

// ── RunDQ ────────────────────────────────────────────────────────────────────

// evaluateOutcome decides Pass/Fail for one submitted rule outcome against
// its rule definition's declared thresholds:
//   - FailCount == 0 -> always Pass.
//   - FailCount > 0 and the rule has NO aggregate_materiality_threshold ->
//     Fail (strict default: any failure fails).
//   - FailCount > 0 and the rule HAS an aggregate_materiality_threshold ->
//     Fail only if failure_magnitude_sum exceeds that threshold; a sum at
//     or below it is tolerated (immaterial in aggregate) and Passes. This
//     is what lets many individually-tiny failures still surface as a
//     failure once their SUM crosses the threshold — the doc's own named
//     acceptance test.
func evaluateOutcome(def *domain.RuleDefinition, ro domain.RuleOutcome) (domain.DQResultStatus, error) {
	if ro.FailCount == 0 {
		return domain.ResultPass, nil
	}
	if def == nil || def.AggregateMaterialityThreshold == nil {
		return domain.ResultFail, nil
	}
	sum, ok := new(big.Rat).SetString(ro.FailureMagnitudeSum)
	if !ok {
		return "", fmt.Errorf("rule %s: failure_magnitude_sum %q is not a decimal", ro.RuleKey, ro.FailureMagnitudeSum)
	}
	threshold, ok := new(big.Rat).SetString(*def.AggregateMaterialityThreshold)
	if !ok {
		return "", fmt.Errorf("rule %s: aggregate_materiality_threshold %q is not a decimal", ro.RuleKey, *def.AggregateMaterialityThreshold)
	}
	if sum.Cmp(threshold) > 0 {
		return domain.ResultFail, nil
	}
	return domain.ResultPass, nil
}

// RunDQ receives caller-computed rule evidence (see the package doc
// comment for why this service never computes rule outcomes itself),
// binds it to an immutable rule-set version and a frozen population, and
// persists one DQResult per submitted outcome with server-evaluated
// materiality.
func (s *PgStore) RunDQ(ctx context.Context, tenantID string, req domain.RunDQRequest, actor string, claim domain.IdempotencyClaim) (*domain.DQRun, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.DQRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := runDQInTx(ctx, tx, tenantID, req, nil, actor, claim)
		out = run
		return err
	})
	return out, err
}

// runDQInTx is RunDQ's actual logic, taking an ALREADY-OPEN transaction —
// so Reperform can run it as one step of its own single transaction
// (mark the old run Reperformed AND insert the new run atomically) rather
// than nesting a second independent transaction, which would both break
// atomicity (the new run could commit while the old run's status update
// rolls back, or vice versa) and risk exhausting the connection pool by
// holding two connections for one logical operation.
func runDQInTx(ctx context.Context, tx pgx.Tx, tenantID string, req domain.RunDQRequest, supersedesRunID *string, actor string, claim domain.IdempotencyClaim) (*domain.DQRun, error) {
	version, err := loadRuleSetVersion(ctx, tx, req.RuleSetVersionID)
	if err != nil {
		return nil, err
	}
	defByKey := map[string]*domain.RuleDefinition{}
	for i := range version.Rules {
		defByKey[version.Rules[i].RuleKey] = &version.Rules[i]
	}

	type evaluated struct {
		outcome domain.RuleOutcome
		status  domain.DQResultStatus
	}
	results := make([]evaluated, 0, len(req.RuleOutcomes))
	anyFail := false
	for _, ro := range req.RuleOutcomes {
		status, err := evaluateOutcome(defByKey[ro.RuleKey], ro)
		if err != nil {
			return nil, err
		}
		if status == domain.ResultFail {
			anyFail = true
		}
		results = append(results, evaluated{outcome: ro, status: status})
	}

	runID := newID(domain.PrefixRun)
	claim.ResourceID = runID
	if err := claimIdempotency(ctx, tx, claim); err != nil {
		return nil, err
	}

	finalStatus := domain.RunRunning
	if anyFail {
		finalStatus = domain.RunExceptionsOpen
	}
	now := time.Now().UTC()
	var run domain.DQRun
	if err := tx.QueryRow(ctx, `
		INSERT INTO dq_runs (run_id, tenant_id, ruleset_version_id, population_ref, population_row_count,
			population_content_hash, status, supersedes_run_id, started_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING run_id, tenant_id, ruleset_version_id, population_ref, population_row_count,
			population_content_hash, status, supersedes_run_id, started_at, completed_at, created_by`,
		runID, tenantID, req.RuleSetVersionID, req.PopulationRef, req.PopulationRowCount,
		req.PopulationContentHash, finalStatus, supersedesRunID, now, actor,
	).Scan(&run.RunID, &run.TenantID, &run.RuleSetVersionID, &run.PopulationRef, &run.PopulationRowCount,
		&run.PopulationContentHash, &run.Status, &run.SupersedesRunID, &run.StartedAt, &run.CompletedAt, &run.CreatedBy); err != nil {
		return nil, fmt.Errorf("insert dq run: %w", err)
	}

	for _, r := range results {
		failingJSON, err := json.Marshal(r.outcome.FailingRecordRefs)
		if err != nil {
			return nil, err
		}
		resultID := newID(domain.PrefixResult)
		if _, err := tx.Exec(ctx, `
			INSERT INTO dq_results (result_id, tenant_id, run_id, rule_key, status, pass_count, fail_count,
				failure_magnitude_sum, failing_record_refs)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9::jsonb)`,
			resultID, tenantID, runID, r.outcome.RuleKey, r.status, r.outcome.PassCount, r.outcome.FailCount,
			r.outcome.FailureMagnitudeSum, failingJSON); err != nil {
			return nil, fmt.Errorf("insert dq result: %w", err)
		}
	}

	eventType := "DATA.DQRunCompleted"
	if anyFail {
		eventType = "DATA.DQFailed"
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_run", AggregateID: run.RunID,
		EventType: eventType, TenantID: &tenantID, Payload: run}); err != nil {
		return nil, err
	}
	return &run, nil
}

const runColumns = `run_id, tenant_id, ruleset_version_id, population_ref, population_row_count,
	population_content_hash, status, supersedes_run_id, started_at, completed_at, created_by`

func scanRun(row pgx.Row) (*domain.DQRun, error) {
	var run domain.DQRun
	if err := row.Scan(&run.RunID, &run.TenantID, &run.RuleSetVersionID, &run.PopulationRef, &run.PopulationRowCount,
		&run.PopulationContentHash, &run.Status, &run.SupersedesRunID, &run.StartedAt, &run.CompletedAt, &run.CreatedBy); err != nil {
		return nil, err
	}
	return &run, nil
}

func loadRun(ctx context.Context, tx pgx.Tx, runID string, forUpdate bool) (*domain.DQRun, error) {
	q := `SELECT ` + runColumns + ` FROM dq_runs WHERE run_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	run, err := scanRun(tx.QueryRow(ctx, q, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRunNotFound
	}
	return run, err
}

func (s *PgStore) GetRun(ctx context.Context, tenantID, runID string) (*domain.DQRun, error) {
	var out *domain.DQRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		r, err := loadRun(ctx, tx, runID, false)
		out = r
		return err
	})
	return out, err
}

const resultColumns = `result_id, tenant_id, run_id, rule_key, status, pass_count, fail_count, failure_magnitude_sum, failing_record_refs, evaluated_at`

func scanResult(row pgx.Row) (*domain.DQResult, error) {
	var res domain.DQResult
	var failingJSON []byte
	if err := row.Scan(&res.ResultID, &res.TenantID, &res.RunID, &res.RuleKey, &res.Status, &res.PassCount,
		&res.FailCount, &res.FailureMagnitudeSum, &failingJSON, &res.EvaluatedAt); err != nil {
		return nil, err
	}
	if len(failingJSON) > 0 {
		if err := json.Unmarshal(failingJSON, &res.FailingRecordRefs); err != nil {
			return nil, err
		}
	}
	return &res, nil
}

func loadResult(ctx context.Context, tx pgx.Tx, resultID string) (*domain.DQResult, error) {
	res, err := scanResult(tx.QueryRow(ctx, `SELECT `+resultColumns+` FROM dq_results WHERE result_id = $1`, resultID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrResultNotFound
	}
	return res, err
}

func (s *PgStore) GetResults(ctx context.Context, tenantID, runID string) ([]domain.DQResult, error) {
	var out []domain.DQResult
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+resultColumns+` FROM dq_results WHERE run_id = $1 ORDER BY evaluated_at`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanResult(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// ── Issues ───────────────────────────────────────────────────────────────────

const issueColumns = `issue_id, tenant_id, run_id, result_id, description, status, assignee, raised_at, raised_by`

func scanIssue(row pgx.Row) (*domain.DQIssue, error) {
	var i domain.DQIssue
	if err := row.Scan(&i.IssueID, &i.TenantID, &i.RunID, &i.ResultID, &i.Description, &i.Status, &i.Assignee,
		&i.RaisedAt, &i.RaisedBy); err != nil {
		return nil, err
	}
	return &i, nil
}

// RaiseIssue creates a DQIssue from a failing result. The run must be
// ExceptionsOpen (i.e. genuinely have failures) and the result must
// actually belong to that run and be a Fail — an issue can't be raised
// against a passing result or a foreign run.
func (s *PgStore) RaiseIssue(ctx context.Context, tenantID string, req domain.RaiseIssueRequest, actor string, claim domain.IdempotencyClaim) (*domain.DQIssue, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.DQIssue
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRun(ctx, tx, req.RunID, false)
		if err != nil {
			return err
		}
		if run.Status != domain.RunExceptionsOpen {
			return fmt.Errorf("%w: run must be ExceptionsOpen to raise an issue", domain.ErrRunNotInExceptionsOpen)
		}
		result, err := loadResult(ctx, tx, req.ResultID)
		if err != nil {
			return err
		}
		if result.RunID != req.RunID {
			return domain.ErrResultNotFound
		}
		if result.Status != domain.ResultFail {
			return fmt.Errorf("%w: result %s is not a Fail", domain.ErrResultNotFound, req.ResultID)
		}

		issueID := newID(domain.PrefixIssue)
		claim.ResourceID = issueID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		got, err := scanIssue(tx.QueryRow(ctx, `
			INSERT INTO dq_issues (issue_id, tenant_id, run_id, result_id, description, raised_by)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING `+issueColumns,
			issueID, tenantID, req.RunID, req.ResultID, req.Description, actor))
		if err != nil {
			return fmt.Errorf("insert dq issue: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_issue", AggregateID: got.IssueID,
			EventType: "DATA.DQIssueRaised", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

// AssignIssue is the ONLY mutation an issue ever undergoes: Open ->
// Assigned, exactly once. There is deliberately no "resolve" command —
// see domain.DQIssue's doc comment.
func (s *PgStore) AssignIssue(ctx context.Context, tenantID string, req domain.AssignIssueRequest, claim domain.IdempotencyClaim) (*domain.DQIssue, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.DQIssue
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var status domain.DQIssueStatus
		if err := tx.QueryRow(ctx, `SELECT status FROM dq_issues WHERE issue_id = $1 FOR UPDATE`, req.IssueID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrIssueNotFound
			}
			return err
		}
		if status != domain.IssueOpen {
			return domain.ErrIssueAlreadyAssigned
		}

		claim.ResourceID = req.IssueID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		got, err := scanIssue(tx.QueryRow(ctx, `
			UPDATE dq_issues SET status = 'Assigned', assignee = $2 WHERE issue_id = $1
			RETURNING `+issueColumns, req.IssueID, req.Assignee))
		if err != nil {
			return fmt.Errorf("assign dq issue: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_issue", AggregateID: got.IssueID,
			EventType: "DATA.DQIssueAssigned", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetIssues(ctx context.Context, tenantID, runID string) ([]domain.DQIssue, error) {
	var out []domain.DQIssue
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+issueColumns+` FROM dq_issues WHERE run_id = $1 ORDER BY raised_at`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			i, err := scanIssue(rows)
			if err != nil {
				return err
			}
			out = append(out, *i)
		}
		return rows.Err()
	})
	return out, err
}

// ── Reperform ────────────────────────────────────────────────────────────────

// Reperform creates a brand NEW DQRun (fresh evidence, submitted the same
// way as RunDQ) referencing oldRunID via SupersedesRunID, then marks the
// old run Reperformed — the old run's own rows are never mutated beyond
// that single status transition, and remain independently queryable.
// Remediation of the underlying source defect happens entirely in the
// owning domain before this is called; this service only records that a
// fresh evaluation was submitted.
func (s *PgStore) Reperform(ctx context.Context, tenantID, oldRunID string, req domain.RunDQRequest, actor string, claim domain.IdempotencyClaim) (*domain.DQRun, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.DQRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		oldRun, err := loadRun(ctx, tx, oldRunID, true)
		if err != nil {
			return err
		}
		if oldRun.Status != domain.RunExceptionsOpen && oldRun.Status != domain.RunRunning {
			return fmt.Errorf("%w: run %s is %s and cannot be reperformed", domain.ErrImmutableViolation, oldRunID, oldRun.Status)
		}
		if req.RuleSetVersionID != oldRun.RuleSetVersionID {
			return fmt.Errorf("reperform must use the same ruleset_version_id as the run it supersedes")
		}

		newRun, err := runDQInTx(ctx, tx, tenantID, req, &oldRunID, actor, claim)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `UPDATE dq_runs SET status = $2 WHERE run_id = $1`, oldRunID, domain.RunReperformed); err != nil {
			return fmt.Errorf("mark old run reperformed: %w", err)
		}
		out = newRun
		return nil
	})
	return out, err
}

// ── CertifyDQ ────────────────────────────────────────────────────────────────

// CertifyDQ seals a DQCertification for a run with no open issues.
// Immutable once sealed — same doctrine as every other sealed-evidence
// table in this repo.
func (s *PgStore) CertifyDQ(ctx context.Context, tenantID, runID, actor string, claim domain.IdempotencyClaim) (*domain.DQCertification, error) {
	var out *domain.DQCertification
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRun(ctx, tx, runID, true)
		if err != nil {
			return err
		}
		if run.Status != domain.RunRunning {
			return fmt.Errorf("%w: run must be Running (clean, no failing results) to certify; it is %s", domain.ErrRunHasOpenIssues, run.Status)
		}
		var openIssues int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dq_issues WHERE run_id = $1 AND status = 'Open'`, runID).Scan(&openIssues); err != nil {
			return err
		}
		if openIssues > 0 {
			return domain.ErrRunHasOpenIssues
		}

		results, err := loadResultsForRun(ctx, tx, runID)
		if err != nil {
			return err
		}

		certID := newID(domain.PrefixCertification)
		claim.ResourceID = certID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		summary := domain.DQCertificationSummary{
			RunID: run.RunID, RuleSetVersionID: run.RuleSetVersionID, PopulationRef: run.PopulationRef,
			PopulationRowCount: run.PopulationRowCount, PopulationContentHash: run.PopulationContentHash, Results: results,
		}
		summaryJSON, err := json.Marshal(summary)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(summaryJSON)
		summarySHA256 := hex.EncodeToString(sum[:])
		now := time.Now().UTC()

		var cert domain.DQCertification
		if err := tx.QueryRow(ctx, `
			INSERT INTO dq_certifications (certification_id, tenant_id, run_id, summary, summary_sha256, certified_at, certified_by)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
			RETURNING certification_id, tenant_id, run_id, summary_sha256, certified_at, certified_by`,
			certID, tenantID, runID, summaryJSON, summarySHA256, now, actor,
		).Scan(&cert.CertificationID, &cert.TenantID, &cert.RunID, &cert.SummarySHA256, &cert.CertifiedAt, &cert.CertifiedBy); err != nil {
			return fmt.Errorf("insert dq certification: %w", err)
		}
		cert.Summary = summary

		if _, err := tx.Exec(ctx, `UPDATE dq_runs SET status = 'Certified', completed_at = $2 WHERE run_id = $1`, runID, now); err != nil {
			return fmt.Errorf("mark run certified: %w", err)
		}

		out = &cert
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_certification", AggregateID: cert.CertificationID,
			EventType: "DATA.DQCertified", TenantID: &tenantID, Payload: cert})
	})
	return out, err
}

func loadResultsForRun(ctx context.Context, tx pgx.Tx, runID string) ([]domain.DQResult, error) {
	rows, err := tx.Query(ctx, `SELECT `+resultColumns+` FROM dq_results WHERE run_id = $1 ORDER BY evaluated_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.DQResult
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *PgStore) GetCertification(ctx context.Context, tenantID, runID string) (*domain.DQCertification, error) {
	var out *domain.DQCertification
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var cert domain.DQCertification
		var summaryJSON []byte
		err := tx.QueryRow(ctx, `SELECT certification_id, tenant_id, run_id, summary, summary_sha256, certified_at, certified_by
			FROM dq_certifications WHERE run_id = $1`, runID,
		).Scan(&cert.CertificationID, &cert.TenantID, &cert.RunID, &summaryJSON, &cert.SummarySHA256, &cert.CertifiedAt, &cert.CertifiedBy)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCertificationNotFound
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(summaryJSON, &cert.Summary); err != nil {
			return err
		}
		out = &cert
		return nil
	})
	return out, err
}
