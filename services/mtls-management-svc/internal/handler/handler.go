package handler

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"
	authzpkg "zoiko.io/mtls-management-svc/internal/authz"
	"zoiko.io/mtls-management-svc/internal/ca"
	"zoiko.io/mtls-management-svc/internal/domain"
	svcenvelope "zoiko.io/mtls-management-svc/internal/envelope"
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
		apicontract.WriteProblem(w, r, apicontract.Problem{
			Status:    http.StatusUnauthorized,
			Code:      "missing_tenant_scope",
			Detail:    "X-Tenant-ID is required — the gateway sets it from a verified identity envelope",
			Retryable: false,
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
	store     store.Store
	ca        *ca.CA
	ledger    *ca.Ledger
	siem      *siem.Client
	authz     AuthzChecker
	logger    *zap.Logger
	bootstrap BootstrapTokens
}

// BootstrapTokens maps a self-provisioning bootstrap token to the ONE service
// it was issued for (Governance audit register M-01, 13 Oct 2026).
//
// There used to be a single shared token. Any holder — gateway-auth-svc,
// identity-context-svc, ai-governance-svc and three more mounted it — could
// mint a certificate for any service_name and common_name, including another
// service's. authorization-svc's workload gate identifies a PEP by the
// certificate's CN, so the holder of the shared token could become any
// registered PEP. A token now authenticates exactly one service, and the
// certificate it buys must name that service and nothing else.
type BootstrapTokens map[string]string

// serviceFor returns the service a presented token belongs to, comparing in
// constant time against every configured token so the lookup does not leak
// which prefix matched.
func (b BootstrapTokens) serviceFor(presented string) (string, bool) {
	found := ""
	for token, service := range b {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1 {
			found = service
		}
	}
	return found, found != ""
}

// New constructs a Handler. ledger is the durable revocation list behind
// GET /v1/mtls/crl and is required. bootstrap enables the self-provisioning
// path on ProvisionCert (see its doc comment); nil or empty disables it and
// every caller goes through the normal principal + authorize flow.
func New(s store.Store, c *ca.CA, ledger *ca.Ledger, siemClient *siem.Client, az AuthzChecker, l *zap.Logger, bootstrap BootstrapTokens) *Handler {
	return &Handler{store: s, ca: c, ledger: ledger, siem: siemClient, authz: az, logger: l, bootstrap: bootstrap}
}

// requirePrincipal returns the gateway-verified principal, or refuses the
// request. This service had no notion of a calling principal at all before
// now — nothing read X-Principal-Id on any route.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		h.errJSON(w, r, http.StatusUnauthorized,
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
			h.errJSON(w, r, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		if errors.Is(err, authzpkg.ErrAuthorizationRejected) {
			h.errJSON(w, r, http.StatusBadRequest, "authorization-svc rejected the request's scope (legal_entity_id or tenant)")
			return false
		}
		h.logger.Error("authorization check failed", zap.Error(err))
		h.errJSON(w, r, http.StatusServiceUnavailable, "authorization service unavailable")
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
	r.Use(svcenvelope.Middleware(envelopePolicy(), svcenvelope.DefaultReporter()))
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "mtls-management-svc"})
	})
	r.Route("/v1/mtls", func(r chi.Router) {
		r.Get("/crl", h.GetCRL) // CRLPath
		r.Post("/certificates", h.ProvisionCert)
		r.Get("/certificates", h.ListCerts)
		r.Get("/certificates/{id}", h.GetCert)
		r.Post("/certificates/{id}/rotate", h.RotateCert)
		r.Delete("/certificates/{id}", h.RevokeCert)
		r.Post("/certificates/csr", h.SignCSR) // CSR signing per O-2
		r.Post("/policies", h.CreatePolicy)
		r.Get("/policies", h.ListPolicies)
	})
	return r
}

// CRLPath serves the CA-signed certificate revocation list.
const CRLPath = "/v1/mtls/crl"

// bootstrapHeader carries a self-provisioning bootstrap token.
const bootstrapHeader = "X-Mtls-Bootstrap-Token"

// envelopePolicy is the generated contract policy with a narrower exemption.
//
// The generated policy (internal/envelope/contract.go) exempts the whole
// /v1/mtls/certificates path. That exempted every caller of POST and GET on
// it, not only a service bootstrapping its first certificate — so a human
// provisioning or listing certificates had no resolved envelope, sent no
// idempotency key, and, because the authz client forwards the tenant from
// the envelope, asked authorization-svc a TENANTLESS question. Only the
// request that cannot carry an envelope is exempt now: a POST presenting a
// bootstrap token. The CRL is exempt because verifiers fetch it with no
// identity at all — it is public, signed data.
func envelopePolicy() svcenvelope.Policy {
	p := svcenvelope.ServicePolicy()
	p.ExemptPaths = nil
	p.Exempt = func(r *http.Request) bool {
		switch r.URL.Path {
		case "/healthz", "/readyz", "/health", CRLPath:
			return true
		case "/v1/mtls/certificates":
			return r.Method == http.MethodPost && r.Header.Get(bootstrapHeader) != ""
		}
		return false
	}
	return p
}

// GetCRL serves the X.509 CRL signed by this CA: DER (application/pkix-crl)
// by default, PEM with ?format=pem. Unauthenticated by design — a CRL lists
// serial numbers only, and its signature, not the channel, is what a verifier
// trusts. Signed per request, so NextUpdate is always CRLValidity ahead.
func (h *Handler) GetCRL(w http.ResponseWriter, r *http.Request) {
	der, err := h.ca.SignCRL(h.ledger.Entries(), time.Now().UTC())
	if err != nil {
		h.logger.Error("failed to sign CRL", zap.Error(err))
		h.errJSON(w, r, http.StatusInternalServerError, "failed to sign CRL")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("format") == "pem" {
		w.Header().Set("Content-Type", "application/x-pem-file")
		_, _ = w.Write(ca.CRLPEM(der))
		return
	}
	w.Header().Set("Content-Type", "application/pkix-crl")
	_, _ = w.Write(der)
}

func (h *Handler) ProvisionCert(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	var req domain.ProvisionCertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, r, 400, "invalid body")
		return
	}
	if err := req.Validate(); err != nil {
		h.errJSON(w, r, 400, err.Error())
		return
	}

	// Self-provisioning bootstrap path: a service requesting its OWN first
	// certificate has no principal, no session, and no prior credential to
	// present — the standard PKI bootstrap problem. h.bootstrapToken is a
	// shared secret distributed to trusted services out-of-band (a
	// docker-compose-provisioned file, same pattern as identity-svc's JWT
	// signing key — see deployments/docker-compose.yml's
	// mtls-bootstrap-keygen), never through this platform's own vault
	// service: secret-vault-integration-svc's Broker deliberately never
	// discloses raw secret material to a caller (see its own doc comment),
	// so it cannot be the delivery mechanism for a value a caller needs to
	// present elsewhere. A caller presenting a valid token is authenticated
	// AS THE SERVICE THAT TOKEN WAS ISSUED TO (BootstrapTokens) for this one
	// provisioning call only — it grants no other capability on this service
	// and bypasses no other route's principal/authorize check.
	//
	// A presented token that matches nothing is refused outright. It used to
	// fall through to the principal path, and since a token-bearing POST is
	// exempt from the envelope, that path then asked authorization-svc a
	// question with no tenant.
	if presented := r.Header.Get(bootstrapHeader); presented != "" {
		service, ok := h.bootstrap.serviceFor(presented)
		if !ok {
			h.logger.Warn("mtls: bootstrap token presented but not recognised — refused",
				zap.String("claimed_service", req.ServiceName))
			h.errJSONCode(w, r, http.StatusUnauthorized, "invalid_bootstrap_token",
				"the bootstrap token is not valid for any service")
			return
		}
		// The certificate must name the token's service and nothing else:
		// authorization-svc's workload gate reads the CN as the caller's
		// identity, so a free CN would let any token holder become any PEP.
		if req.ServiceName != service || req.CommonName != service {
			h.logger.Warn("mtls: bootstrap token used for another identity — refused",
				zap.String("token_service", service),
				zap.String("service_name", req.ServiceName), zap.String("common_name", req.CommonName))
			h.errJSONCode(w, r, http.StatusForbidden, "bootstrap_identity_mismatch",
				"a bootstrap token provisions only its own service: service_name and common_name must both be "+service)
			return
		}
		h.logger.Info("mtls: self-provisioning via bootstrap token", zap.String("service_name", service))
		env := os.Getenv("ENV")
		if env == "" {
			env = "local"
		}
		uriSANs := []string{fmt.Sprintf("spiffe://zoiko/%s/%s", env, service)}
		issued, err := h.ca.IssueLeaf(service, []string{service}, uriSANs, time.Duration(req.RotationDays)*24*time.Hour)
		if err != nil {
			h.logger.Error("failed to issue leaf certificate", zap.String("common_name", service), zap.Error(err))
			h.errJSON(w, r, 500, "failed to issue certificate")
			return
		}
		h.recordAndRespond(w, r, tenantID, "bootstrap:"+service, req, issued)
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, MtlsCertProvision) {
		return
	}

	// Build URI SAN for SPIFFE identity per O-1: spiffe://zoiko/<env>/<service>
	env := os.Getenv("ENV")
	if env == "" {
		env = "local"
	}
	uriSANs := []string{fmt.Sprintf("spiffe://zoiko/%s/%s", env, req.ServiceName)}
	issued, err := h.ca.IssueLeaf(req.CommonName, []string{req.ServiceName}, uriSANs, time.Duration(req.RotationDays)*24*time.Hour)
	if err != nil {
		h.logger.Error("failed to issue leaf certificate", zap.String("common_name", req.CommonName), zap.Error(err))
		h.errJSON(w, r, 500, "failed to issue certificate")
		return
	}
	h.recordAndRespond(w, r, tenantID, principalID, req, issued)
}

// recordAndRespond persists an issued leaf certificate and writes the
// provisioning response — the tail shared by both ProvisionCert's normal
// human/admin-authorized path and its bootstrap self-provisioning path.
// issuedByPrincipalID is audit metadata only (recorded nowhere yet beyond
// logs/SIEM); it is never re-authorized here.
func (h *Handler) recordAndRespond(w http.ResponseWriter, r *http.Request, tenantID, issuedByPrincipalID string, req domain.ProvisionCertRequest, issued *ca.IssuedCertificate) {
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
		h.errJSON(w, r, 500, "failed to provision certificate")
		return
	}

	// Doc 05 §13.2 names "certificate issuance/rotation events" as a
	// required SIEM signal.
	h.siem.Stream(r.Context(), tenantID, "certificate.issued", siem.SeverityLow,
		"Certificate issued for "+req.ServiceName+" ("+req.CommonName+") by "+issuedByPrincipalID)

	h.okJSON(w, r, 201, domain.ProvisionCertResult{
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
		h.errJSON(w, r, 404, "certificate not found")
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
	h.okJSON(w, r, 200, cert)
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
		h.errJSON(w, r, http.StatusBadRequest,
			"legal_entity_id query parameter is required — it is the authorization scope for this listing")
		return
	}
	if !h.authorize(w, r, principalID, legalEntityID, MtlsCertRead) {
		return
	}
	certs, err := h.store.ListCerts(r.Context(), tenantID, legalEntityID, r.URL.Query().Get("status"))
	if err != nil {
		h.errJSON(w, r, 500, "failed to list certificates")
		return
	}
	if certs == nil {
		certs = []domain.MtlsCertificate{}
	}
	h.okJSON(w, r, 200, map[string]interface{}{"data": certs, "count": len(certs)})
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
		h.errJSON(w, r, 404, "certificate not found")
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, MtlsCertRotate) {
		return
	}

	// Rotation issues a genuinely new key pair and certificate signed by
	// the same CA — it is not a metadata refresh. The old private key
	// (which this service never retained anyway) becomes unusable the
	// moment the requesting service adopts the new one.
	env := os.Getenv("ENV")
	if env == "" {
		env = "local"
	}
	uriSANs := []string{fmt.Sprintf("spiffe://zoiko/%s/%s", env, existing.ServiceName)}
	issued, err := h.ca.IssueLeaf(existing.CommonName, []string{existing.ServiceName}, uriSANs, time.Duration(existing.RotationDays)*24*time.Hour)
	if err != nil {
		h.logger.Error("failed to issue rotated certificate", zap.String("id", id), zap.Error(err))
		h.errJSON(w, r, 500, "failed to rotate certificate")
		return
	}

	updated, err := h.store.ReplaceCertMaterial(r.Context(), tenantID, id,
		issued.SerialNumber, issued.Fingerprint, string(issued.CertificatePEM), issued.NotBefore, issued.NotAfter)
	if errors.Is(err, store.ErrCertRevoked) {
		// Rotation used to set the status back to ACTIVE, so rotating a
		// revoked certificate silently un-revoked it and handed out fresh
		// key material for an identity that had been withdrawn.
		h.errJSONCode(w, r, http.StatusConflict, "certificate_revoked",
			"a revoked certificate cannot be rotated; provision a new one")
		return
	}
	if err != nil {
		h.errJSON(w, r, 404, "certificate not found")
		return
	}

	h.siem.Stream(r.Context(), tenantID, "certificate.rotated", siem.SeverityLow, "Certificate rotated: "+id)

	h.okJSON(w, r, 200, domain.ProvisionCertResult{
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
		h.errJSON(w, r, 404, "certificate not found")
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, MtlsCertRevoke) {
		return
	}

	// The ledger first: a revocation is only real once verifiers can learn of
	// it from the CRL, and the ledger is what survives a restart (the record
	// store is in memory). Written again after the store transition with the
	// record as it then stands, which covers a rotation that landed between
	// the read above and the revoke — ReplaceCertMaterial refuses a revoked
	// record, so nothing can be added after that.
	if err := h.revokeSerials(r, tenantID, existing, principalID); err != nil {
		h.logger.Error("failed to persist revocation", zap.String("id", id), zap.Error(err))
		h.errJSON(w, r, http.StatusInternalServerError, "failed to revoke certificate")
		return
	}
	revoked, err := h.store.RevokeCert(r.Context(), tenantID, id)
	if err != nil {
		h.errJSON(w, r, 404, "certificate not found")
		return
	}
	if err := h.revokeSerials(r, tenantID, revoked, principalID); err != nil {
		h.logger.Error("failed to persist revocation", zap.String("id", id), zap.Error(err))
		h.errJSON(w, r, http.StatusInternalServerError, "failed to revoke certificate")
		return
	}
	h.siem.Stream(r.Context(), tenantID, "certificate.revoked", siem.SeverityMedium,
		"Certificate revoked: "+id+" by "+principalID)
	h.okJSON(w, r, 200, map[string]interface{}{
		"message":         "certificate revoked",
		"revoked_serials": revoked.AllSerialNumbers(),
		"crl":             CRLPath,
	})
}

// revokeSerials puts every serial cert has carried on the revocation ledger.
func (h *Handler) revokeSerials(r *http.Request, tenantID string, cert *domain.MtlsCertificate, principalID string) error {
	entries := make([]ca.RevokedSerial, 0, len(cert.PriorSerialNumbers)+1)
	for _, serial := range cert.AllSerialNumbers() {
		entries = append(entries, ca.RevokedSerial{
			SerialNumber: serial, CertID: cert.ID, TenantID: tenantID, Reason: "revoked by " + principalID,
		})
	}
	return h.ledger.Revoke(entries...)
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
		h.errJSON(w, r, 400, "policy_name is required")
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
	if err := h.store.CreatePolicy(r.Context(), tenantID, pol); err != nil {
		h.errJSON(w, r, 500, "failed to create policy")
		return
	}
	h.okJSON(w, r, 201, pol)
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
	pols, err := h.store.ListPolicies(r.Context(), tenantID)
	if err != nil {
		h.errJSON(w, r, 500, "failed to list policies")
		return
	}
	if pols == nil {
		pols = []domain.CommunicationPolicy{}
	}
	h.okJSON(w, r, 200, map[string]interface{}{"data": pols, "count": len(pols)})
}

// SignCSR handles POST /v1/mtls/certificates/csr — signs a CSR and returns
// only the certificate, never a private key (per O-2).
func (h *Handler) SignCSR(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req struct {
		CSR           string `json:"csr"`
		RotationDays  int    `json:"rotation_days"`
		ServiceName   string `json:"service_name"`
		LegalEntityID string `json:"legal_entity_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.errJSON(w, r, 400, "invalid body")
		return
	}
	if req.CSR == "" {
		h.errJSON(w, r, 400, "csr is required")
		return
	}
	if req.ServiceName == "" {
		h.errJSON(w, r, 400, "service_name is required")
		return
	}
	if req.LegalEntityID == "" {
		h.errJSON(w, r, 400, "legal_entity_id is required")
		return
	}
	// Same decision as ProvisionCert: at the legal entity the certificate is for.
	if !h.authorize(w, r, principalID, req.LegalEntityID, MtlsCertProvision) {
		return
	}
	if req.RotationDays <= 0 {
		req.RotationDays = 1
	}
	if req.RotationDays > 90 {
		req.RotationDays = 90
	}

	// Parse the CSR
	csrPEM := []byte(req.CSR)
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		h.errJSON(w, r, 400, "invalid csr")
		return
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		h.errJSON(w, r, 400, "invalid csr: "+err.Error())
		return
	}
	if err := csr.CheckSignature(); err != nil {
		h.errJSON(w, r, 400, "csr signature invalid")
		return
	}

	// Verify the CSR's subject matches the requested service name
	if csr.Subject.CommonName != req.ServiceName {
		h.errJSONCode(w, r, 400, "csr_subject_mismatch", "CSR common name must match service_name")
		return
	}

	// Verify the CSR's DNSNames include the service name
	found := false
	for _, dns := range csr.DNSNames {
		if dns == req.ServiceName {
			found = true
			break
		}
	}
	if !found {
		h.errJSONCode(w, r, 400, "csr_dns_mismatch", "CSR DNS names must include service_name")
		return
	}

	// Verify the CSR's URI SANs include the expected SPIFFE URI
	env := os.Getenv("ENV")
	if env == "" {
		env = "local"
	}
	expectedURI := fmt.Sprintf("spiffe://zoiko/%s/%s", env, req.ServiceName)
	foundURI := false
	for _, uri := range csr.URIs {
		if uri.String() == expectedURI {
			foundURI = true
			break
		}
	}
	if !foundURI {
		h.errJSONCode(w, r, 400, "csr_uri_mismatch", "CSR URI SANs must include "+expectedURI)
		return
	}

	// Sign the CSR
	issued, err := h.ca.SignCSR(csr, time.Duration(req.RotationDays)*24*time.Hour)
	if err != nil {
		h.logger.Error("failed to sign CSR", zap.String("service_name", req.ServiceName), zap.Error(err))
		h.errJSON(w, r, 500, "failed to sign certificate")
		return
	}

	// Record the certificate in the store
	now := time.Now()
	cert := &domain.MtlsCertificate{
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		ServiceName:    req.ServiceName,
		CommonName:     req.ServiceName,
		Issuer:         "ZoikoSuite Internal CA",
		SerialNumber:   issued.SerialNumber,
		Fingerprint:    issued.Fingerprint,
		CertificatePEM: string(issued.CertificatePEM),
		ValidFrom:      issued.NotBefore,
		ValidTo:        issued.NotAfter,
		RotationDays:   req.RotationDays,
		AutoRotate:     true,
		Status:         domain.CertStatusActive,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := h.store.CreateCert(r.Context(), tenantID, cert); err != nil {
		h.errJSON(w, r, 500, "failed to provision certificate")
		return
	}

	// Record audit
	h.siem.Stream(r.Context(), tenantID, "certificate.issued", siem.SeverityLow,
		"Certificate issued for "+req.ServiceName+" via CSR by "+principalID)

	// Return only the certificate (no private key)
	h.okJSON(w, r, 201, domain.ProvisionCertResult{
		Certificate: *cert,
		CACertPEM:   string(h.ca.CertificatePEM()),
	})
}

func (h *Handler) errJSON(w http.ResponseWriter, r *http.Request, code int, msg string) {
	apicontract.WriteProblem(w, r, apicontract.Problem{
		Status:    code,
		Code:      "internal_error",
		Detail:    msg,
		Retryable: code >= 500,
	})
}

// errJSONCode is errJSON with a machine-readable code beside the message.
func (h *Handler) errJSONCode(w http.ResponseWriter, r *http.Request, code int, errCode, msg string) {
	apicontract.WriteProblem(w, r, apicontract.Problem{
		Status:    code,
		Code:      errCode,
		Detail:    msg,
		Retryable: code >= 500,
	})
}

func (h *Handler) okJSON(w http.ResponseWriter, r *http.Request, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
