package context

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// subjectIdentityResponse is GET /v1/subjects/{id}: the principal plus every
// external-IdP link bound to it.
type subjectIdentityResponse struct {
	Principal domain.Principal   `json:"principal"`
	Links     []domain.IdentityLink `json:"links"`
}

// identityLinksResponse is GET /v1/subjects/{id}/links.
type identityLinksResponse struct {
	Links []domain.IdentityLink `json:"links"`
}

// GetSubjectIdentity reads a subject's identity record and its IdP links.
// Authorized at IDENTITY_PRINCIPAL_READ (a principal may read their own).
func (h *Handler) GetSubjectIdentity(w http.ResponseWriter, r *http.Request) {
	principalID := chi.URLParam(r, "principalID")
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.throttle(w, r, "get_subject_identity", tenantID); !ok {
		return
	}
	if !h.authorizeUnlessSelf(w, r, callerPrincipalID, principalID, tenantID, IdentityPrincipalRead) {
		return
	}

	p, err := h.principals.FindByID(r.Context(), principalID, tenantID)
	if err != nil {
		h.log.Error("get subject identity failed",
			zap.String("tenant_id", tenantID),
			zap.String("principal_id", principalID),
			zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "subject lookup failed")
		return
	}
	if p == nil {
		writeCoded(w, r, http.StatusNotFound, "not_found", "subject not found")
		return
	}

	links, err := h.principals.GetIdentityLinks(r.Context(), tenantID, principalID)
	if err != nil {
		h.log.Error("get identity links failed",
			zap.String("tenant_id", tenantID),
			zap.String("principal_id", principalID),
			zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "identity links lookup failed")
		return
	}
	if links == nil {
		links = []domain.IdentityLink{}
	}
	writeJSON(w, http.StatusOK, subjectIdentityResponse{Principal: *p, Links: links})
}

// ListIdentityLinks returns every external-IdP link bound to a subject.
// Authorized at IDENTITY_PRINCIPAL_READ (a principal may list their own).
func (h *Handler) ListIdentityLinks(w http.ResponseWriter, r *http.Request) {
	principalID := chi.URLParam(r, "principalID")
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.throttle(w, r, "list_identity_links", tenantID); !ok {
		return
	}
	if !h.authorizeUnlessSelf(w, r, callerPrincipalID, principalID, tenantID, IdentityPrincipalRead) {
		return
	}

	links, err := h.principals.GetIdentityLinks(r.Context(), tenantID, principalID)
	if err != nil {
		h.log.Error("list identity links failed",
			zap.String("tenant_id", tenantID),
			zap.String("principal_id", principalID),
			zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "identity links lookup failed")
		return
	}
	if links == nil {
		links = []domain.IdentityLink{}
	}
	writeJSON(w, http.StatusOK, identityLinksResponse{Links: links})
}

// MergeDuplicateIdentity retires `retired_id` into the survivor `{survivorID}`.
// Authorized at IDENTITY_SUBJECT_MERGE and a recent step-up. The caller must
// be neither the survivor nor the retired principal (independent approval).
func (h *Handler) MergeDuplicateIdentity(w http.ResponseWriter, r *http.Request) {
	survivorID := chi.URLParam(r, "survivorID")
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.throttle(w, r, "merge_subjects", tenantID); !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, IdentitySubjectMerge) {
		return
	}
	if h.mfa == nil {
		writeError(w, r, http.StatusNotImplemented, "step-up not configured in this deployment")
		return
	}
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return
	}
	if err := h.mfa.requireRecentMFA(c); err != nil {
		h.writeMFAError(w, r, err)
		return
	}

	var req domain.MergeDuplicateIdentityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RetiredID == "" {
		writeCoded(w, r, http.StatusBadRequest, "invalid_request", "retired_id is required")
		return
	}
	if req.StepUpEvidence == "" {
		writeCoded(w, r, http.StatusBadRequest, "invalid_request", "step_up_evidence is required")
		return
	}
	if callerPrincipalID == survivorID || callerPrincipalID == req.RetiredID {
		writeCoded(w, r, http.StatusForbidden, "authorization_denied",
			"the merge actor must be neither the survivor nor the retired principal")
		return
	}
	if survivorID == req.RetiredID {
		writeCoded(w, r, http.StatusBadRequest, "invalid_request", "survivor and retired must differ")
		return
	}

	// If-Match guards mutation of the retired subject's aggregate.
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		writeCoded(w, r, http.StatusPreconditionRequired, "if_match_required",
			"If-Match header is required")
		return
	}
	existing, err := h.principals.FindByID(r.Context(), req.RetiredID, tenantID)
	if err != nil {
		h.log.Error("merge: lookup retired principal failed",
			zap.String("tenant_id", tenantID),
			zap.String("retired_id", req.RetiredID),
			zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "merge lookup failed")
		return
	}
	if existing == nil {
		writeCoded(w, r, http.StatusNotFound, "not_found", "retired subject not found")
		return
	}
	if existing.RecordVersion != parseInt64(ifMatch) {
		writeCoded(w, r, http.StatusPreconditionFailed, "if_match_stale",
			"If-Match does not match the current record version")
		return
	}

	err = h.principals.MergeDuplicateIdentity(r.Context(), tenantID, survivorID, req.RetiredID,
		callerPrincipalID, req.StepUpEvidence, mfaCorrelation(r, r.Header.Get("X-Correlation-ID")))
	if err != nil {
		h.log.Error("merge failed",
			zap.String("tenant_id", tenantID),
			zap.String("survivor_id", survivorID),
			zap.String("retired_id", req.RetiredID),
			zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "merge failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"survivor_id": survivorID,
		"retired_id":  req.RetiredID,
		"status":      "merged",
	})
}

// parseInt64 parses an If-Match style etag; returns 0 when it is not numeric
// so the stale check treats an opaque etag as "not this version".
func parseInt64(s string) int64 {
	var n int64
	for _, b := range s {
		if b < '0' || b > '9' {
			return 0
		}
		n = n*10 + int64(b-'0')
	}
	return n
}
