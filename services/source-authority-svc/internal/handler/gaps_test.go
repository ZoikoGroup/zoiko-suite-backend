package handler

// Tests for the gaps closed in this pass. Each one fails against the previous
// behaviour, which is the only reason to write it down.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/source-authority-svc/internal/authz"
	"zoiko.io/source-authority-svc/internal/domain"
)

// ── GAP 1: normalized facts had no tenant, and resolve read across all of them ─

// The headline gap. normalized_facts had no tenant column, so one pool held
// every tenant's business facts, and GET /resolve composed one caller's query
// over all of them. entity_ref is free text that no registry constrains, so
// guessing one was the entire access control.
func TestResolve_DoesNotReadAnotherTenantsFacts(t *testing.T) {
	h, _, _ := newTestHandler()

	// Tenant A records a fact and a precedence rule for it.
	ra := newTestRouterTenant(h, "tenant-a")
	createMap(t, ra, "PAYROLL_GROSS_PAY", "ADP", 1)
	recordFact(t, ra, "PAYROLL_GROSS_PAY", "emp-1", "ADP", `"120000"`, "2026-01-05T00:00:00Z")

	// Tenant B asks the same question. The precedence rule is platform-wide
	// reference data and is legitimately shared; the FACT is not.
	rb := newTestRouterTenant(h, "tenant-b")
	w := do(rb, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=PAYROLL_GROSS_PAY&entity_ref=emp-1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", w.Code, w.Body.String())
	}
	var res domain.FactResolution
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.AuthoritativeFact != nil {
		t.Fatalf("resolve returned another tenant's fact: %s = %s",
			res.AuthoritativeFact.EntityRef, string(res.AuthoritativeFact.FactValue))
	}
	if len(res.ConflictingFacts) != 0 {
		t.Fatalf("resolve leaked %d of another tenant's facts as conflicts", len(res.ConflictingFacts))
	}
}

func TestListNormalizedFacts_DoesNotReadAnotherTenantsFacts(t *testing.T) {
	h, _, _ := newTestHandler()

	ra := newTestRouterTenant(h, "tenant-a")
	recordFact(t, ra, "PAYROLL_GROSS_PAY", "emp-1", "ADP", `"120000"`, "2026-01-05T00:00:00Z")

	rb := newTestRouterTenant(h, "tenant-b")
	w := do(rb, buildRequest(http.MethodGet, "/v1/normalized-facts?field_family=PAYROLL_GROSS_PAY&entity_ref=emp-1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", w.Code, w.Body.String())
	}
	var facts []domain.NormalizedFact
	_ = json.Unmarshal(w.Body.Bytes(), &facts)
	if len(facts) != 0 {
		t.Fatalf("fact history returned %d rows belonging to another tenant", len(facts))
	}
}

// A read with no tenant at all is 401, not an answer.
//
// This matters specifically on the READ path: the envelope middleware defaults
// to write-strict, which admits reads with no envelope, so nothing upstream of
// the handler guarantees a tenant on GET /resolve.
func TestRoutes_MissingTenantIs401(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouterTenant(h, "") // no tenant installed

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/source-authority/resolve?field_family=F&entity_ref=e-1", nil},
		{http.MethodGet, "/v1/normalized-facts", nil},
		{http.MethodPost, "/v1/normalized-facts", domain.RecordFactRequest{
			FieldFamily: "F", EntityRef: "e-1", SourceSystem: "S", SourceRecord: "r",
			FactValue: json.RawMessage(`"v"`), ObservedAt: "2026-01-05T00:00:00Z",
			CorrelationID: uuid.NewString(),
		}},
	}
	for _, c := range cases {
		w := do(r, buildRequest(c.method, c.path, c.body))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401 got %d: %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
}

// The precedence rules stay platform-wide: superseding tenant scope onto them
// would be the wrong fix, and this pins that they remain shared.
func TestListSourceAuthorityMaps_RulesAreSharedAcrossTenants(t *testing.T) {
	h, _, _ := newTestHandler()

	ra := newTestRouterTenant(h, "tenant-a")
	createMap(t, ra, "PAYROLL_GROSS_PAY", "ADP", 1)

	rb := newTestRouterTenant(h, "tenant-b")
	w := do(rb, buildRequest(http.MethodGet, "/v1/source-authority-maps?field_family=PAYROLL_GROSS_PAY", nil))
	var maps []domain.SourceAuthorityMap
	_ = json.Unmarshal(w.Body.Bytes(), &maps)
	if len(maps) != 1 {
		t.Fatalf("precedence rules are platform reference data and must be visible to every tenant, got %d", len(maps))
	}
}

// ── GAP 2: the precedence register was readable by anyone ────────────────────

// ListSourceAuthorityMaps ran no authorization at all. The rows are not tenant
// data, but they are the platform's trust topology, and that is a disclosure
// whether or not the rows belong to anyone.
func TestListSourceAuthorityMaps_RequiresViewGrant(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(&stubStore{}, &stubPublisher{}, &stubAuthz{err: errDenied()}, logger)
	r := newTestRouter(h)

	w := do(r, buildRequest(http.MethodGet, "/v1/source-authority-maps?field_family=X", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", w.Code, w.Body.String())
	}
}

func TestListSourceAuthorityMaps_RequiresPrincipal(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	req := buildRequest(http.MethodGet, "/v1/source-authority-maps?field_family=X", nil)
	req.Header.Del("X-Principal-Id")
	if w := do(r, req); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d: %s", w.Code, w.Body.String())
	}
}

func TestListNormalizedFacts_RequiresViewGrant(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(&stubStore{}, &stubPublisher{}, &stubAuthz{err: errDenied()}, logger)
	r := newTestRouter(h)

	w := do(r, buildRequest(http.MethodGet, "/v1/normalized-facts?field_family=X&entity_ref=e-1", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", w.Code, w.Body.String())
	}
}

// ── GAP 3: correlation_id was in the wire contract and read by nothing ───────

// A retried fact POST appended a SECOND observation. On an append-only log
// whose whole purpose is to be an exact account of what each source said, that
// is an observation no source ever made.
func TestRecordFact_IsIdempotentOnCorrelationID(t *testing.T) {
	h, st, _ := newTestHandler()
	r := newTestRouter(h)

	correlationID := uuid.NewString()
	body := domain.RecordFactRequest{
		FieldFamily: "PAYROLL_GROSS_PAY", EntityRef: "emp-1", SourceSystem: "ADP",
		SourceRecord: "rec-1", FactValue: json.RawMessage(`"120000"`),
		ObservedAt: "2026-01-05T00:00:00Z", CorrelationID: correlationID,
	}

	first := do(r, buildRequest(http.MethodPost, "/v1/normalized-facts", body))
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", first.Code, first.Body.String())
	}
	second := do(r, buildRequest(http.MethodPost, "/v1/normalized-facts", body))
	if second.Code != http.StatusOK {
		t.Fatalf("a replay must answer 200, not a second creation, got %d: %s", second.Code, second.Body.String())
	}

	var a, b domain.NormalizedFact
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(second.Body.Bytes(), &b)
	if a.NormalizedFactID != b.NormalizedFactID {
		t.Fatalf("replay resolved to a different fact (%s) than the original (%s)", b.NormalizedFactID, a.NormalizedFactID)
	}
	if len(st.facts) != 1 {
		t.Fatalf("a retried POST appended a duplicate observation: %d rows", len(st.facts))
	}
}

// The map path has the mirror-image failure: the uniqueness index on
// (field_family, source_system, effective_from) turned a retry into a 409, so
// an operator was told their rule conflicted with the one they had just
// successfully created.
func TestCreateSourceAuthorityMap_ReplayIsNotAConflict(t *testing.T) {
	h, st, _ := newTestHandler()
	r := newTestRouter(h)

	correlationID := uuid.NewString()
	body := domain.CreateSourceAuthorityMapRequest{
		FieldFamily: "PAYROLL_GROSS_PAY", SourceSystem: "ADP", PrecedenceRank: 1,
		ConflictRoute: "route to Data Governance", EffectiveFrom: "2026-01-01T00:00:00Z",
		CorrelationID: correlationID,
	}

	first := do(r, buildRequest(http.MethodPost, "/v1/source-authority-maps", body))
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", first.Code, first.Body.String())
	}
	second := do(r, buildRequest(http.MethodPost, "/v1/source-authority-maps", body))
	if second.Code != http.StatusOK {
		t.Fatalf("a replay must answer 200, not 409, got %d: %s", second.Code, second.Body.String())
	}
	if len(st.maps) != 1 {
		t.Fatalf("a retried POST created a second rule: %d rows", len(st.maps))
	}
}

func TestWrites_RequireCorrelationID(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := do(r, buildRequest(http.MethodPost, "/v1/normalized-facts", domain.RecordFactRequest{
		FieldFamily: "F", EntityRef: "e-1", SourceSystem: "S", SourceRecord: "r",
		FactValue: json.RawMessage(`"v"`), ObservedAt: "2026-01-05T00:00:00Z",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a fact with no correlation_id, got %d", w.Code)
	}

	w = do(r, buildRequest(http.MethodPost, "/v1/source-authority-maps", domain.CreateSourceAuthorityMapRequest{
		FieldFamily: "F", SourceSystem: "S", PrecedenceRank: 1,
		ConflictRoute: "x", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a rule with no correlation_id, got %d", w.Code)
	}
}

// ── GAP 4: a precedence rule could never be ended ────────────────────────────

// effective_to existed from the first migration and the resolver honoured it,
// but nothing could set it. The documented way to change a precedence — "a
// changed precedence is a new row" — therefore left BOTH rows in force, the
// resolver ranked the same source twice and took the better rank, and the
// demotion silently did nothing.
func TestSupersede_DemotingASourceActuallyTakesEffect(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	// ADP is trusted above NetSuite, and they disagree.
	adp := createMap(t, r, "BILLING_CONTACT_EMAIL", "ADP", 1)
	createMap(t, r, "BILLING_CONTACT_EMAIL", "NetSuite", 2)
	recordFact(t, r, "BILLING_CONTACT_EMAIL", "acct-1", "ADP", `"stale@acme.com"`, "2026-01-05T00:00:00Z")
	recordFact(t, r, "BILLING_CONTACT_EMAIL", "acct-1", "NetSuite", `"billing@acme.com"`, "2026-01-05T00:00:00Z")

	w := do(r, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=BILLING_CONTACT_EMAIL&entity_ref=acct-1", nil))
	var before domain.FactResolution
	_ = json.Unmarshal(w.Body.Bytes(), &before)
	if before.AuthoritativeFact == nil || before.AuthoritativeFact.SourceSystem != "ADP" {
		t.Fatalf("setup: expected ADP to win at rank 1, got %+v", before.AuthoritativeFact)
	}

	// End ADP's rule. This is the operation that did not exist.
	sw := do(r, buildRequest(http.MethodPost, "/v1/source-authority-maps/"+adp.SourceAuthorityMapID+"/supersede",
		domain.SupersedeSourceAuthorityMapRequest{CorrelationID: uuid.NewString()}))
	if sw.Code != http.StatusOK {
		t.Fatalf("expected 200 superseding, got %d: %s", sw.Code, sw.Body.String())
	}
	var superseded domain.SourceAuthorityMap
	_ = json.Unmarshal(sw.Body.Bytes(), &superseded)
	if superseded.EffectiveTo == nil {
		t.Fatal("supersede must set effective_to")
	}
	if superseded.SupersededByPrincipalID == nil || *superseded.SupersededByPrincipalID == "" {
		t.Fatal("supersede must record who ended the rule")
	}

	// NetSuite now wins, and ADP's fact is no longer ranked at all.
	w = do(r, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=BILLING_CONTACT_EMAIL&entity_ref=acct-1", nil))
	var after domain.FactResolution
	_ = json.Unmarshal(w.Body.Bytes(), &after)
	if after.AuthoritativeFact == nil || after.AuthoritativeFact.SourceSystem != "NetSuite" {
		t.Fatalf("after superseding ADP's rule, NetSuite should win, got %+v", after.AuthoritativeFact)
	}
}

// Superseding a rule twice is a 409, not a second success: the window is
// already closed, and reporting a fresh supersession would misattribute who
// ended it.
func TestSupersede_AlreadySupersededIsConflict(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	m := createMap(t, r, "F", "S", 1)
	path := "/v1/source-authority-maps/" + m.SourceAuthorityMapID + "/supersede"

	if w := do(r, buildRequest(http.MethodPost, path, domain.SupersedeSourceAuthorityMapRequest{CorrelationID: uuid.NewString()})); w.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", w.Code, w.Body.String())
	}
	w := do(r, buildRequest(http.MethodPost, path, domain.SupersedeSourceAuthorityMapRequest{CorrelationID: uuid.NewString()}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d: %s", w.Code, w.Body.String())
	}
}

// Back-dating the end of a rule rewrites which rule was in force when an
// earlier resolution was made, so a decision already taken stops being
// explainable by the register.
func TestSupersede_RefusesAPastEffectiveTo(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	m := createMap(t, r, "F", "S", 1)
	w := do(r, buildRequest(http.MethodPost, "/v1/source-authority-maps/"+m.SourceAuthorityMapID+"/supersede",
		domain.SupersedeSourceAuthorityMapRequest{
			EffectiveTo:   time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339),
			CorrelationID: uuid.NewString(),
		}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "must not be in the past") {
		t.Fatalf("expected the past-dating refusal, got %s", w.Body.String())
	}
}

func TestSupersede_UnknownMapIs404(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := do(r, buildRequest(http.MethodPost, "/v1/source-authority-maps/"+uuid.NewString()+"/supersede",
		domain.SupersedeSourceAuthorityMapRequest{CorrelationID: uuid.NewString()}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d: %s", w.Code, w.Body.String())
	}
}

// A superseded rule leaves the default list and stays reachable in the history.
func TestList_SupersededRuleLeavesTheInForceList(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	m := createMap(t, r, "F", "S", 1)
	_ = do(r, buildRequest(http.MethodPost, "/v1/source-authority-maps/"+m.SourceAuthorityMapID+"/supersede",
		domain.SupersedeSourceAuthorityMapRequest{CorrelationID: uuid.NewString()}))

	var inForce []domain.SourceAuthorityMap
	_ = json.Unmarshal(do(r, buildRequest(http.MethodGet, "/v1/source-authority-maps?field_family=F", nil)).Body.Bytes(), &inForce)
	if len(inForce) != 0 {
		t.Fatalf("a superseded rule must not appear in the in-force list, got %d", len(inForce))
	}

	var history []domain.SourceAuthorityMap
	_ = json.Unmarshal(do(r, buildRequest(http.MethodGet, "/v1/source-authority-maps?field_family=F&include_superseded=true", nil)).Body.Bytes(), &history)
	if len(history) != 1 {
		t.Fatalf("the history must still carry the ended rule, got %d", len(history))
	}
}

// ── GAP 5: an unmapped source vanished from the resolution ───────────────────

// The resolver INNER JOINed facts to precedence rules, so a source that had
// reported a fact but had no rule was not outranked — it was invisible, and its
// disagreement with the winner never surfaced. On a register built to surface
// disagreement that is the one confusion it cannot afford.
func TestResolve_ReportsSourcesWithNoPrecedenceRule(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	createMap(t, r, "HR_EMPLOYMENT_STATUS", "Kriton", 1)
	recordFact(t, r, "HR_EMPLOYMENT_STATUS", "emp-1", "Kriton", `"ACTIVE"`, "2026-01-05T00:00:00Z")
	// Nobody has ranked this source yet, and it disagrees.
	recordFact(t, r, "HR_EMPLOYMENT_STATUS", "emp-1", "UnrankedHRIS", `"TERMINATED"`, "2026-01-06T00:00:00Z")

	w := do(r, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=HR_EMPLOYMENT_STATUS&entity_ref=emp-1", nil))
	var res domain.FactResolution
	_ = json.Unmarshal(w.Body.Bytes(), &res)

	if res.AuthoritativeFact == nil || res.AuthoritativeFact.SourceSystem != "Kriton" {
		t.Fatalf("the ranked source should still resolve, got %+v", res.AuthoritativeFact)
	}
	if len(res.UnmappedSources) != 1 || res.UnmappedSources[0] != "UnrankedHRIS" {
		t.Fatalf("a source reporting facts with no precedence rule must be named, got %v", res.UnmappedSources)
	}
}

// ── GAP 6: authority_class was unvalidated free text ─────────────────────────

func TestRecordFact_UnknownAuthorityClassIsRejected(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := do(r, buildRequest(http.MethodPost, "/v1/normalized-facts", domain.RecordFactRequest{
		FieldFamily: "F", EntityRef: "e-1", SourceSystem: "S", SourceRecord: "r",
		FactValue: json.RawMessage(`"v"`), ObservedAt: "2026-01-05T00:00:00Z",
		AuthorityClass: "AUTHORATIVE", // misspelled
		CorrelationID:  uuid.NewString(),
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AUTHORITATIVE, DERIVED, CACHED") {
		t.Fatalf("the refusal should name the vocabulary, got %s", w.Body.String())
	}
}

func TestRecordFact_AcceptsEveryDeclaredAuthorityClass(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	for _, class := range []string{"AUTHORITATIVE", "DERIVED", "CACHED"} {
		w := do(r, buildRequest(http.MethodPost, "/v1/normalized-facts", domain.RecordFactRequest{
			FieldFamily: "F", EntityRef: "e-1", SourceSystem: class, SourceRecord: "r",
			FactValue: json.RawMessage(`"v"`), ObservedAt: "2026-01-05T00:00:00Z",
			AuthorityClass: class, CorrelationID: uuid.NewString(),
		}))
		if w.Code != http.StatusCreated {
			t.Fatalf("%s: expected 201 got %d: %s", class, w.Code, w.Body.String())
		}
	}
}

// ── GAP 7: an unknown field was discarded in silence ─────────────────────────

// Worse here than the usual case: authority_class, effective_at and
// allowed_correction_path are all optional or defaulted, so a typo in any of
// them stored a row that differed from what the caller believed they sent.
func TestRecordFact_UnknownFieldIsRejected(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	body := map[string]any{
		"field_family": "F", "entity_ref": "e-1", "source_system": "S", "source_record": "r",
		"fact_value": "v", "observed_at": "2026-01-05T00:00:00Z",
		"correlation_id": uuid.NewString(),
		"effective_att":  "2026-02-05T00:00:00Z", // misspelled
	}
	if w := do(r, buildRequest(http.MethodPost, "/v1/normalized-facts", body)); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSourceAuthorityMap_UnknownFieldIsRejected(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	body := map[string]any{
		"field_family": "F", "source_system": "S", "precedence_rank": 1,
		"conflict_route": "x", "effective_from": "2026-01-01T00:00:00Z",
		"correlation_id":   uuid.NewString(),
		"precedence_ranks": 2, // misspelled
	}
	if w := do(r, buildRequest(http.MethodPost, "/v1/source-authority-maps", body)); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", w.Code, w.Body.String())
	}
}

// ── GAP 8: paging ────────────────────────────────────────────────────────────

func TestList_PagingValidation(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	for _, q := range []string{"limit=abc", "limit=0", "limit=501", "offset=-1"} {
		if w := do(r, buildRequest(http.MethodGet, "/v1/source-authority-maps?"+q, nil)); w.Code != http.StatusBadRequest {
			t.Fatalf("maps %s: expected 400 got %d", q, w.Code)
		}
		if w := do(r, buildRequest(http.MethodGet, "/v1/normalized-facts?"+q, nil)); w.Code != http.StatusBadRequest {
			t.Fatalf("facts %s: expected 400 got %d", q, w.Code)
		}
	}
}

func TestList_LimitIsApplied(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	createMap(t, r, "F", "SourceA", 1)
	createMap(t, r, "F", "SourceB", 2)

	var maps []domain.SourceAuthorityMap
	_ = json.Unmarshal(do(r, buildRequest(http.MethodGet, "/v1/source-authority-maps?field_family=F&limit=1", nil)).Body.Bytes(), &maps)
	if len(maps) != 1 {
		t.Fatalf("limit=1 should return one rule, got %d", len(maps))
	}
}

// A misspelled boolean must not read as false. Answering a request for the
// full history with the narrow in-force list reads as an answer.
func TestList_UnparseableIncludeSupersededIsRejected(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	if w := do(r, buildRequest(http.MethodGet, "/v1/source-authority-maps?include_superseded=yes", nil)); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", w.Code, w.Body.String())
	}
}

// ── GAP 9: the register could not be browsed ─────────────────────────────────

// field_family was a required query parameter, so a caller had to already know
// the exact family string to see anything at all — and a typo answered with an
// empty list rather than a refusal.
func TestListSourceAuthorityMaps_FieldFamilyIsOptional(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	createMap(t, r, "PAYROLL_GROSS_PAY", "ADP", 1)
	createMap(t, r, "HR_EMPLOYMENT_STATUS", "Kriton", 1)

	w := do(r, buildRequest(http.MethodGet, "/v1/source-authority-maps", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", w.Code, w.Body.String())
	}
	var maps []domain.SourceAuthorityMap
	_ = json.Unmarshal(w.Body.Bytes(), &maps)
	if len(maps) != 2 {
		t.Fatalf("an unfiltered read should return the whole register, got %d", len(maps))
	}
}

func errDenied() error { return authzpkg.ErrAuthorizationDenied }
