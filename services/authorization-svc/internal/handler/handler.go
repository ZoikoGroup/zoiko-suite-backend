package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/jurisdiction"
	"zoiko.io/authorization-svc/internal/siem"
)

// AuthorizationStore is the narrow interface the handler depends on.
type AuthorizationStore interface {
	CreateRole(ctx context.Context, params domain.CreateRoleParams) (*domain.Role, bool, error)
	SetRoleActive(ctx context.Context, roleID, tenantID string, active bool, expectedVersion int64) (*domain.Role, error)
	FindRoleByID(ctx context.Context, roleID string) (*domain.Role, error)
	// CreatePermissionBundle returns whether the row was CREATED or an
	// existing bundle_code was REPLACED. The upsert overwrites
	// permitted_actions wholesale, and a caller that had just emptied a
	// role's grant set used to get the same answer as one that created a
	// bundle — see PgStore.CreatePermissionBundle.
	CreatePermissionBundle(ctx context.Context, params domain.CreatePermissionBundleParams) (*domain.PermissionBundle, bool, error)
	// The bundle read and the bundle off switch. permitted_actions is what a
	// role actually permits and was write-only: ListRoles returns no actions,
	// and pb.active_flag sits in FindGrantedActions' JOIN with nothing able to
	// set it — the SetSoDRuleActive defect on the granting side.
	ListPermissionBundles(ctx context.Context, roleID, tenantID string) ([]domain.PermissionBundle, error)
	SetPermissionBundleActive(ctx context.Context, permissionBundleID, tenantID string, active bool, expectedVersion int64) (*domain.PermissionBundle, error)
	CreateRoleAssignment(ctx context.Context, params domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error)
	RevokeRoleAssignment(ctx context.Context, assignmentID, tenantID string) (*domain.PrincipalRoleAssignment, error)
	ListRoleAssignments(ctx context.Context, tenantID, principalID, roleID string, activeOnly bool) ([]domain.PrincipalRoleAssignment, error)
	// The catalogue reads. Both admin surfaces were write-only until now: a
	// role or a delegation could be created and revoked by id and never listed,
	// so neither register could be audited from outside the caller that wrote it.
	ListRoles(ctx context.Context, tenantID string, activeOnly bool) ([]domain.Role, error)
	ListDelegatedAuthorities(ctx context.Context, tenantID, principalID string, activeOnly bool) ([]domain.DelegatedAuthority, error)
	CreateDelegatedAuthority(ctx context.Context, params domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error)
	FindDelegatedAuthorityByID(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error)
	RevokeDelegatedAuthority(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error)
	CreateSoDRule(ctx context.Context, params domain.CreateSoDRuleParams) (*domain.SoDRule, error)
	ListSoDRules(ctx context.Context, tenantID string) ([]domain.SoDRule, error)
	// The off switch. active_flag is in CheckSoDConflict's predicate and was
	// reachable by no route at all — a conflict rule could be created and
	// never retired, on the one object whose blast radius is every principal
	// holding the pair.
	SetSoDRuleActive(ctx context.Context, sodRuleID, tenantID string, active bool, expectedVersion int64) (*domain.SoDRule, error)

	// ABAC — the attribute-condition layer. CreateABACRule/SetABACRuleActive/
	// ListABACRules are the admin surface; FindABACRules is the evaluation
	// read, called on the /v1/authorize path.
	CreateABACRule(ctx context.Context, params domain.CreateABACRuleParams) (*domain.ABACRule, error)
	SetABACRuleActive(ctx context.Context, abacRuleID, tenantID string, active bool, expectedVersion int64) (*domain.ABACRule, error)
	ListABACRules(ctx context.Context, tenantID, actionType string) ([]domain.ABACRule, error)
	FindABACRules(ctx context.Context, actionType, tenantID string) ([]domain.ABACRule, error)

	FindGrantedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error)
	FindGrantedActionsScoped(ctx context.Context, principalID, legalEntityID, tenantID, bookID, orgUnitID string) ([]string, string, error)
	FindDelegatedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error)
	FindDelegatedActionsScoped(ctx context.Context, principalID, legalEntityID, tenantID, bookID, orgUnitID string) ([]string, string, error)
	CreateAuthorityLimit(ctx context.Context, params domain.CreateAuthorityLimitParams) (*domain.AuthorityLimit, error)
	FindAuthorityLimitByID(ctx context.Context, limitID, tenantID string) (*domain.AuthorityLimit, error)
	ListAuthorityLimits(ctx context.Context, tenantID string, principalID, roleID, authorityType string) ([]domain.AuthorityLimit, error)
	CheckSoDConflict(ctx context.Context, grantedActions []string, candidateAction, tenantID string) (string, bool, error)
	// Satya's own-object Segregation-of-Duties check (ef4cc2c), kept as-is.
	CheckOwnObjectSoD(ctx context.Context, actionType, tenantID string) (bool, error)

	// TENANT-SCOPED SIGNATURES, DELIBERATELY. main carried the pre-f931093
	// shapes: RecordAccessDecision took positional args and FindAccessDecisionByID
	// took no tenant at all. Taking main's side here compiles perfectly and
	// silently reverts the cross-tenant fix — a tenant-wide assignment in tenant A
	// was granting actions against a legal entity in tenant B.
	RecordAccessDecision(ctx context.Context, params domain.RecordAccessDecisionParams) (*domain.AccessDecisionLog, error)
	FindAccessDecisionByID(ctx context.Context, accessDecisionID, tenantID string) (*domain.AccessDecisionLog, error)

	// The audit read. §8.3 requires denials to be "evidentially retrievable",
	// and by-id retrieval is retrieval only for somebody who already holds the
	// id — which, for a denial, exists only in the response given to the
	// service that was refused.
	ListAccessDecisions(ctx context.Context, tenantID string, params domain.ListAccessDecisionsParams) (*domain.AccessDecisionPage, error)

	// FindPrincipalStatus is layer 0 of the evaluation: a principal
	// identity-context-svc has SUSPENDED or DISABLED may execute nothing.
	// Returns domain.PrincipalStatusActive when no status has been projected,
	// so the layer is inert on a deployment that has seen no status event.
	//
	// ProjectPrincipalStatus is deliberately NOT here. The handler must never
	// write this table — identity-context-svc is authoritative for principal
	// standing, and an admin route that could override it would let this
	// service and that one disagree about who is suspended. Only
	// internal/events.LifecycleConsumer holds the writing interface.
	FindPrincipalStatus(ctx context.Context, principalID, tenantID string) (string, error)

	// FindEntityStatus is the legal entity's projected standing, "" when the
	// registry has published none (negative control layer 0.1).
	FindEntityStatus(ctx context.Context, legalEntityID, tenantID string) (string, error)

	// Privileged Access Management (JIT Elevation - ZS-IAM-001 §13 & §21).
	CreatePrivilegedSession(ctx context.Context, params domain.CreatePrivilegedSessionParams) (*domain.PrivilegedSession, error)
	FindPrivilegedSessionByID(ctx context.Context, sessionID, tenantID string) (*domain.PrivilegedSession, error)
	ListPrivilegedSessions(ctx context.Context, tenantID, principalID string, activeOnly bool) ([]domain.PrivilegedSession, error)
	RevokePrivilegedSession(ctx context.Context, sessionID, tenantID, revokedBy string) (*domain.PrivilegedSession, error)

	// Break-Glass Emergency Sessions (ZS-IAM-001 §14 & §21).
	CreateBreakGlassSession(ctx context.Context, params domain.CreateBreakGlassSessionParams) (*domain.BreakGlassSession, error)
	FindBreakGlassSessionByID(ctx context.Context, sessionID, tenantID string) (*domain.BreakGlassSession, error)
	ListBreakGlassSessions(ctx context.Context, tenantID, principalID string, activeOnly bool) ([]domain.BreakGlassSession, error)
	RevokeBreakGlassSession(ctx context.Context, sessionID, tenantID, revokedBy string) (*domain.BreakGlassSession, error)

	// Tenant Support Sessions (ZS-IAM-001 §15 & §21).
	CreateSupportSession(ctx context.Context, params domain.CreateSupportSessionParams) (*domain.SupportSession, error)
	FindSupportSessionByID(ctx context.Context, sessionID, tenantID string) (*domain.SupportSession, error)
	ListSupportSessions(ctx context.Context, tenantID string, activeOnly bool) ([]domain.SupportSession, error)
	RevokeSupportSession(ctx context.Context, sessionID, tenantID, revokedBy string) (*domain.SupportSession, error)

	// Workload Identity (ZS-IAM-001 §16)
	FindWorkloadBinding(ctx context.Context, workloadID, tenantID string) (*domain.WorkloadBinding, error)
	CreateWorkloadBinding(ctx context.Context, binding domain.WorkloadBinding) (*domain.WorkloadBinding, error)

	// Access Reviews & Continuous Access Certification (ZS-IAM-001 §21, §24)
	CreateAccessReview(ctx context.Context, review domain.AccessReview) (*domain.AccessReview, error)
	GetAccessReview(ctx context.Context, reviewID, tenantID string) (*domain.AccessReview, error)
	ListAccessReviews(ctx context.Context, tenantID, reviewerPrincipalID, status string) ([]domain.AccessReview, error)
	RecordAccessReviewDecision(ctx context.Context, reviewID, tenantID, decision, decisionReason, decidedBy string) (*domain.AccessReview, error)
}

// EventPublisher is the narrow interface the handler depends on.
type EventPublisher interface {
	PublishAuthorizationGranted(ctx context.Context, d domain.AccessDecisionLog) error
	PublishAuthorizationDenied(ctx context.Context, d domain.AccessDecisionLog) error
	PublishSoDViolationDetected(ctx context.Context, d domain.AccessDecisionLog, conflictingAction string) error
	PublishBreakGlassStarted(ctx context.Context, session domain.BreakGlassSession) error
	PublishBreakGlassEnded(ctx context.Context, session domain.BreakGlassSession) error
	PublishSupportSessionStarted(ctx context.Context, session domain.SupportSession) error
	PublishSupportSessionEnded(ctx context.Context, session domain.SupportSession) error
	PublishAccessReviewStarted(ctx context.Context, review domain.AccessReview) error
	PublishAccessReviewCompleted(ctx context.Context, review domain.AccessReview) error
	PublishPrivilegedSessionStarted(ctx context.Context, session domain.PrivilegedSession) error
	PublishPrivilegedSessionEnded(ctx context.Context, session domain.PrivilegedSession) error
	PublishAuthorityLimitChanged(ctx context.Context, limit domain.AuthorityLimit, action string) error
	PublishSoDPolicyPublished(ctx context.Context, rule domain.SoDRule) error
	PublishPolicySetPublished(ctx context.Context, version, tenantID, publishedBy string) error
}

type Handler struct {
	store                 AuthorizationStore
	publisher             EventPublisher
	jurisdictionValidator jurisdiction.Validator
	siem                  *siem.Client
	log                   *zap.Logger

	// platformScopeEntityID is the synthetic legal entity a platform-wide act
	// is authorized against — see requirePlatformAction. Empty refuses every
	// platform-wide act, which is the correct default for a control that has
	// not been provisioned yet.
	platformScopeEntityID string

	// enforceTenantOnAuthorize controls whether /v1/authorize requires a tenant
	// scope. When true, missing tenant returns 400. When false (default),
	// tenantless requests are allowed with a warning.
	enforceTenantOnAuthorize bool

	// decisionEvents, when set, moves decision events to the transactional
	// outbox: the store writes them with the decision and a relay publishes
	// them, so a Kafka outage delays them instead of losing them. Nil keeps the
	// direct publish. See UseOutbox.
	decisionEvents func(domain.AccessDecisionLog) ([]domain.OutboxMessage, error)

	// commandContract is the §16 enforcement mode for privileged and
	// destructive commands — see SetCommandContract.
	commandContract CommandContractMode
}

// UseOutbox routes authorization.granted / .denied and sod.violation.detected
// through outbox_events, built by build (events.DecisionEvents), instead of
// publishing them directly after the decision is recorded.
func (h *Handler) UseOutbox(build func(domain.AccessDecisionLog) ([]domain.OutboxMessage, error)) {
	h.decisionEvents = build
}

func New(store AuthorizationStore, publisher EventPublisher, jurisdictionValidator jurisdiction.Validator, siemClient *siem.Client, platformScopeEntityID string, enforceTenantOnAuthorize bool, log *zap.Logger) *Handler {
	return &Handler{
		store:                    store,
		publisher:                publisher,
		jurisdictionValidator:    jurisdictionValidator,
		siem:                     siemClient,
		platformScopeEntityID:    platformScopeEntityID,
		enforceTenantOnAuthorize: enforceTenantOnAuthorize,
		log:                      log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)
	r.Use(auditMiddleware)

	r.Post("/v1/admin/roles", h.CreateRole)
	r.Get("/v1/admin/roles", h.ListRoles)
	r.Post("/v1/admin/roles/{role_id}/retire", h.RetireRole)
	r.Post("/v1/admin/roles/{role_id}/reactivate", h.ReactivateRole)
	r.Post("/v1/admin/roles/{role_id}/permission-bundles", h.CreatePermissionBundle)
	r.Get("/v1/admin/roles/{role_id}/permission-bundles", h.ListPermissionBundles)
	r.Post("/v1/admin/permission-bundles/{permission_bundle_id}/retire", h.RetirePermissionBundle)
	r.Post("/v1/admin/permission-bundles/{permission_bundle_id}/reactivate", h.ReactivatePermissionBundle)
	r.Post("/v1/admin/role-assignments", h.CreateRoleAssignment)
	r.Get("/v1/admin/role-assignments", h.ListRoleAssignments)
	r.Post("/v1/admin/role-assignments/{assignment_id}/revoke", h.RevokeRoleAssignment)
	// Maker-checker on privileged assignments (GOV-12; ZS-IAM-001 §9, A20).
	r.Post("/v1/admin/role-assignments/{assignment_id}/approve", h.ApproveRoleAssignment)
	r.Post("/v1/admin/role-assignments/{assignment_id}/reject", h.RejectRoleAssignment)

	// GOV-03 commands (and the spec's illustrative contract surface).
	r.Post("/v1/admin/authorization-cache/invalidate", h.InvalidateAuthorizationCache)
	r.Post("/internal/v1/gov03/commands/invalidateAuthorizationCache", h.InvalidateAuthorizationCache)
	r.Post("/v1/admin/subjects/{principal_id}/effective-access/recompute", h.RecomputeSubjectEffectiveAccess)
	r.Post("/internal/v1/gov03/commands/recomputeSubjectEffectiveAccess", h.RecomputeSubjectEffectiveAccess)
	r.Post("/v1/admin/delegated-authorities", h.CreateDelegatedAuthority)
	r.Get("/v1/admin/delegated-authorities", h.ListDelegatedAuthorities)
	r.Post("/v1/admin/delegated-authorities/{delegation_id}/revoke", h.RevokeDelegatedAuthority)
	r.Post("/v1/admin/sod-rules", h.CreateSoDRule)
	r.Get("/v1/admin/sod-rules", h.ListSoDRules)
	r.Post("/v1/admin/sod-rules/{sod_rule_id}/retire", h.RetireSoDRule)
	r.Post("/v1/admin/sod-rules/{sod_rule_id}/reactivate", h.ReactivateSoDRule)
	// GOV-04 compensating-control exceptions.
	r.Post("/v1/admin/sod-exceptions", h.RequestSoDException)
	r.Get("/v1/admin/sod-exceptions", h.ListSoDExceptions)
	r.Post("/v1/admin/sod-exceptions/{sod_exception_id}/approve", h.ApproveSoDException)
	r.Post("/v1/admin/sod-exceptions/{sod_exception_id}/reject", h.RejectSoDException)
	r.Post("/v1/admin/sod-exceptions/{sod_exception_id}/revoke", h.RevokeSoDException)
	r.Get("/v1/sod/conflicting-permissions", h.ListConflictingPermissions)
	r.Post("/v1/admin/abac-rules", h.CreateABACRule)
	r.Get("/v1/admin/abac-rules", h.ListABACRules)
	r.Post("/v1/admin/abac-rules/{abac_rule_id}/retire", h.RetireABACRule)
	r.Post("/v1/admin/abac-rules/{abac_rule_id}/reactivate", h.ReactivateABACRule)

	// Privileged Access Management (ZS-IAM-001 §13 & §21)
	r.Post("/admin/v1/privileged-sessions", h.CreatePrivilegedSession)
	r.Get("/admin/v1/privileged-sessions", h.ListPrivilegedSessions)
	r.Post("/admin/v1/privileged-sessions/{session_id}/revoke", h.RevokePrivilegedSession)
	r.Post("/v1/admin/privileged-sessions", h.CreatePrivilegedSession)
	r.Get("/v1/admin/privileged-sessions", h.ListPrivilegedSessions)
	r.Post("/v1/admin/privileged-sessions/{session_id}/revoke", h.RevokePrivilegedSession)

	// Break-Glass Emergency Sessions (ZS-IAM-001 §14 & §21)
	r.Post("/admin/v1/break-glass-sessions", h.CreateBreakGlassSession)
	r.Get("/admin/v1/break-glass-sessions", h.ListBreakGlassSessions)
	r.Get("/admin/v1/break-glass-sessions/{session_id}", h.GetBreakGlassSession)
	r.Post("/admin/v1/break-glass-sessions/{session_id}/revoke", h.RevokeBreakGlassSession)
	r.Post("/v1/admin/break-glass-sessions", h.CreateBreakGlassSession)
	r.Get("/v1/admin/break-glass-sessions", h.ListBreakGlassSessions)
	r.Get("/v1/admin/break-glass-sessions/{session_id}", h.GetBreakGlassSession)
	r.Post("/v1/admin/break-glass-sessions/{session_id}/revoke", h.RevokeBreakGlassSession)

	// Tenant Support Sessions (ZS-IAM-001 §15 & §21)
	r.Post("/v1/support/sessions", h.CreateSupportSession)
	r.Get("/v1/support/sessions", h.ListSupportSessions)
	r.Get("/v1/support/sessions/{session_id}", h.GetSupportSession)
	r.Post("/v1/support/sessions/{session_id}/revoke", h.RevokeSupportSession)

	r.Post(AuthorizePath, h.Authorize)

	// Canonical Authorization Decision API (ZS-IAM-001 §8.1, §8.2, §21)
	r.Post("/internal/authorization/decisions", h.HandleCanonicalDecision)

	// Access Reviews & Continuous Access Certification (ZS-IAM-001 §21, §24)
	r.Get("/v1/iam/access-reviews", h.ListAccessReviews)
	r.Post("/v1/iam/access-reviews/{id}:decide", h.DecideAccessReview)
	r.Post("/v1/iam/access-reviews/{id}/decide", h.DecideAccessReview)

	// Available Actions (ZS-IAM-001 §21, ZS-STATE-001)
	r.Get("/v1/{resource}/{id}/available-actions", h.GetAvailableActions)
	r.Get("/v1/{resource_type}/{resource_id}/available-actions", h.GetAvailableActions)
	r.Get("/v1/me/capabilities", h.GetMyCapabilities)

	// The other three inbound APIs Doc 03 §8.3 names. Folded into
	// /v1/authorize as internal layers until now, which left three questions
	// unanswerable — see internal/handler/validation.go's header for what each
	// one is and why it is not a material write.
	r.Post(EntityScopeValidatePath, h.ValidateEntityScope)
	r.Post(SoDValidatePath, h.ValidateSoDConflicts)
	r.Post(SoDEvaluatePath, h.EvaluateSoD)
	r.Post(DelegatedAccessEvaluatePath, h.EvaluateDelegatedAccess)

	// "Retrieve authorization rationale" — both halves. The collection read is
	// what makes §8.3's "denials must be evidentially retrievable" true;
	// by-id alone is retrieval only for a caller that already holds the id.
	r.Get(AccessDecisionsPath, h.ListAccessDecisions)
	r.Get(AccessDecisionsPath+"/{access_decision_id}", h.GetAccessDecision)
}

// requirePrincipal reads the caller's verified principal from the
// X-Principal-Id header the gateway sets from a verified identity
// envelope, rejecting the request if absent.
//
// Before this fix, none of the /v1/admin/* routes checked this at all:
// created_by_principal_id / assigned_by / delegator_principal_id all came
// straight from the request body, on the platform's own authorization
// engine — the same "attribution taken from the request body defeats
// segregation of duties" shape already found and fixed in
// board-resolutions-svc.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "missing_principal",
			"message": "X-Principal-Id is required — the gateway sets it from a verified identity envelope",
		})
		return "", false
	}
	return principalID, true
}

// requireTenant reads the caller's verified tenant scope from the
// X-Tenant-Id header, rejecting the request if absent.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := r.Header.Get("X-Tenant-Id")
	if tenantID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "missing_tenant_scope",
			"message": "X-Tenant-Id is required — the gateway sets it from a verified identity envelope",
		})
		return "", false
	}
	if !validTenantScope(w, tenantID, "X-Tenant-Id") {
		return "", false
	}
	return tenantID, true
}

// validTenantScope refuses a tenant scope that is not a UUID, writing a 400.
//
// Same defect as a malformed legal_entity_id and one layer deeper, which is
// why it is easy to miss: tenant_id is UUID in every table that carries it,
// and withRLS installs the raw value into app.tenant_id — where the POLICY
// itself does `NULLIF(current_setting('app.tenant_id', true), '')::uuid`. So a
// malformed tenant does not fail in a query this service wrote; it fails
// inside row security, on every table, as a driver error the store reports as
// ErrStoreUnavailable and the handler answers 503 for.
//
// The result is the worst possible diagnosis: a caller sending a mistyped
// X-Tenant-Id is told the platform's authorization plane is down. Refused here
// instead, naming the header or field that was wrong.
//
// `field` names where the value came from — the header on most routes, the
// request body on the /v1/authorize fallback path — because "tenant_id is not a
// UUID" is unhelpful to a caller that never set a field called tenant_id.
func validTenantScope(w http.ResponseWriter, tenantID, field string) bool {
	if validScope(tenantID) {
		return true
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":   "invalid_scope",
		"field":   field,
		"message": field + " must be a UUID",
	})
	return false
}

// refuseForeignTenant reports whether claimed names a tenant other than
// verifiedTenant, writing a 403 if so. Before this fix, every /v1/admin/*
// route trusted whatever tenant_id the request body supplied outright —
// any caller could create a role, grant a permission bundle, or write a
// SoD rule into any tenant, purely by naming it in the JSON body.
func (h *Handler) refuseForeignTenant(w http.ResponseWriter, claimed, verifiedTenant string) bool {
	if claimed != "" && claimed != verifiedTenant {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "tenant_scope_mismatch",
			"message": "request tenant_id does not match the caller's verified tenant scope",
		})
		return true
	}
	return false
}

// refuseOwnRole reports whether principalID holds an assignment of roleID that
// has not ended — current or future-dated — writing a 403 if so, or a 503 if
// that cannot be established.
//
// CreateRoleAssignment refuses assigning a role to yourself, but widening a
// role you already hold is the same elevation by another route (ZS-IAM-001
// §10.2 "Own access elevation"): a holder of iam.permission_bundle.manage could
// add any action to their own role, or reactivate a bundle or role that was
// retired to take access away from them. Retiring is not checked — narrowing
// your own access elevates nothing.
func (h *Handler) refuseOwnRole(w http.ResponseWriter, r *http.Request, principalID, roleID, tenantID string) bool {
	held, err := h.store.ListRoleAssignments(r.Context(), tenantID, principalID, roleID, false)
	if err != nil {
		h.log.Error("own-role check failed — refusing",
			zap.String("correlation_id", r.Header.Get("X-Correlation-ID")),
			zap.String("role_id", roleID),
			zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return true
	}
	now := time.Now()
	for _, a := range held {
		if a.EffectiveTo == nil || a.EffectiveTo.After(now) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "self_grant_not_allowed",
				"message": "a principal cannot change the permissions of a role they hold",
			})
			return true
		}
	}
	return false
}

// requirePermission confirms the caller holds actionType in their tenant scope,
// and records the decision like any other.
//
// This service is the authorization engine, so it does not call itself over
// HTTP — it asks its own store the same question /v1/authorize asks.
func (h *Handler) requirePermission(w http.ResponseWriter, r *http.Request, principalID, tenantID, actionType string) bool {
	correlationID := r.Header.Get("X-Correlation-ID")

	actions, basis, err := h.store.FindGrantedActions(r.Context(), principalID, tenantID, tenantID)
	if err != nil {
		h.log.Error("permission check failed — refusing",
			zap.String("correlation_id", correlationID),
			zap.String("action_type", actionType),
			zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return false
	}

	outcome, decisionBasis := "DENIED", "no_grant"
	if contains(actions, actionType) {
		outcome, decisionBasis = "GRANTED", basis
	}

	if _, recErr := h.store.RecordAccessDecision(r.Context(), domain.RecordAccessDecisionParams{
		PrincipalID:   principalID,
		LegalEntityID: tenantID,
		ActionType:    actionType,
		Outcome:       outcome,
		Basis:         decisionBasis,
		CorrelationID: correlationID,
		TenantID:      tenantID,
	}); recErr != nil {
		h.log.Error("permission check: failed to record decision — refusing",
			zap.String("correlation_id", correlationID),
			zap.String("action_type", actionType),
			zap.Error(recErr))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return false
	}

	if outcome != "GRANTED" {
		h.siem.Stream(r.Context(), tenantID, "authorization.denied", siem.SeverityHigh,
			"Admin action "+actionType+" denied for principal "+principalID)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "authorization_denied",
			"message": actionType + " is required to perform this admin action",
		})
		return false
	}
	return true
}

// requirePlatformAction confirms the caller holds actionType at platform
// scope, and records the decision like any other.
//
// This service is the authorization engine, so it does not call itself over
// HTTP — it asks its own store the same question /v1/authorize asks. What it
// asks about is the synthetic platform-scope legal entity (config
// AUTHZ_PLATFORM_SCOPE_ENTITY_ID, the same pattern policy-svc uses for a
// policy with no owning entity), because a platform-wide act has no legal
// entity of its own.
//
// Fails closed in both directions: an unset platform-scope entity id refuses
// every platform-wide act rather than waving them through, and a store error
// is a refusal, not an allow. A grant here is an assignment on the
// platform-scope entity itself: a tenant-wide assignment (legal_entity_id NULL)
// does not match it (see FindGrantedActionsScoped), and making or ending an
// assignment on that entity itself requires a platform-scope grant (see
// CreateRoleAssignment and RevokeRoleAssignment). The assigned role may be
// owned by any tenant — this query runs with no tenant filter.
func (h *Handler) requirePlatformAction(w http.ResponseWriter, r *http.Request, principalID, actionType string) bool {
	correlationID := r.Header.Get("X-Correlation-ID")

	if h.platformScopeEntityID == "" {
		h.log.Error("platform-scope action refused: AUTHZ_PLATFORM_SCOPE_ENTITY_ID is not configured",
			zap.String("correlation_id", correlationID),
			zap.String("action_type", actionType))
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "platform_scope_not_configured",
			"message": "platform-wide actions require AUTHZ_PLATFORM_SCOPE_ENTITY_ID to be configured",
		})
		return false
	}

	// Empty tenant, deliberately: a platform-wide grant is held against the
	// platform-scope entity and is NOT satisfied by any tenant-level role, so
	// scoping this to the caller's tenant would refuse every platform act.
	actions, basis, err := h.store.FindGrantedActions(r.Context(), principalID, h.platformScopeEntityID, "")
	if err != nil {
		h.log.Error("platform-scope action check failed — refusing",
			zap.String("correlation_id", correlationID),
			zap.String("action_type", actionType),
			zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return false
	}

	outcome, decisionBasis := "DENIED", "no_grant"
	if contains(actions, actionType) {
		outcome, decisionBasis = "GRANTED", basis
	}

	// A platform-wide governance act is a material action, so it gets a
	// decision artifact whichever way it goes — the same constraint
	// /v1/authorize is held to. A failure to record is not a reason to
	// proceed: it is logged and the act is refused below if denied, and
	// allowed only when the decision was recorded.
	if _, recErr := h.store.RecordAccessDecision(r.Context(), domain.RecordAccessDecisionParams{
		PrincipalID:   principalID,
		LegalEntityID: h.platformScopeEntityID,
		ActionType:    actionType,
		Outcome:       outcome,
		Basis:         decisionBasis,
		CorrelationID: correlationID,
	}); recErr != nil {
		h.log.Error("platform-scope action: failed to record decision — refusing",
			zap.String("correlation_id", correlationID),
			zap.String("action_type", actionType),
			zap.Error(recErr))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return false
	}

	if outcome != "GRANTED" {
		h.siem.Stream(r.Context(), "", "authorization.denied", siem.SeverityHigh,
			"Platform-scope action "+actionType+" denied for principal "+principalID)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "authorization_denied",
			"message": actionType + " is required to act at platform scope",
		})
		return false
	}
	return true
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// ── POST /v1/admin/roles ─────────────────────────────────────────────────────

type createRoleRequest struct {
	RoleID        string `json:"role_id,omitempty"`
	TenantID      string `json:"tenant_id"`
	RoleCode      string `json:"role_code"`
	RoleName      string `json:"role_name"`
	RoleScopeType string `json:"role_scope_type"`
}

func (req createRoleRequest) missingField() string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.RoleCode == "":
		return "role_code"
	case req.RoleName == "":
		return "role_name"
	case req.RoleScopeType == "":
		return "role_scope_type"
	default:
		return ""
	}
}

// CreateRole handles POST /v1/admin/roles. Idempotent on (tenant_id, role_code).
//
// Response: 201 created / 200 idempotent replay / 400 missing field / 403 unauthorized / 409 conflict / 503 unavailable.
func (h *Handler) CreateRole(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.role.manage to create roles
	if !h.requirePermission(w, r, principalID, tenantScope, "iam.role.manage") {
		return
	}

	var req createRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	role, created, err := h.store.CreateRole(r.Context(), domain.CreateRoleParams{
		// created_by_principal_id is always the verified caller, never the
		// request body — see requirePrincipal's doc comment.
		RoleID: req.RoleID, TenantID: req.TenantID, RoleCode: req.RoleCode,
		RoleName: req.RoleName, RoleScopeType: req.RoleScopeType, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "role_conflict", "role_code": req.RoleCode})
			return
		}
		h.log.Error("CreateRole: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, role)
}

// ── POST /v1/admin/roles/{role_id}/retire | /reactivate ──────────────────────

// RetireRole handles POST /v1/admin/roles/{role_id}/retire.
//
// Sets active_flag false, which is what actually stops the role granting
// anything: FindGrantedActions joins through `roles.active_flag`, so every
// action this role conferred disappears from the next /v1/authorize decision
// for every principal holding it. Assignments are left intact so the effect is
// reversible — see SetRoleActive for why cascading revocation is a separate
// decision.
//
// Idempotent: retiring an already-retired role is 200, not 409. The caller
// asked for a state, and that state holds.
//
// Response: 200 retired / 401 missing principal or tenant scope /
// 404 role not found or owned by another tenant / 503 unavailable.
func (h *Handler) RetireRole(w http.ResponseWriter, r *http.Request) {
	h.setRoleActive(w, r, false)
}

// ReactivateRole handles POST /v1/admin/roles/{role_id}/reactivate.
//
// Restores exactly the access the retirement suspended, because the
// assignments were never removed. Response shape matches RetireRole.
func (h *Handler) ReactivateRole(w http.ResponseWriter, r *http.Request) {
	h.setRoleActive(w, r, true)
}

func (h *Handler) setRoleActive(w http.ResponseWriter, r *http.Request, active bool) {
	roleID := chi.URLParam(r, "role_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.role.manage to retire/reactivate roles
	if !h.requirePermission(w, r, principalID, tenantScope, "iam.role.manage") {
		return
	}

	// Same doctrine as CreatePermissionBundle and CreateRoleAssignment: the
	// role's OWN tenant decides scope, and it must be the caller's. 404 rather
	// than 403 so a probe against another tenant's role_id cannot confirm it
	// exists.
	role, err := h.store.FindRoleByID(r.Context(), roleID)
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": roleID})
			return
		}
		h.log.Error("setRoleActive: role lookup failed",
			zap.String("correlation_id", correlationID),
			zap.String("role_id", roleID),
			zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if role.TenantID != tenantScope {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": roleID})
		return
	}
	if active && h.refuseOwnRole(w, r, principalID, roleID, tenantScope) {
		return
	}
	if active {
		actions, err := h.roleActiveActions(r.Context(), roleID, tenantScope)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if h.refuseToxicForHolders(w, r, roleID, tenantScope, actions) {
			return
		}
	}

	expectedVersion, ok := h.readCommand(w, &r)
	if !ok {
		return
	}
	role, err = h.store.SetRoleActive(r.Context(), roleID, tenantScope, active, expectedVersion)
	if err != nil {
		if errors.Is(err, domain.ErrVersionConflict) {
			writeVersionConflict(w)
			return
		}
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found"})
			return
		}
		h.log.Error("setRoleActive: store unavailable",
			zap.String("correlation_id", correlationID),
			zap.String("role_id", roleID),
			zap.Bool("active", active),
			zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Logged at info because this is a change to what the platform will
	// enforce, not a read. A retirement that nobody can account for later is
	// the same problem as one that never happened.
	h.log.Info("role active_flag changed",
		zap.String("correlation_id", correlationID),
		zap.String("role_id", role.RoleID),
		zap.String("role_code", role.RoleCode),
		zap.String("tenant_id", role.TenantID),
		zap.Bool("active_flag", role.ActiveFlag))

	writeJSON(w, http.StatusOK, role)
}

// ── POST /v1/admin/roles/{role_id}/permission-bundles ───────────────────────

type createBundleRequest struct {
	BundleCode       string   `json:"bundle_code"`
	PermittedActions []string `json:"permitted_actions"`
	// ExpectedVersion is optional: when set, replacing an existing bundle
	// succeeds only if it is still at that version (409 otherwise), so a
	// concurrent edit is not silently overwritten.
	ExpectedVersion int64 `json:"expected_version,omitempty"`
}

// CreatePermissionBundle handles POST /v1/admin/roles/{role_id}/permission-bundles.
//
// Response: 201 created (or updated in place, same code) / 400 missing field / 403 unauthorized / 404 role not found / 503 unavailable.
func (h *Handler) CreatePermissionBundle(w http.ResponseWriter, r *http.Request) {
	roleID := chi.URLParam(r, "role_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.permission_bundle.manage to create permission bundles
	if !h.requirePermission(w, r, principalID, tenantScope, "iam.permission_bundle.manage") {
		return
	}

	var req createBundleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if req.BundleCode == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "bundle_code"})
		return
	}
	if len(req.PermittedActions) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "permitted_actions"})
		return
	}

	// The role's OWN tenant decides scope here, not anything the body
	// supplies (there is no tenant_id in this request at all) — before
	// this fix, any caller could grant a permission bundle onto any
	// tenant's role just by naming its role_id, since nothing checked
	// which tenant owns it. 404 rather than 403 so a probe against
	// another tenant's role_id cannot confirm it exists.
	role, err := h.store.FindRoleByID(r.Context(), roleID)
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": roleID})
			return
		}
		h.log.Error("CreatePermissionBundle: role lookup failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if role.TenantID != tenantScope {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": roleID})
		return
	}
	if h.refuseOwnRole(w, r, principalID, roleID, tenantScope) {
		return
	}
	// ZS-IAM-001 §9: a tenant custom role "cannot include protected
	// platform-admin permissions". Such a bundle is authored only with the
	// platform-scope grant, never a tenant administrator's.
	if protected := anyProtectedPlatform(req.PermittedActions); len(protected) > 0 &&
		!h.requirePlatformAction(w, r, principalID, "iam.permission_bundle.manage") {
		return
	}
	if h.refuseToxicForHolders(w, r, roleID, tenantScope, req.PermittedActions) {
		return
	}

	bundle, created, err := h.store.CreatePermissionBundle(r.Context(), domain.CreatePermissionBundleParams{
		RoleID: roleID, BundleCode: req.BundleCode, PermittedActions: req.PermittedActions,
		ExpectedVersion: req.ExpectedVersion,
	})
	if err != nil {
		if errors.Is(err, domain.ErrVersionConflict) {
			writeVersionConflict(w)
			return
		}
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": roleID})
			return
		}
		h.log.Error("CreatePermissionBundle: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// 201 for a new bundle, 200 for one whose action set was REPLACED — the
	// distinction the response used to hide. The upsert overwrites
	// permitted_actions wholesale, so a repost against an existing
	// bundle_code can silently narrow or empty what a role grants; answering
	// 201 to that says "created" about a destructive edit. Logged at Info on
	// the replace path with the action count, because it is a change to what
	// a role permits and the previous set is gone from the row.
	//
	// A 409 was considered and rejected: callers re-provision bundles from a
	// declarative catalogue, so refusing a repeat would break replay. Same
	// created/replayed shape CreateRole already answers with.
	if !created {
		h.log.Info("permission bundle replaced",
			zap.String("permission_bundle_id", bundle.PermissionBundleID),
			zap.String("role_id", bundle.RoleID),
			zap.String("bundle_code", bundle.BundleCode),
			zap.Int("permitted_action_count", len(bundle.PermittedActions)),
			zap.String("correlation_id", correlationID),
		)
		writeJSON(w, http.StatusOK, bundle)
		return
	}
	writeJSON(w, http.StatusCreated, bundle)
}

// ── GET /v1/admin/roles/{role_id}/permission-bundles ────────────────────────

// ListPermissionBundles handles GET /v1/admin/roles/{role_id}/permission-bundles
// — read what a role actually permits.
//
// The read that was missing from the object that IS the permission. A role
// could be created, given bundles, retired and reactivated, and nothing could
// list what it granted: ListRoles returns role_code, role_name,
// role_scope_type and active_flag and no actions at all, and the table's only
// other reader is FindGrantedActions, which answers the different question
// "what may THIS principal do here". The console therefore listed role
// LABELS — and a role_code is a name an operator chose, while the bundle is
// the control.
//
// It also made POST to this same path unsafe: the upsert on
// (role_id, bundle_code) replaces permitted_actions wholesale, and with no
// read a code could not be checked for collision beforehand.
//
// Tenant-scoped from the VERIFIED header, and the role's ownership is checked
// in the store rather than trusted: naming another tenant's role_id returns an
// empty list, not its bundles. 200-with-[] rather than 404 for a role that
// does not exist in this scope — the same posture the sibling catalogue reads
// take, and it avoids turning this into an existence oracle for role ids.
//
// Retired bundles are included and ordered last, for the reason ListRoles
// keeps retired roles: a retired bundle is why access someone used to have is
// gone, and hiding it makes that unexplainable.
//
// Response: 200 the bundles (possibly empty) / 401 missing principal or tenant
// scope / 503 unavailable.
func (h *Handler) ListPermissionBundles(w http.ResponseWriter, r *http.Request) {
	roleID := chi.URLParam(r, "role_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// ZS-IAM-001 §21 "no broad IAM discovery beyond administrable scope":
	// the register is readable with the Appendix A read permission.
	if !h.requirePermission(w, r, callerPrincipal, tenantScope, "iam.role.read") {
		return
	}

	bundles, err := h.store.ListPermissionBundles(r.Context(), roleID, tenantScope)
	if err != nil {
		h.log.Error("ListPermissionBundles: store unavailable",
			zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if bundles == nil {
		bundles = []domain.PermissionBundle{}
	}
	writeJSON(w, http.StatusOK, bundles)
}

// ── POST /v1/admin/permission-bundles/{permission_bundle_id}/retire|reactivate ──

// RetirePermissionBundle handles
// POST /v1/admin/permission-bundles/{permission_bundle_id}/retire — withdraw
// one bundle's actions from its role.
//
// The off switch this object never had a route for. `pb.active_flag` is in the
// JOIN of BOTH evaluation reads — FindGrantedActions and FindDelegatedActions
// — so a false flag genuinely removes every action the bundle granted, from
// the next decision, including through delegations of the role. Nothing in the
// service could set it. That is the same defect SetSoDRuleActive was added to
// fix, on the granting side rather than the denying side.
//
// Why retiring the ROLE is not a substitute: it withdraws every bundle the
// role holds at once and suspends the role for every principal assigned it.
// The other workaround — reposting the bundle with a shorter action list —
// destroys the record of what was withdrawn, since the upsert overwrites
// permitted_actions. Neither can take back one bundle and leave the rest.
//
// Deliberately does NOT delete, and does not touch assignments. The bundle
// stays readable because a grant recorded as `rbac:role=<code>` is only
// explainable while the actions that role held can still be read; and
// reactivating restores exactly the access that was suspended.
//
// Response: 200 retired / 401 missing principal or tenant scope / 404 not
// found in this tenant / 503 unavailable.
func (h *Handler) RetirePermissionBundle(w http.ResponseWriter, r *http.Request) {
	h.setPermissionBundleActive(w, r, false)
}

// ReactivatePermissionBundle handles
// POST /v1/admin/permission-bundles/{permission_bundle_id}/reactivate.
// Restores exactly the actions the retirement withdrew. Response shape matches
// RetirePermissionBundle.
func (h *Handler) ReactivatePermissionBundle(w http.ResponseWriter, r *http.Request) {
	h.setPermissionBundleActive(w, r, true)
}

// permissionBundleFinder is the store capability behind the own-role check on
// bundle reactivation. Optional, like assignmentEndScheduler: a store without
// it answers 503 rather than reactivating unchecked.
type permissionBundleFinder interface {
	FindPermissionBundleByID(ctx context.Context, permissionBundleID, tenantID string) (*domain.PermissionBundle, error)
}

func (h *Handler) setPermissionBundleActive(w http.ResponseWriter, r *http.Request, active bool) {
	bundleID := chi.URLParam(r, "permission_bundle_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.permission_bundle.manage to retire/reactivate permission bundles
	if !h.requirePermission(w, r, principalID, tenantScope, "iam.permission_bundle.manage") {
		return
	}

	// Reactivating restores what a role grants, so it is refused to a holder
	// of that role. The bundle's role is only known by reading it first.
	if active {
		finder, ok := h.store.(permissionBundleFinder)
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		existing, err := finder.FindPermissionBundleByID(r.Context(), bundleID, tenantScope)
		if err != nil {
			if errors.Is(err, domain.ErrPermissionBundleNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "permission_bundle_not_found"})
				return
			}
			h.log.Error("setPermissionBundleActive: bundle lookup failed",
				zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if h.refuseOwnRole(w, r, principalID, existing.RoleID, tenantScope) {
			return
		}
		if h.refuseToxicForHolders(w, r, existing.RoleID, tenantScope, existing.PermittedActions) {
			return
		}
	}

	expectedVersion, ok := h.readCommand(w, &r)
	if !ok {
		return
	}
	bundle, err := h.store.SetPermissionBundleActive(r.Context(), bundleID, tenantScope, active, expectedVersion)
	if err != nil {
		if errors.Is(err, domain.ErrVersionConflict) {
			writeVersionConflict(w)
			return
		}
		if errors.Is(err, domain.ErrPermissionBundleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "permission_bundle_not_found"})
			return
		}
		h.log.Error("setPermissionBundleActive: store unavailable",
			zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Logged at Info with the action count, for the reason the SoD flip is:
	// changing what a role grants is a governance event in its own right, and
	// the count is what makes the entry mean anything when read back later.
	h.log.Info("permission bundle active flag set",
		zap.String("permission_bundle_id", bundle.PermissionBundleID),
		zap.String("role_id", bundle.RoleID),
		zap.String("bundle_code", bundle.BundleCode),
		zap.Int("permitted_action_count", len(bundle.PermittedActions)),
		zap.Bool("active", bundle.ActiveFlag),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, bundle)
}

// ── POST /v1/admin/role-assignments ──────────────────────────────────────────

type createAssignmentRequest struct {
	PrincipalRoleAssignmentID string `json:"principal_role_assignment_id,omitempty"`
	PrincipalID               string `json:"principal_id"`
	RoleID                    string `json:"role_id"`
	// LegalEntityID is optional: omit it for a tenant-wide assignment
	// (only accepted if the role's scope_type is TENANT — see
	// domain.ErrLegalEntityRequiredForRoleScope).
	LegalEntityID string    `json:"legal_entity_id,omitempty"`
	BookID        string    `json:"book_id,omitempty"`
	OrgUnitID     string    `json:"org_unit_id,omitempty"`
	EffectiveFrom time.Time `json:"effective_from"`
	// EffectiveTo is optional; omit it for an open-ended assignment. When set
	// it must be after effective_from and in the future.
	EffectiveTo *time.Time `json:"effective_to,omitempty"`
	// ApprovalReference names the governed request a privileged grant was
	// approved under (access-control-svc's request id), recorded as evidence.
	ApprovalReference string `json:"approval_reference,omitempty"`
}

func (req createAssignmentRequest) missingField() string {
	switch {
	case req.PrincipalID == "":
		return "principal_id"
	case req.RoleID == "":
		return "role_id"
	case req.EffectiveFrom.IsZero():
		return "effective_from"
	default:
		return ""
	}
}

// CreateRoleAssignment handles POST /v1/admin/role-assignments.
//
// Response: 201 created / 400 missing field / 403 unauthorized / 404 role not found / 503 unavailable.
func (h *Handler) CreateRoleAssignment(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.assignment.grant to assign roles
	if !h.requirePermission(w, r, principalID, tenantScope, "iam.assignment.grant") {
		return
	}

	var req createAssignmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	if req.EffectiveTo != nil && (!req.EffectiveTo.After(req.EffectiveFrom) || !req.EffectiveTo.After(time.Now())) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_effective_to",
			"message": "effective_to must be after effective_from and in the future",
		})
		return
	}

	// Same doctrine as CreatePermissionBundle: the role being assigned
	// decides the tenant, and it must be the caller's own — before this
	// fix, any caller could hand out a role from any tenant to any
	// principal, in any legal entity, just by naming role_id.
	role, err := h.store.FindRoleByID(r.Context(), req.RoleID)
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": req.RoleID})
			return
		}
		h.log.Error("CreateRoleAssignment: role lookup failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if role.TenantID != tenantScope {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": req.RoleID})
		return
	}

	// Prevent self-grant: a principal cannot assign a role to themselves.
	// Per ZS-IAM-001 §10.1 "Own access elevation" and §10.2 "Own access elevation".
	if req.PrincipalID == principalID {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "self_grant_not_allowed",
			"message": "a principal cannot assign a role to themselves",
		})
		return
	}

	// An assignment on the platform-scope entity confers platform authority —
	// requirePlatformAction reads it across tenants — so making one is a
	// platform act: tenant-scope iam.assignment.grant is not enough. Without
	// this, a tenant administrator could bind a role of their own tenant to the
	// platform entity and its holder could author SoD and ABAC rules for every
	// tenant.
	if h.isPlatformScopeEntity(req.LegalEntityID) &&
		!h.requirePlatformAction(w, r, principalID, "iam.assignment.grant") {
		return
	}

	// Static SoD at grant time (ZS-IAM-001 §10: "deny an assignment"). A
	// conflicting pair used to be assignable and was caught only when the
	// action was evaluated — by which point the toxic combination was already
	// held. Refused here when the role's actions conflict with what the
	// assignee holds where the assignment applies, or with each other.
	if conflicts, ok := h.assignmentSoDConflicts(w, r, req, role.RoleID, tenantScope); !ok {
		return
	} else if len(conflicts) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":     "sod_conflict",
			"message":   "the role's actions conflict with a segregation-of-duties rule for this principal",
			"conflicts": conflicts,
		})
		return
	}

	var legalEntityID *string
	if req.LegalEntityID != "" {
		legalEntityID = &req.LegalEntityID
	}
	var bookID *string
	if req.BookID != "" {
		bookID = &req.BookID
	}
	var orgUnitID *string
	if req.OrgUnitID != "" {
		orgUnitID = &req.OrgUnitID
	}

	// Maker-checker on privileged grants (ZS-IAM-001 §9 / A20, GOV-12). A
	// role carrying access or platform administration, or any assignment on
	// the platform-scope entity, takes effect only with an independent
	// approver: the caller, if they hold iam.assignment.approve_privileged
	// (how access-control-svc provisions what its security approver cleared),
	// otherwise a second principal through /approve.
	params := domain.CreateRoleAssignmentParams{
		// assigned_by is always the verified caller, never the request body.
		PrincipalRoleAssignmentID: req.PrincipalRoleAssignmentID, PrincipalID: req.PrincipalID, RoleID: req.RoleID,
		LegalEntityID: legalEntityID, BookID: bookID, OrgUnitID: orgUnitID, EffectiveFrom: req.EffectiveFrom, EffectiveTo: req.EffectiveTo,
		AssignedBy: principalID,
	}
	if req.ApprovalReference != "" {
		params.ApprovalReference = &req.ApprovalReference
	}
	roleActions, err := h.roleActiveActions(r.Context(), role.RoleID, tenantScope)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if anyPrivileged(roleActions) || h.isPlatformScopeEntity(req.LegalEntityID) {
		approver, err := h.holdsPrivilegedApproval(r, principalID, tenantScope, req.LegalEntityID)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if approver {
			params.ApprovalStatus = domain.ApprovalApproved
			params.ApprovedBy = &principalID
		} else {
			expires := time.Now().UTC().Add(PendingApprovalWindow)
			params.ApprovalStatus = domain.ApprovalPending
			params.ApprovalExpiresAt = &expires
		}
	}

	assignment, err := h.store.CreateRoleAssignment(r.Context(), params)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRoleNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_not_found", "role_id": req.RoleID})
		case errors.Is(err, domain.ErrLegalEntityRequiredForRoleScope):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "legal_entity_id_required", "message": err.Error()})
		default:
			h.log.Error("CreateRoleAssignment: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	if params.ApprovalStatus == domain.ApprovalPending {
		// 202: recorded, not yet in force — it grants nothing until approved.
		writeJSON(w, http.StatusAccepted, assignment)
		return
	}
	writeJSON(w, http.StatusCreated, assignment)
}

// assignmentSoDConflicts returns the static SoD conflicts assigning roleID to
// req.PrincipalID would create, writing a 503 and reporting false if that
// cannot be established.
//
// Held is read where the assignment applies: at its legal entity (what
// /v1/authorize would evaluate there, RBAC and delegated), or — for a
// tenant-wide assignment, which applies in every entity — everything the
// principal holds anywhere in the tenant. Candidates are the actions of the
// role's active bundles.
func (h *Handler) assignmentSoDConflicts(w http.ResponseWriter, r *http.Request, req createAssignmentRequest, roleID, tenantScope string) ([]sodConflict, bool) {
	fail := func(err error) ([]sodConflict, bool) {
		h.log.Error("CreateRoleAssignment: SoD check failed — refusing",
			zap.String("correlation_id", r.Header.Get("X-Correlation-ID")), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return nil, false
	}

	bundles, err := h.store.ListPermissionBundles(r.Context(), roleID, tenantScope)
	if err != nil {
		return fail(err)
	}
	var candidates []string
	for _, b := range bundles {
		if b.ActiveFlag {
			candidates = append(candidates, b.PermittedActions...)
		}
	}
	candidates = dedupeSorted(candidates)
	if len(candidates) == 0 {
		return nil, true
	}

	var held []string
	if req.LegalEntityID == "" {
		all, err := h.heldActionsInTenant(r, req.PrincipalID, tenantScope)
		if err != nil {
			return fail(err)
		}
		for a := range all {
			held = append(held, a)
		}
	} else {
		entity, tenant := req.LegalEntityID, tenantScope
		if h.isPlatformScopeEntity(entity) {
			entity, tenant = h.platformScopeEntityID, ""
		}
		rbac, _, err := h.store.FindGrantedActions(r.Context(), req.PrincipalID, entity, tenant)
		if err != nil {
			return fail(err)
		}
		delegated, _, err := h.store.FindDelegatedActions(r.Context(), req.PrincipalID, entity, tenant)
		if err != nil {
			return fail(err)
		}
		held = append(rbac, delegated...)
	}

	conflicts, err := h.sodConflictsFor(r.Context(), dedupeSorted(held), candidates, tenantScope)
	if err != nil {
		return fail(err)
	}
	return conflicts, true
}

func writeVersionConflict(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":   "version_conflict",
		"message": "the object has changed since expected_version; re-read it and retry",
	})
}

// roleAssignmentFinder is the store capability behind the platform-scope check
// on revoke. Optional; a store without it answers 503 rather than revoking
// unchecked.
type roleAssignmentFinder interface {
	FindRoleAssignmentByID(ctx context.Context, assignmentID, tenantID string) (*domain.PrincipalRoleAssignment, error)
}

// isPlatformScopeEntity reports whether id names the platform-scope entity.
// Parsed, not string-compared: legal_entity_id is a UUID column, so "…F001",
// "{…f001}" and the unhyphenated form all store as the platform-scope id.
func (h *Handler) isPlatformScopeEntity(id string) bool {
	if id == "" || h.platformScopeEntityID == "" {
		return false
	}
	if strings.EqualFold(id, h.platformScopeEntityID) {
		return true
	}
	got, gotErr := uuid.Parse(id)
	want, wantErr := uuid.Parse(h.platformScopeEntityID)
	return gotErr == nil && wantErr == nil && got == want
}

// assignmentEndScheduler is the store capability behind an effective-dated
// revoke. Optional so a store without it answers 503 rather than revoking at
// once, which would end access earlier than the caller asked.
type assignmentEndScheduler interface {
	ScheduleRoleAssignmentEnd(ctx context.Context, assignmentID, tenantID string, at time.Time) (*domain.PrincipalRoleAssignment, error)
}

// RevokeRoleAssignment handles POST /v1/admin/role-assignments/{assignment_id}/revoke.
//
// An empty body (or {}) revokes now. {"effective_to": "<RFC 3339>"} in the
// future schedules the end instead (Authorization Standard §9 "Revocation:
// immediate or effective-dated removal"); a schedule only ever brings an end
// earlier, never later.
//
// Response: 200 revoked or scheduled / 400 invalid effective_to / 403 unauthorized /
// 404 not found or already ended / 503 unavailable.
func (h *Handler) RevokeRoleAssignment(w http.ResponseWriter, r *http.Request) {
	assignmentID := chi.URLParam(r, "assignment_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.assignment.revoke to revoke role assignments
	if !h.requirePermission(w, r, principalID, tenantScope, "iam.assignment.revoke") {
		return
	}

	var body struct {
		EffectiveTo *time.Time `json:"effective_to"`
		commandFields
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
			return
		}
	}
	if !h.applyReason(w, &r, body.reason()) {
		return
	}

	// Ending a platform-scope assignment withdraws platform authority, so it
	// is a platform act too — otherwise any tenant administrator could strip
	// the platform's own rule authors.
	if h.platformScopeEntityID != "" {
		finder, ok := h.store.(roleAssignmentFinder)
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		existing, err := finder.FindRoleAssignmentByID(r.Context(), assignmentID, tenantScope)
		if err != nil {
			if errors.Is(err, domain.ErrRoleAssignmentNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_assignment_not_found"})
				return
			}
			h.log.Error("RevokeRoleAssignment: assignment lookup failed", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if existing.LegalEntityID != nil && h.isPlatformScopeEntity(*existing.LegalEntityID) &&
			!h.requirePlatformAction(w, r, principalID, "iam.assignment.revoke") {
			return
		}
	}

	if body.EffectiveTo != nil {
		if !body.EffectiveTo.After(time.Now()) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_effective_to",
				"message": "effective_to must be in the future; omit it to revoke now",
			})
			return
		}
		scheduler, ok := h.store.(assignmentEndScheduler)
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		assignment, err := scheduler.ScheduleRoleAssignmentEnd(r.Context(), assignmentID, tenantScope, body.EffectiveTo.UTC())
		if err != nil {
			if errors.Is(err, domain.ErrRoleAssignmentNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_assignment_not_found"})
				return
			}
			h.log.Error("RevokeRoleAssignment: schedule failed", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, assignment)
		return
	}

	// The store's own query now carries the tenant predicate through the
	// assignment's role, so a cross-tenant revoke attempt reports
	// role_assignment_not_found rather than revoking (or even confirming
	// the existence of) another tenant's assignment.
	assignment, err := h.store.RevokeRoleAssignment(r.Context(), assignmentID, tenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrRoleAssignmentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_assignment_not_found"})
			return
		}
		h.log.Error("RevokeRoleAssignment: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, assignment)
}

// ── POST /v1/admin/delegated-authorities ─────────────────────────────────────

type createDelegationRequest struct {
	DelegatedAuthorityID string `json:"delegated_authority_id,omitempty"`
	DelegatorPrincipalID string `json:"delegator_principal_id"`
	DelegatePrincipalID  string `json:"delegate_principal_id"`
	ScopeType            string `json:"scope_type"`
	// LegalEntityID is optional: omit it for a delegation that applies
	// across the whole tenant rather than one entity.
	LegalEntityID       string  `json:"legal_entity_id,omitempty"`
	BookID              string  `json:"book_id,omitempty"`
	OrgUnitID           string  `json:"org_unit_id,omitempty"`
	AuthorityLimitType  *string `json:"authority_limit_type,omitempty"`
	AuthorityLimitValue *string `json:"authority_limit_value,omitempty"`
	// DelegatedActions is the subset of the delegator's authority to confer.
	// Omit it — or send an empty array — for the delegator's FULL authority,
	// which is what every delegation created before this field existed means.
	//
	// Required when ScopeType is ACTION_SUBSET: that value has always been
	// accepted and, until migration 000008, nothing ever read it, so a
	// delegation recorded as a subset conferred the delegator's entire grant
	// set. Accepting ACTION_SUBSET with no subset would recreate exactly that
	// — a row that reads as restricted in the register and is not — so it is
	// refused. See missingField.
	DelegatedActions []string   `json:"delegated_actions,omitempty"`
	EffectiveFrom    time.Time  `json:"effective_from"`
	EffectiveTo      *time.Time `json:"effective_to,omitempty"`

	// ZS-IAM-001 §11: the purpose (absence, named operational cover...) and,
	// where policy needs one, the independent approval it was granted under.
	Reason            string `json:"reason,omitempty"`
	ApprovalReference string `json:"approval_reference,omitempty"`
}

// ScopeTypeActionSubset is the scope_type value that declares a delegation to
// confer only part of the delegator's authority. Data, like every other
// scope_type value — but this one the evaluation reads, so the handler checks
// that a delegation claiming it actually names its subset.
const ScopeTypeActionSubset = "ACTION_SUBSET"

func (req createDelegationRequest) missingField() string {
	switch {
	case req.DelegatorPrincipalID == "":
		return "delegator_principal_id"
	case req.DelegatePrincipalID == "":
		return "delegate_principal_id"
	case req.ScopeType == "":
		return "scope_type"
	case req.ScopeType == ScopeTypeActionSubset && len(req.DelegatedActions) == 0:
		return "delegated_actions"
	case req.EffectiveFrom.IsZero():
		return "effective_from"
	default:
		return ""
	}
}

// CreateDelegatedAuthority handles POST /v1/admin/delegated-authorities.
//
// delegated_actions names the subset of the delegator's authority to confer;
// omitting it confers all of it. A scope_type of ACTION_SUBSET without one is
// refused rather than quietly stored as full authority — that combination is
// exactly what shipped before migration 000008 and it read as restricted in
// the register while conferring everything.
//
// The subset is a CEILING, not a grant: it is intersected with the delegator's
// live grants at evaluation time, so naming an action the delegator does not
// hold confers nothing. That is checked in the query rather than here, because
// what the delegator holds can change after the delegation is written.
//
// Response: 201 created / 400 missing field / 401 missing principal or tenant
// scope / 403 delegator is not the caller / 403 unauthorized / 503 unavailable.
func (h *Handler) CreateDelegatedAuthority(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.delegation.grant to create delegations
	if !h.requirePermission(w, r, principalID, tenantScope, "iam.delegation.grant") {
		return
	}

	var req createDelegationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	// delegated_authorities carries no tenant_id at all (only
	// legal_entity_id — see 000001's schema), so the one check this
	// service CAN make without inventing a column is the one that
	// matters most: this table had no ownership check whatsoever, so any
	// caller could delegate ANY principal's authority to ANY other
	// principal, purely by naming them in the body. A principal may only
	// give away authority that is theirs to give — the same doctrine
	// already enforced in delegated-authority-svc (a separate service
	// with its own copy of this concept — see docs/architecture/
	// full-architecture-gap-analysis.md item 12 on the duplication).
	if req.DelegatorPrincipalID != principalID {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "delegator_must_be_caller",
			"message": "a principal may only delegate authority that is their own",
		})
		return
	}
	// Prevent self-delegation: a principal cannot delegate to themselves.
	// Per ZS-IAM-001 §11 constraints and §10.2 "Own access elevation".
	if req.DelegatePrincipalID == principalID {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "self_delegation_not_allowed",
			"message": "a principal cannot delegate authority to themselves",
		})
		return
	}
	// §11 "protected privileges normally non-delegable": access and platform
	// administration cannot be named in a delegation (and never flow through
	// one of full authority — FindDelegatedActionsScoped filters them).
	for _, a := range req.DelegatedActions {
		if domain.IsPrivilegedAction(a) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "protected_privilege_not_delegable",
				"field":   "delegated_actions",
				"message": a + " is a protected privilege and cannot be delegated",
			})
			return
		}
	}
	// §11 "effective_from / effective_to: mandatory finite period".
	if req.EffectiveTo != nil && !req.EffectiveTo.After(req.EffectiveFrom) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_effective_to", "message": "effective_to must be after effective_from"})
		return
	}
	if req.EffectiveTo == nil {
		if h.commandContract == CommandContractEnforce {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "effective_to",
				"message": "a delegation must have a finite period (ZS-IAM-001 §11)"})
			return
		}
		end := req.EffectiveFrom.Add(DefaultDelegationTerm)
		req.EffectiveTo = &end
		w.Header().Set(HeaderCommandContract, "violated")
	}
	// §11 reason: required; in warn mode admitted and marked.
	if !h.applyReason(w, &r, req.Reason) {
		return
	}

	var legalEntityID *string
	if req.LegalEntityID != "" {
		legalEntityID = &req.LegalEntityID
	}
	var bookID *string
	if req.BookID != "" {
		bookID = &req.BookID
	}
	var orgUnitID *string
	if req.OrgUnitID != "" {
		orgUnitID = &req.OrgUnitID
	}

	d, err := h.store.CreateDelegatedAuthority(r.Context(), domain.CreateDelegatedAuthorityParams{
		TenantID:             tenantScope,
		DelegatedAuthorityID: req.DelegatedAuthorityID, DelegatorPrincipalID: req.DelegatorPrincipalID,
		DelegatePrincipalID: req.DelegatePrincipalID, ScopeType: req.ScopeType, LegalEntityID: legalEntityID,
		BookID: bookID, OrgUnitID: orgUnitID,
		AuthorityLimitType: req.AuthorityLimitType, AuthorityLimitValue: req.AuthorityLimitValue,
		DelegatedActions: req.DelegatedActions,
		EffectiveFrom:    req.EffectiveFrom, EffectiveTo: req.EffectiveTo,
		Reason: optionalString(req.Reason), ApprovalReference: optionalString(req.ApprovalReference),
	})
	if err != nil {
		h.log.Error("CreateDelegatedAuthority: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// RevokeDelegatedAuthority handles POST /v1/admin/delegated-authorities/{delegation_id}/revoke.
//
// Response: 200 revoked / 401 no scope / 403 unauthorized / 404 not found / 409 already revoked /
// 503 unavailable.
//
// The lookup below is tenant-scoped, so another tenant's delegation is a 404
// before the delegator check is ever reached — "not yours" and "does not
// exist" are deliberately indistinguishable to a prober.
func (h *Handler) RevokeDelegatedAuthority(w http.ResponseWriter, r *http.Request) {
	delegationID := chi.URLParam(r, "delegation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	revokeTenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// Require iam.delegation.revoke to revoke delegations
	if !h.requirePermission(w, r, principalID, revokeTenantScope, "iam.delegation.revoke") {
		return
	}

	// Fetched and checked BEFORE revoking, same doctrine as
	// secret-vault-integration-svc's RevokeLease: only the delegator may
	// take back authority they gave away — this had no check of any kind
	// before, so any caller could revoke any principal's delegation.
	existing, err := h.store.FindDelegatedAuthorityByID(r.Context(), delegationID, revokeTenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrDelegatedAuthorityNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "delegated_authority_not_found"})
			return
		}
		h.log.Error("RevokeDelegatedAuthority: lookup failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if existing.DelegatorPrincipalID != principalID {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "only_delegator_may_revoke",
			"message": "only the principal who granted this delegation may revoke it",
		})
		return
	}
	if _, ok := h.readCommand(w, &r); !ok {
		return
	}

	d, err := h.store.RevokeDelegatedAuthority(r.Context(), delegationID, revokeTenantScope)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrDelegatedAuthorityNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "delegated_authority_not_found"})
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "already_revoked"})
		default:
			h.log.Error("RevokeDelegatedAuthority: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ── POST /v1/admin/sod-rules ──────────────────────────────────────────────────

// ActionSoDRuleManageGlobal is the action a principal must hold, against the
// platform-scope legal entity, to author a segregation-of-duties rule that
// applies to every tenant. Deliberately not the same grant that authors a
// rule for one tenant: those are different blast radii.
const ActionSoDRuleManageGlobal = "SOD_RULE_MANAGE_GLOBAL"

type createSoDRuleRequest struct {
	DomainCode     string  `json:"domain_code"`
	ActionA        string  `json:"action_a"`
	ActionB        string  `json:"action_b"`
	ConflictType   string  `json:"conflict_type"`
	JurisdictionID *string `json:"jurisdiction_id,omitempty"`
	// TenantID is optional — omit for a rule that applies across every
	// tenant, matching JurisdictionID's own global-when-nil convention.
	TenantID *string `json:"tenant_id,omitempty"`
}

func (req createSoDRuleRequest) missingField() string {
	switch {
	case req.DomainCode == "":
		return "domain_code"
	case req.ActionA == "":
		return "action_a"
	case req.ActionB == "":
		return "action_b"
	case req.ConflictType == "":
		return "conflict_type"
	default:
		return ""
	}
}

// CreateSoDRule handles POST /v1/admin/sod-rules. If jurisdiction_id is
// supplied it's validated synchronously against jurisdiction-rules-svc,
// fail-closed.
//
// A rule with no tenant_id applies to EVERY tenant, and creating one
// therefore requires the platform-scope grant named by
// ActionSoDRuleManageGlobal — not the tenant scope that authorizes a rule for
// the caller's own tenant.
//
// Response: 201 created / 400 missing field / 401 missing principal or tenant
// scope / 403 not authorized for a platform-wide rule / 403 unauthorized / 404 jurisdiction not
// found / 503 unavailable.
func (h *Handler) CreateSoDRule(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req createSoDRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	// A tenant scope is required to reach this route AT ALL, whether or not
	// the body names one. Requiring it only when tenant_id was present made
	// the omission the cheap path: with no tenant_id, one request carrying
	// nothing but a principal header stored a rule with tenant_id = NULL,
	// which CheckSoDConflict matches for every tenant. That is a platform-wide
	// denial of service delivered through the authorization engine — every
	// principal anywhere holding both actions gets DENIED on their next
	// /v1/authorize — and it crossed no tenant boundary because none was asked
	// for.
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.sod_rule.manage for tenant-scoped SoD rules
	if req.TenantID != nil && *req.TenantID != "" {
		if h.refuseForeignTenant(w, *req.TenantID, tenantScope) {
			return
		}
		if !h.requirePermission(w, r, principalID, tenantScope, "iam.sod_rule.manage") {
			return
		}
	} else {
		// nil tenant_id still means "applies to every tenant" — that is a
		// deliberate and useful thing to be able to say. It is now a distinct
		// grant to say it, rather than a side effect of leaving a field out.
		if !h.requirePlatformAction(w, r, principalID, ActionSoDRuleManageGlobal) {
			return
		}
	}

	if req.JurisdictionID != nil && *req.JurisdictionID != "" {
		if err := h.jurisdictionValidator.ValidateExists(r.Context(), *req.JurisdictionID); err != nil {
			switch {
			case errors.Is(err, domain.ErrJurisdictionNotFound):
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "jurisdiction_not_found", "jurisdiction_id": *req.JurisdictionID})
			default:
				h.log.Error("CreateSoDRule: jurisdiction validation failed", zap.String("correlation_id", correlationID), zap.Error(err))
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "jurisdiction_service_unavailable"})
			}
			return
		}
	}

	rule, err := h.store.CreateSoDRule(r.Context(), domain.CreateSoDRuleParams{
		DomainCode: req.DomainCode, ActionA: req.ActionA, ActionB: req.ActionB,
		ConflictType: req.ConflictType, JurisdictionID: req.JurisdictionID,
		TenantID: req.TenantID,
	})
	if err != nil {
		h.log.Error("CreateSoDRule: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

// ── POST /v1/admin/sod-rules/{sod_rule_id}/retire|reactivate ────────────────

// RetireSoDRule handles POST /v1/admin/sod-rules/{sod_rule_id}/retire — stop a
// conflict rule denying.
//
// The lifecycle this object never had. active_flag is in CheckSoDConflict's
// predicate, so retiring genuinely stops the rule denying on the next
// evaluation; until this route existed nothing could set it, and a conflict
// rule authored by mistake denied its action to every principal holding the
// pair with no way to undo it through the API.
//
// Deliberately does NOT delete. The rule has to stay resolvable for the
// decisions it already caused — a denial recorded with
// `sod:conflict_with=<action>` is only explainable while the rule that caused
// it can still be read.
//
// A PLATFORM-WIDE rule answers 404 here rather than being retired from one
// tenant's console, exactly as setABACRuleActive does: a rule binding every
// tenant must not be disableable by any one of them.
//
// Response: 200 retired / 401 missing principal or tenant scope / 404 not
// found or platform-wide / 503 unavailable.
func (h *Handler) RetireSoDRule(w http.ResponseWriter, r *http.Request) {
	h.setSoDRuleActive(w, r, false)
}

// ReactivateSoDRule handles POST /v1/admin/sod-rules/{sod_rule_id}/reactivate.
// Restores exactly the denials the retirement suspended. Response shape matches
// RetireSoDRule.
func (h *Handler) ReactivateSoDRule(w http.ResponseWriter, r *http.Request) {
	h.setSoDRuleActive(w, r, true)
}

// refuseInterestedSoDRetire reports whether principalID holds either action of
// the tenant's SoD rule sodRuleID, writing a 403 if so (or a 503 if that cannot
// be established). A rule not visible to the tenant is left to
// SetSoDRuleActive, which answers 404 for it.
func (h *Handler) refuseInterestedSoDRetire(w http.ResponseWriter, r *http.Request, principalID, sodRuleID, tenantID string) bool {
	rules, err := h.store.ListSoDRules(r.Context(), tenantID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return true
	}
	var rule *domain.SoDRule
	for i := range rules {
		if rules[i].SoDRuleID == sodRuleID {
			rule = &rules[i]
			break
		}
	}
	if rule == nil {
		return false
	}
	held, err := h.heldActionsInTenant(r, principalID, tenantID)
	if err != nil {
		h.log.Error("sod retire: held-action lookup failed — refusing",
			zap.String("correlation_id", r.Header.Get("X-Correlation-ID")), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return true
	}
	if held[rule.ActionA] || held[rule.ActionB] {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "sod_self_interest",
			"message": "a principal holding an action this rule constrains cannot retire it",
		})
		return true
	}
	return false
}

// heldActionsInTenant is every action principalID holds, or will hold, through
// any role assignment in tenantID that has not ended — across all legal
// entities, books and org units, since a rule constrains the principal
// wherever they hold the action. Retired roles and bundles still count:
// reactivation is one call away, and refuseOwnRole only stops this principal
// making that call, not a colleague.
func (h *Handler) heldActionsInTenant(r *http.Request, principalID, tenantID string) (map[string]bool, error) {
	assignments, err := h.store.ListRoleAssignments(r.Context(), tenantID, principalID, "", false)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	held := map[string]bool{}
	seenRole := map[string]bool{}
	for _, a := range assignments {
		if (a.EffectiveTo != nil && !a.EffectiveTo.After(now)) || seenRole[a.RoleID] {
			continue
		}
		seenRole[a.RoleID] = true
		bundles, err := h.store.ListPermissionBundles(r.Context(), a.RoleID, tenantID)
		if err != nil {
			return nil, err
		}
		for _, b := range bundles {
			for _, act := range b.PermittedActions {
				held[act] = true
			}
		}
	}
	return held, nil
}

func (h *Handler) setSoDRuleActive(w http.ResponseWriter, r *http.Request, active bool) {
	sodRuleID := chi.URLParam(r, "sod_rule_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.sod_rule.manage to retire/reactivate SoD rules
	if !h.requirePermission(w, r, principalID, tenantScope, "iam.sod_rule.manage") {
		return
	}

	// An interested party may not switch a conflict rule off: a principal
	// holding either of the rule's actions is the one it constrains — holding
	// both, it denies them now; holding one, it is what stops them acquiring
	// the other (ZS-IAM-001 §10.1). Reactivating is not checked; it only
	// restores a denial.
	if !active && h.refuseInterestedSoDRetire(w, r, principalID, sodRuleID, tenantScope) {
		return
	}

	expectedVersion, ok := h.readCommand(w, &r)
	if !ok {
		return
	}
	rule, err := h.store.SetSoDRuleActive(r.Context(), sodRuleID, tenantScope, active, expectedVersion)
	if err != nil {
		if errors.Is(err, domain.ErrVersionConflict) {
			writeVersionConflict(w)
			return
		}
		if errors.Is(err, domain.ErrSoDRuleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "sod_rule_not_found"})
			return
		}
		h.log.Error("setSoDRuleActive: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Logged at Info with both actions named: turning an SoD control off is a
	// governance event in its own right, and the pair is what makes the log
	// entry mean anything later.
	h.log.Info("sod rule active flag set",
		zap.String("sod_rule_id", rule.SoDRuleID),
		zap.String("action_a", rule.ActionA),
		zap.String("action_b", rule.ActionB),
		zap.Bool("active", rule.ActiveFlag),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, rule)
}

// ── /v1/admin/abac-rules ─────────────────────────────────────────────────────

// ActionABACRuleManageGlobal is the action a principal must hold, against the
// platform-scope legal entity, to author an attribute condition that applies
// to every tenant. Exactly the same posture as ActionSoDRuleManageGlobal, and
// for exactly the same reason: a platform-wide rule on a deny-only layer can
// deny an action for every principal on the estate, so authoring one is a
// distinct grant rather than a side effect of omitting tenant_id.
const ActionABACRuleManageGlobal = "ABAC_RULE_MANAGE_GLOBAL"

type createABACRuleRequest struct {
	RuleCode       string  `json:"rule_code"`
	ActionType     string  `json:"action_type"`
	Effect         string  `json:"effect"`
	AttributeKey   string  `json:"attribute_key"`
	Operator       string  `json:"operator"`
	AttributeValue *string `json:"attribute_value,omitempty"`
	// TenantID is optional — omit for a rule that applies across every
	// tenant, matching sod_rules' convention. Omitting it requires
	// ActionABACRuleManageGlobal at platform scope.
	TenantID *string `json:"tenant_id,omitempty"`
}

func (req createABACRuleRequest) missingField() string {
	switch {
	case req.RuleCode == "":
		return "rule_code"
	case req.ActionType == "":
		return "action_type"
	case req.Effect == "":
		return "effect"
	case req.AttributeKey == "":
		return "attribute_key"
	case req.Operator == "":
		return "operator"
	default:
		return ""
	}
}

// CreateABACRule handles POST /v1/admin/abac-rules — declare one attribute
// condition guarding one action.
//
// This is the surface that makes the ABAC layer real without this service
// inventing a policy: the engine is in internal/abac, and every rule it
// evaluates arrives through here from somebody who knows the business. The
// table ships empty, so until this route is used /v1/authorize behaves exactly
// as it did before the layer existed.
//
// effect and operator are validated against the sets the evaluator actually
// implements, and refused with 400 naming the supported values. An operator
// the evaluator cannot execute would deny the action for everybody holding it
// — a 400 at authoring time is very much cheaper than discovering that from a
// decision log.
//
// Response: 201 created / 400 missing or unsupported field / 401 missing
// principal or tenant scope / 403 foreign tenant, or not authorized for a
// platform-wide rule / 409 rule_code already used in this scope /
// 503 unavailable.
func (h *Handler) CreateABACRule(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req createABACRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	if req.Effect != domain.EffectRequire && req.Effect != domain.EffectForbid {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":     "unsupported_effect",
			"field":     "effect",
			"supported": domain.EffectRequire + "," + domain.EffectForbid,
		})
		return
	}
	operands, known := domain.ABACOperators[req.Operator]
	if !known {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":     "unsupported_operator",
			"field":     "operator",
			"supported": supportedOperators(),
		})
		return
	}
	if operands == 1 && (req.AttributeValue == nil || *req.AttributeValue == "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_field",
			"field":   "attribute_value",
			"message": "operator " + req.Operator + " compares against a value; only exists/not_exists take none",
		})
		return
	}

	// A tenant scope is required to reach this route at all, whether or not the
	// body names one — the same reasoning CreateSoDRule carries. Without it, a
	// request holding nothing but a principal header could store a rule with
	// tenant_id NULL that denies an action for every tenant on the platform.
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if req.TenantID != nil && *req.TenantID != "" {
		if h.refuseForeignTenant(w, *req.TenantID, tenantScope) {
			return
		}
		// Require iam.abac_rule.manage for tenant-scoped ABAC rules
		if !h.requirePermission(w, r, principalID, tenantScope, "iam.abac_rule.manage") {
			return
		}
	} else {
		if !h.requirePlatformAction(w, r, principalID, ActionABACRuleManageGlobal) {
			return
		}
	}

	rule, err := h.store.CreateABACRule(r.Context(), domain.CreateABACRuleParams{
		TenantID:             req.TenantID,
		RuleCode:             req.RuleCode,
		ActionType:           req.ActionType,
		Effect:               req.Effect,
		AttributeKey:         req.AttributeKey,
		Operator:             req.Operator,
		AttributeValue:       req.AttributeValue,
		CreatedByPrincipalID: principalID,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "abac_rule_code_conflict", "rule_code": req.RuleCode})
		case errors.Is(err, domain.ErrUnsupportedABACEffect), errors.Is(err, domain.ErrUnsupportedABACOperator), errors.Is(err, domain.ErrABACOperandRequired):
			// Reachable only if the store's validation is stricter than the
			// handler's, which would be a defect in one of them; reported as
			// 400 rather than 503 because it is the request that is wrong.
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_abac_rule", "message": err.Error()})
		default:
			h.log.Error("CreateABACRule: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	h.log.Info("abac rule created",
		zap.String("abac_rule_id", rule.ABACRuleID),
		zap.String("rule_code", rule.RuleCode),
		zap.String("action_type", rule.ActionType),
		zap.String("effect", rule.Effect),
		zap.Bool("platform_wide", rule.TenantID == nil),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, rule)
}

// ListABACRules handles GET /v1/admin/abac-rules — read the attribute
// conditions that will deny requests in this tenant.
//
// Returns the tenant's own rules AND the platform-wide ones (tenant_id NULL),
// on the same reasoning as ListSoDRules: the platform-wide ones deny just as
// hard and cannot be edited from the tenant, so hiding them would make a
// denial unexplainable from the console.
//
// Optional action_type query param narrows the list.
//
// Response: 200 the rules (possibly empty) / 401 missing principal or tenant
// scope / 503 unavailable.
func (h *Handler) ListABACRules(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// ZS-IAM-001 §21 "no broad IAM discovery beyond administrable scope":
	// the register is readable with the Appendix A read permission.
	if !h.requirePermission(w, r, callerPrincipal, tenantScope, "iam.policy.read") {
		return
	}

	rules, err := h.store.ListABACRules(r.Context(), tenantScope, strings.TrimSpace(r.URL.Query().Get("action_type")))
	if err != nil {
		h.log.Error("ListABACRules: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if rules == nil {
		rules = []domain.ABACRule{}
	}
	writeJSON(w, http.StatusOK, rules)
}

// RetireABACRule handles POST /v1/admin/abac-rules/{abac_rule_id}/retire.
//
// Retiring is how a rule stops denying: active_flag is in FindABACRules'
// predicate, so the next evaluation ignores it. The row stays, because a
// decision the rule already caused has to remain explainable.
//
// Idempotent — retiring an already-retired rule is 200, not 409. The caller
// asked for a state and that state holds.
//
// Response: 200 retired / 401 missing principal or tenant scope / 404 not
// found or owned by another scope / 503 unavailable.
func (h *Handler) RetireABACRule(w http.ResponseWriter, r *http.Request) {
	h.setABACRuleActive(w, r, false)
}

// ReactivateABACRule handles POST /v1/admin/abac-rules/{abac_rule_id}/reactivate.
// Restores exactly the denials the retirement suspended. Response shape
// matches RetireABACRule.
func (h *Handler) ReactivateABACRule(w http.ResponseWriter, r *http.Request) {
	h.setABACRuleActive(w, r, true)
}

func (h *Handler) setABACRuleActive(w http.ResponseWriter, r *http.Request, active bool) {
	abacRuleID := chi.URLParam(r, "abac_rule_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.abac_rule.manage to retire/reactivate ABAC rules
	if !h.requirePermission(w, r, principalID, tenantScope, "iam.abac_rule.manage") {
		return
	}

	// The store's predicate is `tenant_id = $3` with no IS NULL branch, so a
	// PLATFORM-WIDE rule answers 404 here rather than being retired from one
	// tenant's console. That is deliberate and is the point: a rule binding
	// every tenant must not be disableable by any one of them.
	expectedVersion, ok := h.readCommand(w, &r)
	if !ok {
		return
	}
	rule, err := h.store.SetABACRuleActive(r.Context(), abacRuleID, tenantScope, active, expectedVersion)
	if err != nil {
		if errors.Is(err, domain.ErrVersionConflict) {
			writeVersionConflict(w)
			return
		}
		if errors.Is(err, domain.ErrABACRuleNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "abac_rule_not_found"})
			return
		}
		h.log.Error("setABACRuleActive: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	h.log.Info("abac rule active flag set",
		zap.String("abac_rule_id", rule.ABACRuleID),
		zap.String("rule_code", rule.RuleCode),
		zap.Bool("active", rule.ActiveFlag),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, rule)
}

// supportedOperators renders domain.ABACOperators for a 400 body, sorted so
// the message is stable rather than in Go's randomised map order.
func supportedOperators() string {
	ops := make([]string, 0, len(domain.ABACOperators))
	for op := range domain.ABACOperators {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	return strings.Join(ops, ",")
}

// ── POST /v1/authorize ────────────────────────────────────────────────────────

type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
	// TenantID is the FALLBACK source of the tenant scope, kept only for
	// callers that do not forward X-Tenant-Id yet. See resolveTenantScope:
	// the header wins, and a body that disagrees with it is refused.
	TenantID string `json:"tenant_id,omitempty"`
	// BookID and OrgUnitID provide hierarchical scope dimensions (ZS-IAM-001 §4, Scenario A03).
	BookID    string `json:"book_id,omitempty"`
	OrgUnitID string `json:"org_unit_id,omitempty"`
	// ResourceOwnerPrincipalID is optional: the principal who prepared or
	// created the specific object action_type is being performed against
	// (e.g. an invoice's preparer), supplied by the calling service. Until
	// this field existed there was no resource-attribute input to
	// /v1/authorize at all — ZS-IAM-001 §10.2's dynamic Segregation-of-
	// Duties layer ("a preparer cannot approve their own object") had
	// nothing to evaluate against. Omitting it preserves today's
	// behavior: no own-object check is attempted.
	ResourceOwnerPrincipalID string `json:"resource_owner_principal_id,omitempty"`

	// Attributes are the request/resource attributes the ABAC layer evaluates
	// declared conditions against — an amount, a channel, a classification,
	// whatever the rules in abac_rules name. Supplied by the calling service,
	// which is the only party that knows them.
	//
	// Omitting it is the normal case and changes nothing unless a REQUIRE rule
	// exists for the action: an attribute a rule requires and the caller did
	// not send is a denial, because a required condition that cannot be
	// evaluated has not been satisfied. See domain.EffectRequire.
	//
	// Values are strings even when they represent numbers. The comparison
	// operators parse both sides numerically when they can (see
	// internal/abac.compare), so "10000" orders as a number, and a caller does
	// not have to know which of its attributes a rule will treat as ordered.
	Attributes map[string]string `json:"attributes,omitempty"`

	// PrivilegedSessionID is optional: a Just-in-Time elevated session ID (ZS-IAM-001 §13 & §21).
	// If the principal does not hold a standing grant or delegation, an active JIT session
	// with matching requested action grants elevation while preserving ticket reference and basis.
	PrivilegedSessionID string `json:"privileged_session_id,omitempty"`

	// BreakGlassSessionID is optional: emergency break-glass session ID (ZS-IAM-001 §14 & §21).
	// Exception elevation for declared incidents.
	BreakGlassSessionID string `json:"break_glass_session_id,omitempty"`

	// SupportSessionID is optional: tenant support access session ID (ZS-IAM-001 §15 & §21).
	// Purpose-bound diagnostic / support access with operator attribution.
	SupportSessionID string `json:"support_session_id,omitempty"`

	// Workload Identity (ZS-IAM-001 §16)
	PrincipalType       string `json:"principal_type,omitempty"`
	Audience            string `json:"audience,omitempty"`
	InitiatingSubjectID string `json:"initiating_subject_id,omitempty"`

	// The target (ZS-IAM-001 §8.1). Optional; recorded in the decision
	// evidence (GOV-03 "resource") when supplied.
	ResourceType    string `json:"resource_type,omitempty"`
	ResourceID      string `json:"resource_id,omitempty"`
	ResourceVersion string `json:"resource_version,omitempty"`
}

// PlatformScopeSentinel is the legal_entity_id a caller sends to have a
// PLATFORM-WIDE act evaluated — one with no owning legal entity at all.
//
// WHY THIS EXISTS. legal_entity_id is required, and every scope this service
// evaluates is a legal entity, so a caller authorizing something that belongs
// to the platform rather than to an entity had nowhere to put it. Tracker item
// 67: "authorization-svc has no platform-scoped, non-entity resource concept —
// services fake a synthetic legal_entity_id as a workaround." They do, and
// each one picks its own: jurisdiction-rules-svc carries
// AUTHZ_PLATFORM_SCOPE_ID, this service's own main.go hardcodes a different
// constant for its mTLS identity, and a grant seeded against one of those is
// invisible to a check made against the other. Silent, and fail-closed, so it
// reads as "no grant" rather than as a mismatch.
//
// A SENTINEL rather than accepting an empty legal_entity_id, deliberately. An
// omitted field is far more often a bug in the caller than a platform-scope
// request, and quietly promoting it to platform scope would turn that bug into
// an evaluation against the wrong scope. Omitting the field still answers 400.
// Naming the sentinel is an explicit statement of intent.
//
// It resolves to AUTHZ_PLATFORM_SCOPE_ENTITY_ID, so the platform scope is ONE
// id configured in one place — the same id requirePlatformAction already
// authorizes this service's own platform-wide acts against. An unset config
// refuses these requests rather than inventing an id, which is the same
// fail-closed direction requirePlatformAction takes.
const PlatformScopeSentinel = "PLATFORM"

// principalStatusBasisPrefix is the decision_basis a layer-0 denial carries:
// "principal_status:SUSPENDED", "principal_status:DISABLED".
//
// The STATUS is in the basis, not just the fact of denial, because the two
// mean different things to whoever reads the log — suspended is reversible and
// usually deliberate, disabled is usually terminal — and a bare
// "principal_not_active" would make an operator go and ask
// identity-context-svc which it was.
//
// Prefixed rather than bare so the basis vocabulary stays parseable in the
// same way "rbac:", "sod:" and "abac:" already are. NOT prefixed "sod:", which
// would have been convenient for the severity branch and wrong: the sod:
// prefix is what makes recordAndAnswer publish sod.violation.detected, and a
// suspended principal is not a duty conflict.
const principalStatusBasisPrefix = "principal_status:"

// entityStatusBasisPrefix is the basis of a denial because the legal entity is
// SUSPENDED or DISSOLVED: "entity_status:DISSOLVED".
const entityStatusBasisPrefix = "entity_status:"

type authorizeResponse struct {
	// DecisionOutcome is the original contract — GRANTED | DENIED | STEP_UP —
	// and stays as it was for every existing caller. REQUIRE_APPROVAL is
	// DENIED here: a caller that only reads this field must not proceed.
	DecisionOutcome  string `json:"decision_outcome"`
	DecisionBasis    string `json:"decision_basis"`
	AccessDecisionID string `json:"access_decision_id"`
	Reason           string `json:"reason,omitempty"`

	// The ZS-IAM-001 §8.2 canonical decision, added alongside (not instead).
	Decision         string   `json:"decision"`
	PolicySetVersion string   `json:"policy_set_version"`
	Obligations      []string `json:"obligations"`
	ReasonCodes      []string `json:"reason_codes"`
	ExpiresAt        string   `json:"expires_at,omitempty"`
}

// Authorize handles POST /v1/authorize — the core evaluation endpoint.
//
// Layers, in order:
//  0. Principal status — has identity-context-svc suspended or disabled this
//     principal? A principal that is not ACTIVE is denied every action,
//     before any grant is looked up. Deny-only, and ABSENT MEANS ACTIVE, so
//     the layer is inert until a principal.status.changed event has been
//     projected — see domain.PrincipalStatusProjection and migration 000013.
//     This closes a hole session eviction does not: identity-context-svc
//     evicts SESSIONS on suspension, but this endpoint is called
//     service-to-service on envelopes resolved before the suspension and on
//     queued work that carries a principal and no session at all.
//  1. RBAC — does the principal directly hold a role granting action_type
//     in legal_entity_id?
//  2. Delegated access — if not, does the principal have an active
//     delegation from someone who holds that grant?
//  3. Static SoD — if granted by either layer, does granting it conflict
//     with anything else the principal already holds (RBAC ∪ delegated)?
//  4. Dynamic (own-object) SoD — if still granted, and the caller supplied
//     ResourceOwnerPrincipalID, does the principal own the object they are
//     acting on, for an action_type a data-declared rule forbids
//     self-performing? ZS-IAM-001 §10.2's example: a preparer cannot
//     approve their own object. This is independent of layer 3 — it is
//     one action against one object's ownership, not a pair of actions
//     held simultaneously — so it needs its own store query
//     (CheckOwnObjectSoD) rather than reusing CheckSoDConflict.
//  5. ABAC — if still granted, do the attribute conditions declared for
//     this action in abac_rules hold for the attributes the caller sent?
//     Deny-only: a rule removes an action the earlier layers conferred and
//     can never add one. See internal/abac for the engine and
//     domain.ABACRule for the semantics. abac_rules ships EMPTY, so this
//     layer changes no outcome until somebody declares a rule.
//
// Layers 0 through 5 are all cached reads (internal/cache). The DECISION is
// not, and neither is the artifact — see below.
//
// Every evaluation — grant or deny — is written to access_decision_log
// before the response is returned (critical constraint: no material action
// without a decision artifact). That insert is NOT cached, batched or
// deferred on any path: a cache hit removes database reads, never the
// evidence. On any internal error, the result is a denial, never a silent
// allow (fail-closed) — see the deferred-write comment below for the one
// exception, which is documented, not silent.
//
// The tenant scope of the evaluation comes from the verified X-Tenant-Id
// header when the caller sends one — see resolveTenantScope for why that
// matters more than it looks.
//
// A PLATFORM-WIDE act — one with no owning legal entity — is requested by
// sending legal_entity_id=PLATFORM; see PlatformScopeSentinel.
//
// Response: 200 with decision_outcome GRANTED|DENIED (both are 200 — the
// HTTP status reflects "the evaluation succeeded", not the outcome) /
// 400 missing field, or legal_entity_id=PLATFORM on a deployment with no
// platform-scope entity configured / 403 body tenant_id disagrees with the
// verified header / 503 store unavailable (fail-closed, no decision recorded).
// resolveTenantScope decides which tenant's SoD rules apply to this
// evaluation: the verified X-Tenant-Id header if the caller forwards one,
// otherwise the body's tenant_id, otherwise none.
//
// WHY THIS EXISTS. CheckSoDConflict's predicate is
// `tenant_id IS NULL OR tenant_id = <given>`, so an absent tenant narrows the
// check to globally-applicable rules ONLY. The tenant was read exclusively
// from the request body, and across this estate just three of roughly sixty
// authz clients put it there — so a tenant admin could create a
// segregation-of-duties rule, get a 201, see it in the register, and have it
// silently never applied to a decision made through any of the other
// fifty-odd services. SoD enforcement was opt-in per calling service, which
// is the inverse of what a control is for.
//
// Reading the header first makes the tenant arrive with the same identity the
// gateway already verified, so a caller that forwards the header cannot
// under-scope the check by omitting a body field. The body remains a fallback
// rather than an error because ~60 services call this endpoint and most send
// no tenant at all yet: rejecting them would replace a weak check with an
// outage. A body that CONTRADICTS the header is refused outright — that is
// not an old caller, it is a caller trying to be evaluated in someone else's
// scope.
//
// A question about the platform-scope entity is tenantless by design — a
// platform grant is held there by a role of any tenant — so it is admitted
// without a tenant even when enforceTenantOnAuthorize is set. Without that
// exemption, enforcement could not be switched on at all: every platform-scope
// check (decision log, configuration, vault) sends no tenant.
func (h *Handler) resolveTenantScope(w http.ResponseWriter, r *http.Request, bodyTenantID, evaluationEntityID string) (string, bool) {
	headerTenantID := r.Header.Get("X-Tenant-Id")

	if headerTenantID != "" {
		if bodyTenantID != "" && bodyTenantID != headerTenantID {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "tenant_scope_mismatch",
				"message": "request tenant_id does not match the caller's verified tenant scope",
			})
			return "", false
		}
		if !validTenantScope(w, headerTenantID, "X-Tenant-Id") {
			return "", false
		}
		return headerTenantID, true
	}

	if bodyTenantID != "" {
		if !validTenantScope(w, bodyTenantID, "tenant_id") {
			return "", false
		}
		// Logged, not refused: this is the pre-header calling convention, and
		// the log is what makes the remaining callers findable.
		h.log.Debug("authorize: tenant scope taken from request body — caller does not forward X-Tenant-Id",
			zap.String("correlation_id", r.Header.Get("X-Correlation-ID")))
		return bodyTenantID, true
	}

	// No tenant anywhere: only globally-applicable SoD rules can be
	// considered.
	if h.isPlatformScopeEntity(evaluationEntityID) {
		return "", true
	}
	if h.enforceTenantOnAuthorize {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_tenant_scope",
			"message": "X-Tenant-Id header or tenant_id in body is required",
		})
		return "", false
	}

	// Warned at every call so "this tenant's SoD rules never
	// fired" is diagnosable from the logs rather than from an incident.
	// The caller is named so the remaining tenantless callers can be listed and
	// migrated: a tenantless question about an ordinary entity is evaluated
	// against every tenant's roles, and AUTHZ_ENFORCE_TENANT_ON_AUTHORIZE can
	// only be switched on once this log is silent.
	h.log.Warn("authorize: no tenant scope supplied — evaluated across tenants; only global SoD rules apply",
		zap.String("correlation_id", r.Header.Get("X-Correlation-ID")),
		zap.String("source_system", r.Header.Get("X-Source-System")),
		zap.String("user_agent", r.UserAgent()),
		zap.String("legal_entity_id", evaluationEntityID))
	return "", true
}

func (h *Handler) Authorize(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req authorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if req.PrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "principal_id"})
		return
	}
	if req.LegalEntityID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "legal_entity_id"})
		return
	}
	if req.ActionType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "action_type"})
		return
	}

	// PLATFORM resolves to the one configured platform-scope entity, so a
	// platform-wide act is evaluated against the same id everywhere instead of
	// against whichever synthetic uuid each calling service invented. Fails
	// closed on an unconfigured deployment: a 400 naming the missing config,
	// not a guess. Shared with the three §8.3 validation routes, which accept
	// the sentinel on identical terms — see resolvePlatformScope.
	evaluationEntityID, ok := h.resolvePlatformScope(w, req.LegalEntityID, correlationID)
	if !ok {
		return
	}

	tenantScope, ok := h.resolveTenantScope(w, r, req.TenantID, evaluationEntityID)
	if !ok {
		return
	}

	if req.BookID == "" {
		req.BookID = r.Header.Get("X-Book-Id")
	}
	if req.OrgUnitID == "" {
		req.OrgUnitID = r.Header.Get("X-Org-Unit-Id")
	}

	authnAge := 0
	if req.Attributes != nil && req.Attributes["authn_age_seconds"] != "" {
		authnAge, _ = strconv.Atoi(req.Attributes["authn_age_seconds"])
	} else if rawAge := r.Header.Get("X-Authn-Age-Seconds"); rawAge != "" {
		authnAge, _ = strconv.Atoi(rawAge)
	}
	aud := req.Audience
	if aud == "" {
		aud = r.Header.Get("X-Audience")
		if aud == "" {
			aud = r.Header.Get("X-Token-Audience")
		}
	}
	risk := ""
	if req.Attributes != nil {
		risk = req.Attributes["risk"]
	}

	// The same pipeline the canonical decision API and available-actions run
	// — see evaluateCore for why there is exactly one.
	in := evalContext{
		PrincipalID:         req.PrincipalID,
		PrincipalType:       req.PrincipalType,
		TenantID:            tenantScope,
		LegalEntityID:       req.LegalEntityID,
		BookID:              req.BookID,
		OrgUnitID:           req.OrgUnitID,
		ResourceType:        req.ResourceType,
		ResourceID:          req.ResourceID,
		ResourceVersion:     req.ResourceVersion,
		ActionType:          req.ActionType,
		ResourceOwnerID:     req.ResourceOwnerPrincipalID,
		Attributes:          req.Attributes,
		Environment:         domain.EnvironmentContext{AuthnAgeSeconds: authnAge, Assurance: r.Header.Get("X-Assurance-Level"), Risk: risk, Audience: aud},
		PrivilegedSessionID: req.PrivilegedSessionID,
		BreakGlassSessionID: req.BreakGlassSessionID,
		SupportSessionID:    req.SupportSessionID,
		CorrelationID:       correlationID,
		InitiatingSubjectID: req.InitiatingSubjectID,
		SkipAvailable:       true,
	}
	res, err := h.evaluateCore(r.Context(), in, evaluationEntityID)
	if err != nil {
		// Fail closed: "cannot evaluate" is a 503, never a recorded outcome.
		h.log.Error("Authorize: evaluation failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	decision, ok := h.recordDecision(w, r, in, evaluationEntityID, res)
	if !ok {
		return
	}
	h.emitDecisionTelemetry(r.Context(), req.ActionType, req.PrincipalID, evaluationEntityID, tenantScope, correlationID, res, decision)

	h.log.Info("authorization evaluated",
		zap.String("principal_id", req.PrincipalID),
		zap.String("action_type", req.ActionType),
		zap.String("decision", res.Decision),
		zap.String("basis", res.Basis),
		zap.String("correlation_id", correlationID),
	)
	resp := authorizeResponse{
		DecisionOutcome:  res.Outcome,
		DecisionBasis:    res.Basis,
		AccessDecisionID: decision.AccessDecisionID,
		Decision:         res.Decision,
		PolicySetVersion: decision.PolicySetVersion,
		Obligations:      nonNil(res.Obligations),
		ReasonCodes:      nonNil(res.ReasonCodes),
	}
	// reason was only ever set for the workload refusals; kept to that.
	if strings.HasPrefix(res.Basis, "workload:") {
		resp.Reason = res.Reason
	}
	if decision.ExpiresAt != nil {
		resp.ExpiresAt = decision.ExpiresAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// decisionTTL bounds how long a PEP may rely on a decision (§8.2 expires_at).
const decisionTTL = 5 * time.Minute

// delegationSourceFinder is the store capability that names the delegation
// a delegated grant rests on, for the §11 attribution rule. Optional; a store
// without it refuses rather than record a delegated decision unattributed.
type delegationSourceFinder interface {
	FindDelegationSource(ctx context.Context, delegatePrincipalID, legalEntityID, tenantID, bookID, orgUnitID, actionType string) (delegatorPrincipalID, delegationID string, err error)
}

// grantingAssignmentFinder names the assignments behind an RBAC grant.
type grantingAssignmentFinder interface {
	FindGrantingAssignments(ctx context.Context, principalID, legalEntityID, tenantID, bookID, orgUnitID, actionType string) ([]domain.GrantingAssignment, error)
}

// recordDecision writes the decision artifact with its full evidence. Every
// decision leaves /v1/authorize and the canonical API through here, so "no
// material action executes without an authorization decision artifact" holds
// on both. Writes a 503 and returns false if the artifact cannot be written.
func (h *Handler) recordDecision(w http.ResponseWriter, r *http.Request, in evalContext, evaluationEntityID string, res *evalResult) (*domain.AccessDecisionLog, bool) {
	expires := time.Now().UTC().Add(decisionTTL)
	params := domain.RecordAccessDecisionParams{
		PrincipalID: in.PrincipalID,
		// The RESOLVED entity, not the PLATFORM sentinel — legal_entity_id is
		// UUID NOT NULL, and the evidence must name the scope evaluated.
		LegalEntityID:    evaluationEntityID,
		ActionType:       in.ActionType,
		Outcome:          res.Outcome,
		Basis:            res.Basis,
		CorrelationID:    in.CorrelationID,
		TenantID:         in.TenantID,
		Events:           h.decisionEvents,
		Decision:         res.Decision,
		Obligations:      res.Obligations,
		ReasonCodes:      res.ReasonCodes,
		MatchedGrants:    res.MatchedGrants,
		ResourceType:     in.ResourceType,
		ResourceID:       in.ResourceID,
		ResourceVersion:  in.ResourceVersion,
		AttributesDigest: attributesDigest(in.Attributes),
		SessionAssurance: in.Environment.Assurance,
		ExpiresAt:        &expires,
	}
	// §11: "Every delegated action records both actor_subject_id and
	// on_behalf_of_subject_id / delegation_id." The basis named only the
	// first delegator, which need not be the one whose authority was used.
	if strings.HasPrefix(res.Basis, "delegated:") && res.Outcome == domain.OutcomeGranted {
		finder, ok := h.store.(delegationSourceFinder)
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return nil, false
		}
		delegator, delegationID, err := finder.FindDelegationSource(r.Context(), in.PrincipalID, evaluationEntityID, in.TenantID, in.BookID, in.OrgUnitID, in.ActionType)
		if err != nil {
			h.log.Error("decision: delegation attribution failed — refusing", zap.String("correlation_id", in.CorrelationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return nil, false
		}
		params.OnBehalfOf, params.DelegationID = delegator, delegationID
	}
	// §20 "assignment references": which assignment, through which role,
	// conferred an RBAC grant — "assignment:<id>" and "role:<code>" — so use
	// is attributed to the assignment that granted it, not to every role whose
	// code appears in the basis (access reviews' DORMANT, §24).
	if strings.HasPrefix(res.Basis, "rbac:") && res.Outcome == domain.OutcomeGranted {
		finder, ok := h.store.(grantingAssignmentFinder)
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return nil, false
		}
		granting, err := finder.FindGrantingAssignments(r.Context(), in.PrincipalID, evaluationEntityID, in.TenantID, in.BookID, in.OrgUnitID, in.ActionType)
		if err != nil {
			h.log.Error("decision: assignment attribution failed — refusing", zap.String("correlation_id", in.CorrelationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return nil, false
		}
		refs := make([]string, 0, 2*len(granting))
		for _, g := range granting {
			refs = append(refs, "assignment:"+g.AssignmentID, "role:"+g.RoleCode)
		}
		params.MatchedGrants = append(refs, nonRBAC(res.MatchedGrants)...)
	}

	decision, err := h.store.RecordAccessDecision(r.Context(), params)
	if err != nil {
		h.log.Error("failed to record access decision", zap.String("correlation_id", in.CorrelationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return nil, false
	}
	return decision, true
}

// attributesDigest is a SHA-256 over the attribute map in key order, so the
// evidence proves what was evaluated without retaining the values (§25).
func attributesDigest(attrs map[string]string) string {
	if len(attrs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sum := sha256.New()
	for _, k := range keys {
		sum.Write([]byte(k))
		sum.Write([]byte{0})
		sum.Write([]byte(attrs[k]))
		sum.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil))
}

// nonRBAC keeps the matched-grant entries that are not the RBAC basis string
// (e.g. sod_exception references), which assignment references replace.
func nonRBAC(grants []string) []string {
	var out []string
	for _, g := range grants {
		if !strings.HasPrefix(g, "rbac:") {
			out = append(out, g)
		}
	}
	return out
}

func optionalString(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ── GET /v1/admin/role-assignments ──────────────────────────────────────────

// assignmentPager is the store capability behind the paged list.
type assignmentPager interface {
	QueryRoleAssignments(ctx context.Context, q domain.AssignmentQuery) ([]domain.PrincipalRoleAssignment, error)
}

// atoiOrZero parses an optional non-negative query integer; "" is 0.
func atoiOrZero(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.Atoi(v)
}

// ListRoleAssignments handles GET /v1/admin/role-assignments — read the
// grants that actually exist.
//
// Added because every write path here had no read to match it: an assignment
// could be created and revoked, but never listed, so the only way to see who
// held what was to query the database directly. The console consequently had
// no way to offer "revoke" at all — it could not learn the assignment_id.
//
// Authenticated and tenant-scoped, on the same footing as every other admin
// route: an assignment names a principal and the role they hold, which is
// precisely the who-can-do-what map that GetAccessDecision was hardened to
// stop leaking.
//
// Query params, all optional: principal_id and role_id narrow the list;
// include_expired=true adds revoked and not-yet-effective rows (default is
// active only, which is what a revoke decision needs).
//
// Response: 200 the assignments (possibly empty) / 401 missing principal or
// tenant scope / 503 unavailable.
func (h *Handler) ListRoleAssignments(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// ZS-IAM-001 §21 "no broad IAM discovery beyond administrable scope":
	// the register is readable with the Appendix A read permission.
	if !h.requirePermission(w, r, callerPrincipal, tenantScope, "iam.assignment.read") {
		return
	}

	principalFilter := strings.TrimSpace(r.URL.Query().Get("principal_id"))
	roleFilter := strings.TrimSpace(r.URL.Query().Get("role_id"))
	activeOnly := r.URL.Query().Get("include_expired") != "true"

	// Paged / usage form: limit, offset, include_usage=true. The plain form
	// stops at 500 rows without saying so; a caller that must see every
	// assignment (an access review) pages with offset until a short page.
	qs := r.URL.Query()
	if qs.Has("limit") || qs.Has("offset") || qs.Get("include_usage") == "true" {
		limit, lErr := atoiOrZero(qs.Get("limit"))
		offset, oErr := atoiOrZero(qs.Get("offset"))
		if lErr != nil || oErr != nil || limit < 0 || offset < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_paging", "message": "limit and offset must be non-negative integers"})
			return
		}
		pager, ok := h.store.(assignmentPager)
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		page, err := pager.QueryRoleAssignments(r.Context(), domain.AssignmentQuery{
			TenantID: tenantScope, PrincipalID: principalFilter, RoleID: roleFilter, ActiveOnly: activeOnly,
			IncludeUsage: qs.Get("include_usage") == "true", Limit: limit, Offset: offset,
		})
		if err != nil {
			h.log.Error("ListRoleAssignments: store unavailable",
				zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if page == nil {
			page = []domain.PrincipalRoleAssignment{}
		}
		writeJSON(w, http.StatusOK, page)
		return
	}

	// A malformed role_id must not read as an outage — same posture as
	// validScope on the authorize path. role_id is compared as ::text in the
	// query, so a non-UUID is a valid comparison that matches nothing rather
	// than a driver error; nothing to reject here, and nothing to 503 over.

	assignments, err := h.store.ListRoleAssignments(r.Context(), tenantScope, principalFilter, roleFilter, activeOnly)
	if err != nil {
		h.log.Error("ListRoleAssignments: store unavailable",
			zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if assignments == nil {
		assignments = []domain.PrincipalRoleAssignment{}
	}
	writeJSON(w, http.StatusOK, assignments)
}

// ── GET /v1/admin/sod-rules ─────────────────────────────────────────────────

// ListSoDRules handles GET /v1/admin/sod-rules — read the conflict rules
// that will deny requests in this tenant.
//
// Returns the tenant's own rules AND the globally-applicable ones
// (tenant_id NULL), because both deny identically and the global ones are
// the set a tenant operator cannot edit but will still be blocked by. Hiding
// them would make a denial unexplainable from the console.
//
// Response: 200 the rules (possibly empty) / 401 missing principal or tenant
// scope / 503 unavailable.
func (h *Handler) ListSoDRules(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// ZS-IAM-001 §21 "no broad IAM discovery beyond administrable scope":
	// the register is readable with the Appendix A read permission.
	if !h.requirePermission(w, r, callerPrincipal, tenantScope, "iam.sod_rule.read") {
		return
	}

	rules, err := h.store.ListSoDRules(r.Context(), tenantScope)
	if err != nil {
		h.log.Error("ListSoDRules: store unavailable",
			zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if rules == nil {
		rules = []domain.SoDRule{}
	}
	writeJSON(w, http.StatusOK, rules)
}

// ── GET /v1/admin/roles ─────────────────────────────────────────────────────

// ListRoles handles GET /v1/admin/roles — read this tenant's role catalogue.
//
// The read that was missing from a write-only admin surface. Roles could be
// created, retired, reactivated and given permission bundles; none of that
// could be listed back, so the only way to know a role's id was to have
// created it in the same breath, and role_code could not be checked for
// collision before writing.
//
// Tenant-scoped from the VERIFIED header, never a query parameter: a
// tenant_id a caller can choose is not a scope. Retired roles are included by
// default and ordered last — a retired role is why access someone used to
// have is gone, and omitting it makes that unexplainable. `?active_only=true`
// narrows it.
//
// Authorization posture matches the sibling reads (ListRoleAssignments,
// ListSoDRules): principal and tenant required, no per-action grant. Reading
// your own tenant's role catalogue is not a privileged act, and gating it
// behind a grant nobody has seeded would make the console unusable while
// protecting a list the same caller can already infer from its assignments.
//
// Response: 200 the roles (possibly empty) / 401 missing principal or tenant
// scope / 503 unavailable.
func (h *Handler) ListRoles(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// ZS-IAM-001 §21 "no broad IAM discovery beyond administrable scope":
	// the register is readable with the Appendix A read permission.
	if !h.requirePermission(w, r, callerPrincipal, tenantScope, "iam.role.read") {
		return
	}

	activeOnly := r.URL.Query().Get("active_only") == "true"

	roles, err := h.store.ListRoles(r.Context(), tenantScope, activeOnly)
	if err != nil {
		h.log.Error("ListRoles: store unavailable",
			zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if roles == nil {
		roles = []domain.Role{}
	}
	writeJSON(w, http.StatusOK, roles)
}

// ── GET /v1/admin/delegated-authorities ─────────────────────────────────────

// ListDelegatedAuthorities handles GET /v1/admin/delegated-authorities — read
// who is currently acting on whose behalf in this tenant.
//
// The other half of a write-only surface. A delegation could be created and
// revoked by id and never listed, so the register a delegation exists to
// produce — who holds borrowed authority right now — could not be read from
// the service that evaluates against it.
//
// `?principal_id=` matches a principal on EITHER side of the delegation,
// because "this person's delegations" means both the authority they lent out
// and the authority they were lent. `?active_only=true` applies the same three
// conditions the evaluation path applies, so the register cannot report a
// delegation as live that /v1/authorize treats as expired.
//
// Revoked and expired delegations are returned by default, ordered after the
// live ones: a revoked delegation is the evidence that authority was
// withdrawn, which is exactly what an auditor came for.
//
// Response: 200 the delegations (possibly empty) / 401 missing principal or
// tenant scope / 503 unavailable.
func (h *Handler) ListDelegatedAuthorities(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// ZS-IAM-001 §21 "no broad IAM discovery beyond administrable scope":
	// the register is readable with the Appendix A read permission.
	if !h.requirePermission(w, r, callerPrincipal, tenantScope, "iam.delegation.read") {
		return
	}

	principalFilter := strings.TrimSpace(r.URL.Query().Get("principal_id"))
	activeOnly := r.URL.Query().Get("active_only") == "true"

	// principal_id is compared as ::text, so a non-UUID is a comparison that
	// matches nothing rather than a driver error — the same posture
	// ListRoleAssignments takes with role_id, and the reason neither needs to
	// reject a malformed filter with a 400 or surface one as a 503.

	delegations, err := h.store.ListDelegatedAuthorities(r.Context(), tenantScope, principalFilter, activeOnly)
	if err != nil {
		h.log.Error("ListDelegatedAuthorities: store unavailable",
			zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if delegations == nil {
		delegations = []domain.DelegatedAuthority{}
	}
	writeJSON(w, http.StatusOK, delegations)
}

// ── GET /v1/access-decisions/{access_decision_id} ───────────────────────────

// GetAccessDecision handles GET /v1/access-decisions/{access_decision_id} —
// the "retrieve authorization rationale" capability.
//
// Authenticated and tenant-scoped. It was neither: the route checked no
// principal and no tenant, and the query carried no tenant or entity
// predicate, so anyone able to reach the port could walk decision ids and
// read principal_id, legal_entity_id, action_type, decision_outcome and
// decision_basis for every tenant — and decision_basis carries
// `sod:conflict_with=<action>`, which names where the segregation-of-duties
// tripwires are. A readable map of who may do what, and where the alarms are.
//
// A decision belonging to another tenant, and a decision recorded with no
// tenant at all, both answer 404 rather than 403 — a probe must not be able
// to confirm that an id exists.
//
// Response: 200 the decision / 401 missing principal or tenant scope /
// 404 not found, not yours, or unattributed / 503 unavailable.
func (h *Handler) GetAccessDecision(w http.ResponseWriter, r *http.Request) {
	accessDecisionID := chi.URLParam(r, "access_decision_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	d, err := h.store.FindAccessDecisionByID(r.Context(), accessDecisionID, tenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrAccessDecisionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "access_decision_not_found"})
			return
		}
		h.log.Error("GetAccessDecision: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	// Same restriction as the listing (see ListAccessDecisions). Somebody
	// else's decision without iam.policy.read is 404, not 403: confirming
	// that a decision about another principal exists is itself disclosure.
	fullRead, err := h.holdsPermission(r, callerPrincipal, tenantScope, PermissionPolicyRead)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if !fullRead {
		if d.PrincipalID != callerPrincipal {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "access_decision_not_found"})
			return
		}
		redactDecision(d)
	}
	writeJSON(w, http.StatusOK, d)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// validScope reports whether v is a well-formed UUID, for a value that will be
// compared against a uuid COLUMN.
//
// WHY THIS EXISTS. known-gaps.md records the shape as a platform-wide habit:
// "a malformed authorization scope read as an outage". legal_entity_id is a
// uuid column, and passing a non-UUID to a uuid comparison is a driver error,
// which this service's store layer wraps as ErrStoreUnavailable and every
// handler answers 503 for. From a calling service that 503 is
// indistinguishable from this service genuinely being down, so somebody who
// mistyped an entity id was told the authorization plane had failed.
//
// Only for values compared against a uuid column. A malformed value compared
// as ::text — role_id in ListRoleAssignments, principal_id anywhere — is a
// valid comparison that matches nothing, and rejecting those would refuse
// legitimate non-UUID principal ids: this service has never required a
// principal id to be a UUID, and service-account ids are not.
func validScope(v string) bool {
	_, err := uuid.Parse(v)
	return err == nil
}

func contains(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func removeAll(list []string, target string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(withErrorClass(status, v)); err != nil {
		_ = err
	}
}

// ── Privileged Access Management Handlers (ZS-IAM-001 §13 & §21) ───────────

type createPrivilegedSessionRequest struct {
	TenantID         string   `json:"tenant_id,omitempty"`
	PrincipalID      string   `json:"principal_id,omitempty"`
	RequestedActions []string `json:"requested_actions"`
	TicketRef        string   `json:"ticket_ref"`
	Reason           string   `json:"reason"`
	DurationSeconds  int      `json:"duration_seconds"`
}

func (h *Handler) CreatePrivilegedSession(w http.ResponseWriter, r *http.Request) {
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.pam.manage to create privileged sessions
	if !h.requirePermission(w, r, callerPrincipalID, tenantScope, "iam.pam.manage") {
		return
	}

	correlationID := r.Header.Get("X-Correlation-ID")

	var req createPrivilegedSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}

	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	targetPrincipal := req.PrincipalID
	if targetPrincipal == "" {
		targetPrincipal = callerPrincipalID
	}
	if req.TicketRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "ticket_ref"})
		return
	}
	if req.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "reason"})
		return
	}
	if len(req.RequestedActions) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "requested_actions"})
		return
	}

	ps, err := h.store.CreatePrivilegedSession(r.Context(), domain.CreatePrivilegedSessionParams{
		TenantID:         tenantScope,
		PrincipalID:      targetPrincipal,
		RequestedActions: req.RequestedActions,
		TicketRef:        req.TicketRef,
		Reason:           req.Reason,
		DurationSeconds:  req.DurationSeconds,
	})
	if err != nil {
		h.log.Error("CreatePrivilegedSession: store failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	h.log.Info("privileged session created",
		zap.String("session_id", ps.SessionID),
		zap.String("principal_id", ps.PrincipalID),
		zap.String("ticket_ref", ps.TicketRef),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, ps)
}

func (h *Handler) ListPrivilegedSessions(w http.ResponseWriter, r *http.Request) {
	principalID := r.URL.Query().Get("principal_id")
	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Your own sessions are yours to see; anybody else's — or the whole
	// register — needs the permission that administers them.
	if principalID != callerPrincipal && !h.requirePermission(w, r, callerPrincipal, tenantScope, "iam.pam.manage") {
		return
	}
	activeOnly := r.URL.Query().Get("active_only") == "true"

	sessions, err := h.store.ListPrivilegedSessions(r.Context(), tenantScope, principalID, activeOnly)
	if err != nil {
		h.log.Error("ListPrivilegedSessions: store failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (h *Handler) RevokePrivilegedSession(w http.ResponseWriter, r *http.Request) {
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.pam.manage to revoke privileged sessions
	if !h.requirePermission(w, r, callerPrincipalID, tenantScope, "iam.pam.manage") {
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_session_id"})
		return
	}

	ps, err := h.store.RevokePrivilegedSession(r.Context(), sessionID, tenantScope, callerPrincipalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPrivilegedSessionNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "privileged_session_not_found", "session_id": sessionID})
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition", "message": err.Error()})
		default:
			h.log.Error("RevokePrivilegedSession: store failed", zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	writeJSON(w, http.StatusOK, ps)
}

// ── Break-Glass Emergency Session Handlers (ZS-IAM-001 §14 & §21) ───────────

type createBreakGlassRequest struct {
	TenantID         string   `json:"tenant_id,omitempty"`
	PrincipalID      string   `json:"principal_id,omitempty"`
	IncidentID       string   `json:"incident_id"`
	Reason           string   `json:"reason"`
	RequestedActions []string `json:"requested_actions"`
	DurationSeconds  int      `json:"duration_seconds"`
}

func (h *Handler) CreateBreakGlassSession(w http.ResponseWriter, r *http.Request) {
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.break_glass.manage to create break-glass sessions
	if !h.requirePermission(w, r, callerPrincipalID, tenantScope, "iam.break_glass.manage") {
		return
	}

	correlationID := r.Header.Get("X-Correlation-ID")

	var req createBreakGlassRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}

	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	targetPrincipal := req.PrincipalID
	if targetPrincipal == "" {
		targetPrincipal = callerPrincipalID
	}

	// Scenario A16: Break-glass without declared incident is strictly prohibited
	if req.IncidentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_field",
			"field":   "incident_id",
			"message": "break-glass requires a declared incident reference (Scenario A16)",
		})
		return
	}

	if req.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "reason"})
		return
	}
	if len(req.RequestedActions) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "requested_actions"})
		return
	}

	bg, err := h.store.CreateBreakGlassSession(r.Context(), domain.CreateBreakGlassSessionParams{
		TenantID:         tenantScope,
		PrincipalID:      targetPrincipal,
		IncidentID:       req.IncidentID,
		Reason:           req.Reason,
		RequestedActions: req.RequestedActions,
		DurationSeconds:  req.DurationSeconds,
	})
	if err != nil {
		if errors.Is(err, domain.ErrBreakGlassIncidentRequired) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "missing_incident_id",
				"message": err.Error(),
			})
			return
		}
		h.log.Error("CreateBreakGlassSession: store failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	_ = h.publisher.PublishBreakGlassStarted(r.Context(), *bg)

	h.log.Warn("break-glass emergency session created",
		zap.String("session_id", bg.SessionID),
		zap.String("principal_id", bg.PrincipalID),
		zap.String("incident_id", bg.IncidentID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, bg)
}

func (h *Handler) GetBreakGlassSession(w http.ResponseWriter, r *http.Request) {
	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_session_id"})
		return
	}

	bg, err := h.store.FindBreakGlassSessionByID(r.Context(), sessionID, tenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrBreakGlassSessionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "break_glass_session_not_found", "session_id": sessionID})
			return
		}
		h.log.Error("GetBreakGlassSession: store failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	// The session's own principal, or whoever administers sessions; anybody
	// else gets 404 — the session's existence is not theirs to learn.
	if bg.PrincipalID != callerPrincipal {
		allowed, err := h.holdsPermission(r, callerPrincipal, tenantScope, "iam.break_glass.manage")
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if !allowed {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "break_glass_session_not_found", "session_id": sessionID})
			return
		}
	}

	writeJSON(w, http.StatusOK, bg)
}

func (h *Handler) ListBreakGlassSessions(w http.ResponseWriter, r *http.Request) {
	principalID := r.URL.Query().Get("principal_id")
	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Your own sessions are yours to see; anybody else's — or the whole
	// register — needs the permission that administers them.
	if principalID != callerPrincipal && !h.requirePermission(w, r, callerPrincipal, tenantScope, "iam.break_glass.manage") {
		return
	}
	activeOnly := r.URL.Query().Get("active_only") == "true"

	sessions, err := h.store.ListBreakGlassSessions(r.Context(), tenantScope, principalID, activeOnly)
	if err != nil {
		h.log.Error("ListBreakGlassSessions: store failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (h *Handler) RevokeBreakGlassSession(w http.ResponseWriter, r *http.Request) {
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.break_glass.manage to revoke break-glass sessions
	if !h.requirePermission(w, r, callerPrincipalID, tenantScope, "iam.break_glass.manage") {
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_session_id"})
		return
	}

	bg, err := h.store.RevokeBreakGlassSession(r.Context(), sessionID, tenantScope, callerPrincipalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrBreakGlassSessionNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "break_glass_session_not_found", "session_id": sessionID})
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition", "message": err.Error()})
		default:
			h.log.Error("RevokeBreakGlassSession: store failed", zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	_ = h.publisher.PublishBreakGlassEnded(r.Context(), *bg)
	writeJSON(w, http.StatusOK, bg)
}

// ── Tenant Support Session Handlers (ZS-IAM-001 §15 & §21) ──────────────────

type createSupportSessionRequest struct {
	TenantID              string   `json:"tenant_id,omitempty"`
	SupportOperatorID     string   `json:"support_operator_id,omitempty"`
	TicketRef             string   `json:"ticket_ref"`
	Purpose               string   `json:"purpose"`
	ReadOnly              *bool    `json:"read_only,omitempty"`
	AllowBulkExport       bool     `json:"allow_bulk_export"`
	AllowedActions        []string `json:"allowed_actions"`
	DurationSeconds       int      `json:"duration_seconds"`
	TenantConsentObtained *bool    `json:"tenant_consent_obtained,omitempty"`
}

func (h *Handler) CreateSupportSession(w http.ResponseWriter, r *http.Request) {
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.support.manage to create support sessions
	if !h.requirePermission(w, r, callerPrincipalID, tenantScope, "iam.support.manage") {
		return
	}

	correlationID := r.Header.Get("X-Correlation-ID")

	var req createSupportSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}

	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	targetOperator := req.SupportOperatorID
	if targetOperator == "" {
		targetOperator = callerPrincipalID
	}

	if req.TicketRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "ticket_ref"})
		return
	}
	if req.Purpose == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "purpose"})
		return
	}

	readOnly := true
	if req.ReadOnly != nil {
		readOnly = *req.ReadOnly
	}

	consent := true
	if req.TenantConsentObtained != nil {
		consent = *req.TenantConsentObtained
	}

	ss, err := h.store.CreateSupportSession(r.Context(), domain.CreateSupportSessionParams{
		TenantID:              tenantScope,
		SupportOperatorID:     targetOperator,
		TicketRef:             req.TicketRef,
		Purpose:               req.Purpose,
		ReadOnly:              readOnly,
		AllowBulkExport:       req.AllowBulkExport,
		AllowedActions:        req.AllowedActions,
		DurationSeconds:       req.DurationSeconds,
		TenantConsentObtained: consent,
	})
	if err != nil {
		h.log.Error("CreateSupportSession: store failed", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	_ = h.publisher.PublishSupportSessionStarted(r.Context(), *ss)

	h.log.Info("support session created",
		zap.String("session_id", ss.SessionID),
		zap.String("operator_id", ss.SupportOperatorID),
		zap.String("ticket_ref", ss.TicketRef),
		zap.Bool("read_only", ss.ReadOnly),
		zap.Bool("allow_bulk_export", ss.AllowBulkExport),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, ss)
}

func (h *Handler) GetSupportSession(w http.ResponseWriter, r *http.Request) {
	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_session_id"})
		return
	}

	ss, err := h.store.FindSupportSessionByID(r.Context(), sessionID, tenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrSupportSessionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "support_session_not_found", "session_id": sessionID})
			return
		}
		h.log.Error("GetSupportSession: store failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	// The session's own principal, or whoever administers sessions; anybody
	// else gets 404 — the session's existence is not theirs to learn.
	if ss.SupportOperatorID != callerPrincipal {
		allowed, err := h.holdsPermission(r, callerPrincipal, tenantScope, "iam.support.manage")
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if !allowed {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "support_session_not_found", "session_id": sessionID})
			return
		}
	}

	writeJSON(w, http.StatusOK, ss)
}

func (h *Handler) ListSupportSessions(w http.ResponseWriter, r *http.Request) {
	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// ZS-IAM-001 §21 "no broad IAM discovery beyond administrable scope":
	// the register is readable with the Appendix A read permission.
	if !h.requirePermission(w, r, callerPrincipal, tenantScope, "iam.support.manage") {
		return
	}

	activeOnly := r.URL.Query().Get("active_only") == "true"

	sessions, err := h.store.ListSupportSessions(r.Context(), tenantScope, activeOnly)
	if err != nil {
		h.log.Error("ListSupportSessions: store failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (h *Handler) RevokeSupportSession(w http.ResponseWriter, r *http.Request) {
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Require iam.support.manage to revoke support sessions
	if !h.requirePermission(w, r, callerPrincipalID, tenantScope, "iam.support.manage") {
		return
	}

	sessionID := chi.URLParam(r, "session_id")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_session_id"})
		return
	}

	ss, err := h.store.RevokeSupportSession(r.Context(), sessionID, tenantScope, callerPrincipalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSupportSessionNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "support_session_not_found", "session_id": sessionID})
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition", "message": err.Error()})
		default:
			h.log.Error("RevokeSupportSession: store failed", zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	_ = h.publisher.PublishSupportSessionEnded(r.Context(), *ss)
	writeJSON(w, http.StatusOK, ss)
}
