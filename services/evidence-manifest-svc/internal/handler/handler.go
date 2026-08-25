// Package handler exposes evidence-manifest-svc's REST API.
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/aggregator"
	authzpkg "zoiko.io/evidence-manifest-svc/internal/authz"
	"zoiko.io/evidence-manifest-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

type Store interface {
	CreateManifest(ctx context.Context, m *domain.EvidenceManifest) error
	AddRecord(ctx context.Context, r *domain.ManifestRecord) error
	FinalizeGenerated(ctx context.Context, manifestID, checksumSHA256 string) (*domain.EvidenceManifest, error)
	FinalizeFailed(ctx context.Context, manifestID, reason string) (*domain.EvidenceManifest, error)
	FindManifestByID(ctx context.Context, manifestID string) (*domain.EvidenceManifest, error)
	ListRecords(ctx context.Context, manifestID string) ([]domain.ManifestRecord, error)
}

type Publisher interface {
	PublishManifestGenerated(ctx context.Context, m *domain.EvidenceManifest, correlationID string) error
}

// GovernanceSource, AccessSource, WorkflowSource are the narrow interfaces the
// handler depends on — satisfied by aggregator's real HTTP clients, and
// stubbable in tests.
type GovernanceSource interface {
	ListByEntityAndDateRange(ctx context.Context, legalEntityID string, from, to *time.Time) ([]aggregator.SourceRecord, error)
	GetByID(ctx context.Context, decisionID string) (*aggregator.SourceRecord, error)
}

type AccessSource interface {
	GetByID(ctx context.Context, accessDecisionID string) (*aggregator.SourceRecord, error)
}

type WorkflowSource interface {
	GetByID(ctx context.Context, workflowInstanceID string) (*aggregator.SourceRecord, error)
}

// Action constants passed to authorization-svc as action_type.
//
// Two, not one: assembling a manifest and reading one back are different
// privileges. Generation is an expensive fan-out across three source
// services; reading returns the assembled bundle, which is the sensitive
// half. A principal who may request evidence is not automatically a
// principal who may read everyone's evidence.
const (
	EvidenceManifestGenerate = "EVIDENCE_MANIFEST_GENERATE"
	EvidenceManifestRead     = "EVIDENCE_MANIFEST_READ"
)

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store      Store
	governance GovernanceSource
	access     AccessSource
	workflow   WorkflowSource
	publisher  Publisher
	authz      AuthzChecker
	log        *zap.Logger
}

func New(store Store, governance GovernanceSource, access AccessSource,
	workflow WorkflowSource, publisher Publisher, authz AuthzChecker, log *zap.Logger) *Handler {
	return &Handler{store: store, governance: governance, access: access, workflow: workflow, publisher: publisher, authz: authz, log: log}
}

// authorize asks authorization-svc whether this principal may perform
// actionType within legalEntityID, and fails CLOSED.
//
// Until this change the service had no authorization at all — no
// internal/authz package, no check on any route. Tenant isolation was
// added in row 14 and holds (the store scopes every read by tenant), so
// this was never a cross-tenant hole. It was an intra-tenant privilege
// gap: any principal holding any valid envelope for the tenant could read
// any manifest in it, and manifest_records carries record_snapshot —
// verbatim governance decisions, access decisions and workflow instances.
// That is the artefact assembled for an auditor or regulator, so "any
// principal in the tenant" is the wrong audience for it.
//
// legalEntityID is deliberately taken from the manifest ROW on reads, not
// from anything the caller supplied. See tracker row 84a for why that
// matters: commercial-account-svc passes organization_id as the authz
// scope on some routes and commercial_account_id on others, into the same
// legal_entity_id parameter, and a grant recorded in one namespace can
// never match a check in the other.
//
// Note the failure posture this introduces: authorization-svc being
// unreachable now turns a working read into a 503. That is deliberate for
// governed evidence — Doc 03 §22 has evidence fail safe — but it is a real
// behaviour change, not a free addition.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not_authorized", "")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization_unavailable", "")
		return false
	}
	return true
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/evidence-manifests", func(r chi.Router) {
		r.Post("/", h.GenerateManifest)
		r.Get("/{manifestID}", h.GetManifest)
		r.Get("/{manifestID}/records", h.ListRecords)
	})
}

// ── POST /v1/evidence-manifests ──────────────────────────────────────────────

func (h *Handler) GenerateManifest(w http.ResponseWriter, r *http.Request) {
	var req domain.GenerateManifestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if missing := requiredFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	// tenant_id stays in the request contract but is no longer the source of
	// truth: it was previously the ONLY source, validated as non-empty and
	// otherwise trusted, which made the tenant caller-declared. It may now
	// only AGREE with the verified header. Disagreement is refused rather
	// than silently resolved, so a caller meaning to act on another tenant
	// gets an error instead of a quietly reinterpreted request.
	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if req.TenantID != verifiedTenant {
		writeError(w, http.StatusForbidden, "tenant_mismatch",
			"tenant_id in the request does not match the verified X-Tenant-Id")
		return
	}
	if !req.ScenarioType.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_scenario_type", string(req.ScenarioType))
		return
	}
	if req.GovernanceDecisionsFrom == nil && req.GovernanceDecisionsTo == nil &&
		len(req.GovernanceDecisionIDs) == 0 && len(req.AccessDecisionIDs) == 0 && len(req.WorkflowInstanceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "no_records_requested", domain.ErrNoRecordsRequested.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, EvidenceManifestGenerate) {
		return
	}

	// req.RequestedBy is still honoured — a service assembling evidence on
	// behalf of a named human legitimately records that human — but it can no
	// longer stand in for a missing verified principal, because there is no
	// longer such a thing as a request without one.
	requestedBy := req.RequestedBy
	if requestedBy == "" {
		requestedBy = principalID
	}

	manifest := &domain.EvidenceManifest{
		TenantID: req.TenantID, LegalEntityID: req.LegalEntityID,
		ScenarioType: req.ScenarioType, RequestedBy: requestedBy,
	}
	if err := h.store.CreateManifest(r.Context(), manifest); err != nil {
		h.log.Error("GenerateManifest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	records, err := h.collectRecords(r.Context(), req)
	if err != nil {
		// Fail closed: a manifest that can't fully assemble is FAILED, not
		// silently partial — a partial manifest that LOOKS complete is worse
		// than no manifest at all for audit/legal-discovery use.
		reason := err.Error()
		if _, ferr := h.store.FinalizeFailed(r.Context(), manifest.ManifestID, reason); ferr != nil {
			h.log.Error("GenerateManifest: failed to record FAILED status", zap.Error(ferr))
		}
		h.log.Error("GenerateManifest: source aggregation failed — manifest marked FAILED",
			zap.String("manifest_id", manifest.ManifestID), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "source_service_unavailable", domain.ErrSourceServiceUnavailable.Error())
		return
	}

	hasher := sha256.New()
	for _, rec := range records {
		mr := &domain.ManifestRecord{
			ManifestID: manifest.ManifestID, SourceType: rec.SourceType,
			SourceRecordID: rec.SourceRecordID, RecordSnapshot: rec.RawJSON,
		}
		if err := h.store.AddRecord(r.Context(), mr); err != nil {
			h.log.Error("GenerateManifest: failed to persist record", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
			return
		}
		hasher.Write([]byte(rec.SourceType))
		hasher.Write([]byte(rec.SourceRecordID))
		hasher.Write(rec.RawJSON)
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))

	finalManifest, err := h.store.FinalizeGenerated(r.Context(), manifest.ManifestID, checksum)
	if err != nil {
		h.log.Error("GenerateManifest: failed to finalize", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	if pubErr := h.publisher.PublishManifestGenerated(r.Context(), finalManifest, r.Header.Get("X-Correlation-ID")); pubErr != nil {
		h.log.Error("GenerateManifest: failed to publish evidence.manifest.generated",
			zap.String("manifest_id", finalManifest.ManifestID), zap.Error(pubErr))
	}

	writeJSON(w, http.StatusCreated, finalManifest)
}

// collectRecords fetches every requested source record. Fails closed on the
// first source error — a manifest is all-or-nothing.
func (h *Handler) collectRecords(ctx context.Context, req domain.GenerateManifestRequest) ([]aggregator.SourceRecord, error) {
	var out []aggregator.SourceRecord

	if req.GovernanceDecisionsFrom != nil || req.GovernanceDecisionsTo != nil {
		recs, err := h.governance.ListByEntityAndDateRange(ctx, req.LegalEntityID, req.GovernanceDecisionsFrom, req.GovernanceDecisionsTo)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	for _, id := range req.GovernanceDecisionIDs {
		rec, err := h.governance.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	for _, id := range req.AccessDecisionIDs {
		rec, err := h.access.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	for _, id := range req.WorkflowInstanceIDs {
		rec, err := h.workflow.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, nil
}

// ── GET /v1/evidence-manifests/{manifestID} ──────────────────────────────────

func (h *Handler) GetManifest(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	manifestID := chi.URLParam(r, "manifestID")

	// Fetch first, then authorize against the manifest's OWN legal entity.
	// The order matters and is deliberate: the store is tenant-scoped, so a
	// manifest belonging to another tenant is already indistinguishable from
	// a nonexistent one here (404). Authorizing against a caller-supplied
	// scope instead would let a caller nominate an entity they do hold a
	// grant for and read a manifest belonging to a different one.
	m, err := h.store.FindManifestByID(r.Context(), manifestID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, principalID, m.LegalEntityID, EvidenceManifestRead) {
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ── GET /v1/evidence-manifests/{manifestID}/records ──────────────────────────

// ListRecords is the most sensitive route in the service: it returns
// record_snapshot for every record in the manifest — verbatim governance
// decisions, access decisions and workflow instances as they stood at
// generation time. That is the assembled evidence itself, not metadata
// about it.
func (h *Handler) ListRecords(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	manifestID := chi.URLParam(r, "manifestID")
	// The existing parent lookup already fails closed for another tenant's
	// manifest; it now also supplies the legal entity to authorize against.
	m, err := h.store.FindManifestByID(r.Context(), manifestID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, principalID, m.LegalEntityID, EvidenceManifestRead) {
		return
	}
	records, err := h.store.ListRecords(r.Context(), manifestID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, records)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) handleStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrManifestNotFound):
		writeError(w, http.StatusNotFound, "manifest_not_found", "")
	default:
		h.log.Error("store error", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func requiredFieldMissing(req domain.GenerateManifestRequest) string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.ScenarioType == "":
		return "scenario_type"
	default:
		return ""
	}
}

// requirePrincipal returns the gateway-verified principal, or refuses the
// request.
//
// It replaces actorFromHeader's fallback to the literal string "unknown".
// That fallback was the same anti-pattern as the "default-tenant" tenant
// fabrication removed across 15 services in Priority 1b, applied to the
// actor instead: it made a header-less request SUCCEED, and wrote "unknown"
// into evidence_manifests.requested_by.
//
// On this service that is worse than a cosmetic default. requested_by is
// the attribution on an evidence bundle assembled for audit, regulator or
// legal-discovery use. A NOT NULL column populated with "unknown" satisfies
// the schema while carrying no accountability at all — and the platform's
// evidential doctrine requires the actor to be preserved, not merely
// present. It is also now load-bearing for a second reason: it is the
// principal handed to authorization-svc, so a fabricated actor would mean
// asking "is 'unknown' allowed to do this".
//
// Both header spellings are still accepted. X-Actor-Principal-ID takes
// precedence over X-Principal-Id where a caller sends both, preserving the
// existing contract; only the fabricated fallback is gone.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	if a := r.Header.Get("X-Actor-Principal-ID"); a != "" {
		return a, true
	}
	if a := r.Header.Get("X-Principal-Id"); a != "" {
		return a, true
	}
	writeError(w, http.StatusUnauthorized, "missing_principal",
		"X-Principal-Id is required — the gateway sets it from a verified identity envelope")
	return "", false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type errorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorResponse{Error: code, Detail: detail})
}
