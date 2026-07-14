package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/classification"
	"zoiko.io/secret-vault-integration-svc/internal/domain"
	"zoiko.io/secret-vault-integration-svc/internal/store"
)

// SecretVaultStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type SecretVaultStore interface {
	CreateSecretPolicy(ctx context.Context, params domain.CreateSecretPolicyParams) (*domain.SecretPolicy, bool, error)
	FindSecretPolicyByID(ctx context.Context, secretPolicyID string) (*domain.SecretPolicy, error)

	CreateSecretPolicyVersion(ctx context.Context, params domain.CreateSecretPolicyVersionParams) (*domain.SecretPolicyVersion, bool, error)
	FindSecretPolicyVersionByID(ctx context.Context, secretPolicyVersionID string) (*domain.SecretPolicyVersion, error)
	ActivateVersion(ctx context.Context, secretPolicyVersionID, actorID string) (*domain.SecretPolicyVersion, []*domain.SecretPolicyVersion, bool, error)
	ListVersionHistory(ctx context.Context, secretPolicyID string) ([]*domain.SecretPolicyVersion, error)

	FindApplicableVersions(ctx context.Context, secretClass string, tenantID, legalEntityID *string) ([]*domain.ApplicableSecretPolicyVersion, error)
	FindApplicableVersionByPath(ctx context.Context, secretPath string, tenantID, legalEntityID *string) (*domain.ApplicableSecretPolicyVersion, error)

	CreateLease(ctx context.Context, params domain.CreateLeaseParams) (*domain.SecretLease, bool, error)
	FindLeaseByID(ctx context.Context, leaseID string) (*domain.SecretLease, error)
	ListLeases(ctx context.Context, filter store.LeaseListFilter) ([]*domain.SecretLease, error)
	RevokeLease(ctx context.Context, leaseID string) (*domain.SecretLease, bool, error)
	RevokeLeasesBySecretPath(ctx context.Context, secretPath string) ([]*domain.SecretLease, error)

	RecordAuditEntry(ctx context.Context, params domain.RecordAuditEntryParams) (*domain.SecretAccessAuditLog, error)
	FindAuditEntryByRotationRequestID(ctx context.Context, requestID string) (*domain.SecretAccessAuditLog, error)
	ListAuditLog(ctx context.Context, filter store.AuditListFilter) ([]*domain.SecretAccessAuditLog, error)
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
	Get(ctx context.Context, secretPath string) (leaseToken string, err error)
	Put(ctx context.Context, secretPath string, material []byte) error
	Rotate(ctx context.Context, secretPath string) error
}

// EventPublisher is the narrow interface the handler depends on for
// publishing domain events.
type EventPublisher interface {
	PublishAccessRequested(ctx context.Context, secretPath, requestedByPrincipalID, correlationID string) error
	PublishAccessGranted(ctx context.Context, lease domain.SecretLease, correlationID string) error
	PublishRotationCompleted(ctx context.Context, secretPolicyID, secretPath string, revokedLeaseCount int, correlationID string) error
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store     SecretVaultStore
	vault     VaultBackend
	publisher EventPublisher
	log       *zap.Logger
}

// New constructs a Handler.
func New(store SecretVaultStore, vault VaultBackend, publisher EventPublisher, log *zap.Logger) *Handler {
	return &Handler{store: store, vault: vault, publisher: publisher, log: log}
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

	r.Post("/v1/secrets/broker", h.Broker)
	r.Get("/v1/secrets/leases/{lease_id}", h.GetLease)
	r.Get("/v1/secrets/leases", h.ListLeases)
	r.Post("/v1/secrets/leases/{lease_id}/revoke", h.RevokeLease)
	r.Get("/v1/secrets/audit", h.ListAuditLog)
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

	var req createSecretPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
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
	SecretPolicyVersionID  string          `json:"secret_policy_version_id,omitempty"`
	TenantID               *string         `json:"tenant_id,omitempty"`
	LegalEntityID          *string         `json:"legal_entity_id,omitempty"`
	AllowedWorkloadIDs     json.RawMessage `json:"allowed_workload_ids,omitempty"`
	MaxLeaseDurationSeconds int            `json:"max_lease_duration_seconds"`
	EffectiveFrom          time.Time       `json:"effective_from"`
	EffectiveTo            *time.Time      `json:"effective_to,omitempty"`
	CreatedByPrincipalID   string          `json:"created_by_principal_id"`
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

// CreateSecretPolicyVersion handles
// POST /v1/secret-policies/{secret_policy_id}/versions. New versions are
// always created in DRAFT status.
func (h *Handler) CreateSecretPolicyVersion(w http.ResponseWriter, r *http.Request) {
	secretPolicyID := chi.URLParam(r, "secret_policy_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createSecretPolicyVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
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

// ActivateVersion handles
// POST /v1/secret-policies/{secret_policy_id}/versions/{version_id}/activate.
func (h *Handler) ActivateVersion(w http.ResponseWriter, r *http.Request) {
	secretPolicyID := chi.URLParam(r, "secret_policy_id")
	versionID := chi.URLParam(r, "version_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req activateVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
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

	activated, _, _, err := h.store.ActivateVersion(r.Context(), versionID, req.ActivatedByPrincipalID)
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
	writeJSON(w, http.StatusOK, activated)
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
	secretPolicyID := chi.URLParam(r, "secret_policy_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req putSecretMaterialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
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

	if err := h.vault.Put(r.Context(), policy.SecretPath, material); err != nil {
		h.log.Error("PutSecretMaterial: vault backend put failed", zap.String("secret_path", policy.SecretPath), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "vault_backend_unavailable"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"secret_policy_id": secretPolicyID, "secret_path": policy.SecretPath, "status": "material_stored"})
}

// ── GET /v1/secret-policies/{id}/versions ───────────────────────────────────

// ListVersionHistory handles GET /v1/secret-policies/{secret_policy_id}/versions.
func (h *Handler) ListVersionHistory(w http.ResponseWriter, r *http.Request) {
	secretPolicyID := chi.URLParam(r, "secret_policy_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	results, err := h.store.ListVersionHistory(r.Context(), secretPolicyID)
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
	var tenantID, legalEntityID *string
	if v := q.Get("tenant_id"); v != "" {
		tenantID = &v
	}
	if v := q.Get("legal_entity_id"); v != "" {
		legalEntityID = &v
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = req.CorrelationID
	}

	// Step 1: REQUESTED is recorded regardless of outcome.
	if err := h.publisher.PublishAccessRequested(r.Context(), req.SecretPath, req.RequestedByPrincipalID, correlationID); err != nil {
		h.log.Error("Broker: failed to publish secret.access.requested", zap.String("correlation_id", correlationID), zap.Error(err))
	}
	if _, err := h.store.RecordAuditEntry(r.Context(), domain.RecordAuditEntryParams{
		EventType:              "REQUESTED",
		SecretClass:            "",
		SecretPath:             req.SecretPath,
		RequestedByPrincipalID: req.RequestedByPrincipalID,
		TenantID:               req.TenantID,
		LegalEntityID:          req.LegalEntityID,
		CorrelationID:          correlationID,
	}); err != nil {
		h.log.Error("Broker: failed to record REQUESTED audit entry", zap.String("correlation_id", correlationID), zap.Error(err))
	}

	// Step 2: resolve the applicable policy version by secret_path.
	applicable, err := h.store.FindApplicableVersionByPath(r.Context(), req.SecretPath, req.TenantID, req.LegalEntityID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSecretPolicyNotFound), errors.Is(err, domain.ErrSecretPolicyVersionNotFound):
			// Step 3: deny-by-absence — no policy, or none ACTIVE for this
			// scope. secret_class is genuinely unknown here — no policy
			// was ever resolved to read it from.
			h.recordDenial(r.Context(), req, "", nil, "no applicable secret policy for this path/scope", correlationID)
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_applicable_secret_policy", "secret_path": req.SecretPath})
		default:
			h.log.Error("Broker: store unavailable resolving policy", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	// Step 4: is this workload authorized?
	var allowedWorkloads []string
	if err := json.Unmarshal(applicable.AllowedWorkloadIDs, &allowedWorkloads); err != nil {
		h.log.Error("Broker: policy version has invalid allowed_workload_ids", zap.String("secret_policy_version_id", applicable.SecretPolicyVersionID), zap.Error(err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "invalid_policy_payload"})
		return
	}
	if !contains(allowedWorkloads, req.RequestedByPrincipalID) {
		// secret_class IS known here — a policy was resolved, it just
		// didn't authorize this caller. Recording it keeps this DENIED
		// entry as complete evidence as a GRANTED one (context.md §5).
		h.recordDenial(r.Context(), req, applicable.SecretClass, &applicable.SecretPolicyVersionID, "requesting principal not in allowed_workload_ids", correlationID)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access_denied", "secret_path": req.SecretPath})
		return
	}

	// Step 5: grant. Vault call happens before the durable write so a
	// vault failure never leaves a lease row with no token ever issued.
	leaseToken, err := h.vault.Get(r.Context(), req.SecretPath)
	if err != nil {
		h.log.Error("Broker: vault backend unavailable", zap.String("secret_path", req.SecretPath), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "vault_backend_unavailable"})
		return
	}

	expiresAt := time.Now().UTC().Add(time.Duration(applicable.MaxLeaseDurationSeconds) * time.Second)
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

	writeJSON(w, http.StatusOK, brokerResponse{
		LeaseID:    lease.LeaseID,
		SecretPath: lease.SecretPath,
		LeaseToken: leaseToken,
		ExpiresAt:  lease.ExpiresAt,
	})
}

func (h *Handler) recordDenial(ctx context.Context, req brokerRequest, secretClass string, secretPolicyVersionID *string, detail, correlationID string) {
	if _, err := h.store.RecordAuditEntry(ctx, domain.RecordAuditEntryParams{
		EventType:              "DENIED",
		SecretClass:            secretClass,
		SecretPath:             req.SecretPath,
		RequestedByPrincipalID: req.RequestedByPrincipalID,
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
	leaseID := chi.URLParam(r, "lease_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	lease, err := h.store.FindLeaseByID(r.Context(), leaseID)
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
	writeJSON(w, http.StatusOK, lease)
}

// ── GET /v1/secrets/leases ───────────────────────────────────────────────────

func (h *Handler) ListLeases(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	filter := store.LeaseListFilter{
		RequestedByPrincipalID: q.Get("principal"),
		SecretClass:            q.Get("secret_class"),
	}
	if v := q.Get("tenant_id"); v != "" {
		filter.TenantID = &v
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
	leaseID := chi.URLParam(r, "lease_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	lease, transitioned, err := h.store.RevokeLease(r.Context(), leaseID)
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
		spv := lease.SecretPolicyVersionID
		lid := lease.LeaseID
		if _, err := h.store.RecordAuditEntry(r.Context(), domain.RecordAuditEntryParams{
			EventType:              "REVOKED",
			SecretClass:            lease.SecretClass,
			SecretPath:             lease.SecretPath,
			RequestedByPrincipalID: lease.RequestedByPrincipalID,
			TenantID:               lease.TenantID,
			LegalEntityID:          lease.LegalEntityID,
			LeaseID:                &lid,
			SecretPolicyVersionID:  &spv,
			CorrelationID:          correlationID,
		}); err != nil {
			h.log.Error("RevokeLease: failed to record REVOKED audit entry", zap.Error(err))
		}
	}
	writeJSON(w, http.StatusOK, lease)
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
	secretPolicyID := chi.URLParam(r, "secret_policy_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req rotateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
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
		writeJSON(w, http.StatusOK, rotateResponse{
			SecretPolicyID: secretPolicyID,
			SecretPath:     existingEntry.SecretPath,
			RotatedAt:      existingEntry.RecordedAt,
		})
		return
	}

	policy, err := h.store.FindSecretPolicyByID(r.Context(), secretPolicyID)
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

	if err := h.vault.Rotate(r.Context(), policy.SecretPath); err != nil {
		h.log.Error("Rotate: vault backend rotate failed", zap.String("secret_path", policy.SecretPath), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "vault_backend_unavailable"})
		return
	}

	revokedLeases, err := h.store.RevokeLeasesBySecretPath(r.Context(), policy.SecretPath)
	if err != nil {
		h.log.Error("Rotate: failed to revoke leases", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	for _, lease := range revokedLeases {
		spv := lease.SecretPolicyVersionID
		lid := lease.LeaseID
		if _, err := h.store.RecordAuditEntry(r.Context(), domain.RecordAuditEntryParams{
			EventType:              "REVOKED",
			SecretClass:            lease.SecretClass,
			SecretPath:             lease.SecretPath,
			RequestedByPrincipalID: lease.RequestedByPrincipalID,
			TenantID:               lease.TenantID,
			LegalEntityID:          lease.LegalEntityID,
			LeaseID:                &lid,
			SecretPolicyVersionID:  &spv,
			OutcomeDetail:          "revoked as a side effect of secret rotation",
			CorrelationID:          correlationID,
		}); err != nil {
			h.log.Error("Rotate: failed to record REVOKED audit entry for lease", zap.String("lease_id", lease.LeaseID), zap.Error(err))
		}
	}

	rotatedEntry, err := h.store.RecordAuditEntry(r.Context(), domain.RecordAuditEntryParams{
		EventType:              "ROTATED",
		SecretClass:            policy.SecretClass,
		SecretPath:             policy.SecretPath,
		RequestedByPrincipalID: req.RotatedByPrincipalID,
		RequestID:              &req.RequestID,
		OutcomeDetail:          "",
		CorrelationID:          correlationID,
	})
	if err != nil {
		h.log.Error("Rotate: failed to record ROTATED audit entry", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	if err := h.publisher.PublishRotationCompleted(r.Context(), secretPolicyID, policy.SecretPath, len(revokedLeases), correlationID); err != nil {
		h.log.Error("Rotate: failed to publish secret.rotation.completed", zap.Error(err))
	}

	writeJSON(w, http.StatusOK, rotateResponse{
		SecretPolicyID:    secretPolicyID,
		SecretPath:        policy.SecretPath,
		RevokedLeaseCount: len(revokedLeases),
		RotatedAt:         rotatedEntry.RecordedAt,
	})
}

// ── GET /v1/secrets/audit ─────────────────────────────────────────────────────

func (h *Handler) ListAuditLog(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	filter := store.AuditListFilter{
		RequestedByPrincipalID: q.Get("principal"),
		SecretPath:             q.Get("secret_path"),
		EventType:              q.Get("event_type"),
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

// writeJSON serialises v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err
	}
}
