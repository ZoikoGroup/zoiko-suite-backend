package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/envelope"
	"zoiko.io/policy-svc/internal/resolve"
	"zoiko.io/policy-svc/internal/store"
)

const (
	policyTypeApprovalThreshold = "APPROVAL_THRESHOLD"

	// evaluatorThreshold names the evaluator and its contract version; it is
	// stored with every evaluation so a replay knows which logic produced it
	// (V-001 PDC-I-03, PDC-I-17).
	evaluatorThreshold = "policy-svc/APPROVAL_THRESHOLD@1"
)

// evaluateRequest is the wire shape for POST /v1/policies/evaluate.
//
// DecisionID is the caller's decision_request_id: a retry with the same id
// and the same input returns the original result and evidence, even if the
// policy has changed since (V-001 §19.1).
type evaluateRequest struct {
	PolicyType    string          `json:"policy_type"`
	TenantID      *string         `json:"tenant_id,omitempty"`
	LegalEntityID *string         `json:"legal_entity_id,omitempty"`
	ActionContext json.RawMessage `json:"action_context,omitempty"`
	DecisionID    string          `json:"decision_id"`

	// AsOf is the business time of the decision; KnownAt the knowledge cut
	// (V-001 §8.1). See resolve.Times for the defaults.
	AsOf    *time.Time `json:"as_of,omitempty"`
	KnownAt *time.Time `json:"known_at,omitempty"`

	// Accepted for backward compatibility and IGNORED: the requester is the
	// verified X-Principal-Id, never a value the caller types.
	EvaluatedByPrincipalID string `json:"evaluated_by_principal_id,omitempty"`
}

// evaluateResponse is a successful evaluation. evidence_id identifies the
// durable evidence record (GCP §16: "evidence_id returned for material
// governance decision").
type evaluateResponse struct {
	Result           string    `json:"result"`
	PolicyVersionID  string    `json:"policy_version_id"`
	RuleBasis        string    `json:"rule_basis"`
	EvidenceID       string    `json:"evidence_id"`
	DecisionID       string    `json:"decision_id"`
	PolicyID         string    `json:"policy_id"`
	PolicyCode       string    `json:"policy_code"`
	ArtifactDigest   string    `json:"artifact_digest"`
	SelectedRuleIDs  []string  `json:"selected_rule_ids"`
	EvaluatorVersion string    `json:"evaluator_version"`
	InputFingerprint string    `json:"input_fingerprint"`
	AsOf             time.Time `json:"as_of"`
	KnownAt          time.Time `json:"known_at"`
	LedgerStatus     string    `json:"ledger_status"`
	// Replayed is true when this answer is the stored result of an earlier
	// request with the same decision_id.
	Replayed bool `json:"replayed"`
}

func responseFor(ev *domain.Evaluation, replayed bool) evaluateResponse {
	return evaluateResponse{
		Result:           ev.Result,
		PolicyVersionID:  ev.PolicyVersionID,
		RuleBasis:        ev.RuleBasis,
		EvidenceID:       ev.EvaluationID,
		DecisionID:       ev.DecisionID,
		PolicyID:         ev.PolicyID,
		PolicyCode:       ev.PolicyCode,
		ArtifactDigest:   ev.ArtifactDigest,
		SelectedRuleIDs:  ev.SelectedRuleIDs,
		EvaluatorVersion: ev.EvaluatorVersion,
		InputFingerprint: ev.InputFingerprint,
		AsOf:             ev.AsOf,
		KnownAt:          ev.KnownAt,
		LedgerStatus:     ev.LedgerStatus,
		Replayed:         replayed,
	}
}

// Evaluate handles POST /v1/policies/evaluate — "evaluate policy against
// action context" (Doc 03 §8.1).
//
// Sequence (V-001 §14.1): authorize the caller (POLICY_EVALUATE) under its
// verified tenant; reconcile decision_id; resolve the governing version for
// as_of / known_at; verify its digest; evaluate; store the evaluation and its
// ledger outbox row in one transaction; answer. A 200 therefore always has
// durable evidence behind it, and the ledger copy follows from the outbox.
//
//	200 evaluated (or the stored result of the same decision_id)
//	400 malformed · 401 no identity or tenant · 403 denied or foreign tenant
//	404 POLICY_NOT_EFFECTIVE — nothing governs; the caller decides fail-closed
//	409 POLICY_CONFLICTED, or IDEMPOTENCY_MISMATCH (decision_id reused)
//	500 EVIDENCE_INTEGRITY_FAILED · 501 type has no evaluator · 503 unavailable
func (h *Handler) Evaluate(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req evaluateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case req.PolicyType == "":
		missingField(w, "policy_type")
		return
	case req.DecisionID == "":
		missingField(w, "decision_id")
		return
	}
	if tooLong(w, "policy_type", req.PolicyType, 64) || tooLong(w, "decision_id", req.DecisionID, 64) {
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// tenant_id selects whose policy set decides; omitting it means global
	// policy only. Naming another tenant would borrow their rules.
	if !optionalUUID(w, "tenant_id", req.TenantID) || h.refuseForeignTenant(w, req.TenantID, tenantID) {
		return
	}
	if req.TenantID != nil && *req.TenantID == "" {
		req.TenantID = nil
	}
	legalEntityID, ok := legalEntityFromRequest(w, r, req.LegalEntityID)
	if !ok {
		return
	}
	if len(req.ActionContext) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(req.ActionContext, &obj); err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid_action_context", "action_context must be a JSON object", nil)
			return
		}
	}

	principalID, ok := h.gate(w, r, h.opts.EvaluateAuthz, authz.ActionPolicyEvaluate, legalEntityID)
	if !ok {
		return
	}

	now := h.opts.Now().UTC()
	asOf, knownAt, ok := resolve.Times(req.AsOf, req.KnownAt, now)
	if !ok {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid_known_at", "known_at cannot be in the future", nil)
		return
	}
	fp, err := inputFingerprint(req, legalEntityID)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid_action_context", err.Error(), nil)
		return
	}

	// Reconcile a retry to the original result (V-001 §19.1).
	existing, err := h.store.FindEvaluationByDecision(r.Context(), tenantID, req.DecisionID)
	switch {
	case err == nil:
		h.answerReplay(w, existing, fp)
		return
	case !errors.Is(err, domain.ErrEvaluationNotFound):
		h.storeUnavailable(w, r, "Evaluate", err)
		return
	}

	candidates, err := h.candidates.Candidates(r.Context(), req.PolicyType, req.TenantID, legalEntityID)
	if err != nil {
		h.storeUnavailable(w, r, "Evaluate", err)
		return
	}
	res := resolve.Resolve(candidates, asOf, knownAt)
	switch res.Status {
	case resolve.NotApplicable:
		// Nothing governs. This service does not guess fail-open or
		// fail-closed for the caller; nothing was evaluated, so nothing is
		// recorded as a decision.
		writeError(w, http.StatusNotFound, codePolicyNotEffective, "no_applicable_policy", "",
			map[string]any{"policy_type": req.PolicyType, "as_of": asOf, "known_at": knownAt})
		return
	case resolve.Conflicted:
		h.reportConflict(w, r, req, tenantID, legalEntityID, principalID, res)
		return
	}
	selected := res.Selected

	// The bytes about to decide must be the bytes that were approved
	// (V-001 §14.1 step 2: "reject hash mismatch").
	if !digestMatches(selected) {
		h.log.Error("Evaluate: stored payload does not match its artifact digest",
			zap.String("policy_version_id", selected.PolicyVersionID),
			zap.String("correlation_id", correlationID))
		writeError(w, http.StatusInternalServerError, codeEvidenceIntegrity, "artifact_digest_mismatch", "",
			map[string]any{"policy_version_id": selected.PolicyVersionID})
		return
	}

	var result string
	var selectedRules []string
	switch req.PolicyType {
	case policyTypeApprovalThreshold:
		result, selectedRules, ok = h.evaluateApprovalThreshold(w, req, selected)
		if !ok {
			return
		}
	default:
		writeError(w, http.StatusNotImplemented, codeNotImplemented, "policy_type_not_implemented", "",
			map[string]any{"policy_type": req.PolicyType})
		return
	}

	action := req.ActionContext
	if len(action) == 0 {
		action = json.RawMessage(`{}`)
	}
	var causation, requestID string
	if env, ok := envelope.FromContext(r.Context()); ok {
		causation, requestID = env.CausationID, env.RequestID
	}
	saved, created, err := h.store.SaveEvaluation(r.Context(), &domain.Evaluation{
		TenantID:               tenantID,
		DecisionID:             req.DecisionID,
		LegalEntityID:          legalEntityID,
		PolicyType:             req.PolicyType,
		RequestedByPrincipalID: principalID,
		InputFingerprint:       fp,
		ActionContext:          action,
		AsOf:                   asOf,
		KnownAt:                knownAt,
		Result:                 result,
		Outcome:                canonicalOutcome(result),
		PolicyID:               selected.PolicyID,
		PolicyVersionID:        selected.PolicyVersionID,
		PolicyCode:             selected.PolicyCode,
		ArtifactDigest:         selected.ArtifactDigest,
		RuleBasis:              fmt.Sprintf("%s:%s", selected.PolicyCode, selected.PolicyVersionID),
		SelectedRuleIDs:        selectedRules,
		EvaluatorVersion:       evaluatorThreshold,
		CorrelationID:          correlationID,
		CausationID:            causation,
		RequestID:              requestID,
	})
	if err != nil {
		// No evidence, no success (V-001 §14.1 step 7, GCP §1).
		h.storeUnavailable(w, r, "Evaluate", err)
		return
	}
	if !created {
		// A concurrent request with the same decision_id won the insert.
		h.answerReplay(w, saved, fp)
		return
	}
	writeJSON(w, http.StatusOK, responseFor(saved, false))
}

func (h *Handler) answerReplay(w http.ResponseWriter, ev *domain.Evaluation, fingerprint string) {
	if ev.InputFingerprint != fingerprint {
		writeError(w, http.StatusConflict, codeIdempotencyMismatch, "decision_id_conflict",
			"decision_id was already evaluated with different input", map[string]any{"decision_id": ev.DecisionID, "evidence_id": ev.EvaluationID})
		return
	}
	writeJSON(w, http.StatusOK, responseFor(ev, true))
}

// reportConflict answers CONFLICTED with the candidate versions and records
// the conflict as an event (GOV-05: "create stable error code and evidence";
// V-001 §20.1 applicability.conflicted).
func (h *Handler) reportConflict(w http.ResponseWriter, r *http.Request, req evaluateRequest, tenantID string, legalEntityID *string, principalID string, res resolve.Result) {
	ids := make([]string, 0, len(res.Conflicting))
	for _, c := range res.Conflicting {
		ids = append(ids, c.PolicyVersionID)
	}
	meta := eventMeta(r, principalID)
	payload, _ := json.Marshal(map[string]any{
		"policy_type":               req.PolicyType,
		"tenant_id":                 tenantID,
		"legal_entity_id":           legalEntityID,
		"decision_id":               req.DecisionID,
		"as_of":                     res.AsOf,
		"known_at":                  res.KnownAt,
		"candidate_version_ids":     ids,
		"requested_by_principal_id": principalID,
	})
	if err := h.store.EnqueueEvent(r.Context(), store.OutboxEvent{
		EventType:      "policy.applicability.conflicted",
		TenantID:       &tenantID,
		LegalEntityID:  legalEntityID,
		ActorID:        principalID,
		CorrelationID:  meta.CorrelationID,
		CausationID:    meta.CausationID,
		IdempotencyKey: "policy-conflict:" + tenantID + ":" + req.DecisionID,
		AggregateID:    req.PolicyType,
		Payload:        payload,
	}); err != nil {
		h.log.Error("Evaluate: could not record policy.applicability.conflicted", zap.Error(err))
	}
	h.log.Warn("Evaluate: conflicting policies at the same specificity",
		zap.String("policy_type", req.PolicyType), zap.Strings("policy_version_ids", ids),
		zap.String("correlation_id", meta.CorrelationID))
	writeError(w, http.StatusConflict, codePolicyConflicted, "conflicting_policies",
		"multiple distinct policies govern the same scope; none was applied",
		map[string]any{"policy_type": req.PolicyType, "candidate_version_ids": ids})
}

// inputFingerprint is a hash of the request as the caller expressed it —
// type, scope, action context and any explicit times — over a canonical JSON
// form (sorted keys, exact number text), so a retry is recognised however its
// JSON was formatted (V-001 PDC-I-17 "input fingerprint").
func inputFingerprint(req evaluateRequest, legalEntityID *string) (string, error) {
	var action any
	if len(req.ActionContext) > 0 {
		dec := json.NewDecoder(bytes.NewReader(req.ActionContext))
		dec.UseNumber()
		if err := dec.Decode(&action); err != nil {
			return "", err
		}
	}
	ts := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	canon, err := json.Marshal(map[string]any{
		"policy_type":     req.PolicyType,
		"tenant_id":       deref(req.TenantID),
		"legal_entity_id": deref(legalEntityID),
		"action_context":  action,
		"as_of":           ts(req.AsOf),
		"known_at":        ts(req.KnownAt),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// digestMatches verifies a version's payload against its server-derived
// artifact_digest (migration 000007: sha256 of the stored jsonb text, which
// is exactly the bytes read back).
func digestMatches(v *domain.ApplicablePolicyVersion) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(v.ArtifactDigest, prefix) {
		return false
	}
	sum := sha256.Sum256(v.RulePayload)
	return hex.EncodeToString(sum[:]) == v.ArtifactDigest[len(prefix):]
}

// decodeExactAmount extracts field from a raw JSON object as an exact
// decimal value — never float64 (V-001 PDC-I-11). Returns (nil, false, nil)
// if the field is absent or null.
func decodeExactAmount(raw json.RawMessage, field string) (*big.Rat, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]interface{}
	if err := dec.Decode(&obj); err != nil {
		return nil, false, err
	}
	v, present := obj[field]
	if !present || v == nil {
		return nil, false, nil
	}
	num, ok := v.(json.Number)
	if !ok {
		return nil, false, fmt.Errorf("%s is not a JSON number", field)
	}
	amount, ok := new(big.Rat).SetString(num.String())
	if !ok {
		return nil, false, fmt.Errorf("%s is not a valid decimal number: %q", field, num.String())
	}
	return amount, true, nil
}

// evaluateApprovalThreshold compares action_context.amount with the
// version's rule_payload.threshold_amount, exactly: amount > threshold is
// APPROVAL_REQUIRED, otherwise WITHIN_THRESHOLD. It writes its own error
// response and returns ok=false when it cannot decide.
func (h *Handler) evaluateApprovalThreshold(w http.ResponseWriter, req evaluateRequest, v *domain.ApplicablePolicyVersion) (string, []string, bool) {
	threshold, present, err := decodeExactAmount(v.RulePayload, "threshold_amount")
	if err != nil || !present {
		h.log.Error("evaluateApprovalThreshold: policy version has invalid/missing threshold_amount",
			zap.String("policy_version_id", v.PolicyVersionID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, codeEvidenceIntegrity, "invalid_policy_payload", "", nil)
		return "", nil, false
	}
	if len(req.ActionContext) == 0 {
		missingField(w, "action_context.amount")
		return "", nil, false
	}
	amount, present, err := decodeExactAmount(req.ActionContext, "amount")
	if err != nil || !present {
		missingField(w, "action_context.amount")
		return "", nil, false
	}
	result := "WITHIN_THRESHOLD"
	if amount.Cmp(threshold) > 0 {
		result = "APPROVAL_REQUIRED"
	}
	return result, []string{v.PolicyVersionID + "#threshold_amount"}, true
}

// canonicalOutcome maps this service's evaluation result onto the governance
// decision log's outcome vocabulary — GRANTED / DENIED / ESCALATED.
// APPROVAL_REQUIRED is ESCALATED, not DENIED: exceeding a threshold routes
// the action to an approver, it does not refuse it, and the ledger is
// append-only so an overstated outcome could never be corrected. The HTTP
// response keeps the domain result; only the ledger copy is normalised.
func canonicalOutcome(result string) string {
	switch result {
	case "WITHIN_THRESHOLD":
		return "GRANTED"
	case "APPROVAL_REQUIRED":
		return "ESCALATED"
	default:
		return result
	}
}
