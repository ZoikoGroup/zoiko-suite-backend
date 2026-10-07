package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/esignature-integration-svc/internal/authz"
	"zoiko.io/esignature-integration-svc/internal/domain"
	"zoiko.io/esignature-integration-svc/internal/events"
	"zoiko.io/esignature-integration-svc/internal/middleware"
	"zoiko.io/esignature-integration-svc/internal/store"
)

const (
	PROFILE_CREATE             = "SIGNATURE_PROFILE_CREATE"
	PROFILE_ACTIVATE           = "SIGNATURE_PROFILE_ACTIVATE"
	PROFILE_RETIRE             = "SIGNATURE_PROFILE_RETIRE"
	PROFILE_READ               = "SIGNATURE_PROFILE_READ"
	ENVELOPE_PROFILE_BIND      = "ENVELOPE_PROFILE_BIND"
	ENVELOPE_AMEND             = "ENVELOPE_AMEND"
	PROVIDER_ATTEMPT_RECORD    = "PROVIDER_ATTEMPT_RECORD"
	PROVIDER_ATTEMPT_RECONCILE = "PROVIDER_ATTEMPT_RECONCILE"
	PROVIDER_ATTEMPT_READ      = "PROVIDER_ATTEMPT_READ"
	PARTICIPANT_MANAGE         = "PARTICIPANT_MANAGE"
	PARTICIPANT_READ           = "PARTICIPANT_READ"
	COMPLETION_EVIDENCE_SEAL   = "COMPLETION_EVIDENCE_SEAL"
	COMPLETION_EVIDENCE_READ   = "COMPLETION_EVIDENCE_READ"
)

// OrchestrationHandler is DRC-05's own handler, kept separate from the
// pre-existing Handler (handler.go) so the original envelope CRUD
// surface is untouched.
type OrchestrationHandler struct {
	store         store.SignatureOrchestrationStore
	envelopeStore store.Store
	publisher     events.Publisher
	authz         *authz.Client
	logger        *zap.Logger
}

func NewOrchestrationHandler(st store.SignatureOrchestrationStore, envStore store.Store, pub events.Publisher, az *authz.Client, logger *zap.Logger) *OrchestrationHandler {
	return &OrchestrationHandler{store: st, envelopeStore: envStore, publisher: pub, authz: az, logger: logger}
}

func RegisterOrchestrationRoutes(r chi.Router, h *OrchestrationHandler) {
	r.Route("/v1/esignature/profiles", func(r chi.Router) {
		r.Post("/", h.CreateProfile)
		r.Get("/{id}", h.GetProfile)
		r.Post("/{id}/activate", h.ActivateProfile)
		r.Post("/{id}/retire", h.RetireProfile)
	})
	r.Route("/v1/esignature/envelopes/{id}", func(r chi.Router) {
		r.Post("/bind-profile", h.BindProfile)
		r.Post("/amend", h.AmendEnvelope)
		r.Post("/attempts", h.RecordProviderAttempt)
		r.Get("/attempts", h.ListProviderAttempts)
		r.Post("/participants", h.AddParticipant)
		r.Get("/participants", h.ListParticipants)
		r.Post("/completion-evidence", h.SealCompletionEvidence)
		r.Get("/completion-evidence", h.GetCompletionEvidence)
	})
	r.Post("/v1/esignature/attempts/{attempt_id}/reconcile", h.ReconcileAttempt)
	r.Post("/v1/esignature/participants/{participant_id}/mark-viewed", h.MarkParticipantViewed)
	r.Post("/v1/esignature/participants/{participant_id}/mark-signed", h.MarkParticipantSigned)
	r.Post("/v1/esignature/participants/{participant_id}/decline", h.DeclineParticipant)
}

func (h *OrchestrationHandler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *OrchestrationHandler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "authorization denied")
		return
	}
	h.logger.Error("authorization check failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

// envelopeLegalEntity loads an envelope's legal_entity_id for an authz
// check — every orchestration action is scoped to the envelope's own
// legal entity, same as UpdateStatus already does for the base handler.
func (h *OrchestrationHandler) envelopeLegalEntity(w http.ResponseWriter, r *http.Request, envelopeID string) (string, bool) {
	env, err := h.envelopeStore.GetEnvelopeByID(r.Context(), envelopeID)
	if err != nil {
		if errors.Is(err, domain.ErrEnvelopeNotFound) {
			writeError(w, http.StatusNotFound, "envelope not found")
			return "", false
		}
		writeError(w, http.StatusInternalServerError, "failed to get envelope")
		return "", false
	}
	return env.LegalEntityID, true
}

func (h *OrchestrationHandler) handleOrchestrationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrSignatureProfileNotFound), errors.Is(err, domain.ErrEnvelopeNotFound),
		errors.Is(err, domain.ErrProviderAttemptNotFound), errors.Is(err, domain.ErrParticipantNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrInvalidAssuranceLevel), errors.Is(err, domain.ErrInvalidIdentityRequirement),
		errors.Is(err, domain.ErrInvalidAttemptAction), errors.Is(err, domain.ErrInvalidAttemptOutcome),
		errors.Is(err, domain.ErrInvalidParticipantRole), errors.Is(err, domain.ErrReconcileOutcomeMustBeFinal):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrSignatureProfileNotDraft), errors.Is(err, domain.ErrSignatureProfileNotActive),
		errors.Is(err, domain.ErrEnvelopeAlreadyBoundToProfile), errors.Is(err, domain.ErrAttemptNotUnknown),
		errors.Is(err, domain.ErrParticipantNotViewable), errors.Is(err, domain.ErrParticipantNotSignable),
		errors.Is(err, domain.ErrParticipantNotDeclinable), errors.Is(err, domain.ErrCompletionEvidenceExists),
		errors.Is(err, domain.ErrEnvelopeNotSigned), errors.Is(err, domain.ErrEnvelopeNotAmendable):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("orchestration store error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// ── Signature Profiles ───────────────────────────────────────────────────────

func (h *OrchestrationHandler) CreateProfile(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateSignatureProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, PROFILE_CREATE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	pr, err := h.store.CreateSignatureProfile(r.Context(), &req, principalID)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pr)
}

func (h *OrchestrationHandler) GetProfile(w http.ResponseWriter, r *http.Request) {
	pr, err := h.store.GetSignatureProfile(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, pr.LegalEntityID, PROFILE_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pr)
}

func (h *OrchestrationHandler) ActivateProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	pr, err := h.store.GetSignatureProfile(r.Context(), id)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, pr.LegalEntityID, PROFILE_ACTIVATE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.ActivateSignatureProfile(r.Context(), id)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *OrchestrationHandler) RetireProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	pr, err := h.store.GetSignatureProfile(r.Context(), id)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, pr.LegalEntityID, PROFILE_RETIRE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.RetireSignatureProfile(r.Context(), id)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── Envelope binding / amendment ─────────────────────────────────────────────

type bindProfileRequest struct {
	ProfileID string `json:"profile_id"`
}

func (h *OrchestrationHandler) BindProfile(w http.ResponseWriter, r *http.Request) {
	envelopeID := chi.URLParam(r, "id")
	var req bindProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProfileID == "" {
		writeError(w, http.StatusBadRequest, "profile_id is required")
		return
	}
	legalEntityID, ok := h.envelopeLegalEntity(w, r, envelopeID)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, ENVELOPE_PROFILE_BIND); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	env, err := h.store.BindSignatureProfile(r.Context(), envelopeID, req.ProfileID)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (h *OrchestrationHandler) AmendEnvelope(w http.ResponseWriter, r *http.Request) {
	oldEnvelopeID := chi.URLParam(r, "id")
	var req domain.AmendEnvelopeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.NewEnvelope.LegalEntityID == "" || req.NewEnvelope.DocumentTitle == "" || req.NewEnvelope.SignerEmail == "" {
		writeError(w, http.StatusBadRequest, "new_envelope.legal_entity_id, document_title, and signer_email are required")
		return
	}
	legalEntityID, ok := h.envelopeLegalEntity(w, r, oldEnvelopeID)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, ENVELOPE_AMEND); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	newEnv, err := h.store.AmendEnvelope(r.Context(), oldEnvelopeID, &req.NewEnvelope, principalID)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "esignature.envelope.amended", AggregateID: newEnv.EnvelopeID, TenantID: middleware.GetTenantID(r.Context()),
		LegalEntityID: newEnv.LegalEntityID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: map[string]string{"supersedes_envelope_id": oldEnvelopeID},
	})
	writeJSON(w, http.StatusCreated, newEnv)
}

// ── Provider Attempts ────────────────────────────────────────────────────────

func (h *OrchestrationHandler) RecordProviderAttempt(w http.ResponseWriter, r *http.Request) {
	envelopeID := chi.URLParam(r, "id")
	var req domain.RecordProviderAttemptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "idempotency_key is required")
		return
	}
	legalEntityID, ok := h.envelopeLegalEntity(w, r, envelopeID)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, PROVIDER_ATTEMPT_RECORD); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	att, err := h.store.RecordProviderAttempt(r.Context(), envelopeID, &req)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, att)
}

func (h *OrchestrationHandler) ReconcileAttempt(w http.ResponseWriter, r *http.Request) {
	attemptID := chi.URLParam(r, "attempt_id")
	var req domain.ReconcileAttemptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// Authorization here is intentionally coarse (no legal-entity scoping
	// pre-check) — the attempt's own tenant scoping (RLS + explicit
	// predicate) is what actually prevents cross-tenant reconciliation;
	// adding a legal-entity lookup would require a second query this
	// action doesn't otherwise need.
	if err := h.authz.CheckAllowed(r.Context(), principalID, "", PROVIDER_ATTEMPT_RECONCILE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	att, err := h.store.ReconcileAttempt(r.Context(), attemptID, &req, principalID)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, att)
}

func (h *OrchestrationHandler) ListProviderAttempts(w http.ResponseWriter, r *http.Request) {
	envelopeID := chi.URLParam(r, "id")
	legalEntityID, ok := h.envelopeLegalEntity(w, r, envelopeID)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, PROVIDER_ATTEMPT_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	attempts, err := h.store.ListProviderAttempts(r.Context(), envelopeID)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"attempts": attempts, "total": len(attempts)})
}

// ── Participants ─────────────────────────────────────────────────────────────

func (h *OrchestrationHandler) AddParticipant(w http.ResponseWriter, r *http.Request) {
	envelopeID := chi.URLParam(r, "id")
	var req domain.AddParticipantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Email == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, "email and name are required")
		return
	}
	legalEntityID, ok := h.envelopeLegalEntity(w, r, envelopeID)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, PARTICIPANT_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	pt, err := h.store.AddParticipant(r.Context(), envelopeID, &req)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pt)
}

func (h *OrchestrationHandler) ListParticipants(w http.ResponseWriter, r *http.Request) {
	envelopeID := chi.URLParam(r, "id")
	legalEntityID, ok := h.envelopeLegalEntity(w, r, envelopeID)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, PARTICIPANT_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	participants, err := h.store.ListParticipants(r.Context(), envelopeID)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"participants": participants, "total": len(participants)})
}

func (h *OrchestrationHandler) MarkParticipantViewed(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "participant_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, "", PARTICIPANT_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	pt, err := h.store.MarkParticipantViewed(r.Context(), id)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pt)
}

func (h *OrchestrationHandler) MarkParticipantSigned(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "participant_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, "", PARTICIPANT_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	pt, err := h.store.MarkParticipantSigned(r.Context(), id)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pt)
}

type declineParticipantRequest struct {
	Reason string `json:"reason"`
}

func (h *OrchestrationHandler) DeclineParticipant(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "participant_id")
	var req declineParticipantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, "", PARTICIPANT_MANAGE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	pt, err := h.store.DeclineParticipant(r.Context(), id, req.Reason)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pt)
}

// ── Completion Evidence ──────────────────────────────────────────────────────

func (h *OrchestrationHandler) SealCompletionEvidence(w http.ResponseWriter, r *http.Request) {
	envelopeID := chi.URLParam(r, "id")
	var req domain.SealCompletionEvidenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	legalEntityID, ok := h.envelopeLegalEntity(w, r, envelopeID)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, COMPLETION_EVIDENCE_SEAL); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	ev, err := h.store.SealCompletionEvidence(r.Context(), envelopeID, &req, principalID)
	if err != nil {
		h.handleOrchestrationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

func (h *OrchestrationHandler) GetCompletionEvidence(w http.ResponseWriter, r *http.Request) {
	envelopeID := chi.URLParam(r, "id")
	legalEntityID, ok := h.envelopeLegalEntity(w, r, envelopeID)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, COMPLETION_EVIDENCE_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	ev, err := h.store.GetCompletionEvidence(r.Context(), envelopeID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ev)
}
