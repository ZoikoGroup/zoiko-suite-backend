package context

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/lifecycle"
	"zoiko.io/identity-envelope/apicontract"
)

// subjectIdentityResponse is GET /v1/subjects/{id} (ORG-1 GetSubjectIdentity):
// the subject that answers for the id, plus every external identity bound to
// it. MergedInto is set when the requested id was retired by a merge.
type subjectIdentityResponse struct {
	RequestedID string                `json:"requested_id"`
	MergedInto  string                `json:"merged_into,omitempty"`
	Principal   domain.Principal      `json:"principal"`
	Links       []domain.IdentityLink `json:"links"`
}

// identityLinksResponse is GET /v1/subjects/{id}/links.
type identityLinksResponse struct {
	PrincipalID string                `json:"principal_id"`
	Links       []domain.IdentityLink `json:"links"`
}

// GetSubjectIdentity reads a subject's identity record and its links. A
// retired id resolves to its survivor, never to the disabled duplicate.
// Authorized at IDENTITY_PRINCIPAL_READ (a principal may read their own).
func (h *Handler) GetSubjectIdentity(w http.ResponseWriter, r *http.Request) {
	requestedID := chi.URLParam(r, "principalID")
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
	if !h.authorizeUnlessSelf(w, r, callerPrincipalID, requestedID, tenantID, IdentityPrincipalRead) {
		return
	}
	survivorID, merged, err := h.principals.ResolveSubjectID(r.Context(), tenantID, requestedID)
	if err != nil {
		h.log.Error("resolve subject alias failed", zap.String("tenant_id", tenantID), zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "subject lookup failed")
		return
	}
	p, err := h.principals.FindByID(r.Context(), survivorID, tenantID)
	if err != nil {
		h.log.Error("get subject identity failed", zap.String("tenant_id", tenantID), zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "subject lookup failed")
		return
	}
	if p == nil {
		writeCoded(w, r, http.StatusNotFound, "not_found", "subject not found")
		return
	}
	links, err := h.principals.GetIdentityLinks(r.Context(), tenantID, survivorID)
	if err != nil {
		h.log.Error("get identity links failed", zap.String("tenant_id", tenantID), zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "identity links lookup failed")
		return
	}
	resp := subjectIdentityResponse{RequestedID: requestedID, Principal: *p, Links: links}
	if merged {
		resp.MergedInto = survivorID
	}
	apicontract.WriteETag(w, p.RecordVersion)
	writeJSON(w, http.StatusOK, resp)
}

// ListIdentityLinks returns every external identity bound to a subject
// (ORG-1 ListIdentityLinks). A retired id lists its survivor's links.
func (h *Handler) ListIdentityLinks(w http.ResponseWriter, r *http.Request) {
	requestedID := chi.URLParam(r, "principalID")
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
	if !h.authorizeUnlessSelf(w, r, callerPrincipalID, requestedID, tenantID, IdentityPrincipalRead) {
		return
	}
	survivorID, _, err := h.principals.ResolveSubjectID(r.Context(), tenantID, requestedID)
	if err != nil {
		h.log.Error("resolve subject alias failed", zap.String("tenant_id", tenantID), zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "identity links lookup failed")
		return
	}
	links, err := h.principals.GetIdentityLinks(r.Context(), tenantID, survivorID)
	if err != nil {
		h.log.Error("list identity links failed", zap.String("tenant_id", tenantID), zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "identity links lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, identityLinksResponse{PrincipalID: survivorID, Links: links})
}

// MergeDuplicateIdentity retires duplicate_id into {survivorID} (ORG-1
// MergeDuplicateIdentity). Authorized at IDENTITY_SUBJECT_MERGE; needs a
// recent second factor from the caller's verified envelope, a reason, and
// If-Match with the duplicate's ETag. The caller may be neither subject.
func (h *Handler) MergeDuplicateIdentity(w http.ResponseWriter, r *http.Request) {
	if h.lifecycle == nil {
		writeCoded(w, r, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"subject merge is not configured in this deployment")
		return
	}
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
	version, any, present, err := apicontract.IfMatchVersion(r)
	if !present {
		writeCoded(w, r, http.StatusPreconditionRequired, "precondition_required",
			"If-Match with the duplicate subject's ETag is required")
		return
	}
	if err != nil || any {
		writeCoded(w, r, http.StatusBadRequest, "invalid_if_match",
			"If-Match must be the duplicate subject's ETag from GET /v1/subjects/{id}")
		return
	}
	var req domain.MergeDuplicateIdentityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	actor, ok := h.steppedUpActor(w, r, tenantID, callerPrincipalID)
	if !ok {
		return
	}
	res, err := h.lifecycle.Merge(r.Context(), actor, survivorID, req.DuplicateID, req.Reason, version)
	if err != nil {
		h.lifecycleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// steppedUpActor builds the lifecycle actor from the caller's verified
// envelope, so the second-factor evidence a command checks is the session's
// own (mfa_verified and auth_time), never a value the caller supplies.
func (h *Handler) steppedUpActor(w http.ResponseWriter, r *http.Request, tenantID, callerPrincipalID string) (lifecycle.Actor, bool) {
	c, ok := h.mfaCaller(w, r)
	if !ok {
		return lifecycle.Actor{}, false
	}
	if c.Principal.PrincipalID != callerPrincipalID || c.Principal.TenantID != tenantID {
		writeCoded(w, r, http.StatusForbidden, errCodeIdentityMismatch, "the session does not match the caller")
		return lifecycle.Actor{}, false
	}
	return lifecycle.Actor{
		PrincipalID:   callerPrincipalID,
		TenantID:      tenantID,
		MFAVerified:   c.MFAVerified,
		AuthTime:      c.AuthTime,
		CorrelationID: mfaCorrelation(r, r.Header.Get("X-Correlation-ID")),
	}, true
}
