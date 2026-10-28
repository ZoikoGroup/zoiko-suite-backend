package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
	svcenvelope "zoiko.io/secret-vault-integration-svc/internal/envelope"
)

// SigningSecretClass is the only secret class the sign operation will use. A
// credential of any other class (database password, API key, ...) cannot be
// turned into a signing oracle by naming its path.
const SigningSecretClass = "WEBHOOK_SIGNING"

// maxSignedMessageBytes bounds what is signed per call (the request body limit is
// 256 KiB and the message travels base64-encoded inside it).
const maxSignedMessageBytes = 180 << 10

type signRequest struct {
	SecretPath string `json:"secret_path,omitempty"`
	// SecretPolicyID names the policy instead of its path (a connector stores the policy id as its credential reference).
	SecretPolicyID         string  `json:"secret_policy_id,omitempty"`
	TenantID               *string `json:"tenant_id,omitempty"`
	LegalEntityID          *string `json:"legal_entity_id,omitempty"`
	RequestedByPrincipalID string  `json:"requested_by_principal_id"`
	RequestID              string  `json:"request_id"`
	MessageBase64          string  `json:"message_base64"`
	Algorithm              string  `json:"algorithm,omitempty"`
}

type signResponse struct {
	SecretPath       string `json:"secret_path"`
	Algorithm        string `json:"algorithm"`
	SignatureHex     string `json:"signature_hex"`
	PolicyVersionRef string `json:"secret_policy_version_id"`
}

func (r signRequest) missingField() string {
	switch {
	case r.SecretPath == "" && r.SecretPolicyID == "":
		return "secret_path"
	case r.RequestedByPrincipalID == "":
		return "requested_by_principal_id"
	case r.RequestID == "":
		return "request_id"
	case r.MessageBase64 == "":
		return "message_base64"
	}
	return ""
}

// Sign handles POST /v1/secrets/sign: it computes HMAC-SHA256 over the supplied
// message with the key material stored at secret_path and returns only the
// signature. The key never leaves this service, which is what lets a caller such as
// notification-svc sign outbound webhooks (ZS-SVC-M-001 INT-04) while the signing
// key stays in SEC-01 and nobody else ever holds it.
//
// It applies the same gates as Broker: a gateway-verified identity that matches the
// claimed requester, tenant scoping, an applicable ACTIVE policy version, and an
// allowed-workload check. In addition the policy's secret class must be
// WEBHOOK_SIGNING, and every use (granted or denied) is written to the access audit
// log. Nothing about the key or the message is logged.
func (h *Handler) Sign(w http.ResponseWriter, r *http.Request) {
	var req signRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeError(w, r, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if req.Algorithm != "" && req.Algorithm != "HMAC-SHA256" {
		writeError(w, r, http.StatusBadRequest, "unsupported_algorithm", "")
		return
	}
	message, err := base64.StdEncoding.DecodeString(req.MessageBase64)
	if err != nil || len(message) == 0 || len(message) > maxSignedMessageBytes {
		writeError(w, r, http.StatusBadRequest, "invalid_field", "message_base64")
		return
	}

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if h.refuseForeignTenant(w, r, req.TenantID, tenantScope) {
		return
	}
	// A signing key belongs to a tenant: pin the lookup to the verified tenant (that
	// tenant's policy or a global one, never another tenant's) even when the body omitted it.
	req.TenantID = &tenantScope

	correlationID := r.Header.Get("X-Correlation-ID")

	env, ok := svcenvelope.FromContext(r.Context())
	if !ok {
		env = svcenvelope.Parse(r)
	}
	deny := func(secretClass string, spv *string, detail string, actedBy string) {
		if _, aerr := h.store.RecordAuditEntry(r.Context(), domain.RecordAuditEntryParams{
			EventType:              "DENIED",
			SecretClass:            secretClass,
			SecretPath:             req.SecretPath,
			RequestedByPrincipalID: req.RequestedByPrincipalID,
			ActedByPrincipalID:     &actedBy,
			TenantID:               req.TenantID,
			LegalEntityID:          req.LegalEntityID,
			SecretPolicyVersionID:  spv,
			RequestID:              &req.RequestID,
			OutcomeDetail:          detail,
			CorrelationID:          correlationID,
		}); aerr != nil {
			h.log.Error("Sign: failed to record DENIED audit entry", zap.String("correlation_id", correlationID), zap.Error(aerr))
		}
	}
	if req.RequestedByPrincipalID != env.Actor() {
		deny("", nil, "requested_by_principal_id does not match the gateway-verified envelope identity", env.Actor())
		h.metrics.BrokerDecision("sign_denied")
		writeError(w, r, http.StatusForbidden, "workload_identity_mismatch", req.SecretPath)
		return
	}

	if req.SecretPath == "" {
		pol, perr := h.store.FindSecretPolicyByID(r.Context(), req.SecretPolicyID)
		if perr != nil {
			if errors.Is(perr, domain.ErrSecretPolicyNotFound) || isInvalidID(perr) {
				deny("", nil, "no secret policy with that id", req.RequestedByPrincipalID)
				h.metrics.BrokerDecision("sign_no_policy")
				writeError(w, r, http.StatusNotFound, "no_applicable_secret_policy", "")
			} else {
				h.log.Error("Sign: store unavailable resolving policy id", zap.String("correlation_id", correlationID), zap.Error(perr))
				writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
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
			h.metrics.BrokerDecision("sign_no_policy")
			writeError(w, r, http.StatusNotFound, "no_applicable_secret_policy", req.SecretPath)
		default:
			h.log.Error("Sign: store unavailable resolving policy", zap.String("correlation_id", correlationID), zap.Error(err))
			h.metrics.BrokerDecision("sign_error")
			writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		}
		return
	}
	spv := applicable.SecretPolicyVersionID
	if applicable.SecretClass != SigningSecretClass {
		deny(applicable.SecretClass, &spv, "secret class is not a signing class", req.RequestedByPrincipalID)
		h.metrics.BrokerDecision("sign_denied")
		writeError(w, r, http.StatusForbidden, "not_a_signing_secret", req.SecretPath)
		return
	}
	var allowed []string
	if err := json.Unmarshal(applicable.AllowedWorkloadIDs, &allowed); err != nil {
		h.log.Error("Sign: policy version has invalid allowed_workload_ids", zap.String("secret_policy_version_id", spv), zap.Error(err))
		h.metrics.BrokerDecision("sign_error")
		writeError(w, r, http.StatusInternalServerError, "invalid_policy_payload", "")
		return
	}
	if !contains(allowed, req.RequestedByPrincipalID) {
		deny(applicable.SecretClass, &spv, "requesting principal not in allowed_workload_ids", req.RequestedByPrincipalID)
		h.metrics.BrokerDecision("sign_denied")
		writeError(w, r, http.StatusForbidden, "access_denied", req.SecretPath)
		return
	}

	material, err := h.vault.GetMaterial(r.Context(), applicable.SecretPath)
	if err != nil {
		h.log.Error("Sign: vault backend unavailable", zap.String("secret_path", applicable.SecretPath), zap.Error(err))
		h.metrics.VaultBackendError("get")
		h.metrics.BrokerDecision("sign_vault_error")
		writeError(w, r, http.StatusServiceUnavailable, "vault_backend_unavailable", "")
		return
	}
	mac := hmac.New(sha256.New, material)
	for i := range material {
		material[i] = 0 // the key is not kept in memory longer than the computation
	}
	mac.Write(message)
	sig := hex.EncodeToString(mac.Sum(nil))

	// The use is the evidence. A failure to write it must not hand out a signature
	// no one can account for.
	if _, err := h.store.RecordAuditEntry(r.Context(), domain.RecordAuditEntryParams{
		EventType:              "SIGNED",
		SecretClass:            applicable.SecretClass,
		SecretPath:             applicable.SecretPath,
		RequestedByPrincipalID: req.RequestedByPrincipalID,
		ActedByPrincipalID:     &req.RequestedByPrincipalID,
		TenantID:               req.TenantID,
		LegalEntityID:          req.LegalEntityID,
		SecretPolicyVersionID:  &spv,
		RequestID:              &req.RequestID,
		CorrelationID:          correlationID,
	}); err != nil {
		h.log.Error("Sign: failed to record SIGNED audit entry; refusing the signature", zap.String("correlation_id", correlationID), zap.Error(err))
		h.metrics.BrokerDecision("sign_error")
		writeError(w, r, http.StatusServiceUnavailable, "audit_unavailable", "")
		return
	}
	h.metrics.BrokerDecision("sign_granted")
	writeJSON(w, http.StatusOK, signResponse{
		SecretPath: applicable.SecretPath, Algorithm: "HMAC-SHA256", SignatureHex: sig, PolicyVersionRef: spv,
	})
}

