package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// REF-01 ImportJurisdictionUpdate at the HTTP layer: the actor is the verified
// caller, each refusal is its own answer, and every applied change is announced.
// The transaction, the all-or-nothing apply and the guards are proven on a real
// database in internal/store.

type importStub struct {
	*stubStore

	batch      *domain.ImportBatch
	created    bool
	proposeErr error
	gotPropose domain.ProposeImportParams

	approved   *domain.ImportBatch
	changes    []domain.AppliedChange
	approveErr error
	gotDecide  domain.DecideImportParams

	rejectErr error
	gotReject domain.DecideImportParams

	getErr error
	gotGet string
	list   []*domain.ImportBatch
	gotSt  string
	gotLim int
}

func (s *importStub) ProposeJurisdictionImportWithQuerier(_ context.Context, _ store.Querier, p domain.ProposeImportParams) (*domain.ImportBatch, bool, error) {
	s.gotPropose = p
	return s.batch, s.created, s.proposeErr
}

func (s *importStub) ApproveJurisdictionImportWithQuerier(_ context.Context, _ store.Querier, p domain.DecideImportParams) (*domain.ImportBatch, []domain.AppliedChange, error) {
	s.gotDecide = p
	return s.approved, s.changes, s.approveErr
}

func (s *importStub) RejectJurisdictionImportWithQuerier(_ context.Context, _ store.Querier, p domain.DecideImportParams) (*domain.ImportBatch, error) {
	s.gotReject = p
	return s.batch, s.rejectErr
}

func (s *importStub) GetJurisdictionImport(_ context.Context, id string) (*domain.ImportBatch, error) {
	s.gotGet = id
	return s.batch, s.getErr
}

func (s *importStub) ListJurisdictionImports(_ context.Context, status string, limit int) ([]*domain.ImportBatch, error) {
	s.gotSt, s.gotLim = status, limit
	return s.list, nil
}

// importSpy is the shared spy plus both optional capabilities an import uses.
type importSpy struct {
	*updateSpy
	moves []movedParent
}

type movedParent struct {
	previous *string
	current  *string
	src      *events.ImportSource
}

func (p *importSpy) PublishJurisdictionHierarchyChanged(_ context.Context, j domain.Jurisdiction, prev *string, src *events.ImportSource, _ string) error {
	p.moves = append(p.moves, movedParent{previous: prev, current: j.ParentJurisdictionID, src: src})
	return p.record("jurisdiction.hierarchy.changed")
}

func newImportStub() *importStub {
	b := &domain.ImportBatch{BatchID: "b-1", SourceStandard: "ISO 3166-2", SourceVersion: "2026-1", ImportHash: strings.Repeat("a", 64),
		Status: domain.ImportProposed, ItemCount: 1, ProposedBy: "maker-1"}
	return &importStub{stubStore: &stubStore{}, batch: b, created: true, approved: b}
}

func importRouter(st *importStub, a authz.AuthorizationClient) (http.Handler, *importSpy) {
	pub := &importSpy{updateSpy: &updateSpy{spyPublisher: &spyPublisher{}}}
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(st, a, pub, testAuthzScopeID, zap.NewNop()))
	return r, pub
}

func getAs(t *testing.T, h http.Handler, path, principal string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

var proposeBody = map[string]any{
	"source_standard": "ISO 3166-2", "source_version": "2026-1", "evidence_ref": "https://example.test/iso-2026-1.csv",
	"summary": "2026 subdivision changes",
	"items": []map[string]any{
		{"action": "ADD", "target": map[string]any{"code": "GB-NEW", "type": "STATE_PROVINCE"}, "jurisdiction_name": "New",
			"authority_type": "STATE", "effective_from": "2026-01-01T00:00:00Z", "parent": map[string]any{"code": "GB", "type": "COUNTRY"}},
	},
}

// ── propose ─────────────────────────────────────────────────────────────────

func TestImport_ProposeIsAsTheVerifiedCallerAndReplayIs200(t *testing.T) {
	st := newImportStub()
	h, _ := importRouter(st, permitAll())
	rr := postJSON(t, h, "/v1/admin/jurisdiction-imports", proposeBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	p := st.gotPropose
	if p.ActorID != testPrincipal || p.SourceStandard != "ISO 3166-2" || p.SourceVersion != "2026-1" || len(p.Items) != 1 ||
		p.Items[0].Action != "ADD" || p.Items[0].Target.Code != "GB-NEW" || p.Items[0].Parent == nil || p.EvidenceRef == "" {
		t.Errorf("params = %+v, want the verified caller and the body", p)
	}
	if body := decodeMap(t, rr); body["batch_id"] != "b-1" || body["status"] != "PROPOSED" {
		t.Errorf("body = %v", body)
	}

	st.created = false
	h2, _ := importRouter(st, permitAll())
	if rr := postJSON(t, h2, "/v1/admin/jurisdiction-imports", proposeBody); rr.Code != http.StatusOK {
		t.Errorf("replay: %d, want 200", rr.Code)
	}
}

func TestImport_ProposeRefusalsAreMapped(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"invalid batch":    {domain.ErrInvalidImport, http.StatusBadRequest, "invalid_import"},
		"version conflict": {domain.ErrImportVersionConflict, http.StatusConflict, "import_version_conflict"},
		"store down":       {domain.ErrStoreUnavailable, http.StatusServiceUnavailable, "store_unavailable"},
	} {
		st := newImportStub()
		st.proposeErr = c.err
		h, _ := importRouter(st, permitAll())
		rr := postJSON(t, h, "/v1/admin/jurisdiction-imports", proposeBody)
		if rr.Code != c.status || problemCode(t, rr) != c.code {
			t.Errorf("%s: %d %q, want %d %q", name, rr.Code, problemCode(t, rr), c.status, c.code)
		}
	}
}

func TestImport_MalformedOrOversizedBodyIs400(t *testing.T) {
	st := newImportStub()
	h, _ := importRouter(st, permitAll())
	for name, body := range map[string]string{
		"not json":  "{not json",
		"too large": `{"summary":"` + strings.Repeat("x", 5<<20) + `"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/jurisdiction-imports", strings.NewReader(body))
		req.Header.Set("X-Principal-Id", testPrincipal)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest || st.gotPropose.ActorID != "" {
			t.Errorf("%s: %d, store reached=%v, want 400 and no store call", name, rr.Code, st.gotPropose.ActorID != "")
		}
	}
}

// ── approve ─────────────────────────────────────────────────────────────────

func TestImport_ApproveAnnouncesEveryChangeInTheSameTransaction(t *testing.T) {
	parentA, parentB := "p-a", "p-b"
	st := newImportStub()
	st.approved = &domain.ImportBatch{BatchID: "b-1", SourceStandard: "ISO 3166-2", SourceVersion: "2026-1", ImportHash: strings.Repeat("b", 64), Status: domain.ImportApproved}
	st.changes = []domain.AppliedChange{
		{ItemNo: 1, Outcome: domain.OutcomeAdded, Jurisdiction: &domain.Jurisdiction{JurisdictionID: "new"}},
		{ItemNo: 2, Outcome: domain.OutcomeUpdated, Before: &domain.Jurisdiction{JurisdictionID: "mv", ParentJurisdictionID: &parentA, JurisdictionName: "Old"},
			Jurisdiction: &domain.Jurisdiction{JurisdictionID: "mv", ParentJurisdictionID: &parentB, JurisdictionName: "New"}, ParentChanged: true, AttributesChanged: true},
		{ItemNo: 3, Outcome: domain.OutcomeUpdated, Before: &domain.Jurisdiction{JurisdictionID: "rn", JurisdictionName: "A"},
			Jurisdiction: &domain.Jurisdiction{JurisdictionID: "rn", JurisdictionName: "B"}, AttributesChanged: true},
		{ItemNo: 4, Outcome: domain.OutcomeRetired, Jurisdiction: &domain.Jurisdiction{JurisdictionID: "gone"}},
	}
	h, pub := importRouter(st, permitAll())
	rr := postJSON(t, h, "/v1/admin/jurisdiction-imports/b-1/approve", map[string]any{"reason": "checked against the source"})
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if st.gotDecide.ActorID != testPrincipal || st.gotDecide.BatchID != "b-1" || st.gotDecide.Reason != "checked against the source" {
		t.Errorf("params = %+v", st.gotDecide)
	}
	want := []string{"jurisdiction.created", "jurisdiction.hierarchy.changed", "jurisdiction.updated", "jurisdiction.updated", "jurisdiction.deactivated"}
	if strings.Join(pub.emitted, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", pub.emitted, want)
	}
	if len(pub.moves) != 1 || pub.moves[0].previous == nil || *pub.moves[0].previous != "p-a" || *pub.moves[0].current != "p-b" ||
		pub.moves[0].src == nil || pub.moves[0].src.BatchID != "b-1" || pub.moves[0].src.SourceVersion != "2026-1" || pub.moves[0].src.ImportHash == "" {
		t.Errorf("hierarchy change = %+v, want the old and new parent and the import it came from", pub.moves)
	}
	if len(pub.changes) != 2 || pub.changes[0].Type != events.ChangeAttributesChanged || pub.changes[0].Before == nil || pub.changes[0].Import == nil {
		t.Errorf("updated changes = %+v", pub.changes)
	}
}

func TestImport_AChangeThatChangedNothingEmitsNothing(t *testing.T) {
	st := newImportStub()
	st.changes = nil
	h, pub := importRouter(st, permitAll())
	if rr := postJSON(t, h, "/v1/admin/jurisdiction-imports/b-1/approve", nil); rr.Code != http.StatusOK || len(pub.emitted) != 0 {
		t.Errorf("%d events %v, want 200 and none", rr.Code, pub.emitted)
	}
}

func TestImport_ApproveRefusalsAreMappedAndEmitNothing(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"the proposer":        {domain.ErrNotIndependent, http.StatusForbidden, "segregation_of_duties"},
		"already decided":     {domain.ErrImportDecided, http.StatusConflict, "import_already_decided"},
		"unknown":             {domain.ErrImportNotFound, http.StatusNotFound, "import_not_found"},
		"a row failed":        {domain.ErrImportHasInvalidRows, http.StatusUnprocessableEntity, "import_has_invalid_rows"},
		"a row cannot apply":  {&domain.ImportItemError{ItemNo: 3, Message: "parent: no active jurisdiction is COUNTRY \"ZZ\""}, http.StatusUnprocessableEntity, "import_row_failed"},
		"store unavailable":   {domain.ErrStoreUnavailable, http.StatusServiceUnavailable, "store_unavailable"},
		"conflicting version": {domain.ErrImportVersionConflict, http.StatusConflict, "import_version_conflict"},
	} {
		st := newImportStub()
		st.approveErr = c.err
		st.changes = []domain.AppliedChange{{ItemNo: 1, Outcome: domain.OutcomeAdded, Jurisdiction: &domain.Jurisdiction{JurisdictionID: "x"}}}
		h, pub := importRouter(st, permitAll())
		rr := postJSON(t, h, "/v1/admin/jurisdiction-imports/b-1/approve", nil)
		if rr.Code != c.status || problemCode(t, rr) != c.code {
			t.Errorf("%s: %d %q, want %d %q", name, rr.Code, problemCode(t, rr), c.status, c.code)
		}
		if len(pub.emitted) != 0 {
			t.Errorf("%s: a refused approval emitted %v", name, pub.emitted)
		}
	}

	// The row that failed is named in the body.
	st := newImportStub()
	st.approveErr = &domain.ImportItemError{ItemNo: 3, Message: "boom"}
	h, _ := importRouter(st, permitAll())
	if body := decodeMap(t, postJSON(t, h, "/v1/admin/jurisdiction-imports/b-1/approve", nil)); body["item_no"] != float64(3) || body["message"] != "boom" {
		t.Errorf("body = %v, want the failing row named", body)
	}
}

func TestImport_AnAnnouncementFailureFailsTheApproval(t *testing.T) {
	st := newImportStub()
	st.changes = []domain.AppliedChange{{ItemNo: 1, Outcome: domain.OutcomeAdded, Jurisdiction: &domain.Jurisdiction{JurisdictionID: "x"}}}
	h, pub := importRouter(st, permitAll())
	pub.err = domain.ErrStoreUnavailable
	if rr := postJSON(t, h, "/v1/admin/jurisdiction-imports/b-1/approve", nil); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("%d, want 503: an approval whose events cannot be recorded must not succeed", rr.Code)
	}
}

// ── reject, read ────────────────────────────────────────────────────────────

func TestImport_RejectCarriesTheReason(t *testing.T) {
	st := newImportStub()
	h, _ := importRouter(st, permitAll())
	rr := postJSON(t, h, "/v1/admin/jurisdiction-imports/b-1/reject", map[string]any{"reason": "wrong file"})
	if rr.Code != http.StatusOK || st.gotReject.Reason != "wrong file" || st.gotReject.ActorID != testPrincipal || st.gotReject.BatchID != "b-1" {
		t.Errorf("%d params %+v", rr.Code, st.gotReject)
	}
	st.rejectErr = domain.ErrInvalidImport
	h2, _ := importRouter(st, permitAll())
	if rr := postJSON(t, h2, "/v1/admin/jurisdiction-imports/b-1/reject", nil); rr.Code != http.StatusBadRequest || problemCode(t, rr) != "invalid_import" {
		t.Errorf("no reason: %d %q", rr.Code, problemCode(t, rr))
	}
	st.rejectErr = domain.ErrImportDecided
	h3, _ := importRouter(st, permitAll())
	if rr := postJSON(t, h3, "/v1/admin/jurisdiction-imports/b-1/reject", map[string]any{"reason": "x"}); rr.Code != http.StatusConflict {
		t.Errorf("decided: %d", rr.Code)
	}
}

func TestImport_GetAndList(t *testing.T) {
	st := newImportStub()
	st.list = []*domain.ImportBatch{{BatchID: "b-1"}}
	h, _ := importRouter(st, permitAll())
	if rr := getAs(t, h, "/v1/admin/jurisdiction-imports/b-1", testPrincipal); rr.Code != http.StatusOK || st.gotGet != "b-1" {
		t.Errorf("get: %d id=%q", rr.Code, st.gotGet)
	}
	st.getErr = domain.ErrImportNotFound
	h2, _ := importRouter(st, permitAll())
	if rr := getAs(t, h2, "/v1/admin/jurisdiction-imports/nope", testPrincipal); rr.Code != http.StatusNotFound || problemCode(t, rr) != "import_not_found" {
		t.Errorf("unknown: %d %q", rr.Code, problemCode(t, rr))
	}
	if rr := getAs(t, h, "/v1/admin/jurisdiction-imports?status=PROPOSED&limit=5", testPrincipal); rr.Code != http.StatusOK || st.gotSt != "PROPOSED" || st.gotLim != 5 {
		t.Errorf("list: %d status=%q limit=%d", rr.Code, st.gotSt, st.gotLim)
	}
	for _, bad := range []string{"?status=DONE", "?limit=0", "?limit=201", "?limit=x"} {
		if rr := getAs(t, h, "/v1/admin/jurisdiction-imports"+bad, testPrincipal); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, rr.Code)
		}
	}
}

// ── who may ─────────────────────────────────────────────────────────────────

// Each route asks for its own permission, and a missing or denied caller never
// reaches the store.
func TestImport_EachRouteAsksForItsOwnPermission(t *testing.T) {
	for _, c := range []struct {
		method, path, action string
	}{
		{http.MethodPost, "/v1/admin/jurisdiction-imports", "import_propose"},
		{http.MethodGet, "/v1/admin/jurisdiction-imports", "import_view"},
		{http.MethodGet, "/v1/admin/jurisdiction-imports/b-1", "import_view"},
		{http.MethodPost, "/v1/admin/jurisdiction-imports/b-1/approve", "import_approve"},
		{http.MethodPost, "/v1/admin/jurisdiction-imports/b-1/reject", "import_approve"},
	} {
		st := newImportStub()
		a := &mockAuthZ{}
		h, _ := importRouter(st, a)
		var rr *httptest.ResponseRecorder
		if c.method == http.MethodPost {
			rr = postJSON(t, h, c.path, proposeBody)
		} else {
			rr = getAs(t, h, c.path, testPrincipal)
		}
		if rr.Code == http.StatusForbidden || a.resource != "jurisdiction" || a.action != c.action || a.principalID != testPrincipal {
			t.Errorf("%s %s: asked %q/%q as %q (%d), want jurisdiction/%s as the caller", c.method, c.path, a.resource, a.action, a.principalID, rr.Code, c.action)
		}

		st = newImportStub()
		h, _ = importRouter(st, &mockAuthZ{err: authz.ErrUnauthorized})
		if c.method == http.MethodPost {
			rr = postJSON(t, h, c.path, proposeBody)
		} else {
			rr = getAs(t, h, c.path, testPrincipal)
		}
		if rr.Code != http.StatusForbidden || st.gotPropose.ActorID != "" || st.gotDecide.ActorID != "" || st.gotReject.ActorID != "" || st.gotGet != "" {
			t.Errorf("%s %s denied: %d, store reached", c.method, c.path, rr.Code)
		}

		st = newImportStub()
		h, _ = importRouter(st, permitAll())
		if c.method == http.MethodPost {
			rr = postJSONAs(t, h, c.path, proposeBody, "")
		} else {
			rr = getAs(t, h, c.path, "")
		}
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no principal: %d, want 401", c.method, c.path, rr.Code)
		}
	}
}
