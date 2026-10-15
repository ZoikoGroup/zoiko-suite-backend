package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"zoiko.io/access-control-svc/internal/clients"
	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/telemetry"
)

// Group subjects (Authorization Standard §2, §9; audit gap S9-C1).
//
// §9's access assignment is "subject/GROUP + role/policy + scope + effective
// dates"; §2's group "may create candidate assignments but never bypass
// policy". authorization-svc evaluates principals, never groups, so a group
// assignment is a template this service FANS OUT: one governed assignment
// request per live member, through submitAssignment — the same pending check,
// principal-aware SoD check, risk-tiered approval and provisioning an
// individual request gets. A protected role reached through a group therefore
// enters the required approval path per member (A20), and a member whose
// existing access conflicts with the role is refused alone, without blocking
// the rest of the group.
//
//	join   a new member is fanned out to every ACTIVE group assignment;
//	leave  their group-sourced requests are ended: pending ones cancelled,
//	       provisioned ones revoked there and here;
//	revoke a group assignment ends every member's request, then closes it.
//
// Every command runs through begin (ROLE_MANAGE on the body's entity), bound
// to the group's own entity, exactly as the assignment commands are.

// GroupStore holds groups, memberships and group assignments (000014).
type GroupStore interface {
	CreateGroup(ctx context.Context, g *domain.Group) (bool, error)
	GetGroup(ctx context.Context, groupID string) (*domain.Group, error)
	ListGroups(ctx context.Context) ([]domain.Group, error)
	AddGroupMember(ctx context.Context, groupID, principalID, actorID string) (*domain.GroupMember, error)
	RemoveGroupMember(ctx context.Context, groupID, principalID, actorID, reason string) error
	FindGroupAssignmentByCorrelation(ctx context.Context, correlationID string) (*domain.GroupAssignment, error)
	CreateGroupAssignment(ctx context.Context, a *domain.GroupAssignment) (bool, error)
	GetGroupAssignment(ctx context.Context, groupAssignmentID string) (*domain.GroupAssignment, error)
	ListGroupAssignments(ctx context.Context, groupID string, activeOnly bool) ([]domain.GroupAssignment, error)
	MarkGroupAssignmentRevoked(ctx context.Context, groupAssignmentID, actorID, reason string) (*domain.GroupAssignment, error)
	ListGroupAssignmentRequests(ctx context.Context, groupAssignmentID, principalID string) ([]domain.AssignmentRequest, error)
}

// SetGroupStore enables the group routes.
func (g *Gov) SetGroupStore(s GroupStore) {
	g.groups = s
}

func registerGroupRoutes(r chi.Router, g *Gov) {
	r.Route("/v1/iam/groups", func(r chi.Router) {
		r.Post("/", g.CreateGroup)
		r.Get("/", g.ListGroups)
		r.Get("/{group_id}", g.GetGroup)
		r.Post("/{group_id}/members", g.AddGroupMember)
		r.Post("/{group_id}/members/{principal_id}:remove", g.RemoveGroupMember)
		r.Post("/{group_id}/members/{principal_id}/remove", g.RemoveGroupMember)
		r.Post("/{group_id}/assignments", g.AssignGroup)
		r.Get("/{group_id}/assignments", g.ListGroupAssignments)
	})
	r.Route("/v1/iam/group-assignments", func(r chi.Router) {
		r.Get("/{group_assignment_id}", g.GetGroupAssignment)
		for _, verb := range []string{"revoke", "sync"} {
			hf := g.groupAssignmentCommand(verb)
			r.Post("/{group_assignment_id}:"+verb, hf)
			r.Post("/{group_assignment_id}/"+verb, hf)
		}
	})
}

// memberCorrelation is the correlation id of one member's share of a group
// assignment: deterministic, so a re-run fan-out (sync, a retried join)
// replays instead of doubling, and keyed on the membership's start, so a
// member who leaves and re-joins gets a new request, not the revoked one.
func memberCorrelation(groupAssignmentID string, m domain.GroupMember) string {
	key := groupAssignmentID + "|" + m.PrincipalID + "|" + m.AddedAt.UTC().Format(time.RFC3339Nano)
	return "grp-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

// CreateGroup handles POST /v1/iam/groups.
func (g *Gov) CreateGroup(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovCreateGroup
	var req domain.CreateGroupRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	if req.LegalEntityID == "" || strings.TrimSpace(req.GroupCode) == "" || strings.TrimSpace(req.GroupName) == "" || req.CorrelationID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, group_code, group_name and correlation_id are required", req)
		return
	}
	if req.Source == "" {
		req.Source = domain.GroupManual
	}
	if req.Source != domain.GroupManual && req.Source != domain.GroupSCIM {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_source", "source must be MANUAL or SCIM", req)
		return
	}
	principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
	if !ok {
		return
	}
	now := time.Now().UTC()
	grp := &domain.Group{
		GroupID: uuid.NewString(), TenantID: tenantID, LegalEntityID: req.LegalEntityID,
		GroupCode: strings.TrimSpace(req.GroupCode), GroupName: strings.TrimSpace(req.GroupName),
		Status: domain.GroupActive, Source: req.Source, CreatedByPrincipalID: principalID,
		CorrelationID: req.CorrelationID, CreatedAt: now, UpdatedAt: now,
	}
	created, err := g.groups.CreateGroup(r.Context(), grp)
	if err != nil {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
		return
	}
	if !created {
		g.count(op, telemetry.WriteReplayed)
		writeJSON(w, http.StatusOK, grp)
		return
	}
	g.count(op, telemetry.WriteCreated)
	writeJSON(w, http.StatusCreated, grp)
}

func (g *Gov) ListGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	list, err := g.groups.ListGroups(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (g *Gov) GetGroup(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	grp, err := g.groups.GetGroup(r.Context(), chi.URLParam(r, "group_id"))
	if err != nil {
		status, code := classify(err)
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, grp)
}

func (g *Gov) ListGroupAssignments(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	list, err := g.groups.ListGroupAssignments(r.Context(), chi.URLParam(r, "group_id"), false)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// GetGroupAssignment answers with the member requests it fanned out to.
func (g *Gov) GetGroupAssignment(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	ga, err := g.groups.GetGroupAssignment(r.Context(), chi.URLParam(r, "group_assignment_id"))
	if err != nil {
		status, code := classify(err)
		writeError(w, status, code, err.Error())
		return
	}
	reqs, err := g.groups.ListGroupAssignmentRequests(r.Context(), ga.GroupAssignmentID, "")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	for _, a := range reqs {
		ga.Members = append(ga.Members, domain.GroupMemberOutcome{PrincipalID: a.TargetPrincipalID, RequestID: a.RequestID, Status: a.Status})
	}
	writeJSON(w, http.StatusOK, ga)
}

// groupFor loads the group a command names and binds the caller's ROLE_MANAGE
// (checked by begin on the body's entity) to the group's own entity, so a
// manager of one entity cannot administer another entity's group.
func (g *Gov) groupFor(ctx context.Context, groupID, bodyEntity string) (*domain.Group, error) {
	grp, err := g.groups.GetGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if grp.LegalEntityID != bodyEntity {
		return nil, domain.ErrEntityMismatch
	}
	return grp, nil
}

// AssignGroup handles POST /v1/iam/groups/{group_id}/assignments: records the
// group assignment and fans it out to every live member.
//
//	201  recorded; members carries each member's request, or refusal.
//	200  a replay of this correlation_id.
func (g *Gov) AssignGroup(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovAssignGroup
	var req domain.CreateGroupAssignmentRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	if req.LegalEntityID == "" || req.RoleDefinitionID == "" || strings.TrimSpace(req.Justification) == "" || req.CorrelationID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, role_definition_id, justification and correlation_id are required", req)
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
	if prior, err := g.groups.FindGroupAssignmentByCorrelation(r.Context(), req.CorrelationID); err != nil {
		fail(err)
		return
	} else if prior != nil {
		g.count(op, telemetry.WriteReplayed)
		writeJSON(w, http.StatusOK, prior)
		return
	}
	grp, err := g.groupFor(r.Context(), chi.URLParam(r, "group_id"), req.LegalEntityID)
	if err != nil {
		fail(err)
		return
	}
	if grp.Status != domain.GroupActive {
		fail(domain.ErrGroupRetired)
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
	now := time.Now().UTC()
	effective := req.EffectiveFrom.UTC()
	if req.EffectiveFrom.IsZero() {
		effective = now
	}
	var end *time.Time
	if req.EffectiveTo != nil {
		e := req.EffectiveTo.UTC()
		end = &e
	}
	ga := &domain.GroupAssignment{
		GroupAssignmentID: uuid.NewString(), TenantID: tenantID, GroupID: grp.GroupID, RoleDefinitionID: role.RoleDefinitionID,
		LegalEntityID: grp.LegalEntityID, EffectiveFrom: effective, EffectiveTo: end, Justification: req.Justification,
		Status: domain.GroupAssignmentActive, CreatedByPrincipalID: principalID, CorrelationID: req.CorrelationID, CreatedAt: now,
	}
	created, err := g.groups.CreateGroupAssignment(r.Context(), ga)
	if err != nil {
		fail(err)
		return
	}
	if !created {
		g.count(op, telemetry.WriteReplayed)
		writeJSON(w, http.StatusOK, ga)
		return
	}
	for _, m := range grp.Members {
		ga.Members = append(ga.Members, g.fanOutMember(r.Context(), tenantID, principalID, role, ga, m))
	}
	g.count(op, telemetry.WriteCreated)
	writeJSON(w, http.StatusCreated, ga)
}

// fanOutMember submits one member's share of a group assignment. A refusal
// (SoD conflict, authorization-svc down, an identical request pending) is
// recorded as refused-command evidence and returned in the outcome; it does
// not stop the other members.
func (g *Gov) fanOutMember(ctx context.Context, tenantID, principalID string, role *domain.RoleDefinition, ga *domain.GroupAssignment, m domain.GroupMember) domain.GroupMemberOutcome {
	out := domain.GroupMemberOutcome{PrincipalID: m.PrincipalID}
	corr := memberCorrelation(ga.GroupAssignmentID, m)
	refused := func(err error) domain.GroupMemberOutcome {
		_, code := classify(err)
		out.Error = code
		g.h.recordRefusal(ctx, tenantID, ga.LegalEntityID, principalID, corr, strings.ToUpper(telemetry.GovAssignGroup), code, code, err.Error(),
			map[string]string{"group_assignment_id": ga.GroupAssignmentID, "target_principal_id": m.PrincipalID})
		return out
	}
	if prior, err := g.store.FindAssignmentByCorrelation(ctx, corr); err != nil {
		return refused(err)
	} else if prior != nil {
		out.RequestID, out.Status = prior.RequestID, prior.Status
		return out
	}
	if ga.EffectiveTo != nil && !ga.EffectiveTo.After(time.Now()) {
		return refused(domain.ErrAssignmentWindowOver)
	}
	from := ga.EffectiveFrom
	if from.Before(time.Now()) {
		from = time.Time{}
	}
	a, _, err := g.submitAssignment(ctx, tenantID, principalID, role, assignmentSpec{
		target: m.PrincipalID, legalEntityID: ga.LegalEntityID, correlationID: corr, groupAssignmentID: ga.GroupAssignmentID,
		justification: ga.Justification, from: from, to: ga.EffectiveTo,
	})
	if err != nil {
		return refused(err)
	}
	out.RequestID, out.Status = a.RequestID, a.Status
	return out
}

// memberJoined is the answer to a member add: the membership, and what each
// ACTIVE group assignment did for the new member.
type memberJoined struct {
	Member      domain.GroupMember        `json:"member"`
	Assignments []groupAssignmentOutcomes `json:"assignments"`
}

type groupAssignmentOutcomes struct {
	GroupAssignmentID string                    `json:"group_assignment_id"`
	Outcome           domain.GroupMemberOutcome `json:"outcome"`
}

// AddGroupMember handles POST /v1/iam/groups/{group_id}/members: opens the
// membership and fans every ACTIVE group assignment out to the new member.
func (g *Gov) AddGroupMember(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovAddGroupMember
	var req domain.GroupMemberRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	if req.LegalEntityID == "" || req.PrincipalID == "" || req.CorrelationID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, principal_id and correlation_id are required", req)
		return
	}
	principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
	if !ok {
		return
	}
	fail := func(err error) {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
	}
	grp, err := g.groupFor(r.Context(), chi.URLParam(r, "group_id"), req.LegalEntityID)
	if err != nil {
		fail(err)
		return
	}
	gas, err := g.groups.ListGroupAssignments(r.Context(), grp.GroupID, true)
	if err != nil {
		fail(err)
		return
	}
	m, err := g.groups.AddGroupMember(r.Context(), grp.GroupID, req.PrincipalID, principalID)
	if err != nil {
		fail(err)
		return
	}
	out := memberJoined{Member: *m, Assignments: []groupAssignmentOutcomes{}}
	for i := range gas {
		ga := &gas[i]
		o := domain.GroupMemberOutcome{PrincipalID: m.PrincipalID}
		role, err := g.h.store.GetRole(r.Context(), ga.RoleDefinitionID)
		switch {
		case err != nil:
			_, o.Error = classify(err)
		case role.Status != domain.RoleStatusActive:
			o.Error = "role_retired"
		default:
			o = g.fanOutMember(r.Context(), tenantID, principalID, role, ga, *m)
		}
		out.Assignments = append(out.Assignments, groupAssignmentOutcomes{GroupAssignmentID: ga.GroupAssignmentID, Outcome: o})
	}
	g.count(op, telemetry.WriteCreated)
	writeJSON(w, http.StatusCreated, out)
}

// RemoveGroupMember handles POST /v1/iam/groups/{group_id}/members/{principal_id}:remove.
// Ends the member's group-sourced requests first and closes the membership
// only when all of them ended, so a failure leaves a state the same call can
// finish rather than a former member still holding the group's roles.
func (g *Gov) RemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovRemoveGroupMember
	var req domain.GroupMemberRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	if req.LegalEntityID == "" || strings.TrimSpace(req.Reason) == "" || req.CorrelationID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, reason and correlation_id are required", req)
		return
	}
	principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
	if !ok {
		return
	}
	fail := func(err error) {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
	}
	member := chi.URLParam(r, "principal_id")
	grp, err := g.groupFor(r.Context(), chi.URLParam(r, "group_id"), req.LegalEntityID)
	if err != nil {
		fail(err)
		return
	}
	isMember := false
	for _, m := range grp.Members {
		isMember = isMember || m.PrincipalID == member
	}
	if !isMember {
		fail(domain.ErrGroupMemberNotFound)
		return
	}
	gas, err := g.groups.ListGroupAssignments(r.Context(), grp.GroupID, true)
	if err != nil {
		fail(err)
		return
	}
	var ended []domain.GroupMemberOutcome
	for i := range gas {
		outs, err := g.endMemberRequests(r.Context(), tenantID, principalID, req.CorrelationID, &gas[i], member, "GROUP_MEMBERSHIP_ENDED", req.Reason)
		ended = append(ended, outs...)
		if err != nil {
			fail(err)
			return
		}
	}
	if err := g.groups.RemoveGroupMember(r.Context(), grp.GroupID, member, principalID, req.Reason); err != nil {
		fail(err)
		return
	}
	if ended == nil {
		ended = []domain.GroupMemberOutcome{}
	}
	g.count(op, telemetry.WriteUpdated)
	writeJSON(w, http.StatusOK, map[string]any{"group_id": grp.GroupID, "principal_id": member, "ended": ended})
}

// endMemberRequests ends the requests a group assignment fanned out to (one
// member's, or every member's when member is empty): a pending one is
// cancelled, a provisioned one revoked in authorization-svc and then here.
// Stops at the first failure; whatever ended stays ended and a retry skips it.
func (g *Gov) endMemberRequests(ctx context.Context, tenantID, principalID, correlationID string, ga *domain.GroupAssignment, member, reasonCode, reason string) ([]domain.GroupMemberOutcome, error) {
	reqs, err := g.groups.ListGroupAssignmentRequests(ctx, ga.GroupAssignmentID, member)
	if err != nil {
		return nil, err
	}
	var out []domain.GroupMemberOutcome
	for _, a := range reqs {
		var done *domain.AssignmentRequest
		switch a.Status {
		case domain.AssignmentPendingApproval:
			done, err = g.store.DecideAssignmentRequest(ctx, a.RequestID, domain.AssignmentCancelled, principalID, reason, "")
		case domain.AssignmentProvisioned:
			scope := clients.Scope{PrincipalID: principalID, TenantID: tenantID, LegalEntityID: a.LegalEntityID, CorrelationID: correlationID,
				ReasonCode: reasonCode, Purpose: reason, ApprovalReference: a.RequestID}
			if rerr := g.admin.RevokeRoleAssignment(ctx, a.AuthzAssignmentID, scope); rerr != nil && !errors.Is(rerr, domain.ErrAuthzAssignmentAbsent) {
				g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminRevokeAssignment, adminOutcome(rerr)).Inc()
				return out, rerr
			}
			g.h.metrics.AuthzAdminCalls.WithLabelValues(telemetry.AdminRevokeAssignment, telemetry.AdminOK).Inc()
			done, err = g.store.RevokeAssignmentRequest(ctx, a.RequestID, principalID, reason)
		default:
			continue
		}
		if err != nil {
			return out, err
		}
		out = append(out, domain.GroupMemberOutcome{PrincipalID: done.TargetPrincipalID, RequestID: done.RequestID, Status: done.Status})
	}
	return out, nil
}

// groupAssignmentCommand serves :revoke (end every member's request, then
// close the group assignment) and :sync (re-run the fan-out for every live
// member; idempotent, it fills in members a racing join or an outage missed).
func (g *Gov) groupAssignmentCommand(verb string) http.HandlerFunc {
	op := map[string]string{"revoke": telemetry.GovRevokeGroupAssign, "sync": telemetry.GovAssignGroup}[verb]
	return func(w http.ResponseWriter, r *http.Request) {
		var req domain.AssignmentDecisionRequest
		if err := decode(r, &req); err != nil {
			g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
			return
		}
		if req.LegalEntityID == "" || req.CorrelationID == "" {
			g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields", "legal_entity_id and correlation_id are required", req)
			return
		}
		if verb == "revoke" && strings.TrimSpace(req.Reason) == "" {
			g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_reason", "a reason is required to revoke a group assignment", req)
			return
		}
		principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
		if !ok {
			return
		}
		fail := func(err error) {
			g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
		}
		ga, err := g.groups.GetGroupAssignment(r.Context(), chi.URLParam(r, "group_assignment_id"))
		if err != nil {
			fail(err)
			return
		}
		if ga.LegalEntityID != req.LegalEntityID {
			fail(domain.ErrEntityMismatch)
			return
		}
		if ga.Status != domain.GroupAssignmentActive {
			fail(domain.ErrGroupAssignmentRevoked)
			return
		}

		if verb == "revoke" {
			ended, err := g.endMemberRequests(r.Context(), tenantID, principalID, req.CorrelationID, ga, "", "GROUP_ASSIGNMENT_REVOKED", req.Reason)
			if err != nil {
				fail(err)
				return
			}
			out, err := g.groups.MarkGroupAssignmentRevoked(r.Context(), ga.GroupAssignmentID, principalID, req.Reason)
			if err != nil {
				fail(err)
				return
			}
			out.Members = ended
			g.count(op, telemetry.WriteUpdated)
			writeJSON(w, http.StatusOK, out)
			return
		}

		grp, err := g.groups.GetGroup(r.Context(), ga.GroupID)
		if err != nil {
			fail(err)
			return
		}
		role, err := g.h.store.GetRole(r.Context(), ga.RoleDefinitionID)
		if err != nil {
			fail(err)
			return
		}
		if role.Status != domain.RoleStatusActive {
			g.refuse(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, http.StatusConflict, "role_retired", string(domain.ErrRoleRetired), req)
			return
		}
		for _, m := range grp.Members {
			ga.Members = append(ga.Members, g.fanOutMember(r.Context(), tenantID, principalID, role, ga, m))
		}
		g.count(op, telemetry.WriteUpdated)
		writeJSON(w, http.StatusOK, ga)
	}
}
