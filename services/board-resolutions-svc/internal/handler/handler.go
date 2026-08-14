package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/board-resolutions-svc/internal/authz"
	"zoiko.io/board-resolutions-svc/internal/domain"
	"zoiko.io/board-resolutions-svc/internal/events"
	"zoiko.io/board-resolutions-svc/internal/evidencereq"
	"zoiko.io/board-resolutions-svc/internal/middleware"
	"zoiko.io/board-resolutions-svc/internal/store"
)

// AuthZClient is the subset of authz.Client the handler depends on. Defined as an
// interface here so tests can supply a stub.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// EvidenceReqClient verifies evidence sufficiency before a resolution may be
// passed. Implementations must fail closed, same doctrine as AuthZClient.
type EvidenceReqClient interface {
	EvaluateSufficient(ctx context.Context, tenantID, legalEntityID, domainCode, actionType, correlationID, principalID string, artifacts []evidencereq.Artifact) error
}

const (
	actionMeetingCreate    = "MEETING_CREATE"
	actionResolutionCreate = "RESOLUTION_CREATE"
	actionResolutionVote   = "RESOLUTION_VOTE"
	actionResolutionPass   = "RESOLUTION_PASS"
)

type Handler struct {
	store       store.Store
	publisher   events.Publisher
	authz       AuthZClient
	evidenceReq EvidenceReqClient
	logger      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthZClient, er EvidenceReqClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, evidenceReq: er, logger: logger}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "principal identity missing")
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden")
	} else {
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
	}
}

func (h *Handler) writeEvidenceErr(w http.ResponseWriter, err error) {
	if errors.Is(err, evidencereq.ErrEvidenceMissing) {
		writeError(w, http.StatusUnprocessableEntity, "required evidence is missing")
	} else {
		writeError(w, http.StatusServiceUnavailable, "evidence-requirements-svc unavailable")
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/meetings", func(r chi.Router) {
		r.Post("/", h.CreateMeeting)
		r.Get("/", h.ListMeetings)
		r.Get("/{id}", h.GetMeeting)
	})

	r.Route("/v1/resolutions", func(r chi.Router) {
		r.Post("/", h.CreateResolution)
		r.Get("/", h.ListResolutions)
		r.Get("/{id}", h.GetResolution)
		r.Post("/{id}/vote", h.RecordVotes)
		r.Post("/{id}/pass", h.PassResolution)
	})
}

// --- Meeting Handlers ---

func (h *Handler) CreateMeeting(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.CreateMeetingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.ScheduledAt.IsZero() {
		writeError(w, http.StatusBadRequest, "title and scheduled_at are required")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionMeetingCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	m := &domain.BoardMeeting{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		Title:         req.Title,
		ScheduledAt:   req.ScheduledAt,
		Location:      req.Location,
		EffectiveFrom: req.EffectiveFrom,
		CreatedBy:     req.CreatedBy,
	}

	if err := h.store.CreateMeeting(r.Context(), m); err != nil {
		h.logger.Error("create meeting failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create meeting")
		return
	}

	_ = h.publisher.Publish(r.Context(), "meeting.created", m.MeetingID, tenantID, m)
	writeJSON(w, http.StatusCreated, m)
}

func (h *Handler) GetMeeting(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	m, err := h.store.GetMeeting(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrMeetingNotFound) {
			writeError(w, http.StatusNotFound, "meeting not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get meeting")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) ListMeetings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	meetings, err := h.store.ListMeetings(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list meetings")
		return
	}
	if meetings == nil {
		meetings = []domain.BoardMeeting{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"meetings": meetings, "total": len(meetings)})
}

// --- Resolution Handlers ---

func (h *Handler) CreateResolution(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.CreateResolutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.Content == "" || req.Category == "" {
		writeError(w, http.StatusBadRequest, "title, content, and category are required")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionResolutionCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	res := &domain.BoardResolution{
		MeetingID:        req.MeetingID,
		TenantID:         tenantID,
		LegalEntityID:    req.LegalEntityID,
		ResolutionNumber: req.ResolutionNumber,
		Title:            req.Title,
		Content:          req.Content,
		Category:         req.Category,
		EffectiveFrom:    req.EffectiveFrom,
		EffectiveTo:      req.EffectiveTo,
		CreatedBy:        req.CreatedBy,
	}

	if err := h.store.CreateResolution(r.Context(), res); err != nil {
		h.logger.Error("create resolution failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create resolution")
		return
	}

	_ = h.publisher.Publish(r.Context(), "resolution.created", res.ResolutionID, tenantID, res)
	writeJSON(w, http.StatusCreated, res)
}

func (h *Handler) GetResolution(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	res, err := h.store.GetResolution(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrResolutionNotFound) {
			writeError(w, http.StatusNotFound, "resolution not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get resolution")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) ListResolutions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	meetingID := r.URL.Query().Get("meeting_id")
	status := r.URL.Query().Get("status")
	resolutions, err := h.store.ListResolutions(r.Context(), legalEntityID, meetingID, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list resolutions")
		return
	}
	if resolutions == nil {
		resolutions = []domain.BoardResolution{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"resolutions": resolutions, "total": len(resolutions)})
}

func (h *Handler) RecordVotes(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.RecordVotesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existing, err := h.store.GetResolution(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrResolutionNotFound) {
			writeError(w, http.StatusNotFound, "resolution not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get resolution")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionResolutionVote); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	res, err := h.store.RecordVotes(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrResolutionNotFound):
			writeError(w, http.StatusNotFound, "resolution not found")
		case errors.Is(err, domain.ErrResolutionAlreadyFinalized):
			writeError(w, http.StatusConflict, "resolution is already finalized")
		default:
			writeError(w, http.StatusInternalServerError, "failed to record votes")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "resolution.votes_recorded", id, tenantID, res)
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) PassResolution(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.PassResolutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.PassedBy == "" {
		writeError(w, http.StatusBadRequest, "passed_by is required")
		return
	}

	existing, err := h.store.GetResolution(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrResolutionNotFound) {
			writeError(w, http.StatusNotFound, "resolution not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get resolution")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionResolutionPass); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3):
	// PassResolution is the distinct closing action that finalizes a
	// resolution as PASSED — the resolution's drafter/creator may not be
	// the principal who closes it. (RecordVotes only tallies aggregate
	// vote counts and does not finalize status, so the doctrine's
	// self-approval check belongs here, not there.)
	if existing.CreatedBy == principalID {
		writeError(w, http.StatusForbidden, domain.ErrSelfApprovalNotAllowed.Error())
		return
	}

	// No finalization path may skip required evidence states
	// (03-microservices.md §8.6). domain_code is the resolution's own
	// category (GOVERNANCE/FINANCIAL/OPERATIONAL/EXECUTIVE/STATUTORY) —
	// real data already on the record, not an invented dimension.
	//
	// correlationID must NOT default to the resolution's own ID:
	// evidence-requirements-svc's Evaluate is deliberately idempotent on
	// correlation_id (a genuine retry must replay, not re-evaluate) — falling
	// back to a fixed per-resolution value made every retry-after-attaching-
	// evidence permanently replay the FIRST attempt's result, even after
	// real evidence was attached. Caught live: a resolution blocked on the
	// first pass attempt stayed blocked forever on retry until this was
	// fixed. Each call without a caller-supplied X-Correlation-ID is a
	// distinct real-world attempt, not a retry — retries are exactly what
	// the header is for, and callers who want retry-safety must supply it.
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = "resolution-pass-" + uuid.New().String()
	}
	var artifacts []evidencereq.Artifact
	if req.DocumentVaultID != nil && *req.DocumentVaultID != "" {
		artifacts = append(artifacts, evidencereq.Artifact{
			EvidenceType: "SUPPORTING_DOCUMENT",
			ReferenceID:  *req.DocumentVaultID,
		})
	}
	if err := h.evidenceReq.EvaluateSufficient(r.Context(), tenantID, existing.LegalEntityID,
		string(existing.Category), actionResolutionPass, correlationID, principalID, artifacts); err != nil {
		h.writeEvidenceErr(w, err)
		return
	}

	res, err := h.store.PassResolution(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrResolutionNotFound):
			writeError(w, http.StatusNotFound, "resolution not found")
		case errors.Is(err, domain.ErrResolutionAlreadyFinalized):
			writeError(w, http.StatusConflict, "resolution is already finalized")
		default:
			writeError(w, http.StatusInternalServerError, "failed to pass resolution")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "resolution.passed", id, tenantID, res)
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
