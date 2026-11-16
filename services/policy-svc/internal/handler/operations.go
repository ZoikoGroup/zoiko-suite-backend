package handler

// Operator commands outside the version lifecycle:
//
//	POST /v1/policies/{policy_id}/retire                     retire a policy family
//	POST /v1/policy-evaluations/{evidence_id}/redeliver      re-queue refused evidence

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
)

// operationsStore is the store's operator commands, optional like the
// accounting and register reads.
type operationsStore interface {
	RetirePolicy(ctx context.Context, params domain.RetirePolicyParams) (*domain.Policy, bool, error)
	RedeliverEvidence(ctx context.Context, params domain.EvidenceRedeliveryParams) (bool, error)
}

// RegisterOperationRoutes mounts the operator commands.
func RegisterOperationRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/policies/{policy_id}/retire", h.RetirePolicy)
	r.Post("/v1/policy-evaluations/{evidence_id}/redeliver", h.RedeliverEvidence)
}

func (h *Handler) operations(w http.ResponseWriter, r *http.Request) (operationsStore, bool) {
	s, ok := h.store.(operationsStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "store_unavailable", "the store does not serve operator commands", nil)
	}
	return s, ok
}

type reasonRequest struct {
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	ReasonCode      string `json:"reason_code"`
	Reason          string `json:"reason,omitempty"`
}

func (req reasonRequest) text() string {
	reason := strings.TrimSpace(req.ReasonCode)
	if free := strings.TrimSpace(req.Reason); free != "" {
		reason += ": " + free
	}
	return reason
}

// RetirePolicy handles POST /v1/policies/{policy_id}/retire. A retired family
// takes no new versions; its versions and their history stay readable. It is
// refused while a version governs (ACTIVE or SCHEDULED): retire or supersede
// that version first. Needs the family's record_version (If-Match or
// expected_version) and a reason_code; authorized as a retirement, globally
// for a platform family.
//
//	200 retired, or already retired · 400 malformed · 401 · 403
//	404 not found · 409 POLICY_IN_USE · 412 stale · 428 no version
func (h *Handler) RetirePolicy(w http.ResponseWriter, r *http.Request) {
	s, ok := h.operations(w, r)
	if !ok {
		return
	}
	policyID := chi.URLParam(r, "policy_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !requireUUID(w, r, "policy_id", policyID) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req reasonRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	expected, ok := commandVersion(w, r, req.ExpectedVersion)
	if !ok {
		return
	}
	if strings.TrimSpace(req.ReasonCode) == "" {
		missingField(w, r, "reason_code")
		return
	}
	if tooLong(w, r, "reason", req.text(), 2000) {
		return
	}
	family, err := h.store.FindPolicyByID(r.Context(), policyID)
	if err != nil {
		if errors.Is(err, domain.ErrPolicyNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "policy_not_found", "", map[string]any{"policy_id": policyID})
			return
		}
		h.storeUnavailable(w, r, "RetirePolicy", err)
		return
	}
	action := authz.ActionPolicyVersionRetire
	if family.TenantID == nil {
		action = authz.ActionPolicyVersionRetireGlobal
	}
	if !h.authorize(w, r, principalID, action, nil) {
		return
	}
	if family.PolicyStatus != domain.PolicyStatusRetired && refuseStale(w, r, expected, int(family.RecordVersion)) {
		return
	}
	p, _, err := s.RetirePolicy(r.Context(), domain.RetirePolicyParams{
		PolicyID: policyID, ExpectedVersion: int64(expected), ActorID: principalID, Reason: req.text(), Meta: eventMeta(r, principalID),
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPolicyNotFound):
			writeError(w, r, http.StatusNotFound, codeNotFound, "policy_not_found", "", map[string]any{"policy_id": policyID})
		case errors.Is(err, domain.ErrPolicyInUse):
			writeError(w, r, http.StatusConflict, codePolicyInUse, "policy_in_use",
				"a version of this family is ACTIVE or SCHEDULED: retire or supersede it first", map[string]any{"policy_id": policyID})
		case errors.Is(err, domain.ErrConcurrencyConflict):
			writeError(w, r, http.StatusConflict, codeConcurrency, "concurrency_conflict", "", map[string]any{"policy_id": policyID})
		default:
			h.storeUnavailable(w, r, "RetirePolicy", err)
		}
		return
	}
	apicontract.WriteETag(w, p.RecordVersion)
	writeJSON(w, http.StatusOK, p)
}

// RedeliverEvidence handles POST /v1/policy-evaluations/{evidence_id}/redeliver:
// evidence the governance ledger refused for good (ledger_status FAILED) goes
// back on the relay's queue after an operator has fixed the cause. Pending
// evidence is left as it is; recorded evidence is 409. Needs a reason_code.
//
//	200 the evaluation, ledger_status PENDING · 400 · 401 · 403 · 404
//	409 EVIDENCE_ALREADY_RECORDED
func (h *Handler) RedeliverEvidence(w http.ResponseWriter, r *http.Request) {
	s, ok := h.operations(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "evidence_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !requireUUID(w, r, "evidence_id", id) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req reasonRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ReasonCode) == "" {
		missingField(w, r, "reason_code")
		return
	}
	if tooLong(w, r, "reason", req.text(), 2000) {
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionPolicyEvidenceRedeliver, nil) {
		return
	}
	_, err := s.RedeliverEvidence(r.Context(), domain.EvidenceRedeliveryParams{
		TenantID: tenantID, EvaluationID: id, ActorID: principalID, Reason: req.text(), Meta: eventMeta(r, principalID),
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrEvaluationNotFound):
			writeError(w, r, http.StatusNotFound, codeNotFound, "evaluation_not_found", "", map[string]any{"evidence_id": id})
		case errors.Is(err, domain.ErrEvidenceAlreadyRecorded):
			writeError(w, r, http.StatusConflict, codeEvidenceRecorded, "evidence_already_recorded",
				"the governance ledger already holds this evidence", map[string]any{"evidence_id": id})
		default:
			h.storeUnavailable(w, r, "RedeliverEvidence", err)
		}
		return
	}
	ev, err := h.store.FindEvaluationByID(r.Context(), tenantID, id)
	if err != nil {
		h.storeUnavailable(w, r, "RedeliverEvidence", err)
		return
	}
	writeJSON(w, http.StatusOK, responseFor(ev, false))
}
