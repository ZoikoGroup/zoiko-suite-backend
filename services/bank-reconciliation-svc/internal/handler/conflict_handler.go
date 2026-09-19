package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
)

// ── POST /v1/evidence-conflicts ───────────────────────────────────────────────

// RaiseConflict creates an evidence conflict record. Idempotent on
// (statement_line_id, payment_id): if an OPEN conflict already exists for
// the same pair, the existing record is returned with 200.
func (h *Handler) RaiseConflict(w http.ResponseWriter, r *http.Request) {
	var req domain.RaiseEvidenceConflictRequest
	if !decodeBody(w, r, &req) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionConflictResolve); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if req.StatementLineID == "" || req.PaymentID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "statement_line_id, payment_id are required")
		return
	}
	conflict, created, err := h.store.RaiseEvidenceConflict(r.Context(), tenantID, req)
	if err != nil {
		h.writeStoreErr(w, "RaiseEvidenceConflict", err)
		return
	}

	h.publisher.PublishEvidenceConflictRaised(r.Context(), *conflict)

	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, conflict)
}

// ── GET /v1/evidence-conflicts ────────────────────────────────────────────────

func (h *Handler) ListConflicts(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	conflicts, err := h.store.ListOpenConflicts(r.Context(), tenantID, limit)
	if err != nil {
		h.writeStoreErr(w, "ListOpenConflicts", err)
		return
	}
	writeJSON(w, http.StatusOK, conflicts)
}

// ── GET /v1/evidence-conflicts/{conflict_id} ──────────────────────────────────

func (h *Handler) GetConflict(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	conflictID := chi.URLParam(r, "conflict_id")
	conflict, err := h.store.GetEvidenceConflict(r.Context(), tenantID, conflictID)
	if err != nil {
		if errors.Is(err, domain.ErrConflictNotFound) {
			writeError(w, http.StatusNotFound, "conflict_not_found", "")
			return
		}
		h.writeStoreErr(w, "GetEvidenceConflict", err)
		return
	}
	writeJSON(w, http.StatusOK, conflict)
}

// ── POST /v1/evidence-conflicts/{conflict_id}/resolve ─────────────────────────

func (h *Handler) ResolveConflict(w http.ResponseWriter, r *http.Request) {
	var req domain.ResolveEvidenceConflictRequest
	if !decodeBody(w, r, &req) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	conflictID := chi.URLParam(r, "conflict_id")

	// Load conflict for authz against legal entity.
	existing, err := h.store.GetEvidenceConflict(r.Context(), tenantID, conflictID)
	if err != nil {
		if errors.Is(err, domain.ErrConflictNotFound) {
			writeError(w, http.StatusNotFound, "conflict_not_found", "")
			return
		}
		h.writeStoreErr(w, "ResolveConflict/Get", err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionConflictResolve); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	conflict, err := h.store.ResolveEvidenceConflict(r.Context(), tenantID, conflictID, principalID, req.ResolutionNote)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflictNotFound):
			writeError(w, http.StatusNotFound, "conflict_not_found", "")
		case errors.Is(err, domain.ErrConflictAlreadyResolved):
			writeError(w, http.StatusConflict, "conflict_already_resolved", err.Error())
		default:
			h.writeStoreErr(w, "ResolveEvidenceConflict", err)
		}
		return
	}

	h.publisher.PublishEvidenceConflictResolved(r.Context(), *conflict)
	writeJSON(w, http.StatusOK, conflict)
}
