package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
)

// ZS-SVC-Y-001 NCD-02 API (section 5.5): recipient convenience preferences.
//
//	GET  /v1/preferences    the caller's own profile
//	POST /v1/preferences    replace the caller's own profile
//
// "Governed user preference change; cannot mutate PRV consent or PDC rule." Governed here
// means a person changes only their own profile: the principal comes from the
// authenticated request, never from the body, so nobody can mute or schedule another
// person's notices. A preference never creates permission to send and never delays a
// security or transactional notice; it is applied by the delivery guard, not here.

// PreferenceStore is the preference persistence boundary.
type PreferenceStore interface {
	GetPreferences(ctx context.Context, principalID string) (*domain.RecipientPreferences, error)
	SetPreferences(ctx context.Context, p domain.SetPreferencesParams) (*domain.RecipientPreferences, error)
}

func registerPreferenceRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/preferences", func(r chi.Router) {
		r.Get("/", h.GetMyPreferences)
		r.Post("/", h.SetMyPreferences)
	})
}

func (h *Handler) preferenceGate(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.preferences == nil {
		writeError(w, http.StatusServiceUnavailable, "preferences_unavailable", "recipient preferences are not configured")
		return "", false
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return "", false
	}
	if _, ok = h.requireTenant(w, r); !ok {
		return "", false
	}
	return principalID, true
}

// GetMyPreferences returns the caller's profile, or 404 when they have set none.
func (h *Handler) GetMyPreferences(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.preferenceGate(w, r)
	if !ok {
		return
	}
	p, err := h.preferences.GetPreferences(r.Context(), principalID)
	if errors.Is(err, domain.ErrPreferencesNotFound) {
		writeError(w, http.StatusNotFound, "preferences_not_found", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to read preferences", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type setPreferencesRequest struct {
	TimeZone      string   `json:"time_zone"`
	QuietStart    *string  `json:"quiet_start"`
	QuietEnd      *string  `json:"quiet_end"`
	MutedChannels []string `json:"muted_channels"`
}

// SetMyPreferences replaces the caller's profile.
func (h *Handler) SetMyPreferences(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.preferenceGate(w, r)
	if !ok {
		return
	}
	var req setPreferencesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := h.preferences.SetPreferences(r.Context(), domain.SetPreferencesParams{PrincipalID: principalID,
		TimeZone: req.TimeZone, QuietStart: req.QuietStart, QuietEnd: req.QuietEnd, MutedChannels: req.MutedChannels, UpdatedBy: principalID})
	if errors.Is(err, domain.ErrPreferencesInvalid) {
		writeError(w, http.StatusBadRequest, "preferences_invalid", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to store preferences", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}
