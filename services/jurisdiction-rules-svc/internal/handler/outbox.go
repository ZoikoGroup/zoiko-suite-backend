package handler

// Parked outbox events (Governance audit J-16). After MaxOutboxAttempts failed
// publishes the relay parks an event so the events behind it keep flowing; it
// stays in event_outbox until an operator, having fixed the cause, requeues it.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// OutboxRecoveryStore lists and requeues parked events (store.PgStore).
type OutboxRecoveryStore interface {
	ListParkedOutboxEvents(ctx context.Context, limit, offset int) ([]*store.ParkedOutboxEvent, error)
	RequeueOutboxEvent(ctx context.Context, outboxID, reason, evidenceRef, actor string) (*store.OutboxRequeue, error)
}

// EventOutboxEventRequeued is published when a parked event is put back.
const EventOutboxEventRequeued = "jurisdiction-outbox.event-requeued"

// WithOutboxRecovery mounts the parked-event routes.
func (h *Handler) WithOutboxRecovery(s OutboxRecoveryStore) *Handler {
	h.outbox = s
	return h
}

func registerOutboxRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/admin/outbox/parked", h.ListParkedOutboxEvents)
	r.Post("/v1/admin/outbox/{outbox_id}/requeue", h.RequeueOutboxEvent)
}

// ListParkedOutboxEvents handles GET /v1/admin/outbox/parked: the parked
// events, oldest first, each with its last error and earlier requeues.
func (h *Handler) ListParkedOutboxEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "jurisdiction_outbox_event", "view"); !ok {
		return
	}
	p, ok := h.beginList(w, r, nil, nil)
	if !ok {
		return
	}
	limit := p.fetch()
	if limit <= 0 {
		limit = 50
	}
	items, err := h.outbox.ListParkedOutboxEvents(r.Context(), limit, p.offset)
	if err != nil {
		h.writeRegistryError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	finishList(h, w, r, p, items)
}

type requeueRequest struct {
	Reason      string `json:"reason"`
	EvidenceRef string `json:"evidence_ref"`
}

// RequeueOutboxEvent handles POST /v1/admin/outbox/{outbox_id}/requeue: a
// parked event gets its attempts reset so the relay publishes it again. A
// reason is required; the requeue is recorded append-only and announced.
//
//	200 → the recorded requeue
//	400 → reason missing
//	404 → no such outbox event
//	409 → the event is not parked (published, or still being retried)
func (h *Handler) RequeueOutboxEvent(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_outbox_event", "requeue")
	if !ok {
		return
	}
	var req requeueRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeMissingField(w, r, "reason")
		return
	}
	id := chi.URLParam(r, "outbox_id")
	x, err := h.outbox.RequeueOutboxEvent(r.Context(), id, req.Reason, strings.TrimSpace(req.EvidenceRef), actor)
	switch {
	case errors.Is(err, store.ErrOutboxEventNotFound):
		writeError(w, r, http.StatusNotFound, "outbox_event_not_found", err.Error())
		return
	case errors.Is(err, store.ErrOutboxEventNotParked):
		writeError(w, r, http.StatusConflict, "outbox_event_not_parked", err.Error())
		return
	case err != nil:
		h.writeRegistryError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	h.emitRegistry(r, EventOutboxEventRequeued, x.OutboxID, actor, map[string]any{
		"requeue_id": x.RequeueID, "outbox_id": x.OutboxID, "event_type": x.EventType,
		"attempts_before": x.AttemptsBefore, "reason": x.Reason, "evidence_ref": x.EvidenceRef,
	})
	writeJSON(w, http.StatusOK, x)
}
