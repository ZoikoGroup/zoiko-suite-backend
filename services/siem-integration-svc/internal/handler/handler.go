package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	authzpkg "zoiko.io/siem-integration-svc/internal/authz"
	"zoiko.io/siem-integration-svc/internal/domain"
	svcenvelope "zoiko.io/siem-integration-svc/internal/envelope"
	"zoiko.io/siem-integration-svc/internal/store"
)

// requireTenant returns the gateway-verified tenant, or refuses the
// request. It replaces a getTenant helper that substituted the literal
// string "default-tenant" when X-Tenant-ID was absent.
//
// A missing check makes a request fail; a fabricated tenant makes it
// SUCCEED, into one synthetic bucket shared by every header-less caller.
// The store is correctly scoped, so it enforced that shared bucket
// faithfully — and here the shared bucket held the worst payload of the
// three security services in this tier. A SIEMExporter carries
// endpoint_url AND auth_token, the token is stored as supplied and is
// never redacted on read, so header-less callers could read each other's
// live SIEM destination credential. They also shared ListEvents, i.e. each
// other's security event stream — the pipeline that exists to detect this
// kind of thing in the first place.
func requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := r.Header.Get("X-Tenant-ID")
	if tenantID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "missing_tenant_scope",
			"message": "X-Tenant-ID is required — the gateway sets it from a verified identity envelope",
		})
		return "", false
	}
	return tenantID, true
}

// Action constants passed to authorization-svc as action_type.
const (
	SiemExporterCreate = "SIEM_EXPORTER_CREATE"
	SiemExporterRead   = "SIEM_EXPORTER_READ"
	SiemEventRead      = "SIEM_EVENT_READ"
)

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store  store.Store
	authz  AuthzChecker
	logger *zap.Logger
}

func New(s store.Store, az AuthzChecker, l *zap.Logger) *Handler {
	return &Handler{store: s, authz: az, logger: l}
}

// ── Which routes are authorized, and which deliberately are not ─────────
//
// Three of this service's five routes now require a verified principal and
// an authorization decision. TWO DO NOT, and that asymmetry is deliberate,
// verified, and load-bearing — do not "finish the job" by adding the guard
// to the other two without reading this.
//
// POST /v1/siem/stream and GET /v1/siem/exporters are called by FIVE other
// services (authorization-svc, gateway-auth-svc, identity-context-svc,
// key-management-svc, mtls-management-svc) through their internal/siem
// clients. Every one of those clients sends X-Tenant-ID and NOTHING else —
// no X-Principal-Id. Adding requirePrincipal to either route would return
// 401 to all five and silently stop security-event streaming across the
// platform. The clients log a warning and move on, so it would not even
// fail loudly.
//
// It is also category-wrong, not merely inconvenient. gateway-auth-svc
// streams an authentication-FAILURE event: by definition there is no
// authenticated principal to forward. A user-principal check cannot be the
// right control for an endpoint whose whole job is to record that
// authentication did not happen.
//
// The correct control for those two routes is a SERVICE identity — mutual
// TLS, which is exactly what mtls-management-svc exists to issue — not a
// user principal. Until that is wired, they remain tenant-scoped only, and
// the residual exposure is logged as its own tracker row rather than
// papered over: a caller holding a tenant header can inject fabricated
// security events, and can enumerate exporter names and endpoint URLs.
// Event injection into a SIEM feed is a real concern (it is how an attacker
// buries a trail), so this is a gap being named, not dismissed.
//
// What that residual exposure no longer includes is the credential itself:
// domain.SIEMExporter.AuthToken is now json:"-", so the list route cannot
// serialise it regardless of who calls it. That was the actual leak.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		h.errJSON(w, http.StatusUnauthorized,
			"X-Principal-Id is required — the gateway sets it from a verified identity envelope")
		return "", false
	}
	return principalID, true
}

// authorize asks authorization-svc whether this principal may perform
// actionType within legalEntityID, and fails CLOSED.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			h.errJSON(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.logger.Error("authorization check failed", zap.Error(err))
		h.errJSON(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, chimw.Logger, chimw.Recoverer)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer so a refusal is still traced, and ahead of every handler so no
	// request reaches business logic without a resolved tenant, actor,
	// correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	//
	// POST /v1/siem/stream is exempt, and the exemption is the point of the
	// route. gateway-auth-svc calls it to report an AUTHENTICATION FAILURE, so
	// by definition there is no authenticated principal to put in an envelope.
	// Requiring one would silently drop exactly the events the SIEM exists to
	// receive. Same shape as carta-svc's /evaluate and identity-context-svc's
	// /v1/context/resolve. TestServiceToServiceRoutesStillWork pins it.
	envelopePolicy := svcenvelope.ServicePolicy()
	envelopePolicy.ExemptPaths = []string{"/v1/siem/stream"}
	r.Use(svcenvelope.Middleware(envelopePolicy, svcenvelope.DefaultReporter()))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "siem-integration-svc"})
	})
	r.Route("/v1/siem", func(r chi.Router) {
		r.Post("/exporters", h.CreateExporter)
		r.Get("/exporters", h.ListExporters)
		r.Get("/exporters/{id}", h.GetExporter)
		r.Post("/stream", h.StreamEvent)
		r.Get("/events", h.ListEvents)
	})
	return r
}

func (h *Handler) CreateExporter(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	var req domain.CreateExporterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, 400, "invalid body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, 400, err.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, SiemExporterCreate) {
		return
	}
	exp := &domain.SIEMExporter{
		LegalEntityID: req.LegalEntityID,
		Name:          req.Name,
		Platform:      req.Platform,
		EndpointURL:   req.EndpointURL,
		AuthToken:     req.AuthToken,
	}
	if err := h.store.CreateExporter(r.Context(), tenantID, exp); err != nil {
		h.errJSON(w, 500, "failed to create exporter")
		return
	}
	h.okJSON(w, 201, exp)
}

func (h *Handler) GetExporter(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	exp, err := h.store.GetExporterByID(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.errJSON(w, 404, "exporter not found")
		return
	}
	// Authorized against the exporter row's own legal entity, never a
	// caller-supplied scope. Unlike the list route below, nothing
	// service-to-service calls this one, so it can carry the full guard.
	if !h.authorize(w, r, principalID, exp.LegalEntityID, SiemExporterRead) {
		return
	}
	h.okJSON(w, 200, exp)
}

func (h *Handler) ListExporters(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	exps, _ := h.store.ListExporters(r.Context(), tenantID, r.URL.Query().Get("legal_entity_id"))
	if exps == nil {
		exps = []domain.SIEMExporter{}
	}
	h.okJSON(w, 200, map[string]interface{}{"data": exps, "count": len(exps)})
}

func (h *Handler) StreamEvent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	var req domain.StreamEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, 400, "invalid body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, 400, err.Error())
		return
	}
	evt := &domain.SIEMEvent{
		ExporterID: req.ExporterID,
		SourceSvc:  req.SourceSvc,
		EventType:  req.EventType,
		Severity:   req.Severity,
		Message:    req.Message,
		Payload:    req.Payload,
	}
	if err := h.store.StreamEvent(r.Context(), tenantID, evt); err != nil {
		h.errJSON(w, 404, err.Error())
		return
	}
	h.okJSON(w, 201, evt)
}

func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// exporter_id is now REQUIRED. It was an optional filter, but it is the
	// only thing that identifies a scope to authorize against — and an
	// optional scope parameter is a scope that disables itself, the same
	// shape as the self-disabling filters found across the connector
	// services. Omitting it previously returned every security event in the
	// tenant.
	//
	// Reading this stream is a privileged action in its own right: the
	// events describe authentication failures, certificate revocations and
	// key operations across the tenant, which is a map of where its
	// defences are weakest.
	exporterID := r.URL.Query().Get("exporter_id")
	if exporterID == "" {
		h.errJSON(w, http.StatusBadRequest,
			"exporter_id query parameter is required — it identifies the authorization scope for this listing")
		return
	}
	exp, err := h.store.GetExporterByID(r.Context(), tenantID, exporterID)
	if err != nil {
		h.errJSON(w, 404, "exporter not found")
		return
	}
	if !h.authorize(w, r, principalID, exp.LegalEntityID, SiemEventRead) {
		return
	}
	evts, _ := h.store.ListEvents(r.Context(), tenantID, exporterID)
	if evts == nil {
		evts = []domain.SIEMEvent{}
	}
	h.okJSON(w, 200, map[string]interface{}{"data": evts, "count": len(evts)})
}

func (h *Handler) okJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (h *Handler) errJSON(w http.ResponseWriter, code int, msg string) {
	h.okJSON(w, code, map[string]string{"error": msg})
}
