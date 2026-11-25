package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"zoiko.io/data-quality-svc/internal/domain"
	customMiddleware "zoiko.io/data-quality-svc/internal/middleware"
	"zoiko.io/data-quality-svc/internal/store"
)

// DQC-01 contract registry API (ZS-SVC-Z-001 section 5). Reading a contract, changing one and
// publishing one are three different permissions: whoever may author a contract is not thereby
// allowed to publish it.
const (
	ActionContractRead    = "DQC_CONTRACT_READ"
	ActionContractManage  = "DQC_CONTRACT_MANAGE"
	ActionContractPublish = "DQC_CONTRACT_PUBLISH"
)

func registerContractRoutes(r chi.Router, h *Handler) {
	r.Post("/dqc/contracts", h.CreateContract)
	r.Get("/dqc/contracts/{assetID}/versions", h.ListContractVersions)
	r.Get("/dqc/contracts/{assetID}/versions/{version}", h.GetContractVersion)
	r.Post("/dqc/contracts/{assetID}/versions/{version}:submit", h.SubmitContract)
	r.Post("/dqc/contracts/{assetID}/versions/{version}:publish", h.PublishContract)
	r.Post("/dqc/contracts/{assetID}/versions/{version}:deprecate", h.DeprecateContract)
	r.Post("/dqc/contracts/{assetID}/versions/{version}:retire", h.RetireContract)
	r.Post("/dqc/contracts/{assetID}/compatibility-check", h.CheckContractCompatibility)
}

// strictBody decodes a JSON body refusing unknown fields (a misspelt field must not be silently
// dropped from a contract) and returns the request hash for idempotency. An empty body is allowed
// only when allowEmpty.
func strictBody(r *http.Request, v any, allowEmpty bool) (string, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if !allowEmpty {
			return "", errors.New("a request body is required")
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return "", err
		}
		if dec.More() {
			return "", errors.New("unexpected data after the JSON body")
		}
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (h *Handler) versionParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || n < 1 {
		h.respondError(w, http.StatusBadRequest, "version must be a positive integer")
		return 0, false
	}
	return n, true
}

func (h *Handler) gate(w http.ResponseWriter, r *http.Request, action string) (tenant, principal string, ok bool) {
	tenant = customMiddleware.GetTenantID(r.Context())
	principal, ok = h.requirePrincipal(w, r)
	if !ok {
		return "", "", false
	}
	if !h.authorize(w, r, principal, tenant, action) {
		return "", "", false
	}
	return tenant, principal, true
}

func (h *Handler) CreateContract(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.gate(w, r, ActionContractManage)
	if !ok {
		return
	}
	var req domain.CreateContractRequest
	hash, err := strictBody(r, &req, false)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	claim := h.idempotencyClaim(principal, "CreateContract", req.AssetKey, hash, r.Header.Get("Idempotency-Key"))
	v, err := h.store.CreateContract(r.Context(), tenant, req, principal, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, v)
}

func (h *Handler) GetContractVersion(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.gate(w, r, ActionContractRead)
	if !ok {
		return
	}
	n, ok := h.versionParam(w, r)
	if !ok {
		return
	}
	v, err := h.store.GetContractVersion(r.Context(), tenant, chi.URLParam(r, "assetID"), n)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, v)
}

func (h *Handler) ListContractVersions(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.gate(w, r, ActionContractRead)
	if !ok {
		return
	}
	vs, err := h.store.ListContractVersions(r.Context(), tenant, chi.URLParam(r, "assetID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, map[string]any{"data": vs, "count": len(vs)})
}

func (h *Handler) SubmitContract(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.gate(w, r, ActionContractManage)
	if !ok {
		return
	}
	n, ok := h.versionParam(w, r)
	if !ok {
		return
	}
	var none struct{}
	hash, err := strictBody(r, &none, true)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	assetID := chi.URLParam(r, "assetID")
	claim := h.idempotencyClaim(principal, "SubmitContract", assetID+"/"+chi.URLParam(r, "version"), hash, r.Header.Get("Idempotency-Key"))
	v, err := h.store.SubmitContract(r.Context(), tenant, assetID, n, principal, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, v)
}

func (h *Handler) PublishContract(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.gate(w, r, ActionContractPublish)
	if !ok {
		return
	}
	n, ok := h.versionParam(w, r)
	if !ok {
		return
	}
	var req domain.PublishContractRequest
	hash, err := strictBody(r, &req, true)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	assetID := chi.URLParam(r, "assetID")
	claim := h.idempotencyClaim(principal, "PublishContract", assetID+"/"+chi.URLParam(r, "version"), hash, r.Header.Get("Idempotency-Key"))
	v, err := h.store.PublishContract(r.Context(), tenant, assetID, n, principal, req, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, v)
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) endOfLife(w http.ResponseWriter, r *http.Request, op string,
	do func(ctx *http.Request, tenant, assetID string, n int, principal, reason string, claim domain.IdempotencyClaim) (*domain.ContractVersion, error)) {
	tenant, principal, ok := h.gate(w, r, ActionContractPublish)
	if !ok {
		return
	}
	n, ok := h.versionParam(w, r)
	if !ok {
		return
	}
	var req reasonRequest
	hash, err := strictBody(r, &req, false)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	assetID := chi.URLParam(r, "assetID")
	claim := h.idempotencyClaim(principal, op, assetID+"/"+chi.URLParam(r, "version"), hash, r.Header.Get("Idempotency-Key"))
	v, err := do(r, tenant, assetID, n, principal, req.Reason, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, v)
}

func (h *Handler) DeprecateContract(w http.ResponseWriter, r *http.Request) {
	h.endOfLife(w, r, "DeprecateContract", func(rq *http.Request, tenant, assetID string, n int, principal, reason string, claim domain.IdempotencyClaim) (*domain.ContractVersion, error) {
		return h.store.DeprecateContract(rq.Context(), tenant, assetID, n, principal, reason, claim)
	})
}

func (h *Handler) RetireContract(w http.ResponseWriter, r *http.Request) {
	h.endOfLife(w, r, "RetireContract", func(rq *http.Request, tenant, assetID string, n int, principal, reason string, claim domain.IdempotencyClaim) (*domain.ContractVersion, error) {
		return h.store.RetireContract(rq.Context(), tenant, assetID, n, principal, reason, claim)
	})
}

func (h *Handler) CheckContractCompatibility(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.gate(w, r, ActionContractRead)
	if !ok {
		return
	}
	var req domain.CompatibilityCheckRequest
	if _, err := strictBody(r, &req, false); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	rep, err := h.store.CheckContractCompatibility(r.Context(), tenant, chi.URLParam(r, "assetID"), req)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, rep)
}

// contractError maps contract-registry errors to statuses. It reports whether it handled err.
func (h *Handler) contractError(w http.ResponseWriter, err error) bool {
	var inc *store.IncompatibleError
	switch {
	case errors.As(err, &inc):
		h.respondJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "DQC-003", "evidence": inc.Report})
	case errors.Is(err, domain.ErrContractNotFound), errors.Is(err, domain.ErrContractVersionUnknown):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrContractTransition), errors.Is(err, domain.ErrContractDraftOpen),
		errors.Is(err, domain.ErrContractAssetKindClash), errors.Is(err, domain.ErrBreakingNotAcknowledged),
		errors.Is(err, domain.ErrContractNotPublishedBase), errors.Is(err, domain.ErrContractNotPublished):
		h.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrCertificationAlreadyRevoked):
		h.respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrRevocationInvalid):
		h.respondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrContractNotIndependent), errors.Is(err, domain.ErrRevokerIsCertifier):
		h.respondError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, domain.ErrContractInvalid):
		h.respondError(w, http.StatusBadRequest, err.Error())
	default:
		return false
	}
	return true
}
