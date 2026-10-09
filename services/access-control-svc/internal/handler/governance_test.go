package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/clients"
	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/handler"
	"zoiko.io/access-control-svc/internal/middleware"
	"zoiko.io/access-control-svc/internal/telemetry"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubGovStore struct {
	base      *stubStore
	templates map[string]*domain.RoleTemplate
	versions  map[string][]domain.RoleTemplateVersion

	assignments map[string]*domain.AssignmentRequest
	campaigns   map[string]*domain.ReviewCampaign
	events      []string
}

func newStubGovStore(base *stubStore) *stubGovStore {
	return &stubGovStore{
		base: base,
		templates: map[string]*domain.RoleTemplate{
			"TREASURY_PREPARER":        {TemplateCode: "TREASURY_PREPARER", TemplateName: "Treasury Preparer", RoleScopeType: "LEGAL_ENTITY", Status: "ACTIVE", IsArchetype: true, LatestVersion: 2},
			"IAM_ACCESS_ADMINISTRATOR": {TemplateCode: "IAM_ACCESS_ADMINISTRATOR", TemplateName: "IAM Access Administrator", RoleScopeType: "TENANT", Status: "ACTIVE", IsArchetype: true, LatestVersion: 1},
		},
		versions: map[string][]domain.RoleTemplateVersion{
			"TREASURY_PREPARER": {
				{TemplateCode: "TREASURY_PREPARER", TemplateVersion: 1, PermittedActions: []string{"payment.create", "payment.prepare"}},
				{TemplateCode: "TREASURY_PREPARER", TemplateVersion: 2, PermittedActions: []string{"payment.create", "payment.prepare", "payment.submit"}},
			},
			"IAM_ACCESS_ADMINISTRATOR": {
				{TemplateCode: "IAM_ACCESS_ADMINISTRATOR", TemplateVersion: 1, PermittedActions: []string{"iam.assignment.list", "iam.assignment.grant"}},
			},
		},
		assignments: map[string]*domain.AssignmentRequest{},
		campaigns:   map[string]*domain.ReviewCampaign{},
	}
}

func (s *stubGovStore) ListTemplates(context.Context) ([]domain.RoleTemplate, error) {
	var out []domain.RoleTemplate
	for _, t := range s.templates {
		out = append(out, *t)
	}
	return out, nil
}
func (s *stubGovStore) GetTemplate(_ context.Context, code string) (*domain.RoleTemplate, error) {
	t, ok := s.templates[code]
	if !ok {
		return nil, domain.ErrTemplateNotFound
	}
	cp := *t
	return &cp, nil
}
func (s *stubGovStore) ListTemplateVersions(_ context.Context, code string) ([]domain.RoleTemplateVersion, error) {
	return s.versions[code], nil
}
func (s *stubGovStore) GetTemplateVersion(_ context.Context, code string, v int) (*domain.RoleTemplateVersion, error) {
	vs := s.versions[code]
	if len(vs) == 0 {
		return nil, domain.ErrTemplateVersionNotFound
	}
	if v == 0 {
		cp := vs[len(vs)-1]
		return &cp, nil
	}
	for _, x := range vs {
		if x.TemplateVersion == v {
			cp := x
			return &cp, nil
		}
	}
	return nil, domain.ErrTemplateVersionNotFound
}
func (s *stubGovStore) CreateTemplateRole(ctx context.Context, r *domain.RoleDefinition, b *domain.PermissionBundleDef, actor string) (bool, error) {
	created, err := s.base.CreateRole(ctx, r, actor)
	if err != nil || !created {
		if err == nil {
			for _, x := range s.base.bundlesByID {
				if x.RoleDefinitionID == r.RoleDefinitionID && x.TemplateCode != "" {
					*b = *x
				}
			}
		}
		return false, err
	}
	b.RoleDefinitionID = r.RoleDefinitionID
	if _, err := s.base.CreateBundle(ctx, b, actor); err != nil {
		return false, err
	}
	s.events = append(s.events, "iam.role.published")
	return true, nil
}
func (s *stubGovStore) GetTemplateBundle(_ context.Context, roleID string) (*domain.PermissionBundleDef, error) {
	for _, b := range s.base.bundlesByID {
		if b.RoleDefinitionID == roleID && b.TemplateCode != "" {
			cp := *b
			return &cp, nil
		}
	}
	return nil, domain.ErrNotTemplateRole
}
func (s *stubGovStore) UpgradeTemplateRole(_ context.Context, roleID string, version int, actions []string, _ string) (*domain.InstantiatedRole, error) {
	r := s.base.rolesByID[roleID]
	if version <= r.TemplateVersion {
		return nil, domain.ErrTemplateVersionNotNewer
	}
	r.TemplateVersion = version
	var out domain.InstantiatedRole
	for _, b := range s.base.bundlesByID {
		if b.RoleDefinitionID == roleID && b.TemplateCode != "" {
			b.PermittedActions, b.TemplateVersion = actions, version
			out.Bundle = *b
		}
	}
	out.Role = *r
	s.events = append(s.events, "iam.role.published", "role.updated")
	return &out, nil
}

func (s *stubGovStore) FindAssignmentByCorrelation(_ context.Context, cid string) (*domain.AssignmentRequest, error) {
	for _, a := range s.assignments {
		if a.CorrelationID == cid {
			cp := *a
			return &cp, nil
		}
	}
	return nil, nil
}
func (s *stubGovStore) PendingAssignmentExists(_ context.Context, target, role, entity string) (bool, error) {
	for _, a := range s.assignments {
		if a.TargetPrincipalID == target && a.RoleDefinitionID == role && a.LegalEntityID == entity && a.Status == domain.AssignmentPendingApproval {
			return true, nil
		}
	}
	return false, nil
}
func (s *stubGovStore) CreateAssignmentRequest(_ context.Context, a *domain.AssignmentRequest, _ string) (bool, error) {
	cp := *a
	s.assignments[a.RequestID] = &cp
	if a.Status == domain.AssignmentProvisioned {
		s.events = append(s.events, "iam.assignment.granted")
	} else {
		s.events = append(s.events, "iam.assignment.requested")
	}
	return true, nil
}
func (s *stubGovStore) GetAssignmentRequest(_ context.Context, id string) (*domain.AssignmentRequest, error) {
	a, ok := s.assignments[id]
	if !ok {
		return nil, domain.ErrAssignmentNotFound
	}
	cp := *a
	return &cp, nil
}
func (s *stubGovStore) ListAssignmentRequests(context.Context, domain.AssignmentListFilter) ([]domain.AssignmentRequest, error) {
	var out []domain.AssignmentRequest
	for _, a := range s.assignments {
		out = append(out, *a)
	}
	return out, nil
}
func (s *stubGovStore) DecideAssignmentRequest(_ context.Context, id, to, decider, reason, authzID string) (*domain.AssignmentRequest, error) {
	a := s.assignments[id]
	if a.Status != domain.AssignmentPendingApproval {
		return nil, domain.ErrAssignmentState
	}
	a.Status, a.DecidedByPrincipalID, a.DecisionReason = to, decider, reason
	if authzID != "" {
		a.AuthzAssignmentID = authzID
	}
	if to == domain.AssignmentProvisioned {
		s.events = append(s.events, "iam.assignment.granted")
	}
	cp := *a
	return &cp, nil
}
func (s *stubGovStore) RevokeAssignmentRequest(_ context.Context, id, by, reason string) (*domain.AssignmentRequest, error) {
	a := s.assignments[id]
	if a.Status != domain.AssignmentProvisioned {
		return nil, domain.ErrAssignmentState
	}
	a.Status, a.RevokedByPrincipalID, a.RevocationReason = domain.AssignmentRevoked, by, reason
	s.events = append(s.events, "iam.assignment.revoked")
	cp := *a
	return &cp, nil
}

func (s *stubGovStore) ScheduleAssignmentEnd(_ context.Context, id, by, reason string, at time.Time) (*domain.AssignmentRequest, error) {
	a := s.assignments[id]
	if a.Status != domain.AssignmentProvisioned {
		return nil, domain.ErrAssignmentState
	}
	end := at
	a.EffectiveTo, a.RevokedByPrincipalID, a.RevocationReason = &end, by, reason
	cp := *a
	return &cp, nil
}

func (s *stubGovStore) FindCampaignByCorrelation(_ context.Context, cid string) (*domain.ReviewCampaign, error) {
	for _, c := range s.campaigns {
		if c.CorrelationID == cid {
			return c, nil
		}
	}
	return nil, nil
}
func (s *stubGovStore) CreateCampaign(_ context.Context, c *domain.ReviewCampaign, items []domain.ReviewItem, _ string) (bool, error) {
	c.Items = items
	s.campaigns[c.CampaignID] = c
	s.events = append(s.events, "iam.access_review.started")
	return true, nil
}
func (s *stubGovStore) GetCampaign(_ context.Context, id string) (*domain.ReviewCampaign, error) {
	c, ok := s.campaigns[id]
	if !ok {
		return nil, domain.ErrCampaignNotFound
	}
	return c, nil
}
func (s *stubGovStore) ListCampaigns(context.Context, string, int, int) ([]domain.ReviewCampaign, error) {
	return nil, nil
}
func (s *stubGovStore) ListReviewItems(_ context.Context, reviewer, _ string) ([]domain.ReviewItem, error) {
	var out []domain.ReviewItem
	for _, c := range s.campaigns {
		for _, it := range c.Items {
			if it.ReviewerPrincipalID == reviewer {
				out = append(out, it)
			}
		}
	}
	return out, nil
}
func (s *stubGovStore) item(cid, iid string) *domain.ReviewItem {
	c, ok := s.campaigns[cid]
	if !ok {
		return nil
	}
	for i := range c.Items {
		if c.Items[i].ItemID == iid {
			return &c.Items[i]
		}
	}
	return nil
}
func (s *stubGovStore) GetReviewItem(_ context.Context, cid, iid string) (*domain.ReviewItem, error) {
	it := s.item(cid, iid)
	if it == nil {
		return nil, domain.ErrReviewItemNotFound
	}
	cp := *it
	return &cp, nil
}
func (s *stubGovStore) DecideReviewItem(_ context.Context, cid, iid, decision, reason, by string, revoked bool) (*domain.ReviewItem, error) {
	it := s.item(cid, iid)
	it.Decision, it.DecisionReason, it.DecidedByPrincipalID, it.RevocationApplied = decision, reason, by, revoked
	it.Status = domain.ReviewItemDecided
	if decision == domain.DecisionEscalate {
		it.Status = domain.ReviewItemEscalated
	}
	if revoked {
		s.events = append(s.events, "iam.assignment.revoked")
	}
	cp := *it
	return &cp, nil
}
func (s *stubGovStore) ReassignReviewItem(_ context.Context, cid, iid, reviewer string) (*domain.ReviewItem, error) {
	it := s.item(cid, iid)
	it.ReviewerPrincipalID, it.Status = reviewer, domain.ReviewItemOpen
	cp := *it
	return &cp, nil
}
func (s *stubGovStore) CompleteCampaign(_ context.Context, cid, by string) (*domain.ReviewCampaign, error) {
	c := s.campaigns[cid]
	for _, it := range c.Items {
		if domain.RiskRank(it.RiskTier) >= domain.RiskRank(domain.RiskHigh) && (it.Status == domain.ReviewItemOpen || it.Status == domain.ReviewItemEscalated) {
			return nil, domain.ErrUnresolvedHighRisk
		}
	}
	for i := range c.Items {
		if c.Items[i].Status == domain.ReviewItemOpen {
			c.Items[i].Status = domain.ReviewItemExpired
		}
	}
	c.Status, c.CompletedByPrincipalID = domain.CampaignCompleted, by
	return c, nil
}

type assignCall struct {
	assignmentID, principal, role, entity string
	effectiveTo                           *time.Time
	scope                                 clients.Scope
}

type scheduleCall struct {
	assignmentID string
	at           time.Time
}

type stubAssignAdmin struct {
	createErr   error
	revokeErr   error
	creates     []assignCall
	revokes     []string
	revokeScope []clients.Scope
	schedules   []scheduleCall
	scheduleErr error
	assignments map[string][]domain.AuthzAssignment // by role id
}

func (a *stubAssignAdmin) CreateRoleAssignment(_ context.Context, id, principal, role, entity string, _ time.Time, end *time.Time, s clients.Scope) (string, error) {
	a.creates = append(a.creates, assignCall{id, principal, role, entity, end, s})
	if a.createErr != nil {
		// authorization-svc returns the id with a PENDING answer, as the
		// real client does with ErrAuthzApprovalPending.
		return id, a.createErr
	}
	return id, nil
}
func (a *stubAssignAdmin) RevokeRoleAssignment(_ context.Context, id string, s clients.Scope) error {
	a.revokes = append(a.revokes, id)
	a.revokeScope = append(a.revokeScope, s)
	return a.revokeErr
}
func (a *stubAssignAdmin) ScheduleRoleAssignmentEnd(_ context.Context, id string, at time.Time, _ clients.Scope) error {
	a.schedules = append(a.schedules, scheduleCall{id, at})
	return a.scheduleErr
}
func (a *stubAssignAdmin) ListRoleAssignments(_ context.Context, role string, _ clients.Scope) ([]domain.AuthzAssignment, error) {
	return a.assignments[role], nil
}

// actionAuthZ grants every action except those in deny.
type actionAuthZ struct {
	deny  map[string]bool
	asked []string
}

func (a *actionAuthZ) CheckAllowed(_ context.Context, _, _, action string) error {
	a.asked = append(a.asked, action)
	if a.deny[action] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

type govRig struct {
	authz *actionAuthZ
	r     chi.Router
	base  *stubStore
	gov   *stubGovStore
	admin *stubAuthzAdmin
	asg   *stubAssignAdmin
	sod   *stubSoD
	tax   *stubTaxonomy
	grp   *stubGroupStore
	gv    *handler.Gov
	links *stubLinkStore
}

func newGovRig() *govRig {
	g := &govRig{
		authz: &actionAuthZ{deny: map[string]bool{}},
		base:  newStubStore(),
		admin: &stubAuthzAdmin{},
		asg:   &stubAssignAdmin{assignments: map[string][]domain.AuthzAssignment{}},
		sod:   &stubSoD{},
		tax:   &stubTaxonomy{risk: map[string]string{"payment.release": domain.RiskCritical}, protected: map[string]bool{"iam.assignment.grant": true}},
	}
	g.gov = newStubGovStore(g.base)
	g.grp = newStubGroupStore(g.gov)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	metrics := telemetry.NewDomainWith(telemetry.NewRegistry(), "access-control-svc")
	h := handler.New(g.base, g.authz, g.admin, g.sod,
		&stubProtectedActions{actions: []string{"PLATFORM_ADMIN", "iam.assignment.grant"}}, g.tax, metrics, zap.NewNop())
	handler.RegisterRoutes(r, h)
	gv := handler.NewGov(h, g.gov, g.asg)
	gv.SetServicePrincipal("svc-access-control")
	gv.SetGroupStore(g.grp)
	g.links = &stubLinkStore{links: map[string]*domain.SubjectLink{}}
	gv.SetSubjectLinks(g.links, "sec-1", 0)
	g.gv = gv
	handler.RegisterGovernanceRoutes(r, gv)
	g.r = r
	return g
}

// seedRole puts an ACTIVE role with one bundle into the base store.
func (g *govRig) seedRole(scope string, status domain.RoleStatus, actions ...string) *domain.RoleDefinition {
	role := &domain.RoleDefinition{
		RoleDefinitionID: uuid.NewString(), TenantID: "tenant-abc", RoleCode: "R_" + uuid.NewString()[:8],
		RoleName: "Role", RoleScopeType: scope, Status: status, CorrelationID: uuid.NewString(),
	}
	g.base.rolesByID[role.RoleDefinitionID] = role
	g.base.rolesByCorrelation[role.CorrelationID] = role
	if len(actions) > 0 {
		b := &domain.PermissionBundleDef{BundleID: uuid.NewString(), RoleDefinitionID: role.RoleDefinitionID,
			BundleCode: "B", PermittedActions: actions, ActiveFlag: true, CorrelationID: uuid.NewString()}
		g.base.bundlesByID[b.BundleID] = b
	}
	return role
}

func decodeBody[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func govCode(raw []byte) string {
	var m map[string]string
	_ = json.Unmarshal(raw, &m)
	return m["error_code"]
}

// ── taxonomy on the existing bundle paths ───────────────────────────────────

func TestCreateBundle_UnregisteredActionRefused(t *testing.T) {
	g := newGovRig()
	g.tax.unknown = map[string]bool{"payment.relase": true}
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive)
	rr := doReq(g.r, http.MethodPost, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles", map[string]any{
		"legal_entity_id": "le-1", "bundle_code": "X", "permitted_actions": []string{"payment.create", "payment.relase"}, "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusBadRequest || govCode(rr.Body.Bytes()) != "unknown_permission" {
		t.Fatalf("want 400 unknown_permission, got %d %s", rr.Code, rr.Body.String())
	}
	if len(g.admin.gotScopes) != 0 {
		t.Fatal("an unregistered action must not reach authorization-svc")
	}
	if n := len(g.base.refusals); n == 0 || g.base.refusals[n-1].ErrorCode != "unknown_permission" {
		t.Fatalf("refusal not recorded: %+v", g.base.refusals)
	}
}

func TestCreateBundle_TaxonomyUnavailableFailsClosed(t *testing.T) {
	g := newGovRig()
	g.tax.err = fmt.Errorf("connection refused")
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive)
	rr := doReq(g.r, http.MethodPost, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles", map[string]any{
		"legal_entity_id": "le-1", "bundle_code": "X", "permitted_actions": []string{"payment.create"}, "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusServiceUnavailable || govCode(rr.Body.Bytes()) != "taxonomy_unavailable" {
		t.Fatalf("want 503 taxonomy_unavailable, got %d %s", rr.Code, rr.Body.String())
	}
}

// ── templates ───────────────────────────────────────────────────────────────

func instantiate(g *govRig, code, cid string) (int, []byte) {
	rr := doReq(g.r, http.MethodPost, "/v1/role-templates/"+code+"/instantiate", map[string]any{
		"legal_entity_id": "le-1", "correlation_id": cid,
	}, "admin-1")
	return rr.Code, rr.Body.Bytes()
}

func TestInstantiateTemplate_CreatesRoleAndManagedBundle(t *testing.T) {
	g := newGovRig()
	code, raw := instantiate(g, "TREASURY_PREPARER", uuid.NewString())
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d %s", code, raw)
	}
	out := decodeBody[domain.InstantiatedRole](t, raw)
	if out.Role.TemplateCode != "TREASURY_PREPARER" || out.Role.TemplateVersion != 2 {
		t.Fatalf("provenance not recorded: %+v", out.Role)
	}
	if out.Bundle.BundleCode != handler.TemplateBundleCode || len(out.Bundle.PermittedActions) != 3 {
		t.Fatalf("managed bundle wrong: %+v", out.Bundle)
	}
	if len(g.admin.gotScopes) != 2 {
		t.Fatalf("role and bundle must both be provisioned, got %d calls", len(g.admin.gotScopes))
	}
}

func TestInstantiateTemplate_PinnedVersion(t *testing.T) {
	g := newGovRig()
	rr := doReq(g.r, http.MethodPost, "/v1/role-templates/TREASURY_PREPARER/instantiate", map[string]any{
		"legal_entity_id": "le-1", "correlation_id": uuid.NewString(), "template_version": 1,
	}, "admin-1")
	out := decodeBody[domain.InstantiatedRole](t, rr.Body.Bytes())
	if rr.Code != http.StatusCreated || out.Role.TemplateVersion != 1 || len(out.Bundle.PermittedActions) != 2 {
		t.Fatalf("want v1, got %d %+v", rr.Code, out)
	}
}

func TestInstantiateTemplate_ProtectedActionsAllowedForTemplates(t *testing.T) {
	g := newGovRig()
	code, raw := instantiate(g, "IAM_ACCESS_ADMINISTRATOR", uuid.NewString())
	if code != http.StatusCreated {
		t.Fatalf("a ZoikoSuite-maintained template may carry iam.assignment.grant; got %d %s", code, raw)
	}
}

func TestInstantiateTemplate_SoDConflictRefused(t *testing.T) {
	g := newGovRig()
	g.sod.err = &domain.SoDConflictError{Conflicts: []domain.SoDConflict{{CandidateAction: "payment.prepare", ConflictsWith: "payment.release"}}}
	code, raw := instantiate(g, "TREASURY_PREPARER", uuid.NewString())
	if code != http.StatusForbidden || govCode(raw) != "sod_conflict" {
		t.Fatalf("want 403 sod_conflict, got %d %s", code, raw)
	}
	if len(g.admin.gotScopes) != 0 {
		t.Fatal("nothing may be provisioned for a conflicted template")
	}
}

func TestInstantiateTemplate_UnknownTemplate404(t *testing.T) {
	g := newGovRig()
	if code, raw := instantiate(g, "NOPE", uuid.NewString()); code != http.StatusNotFound {
		t.Fatalf("want 404, got %d %s", code, raw)
	}
}

func TestInstantiateTemplate_ReplayIs200(t *testing.T) {
	g := newGovRig()
	cid := uuid.NewString()
	if code, raw := instantiate(g, "TREASURY_PREPARER", cid); code != http.StatusCreated {
		t.Fatalf("first: %d %s", code, raw)
	}
	if code, raw := instantiate(g, "TREASURY_PREPARER", cid); code != http.StatusOK {
		t.Fatalf("replay: want 200, got %d %s", code, raw)
	}
}

func TestTemplateManagedBundle_ActionsCannotBeEdited(t *testing.T) {
	g := newGovRig()
	_, raw := instantiate(g, "TREASURY_PREPARER", uuid.NewString())
	out := decodeBody[domain.InstantiatedRole](t, raw)
	rr := doReq(g.r, http.MethodPatch, "/v1/role-definitions/"+out.Role.RoleDefinitionID+"/permission-bundles/"+out.Bundle.BundleID, map[string]any{
		"legal_entity_id": "le-1", "permitted_actions": []string{"payment.create", "payment.release"}, "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusConflict || govCode(rr.Body.Bytes()) != "template_managed_bundle" {
		t.Fatalf("want 409 template_managed_bundle, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestTemplateUpgrade(t *testing.T) {
	g := newGovRig()
	rr := doReq(g.r, http.MethodPost, "/v1/role-templates/TREASURY_PREPARER/instantiate", map[string]any{
		"legal_entity_id": "le-1", "correlation_id": uuid.NewString(), "template_version": 1,
	}, "admin-1")
	out := decodeBody[domain.InstantiatedRole](t, rr.Body.Bytes())

	up := func(v int) (int, []byte) {
		rr := doReq(g.r, http.MethodPost, "/v1/role-templates/TREASURY_PREPARER/upgrade", map[string]any{
			"legal_entity_id": "le-1", "correlation_id": uuid.NewString(), "role_definition_id": out.Role.RoleDefinitionID, "template_version": v,
		}, "admin-1")
		return rr.Code, rr.Body.Bytes()
	}
	if code, raw := up(1); code != http.StatusConflict || govCode(raw) != "template_version_not_newer" {
		t.Fatalf("same version: want 409, got %d %s", code, raw)
	}
	code, raw := up(2)
	if code != http.StatusOK {
		t.Fatalf("upgrade: want 200, got %d %s", code, raw)
	}
	got := decodeBody[domain.InstantiatedRole](t, raw)
	if got.Role.TemplateVersion != 2 || len(got.Bundle.PermittedActions) != 3 {
		t.Fatalf("not upgraded: %+v", got)
	}
	// The SoD candidate set for the upgrade excludes the bundle being replaced.
	last := g.sod.reqs[len(g.sod.reqs)-1]
	if len(last.CandidateActions) != 3 {
		t.Fatalf("SoD candidate set = %v, want the 3 new actions", last.CandidateActions)
	}
}

func TestTemplateUpgrade_CustomRoleRefused(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.create")
	rr := doReq(g.r, http.MethodPost, "/v1/role-templates/TREASURY_PREPARER/upgrade", map[string]any{
		"legal_entity_id": "le-1", "correlation_id": uuid.NewString(), "role_definition_id": role.RoleDefinitionID,
	}, "admin-1")
	if rr.Code != http.StatusConflict || govCode(rr.Body.Bytes()) != "not_template_role" {
		t.Fatalf("want 409 not_template_role, got %d %s", rr.Code, rr.Body.String())
	}
}

// ── assignment requests ─────────────────────────────────────────────────────

func requestAssignment(g *govRig, caller, target, roleID, cid string) (int, []byte) {
	rr := doReq(g.r, http.MethodPost, "/v1/iam/access-assignments/", map[string]any{
		"legal_entity_id": "le-1", "target_principal_id": target, "role_definition_id": roleID,
		"justification": "joins AP team", "correlation_id": cid,
	}, caller)
	return rr.Code, rr.Body.Bytes()
}

func TestRequestAssignment_StandardRiskProvisionsImmediately(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	code, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d %s", code, raw)
	}
	a := decodeBody[domain.AssignmentRequest](t, raw)
	if a.Status != domain.AssignmentProvisioned || a.AuthzAssignmentID != a.RequestID {
		t.Fatalf("want PROVISIONED with authz id = request id, got %+v", a)
	}
	if len(g.asg.creates) != 1 || g.asg.creates[0].scope.PrincipalID != "admin-1" || g.asg.creates[0].entity != "le-1" {
		t.Fatalf("provisioning call wrong: %+v", g.asg.creates)
	}
	sodReq := g.sod.reqs[len(g.sod.reqs)-1]
	if sodReq.SubjectPrincipalID != "user-7" || sodReq.LegalEntityID != "le-1" {
		t.Fatalf("SoD must be checked against the subject's holdings: %+v", sodReq)
	}
}

func TestRequestAssignment_TenantRoleAssignedTenantWide(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("TENANT", domain.RoleStatusActive, "support.case.read")
	if code, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString()); code != http.StatusCreated {
		t.Fatalf("got %d %s", code, raw)
	}
	if g.asg.creates[0].entity != "" {
		t.Fatalf("a TENANT role is assigned without an entity, got %q", g.asg.creates[0].entity)
	}
}

func TestRequestAssignment_HighRiskNeedsApproval(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	code, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	if code != http.StatusAccepted {
		t.Fatalf("want 202, got %d %s", code, raw)
	}
	a := decodeBody[domain.AssignmentRequest](t, raw)
	if a.Status != domain.AssignmentPendingApproval || a.RiskTier != domain.RiskCritical || !a.ApprovalRequired {
		t.Fatalf("want pending CRITICAL, got %+v", a)
	}
	if len(g.asg.creates) != 0 {
		t.Fatal("a pending request must not be provisioned")
	}
}

func TestRequestAssignment_ProtectedActionIsCritical(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("TENANT", domain.RoleStatusActive, "iam.assignment.grant")
	_, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	if a := decodeBody[domain.AssignmentRequest](t, raw); a.RiskTier != domain.RiskCritical || a.Status != domain.AssignmentPendingApproval {
		t.Fatalf("protected action must be CRITICAL and pending: %+v", a)
	}
}

func TestRequestAssignment_SelfRequestNeverSelfProvisions(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	code, raw := requestAssignment(g, "admin-1", "admin-1", role.RoleDefinitionID, uuid.NewString())
	if code != http.StatusAccepted {
		t.Fatalf("self-request: want 202 pending, got %d %s", code, raw)
	}
}

func TestRequestAssignment_SoDConflictRefused(t *testing.T) {
	g := newGovRig()
	g.sod.err = &domain.SoDConflictError{Conflicts: []domain.SoDConflict{{CandidateAction: "payment.release", ConflictsWith: "payment.prepare", Source: "held"}}}
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	code, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	if code != http.StatusForbidden || govCode(raw) != "sod_conflict" {
		t.Fatalf("want 403 sod_conflict, got %d %s", code, raw)
	}
}

func TestRequestAssignment_RetiredRoleRefused(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusRetired, "supplier_invoice.create")
	if code, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString()); code != http.StatusConflict || govCode(raw) != "role_retired" {
		t.Fatalf("want 409 role_retired, got %d %s", code, raw)
	}
}

func TestRequestAssignment_ReplayAndDuplicatePending(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	cid := uuid.NewString()
	requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, cid)
	if code, _ := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, cid); code != http.StatusOK {
		t.Fatalf("replay: want 200, got %d", code)
	}
	if code, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString()); code != http.StatusConflict || govCode(raw) != "assignment_pending" {
		t.Fatalf("duplicate pending: want 409, got %d %s", code, raw)
	}
}

func decideAssignment(g *govRig, verb, caller, id string, colon bool) (int, []byte) {
	path := "/v1/iam/access-assignments/" + id + "/" + verb
	if colon {
		path = "/v1/iam/access-assignments/" + id + ":" + verb
	}
	rr := doReq(g.r, http.MethodPost, path, map[string]any{
		"legal_entity_id": "le-1", "reason": "checked", "correlation_id": uuid.NewString(),
	}, caller)
	return rr.Code, rr.Body.Bytes()
}

func TestApproveAssignment_IndependenceAndProvisioning(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	_, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	a := decodeBody[domain.AssignmentRequest](t, raw)

	for _, who := range []string{"admin-1", "user-7"} {
		if code, raw := decideAssignment(g, "approve", who, a.RequestID, true); code != http.StatusForbidden || govCode(raw) != "self_approval" {
			t.Fatalf("%s approving: want 403 self_approval, got %d %s", who, code, raw)
		}
	}
	code, raw := decideAssignment(g, "approve", "approver-2", a.RequestID, true)
	if code != http.StatusOK {
		t.Fatalf("approve: want 200, got %d %s", code, raw)
	}
	out := decodeBody[domain.AssignmentRequest](t, raw)
	if out.Status != domain.AssignmentProvisioned || out.DecidedByPrincipalID != "approver-2" {
		t.Fatalf("not provisioned: %+v", out)
	}
	if len(g.asg.creates) != 1 || g.asg.creates[0].scope.PrincipalID != "approver-2" {
		t.Fatalf("must provision AS the approver: %+v", g.asg.creates)
	}
	if code, _ := decideAssignment(g, "approve", "approver-3", a.RequestID, false); code != http.StatusConflict {
		t.Fatalf("second approve: want 409, got %d", code)
	}
}

func TestRejectAndCancelAssignment(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	_, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	a := decodeBody[domain.AssignmentRequest](t, raw)
	if code, _ := decideAssignment(g, "cancel", "approver-2", a.RequestID, false); code != http.StatusForbidden {
		t.Fatalf("only the requester cancels; got %d", code)
	}
	if code, raw := decideAssignment(g, "reject", "approver-2", a.RequestID, false); code != http.StatusOK || decodeBody[domain.AssignmentRequest](t, raw).Status != domain.AssignmentRejected {
		t.Fatalf("reject: got %d %s", code, raw)
	}
}

func TestRevokeAssignment(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	_, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	a := decodeBody[domain.AssignmentRequest](t, raw)
	code, raw := decideAssignment(g, "revoke", "admin-1", a.RequestID, true)
	if code != http.StatusOK || decodeBody[domain.AssignmentRequest](t, raw).Status != domain.AssignmentRevoked {
		t.Fatalf("revoke: got %d %s", code, raw)
	}
	if len(g.asg.revokes) != 1 || g.asg.revokes[0] != a.AuthzAssignmentID {
		t.Fatalf("authorization-svc revoke not called: %v", g.asg.revokes)
	}
	if g.gov.events[len(g.gov.events)-1] != "iam.assignment.revoked" {
		t.Fatalf("no revoked event: %v", g.gov.events)
	}
}

func TestRevokeAssignment_ProvisioningForbiddenLeavesItProvisioned(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	_, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	a := decodeBody[domain.AssignmentRequest](t, raw)
	g.asg.revokeErr = fmt.Errorf("%w: iam.assignment.revoke is required", domain.ErrProvisioningForbidden)
	if code, raw := decideAssignment(g, "revoke", "admin-1", a.RequestID, true); code != http.StatusForbidden || govCode(raw) != "provisioning_forbidden" {
		t.Fatalf("want 403 provisioning_forbidden, got %d %s", code, raw)
	}
	if g.gov.assignments[a.RequestID].Status != domain.AssignmentProvisioned {
		t.Fatal("a revocation that did not happen there must not be recorded here")
	}
}

// ── access review campaigns ─────────────────────────────────────────────────

func createCampaign(g *govRig, body map[string]any) (int, []byte) {
	base := map[string]any{
		"legal_entity_id": "le-1", "campaign_name": "Q4 certification", "review_type": "PERIODIC",
		"default_reviewer_principal_id": "mgr-1", "due_at": time.Now().Add(72 * time.Hour), "correlation_id": uuid.NewString(),
	}
	for k, v := range body {
		base[k] = v
	}
	rr := doReq(g.r, http.MethodPost, "/v1/access-review-campaigns/", base, "admin-1")
	return rr.Code, rr.Body.Bytes()
}

func entity(s string) *string { return &s }

func TestCreateCampaign_SnapshotsFlagsAndToxicCombinations(t *testing.T) {
	g := newGovRig()
	prep := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.prepare")
	rel := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	retired := g.seedRole("LEGAL_ENTITY", domain.RoleStatusRetired, "supplier_invoice.create")
	g.asg.assignments[prep.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-7", RoleID: prep.RoleDefinitionID, LegalEntityID: entity("le-1")}}
	g.asg.assignments[rel.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-7", RoleID: rel.RoleDefinitionID, LegalEntityID: entity("le-1")}}
	g.asg.assignments[retired.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-8", RoleID: retired.RoleDefinitionID, LegalEntityID: entity("le-1")}}
	g.sod.decide = func(req domain.SoDCheckRequest) error {
		has := map[string]bool{}
		for _, a := range req.CandidateActions {
			has[a] = true
		}
		if has["payment.prepare"] && has["payment.release"] {
			return &domain.SoDConflictError{Conflicts: []domain.SoDConflict{{CandidateAction: "payment.prepare", ConflictsWith: "payment.release"}}}
		}
		return nil
	}

	code, raw := createCampaign(g, nil)
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d %s", code, raw)
	}
	c := decodeBody[domain.ReviewCampaign](t, raw)
	if len(c.Items) != 3 {
		t.Fatalf("want 3 items, got %d", len(c.Items))
	}
	for _, it := range c.Items {
		flags := fmt.Sprint(it.Flags)
		switch it.RoleDefinitionID {
		case prep.RoleDefinitionID:
			if flags != "[SOD_CONFLICT]" || it.RiskTier != domain.RiskHigh {
				t.Fatalf("preparer item: %+v", it)
			}
		case retired.RoleDefinitionID:
			if flags != "[ORPHANED_ROLE]" {
				t.Fatalf("orphan item: %+v", it)
			}
		}
	}
}

func TestCreateCampaign_NoSelfAttestation(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	g.asg.assignments[role.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "mgr-1", RoleID: role.RoleDefinitionID}}
	if code, raw := createCampaign(g, nil); code != http.StatusBadRequest || govCode(raw) != "escalation_reviewer_required" {
		t.Fatalf("want 400 escalation_reviewer_required, got %d %s", code, raw)
	}
	code, raw := createCampaign(g, map[string]any{"escalation_reviewer_principal_id": "sec-1"})
	if code != http.StatusCreated {
		t.Fatalf("got %d %s", code, raw)
	}
	it := decodeBody[domain.ReviewCampaign](t, raw).Items[0]
	if it.ReviewerPrincipalID != "sec-1" {
		t.Fatalf("item about the default reviewer must go to the escalation reviewer: %+v", it)
	}
}

func TestReviewDecisionsAndCompletion(t *testing.T) {
	g := newGovRig()
	high := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	std := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	highAsg := uuid.NewString()
	g.asg.assignments[high.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: highAsg, PrincipalID: "user-7", RoleID: high.RoleDefinitionID}}
	g.asg.assignments[std.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-8", RoleID: std.RoleDefinitionID}}
	_, raw := createCampaign(g, nil)
	c := decodeBody[domain.ReviewCampaign](t, raw)
	var highItem string
	for _, it := range c.Items {
		if it.RoleDefinitionID == high.RoleDefinitionID {
			highItem = it.ItemID
		}
	}

	decide := func(caller, decision string) (int, []byte) {
		rr := doReq(g.r, http.MethodPost, "/v1/access-review-campaigns/"+c.CampaignID+"/items/"+highItem+"/decide", map[string]any{
			"decision": decision, "reason": "no longer in treasury", "correlation_id": uuid.NewString(),
		}, caller)
		return rr.Code, rr.Body.Bytes()
	}
	complete := func() (int, []byte) {
		rr := doReq(g.r, http.MethodPost, "/v1/access-review-campaigns/"+c.CampaignID+"/complete", map[string]any{
			"legal_entity_id": "le-1", "correlation_id": uuid.NewString(),
		}, "admin-1")
		return rr.Code, rr.Body.Bytes()
	}

	if code, raw := decide("user-7", "KEEP"); code != http.StatusForbidden || govCode(raw) != "self_attestation" {
		t.Fatalf("subject deciding: want 403 self_attestation, got %d %s", code, raw)
	}
	if code, raw := decide("someone", "KEEP"); code != http.StatusForbidden || govCode(raw) != "not_item_reviewer" {
		t.Fatalf("non-reviewer: want 403, got %d %s", code, raw)
	}
	if code, raw := complete(); code != http.StatusConflict || govCode(raw) != "unresolved_high_risk" {
		t.Fatalf("complete with open CRITICAL item: want 409, got %d %s", code, raw)
	}
	code, raw := decide("mgr-1", "REVOKE")
	if code != http.StatusOK {
		t.Fatalf("revoke: got %d %s", code, raw)
	}
	if it := decodeBody[domain.ReviewItem](t, raw); !it.RevocationApplied || len(g.asg.revokes) != 1 || g.asg.revokes[0] != highAsg {
		t.Fatalf("REVOKE must revoke in authorization-svc: %+v %v", it, g.asg.revokes)
	}
	if code, _ := decide("mgr-1", "KEEP"); code != http.StatusConflict {
		t.Fatalf("re-deciding: want 409, got %d", code)
	}
	code, raw = complete()
	if code != http.StatusOK {
		t.Fatalf("complete: got %d %s", code, raw)
	}
	for _, it := range decodeBody[domain.ReviewCampaign](t, raw).Items {
		if it.RoleDefinitionID == std.RoleDefinitionID && it.Status != domain.ReviewItemExpired {
			t.Fatalf("an undecided STANDARD item is recorded EXPIRED, got %s", it.Status)
		}
	}
}

func TestMyAccessReviews(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	g.asg.assignments[role.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-8", RoleID: role.RoleDefinitionID}}
	createCampaign(g, nil)
	rr := doReq(g.r, http.MethodGet, "/v1/iam/access-reviews", nil, "mgr-1")
	if items := decodeBody[[]domain.ReviewItem](t, rr.Body.Bytes()); rr.Code != http.StatusOK || len(items) != 1 {
		t.Fatalf("want the reviewer's 1 item, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(g.r, http.MethodGet, "/v1/iam/access-reviews", nil, "someone-else")
	if items := decodeBody[[]domain.ReviewItem](t, rr.Body.Bytes()); len(items) != 0 {
		t.Fatalf("another principal sees none, got %d", len(items))
	}
}
