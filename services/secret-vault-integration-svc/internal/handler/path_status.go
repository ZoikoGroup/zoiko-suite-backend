package handler

import (
	"errors"
	"net/http"

	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
)

type secretPathStatusResponse struct {
	SecretPath            string `json:"secret_path"`
	Applicable            bool   `json:"applicable"`
	SecretClass           string `json:"secret_class"`
	SecretPolicyVersionID string `json:"secret_policy_version_id"`
}

// SecretPathStatus handles GET /v1/secret-paths/status?secret_path=...: does an ACTIVE policy govern this path in the
// caller's scope? (S3-B3.) It exists so a service that stores a secret REFERENCE (configuration-feature-flag-svc, INV-10)
// can refuse one that names nothing, without ever receiving material, a lease or the policy's allowed workloads.
//
// 200 says a policy governs the path; 404 no_applicable_secret_policy says none does (the same deny-by-absence answer
// the broker gives). It reads policy metadata only and issues nothing, so it is authorized as a policy list.
func (h *Handler) SecretPathStatus(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()
	path := q.Get("secret_path")
	if path == "" {
		writeError(w, r, http.StatusBadRequest, "missing_field", "secret_path")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretPolicyList) {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var tenantID, legalEntityID *string
	if v := q.Get("tenant_id"); v != "" {
		tenantID = &v
	}
	if v := q.Get("legal_entity_id"); v != "" {
		legalEntityID = &v
	}
	if h.refuseForeignTenant(w, r, tenantID, tenantScope) {
		return
	}
	applicable, err := h.store.FindApplicableVersionByPath(r.Context(), path, tenantID, legalEntityID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSecretPolicyNotFound), errors.Is(err, domain.ErrSecretPolicyVersionNotFound):
			writeError(w, r, http.StatusNotFound, "no_applicable_secret_policy", path)
		default:
			h.log.Error("SecretPathStatus: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		}
		return
	}
	writeJSON(w, http.StatusOK, secretPathStatusResponse{SecretPath: path, Applicable: true,
		SecretClass: applicable.SecretClass, SecretPolicyVersionID: applicable.SecretPolicyVersionID})
}
