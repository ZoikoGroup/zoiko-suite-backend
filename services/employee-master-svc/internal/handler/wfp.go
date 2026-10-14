package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/employee-master-svc/internal/clients"
	"zoiko.io/employee-master-svc/internal/domain"
	"zoiko.io/employee-master-svc/internal/events"
	svcmiddleware "zoiko.io/employee-master-svc/internal/middleware"
	"zoiko.io/employee-master-svc/internal/store"
)

// WFPStore is the persistence surface of WFP-01. Separate from Store so the
// employee handlers and their stubs are untouched.
type WFPStore interface {
	SyncWorkerFinancialProfile(ctx context.Context, p *domain.WorkerFinancialProfile, preReason string, mk store.EventBuilder) (*store.SyncResult, error)
	RevalidateWorkerFinancialProfile(ctx context.Context, id string, expectedVersion int, preReason string, mk store.EventBuilder) (*store.SyncResult, error)
	DeactivateWorkerFinancialProfile(ctx context.Context, id string, expectedVersion int, mk store.EventBuilder) (*domain.WorkerFinancialProfile, error)
	GetWorkerFinancialProfile(ctx context.Context, id string) (*domain.WorkerFinancialProfile, error)
	ListProfileVersions(ctx context.Context, workerID, assignmentID string) ([]domain.WorkerFinancialProfile, error)
	GetAsOfProfiles(ctx context.Context, workerID, assignmentID, asOf string) ([]domain.WorkerFinancialProfile, error)
	WorkerFactsFor(ctx context.Context, workerID string) (domain.WorkerFacts, error)
}

// SourceTrustChecker asks whether a source system may submit a capability.
type SourceTrustChecker interface {
	Check(ctx context.Context, caller clients.CallerContext, sourceSystemID, capability string) error
}

// JurisdictionChecker probes the jurisdiction registry (fail closed).
type JurisdictionChecker interface {
	Exists(ctx context.Context, jurisdictionID, correlationID string) (bool, error)
}

const (
	actionWFPProfileSync   = "WFP_FINANCIAL_PROFILE_SYNC"
	actionWFPProfileView   = "WFP_FINANCIAL_PROFILE_VIEW"
	actionWFPProfileManage = "WFP_FINANCIAL_PROFILE_MANAGE"
)

type WFPHandler struct {
	store         WFPStore
	authz         AuthZClient
	trust         SourceTrustChecker
	jurisdictions JurisdictionChecker
	log           *zap.Logger
}

func NewWFP(st WFPStore, authz AuthZClient, trust SourceTrustChecker, jur JurisdictionChecker, log *zap.Logger) *WFPHandler {
	return &WFPHandler{store: st, authz: authz, trust: trust, jurisdictions: jur, log: log}
}

func RegisterWFPRoutes(r chi.Router, h *WFPHandler) {
	r.Route("/v1/wfp/worker-financial-profiles", func(r chi.Router) {
		r.Post("/", h.Sync)
		r.Get("/", h.ListVersions)
		r.Get("/as-of", h.GetAsOf)
		r.Get("/validate-context", h.ValidateContext)
		r.Get("/{id}", h.Get)
		r.Post("/{id}/revalidate", h.Revalidate)
		r.Post("/{id}/deactivate", h.Deactivate)
	})
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func scopeEntity(r *http.Request) string { return strings.TrimSpace(r.Header.Get("X-Legal-Entity-Id")) }

// actor authenticates the caller and checks one action against the entity the
// request is scoped to. The tenant is always the authenticated one.
func (h *WFPHandler) actor(w http.ResponseWriter, r *http.Request, entityID, action string) (principal, tenant string, ok bool) {
	principal = r.Header.Get("X-Principal-Id")
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if principal == "" || tenant == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", "", false
	}
	if entityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "X-Legal-Entity-Id is required")
		return "", "", false
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, entityID, action); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "forbidden", err.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
		}
		return "", "", false
	}
	return principal, tenant, true
}

func (h *WFPHandler) writeWFPError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrProfileNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrStaleSourceVersion):
		writeError(w, http.StatusConflict, "STALE_SOURCE_VERSION", err.Error())
	case errors.Is(err, domain.ErrSourceVersionConflict):
		writeError(w, http.StatusConflict, "SOURCE_VERSION_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrProfileVersionConflict):
		writeError(w, http.StatusConflict, "VERSION_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrPartialOverlap):
		writeError(w, http.StatusConflict, "PARTIAL_OVERLAP_UNSUPPORTED", err.Error())
	case errors.Is(err, domain.ErrProfileNotQuarantined), errors.Is(err, domain.ErrProfileNotActive):
		writeError(w, http.StatusConflict, "INVALID_STATE", err.Error())
	case errors.Is(err, domain.ErrSourceNotTrusted):
		writeError(w, http.StatusUnprocessableEntity, "SOURCE_NOT_TRUSTED", err.Error())
	case errors.Is(err, domain.ErrSourceTrustUnavailable):
		writeError(w, http.StatusServiceUnavailable, "source_trust_unavailable", err.Error())
	case errors.Is(err, domain.ErrJurisdictionUnavailable):
		writeError(w, http.StatusServiceUnavailable, "jurisdiction_registry_unavailable", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
	default:
		// The raw error is not echoed: a store error can carry row content.
		h.log.Error("wfp-01 store failure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "worker financial profile store unavailable")
	}
}

func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// expectedVersion reads the optimistic-concurrency token from If-Match
// (`"3"` or `3`); the body value is the fallback.
func expectedVersion(r *http.Request, bodyValue int) (int, bool) {
	if m := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`); m != "" {
		n, err := strconv.Atoi(m)
		return n, err == nil && n > 0
	}
	return bodyValue, bodyValue > 0
}

func (h *WFPHandler) caller(r *http.Request, principal, tenant, entity string) clients.CallerContext {
	return clients.CallerContext{TenantID: tenant, PrincipalID: principal, LegalEntityID: entity, CorrelationID: getCorrelationID(r)}
}

// profileEvents builds the events for a committed change. payee_ref itself is
// never put in an event; only payee_ref_changed travels.
func (h *WFPHandler) profileEvents(r *http.Request, actor, tenant string) store.EventBuilder {
	return func(p *domain.WorkerFinancialProfile, superseded []domain.WorkerFinancialProfile) ([]domain.OutboxEvent, error) {
		corr, cause := getCorrelationID(r), r.Header.Get("X-Causation-Id")
		mk := func(typ string, payload map[string]any) (domain.OutboxEvent, error) {
			body, err := events.BuildWFPEnvelope(typ, corr, cause, actor, tenant, p.LegalEntityID, p.PayrollJurisdictionID, payload)
			return domain.OutboxEvent{TenantID: tenant, ObjectID: p.WorkerID, EventType: typ, Payload: body}, err
		}
		base := map[string]any{
			"profile_id": p.ProfileID, "worker_id": p.WorkerID, "assignment_id": p.AssignmentID,
			"source_system_id": p.SourceSystemID, "source_version": p.SourceVersion, "version": p.Version,
		}
		var out []domain.OutboxEvent
		add := func(typ string, extra map[string]any) error {
			payload := map[string]any{}
			for k, v := range base {
				payload[k] = v
			}
			for k, v := range extra {
				payload[k] = v
			}
			ev, err := mk(typ, payload)
			out = append(out, ev)
			return err
		}
		switch p.State {
		case domain.ProfileQuarantined:
			if err := add(domain.EventProfileRejected, map[string]any{"outcome": "QUARANTINED", "reason": p.QuarantineReason}); err != nil {
				return nil, err
			}
		case domain.ProfileDeactivated:
			if err := add(domain.EventProfileDeactivated, nil); err != nil {
				return nil, err
			}
		default:
			if err := add(domain.EventProfileSynced, map[string]any{
				"effective_from": p.EffectiveFrom, "effective_to": p.EffectiveTo,
				"payroll_jurisdiction_id": p.PayrollJurisdictionID, "pay_currency": p.PayCurrency,
				"payee_ref_changed": p.PayeeRefChanged, "supersedes_profile_id": p.SupersedesProfileID,
			}); err != nil {
				return nil, err
			}
			for _, s := range superseded {
				ev, err := events.BuildWFPEnvelope(domain.EventProfileSuperseded, corr, cause, actor, tenant, s.LegalEntityID, s.PayrollJurisdictionID,
					map[string]any{"profile_id": s.ProfileID, "worker_id": s.WorkerID, "assignment_id": s.AssignmentID, "superseded_by": p.ProfileID})
				if err != nil {
					return nil, err
				}
				out = append(out, domain.OutboxEvent{TenantID: tenant, ObjectID: p.WorkerID, EventType: domain.EventProfileSuperseded, Payload: ev})
			}
		}
		return out, nil
	}
}

// ── commands ─────────────────────────────────────────────────────────────────

// Sync is SyncWorkerFinancialProfile (a new source_version supersedes the old).
func (h *WFPHandler) Sync(w http.ResponseWriter, r *http.Request) {
	var req domain.SyncWorkerFinancialProfileRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	principal, tenant, ok := h.actor(w, r, req.LegalEntityID, actionWFPProfileSync)
	if !ok {
		return
	}

	// The source must be trusted to submit worker profiles.
	if err := h.trust.Check(r.Context(), h.caller(r, principal, tenant, req.LegalEntityID), req.SourceSystemID, domain.SourceCapabilityWorkerProfile); err != nil {
		h.writeWFPError(w, err)
		return
	}

	// The jurisdiction must be one the platform supports. Unsupported is
	// quarantined (kept as evidence, affecting nothing); unreachable is a 503.
	preReason := ""
	exists, err := h.jurisdictions.Exists(r.Context(), req.PayrollJurisdictionID, getCorrelationID(r))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	if !exists {
		preReason = domain.ReasonUnsupportedJurisdiction
	}

	p := &domain.WorkerFinancialProfile{
		ProfileID: uuid.NewString(), WorkerID: req.WorkerID, AssignmentID: req.AssignmentID,
		LegalEntityID: req.LegalEntityID, PayrollJurisdictionID: req.PayrollJurisdictionID, PayCurrency: req.PayCurrency,
		EmploymentClassification: req.EmploymentClassification, CostCenter: req.CostCenter, Dimensions: req.Dimensions,
		PayeeRef: req.PayeeRef, EffectiveFrom: req.EffectiveFrom, EffectiveTo: req.EffectiveTo,
		SourceSystemID: req.SourceSystemID, SourceVersion: req.SourceVersion,
		SourceFingerprint: req.Fingerprint(tenant), CorrelationID: getCorrelationID(r), CreatedBy: principal,
	}
	res, err := h.store.SyncWorkerFinancialProfile(r.Context(), p, preReason, h.profileEvents(r, principal, tenant))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	h.writeSyncResult(w, res)
}

func (h *WFPHandler) writeSyncResult(w http.ResponseWriter, res *store.SyncResult) {
	if res.Profile.State == domain.ProfileQuarantined {
		reason := ""
		if res.Profile.QuarantineReason != nil {
			reason = *res.Profile.QuarantineReason
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error_code": "PROFILE_QUARANTINED", "error_message": reason, "profile": res.Profile,
		})
		return
	}
	status := http.StatusOK // replay of an unchanged source_version
	if res.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, res.Profile)
}

// Revalidate is RevalidateMappings for one quarantined profile.
func (h *WFPHandler) Revalidate(w http.ResponseWriter, r *http.Request) {
	var req domain.RevalidateProfileRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return
	}
	entity := scopeEntity(r)
	principal, tenant, ok := h.actor(w, r, entity, actionWFPProfileManage)
	if !ok {
		return
	}
	cur, err := h.store.GetWorkerFinancialProfile(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	// Re-run the checks that live outside this service as well.
	preReason := ""
	exists, err := h.jurisdictions.Exists(r.Context(), cur.PayrollJurisdictionID, getCorrelationID(r))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	if !exists {
		preReason = domain.ReasonUnsupportedJurisdiction
	}
	if err := h.trust.Check(r.Context(), h.caller(r, principal, tenant, entity), cur.SourceSystemID, domain.SourceCapabilityWorkerProfile); err != nil {
		h.writeWFPError(w, err)
		return
	}
	res, err := h.store.RevalidateWorkerFinancialProfile(r.Context(), cur.ProfileID, version, preReason, h.profileEvents(r, principal, tenant))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	h.writeSyncResult(w, res)
}

func (h *WFPHandler) Deactivate(w http.ResponseWriter, r *http.Request) {
	var req domain.DeactivateProfileRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required")
		return
	}
	principal, tenant, ok := h.actor(w, r, scopeEntity(r), actionWFPProfileManage)
	if !ok {
		return
	}
	p, err := h.store.DeactivateWorkerFinancialProfile(r.Context(), chi.URLParam(r, "id"), version, h.profileEvents(r, principal, tenant))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── queries ──────────────────────────────────────────────────────────────────

func (h *WFPHandler) Get(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.actor(w, r, scopeEntity(r), actionWFPProfileView); !ok {
		return
	}
	p, err := h.store.GetWorkerFinancialProfile(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func validUUID(s string) bool { _, err := uuid.Parse(s); return err == nil }

func (h *WFPHandler) ListVersions(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.actor(w, r, scopeEntity(r), actionWFPProfileView); !ok {
		return
	}
	q := r.URL.Query()
	if !validUUID(q.Get("worker_id")) {
		writeError(w, http.StatusBadRequest, "validation_failed", "worker_id (UUID) is required")
		return
	}
	ps, err := h.store.ListProfileVersions(r.Context(), q.Get("worker_id"), q.Get("assignment_id"))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	if ps == nil {
		ps = []domain.WorkerFinancialProfile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": ps})
}

// GetAsOf resolves the profile in force on a date. as_of is mandatory: asking
// for "current" by omission is not a historical answer.
func (h *WFPHandler) GetAsOf(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.actor(w, r, scopeEntity(r), actionWFPProfileView); !ok {
		return
	}
	q := r.URL.Query()
	if !validUUID(q.Get("worker_id")) {
		writeError(w, http.StatusBadRequest, "validation_failed", "worker_id (UUID) is required")
		return
	}
	if _, err := time.Parse("2006-01-02", q.Get("as_of")); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "as_of (YYYY-MM-DD) is required")
		return
	}
	ps, err := h.store.GetAsOfProfiles(r.Context(), q.Get("worker_id"), q.Get("assignment_id"), q.Get("as_of"))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	if len(ps) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no ACTIVE worker financial profile covers that date")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": ps})
}

// ValidateContext is ValidateWorkerFinancialContext (a GET: it changes nothing): may this worker be paid by
// this entity on this date? It reports every reason it cannot, and changes nothing.
func (h *WFPHandler) ValidateContext(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := struct{ WorkerID, LegalEntityID, AsOf string }{q.Get("worker_id"), q.Get("legal_entity_id"), q.Get("as_of")}
	if !validUUID(req.WorkerID) || strings.TrimSpace(req.LegalEntityID) == "" {
		writeError(w, http.StatusBadRequest, "validation_failed", "worker_id (UUID) and legal_entity_id are required")
		return
	}
	if _, err := time.Parse("2006-01-02", req.AsOf); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "as_of (YYYY-MM-DD) is required")
		return
	}
	if _, _, ok := h.actor(w, r, req.LegalEntityID, actionWFPProfileView); !ok {
		return
	}
	facts, err := h.store.WorkerFactsFor(r.Context(), req.WorkerID)
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	reasons := []string{}
	if reason := domain.MappingReason(facts, req.LegalEntityID, req.AsOf); reason != "" {
		reasons = append(reasons, reason)
	}
	profiles, err := h.store.GetAsOfProfiles(r.Context(), req.WorkerID, "", req.AsOf)
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	var inEntity []domain.WorkerFinancialProfile
	for _, p := range profiles {
		if p.LegalEntityID == req.LegalEntityID {
			inEntity = append(inEntity, p)
		}
	}
	if len(inEntity) == 0 {
		reasons = append(reasons, "NO_ACTIVE_PROFILE")
	}
	// Pay currencies and jurisdictions of the profiles in force, so a consumer can
	// refuse a mismatch instead of converting. Distinct, sorted; payee_ref is never here.
	currencies, jurisdictions := distinctSorted(inEntity, func(p domain.WorkerFinancialProfile) string { return p.PayCurrency }),
		distinctSorted(inEntity, func(p domain.WorkerFinancialProfile) string { return p.PayrollJurisdictionID })
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": len(reasons) == 0, "reasons": reasons, "profile_count": len(inEntity),
		"pay_currencies": currencies, "payroll_jurisdictions": jurisdictions,
	})
}

func distinctSorted(ps []domain.WorkerFinancialProfile, pick func(domain.WorkerFinancialProfile) string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range ps {
		if v := pick(p); !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
