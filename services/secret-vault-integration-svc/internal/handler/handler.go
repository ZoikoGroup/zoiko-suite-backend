package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/authz"
	"zoiko.io/secret-vault-integration-svc/internal/classification"
	"zoiko.io/secret-vault-integration-svc/internal/domain"
	svcenvelope "zoiko.io/secret-vault-integration-svc/internal/envelope"
	svcmiddleware "zoiko.io/secret-vault-integration-svc/internal/middleware"
	"zoiko.io/secret-vault-integration-svc/internal/store"
	"zoiko.io/secret-vault-integration-svc/internal/vault"
)

// SecretVaultStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type SecretVaultStore interface {
	CreateSecretPolicy(ctx context.Context, params domain.CreateSecretPolicyParams) (*domain.SecretPolicy, bool, error)
	FindSecretPolicyByID(ctx context.Context, secretPolicyID string) (*domain.SecretPolicy, error)

	CreateSecretPolicyVersion(ctx context.Context, params domain.CreateSecretPolicyVersionParams) (*domain.SecretPolicyVersion, bool, error)
	FindSecretPolicyVersionByID(ctx context.Context, secretPolicyVersionID string) (*domain.SecretPolicyVersion, error)
	ActivateVersion(ctx context.Context, secretPolicyVersionID, actorID string) (*domain.SecretPolicyVersion, []*domain.SecretPolicyVersion, bool, error)
	ListVersionHistory(ctx context.Context, secretPolicyID, tenantID string) ([]*domain.SecretPolicyVersion, error)

	FindApplicableVersions(ctx context.Context, secretClass string, tenantID, legalEntityID *string) ([]*domain.ApplicableSecretPolicyVersion, error)
	FindApplicableVersionByPath(ctx context.Context, secretPath string, tenantID, legalEntityID *string) (*domain.ApplicableSecretPolicyVersion, error)

	CreateLease(ctx context.Context, params domain.CreateLeaseParams) (*domain.SecretLease, bool, error)
	FindLeaseByID(ctx context.Context, leaseID, tenantID string) (*domain.SecretLease, error)
	ListLeases(ctx context.Context, filter store.LeaseListFilter) ([]*domain.SecretLease, error)
	RevokeLease(ctx context.Context, leaseID, tenantID string) (*domain.SecretLease, bool, error)
	RevokeLeasesBySecretPath(ctx context.Context, secretPath string) ([]*domain.SecretLease, error)

	RecordAuditEntry(ctx context.Context, params domain.RecordAuditEntryParams) (*domain.SecretAccessAuditLog, error)
	FindAuditEntryByRotationRequestID(ctx context.Context, requestID string) (*domain.SecretAccessAuditLog, error)
	ListAuditLog(ctx context.Context, filter store.AuditListFilter) ([]*domain.SecretAccessAuditLog, error)

	CreateSharedSecretException(ctx context.Context, params domain.SharedSecretException) (*domain.SharedSecretException, bool, error)
	FindSharedSecretExceptionByID(ctx context.Context, exceptionID, tenantID string) (*domain.SharedSecretException, error)
	ListSharedSecretExceptions(ctx context.Context, filter domain.ListSharedSecretExceptionsFilter) ([]*domain.SharedSecretException, error)
	RevokeSharedSecretException(ctx context.Context, exceptionID, tenantID, actorID string) (*domain.SharedSecretException, bool, error)
}

// VaultBackend is the narrow interface the handler depends on for the
// actual secret material — see internal/vault.Backend.
//
// Put is exposed via a real endpoint (context.md didn't call for one,
// but found missing during live verification: without any way to seed
// material, Broker's call to Get can never succeed for a real deployment
// — the grant path was completely unreachable end to end). Administrative
// seeding, never called from the broker flow itself.
type VaultBackend interface {
	Get(ctx context.Context, secretPath string, expiresAt time.Time) (leaseToken string, err error)
	Verify(ctx context.Context, leaseToken string) (info vault.LeaseTokenInfo, err error)
	GetMaterial(ctx context.Context, secretPath string) ([]byte, error)
	Put(ctx context.Context, secretPath string, material []byte) error
	Rotate(ctx context.Context, secretPath string) error
}

// EventPublisher is the narrow interface the handler depends on for
// publishing domain events.
type EventPublisher interface {
	PublishAccessRequested(ctx context.Context, secretPath, requestedByPrincipalID, correlationID string) error
	PublishAccessGranted(ctx context.Context, lease domain.SecretLease, correlationID string) error
	PublishRotationCompleted(ctx context.Context, secretPolicyID, secretPath, rotatedByPrincipalID string, revokedLeaseCount int, correlationID string) error
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store     SecretVaultStore
	vault     VaultBackend
	publisher EventPublisher
	authz     authz.Client
	log       *zap.Logger

	// authzPlatformScopeID is the legal_entity_id used for secret policy
	// administration, which is platform-scoped rather than entity-scoped.
	// authorization-svc rejects an empty legal_entity_id.
	authzPlatformScopeID string

	// maxLeaseDurationCeiling is the platform-wide ceiling for
	// max_lease_duration_seconds on secret policy versions. 0 means no ceiling.
	maxLeaseDurationCeiling int

	// metrics is never nil -- New installs nopMetrics.
	metrics DomainMetrics
}

// DomainMetrics records the business-level outcomes this service's HTTP
// metrics cannot express.
//
// Declared here, in the package that produces the events, rather than the
// handler importing internal/telemetry: that keeps the handler's tests free of
// a Prometheus registry, and stops a second registration of the same collector
// inside a test binary. internal/telemetry.Metrics satisfies it.
type DomainMetrics interface {
	// BrokerDecision records a terminal outcome of a brokerage request:
	// granted, denied, no_policy, vault_error or error.
	BrokerDecision(outcome string)
	// LeaseRevoked records a revocation by cause: "explicit" or "rotation".
	LeaseRevoked(cause string)
	// SecretRotated records a completed rotation and the leases it killed.
	SecretRotated(revokedLeases int)
	// AuthzDecision records an authorization-svc outcome per action:
	// allowed, denied or unavailable.
	AuthzDecision(action, outcome string)
	// VaultBackendError records a vault backend failure by operation.
	VaultBackendError(operation string)
}

// nopMetrics is the default, so a Handler built without metrics -- every
// handler unit test -- behaves identically and needs no wiring.
type nopMetrics struct{}

func (nopMetrics) BrokerDecision(string)        {}
func (nopMetrics) LeaseRevoked(string)          {}
func (nopMetrics) SecretRotated(int)            {}
func (nopMetrics) AuthzDecision(string, string) {}
func (nopMetrics) VaultBackendError(string)     {}

// New constructs a Handler.
func New(store SecretVaultStore, vault VaultBackend, publisher EventPublisher, authzClient authz.Client, authzPlatformScopeID string, maxLeaseDurationCeiling int, log *zap.Logger) *Handler {
	return &Handler{
		store:                    store,
		vault:                    vault,
		publisher:                publisher,
		authz:                    authzClient,
		authzPlatformScopeID:     authzPlatformScopeID,
		maxLeaseDurationCeiling:  maxLeaseDurationCeiling,
		log:                      log,
		metrics:                  nopMetrics{},
	}
}

// UseMetrics attaches a domain metrics recorder. Separate from New so the
// existing constructor signature -- and every caller of it -- is unchanged.
func (h *Handler) UseMetrics(m DomainMetrics) *Handler {
	if m != nil {
		h.metrics = m
	}
	return h
}

// RegisterRoutes mounts all routes on the given chi router.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	r.Post("/v1/secret-policies", h.CreateSecretPolicy)
	r.Get("/v1/secret-policies", h.ListApplicableSecretPolicyVersions)
	r.Post("/v1/secret-policies/{secret_policy_id}/versions", h.CreateSecretPolicyVersion)
	r.Post("/v1/secret-policies/{secret_policy_id}/versions/{version_id}/activate", h.ActivateVersion)
	r.Get("/v1/secret-policies/{secret_policy_id}/versions", h.ListVersionHistory)
	r.Post("/v1/secret-policies/{secret_policy_id}/rotate", h.Rotate)
	r.Post("/v1/secret-policies/{secret_policy_id}/material", h.PutSecretMaterial)
	r.Post("/v1/secret-policies/{secret_policy_id}/emergency-retrieval", h.EmergencyRetrieval)

	r.Post("/v1/secrets/broker", h.Broker)
	r.Get("/v1/secrets/leases/{lease_id}", h.GetLease)
	r.Post("/v1/secrets/leases/{lease_id}/verify", h.VerifyLease)
	r.Get("/v1/secrets/leases", h.ListLeases)
	r.Post("/v1/secrets/leases/{lease_id}/revoke", h.RevokeLease)
	r.Get("/v1/secrets/audit", h.ListAuditLog)

	// §13 controls: shared-secret exception register (break-glass evidence
	// must be answerable from inside this service, not from a runbook).
	r.Post("/v1/shared-secret-exceptions", h.CreateSharedSecretException)
	r.Get("/v1/shared-secret-exceptions", h.ListSharedSecretExceptions)
	r.Post("/v1/shared-secret-exceptions/{exception_id}/revoke", h.RevokeSharedSecretException)
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// ── POST /v1/secret-policies ─────────────────────────────────────────────────

type createSecretPolicyRequest struct {
	SecretPolicyID       string `json:"secret_policy_id,omitempty"`
	SecretClass          string `json:"secret_class"`
	SecretPath           string `json:"secret_path"`
	CreatedByPrincipalID string `json:"created_by_principal_id"`
	DataClassification   string `json:"data_classification,omitempty"`
}

func (req createSecretPolicyRequest) missingField() string {
	switch {
	case req.SecretClass == "":
		return "secret_class"
	case req.SecretPath == "":
		return "secret_path"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

// CreateSecretPolicy handles POST /v1/secret-policies. Idempotent on
// secret_path.
func (h *Handler) CreateSecretPolicy(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretPolicyCreate) {
		return
	}

	var req createSecretPolicyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	if req.DataClassification != "" {
		if !classification.Classification(req.DataClassification).Valid() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_classification", "message": "data_classification must be PUBLIC, INTERNAL, CONFIDENTIAL, or RESTRICTED"})
			return
		}
	}

	p, created, err := h.store.CreateSecretPolicy(r.Context(), domain.CreateSecretPolicyParams{
		SecretPolicyID:       req.SecretPolicyID,
		SecretClass:          req.SecretClass,
		SecretPath:           req.SecretPath,
		CreatedByPrincipalID: req.CreatedByPrincipalID,
		DataClassification:   req.DataClassification,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "secret_policy_conflict", "secret_path": req.SecretPath})
		default:
			h.log.Error("CreateSecretPolicy: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, p)
}

// ── POST /v1/secret-policies/{id}/versions ──────────────────────────────────

type createSecretPolicyVersionRequest struct {
	SecretPolicyVersionID   string          `json:"secret_policy_version_id,omitempty"`
	TenantID                *string         `json:"tenant_id,omitempty"`
	LegalEntityID           *string         `json:"legal_entity_id,omitempty"`
	AllowedWorkloadIDs      json.RawMessage `json:"allowed_workload_ids,omitempty"`
	MaxLeaseDurationSeconds int             `json:"max_lease_duration_seconds"`
	EffectiveFrom           time.Time       `json:"effective_from"`
	EffectiveTo             *time.Time      `json:"effective_to,omitempty"`
	CreatedByPrincipalID    string          `json:"created_by_principal_id"`
	// RotationIntervalSeconds, when > 0, schedules automated material
	// rotation for this version (compliance-close §13).
	RotationIntervalSeconds int `json:"rotation_interval_seconds,omitempty"`
}

func (req createSecretPolicyVersionRequest) missingField() string {
	switch {
	case req.EffectiveFrom.IsZero():
		return "effective_from"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

func (req createSecretPolicyVersionRequest) rotationInterval() int {
	if req.RotationIntervalSeconds < 0 {
		return 0
	}
	return req.RotationIntervalSeconds
}

// CreateSecretPolicyVersion handles
// POST /v1/secret-policies/{secret_policy_id}/versions. New versions are
// always created in DRAFT status.
func (h *Handler) CreateSecretPolicyVersion(w http.ResponseWriter, r *http.Request) {
	secretPolicyID, ok := requireUUIDParam(w, r, "secret_policy_id")
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretPolicyVersionCreate) {
		return
	}

	var req createSecretPolicyVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	if req.MaxLeaseDurationSeconds <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_field", "field": "max_lease_duration_seconds", "message": "must be greater than 0",
		})
		return
	}
	if h.maxLeaseDurationCeiling > 0 && req.MaxLeaseDurationSeconds > h.maxLeaseDurationCeiling {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_field",
			"field":   "max_lease_duration_seconds",
			"message": fmt.Sprintf("exceeds platform maximum of %d seconds", h.maxLeaseDurationCeiling),
		})
		return
	}
	// tenant_id in the body used to be written straight through, so a caller
	// could publish the secret policy version that governs another tenant's
	// secret path — including the allowed_workload_ids list that decides who may
	// broker it.
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}
	// tenant_id nil means GLOBAL, and a global version bound to one legal entity
	// is an incoherent scope: it would govern every tenant while naming an entity
	// inside one of them.
	if req.TenantID == nil && req.LegalEntityID != nil && *req.LegalEntityID != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_scope",
			"message": "a global version (no tenant_id) cannot name a legal_entity_id",
		})
		return
	}

	v, created, err := h.store.CreateSecretPolicyVersion(r.Context(), domain.CreateSecretPolicyVersionParams{
		SecretPolicyVersionID:   req.SecretPolicyVersionID,
		SecretPolicyID:          secretPolicyID,
		TenantID:                req.TenantID,
		LegalEntityID:           req.LegalEntityID,
		AllowedWorkloadIDs:      []byte(req.AllowedWorkloadIDs),
		MaxLeaseDurationSeconds: req.MaxLeaseDurationSeconds,
		EffectiveFrom:           req.EffectiveFrom,
		EffectiveTo:             req.EffectiveTo,
		CreatedByPrincipalID:    req.CreatedByPrincipalID,
		RotationIntervalSeconds: req.rotationInterval(),
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSecretPolicyNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "secret_policy_not_found", "secret_policy_id": secretPolicyID})
		case errors.Is(err, domain.ErrConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "secret_policy_version_conflict"})
		default:
			h.log.Error("CreateSecretPolicyVersion: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, v)
}

// ── POST /v1/secret-policies/{id}/versions/{version_id}/activate ───────────

type activateVersionRequest struct {
	ActivatedByPrincipalID string `json:"activated_by_principal_id"`
}

// activateVersionResponse is the activated version plus whether this call is
// what activated it.
//
// The store already distinguishes the two cases — it short-circuits when the
// target is already ACTIVE and returns transitioned=false — but that flag used
// to be dropped here, so a real DRAFT->ACTIVE transition and a no-op repeat
// were byte-identical 200s. A caller could not tell whether its own request
// changed anything, which is the same "a replay must not read as a write"
// distinction CreateSecretPolicy makes with 201-vs-200.
//
// The version is embedded by pointer so every field it already returned stays
// exactly where it was: this only adds a key.
type activateVersionResponse struct {
	*domain.SecretPolicyVersion
	Transitioned bool `json:"transitioned"`
}

// ActivateVersion handles
// POST /v1/secret-policies/{secret_policy_id}/versions/{version_id}/activate.
func (h *Handler) ActivateVersion(w http.ResponseWriter, r *http.Request) {
	secretPolicyID, ok := requireUUIDParam(w, r, "secret_policy_id")
	if !ok {
		return
	}
	versionID, ok := requireUUIDParam(w, r, "version_id")
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretPolicyVersionActivate) {
		return
	}

	var req activateVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ActivatedByPrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "activated_by_principal_id"})
		return
	}

	existing, err := h.store.FindSecretPolicyVersionByID(r.Context(), versionID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSecretPolicyVersionNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "secret_policy_version_not_found", "secret_policy_version_id": versionID})
		default:
			h.log.Error("ActivateVersion: lookup failed", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	if existing.SecretPolicyID != secretPolicyID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "secret_policy_version_not_found", "secret_policy_id": secretPolicyID})
		return
	}

	activated, _, transitioned, err := h.store.ActivateVersion(r.Context(), versionID, req.ActivatedByPrincipalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition", "secret_policy_version_id": versionID})
		default:
			h.log.Error("ActivateVersion: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, activateVersionResponse{
		SecretPolicyVersion: activated,
		Transitioned:        transitioned,
	})
}

// ── POST /v1/secret-policies/{id}/material ──────────────────────────────────

type putSecretMaterialRequest struct {
	MaterialBase64 string `json:"material_base64"`
}

// PutSecretMaterial handles
// POST /v1/secret-policies/{secret_policy_id}/material — administrative
// seeding of the actual secret material into the vault backend.
//
// Found missing during live verification, not part of the original spec:
// without this, Broker's call to vault.Get can never succeed for any
// real deployment — there would be no way to ever populate the backend,
// making the grant path completely unreachable end to end despite every
// other piece (policy, lease, audit) working correctly. This endpoint is
// deliberately separate from Broker and from the policy-administration
// endpoints — it never runs on the request path, only when an operator
// is provisioning a secret.
func (h *Handler) PutSecretMaterial(w http.ResponseWriter, r *http.Request) {
	secretPolicyID, ok := requireUUIDParam(w, r, "secret_policy_id")
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretMaterialWrite) {
		return
	}

	var req putSecretMaterialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.MaterialBase64 == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "material_base64"})
		return
	}
	material, err := base64.StdEncoding.DecodeString(req.MaterialBase64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "material_base64", "message": "must be valid base64"})
		return
	}

	policy, err := h.store.FindSecretPolicyByID(r.Context(), secretPolicyID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSecretPolicyNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "secret_policy_not_found", "secret_policy_id": secretPolicyID})
		default:
			h.log.Error("PutSecretMaterial: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	// There is deliberately no tenant comparison on the policy row: a
	// secret_policies row has no tenant_id at all — tenancy lives on its
	// versions, as in policy-svc — so a material write cannot be scoped by
	// reading the policy.
	//
	// What bounds this write is the authorization above: SECRET_MATERIAL_WRITE is
	// checked with an empty entity, which resolves to the PLATFORM scope, so only
	// a platform-level principal can store material at all. A per-tenant check
	// via the applicable version was considered and rejected: material is
	// legitimately written before any version is active (create policy, store
	// material, then publish a version), so requiring one would break the
	// bootstrap order rather than close a hole. The caller's tenant is required
	// for attribution and logged with the write.
	h.log.Info("secret material stored",
		zap.String("secret_policy_id", secretPolicyID),
		zap.String("principal_id", principalID),
		zap.String("caller_tenant_id", tenantScope),
		zap.String("correlation_id", correlationID),
	)

	if err := h.vault.Put(r.Context(), policy.SecretPath, material); err != nil {
		h.log.Error("PutSecretMaterial: vault backend put failed", zap.String("secret_path", policy.SecretPath), zap.Error(err))
		h.metrics.VaultBackendError("put")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "vault_backend_unavailable"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"secret_policy_id": secretPolicyID, "secret_path": policy.SecretPath, "status": "material_stored"})
}

// ── GET /v1/secret-policies/{id}/versions ───────────────────────────────────

// ListVersionHistory handles GET /v1/secret-policies/{secret_policy_id}/versions.
//
// Had no tenant scoping at all until now: any authenticated caller could
// list every tenant's allowed_workload_ids and lease-duration limits for
// a policy — provisioning data about who may broker the secret, not
// something a read endpoint should hand out platform-wide by default.
// Now requires X-Tenant-Id, same as its sibling read endpoints
// (GetLease, ListLeases).
func (h *Handler) ListVersionHistory(w http.ResponseWriter, r *http.Request) {
	secretPolicyID, ok := requireUUIDParam(w, r, "secret_policy_id")
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretPolicyVersionList) {
		return
	}

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	results, err := h.store.ListVersionHistory(r.Context(), secretPolicyID, tenantScope)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSecretPolicyNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "secret_policy_not_found", "secret_policy_id": secretPolicyID})
		default:
			h.log.Error("ListVersionHistory: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	if results == nil {
		results = []*domain.SecretPolicyVersion{}
	}
	writeJSON(w, http.StatusOK, results)
}

// ── GET /v1/secret-policies (applicable set) ────────────────────────────────

// ListApplicableSecretPolicyVersions handles GET /v1/secret-policies.
// secret_class is required (400 if missing) — same posture as
// policy-svc requiring policy_type (context.md §7.2).
func (h *Handler) ListApplicableSecretPolicyVersions(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	secretClass := q.Get("secret_class")
	if secretClass == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "secret_class"})
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretPolicyList) {
		return
	}

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var tenantID, legalEntityID *string
	if v := q.Get("tenant_id"); v != "" {
		tenantID = &v
	}
	if v := q.Get("legal_entity_id"); v != "" {
		legalEntityID = &v
	}
	// ?tenant_id= used to be taken as the scope outright, so any caller could read
	// the secret policies governing another tenant's paths. Omitting it still
	// means "global versions only".
	if h.refuseForeignTenant(w, tenantID, tenantScope) {
		return
	}

	results, err := h.store.FindApplicableVersions(r.Context(), secretClass, tenantID, legalEntityID)
	if err != nil {
		h.log.Error("ListApplicableSecretPolicyVersions: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if results == nil {
		results = []*domain.ApplicableSecretPolicyVersion{}
	}
	writeJSON(w, http.StatusOK, results)
}

// ── POST /v1/secrets/broker ──────────────────────────────────────────────────

type brokerRequest struct {
	SecretPath             string  `json:"secret_path"`
	TenantID               *string `json:"tenant_id,omitempty"`
	LegalEntityID          *string `json:"legal_entity_id,omitempty"`
	RequestedByPrincipalID string  `json:"requested_by_principal_id"`
	RequestID              string  `json:"request_id"`
	// CorrelationID is part of the documented body shape (context.md
	// §7.2). The X-Correlation-ID header, used by every other endpoint
	// in this service, takes precedence if present.
	CorrelationID string `json:"correlation_id,omitempty"`
}

func (req brokerRequest) missingField() string {
	switch {
	case req.SecretPath == "":
		return "secret_path"
	case req.RequestedByPrincipalID == "":
		return "requested_by_principal_id"
	case req.RequestID == "":
		return "request_id"
	default:
		return ""
	}
}

type brokerResponse struct {
	LeaseID    string    `json:"lease_id"`
	SecretPath string    `json:"secret_path"`
	LeaseToken string    `json:"lease_token"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Broker handles POST /v1/secrets/broker — the core value of this
// service. See context.md §7.2 for the full decision tree; this is a
// direct implementation of it, including the "deny-by-absence" posture
// (no applicable policy is treated as a refusal, not pushed back to the
// caller the way policy-svc's Evaluate does).
//
// Note on lease_token and idempotency: the durable state (lease row,
// audit trail, published events) is fully idempotent on request_id — a
// retried request never creates a second lease or re-emits
// secret.access.granted. The lease_token itself is minted fresh from the
// vault backend on every call, including retries — it's a short-lived
// opaque pointer, not the lease's identity, so re-minting it on retry is
// safe and avoids having to persist a live credential-adjacent token in
// Postgres. This is an implementation decision not spelled out in
// context.md — flag if a byte-identical token on replay is actually
// required.
func (h *Handler) Broker(w http.ResponseWriter, r *http.Request) {
	var req brokerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	// tenant_id in the body chose which policy scope governed the request, and
	// was stamped on the lease and on every audit entry — so a request could be
	// decided by another tenant's secret policy and then recorded in their access
	// log. Omitting it still means the global policy set.
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = req.CorrelationID
	}

	// The authorization input for the allowlist gate is the gateway-verified
	// envelope identity, never the body. §9 "the receiving service revalidates
	// forwarded context": this is the credential-issuing endpoint of the
	// platform, so a requested_by_principal_id that disagrees with the verified
	// actor is an identity the caller could not prove. Before, any caller
	// inside the right tenant who knew a name from allowed_workload_ids got a
	// lease — and the audit trail recorded that guessed name as both requester
	// and actor, so the evidence vouched for the impersonation.
	//
	// Actor() prefers X-Principal-Id — the one header the gateway overwrites
	// from the verified token — over X-Workload-Id. FromContext is the
	// middleware-entered envelope (main.go mounts it); the Parse fallback
	// covers a handler built without it, the same direct-header path
	// requirePrincipal and requireTenant already take.
	env, ok := svcenvelope.FromContext(r.Context())
	if !ok {
		env = svcenvelope.Parse(r)
	}
	if req.RequestedByPrincipalID != env.Actor() {
		// REQUESTED is recorded regardless of outcome; then a DENIED entry
		// whose SUBJECT is the claimed identity and whose ACTOR is the
		// verified caller — deliberately two different names, so the audit is
		// not wrong in exactly the case it must be right.
		h.recordRequested(r.Context(), req, correlationID)
		h.recordDenial(r.Context(), req, "", nil,
			"requested_by_principal_id does not match the gateway-verified envelope identity (X-Principal-Id / X-Workload-Id)",
			correlationID, env.Actor())
		h.metrics.BrokerDecision("denied")
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":       "workload_identity_mismatch",
			"secret_path": req.SecretPath,
		})
		return
	}

	// Step 1: REQUESTED is recorded regardless of outcome.
	h.recordRequested(r.Context(), req, correlationID)

	// Step 2: resolve the applicable policy version by secret_path.
	applicable, err := h.store.FindApplicableVersionByPath(r.Context(), req.SecretPath, req.TenantID, req.LegalEntityID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSecretPolicyNotFound), errors.Is(err, domain.ErrSecretPolicyVersionNotFound):
			// Step 3: deny-by-absence — no policy, or none ACTIVE for this
			// scope. secret_class is genuinely unknown here — no policy
			// was ever resolved to read it from.
			h.recordDenial(r.Context(), req, "", nil, "no applicable secret policy for this path/scope", correlationID, req.RequestedByPrincipalID)
			h.metrics.BrokerDecision("no_policy")
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_applicable_secret_policy", "secret_path": req.SecretPath})
		default:
			h.log.Error("Broker: store unavailable resolving policy", zap.String("correlation_id", correlationID), zap.Error(err))
			h.metrics.BrokerDecision("error")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	// Step 4: is this workload authorized?
	var allowedWorkloads []string
	if err := json.Unmarshal(applicable.AllowedWorkloadIDs, &allowedWorkloads); err != nil {
		h.log.Error("Broker: policy version has invalid allowed_workload_ids", zap.String("secret_policy_version_id", applicable.SecretPolicyVersionID), zap.Error(err))
		h.metrics.BrokerDecision("error")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "invalid_policy_payload"})
		return
	}
	if !contains(allowedWorkloads, req.RequestedByPrincipalID) {
		// secret_class IS known here — a policy was resolved, it just
		// didn't authorize this caller. Recording it keeps this DENIED
		// entry as complete evidence as a GRANTED one (context.md §5).
		h.recordDenial(r.Context(), req, applicable.SecretClass, &applicable.SecretPolicyVersionID, "requesting principal not in allowed_workload_ids", correlationID, req.RequestedByPrincipalID)
		h.metrics.BrokerDecision("denied")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access_denied", "secret_path": req.SecretPath})
		return
	}

	// Step 5: grant. Vault call happens before the durable write so a
	// vault failure never leaves a lease row with no token ever issued.
	// The expiry is fixed up front and signed into the lease token: since
	// the audit, a token is bound to the lease's own expiry, so an expired
	// (or later revoked) lease no longer vouches for access on paper.
	expiresAt := time.Now().UTC().Add(time.Duration(applicable.MaxLeaseDurationSeconds) * time.Second)
	leaseToken, err := h.vault.Get(r.Context(), req.SecretPath, expiresAt)
	if err != nil {
		h.log.Error("Broker: vault backend unavailable", zap.String("secret_path", req.SecretPath), zap.Error(err))
		h.metrics.VaultBackendError("get")
		h.metrics.BrokerDecision("vault_error")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "vault_backend_unavailable"})
		return
	}

	lease, created, err := h.store.CreateLease(r.Context(), domain.CreateLeaseParams{
		RequestID:              req.RequestID,
		SecretPolicyVersionID:  applicable.SecretPolicyVersionID,
		SecretClass:            applicable.SecretClass,
		SecretPath:             applicable.SecretPath,
		RequestedByPrincipalID: req.RequestedByPrincipalID,
		TenantID:               req.TenantID,
		LegalEntityID:          req.LegalEntityID,
		ExpiresAt:              expiresAt,
		CorrelationID:          correlationID,
	})
	if err != nil {
		h.log.Error("Broker: failed to create lease", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	if created {
		// Only a real transition is a new fact.
		spv := applicable.SecretPolicyVersionID
		if _, err := h.store.RecordAuditEntry(r.Context(), domain.RecordAuditEntryParams{
			EventType:              "GRANTED",
			SecretClass:            lease.SecretClass,
			SecretPath:             lease.SecretPath,
			RequestedByPrincipalID: lease.RequestedByPrincipalID,
			ActedByPrincipalID:     &lease.RequestedByPrincipalID,
			TenantID:               lease.TenantID,
			LegalEntityID:          lease.LegalEntityID,
			LeaseID:                &lease.LeaseID,
			SecretPolicyVersionID:  &spv,
			CorrelationID:          correlationID,
		}); err != nil {
			h.log.Error("Broker: failed to record GRANTED audit entry", zap.Error(err))
		}
		if err := h.publisher.PublishAccessGranted(r.Context(), *lease, correlationID); err != nil {
			h.log.Error("Broker: failed to publish secret.access.granted", zap.Error(err))
		}
	}

	h.metrics.BrokerDecision("granted")
	writeJSON(w, http.StatusOK, brokerResponse{
		LeaseID:    lease.LeaseID,
		SecretPath: lease.SecretPath,
		LeaseToken: leaseToken,
		ExpiresAt:  lease.ExpiresAt,
	})
}

// recordRequested records the REQUESTED fact every broker attempt produces,
// before any decision is taken.
func (h *Handler) recordRequested(ctx context.Context, req brokerRequest, correlationID string) {
	if err := h.publisher.PublishAccessRequested(ctx, req.SecretPath, req.RequestedByPrincipalID, correlationID); err != nil {
		h.log.Error("Broker: failed to publish secret.access.requested", zap.String("correlation_id", correlationID), zap.Error(err))
	}
	if _, err := h.store.RecordAuditEntry(ctx, domain.RecordAuditEntryParams{
		EventType:              "REQUESTED",
		SecretClass:            "",
		SecretPath:             req.SecretPath,
		RequestedByPrincipalID: req.RequestedByPrincipalID,
		// On the broker path the workload asks for its own access, so
		// actor and subject are the same principal. Recorded anyway
		// rather than left NULL: "who did this" must be answerable by
		// one predicate across all five event types, without the reader
		// having to know which ones happen to coincide.
		ActedByPrincipalID: &req.RequestedByPrincipalID,
		TenantID:           req.TenantID,
		LegalEntityID:      req.LegalEntityID,
		CorrelationID:      correlationID,
	}); err != nil {
		h.log.Error("Broker: failed to record REQUESTED audit entry", zap.String("correlation_id", correlationID), zap.Error(err))
	}
}

func (h *Handler) recordDenial(ctx context.Context, req brokerRequest, secretClass string, secretPolicyVersionID *string, detail, correlationID, actedBy string) {
	if _, err := h.store.RecordAuditEntry(ctx, domain.RecordAuditEntryParams{
		EventType:              "DENIED",
		SecretClass:            secretClass,
		SecretPath:             req.SecretPath,
		RequestedByPrincipalID: req.RequestedByPrincipalID,
		ActedByPrincipalID:     &actedBy,
		TenantID:               req.TenantID,
		LegalEntityID:          req.LegalEntityID,
		SecretPolicyVersionID:  secretPolicyVersionID,
		OutcomeDetail:          detail,
		CorrelationID:          correlationID,
	}); err != nil {
		h.log.Error("recordDenial: failed to record DENIED audit entry", zap.String("correlation_id", correlationID), zap.Error(err))
	}
}

func contains(list []string, val string) bool {
	for _, v := range list {
		if v == val {
			return true
		}
	}
	return false
}

// ── GET /v1/secrets/leases/{lease_id} ────────────────────────────────────────

func (h *Handler) GetLease(w http.ResponseWriter, r *http.Request) {
	leaseID, ok := requireUUIDParam(w, r, "lease_id")
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretLeaseRead) {
		return
	}

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	lease, err := h.store.FindLeaseByID(r.Context(), leaseID, tenantScope)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrLeaseNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "lease_not_found", "lease_id": leaseID})
		default:
			h.log.Error("GetLease: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	// A lease says which principal holds live access to which secret path. Any id
	// returned one, from any tenant. FindLeaseByID's own predicate is now the
	// primary control; this stays as the belt-and-suspenders check it always was.
	if h.refuseForeignRow(w, lease.TenantID, tenantScope, "lease_not_found", "lease_id", leaseID) {
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

// ── POST /v1/secrets/leases/{lease_id}/verify ────────────────────────────────

// VerifyLease is the redemption surface the audit said did not exist:
// "nothing consumes the token, so neither control [expiry, revoke] has
// effect." A lease token is now signed, bound to its secret path and to the
// lease's own expiry, and a holder can present it here and get a real
// answer. Expiry is rejected with no database read (the token itself
// refuses); revocation requires consulting the lease register the token
// is bound to — an expired token was real invalidation but a revoked lease
// still held a still-valid signature, so the register is the second gate.
//
// A malformed or mis-binding token answers 400 (a client fault). An
// expired or revoked lease answers 200 with valid=false: the caller asked
// "is this still good" and the honest answer is "no", with the reason.
func (h *Handler) VerifyLease(w http.ResponseWriter, r *http.Request) {
	leaseID, ok := requireUUIDParam(w, r, "lease_id")
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req verifyLeaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LeaseToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "lease_token"})
		return
	}

	info, err := h.vault.Verify(r.Context(), req.LeaseToken)
	if err != nil {
		// Expired tokens ARE validly signed; they are just past their
		// bound. Everything else is a client fault.
		if errors.Is(err, vault.ErrLeaseTokenExpired) {
			writeJSON(w, http.StatusOK, verifyLeaseResponse{
				Valid:     false,
				LeaseID:   leaseID,
				ExpiresAt: info.ExpiresAt,
				Reason:    "token_expired",
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_lease_token"})
		return
	}

	lease, err := h.store.FindLeaseByID(r.Context(), leaseID, tenantScope)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrLeaseNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "lease_not_found", "lease_id": leaseID})
		default:
			h.log.Error("VerifyLease: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	if h.refuseForeignRow(w, lease.TenantID, tenantScope, "lease_not_found", "lease_id", leaseID) {
		return
	}
	if lease.SecretPath != info.SecretPath {
		h.log.Warn("VerifyLease refused: token bound to a different secret path",
			zap.String("lease_id", leaseID),
			zap.String("token_secret_path", info.SecretPath),
			zap.String("lease_secret_path", lease.SecretPath),
			zap.String("correlation_id", correlationID),
		)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "lease_token_mismatch"})
		return
	}

	resp := verifyLeaseResponse{
		Valid:      true,
		LeaseID:    lease.LeaseID,
		SecretPath: lease.SecretPath,
		Status:     lease.Status,
		ExpiresAt:  info.ExpiresAt,
	}
	switch lease.Status {
	case "REVOKED":
		resp.Valid, resp.Reason = false, "lease_revoked"
	case "EXPIRED":
		resp.Valid, resp.Reason = false, "lease_expired"
	case "GRANTED":
		// Still within the token's bound (the backend already refused an
		// expired token), and the lease register says GRANTED: this is the
		// only path that answers true.
	default:
		resp.Valid, resp.Reason = false, "lease_"+lease.Status
	}
	writeJSON(w, http.StatusOK, resp)
}

type verifyLeaseRequest struct {
	LeaseToken string `json:"lease_token"`
}

type verifyLeaseResponse struct {
	Valid      bool      `json:"valid"`
	LeaseID    string    `json:"lease_id"`
	SecretPath string    `json:"secret_path,omitempty"`
	Status     string    `json:"status,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

// ── GET /v1/secrets/leases ───────────────────────────────────────────────────

func (h *Handler) ListLeases(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretLeaseRead) {
		return
	}

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	filter := store.LeaseListFilter{
		RequestedByPrincipalID: q.Get("principal"),
		SecretClass:            q.Get("secret_class"),
	}
	// The tenant filter was OPTIONAL, so omitting it listed every tenant's live
	// leases — who currently holds access to which secret path, platform-wide.
	// It is now the caller's verified scope, and a query parameter naming another
	// tenant is refused rather than ignored.
	if v := q.Get("tenant_id"); v != "" && v != tenantScope {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "tenant_scope_mismatch",
			"message": "request tenant_id does not match the caller's verified tenant scope",
		})
		return
	}
	filter.TenantID = &tenantScope
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_from"})
			return
		}
		filter.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_to"})
			return
		}
		filter.To = t
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Offset = n
		}
	}

	results, err := h.store.ListLeases(r.Context(), filter)
	if err != nil {
		h.log.Error("ListLeases: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if results == nil {
		results = []*domain.SecretLease{}
	}
	writeJSON(w, http.StatusOK, results)
}

// ── POST /v1/secrets/leases/{lease_id}/revoke ───────────────────────────────

func (h *Handler) RevokeLease(w http.ResponseWriter, r *http.Request) {
	leaseID, ok := requireUUIDParam(w, r, "lease_id")
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretLeaseRevoke) {
		return
	}
	// Read the lease before revoking it: any lease id could be revoked from any
	// tenant, and a revocation cannot be undone, so the scope check has to happen
	// BEFORE the transition rather than on the way out.
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	existing, err := h.store.FindLeaseByID(r.Context(), leaseID, tenantScope)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrLeaseNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "lease_not_found", "lease_id": leaseID})
		default:
			h.log.Error("RevokeLease: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "lease_not_found", "lease_id": leaseID})
		return
	}
	if h.refuseForeignRow(w, existing.TenantID, tenantScope, "lease_not_found", "lease_id", leaseID) {
		h.log.Warn("RevokeLease refused: lease belongs to another tenant",
			zap.String("lease_id", leaseID),
			zap.String("caller_tenant_id", tenantScope),
			zap.String("correlation_id", correlationID),
		)
		return
	}

	lease, transitioned, err := h.store.RevokeLease(r.Context(), leaseID, tenantScope)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrLeaseNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "lease_not_found", "lease_id": leaseID})
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition", "lease_id": leaseID})
		default:
			h.log.Error("RevokeLease: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	if transitioned {
		// Only on a real transition: an idempotent repeat revokes nothing and
		// must not inflate the count an operator reads during an incident.
		h.metrics.LeaseRevoked("explicit")
		spv := lease.SecretPolicyVersionID
		lid := lease.LeaseID
		if _, err := h.store.RecordAuditEntry(r.Context(), domain.RecordAuditEntryParams{
			EventType:   "REVOKED",
			SecretClass: lease.SecretClass,
			SecretPath:  lease.SecretPath,
			// Subject: the principal whose access this lease was.
			RequestedByPrincipalID: lease.RequestedByPrincipalID,
			// Actor: the operator ending it, which is a DIFFERENT
			// principal in every case this endpoint exists for. This is
			// the one place the two genuinely diverge, and until
			// migration 000004 the actor was authorized against
			// SECRET_LEASE_REVOKE and then dropped — so the audit trail
			// could not say who revoked a lease.
			ActedByPrincipalID:    &principalID,
			TenantID:              lease.TenantID,
			LegalEntityID:         lease.LegalEntityID,
			LeaseID:               &lid,
			SecretPolicyVersionID: &spv,
			CorrelationID:         correlationID,
		}); err != nil {
			h.log.Error("RevokeLease: failed to record REVOKED audit entry", zap.Error(err))
		}
	}
	writeJSON(w, http.StatusOK, revokeLeaseResponse{
		SecretLease:  lease,
		Transitioned: transitioned,
	})
}

// revokeLeaseResponse is the lease plus whether this call is what revoked it.
//
// Revoking an already-REVOKED lease is a 200 returning the record untouched
// (the store short-circuits on that status) and writes no second audit entry.
// Without this flag a first revoke and a repeat were indistinguishable, so a
// caller had to guess whether the REVOKED audit entry came from its own
// request. `revoked_at` cannot answer that — it is already set either way.
//
// Embedded by pointer so the existing lease fields are unchanged.
type revokeLeaseResponse struct {
	*domain.SecretLease
	Transitioned bool `json:"transitioned"`
}

// ── POST /v1/secret-policies/{id}/rotate ────────────────────────────────────

type rotateRequest struct {
	RequestID            string `json:"request_id"`
	RotatedByPrincipalID string `json:"rotated_by_principal_id"`
}

type rotateResponse struct {
	SecretPolicyID    string    `json:"secret_policy_id"`
	SecretPath        string    `json:"secret_path"`
	RevokedLeaseCount int       `json:"revoked_lease_count"`
	RotatedAt         time.Time `json:"rotated_at"`
}

// Rotate handles POST /v1/secret-policies/{secret_policy_id}/rotate.
// Idempotent on request_id via a partial unique index on
// secret_access_audit_log (context.md §7.2/§7.3). Also mass-revokes
// every currently-GRANTED lease for the policy's secret_path — the fix
// found during design review: rotating without revoking existing leases
// would leave old leases pointing at now-stale material.
//
// Known limitation: the revoke-leases step and the ROTATED audit write
// are two separate store calls, not one database transaction — a crash
// between them could leave leases revoked without a matching ROTATED
// record (or vice versa on the dedup check). Flagged here rather than
// silently assumed correct; acceptable for v1, worth a real transaction
// if this service's reliability bar rises later.
func (h *Handler) Rotate(w http.ResponseWriter, r *http.Request) {
	secretPolicyID, ok := requireUUIDParam(w, r, "secret_policy_id")
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// The rotating caller's verified tenant scope. Needed for the ROTATED
	// audit entry below — see the comment on that call.
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretRotate) {
		return
	}

	var req rotateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RequestID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "request_id"})
		return
	}
	if req.RotatedByPrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "rotated_by_principal_id"})
		return
	}

	// Idempotency check first — a retried rotate must not rotate twice.
	existingEntry, err := h.store.FindAuditEntryByRotationRequestID(r.Context(), req.RequestID)
	if err != nil {
		h.log.Error("Rotate: idempotency check failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if existingEntry != nil {
		// RevokedLeaseCount is read back out of the original entry's
		// outcome_detail. A replay used to answer 0 here, which reads as
		// "this rotation revoked nothing" — the opposite of what the first
		// call actually did, and materially misleading in an evidence trail
		// where the count is the whole point of the record.
		writeJSON(w, http.StatusOK, rotateResponse{
			SecretPolicyID:    secretPolicyID,
			SecretPath:        existingEntry.SecretPath,
			RevokedLeaseCount: revokedCountFromOutcomeDetail(existingEntry.OutcomeDetail),
			RotatedAt:         existingEntry.RecordedAt,
		})
		return
	}

	secretPath, revokedLeases, rotatedAt, err := h.PerformRotation(r.Context(), secretPolicyID, req.RotatedByPrincipalID, &tenantScope, req.RequestID, correlationID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSecretPolicyNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "secret_policy_not_found", "secret_policy_id": secretPolicyID})
		default:
			h.log.Error("Rotate: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	writeJSON(w, http.StatusOK, rotateResponse{
		SecretPolicyID:    secretPolicyID,
		SecretPath:        secretPath,
		RevokedLeaseCount: revokedLeases,
		RotatedAt:         rotatedAt,
	})
}

// PerformRotation is the shared core of POST /v1/secret-policies/{id}/rotate
// and the automated rotation sweeper (§13): rotate the vault material, mass
// revoke every GRANTED lease across every tenant, then record REVOKED and
// ROTATED audit evidence and publish secret.rotation.completed. Returns the
// policy's secret path, the number of leases revoked, and the rotation
// timestamp.
//
// rotatedByPrincipalID names the human/operator driving the rotation;
// correlationID ties it to the caller. For sweeper-driven rotations the
// actor is the sweeper's own system identity. The idempotency check on
// request_id is the caller's responsibility — PerformRotation is only ever
// called after a confirmed-new rotation request id.
func (h *Handler) PerformRotation(ctx context.Context, secretPolicyID, rotatedByPrincipalID string, tenantScope *string, requestID, correlationID string) (string, int, time.Time, error) {
	policy, err := h.store.FindSecretPolicyByID(ctx, secretPolicyID)
	if err != nil {
		return "", 0, time.Time{}, err
	}

	if err := h.vault.Rotate(ctx, policy.SecretPath); err != nil {
		h.log.Error("PerformRotation: vault backend rotate failed", zap.String("secret_path", policy.SecretPath), zap.Error(err))
		h.metrics.VaultBackendError("rotate")
		return "", 0, time.Time{}, fmt.Errorf("vault rotate: %w", err)
	}

	revokedLeases, err := h.store.RevokeLeasesBySecretPath(ctx, policy.SecretPath)
	if err != nil {
		h.log.Error("PerformRotation: failed to revoke leases", zap.String("correlation_id", correlationID), zap.Error(err))
		return "", 0, time.Time{}, err
	}
	for _, lease := range revokedLeases {
		spv := lease.SecretPolicyVersionID
		lid := lease.LeaseID
		if _, err := h.store.RecordAuditEntry(ctx, domain.RecordAuditEntryParams{
			EventType:   "REVOKED",
			SecretClass: lease.SecretClass,
			SecretPath:  lease.SecretPath,
			// Subject: the lease holder, who loses access here without
			// having asked for anything. Actor: the rotating operator.
			// These rows land in the HOLDER's tenant, which may not be
			// the rotator's, so without the actor column a tenant could
			// see that its lease died and not who did it.
			RequestedByPrincipalID: lease.RequestedByPrincipalID,
			ActedByPrincipalID:     &rotatedByPrincipalID,
			TenantID:               lease.TenantID,
			LegalEntityID:          lease.LegalEntityID,
			LeaseID:                &lid,
			SecretPolicyVersionID:  &spv,
			OutcomeDetail:          "revoked as a side effect of secret rotation",
			CorrelationID:          correlationID,
		}); err != nil {
			h.log.Error("PerformRotation: failed to record REVOKED audit entry for lease", zap.String("lease_id", lease.LeaseID), zap.Error(err))
		}
	}

	// TenantID is the rotating caller's verified scope. It used to be left
	// unset, so every ROTATED row landed with tenant_id = NULL — and
	// ListAuditLog always filters on the caller's tenant, so a rotation was
	// invisible in the audit log of every tenant, including the one that
	// performed it. The one event that invalidates every lease on a path
	// was the one event no one could retrieve evidence of.
	//
	// Bound to the rotating tenant rather than made globally visible: a
	// secret_path is a platform-wide address, so a NULL-tenant row readable
	// by everyone would let any tenant enumerate every other tenant's secret
	// paths and the principals administering them. Tenants other than the
	// rotator are not left without evidence — the mass revocation above
	// writes each of them a REVOKED row in their own scope, carrying
	// "revoked as a side effect of secret rotation".
	rotatedEntry, err := h.store.RecordAuditEntry(ctx, domain.RecordAuditEntryParams{
		EventType:   "ROTATED",
		SecretClass: policy.SecretClass,
		SecretPath:  policy.SecretPath,
		// Rotation has no access subject — nobody is asking to read the
		// material — so the rotator occupies both columns. Filling the
		// actor column keeps the "everything this principal did" query
		// complete rather than silently missing rotations.
		RequestedByPrincipalID: rotatedByPrincipalID,
		ActedByPrincipalID:     &rotatedByPrincipalID,
		TenantID:               tenantScope,
		RequestID:              &requestID,
		OutcomeDetail:          rotationOutcomeDetail(len(revokedLeases)),
		CorrelationID:          correlationID,
	})
	if err != nil {
		h.log.Error("PerformRotation: failed to record ROTATED audit entry", zap.String("correlation_id", correlationID), zap.Error(err))
		return "", 0, time.Time{}, err
	}

	if err := h.publisher.PublishRotationCompleted(ctx, secretPolicyID, policy.SecretPath, rotatedByPrincipalID, len(revokedLeases), correlationID); err != nil {
		h.log.Error("PerformRotation: failed to publish secret.rotation.completed", zap.Error(err))
	}

	h.metrics.SecretRotated(len(revokedLeases))
	return policy.SecretPath, len(revokedLeases), rotatedEntry.RecordedAt, nil
}

// rotationOutcomeDetail renders the number of leases a rotation revoked into
// the ROTATED entry's outcome_detail.
//
// The count is recorded because it is the part of a rotation that cannot be
// reconstructed afterwards: the REVOKED rows it produced are scattered across
// the tenants that held those leases, and an auditor reading one tenant's log
// can see its own revocations but never the size of the event that caused
// them. Stored as text in the existing free-form column rather than as a new
// typed column, so no migration is needed to make a replay answer honestly.
func rotationOutcomeDetail(revokedLeaseCount int) string {
	return fmt.Sprintf("%s%d", rotationOutcomePrefix, revokedLeaseCount)
}

// rotationOutcomePrefix is the machine-readable lead-in the count is parsed
// back out of. Kept deliberately boring — this string is written into an
// append-only evidence table, so changing it later would silently orphan the
// count on every row already recorded.
const rotationOutcomePrefix = "revoked_lease_count="

// revokedCountFromOutcomeDetail recovers the count written by
// rotationOutcomeDetail, returning 0 when the entry predates it or is not a
// rotation entry. 0 is the honest answer there: the original count was never
// recorded, so there is nothing to report.
func revokedCountFromOutcomeDetail(detail string) int {
	if !strings.HasPrefix(detail, rotationOutcomePrefix) {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(detail, rotationOutcomePrefix))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ── GET /v1/secrets/audit ─────────────────────────────────────────────────────

func (h *Handler) ListAuditLog(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretAuditRead) {
		return
	}

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// This filter had NO tenant field at all, so the secret-access audit log —
	// every REQUESTED, GRANTED, DENIED and REVOKED event, with principal and
	// secret path — was readable across every tenant by anything that could reach
	// the port.
	filter := store.AuditListFilter{
		RequestedByPrincipalID: q.Get("principal"),
		SecretPath:             q.Get("secret_path"),
		EventType:              q.Get("event_type"),
		TenantID:               &tenantScope,
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_from"})
			return
		}
		filter.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_to"})
			return
		}
		filter.To = t
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Offset = n
		}
	}

	results, err := h.store.ListAuditLog(r.Context(), filter)
	if err != nil {
		h.log.Error("ListAuditLog: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if results == nil {
		results = []*domain.SecretAccessAuditLog{}
	}
	writeJSON(w, http.StatusOK, results)
}

// requireUUIDParam reads a UUID-typed path parameter, answering 400 rather than
// letting a malformed value reach the store.
//
// Every id in this service's routes maps to a Postgres UUID column. A value
// that is not a UUID is rejected by the driver, which surfaces as a generic
// query error — and the handlers translate an unrecognised store error into
// 503 store_unavailable. So GET /v1/secrets/leases/not-a-uuid used to answer
// "this service is down" to what is purely a caller mistake: the client then
// retries, backs off, and trips an availability alert over a bad id. The value
// never reaches the database now, and the caller is told which parameter it got
// wrong.
//
// 400 rather than 404 on purpose: a syntactically invalid id is not a row that
// might exist, and reporting "not found" would tell a caller to go looking for
// something it can never have addressed. Well-formed ids that match no row
// still answer 404 through the normal store path.
func requireUUIDParam(w http.ResponseWriter, r *http.Request, param string) (string, bool) {
	raw := chi.URLParam(r, param)
	if _, err := uuid.Parse(raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_path_parameter",
			"field":   param,
			"message": param + " must be a UUID",
		})
		return "", false
	}
	return raw, true
}

// writeJSON serialises v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err
	}
}

// ── emergency material retrieval ─────────────────────────────────────────────

type emergencyRetrievalRequest struct {
	RequestID string `json:"request_id"`
	Reason    string `json:"reason"`
}

// emergencyRetrievalResponse returns the material itself, base64-encoded.
//
// This endpoint is the one place in the service the raw secret value can
// legitimately leave — a controlled break-glass path, gated on
// ActionSecretEmergencyRetrieval (a platform-scoped action, unlike
// everything else that touches material), recorded to the audit log as
// EMERGENCY_RETRIEVAL so the exception's own evidence trail is complete.
// Only ever reachable with a registered shared-secret exception (an
// override with reason + evidence reference) — without that combination
// the request is refused before the vault is ever asked.
func (h *Handler) EmergencyRetrieval(w http.ResponseWriter, r *http.Request) {
	secretPolicyID, ok := requireUUIDParam(w, r, "secret_policy_id")
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretEmergencyRetrieval) {
		return
	}

	var req emergencyRetrievalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RequestID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "request_id"})
		return
	}

	policy, err := h.store.FindSecretPolicyByID(r.Context(), secretPolicyID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSecretPolicyNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "secret_policy_not_found", "secret_policy_id": secretPolicyID})
		default:
			h.log.Error("EmergencyRetrieval: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	// Break-glass gate: an ACTIVE shared-secret exception must exist for this
	// path before the material may leave the vault. This is the register's
	// whole reason to exist — it is the documented, evidence-backed override
	// that lets a vault still fail safely (403) when an incident driver is
	// asking for something no one has authorized.
	exceptions, err := h.store.ListSharedSecretExceptions(r.Context(), domain.ListSharedSecretExceptionsFilter{
		SecretPath: policy.SecretPath,
		Status:     "ACTIVE",
		TenantID:   nil,
	})
	if err != nil {
		h.log.Error("EmergencyRetrieval: exception lookup failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	var activeException *domain.SharedSecretException
	for _, e := range exceptions {
		if !e.ExpiresAt.Before(time.Now()) {
			activeException = e
			break
		}
	}
	if activeException == nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "no_active_exception"})
		return
	}

	material, err := h.vault.GetMaterial(r.Context(), policy.SecretPath)
	if err != nil {
		h.log.Error("EmergencyRetrieval: vault backend get failed", zap.String("secret_path", policy.SecretPath), zap.Error(err))
		h.metrics.VaultBackendError("get_material")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "vault_backend_unavailable"})
		return
	}

	if _, err := h.store.RecordAuditEntry(r.Context(), domain.RecordAuditEntryParams{
		EventType:   "EMERGENCY_RETRIEVAL",
		SecretClass: policy.SecretClass,
		SecretPath:  policy.SecretPath,
		// The emergency actor requests and retrieves in the same act.
		RequestedByPrincipalID: principalID,
		ActedByPrincipalID:     &principalID,
		RequestID:              &req.RequestID,
		OutcomeDetail:          fmt.Sprintf("exception_id=%s reason=%q", activeException.ExceptionID, req.Reason),
		CorrelationID:          correlationID,
	}); err != nil {
		h.log.Error("EmergencyRetrieval: failed to record audit entry", zap.String("correlation_id", correlationID), zap.Error(err))
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"secret_policy_id": secretPolicyID,
		"secret_path":      policy.SecretPath,
		"material_base64":  base64.StdEncoding.EncodeToString(material),
	})
}

// ── shared-secret exception register ─────────────────────────────────────────

type createSharedSecretExceptionRequest struct {
	SecretPath        string `json:"secret_path"`
	Reason            string `json:"reason"`
	EvidenceReference string `json:"evidence_reference"`
	TenantID          *string `json:"tenant_id,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
}

func (req createSharedSecretExceptionRequest) missingField() string {
	switch {
	case req.SecretPath == "":
		return "secret_path"
	case req.Reason == "":
		return "reason"
	case req.EvidenceReference == "":
		return "evidence_reference"
	case req.ExpiresAt.IsZero():
		return "expires_at"
	default:
		return ""
	}
}

// CreateSharedSecretException registers one evidence-backed override.
// Requires SECRET_EXCEPTION_CREATE (platform-scoped), an evidence
// reference, and a time-boxed expiry that is still in the future.
func (h *Handler) CreateSharedSecretException(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretExceptionCreate) {
		return
	}

	var req createSharedSecretExceptionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	if !req.ExpiresAt.After(time.Now()) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "expires_at", "message": "must be in the future"})
		return
	}

	e, created, err := h.store.CreateSharedSecretException(r.Context(), domain.SharedSecretException{
		ExceptionID:          uuid.NewString(),
		SecretPath:           req.SecretPath,
		Reason:               req.Reason,
		EvidenceReference:    req.EvidenceReference,
		ApprovedByPrincipalID: principalID,
		TenantID:             req.TenantID,
		ExpiresAt:            req.ExpiresAt,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "shared_secret_exception_conflict"})
		default:
			h.log.Error("CreateSharedSecretException: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, e)
}

// ListSharedSecretExceptions answers the register, optionally filtered by
// status and secret_path. Requires SECRET_EXCEPTION_LIST.
func (h *Handler) ListSharedSecretExceptions(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretExceptionList) {
		return
	}

	q := r.URL.Query()
	results, err := h.store.ListSharedSecretExceptions(r.Context(), domain.ListSharedSecretExceptionsFilter{
		Status:     q.Get("status"),
		SecretPath: q.Get("secret_path"),
	})
	if err != nil {
		h.log.Error("ListSharedSecretExceptions: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if results == nil {
		results = []*domain.SharedSecretException{}
	}
	writeJSON(w, http.StatusOK, results)
}

// RevokeSharedSecretException closes an ACTIVE exception early (before its
// expiry) with an audit trail of who revoked it. Requires
// SECRET_EXCEPTION_REVOKE.
func (h *Handler) RevokeSharedSecretException(w http.ResponseWriter, r *http.Request) {
	exceptionID, ok := requireUUIDParam(w, r, "exception_id")
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", ActionSecretExceptionRevoke) {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	e, changed, err := h.store.RevokeSharedSecretException(r.Context(), exceptionID, tenantScope, principalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition", "except_id": exceptionID})
		default:
			h.log.Error("RevokeSharedSecretException: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	if !changed {
		writeJSON(w, http.StatusOK, map[string]string{"exception_id": exceptionID, "status": "already_revoked"})
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// ── authorization ────────────────────────────────────────────────────────────

// Action types this service asks authorization-svc about.
//
// These cover secret POLICY administration. POST /v1/secrets/broker is
// deliberately not in this list: brokering is this service's own gate — it
// evaluates the active secret policy for the requested path and issues a
// scoped, expiring lease — and putting a second, coarser RBAC check in front
// of it would obscure which decision actually refused an access.
//
// Read operations on policies, leases, and the audit log are also
// authorization-gated so that §5's "restricted data access remains
// authorization-required under masking" is enforced.
const (
	ActionSecretPolicyCreate          = "SECRET_POLICY_CREATE"
	ActionSecretPolicyVersionCreate   = "SECRET_POLICY_VERSION_CREATE"
	ActionSecretPolicyVersionActivate = "SECRET_POLICY_VERSION_ACTIVATE"
	ActionSecretMaterialWrite         = "SECRET_MATERIAL_WRITE"
	ActionSecretLeaseRevoke           = "SECRET_LEASE_REVOKE"
	ActionSecretRotate                = "SECRET_ROTATE"
	ActionSecretEmergencyRetrieval    = "SECRET_EMERGENCY_RETRIEVAL"
	ActionSecretExceptionCreate       = "SECRET_EXCEPTION_CREATE"
	ActionSecretExceptionList         = "SECRET_EXCEPTION_LIST"
	ActionSecretExceptionRevoke       = "SECRET_EXCEPTION_REVOKE"

	// Read actions
	ActionSecretPolicyList          = "SECRET_POLICY_LIST"
	ActionSecretPolicyVersionList   = "SECRET_POLICY_VERSION_LIST"
	ActionSecretLeaseRead           = "SECRET_LEASE_READ"
	ActionSecretAuditRead           = "SECRET_AUDIT_READ"
)

// requirePrincipal resolves the acting principal from the gateway-verified
// X-Principal-Id header, writing 401 and returning false when absent.
// requireTenant reads the caller's verified tenant scope from context (set by
// middleware.TenantContext from X-Tenant-Id).
//
// Nothing in this service used to read that header. Which secret policy applied,
// whose leases were listed, and whose secret-access audit log came back were all
// decided by values the request supplied about itself.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	if id := strings.TrimSpace(svcmiddleware.TenantFromContext(r.Context())); id != "" {
		return id, true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error":   "tenant_scope_missing",
		"message": "X-Tenant-Id is required — the gateway sets it from a verified identity envelope",
	})
	return "", false
}

// refuseForeignTenant reports whether claimed names a tenant other than the
// caller's verified scope, answering 403 if so. A nil or empty claimed value is
// not a disagreement: for secret policies nil means GLOBAL scope, a deliberate
// request that authorization rather than scoping decides on.
func (h *Handler) refuseForeignTenant(w http.ResponseWriter, claimed *string, tenantID string) bool {
	if claimed == nil || *claimed == "" || *claimed == tenantID {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error":   "tenant_scope_mismatch",
		"message": "request tenant_id does not match the caller's verified tenant scope",
	})
	return true
}

// refuseForeignRow reports whether a row this service found by id belongs to
// another tenant, answering 404 if so — the same answer as a row that does not
// exist, so an id-addressed route cannot be used to probe for another tenant's
// leases or policies. A row with no tenant at all is global and stays visible.
func (h *Handler) refuseForeignRow(w http.ResponseWriter, rowTenantID *string, tenantID string, notFoundError, idField, idValue string) bool {
	if rowTenantID == nil || *rowTenantID == "" || *rowTenantID == tenantID {
		return false
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": notFoundError, idField: idValue})
	return true
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	if id := strings.TrimSpace(r.Header.Get("X-Principal-Id")); id != "" {
		return id, true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error":   "missing_principal",
		"message": "X-Principal-Id is required — the gateway sets it from a verified identity envelope",
	})
	return "", false
}

// authorize fails closed on both a denial and an unobtainable decision.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	scope := h.authzPlatformScopeID
	if legalEntityID != "" {
		scope = legalEntityID
	}
	err := h.authz.CheckAllowed(r.Context(), principalID, scope, actionType)
	switch {
	case err == nil:
		h.metrics.AuthzDecision(actionType, "allowed")
		return true
	case errors.Is(err, authz.ErrDenied):
		h.metrics.AuthzDecision(actionType, "denied")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authorization_denied"})
	default:
		// "unavailable", not "denied": this service fails closed, so an
		// authorization-svc outage refuses every mutation. Without the
		// distinction that outage is indistinguishable in metrics from a
		// wave of legitimate denials.
		h.metrics.AuthzDecision(actionType, "unavailable")
		h.log.Error("authorization check failed — refusing the mutation",
			zap.String("principal_id", principalID),
			zap.String("action_type", actionType),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authz_unavailable"})
	}
	return false
}

// maxRequestBytes caps a JSON request body. A bare json.Decoder reads until EOF,
// so without this a single request can make the service allocate whatever the
// client is willing to send -- no auth needed, and nothing in the metrics to
// distinguish it from load.
const maxRequestBytes = 256 << 10 // 256 KiB

// decodeJSON reads a size-capped JSON body, answering 413 rather than 400 when
// the cap is what stopped it: "too large" and "malformed" are different faults
// and a caller can only act on the difference.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request_too_large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return false
	}
	return true
}
