package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/handler"
	"zoiko.io/org-structure-svc/internal/middleware"
)

// ── stub maker-checker store: same rules as PgStore, in memory ────────────────

func (s *stubStore) ReparentImpact(_ context.Context, id, _ string) (*domain.ImpactReport, error) {
	if _, ok := s.units[id]; !ok {
		return nil, domain.ErrOrgUnitNotFound
	}
	affected, queue := 0, []string{id}
	var mapped []domain.MappedUnit
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		u := s.units[cur]
		if u.Status == domain.UnitStatusInactive && cur != id {
			continue
		}
		affected++
		if u.CostCenterCode != "" || u.ProfitCenterCode != "" {
			mapped = append(mapped, domain.MappedUnit{UnitID: u.UnitID, Code: u.Code, CostCenterCode: u.CostCenterCode, ProfitCenterCode: u.ProfitCenterCode})
		}
		for _, c := range s.units {
			if c.ParentUnitID != nil && *c.ParentUnitID == cur {
				queue = append(queue, c.UnitID)
			}
		}
	}
	r := domain.NewImpactReport(id, affected, mapped)
	return &r, nil
}

func (s *stubStore) CreateReparentRequest(ctx context.Context, id string, req *domain.ReparentOrgUnitRequest, expected int64, actor string, impact domain.ImpactReport) (*domain.ChangeRequest, bool, error) {
	// Dry run: apply the move, then put the unit back exactly as it was.
	u := s.units[id]
	if u == nil {
		return nil, false, domain.ErrOrgUnitNotFound
	}
	parent, version, from := u.ParentUnitID, u.RecordVersion, s.placeFrom[id]
	_, err := s.ReparentOrgUnit(ctx, id, req, expected, actor)
	u.ParentUnitID, u.RecordVersion, s.placeFrom[id] = parent, version, from
	if err != nil {
		return nil, false, err
	}

	for _, cr := range s.changes {
		if cr.UnitID == id && cr.Status == domain.ChangePending {
			same := (cr.NewParentUnitID == nil && req.NewParentUnitID == nil) ||
				(cr.NewParentUnitID != nil && req.NewParentUnitID != nil && *cr.NewParentUnitID == *req.NewParentUnitID)
			if same && cr.EffectiveAt == req.EffectiveAt {
				return cr, false, nil
			}
			return nil, false, domain.ErrChangePending
		}
	}
	cr := &domain.ChangeRequest{
		RequestID: fmt.Sprintf("cr-%d", len(s.changes)+1), TenantID: "tenant-abc", LegalEntityID: u.LegalEntityID, UnitID: id,
		Command: domain.ChangeCommandReparent, NewParentUnitID: req.NewParentUnitID, EffectiveAt: req.EffectiveAt, Reason: req.Reason,
		UnitVersion: expected, Impact: impact, Status: domain.ChangePending, RequestedBy: actor, RequestedAt: time.Now().UTC(),
	}
	s.changes[cr.RequestID] = cr
	return cr, true, nil
}

func (s *stubStore) GetChangeRequest(_ context.Context, id string) (*domain.ChangeRequest, error) {
	cr, ok := s.changes[id]
	if !ok {
		return nil, domain.ErrChangeRequestNotFound
	}
	return cr, nil
}

func (s *stubStore) ListChangeRequests(_ context.Context, entity, status string) ([]domain.ChangeRequest, error) {
	out := []domain.ChangeRequest{}
	for _, cr := range s.changes {
		if cr.LegalEntityID == entity && (status == "" || cr.Status == status) {
			out = append(out, *cr)
		}
	}
	return out, nil
}

func (s *stubStore) decide(cr *domain.ChangeRequest, status, by string) {
	now := time.Now().UTC()
	cr.Status, cr.DecidedBy, cr.DecidedAt = status, &by, &now
}

func (s *stubStore) ApproveChangeRequest(ctx context.Context, id, approver string) (*domain.ChangeRequest, *domain.OrgUnit, error) {
	cr, ok := s.changes[id]
	if !ok {
		return nil, nil, domain.ErrChangeRequestNotFound
	}
	if cr.Status != domain.ChangePending {
		return nil, nil, domain.ErrChangeRequestDecided
	}
	if cr.RequestedBy == approver {
		return nil, nil, domain.ErrSelfApproval
	}
	unit, err := s.ReparentOrgUnit(ctx, cr.UnitID, &domain.ReparentOrgUnitRequest{NewParentUnitID: cr.NewParentUnitID, EffectiveAt: cr.EffectiveAt, Reason: cr.Reason}, cr.UnitVersion, cr.RequestedBy)
	if err != nil {
		if domain.IsTerminalApplyError(err) {
			s.decide(cr, domain.ChangeFailed, approver)
			cr.FailureCode = domain.FailureCode(err)
		}
		return nil, nil, err
	}
	s.decide(cr, domain.ChangeApproved, approver)
	return cr, unit, nil
}

func (s *stubStore) RejectChangeRequest(_ context.Context, id, decider, reason string) (*domain.ChangeRequest, error) {
	cr, ok := s.changes[id]
	if !ok {
		return nil, domain.ErrChangeRequestNotFound
	}
	if cr.Status != domain.ChangePending {
		return nil, domain.ErrChangeRequestDecided
	}
	s.decide(cr, domain.ChangeRejected, decider)
	cr.DecisionReason = reason
	return cr, nil
}

// ── helpers ────────────────────────────────────────────────────────────────────

// actionAuthZ denies the listed actions and allows everything else.
type actionAuthZ struct{ deny map[string]bool }

func (a *actionAuthZ) CheckAllowed(_ context.Context, _, _, action string) error {
	if a.deny[action] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

type recordingPublisher struct{ events []string }

func (p *recordingPublisher) PublishPositionCreated(context.Context, string, string, domain.Position) {
}
func (p *recordingPublisher) PublishEmployeeAssigned(context.Context, string, string, domain.OrgAssignment) {
}
func (p *recordingPublisher) PublishOrgStructureChanged(_ context.Context, _, _, _, _, eventType, _ string) {
	p.events = append(p.events, eventType)
}

func routerFor(s *stubStore, pub handler.Publisher, authz handler.AuthZClient) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterRoutes(r, handler.New(s, pub, authz, &stubEmployeeValidator{}, zap.NewNop()))
	return r
}

func call(r http.Handler, method, path, principal, ifMatch string, body any) *httptest.ResponseRecorder {
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func mappedUnitBody(code, corr string, parent *string, costCenter string) map[string]any {
	b := unitBody(code, corr, parent)
	b["cost_center_code"] = costCenter
	return b
}

type fixture struct {
	r              chi.Router
	s              *stubStore
	pub            *recordingPublisher
	root, fin, alt string // alt is a second root; fin is mapped and sits under root
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, pub := newStubStore(), &recordingPublisher{}
	r := routerFor(s, pub, &stubAuthZ{})
	f := &fixture{r: r, s: s, pub: pub}
	f.root = decodeUnit(t, call(r, "POST", "/v1/org/units", "maker", "", unitBody("ROOT", "f-1", nil))).UnitID
	f.fin = decodeUnit(t, call(r, "POST", "/v1/org/units", "maker", "", mappedUnitBody("FIN", "f-2", &f.root, "CC100"))).UnitID
	f.alt = decodeUnit(t, call(r, "POST", "/v1/org/units", "maker", "", unitBody("ALT", "f-3", nil))).UnitID
	return f
}

func (f *fixture) propose(t *testing.T, unit, to, at string) *httptest.ResponseRecorder {
	t.Helper()
	return call(f.r, "POST", "/v1/org/units/"+unit+"/reparent", "maker", "1", map[string]any{
		"new_parent_unit_id": to, "effective_at": at, "reason": "reorganisation"})
}

func decodeProposed(t *testing.T, rr *httptest.ResponseRecorder) handler.ProposedChange {
	t.Helper()
	var p handler.ProposedChange
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode proposal: %v (%s)", err, rr.Body.String())
	}
	return p
}

// ── tests ──────────────────────────────────────────────────────────────────────

// A move that carries a financial mapping is not applied by its maker.
func TestMakerChecker_MappedMoveWaitsForApproval(t *testing.T) {
	f := newFixture(t)
	rr := f.propose(t, f.fin, f.alt, "2026-03-01")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d %s", rr.Code, rr.Body.String())
	}
	p := decodeProposed(t, rr)
	if p.Status != "PENDING_APPROVAL" || p.ChangeRequest.Status != domain.ChangePending || p.ChangeRequest.RequestedBy != "maker" {
		t.Fatalf("unexpected proposal: %+v", p)
	}
	if !p.Impact.RequiresApproval || len(p.Impact.MappedUnits) != 1 || p.Impact.MappedUnits[0].CostCenterCode != "CC100" {
		t.Fatalf("the impact must name the mapped unit: %+v", p.Impact)
	}
	if *f.s.units[f.fin].ParentUnitID != f.root || f.s.units[f.fin].RecordVersion != 1 {
		t.Fatal("nothing may move before approval")
	}
	// Org-unit events are written by the store, in the change's transaction
	// (see the outbox tests); the handler must not also publish them.
	if len(f.pub.events) != 0 {
		t.Fatalf("the handler must not publish org-unit events directly, got %v", f.pub.events)
	}
}

// The mapping can be below the unit being moved: moving its parent reaches it.
func TestMakerChecker_MappedDescendantAlsoTriggersApproval(t *testing.T) {
	f := newFixture(t)
	rr := f.propose(t, f.root, f.alt, "2026-03-01")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("moving the parent of a mapped unit: want 202, got %d %s", rr.Code, rr.Body.String())
	}
	if p := decodeProposed(t, rr); p.Impact.UnitsAffected != 2 || len(p.Impact.MappedUnits) != 1 {
		t.Fatalf("ROOT and FIN are reached, FIN is mapped: %+v", p.Impact)
	}
}

func TestMakerChecker_UnmappedMoveStillAppliesDirectly(t *testing.T) {
	f := newFixture(t)
	leaf := decodeUnit(t, call(f.r, "POST", "/v1/org/units", "maker", "", unitBody("LEAF", "f-9", &f.alt))).UnitID
	rr := call(f.r, "POST", "/v1/org/units/"+leaf+"/reparent", "maker", "1", map[string]any{
		"new_parent_unit_id": f.root, "effective_at": "2026-03-01", "reason": "tidy"})
	if rr.Code != http.StatusOK || *decodeUnit(t, rr).ParentUnitID != f.root {
		t.Fatalf("an unmapped move needs no approver: got %d %s", rr.Code, rr.Body.String())
	}
	if len(f.s.changes) != 0 {
		t.Fatal("no change request for an unmapped move")
	}
}

func TestMakerChecker_ProposingTwiceIsIdempotentButConflictsAreRefused(t *testing.T) {
	f := newFixture(t)
	first := decodeProposed(t, f.propose(t, f.fin, f.alt, "2026-03-01"))

	again := f.propose(t, f.fin, f.alt, "2026-03-01")
	if again.Code != http.StatusOK || decodeProposed(t, again).ChangeRequest.RequestID != first.ChangeRequest.RequestID {
		t.Fatalf("the same move returns the pending request: got %d %s", again.Code, again.Body.String())
	}
	other := f.propose(t, f.fin, f.alt, "2026-04-01")
	if other.Code != http.StatusConflict || errCode(other) != "change_pending" {
		t.Fatalf("a different move for the same unit: got %d %s", other.Code, other.Body.String())
	}
	if len(f.s.changes) != 1 {
		t.Fatalf("exactly one pending change per unit, got %d", len(f.s.changes))
	}
}

// Impossible moves are refused to the maker at once, not parked for an approver.
func TestMakerChecker_ImpossibleMoveRefusedAtProposal(t *testing.T) {
	f := newFixture(t)
	rr := f.propose(t, f.root, f.fin, "2026-03-01") // ROOT under its own mapped child
	if rr.Code != 422 || errCode(rr) != "hierarchy_loop" {
		t.Fatalf("loop: got %d %s", rr.Code, rr.Body.String())
	}
	if rr := call(f.r, "POST", "/v1/org/units/"+f.fin+"/reparent", "maker", "9", map[string]any{
		"new_parent_unit_id": f.alt, "effective_at": "2026-03-01", "reason": "x"}); rr.Code != 409 || errCode(rr) != "version_conflict" {
		t.Fatalf("stale version: got %d %s", rr.Code, rr.Body.String())
	}
	if len(f.s.changes) != 0 {
		t.Fatal("a refused proposal must not leave a request behind")
	}
}

// Segregation of duties: nobody approves what they proposed.
func TestMakerChecker_SelfApprovalRefused(t *testing.T) {
	f := newFixture(t)
	id := decodeProposed(t, f.propose(t, f.fin, f.alt, "2026-03-01")).ChangeRequest.RequestID

	rr := call(f.r, "POST", "/v1/org/change-requests/"+id+"/approve", "maker", "", nil)
	if rr.Code != 403 || errCode(rr) != "sod_denied" {
		t.Fatalf("self-approval: got %d %s", rr.Code, rr.Body.String())
	}
	if f.s.changes[id].Status != domain.ChangePending || *f.s.units[f.fin].ParentUnitID != f.root {
		t.Fatal("a refused self-approval must change nothing")
	}
}

func TestMakerChecker_IndependentApprovalAppliesTheMove(t *testing.T) {
	f := newFixture(t)
	id := decodeProposed(t, f.propose(t, f.fin, f.alt, "2026-03-01")).ChangeRequest.RequestID

	rr := call(f.r, "POST", "/v1/org/change-requests/"+id+"/approve", "checker", "", nil)
	if rr.Code != 200 {
		t.Fatalf("approve: got %d %s", rr.Code, rr.Body.String())
	}
	var out handler.DecidedChange
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.ChangeRequest.Status != domain.ChangeApproved || *out.ChangeRequest.DecidedBy != "checker" || out.ChangeRequest.RequestedBy != "maker" {
		t.Fatalf("approval must record both people: %+v", out.ChangeRequest)
	}
	if *out.Unit.ParentUnitID != f.alt || out.Unit.RecordVersion != 2 {
		t.Fatalf("the move must now be applied: %+v", out.Unit)
	}
	if len(f.pub.events) != 0 {
		t.Fatalf("the handler must not publish org-unit events directly, got %v", f.pub.events)
	}

	if rr := call(f.r, "POST", "/v1/org/change-requests/"+id+"/approve", "checker2", "", nil); rr.Code != 409 || errCode(rr) != "change_request_decided" {
		t.Fatalf("a decided request cannot be approved again: got %d %s", rr.Code, rr.Body.String())
	}
}

func TestMakerChecker_RejectLeavesTheStructureAlone(t *testing.T) {
	f := newFixture(t)
	id := decodeProposed(t, f.propose(t, f.fin, f.alt, "2026-03-01")).ChangeRequest.RequestID

	if rr := call(f.r, "POST", "/v1/org/change-requests/"+id+"/reject", "checker", "", map[string]any{}); rr.Code != 400 || errCode(rr) != "missing_fields" {
		t.Fatalf("reject needs a reason: got %d %s", rr.Code, rr.Body.String())
	}
	rr := call(f.r, "POST", "/v1/org/change-requests/"+id+"/reject", "checker", "", map[string]any{"reason": "wrong cost centre"})
	var cr domain.ChangeRequest
	_ = json.Unmarshal(rr.Body.Bytes(), &cr)
	if rr.Code != 200 || cr.Status != domain.ChangeRejected || cr.DecisionReason != "wrong cost centre" {
		t.Fatalf("reject: got %d %s", rr.Code, rr.Body.String())
	}
	if *f.s.units[f.fin].ParentUnitID != f.root {
		t.Fatal("a rejected change must not move anything")
	}
	if rr := call(f.r, "POST", "/v1/org/change-requests/"+id+"/approve", "checker", "", nil); rr.Code != 409 {
		t.Fatalf("a rejected request cannot be approved: got %d", rr.Code)
	}
	// With the request closed, the unit can be proposed for again.
	if rr := f.propose(t, f.fin, f.alt, "2026-03-01"); rr.Code != http.StatusAccepted {
		t.Fatalf("a closed request frees the unit: got %d %s", rr.Code, rr.Body.String())
	}
}

// If the unit changed after the proposal, approval cannot force it through: the
// request is closed as FAILED with the reason.
func TestMakerChecker_StaleProposalFailsInsteadOfBeingForced(t *testing.T) {
	f := newFixture(t)
	id := decodeProposed(t, f.propose(t, f.fin, f.alt, "2026-03-01")).ChangeRequest.RequestID

	// Someone else changes FIN's manager meanwhile: version 1 -> 2.
	if rr := changeManager(f.r, f.fin, "1", mgrMove("emp-1", "2026-02-01")); rr.Code != 200 {
		t.Fatalf("setup change manager: got %d %s", rr.Code, rr.Body.String())
	}

	rr := call(f.r, "POST", "/v1/org/change-requests/"+id+"/approve", "checker", "", nil)
	if rr.Code != 409 || errCode(rr) != "version_conflict" {
		t.Fatalf("stale approval: got %d %s", rr.Code, rr.Body.String())
	}
	got := call(f.r, "GET", "/v1/org/change-requests/"+id, "checker", "", nil)
	var cr domain.ChangeRequest
	_ = json.Unmarshal(got.Body.Bytes(), &cr)
	if cr.Status != domain.ChangeFailed || cr.FailureCode != "version_conflict" {
		t.Fatalf("the request must be closed as FAILED, got %+v", cr)
	}
	if *f.s.units[f.fin].ParentUnitID != f.root {
		t.Fatal("a failed approval must not move the unit")
	}
}

func TestMakerChecker_ImpactPreviewChangesNothing(t *testing.T) {
	f := newFixture(t)
	rr := call(f.r, "GET", "/v1/org/units/"+f.root+"/reparent-impact?effective_at=2026-03-01", "maker", "", nil)
	var im domain.ImpactReport
	_ = json.Unmarshal(rr.Body.Bytes(), &im)
	if rr.Code != 200 || !im.RequiresApproval || im.UnitsAffected != 2 {
		t.Fatalf("preview: got %d %+v", rr.Code, im)
	}
	if len(f.s.changes) != 0 {
		t.Fatal("a preview must not open a request")
	}
	if rr := call(f.r, "GET", "/v1/org/units/"+f.root+"/reparent-impact?effective_at=soon", "maker", "", nil); rr.Code != 400 {
		t.Fatalf("bad date: want 400, got %d", rr.Code)
	}
	if rr := call(f.r, "GET", "/v1/org/units/nope/reparent-impact?effective_at=2026-03-01", "maker", "", nil); rr.Code != 404 {
		t.Fatalf("unknown unit: want 404, got %d", rr.Code)
	}
}

// Proposing needs ORG_UNIT_REPARENT; deciding needs ORG_UNIT_APPROVE_STRUCTURE.
func TestMakerChecker_EachStepNeedsItsOwnRight(t *testing.T) {
	f := newFixture(t)
	id := decodeProposed(t, f.propose(t, f.fin, f.alt, "2026-03-01")).ChangeRequest.RequestID

	noApprove := routerFor(f.s, f.pub, &actionAuthZ{deny: map[string]bool{"ORG_UNIT_APPROVE_STRUCTURE": true}})
	if rr := call(noApprove, "POST", "/v1/org/change-requests/"+id+"/approve", "checker", "", nil); rr.Code != 403 || errCode(rr) != "forbidden" {
		t.Fatalf("approve without the right: got %d %s", rr.Code, rr.Body.String())
	}
	if rr := call(noApprove, "POST", "/v1/org/change-requests/"+id+"/reject", "checker", "", map[string]any{"reason": "x"}); rr.Code != 403 {
		t.Fatalf("reject without the right: got %d", rr.Code)
	}
	if f.s.changes[id].Status != domain.ChangePending {
		t.Fatal("a denied decision must leave the request pending")
	}

	noReparent := routerFor(f.s, f.pub, &actionAuthZ{deny: map[string]bool{"ORG_UNIT_REPARENT": true}})
	for name, rr := range map[string]*httptest.ResponseRecorder{
		"propose": call(noReparent, "POST", "/v1/org/units/"+f.fin+"/reparent", "maker", "1", map[string]any{"new_parent_unit_id": f.alt, "effective_at": "2026-04-01", "reason": "x"}),
		"preview": call(noReparent, "GET", "/v1/org/units/"+f.fin+"/reparent-impact?effective_at=2026-04-01", "maker", "", nil),
	} {
		if rr.Code != 403 {
			t.Errorf("%s without ORG_UNIT_REPARENT: want 403, got %d", name, rr.Code)
		}
	}
	// Reading requests only needs view.
	if rr := call(noApprove, "GET", "/v1/org/change-requests/"+id, "viewer", "", nil); rr.Code != 200 {
		t.Fatalf("view: got %d", rr.Code)
	}
}

func TestMakerChecker_ListAndLookup(t *testing.T) {
	f := newFixture(t)
	id := decodeProposed(t, f.propose(t, f.fin, f.alt, "2026-03-01")).ChangeRequest.RequestID

	count := func(q string) int {
		rr := call(f.r, "GET", "/v1/org/change-requests?"+q, "viewer", "", nil)
		var out []domain.ChangeRequest
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return len(out)
	}
	if count("legal_entity_id=le-us") != 1 || count("legal_entity_id=le-us&status=PENDING") != 1 || count("legal_entity_id=le-us&status=APPROVED") != 0 {
		t.Fatal("the list must filter by entity and status")
	}
	if rr := call(f.r, "GET", "/v1/org/change-requests?legal_entity_id=le-us&status=DONE", "viewer", "", nil); rr.Code != 400 || errCode(rr) != "invalid_status" {
		t.Fatalf("bad status: got %d %s", rr.Code, rr.Body.String())
	}
	if rr := call(f.r, "GET", "/v1/org/change-requests", "viewer", "", nil); rr.Code != 400 {
		t.Fatalf("missing entity: want 400, got %d", rr.Code)
	}
	if rr := call(f.r, "GET", "/v1/org/change-requests/"+id, "viewer", "", nil); rr.Code != 200 {
		t.Fatalf("get: got %d", rr.Code)
	}
	if rr := call(f.r, "GET", "/v1/org/change-requests/nope", "viewer", "", nil); rr.Code != 404 || errCode(rr) != "change_request_not_found" {
		t.Fatalf("unknown request: got %d %s", rr.Code, rr.Body.String())
	}
}
