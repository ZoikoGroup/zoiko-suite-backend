package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
)

func computeRequestHash(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// CreatePurpose handles POST /privacy/purposes.
func (h *Handler) CreatePurpose(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.begin(w, r, PrivacyPurposeCreate, true)
	if !ok {
		return
	}
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	key, hash, handled := h.idempotency(w, r, cc, body)
	if handled {
		return
	}

	var req domain.CreatePurposeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Statement) == "" || strings.TrimSpace(req.CompatibilityClass) == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "statement and compatibility_class are required")
		return
	}
	if scopeMismatch(w, req.TenantID, cc) {
		return
	}
	req.LawfulBasisRefs = domain.NormalizeRefs(req.LawfulBasisRefs)
	req.SpecialConditionRefs = domain.NormalizeRefs(req.SpecialConditionRefs)
	req.NecessityRationale = strings.TrimSpace(req.NecessityRationale)

	payload, existing, err := h.execute(r, cc, "CreatePurpose", key, hash, http.StatusCreated, func(ctx context.Context) (any, error) {
		_, version, err := h.store.CreatePurpose(ctx, cc.RecordTenant(), req, cc.Principal)
		return version, err
	})
	if err != nil {
		h.storeUnavailable(w, "CreatePurpose", err)
		return
	}
	h.finish(w, existing, hash, http.StatusCreated, payload)
}

// CreatePurposeVersion handles POST /privacy/purposes/{purposeID}/versions.
func (h *Handler) CreatePurposeVersion(w http.ResponseWriter, r *http.Request) {
	purposeID := chi.URLParam(r, "purposeID")
	cc, ok := h.begin(w, r, PrivacyPurposeCreate, true)
	if !ok {
		return
	}
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	key, hash, handled := h.idempotency(w, r, cc, body)
	if handled {
		return
	}

	var req domain.CreatePurposeVersionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "invalid request body")
		return
	}
	if req.ParentVersionID == "" || strings.TrimSpace(req.Statement) == "" || strings.TrimSpace(req.CompatibilityClass) == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "parent_version_id, statement and compatibility_class are required")
		return
	}

	parent, err := h.store.FindPurposeVersion(r.Context(), purposeID, req.ParentVersionID)
	if err != nil {
		if errors.Is(err, domain.ErrPurposeVersionNotFound) {
			writeCoded(w, http.StatusNotFound, domain.ErrCodePurposeNotRegistered, "parent purpose version not found")
			return
		}
		h.storeUnavailable(w, "CreatePurposeVersion", err)
		return
	}
	// Platform tenant isolation (§23.1): only the owning plane may extend a record.
	if wrongPlane(w, cc, parent.TenantID) {
		return
	}
	req.LawfulBasisRefs = domain.NormalizeRefs(req.LawfulBasisRefs)
	req.SpecialConditionRefs = domain.NormalizeRefs(req.SpecialConditionRefs)
	req.NecessityRationale = strings.TrimSpace(req.NecessityRationale)

	payload, existing, err := h.execute(r, cc, "CreatePurposeVersion", key, hash, http.StatusCreated, func(ctx context.Context) (any, error) {
		return h.store.CreatePurposeVersion(ctx, purposeID, req, cc.Principal)
	})
	if err != nil {
		if errors.Is(err, domain.ErrPurposeVersionNotFound) {
			writeCoded(w, http.StatusNotFound, domain.ErrCodePurposeNotRegistered, "parent purpose version not found")
			return
		}
		h.storeUnavailable(w, "CreatePurposeVersion", err)
		return
	}
	h.finish(w, existing, hash, http.StatusCreated, payload)
}

// PublishPurposeVersion handles POST /privacy/purposes/{purposeID}/versions/{versionID}/publish.
// Segregation of Duties (Maker-Checker, §18): a maker cannot publish their own purpose version.
func (h *Handler) PublishPurposeVersion(w http.ResponseWriter, r *http.Request) {
	purposeID := chi.URLParam(r, "purposeID")
	versionID := chi.URLParam(r, "versionID")
	cc, ok := h.begin(w, r, PrivacyPurposePublish, true)
	if !ok {
		return
	}
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	key, hash, handled := h.idempotency(w, r, cc, body)
	if handled {
		return
	}

	existing, err := h.store.FindPurposeVersion(r.Context(), purposeID, versionID)
	if err != nil {
		if errors.Is(err, domain.ErrPurposeVersionNotFound) {
			writeCoded(w, http.StatusNotFound, domain.ErrCodePurposeNotRegistered, "purpose version not found")
			return
		}
		h.storeUnavailable(w, "PublishPurposeVersion", err)
		return
	}
	if wrongPlane(w, cc, existing.TenantID) {
		return
	}
	if existing.CreatedByPrincipalID != "" && existing.CreatedByPrincipalID == cc.Principal {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeSoDDenied, "maker cannot publish their own purpose version (segregation of duties)")
		return
	}

	payload, existing2, err := h.execute(r, cc, "PublishPurposeVersion", key, hash, http.StatusOK, func(ctx context.Context) (any, error) {
		return h.store.PublishPurposeVersion(ctx, purposeID, versionID, cc.Principal, cc.Correlation)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPurposeVersionNotFound):
			writeCoded(w, http.StatusNotFound, domain.ErrCodePurposeNotRegistered, "purpose version not found")
		case errors.Is(err, domain.ErrPurposeAlreadyPublished):
			// A published version is append-only evidence: re-publishing is an
			// attempted edit of it (§32 PRV-020).
			writeCoded(w, http.StatusConflict, domain.ErrCodeImmutableEvidence, "purpose version is already published and immutable")
		default:
			h.storeUnavailable(w, "PublishPurposeVersion", err)
		}
		return
	}
	h.finish(w, existing2, hash, http.StatusOK, payload)
}

// GetPurpose handles GET /privacy/purposes/{purposeID}?as_of=&known_as_of=.
func (h *Handler) GetPurpose(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.begin(w, r, PrivacyPurposeRead, false)
	_ = cc
	if !ok {
		return
	}
	asOf, knownAsOf, ok := h.parseInstants(w, r)
	if !ok {
		return
	}
	version, err := h.store.ResolvePurposeAsOf(r.Context(), chi.URLParam(r, "purposeID"), asOf, knownAsOf)
	if err != nil {
		if errors.Is(err, domain.ErrPurposeNotFound) {
			writeCoded(w, http.StatusNotFound, domain.ErrCodePurposeNotRegistered, "no published purpose version in force at the requested time")
			return
		}
		h.storeUnavailable(w, "GetPurpose", err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

// ListPurposes handles GET /privacy/purposes.
func (h *Handler) ListPurposes(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, PrivacyPurposeRead, false); !ok {
		return
	}
	versions, err := h.store.ListPurposes(r.Context())
	if err != nil {
		h.storeUnavailable(w, "ListPurposes", err)
		return
	}
	if versions == nil {
		versions = []domain.PurposeVersion{}
	}
	writeJSON(w, http.StatusOK, versions)
}
