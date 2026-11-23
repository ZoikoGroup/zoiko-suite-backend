package handler

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/key-management-svc/internal/authz"
	"zoiko.io/key-management-svc/internal/domain"
	"zoiko.io/key-management-svc/internal/siem"
	"zoiko.io/key-management-svc/internal/store"
)

// The token vault: POST /v1/tokenize and POST /v1/detokenize.
//
// Implements Security Standard §14 ("business services use token whenever raw
// value is unnecessary") for the contract that banking-connector-svc and
// payee-banking-identity-svc built their clients against
// (internal/tokenization/client.go in each):
//
//	POST /v1/tokenize    {"value", "classification"} -> 200 {"token"}
//	POST /v1/detokenize  {"token"}                   -> 200 {"value"}
//	                                                    403 denied (SEC-T15 / SEC-T29)
//
// Headers: X-Tenant-Id and X-Principal-Id required on both, X-Correlation-ID
// recorded. The token is the vault row's token_id, a random UUID: it carries
// nothing about the value, so holding one discloses nothing without
// TOKEN_DETOKENIZE.
//
// The raw value is never logged, never sent to SIEM and never stored
// unencrypted (S3 Restricted: "no raw telemetry").

// maxVaultBody bounds a tokenize/detokenize request body. Values are capped
// at domain.MaxTokenizableValueBytes; this leaves room for JSON escaping.
const maxVaultBody = 16 << 10

type tokenizeRequest struct {
	Value          string                `json:"value"`
	Classification domain.Classification `json:"classification"`
}

type tokenizeResponse struct {
	Token string `json:"token"`
}

type detokenizeRequest struct {
	Token string `json:"token"`
}

type detokenizeResponse struct {
	Value string `json:"value"`
}

// vaultAAD binds a ciphertext to its tenant and token, so it decrypts only on
// the row it was written for (internal/crypto).
func vaultAAD(tenantID, tokenID string) []byte {
	return []byte("key-management-svc/tokenized_values/v1|" + tenantID + "|" + tokenID)
}

// vaultReady refuses with 503 when the vault was never wired. cmd/server
// always wires it; this guards a Handler built without WithVault.
func (h *Handler) vaultReady(w http.ResponseWriter, r *http.Request) bool {
	if h.vault == nil || h.cipher == nil {
		h.errJSON(w, r, http.StatusServiceUnavailable, "token vault not configured")
		return false
	}
	return true
}

// Tokenize encrypts a value into the caller's tenant and returns its token.
//
// Idempotent on Idempotency-Key (INV-08): a retry with the same key and the
// same request returns the same token; the same key with a different request
// is a 409. The comparison decrypts the stored row rather than keeping a hash
// of the value — a plain hash of a bank account number is brute-forceable
// from the small space of valid account numbers.
func (h *Handler) Tokenize(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.vaultReady(w, r) {
		return
	}
	var req tokenizeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxVaultBody)).Decode(&req); err != nil {
		h.errJSON(w, r, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Value == "" {
		h.errJSON(w, r, http.StatusBadRequest, "value is required")
		return
	}
	if len(req.Value) > domain.MaxTokenizableValueBytes {
		h.errJSON(w, r, http.StatusBadRequest, "value exceeds the maximum tokenizable length")
		return
	}
	if !req.Classification.Tokenizable() {
		h.errJSON(w, r, http.StatusBadRequest,
			"classification must be S2_CONFIDENTIAL or S3_RESTRICTED; S4 values require HSM custody this vault does not provide")
		return
	}
	// The legal entity the value belongs to: the authorization scope now, and
	// the scope TOKEN_DETOKENIZE is checked against later, from the row.
	legalEntityID := r.Header.Get("X-Legal-Entity-Id")
	if legalEntityID == "" {
		h.errJSON(w, r, http.StatusBadRequest, "X-Legal-Entity-Id is required — it scopes the token and its later detokenization")
		return
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		h.errJSON(w, r, http.StatusBadRequest, "Idempotency-Key is required — tokenize is a material write (INV-08)")
		return
	}
	if !h.authorize(w, r, principalID, legalEntityID, TokenCreate) {
		return
	}

	tokenID := uuid.NewString()
	ciphertext, err := h.cipher.Encrypt([]byte(req.Value), vaultAAD(tenantID, tokenID))
	if err != nil {
		h.logger.Error("tokenize: encryption failed", zap.Error(err))
		h.errJSON(w, r, http.StatusInternalServerError, "failed to tokenize value")
		return
	}
	row := &domain.TokenizedValue{
		TokenID: tokenID, LegalEntityID: legalEntityID, EncryptedValue: ciphertext,
		Classification: req.Classification, CreatedByPrincipalID: principalID, IdempotencyKey: idempotencyKey,
	}
	existing, created, err := h.vault.CreateToken(r.Context(), tenantID, row)
	if errors.Is(err, store.ErrInvalidTenant) {
		h.errJSON(w, r, http.StatusBadRequest, "X-Tenant-ID must be a UUID")
		return
	}
	if err != nil {
		h.logger.Error("tokenize: vault write failed", zap.Error(err))
		h.errJSON(w, r, http.StatusInternalServerError, "failed to tokenize value")
		return
	}
	if !created {
		if !h.sameTokenizeRequest(existing, tenantID, principalID, legalEntityID, req) {
			h.errJSON(w, r, http.StatusConflict, "Idempotency-Key was already used for a different tokenize request")
			return
		}
		tokenID = existing.TokenID // replay: same token, nothing new created or reported
	} else {
		h.siem.Stream(r.Context(), tenantID, "token.created", siem.SeverityMedium,
			"Token created: "+tokenID+" classification="+string(req.Classification)+
				" legal_entity="+legalEntityID+" principal="+principalID+correlationSuffix(r))
	}
	w.Header().Set("Cache-Control", "no-store")
	h.okJSON(w, r, http.StatusOK, tokenizeResponse{Token: tokenID})
}

// sameTokenizeRequest reports whether the row an Idempotency-Key already
// claimed was written by an identical request, comparing the value in
// constant time after decrypting it.
func (h *Handler) sameTokenizeRequest(existing *domain.TokenizedValue, tenantID, principalID, legalEntityID string, req tokenizeRequest) bool {
	if existing.LegalEntityID != legalEntityID || existing.Classification != req.Classification ||
		existing.CreatedByPrincipalID != principalID {
		return false
	}
	stored, err := h.cipher.Decrypt(existing.EncryptedValue, vaultAAD(tenantID, existing.TokenID))
	if err != nil {
		h.logger.Error("tokenize replay: stored value did not decrypt", zap.String("token_id", existing.TokenID))
		return false
	}
	return subtle.ConstantTimeCompare(stored, []byte(req.Value)) == 1
}

// Detokenize returns the raw value behind a token — the one route in this
// service that discloses an S3 Restricted value.
//
// Every attempt that gets as far as an identified tenant and principal is
// recorded as token.detokenized with its outcome, refusals included
// (SEC-INV-15: the evidence is of the attempt, not only of successes;
// SEC-T29: "denies and records attempt").
//
// A token that is not this tenant's is refused with 403, and so is a token
// that does not exist at all — deliberately the same answer. Under RLS the
// vault cannot see another tenant's row, so it could not tell the two apart
// even if asked to, and it must not: a 403-for-foreign / 404-for-missing split
// would tell a caller that some other tenant holds that token.
func (h *Handler) Detokenize(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.vaultReady(w, r) {
		return
	}
	var req detokenizeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxVaultBody)).Decode(&req); err != nil || req.Token == "" {
		h.recordDetokenize(r, tenantID, principalID, "", "refused:invalid_request")
		h.errJSON(w, r, http.StatusBadRequest, "token is required")
		return
	}

	row, err := h.vault.GetToken(r.Context(), tenantID, req.Token)
	if errors.Is(err, domain.ErrTokenNotFound) || errors.Is(err, store.ErrInvalidTenant) {
		h.recordDetokenize(r, tenantID, principalID, req.Token, "refused:not_available_to_tenant")
		h.errJSON(w, r, http.StatusForbidden, "token is not available to this caller")
		return
	}
	if err != nil {
		h.recordDetokenize(r, tenantID, principalID, req.Token, "failed:vault_unavailable")
		h.logger.Error("detokenize: vault read failed", zap.Error(err))
		h.errJSON(w, r, http.StatusServiceUnavailable, "token vault unavailable")
		return
	}

	// SEC-T15: separate, elevated authorization, at the legal entity the
	// token was created under — from the row, never from the caller.
	if err := h.authz.CheckAllowed(r.Context(), principalID, row.LegalEntityID, TokenDetokenize); err != nil {
		outcome := "refused:not_authorized"
		if !errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			outcome = "refused:authorization_unavailable"
		}
		h.recordDetokenize(r, tenantID, principalID, req.Token, outcome)
		h.writeAuthzRefusal(w, r, err)
		return
	}

	value, err := h.cipher.Decrypt(row.EncryptedValue, vaultAAD(tenantID, row.TokenID))
	if err != nil {
		h.recordDetokenize(r, tenantID, principalID, req.Token, "failed:decrypt")
		h.logger.Error("detokenize: stored value did not decrypt", zap.String("token_id", row.TokenID))
		h.errJSON(w, r, http.StatusInternalServerError, "failed to detokenize")
		return
	}
	h.recordDetokenize(r, tenantID, principalID, req.Token, "succeeded")
	w.Header().Set("Cache-Control", "no-store")
	h.okJSON(w, r, http.StatusOK, detokenizeResponse{Value: string(value)})
}

// recordDetokenize is the SEC-INV-15 evidence for one detokenization attempt:
// a token.detokenized SIEM event (HIGH, every outcome) and the same record in
// this service's own log, which exists whether or not the tenant has a SIEM
// exporter. Neither carries the value.
func (h *Handler) recordDetokenize(r *http.Request, tenantID, principalID, token, outcome string) {
	tok := auditToken(token)
	h.siem.Stream(r.Context(), tenantID, "token.detokenized", siem.SeverityHigh,
		"Detokenization "+outcome+": token="+tok+" principal="+principalID+correlationSuffix(r))
	h.logger.Info("detokenization attempt",
		zap.String("event_type", "token.detokenized"),
		zap.String("outcome", outcome),
		zap.String("tenant_id", tenantID),
		zap.String("principal_id", principalID),
		zap.String("token", tok),
		zap.String("correlation_id", r.Header.Get("X-Correlation-ID")))
}

// auditToken is the token as it may appear in evidence. A token is a UUID; a
// caller-supplied string that is not one is replaced rather than copied into
// SIEM and logs verbatim.
func auditToken(token string) string {
	if token == "" {
		return "<none>"
	}
	if _, err := uuid.Parse(token); err != nil {
		return "<malformed>"
	}
	return strings.ToLower(token)
}

func correlationSuffix(r *http.Request) string {
	if c := r.Header.Get("X-Correlation-ID"); c != "" {
		return " correlation=" + c
	}
	return ""
}
