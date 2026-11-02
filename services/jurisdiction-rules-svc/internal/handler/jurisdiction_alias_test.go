package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// REF-01 aliases and code resolution at the HTTP layer: every refusal is its own
// status and code, the actor is the verified caller, and a replay emits nothing.
// The SQL and the constraint are proven on a real database in internal/store.

type aliasStub struct {
	*stubStore

	resolved   *domain.CodeResolution
	resolveErr error
	gotQuery   domain.ResolveQuery

	aliases    []*domain.JurisdictionAlias
	listErr    error
	gotListAt  *time.Time
	gotListID  string
	added      *domain.JurisdictionAlias
	addCreated bool
	addErr     error
	gotAdd     domain.AddAliasParams
	ended      *domain.JurisdictionAlias
	endChanged bool
	endErr     error
	gotEnd     domain.EndAliasParams
}

func (s *aliasStub) ResolveCode(_ context.Context, q domain.ResolveQuery) (*domain.CodeResolution, error) {
	s.gotQuery = q
	return s.resolved, s.resolveErr
}

func (s *aliasStub) ListJurisdictionAliases(_ context.Context, id string, at *time.Time) ([]*domain.JurisdictionAlias, error) {
	s.gotListID, s.gotListAt = id, at
	return s.aliases, s.listErr
}

func (s *aliasStub) AddJurisdictionAliasWithQuerier(_ context.Context, _ store.Querier, p domain.AddAliasParams) (*domain.JurisdictionAlias, bool, *domain.Jurisdiction, error) {
	s.gotAdd = p
	return s.added, s.addCreated, &domain.Jurisdiction{JurisdictionID: p.JurisdictionID, RowVersion: 4}, s.addErr
}

func (s *aliasStub) EndJurisdictionAliasWithQuerier(_ context.Context, _ store.Querier, p domain.EndAliasParams) (*domain.JurisdictionAlias, bool, *domain.Jurisdiction, error) {
	s.gotEnd = p
	return s.ended, s.endChanged, &domain.Jurisdiction{JurisdictionID: p.JurisdictionID, RowVersion: 5}, s.endErr
}

// updateSpy is the shared spy plus the optional jurisdiction.updated capability.
type updateSpy struct {
	*spyPublisher
	changes []events.JurisdictionChange
}

func (p *updateSpy) PublishJurisdictionUpdated(_ context.Context, _ domain.Jurisdiction, c events.JurisdictionChange, _ string) error {
	p.changes = append(p.changes, c)
	return p.record("jurisdiction.updated")
}

func newAliasStub() *aliasStub {
	return &aliasStub{
		stubStore:  &stubStore{},
		resolved:   &domain.CodeResolution{JurisdictionID: "j-1", JurisdictionCode: "GB", MatchedVia: domain.SystemISO3166Alpha2},
		added:      &domain.JurisdictionAlias{AliasID: "a-1", JurisdictionID: "j-1", AliasSystem: "SOME-ERP", AliasCode: "X"},
		addCreated: true,
		ended:      &domain.JurisdictionAlias{AliasID: "a-1", JurisdictionID: "j-1"},
		endChanged: true,
	}
}

func aliasRouter(st *aliasStub) (http.Handler, *updateSpy) {
	pub := &updateSpy{spyPublisher: &spyPublisher{}}
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(st, permitAll(), pub, testAuthzScopeID, zap.NewNop()))
	return r, pub
}

func decodeMap(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return m
}

// ── resolution ──────────────────────────────────────────────────────────────

func TestResolve_ParametersReachTheStoreNormalisedByTheStoreNotTheHandler(t *testing.T) {
	st := newAliasStub()
	h, _ := aliasRouter(st)
	rr := get(t, h, "/v1/jurisdiction-resolutions?code=gb&system=ISO3166-1-ALPHA2&type=COUNTRY&as_of=2026-03-31")
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	q := st.gotQuery
	if q.Code != "gb" || q.System != "ISO3166-1-ALPHA2" || q.Type != "COUNTRY" || !q.AsOf.Equal(time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("query = %+v", q)
	}
	if body := decodeMap(t, rr); body["matched_via"] != "ISO3166-1-ALPHA2" {
		t.Errorf("body = %v", body)
	}
}

func TestResolve_InputRefusals(t *testing.T) {
	for name, c := range map[string]struct{ path, code string }{
		"no code":                {"/v1/jurisdiction-resolutions", "missing_field"},
		"blank code":             {"/v1/jurisdiction-resolutions?code=%20", "missing_field"},
		"bad as_of":              {"/v1/jurisdiction-resolutions?code=GB&as_of=yesterday", "invalid_as_of"},
		"over-long code":         {"/v1/jurisdiction-resolutions?code=" + strings.Repeat("A", 65), "input_too_long"},
		"mapping with no system": {"/v1/jurisdiction-code-mappings?code=GB", "missing_field"},
		"mapping as INTERNAL":    {"/v1/jurisdiction-code-mappings?code=GB&system=INTERNAL", "missing_field"},
	} {
		st := newAliasStub()
		h, _ := aliasRouter(st)
		rr := get(t, h, c.path)
		if rr.Code != http.StatusBadRequest || problemCode(t, rr) != c.code {
			t.Errorf("%s: %d %q, want 400 %q", name, rr.Code, problemCode(t, rr), c.code)
		}
		if st.gotQuery.Code != "" {
			t.Errorf("%s: the store was queried", name)
		}
	}
}

func TestResolve_EveryRefusalIsItsOwnAnswer(t *testing.T) {
	retiredAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	succ, reason := "j-2", "dissolved"
	cases := []struct {
		name   string
		err    error
		status int
		code   string
		check  func(map[string]any) string
	}{
		{"unknown", domain.ErrUnknownJurisdictionCode, http.StatusNotFound, "unknown_jurisdiction_code", nil},
		{"not mapped then", &domain.AliasNotEffectiveError{System: "S", Code: "C", AsOf: retiredAt, From: retiredAt.AddDate(1, 0, 0)}, http.StatusNotFound, "code_not_mapped_at", nil},
		{"retired", &domain.RetiredError{JurisdictionID: "j-1", Code: "OLD", RetiredAt: &retiredAt, SupersededBy: &succ, Reason: &reason}, http.StatusGone, "jurisdiction_retired",
			func(b map[string]any) string {
				if b["superseded_by_jurisdiction_id"] != "j-2" || b["retirement_reason"] != "dissolved" || b["jurisdiction_id"] != "j-1" || b["retired_at"] == nil {
					return "the retirement detail is missing from the body"
				}
				return ""
			}},
		{"ambiguous", &domain.AmbiguousError{Code: "GB", Candidates: []domain.AmbiguityCandidate{{JurisdictionID: "a", JurisdictionType: "COUNTRY"}, {JurisdictionID: "b", JurisdictionType: "TAX_AUTHORITY"}}},
			http.StatusConflict, "ambiguous_jurisdiction_code",
			func(b map[string]any) string {
				if c, _ := b["candidates"].([]any); len(c) != 2 {
					return "the candidates are missing from the body"
				}
				return ""
			}},
		{"not yet in force", &domain.NotEffectiveError{JurisdictionID: "j", AsOf: retiredAt, EffectiveFrom: retiredAt.AddDate(1, 0, 0)}, http.StatusNotFound, "jurisdiction_not_effective", nil},
		{"store down", domain.ErrStoreUnavailable, http.StatusServiceUnavailable, "store_unavailable", nil},
	}
	for _, c := range cases {
		st := newAliasStub()
		st.resolveErr = c.err
		h, _ := aliasRouter(st)
		rr := get(t, h, "/v1/jurisdiction-resolutions?code=GB")
		if rr.Code != c.status || problemCode(t, rr) != c.code {
			t.Errorf("%s: %d %q, want %d %q (%s)", c.name, rr.Code, problemCode(t, rr), c.status, c.code, rr.Body.String())
			continue
		}
		if c.check != nil {
			if msg := c.check(decodeMap(t, rr)); msg != "" {
				t.Errorf("%s: %s: %s", c.name, msg, rr.Body.String())
			}
		}
	}
}

func TestMapExternalCode_UsesTheSameResolutionWithTheSystemRequired(t *testing.T) {
	st := newAliasStub()
	h, _ := aliasRouter(st)
	rr := get(t, h, "/v1/jurisdiction-code-mappings?system=SOME-ERP&code=X-1")
	if rr.Code != http.StatusOK || st.gotQuery.System != "SOME-ERP" || st.gotQuery.Code != "X-1" {
		t.Errorf("%d query %+v", rr.Code, st.gotQuery)
	}
}

func TestListAliases(t *testing.T) {
	st := newAliasStub()
	st.aliases = []*domain.JurisdictionAlias{{AliasID: "a-1"}}
	h, _ := aliasRouter(st)
	if rr := get(t, h, "/v1/jurisdictions/j-1/aliases"); rr.Code != http.StatusOK || st.gotListAt != nil || st.gotListID != "j-1" {
		t.Errorf("history: %d at=%v id=%q", rr.Code, st.gotListAt, st.gotListID)
	}
	if rr := get(t, h, "/v1/jurisdictions/j-1/aliases?as_of=2026-01-01"); rr.Code != http.StatusOK || st.gotListAt == nil || st.gotListAt.Year() != 2026 {
		t.Errorf("as of: %d at=%v", rr.Code, st.gotListAt)
	}
	if rr := get(t, h, "/v1/jurisdictions/j-1/aliases?as_of=nope"); rr.Code != http.StatusBadRequest {
		t.Errorf("bad as_of: %d", rr.Code)
	}
	st.listErr = domain.ErrJurisdictionNotFound
	if rr := get(t, h, "/v1/jurisdictions/j-1/aliases"); rr.Code != http.StatusNotFound || problemCode(t, rr) != "jurisdiction_not_found" {
		t.Errorf("unknown jurisdiction: %d %q", rr.Code, problemCode(t, rr))
	}
}

// ── amendment ───────────────────────────────────────────────────────────────

var amendBody = map[string]any{
	"alias_system": "SOME-ERP", "alias_code": "X-1", "source_standard": "ERP code list", "source_version": "7",
	"mapping_rationale": "the ERP's code for this country", "evidence_ref": "ticket-9", "valid_from": "2026-01-01T00:00:00Z",
	"replaces_alias_id": "a-0",
}

func TestAmendAlias_CreatesAsTheVerifiedCallerAndEmitsJurisdictionUpdated(t *testing.T) {
	st := newAliasStub()
	h, pub := aliasRouter(st)
	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/aliases", amendBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	p := st.gotAdd
	if p.ActorID != testPrincipal || p.JurisdictionID != "j-1" || p.AliasSystem != "SOME-ERP" || p.ReplacesAliasID != "a-0" ||
		p.EvidenceRef != "ticket-9" || p.SourceVersion != "7" {
		t.Errorf("params = %+v, want the verified caller and the body", p)
	}
	if !pub.has("jurisdiction.updated") || len(pub.changes) != 1 || pub.changes[0].Type != events.ChangeAliasAdded || pub.changes[0].Alias == nil {
		t.Errorf("events = %v changes = %+v", pub.emitted, pub.changes)
	}
}

func TestAmendAlias_AReplayIs200AndEmitsNothing(t *testing.T) {
	st := newAliasStub()
	st.addCreated = false
	h, pub := aliasRouter(st)
	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/aliases", amendBody)
	if rr.Code != http.StatusOK || len(pub.emitted) != 0 {
		t.Errorf("%d events %v, want 200 and none", rr.Code, pub.emitted)
	}
}

func TestAmendAlias_RefusalsAreMapped(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"invalid":           {domain.ErrInvalidAlias, http.StatusBadRequest, "invalid_alias"},
		"conflict":          {domain.ErrAliasConflict, http.StatusConflict, "alias_conflict"},
		"unknown alias":     {domain.ErrAliasNotFound, http.StatusNotFound, "alias_not_found"},
		"unknown/retired":   {domain.ErrJurisdictionNotFound, http.StatusNotFound, "jurisdiction_not_found"},
		"input too long":    {domain.ErrInputTooLong, http.StatusBadRequest, "input_too_long"},
		"store unavailable": {domain.ErrStoreUnavailable, http.StatusServiceUnavailable, "store_unavailable"},
	} {
		st := newAliasStub()
		st.addErr = c.err
		h, pub := aliasRouter(st)
		rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/aliases", amendBody)
		if rr.Code != c.status || problemCode(t, rr) != c.code {
			t.Errorf("%s: %d %q, want %d %q", name, rr.Code, problemCode(t, rr), c.status, c.code)
		}
		if len(pub.emitted) != 0 {
			t.Errorf("%s: a refused amendment emitted %v", name, pub.emitted)
		}
	}
}

func TestAmendAlias_NeedsAnAuthenticatedAuthorizedCaller(t *testing.T) {
	st := newAliasStub()
	h, _ := aliasRouter(st)
	rr := postJSONAs(t, h, "/v1/admin/jurisdictions/j-1/aliases", amendBody, "")
	if rr.Code != http.StatusUnauthorized || st.gotAdd.AliasCode != "" {
		t.Errorf("no principal: %d, store reached=%v", rr.Code, st.gotAdd.AliasCode != "")
	}

	st = newAliasStub()
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(st, &mockAuthZ{err: authz.ErrUnauthorized}, &updateSpy{spyPublisher: &spyPublisher{}}, testAuthzScopeID, zap.NewNop()))
	rr = postJSON(t, r, "/v1/admin/jurisdictions/j-1/aliases", amendBody)
	if rr.Code != http.StatusForbidden || st.gotAdd.AliasCode != "" {
		t.Errorf("denied: %d, store reached=%v", rr.Code, st.gotAdd.AliasCode != "")
	}
}

func TestAmendAlias_MalformedBodyIs400(t *testing.T) {
	st := newAliasStub()
	h, _ := aliasRouter(st)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/jurisdictions/j-1/aliases", strings.NewReader("{not json"))
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("%d", rr.Code)
	}
}

func TestEndAlias(t *testing.T) {
	st := newAliasStub()
	h, pub := aliasRouter(st)
	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/aliases/a-1/end", map[string]any{"valid_to": "2026-06-01T00:00:00Z", "reason": "withdrawn"})
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	e := st.gotEnd
	if e.AliasID != "a-1" || e.JurisdictionID != "j-1" || e.ActorID != testPrincipal || e.Reason != "withdrawn" {
		t.Errorf("params = %+v", e)
	}
	if len(pub.changes) != 1 || pub.changes[0].Type != events.ChangeAliasEnded {
		t.Errorf("changes = %+v", pub.changes)
	}

	// A replay (already ended) answers 200 and emits nothing more.
	st.endChanged = false
	h2, pub2 := aliasRouter(st)
	if rr := postJSON(t, h2, "/v1/admin/jurisdictions/j-1/aliases/a-1/end", map[string]any{"valid_to": "2026-06-01T00:00:00Z", "reason": "x"}); rr.Code != http.StatusOK || len(pub2.emitted) != 0 {
		t.Errorf("replay: %d events %v", rr.Code, pub2.emitted)
	}
	st.endErr = domain.ErrAliasNotFound
	h3, _ := aliasRouter(st)
	if rr := postJSON(t, h3, "/v1/admin/jurisdictions/j-1/aliases/zzz/end", map[string]any{"valid_to": "2026-06-01T00:00:00Z", "reason": "x"}); rr.Code != http.StatusNotFound {
		t.Errorf("unknown alias: %d", rr.Code)
	}
}
