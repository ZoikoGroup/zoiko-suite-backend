package handler_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
)

// ── GET /v1/access-decisions ────────────────────────────────────────────────
//
// The audit read. §8.3 requires denials to be "evidentially retrievable", and
// by-id retrieval only serves a caller who already holds the id — which, for a
// denial, exists only in the response given to the service that was refused.
//
// These tests are about the filter surface rather than the SQL, which the store
// integration suite covers: what the handler REFUSES matters most here, because
// every refusal below is a case that would otherwise return an empty page, and
// an empty page from a typo is indistinguishable from a tenant that has denied
// nobody.

func getDecisions(t *testing.T, r chi.Router, query string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, handler.AccessDecisionsPath+query, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func auditHeaders() map[string]string {
	return map[string]string{"X-Principal-Id": "p-auditor", "X-Tenant-Id": "t-1"}
}

func emptyPageStore() *stubStore {
	return &stubStore{listDecisions: &domain.AccessDecisionPage{Decisions: []domain.AccessDecisionLog{}}}
}

func TestListAccessDecisions_RequiresPrincipal(t *testing.T) {
	r := newTestRouter(emptyPageStore())
	w := getDecisions(t, r, "", map[string]string{"X-Tenant-Id": "t-1"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListAccessDecisions_RequiresTenantScope(t *testing.T) {
	r := newTestRouter(emptyPageStore())
	w := getDecisions(t, r, "", map[string]string{"X-Principal-Id": "p-auditor"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no tenant: expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// The property that matters most on this route: the scope is the VERIFIED
// header, and no query parameter can widen it. A tenant_id query param is
// ignored outright rather than merged.
func TestListAccessDecisions_TenantComesFromHeaderNotQuery(t *testing.T) {
	store := emptyPageStore()
	r := newTestRouter(store)

	w := getDecisions(t, r, "?tenant_id=t-someone-else", auditHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.gotListDecisionsTenant != "t-1" {
		t.Fatalf("store was scoped to %q, want the verified header tenant %q — a query parameter widened the audit read",
			store.gotListDecisionsTenant, "t-1")
	}
}

func TestListAccessDecisions_ForwardsEveryFilter(t *testing.T) {
	store := emptyPageStore()
	r := newTestRouter(store)

	q := "?principal_id=p-9&decision_outcome=denied&action_type=PAYMENT_APPROVE" +
		"&legal_entity_id=11111111-1111-1111-1111-111111111111" +
		"&decided_from=2026-09-01T00:00:00Z&decided_to=2026-09-30T00:00:00Z&limit=25"
	w := getDecisions(t, r, q, auditHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	got := store.gotListDecisionsParams
	if got.PrincipalID != "p-9" {
		t.Errorf("principal_id = %q, want p-9", got.PrincipalID)
	}
	// Upper-cased by the handler, so a caller writing "denied" is not silently
	// answered with an empty page.
	if got.Outcome != "DENIED" {
		t.Errorf("decision_outcome = %q, want DENIED (upper-cased)", got.Outcome)
	}
	if got.ActionType != "PAYMENT_APPROVE" {
		t.Errorf("action_type = %q", got.ActionType)
	}
	if got.LegalEntityID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("legal_entity_id = %q", got.LegalEntityID)
	}
	if got.Limit != 25 {
		t.Errorf("limit = %d, want 25", got.Limit)
	}
	if got.DecidedFrom == nil || !got.DecidedFrom.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("decided_from = %v", got.DecidedFrom)
	}
	if got.DecidedTo == nil || !got.DecidedTo.Equal(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("decided_to = %v", got.DecidedTo)
	}
}

// An outcome that is neither GRANTED nor DENIED is refused, not passed through
// to match nothing. This is the single most important refusal on the route: a
// listing that is empty because somebody wrote "DENY" looks exactly like a
// tenant that has denied nobody.
func TestListAccessDecisions_RefusesUnknownOutcomeFilter(t *testing.T) {
	store := emptyPageStore()
	r := newTestRouter(store)

	w := getDecisions(t, r, "?decision_outcome=DENY", auditHeaders())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown outcome, got %d: %s", w.Code, w.Body.String())
	}
	if store.gotListDecisionsTenant != "" {
		t.Error("the store was queried despite an invalid filter — the refusal must happen before the read")
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["field"] != "decision_outcome" {
		t.Errorf("error names field %q, want decision_outcome — the caller has to know which filter was wrong", body["field"])
	}
}

// A malformed legal_entity_id must be a 400 that names the field, not the 503
// the store would produce comparing a non-UUID against a uuid column. See
// handler.validScope: known-gaps.md records this as a platform-wide habit of
// reporting a bad input as an outage.
func TestListAccessDecisions_MalformedEntityIsBadRequestNotOutage(t *testing.T) {
	store := emptyPageStore()
	r := newTestRouter(store)

	w := getDecisions(t, r, "?legal_entity_id=not-a-uuid", auditHeaders())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed entity filter, got %d: %s", w.Code, w.Body.String())
	}
	if store.gotListDecisionsTenant != "" {
		t.Error("the store was queried with a value that would have produced a driver error reported as a 503")
	}
}

func TestListAccessDecisions_RefusesUnparseableDates(t *testing.T) {
	for _, q := range []string{"?decided_from=2026-09-01", "?decided_to=yesterday", "?decided_from=09/01/2026"} {
		store := emptyPageStore()
		r := newTestRouter(store)
		w := getDecisions(t, r, q, auditHeaders())
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", q, w.Code, w.Body.String())
		}
	}
}

// An inverted window returns nothing, and nothing is a legitimate answer to a
// well-formed question — so it is refused rather than answered.
func TestListAccessDecisions_RefusesInvertedWindow(t *testing.T) {
	store := emptyPageStore()
	r := newTestRouter(store)

	w := getDecisions(t, r, "?decided_from=2026-09-30T00:00:00Z&decided_to=2026-09-01T00:00:00Z", auditHeaders())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for decided_to before decided_from, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListAccessDecisions_RefusesNonPositiveLimit(t *testing.T) {
	for _, q := range []string{"?limit=0", "?limit=-5", "?limit=lots"} {
		store := emptyPageStore()
		r := newTestRouter(store)
		w := getDecisions(t, r, q, auditHeaders())
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", q, w.Code, w.Body.String())
		}
	}
}

// A limit ABOVE the cap is not a malformed request — a caller asking for 1000
// wants as much as it can have — so it is clamped by the store rather than
// refused by the handler. The handler forwards it verbatim.
func TestListAccessDecisions_OversizedLimitIsForwardedNotRefused(t *testing.T) {
	store := emptyPageStore()
	r := newTestRouter(store)

	w := getDecisions(t, r, "?limit=100000", auditHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for an oversized limit, got %d: %s", w.Code, w.Body.String())
	}
	if store.gotListDecisionsParams.Limit != 100000 {
		t.Fatalf("limit forwarded as %d — the handler must not clamp; the store owns the cap so there is one place to change it",
			store.gotListDecisionsParams.Limit)
	}
}

// A cursor this service did not issue is refused rather than ignored. Silently
// resetting to page one would hand an auditor the newest page while they
// believed they were continuing through the oldest — wrong in a way that is
// invisible in the output.
func TestListAccessDecisions_RefusesForgedCursor(t *testing.T) {
	for _, c := range []string{"?cursor=not-base64!!", "?cursor=aGVsbG8", "?cursor=" + base64Raw("no-pipe-here")} {
		store := emptyPageStore()
		r := newTestRouter(store)
		w := getDecisions(t, r, c, auditHeaders())
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", c, w.Code, w.Body.String())
		}
	}
}

func TestListAccessDecisions_RoundTripsAnIssuedCursor(t *testing.T) {
	at := time.Date(2026, 9, 9, 12, 34, 56, 789, time.UTC)
	token := domain.AccessDecisionCursor{DecidedAt: at, AccessDecisionID: "dec-42"}.Encode()
	if token == "" {
		t.Fatal("Encode returned empty for a populated cursor")
	}

	store := emptyPageStore()
	r := newTestRouter(store)
	w := getDecisions(t, r, "?cursor="+token, auditHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for a cursor this service issued, got %d: %s", w.Code, w.Body.String())
	}

	got := store.gotListDecisionsParams.Cursor
	if got.AccessDecisionID != "dec-42" {
		t.Errorf("cursor id = %q, want dec-42", got.AccessDecisionID)
	}
	if !got.DecidedAt.Equal(at) {
		t.Errorf("cursor decided_at = %v, want %v — nanosecond precision has to survive the round trip or a page boundary between two decisions in the same microsecond repeats or drops one",
			got.DecidedAt, at)
	}
}

// An empty token is "start at the newest", not a malformed cursor.
func TestListAccessDecisions_EmptyCursorIsPageOne(t *testing.T) {
	store := emptyPageStore()
	r := newTestRouter(store)

	w := getDecisions(t, r, "?cursor=", auditHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !store.gotListDecisionsParams.Cursor.IsZero() {
		t.Error("an empty cursor produced a non-zero position")
	}
}

func TestListAccessDecisions_ReturnsPageAndNextCursor(t *testing.T) {
	store := &stubStore{listDecisions: &domain.AccessDecisionPage{
		Decisions: []domain.AccessDecisionLog{{
			AccessDecisionID: "dec-1",
			PrincipalID:      "p-9",
			ActionType:       "PAYMENT_APPROVE",
			DecisionOutcome:  "DENIED",
			DecisionBasis:    "sod:conflict_with=PAYMENT_INITIATE",
		}},
		NextCursor: "opaque-token",
	}}
	r := newTestRouter(store)

	w := getDecisions(t, r, "?decision_outcome=DENIED", auditHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var page domain.AccessDecisionPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.Decisions) != 1 || page.Decisions[0].AccessDecisionID != "dec-1" {
		t.Fatalf("decisions = %+v", page.Decisions)
	}
	if page.NextCursor != "opaque-token" {
		t.Errorf("next_cursor = %q — its presence is the only correct test for 'there is more'", page.NextCursor)
	}
	// The whole point of the route: the denial's basis is readable without
	// anybody having had to find its id first.
	if page.Decisions[0].DecisionBasis != "sod:conflict_with=PAYMENT_INITIATE" {
		t.Errorf("decision_basis = %q", page.Decisions[0].DecisionBasis)
	}
}

func TestListAccessDecisions_StoreUnavailableIs503(t *testing.T) {
	store := &stubStore{listDecisionsErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store)

	w := getDecisions(t, r, "", auditHeaders())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

// The by-id route still works and still lives under the same prefix. Registered
// as AccessDecisionsPath+"/{id}" after the collection route was added, and chi
// routes the two independently — a regression here would mean the collection
// route swallowed the by-id one.
func TestGetAccessDecision_StillRoutesUnderTheCollectionPrefix(t *testing.T) {
	store := &stubStore{findDecision: &domain.AccessDecisionLog{AccessDecisionID: "dec-7"}}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, handler.AccessDecisionsPath+"/dec-7", nil)
	req.Header.Set("X-Principal-Id", "p-auditor")
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var d domain.AccessDecisionLog
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.AccessDecisionID != "dec-7" {
		t.Fatalf("access_decision_id = %q", d.AccessDecisionID)
	}
}

// ── cursor unit properties ──────────────────────────────────────────────────

func TestAccessDecisionCursor_ZeroValueEncodesEmpty(t *testing.T) {
	if got := (domain.AccessDecisionCursor{}).Encode(); got != "" {
		t.Fatalf("zero cursor encoded to %q, want empty", got)
	}
	// A cursor with a time but no id is also "no position": the id is the
	// tie-breaker and a position without it cannot page a table where two
	// decisions share a timestamp.
	if got := (domain.AccessDecisionCursor{DecidedAt: time.Now()}).Encode(); got != "" {
		t.Fatalf("cursor with no id encoded to %q, want empty", got)
	}
}

func TestDecodeAccessDecisionCursor_RejectsMalformed(t *testing.T) {
	for _, token := range []string{
		"!!!not base64!!!",
		base64Raw("missing-separator"),
		base64Raw("|no-timestamp"),
		base64Raw("2026-09-09T00:00:00Z|"),
		base64Raw("not-a-time|dec-1"),
	} {
		if _, err := domain.DecodeAccessDecisionCursor(token); err == nil {
			t.Errorf("%q decoded without error", token)
		}
	}
}

// base64Raw is domain's own cursor encoding, so a test can build a token that
// is well-formed base64 and malformed inside it — which is the case that
// distinguishes "not a cursor" from "not base64".
func base64Raw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
