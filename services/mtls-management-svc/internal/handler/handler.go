package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	authzpkg "zoiko.io/mtls-management-svc/internal/authz"
	"zoiko.io/mtls-management-svc/internal/ca"
	"zoiko.io/mtls-management-svc/internal/domain"
	"zoiko.io/mtls-management-svc/internal/siem"
	"zoiko.io/mtls-management-svc/internal/store"
)

type ctxKey string

const tenantKey ctxKey = "tenant_id"

// requireTenant returns the gateway-verified tenant, or refuses the
// request. It replaces a getTenant helper that substituted the literal
// string "default-tenant" when X-Tenant-ID was absent.
//
// A missing check makes a request fail; a fabricated tenant makes it
// SUCCEED, into one synthetic bucket shared by every header-less caller.
// The store is correctly scoped, so it enforced that shared bucket
// faithfully — meaning header-less callers could read each other's
// certificate records and, through RevokeCert and ReplaceCertMaterial,
// REVOKE or re-issue each other's mTLS certificates. Revoking a
// certificate breaks the service-to-service authentication that depends on
// it, so the fabricated identity turned a correct store into a shared
// control plane over the platform's mutual-TLS trust.
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
//
// Split by consequence, not by HTTP verb. Revocation gets its own action
// because it is the destructive one: revoking a certificate breaks the
// service-to-service authentication that depends on it, so it is closer to
// a kill switch than to an edit. Rotation is separated from provisioning
// for the same reason — re-issuing material for an existing service
// identity is a different privilege from minting a new one.
const (
	MtlsCertProvision = "MTLS_CERT_PROVISION"
	MtlsCertRead      = "MTLS_CERT_READ"
	MtlsCertRotate    = "MTLS_CERT_ROTATE"
	MtlsCertRevoke    = "MTLS_CERT_REVOKE"
	MtlsPolicyWrite   = "MTLS_POLICY_WRITE"
	MtlsPolicyRead    = "MTLS_POLICY_READ"
)

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store  store.Store
	ca     *ca.CA
	siem   *siem.Client
	authz  AuthzChecker
	logger *zap.Logger
}

func New(s store.Store, c *ca.CA, siemClient *siem.Client, az AuthzChecker, l *zap.Logger) *Handler {
	return &Handler{store: s, ca: c, siem: siemClient, authz: az, logger: l}
}

// requirePrincipal returns the gateway-verified principal, or refuses the
// request. This service had no notion of a calling principal at all before
// now — nothing read X-Principal-Id on any route.
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
// actionType within scopeID, and fails CLOSED.
//
// Priority 1b gave this service a verified TENANT. That closed the
// cross-tenant hole but left an intra-tenant one: any principal holding any
// valid envelope for the tenant could list every certificate in it, and
// revoke or rotate any of them. On a service that issues the platform's
// mutual-TLS material, "any principal in the tenant" is the wrong audience
// for a revoke button.
//
// ── On the scope argument, deliberately ────────────────────────────────
//
// This service passes TWO different identifiers into authorization-svc's
// single legal_entity_id parameter, and that needs justifying because
// tracker row 84a records exactly that pattern as a defect elsewhere.
//
// Certificate routes pass the certificate's legal_entity_id. Certificates
// carry one (domain.MtlsCertificate.LegalEntityID, required on issue), it is
// the natural granularity for "who may mint material for this entity", and
// it matches the parameter's own name.
//
// Policy routes pass the tenant id, because domain.CommunicationPolicy has
// no legal entity at all — a communication policy governs service-to-service
// traffic across the whole tenant. There is no finer scope to pass, and
// inventing one would be worse than naming the real one.
//
// The difference from row 84a is that this is two resource GRANULARITIES,
// each scoped at the level it actually exists at, and documented. Row 84a is
// one resource scoped inconsistently — organization_id on some routes and
// commercial_account_id on others for the same commercial account — where a
// grant recorded in one namespace can never match a check in the other.
// Whoever seeds grants for this service needs to know both facts, so this
// comment is the contract.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, scopeID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, scopeID, actionType); err != nil {
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
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "mtls-management-svc"})
	})
	r.Route("/v1/mtls", func(r chi.Router) {
		r.Post("/certificates", h.ProvisionCert)
		r.Get("/certificates", h.ListCerts)
		r.Get("/certificates/{id}", h.GetCert)
		r.Post("/certificates/{id}/rotate", h.RotateCert)
		r.Delete("/certificates/{id}", h.RevokeCert)
		r.Post("/policies", h.CreatePolicy)
		r.Get("/policies", h.ListPolicies)
	})
	return r
}

func (h *Handler) ProvisionCert(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	var req domain.ProvisionCertRequest
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
	if !h.authorize(w, r, principalID, req.LegalEntityID, MtlsCertProvision) {
		return
	}

	issued, err := h.ca.IssueLeaf(req.CommonName, []string{req.ServiceName}, time.Duration(req.RotationDays)*24*time.Hour)
	if err != nil {
		h.logger.Error("failed to issue leaf certificate", zap.String("common_name", req.CommonName), zap.Error(err))
		h.errJSON(w, 500, "failed to issue certificate")
		return
	}

	now := time.Now()
	cert := &domain.MtlsCertificate{
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		ServiceName:    req.ServiceName,
		CommonName:     req.CommonName,
		Issuer:         "ZoikoSuite Internal CA",
		SerialNumber:   issued.SerialNumber,
		Fingerprint:    issued.Fingerprint,
		CertificatePEM: string(issued.CertificatePEM),
		ValidFrom:      issued.NotBefore,
		ValidTo:        issued.NotAfter,
		RotationDays:   req.RotationDays,
		AutoRotate:     req.AutoRotate,
		Status:         domain.CertStatusActive,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := h.store.CreateCert(r.Context(), tenantID, cert); err != nil {
		h.errJSON(w, 500, "failed to provision certificate")
		return
	}

	// Doc 05 §13.2 names "certificate issuance/rotation events" as a
	// required SIEM signal.
	h.siem.Stream(r.Context(), tenantID, "certificate.issued", siem.SeverityLow,
		"Certificate issued for "+req.ServiceName+" ("+req.CommonName+")")

	h.okJSON(w, 201, domain.ProvisionCertResult{
		Certificate:   *cert,
		PrivateKeyPEM: string(issued.PrivateKeyPEM),
		CACertPEM:     string(h.ca.CertificatePEM()),
	})
}

func (h *Handler) GetCert(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	cert, err := h.store.GetCertByID(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.errJSON(w, 404, "certificate not found")
		return
	}
	// Authorized against the certificate row own legal entity, never a
	// caller-supplied scope: the store is already tenant-scoped, so a
	// foreign cert is a 404 here, and authorizing against something the
	// caller nominated would let them name an entity they do hold a grant
	// for and read a cert belonging to another.
	if !h.authorize(w, r, principalID, cert.LegalEntityID, MtlsCertRead) {
		return
	}
	h.okJSON(w, 200, cert)
}

// ListCerts now REQUIRES legal_entity_id rather than treating it as an
// optional filter.
//
// That is a deliberate API tightening, for the same reason the connector
// services' self-disabling filters were made mandatory: legal_entity_id is
// now the authorization scope, and an optional scope parameter is a scope
// that disables itself. Omitting it used to return every certificate in the
// tenant across all entities, which is precisely the request that has no
// single scope to authorize against.
func (h *Handler) ListCerts(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		h.errJSON(w, http.StatusBadRequest,
			"legal_entity_id query parameter is required — it is the authorization scope for this listing")
		return
	}
	if !h.authorize(w, r, principalID, legalEntityID, MtlsCertRead) {
		return
	}
	certs, _ := h.store.ListCerts(r.Context(), tenantID, legalEntityID, r.URL.Query().Get("status"))
	if certs == nil {
		certs = []domain.MtlsCertificate{}
	}
	h.okJSON(w, 200, map[string]interface{}{"data": certs, "count": len(certs)})
}

func (h *Handler) RotateCert(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetCertByID(r.Context(), tenantID, id)
	if err != nil {
		h.errJSON(w, 404, "certificate not found")
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, MtlsCertRotate) {
		return
	}

	// Rotation issues a genuinely new key pair and certificate signed by
	// the same CA — it is not a metadata refresh. The old private key
	// (which this service never retained anyway) becomes unusable the
	// moment the requesting service adopts the new one.
	issued, err := h.ca.IssueLeaf(existing.CommonName, []string{existing.ServiceName}, time.Duration(existing.RotationDays)*24*time.Hour)
	if err != nil {
		h.logger.Error("failed to issue rotated certificate", zap.String("id", id), zap.Error(err))
		h.errJSON(w, 500, "failed to rotate certificate")
		return
	}

	updated, err := h.store.ReplaceCertMaterial(r.Context(), tenantID, id,
		issued.SerialNumber, issued.Fingerprint, string(issued.CertificatePEM), issued.NotBefore, issued.NotAfter)
	if err != nil {
		h.errJSON(w, 404, "certificate not found")
		return
	}

	h.siem.Stream(r.Context(), tenantID, "certificate.rotated", siem.SeverityLow, "Certificate rotated: "+id)

	h.okJSON(w, 200, domain.ProvisionCertResult{
		Certificate:   *updated,
		PrivateKeyPEM: string(issued.PrivateKeyPEM),
		CACertPEM:     string(h.ca.CertificatePEM()),
	})
}

func (h *Handler) RevokeCert(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	// Read before revoking, purely to obtain the certificate's own legal
	// entity to authorize against. RevokeCert alone would tell us nothing
	// about who owns the row until after it had already acted on it, and
	// revocation is the destructive operation here: it breaks whatever
	// service-to-service authentication depends on this certificate.
	existing, err := h.store.GetCertByID(r.Context(), tenantID, id)
	if err != nil {
		h.errJSON(w, 404, "certificate not found")
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, MtlsCertRevoke) {
		return
	}

	if err := h.store.RevokeCert(r.Context(), tenantID, id); err != nil {
		h.errJSON(w, 404, "certificate not found")
		return
	}
	h.siem.Stream(r.Context(), tenantID, "certificate.revoked", siem.SeverityMedium, "Certificate revoked: "+id)
	h.okJSON(w, 200, map[string]string{"message": "certificate revoked"})
}

func (h *Handler) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req domain.CreatePolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PolicyName == "" {
		h.errJSON(w, 400, "policy_name is required")
		return
	}
	// Scoped at the tenant, not a legal entity: a CommunicationPolicy has no
	// legal_entity_id — it governs service-to-service traffic across the
	// whole tenant. See the authorize() doc comment for why this service
	// passes two different identifiers and why that is not row 84a.
	if !h.authorize(w, r, principalID, tenantID, MtlsPolicyWrite) {
		return
	}
	pol := &domain.CommunicationPolicy{
		PolicyName:    req.PolicyName,
		SourceService: req.SourceService,
		TargetService: req.TargetService,
		Action:        req.Action,
		RequiresMtls:  req.RequiresMtls,
	}
	_ = h.store.CreatePolicy(r.Context(), tenantID, pol)
	h.okJSON(w, 201, pol)
}

func (h *Handler) ListPolicies(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, MtlsPolicyRead) {
		return
	}
	pols, _ := h.store.ListPolicies(r.Context(), tenantID)
	if pols == nil {
		pols = []domain.CommunicationPolicy{}
	}
	h.okJSON(w, 200, map[string]interface{}{"data": pols, "count": len(pols)})
}

func (h *Handler) okJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (h *Handler) errJSON(w http.ResponseWriter, code int, msg string) {
	h.okJSON(w, code, map[string]string{"error": msg})
}
