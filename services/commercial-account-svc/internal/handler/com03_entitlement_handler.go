// COM-03 Entitlement HTTP surface (ZS-SVC-Q-001 §4.3, §10).
package handler

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/store"
)

// Entitlement decisions are read by the tenant itself, or by a ZoikoSuite
// operator naming the organization with the platform read grant.
// Restrictions and the ended-access policy are platform authority only: an
// organization can never apply or lift its own restriction.
const (
	ActionEntitlementRead         = "COMMERCIAL_ENTITLEMENT_READ"
	ActionRestrictionApply        = "COMMERCIAL_RESTRICTION_APPLY"
	ActionRestrictionRead         = "COMMERCIAL_RESTRICTION_READ"
	ActionEntitlementPolicyManage = "COMMERCIAL_ENTITLEMENT_POLICY_MANAGE"
)

var capabilityKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{1,127}$`)

const (
	CodeCapabilityKeyRequired    = "CAPABILITY_KEY_REQUIRED"
	CodeRestrictionNotFound      = "RESTRICTION_NOT_FOUND"
	CodeRestrictionAlreadyLifted = "RESTRICTION_ALREADY_LIFTED"
)

// entitlementFailure maps COM-03 errors; writeFailure consults it last, same
// as governanceFailure.
func entitlementFailure(err error) (int, string, bool) {
	switch {
	case errors.Is(err, domain.ErrRestrictionNotFound):
		return http.StatusNotFound, CodeRestrictionNotFound, true
	case errors.Is(err, domain.ErrRestrictionAlreadyLifted):
		return http.StatusConflict, CodeRestrictionAlreadyLifted, true
	}
	return 0, "", false
}

type EntitlementHandler struct {
	store  store.EntitlementStore
	authz  AuthzChecker
	logger *zap.Logger
	now    func() time.Time
}

func NewEntitlementHandler(st store.EntitlementStore, az AuthzChecker, logger *zap.Logger) *EntitlementHandler {
	return &EntitlementHandler{store: st, authz: az, logger: logger, now: serverNow}
}

func (h *EntitlementHandler) WithClock(now func() time.Time) *EntitlementHandler {
	h.now = now
	return h
}

func RegisterEntitlementRoutes(r chi.Router, h *EntitlementHandler) {
	r.Post("/v1/commercial/entitlements:evaluate", h.Evaluate)
	r.Post("/v1/commercial/entitlements:explain", h.Explain)
	r.Post("/v1/commercial/entitlements:recompute", h.Recompute)
	r.Route("/v1/commercial/entitlements", func(r chi.Router) {
		r.Get("/effective", h.GetEffective)
		r.Get("/limit", h.GetLimit)
	})
	r.Route("/v1/commercial/restrictions", func(r chi.Router) {
		r.Post("/", h.ApplyRestriction)
		r.Get("/", h.ListRestrictions)
		r.Post("/{id}", h.RestrictionAction)
	})
	r.Route("/v1/commercial/entitlement-policy", func(r chi.Router) {
		r.Post("/", h.PublishPolicy)
		r.Get("/", h.GetPolicy)
	})
}

func (h *EntitlementHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeFailure(w, r, h.logger, err)
}

func (h *EntitlementHandler) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if p == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: "X-Principal-Id is required"})
		return "", false
	}
	return p, true
}

func (h *EntitlementHandler) authorizePlatform(w http.ResponseWriter, r *http.Request, principal, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principal, platformScopeID, action); err != nil {
		writeProblem(w, r, Problem{Status: http.StatusForbidden, Code: CodeAuthorizationDenied, Detail: "not authorized: " + action})
		return false
	}
	return true
}

// orgScope resolves the organization an entitlement read is about: an
// operator-named organization (needs the platform restriction-read grant),
// or the caller's own verified tenant (needs the entitlement-read grant on
// itself). It returns a context carrying that organization as the tenant
// scope every store call is made under.
func (h *EntitlementHandler) orgScope(w http.ResponseWriter, r *http.Request, declared string) (context.Context, bool) {
	principal, ok := h.principal(w, r)
	if !ok {
		return nil, false
	}
	if declared != "" {
		if !h.authorizePlatform(w, r, principal, ActionRestrictionRead) {
			return nil, false
		}
		return svcmiddleware.WithTenant(r.Context(), declared), true
	}
	org := svcmiddleware.TenantFromContext(r.Context())
	if org == "" {
		writeProblem(w, r, Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated,
			Detail: "X-Tenant-Id is required — the gateway sets it from a verified identity envelope"})
		return nil, false
	}
	if h.authz.CheckAllowed(r.Context(), principal, org, ActionEntitlementRead) != nil {
		writeProblem(w, r, Problem{Status: http.StatusForbidden, Code: CodeAuthorizationDenied, Detail: "not authorized to read entitlements"})
		return nil, false
	}
	return r.Context(), true
}

type evaluateRequest struct {
	CapabilityKey     string `json:"capability_key"`
	RequestedQuantity *int64 `json:"requested_quantity"`
	OrganizationID    string `json:"organization_id"`
}

// Evaluate is EvaluateCapability.
func (h *EntitlementHandler) Evaluate(w http.ResponseWriter, r *http.Request) {
	var req evaluateRequest
	if _, ok := readBody(w, r, &req, false); !ok {
		return
	}
	if !capabilityKeyPattern.MatchString(req.CapabilityKey) {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeCapabilityKeyRequired, Field: "capability_key",
			Detail: "capability_key is required"})
		return
	}
	ctx, ok := h.orgScope(w, r, req.OrganizationID)
	if !ok {
		return
	}
	d, err := h.store.EvaluateCapability(ctx, req.CapabilityKey, req.RequestedQuantity, h.now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *EntitlementHandler) GetEffective(w http.ResponseWriter, r *http.Request) {
	ctx, ok := h.orgScope(w, r, r.URL.Query().Get("organization_id"))
	if !ok {
		return
	}
	ds, err := h.store.GetEffectiveEntitlements(ctx, h.now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": ds})
}

func (h *EntitlementHandler) GetLimit(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("capability_key")
	if !capabilityKeyPattern.MatchString(key) {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeCapabilityKeyRequired, Field: "capability_key", Detail: "capability_key is required"})
		return
	}
	ctx, ok := h.orgScope(w, r, r.URL.Query().Get("organization_id"))
	if !ok {
		return
	}
	d, err := h.store.GetLimit(ctx, key, h.now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *EntitlementHandler) Explain(w http.ResponseWriter, r *http.Request) {
	var req evaluateRequest
	if _, ok := readBody(w, r, &req, false); !ok {
		return
	}
	if !capabilityKeyPattern.MatchString(req.CapabilityKey) {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeCapabilityKeyRequired, Field: "capability_key", Detail: "capability_key is required"})
		return
	}
	ctx, ok := h.orgScope(w, r, req.OrganizationID)
	if !ok {
		return
	}
	d, err := h.store.ExplainDecision(ctx, req.CapabilityKey, req.RequestedQuantity, h.now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// Recompute is RecomputeEntitlements: forces an EntitlementSnapshotChanged
// event a cache-fronted consumer can invalidate on. The decision itself is
// always computed fresh regardless of whether this was ever called.
func (h *EntitlementHandler) Recompute(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	ctx, ok := h.orgScope(w, r, r.URL.Query().Get("organization_id"))
	if !ok {
		return
	}
	ds, err := h.store.RecomputeEntitlements(ctx, principal, h.now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": ds})
}

// ── Restrictions ─────────────────────────────────────────────────────────────

type applyRestrictionRequest struct {
	OrganizationID string `json:"organization_id"`
	Level          string `json:"level"`
	ReasonCode     string `json:"reason_code"`
	PolicyRef      string `json:"policy_ref"`
	BasisRef       string `json:"basis_ref"`
}

// ApplyRestriction is platform authority only: an organization restricting
// itself is not a control, it is a way to fake compliance.
func (h *EntitlementHandler) ApplyRestriction(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok || !h.authorizePlatform(w, r, principal, ActionRestrictionApply) {
		return
	}
	var req applyRestrictionRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if req.Level != string(domain.RestrictionReadOnly) && req.Level != string(domain.RestrictionRestricted) {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "level", Detail: "must be READ_ONLY or RESTRICTED"})
		return
	}
	if req.OrganizationID == "" || req.ReasonCode == "" || strings.TrimSpace(req.PolicyRef) == "" || strings.TrimSpace(req.BasisRef) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext,
			Detail: "organization_id, level, reason_code, policy_ref and basis_ref are required"})
		return
	}
	rest := &domain.CommercialRestriction{RestrictionID: domain.NewCommercialID(domain.PrefixRestriction), OrganizationID: req.OrganizationID,
		Level: domain.RestrictionLevel(req.Level), ReasonCode: req.ReasonCode, PolicyRef: req.PolicyRef, BasisRef: req.BasisRef,
		AppliedAt: h.now(), AppliedByPrincipalID: principal}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "ApplyRestriction", rest.RestrictionID, raw, false)
	if !ok {
		return
	}
	created, err := h.store.ApplyRestriction(svcmiddleware.WithTenant(r.Context(), req.OrganizationID), rest, cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *EntitlementHandler) ListRestrictions(w http.ResponseWriter, r *http.Request) {
	ctx, ok := h.orgScope(w, r, r.URL.Query().Get("organization_id"))
	if !ok {
		return
	}
	rs, err := h.store.ListRestrictions(ctx)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if rs == nil {
		rs = []domain.CommercialRestriction{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"restrictions": rs})
}

// RestrictionAction dispatches POST /restrictions/{id}:remove.
func (h *EntitlementHandler) RestrictionAction(w http.ResponseWriter, r *http.Request) {
	rawID, action, found := strings.Cut(chi.URLParam(r, "id"), ":")
	if !found || action != "remove" {
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound, Detail: "expected /restrictions/{id}:remove"})
		return
	}
	id, ok := parseID(w, r, domain.PrefixRestriction, rawID)
	if !ok {
		return
	}
	principal, ok := h.principal(w, r)
	if !ok || !h.authorizePlatform(w, r, principal, ActionRestrictionApply) {
		return
	}
	var req lifecycleRequest
	if _, ok := readBody(w, r, &req, false); !ok {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason", Detail: "a reason is required to remove a restriction"})
		return
	}
	got, err := h.store.RemoveRestriction(r.Context(), id, principal, req.Reason, h.now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// ── Ended-access policy ──────────────────────────────────────────────────────

type publishPolicyRequest struct {
	EndedOutcome      string     `json:"ended_outcome"`
	EndedReadOnlyDays int        `json:"ended_read_only_days"`
	EffectiveFrom     *time.Time `json:"effective_from"`
	Reason            string     `json:"reason"`
}

func (h *EntitlementHandler) PublishPolicy(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok || !h.authorizePlatform(w, r, principal, ActionEntitlementPolicyManage) {
		return
	}
	var req publishPolicyRequest
	raw, ok := readBody(w, r, &req, false)
	if !ok {
		return
	}
	if req.EndedOutcome != "READ_ONLY" && req.EndedOutcome != "DENY" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "ended_outcome", Detail: "must be READ_ONLY or DENY"})
		return
	}
	if req.EndedOutcome == "READ_ONLY" && req.EndedReadOnlyDays < 1 {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "ended_read_only_days", Detail: "must be at least 1 for READ_ONLY"})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, r, Problem{Status: http.StatusBadRequest, Code: CodeInvalidCommercialContext, Field: "reason", Detail: "is required"})
		return
	}
	now := h.now()
	effFrom := now
	if req.EffectiveFrom != nil {
		effFrom = req.EffectiveFrom.UTC().Truncate(time.Microsecond)
	}
	p := &domain.EntitlementPolicyVersion{EndedOutcome: req.EndedOutcome, EndedReadOnlyDays: req.EndedReadOnlyDays,
		EffectiveFrom: effFrom, Reason: strings.TrimSpace(req.Reason), CreatedAt: now, CreatedByPrincipalID: principal}
	cmd, ok := commandFor(w, r, domain.SellerScope, principal, "PublishEntitlementPolicy", "grace-policy", raw, false)
	if !ok {
		return
	}
	created, err := h.store.PublishEntitlementPolicy(r.Context(), p, cmd.claim)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *EntitlementHandler) GetPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.GetEntitlementPolicy(r.Context(), h.now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if p == nil {
		writeProblem(w, r, Problem{Status: http.StatusNotFound, Code: CodeNotFound, Detail: "no entitlement policy has been published"})
		return
	}
	writeJSON(w, http.StatusOK, p)
}
