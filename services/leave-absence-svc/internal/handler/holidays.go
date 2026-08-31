package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/leave-absence-svc/internal/domain"
)

var allowedHolidayTypes = map[string]bool{
	"PUBLIC":   true,
	"COMPANY":  true,
	"OPTIONAL": true,
}

// ── POST /v1/leave/holidays ────────────────────────────────────────────────────

func (h *Handler) CreateHoliday(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateHolidayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.Name == "" || req.Date == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, name, date are required")
		return
	}

	if _, err := time.Parse(dateLayout, req.Date); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_date", string(domain.ErrInvalidDate))
		return
	}

	holidayType := req.HolidayType
	if holidayType == "" {
		holidayType = "PUBLIC"
	}
	if !allowedHolidayTypes[holidayType] {
		writeError(w, http.StatusBadRequest, "invalid_holiday_type", string(domain.ErrInvalidHolidayType))
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionHolidayCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	now := time.Now().UTC()
	holiday := &domain.Holiday{
		HolidayID:     uuid.NewString(),
		LegalEntityID: req.LegalEntityID,
		Name:          req.Name,
		Date:          req.Date,
		HolidayType:   holidayType,
		IsRecurring:   req.IsRecurring,
		Description:   req.Description,
		Status:        "ACTIVE",
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := h.store.CreateHoliday(r.Context(), holiday); errors.Is(err, domain.ErrHolidayDateExists) {
		writeError(w, http.StatusConflict, "holiday_date_exists", err.Error())
		return
	} else if err != nil {
		h.log.Error("failed to create holiday", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, holiday)
}

// ── GET /v1/leave/holidays ─────────────────────────────────────────────────────

func (h *Handler) ListHolidays(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := domain.HolidayFilter{
		LegalEntityID:   q.Get("legal_entity_id"),
		From:            q.Get("from"),
		To:              q.Get("to"),
		IncludeInactive: q.Get("include_inactive") == "true",
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// The calendar is entity-scoped, and so is the authorization decision:
	// without an entity there is nothing to authorize the read against.
	if filter.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}

	for _, d := range []string{filter.From, filter.To} {
		if d == "" {
			continue
		}
		if _, err := time.Parse(dateLayout, d); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_date", string(domain.ErrInvalidDate))
			return
		}
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, filter.LegalEntityID, actionHolidayView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListHolidays(r.Context(), filter)
	if err != nil {
		h.log.Error("failed to list holidays", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.Holiday{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── DELETE /v1/leave/holidays/{id} ─────────────────────────────────────────────
//
// Retires a calendar entry rather than deleting it. Leave already approved
// against the old calendar must stay explicable.

func (h *Handler) DeactivateHoliday(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// Read first: the authorization decision is scoped to the legal entity that
	// owns the holiday, which only the stored row can tell us.
	holiday, err := h.store.GetHoliday(r.Context(), id)
	if errors.Is(err, domain.ErrHolidayNotFound) {
		writeError(w, http.StatusNotFound, "holiday_not_found", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to fetch holiday", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, holiday.LegalEntityID, actionHolidayDeactivate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.DeactivateHoliday(r.Context(), id); errors.Is(err, domain.ErrHolidayNotFound) {
		// Already inactive: the caller asked for a state the row is in.
		writeError(w, http.StatusNotFound, "holiday_not_found", err.Error())
		return
	} else if err != nil {
		h.log.Error("failed to deactivate holiday", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	holiday.Status = "INACTIVE"
	holiday.UpdatedAt = time.Now().UTC()
	writeJSON(w, http.StatusOK, holiday)
}
