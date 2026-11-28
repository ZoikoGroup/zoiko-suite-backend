package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"zoiko.io/identity-envelope/govdecision"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/signatory"
)

// Signature handoff from policy-svc (ZS-GOV-INT-002 §4.2, §8.2).

// signatoryStore is the store's signatory reads, optional so a test double
// needs them only when its test exercises them.
type signatoryStore interface {
	FindSignatoryBinding(ctx context.Context, workflowInstanceID string) (*domain.SignatoryBinding, error)
	SignaturesFor(ctx context.Context, workflowInstanceID string) ([]domain.Signer, error)
}

// WithSignatoryVerifier attaches the policy-svc decision verifier. Without it
// a workflow naming a signatory decision is refused (503), never created
// unverified.
func (h *Handler) WithSignatoryVerifier(v signatory.Verifier) *Handler {
	h.signatory = v
	return h
}

func (h *Handler) signatoryStore() (signatoryStore, bool) {
	s, ok := h.store.(signatoryStore)
	return s, ok
}

// bindSignatoryDecision verifies the policy-svc decision a workflow is created
// from and returns the binding to store with it. It writes the refusal itself
// and returns false when the workflow must not be created:
//
//	503 policy-svc or the verifier unavailable (fail closed)
//	422 decision not found, untrusted, not SIGNATURE_REQUIRED, or a QUORUM
//	    stage too small for the required distinct signers
//	409 the request's subject differs from the decision's resource or document
//	400 the decision's preparer is listed as a signer
func (h *Handler) bindSignatoryDecision(w http.ResponseWriter, r *http.Request, req *createWorkflowRequest, principalID, correlationID string) (*domain.SignatoryBinding, bool) {
	if !isValidUUID(req.SignatoryDecisionID) {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_uuid", "field": "signatory_decision_id"})
		return nil, false
	}
	if h.signatory == nil {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "signatory_verifier_unavailable",
			"message": "this instance cannot verify policy-svc decisions; a signature workflow is never created unverified"})
		return nil, false
	}
	d, err := h.signatory.Verify(r.Context(), signatory.Request{
		TenantID: req.TenantID, LegalEntityID: req.LegalEntityID, DecisionID: req.SignatoryDecisionID,
		PrincipalID: principalID, CorrelationID: correlationID, RequireValid: true,
	})
	if !h.signatoryVerifyFailed(w, r, err, correlationID) {
		return nil, false
	}
	if d.Outcome != govdecision.SignatureRequired {
		writeJSON(w, r, http.StatusUnprocessableEntity, map[string]any{
			"error": "signatory_decision_not_actionable", "outcome": d.Outcome, "reason_codes": d.ReasonCodes,
			"message": "only a SIGNATURE_REQUIRED decision creates a signature workflow; any other outcome holds or blocks the action",
		})
		return nil, false
	}
	q, ok := signatory.Quorum(d)
	if !ok || q.MinDistinctSigners < 1 || !svcFingerprint(q.DocumentHash) {
		writeJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": "signatory_decision_untrusted",
			"message": "the decision carries no usable SIGNATURE_QUORUM (distinct signers and document hash)"})
		return nil, false
	}

	// The preparer of the instrument never signs it, and the requirement's
	// distinct signers must all be collectable in one QUORUM stage.
	quorumStages := 0
	for i, st := range req.Stages {
		for _, a := range stageApprovers(st) {
			if a == d.ActorPrincipalID {
				writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "preparer_cannot_sign",
					"field": "stages[" + strconv.Itoa(i) + "]", "message": "the decision's preparer may not be a signer"})
				return nil, false
			}
		}
		if st.StageType == domain.StageTypeQuorum {
			quorumStages++
			if st.RequiredApprovals < q.MinDistinctSigners {
				writeJSON(w, r, http.StatusUnprocessableEntity, map[string]any{"error": "signature_quorum_insufficient",
					"min_distinct_signers": q.MinDistinctSigners, "required_approvals": st.RequiredApprovals})
				return nil, false
			}
		}
	}
	if quorumStages != 1 {
		writeJSON(w, r, http.StatusUnprocessableEntity, map[string]any{"error": "signature_quorum_stage_required",
			"message": "a signature workflow has exactly one QUORUM stage, whose votes are the signatures", "quorum_stages": quorumStages})
		return nil, false
	}

	// The workflow's subject is the decision's resource and document: the
	// release rule (verify-release) then refuses any later change to either.
	version, err := strconv.Atoi(d.Resource.Version)
	if err != nil || version < 0 {
		writeJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": "signatory_resource_version_not_integer",
			"message": "a signature workflow binds an integer resource version"})
		return nil, false
	}
	mismatch := func(field string) bool {
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "signatory_subject_mismatch", "field": field,
			"message": "the workflow subject must be the decision's resource and document"})
		return false
	}
	if req.SubjectType != nil && *req.SubjectType != "" && *req.SubjectType != d.Resource.Type {
		return nil, mismatch("subject_type")
	}
	if req.SubjectID != nil && *req.SubjectID != "" && *req.SubjectID != d.Resource.ID {
		return nil, mismatch("subject_id")
	}
	if req.SubjectVersion != nil && *req.SubjectVersion != version {
		return nil, mismatch("subject_version")
	}
	if req.SubjectFingerprint != nil && *req.SubjectFingerprint != "" && *req.SubjectFingerprint != q.DocumentHash {
		return nil, mismatch("subject_fingerprint")
	}
	req.SubjectType, req.SubjectID, req.SubjectVersion, req.SubjectFingerprint = &d.Resource.Type, &d.Resource.ID, &version, &q.DocumentHash

	requirement, err := json.Marshal(q)
	if err != nil {
		writeJSON(w, r, http.StatusInternalServerError, map[string]string{"error": "signatory_requirement_unencodable"})
		return nil, false
	}
	return &domain.SignatoryBinding{
		TenantID: req.TenantID, DecisionID: d.DecisionID, PolicyVersionID: d.PolicyVersionID,
		RuleID: d.RuleID, DocumentHash: q.DocumentHash, Requirement: requirement,
	}, true
}

// signatoryVerifyFailed writes the refusal for a verifier error and reports
// whether there was none.
func (h *Handler) signatoryVerifyFailed(w http.ResponseWriter, r *http.Request, err error, correlationID string) bool {
	var untrusted *signatory.UntrustedError
	switch {
	case err == nil:
		return true
	case errors.Is(err, signatory.ErrNotFound):
		writeJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": "signatory_decision_not_found"})
	case errors.As(err, &untrusted):
		writeJSON(w, r, http.StatusUnprocessableEntity, map[string]any{"error": "signatory_decision_untrusted",
			"outcome": govdecision.Error, "problems": untrusted.Problems})
	default:
		h.log.Error("signatory decision unavailable — failing closed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "policy_service_unavailable"})
	}
	return false
}

// signerOffices asks authorization-svc, at the moment of signing, which of the
// requirement's accepted offices the signer holds in the workflow's legal
// entity (Doc 02 §4.2: "Authorization validates each person's office ...
// at the moment of signature"). A denied office is simply not held; an
// unavailable authorization is an error (fail closed).
func (h *Handler) signerOffices(ctx context.Context, principalID, legalEntityID, initiator string, req *domain.SignatureRequirement) ([]string, error) {
	var held []string
	for _, office := range req.AcceptedOffices() {
		err := h.authz.CheckAllowed(ctx, principalID, legalEntityID, domain.SignatoryActionPrefix+office, initiator)
		switch {
		case err == nil:
			held = append(held, office)
		case errors.Is(err, domain.ErrAuthorizationDenied):
		default:
			return nil, err
		}
	}
	return held, nil
}

// signatureReleaseCheck is the execution-time recheck of a signature-bound
// workflow (Doc 02 §8.4 signatory_quorum_and_mandate_are_valid): a fresh
// policy-svc decision for the same resource and document must still be met
// by the signatures given. It returns ("", true) when release may proceed,
// a reason when it may not, or false after writing a refusal.
func (h *Handler) signatureReleaseCheck(w http.ResponseWriter, r *http.Request, instance *domain.WorkflowInstance, binding *domain.SignatoryBinding,
	currentDecisionID, principalID, correlationID string) (string, bool) {
	if currentDecisionID == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "current_signatory_decision_id",
			"message": "a signature-bound workflow is released only against a fresh policy-svc decision for the same document"})
		return "", false
	}
	if !isValidUUID(currentDecisionID) {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_uuid", "field": "current_signatory_decision_id"})
		return "", false
	}
	if h.signatory == nil {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "signatory_verifier_unavailable"})
		return "", false
	}
	res := govdecision.Resource{}
	if instance.SubjectType != nil && instance.SubjectID != nil && instance.SubjectVersion != nil {
		res = govdecision.Resource{Type: *instance.SubjectType, ID: *instance.SubjectID, Version: strconv.Itoa(*instance.SubjectVersion)}
	}
	d, err := h.signatory.Verify(r.Context(), signatory.Request{
		TenantID: instance.TenantID, LegalEntityID: instance.LegalEntityID, DecisionID: currentDecisionID,
		PrincipalID: principalID, CorrelationID: correlationID, Resource: res, RequireValid: true,
	})
	if err != nil {
		var untrusted *signatory.UntrustedError
		if errors.As(err, &untrusted) {
			return "the current signatory decision cannot be trusted: " + strings.Join(untrusted.Problems, "; "), true
		}
		if errors.Is(err, signatory.ErrNotFound) {
			return "the current signatory decision does not exist", true
		}
		h.log.Error("VerifyRelease: policy-svc unavailable — failing closed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "policy_service_unavailable"})
		return "", false
	}
	switch d.Outcome {
	case govdecision.Permit:
		return "", true // now explicitly exempt: no signature needed
	case govdecision.SignatureRequired:
	default:
		return "the current signatory decision is " + d.Outcome + ": release is held or blocked", true
	}
	q, ok := signatory.Quorum(d)
	if !ok {
		return "the current signatory decision carries no signature quorum", true
	}
	if q.DocumentHash != binding.DocumentHash {
		return govdecision.ReasonSignatureDocumentChanged + ": the document changed after it was signed", true
	}
	ss, ok := h.signatoryStore()
	if !ok {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return "", false
	}
	signers, err := ss.SignaturesFor(r.Context(), instance.WorkflowInstanceID)
	if err != nil {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return "", false
	}
	raw, _ := json.Marshal(q)
	current, err := domain.ParseSignatureRequirement(raw)
	if err != nil {
		return "the current signature requirement is unreadable", true
	}
	if len(signers) < current.MinDistinctSigners || !current.OfficesFilled(signers) {
		return "the signature requirement in force now (" + d.RuleID + ") is not met by the signatures given", true
	}
	return "", true
}

func svcFingerprint(s string) bool {
	if !strings.HasPrefix(s, "sha256:") || len(s) != 71 {
		return false
	}
	for _, c := range s[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
