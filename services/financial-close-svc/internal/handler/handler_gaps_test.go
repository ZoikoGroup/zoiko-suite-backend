package handler_test

// Regression tests for the defects closed when this service was wired to the
// console. Each one fails against the behaviour it replaced.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"zoiko.io/financial-close-svc/internal/domain"
)

// ── tenant scope ─────────────────────────────────────────────────────────────

// A missing tenant scope used to reach the caller as 503 store_unavailable,
// because every store error was reported identically — sending an operator to
// look at a database over a request that never passed identity verification.
func TestMissingTenantScope_Returns401NotServiceUnavailable(t *testing.T) {
	cases := []struct {
		name, method, path string
		body               any
	}{
		{"create", http.MethodPost, "/v1/close/periods", domain.PeriodCreateRequest{
			LegalEntityID: "le-1", PeriodName: "2026-01",
			PeriodStart: time.Now().Add(-24 * time.Hour), PeriodEnd: time.Now(),
		}},
		{"list", http.MethodGet, "/v1/close/periods?legal_entity_id=le-1", nil},
		{"status", http.MethodGet, "/v1/close/periods/status?legal_entity_id=le-1&period_name=2026-01", nil},
		{"readiness", http.MethodGet, "/v1/close/periods/fp-1/readiness", nil},
		{"lock", http.MethodPost, "/v1/close/periods/fp-1/lock", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubStore()
			s.periods["fp-1"] = &domain.FiscalPeriod{
				FiscalPeriodID: "fp-1", TenantID: testTenantID, LegalEntityID: "le-1",
				PeriodName: "2026-01", CloseStatus: "OPEN",
			}
			r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
			rr := doReqAs(r, tc.method, tc.path, tc.body, "principal-1", "")
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// GetPeriodStatus answers OPEN for a period nobody registered — that default is
// intended. It is also why the tenant scope is mandatory: without it every
// period looks unregistered, so a caller could be told OPEN about a LOCKED
// period simply by omitting a header, and general-ledger-svc — which fails
// closed on everything else precisely so a locked period cannot be posted into
// — would believe it.
func TestGetPeriodStatus_MissingTenantScope_DoesNotFailOpen(t *testing.T) {
	s := newStubStore()
	s.periods["fp-locked"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-locked", TenantID: testTenantID, LegalEntityID: "le-1",
		PeriodName: "2026-01", CloseStatus: "LOCKED",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	rr := doReqAs(r, http.MethodGet,
		"/v1/close/periods/status?legal_entity_id=le-1&period_name=2026-01", nil, "", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("OPEN")) {
		t.Fatal("a request with no verified tenant scope was told the period is OPEN — this is the period-lock bypass")
	}
}

// ── period-scoped readiness ──────────────────────────────────────────────────

func TestGetPeriodReadiness_BlockedPeriod_NamesEveryIssue(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open", TenantID: testTenantID, LegalEntityID: "le-1",
		PeriodName: "2026-01", CloseStatus: "OPEN",
	}
	// No AR or AP control run in the store: two more blockers.
	r := newGatedRouter(s,
		&stubClients{unpostedCount: 2, backlog: domain.PostingBacklog{Count: 1,
			Samples: []domain.PostingBacklogItem{{Reference: "evt-1", Status: "FAILED", FailureReason: "boom"}}}})

	rr := doReq(r, http.MethodGet, "/v1/close/periods/fp-open/readiness", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.ReadinessCheckResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.IsReady {
		t.Fatal("expected is_ready=false")
	}
	// Every blocker, not the first one found: an operator clearing blockers
	// one at a time needs the whole list, or every fix reveals another.
	// unposted journals + posting backlog + AR not run + AP not run.
	if len(resp.BlockingIssues) != 4 {
		t.Fatalf("expected all four blockers to report, got %v", resp.BlockingIssues)
	}
}

func TestGetPeriodReadiness_AlreadyLocked_IsNotReady(t *testing.T) {
	s := newStubStore()
	s.periods["fp-locked"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-locked", TenantID: testTenantID, LegalEntityID: "le-1",
		PeriodName: "2026-01", CloseStatus: "HARD_CLOSED",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})

	rr := doReq(r, http.MethodGet, "/v1/close/periods/fp-locked/readiness", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.ReadinessCheckResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.IsReady {
		t.Fatal("a LOCKED period is not 'ready to close' — saying so invites a lock that then 422s")
	}
}

func TestGetPeriodReadiness_EmptyIssuesIsArrayNotNull(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open", TenantID: testTenantID, LegalEntityID: "le-1",
		PeriodName: "2026-01", CloseStatus: "OPEN",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/close/periods/fp-open/readiness", nil, "principal-1")
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"blocking_issues":[]`)) {
		t.Fatalf("expected an empty array, got %s", rr.Body.String())
	}
}

func TestGetPeriodReadiness_DependencyDown_FailsClosed(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open", TenantID: testTenantID, LegalEntityID: "le-1",
		PeriodName: "2026-01", CloseStatus: "OPEN",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{},
		&stubClients{unpostedErr: domain.ErrGLServiceUnavailable})

	rr := doReq(r, http.MethodGet, "/v1/close/periods/fp-open/readiness", nil, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when a dependency cannot be queried, got %d: %s", rr.Code, rr.Body.String())
	}
	// "We could not check" must never render as "nothing to report".
	if bytes.Contains(rr.Body.Bytes(), []byte(`"is_ready":true`)) {
		t.Fatal("an unqueryable dependency was reported as ready")
	}
}

// A ledger page that came back full means there may be journals this service
// never saw, so the trial balance it would compile is wrong rather than merely
// short — and that balance is what gets hashed, signed and locked.
func TestLockPeriod_LedgerPageTruncated_RefusesToClose(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open", TenantID: testTenantID, LegalEntityID: "le-1",
		PeriodName: "2026-01", CloseStatus: "CLOSE_REVIEW",
	}
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{},
		&stubClients{unpostedErr: domain.ErrLedgerPageTruncated})

	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("ledger_page_truncated")) {
		t.Fatalf("the refusal must name its reason, got %s", rr.Body.String())
	}
	if s.periods["fp-open"].CloseStatus != "CLOSE_REVIEW" {
		t.Fatal("the period must not be locked over a trial balance that may be incomplete")
	}
	if pub.closed != 0 {
		t.Fatal("no closed event for a close that did not happen")
	}
}

// ── close evidence ───────────────────────────────────────────────────────────

// The evidence row IS the close: the trial balance hash and its signature are
// the only record of what the books said when they were sealed. A failure to
// write it used to be logged and swallowed, and the response still returned a
// verification_hash — vouching for evidence that existed nowhere but in that
// reply.
func TestLockPeriod_EvidenceWriteFails_IsReported(t *testing.T) {
	s := newStubStore()
	s.evidenceErr = errors.New("insert failed")
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open", TenantID: testTenantID, LegalEntityID: "le-1",
		PeriodName: "2026-01", CloseStatus: "CLOSE_REVIEW",
	}
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubClients{})

	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code == http.StatusOK {
		t.Fatal("a close whose evidence was not recorded must not be reported as a successful close")
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("evidence_not_recorded")) {
		t.Fatalf("expected evidence_not_recorded, got %s", rr.Body.String())
	}
	// The period genuinely IS locked, and saying otherwise would be a second
	// untruth — but nothing may claim the close is evidenced.
	if s.periods["fp-open"].CloseStatus != "HARD_CLOSED" {
		t.Fatal("the period was locked before the evidence write; the response must not pretend it was not")
	}
	if pub.closed != 0 {
		t.Fatal("a closed event must not be published for a close with no evidence")
	}
}

func TestLockPeriod_EvidenceSignedWithConfiguredKeyNotTheTenantID(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open", TenantID: testTenantID, LegalEntityID: "le-1",
		PeriodName: "2026-01", CloseStatus: "CLOSE_REVIEW",
	}
	balances := map[string]float64{"1000-Cash": 10000.00, "4000-Rev": -10000.00}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{trialBalances: balances})

	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.evidence) != 1 {
		t.Fatalf("expected exactly one evidence row, got %d", len(s.evidence))
	}
	ev := s.evidence[0]

	// Recompute the hash the way the handler does, then check the signature is
	// the HMAC of it under the CONFIGURED key.
	var buf bytes.Buffer
	for _, k := range []string{"1000-Cash", "4000-Rev"} {
		buf.WriteString(fmt.Sprintf("%s:%.2f;", k, balances[k]))
	}
	sum := sha256.Sum256(buf.Bytes())
	if ev.TrialBalanceHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("stored hash %s does not cover the trial balance", ev.TrialBalanceHash)
	}

	good := hmac.New(sha256.New, testSigningKey)
	good.Write(sum[:])
	if ev.Signature != hex.EncodeToString(good.Sum(nil)) {
		t.Fatal("the signature was not produced with the configured key")
	}

	// And the forgery the old code allowed: the tenant id is a public value
	// that travels in a header on every request, so anyone who had seen one
	// could reproduce a signature keyed with it.
	forged := hmac.New(sha256.New, []byte(testTenantID))
	forged.Write(sum[:])
	if ev.Signature == hex.EncodeToString(forged.Sum(nil)) {
		t.Fatal("the signature is still keyed with the tenant id, which every caller knows — it guarantees nothing")
	}
}
