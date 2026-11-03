package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

func (h *KernelHandler) orchestration(w http.ResponseWriter) (*kernel.OrchestrationService, bool) {
	cr, ok := h.repo.(kernel.ConsequenceRepo)
	if !ok {
		writeError(w, http.StatusNotImplemented, "orchestration_not_configured", "")
		return nil, false
	}
	return &kernel.OrchestrationService{Repo: h.repo, Cons: cr}, true
}

// GetEventPostingStatus says how far one accounting event's consequences have come across the books of its entity (standard §4, §37):
// COMPLETE when every book that must carry it has posted, INCOMPLETE — naming the books still outstanding and why — until then.
func (h *KernelHandler) GetEventPostingStatus(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionPostingView)
	if !ok {
		return
	}
	svc, ok := h.orchestration(w)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	ev, err := h.repo.GetEvent(r.Context(), tenant, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if _, _, ok := h.guardFor(w, r, actionPostingView, ev.LegalEntityID); !ok {
		return
	}
	st, err := svc.Status(r.Context(), tenant, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ListIncompleteEvents lists the entity's recent accounting events that are not complete across their books. It looks at the most recent
// events (scan, at most 500) and skips those registered less than min_age_minutes ago, so an event still on its way is not reported.
func (h *KernelHandler) ListIncompleteEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entity := q.Get("legal_entity_id")
	if _, err := uuid.Parse(entity); err != nil {
		writeError(w, http.StatusBadRequest, "legal_entity_id_required", "name the legal entity")
		return
	}
	tenant, _, ok := h.guardFor(w, r, actionPostingView, entity)
	if !ok {
		return
	}
	svc, ok := h.orchestration(w)
	if !ok {
		return
	}
	num := func(key string, def int) (int, bool) {
		v := q.Get(key)
		if v == "" {
			return def, true
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_"+key, key+" is a non-negative number")
			return 0, false
		}
		return n, true
	}
	scan, ok1 := num("scan", 0)
	limit, ok2 := num("limit", 0)
	minAge, ok3 := num("min_age_minutes", 0)
	if !ok1 || !ok2 || !ok3 {
		return
	}
	res, err := svc.Incomplete(r.Context(), tenant, entity, scan, limit, time.Duration(minAge)*time.Minute)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
