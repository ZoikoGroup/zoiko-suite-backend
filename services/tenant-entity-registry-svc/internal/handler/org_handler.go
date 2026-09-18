package handler

// HTTP surface for the ORG-02 (Tenant) and ORG-03 (Legal Entity) named
// commands and read surfaces.
//
// ROUTE SHAPE. Lifecycle commands are POST /v1/tenants/{id}/commands/{command}
// rather than one endpoint per command. Both satisfy §4.2's DoD gate ("no
// generic path bypasses named governance commands") because the command is
// named in the URL and refused if it is not one of ORG-02's; the path form was
// chosen because adding ORG-02's next named command should not require editing
// the router, and because a gateway policy can allow or deny a single command
// by path prefix without a rule per verb.
//
// The distinction that matters is not one URL per command but whether the
// command NAME reaches the evidence record. It does: it is written to
// tenant_lifecycle_history.command_name and carried in the event payload.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// ORGService is the ORG-02/ORG-03 half of the service contract, kept separate
// from Service for the same reason ORGStore is kept separate from Store.
type ORGService interface {
	// ORG-02
	ExecuteTenantCommand(ctx context.Context, tenantID string, command domain.TenantCommand, req domain.ExecuteTenantCommandRequest) (*registry.TenantCommandResult, error)
	ChangeDefaultLocale(ctx context.Context, tenantID string, req domain.ChangeDefaultLocaleRequest) (*domain.Tenant, error)
	ListTenantLifecycleHistory(ctx context.Context, tenantID string) ([]*domain.TenantLifecycleEvent, error)
	GetTenantDefaults(ctx context.Context, tenantID string) (*domain.TenantDefaults, error)
	BindTenantHost(ctx context.Context, tenantID string, req domain.BindTenantHostRequest) (*domain.TenantHostBinding, error)
	ListTenantHostBindings(ctx context.Context, tenantID string) ([]*domain.TenantHostBinding, error)
	ResolveTenantByHost(ctx context.Context, hostname string) (*domain.ResolvedTenantByHost, error)
	VerifyHostTenant(ctx context.Context, hostname, claimedTenantID string) error

	// ORG-03
	AmendLegalProfile(ctx context.Context, legalEntityID string, req domain.AmendLegalProfileRequest) (*domain.LegalEntityProfileVersion, error)
	ChangeLegalName(ctx context.Context, legalEntityID string, req domain.ChangeLegalNameRequest) (*domain.LegalEntityProfileVersion, error)
	ChangeRegisteredOffice(ctx context.Context, legalEntityID string, req domain.ChangeRegisteredOfficeRequest) (*domain.LegalEntityProfileVersion, error)
	ListEntityVersions(ctx context.Context, legalEntityID string) ([]*domain.LegalEntityProfileVersion, error)
	GetLegalEntityAsOf(ctx context.Context, legalEntityID string, asOf time.Time) (*domain.EntityAsOf, error)
	FindByRegistryNumber(ctx context.Context, registrationNumber, jurisdictionID string) ([]*domain.LegalEntity, error)
	ListRegistryConflicts(ctx context.Context, openOnly bool) ([]*domain.EntityRegistryConflict, error)
	ResolveRegistryConflict(ctx context.Context, conflictID string, req domain.ResolveRegistryConflictRequest) error
}

// registerORGRoutes mounts the ORG-02/ORG-03 endpoints.
//
// Takes the router ALREADY scoped to /v1 and is called from inside
// RegisterRoutes' own r.Route("/v1", ...) block. It must not open a second
// /v1 group of its own: chi mounts a Route() subrouter at that pattern, and
// mounting twice on one path panics at startup.
//
// Static segments (by-registry-number) coexist with the parameterised ones
// ({entityID}) because chi's trie matches static nodes ahead of parameter
// nodes at the same depth — within one routing group.
func registerORGRoutes(r chi.Router, h *Handler) {
	{
		// ── ORG-02: tenant named commands ───────────────────────────────────
		r.Post("/tenants/{tenantID}/commands/{command}", h.ExecuteTenantCommand)
		r.Post("/tenants/{tenantID}/defaults", h.ChangeDefaultLocale)

		// ── ORG-02: tenant read surfaces ────────────────────────────────────
		r.Get("/tenants/{tenantID}/lifecycle-history", h.ListTenantLifecycleHistory)
		r.Get("/tenants/{tenantID}/defaults", h.GetTenantDefaults)

		// ── ORG-02: host bindings ───────────────────────────────────────────
		r.Post("/tenants/{tenantID}/host-bindings", h.BindTenantHost)
		r.Get("/tenants/{tenantID}/host-bindings", h.ListTenantHostBindings)
		// Unauthenticated by design — this is how a caller learns its tenant.
		r.Get("/resolve-tenant", h.ResolveTenantByHost)

		// ── ORG-03: profile commands ────────────────────────────────────────
		r.Post("/entities/{entityID}/profile-amendments", h.AmendLegalProfile)
		r.Post("/entities/{entityID}/legal-name", h.ChangeLegalName)
		r.Post("/entities/{entityID}/registered-office", h.ChangeRegisteredOffice)

		// ── ORG-03: as-of and version reads ─────────────────────────────────
		r.Get("/entities/{entityID}/versions", h.ListEntityVersions)
		r.Get("/entities/{entityID}/as-of", h.GetLegalEntityAsOf)
		r.Get("/entities/by-registry-number", h.FindByRegistryNumber)

		// ── ORG-03: registry conflict quarantine ────────────────────────────
		r.Get("/registry-conflicts", h.ListRegistryConflicts)
		r.Post("/registry-conflicts/{conflictID}/resolution", h.ResolveRegistryConflict)
	}
}

// ---------------------------------------------------------------------------
// ORG-02 handlers
// ---------------------------------------------------------------------------

// ExecuteTenantCommand applies one ORG-02 named lifecycle command.
func (h *Handler) ExecuteTenantCommand(w http.ResponseWriter, r *http.Request) {
	var req domain.ExecuteTenantCommandRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}

	tenantID := chi.URLParam(r, "tenantID")

	// §8 NP3, applied before anything reads data: if the hostname this request
	// arrived on is bound to a different tenant, refuse. Deliberately at the
	// edge rather than in the service, because "before data access" is a
	// property of WHERE the check runs, not just that it runs.
	if err := h.svc.VerifyHostTenant(r.Context(), r.Host, tenantID); err != nil {
		h.writeErr(w, r, err)
		return
	}

	command := domain.TenantCommand(chi.URLParam(r, "command"))
	res, err := h.svc.ExecuteTenantCommand(r.Context(), tenantID, command, req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ChangeDefaultLocale applies the ORG-02 command of the same name.
func (h *Handler) ChangeDefaultLocale(w http.ResponseWriter, r *http.Request) {
	var req domain.ChangeDefaultLocaleRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	tenantID := chi.URLParam(r, "tenantID")
	if err := h.svc.VerifyHostTenant(r.Context(), r.Host, tenantID); err != nil {
		h.writeErr(w, r, err)
		return
	}
	t, err := h.svc.ChangeDefaultLocale(r.Context(), tenantID, req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// ListTenantLifecycleHistory is the ORG-02 read surface of the same name.
func (h *Handler) ListTenantLifecycleHistory(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListTenantLifecycleHistory(r.Context(), chi.URLParam(r, "tenantID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetTenantDefaults is the ORG-02 read surface of the same name.
func (h *Handler) GetTenantDefaults(w http.ResponseWriter, r *http.Request) {
	d, err := h.svc.GetTenantDefaults(r.Context(), chi.URLParam(r, "tenantID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// BindTenantHost maps a hostname to a tenant.
func (h *Handler) BindTenantHost(w http.ResponseWriter, r *http.Request) {
	var req domain.BindTenantHostRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	b, err := h.svc.BindTenantHost(r.Context(), chi.URLParam(r, "tenantID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

// ListTenantHostBindings returns a tenant's hostnames.
func (h *Handler) ListTenantHostBindings(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListTenantHostBindings(r.Context(), chi.URLParam(r, "tenantID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ResolveTenantByHost is the ORG-02 read surface of the same name.
//
// Takes the hostname from ?hostname= rather than from the Host header, because
// the caller is typically resolving a hostname OTHER than the one it connected
// on — an ingress layer asking about the host in an inbound request it is
// deciding how to route.
func (h *Handler) ResolveTenantByHost(w http.ResponseWriter, r *http.Request) {
	hostname := r.URL.Query().Get("hostname")
	if hostname == "" {
		hostname = r.Host
	}
	res, err := h.svc.ResolveTenantByHost(r.Context(), hostname)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------------------
// ORG-03 handlers
// ---------------------------------------------------------------------------

// AmendLegalProfile creates the next effective-dated profile version.
func (h *Handler) AmendLegalProfile(w http.ResponseWriter, r *http.Request) {
	var req domain.AmendLegalProfileRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	v, err := h.svc.AmendLegalProfile(r.Context(), chi.URLParam(r, "entityID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// ChangeLegalName applies the ORG-03 command of the same name.
func (h *Handler) ChangeLegalName(w http.ResponseWriter, r *http.Request) {
	var req domain.ChangeLegalNameRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	v, err := h.svc.ChangeLegalName(r.Context(), chi.URLParam(r, "entityID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// ChangeRegisteredOffice applies the ORG-03 command of the same name.
func (h *Handler) ChangeRegisteredOffice(w http.ResponseWriter, r *http.Request) {
	var req domain.ChangeRegisteredOfficeRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	v, err := h.svc.ChangeRegisteredOffice(r.Context(), chi.URLParam(r, "entityID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// ListEntityVersions is the ORG-03 read surface of the same name.
func (h *Handler) ListEntityVersions(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListEntityVersions(r.Context(), chi.URLParam(r, "entityID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetLegalEntityAsOf is the ORG-03 read surface of the same name.
//
// ?as_of= accepts RFC3339 or a bare date. A bare date is the common case for a
// financial reconstruction ("as the books stood on 2026-03-31") and rejecting
// it would push every caller into building a timestamp, which is where
// timezone mistakes come from. A bare date is read as midnight UTC.
func (h *Handler) GetLegalEntityAsOf(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("as_of"))
	asOf := time.Now().UTC()
	if raw != "" {
		parsed, err := parseAsOf(raw)
		if err != nil {
			writeErrJSON(w, http.StatusBadRequest,
				"as_of must be an RFC3339 timestamp or a YYYY-MM-DD date", correlationID(r))
			return
		}
		asOf = parsed
	}

	out, err := h.svc.GetLegalEntityAsOf(r.Context(), chi.URLParam(r, "entityID"), asOf)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// parseAsOf accepts RFC3339 or YYYY-MM-DD.
func parseAsOf(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// FindByRegistryNumber is the ORG-03 read surface of the same name.
func (h *Handler) FindByRegistryNumber(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.FindByRegistryNumber(r.Context(),
		r.URL.Query().Get("registration_number"),
		r.URL.Query().Get("jurisdiction_id"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ListRegistryConflicts returns quarantined registry conflicts.
//
// Defaults to open conflicts only: the reason to look at this endpoint is
// almost always "what needs resolving", and a caller wanting the history can
// ask with ?status=all.
func (h *Handler) ListRegistryConflicts(w http.ResponseWriter, r *http.Request) {
	openOnly := !strings.EqualFold(r.URL.Query().Get("status"), "all")
	out, err := h.svc.ListRegistryConflicts(r.Context(), openOnly)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ResolveRegistryConflict records a human's conclusion about a conflict.
func (h *Handler) ResolveRegistryConflict(w http.ResponseWriter, r *http.Request) {
	var req domain.ResolveRegistryConflictRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	if err := h.svc.ResolveRegistryConflict(r.Context(), chi.URLParam(r, "conflictID"), req); err != nil {
		h.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeORGErr extends writeErr with the ORG-specific sentinels.
//
// Three statuses worth explaining:
//
//   - ErrTenantNotTransactable is 409, not 403. 403 means "this principal
//     lacks a grant" and sends an operator to the RBAC console, where they
//     will find nothing wrong. 409 says the resource is in a state that
//     conflicts with the request, which is exactly what a suspended tenant is.
//   - ErrRegistryConflict is 409 for the same reason, and its body names the
//     conflict id so the caller can go straight to the quarantine record.
//   - ErrApprovalRequired is 422: the request is well-formed and the caller is
//     permitted, but the operation cannot be carried out as submitted.
func mapORGError(err error) (int, string, bool) {
	switch {
	case errors.Is(err, registry.ErrTenantNotTransactable):
		return http.StatusConflict, err.Error(), true
	case errors.Is(err, registry.ErrRegistryConflict):
		return http.StatusConflict, err.Error(), true
	case errors.Is(err, registry.ErrApprovalRequired):
		return http.StatusUnprocessableEntity, err.Error(), true
	case errors.Is(err, registry.ErrHostTenantMismatch):
		// 403 and a body that does NOT echo which tenant the host resolves to:
		// telling a caller "this host belongs to tenant X" would turn the
		// refusal into a tenant-enumeration oracle.
		return http.StatusForbidden, "host/tenant mismatch: request refused before data access", true
	case errors.Is(err, registry.ErrUnknownCommand):
		return http.StatusNotFound, err.Error(), true
	}
	return 0, "", false
}
