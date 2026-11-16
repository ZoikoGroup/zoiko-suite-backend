package handler

// HTTP surface for ORG-09 registrations and licences.
//
//	POST /v1/registrations                          CreateRegistration (201, 200 replay); starts DRAFT, never active
//	GET  /v1/registrations?holder_...&type=&status=&jurisdiction=   ListRegistrations
//	GET  /v1/registrations/{id}?reveal=true         GetRegistration
//	GET  /v1/registrations/{id}/history             GetRegistrationHistory
//	POST /v1/registrations/{id}:verify              VerifyRegistration
//	POST /v1/registrations/{id}:renew               RenewRegistration
//	POST /v1/registrations/{id}:suspend             SuspendRegistration
//	POST /v1/registrations/{id}:expire              ExpireRegistration
//	POST /v1/registrations/{id}:revoke              RevokeRegistration
//	GET  /v1/registrations:find?number=&type=&authority=&jurisdiction=   FindByNumber
//	GET  /v1/registrations/expiring?within_days=30&as_of=                ListExpiringRegistrations
//
// Sensitive numbers are masked unless reveal=true and the caller holds the reveal permission. Mounted only when the
// service satisfies RegistrationService, which *registry.Service does.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// RegistrationService is the ORG-09 half of the service contract.
type RegistrationService interface {
	CreateRegistration(ctx context.Context, req domain.CreateRegistrationRequest) (*registry.RegistrationOutcome, error)
	VerifyRegistration(ctx context.Context, id string, req domain.VerifyRegistrationRequest) (*registry.RegistrationOutcome, error)
	RenewRegistration(ctx context.Context, id string, req domain.RenewRegistrationRequest) (*registry.RegistrationOutcome, error)
	ChangeRegistrationStatus(ctx context.Context, id string, to domain.RegistrationStatus, req domain.RegistrationEndRequest) (*registry.RegistrationOutcome, error)
	GetRegistration(ctx context.Context, id string, reveal bool) (*domain.Registration, error)
	GetRegistrationHistory(ctx context.Context, id string) ([]domain.RegistrationTransition, error)
	FindRegistrationsByNumber(ctx context.Context, number string, typ domain.RegistrationType, authority, jurisdiction string, reveal bool) ([]domain.Registration, error)
	ListRegistrations(ctx context.Context, q domain.RegistrationSearchQuery, reveal bool) ([]domain.Registration, string, error)
	ListExpiringRegistrations(ctx context.Context, withinDays int, asOf time.Time, reveal bool) ([]domain.Registration, error)
}

func registerRegistrationRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(RegistrationService); !ok {
		return
	}
	r.Post("/registrations", h.CreateRegistration)
	r.Get("/registrations", h.ListRegistrations)
	r.Get("/registrations:find", h.FindRegistrationsByNumber)
	r.Get("/registrations/expiring", h.ListExpiringRegistrations)
	r.Get("/registrations/{registrationID}", h.GetRegistration)
	r.Get("/registrations/{registrationID}/history", h.GetRegistrationHistory)
	r.Post("/registrations/{registrationID}:verify", h.VerifyRegistration)
	r.Post("/registrations/{registrationID}:renew", h.RenewRegistration)
	r.Post("/registrations/{registrationID}:suspend", h.registrationEnd(domain.RegSuspended))
	r.Post("/registrations/{registrationID}:expire", h.registrationEnd(domain.RegExpired))
	r.Post("/registrations/{registrationID}:revoke", h.registrationEnd(domain.RegRevoked))
}

func (h *Handler) registrations() RegistrationService {
	rs, _ := h.svc.(RegistrationService)
	return rs
}

// CreateRegistration records a DRAFT registration.
func (h *Handler) CreateRegistration(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRegistrationRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.registrations().CreateRegistration(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	status := http.StatusCreated
	if out.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// VerifyRegistration moves a registration to ACTIVE on evidence.
func (h *Handler) VerifyRegistration(w http.ResponseWriter, r *http.Request) {
	var req domain.VerifyRegistrationRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.registrations().VerifyRegistration(r.Context(), chi.URLParam(r, "registrationID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// RenewRegistration extends a registration's expiry on evidence.
func (h *Handler) RenewRegistration(w http.ResponseWriter, r *http.Request) {
	var req domain.RenewRegistrationRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.registrations().RenewRegistration(r.Context(), chi.URLParam(r, "registrationID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// registrationEnd is one named ending command: the route, not the body, names the target status.
func (h *Handler) registrationEnd(to domain.RegistrationStatus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req domain.RegistrationEndRequest
		if !decode(w, r, &req) {
			return
		}
		if req.CorrelationID == "" {
			req.CorrelationID = correlationID(r)
		}
		out, err := h.registrations().ChangeRegistrationStatus(r.Context(), chi.URLParam(r, "registrationID"), to, req)
		if err != nil {
			h.writeErr(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// GetRegistration returns a registration; the number is masked if sensitive unless revealed.
func (h *Handler) GetRegistration(w http.ResponseWriter, r *http.Request) {
	reg, err := h.registrations().GetRegistration(r.Context(), chi.URLParam(r, "registrationID"), r.URL.Query().Get("reveal") == "true")
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reg)
}

// GetRegistrationHistory returns every recorded step.
func (h *Handler) GetRegistrationHistory(w http.ResponseWriter, r *http.Request) {
	out, err := h.registrations().GetRegistrationHistory(r.Context(), chi.URLParam(r, "registrationID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// FindRegistrationsByNumber finds registrations by their normalized number.
func (h *Handler) FindRegistrationsByNumber(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := h.registrations().FindRegistrationsByNumber(r.Context(), q.Get("number"), domain.RegistrationType(q.Get("type")),
		q.Get("authority"), q.Get("jurisdiction"), q.Get("reveal") == "true")
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ListRegistrations lists registrations.
func (h *Handler) ListRegistrations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeErrJSON(w, r, http.StatusBadRequest, "limit must be an integer from 1 to 200", correlationID(r))
			return
		}
		limit = n
	}
	out, next, err := h.registrations().ListRegistrations(r.Context(), domain.RegistrationSearchQuery{
		HolderLegalEntityID: q.Get("holder_legal_entity_id"), HolderPartyID: q.Get("holder_party_id"),
		Type: domain.RegistrationType(q.Get("type")), Status: domain.RegistrationStatus(q.Get("status")),
		Jurisdiction: q.Get("jurisdiction"), Limit: limit, After: q.Get("cursor"),
	}, q.Get("reveal") == "true")
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_cursor": next})
}

// ListExpiringRegistrations lists registrations expiring within a window, and any already lapsed.
func (h *Handler) ListExpiringRegistrations(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	days := 30
	if raw := r.URL.Query().Get("within_days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 3650 {
			writeErrJSON(w, r, http.StatusBadRequest, "within_days must be an integer from 0 to 3650", correlationID(r))
			return
		}
		days = n
	}
	out, err := h.registrations().ListExpiringRegistrations(r.Context(), days, asOf, r.URL.Query().Get("reveal") == "true")
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// The production service offers registrations; if it ever stops, the routes would silently vanish.
var _ RegistrationService = (*registry.Service)(nil)
