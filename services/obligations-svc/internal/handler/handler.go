package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/obligations-svc/internal/authz"
	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/envelope"
	svcerrors "zoiko.io/obligations-svc/internal/errors"
	"zoiko.io/obligations-svc/internal/jurisdiction"
	"zoiko.io/obligations-svc/internal/middleware"
	"zoiko.io/obligations-svc/internal/registry"
	"zoiko.io/obligations-svc/internal/store"
)

// ObligationStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type ObligationStore interface {
	CreateObligation(ctx context.Context, params domain.CreateObligationParams) (*domain.Obligation, bool, error)
	FindObligationByID(ctx context.Context, obligationID string) (*domain.Obligation, error)
	ListObligations(ctx context.Context, filter domain.ListObligationsFilter) ([]*domain.Obligation, error)
	UpdateObligationStatus(ctx context.Context, obligationID, newStatus string, expectedVersion int, actorID, correlationID string) (*domain.Obligation, bool, error)
	UpdateObligationWorkflow(ctx context.Context, obligationID, workflowID, actorID, correlationID string) (*domain.Obligation, error)
	UpdateObligation(ctx context.Context, params domain.UpdateObligationParams) (*domain.Obligation, bool, error)
	CreateFilingRequirement(ctx context.Context, params domain.CreateFilingRequirementParams) (*domain.FilingRequirement, error)
	ListFilingRequirements(ctx context.Context, obligationID string, limit int) ([]*domain.FilingRequirement, error)
	FindFilingRequirement(ctx context.Context, obligationID, filingRequirementID string) (*domain.FilingRequirement, error)
	UpdateFilingStatus(ctx context.Context, params domain.UpdateFilingStatusParams) (*domain.FilingRequirement, bool, error)
	ListFilingTransitions(ctx context.Context, obligationID, filingRequirementID string) ([]*domain.FilingTransition, error)

	// Idempotency key methods
	CheckIdempotencyKey(ctx context.Context, idempotencyKey, requestHash string) (bool, string, string, error)
	StoreIdempotencyKey(ctx context.Context, idempotencyKey, requestHash, resourceID, resourceType string) error

	// ── Chunk 10: applicability decisions ────────────────────────────────────
	CreateApplicabilityDecision(ctx context.Context, params domain.CreateApplicabilityDecisionParams) (*domain.ApplicabilityDecision, bool, error)
	ListApplicabilityDecisions(ctx context.Context, obligationID, jurisdictionCode, entityRef string, limit int) ([]*domain.ApplicabilityDecision, error)
	FindCurrentApplicability(ctx context.Context, obligationID, jurisdictionCode, entityRef string) (*domain.CurrentApplicability, error)

	// ── definitions and entity mappings (Doc 03 §8.5 "Owns") ─────────────────
	CreateDefinition(ctx context.Context, params domain.CreateDefinitionParams) (*domain.ObligationDefinition, bool, error)
	GetDefinition(ctx context.Context, id string) (*domain.ObligationDefinition, error)
	ListDefinitions(ctx context.Context, status string, limit int, afterCode string) ([]*domain.ObligationDefinition, error)
	ListDefinitionVersions(ctx context.Context, id string) ([]*domain.ObligationDefinitionVersion, error)
	NewDefinitionVersion(ctx context.Context, params domain.NewDefinitionVersionParams) (*domain.ObligationDefinition, error)
	RetireDefinition(ctx context.Context, id string, expectedVersion int, actor, correlationID string) (*domain.ObligationDefinition, error)
	CreateMapping(ctx context.Context, params domain.CreateMappingParams) (*domain.ObligationEntityMapping, error)
	ListMappings(ctx context.Context, definitionID string) ([]*domain.ObligationEntityMapping, error)
	FindMapping(ctx context.Context, definitionID, mappingID string) (*domain.ObligationEntityMapping, error)
	EndMapping(ctx context.Context, definitionID, mappingID string, effectiveTo time.Time, actor, correlationID string) (*domain.ObligationEntityMapping, error)
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store                 ObligationStore
	jurisdictionValidator jurisdiction.Validator
	authz                 authz.Client
	registry              registry.Checker
	log                   *zap.Logger
}

// New constructs a Handler. The legal-entity registry check is off until
// WithRegistry is called; main.go always calls it outside local/dev.
func New(store ObligationStore, jurisdictionValidator jurisdiction.Validator, authzClient authz.Client, log *zap.Logger) *Handler {
	return &Handler{
		store:                 store,
		jurisdictionValidator: jurisdictionValidator,
		authz:                 authzClient,
		registry:              registry.Noop{},
		log:                   log,
	}
}

// WithRegistry confirms every legal entity a write names against
// tenant-entity-registry-svc (exists, and in the caller's tenant).
func (h *Handler) WithRegistry(c registry.Checker) *Handler {
	h.registry = c
	return h
}

// checkEntity refuses a legal entity the registry does not hold in the
// caller's tenant (422), and fails closed when the registry cannot answer.
func (h *Handler) checkEntity(w http.ResponseWriter, r *http.Request, legalEntityID string) bool {
	tenantID := middleware.TenantFromContext(r.Context())
	err := h.registry.CheckEntity(r.Context(), tenantID, legalEntityID)
	switch {
	case err == nil:
		return true
	case errors.Is(err, domain.ErrLegalEntityNotFound):
		writeJSON(w, r, http.StatusUnprocessableEntity, map[string]string{
			"error": "legal_entity_not_found", "legal_entity_id": legalEntityID,
			"detail": "the legal entity does not exist in this tenant (tenant-entity-registry-svc)"})
	default:
		h.log.Error("legal entity check failed — failing closed", zap.String("legal_entity_id", legalEntityID), zap.Error(err))
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "registry_unavailable"})
	}
	return false
}

// ── request preconditions ────────────────────────────────────────────────────

// requirePrincipal refuses a request the gateway never identified.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeJSON(w, r, http.StatusUnauthorized, map[string]string{"error": "identity_missing"})
		return "", false
	}
	return principalID, true
}

// requireTenant refuses a request that carries no tenant scope.
//
// This service had no tenant dimension at all until 000002, so there was
// nothing to refuse: every read returned every tenant's obligations.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := middleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeJSON(w, r, http.StatusUnauthorized, map[string]string{"error": "tenant_missing"})
		return "", false
	}
	return tenantID, true
}

// authorize gates a mutation, failing closed.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, action string) bool {
	err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, r.Header.Get("X-Tenant-Id"), action, r.Header.Get("X-Correlation-ID"))
	if err == nil {
		return true
	}
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeJSON(w, r, http.StatusForbidden, map[string]string{"error": "forbidden", "action_type": action})
		return false
	}
	h.log.Error("authorization check failed — failing closed",
		zap.String("action_type", action), zap.Error(err))
	writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "authorization_unavailable"})
	return false
}

// maxRequestBytes bounds a request body. responsible_function and
// source_reference are TEXT columns with no width limit, so without a cap one
// request could stream unbounded memory into the decoder before any validation.
const maxRequestBytes = 256 << 10 // 256 KiB

// decodeJSON reads a JSON body with a size cap and no tolerance for unknown
// fields — a misspelled "due_dat" used to be discarded silently and answer
// "missing_field: due_date" for a field the caller believed they had sent.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, r, http.StatusRequestEntityTooLarge, map[string]string{"error": "request_too_large"})
			return false
		}
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return false
	}
	return true
}

// API standard L556-570: default page 50, maximum 200, cursor continuation.
const (
	defaultLimit = 50
	maxLimit     = 200
)

// parsePaging bounds a register read. A discarded strconv error is the
// platform's recurring shape: limit=abc silently defaulted, and offset=-1
// reached Postgres and answered 503. offset is still accepted for callers
// written before the cursor; new callers follow next_cursor.
func parsePaging(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = defaultLimit, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{
				"error": "invalid_limit", "detail": "limit must be between 1 and " + strconv.Itoa(maxLimit)})
			return 0, 0, false
		}
		limit = n
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{
				"error": "invalid_offset", "detail": "offset must be a non-negative integer"})
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// writeStoreErr maps a store failure to the status it deserves.
func (h *Handler) writeStoreErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	code := svcerrors.GetCode(err)
	if code != "" {
		writeJSON(w, r, h.statusForCode(code), map[string]string{"error": string(code), "detail": err.Error()})
		return
	}
	switch {
	case errors.Is(err, domain.ErrTenantMissing):
		writeJSON(w, r, http.StatusUnauthorized, map[string]string{"error": string(svcerrors.ErrCodeTenantMissing)})
	case errors.Is(err, domain.ErrObligationNotFound):
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": string(svcerrors.ErrCodeObligationNotFound)})
	case errors.Is(err, domain.ErrFilingRequirementNotFound):
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "filing_requirement_not_found"})
	case errors.Is(err, domain.ErrDefinitionNotFound):
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "obligation_definition_not_found"})
	case errors.Is(err, domain.ErrMappingNotFound):
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "entity_mapping_not_found"})
	case errors.Is(err, domain.ErrInvalidIdentifier):
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": string(svcerrors.ErrCodeInvalidIdentifier)})
	case errors.Is(err, domain.ErrInvalidField):
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": string(svcerrors.ErrCodeInvalidField), "detail": err.Error()})
	case errors.Is(err, domain.ErrInvalidTransition):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": string(svcerrors.ErrCodeInvalidTransition)})
	case errors.Is(err, domain.ErrVersionMismatch):
		writeJSON(w, r, http.StatusPreconditionFailed, map[string]string{"error": string(svcerrors.ErrCodeVersionMismatch), "detail": err.Error()})
	case errors.Is(err, domain.ErrObligationIDConflict):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "obligation_id_conflict", "detail": err.Error()})
	case errors.Is(err, domain.ErrIdempotencyKeyConflict):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "idempotency_key_conflict", "detail": "idempotency key already used with a different request"})
	case errors.Is(err, domain.ErrConflict):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "conflict", "detail": err.Error()})
	case errors.Is(err, domain.ErrObligationClosed):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "already_closed", "detail": err.Error()})
	case errors.Is(err, domain.ErrDefinitionRetired):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "obligation_definition_retired"})
	case errors.Is(err, domain.ErrDefinitionCodeConflict):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "definition_code_conflict", "detail": err.Error()})
	case errors.Is(err, domain.ErrMappingExists):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "entity_mapping_exists", "detail": err.Error()})
	case errors.Is(err, domain.ErrEntityNotMapped):
		writeJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": "entity_not_mapped", "detail": err.Error()})
	default:
		h.log.Error(what, zap.Error(err))
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": string(svcerrors.ErrCodeStoreUnavailable)})
	}
}

// statusForCode returns the appropriate HTTP status code for a stable error code.
func (h *Handler) statusForCode(code svcerrors.ErrorCode) int {
	switch code {
	case svcerrors.ErrCodeIdentityMissing, svcerrors.ErrCodeTenantMissing, svcerrors.ErrCodeEnvelopeIncomplete:
		return http.StatusUnauthorized
	case svcerrors.ErrCodeForbidden:
		return http.StatusForbidden
	case svcerrors.ErrCodeMissingField, svcerrors.ErrCodeInvalidField, svcerrors.ErrCodeInvalidJSON,
		svcerrors.ErrCodeUnknownField, svcerrors.ErrCodeRequestTooLarge, svcerrors.ErrCodeInvalidLimit,
		svcerrors.ErrCodeInvalidOffset, svcerrors.ErrCodeInvalidIfMatch, svcerrors.ErrCodeInvalidIdentifier:
		return http.StatusBadRequest
	case svcerrors.ErrCodeObligationNotFound, svcerrors.ErrCodeJurisdictionNotFound:
		return http.StatusNotFound
	case svcerrors.ErrCodeConflict, svcerrors.ErrCodeObligationConflict, svcerrors.ErrCodeIdempotencyKeyConflict,
		svcerrors.ErrCodeWorkflowAlreadyLinked, svcerrors.ErrCodeWorkflowNotLinked:
		return http.StatusConflict
	case svcerrors.ErrCodeInvalidTransition:
		return http.StatusConflict
	case svcerrors.ErrCodeVersionMismatch:
		return http.StatusPreconditionFailed
	case svcerrors.ErrCodeJurisdictionServiceUnavailable, svcerrors.ErrCodeAuthorizationUnavailable,
		svcerrors.ErrCodeStoreUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// RegisterRoutes mounts all routes on the given chi router.
// correlationIDMiddleware is applied at the router level so every response
// carries an X-Correlation-ID regardless of path (same convention as
// policy-svc and jurisdiction-rules-svc).
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	r.Post("/v1/obligations", h.CreateObligation)
	r.Get("/v1/obligations", h.ListObligations)
	r.Get("/v1/obligations/{obligation_id}", h.GetObligation)
	r.Post("/v1/obligations/{obligation_id}/status", h.UpdateObligationStatus)
	r.Post("/v1/obligations/{obligation_id}/workflow", h.LinkWorkflow)
	r.Delete("/v1/obligations/{obligation_id}/workflow", h.UnlinkWorkflow)
	r.Post("/v1/obligations/{obligation_id}/filing-requirements", h.CreateFilingRequirement)
	r.Get("/v1/obligations/{obligation_id}/filing-requirements", h.ListFilingRequirements)
	registerCompletionRoutes(r, h)
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// ── POST /v1/obligations ─────────────────────────────────────────────────────

// createObligationRequest is the wire shape for POST /v1/obligations.
// ObligationID is optional — callers may supply their own idempotency key.
type createObligationRequest struct {
	ObligationID         string    `json:"obligation_id,omitempty"`
	LegalEntityID        string    `json:"legal_entity_id"`
	JurisdictionID       string    `json:"jurisdiction_id"`
	ObligationSourceType string    `json:"obligation_source_type"`
	ObligationSourceID   string    `json:"obligation_source_id"`
	ObligationCode       string    `json:"obligation_code"`
	ObligationType       string    `json:"obligation_type"`
	DueDate              time.Time `json:"due_date"`
	SeverityLevel        string    `json:"severity_level"`
	ResponsibleFunction  string    `json:"responsible_function"`
	SourceReference      string    `json:"source_reference"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	WorkflowID           *string   `json:"workflow_id,omitempty"`

	// ObligationDefinitionID raises the obligation from a catalogue
	// definition. Fields the definition holds may then be omitted (they are
	// taken from its current version) or must equal it.
	ObligationDefinitionID string `json:"obligation_definition_id,omitempty"`
	DefinitionVersion      int    `json:"definition_version,omitempty"`
}

// applyDefinition fills the definition-held fields from version v, and names
// the first field the request sets to something else.
func (req *createObligationRequest) applyDefinition(v *domain.ObligationDefinitionVersion) string {
	fill := func(dst *string, src, name string) string {
		if *dst == "" {
			*dst = src
			return ""
		}
		if *dst != src {
			return name
		}
		return ""
	}
	for _, f := range []struct {
		dst       *string
		src, name string
	}{
		{&req.JurisdictionID, v.JurisdictionID, "jurisdiction_id"},
		{&req.ObligationType, v.ObligationType, "obligation_type"},
		{&req.ObligationSourceType, v.ObligationSourceType, "obligation_source_type"},
		{&req.ObligationSourceID, v.ObligationSourceID, "obligation_source_id"},
		{&req.SourceReference, v.SourceReference, "source_reference"},
		{&req.SeverityLevel, v.SeverityLevel, "severity_level"},
		{&req.ResponsibleFunction, v.ResponsibleFunction, "responsible_function"},
	} {
		if bad := fill(f.dst, f.src, f.name); bad != "" {
			return bad
		}
	}
	return ""
}

func (req createObligationRequest) missingField() string {
	switch {
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.JurisdictionID == "":
		return "jurisdiction_id"
	case req.ObligationSourceType == "":
		return "obligation_source_type"
	case req.ObligationSourceID == "":
		return "obligation_source_id"
	case req.ObligationCode == "":
		return "obligation_code"
	case req.ObligationType == "":
		return "obligation_type"
	case req.DueDate.IsZero():
		return "due_date"
	case req.SeverityLevel == "":
		return "severity_level"
	case req.ResponsibleFunction == "":
		return "responsible_function"
	case req.SourceReference == "":
		// Atomic Linking — every obligation must point to its originating
		// source. Enforced here, not just at the DB NOT NULL level, so the
		// caller gets a clear 400 instead of a generic 503/constraint error.
		return "source_reference"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

// CreateObligation handles POST /v1/obligations.
//
// Validates jurisdiction_id against jurisdiction-rules-svc before
// persisting — critical constraint (03-microservices.md §8.5): every
// obligation must be jurisdiction-bound. Fails closed: an unreachable
// jurisdiction-rules-svc rejects the write (503), it does not silently
// accept an unvalidated jurisdiction_id.
//
// Idempotent on obligation_code: a repeat POST with identical attributes
// returns 200 instead of creating a duplicate row. A repeat POST with the
// same code but different attributes returns 409.
//
// Response:
//
//	201 → obligation created for the first time
//	200 → obligation_code already existed with identical attributes; no-op, OR idempotent replay
//	400 → missing required field
//	404 → jurisdiction_id does not exist
//	409 → obligation_code already exists with differing attributes, OR idempotency key conflict
//	503 → store or jurisdiction-rules-svc unavailable
func (h *Handler) CreateObligation(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	// Read raw body for idempotency key hash computation
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeStoreErr(w, r, "CreateObligation: failed to read body", err)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	env, _ := envelope.FromContext(r.Context())
	idempotencyKey := env.IdempotencyKey

	// Check idempotency key if present
	if idempotencyKey != "" {
		requestHash := store.ComputeRequestHash(bodyBytes)
		alreadyProcessed, existingResourceID, existingResourceType, err := h.store.CheckIdempotencyKey(r.Context(), idempotencyKey, requestHash)
		if err != nil {
			h.writeStoreErr(w, r, "CreateObligation: idempotency check failed", err)
			return
		}
		if alreadyProcessed && existingResourceType == "obligation" {
			// Replay: fetch and return the existing obligation
			existing, err := h.store.FindObligationByID(r.Context(), existingResourceID)
			if err != nil {
				h.writeStoreErr(w, r, "CreateObligation: replay lookup failed", err)
				return
			}
			h.log.Info("idempotent replay: obligation already exists",
				zap.String("obligation_id", existing.ObligationID),
				zap.String("idempotency_key", idempotencyKey),
				zap.String("correlation_id", correlationID),
			)
			writeJSON(w, r, http.StatusOK, existing)
			return
		}
		if alreadyProcessed && existingResourceType != "obligation" {
			// Key used for different resource type
			writeJSON(w, r, http.StatusConflict, map[string]string{
				"error":  "idempotency_key_conflict",
				"detail": "idempotency key already used for different resource type",
			})
			return
		}
	}

	var req createObligationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ObligationDefinitionID != "" {
		def, err := h.store.GetDefinition(r.Context(), req.ObligationDefinitionID)
		if err != nil {
			h.writeStoreErr(w, r, "CreateObligation: definition lookup failed", err)
			return
		}
		if def.DefinitionStatus != "ACTIVE" {
			writeJSON(w, r, http.StatusConflict, map[string]string{"error": "obligation_definition_retired"})
			return
		}
		if req.DefinitionVersion != 0 && req.DefinitionVersion != def.CurrentVersion {
			writeJSON(w, r, http.StatusConflict, map[string]any{"error": "version_mismatch",
				"detail": "definition_version is not the definition's current version", "current_version": def.CurrentVersion})
			return
		}
		if bad := req.applyDefinition(def.Current); bad != "" {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "definition_mismatch", "field": bad,
				"detail": "the field differs from the definition's current version; omit it or send the definition's value"})
			return
		}
		req.DefinitionVersion = def.CurrentVersion
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}
	// Envelope LegalEntityID (from X-Legal-Entity-Id header) must match the
	// body's legal_entity_id. The envelope is required on writes (contract.go:
	// LegalEntityID: RequiredOnWrite) and the body field is required by the
	// request schema. A mismatch indicates a confused or malicious caller.
	if env.LegalEntityID != "" && env.LegalEntityID != req.LegalEntityID {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":        "legal_entity_mismatch",
			"detail":       "X-Legal-Entity-Id header must match body legal_entity_id",
			"header_value": env.LegalEntityID,
			"body_value":   req.LegalEntityID,
		})
		return
	}
	// created_by_principal_id is accepted so the console's existing payload is
	// not a 400, but it must name the caller. Taking it from the body would
	// make the record of who raised a statutory obligation self-declared —
	// the same defect board-resolutions-svc had, where a body-supplied
	// created_by defeated the segregation-of-duties check outright.
	if req.CreatedByPrincipalID != principalID {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "created_by_mismatch",
			"detail": "created_by_principal_id must be the authenticated principal — attribution comes from X-Principal-Id, not the request body",
		})
		return
	}

	// Authorized against the obligation's own legal entity: the authority to
	// raise an obligation is entity-scoped, not platform-wide.
	if !h.authorize(w, r, principalID, req.LegalEntityID, authz.ActionObligationCreate) {
		return
	}
	// Entity-bound (Doc 03 §8.5): the entity must exist in this tenant.
	if !h.checkEntity(w, r, req.LegalEntityID) {
		return
	}

	if err := h.jurisdictionValidator.ValidateExists(r.Context(), req.JurisdictionID); err != nil {
		switch {
		case errors.Is(err, domain.ErrJurisdictionNotFound):
			writeJSON(w, r, http.StatusNotFound, map[string]string{
				"error":           "jurisdiction_not_found",
				"jurisdiction_id": req.JurisdictionID,
			})
		default:
			h.log.Error("CreateObligation: jurisdiction validation failed",
				zap.String("jurisdiction_id", req.JurisdictionID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "jurisdiction_service_unavailable"})
		}
		return
	}

	params := domain.CreateObligationParams{
		ObligationID:         req.ObligationID,
		LegalEntityID:        req.LegalEntityID,
		JurisdictionID:       req.JurisdictionID,
		ObligationSourceType: req.ObligationSourceType,
		ObligationSourceID:   req.ObligationSourceID,
		ObligationCode:       req.ObligationCode,
		ObligationType:       req.ObligationType,
		DueDate:              req.DueDate,
		SeverityLevel:        req.SeverityLevel,
		ResponsibleFunction:  req.ResponsibleFunction,
		SourceReference:      req.SourceReference,
		CreatedByPrincipalID: principalID,
		WorkflowID:           req.WorkflowID,
		CorrelationID:        correlationID,

		ObligationDefinitionID: req.ObligationDefinitionID,
		DefinitionVersion:      req.DefinitionVersion,
	}

	o, created, err := h.store.CreateObligation(r.Context(), params)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			writeJSON(w, r, http.StatusConflict, map[string]string{
				"error":           "obligation_conflict",
				"obligation_code": req.ObligationCode,
			})
		case errors.Is(err, domain.ErrIdempotencyKeyConflict):
			writeJSON(w, r, http.StatusConflict, map[string]string{
				"error":  "idempotency_key_conflict",
				"detail": "idempotency key already used with different request",
			})
		case errors.Is(err, domain.ErrVersionMismatch):
			// The definition was superseded between the read and the pin.
			writeJSON(w, r, http.StatusConflict, map[string]string{"error": "version_mismatch",
				"detail": "the definition changed while the obligation was being raised; retry"})
		default:
			h.writeStoreErr(w, r, "CreateObligation: store unavailable", err)
		}
		return
	}

	// Store idempotency key after successful creation
	if idempotencyKey != "" {
		requestHash := store.ComputeRequestHash(bodyBytes)
		if err := h.store.StoreIdempotencyKey(r.Context(), idempotencyKey, requestHash, o.ObligationID, "obligation"); err != nil {
			h.log.Error("CreateObligation: failed to store idempotency key",
				zap.String("obligation_id", o.ObligationID),
				zap.String("idempotency_key", idempotencyKey),
				zap.Error(err),
			)
			// Non-fatal: log but continue
		}
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		// obligation.created was written to event_outbox in the same
		// transaction as the insert; the relay publishes it after commit.
		w.Header().Set("Location", "/v1/obligations/"+o.ObligationID)
	}
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(o.Version)))
	h.log.Info("obligation created",
		zap.String("obligation_id", o.ObligationID),
		zap.String("obligation_code", o.ObligationCode),
		zap.Bool("created", created),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, r, status, o)
}

// ── GET /v1/obligations/{obligation_id} ──────────────────────────────────────

// GetObligation handles GET /v1/obligations/{obligation_id}.
//
// Response:
//
//	200 → the Obligation
//	404 → obligation_id not found
//	403 → principal holds no OBLIGATION_READ grant on the entity
//	503 → store unavailable
func (h *Handler) GetObligation(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	o, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, r, "GetObligation: store unavailable", err)
		return
	}

	if !h.authorize(w, r, principalID, o.LegalEntityID, authz.ActionObligationRead) {
		return
	}

	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(o.Version)))
	writeJSON(w, r, http.StatusOK, o)
}

// ── GET /v1/obligations ──────────────────────────────────────────────────────

// ListObligations handles GET /v1/obligations.
//
// Query parameters (all optional, combined with AND):
//
//	legal_entity_id  (required for authorization)
//	jurisdiction_id
//	obligation_type
//	status
//	due_before       RFC3339 timestamp
//	due_after        RFC3339 timestamp
//
// Response:
//
//	200 → JSON array of Obligation (may be empty)
//	400 → legal_entity_id filter required for authorization, or due_before/due_after not valid RFC3339
//	403 → principal holds no OBLIGATION_READ grant on the entity
//	503 → store unavailable
func (h *Handler) ListObligations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}

	// Require legal_entity_id filter for object-level authorization.
	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "legal_entity_id query parameter required for authorization",
		})
		return
	}
	if !h.authorize(w, r, principalID, legalEntityID, authz.ActionObligationRead) {
		return
	}

	filter := domain.ListObligationsFilter{
		Limit:          limit,
		Offset:         offset,
		LegalEntityID:  legalEntityID,
		JurisdictionID: q.Get("jurisdiction_id"),
		ObligationType: q.Get("obligation_type"),
		Status:         q.Get("status"),
	}

	if v := q.Get("due_before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "due_before"})
			return
		}
		filter.DueBefore = &t
	}
	if v := q.Get("due_after"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "due_after"})
			return
		}
		filter.DueAfter = &t
	}
	if filter.Status != "" && !domain.ObligationStatuses[filter.Status] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "status"})
		return
	}
	if c := q.Get("cursor"); c != "" {
		at, id, ok := decodeCursor(c)
		if !ok || offset != 0 {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "cursor",
				"detail": "cursor must be a next_cursor this endpoint returned, and cannot be combined with offset"})
			return
		}
		filter.AfterCreatedAt, filter.AfterID = &at, id
	}

	results, err := h.store.ListObligations(r.Context(), filter)
	if err != nil {
		h.writeStoreErr(w, r, "ListObligations: store unavailable", err)
		return
	}

	// Always return an array — never null. A full page carries the cursor
	// for the next one (headers, so the body stays the array callers read).
	if results == nil {
		results = []*domain.Obligation{}
	}
	if len(results) == limit {
		last := results[len(results)-1]
		next := encodeCursor(last.CreatedAt, last.ObligationID)
		nq := r.URL.Query()
		nq.Set("cursor", next)
		nq.Del("offset")
		w.Header().Set("X-Next-Cursor", next)
		w.Header().Set("Link", "<"+r.URL.Path+"?"+nq.Encode()+`>; rel="next"`)
	}
	writeJSON(w, r, http.StatusOK, results)
}

// encodeCursor / decodeCursor carry the keyset position of a list page:
// (created_at, id) of its last row, opaque to the caller.
func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeCursor(c string) (time.Time, string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", false
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return time.Time{}, "", false
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", false
	}
	return at, id, true
}

// ── POST /v1/obligations/{obligation_id}/status ─────────────────────────────

type updateStatusRequest struct {
	ObligationStatus string `json:"obligation_status"`
}

// UpdateObligationStatus handles POST /v1/obligations/{obligation_id}/status.
//
// Transitions the obligation's status per the legal state machine: OPEN can
// move to IN_PROGRESS, OVERDUE, or CLOSED; IN_PROGRESS can move to OVERDUE
// or CLOSED; OVERDUE can move to CLOSED; CLOSED is terminal. Idempotent:
// requesting the status the obligation is already in returns 200 unchanged,
// not an error.
//
// Publishes obligation.updated on every real transition, plus
// obligation.overdue or obligation.closed when the new status is OVERDUE or
// CLOSED respectively.
//
// v1 scope note: there is no built-in scheduler that calls this endpoint
// automatically when due_date passes — an external caller (cron job,
// orchestrator) is expected to detect overdue obligations (e.g. via
// GET /v1/obligations?status=OPEN&due_before=<now>) and drive the
// OPEN/IN_PROGRESS -> OVERDUE transition itself. Building a scheduler is
// out of scope for this service.
//
// Response:
//
//	200 → transitioned (or already in that status — idempotent no-op)
//	400 → missing/invalid obligation_status
//	404 → obligation_id not found
//	409 → illegal transition
//	412 → version mismatch (If-Match/expected_version)
//	503 → store unavailable
func (h *Handler) UpdateObligationStatus(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	var req updateStatusRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ObligationStatus == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "obligation_status",
		})
		return
	}
	// An unknown status is a malformed request (400), not an illegal
	// transition between real states (409) — "FOO" used to answer 409.
	if !domain.ObligationStatuses[req.ObligationStatus] {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error":  "invalid_field",
			"field":  "obligation_status",
			"detail": "obligation_status must be one of OPEN, IN_PROGRESS, OVERDUE, CLOSED",
		})
		return
	}

	// Parse If-Match header for optimistic locking (expected_version).
	// If-Match: "123" or If-Match: W/"123" (weak etag not supported, treat as strong).
	expectedVersion := 0
	if match := r.Header.Get("If-Match"); match != "" {
		// Strip weak prefix if present
		match = strings.TrimPrefix(match, "W/")
		match = strings.Trim(match, `"`)
		if v, err := strconv.Atoi(match); err == nil {
			expectedVersion = v
		} else {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{
				"error":  "invalid_if_match",
				"detail": "If-Match must be an integer version",
			})
			return
		}
	}

	// The obligation is read before the write purely to learn its legal
	// entity, which is the authorization scope. The transition itself is still
	// decided under a row lock inside the store — this read is not the check.
	existing, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, r, "UpdateObligationStatus: lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, authz.ActionObligationStatusUpdate) {
		return
	}

	updated, transitioned, err := h.store.UpdateObligationStatus(r.Context(), obligationID, req.ObligationStatus, expectedVersion, principalID, correlationID)
	if err != nil {
		h.writeStoreErr(w, r, "UpdateObligationStatus: store unavailable", err)
		return
	}

	// obligation.updated (and overdue/closed when applicable) are written to
	// event_outbox inside the same transaction as the status change.
	// Publishing directly here would both duplicate those records and lose
	// the event whenever Kafka was down after the commit.

	h.log.Info("obligation status updated",
		zap.String("obligation_id", obligationID),
		zap.String("new_status", req.ObligationStatus),
		zap.Bool("transitioned", transitioned),
		zap.String("correlation_id", correlationID),
	)
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(updated.Version)))
	writeJSON(w, r, http.StatusOK, updated)
}

// ── POST /v1/obligations/{obligation_id}/workflow ───────────────────────────

type linkWorkflowRequest struct {
	WorkflowID string `json:"workflow_id"`
}

func (req linkWorkflowRequest) missingField() string {
	if req.WorkflowID == "" {
		return "workflow_id"
	}
	return ""
}

// LinkWorkflow handles POST /v1/obligations/{obligation_id}/workflow.
// Links an obligation to a workflow instance.
//
// Response:
//
//	200 → obligation updated with workflow_id
//	400 → missing workflow_id
//	404 → obligation_id not found
//	409 → obligation already linked to a workflow
//	503 → store unavailable
func (h *Handler) LinkWorkflow(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	var req linkWorkflowRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}

	existing, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, r, "LinkWorkflow: lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, authz.ActionObligationStatusUpdate) {
		return
	}

	if existing.WorkflowID != nil && *existing.WorkflowID != "" {
		writeJSON(w, r, http.StatusConflict, map[string]string{
			"error":       "workflow_already_linked",
			"workflow_id": *existing.WorkflowID,
		})
		return
	}

	updated, err := h.store.UpdateObligationWorkflow(r.Context(), obligationID, req.WorkflowID, principalID, correlationID)
	if err != nil {
		h.writeStoreErr(w, r, "LinkWorkflow: store unavailable", err)
		return
	}

	h.log.Info("obligation linked to workflow",
		zap.String("obligation_id", obligationID),
		zap.String("workflow_id", req.WorkflowID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, r, http.StatusOK, updated)
}

// ── DELETE /v1/obligations/{obligation_id}/workflow ─────────────────────────

// UnlinkWorkflow handles DELETE /v1/obligations/{obligation_id}/workflow.
// Removes the workflow link from an obligation.
//
// Response:
//
//	200 → obligation updated with workflow_id cleared
//	404 → obligation_id not found
//	409 → obligation not linked to a workflow
//	503 → store unavailable
func (h *Handler) UnlinkWorkflow(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	existing, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, r, "UnlinkWorkflow: lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, existing.LegalEntityID, authz.ActionObligationStatusUpdate) {
		return
	}

	if existing.WorkflowID == nil || *existing.WorkflowID == "" {
		writeJSON(w, r, http.StatusConflict, map[string]string{
			"error": "workflow_not_linked",
		})
		return
	}

	updated, err := h.store.UpdateObligationWorkflow(r.Context(), obligationID, "", principalID, correlationID)
	if err != nil {
		h.writeStoreErr(w, r, "UnlinkWorkflow: store unavailable", err)
		return
	}

	h.log.Info("obligation unlinked from workflow",
		zap.String("obligation_id", obligationID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, r, http.StatusOK, updated)
}

// ── POST /v1/obligations/{obligation_id}/filing-requirements ───────────────

type createFilingRequirementRequest struct {
	FilingRequirementID string `json:"filing_requirement_id,omitempty"`
	FilingType          string `json:"filing_type"`
	FilingAuthority     string `json:"filing_authority"`
	SubmissionChannel   string `json:"submission_channel"`
}

func (req createFilingRequirementRequest) missingField() string {
	switch {
	case req.FilingType == "":
		return "filing_type"
	case req.FilingAuthority == "":
		return "filing_authority"
	case req.SubmissionChannel == "":
		return "submission_channel"
	default:
		return ""
	}
}

// CreateFilingRequirement handles
// POST /v1/obligations/{obligation_id}/filing-requirements.
//
// Response:
//
//	201 → filing requirement created
//	400 → missing required field
//	404 → obligation_id not found
//	503 → store unavailable
func (h *Handler) CreateFilingRequirement(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	var req createFilingRequirementRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}

	// The parent obligation carries the legal entity this filing is authorized
	// against — a filing requirement has no entity of its own.
	parent, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, r, "CreateFilingRequirement: lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, parent.LegalEntityID, authz.ActionFilingRequirementAdd) {
		return
	}

	f, err := h.store.CreateFilingRequirement(r.Context(), domain.CreateFilingRequirementParams{
		FilingRequirementID: req.FilingRequirementID,
		ObligationID:        obligationID,
		FilingType:          req.FilingType,
		FilingAuthority:     req.FilingAuthority,
		SubmissionChannel:   req.SubmissionChannel,
	})
	if err != nil {
		h.writeStoreErr(w, r, "CreateFilingRequirement: store unavailable", err)
		return
	}

	h.log.Info("filing requirement created",
		zap.String("obligation_id", obligationID),
		zap.String("filing_requirement_id", f.FilingRequirementID),
		zap.String("correlation_id", correlationID),
	)
	w.Header().Set("Location", "/v1/obligations/"+obligationID+"/filing-requirements/"+f.FilingRequirementID)
	writeJSON(w, r, http.StatusCreated, f)
}

// ── GET /v1/obligations/{obligation_id}/filing-requirements ────────────────

// ListFilingRequirements handles
// GET /v1/obligations/{obligation_id}/filing-requirements.
//
// Response:
//
//	200 → JSON array of FilingRequirement (may be empty)
//	404 → obligation_id not found
//	403 → principal holds no FILING_REQUIREMENT_READ grant on the entity
//	503 → store unavailable
func (h *Handler) ListFilingRequirements(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	parent, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, r, "ListFilingRequirements: lookup failed", err)
		return
	}

	if !h.authorize(w, r, principalID, parent.LegalEntityID, authz.ActionFilingRequirementRead) {
		return
	}
	limit, _, ok := parsePaging(w, r)
	if !ok {
		return
	}

	results, err := h.store.ListFilingRequirements(r.Context(), obligationID, limit)
	if err != nil {
		h.writeStoreErr(w, r, "ListFilingRequirements: store unavailable", err)
		return
	}

	if results == nil {
		results = []*domain.FilingRequirement{}
	}
	writeJSON(w, r, http.StatusOK, results)
}

// writeJSON serialises v as JSON and writes it to w with the given status code.
// If status >= 400 and v is a map with an "error" key, it renders an RFC 9457
// problem+json via apicontract.WriteProblem instead.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	if status >= 400 {
		var m map[string]any
		switch t := v.(type) {
		case map[string]any:
			m = t
		case map[string]string:
			m = make(map[string]any, len(t))
			for k, s := range t {
				m[k] = s
			}
		}
		if code, _ := m["error"].(string); code != "" {
			// error becomes code, detail (or the offending field) becomes
			// detail, and every other member (obligation_id, header_value, …)
			// stays on the problem as an extension member.
			p := apicontract.Problem{Status: status, Code: code, Extra: map[string]any{}}
			p.Detail, _ = m["detail"].(string)
			if p.Detail == "" {
				p.Detail, _ = m["field"].(string)
			}
			for k, val := range m {
				if k != "error" && k != "detail" {
					p.Extra[k] = val
				}
			}
			apicontract.WriteProblem(w, r, p)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err
	}
}


