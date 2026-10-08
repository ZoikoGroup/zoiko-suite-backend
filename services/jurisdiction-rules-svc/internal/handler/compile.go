package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 Wave 1: compile, sign, verify and the trusted-key registry.
// Compile, sign, verify and key administration are separate authorization
// actions: authoring, signing and promotion are separate privileged
// capabilities (s28).

// WithSigner supplies the signer used by the sign command. Without one the
// sign command fails closed with 503 signing_not_configured.
func (h *Handler) WithSigner(s domain.Signer) *Handler {
	h.signer = s
	return h
}

func registerCompileRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/packs/{pack_ref}/versions/{version}/artifact", h.GetPackArtifact)
	r.Post("/v1/packs/{pack_ref}/versions/{version}/verify", h.VerifyPackArtifact)
	r.Get("/v1/pack-signing-keys", h.ListSigningKeys)

	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/compile", h.CompilePackVersion)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/sign", h.SignPackVersion)
	r.Post("/v1/admin/pack-signing-keys", h.RegisterSigningKey)
	r.Post("/v1/admin/pack-signing-keys/{key_ref}/retire", h.RetireSigningKey)
	r.Post("/v1/admin/pack-signing-keys/{key_ref}/revoke", h.RevokeSigningKey)
}

func (h *Handler) writeCompileError(w http.ResponseWriter, err error, corr string) {
	var cf *store.CompileFailure
	switch {
	case errors.As(err, &cf):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "compile_failed", "message": "the pack has errors and no artifact was produced", "report": cf.Report})
	case errors.Is(err, domain.ErrNotUnderReview):
		writeError(w, http.StatusConflict, "not_under_review", err.Error())
	case errors.Is(err, domain.ErrAlreadyCompiled):
		writeError(w, http.StatusConflict, "already_compiled", err.Error())
	case errors.Is(err, domain.ErrNotCompiled):
		writeError(w, http.StatusNotFound, "not_compiled", err.Error())
	case errors.Is(err, domain.ErrKeyNotFound):
		writeError(w, http.StatusNotFound, "signing_key_not_found", err.Error())
	case errors.Is(err, domain.ErrKeyMismatch):
		writeError(w, http.StatusConflict, "key_mismatch", err.Error())
	case errors.Is(err, domain.ErrSigningNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "signing_not_configured", err.Error())
	case errors.Is(err, domain.ErrSigningKeyNotActive):
		writeError(w, http.StatusServiceUnavailable, "signing_key_not_active", err.Error())
	default:
		h.writeRegistryError(w, err, corr)
	}
}

// CompilePackVersion compiles a REVIEW version into an unsigned artifact.
// 201 new artifact, 200 identical replay, 422 compile errors (report in body).
func (h *Handler) CompilePackVersion(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "compile")
	if !ok {
		return
	}
	a, created, err := h.registry.CompilePackVersion(r.Context(), chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version"), actor)
	if err != nil {
		h.writeCompileError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		h.emitRegistry(r, events.EventPackVersionCompiled, a.PackVersionID, actor, map[string]any{
			"pack_version_id": a.PackVersionID, "pack_ref": a.PackRef, "version": a.Version, "artifact_digest": a.ArtifactDigest,
			"warnings": a.Report.Warnings})
		writeJSON(w, http.StatusCreated, a)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// SignPackVersion signs the compiled artifact with the service's signing key.
func (h *Handler) SignPackVersion(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "sign")
	if !ok {
		return
	}
	if h.signer == nil {
		h.writeCompileError(w, domain.ErrSigningNotConfigured, "")
		return
	}
	a, changed, err := h.registry.SignPackVersion(r.Context(), chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version"), h.signer, actor)
	if err != nil {
		h.writeCompileError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventPackVersionSigned, a.PackVersionID, actor, map[string]any{
			"pack_version_id": a.PackVersionID, "pack_ref": a.PackRef, "version": a.Version,
			"artifact_digest": a.ArtifactDigest, "signature_key_ref": a.SignatureKeyRef})
	}
	writeJSON(w, http.StatusOK, a)
}

// GetPackArtifact returns the artifact and its live verification verdict.
func (h *Handler) GetPackArtifact(w http.ResponseWriter, r *http.Request) {
	a, res, err := h.registry.VerifyPackArtifact(r.Context(), chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version"))
	if err != nil {
		h.writeCompileError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifact": a, "verification": res})
}

// VerifyPackArtifact is the loader-facing check (JUR-NEG-03, JUR-NEG-04).
// It answers 200 only when the artifact is verified and 409 otherwise, so a
// caller that ignores the body still fails closed. A failure also emits a
// security event.
func (h *Handler) VerifyPackArtifact(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "verify")
	if !ok {
		return
	}
	ref, version := chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version")
	a, res, err := h.registry.VerifyPackArtifact(r.Context(), ref, version)
	if err != nil {
		// A pack version with no artifact is itself unverifiable: fail closed.
		if errors.Is(err, domain.ErrNotCompiled) {
			h.emitRegistry(r, events.EventPackVerificationFailed, ref+"@"+version, actor, map[string]any{
				"pack_ref": ref, "version": version, "reasons": []string{domain.ReasonUnsigned, "not_compiled"}})
			writeJSON(w, http.StatusConflict, map[string]any{"verified": false, "reasons": []string{"not_compiled"}})
			return
		}
		h.writeCompileError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if !res.Verified {
		h.emitRegistry(r, events.EventPackVerificationFailed, a.PackVersionID, actor, map[string]any{
			"pack_version_id": a.PackVersionID, "pack_ref": ref, "version": version, "reasons": res.Reasons})
		writeJSON(w, http.StatusConflict, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ── trusted signing keys ────────────────────────────────────────────────────

type RegisterSigningKeyRequest struct {
	KeyRef    string `json:"key_ref"`
	Algorithm string `json:"algorithm"`
	PublicKey string `json:"public_key"` // base64
}

// RegisterSigningKey registers a PUBLIC key. A private key is never accepted.
func (h *Handler) RegisterSigningKey(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "pack_signing_key", "register")
	if !ok {
		return
	}
	var req RegisterSigningKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"key_ref", req.KeyRef}, requiredField{"public_key", req.PublicKey}); field != "" {
		writeMissingField(w, field)
		return
	}
	alg := strings.ToUpper(strings.TrimSpace(req.Algorithm))
	if alg == "" {
		alg = domain.AlgorithmEd25519
	}
	if alg != domain.AlgorithmEd25519 {
		writeError(w, http.StatusBadRequest, "unsupported_algorithm", "only ED25519 is supported")
		return
	}
	pub, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil || len(pub) != 32 {
		writeError(w, http.StatusBadRequest, "invalid_public_key", "public_key must be a base64 32-byte Ed25519 public key")
		return
	}
	if !keyRefRe.MatchString(req.KeyRef) {
		writeError(w, http.StatusBadRequest, "invalid_key_ref", "key_ref must be lowercase letters, digits and . _ : -")
		return
	}
	k, created, err := h.registry.RegisterSigningKey(r.Context(), req.KeyRef, alg, pub, actor)
	if err != nil {
		h.writeCompileError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		h.emitRegistry(r, events.EventSigningKeyRegistered, k.KeyRef, actor, map[string]any{"key_ref": k.KeyRef, "algorithm": k.Algorithm})
		writeJSON(w, http.StatusCreated, k)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

func (h *Handler) ListSigningKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := h.registry.ListSigningKeys(r.Context())
	if err != nil {
		h.writeCompileError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": ks})
}

type KeyStatusRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) RetireSigningKey(w http.ResponseWriter, r *http.Request) {
	h.setKeyStatus(w, r, "retire", domain.KeyRetired, events.EventSigningKeyRetired)
}

func (h *Handler) RevokeSigningKey(w http.ResponseWriter, r *http.Request) {
	h.setKeyStatus(w, r, "revoke", domain.KeyRevoked, events.EventSigningKeyRevoked)
}

func (h *Handler) setKeyStatus(w http.ResponseWriter, r *http.Request, action, status, event string) {
	actor, ok := h.admin(w, r, "pack_signing_key", action)
	if !ok {
		return
	}
	var req KeyStatusRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeMissingField(w, "reason")
		return
	}
	k, changed, err := h.registry.SetSigningKeyStatus(r.Context(), chi.URLParam(r, "key_ref"), status, req.Reason, actor)
	if err != nil {
		h.writeCompileError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, event, k.KeyRef, actor, map[string]any{"key_ref": k.KeyRef, "status": k.Status, "reason": req.Reason})
	}
	writeJSON(w, http.StatusOK, k)
}
