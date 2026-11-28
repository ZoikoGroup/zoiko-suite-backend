package handler

// V-001 PDC-05: certification test suites, waivers, simulation and governed
// rollback drafts.
//
//	POST /v1/policies/{policy_id}/test-cases                        POLICY_TEST_MANAGE
//	GET  /v1/policies/{policy_id}/test-cases                        POLICY_READ
//	POST /v1/policies/{policy_id}/test-cases/{test_case_id}/retire  POLICY_TEST_MANAGE
//	POST /v1/policy-versions/{version_id}/test-runs                 POLICY_TEST_RUN
//	GET  /v1/policy-versions/{version_id}/test-runs                 POLICY_READ
//	GET  /v1/policy-test-runs/{test_run_id}                         POLICY_READ
//	GET  /v1/policy-versions/{version_id}/certification             POLICY_READ
//	POST /v1/policy-versions/{version_id}/waivers                   POLICY_WAIVER_REQUEST
//	GET  /v1/policy-versions/{version_id}/waivers                   POLICY_READ
//	POST /v1/policy-waivers/{waiver_id}/approve                     POLICY_WAIVER_APPROVE (≠ requester)
//	POST /v1/policy-waivers/{waiver_id}/revoke                      POLICY_WAIVER_APPROVE
//	POST /v1/simulations                                            POLICY_SIMULATE
//	GET  /v1/simulations/{simulation_id}                            POLICY_READ
//	GET  /v1/simulations/{simulation_id}/differences                POLICY_READ
//	POST /v1/policy-versions/{version_id}/rollback-draft            POLICY_VERSION_CREATE[_GLOBAL]

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
)

const (
	codeCertificationRequired = "CERTIFICATION_REQUIRED"
	codeNoEvaluator           = "NO_EVALUATOR"

	// defaultSimulationWindow is the corpus a simulation reads when the
	// request names none: the last 90 days of the tenant's evaluations.
	defaultSimulationWindow = 90 * 24 * time.Hour
)

type certificationStore interface {
	CreateTestCase(ctx context.Context, p domain.CreateTestCaseParams) (*domain.TestCase, bool, error)
	ListTestCases(ctx context.Context, policyID string) ([]*domain.TestCase, error)
	RetireTestCase(ctx context.Context, policyID, testCaseID, actor string, meta domain.EventMeta) (*domain.TestCase, error)
	RunTests(ctx context.Context, versionID, actor string, meta domain.EventMeta) (*domain.TestRun, error)
	ListTestRuns(ctx context.Context, versionID string) ([]*domain.TestRun, error)
	GetTestRun(ctx context.Context, runID string) (*domain.TestRun, error)
	Certification(ctx context.Context, versionID string) (*domain.Certification, error)
	RequestWaiver(ctx context.Context, p domain.RequestWaiverParams) (*domain.Waiver, error)
	ApproveWaiver(ctx context.Context, p domain.WaiverDecisionParams) (*domain.Waiver, error)
	RevokeWaiver(ctx context.Context, p domain.WaiverDecisionParams) (*domain.Waiver, error)
	ListWaivers(ctx context.Context, versionID string) ([]*domain.Waiver, error)
	Simulate(ctx context.Context, p domain.SimulationParams) (*domain.SimulationRun, error)
	GetSimulation(ctx context.Context, id string) (*domain.SimulationRun, error)
	ListSimulationDiffs(ctx context.Context, id string, q domain.ListQuery) ([]*domain.SimulationDiff, error)
	RollbackDraft(ctx context.Context, targetID string, effectiveFrom time.Time, actor, rationale, requestKey string, meta domain.EventMeta) (*domain.PolicyVersion, bool, error)
}

// RegisterCertificationRoutes mounts the PDC-05 routes.
func RegisterCertificationRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/policies/{policy_id}/test-cases", h.CreateTestCase)
	r.Get("/v1/policies/{policy_id}/test-cases", h.ListTestCases)
	r.Post("/v1/policies/{policy_id}/test-cases/{test_case_id}/retire", h.RetireTestCase)
	r.Post("/v1/policy-versions/{version_id}/test-runs", h.RunTests)
	r.Get("/v1/policy-versions/{version_id}/test-runs", h.ListTestRuns)
	r.Get("/v1/policy-test-runs/{test_run_id}", h.GetTestRun)
	r.Get("/v1/policy-versions/{version_id}/certification", h.GetCertification)
	r.Post("/v1/policy-versions/{version_id}/waivers", h.RequestWaiver)
	r.Get("/v1/policy-versions/{version_id}/waivers", h.ListWaivers)
	r.Post("/v1/policy-waivers/{waiver_id}/approve", h.ApproveWaiver)
	r.Post("/v1/policy-waivers/{waiver_id}/revoke", h.RevokeWaiver)
	r.Post("/v1/simulations", h.Simulate)
	r.Get("/v1/simulations/{simulation_id}", h.GetSimulation)
	r.Get("/v1/simulations/{simulation_id}/differences", h.ListSimulationDiffs)
	r.Post("/v1/policy-versions/{version_id}/rollback-draft", h.RollbackDraft)
}

func (h *Handler) certification(w http.ResponseWriter, r *http.Request) (certificationStore, bool) {
	s, ok := h.store.(certificationStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "store_unavailable", "the store does not serve certification", nil)
	}
	return s, ok
}

// pdcFailed maps a PDC-05 store refusal.
func (h *Handler) pdcFailed(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrPolicyNotFound), errors.Is(err, domain.ErrPolicyVersionNotFound),
		errors.Is(err, domain.ErrTestCaseNotFound), errors.Is(err, domain.ErrWaiverNotFound),
		errors.Is(err, domain.ErrSimulationNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, domain.ErrNoEvaluator):
		writeError(w, r, http.StatusConflict, codeNoEvaluator, "no_evaluator",
			"this policy type has no evaluator, so it has no test suite and cannot be simulated", nil)
	case errors.Is(err, domain.ErrConflict):
		writeError(w, r, http.StatusConflict, codeIdempotencyMismatch, "conflict", "the id or code was already used with different content", nil)
	case errors.Is(err, domain.ErrSoDConflict):
		writeError(w, r, http.StatusForbidden, codeSoDConflict, "maker_checker_violation", "the requester of a waiver cannot approve it", nil)
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, r, http.StatusConflict, codeConcurrency, "invalid_transition", "the waiver's current status does not allow this command", nil)
	case errors.Is(err, domain.ErrInvalidWaiver), errors.Is(err, domain.ErrInvalidScope), errors.Is(err, domain.ErrNotRollbackEligible):
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidRequest, "not_allowed", err.Error(), nil)
	case errors.Is(err, domain.ErrPolicyRetired):
		writeError(w, r, http.StatusConflict, codePolicyRetired, "policy_retired", "the policy family is retired", nil)
	default:
		h.storeUnavailable(w, r, op, err)
	}
}

// ── test cases ───────────────────────────────────────────────────────────────

type createTestCaseRequest struct {
	TestCaseID     string          `json:"test_case_id,omitempty"`
	TestCode       string          `json:"test_code"`
	Layer          string          `json:"layer"`
	Mandatory      *bool           `json:"mandatory,omitempty"`
	Description    string          `json:"description"`
	ActionContext  json.RawMessage `json:"action_context"`
	ExpectedResult string          `json:"expected_result"`
}

// CreateTestCase handles POST /v1/policies/{policy_id}/test-cases.
func (h *Handler) CreateTestCase(w http.ResponseWriter, r *http.Request) {
	s, ok := h.certification(w, r)
	if !ok {
		return
	}
	policyID := chi.URLParam(r, "policy_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok || !requireUUID(w, r, "policy_id", policyID) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createTestCaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Layer = strings.ToUpper(strings.TrimSpace(req.Layer))
	switch {
	case req.TestCode == "":
		missingField(w, r, "test_code")
		return
	case req.Description == "":
		missingField(w, r, "description")
		return
	case req.ExpectedResult == "":
		missingField(w, r, "expected_result")
		return
	case len(req.ActionContext) == 0:
		missingField(w, r, "action_context")
		return
	case !contains([]string{domain.TestLayerUnit, domain.TestLayerBoundary, domain.TestLayerRegression}, req.Layer):
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_layer", "layer must be UNIT, BOUNDARY or REGRESSION", nil)
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(req.ActionContext, &obj) != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_action_context", "action_context must be a JSON object", nil)
		return
	}
	if (req.TestCaseID != "" && !requireUUID(w, r, "test_case_id", req.TestCaseID)) ||
		tooLong(w, r, "test_code", req.TestCode, 128) || tooLong(w, r, "expected_result", req.ExpectedResult, 64) ||
		tooLong(w, r, "description", req.Description, 4000) {
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionPolicyTestManage, nil) {
		return
	}
	mandatory := req.Mandatory == nil || *req.Mandatory
	c, created, err := s.CreateTestCase(r.Context(), domain.CreateTestCaseParams{
		TestCaseID: req.TestCaseID, PolicyID: policyID, TestCode: req.TestCode, Layer: req.Layer, Mandatory: mandatory,
		Description: req.Description, ActionContext: req.ActionContext, ExpectedResult: req.ExpectedResult,
		ActorID: principalID, Meta: eventMeta(r, principalID),
	})
	if err != nil {
		h.pdcFailed(w, r, "CreateTestCase", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, c)
}

// ListTestCases handles GET /v1/policies/{policy_id}/test-cases.
func (h *Handler) ListTestCases(w http.ResponseWriter, r *http.Request) {
	s, ok := h.certification(w, r)
	if !ok {
		return
	}
	policyID := chi.URLParam(r, "policy_id")
	if !requireUUID(w, r, "policy_id", policyID) {
		return
	}
	lr, ok := h.beginList(w, r, nil)
	if !ok {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	cases, err := s.ListTestCases(r.Context(), policyID)
	if err != nil {
		h.pdcFailed(w, r, "ListTestCases", err)
		return
	}
	pageOf(h, w, r, lr, cases)
}

// RetireTestCase handles POST /v1/policies/{policy_id}/test-cases/{test_case_id}/retire.
func (h *Handler) RetireTestCase(w http.ResponseWriter, r *http.Request) {
	s, ok := h.certification(w, r)
	if !ok {
		return
	}
	policyID, caseID := chi.URLParam(r, "policy_id"), chi.URLParam(r, "test_case_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok || !requireUUID(w, r, "policy_id", policyID) || !requireUUID(w, r, "test_case_id", caseID) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionPolicyTestManage, nil) {
		return
	}
	c, err := s.RetireTestCase(r.Context(), policyID, caseID, principalID, eventMeta(r, principalID))
	if err != nil {
		h.pdcFailed(w, r, "RetireTestCase", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ── test runs and certification ──────────────────────────────────────────────

// RunTests handles POST /v1/policy-versions/{version_id}/test-runs: runs the
// family's suite against the version and answers the immutable run.
func (h *Handler) RunTests(w http.ResponseWriter, r *http.Request) {
	s, ok := h.certification(w, r)
	if !ok {
		return
	}
	versionID := chi.URLParam(r, "version_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok || !requireUUID(w, r, "version_id", versionID) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionPolicyTestRun, nil) {
		return
	}
	run, err := s.RunTests(r.Context(), versionID, principalID, eventMeta(r, principalID))
	if err != nil {
		h.pdcFailed(w, r, "RunTests", err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (h *Handler) readGate(w http.ResponseWriter, r *http.Request, param string) (certificationStore, string, bool) {
	s, ok := h.certification(w, r)
	if !ok {
		return nil, "", false
	}
	id := chi.URLParam(r, param)
	if !requireUUID(w, r, param, id) {
		return nil, "", false
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return nil, "", false
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return nil, "", false
	}
	return s, id, true
}

// ListTestRuns handles GET /v1/policy-versions/{version_id}/test-runs.
func (h *Handler) ListTestRuns(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "version_id")
	if !requireUUID(w, r, "version_id", versionID) {
		return
	}
	lr, ok := h.beginList(w, r, nil)
	if !ok {
		return
	}
	s, _, ok := h.readGate(w, r, "version_id")
	if !ok {
		return
	}
	runs, err := s.ListTestRuns(r.Context(), versionID)
	if err != nil {
		h.pdcFailed(w, r, "ListTestRuns", err)
		return
	}
	pageOf(h, w, r, lr, runs)
}

// GetTestRun handles GET /v1/policy-test-runs/{test_run_id}.
func (h *Handler) GetTestRun(w http.ResponseWriter, r *http.Request) {
	s, id, ok := h.readGate(w, r, "test_run_id")
	if !ok {
		return
	}
	run, err := s.GetTestRun(r.Context(), id)
	if err != nil {
		h.pdcFailed(w, r, "GetTestRun", err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// GetCertification handles GET /v1/policy-versions/{version_id}/certification:
// whether the version may be activated under the certification gate, and why.
func (h *Handler) GetCertification(w http.ResponseWriter, r *http.Request) {
	s, id, ok := h.readGate(w, r, "version_id")
	if !ok {
		return
	}
	c, err := s.Certification(r.Context(), id)
	if err != nil {
		h.pdcFailed(w, r, "GetCertification", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ── waivers ──────────────────────────────────────────────────────────────────

type requestWaiverRequest struct {
	WaiverID      string    `json:"waiver_id,omitempty"`
	TestCaseID    string    `json:"test_case_id"`
	ReasonCode    string    `json:"reason_code"`
	Justification string    `json:"justification"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// RequestWaiver handles POST /v1/policy-versions/{version_id}/waivers.
func (h *Handler) RequestWaiver(w http.ResponseWriter, r *http.Request) {
	s, ok := h.certification(w, r)
	if !ok {
		return
	}
	versionID := chi.URLParam(r, "version_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok || !requireUUID(w, r, "version_id", versionID) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req requestWaiverRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case req.TestCaseID == "":
		missingField(w, r, "test_case_id")
		return
	case strings.TrimSpace(req.ReasonCode) == "":
		missingField(w, r, "reason_code")
		return
	case strings.TrimSpace(req.Justification) == "":
		missingField(w, r, "justification")
		return
	case req.ExpiresAt.IsZero():
		// NP-43: a waiver without an expiry is refused.
		missingField(w, r, "expires_at")
		return
	}
	if !requireUUID(w, r, "test_case_id", req.TestCaseID) || (req.WaiverID != "" && !requireUUID(w, r, "waiver_id", req.WaiverID)) ||
		tooLong(w, r, "reason_code", req.ReasonCode, 64) || tooLong(w, r, "justification", req.Justification, 4000) {
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionPolicyWaiverRequest, nil) {
		return
	}
	wv, err := s.RequestWaiver(r.Context(), domain.RequestWaiverParams{
		WaiverID: req.WaiverID, PolicyVersionID: versionID, TestCaseID: req.TestCaseID, ReasonCode: strings.TrimSpace(req.ReasonCode),
		Justification: req.Justification, ExpiresAt: req.ExpiresAt.UTC(), ActorID: principalID, Meta: eventMeta(r, principalID),
	})
	if err != nil {
		h.pdcFailed(w, r, "RequestWaiver", err)
		return
	}
	writeJSON(w, http.StatusCreated, wv)
}

// ListWaivers handles GET /v1/policy-versions/{version_id}/waivers.
func (h *Handler) ListWaivers(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "version_id")
	if !requireUUID(w, r, "version_id", versionID) {
		return
	}
	lr, ok := h.beginList(w, r, nil)
	if !ok {
		return
	}
	s, _, ok := h.readGate(w, r, "version_id")
	if !ok {
		return
	}
	ws, err := s.ListWaivers(r.Context(), versionID)
	if err != nil {
		h.pdcFailed(w, r, "ListWaivers", err)
		return
	}
	pageOf(h, w, r, lr, ws)
}

func (h *Handler) decideWaiver(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.certification(w, r)
		if !ok {
			return
		}
		id := chi.URLParam(r, "waiver_id")
		principalID, ok := h.requirePrincipal(w, r)
		if !ok || !requireUUID(w, r, "waiver_id", id) {
			return
		}
		if _, ok := h.requireTenant(w, r); !ok {
			return
		}
		var req reasonRequest
		if !decodeOptionalJSON(w, r, &req) {
			return
		}
		if !approve && strings.TrimSpace(req.ReasonCode) == "" {
			missingField(w, r, "reason_code")
			return
		}
		if tooLong(w, r, "reason", req.text(), 2000) {
			return
		}
		if !h.authorize(w, r, principalID, authz.ActionPolicyWaiverApprove, nil) {
			return
		}
		p := domain.WaiverDecisionParams{WaiverID: id, ActorID: principalID, Reason: req.text(), Meta: eventMeta(r, principalID)}
		var wv *domain.Waiver
		var err error
		if approve {
			wv, err = s.ApproveWaiver(r.Context(), p)
		} else {
			wv, err = s.RevokeWaiver(r.Context(), p)
		}
		if err != nil {
			h.pdcFailed(w, r, "decideWaiver", err)
			return
		}
		writeJSON(w, http.StatusOK, wv)
	}
}

// ApproveWaiver handles POST /v1/policy-waivers/{waiver_id}/approve.
func (h *Handler) ApproveWaiver(w http.ResponseWriter, r *http.Request) { h.decideWaiver(true)(w, r) }

// RevokeWaiver handles POST /v1/policy-waivers/{waiver_id}/revoke.
func (h *Handler) RevokeWaiver(w http.ResponseWriter, r *http.Request) { h.decideWaiver(false)(w, r) }

// ── simulation ───────────────────────────────────────────────────────────────

type simulateRequest struct {
	SimulationID       string     `json:"simulation_id,omitempty"`
	TenantID           *string    `json:"tenant_id,omitempty"`
	CandidateVersionID string     `json:"candidate_version_id"`
	BaselineVersionID  string     `json:"baseline_version_id,omitempty"`
	CorpusFrom         *time.Time `json:"corpus_from,omitempty"`
	CorpusTo           *time.Time `json:"corpus_to,omitempty"`
	CorpusLimit        int        `json:"corpus_limit,omitempty"`
}

// Simulate handles POST /v1/simulations: candidate against baseline over the
// caller tenant's own recorded evaluations, bounded, with no side effects.
func (h *Handler) Simulate(w http.ResponseWriter, r *http.Request) {
	s, ok := h.certification(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req simulateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// NP-44: a corpus is only ever the caller's own tenant's decisions.
	if h.refuseForeignTenant(w, r, req.TenantID, tenantID) {
		return
	}
	if req.CandidateVersionID == "" {
		missingField(w, r, "candidate_version_id")
		return
	}
	if !requireUUID(w, r, "candidate_version_id", req.CandidateVersionID) ||
		!optionalUUID(w, r, "baseline_version_id", &req.BaselineVersionID) ||
		(req.SimulationID != "" && !requireUUID(w, r, "simulation_id", req.SimulationID)) {
		return
	}
	now := h.opts.Now().UTC()
	to, from := now, now.Add(-defaultSimulationWindow)
	if req.CorpusTo != nil {
		to = req.CorpusTo.UTC()
	}
	if req.CorpusFrom != nil {
		from = req.CorpusFrom.UTC()
	}
	if !to.After(from) {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_temporal_range", "corpus_to must be after corpus_from", nil)
		return
	}
	if req.CorpusLimit < 0 {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_corpus_limit", "corpus_limit must not be negative", nil)
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionPolicySimulate, nil) {
		return
	}
	run, err := s.Simulate(r.Context(), domain.SimulationParams{
		SimulationID: req.SimulationID, TenantID: tenantID, CandidateVersionID: req.CandidateVersionID,
		BaselineVersionID: req.BaselineVersionID, CorpusFrom: from, CorpusTo: to, CorpusLimit: req.CorpusLimit,
		ActorID: principalID, Meta: eventMeta(r, principalID),
	})
	if err != nil {
		h.pdcFailed(w, r, "Simulate", err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// GetSimulation handles GET /v1/simulations/{simulation_id}.
func (h *Handler) GetSimulation(w http.ResponseWriter, r *http.Request) {
	s, id, ok := h.readGate(w, r, "simulation_id")
	if !ok {
		return
	}
	run, err := s.GetSimulation(r.Context(), id)
	if err != nil {
		h.pdcFailed(w, r, "GetSimulation", err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ListSimulationDiffs handles GET /v1/simulations/{simulation_id}/differences.
func (h *Handler) ListSimulationDiffs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "simulation_id")
	if !requireUUID(w, r, "simulation_id", id) {
		return
	}
	lr, ok := h.beginList(w, r, nil)
	if !ok {
		return
	}
	s, _, ok := h.readGate(w, r, "simulation_id")
	if !ok {
		return
	}
	diffs, err := s.ListSimulationDiffs(r.Context(), id, lr.query())
	if err != nil {
		h.pdcFailed(w, r, "ListSimulationDiffs", err)
		return
	}
	finishList(h, w, r, lr, diffs)
}

// ── rollback ─────────────────────────────────────────────────────────────────

type rollbackDraftRequest struct {
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`
	Rationale     string     `json:"rationale"`
}

// RollbackDraft handles POST /v1/policy-versions/{version_id}/rollback-draft:
// a new DRAFT carrying an earlier version's payload, effective no earlier
// than now (PDC-I-28). It then needs review, approval, certification and
// activation like any version.
func (h *Handler) RollbackDraft(w http.ResponseWriter, r *http.Request) {
	s, ok := h.certification(w, r)
	if !ok {
		return
	}
	versionID := chi.URLParam(r, "version_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok || !requireUUID(w, r, "version_id", versionID) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req rollbackDraftRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Rationale) == "" {
		missingField(w, r, "rationale")
		return
	}
	if tooLong(w, r, "rationale", req.Rationale, 4000) {
		return
	}
	effective := h.opts.Now().UTC()
	if req.EffectiveFrom != nil {
		effective = req.EffectiveFrom.UTC()
		if effective.Before(h.opts.Now().UTC().Add(-time.Minute)) {
			// NP-48: a rollback never fabricates an earlier effective date.
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "backdated_rollback",
				"a rollback takes effect now or later, never in the past", nil)
			return
		}
	}
	target, err := h.store.FindPolicyVersionByID(r.Context(), versionID)
	if err != nil {
		h.pdcFailed(w, r, "RollbackDraft", err)
		return
	}
	action := authz.ActionPolicyVersionCreate
	if target.TenantID == nil {
		action = authz.ActionPolicyVersionCreateGlobal
	}
	if !h.authorize(w, r, principalID, action, target.LegalEntityID) {
		return
	}
	meta := eventMeta(r, principalID)
	v, created, err := s.RollbackDraft(r.Context(), versionID, effective, principalID, strings.TrimSpace(req.Rationale), meta.IdempotencyKey, meta)
	if err != nil {
		h.pdcFailed(w, r, "RollbackDraft", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSONWithETag(w, status, v, int64(v.RowVersion))
}
