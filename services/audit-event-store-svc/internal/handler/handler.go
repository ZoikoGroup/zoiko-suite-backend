// Package handler is AUD-10's archive/verify HTTP API — the first business
// HTTP endpoint set this service has ever exposed (everything before this
// was Kafka-consumer-only; see cmd/server/main.go's own package doc). It
// stays deliberately narrow: two write actions (create, verify) plus two
// reads, all operating on the hash chain this service already owns.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/domain"
	"zoiko.io/audit-event-store-svc/internal/envelope"
	"zoiko.io/audit-event-store-svc/internal/store"
)

type Handler struct {
	store store.ArchiveStore
	log   *zap.Logger
}

func New(s store.ArchiveStore, log *zap.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// RegisterRoutes mounts AUD-10's archive routes on r.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/archives", h.createArchive)
	r.Get("/v1/archives/{id}", h.getArchive)
	r.Post("/v1/archives/{id}/verify", h.verifyArchive)
	r.Get("/v1/archives/{id}/verifications", h.listVerifications)
}

// requireActor reads the acting principal already validated by the
// envelope middleware (ZS-ARCH-SVC-001 v2.0 §4 — every request reaching a
// handler has passed Policy.Validate, so actor_subject_id/workload_id is
// guaranteed present here; this just surfaces it for created_by/verified_by
// attribution rather than re-deriving it from a raw header).
func requireActor(r *http.Request) string {
	e, _ := envelope.FromContext(r.Context())
	return e.Actor()
}

type createArchiveRequest struct {
	FromSequence int64 `json:"from_sequence"`
	ToSequence   int64 `json:"to_sequence"`
}

func (h *Handler) createArchive(w http.ResponseWriter, r *http.Request) {
	var req createArchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	a, err := h.store.CreateArchive(r.Context(), domain.CreateArchiveParams{
		FromSequence:         req.FromSequence,
		ToSequence:           req.ToSequence,
		CreatedByPrincipalID: requireActor(r),
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) getArchive(w http.ResponseWriter, r *http.Request) {
	a, err := h.store.GetArchive(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) verifyArchive(w http.ResponseWriter, r *http.Request) {
	v, err := h.store.VerifyArchive(r.Context(), domain.VerifyArchiveParams{
		ArchiveID:             chi.URLParam(r, "id"),
		VerifiedByPrincipalID: requireActor(r),
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) listVerifications(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListVerifications(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrArchiveNotFound):
		writeError(w, http.StatusNotFound, "archive_not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidRange):
		writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
	case errors.Is(err, domain.ErrEmptyRange):
		writeError(w, http.StatusBadRequest, "empty_range", err.Error())
	case errors.Is(err, domain.ErrChainBroken):
		writeError(w, http.StatusConflict, "chain_broken", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}
