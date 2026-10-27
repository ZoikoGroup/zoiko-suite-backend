package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
)

// WFPStore is the persistence surface of the ZS-SVC-L-001 foundation objects.
// It is a separate interface from Store so the legacy payroll-run handlers and
// their stubs are untouched.
type WFPStore interface {
	CreateSourceTrustProfile(ctx context.Context, p *domain.SourceTrustProfile, mk store.EventBuilder[domain.SourceTrustProfile]) (bool, error)
	GetSourceTrustProfile(ctx context.Context, id string) (*domain.SourceTrustProfile, error)
	GetSourceTrustProfileBySource(ctx context.Context, sourceSystemID string) (*domain.SourceTrustProfile, error)
	ListSourceTrustProfiles(ctx context.Context, status string) ([]domain.SourceTrustProfile, error)
	TransitionSourceTrustProfile(ctx context.Context, id string, expectedVersion int, to, certificationRef string, mk store.EventBuilder[domain.SourceTrustProfile]) (*domain.SourceTrustProfile, error)

	RegisterRuleManifest(ctx context.Context, m *domain.PayrollRuleManifest, mk store.EventBuilder[domain.PayrollRuleManifest]) (bool, error)
	GetRuleManifest(ctx context.Context, id string) (*domain.PayrollRuleManifest, error)
	ResolveRuleManifest(ctx context.Context, sourceSystemID, jurisdictionID, asOf string) (*domain.PayrollRuleManifest, error)

	RegisterPayrollRunRef(ctx context.Context, r *domain.PayrollRunRef, mk store.EventBuilder[domain.PayrollRunRef]) (bool, error)
	GetPayrollRunRef(ctx context.Context, id string) (*domain.PayrollRunRef, error)
	ListPayrollRunRefs(ctx context.Context, f store.RunRefFilter) ([]domain.PayrollRunRef, error)
}

const (
	actionWFPView             = "WFP_VIEW"
	actionWFPTrustManage      = "WFP_SOURCE_TRUST_MANAGE"
	actionWFPManifestRegister = "WFP_RULE_MANIFEST_REGISTER"
	actionWFPRunRefRegister   = "WFP_RUN_REF_REGISTER"
)

type WFPHandler struct {
	store WFPStore
	authz AuthZClient
	log   *zap.Logger
}

func NewWFP(st WFPStore, authz AuthZClient, log *zap.Logger) *WFPHandler {
	return &WFPHandler{store: st, authz: authz, log: log}
}

func RegisterWFPRoutes(r chi.Router, h *WFPHandler) {
	r.Route("/v1/payroll/wfp", func(r chi.Router) {
		r.Route("/source-trust-profiles", func(r chi.Router) {
			r.Post("/", h.CreateTrustProfile)
			r.Get("/", h.ListTrustProfiles)
			r.Get("/by-source/{source_system_id}", h.GetTrustProfileBySource)
			r.Get("/{id}", h.GetTrustProfile)
			r.Post("/{id}/activate", h.transitionTrust(domain.TrustActive))
			r.Post("/{id}/suspend", h.transitionTrust(domain.TrustSuspended))
			r.Post("/{id}/retire", h.transitionTrust(domain.TrustRetired))
		})
		r.Route("/rule-manifests", func(r chi.Router) {
			r.Post("/", h.RegisterManifest)
			r.Get("/resolve", h.ResolveManifest)
			r.Get("/{id}", h.GetManifest)
		})
		r.Route("/run-refs", func(r chi.Router) {
			r.Post("/", h.RegisterRunRef)
			r.Get("/", h.ListRunRefs)
			r.Get("/{id}", h.GetRunRef)
		})
	})
}

// ── shared plumbing ──────────────────────────────────────────────────────────

// actor authenticates the caller and checks one action against the entity the
// request is scoped to. The tenant is always the authenticated one.
func (h *WFPHandler) actor(w http.ResponseWriter, r *http.Request, entityID, action string) (principal, tenant string, ok bool) {
	principal = r.Header.Get("X-Principal-Id")
	if principal == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", "", false
	}
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if tenant == "" {
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

func scopeEntity(r *http.Request) string { return strings.TrimSpace(r.Header.Get("X-Legal-Entity-Id")) }

// writeWFPError maps the domain's sentinel errors to stable status + code pairs.
func (h *WFPHandler) writeWFPError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrTrustProfileNotFound), errors.Is(err, domain.ErrManifestNotFound), errors.Is(err, domain.ErrRunRefNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrVersionConflict):
		writeError(w, http.StatusConflict, "VERSION_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrTrustProfileConflict):
		writeError(w, http.StatusConflict, "TRUST_PROFILE_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrRunIdentityConflict):
		writeError(w, http.StatusConflict, "RUN_IDENTITY_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrPriorVersionMissing):
		writeError(w, http.StatusConflict, "PRIOR_VERSION_MISSING", err.Error())
	case errors.Is(err, domain.ErrInvalidTrustTransition):
		writeError(w, http.StatusConflict, "INVALID_TRANSITION", err.Error())
	case errors.Is(err, domain.ErrCertificationRequired):
		writeError(w, http.StatusUnprocessableEntity, "CERTIFICATION_REQUIRED", err.Error())
	case errors.Is(err, domain.ErrSourceNotTrusted):
		writeError(w, http.StatusUnprocessableEntity, "SOURCE_NOT_TRUSTED", err.Error())
	case errors.Is(err, domain.ErrManifestNotEffective):
		writeError(w, http.StatusUnprocessableEntity, "RULE_MANIFEST_NOT_EFFECTIVE", err.Error())
	case errors.Is(err, domain.ErrManifestSourceMismatch):
		writeError(w, http.StatusUnprocessableEntity, "RULE_MANIFEST_SOURCE_MISMATCH", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
	default:
		h.log.Error("wfp store failure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "payroll store unavailable")
	}
}

func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// expectedVersion reads the optimistic-concurrency token from If-Match
// (`"3"` or `3`), the form the spec names; the body value is the fallback.
func expectedVersion(r *http.Request, bodyValue int) (int, bool) {
	if m := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`); m != "" {
		n, err := strconv.Atoi(m)
		return n, err == nil && n > 0
	}
	return bodyValue, bodyValue > 0
}

func (h *WFPHandler) event(eventType string, r *http.Request, actorID, tenant, entity, objectID string, payload any) (domain.OutboxEvent, error) {
	causation := r.Header.Get("X-Causation-Id")
	body, err := events.BuildWFPEnvelope(eventType, getCorrelationID(r), causation, actorID, tenant, entity, "", payload)
	if err != nil {
		return domain.OutboxEvent{}, err
	}
	return domain.OutboxEvent{TenantID: tenant, ObjectID: objectID, EventType: eventType, Payload: body}, nil
}

// ── source trust profiles ────────────────────────────────────────────────────

func (h *WFPHandler) CreateTrustProfile(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateSourceTrustProfileRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	entity := scopeEntity(r)
	principal, tenant, ok := h.actor(w, r, entity, actionWFPTrustManage)
	if !ok {
		return
	}
	p := &domain.SourceTrustProfile{
		ProfileID: uuid.NewString(), SourceSystemID: req.SourceSystemID, SourceKind: req.SourceKind,
		AllowedCapabilities: req.AllowedCapabilities, CreatedBy: principal,
	}
	created, err := h.store.CreateSourceTrustProfile(r.Context(), p, func(p *domain.SourceTrustProfile) (domain.OutboxEvent, error) {
		return h.event(domain.EventSourceTrustProfileChanged, r, principal, tenant, entity, p.ProfileID, map[string]any{
			"profile_id": p.ProfileID, "source_system_id": p.SourceSystemID, "status": p.Status, "version": p.Version,
		})
	})
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	status := http.StatusOK // replay of an identical registration
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, p)
}

func (h *WFPHandler) GetTrustProfile(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.actor(w, r, scopeEntity(r), actionWFPView); !ok {
		return
	}
	p, err := h.store.GetSourceTrustProfile(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// GetTrustProfileBySource is the lookup other services make before accepting a
// fact from a source system: 200 with the profile (the caller checks status and
// capability), 404 when the source has no profile at all.
func (h *WFPHandler) GetTrustProfileBySource(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.actor(w, r, scopeEntity(r), actionWFPView); !ok {
		return
	}
	p, err := h.store.GetSourceTrustProfileBySource(r.Context(), chi.URLParam(r, "source_system_id"))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *WFPHandler) ListTrustProfiles(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.actor(w, r, scopeEntity(r), actionWFPView); !ok {
		return
	}
	ps, err := h.store.ListSourceTrustProfiles(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	if ps == nil {
		ps = []domain.SourceTrustProfile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": ps})
}

func (h *WFPHandler) transitionTrust(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req domain.SourceTrustTransitionRequest
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
		principal, tenant, ok := h.actor(w, r, entity, actionWFPTrustManage)
		if !ok {
			return
		}
		p, err := h.store.TransitionSourceTrustProfile(r.Context(), chi.URLParam(r, "id"), version, to, req.CertificationRef,
			func(p *domain.SourceTrustProfile) (domain.OutboxEvent, error) {
				return h.event(domain.EventSourceTrustProfileChanged, r, principal, tenant, entity, p.ProfileID, map[string]any{
					"profile_id": p.ProfileID, "source_system_id": p.SourceSystemID, "status": p.Status,
					"version": p.Version, "reason": req.Reason,
				})
			})
		if err != nil {
			h.writeWFPError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

// ── rule manifests ───────────────────────────────────────────────────────────

func (h *WFPHandler) RegisterManifest(w http.ResponseWriter, r *http.Request) {
	var req domain.RegisterRuleManifestRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	entity := scopeEntity(r)
	principal, tenant, ok := h.actor(w, r, entity, actionWFPManifestRegister)
	if !ok {
		return
	}
	m := &domain.PayrollRuleManifest{
		ManifestID: uuid.NewString(), SourceSystemID: req.SourceSystemID, JurisdictionID: req.JurisdictionID,
		ManifestVersion: req.ManifestVersion, ManifestDigest: req.ManifestDigest, EffectiveFrom: req.EffectiveFrom,
		EffectiveTo: req.EffectiveTo, EvidenceRef: req.EvidenceRef, RegisteredBy: principal,
	}
	created, err := h.store.RegisterRuleManifest(r.Context(), m, func(m *domain.PayrollRuleManifest) (domain.OutboxEvent, error) {
		return h.event(domain.EventRuleManifestRegistered, r, principal, tenant, entity, m.ManifestID, map[string]any{
			"manifest_id": m.ManifestID, "source_system_id": m.SourceSystemID, "jurisdiction_id": m.JurisdictionID,
			"manifest_version": m.ManifestVersion, "manifest_digest": m.ManifestDigest,
			"effective_from": m.EffectiveFrom, "effective_to": m.EffectiveTo,
		})
	})
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, m)
}

func (h *WFPHandler) GetManifest(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.actor(w, r, scopeEntity(r), actionWFPView); !ok {
		return
	}
	m, err := h.store.GetRuleManifest(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ResolveManifest answers "which rules governed this pay date". There is
// deliberately no default for as_of: asking for the current rules by omission is
// the anti-pattern the spec prohibits for historical runs.
func (h *WFPHandler) ResolveManifest(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.actor(w, r, scopeEntity(r), actionWFPView); !ok {
		return
	}
	q := r.URL.Query()
	src, jur, asOf := q.Get("source_system_id"), q.Get("jurisdiction_id"), q.Get("as_of")
	if src == "" || jur == "" || asOf == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "source_system_id, jurisdiction_id and as_of (YYYY-MM-DD) are required")
		return
	}
	m, err := h.store.ResolveRuleManifest(r.Context(), src, jur, asOf)
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ── payroll run references ───────────────────────────────────────────────────

func (h *WFPHandler) RegisterRunRef(w http.ResponseWriter, r *http.Request) {
	var req domain.RegisterPayrollRunRefRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	principal, tenant, ok := h.actor(w, r, req.LegalEntityID, actionWFPRunRefRegister)
	if !ok {
		return
	}

	// The source must be trusted to submit payroll results...
	profile, err := h.store.GetSourceTrustProfileBySource(r.Context(), req.SourceSystemID)
	if err != nil {
		if errors.Is(err, domain.ErrTrustProfileNotFound) {
			err = domain.ErrSourceNotTrusted
		}
		h.writeWFPError(w, err)
		return
	}
	if !profile.Allows(domain.CapPayrollResult) {
		h.writeWFPError(w, domain.ErrSourceNotTrusted)
		return
	}
	// ...and the rules it used must be on record, from that source, and in force
	// on the pay date. Missing or future-effective provenance blocks the run.
	manifest, err := h.store.GetRuleManifest(r.Context(), req.RuleManifestID)
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	if manifest.SourceSystemID != req.SourceSystemID {
		h.writeWFPError(w, domain.ErrManifestSourceMismatch)
		return
	}
	if !manifest.EffectiveOn(req.PayDate) {
		h.writeWFPError(w, domain.ErrManifestNotEffective)
		return
	}

	ref := &domain.PayrollRunRef{
		RunRefID: uuid.NewString(), LegalEntityID: req.LegalEntityID, SourceSystemID: req.SourceSystemID,
		SourceRunID: req.SourceRunID, SourceRunVersion: req.SourceRunVersion, RunKind: req.RunKind,
		PayrollPeriodID: req.PayrollPeriodID, PayDate: req.PayDate, RuleManifestID: req.RuleManifestID,
		PayrollRunID: req.PayrollRunID, IdentityFingerprint: domain.RunIdentityFingerprint(tenant, req),
		CorrelationID: getCorrelationID(r), CausationID: req.CausationID, CreatedBy: principal,
	}
	if ref.CausationID == nil {
		if c := r.Header.Get("X-Causation-Id"); c != "" {
			ref.CausationID = &c
		}
	}
	created, err := h.store.RegisterPayrollRunRef(r.Context(), ref, func(p *domain.PayrollRunRef) (domain.OutboxEvent, error) {
		return h.event(domain.EventPayrollResultReceived, r, principal, tenant, p.LegalEntityID, p.RunRefID, map[string]any{
			"run_ref_id": p.RunRefID, "source_system_id": p.SourceSystemID, "source_run_id": p.SourceRunID,
			"source_run_version": p.SourceRunVersion, "run_kind": p.RunKind, "payroll_period_id": p.PayrollPeriodID,
			"pay_date": p.PayDate, "rule_manifest_id": p.RuleManifestID, "identity_fingerprint": p.IdentityFingerprint,
			"supersedes_run_ref_id": p.SupersedesRunRefID, "acceptance_state": p.AcceptanceState,
		})
	})
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, ref)
}

func (h *WFPHandler) GetRunRef(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.actor(w, r, scopeEntity(r), actionWFPView); !ok {
		return
	}
	ref, err := h.store.GetPayrollRunRef(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

func (h *WFPHandler) ListRunRefs(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.actor(w, r, scopeEntity(r), actionWFPView); !ok {
		return
	}
	q := r.URL.Query()
	refs, err := h.store.ListPayrollRunRefs(r.Context(), store.RunRefFilter{
		LegalEntityID: q.Get("legal_entity_id"), SourceSystemID: q.Get("source_system_id"),
		SourceRunID: q.Get("source_run_id"), State: q.Get("acceptance_state"),
	})
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	if refs == nil {
		refs = []domain.PayrollRunRef{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_refs": refs})
}
