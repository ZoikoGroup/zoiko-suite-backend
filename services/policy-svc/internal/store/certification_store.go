package store

// V-001 PDC-05: certification test suites, waivers, the activation gate,
// differential simulation and governed rollback drafts.
//
// A family's suite is its ACTIVE test cases. Running it against a version
// evaluates every case with internal/evaluator, the code production runs
// (PDC-I-18), and stores an immutable run. A version is certified against
// the suite as it is now (suite_hash) and the bytes it carries
// (artifact_digest): a run on another suite or other bytes is STALE. Failed
// mandatory cases can be covered by approved, unexpired waivers (WAIVED),
// never turned into a pass (PDC-I-25).

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
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/evaluator"
)

// maxWaiverLife bounds a waiver: time-bounded means a review date, not a
// permanent exception with a far-off expiry.
const maxWaiverLife = 180 * 24 * time.Hour

// inputErrorResult is the "actual" of a case whose input the evaluator
// refused; a negative test expects it.
const inputErrorResult = "INPUT_ERROR"

const testCaseColumns = `test_case_id, policy_id, tenant_id, test_code, layer, mandatory, description,
	action_context, expected_result, test_status, created_at, created_by_principal_id, retired_at, retired_by_principal_id`

func scanTestCase(row pgx.Row) (*domain.TestCase, error) {
	c := &domain.TestCase{}
	var action []byte
	err := row.Scan(&c.TestCaseID, &c.PolicyID, &c.TenantID, &c.TestCode, &c.Layer, &c.Mandatory, &c.Description,
		&action, &c.ExpectedResult, &c.TestStatus, &c.CreatedAt, &c.CreatedByPrincipalID, &c.RetiredAt, &c.RetiredByPrincipalID)
	c.ActionContext = action
	return c, err
}

const testRunColumns = `test_run_id, tenant_id, policy_id, policy_version_id, artifact_digest, suite_hash,
	evaluator_version, run_status, case_count, failed_count, static_findings, results, run_by_principal_id, run_at`

func scanTestRun(row pgx.Row) (*domain.TestRun, error) {
	r := &domain.TestRun{}
	var findings, results []byte
	err := row.Scan(&r.TestRunID, &r.TenantID, &r.PolicyID, &r.PolicyVersionID, &r.ArtifactDigest, &r.SuiteHash,
		&r.EvaluatorVersion, &r.RunStatus, &r.CaseCount, &r.FailedCount, &findings, &results, &r.RunByPrincipalID, &r.RunAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(findings, &r.StaticFindings); err != nil {
		return nil, err
	}
	return r, json.Unmarshal(results, &r.Results)
}

const waiverColumns = `waiver_id, tenant_id, policy_version_id, test_case_id, reason_code, justification, expires_at,
	waiver_status, requested_by_principal_id, requested_at, approved_by_principal_id, approved_at,
	revoked_by_principal_id, revoked_at, revocation_reason`

func scanWaiver(row pgx.Row) (*domain.Waiver, error) {
	w := &domain.Waiver{}
	err := row.Scan(&w.WaiverID, &w.TenantID, &w.PolicyVersionID, &w.TestCaseID, &w.ReasonCode, &w.Justification, &w.ExpiresAt,
		&w.WaiverStatus, &w.RequestedByPrincipalID, &w.RequestedAt, &w.ApprovedByPrincipalID, &w.ApprovedAt,
		&w.RevokedByPrincipalID, &w.RevokedAt, &w.RevocationReason)
	return w, err
}

const simulationColumns = `simulation_id, tenant_id, policy_type, candidate_version_id, baseline_version_id, sim_status,
	corpus_from, corpus_to, corpus_limit, corpus_size, changed_count, error_count, requested_by_principal_id,
	created_at, completed_at`

func scanSimulation(row pgx.Row) (*domain.SimulationRun, error) {
	r := &domain.SimulationRun{}
	err := row.Scan(&r.SimulationID, &r.TenantID, &r.PolicyType, &r.CandidateVersionID, &r.BaselineVersionID, &r.SimStatus,
		&r.CorpusFrom, &r.CorpusTo, &r.CorpusLimit, &r.CorpusSize, &r.ChangedCount, &r.ErrorCount, &r.RequestedByPrincipalID,
		&r.CreatedAt, &r.CompletedAt)
	return r, err
}

// pdcEvent is an event about a PDC-05 record, keyed by aggregate.
func pdcEvent(eventType string, tenantID *string, aggregateID string, meta domain.EventMeta, actor string, payload any) OutboxEvent {
	key := meta.IdempotencyKey
	if key == "" {
		key = eventType + ":" + aggregateID
	} else {
		key = eventType + ":" + key
	}
	return OutboxEvent{
		EventType: eventType, TenantID: tenantID, ActorID: actor, CorrelationID: meta.CorrelationID,
		CausationID: meta.CausationID, IdempotencyKey: key, JurisdictionContext: meta.JurisdictionContext,
		AggregateID: aggregateID, AggregateVersion: 1, Payload: mustJSON(payload),
	}
}

func lockFamily(ctx context.Context, tx pgx.Tx, policyID string) (*domain.Policy, error) {
	p, err := scanPolicy(tx.QueryRow(ctx, `SELECT `+policyColumns+` FROM policies WHERE policy_id = $1 FOR UPDATE;`, policyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPolicyNotFound
	}
	return p, err
}

// ── test cases ───────────────────────────────────────────────────────────────

// CreateTestCase adds a case to an evaluable family's suite. Idempotent on
// (family, test_code): the same content is a replay, other content 409.
func (s *PgStore) CreateTestCase(ctx context.Context, p domain.CreateTestCaseParams) (*domain.TestCase, bool, error) {
	if p.TestCaseID == "" {
		p.TestCaseID = uuid.NewString()
	}
	var out *domain.TestCase
	var created bool
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		family, err := lockFamily(ctx, tx, p.PolicyID)
		if err != nil {
			return err
		}
		if !evaluator.Supports(family.PolicyType) {
			return domain.ErrNoEvaluator
		}
		existing, err := scanTestCase(tx.QueryRow(ctx, `SELECT `+testCaseColumns+` FROM policy_test_cases WHERE policy_id = $1 AND test_code = $2;`, p.PolicyID, p.TestCode))
		if err == nil {
			if existing.Layer != p.Layer || existing.Mandatory != p.Mandatory || existing.ExpectedResult != p.ExpectedResult ||
				!jsonEqual(existing.ActionContext, p.ActionContext) {
				return domain.ErrConflict
			}
			out = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = scanTestCase(tx.QueryRow(ctx, `
			INSERT INTO policy_test_cases (test_case_id, policy_id, tenant_id, test_code, layer, mandatory, description,
			                               action_context, expected_result, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING `+testCaseColumns+`;`,
			p.TestCaseID, p.PolicyID, family.TenantID, p.TestCode, p.Layer, p.Mandatory, p.Description,
			[]byte(p.ActionContext), p.ExpectedResult, s.now(), p.ActorID))
		if isUniqueViolation(err) {
			return domain.ErrConflict
		}
		if err != nil {
			return err
		}
		created = true
		return s.enqueue(ctx, tx, pdcEvent("policy.test_case.added", family.TenantID, family.PolicyID, p.Meta, p.ActorID, out))
	})
	if err != nil {
		return nil, false, s.domainError("CreateTestCase", err)
	}
	return out, created, nil
}

// ListTestCases returns a family's cases, active and retired, oldest first.
func (s *PgStore) ListTestCases(ctx context.Context, policyID string) ([]*domain.TestCase, error) {
	if _, err := s.FindPolicyByID(ctx, policyID); err != nil {
		return nil, err
	}
	return listRows(ctx, s, "ListTestCases",
		`SELECT `+testCaseColumns+` FROM policy_test_cases WHERE policy_id = $1 ORDER BY created_at, test_case_id;`,
		[]any{policyID}, scanTestCase)
}

// RetireTestCase takes a case out of the suite (the suite hash changes, so
// every certification on the old suite becomes STALE).
func (s *PgStore) RetireTestCase(ctx context.Context, policyID, testCaseID, actor string, meta domain.EventMeta) (*domain.TestCase, error) {
	var out *domain.TestCase
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		c, err := scanTestCase(tx.QueryRow(ctx, `SELECT `+testCaseColumns+` FROM policy_test_cases WHERE test_case_id = $1 AND policy_id = $2 FOR UPDATE;`, testCaseID, policyID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrTestCaseNotFound
		}
		if err != nil {
			return err
		}
		if c.TestStatus == "RETIRED" {
			out = c
			return nil
		}
		out, err = scanTestCase(tx.QueryRow(ctx, `
			UPDATE policy_test_cases SET test_status = 'RETIRED', retired_at = $2, retired_by_principal_id = $3
			WHERE test_case_id = $1 RETURNING `+testCaseColumns+`;`, testCaseID, s.now(), actor))
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, pdcEvent("policy.test_case.retired", out.TenantID, out.PolicyID, meta, actor, out))
	})
	if err != nil {
		return nil, s.domainError("RetireTestCase", err)
	}
	return out, nil
}

// activeCases reads a family's suite and its hash. The hash covers each
// case's identity and everything that decides its outcome, so adding,
// retiring or (impossibly) changing a case changes it.
func activeCases(ctx context.Context, tx pgx.Tx, policyID string) ([]*domain.TestCase, string, error) {
	rows, err := tx.Query(ctx, `SELECT `+testCaseColumns+` FROM policy_test_cases
		WHERE policy_id = $1 AND test_status = 'ACTIVE' ORDER BY test_case_id;`, policyID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var cases []*domain.TestCase
	h := sha256.New()
	for rows.Next() {
		c, err := scanTestCase(rows)
		if err != nil {
			return nil, "", err
		}
		cases = append(cases, c)
		fmt.Fprintf(h, "%s|%s|%t|%s|%s\n", c.TestCaseID, c.Layer, c.Mandatory, c.ExpectedResult, canonicalJSON(c.ActionContext))
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return cases, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalJSON re-encodes raw with sorted keys and exact numbers.
func canonicalJSON(raw []byte) string {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ── test runs ────────────────────────────────────────────────────────────────

// staticFindings is the static validation layer (V-001 §17.1): the bytes are
// the approved bytes, the payload is one the evaluator can use, the window is
// well formed, and the suite has at least one mandatory case.
func staticFindings(v *domain.PolicyVersion, policyType string, cases []*domain.TestCase) []string {
	var out []string
	sum := sha256.Sum256(v.RulePayload)
	if v.ArtifactDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		out = append(out, "artifact_digest does not match the stored payload")
	}
	if err := evaluator.ValidatePayload(policyType, v.RulePayload); err != nil {
		out = append(out, err.Error())
	}
	if v.EffectiveTo != nil && !v.EffectiveTo.After(v.EffectiveFrom) {
		out = append(out, "effective_to is not after effective_from")
	}
	mandatory := 0
	for _, c := range cases {
		if c.Mandatory {
			mandatory++
		}
	}
	if mandatory == 0 {
		out = append(out, "the family's suite has no mandatory test case")
	}
	return out
}

// RunTests runs the family's suite against one version and stores the run.
// It evaluates in memory and writes only the run and its events (PDC-I-23).
func (s *PgStore) RunTests(ctx context.Context, versionID, actor string, meta domain.EventMeta) (*domain.TestRun, error) {
	var out *domain.TestRun
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		v, policyType, err := lockVersion(ctx, tx, versionID)
		if err != nil {
			return err
		}
		if !evaluator.Supports(policyType) {
			return domain.ErrNoEvaluator
		}
		cases, suiteHash, err := activeCases(ctx, tx, v.PolicyID)
		if err != nil {
			return err
		}
		run := &domain.TestRun{
			TestRunID: uuid.NewString(), TenantID: v.TenantID, PolicyID: v.PolicyID, PolicyVersionID: v.PolicyVersionID,
			ArtifactDigest: v.ArtifactDigest, SuiteHash: suiteHash, EvaluatorVersion: evaluator.VersionOf(policyType),
			StaticFindings: staticFindings(v, policyType, cases), Results: []domain.CaseResult{},
			RunByPrincipalID: actor, RunAt: s.now(), CaseCount: len(cases),
		}
		mandatoryFailed := 0
		for _, c := range cases {
			res := domain.CaseResult{TestCaseID: c.TestCaseID, TestCode: c.TestCode, Mandatory: c.Mandatory, Expected: c.ExpectedResult}
			outcome, evalErr := evaluator.Evaluate(policyType, v.PolicyVersionID, v.RulePayload, c.ActionContext)
			var inErr *evaluator.InputError
			switch {
			case evalErr == nil:
				res.Actual = outcome.Result
			case errors.As(evalErr, &inErr):
				res.Actual, res.Error = inputErrorResult, inErr.Error()
			default:
				res.Error = evalErr.Error()
			}
			res.Passed = res.Actual == c.ExpectedResult
			if !res.Passed {
				run.FailedCount++
				if c.Mandatory {
					mandatoryFailed++
				}
			}
			run.Results = append(run.Results, res)
		}
		if run.StaticFindings == nil {
			run.StaticFindings = []string{}
		}
		run.RunStatus = domain.TestRunPass
		if mandatoryFailed > 0 || len(run.StaticFindings) > 0 {
			run.RunStatus = domain.TestRunFail
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO policy_test_runs (test_run_id, tenant_id, policy_id, policy_version_id, artifact_digest, suite_hash,
			    evaluator_version, run_status, case_count, failed_count, static_findings, results, run_by_principal_id, run_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14);`,
			run.TestRunID, run.TenantID, run.PolicyID, run.PolicyVersionID, run.ArtifactDigest, run.SuiteHash,
			run.EvaluatorVersion, run.RunStatus, run.CaseCount, run.FailedCount, mustJSON(run.StaticFindings),
			mustJSON(run.Results), run.RunByPrincipalID, run.RunAt); err != nil {
			return err
		}
		out = run
		summary := map[string]any{
			"test_run_id": run.TestRunID, "policy_version_id": run.PolicyVersionID, "suite_hash": run.SuiteHash,
			"run_status": run.RunStatus, "case_count": run.CaseCount, "failed_count": run.FailedCount,
		}
		if err := s.enqueue(ctx, tx, pdcEvent("policy.test_run.completed", v.TenantID, run.TestRunID, meta, actor, summary)); err != nil {
			return err
		}
		if run.RunStatus == domain.TestRunPass {
			return s.enqueue(ctx, tx, pdcEvent("policy.version.certified", v.TenantID, v.PolicyVersionID, meta, actor, summary))
		}
		return nil
	})
	if err != nil {
		return nil, s.domainError("RunTests", err)
	}
	return out, nil
}

// ListTestRuns returns a version's runs, newest first.
func (s *PgStore) ListTestRuns(ctx context.Context, versionID string) ([]*domain.TestRun, error) {
	if _, err := s.FindPolicyVersionByID(ctx, versionID); err != nil {
		return nil, err
	}
	return listRows(ctx, s, "ListTestRuns",
		`SELECT `+testRunColumns+` FROM policy_test_runs WHERE policy_version_id = $1 ORDER BY run_at DESC, test_run_id DESC;`,
		[]any{versionID}, scanTestRun)
}

// GetTestRun returns one run visible to the caller.
func (s *PgStore) GetTestRun(ctx context.Context, runID string) (*domain.TestRun, error) {
	runs, err := listRows(ctx, s, "GetTestRun", `SELECT `+testRunColumns+` FROM policy_test_runs WHERE test_run_id = $1;`, []any{runID}, scanTestRun)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, domain.ErrPolicyVersionNotFound
	}
	return runs[0], nil
}

// ── certification and the gate ───────────────────────────────────────────────

// certificationTx is a version's standing against its family's suite now.
func (s *PgStore) certificationTx(ctx context.Context, tx pgx.Tx, v *domain.PolicyVersion, policyType string) (*domain.Certification, error) {
	c := &domain.Certification{PolicyVersionID: v.PolicyVersionID}
	if !evaluator.Supports(policyType) {
		// No evaluator, nothing to test: certified by authoring-time
		// validation alone (SPEC_DEVIATIONS: accounting policies are
		// resolved, not evaluated; P-01 types await their rules).
		c.Status, c.Reason = domain.CertificationExempt, "policy type "+policyType+" has no evaluator"
		return c, nil
	}
	_, suiteHash, err := activeCases(ctx, tx, v.PolicyID)
	if err != nil {
		return nil, err
	}
	c.SuiteHash = suiteHash
	run, err := scanTestRun(tx.QueryRow(ctx, `SELECT `+testRunColumns+` FROM policy_test_runs
		WHERE policy_version_id = $1 AND suite_hash = $2 AND artifact_digest = $3
		ORDER BY run_at DESC, test_run_id DESC LIMIT 1;`, v.PolicyVersionID, suiteHash, v.ArtifactDigest))
	if errors.Is(err, pgx.ErrNoRows) {
		var ran bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM policy_test_runs WHERE policy_version_id = $1);`, v.PolicyVersionID).Scan(&ran); err != nil {
			return nil, err
		}
		c.Status, c.Reason = domain.CertificationNotRun, "no test run against the family's current suite"
		if ran {
			c.Status, c.Reason = domain.CertificationStale, "the suite changed since the last run"
		}
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	c.TestRunID = &run.TestRunID
	if run.RunStatus == domain.TestRunPass {
		c.Status = domain.CertificationCertified
		return c, nil
	}
	if len(run.StaticFindings) > 0 {
		c.Status, c.Reason = domain.CertificationFailed, "static validation failed: "+strings.Join(run.StaticFindings, "; ")
		return c, nil
	}
	waived := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT test_case_id FROM policy_test_waivers
		WHERE policy_version_id = $1 AND waiver_status = 'APPROVED' AND expires_at > $2;`, v.PolicyVersionID, s.now())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		waived[id] = true
	}
	rows.Close()
	for _, r := range run.Results {
		if r.Mandatory && !r.Passed && !waived[r.TestCaseID] {
			c.UnwaivedFailed = append(c.UnwaivedFailed, r.TestCaseID)
		}
	}
	sort.Strings(c.UnwaivedFailed)
	if len(c.UnwaivedFailed) > 0 {
		c.Status, c.Reason = domain.CertificationFailed, "mandatory cases failed without a valid waiver"
		return c, nil
	}
	c.Status, c.Reason = domain.CertificationWaived, "every failed mandatory case has an approved, unexpired waiver"
	return c, nil
}

// Certification reports a version's certification (read-only).
func (s *PgStore) Certification(ctx context.Context, versionID string) (*domain.Certification, error) {
	var out *domain.Certification
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		v, policyType, err := lockVersionShared(ctx, tx, versionID)
		if err != nil {
			return err
		}
		out, err = s.certificationTx(ctx, tx, v, policyType)
		return err
	})
	if err != nil {
		return nil, s.domainError("Certification", err)
	}
	return out, nil
}

func lockVersionShared(ctx context.Context, tx pgx.Tx, id string) (*domain.PolicyVersion, string, error) {
	var policyType string
	v, err := scanPolicyVersion(tx.QueryRow(ctx, `
		SELECT `+versionColumns("pv")+`, p.policy_type
		FROM policy_versions pv JOIN policies p ON p.policy_id = pv.policy_id
		WHERE pv.policy_version_id = $1;`, id), &policyType)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", domain.ErrPolicyVersionNotFound
	}
	return v, policyType, err
}

// guardCertification is the activation gate (PDC-I-24, NP-41).
func (s *PgStore) guardCertification(ctx context.Context, tx pgx.Tx, v *domain.PolicyVersion, policyType string) error {
	if s.gate != domain.GateEnforce && s.gate != domain.GateWarn {
		return nil
	}
	c, err := s.certificationTx(ctx, tx, v, policyType)
	if err != nil {
		return err
	}
	if c.Certified() {
		return nil
	}
	if s.gate == domain.GateWarn {
		s.log.Warn("certification gate (warn): activating an uncertified version",
			zap.String("policy_version_id", v.PolicyVersionID), zap.String("status", c.Status), zap.String("reason", c.Reason))
		return nil
	}
	return fmt.Errorf("%w: %s (%s)", domain.ErrCertificationRequired, c.Status, c.Reason)
}

// ── waivers ──────────────────────────────────────────────────────────────────

// RequestWaiver asks for a waiver of one mandatory case of one version.
func (s *PgStore) RequestWaiver(ctx context.Context, p domain.RequestWaiverParams) (*domain.Waiver, error) {
	if p.WaiverID == "" {
		p.WaiverID = uuid.NewString()
	}
	var out *domain.Waiver
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		v, _, err := lockVersionShared(ctx, tx, p.PolicyVersionID)
		if err != nil {
			return err
		}
		now := s.now()
		if !p.ExpiresAt.After(now) || p.ExpiresAt.Sub(now) > maxWaiverLife {
			return fmt.Errorf("%w: expires_at must be in the future and at most %d days away", domain.ErrInvalidWaiver, int(maxWaiverLife.Hours()/24))
		}
		var mandatory bool
		err = tx.QueryRow(ctx, `SELECT mandatory FROM policy_test_cases WHERE test_case_id = $1 AND policy_id = $2;`,
			p.TestCaseID, v.PolicyID).Scan(&mandatory)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrTestCaseNotFound
		}
		if err != nil {
			return err
		}
		if !mandatory {
			return fmt.Errorf("%w: only a mandatory case needs a waiver", domain.ErrInvalidWaiver)
		}
		out, err = scanWaiver(tx.QueryRow(ctx, `
			INSERT INTO policy_test_waivers (waiver_id, tenant_id, policy_version_id, test_case_id, reason_code, justification,
			    expires_at, requested_by_principal_id, requested_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+waiverColumns+`;`,
			p.WaiverID, v.TenantID, p.PolicyVersionID, p.TestCaseID, p.ReasonCode, p.Justification, p.ExpiresAt, p.ActorID, now))
		if isUniqueViolation(err) {
			return domain.ErrConflict
		}
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, pdcEvent("policy.waiver.requested", out.TenantID, out.WaiverID, p.Meta, p.ActorID, out))
	})
	if err != nil {
		return nil, s.domainError("RequestWaiver", err)
	}
	return out, nil
}

func (s *PgStore) decideWaiver(ctx context.Context, p domain.WaiverDecisionParams, approve bool) (*domain.Waiver, error) {
	var out *domain.Waiver
	op := "RevokeWaiver"
	if approve {
		op = "ApproveWaiver"
	}
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		w, err := scanWaiver(tx.QueryRow(ctx, `SELECT `+waiverColumns+` FROM policy_test_waivers WHERE waiver_id = $1 FOR UPDATE;`, p.WaiverID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrWaiverNotFound
		}
		if err != nil {
			return err
		}
		now := s.now()
		var eventType string
		switch {
		case approve && w.WaiverStatus == domain.WaiverApproved, !approve && w.WaiverStatus == domain.WaiverRevoked:
			out = w
			return nil
		case approve && w.WaiverStatus != domain.WaiverRequested:
			return domain.ErrInvalidTransition
		case approve && p.ActorID == w.RequestedByPrincipalID:
			return domain.ErrSoDConflict
		case approve && !w.ExpiresAt.After(now):
			return fmt.Errorf("%w: the waiver has expired", domain.ErrInvalidWaiver)
		case approve:
			out, err = scanWaiver(tx.QueryRow(ctx, `UPDATE policy_test_waivers
				SET waiver_status = 'APPROVED', approved_by_principal_id = $2, approved_at = $3
				WHERE waiver_id = $1 RETURNING `+waiverColumns+`;`, p.WaiverID, p.ActorID, now))
			eventType = "policy.waiver.approved"
		default:
			out, err = scanWaiver(tx.QueryRow(ctx, `UPDATE policy_test_waivers
				SET waiver_status = 'REVOKED', revoked_by_principal_id = $2, revoked_at = $3, revocation_reason = $4
				WHERE waiver_id = $1 RETURNING `+waiverColumns+`;`, p.WaiverID, p.ActorID, now, p.Reason))
			eventType = "policy.waiver.revoked"
		}
		if err != nil {
			return err
		}
		return s.enqueue(ctx, tx, pdcEvent(eventType, out.TenantID, out.WaiverID, p.Meta, p.ActorID, out))
	})
	if err != nil {
		return nil, s.domainError(op, err)
	}
	return out, nil
}

// ApproveWaiver approves a requested waiver; the approver is never the requester.
func (s *PgStore) ApproveWaiver(ctx context.Context, p domain.WaiverDecisionParams) (*domain.Waiver, error) {
	return s.decideWaiver(ctx, p, true)
}

// RevokeWaiver withdraws a waiver, requested or approved.
func (s *PgStore) RevokeWaiver(ctx context.Context, p domain.WaiverDecisionParams) (*domain.Waiver, error) {
	return s.decideWaiver(ctx, p, false)
}

// ListWaivers returns a version's waivers, newest first.
func (s *PgStore) ListWaivers(ctx context.Context, versionID string) ([]*domain.Waiver, error) {
	if _, err := s.FindPolicyVersionByID(ctx, versionID); err != nil {
		return nil, err
	}
	return listRows(ctx, s, "ListWaivers",
		`SELECT `+waiverColumns+` FROM policy_test_waivers WHERE policy_version_id = $1 ORDER BY requested_at DESC, waiver_id DESC;`,
		[]any{versionID}, scanWaiver)
}

// ── simulation ───────────────────────────────────────────────────────────────

// MaxSimulationCorpus bounds one simulation; a larger corpus is PARTIAL.
const MaxSimulationCorpus = 5000

// Simulate evaluates the candidate (and the baseline, when there is one)
// over the caller tenant's own recorded evaluations of the candidate's
// policy type and records the decisions whose result would change. It reads
// the evaluations, evaluates in memory and writes only the simulation and its
// event: no evaluation, evidence, version or workflow is touched (PDC-I-23,
// NP-45), and another tenant's decisions are never read (NP-44).
func (s *PgStore) Simulate(ctx context.Context, p domain.SimulationParams) (*domain.SimulationRun, error) {
	if p.SimulationID == "" {
		p.SimulationID = uuid.NewString()
	}
	var out *domain.SimulationRun
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		cand, policyType, err := lockVersionShared(ctx, tx, p.CandidateVersionID)
		if err != nil {
			return err
		}
		if !evaluator.Supports(policyType) {
			return domain.ErrNoEvaluator
		}
		var base *domain.PolicyVersion
		if p.BaselineVersionID != "" {
			var baseType string
			if base, baseType, err = lockVersionShared(ctx, tx, p.BaselineVersionID); err != nil {
				return err
			}
			if base.PolicyID != cand.PolicyID || baseType != policyType {
				return fmt.Errorf("%w: the baseline is not a version of the candidate's family", domain.ErrInvalidScope)
			}
		} else {
			base, err = scanPolicyVersion(tx.QueryRow(ctx, `SELECT `+policyVersionColumns+` FROM policy_versions
				WHERE policy_id = $1 AND version_status = 'ACTIVE'
				  AND tenant_id IS NOT DISTINCT FROM $2::uuid AND legal_entity_id IS NOT DISTINCT FROM $3::uuid
				  AND policy_version_id <> $4
				ORDER BY effective_from DESC LIMIT 1;`, cand.PolicyID, cand.TenantID, cand.LegalEntityID, cand.PolicyVersionID))
			if errors.Is(err, pgx.ErrNoRows) {
				base, err = nil, nil
			}
			if err != nil {
				return err
			}
		}
		limit := p.CorpusLimit
		if limit <= 0 || limit > MaxSimulationCorpus {
			limit = MaxSimulationCorpus
		}
		run := &domain.SimulationRun{
			SimulationID: p.SimulationID, TenantID: p.TenantID, PolicyType: policyType, CandidateVersionID: cand.PolicyVersionID,
			CorpusFrom: p.CorpusFrom, CorpusTo: p.CorpusTo, CorpusLimit: limit, RequestedByPrincipalID: p.ActorID, CreatedAt: s.now(),
		}
		if base != nil {
			run.BaselineVersionID = &base.PolicyVersionID
		}
		rows, err := tx.Query(ctx, `SELECT evaluation_id, decision_id, action_context, result FROM policy_evaluations
			WHERE tenant_id = $1::uuid AND policy_type = $2 AND evaluated_at >= $3 AND evaluated_at < $4
			ORDER BY evaluated_at DESC, evaluation_id DESC LIMIT $5;`, p.TenantID, policyType, p.CorpusFrom, p.CorpusTo, limit+1)
		if err != nil {
			return err
		}
		type sample struct {
			id, decision, recorded string
			action                 []byte
		}
		var corpus []sample
		for rows.Next() {
			var c sample
			if err := rows.Scan(&c.id, &c.decision, &c.action, &c.recorded); err != nil {
				rows.Close()
				return err
			}
			corpus = append(corpus, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		truncated := len(corpus) > limit
		if truncated {
			corpus = corpus[:limit]
		}
		run.CorpusSize = len(corpus)
		var diffs []domain.SimulationDiff
		for _, c := range corpus {
			baseline := c.recorded // as-issued, when nothing governs to compare with
			if base != nil {
				if o, err := evaluator.Evaluate(policyType, base.PolicyVersionID, base.RulePayload, c.action); err == nil {
					baseline = o.Result
				}
			}
			d := domain.SimulationDiff{EvaluationID: c.id, DecisionID: c.decision, BaselineResult: &baseline}
			o, err := evaluator.Evaluate(policyType, cand.PolicyVersionID, cand.RulePayload, c.action)
			switch {
			case err != nil:
				msg := err.Error()
				d.CandidateError = &msg
				run.ErrorCount++
			case o.Result != baseline:
				result := o.Result
				d.CandidateResult = &result
				run.ChangedCount++
			default:
				continue
			}
			diffs = append(diffs, d)
		}
		run.SimStatus = domain.SimulationComplete
		if truncated || run.ErrorCount > 0 {
			run.SimStatus = domain.SimulationPartial
		}
		done := s.now()
		run.CompletedAt = &done
		if _, err := tx.Exec(ctx, `INSERT INTO policy_simulation_runs (simulation_id, tenant_id, policy_type, candidate_version_id,
			    baseline_version_id, sim_status, corpus_from, corpus_to, corpus_limit, corpus_size, changed_count, error_count,
			    requested_by_principal_id, created_at, completed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15);`,
			run.SimulationID, run.TenantID, run.PolicyType, run.CandidateVersionID, run.BaselineVersionID, run.SimStatus,
			run.CorpusFrom, run.CorpusTo, run.CorpusLimit, run.CorpusSize, run.ChangedCount, run.ErrorCount,
			run.RequestedByPrincipalID, run.CreatedAt, run.CompletedAt); err != nil {
			return err
		}
		for _, d := range diffs {
			if _, err := tx.Exec(ctx, `INSERT INTO policy_simulation_diffs (simulation_id, evaluation_id, tenant_id, decision_id,
				    baseline_result, candidate_result, candidate_error) VALUES ($1, $2, $3, $4, $5, $6, $7);`,
				run.SimulationID, d.EvaluationID, p.TenantID, d.DecisionID, d.BaselineResult, d.CandidateResult, d.CandidateError); err != nil {
				return err
			}
		}
		out = run
		tenant := p.TenantID
		return s.enqueue(ctx, tx, pdcEvent("policy.simulation.completed", &tenant, run.SimulationID, p.Meta, p.ActorID, run))
	})
	if err != nil {
		return nil, s.domainError("Simulate", err)
	}
	return out, nil
}

// GetSimulation returns one of the caller tenant's simulations.
func (s *PgStore) GetSimulation(ctx context.Context, id string) (*domain.SimulationRun, error) {
	runs, err := listRows(ctx, s, "GetSimulation", `SELECT `+simulationColumns+` FROM policy_simulation_runs WHERE simulation_id = $1;`, []any{id}, scanSimulation)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, domain.ErrSimulationNotFound
	}
	return runs[0], nil
}

// ListSimulationDiffs pages a simulation's changed decisions.
func (s *PgStore) ListSimulationDiffs(ctx context.Context, id string, q domain.ListQuery) ([]*domain.SimulationDiff, error) {
	if _, err := s.GetSimulation(ctx, id); err != nil {
		return nil, err
	}
	return listRows(ctx, s, "ListSimulationDiffs", `SELECT evaluation_id, decision_id, baseline_result, candidate_result, candidate_error
		FROM policy_simulation_diffs WHERE simulation_id = $1 ORDER BY evaluation_id LIMIT $2 OFFSET $3;`,
		[]any{id, q.Limit, q.Offset}, func(row pgx.Row) (*domain.SimulationDiff, error) {
			d := &domain.SimulationDiff{}
			return d, row.Scan(&d.EvaluationID, &d.DecisionID, &d.BaselineResult, &d.CandidateResult, &d.CandidateError)
		})
}

// ── rollback drafts ──────────────────────────────────────────────────────────

// RollbackDraft starts a governed rollback (PDC-I-28, NP-48): a new DRAFT in
// the target's family and scope carrying the target's payload, taking effect
// no earlier than now. The target must have governed once (SUPERSEDED or
// RETIRED after activation). Nothing is mutated; the draft goes through
// review, approval, certification and activation like any other.
func (s *PgStore) RollbackDraft(ctx context.Context, targetID string, effectiveFrom time.Time, actor, rationale, requestKey string, meta domain.EventMeta) (*domain.PolicyVersion, bool, error) {
	var out *domain.PolicyVersion
	var created bool
	err := s.withRLS(ctx, contextTenantID(ctx), func(tx pgx.Tx) error {
		target, policyType, err := lockVersionShared(ctx, tx, targetID)
		if err != nil {
			return err
		}
		if policyType == domain.PolicyTypeAccounting || target.ActivatedAt == nil ||
			(target.VersionStatus != domain.StatusSuperseded && target.VersionStatus != domain.StatusRetired) {
			return domain.ErrNotRollbackEligible
		}
		if effectiveFrom.Before(s.now().Add(-time.Minute)) {
			return fmt.Errorf("%w: a rollback never takes effect in the past", domain.ErrNotRollbackEligible)
		}
		out, created, err = s.createVersionTx(ctx, tx, domain.CreatePolicyVersionParams{
			PolicyID: target.PolicyID, TenantID: target.TenantID, LegalEntityID: target.LegalEntityID,
			RulePayload: target.RulePayload, EffectiveFrom: effectiveFrom, Source: "ROLLBACK:" + target.PolicyVersionID,
			Rationale: rationale, CreatedByPrincipalID: actor, RequestKey: requestKey, Meta: meta,
		})
		return err
	})
	if err != nil {
		return nil, false, s.domainError("RollbackDraft", err)
	}
	return out, created, nil
}
