package handler

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
	svcenvelope "zoiko.io/secret-vault-integration-svc/internal/envelope"
)

// Secret classes the use operation will hand out. Anything else (database passwords, signing keys, ...) can
// never be returned through it by naming its path.
const (
	ProviderCredentialClass = "PROVIDER_CREDENTIAL"
	CallbackSecretClass     = "CALLBACK_SECRET"
)

type useRequest struct {
	SecretPath string `json:"secret_path,omitempty"`
	// SecretPolicyID names the policy instead of its path: a connector stores the policy id as its credential
	// reference, and the adapter should not need to know vault paths.
	SecretPolicyID         string  `json:"secret_policy_id,omitempty"`
	TenantID               *string `json:"tenant_id,omitempty"`
	LegalEntityID          *string `json:"legal_entity_id,omitempty"`
	RequestedByPrincipalID string  `json:"requested_by_principal_id"`
	RequestID              string  `json:"request_id"`
}

type useResponse struct {
	SecretPath       string `json:"secret_path"`
	SecretClass      string `json:"secret_class"`
	CredentialBase64 string `json:"credential_base64"`
	PolicyVersionRef string `json:"secret_policy_version_id"`
}

// Use handles POST /v1/secrets/use. It is the ONE documented exception to "the vault never returns secret
// material" (ZS-SVC-M-001 INT-07/08/09): a bank or tax provider that does not accept an HMAC the vault can
// compute needs a bearer token or API key on the wire, and only the calling adapter can put it there.
//
// It is deliberately narrow:
//   - only PROVIDER_CREDENTIAL and CALLBACK_SECRET classes, never any other;
//   - only a workload named in the policy version's allowed_workload_ids, whose gateway-verified identity
//     equals the claimed requester, in the caller's own tenant;
//   - every use, granted or denied, is audited (USED / DENIED), and a failure to write the audit entry
//     withholds the credential;
//   - the response is never cacheable, and neither request nor response content is logged. The caller holds
//     the value in memory for one call and does not store it.
func (h *Handler) Use(w http.ResponseWriter, r *http.Request) {
	var req useRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	for f, v := range map[string]string{"requested_by_principal_id": req.RequestedByPrincipalID, "request_id": req.RequestID} {
		if v == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": f})
			return
		}
	}
	if req.SecretPath == "" && req.SecretPolicyID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "secret_path"})
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}
	req.TenantID = &tenantScope
	correlationID := r.Header.Get("X-Correlation-ID")

	env, ok := svcenvelope.FromContext(r.Context())
	if !ok {
		env = svcenvelope.Parse(r)
	}
	deny := func(secretClass string, spv *string, detail, actedBy string) {
		if _, aerr := h.store.RecordAuditEntry(r.Context(), domain.RecordAuditEntryParams{
			EventType: "DENIED", SecretClass: secretClass, SecretPath: req.SecretPath,
			RequestedByPrincipalID: req.RequestedByPrincipalID, ActedByPrincipalID: &actedBy,
			TenantID: req.TenantID, LegalEntityID: req.LegalEntityID, SecretPolicyVersionID: spv,
			RequestID: &req.RequestID, OutcomeDetail: detail, CorrelationID: correlationID,
		}); aerr != nil {
			h.log.Error("Use: failed to record DENIED audit entry", zap.String("correlation_id", correlationID), zap.Error(aerr))
		}
	}
	if req.RequestedByPrincipalID != env.Actor() {
		deny("", nil, "requested_by_principal_id does not match the gateway-verified envelope identity", env.Actor())
		h.metrics.BrokerDecision("use_denied")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "workload_identity_mismatch", "secret_path": req.SecretPath})
		return
	}

	if req.SecretPath == "" {
		pol, perr := h.store.FindSecretPolicyByID(r.Context(), req.SecretPolicyID)
		if perr != nil {
			if errors.Is(perr, domain.ErrSecretPolicyNotFound) || isInvalidID(perr) {
				deny("", nil, "no secret policy with that id", req.RequestedByPrincipalID)
				h.metrics.BrokerDecision("use_no_policy")
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_applicable_secret_policy"})
			} else {
				h.log.Error("Use: store unavailable resolving policy id", zap.String("correlation_id", correlationID), zap.Error(perr))
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			}
			return
		}
		req.SecretPath = pol.SecretPath
	}

	applicable, err := h.store.FindApplicableVersionByPath(r.Context(), req.SecretPath, req.TenantID, req.LegalEntityID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSecretPolicyNotFound), errors.Is(err, domain.ErrSecretPolicyVersionNotFound):
			deny("", nil, "no applicable secret policy for this path/scope", req.RequestedByPrincipalID)
			h.metrics.BrokerDecision("use_no_policy")
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_applicable_secret_policy", "secret_path": req.SecretPath})
		default:
			h.log.Error("Use: store unavailable resolving policy", zap.String("correlation_id", correlationID), zap.Error(err))
			h.metrics.BrokerDecision("use_error")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	spv := applicable.SecretPolicyVersionID
	if applicable.SecretClass != ProviderCredentialClass && applicable.SecretClass != CallbackSecretClass {
		deny(applicable.SecretClass, &spv, "secret class may not be returned by the use operation", req.RequestedByPrincipalID)
		h.metrics.BrokerDecision("use_denied")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not_a_usable_secret", "secret_path": req.SecretPath})
		return
	}
	var allowed []string
	if err := json.Unmarshal(applicable.AllowedWorkloadIDs, &allowed); err != nil {
		h.log.Error("Use: policy version has invalid allowed_workload_ids", zap.String("secret_policy_version_id", spv), zap.Error(err))
		h.metrics.BrokerDecision("use_error")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "invalid_policy_payload"})
		return
	}
	if !contains(allowed, req.RequestedByPrincipalID) {
		deny(applicable.SecretClass, &spv, "requesting principal not in allowed_workload_ids", req.RequestedByPrincipalID)
		h.metrics.BrokerDecision("use_denied")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access_denied", "secret_path": req.SecretPath})
		return
	}

	material, err := h.vault.GetMaterial(r.Context(), applicable.SecretPath)
	if err != nil {
		h.log.Error("Use: vault backend unavailable", zap.String("secret_path", applicable.SecretPath), zap.Error(err))
		h.metrics.VaultBackendError("get")
		h.metrics.BrokerDecision("use_vault_error")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "vault_backend_unavailable"})
		return
	}
	encoded := base64.StdEncoding.EncodeToString(material)
	for i := range material {
		material[i] = 0
	}

	// The use is the evidence: a credential nobody can account for is not handed out.
	if _, err := h.store.RecordAuditEntry(r.Context(), domain.RecordAuditEntryParams{
		EventType: "USED", SecretClass: applicable.SecretClass, SecretPath: applicable.SecretPath,
		RequestedByPrincipalID: req.RequestedByPrincipalID, ActedByPrincipalID: &req.RequestedByPrincipalID,
		TenantID: req.TenantID, LegalEntityID: req.LegalEntityID, SecretPolicyVersionID: &spv,
		RequestID: &req.RequestID, CorrelationID: correlationID,
	}); err != nil {
		h.log.Error("Use: failed to record USED audit entry; withholding the credential", zap.String("correlation_id", correlationID), zap.Error(err))
		h.metrics.BrokerDecision("use_error")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit_unavailable"})
		return
	}
	h.metrics.BrokerDecision("use_granted")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, useResponse{
		SecretPath: applicable.SecretPath, SecretClass: applicable.SecretClass, CredentialBase64: encoded, PolicyVersionRef: spv,
	})
}

// isInvalidID reports a malformed policy id: it names nothing, so it is "not found", not an outage.
func isInvalidID(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid")
}
