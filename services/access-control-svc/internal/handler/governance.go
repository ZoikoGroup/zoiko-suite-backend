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

	"zoiko.io/access-control-svc/internal/clients"
	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/telemetry"
)

// The governance surface: permission taxonomy reads (§5), system role
// templates (§9, §9.1), governed assignment requests (§9, §21) and access
// review campaigns (§9, §24). See domain/governance.go for the ownership
// statement. Every command here follows the rules the role/bundle commands
// already follow: authorize the caller (ROLE_MANAGE on the request's entity),
// run the guards, provision into authorization-svc BEFORE recording, record
// and enqueue the event in one transaction, and leave a refused_escalations
// row for every refusal.

// GovStore is the persistence the governance surface needs.
type GovStore interface {
	ListTemplates(ctx context.Context) ([]domain.RoleTemplate, error)
	GetTemplate(ctx context.Context, code string) (*domain.RoleTemplate, error)
	ListTemplateVersions(ctx context.Context, code string) ([]domain.RoleTemplateVersion, error)
	GetTemplateVersion(ctx context.Context, code string, version int) (*domain.RoleTemplateVersion, error)
	CreateTemplateRole(ctx context.Context, r *domain.RoleDefinition, b *domain.PermissionBundleDef, actorID string) (bool, error)
	GetTemplateBundle(ctx context.Context, roleDefinitionID string) (*domain.PermissionBundleDef, error)
	UpgradeTemplateRole(ctx context.Context, roleDefinitionID string, version int, actions []string, actorID string) (*domain.InstantiatedRole, error)

	FindAssignmentByCorrelation(ctx context.Context, correlationID string) (*domain.AssignmentRequest, error)
	PendingAssignmentExists(ctx context.Context, target, roleDefinitionID, legalEntityID string) (bool, error)
	CreateAssignmentRequest(ctx context.Context, a *domain.AssignmentRequest, actorID string) (bool, error)
	GetAssignmentRequest(ctx context.Context, requestID string) (*domain.AssignmentRequest, error)
	ListAssignmentRequests(ctx context.Context, f domain.AssignmentListFilter) ([]domain.AssignmentRequest, error)
	DecideAssignmentRequest(ctx context.Context, requestID, toStatus, deciderID, reason, authzAssignmentID string) (*domain.AssignmentRequest, error)
	RevokeAssignmentRequest(ctx context.Context, requestID, revokerID, reason string) (*domain.AssignmentRequest, error)
	ScheduleAssignmentEnd(ctx context.Context, requestID, revokerID, reason string, at time.Time) (*domain.AssignmentRequest, error)

	FindCampaignByCorrelation(ctx context.Context, correlationID string) (*domain.ReviewCampaign, error)
	CreateCampaign(ctx context.Context, c *domain.ReviewCampaign, items []domain.ReviewItem, actorID string) (bool, error)
	GetCampaign(ctx context.Context, campaignID string) (*domain.ReviewCampaign, error)
	ListCampaigns(ctx context.Context, status string, limit, offset int) ([]domain.ReviewCampaign, error)
	ListReviewItems(ctx context.Context, reviewerID, status string) ([]domain.ReviewItem, error)
	GetReviewItem(ctx context.Context, campaignID, itemID string) (*domain.ReviewItem, error)
	DecideReviewItem(ctx context.Context, campaignID, itemID, decision, reason, deciderID string, revoked bool) (*domain.ReviewItem, error)
	ReassignReviewItem(ctx context.Context, campaignID, itemID, reviewerID string) (*domain.ReviewItem, error)
	CompleteCampaign(ctx context.Context, campaignID, actorID string) (*domain.ReviewCampaign, error)
}

// AssignmentAdmin provisions and reads assignments in authorization-svc.
type AssignmentAdmin interface {
	CreateRoleAssignment(ctx context.Context, assignmentID, principalID, roleID, legalEntityID string, effectiveFrom time.Time, effectiveTo *time.Time, s clients.Scope) (string, error)
	RevokeRoleAssignment(ctx context.Context, assignmentID string, s clients.Scope) error
	ScheduleRoleAssignmentEnd(ctx context.Context, assignmentID string, at time.Time, s clients.Scope) error
	ListRoleAssignments(ctx context.Context, roleID string, s clients.Scope) ([]domain.AuthzAssignment, error)
}

// TemplateBundleCode is the bundle_code of the one template-managed bundle on
// a role. Constant across versions: authorization-svc keys bundles by
// (role_id, bundle_code) and its attach is an upsert-replace, so an upgrade
// replaces the bundle's actions in place instead of adding a second bundle.
const TemplateBundleCode = "TEMPLATE"

// Gov serves the governance routes. It shares the role/bundle Handler's
// clients, guards and evidence helpers rather than duplicating them.
type Gov struct {
	h     *Handler
	store GovStore
	admin AssignmentAdmin

	// servicePrincipalID is this service's own identity in authorization-svc,
	// holding iam.assignment.revoke. A review REVOKE is the reviewer's DECISION
	// and this service's EXECUTION of it: a line manager attesting access is
	// not an IAM administrator, and provisioning as the reviewer left every
	// such item open behind 403 provisioning_forbidden (audit gap S9-B). The
	// reviewer, campaign and item travel as the §16 purpose and are recorded
	// with the revocation there. Empty falls back to the reviewer.
	servicePrincipalID string

	// groups enables the group-subject routes (groups.go); nil leaves them
	// unregistered.
	groups GroupStore

	// links, eventReviewer and eventReviewDue serve S9-C2: the subject-link
	// routes and the reviews HR events open (subject_links.go).
	links          SubjectLinkStore
	eventReviewer  string
	eventReviewDue time.Duration
}

// SetServicePrincipal sets the identity review revocations are executed as.
func (g *Gov) SetServicePrincipal(id string) {
	g.servicePrincipalID = id
}

func NewGov(h *Handler, store GovStore, admin AssignmentAdmin) *Gov {
	return &Gov{h: h, store: store, admin: admin}
}

func RegisterGovernanceRoutes(r chi.Router, g *Gov) {
	r.Get("/v1/permission-definitions", g.ListPermissionDefinitions)

	r.Route("/v1/role-templates", func(r chi.Router) {
		r.Get("/", g.ListTemplates)
		r.Get("/{template_code}", g.GetTemplate)
		r.Get("/{template_code}/versions", g.ListTemplateVersions)
		r.Post("/{template_code}/instantiate", g.InstantiateTemplate)
		r.Post("/{template_code}/upgrade", g.UpgradeTemplateRole)
	})

	// §21 names POST /v1/iam/access-assignments and .../{id}:revoke. The
	// colon form is the documented one; the slash form is served too because
	// some HTTP tooling percent-encodes a colon in a path segment.
	r.Route("/v1/iam/access-assignments", func(r chi.Router) {
		r.Post("/", g.RequestAssignment)
		r.Get("/", g.ListAssignments)
		r.Get("/{request_id}", g.GetAssignment)
		for _, verb := range []string{"approve", "reject", "cancel", "revoke"} {
			hf := g.assignmentCommand(verb)
			r.Post("/{request_id}:"+verb, hf)
			r.Post("/{request_id}/"+verb, hf)
		}
	})

	r.Route("/v1/access-review-campaigns", func(r chi.Router) {
		r.Post("/", g.CreateCampaign)
		r.Get("/", g.ListCampaigns)
		r.Get("/{campaign_id}", g.GetCampaign)
		r.Post("/{campaign_id}/items/{item_id}/decide", g.DecideReviewItem)
		r.Post("/{campaign_id}/items/{item_id}/reassign", g.ReassignReviewItem)
		r.Post("/{campaign_id}/complete", g.CompleteCampaign)
	})
	// §21: "retrieve assigned attestations/reviews" — the caller's own items.
	r.Get("/v1/iam/access-reviews", g.ListMyReviewItems)
	if g.groups != nil {
		registerGroupRoutes(r, g)
	}
	if g.links != nil {
		registerSubjectLinkRoutes(r, g)
	}
}

// ── shared helpers ───────────────────────────────────────────────────────────

func (g *Gov) count(op, outcome string) {
	g.h.metrics.GovernanceWrites.WithLabelValues(op, outcome).Inc()
}

// refuse answers a refusal, counts it under op and records the evidence row.
func (g *Gov) refuse(w http.ResponseWriter, r *http.Request, op, tenantID, entityID, principalID, correlationID string, status int, code, msg string, payload any) {
	outcome := telemetry.WriteInvalidRequest
	switch {
	case status == http.StatusForbidden:
		outcome = telemetry.WriteForbidden
	case status == http.StatusNotFound:
		outcome = telemetry.WriteNotFound
	case status == http.StatusConflict:
		outcome = telemetry.WriteConflict
	case status >= 500:
		outcome = telemetry.WriteStoreUnavailable
	}
	g.count(op, outcome)
	g.h.recordRefusal(r.Context(), tenantID, entityID, principalID, correlationID, strings.ToUpper(op), code, code, msg, payload)
	writeError(w, status, code, msg)
}

// fail maps an error from a guard, authorization-svc or the store to the
// status and code a caller can act on, and records it as a refusal.
func (g *Gov) fail(w http.ResponseWriter, r *http.Request, op, tenantID, entityID, principalID, correlationID string, err error, payload any) {
	status, code := classify(err)
	if status >= 500 {
		g.h.log.Error("governance command failed", zap.String("operation", op), zap.Error(err))
	}
	g.refuse(w, r, op, tenantID, entityID, principalID, correlationID, status, code, err.Error(), payload)
}

// classify maps an error to the status and code fail answers it with.
func classify(err error) (int, string) {
	status, code := http.StatusServiceUnavailable, "store_unavailable"
	switch {
	case errors.Is(err, domain.ErrUnknownPermission):
		status, code = http.StatusBadRequest, "unknown_permission"
	case errors.Is(err, ErrTaxonomyUnavailable):
		code = "taxonomy_unavailable"
	case errors.Is(err, domain.ErrSoDConflict):
		status, code = http.StatusForbidden, "sod_conflict"
	case errors.Is(err, domain.ErrSoDUnavailable):
		code = "sod_unavailable"
	case errors.Is(err, domain.ErrProvisioningForbidden):
		status, code = http.StatusForbidden, "provisioning_forbidden"
	case errors.Is(err, domain.ErrAuthorizationDenied):
		status, code = http.StatusForbidden, "forbidden"
	case errors.Is(err, domain.ErrAuthzServiceUnavailable):
		code = "authz_unavailable"
	case errors.Is(err, domain.ErrRoleNotFound), errors.Is(err, domain.ErrTemplateNotFound),
		errors.Is(err, domain.ErrTemplateVersionNotFound), errors.Is(err, domain.ErrAssignmentNotFound),
		errors.Is(err, domain.ErrCampaignNotFound), errors.Is(err, domain.ErrReviewItemNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, domain.ErrRoleCodeExists):
		status, code = http.StatusConflict, "role_code_exists"
	case errors.Is(err, domain.ErrNotTemplateRole):
		status, code = http.StatusConflict, "not_template_role"
	case errors.Is(err, domain.ErrTemplateVersionNotNewer):
		status, code = http.StatusConflict, "template_version_not_newer"
	case errors.Is(err, domain.ErrAssignmentState):
		status, code = http.StatusConflict, "invalid_state"
	case errors.Is(err, domain.ErrCampaignClosed):
		status, code = http.StatusConflict, "campaign_closed"
	case errors.Is(err, domain.ErrItemAlreadyDecided):
		status, code = http.StatusConflict, "already_decided"
	case errors.Is(err, domain.ErrUnresolvedHighRisk):
		status, code = http.StatusConflict, "unresolved_high_risk"
	case errors.Is(err, domain.ErrEscalationReviewerReq):
		status, code = http.StatusBadRequest, "escalation_reviewer_required"
	case errors.Is(err, domain.ErrAuthzAssignmentAbsent):
		status, code = http.StatusConflict, "assignment_absent"
	case errors.Is(err, domain.ErrEntityMismatch):
		status, code = http.StatusBadRequest, "entity_mismatch"
	case errors.Is(err, domain.ErrInvalidEffectiveTo):
		status, code = http.StatusBadRequest, "invalid_effective_to"
	case errors.Is(err, domain.ErrInvalidEffectiveAt):
		status, code = http.StatusBadRequest, "invalid_effective_at"
	case errors.Is(err, domain.ErrAssignmentWindowOver):
		status, code = http.StatusConflict, "assignment_window_elapsed"
	case errors.Is(err, domain.ErrSecurityApproval):
		status, code = http.StatusForbidden, "security_approval_required"
	case errors.Is(err, domain.ErrAuthzApprovalPending):
		status, code = http.StatusConflict, "authz_approval_pending"
	case errors.Is(err, domain.ErrAssignmentPending):
		status, code = http.StatusConflict, "assignment_pending"
	case errors.Is(err, domain.ErrGroupNotFound), errors.Is(err, domain.ErrGroupAssignmentNotFound),
		errors.Is(err, domain.ErrGroupMemberNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, domain.ErrGroupCodeExists):
		status, code = http.StatusConflict, "group_code_exists"
	case errors.Is(err, domain.ErrGroupRetired):
		status, code = http.StatusConflict, "group_retired"
	case errors.Is(err, domain.ErrGroupMemberExists):
		status, code = http.StatusConflict, "member_exists"
	case errors.Is(err, domain.ErrGroupAssignmentRevoked):
		status, code = http.StatusConflict, "group_assignment_revoked"
	case strings.Contains(err.Error(), "authorization-svc admin API"):
		code = "authz_admin_unavailable"
	}
	return status, code
}

// begin runs the checks every governance command starts with: a principal, a
// tenant, and ROLE_MANAGE on the entity. ok=false means it has answered.
func (g *Gov) begin(w http.ResponseWriter, r *http.Request, op, legalEntityID, correlationID string, payload any) (principalID, tenantID string, ok bool) {
	principalID = r.Header.Get("X-Principal-Id")
	if principalID == "" {
		g.count(op, telemetry.WriteIdentityMissing)
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", "", false
	}
	tenantID, ok = g.h.requireTenant(w, r, nil)
	if !ok {
		g.count(op, telemetry.WriteIdentityMissing)
		return "", "", false
	}
	if err := g.h.checkAllowed(r.Context(), principalID, legalEntityID); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			g.refuse(w, r, op, tenantID, legalEntityID, principalID, correlationID, http.StatusForbidden, "forbidden", err.Error(), payload)
		} else {
			g.count(op, telemetry.WriteAuthzUnavailable)
			writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
		}
		return "", "", false
	}
	return principalID, tenantID, true
}

// identify is begin without the grant check: a principal and a tenant. Used
// where the authority is a designation (a review item's reviewer) rather than
// ROLE_MANAGE.
func (g *Gov) identify(w http.ResponseWriter, r *http.Request, op string) (principalID, tenantID string, ok bool) {
	principalID = r.Header.Get("X-Principal-Id")
	if principalID == "" {
		g.count(op, telemetry.WriteIdentityMissing)
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", "", false
	}
	tenantID, ok = g.h.requireTenant(w, r, nil)
	if !ok {
		g.count(op, telemetry.WriteIdentityMissing)
	}
	return principalID, tenantID, ok
}

// checkAction asks authorization-svc whether principalID holds action in the
// entity, counting the decision like checkAllowed does for ROLE_MANAGE.
func (g *Gov) checkAction(ctx context.Context, principalID, legalEntityID, action string) error {
	err := g.h.authz.CheckAllowed(ctx, principalID, legalEntityID, action)
	switch {
	case err == nil:
		g.h.metrics.AuthZDecisions.WithLabelValues(action, telemetry.AuthZGranted).Inc()
	case errors.Is(err, domain.ErrAuthorizationDenied):
		g.h.metrics.AuthZDecisions.WithLabelValues(action, telemetry.AuthZDenied).Inc()
	default:
		g.h.metrics.AuthZDecisions.WithLabelValues(action, telemetry.AuthZUnavailable).Inc()
	}
	return err
}

func (g *Gov) reader(w http.ResponseWriter, r *http.Request) (string, bool) {
	caller, ok := g.h.requireCaller(w, r)
	if !ok {
		return "", false
	}
	if _, ok := g.h.requireTenant(w, r, nil); !ok {
		return "", false
	}
	return caller, true
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

// roleActions is the union of a role's ACTIVE bundles' actions.
func (g *Gov) roleActions(ctx context.Context, roleDefinitionID string) ([]string, error) {
	bundles, err := g.h.store.ListBundles(ctx, roleDefinitionID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, b := range bundles {
		if !b.ActiveFlag {
			continue
		}
		for _, a := range b.PermittedActions {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// ── GET /v1/permission-definitions ──────────────────────────────────────────

func (g *Gov) ListPermissionDefinitions(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	naming := r.URL.Query().Get("naming")
	if naming != "" && naming != "TAXONOMY" && naming != "LEGACY" {
		writeError(w, http.StatusBadRequest, "invalid_naming", "naming must be TAXONOMY or LEGACY")
		return
	}
	list, err := g.h.taxonomy.List(r.Context(), naming, r.URL.Query().Get("search"))
	if err != nil {
		g.h.log.Error("failed to list permission definitions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "taxonomy_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.PermissionDefinition{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── role templates ──────────────────────────────────────────────────────────

func (g *Gov) ListTemplates(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	list, err := g.store.ListTemplates(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.RoleTemplate{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (g *Gov) GetTemplate(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	t, err := g.store.GetTemplate(r.Context(), chi.URLParam(r, "template_code"))
	if err != nil {
		if errors.Is(err, domain.ErrTemplateNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (g *Gov) ListTemplateVersions(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	code := chi.URLParam(r, "template_code")
	if _, err := g.store.GetTemplate(r.Context(), code); err != nil {
		if errors.Is(err, domain.ErrTemplateNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	list, err := g.store.ListTemplateVersions(r.Context(), code)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// InstantiateTemplate handles POST /v1/role-templates/{template_code}/instantiate.
//
// It creates a tenant role from a published template version, with ONE
// template-managed bundle carrying exactly that version's actions. The role is
// provisioned into authorization-svc before it is recorded, like any other.
//
// Guards: the template's actions must still be registered (the registry can
// retire an action after a template was published) and conflict-free under
// the §10.1 rules. The protected-action guard does not apply: §9 reserves
// protected actions from TENANT CUSTOM roles, and a template is
// ZoikoSuite-maintained (the IAM Access Administrator archetype grants
// iam.assignment.grant by design). What keeps that safe is that the bundle
// cannot be edited here (UpdateBundle refuses), and that assigning such a role
// is CRITICAL risk and needs an independent approver.
func (g *Gov) InstantiateTemplate(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovInstantiateTemplate
	code := chi.URLParam(r, "template_code")
	var req domain.InstantiateTemplateRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	if req.LegalEntityID == "" || req.CorrelationID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields", "legal_entity_id and correlation_id are required", req)
		return
	}
	principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
	if !ok {
		return
	}
	fail := func(err error) {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
	}

	tmpl, err := g.store.GetTemplate(r.Context(), code)
	if err != nil {
		fail(err)
		return
	}
	if tmpl.Status != "ACTIVE" {
		g.refuse(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, http.StatusConflict, "template_deprecated", "the role template is deprecated and cannot be instantiated", req)
		return
	}
	version, err := g.store.GetTemplateVersion(r.Context(), code, req.TemplateVersion)
	if err != nil {
		fail(err)
		return
	}
	roleCode := req.RoleCode
	if roleCode == "" {
		roleCode = tmpl.TemplateCode
	}
	roleName := req.RoleName
	if roleName == "" {
		roleName = tmpl.TemplateName
	}

	if err := g.h.checkTaxonomy(r.Context(), version.PermittedActions); err != nil {
		fail(err)
		return
	}
	if err := g.h.sod.CheckConflict(r.Context(), domain.SoDCheckRequest{
		TenantID: tenantID, CallerID: principalID, CorrelationID: req.CorrelationID, CandidateActions: version.PermittedActions,
	}); err != nil {
		fail(err)
		return
	}
	taken, err := g.h.store.RoleCodeTaken(r.Context(), roleCode, req.CorrelationID)
	if err != nil {
		fail(err)
		return
	}
	if taken {
		fail(domain.ErrRoleCodeExists)
		return
	}

	roleID := uuid.NewString()
	scope := clients.Scope{PrincipalID: principalID, TenantID: tenantID, LegalEntityID: req.LegalEntityID, CorrelationID: req.CorrelationID}
	if err := g.h.authzAdmin.CreateRole(r.Context(), roleID, roleCode, roleName, tmpl.RoleScopeType, scope); err != nil {
		g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateRole, adminOutcome(err)).Inc()
		fail(err)
		return
	}
	g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateRole, telemetry.AdminOK).Inc()
	if err := g.h.authzAdmin.CreatePermissionBundle(r.Context(), roleID, TemplateBundleCode, version.PermittedActions, scope); err != nil {
		g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateBundle, adminOutcome(err)).Inc()
		// The role exists there without its bundle: it grants nothing, which
		// fails safe. Retire it so it is not left as an empty live role.
		_ = g.h.authzAdmin.SetRoleActive(r.Context(), roleID, false, scope)
		fail(err)
		return
	}
	g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateBundle, telemetry.AdminOK).Inc()

	now := time.Now().UTC()
	role := &domain.RoleDefinition{
		RoleDefinitionID: roleID, TenantID: tenantID, RoleCode: roleCode, RoleName: roleName,
		RoleScopeType: tmpl.RoleScopeType, Status: domain.RoleStatusActive, CreatedByPrincipalID: principalID,
		CorrelationID: req.CorrelationID, CreatedAt: now, UpdatedAt: now,
		TemplateCode: tmpl.TemplateCode, TemplateVersion: version.TemplateVersion,
	}
	bundle := &domain.PermissionBundleDef{
		BundleID: uuid.NewString(), TenantID: tenantID, RoleDefinitionID: roleID, BundleCode: TemplateBundleCode,
		PermittedActions: version.PermittedActions, ActiveFlag: true, CorrelationID: req.CorrelationID,
		CreatedAt: now, UpdatedAt: now, TemplateCode: tmpl.TemplateCode, TemplateVersion: version.TemplateVersion,
	}
	created, err := g.store.CreateTemplateRole(r.Context(), role, bundle, principalID)
	if err != nil {
		fail(err)
		return
	}
	out := domain.InstantiatedRole{Role: *role, Bundle: *bundle}
	if created {
		g.count(op, telemetry.WriteCreated)
		writeJSON(w, http.StatusCreated, out)
		return
	}
	g.count(op, telemetry.WriteReplayed)
	writeJSON(w, http.StatusOK, out)
}

type upgradeRequest struct {
	domain.TemplateUpgradeRequest
	RoleDefinitionID string `json:"role_definition_id"`
}

// UpgradeTemplateRole handles POST /v1/role-templates/{template_code}/upgrade.
//
// The only way a template-derived role's managed bundle changes. Explicit and
// evented (§9: not "silently"): the caller names the role and the version, the
// new action set passes the same guards as an instantiation plus SoD against
// the role's other bundles, it is provisioned before it is recorded, and
// role.updated ends the sessions that hold the old grant.
func (g *Gov) UpgradeTemplateRole(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovUpgradeTemplate
	code := chi.URLParam(r, "template_code")
	var req upgradeRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	if req.LegalEntityID == "" || req.CorrelationID == "" || req.RoleDefinitionID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields", "role_definition_id, legal_entity_id and correlation_id are required", req)
		return
	}
	principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
	if !ok {
		return
	}
	fail := func(err error) {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
	}

	role, err := g.h.store.GetRole(r.Context(), req.RoleDefinitionID)
	if err != nil {
		fail(err)
		return
	}
	if role.TemplateCode != code {
		fail(domain.ErrNotTemplateRole)
		return
	}
	version, err := g.store.GetTemplateVersion(r.Context(), code, req.TemplateVersion)
	if err != nil {
		fail(err)
		return
	}
	if version.TemplateVersion <= role.TemplateVersion {
		fail(domain.ErrTemplateVersionNotNewer)
		return
	}
	current, err := g.store.GetTemplateBundle(r.Context(), role.RoleDefinitionID)
	if err != nil {
		fail(err)
		return
	}
	if !current.ActiveFlag {
		g.refuse(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, http.StatusConflict, "template_bundle_detached", "the role's template bundle is detached; reattach it before upgrading", req)
		return
	}
	if err := g.h.checkTaxonomy(r.Context(), version.PermittedActions); err != nil {
		fail(err)
		return
	}
	if err := g.h.checkSoDConflict(r.Context(), tenantID, principalID, req.CorrelationID, role.RoleDefinitionID, current.BundleID, version.PermittedActions); err != nil {
		fail(err)
		return
	}
	scope := clients.Scope{PrincipalID: principalID, TenantID: tenantID, LegalEntityID: req.LegalEntityID, CorrelationID: req.CorrelationID}
	if err := g.h.authzAdmin.CreatePermissionBundle(r.Context(), role.RoleDefinitionID, current.BundleCode, version.PermittedActions, scope); err != nil {
		g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateBundle, adminOutcome(err)).Inc()
		fail(err)
		return
	}
	g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateBundle, telemetry.AdminOK).Inc()
	out, err := g.store.UpgradeTemplateRole(r.Context(), role.RoleDefinitionID, version.TemplateVersion, version.PermittedActions, principalID)
	if err != nil {
		fail(err)
		return
	}
	g.count(op, telemetry.WriteUpdated)
	writeJSON(w, http.StatusOK, out)
}

// ── assignment requests ─────────────────────────────────────────────────────

// approvalPolicy decides whether a request needs an independent approver, and
// says why. §9: approval "depending on risk"; §10.2 "own access elevation":
// a request for one's own access never self-provisions, whatever its risk.
func approvalPolicy(requester, target, riskTier string, drivers []string) (bool, string) {
	var reasons []string
	if requester == target {
		reasons = append(reasons, "self-request (own access elevation)")
	}
	if domain.RiskRank(riskTier) >= domain.RiskRank(domain.RiskHigh) {
		reasons = append(reasons, "risk "+riskTier+" ("+strings.Join(drivers, ", ")+")")
	}
	if riskTier == domain.RiskCritical {
		reasons = append(reasons, "security approver required ("+domain.ActionApprovePrivileged+")")
	}
	return len(reasons) > 0, strings.Join(reasons, "; ")
}

// RequestAssignment handles POST /v1/iam/access-assignments.
//
//	201  STANDARD risk, not a self-request: provisioned on submission.
//	202  needs an independent approver: recorded PENDING_APPROVAL.
//	200  a replay of this correlation_id.
//
// Before either: the role must be ACTIVE in this tenant, no identical request
// may be pending, and the subject plus the role must be SoD-clean against what
// the subject already holds in the entity (principal-aware /v1/sod/validate).
func (g *Gov) RequestAssignment(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovRequestAssignment
	var req domain.CreateAssignmentRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	if req.LegalEntityID == "" || req.TargetPrincipalID == "" || req.RoleDefinitionID == "" || strings.TrimSpace(req.Justification) == "" || req.CorrelationID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, target_principal_id, role_definition_id, justification and correlation_id are required", req)
		return
	}
	if req.EffectiveTo != nil {
		start := req.EffectiveFrom
		if start.IsZero() {
			start = time.Now()
		}
		if !req.EffectiveTo.After(start) || !req.EffectiveTo.After(time.Now()) {
			g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_effective_to", string(domain.ErrInvalidEffectiveTo), req)
			return
		}
	}
	principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
	if !ok {
		return
	}
	fail := func(err error) {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
	}

	if prior, err := g.store.FindAssignmentByCorrelation(r.Context(), req.CorrelationID); err != nil {
		fail(err)
		return
	} else if prior != nil {
		g.count(op, telemetry.WriteReplayed)
		writeJSON(w, http.StatusOK, prior)
		return
	}

	role, err := g.h.store.GetRole(r.Context(), req.RoleDefinitionID)
	if err != nil {
		fail(err)
		return
	}
	if role.Status != domain.RoleStatusActive {
		g.refuse(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, http.StatusConflict, "role_retired", string(domain.ErrRoleRetired), req)
		return
	}
	a, created, err := g.submitAssignment(r.Context(), tenantID, principalID, role, assignmentSpec{
		target: req.TargetPrincipalID, legalEntityID: req.LegalEntityID, justification: req.Justification,
		correlationID: req.CorrelationID, from: req.EffectiveFrom, to: req.EffectiveTo,
	})
	if err != nil {
		fail(err)
		return
	}
	switch {
	case !created:
		g.count(op, telemetry.WriteReplayed)
		writeJSON(w, http.StatusOK, a)
	case a.Status == domain.AssignmentProvisioned:
		g.count(op, telemetry.WriteCreated)
		writeJSON(w, http.StatusCreated, a)
	default:
		g.count(op, telemetry.WriteCreated)
		writeJSON(w, http.StatusAccepted, a)
	}
}

// assignmentSpec is one subject's request: from the individual endpoint, or
// one member's share of a group assignment's fan-out.
type assignmentSpec struct {
	target, legalEntityID, justification, correlationID string
	groupAssignmentID                                   string
	from                                                time.Time
	to                                                  *time.Time
}

// submitAssignment runs the governed request pipeline for one subject: no
// identical request pending, SoD-clean against what the subject already holds
// in the entity (principal-aware /v1/sod/validate), risk-tiered approval,
// provisioning when no independent approver is needed, and the record with
// its event. A group fan-out runs it once per member, so membership never
// bypasses any of it (§2 "never bypass policy", A20). created=false is a
// replay of the correlation id.
func (g *Gov) submitAssignment(ctx context.Context, tenantID, principalID string, role *domain.RoleDefinition, s assignmentSpec) (*domain.AssignmentRequest, bool, error) {
	if pending, err := g.store.PendingAssignmentExists(ctx, s.target, role.RoleDefinitionID, s.legalEntityID); err != nil {
		return nil, false, err
	} else if pending {
		return nil, false, domain.ErrAssignmentPending
	}
	actions, err := g.roleActions(ctx, role.RoleDefinitionID)
	if err != nil {
		return nil, false, err
	}
	if len(actions) > 0 {
		if err := g.h.sod.CheckConflict(ctx, domain.SoDCheckRequest{
			TenantID: tenantID, CallerID: principalID, CorrelationID: s.correlationID, CandidateActions: actions,
			SubjectPrincipalID: s.target, LegalEntityID: s.legalEntityID,
		}); err != nil {
			return nil, false, err
		}
	}
	risk, drivers, err := g.h.roleRisk(ctx, actions)
	if err != nil {
		return nil, false, err
	}
	needsApproval, why := approvalPolicy(principalID, s.target, risk, drivers)

	now := time.Now().UTC()
	effective := s.from.UTC()
	if s.from.IsZero() {
		effective = now
	}
	var end *time.Time
	if s.to != nil {
		e := s.to.UTC()
		end = &e
	}
	a := &domain.AssignmentRequest{
		RequestID: uuid.NewString(), TenantID: tenantID, TargetPrincipalID: s.target,
		RoleDefinitionID: role.RoleDefinitionID, LegalEntityID: s.legalEntityID, EffectiveFrom: effective, EffectiveTo: end,
		Justification: s.justification, RiskTier: risk, ApprovalRequired: needsApproval, ApprovalReason: why,
		Status: domain.AssignmentPendingApproval, RequestedByPrincipalID: principalID,
		CorrelationID: s.correlationID, CreatedAt: now, UpdatedAt: now, GroupAssignmentID: s.groupAssignmentID,
	}
	if !needsApproval {
		// The request id is the assignment id there, so the two are joined by
		// construction and a racing duplicate collides instead of doubling.
		scope := clients.Scope{PrincipalID: principalID, TenantID: tenantID, LegalEntityID: s.legalEntityID, CorrelationID: s.correlationID,
			ApprovalReference: a.RequestID, Purpose: s.justification}
		id, err := g.admin.CreateRoleAssignment(ctx, a.RequestID, a.TargetPrincipalID, a.RoleDefinitionID, assignmentEntity(role, a.LegalEntityID), a.EffectiveFrom, a.EffectiveTo, scope)
		if err != nil {
			g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateAssignment, adminOutcome(err)).Inc()
			g.withdrawPending(ctx, err, id, scope)
			return nil, false, err
		}
		g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateAssignment, telemetry.AdminOK).Inc()
		a.Status, a.AuthzAssignmentID = domain.AssignmentProvisioned, id
	}
	created, err := g.store.CreateAssignmentRequest(ctx, a, principalID)
	if err != nil {
		return nil, false, err
	}
	return a, created, nil
}

// assignmentEntity is the entity sent to authorization-svc: a TENANT-scoped
// role is assigned tenant-wide there (it refuses an entity-less assignment of
// an entity-scoped role, and an entity on a tenant role would narrow it).
func assignmentEntity(role *domain.RoleDefinition, entityID string) string {
	if role.RoleScopeType == "TENANT" {
		return ""
	}
	return entityID
}

func (g *Gov) ListAssignments(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	f := domain.AssignmentListFilter{Status: r.URL.Query().Get("status"), TargetPrincipalID: r.URL.Query().Get("target_principal_id")}
	switch f.Status {
	case "", domain.AssignmentPendingApproval, domain.AssignmentProvisioned, domain.AssignmentRejected, domain.AssignmentCancelled,
		domain.AssignmentRevoked, domain.AssignmentExpired:
	default:
		writeError(w, http.StatusBadRequest, "invalid_status", "unknown status "+strconv.Quote(f.Status))
		return
	}
	var err error
	if f.Limit, err = parseIntQuery(r, "limit"); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	if f.Offset, err = parseIntQuery(r, "offset"); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_offset", err.Error())
		return
	}
	list, err := g.store.ListAssignmentRequests(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.AssignmentRequest{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (g *Gov) GetAssignment(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	a, err := g.store.GetAssignmentRequest(r.Context(), chi.URLParam(r, "request_id"))
	if err != nil {
		if errors.Is(err, domain.ErrAssignmentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// assignmentCommand serves approve / reject / cancel / revoke.
func (g *Gov) assignmentCommand(verb string) http.HandlerFunc {
	op := map[string]string{
		"approve": telemetry.GovApproveAssignment, "reject": telemetry.GovRejectAssignment,
		"cancel": telemetry.GovCancelAssignment, "revoke": telemetry.GovRevokeAssignment,
	}[verb]
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := chi.URLParam(r, "request_id")
		var req domain.AssignmentDecisionRequest
		if err := decode(r, &req); err != nil {
			g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
			return
		}
		if req.LegalEntityID == "" || req.CorrelationID == "" {
			g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields", "legal_entity_id and correlation_id are required", req)
			return
		}
		if verb != "approve" && strings.TrimSpace(req.Reason) == "" {
			g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_reason", "a reason is required to "+verb+" an assignment request", req)
			return
		}
		principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
		if !ok {
			return
		}
		fail := func(err error) {
			g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
		}
		a, err := g.store.GetAssignmentRequest(r.Context(), requestID)
		if err != nil {
			fail(err)
			return
		}
		// The ROLE_MANAGE check in begin ran on the BODY's entity. Bound to
		// the request's own entity here, or a manager of one entity could
		// decide another entity's requests by naming their own in the body.
		if req.LegalEntityID != a.LegalEntityID {
			fail(domain.ErrEntityMismatch)
			return
		}
		scope := clients.Scope{PrincipalID: principalID, TenantID: tenantID, LegalEntityID: req.LegalEntityID, CorrelationID: req.CorrelationID,
			Purpose: req.Reason, ApprovalReference: a.RequestID}

		switch verb {
		case "approve", "reject":
			if a.Status != domain.AssignmentPendingApproval {
				fail(domain.ErrAssignmentState)
				return
			}
			// Independence. The subject never decides their own access, and
			// the requester's own request is cancelled, not rejected or
			// approved by them (§10.1 row 6; §10.2 own access elevation).
			if principalID == a.TargetPrincipalID || principalID == a.RequestedByPrincipalID {
				g.refuse(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, http.StatusForbidden, "self_approval", string(domain.ErrSelfApproval), req)
				return
			}
			if verb == "reject" {
				out, err := g.store.DecideAssignmentRequest(r.Context(), a.RequestID, domain.AssignmentRejected, principalID, req.Reason, "")
				if err != nil {
					fail(err)
					return
				}
				g.count(op, telemetry.WriteUpdated)
				writeJSON(w, http.StatusOK, out)
				return
			}
			role, err := g.h.store.GetRole(r.Context(), a.RoleDefinitionID)
			if err != nil {
				fail(err)
				return
			}
			if role.Status != domain.RoleStatusActive {
				g.refuse(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, http.StatusConflict, "role_retired", string(domain.ErrRoleRetired), req)
				return
			}
			if a.EffectiveTo != nil && !a.EffectiveTo.After(time.Now()) {
				fail(domain.ErrAssignmentWindowOver)
				return
			}
			// §9: approval by manager, data owner or SECURITY depending on
			// risk. ROLE_MANAGE (begin) is the manager tier; a CRITICAL grant
			// also needs the approver to hold the security approval action.
			if a.RiskTier == domain.RiskCritical {
				if err := g.checkAction(r.Context(), principalID, a.LegalEntityID, domain.ActionApprovePrivileged); err != nil {
					if errors.Is(err, domain.ErrAuthorizationDenied) {
						fail(domain.ErrSecurityApproval)
					} else {
						fail(domain.ErrAuthzServiceUnavailable)
					}
					return
				}
			}
			// Re-checked at approval: what the subject holds, and what the
			// role grants, may both have changed since the request.
			actions, err := g.roleActions(r.Context(), role.RoleDefinitionID)
			if err != nil {
				fail(err)
				return
			}
			if len(actions) > 0 {
				if err := g.h.sod.CheckConflict(r.Context(), domain.SoDCheckRequest{
					TenantID: tenantID, CallerID: principalID, CorrelationID: req.CorrelationID, CandidateActions: actions,
					SubjectPrincipalID: a.TargetPrincipalID, LegalEntityID: a.LegalEntityID,
				}); err != nil {
					fail(err)
					return
				}
			}
			id, err := g.admin.CreateRoleAssignment(r.Context(), a.RequestID, a.TargetPrincipalID, a.RoleDefinitionID, assignmentEntity(role, a.LegalEntityID), a.EffectiveFrom, a.EffectiveTo, scope)
			if err != nil {
				g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateAssignment, adminOutcome(err)).Inc()
				g.withdrawPending(r.Context(), err, id, scope)
				fail(err)
				return
			}
			g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminCreateAssignment, telemetry.AdminOK).Inc()
			out, err := g.store.DecideAssignmentRequest(r.Context(), a.RequestID, domain.AssignmentProvisioned, principalID, req.Reason, id)
			if err != nil {
				fail(err)
				return
			}
			g.count(op, telemetry.WriteUpdated)
			writeJSON(w, http.StatusOK, out)

		case "cancel":
			if principalID != a.RequestedByPrincipalID {
				g.refuse(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, http.StatusForbidden, "not_requester", "only the requester can cancel an assignment request", req)
				return
			}
			out, err := g.store.DecideAssignmentRequest(r.Context(), a.RequestID, domain.AssignmentCancelled, principalID, req.Reason, "")
			if err != nil {
				fail(err)
				return
			}
			g.count(op, telemetry.WriteUpdated)
			writeJSON(w, http.StatusOK, out)

		case "revoke":
			if a.Status != domain.AssignmentProvisioned {
				fail(domain.ErrAssignmentState)
				return
			}
			// Effective-dated removal (§9): schedule the end there, record it
			// here, and leave the row PROVISIONED until the expiry sweep closes
			// it and enqueues iam.assignment.revoked at that instant.
			if req.EffectiveAt != nil {
				at := req.EffectiveAt.UTC()
				if !at.After(time.Now()) {
					fail(domain.ErrInvalidEffectiveAt)
					return
				}
				if a.EffectiveTo != nil && !at.Before(*a.EffectiveTo) {
					g.refuse(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, http.StatusConflict, "ends_sooner",
						"the assignment already ends at "+a.EffectiveTo.UTC().Format(time.RFC3339)+"; a scheduled revoke can only bring the end earlier", req)
					return
				}
				err := g.admin.ScheduleRoleAssignmentEnd(r.Context(), a.AuthzAssignmentID, at, scope)
				if err != nil && !errors.Is(err, domain.ErrAuthzAssignmentAbsent) {
					g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminScheduleRevoke, adminOutcome(err)).Inc()
					fail(err)
					return
				}
				g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminScheduleRevoke, telemetry.AdminOK).Inc()
				var out *domain.AssignmentRequest
				if errors.Is(err, domain.ErrAuthzAssignmentAbsent) {
					// Already ended there: nothing to schedule, close it now.
					out, err = g.store.RevokeAssignmentRequest(r.Context(), a.RequestID, principalID, req.Reason)
				} else {
					out, err = g.store.ScheduleAssignmentEnd(r.Context(), a.RequestID, principalID, req.Reason, at)
				}
				if err != nil {
					fail(err)
					return
				}
				g.count(op, telemetry.WriteUpdated)
				writeJSON(w, http.StatusOK, out)
				return
			}
			// Revoke there first; a 404 there means it has already ended
			// (expired, or revoked through the admin API), which is the
			// outcome asked for, so the record is closed either way.
			if err := g.admin.RevokeRoleAssignment(r.Context(), a.AuthzAssignmentID, scope); err != nil && !errors.Is(err, domain.ErrAuthzAssignmentAbsent) {
				g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminRevokeAssignment, adminOutcome(err)).Inc()
				fail(err)
				return
			}
			g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminRevokeAssignment, telemetry.AdminOK).Inc()
			out, err := g.store.RevokeAssignmentRequest(r.Context(), a.RequestID, principalID, req.Reason)
			if err != nil {
				fail(err)
				return
			}
			g.count(op, telemetry.WriteUpdated)
			writeJSON(w, http.StatusOK, out)
		}
	}
}

// withdrawPending ends an assignment authorization-svc parked PENDING_APPROVAL
// (ErrAuthzApprovalPending), so a grant this service did not record as
// provisioned cannot be approved into force later behind its back. Best
// effort: the pending row also expires on its own after its window.
func (g *Gov) withdrawPending(ctx context.Context, err error, assignmentID string, scope clients.Scope) {
	if !errors.Is(err, domain.ErrAuthzApprovalPending) || assignmentID == "" {
		return
	}
	scope.ReasonCode, scope.Purpose = "PROVISIONING_WITHDRAWN", "authorization-svc did not accept the provisioning principal as the independent approver"
	if rerr := g.admin.RevokeRoleAssignment(ctx, assignmentID, scope); rerr != nil && !errors.Is(rerr, domain.ErrAuthzAssignmentAbsent) {
		g.h.log.Warn("could not withdraw an assignment authorization-svc holds pending", zap.String("assignment_id", assignmentID), zap.Error(rerr))
	}
}

// ── access review campaigns ─────────────────────────────────────────────────

// CreateCampaign handles POST /v1/access-review-campaigns.
//
// Snapshots the ACTIVE assignments of the roles it covers (all of this
// tenant's roles, retired ones included, unless role_definition_ids narrows
// it) from authorization-svc, and builds one item per assignment:
//
//   - risk tier from the role's actions (000008), CRITICAL for protected ones;
//   - ORPHANED_ROLE when the role is retired here but still assigned there;
//   - SOD_CONFLICT when one principal's reviewed roles in one entity combine
//     to a §10.1 conflict (§24 "toxic combinations"), raising the item to at
//     least HIGH so the campaign cannot close over it undecided;
//   - no self-attestation: an item whose subject is the default reviewer goes
//     to escalation_reviewer_principal_id, which is then required.
func (g *Gov) CreateCampaign(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovCreateCampaign
	var req domain.CreateCampaignRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	if req.LegalEntityID == "" || strings.TrimSpace(req.CampaignName) == "" || req.DefaultReviewerPrincipalID == "" || req.DueAt.IsZero() || req.CorrelationID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, campaign_name, review_type, default_reviewer_principal_id, due_at and correlation_id are required", req)
		return
	}
	switch req.ReviewType {
	case "PERIODIC", "EVENT_TRIGGERED", "PRIVILEGED":
	default:
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_review_type", "review_type must be PERIODIC, EVENT_TRIGGERED or PRIVILEGED", req)
		return
	}
	if req.ReviewType == "EVENT_TRIGGERED" && strings.TrimSpace(req.TriggerReason) == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_trigger_reason", "an EVENT_TRIGGERED review names its trigger (§24: manager change, entity transfer, privilege grant, ...)", req)
		return
	}
	if !req.DueAt.After(time.Now()) {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_due_at", "due_at must be in the future", req)
		return
	}
	if req.DormancyDays == 0 {
		req.DormancyDays = domain.DefaultDormancyDays
	}
	if req.DormancyDays < 1 || req.DormancyDays > 3650 {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_dormancy_days", "dormancy_days must be between 1 and 3650", req)
		return
	}
	principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
	if !ok {
		return
	}
	c, created, err := g.buildCampaign(r.Context(), tenantID, principalID, req, false)
	if err != nil {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
		return
	}
	if created {
		g.count(op, telemetry.WriteCreated)
		writeJSON(w, http.StatusCreated, c)
		return
	}
	g.count(op, telemetry.WriteReplayed)
	writeJSON(w, http.StatusOK, c)
}

// buildCampaign snapshots the assignments a campaign covers, builds its items
// and records it. Shared by POST /v1/access-review-campaigns and the HR-event
// trigger (hrevents), which opens an EVENT_TRIGGERED review of one subject.
// callerID is the principal the authorization-svc reads run as. created=false
// is a replay of the correlation id. skipEmpty records nothing, and returns
// (nil, false, nil), when no assignment matched: a leaver who held nothing
// needs no review.
func (g *Gov) buildCampaign(ctx context.Context, tenantID, callerID string, req domain.CreateCampaignRequest, skipEmpty bool) (*domain.ReviewCampaign, bool, error) {
	if prior, err := g.store.FindCampaignByCorrelation(ctx, req.CorrelationID); err != nil {
		return nil, false, err
	} else if prior != nil {
		full, err := g.store.GetCampaign(ctx, prior.CampaignID)
		return full, false, err
	}

	var roles []domain.RoleDefinition
	if len(req.RoleDefinitionIDs) > 0 {
		for _, id := range req.RoleDefinitionIDs {
			role, err := g.h.store.GetRole(ctx, id)
			if err != nil {
				return nil, false, err
			}
			roles = append(roles, *role)
		}
	} else {
		all, err := g.h.store.ListRoles(ctx, domain.ListFilter{})
		if err != nil {
			return nil, false, err
		}
		roles = all
	}

	scope := clients.Scope{PrincipalID: callerID, TenantID: tenantID, LegalEntityID: req.LegalEntityID, CorrelationID: req.CorrelationID}
	now := time.Now().UTC()
	dormantBefore := now.AddDate(0, 0, -req.DormancyDays)
	var items []domain.ReviewItem
	for _, role := range roles {
		actions, err := g.roleActions(ctx, role.RoleDefinitionID)
		if err != nil {
			return nil, false, err
		}
		risk, _, err := g.h.roleRisk(ctx, actions)
		if err != nil {
			return nil, false, err
		}
		if req.ReviewType == "PRIVILEGED" && domain.RiskRank(risk) < domain.RiskRank(domain.RiskHigh) {
			continue
		}
		assignments, err := g.admin.ListRoleAssignments(ctx, role.RoleDefinitionID, scope)
		if err != nil {
			g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminListAssignments, adminOutcome(err)).Inc()
			return nil, false, err
		}
		g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminListAssignments, telemetry.AdminOK).Inc()
		for _, as := range assignments {
			if req.SubjectPrincipalID != "" && as.PrincipalID != req.SubjectPrincipalID {
				continue
			}
			flags := []string{}
			if role.Status != domain.RoleStatusActive {
				flags = append(flags, domain.FlagOrphanedRole)
			}
			reviewer := req.DefaultReviewerPrincipalID
			if as.PrincipalID == reviewer {
				if req.EscalationReviewerPrincipalID == "" || req.EscalationReviewerPrincipalID == as.PrincipalID {
					return nil, false, domain.ErrEscalationReviewerReq
				}
				reviewer = req.EscalationReviewerPrincipalID
				flags = append(flags, domain.FlagSelfReviewReassigned)
			}
			entity := ""
			if as.LegalEntityID != nil {
				entity = *as.LegalEntityID
			}
			itemRisk := risk
			// §24 orphan detection: an assignment "without valid owner". A
			// suspended or disabled subject still holding the role is raised
			// to HIGH, so the campaign cannot close over it undecided.
			if as.PrincipalStatus != "" && as.PrincipalStatus != "ACTIVE" {
				flags = append(flags, domain.FlagSubjectInactive)
				if domain.RiskRank(itemRisk) < domain.RiskRank(domain.RiskHigh) {
					itemRisk = domain.RiskHigh
				}
			}
			// §24 dormancy: a privileged / high-risk assignment held for the
			// whole window with no GRANTED decision inside it. Flagged for
			// removal, never removed: "usage alone does not prove necessity".
			if domain.RiskRank(risk) >= domain.RiskRank(domain.RiskHigh) && !as.EffectiveFrom.IsZero() && as.EffectiveFrom.Before(dormantBefore) &&
				(as.LastGrantedAt == nil || as.LastGrantedAt.Before(dormantBefore)) {
				flags = append(flags, domain.FlagDormant)
			}
			items = append(items, domain.ReviewItem{
				ItemID: uuid.NewString(), TenantID: tenantID, AuthzAssignmentID: as.PrincipalRoleAssignmentID,
				TargetPrincipalID: as.PrincipalID, RoleDefinitionID: role.RoleDefinitionID, RoleCode: role.RoleCode,
				LegalEntityID: entity, GrantedActions: actions, RiskTier: itemRisk, Flags: flags,
				ReviewerPrincipalID: reviewer, Status: domain.ReviewItemOpen, CreatedAt: now, UpdatedAt: now,
				LastGrantedAt: as.LastGrantedAt, SubjectStatus: as.PrincipalStatus,
			})
		}
	}

	if skipEmpty && len(items) == 0 {
		return nil, false, nil
	}
	if err := g.flagToxicCombinations(ctx, tenantID, callerID, req.CorrelationID, items); err != nil {
		return nil, false, err
	}

	c := &domain.ReviewCampaign{
		CampaignID: uuid.NewString(), TenantID: tenantID, CampaignName: req.CampaignName, ReviewType: req.ReviewType,
		TriggerReason: req.TriggerReason, LegalEntityID: req.LegalEntityID, DefaultReviewerPrincipalID: req.DefaultReviewerPrincipalID,
		Status: domain.CampaignOpen, DueAt: req.DueAt.UTC(), DormancyDays: req.DormancyDays, CreatedByPrincipalID: callerID,
		CorrelationID: req.CorrelationID, CreatedAt: now, UpdatedAt: now,
	}
	created, err := g.store.CreateCampaign(ctx, c, items, callerID)
	if err != nil {
		return nil, false, err
	}
	if c.Items == nil {
		c.Items = []domain.ReviewItem{}
	}
	return c, created, nil
}

// flagToxicCombinations asks the SoD engine whether a principal's reviewed
// roles, together, conflict within an entity.
//
// A TENANT-scoped assignment (no entity) applies in EVERY entity, so it joins
// each of the principal's entity groups. Grouping by (principal, entity)
// alone, as this used to, put a tenant-wide Payment Preparer and an entity
// Payment Releaser in different groups and never compared them: the toxic
// combination §24 exists to catch passed the review clean.
func (g *Gov) flagToxicCombinations(ctx context.Context, tenantID, callerID, correlationID string, items []domain.ReviewItem) error {
	byPrincipal := map[string]map[string][]int{}
	for i, it := range items {
		if byPrincipal[it.TargetPrincipalID] == nil {
			byPrincipal[it.TargetPrincipalID] = map[string][]int{}
		}
		byPrincipal[it.TargetPrincipalID][it.LegalEntityID] = append(byPrincipal[it.TargetPrincipalID][it.LegalEntityID], i)
	}
	var groups [][]int
	for _, entities := range byPrincipal {
		tenantWide := entities[""]
		scoped := false
		for entity, idx := range entities {
			if entity == "" {
				continue
			}
			scoped = true
			groups = append(groups, append(append([]int{}, idx...), tenantWide...))
		}
		if !scoped {
			groups = append(groups, tenantWide)
		}
	}
	for _, idx := range groups {
		if len(idx) < 2 {
			continue
		}
		seen := map[string]bool{}
		var union []string
		for _, i := range idx {
			for _, a := range items[i].GrantedActions {
				if !seen[a] {
					seen[a] = true
					union = append(union, a)
				}
			}
		}
		if len(union) == 0 {
			continue
		}
		err := g.h.sod.CheckConflict(ctx, domain.SoDCheckRequest{TenantID: tenantID, CallerID: callerID, CorrelationID: correlationID, CandidateActions: union})
		var conflict *domain.SoDConflictError
		switch {
		case err == nil:
			continue
		case errors.As(err, &conflict):
			involved := map[string]bool{}
			for _, c := range conflict.Conflicts {
				involved[c.CandidateAction], involved[c.ConflictsWith] = true, true
			}
			for _, i := range idx {
				for _, a := range items[i].GrantedActions {
					if involved[a] {
						if !hasFlag(items[i].Flags, domain.FlagSoDConflict) {
							items[i].Flags = append(items[i].Flags, domain.FlagSoDConflict)
						}
						if domain.RiskRank(items[i].RiskTier) < domain.RiskRank(domain.RiskHigh) {
							items[i].RiskTier = domain.RiskHigh
						}
						break
					}
				}
			}
		default:
			return err
		}
	}
	return nil
}

func hasFlag(flags []string, f string) bool {
	for _, x := range flags {
		if x == f {
			return true
		}
	}
	return false
}

func (g *Gov) ListCampaigns(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && status != domain.CampaignOpen && status != domain.CampaignCompleted {
		writeError(w, http.StatusBadRequest, "invalid_status", "status must be OPEN or COMPLETED")
		return
	}
	limit, err := parseIntQuery(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}
	offset, err := parseIntQuery(r, "offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_offset", err.Error())
		return
	}
	list, err := g.store.ListCampaigns(r.Context(), status, limit, offset)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.ReviewCampaign{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (g *Gov) GetCampaign(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	c, err := g.store.GetCampaign(r.Context(), chi.URLParam(r, "campaign_id"))
	if err != nil {
		if errors.Is(err, domain.ErrCampaignNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if c.Items == nil {
		c.Items = []domain.ReviewItem{}
	}
	writeJSON(w, http.StatusOK, c)
}

// ListMyReviewItems handles GET /v1/iam/access-reviews: the items assigned to
// the caller. Principals only: a workload has no reviews.
func (g *Gov) ListMyReviewItems(w http.ResponseWriter, r *http.Request) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return
	}
	if _, ok := g.h.requireTenant(w, r, nil); !ok {
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", domain.ReviewItemOpen, domain.ReviewItemDecided, domain.ReviewItemEscalated, domain.ReviewItemExpired:
	default:
		writeError(w, http.StatusBadRequest, "invalid_status", "status must be OPEN, DECIDED, ESCALATED or EXPIRED")
		return
	}
	list, err := g.store.ListReviewItems(r.Context(), principalID, status)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.ReviewItem{}
	}
	writeJSON(w, http.StatusOK, list)
}

// DecideReviewItem handles POST .../items/{item_id}/decide.
//
// The authority is the designation: only the item's reviewer may decide it,
// and never the item's subject. REVOKE revokes the assignment in
// authorization-svc as the reviewer before the decision is recorded (so the
// reviewer needs iam.assignment.revoke there; without it the item stays open
// and the caller gets 403 provisioning_forbidden). MODIFY is recorded as a
// decision whose follow-up is a new assignment request; ESCALATE leaves the
// item ESCALATED for reassignment.
func (g *Gov) DecideReviewItem(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovDecideReviewItem
	campaignID, itemID := chi.URLParam(r, "campaign_id"), chi.URLParam(r, "item_id")
	var req domain.DecideItemRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	req.Decision = strings.ToUpper(strings.TrimSpace(req.Decision))
	switch req.Decision {
	case domain.DecisionKeep, domain.DecisionRevoke, domain.DecisionModify, domain.DecisionEscalate:
	default:
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_decision", "decision must be KEEP, REVOKE, MODIFY or ESCALATE", req)
		return
	}
	if strings.TrimSpace(req.Reason) == "" || req.CorrelationID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields", "reason and correlation_id are required (§24: the evidence records the reason)", req)
		return
	}
	principalID, tenantID, ok := g.identify(w, r, op)
	if !ok {
		return
	}
	fail := func(err error) {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
	}

	it, err := g.store.GetReviewItem(r.Context(), campaignID, itemID)
	if err != nil {
		fail(err)
		return
	}
	if it.TargetPrincipalID == principalID {
		g.refuse(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, http.StatusForbidden, "self_attestation", string(domain.ErrSelfAttestation), req)
		return
	}
	if it.ReviewerPrincipalID != principalID {
		g.refuse(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, http.StatusForbidden, "not_item_reviewer", string(domain.ErrNotItemReviewer), req)
		return
	}
	if it.Status == domain.ReviewItemDecided || it.Status == domain.ReviewItemExpired {
		fail(domain.ErrItemAlreadyDecided)
		return
	}

	campaign, err := g.store.GetCampaign(r.Context(), campaignID)
	if err != nil {
		fail(err)
		return
	}
	if req.LegalEntityID != "" && req.LegalEntityID != campaign.LegalEntityID {
		fail(domain.ErrEntityMismatch)
		return
	}
	revoked := false
	if req.Decision == domain.DecisionRevoke {
		if campaign.Status != domain.CampaignOpen {
			fail(domain.ErrCampaignClosed)
			return
		}
		executor := principalID
		if g.servicePrincipalID != "" {
			executor = g.servicePrincipalID
		}
		scope := clients.Scope{PrincipalID: executor, TenantID: tenantID, LegalEntityID: campaign.LegalEntityID, CorrelationID: req.CorrelationID,
			ReasonCode: "ACCESS_REVIEW_REVOKE",
			Purpose:    "access review " + campaignID + " item " + itemID + " decided REVOKE by " + principalID + ": " + req.Reason}
		if err := g.admin.RevokeRoleAssignment(r.Context(), it.AuthzAssignmentID, scope); err != nil && !errors.Is(err, domain.ErrAuthzAssignmentAbsent) {
			g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminRevokeAssignment, adminOutcome(err)).Inc()
			fail(err)
			return
		}
		g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminRevokeAssignment, telemetry.AdminOK).Inc()
		revoked = true
	}
	out, err := g.store.DecideReviewItem(r.Context(), campaignID, itemID, req.Decision, req.Reason, principalID, revoked)
	if err != nil {
		fail(err)
		return
	}
	g.count(op, telemetry.WriteUpdated)
	writeJSON(w, http.StatusOK, out)
}

// ReassignReviewItem hands an undecided or escalated item to a new reviewer.
func (g *Gov) ReassignReviewItem(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovReassignReviewItem
	campaignID, itemID := chi.URLParam(r, "campaign_id"), chi.URLParam(r, "item_id")
	var req domain.ReassignItemRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	if req.LegalEntityID == "" || req.ReviewerPrincipalID == "" || req.CorrelationID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields", "legal_entity_id, reviewer_principal_id and correlation_id are required", req)
		return
	}
	principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
	if !ok {
		return
	}
	fail := func(err error) {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
	}
	if err := g.campaignEntity(r.Context(), campaignID, req.LegalEntityID); err != nil {
		fail(err)
		return
	}
	it, err := g.store.GetReviewItem(r.Context(), campaignID, itemID)
	if err != nil {
		fail(err)
		return
	}
	if it.TargetPrincipalID == req.ReviewerPrincipalID {
		g.refuse(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, http.StatusBadRequest, "self_attestation", string(domain.ErrSelfAttestation), req)
		return
	}
	out, err := g.store.ReassignReviewItem(r.Context(), campaignID, itemID, req.ReviewerPrincipalID)
	if err != nil {
		fail(err)
		return
	}
	g.count(op, telemetry.WriteUpdated)
	writeJSON(w, http.StatusOK, out)
}

// campaignEntity binds a command's legal_entity_id (on which begin checked
// ROLE_MANAGE) to the campaign's own entity.
func (g *Gov) campaignEntity(ctx context.Context, campaignID, legalEntityID string) error {
	c, err := g.store.GetCampaign(ctx, campaignID)
	if err != nil {
		return err
	}
	if c.LegalEntityID != legalEntityID {
		return domain.ErrEntityMismatch
	}
	return nil
}

// CompleteCampaign handles POST .../{campaign_id}/complete.
func (g *Gov) CompleteCampaign(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovCompleteCampaign
	campaignID := chi.URLParam(r, "campaign_id")
	var req domain.CompleteCampaignRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	if req.LegalEntityID == "" || req.CorrelationID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields", "legal_entity_id and correlation_id are required", req)
		return
	}
	principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
	if !ok {
		return
	}
	if err := g.campaignEntity(r.Context(), campaignID, req.LegalEntityID); err != nil {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
		return
	}
	out, err := g.store.CompleteCampaign(r.Context(), campaignID, principalID)
	if err != nil {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
		return
	}
	g.count(op, telemetry.WriteUpdated)
	writeJSON(w, http.StatusOK, out)
}
