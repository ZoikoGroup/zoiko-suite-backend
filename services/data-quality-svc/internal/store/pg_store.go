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
	"sort"
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
	CertifyDQ(ctx context.Context, tenantID, runID, actor string, req domain.CertifyDQRequest, claim domain.IdempotencyClaim) (*domain.DQCertification, error)

	CreateContract(ctx context.Context, tenantID string, req domain.CreateContractRequest, actor string, claim domain.IdempotencyClaim) (*domain.ContractVersion, error)
	SubmitContract(ctx context.Context, tenantID, assetID string, version int, actor string, claim domain.IdempotencyClaim) (*domain.ContractVersion, error)
	PublishContract(ctx context.Context, tenantID, assetID string, version int, actor string, req domain.PublishContractRequest, claim domain.IdempotencyClaim) (*domain.ContractVersion, error)
	DeprecateContract(ctx context.Context, tenantID, assetID string, version int, actor, note string, claim domain.IdempotencyClaim) (*domain.ContractVersion, error)
	RetireContract(ctx context.Context, tenantID, assetID string, version int, actor, note string, claim domain.IdempotencyClaim) (*domain.ContractVersion, error)
	GetContractVersion(ctx context.Context, tenantID, assetID string, version int) (*domain.ContractVersion, error)
	ListContractVersions(ctx context.Context, tenantID, assetID string) ([]domain.ContractVersion, error)
	CheckContractCompatibility(ctx context.Context, tenantID, assetID string, req domain.CompatibilityCheckRequest) (*domain.CompatibilityReport, error)

	GetRun(ctx context.Context, tenantID, runID string) (*domain.DQRun, error)
	GetResults(ctx context.Context, tenantID, runID string) ([]domain.DQResult, error)
	GetIssues(ctx context.Context, tenantID, runID string) ([]domain.DQIssue, error)
	GetCertification(ctx context.Context, tenantID, runID string) (*domain.DQCertification, error)
	RecordObservation(ctx context.Context, tenantID string, req domain.RecordObservationRequest, actor string, claim domain.IdempotencyClaim) (*domain.DQObservation, error)
	GetObservation(ctx context.Context, tenantID, observationRunID string) (*domain.DQObservation, error)
	RequestRiskAcceptance(ctx context.Context, tenantID, runID, actor string, req domain.RequestRiskAcceptanceRequest, claim domain.IdempotencyClaim) (*domain.RiskAcceptance, error)
	ApproveRiskAcceptance(ctx context.Context, tenantID, acceptanceID, actor string, req domain.ApproveRiskAcceptanceRequest, claim domain.IdempotencyClaim) (*domain.RiskAcceptance, error)
	ListRiskAcceptances(ctx context.Context, tenantID, runID string) ([]domain.RiskAcceptance, error)
	RevokeCertification(ctx context.Context, tenantID, runID, actor string, req domain.RevokeCertificationRequest, claim domain.IdempotencyClaim) (*domain.CertificationRevocation, error)
}

// DefaultEvidenceMaxAge is the freshness window applied when none is configured:
// evidence older than this cannot be certified (ZS-SVC-Z-001 DQC-05, DQC-016).
// Certification normally follows a run within minutes; a day is generous, and
// a caller who needs longer must say so in configuration, not in a request.
const DefaultEvidenceMaxAge = 24 * time.Hour

type PgStore struct {
	pool   *pgxpool.Pool
	maxAge time.Duration
	now    func() time.Time
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool, maxAge: DefaultEvidenceMaxAge, now: func() time.Time { return time.Now().UTC() }}
}

// WithEvidenceMaxAge sets the freshness window. A non-positive value is
// ignored: an unlimited window is not a setting this service offers, because
// "never expires" is exactly the gap this exists to close.
func (s *PgStore) WithEvidenceMaxAge(d time.Duration) *PgStore {
	if d > 0 {
		s.maxAge = d
	}
	return s
}

// WithClock replaces the time source, for tests.
func (s *PgStore) WithClock(now func() time.Time) *PgStore {
	s.now = now
	return s
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
	// A rule that evaluated no records has no denominator: "0 failures out of
	// 0" is the absence of evidence, not a pass (ZS-SVC-Z-001 INV-04/INV-05).
	if ro.PassCount == 0 && ro.FailCount == 0 {
		return domain.ResultIndeterminate, nil
	}
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
	src, err := loadRuleSource(ctx, tx, req)
	if err != nil {
		return nil, err
	}

	type evaluated struct {
		outcome domain.RuleOutcome
		status  domain.DQResultStatus
		spec    ruleSpec
	}
	results := make([]evaluated, 0, len(src.Specs))
	anyFail, anyIndeterminate := false, false
	seen := make(map[string]bool, len(req.RuleOutcomes))
	for _, ro := range req.RuleOutcomes {
		// An outcome for a rule the pinned version does not contain is not
		// evidence about this contract, and must not be guessed at (INV-15).
		spec, known := src.spec(ro.RuleKey)
		if !known {
			return nil, fmt.Errorf("%w: %q", domain.ErrUnknownRule, ro.RuleKey)
		}
		if seen[ro.RuleKey] {
			return nil, fmt.Errorf("%w: %q", domain.ErrDuplicateRuleOutcome, ro.RuleKey)
		}
		seen[ro.RuleKey] = true

		status, err := spec.evaluate(ro)
		if err != nil {
			return nil, err
		}
		// Only a MANDATORY rule's failure opens exceptions and blocks certification. A rule the contract's
		// certification profile does not require is recorded, with its severity, but does not (all rules of a
		// rule-set run are mandatory, so for those nothing changes).
		if spec.Mandatory {
			switch status {
			case domain.ResultFail:
				anyFail = true
			case domain.ResultIndeterminate:
				anyIndeterminate = true
			}
		}
		results = append(results, evaluated{outcome: ro, status: status, spec: spec})
	}
	// Every rule in the pinned version must appear in the run. One the caller
	// did not report on is recorded as Indeterminate rather than dropped, so
	// the gap is itself evidence and the run cannot certify without it
	// (INV-04: PASS requires complete mandatory evidence).
	for _, sp := range src.Specs {
		if seen[sp.Key] {
			continue
		}
		if sp.Mandatory {
			anyIndeterminate = true
		}
		results = append(results, evaluated{
			outcome: domain.RuleOutcome{RuleKey: sp.Key, FailureMagnitudeSum: "0"},
			status:  domain.ResultIndeterminate,
			spec:    sp,
		})
	}

	// The same evidence judged against the same contract is ONE run (ZS-SVC-Z-001 section 11: "same pinned
	// asset/contract/evidence returns same logical result"). Checked before the idempotency key is claimed, so
	// returning the existing run consumes nothing. A reperform of identical evidence is refused: it would
	// supersede a run with itself.
	// A run that names an observation must be judged on exactly what was observed.
	var observationID *string
	if req.ObservationID != "" {
		if !src.isContract() {
			return nil, fmt.Errorf("%w: an observation can only be linked to a run judged against a contract", domain.ErrObservationMismatch)
		}
		obs, err := scanObservation(tx.QueryRow(ctx, `SELECT `+observationCols+` FROM dq_observations WHERE observation_run_id = $1`, req.ObservationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrObservationNotFound
		}
		if err != nil {
			return nil, err
		}
		switch {
		case obs.AssetID != src.AssetID:
			return nil, fmt.Errorf("%w: the observation is of a different asset than the contract", domain.ErrObservationMismatch)
		case obs.RowCount != req.PopulationRowCount:
			return nil, fmt.Errorf("%w: the observation recorded %d rows, the run says %d", domain.ErrObservationMismatch, obs.RowCount, req.PopulationRowCount)
		case obs.EvidenceHash != req.PopulationContentHash:
			return nil, fmt.Errorf("%w: the population content hash is not the observation's evidence hash", domain.ErrObservationMismatch)
		case req.EvidenceAsOf != nil && req.EvidenceAsOf.After(obs.RecordedAt):
			return nil, fmt.Errorf("%w: the evidence cannot have been observed after the observation was recorded", domain.ErrObservationMismatch)
		}
		if req.EvidenceAsOf == nil {
			// Evidence is as old as the observation that recorded it.
			asOf := obs.RecordedAt
			req.EvidenceAsOf = &asOf
		}
		observationID = &obs.ObservationRunID
	}

	var fingerprint *string
	if src.isContract() {
		fp := evidenceFingerprint(*src.ContractID, req)
		fingerprint = &fp
		existing, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM dq_runs WHERE tenant_id = $1 AND evidence_fingerprint = $2`, tenantID, fp))
		switch {
		case err == nil:
			if supersedesRunID != nil {
				return nil, domain.ErrEvidenceAlreadyRecorded
			}
			existing.Deduplicated = true
			return existing, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, err
		}
	}

	runID := newID(domain.PrefixRun)
	claim.ResourceID = runID
	if err := claimIdempotency(ctx, tx, claim); err != nil {
		return nil, err
	}

	finalStatus := domain.RunRunning
	if anyFail || anyIndeterminate {
		finalStatus = domain.RunExceptionsOpen
	}
	now := time.Now().UTC()
	var run domain.DQRun
	if err := tx.QueryRow(ctx, `
		INSERT INTO dq_runs (run_id, tenant_id, ruleset_version_id, contract_id, population_ref, population_row_count,
			population_content_hash, status, supersedes_run_id, started_at, created_by, evidence_as_of, evidence_fingerprint, observation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING `+runColumns,
		runID, tenantID, src.RuleSetVersionID, src.ContractID, req.PopulationRef, req.PopulationRowCount,
		req.PopulationContentHash, finalStatus, supersedesRunID, now, actor, req.EvidenceAsOf, fingerprint, observationID,
	).Scan(runDest(&run)...); err != nil {
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
				failure_magnitude_sum, failing_record_refs, severity, dimension, mandatory)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9::jsonb, $10, $11, $12)`,
			resultID, tenantID, runID, r.outcome.RuleKey, r.status, r.outcome.PassCount, r.outcome.FailCount,
			r.outcome.FailureMagnitudeSum, failingJSON, severityOf(r.spec), dimensionOf(r.spec), mandatoryOf(r.spec)); err != nil {
			return nil, fmt.Errorf("insert dq result: %w", err)
		}
	}

	eventType := "DATA.DQRunCompleted"
	switch {
	case anyFail:
		eventType = "DATA.DQFailed"
	case anyIndeterminate:
		eventType = "DATA.DQIndeterminate"
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_run", AggregateID: run.RunID,
		EventType: eventType, TenantID: &tenantID, Payload: run}); err != nil {
		return nil, err
	}
	// The spec's own event (ZS-SVC-Z-001 section 8), for runs judged against a contract: the per-rule
	// outcome with its severity, so a consumer can see a BLOCKER failure without reading the results.
	if src.isContract() {
		type ruleOutcomeEvent struct {
			RuleKey   string `json:"rule_key"`
			Status    string `json:"status"`
			Severity  string `json:"severity"`
			Mandatory bool   `json:"mandatory"`
		}
		evs := make([]ruleOutcomeEvent, 0, len(results))
		blockers := 0
		for _, r := range results {
			evs = append(evs, ruleOutcomeEvent{RuleKey: r.outcome.RuleKey, Status: string(r.status), Severity: string(r.spec.Rule.Severity), Mandatory: r.spec.Mandatory})
			if r.spec.Rule.Severity == domain.SeverityBlocker && r.status != domain.ResultPass {
				blockers++
			}
		}
		if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_run", AggregateID: run.RunID,
			EventType: "dqc.validation.completed", TenantID: &tenantID, Payload: map[string]any{
				"run_id": run.RunID, "contract_id": *src.ContractID, "contract_sha256": src.ContractSHA256, "status": run.Status,
				"population_ref": run.PopulationRef, "blocker_not_passed": blockers, "results": evs,
			}}); err != nil {
			return nil, err
		}
	}
	return &run, nil
}

const runColumns = `run_id, tenant_id, COALESCE(ruleset_version_id, ''), contract_id, population_ref, population_row_count,
	population_content_hash, status, supersedes_run_id, started_at, completed_at, created_by, evidence_as_of, observation_id`

// runDest lists the scan targets for runColumns, in order.
func runDest(run *domain.DQRun) []any {
	return []any{&run.RunID, &run.TenantID, &run.RuleSetVersionID, &run.ContractID, &run.PopulationRef, &run.PopulationRowCount,
		&run.PopulationContentHash, &run.Status, &run.SupersedesRunID, &run.StartedAt, &run.CompletedAt, &run.CreatedBy, &run.EvidenceAsOf, &run.ObservationID}
}

func scanRun(row pgx.Row) (*domain.DQRun, error) {
	var run domain.DQRun
	if err := row.Scan(runDest(&run)...); err != nil {
		return nil, err
	}
	return &run, nil
}

func severityOf(sp ruleSpec) *string {
	if sp.Rule == nil {
		return nil
	}
	s := string(sp.Rule.Severity)
	return &s
}

func dimensionOf(sp ruleSpec) *string {
	if sp.Rule == nil {
		return nil
	}
	return &sp.Rule.Dimension
}

func mandatoryOf(sp ruleSpec) *bool {
	if sp.Rule == nil {
		return nil
	}
	return &sp.Mandatory
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

const resultColumns = `result_id, tenant_id, run_id, rule_key, status, pass_count, fail_count, failure_magnitude_sum, failing_record_refs, evaluated_at, severity, dimension, mandatory`

func scanResult(row pgx.Row) (*domain.DQResult, error) {
	var res domain.DQResult
	var failingJSON []byte
	if err := row.Scan(&res.ResultID, &res.TenantID, &res.RunID, &res.RuleKey, &res.Status, &res.PassCount,
		&res.FailCount, &res.FailureMagnitudeSum, &failingJSON, &res.EvaluatedAt, &res.Severity, &res.Dimension, &res.Mandatory); err != nil {
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
		if result.Status != domain.ResultFail && result.Status != domain.ResultIndeterminate {
			return fmt.Errorf("%w: result %s is neither a Fail nor Indeterminate", domain.ErrResultNotFound, req.ResultID)
		}

		// One exception per failing result: a second report of the same root evidence returns the first, not a
		// second alert (ZS-SVC-Z-001 section 11: "dedup key prevents alert storms").
		existing, err := scanIssue(tx.QueryRow(ctx, `SELECT `+issueColumns+` FROM dq_issues WHERE tenant_id = $1 AND result_id = $2`, tenantID, req.ResultID))
		switch {
		case err == nil:
			existing.Deduplicated = true
			out = existing
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
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
		if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_issue", AggregateID: got.IssueID,
			EventType: "DATA.DQIssueRaised", TenantID: &tenantID, Payload: got}); err != nil {
			return err
		}
		// The spec's own event (ZS-SVC-Z-001 section 12). severity is the rule's, frozen on the result, and is
		// null for a rule-set run, which has none; the control is the contract (or rule set) and the rule.
		control := contractIDOf(run)
		if control == "" {
			control = run.RuleSetVersionID
		}
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_issue", AggregateID: got.IssueID,
			EventType: "dqc.exception.opened", TenantID: &tenantID, Payload: map[string]any{
				"exception_id": got.IssueID, "run_id": run.RunID, "result_id": result.ResultID, "rule_key": result.RuleKey,
				"severity": result.Severity, "control_id": control,
				"evidence_refs": []string{run.RunID, result.ResultID, run.PopulationRef + "@" + run.PopulationContentHash},
				"wfc_case_ref":  nil, "raised_by": actor,
			}})
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
		// A reperform is evidence about the SAME rules: the same rule-set version, or the same contract version.
		newSrc, err := loadRuleSource(ctx, tx, req)
		if err != nil {
			return err
		}
		if !sameSource(newSrc, oldRun) {
			return fmt.Errorf("reperform must use the same ruleset_version_id or contract version as the run it supersedes")
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
func (s *PgStore) CertifyDQ(ctx context.Context, tenantID, runID, actor string, req domain.CertifyDQRequest, claim domain.IdempotencyClaim) (*domain.DQCertification, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.DQCertification
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRun(ctx, tx, runID, true)
		if err != nil {
			return err
		}
		if run.Status != domain.RunRunning && run.Status != domain.RunExceptionsOpen {
			return fmt.Errorf("%w: run must be Running (clean, no failing results) to certify; it is %s", domain.ErrRunHasOpenIssues, run.Status)
		}
		results, err := loadResultsForRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		src, err := loadRuleSourceForRun(ctx, tx, run)
		if err != nil {
			return err
		}
		// A run with exceptions open is certifiable only over GOVERNED risk acceptance: every mandatory result that is
		// not a Pass is a Fail with an accepted risk acceptance (two approvers), and the certifier is independent of them.
		var accepted []domain.AcceptedRisk
		acceptedResults := []string{}
		acceptedRules := map[string]bool{}
		if run.Status == domain.RunExceptionsOpen {
			accepted, err = s.acceptedRisksFor(ctx, tx, run, src, results, actor)
			if err != nil {
				return err
			}
			for _, a := range accepted {
				acceptedResults = append(acceptedResults, a.ResultID)
				acceptedRules[a.RuleKey] = true
			}
		}
		var openIssues int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dq_issues WHERE run_id = $1 AND status = 'Open' AND NOT (result_id = ANY($2))`,
			runID, acceptedResults).Scan(&openIssues); err != nil {
			return err
		}
		if openIssues > 0 {
			return domain.ErrRunHasOpenIssues
		}
		// Do not rely on the run's status alone. A run recorded before
		// completeness was enforced can be Running with only some of the
		// version's rules reported; certification re-derives completeness from
		// the pinned version and the stored results (INV-04, INV-21).
		if err := requireCompleteEvidence(src, results, acceptedRules); err != nil {
			return err
		}

		// Freshness (DQC-05, NP-43, DQC-016). Evidence is as old as the moment it
		// was observed — the caller-asserted evidence_as_of, or failing that when
		// this service first recorded it. Stale evidence is never certified; the
		// remedy is a Reperform with current evidence, which is a recorded fact,
		// not a flag flipped.
		// Truncated to the microsecond Postgres stores, so what is returned is exactly what was written.
		now := s.now().UTC().Truncate(time.Microsecond)
		asOf := run.StartedAt
		if run.EvidenceAsOf != nil {
			asOf = *run.EvidenceAsOf
		}
		age := now.Sub(asOf)
		if age > s.maxAge {
			return fmt.Errorf("%w: evidence is %s old, window is %s", domain.ErrEvidenceStale,
				age.Round(time.Second), s.maxAge)
		}

		// The basis: WHAT is certified, independent of which run, who or when.
		basisSHA256, err := certificationBasisHash(tenantID, run, src, asOf, s.maxAge, results, req, accepted)
		if err != nil {
			return err
		}
		var existingID string
		switch err := tx.QueryRow(ctx, `SELECT certification_id FROM dq_certifications WHERE basis_sha256 = $1`,
			basisSHA256).Scan(&existingID); {
		case err == nil:
			return &domain.BasisExistsError{ExistingID: existingID}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		certID := newID(domain.PrefixCertification)
		claim.ResourceID = certID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		asOfUTC := asOf.UTC()
		summary := domain.DQCertificationSummary{
			RunID: run.RunID, RuleSetVersionID: run.RuleSetVersionID, ContractID: contractIDOf(run), ContractSHA256: src.ContractSHA256, PopulationRef: run.PopulationRef,
			PopulationRowCount: run.PopulationRowCount, PopulationContentHash: run.PopulationContentHash, Results: results,
			BasisSHA256: basisSHA256, EvidenceAsOf: &asOfUTC, EvidenceMaxAgeSeconds: int64(s.maxAge / time.Second),
			CertifiedAgainstAge: int64(age / time.Second), AcceptedRisks: accepted,
			LineageManifestID: req.LineageManifestID, LineageManifestSHA256: req.LineageManifestSHA256,
		}
		summaryJSON, err := json.Marshal(summary)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(summaryJSON)
		summarySHA256 := hex.EncodeToString(sum[:])

		// A contract that states a validity window freezes, on the certification, when it stops being reliable.
		var windowSecs *int64
		var validUntil *time.Time
		if src.ValidityWindow > 0 {
			w := src.ValidityWindow
			u := now.Add(time.Duration(w) * time.Second).UTC()
			windowSecs, validUntil = &w, &u
		}
		var cert domain.DQCertification
		if err := tx.QueryRow(ctx, `
			INSERT INTO dq_certifications (certification_id, tenant_id, run_id, summary, summary_sha256, certified_at, certified_by,
				basis_sha256, lineage_manifest_id, lineage_manifest_sha256, evidence_as_of, evidence_max_age_seconds,
				validity_window_seconds, valid_until)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			RETURNING certification_id, tenant_id, run_id, summary_sha256, certified_at, certified_by`,
			certID, tenantID, runID, summaryJSON, summarySHA256, now, actor,
			basisSHA256, req.LineageManifestID, req.LineageManifestSHA256, asOfUTC, int64(s.maxAge/time.Second),
			windowSecs, validUntil,
		).Scan(&cert.CertificationID, &cert.TenantID, &cert.RunID, &cert.SummarySHA256, &cert.CertifiedAt, &cert.CertifiedBy); err != nil {
			// The pre-check above covers the common case; this covers two
			// certifications of one basis racing each other.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_dq_certifications_basis" {
				return domain.ErrCertificationBasisExists
			}
			return fmt.Errorf("insert dq certification: %w", err)
		}
		cert.Summary = summary
		cert.BasisSHA256, cert.LineageManifestID, cert.LineageManifestSHA256 = basisSHA256, req.LineageManifestID, req.LineageManifestSHA256

		if _, err := tx.Exec(ctx, `UPDATE dq_runs SET status = 'Certified', completed_at = $2 WHERE run_id = $1`, runID, now); err != nil {
			return fmt.Errorf("mark run certified: %w", err)
		}

		cert.Status = domain.CertificationActive
		cert.ValidityWindowSeconds, cert.ValidUntil = windowSecs, validUntil
		out = &cert
		if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_certification", AggregateID: cert.CertificationID,
			EventType: "DATA.DQCertified", TenantID: &tenantID, Payload: cert}); err != nil {
			return err
		}
		// The spec's own event (ZS-SVC-Z-001 section 8).
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_certification", AggregateID: cert.CertificationID,
			EventType: "dqc.certification.issued", TenantID: &tenantID, Payload: map[string]any{
				"certification_id": cert.CertificationID, "run_id": run.RunID, "contract_id": contractIDOf(run),
				"summary_sha256": cert.SummarySHA256, "basis_sha256": cert.BasisSHA256, "certified_by": actor, "certified_at": cert.CertifiedAt,
			}})
	})
	return out, err
}

// certificationBasis is the canonical content hashed into basis_sha256. It names
// what was certified — and deliberately omits the run id, the certifier and the
// seal time, so the same evidence yields the same basis wherever and whenever it
// is certified. Field order is fixed by the struct; results are sorted by rule.
type certificationBasis struct {
	TenantID              string       `json:"tenant_id"`
	RuleSetVersionID      string       `json:"ruleset_version_id,omitempty"`
	ContractID            string       `json:"contract_id,omitempty"`
	ContractSHA256        string       `json:"contract_sha256,omitempty"`
	PopulationRef         string       `json:"population_ref"`
	PopulationRowCount    int64        `json:"population_row_count"`
	PopulationContentHash string       `json:"population_content_hash"`
	EvidenceAsOf          string       `json:"evidence_as_of"`
	EvidenceMaxAgeSeconds int64        `json:"evidence_max_age_seconds"`
	LineageManifestID     string       `json:"lineage_manifest_id"`
	LineageManifestSHA256 string       `json:"lineage_manifest_sha256"`
	Results               []basisEntry `json:"results"`
	// AcceptedRisks are the acceptance ids the certification rests on, so the same failing evidence certified over
	// different governed acceptances is a different basis, and a clean certification's basis is unchanged.
	AcceptedRisks []string `json:"accepted_risks,omitempty"`
}

type basisEntry struct {
	RuleKey             string `json:"rule_key"`
	Status              string `json:"status"`
	PassCount           int64  `json:"pass_count"`
	FailCount           int64  `json:"fail_count"`
	FailureMagnitudeSum string `json:"failure_magnitude_sum"`
}

func certificationBasisHash(tenantID string, run *domain.DQRun, src *ruleSource, asOf time.Time, maxAge time.Duration,
	results []domain.DQResult, req domain.CertifyDQRequest, accepted []domain.AcceptedRisk) (string, error) {
	b := certificationBasis{
		TenantID: tenantID, RuleSetVersionID: run.RuleSetVersionID, PopulationRef: run.PopulationRef,
		PopulationRowCount: run.PopulationRowCount, PopulationContentHash: run.PopulationContentHash,
		EvidenceAsOf:          asOf.UTC().Format(time.RFC3339Nano),
		EvidenceMaxAgeSeconds: int64(maxAge / time.Second),
	}
	if src.isContract() {
		b.ContractID, b.ContractSHA256 = *src.ContractID, src.ContractSHA256
	}
	if req.LineageManifestID != nil {
		b.LineageManifestID, b.LineageManifestSHA256 = *req.LineageManifestID, *req.LineageManifestSHA256
	}
	for _, r := range results {
		b.Results = append(b.Results, basisEntry{RuleKey: r.RuleKey, Status: string(r.Status), PassCount: r.PassCount,
			FailCount: r.FailCount, FailureMagnitudeSum: r.FailureMagnitudeSum})
	}
	sort.Slice(b.Results, func(i, j int) bool { return b.Results[i].RuleKey < b.Results[j].RuleKey })
	for _, a := range accepted {
		b.AcceptedRisks = append(b.AcceptedRisks, a.AcceptanceID)
	}
	sort.Strings(b.AcceptedRisks)
	raw, err := json.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("encode certification basis: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// requireCompleteEvidence returns ErrEvidenceIncomplete unless every rule in
// the pinned version has exactly one stored result and that result is a Pass.
func requireCompleteEvidence(src *ruleSource, results []domain.DQResult, acceptedRules map[string]bool) error {
	byRule := make(map[string]domain.DQResultStatus, len(results))
	for _, r := range results {
		byRule[r.RuleKey] = r.Status
	}
	for _, sp := range src.Specs {
		if !sp.Mandatory {
			continue // recorded evidence, but the certification profile does not require it
		}
		status, ok := byRule[sp.Key]
		if !ok {
			return fmt.Errorf("%w: no result for rule %q", domain.ErrEvidenceIncomplete, sp.Key)
		}
		// A FAILED rule whose risk was governedly accepted is carried on the certification, not hidden by it.
		if status == domain.ResultFail && acceptedRules[sp.Key] {
			continue
		}
		if status != domain.ResultPass {
			return fmt.Errorf("%w: rule %q is %s", domain.ErrEvidenceIncomplete, sp.Key, status)
		}
	}
	return nil
}

// sameSource reports whether a request's rule source is the one a stored run was judged against.
func sameSource(src *ruleSource, run *domain.DQRun) bool {
	if src.isContract() {
		return run.ContractID != nil && *run.ContractID == *src.ContractID
	}
	return run.ContractID == nil && src.RuleSetVersionID != nil && *src.RuleSetVersionID == run.RuleSetVersionID
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
		var basis *string
		err := tx.QueryRow(ctx, `SELECT certification_id, tenant_id, run_id, summary, summary_sha256, certified_at, certified_by,
				basis_sha256, lineage_manifest_id, lineage_manifest_sha256, validity_window_seconds, valid_until
			FROM dq_certifications WHERE run_id = $1`, runID,
		).Scan(&cert.CertificationID, &cert.TenantID, &cert.RunID, &summaryJSON, &cert.SummarySHA256, &cert.CertifiedAt, &cert.CertifiedBy,
			&basis, &cert.LineageManifestID, &cert.LineageManifestSHA256, &cert.ValidityWindowSeconds, &cert.ValidUntil)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCertificationNotFound
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(summaryJSON, &cert.Summary); err != nil {
			return err
		}
		if basis != nil {
			cert.BasisSHA256 = *basis
		}
		cert.Status = domain.CertificationActive
		// Past its validity window a certification is no longer to be relied on (DQC-016). Revocation, which is
		// a deliberate act with a reason, outranks it below.
		if cert.ValidUntil != nil && s.now().UTC().After(*cert.ValidUntil) {
			cert.Status = domain.CertificationStale
		}
		var rev domain.CertificationRevocation
		switch err := tx.QueryRow(ctx, `SELECT `+revocationCols+` FROM dq_certification_revocations WHERE certification_id = $1`, cert.CertificationID).Scan(revocationDest(&rev)...); {
		case err == nil:
			cert.Status, cert.Revocation = domain.CertificationRevoked, &rev
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		out = &cert
		return nil
	})
	return out, err
}

const revocationCols = `revocation_id, certification_id, reason_code, reason, evidence_refs, certified_by, certified_at, revoked_by, revoked_at`

func revocationDest(r *domain.CertificationRevocation) []any {
	return []any{&r.RevocationID, &r.CertificationID, &r.ReasonCode, &r.Reason, &r.EvidenceRefs, &r.CertifiedBy, &r.CertifiedAt, &r.RevokedBy, &r.RevokedAt}
}

// RevokeCertification withdraws reliance on a certification, prospectively (ZS-SVC-Z-001 RB-05, DQC-017).
// The sealed certification and its evidence are not touched: the revocation is its own append-only record,
// once per certification. The certification's basis stays claimed, so the same evidence cannot be certified
// again; the remedy for a false pass is new evidence.
func (s *PgStore) RevokeCertification(ctx context.Context, tenantID, runID, actor string, req domain.RevokeCertificationRequest, claim domain.IdempotencyClaim) (*domain.CertificationRevocation, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.CertificationRevocation
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var certID, summarySHA, certifiedBy string
		var certifiedAt time.Time
		var basis *string
		err := tx.QueryRow(ctx, `SELECT certification_id, summary_sha256, basis_sha256, certified_by, certified_at
			FROM dq_certifications WHERE run_id = $1 FOR UPDATE`, runID).
			Scan(&certID, &summarySHA, &basis, &certifiedBy, &certifiedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCertificationNotFound
		}
		if err != nil {
			return err
		}
		// Checked before the idempotency key is claimed, so a refused revocation does not consume it.
		if actor == certifiedBy {
			return domain.ErrRevokerIsCertifier
		}
		revID := newID(domain.PrefixRevocation)
		claim.ResourceID = revID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		var rev domain.CertificationRevocation
		if err := tx.QueryRow(ctx, `
			INSERT INTO dq_certification_revocations (revocation_id, tenant_id, certification_id, reason_code, reason, evidence_refs,
				certified_by, certified_at, revoked_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+revocationCols,
			revID, tenantID, certID, req.ReasonCode, strings.TrimSpace(req.Reason), req.EvidenceRefs, certifiedBy, certifiedAt, actor,
		).Scan(revocationDest(&rev)...); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.ErrCertificationAlreadyRevoked
			}
			return fmt.Errorf("insert revocation: %w", err)
		}
		out = &rev
		basisStr := ""
		if basis != nil {
			basisStr = *basis
		}
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dq_certification", AggregateID: certID,
			EventType: "dqc.certification.revoked", TenantID: &tenantID, Payload: map[string]any{
				"revocation_id": rev.RevocationID, "certification_id": certID, "run_id": runID, "reason_code": rev.ReasonCode,
				"reason": rev.Reason, "evidence_refs": rev.EvidenceRefs, "revoked_by": actor, "effective_from": rev.RevokedAt, "revoked_at": rev.RevokedAt,
				"summary_sha256": summarySHA, "basis_sha256": basisStr,
			}})
	})
	return out, err
}
