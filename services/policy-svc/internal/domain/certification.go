package domain

import (
	"encoding/json"
	"time"
)

// V-001 PDC-05: certification test suites, waivers and simulation.

// Test layers a case belongs to (V-001 §17.1).
const (
	TestLayerUnit       = "UNIT"
	TestLayerBoundary   = "BOUNDARY"
	TestLayerRegression = "REGRESSION"
)

// Test run outcomes. A run with failed cases that are all covered by valid
// waivers is still FAIL; certification reads it as WAIVED (PDC-I-25: a
// waiver is never a pass).
const (
	TestRunPass = "PASS"
	TestRunFail = "FAIL"

	CertificationCertified = "CERTIFIED"
	CertificationWaived    = "WAIVED"
	CertificationFailed    = "FAILED"
	CertificationNotRun    = "NOT_RUN"
	CertificationStale     = "STALE" // the suite or the version changed since the last run
	CertificationExempt    = "EXEMPT_NO_EVALUATOR"
)

// Waiver states.
const (
	WaiverRequested = "REQUESTED"
	WaiverApproved  = "APPROVED"
	WaiverRevoked   = "REVOKED"
)

// Simulation states (V-001 §5).
const (
	SimulationComplete = "COMPLETE"
	SimulationPartial  = "PARTIAL"
	SimulationFailed   = "FAILED"
)

// Certification gate modes (POLICY_CERTIFICATION_GATE).
const (
	GateEnforce = "enforce"
	GateWarn    = "warn"
	GateOff     = "off"
)

// TestCase is one known input and its expected result.
type TestCase struct {
	TestCaseID           string          `json:"test_case_id"`
	PolicyID             string          `json:"policy_id"`
	TenantID             *string         `json:"tenant_id,omitempty"`
	TestCode             string          `json:"test_code"`
	Layer                string          `json:"layer"`
	Mandatory            bool            `json:"mandatory"`
	Description          string          `json:"description"`
	ActionContext        json.RawMessage `json:"action_context"`
	ExpectedResult       string          `json:"expected_result"`
	TestStatus           string          `json:"test_status"`
	CreatedAt            time.Time       `json:"created_at"`
	CreatedByPrincipalID string          `json:"created_by_principal_id"`
	RetiredAt            *time.Time      `json:"retired_at,omitempty"`
	RetiredByPrincipalID *string         `json:"retired_by_principal_id,omitempty"`
}

// CreateTestCaseParams adds a case to a family's suite.
type CreateTestCaseParams struct {
	TestCaseID     string
	PolicyID       string
	TestCode       string
	Layer          string
	Mandatory      bool
	Description    string
	ActionContext  json.RawMessage
	ExpectedResult string
	ActorID        string
	Meta           EventMeta
}

// CaseResult is one case's outcome in a run.
type CaseResult struct {
	TestCaseID string `json:"test_case_id"`
	TestCode   string `json:"test_code"`
	Mandatory  bool   `json:"mandatory"`
	Expected   string `json:"expected"`
	Actual     string `json:"actual,omitempty"`
	Error      string `json:"error,omitempty"`
	Passed     bool   `json:"passed"`
}

// TestRun is one immutable run of a family's suite against a version.
type TestRun struct {
	TestRunID        string       `json:"test_run_id"`
	TenantID         *string      `json:"tenant_id,omitempty"`
	PolicyID         string       `json:"policy_id"`
	PolicyVersionID  string       `json:"policy_version_id"`
	ArtifactDigest   string       `json:"artifact_digest"`
	SuiteHash        string       `json:"suite_hash"`
	EvaluatorVersion string       `json:"evaluator_version"`
	RunStatus        string       `json:"run_status"`
	CaseCount        int          `json:"case_count"`
	FailedCount      int          `json:"failed_count"`
	StaticFindings   []string     `json:"static_findings"`
	Results          []CaseResult `json:"results"`
	RunByPrincipalID string       `json:"run_by_principal_id"`
	RunAt            time.Time    `json:"run_at"`
}

// Waiver excuses one failed mandatory case of one version, for a time.
type Waiver struct {
	WaiverID               string     `json:"waiver_id"`
	TenantID               *string    `json:"tenant_id,omitempty"`
	PolicyVersionID        string     `json:"policy_version_id"`
	TestCaseID             string     `json:"test_case_id"`
	ReasonCode             string     `json:"reason_code"`
	Justification          string     `json:"justification"`
	ExpiresAt              time.Time  `json:"expires_at"`
	WaiverStatus           string     `json:"waiver_status"`
	RequestedByPrincipalID string     `json:"requested_by_principal_id"`
	RequestedAt            time.Time  `json:"requested_at"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	RevokedByPrincipalID   *string    `json:"revoked_by_principal_id,omitempty"`
	RevokedAt              *time.Time `json:"revoked_at,omitempty"`
	RevocationReason       *string    `json:"revocation_reason,omitempty"`
}

// RequestWaiverParams asks for a waiver.
type RequestWaiverParams struct {
	WaiverID        string
	PolicyVersionID string
	TestCaseID      string
	ReasonCode      string
	Justification   string
	ExpiresAt       time.Time
	ActorID         string
	Meta            EventMeta
}

// WaiverDecisionParams approves or revokes a waiver.
type WaiverDecisionParams struct {
	WaiverID string
	ActorID  string
	Reason   string
	Meta     EventMeta
}

// Certification is a version's standing against its family's current suite.
type Certification struct {
	PolicyVersionID string   `json:"policy_version_id"`
	Status          string   `json:"status"`
	SuiteHash       string   `json:"suite_hash"`
	TestRunID       *string  `json:"test_run_id,omitempty"`
	UnwaivedFailed  []string `json:"unwaived_failed_test_case_ids,omitempty"`
	Reason          string   `json:"reason,omitempty"`
}

// Certified reports whether activation may proceed on this certification.
func (c Certification) Certified() bool {
	return c.Status == CertificationCertified || c.Status == CertificationWaived || c.Status == CertificationExempt
}

// SimulationParams asks for a differential simulation.
type SimulationParams struct {
	SimulationID       string
	TenantID           string
	CandidateVersionID string
	BaselineVersionID  string // empty: the version governing the candidate's scope now, if any
	CorpusFrom         time.Time
	CorpusTo           time.Time
	CorpusLimit        int
	ActorID            string
	Meta               EventMeta
}

// SimulationRun is one simulation's summary.
type SimulationRun struct {
	SimulationID           string     `json:"simulation_id"`
	TenantID               string     `json:"tenant_id"`
	PolicyType             string     `json:"policy_type"`
	CandidateVersionID     string     `json:"candidate_version_id"`
	BaselineVersionID      *string    `json:"baseline_version_id,omitempty"`
	SimStatus              string     `json:"sim_status"`
	CorpusFrom             time.Time  `json:"corpus_from"`
	CorpusTo               time.Time  `json:"corpus_to"`
	CorpusLimit            int        `json:"corpus_limit"`
	CorpusSize             int        `json:"corpus_size"`
	ChangedCount           int        `json:"changed_count"`
	ErrorCount             int        `json:"error_count"`
	RequestedByPrincipalID string     `json:"requested_by_principal_id"`
	CreatedAt              time.Time  `json:"created_at"`
	CompletedAt            *time.Time `json:"completed_at,omitempty"`
}

// SimulationDiff is one recorded decision whose result would change.
type SimulationDiff struct {
	EvaluationID    string  `json:"evaluation_id"`
	DecisionID      string  `json:"decision_id"`
	BaselineResult  *string `json:"baseline_result,omitempty"`
	CandidateResult *string `json:"candidate_result,omitempty"`
	CandidateError  *string `json:"candidate_error,omitempty"`
}

// PDC-05 errors.
var (
	ErrCertificationRequired = errorString("the version is not certified against its family's current test suite")
	ErrTestCaseNotFound      = errorString("test case not found")
	ErrWaiverNotFound        = errorString("waiver not found")
	ErrInvalidWaiver         = errorString("waiver is not valid for this version and case")
	ErrNoEvaluator           = errorString("the policy type has no evaluator")
	ErrSimulationNotFound    = errorString("simulation not found")
	ErrNotRollbackEligible   = errorString("the version is not an eligible rollback target")
)
