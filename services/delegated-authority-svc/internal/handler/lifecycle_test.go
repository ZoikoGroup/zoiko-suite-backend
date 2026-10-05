package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/events"
)

func decodeGrant(t *testing.T, rr *httptest.ResponseRecorder) domain.DelegationGrant {
	t.Helper()
	var d domain.DelegationGrant
	if err := json.NewDecoder(rr.Body).Decode(&d); err != nil {
		t.Fatalf("decode: %v (%d)", err, rr.Code)
	}
	return d
}

func transition(r chi.Router, id, verb string, version int64, caller string, body any) *httptest.ResponseRecorder {
	return doReq(r, http.MethodPost, fmt.Sprintf("/v1/delegations/%s/%s?expected_version=%d", id, verb, version), body, caller)
}

func propose(t *testing.T, r chi.Router, maker string) domain.DelegationGrant {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", uuid.NewString(), time.Now(), time.Now().Add(24*time.Hour)), maker)
	if rr.Code != http.StatusCreated {
		t.Fatalf("propose: %d %s", rr.Code, rr.Body.String())
	}
	d := decodeGrant(t, rr)
	if d.Status != domain.DelegationStatusProposed {
		t.Fatalf("expected PROPOSED, got %s", d.Status)
	}
	return d
}

// ── Proposed → Active: maker-checker ─────────────────────────────────────────

func TestActivate_DelegatorApprovesAProposal(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, nil)
	d := propose(t, r, "admin-1")

	rr := transition(r, d.DelegationID, "activate", d.Version, "delegator-1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", rr.Code, rr.Body.String())
	}
	got := decodeGrant(t, rr)
	if got.Status != domain.DelegationStatusActive || got.ApprovalMethod == nil || *got.ApprovalMethod != domain.ApprovalDelegator {
		t.Fatalf("expected ACTIVE by DELEGATOR_APPROVAL, got %s %v", got.Status, got.ApprovalMethod)
	}
	if s.countEvents(events.EventDelegated) != 1 {
		t.Errorf("activation is DelegationActivated: exactly one authority.delegated, got %d", s.countEvents(events.EventDelegated))
	}
}

func TestActivate_MakerAndDelegateCannotApprove(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, nil)
	d := propose(t, r, "admin-1")

	for _, who := range []string{"admin-1", "delegate-1"} {
		rr := transition(r, d.DelegationID, "activate", d.Version, who, nil)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s approving: expected 403, got %d %s", who, rr.Code, rr.Body.String())
		}
	}
	if len(s.refusals) != 2 || s.refusals[0] != "approval_not_segregated" {
		t.Errorf("refused approvals must leave durable evidence, got %v", s.refusals)
	}
	// A second administrator who is neither maker nor delegate may.
	rr := transition(r, d.DelegationID, "activate", d.Version, "admin-2", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("second administrator: expected 200, got %d %s", rr.Code, rr.Body.String())
	}
	if got := decodeGrant(t, rr); got.ApprovalMethod == nil || *got.ApprovalMethod != domain.ApprovalAdministrator {
		t.Errorf("expected ADMINISTRATOR_APPROVAL, got %v", got.ApprovalMethod)
	}
}

func TestActivate_RechecksTheDelegatorsAuthority(t *testing.T) {
	s := newStubStore()
	az := &stubAuthZ{}
	r := newRouter(s, az, nil)
	d := propose(t, r, "admin-1")
	az.delegatorDenied = "delegator-1" // lost the authority since the proposal
	rr := transition(r, d.DelegationID, "activate", d.Version, "admin-2", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 delegator_lacks_authority, got %d %s", rr.Code, rr.Body.String())
	}
}

// ── versions ─────────────────────────────────────────────────────────────────

func TestTransitions_RequireTheCurrentVersion(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, nil)
	d := createActiveDelegation(t, r)

	rr := doReq(r, http.MethodPost, "/v1/delegations/"+d.DelegationID+"/revoke", map[string]any{"reason": "x"}, "admin-1")
	if rr.Code != http.StatusPreconditionRequired {
		t.Errorf("no version: expected 428, got %d", rr.Code)
	}
	rr = doReq(r, http.MethodPost, "/v1/delegations/"+d.DelegationID+"/revoke?expected_version=abc", map[string]any{"reason": "x"}, "admin-1")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("malformed version: expected 400 (it used to mean 'no check'), got %d", rr.Code)
	}
	rr = transition(r, d.DelegationID, "revoke", d.Version+5, "admin-1", map[string]any{"reason": "x"})
	if rr.Code != http.StatusConflict {
		t.Errorf("stale version: expected 409, got %d", rr.Code)
	}
	rr = transition(r, d.DelegationID, "revoke", d.Version, "admin-1", map[string]any{})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("revoke without a reason: expected 400, got %d", rr.Code)
	}
}

// ── suspend / resume ─────────────────────────────────────────────────────────

func TestSuspendAndResume(t *testing.T) {
	s := newStubStore()
	az := &stubAuthZ{}
	r := newRouter(s, az, nil)
	d := createActiveDelegation(t, r)

	rr := transition(r, d.DelegationID, "suspend", d.Version, "delegator-1", map[string]any{"reason": "investigation"})
	if rr.Code != http.StatusOK {
		t.Fatalf("suspend: %d %s", rr.Code, rr.Body.String())
	}
	sus := decodeGrant(t, rr)
	if sus.Status != domain.DelegationStatusSuspended || sus.SuspensionReason == nil || s.countEvents(events.EventSuspended) != 1 {
		t.Fatalf("expected SUSPENDED with reason and one authority.suspended, got %+v", sus)
	}
	rr = transition(r, d.DelegationID, "resume", sus.Version, "delegate-1", nil)
	if rr.Code != http.StatusForbidden {
		t.Errorf("the delegate must not resume their own grant, got %d", rr.Code)
	}
	az.delegatorDenied = "delegator-1"
	rr = transition(r, d.DelegationID, "resume", sus.Version, "delegator-1", nil)
	if rr.Code != http.StatusForbidden {
		t.Errorf("resume must re-check the delegator's authority, got %d", rr.Code)
	}
	az.delegatorDenied = ""
	rr = transition(r, d.DelegationID, "resume", sus.Version, "delegator-1", nil)
	if rr.Code != http.StatusOK || decodeGrant(t, rr).Status != domain.DelegationStatusActive {
		t.Fatalf("resume: %d", rr.Code)
	}
	if s.countEvents(events.EventResumed) != 1 {
		t.Errorf("expected one authority.resumed")
	}
}

// ── extend ───────────────────────────────────────────────────────────────────

// The 5 Oct re-audit probe: every extend with a fresh correlation id answered
// 409 overlap_conflict, because the grant overlapped its own extended window.
func TestExtend_WithAFreshCorrelationIDSucceeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, nil)
	d := createActiveDelegation(t, r)
	rr := transition(r, d.DelegationID, "extend", d.Version, "delegator-1", map[string]any{
		"new_effective_to": d.EffectiveTo.Add(24 * time.Hour), "correlation_id": uuid.NewString(), "reason": "leave extended"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", rr.Code, rr.Body.String())
	}
	if got := decodeGrant(t, rr); !got.EffectiveTo.Equal(d.EffectiveTo.Add(24*time.Hour)) || s.countEvents(events.EventExtended) != 1 {
		t.Errorf("extension not applied or not announced: %v", got.EffectiveTo)
	}
}

func TestExtend_StillRefusesARealOverlapAndRechecksAuthority(t *testing.T) {
	s := newStubStore()
	az := &stubAuthZ{}
	r := newRouter(s, az, nil)
	d := createActiveDelegation(t, r)
	// A second grant of the same action to the same delegate, starting where
	// the first ends ([from, to) — adjacency is not an overlap).
	rr := doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", uuid.NewString(), d.EffectiveTo, d.EffectiveTo.Add(48*time.Hour)), "delegator-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("back-to-back grants must be allowed, got %d %s", rr.Code, rr.Body.String())
	}
	rr = transition(r, d.DelegationID, "extend", d.Version, "delegator-1", map[string]any{
		"new_effective_to": d.EffectiveTo.Add(24 * time.Hour), "correlation_id": uuid.NewString(), "reason": "x"})
	if rr.Code != http.StatusConflict {
		t.Errorf("extending into the next grant must be 409, got %d", rr.Code)
	}
	az.delegatorDenied = "delegator-1"
	rr = transition(r, d.DelegationID, "extend", d.Version, "delegator-1", map[string]any{
		"new_effective_to": d.EffectiveTo.Add(time.Hour), "correlation_id": uuid.NewString(), "reason": "x"})
	if rr.Code != http.StatusForbidden {
		t.Errorf("extension must re-check the delegator's authority, got %d", rr.Code)
	}
}

// ── limits (ORG-06 negative case 11) ─────────────────────────────────────────

func limitBody(cents int64, currency string) map[string]any {
	b := delegationBody("delegator-1", uuid.NewString(), time.Now(), time.Now().Add(24*time.Hour))
	b["authority_limit_cents"] = cents
	b["authority_limit_currency"] = currency
	return b
}

func TestLimit_AboveTheDelegatorsOwnIsRefused(t *testing.T) {
	s := newStubStore()
	az := &stubAuthZ{limitDenied: true}
	r := newRouter(s, az, nil)
	rr := doReq(r, http.MethodPost, "/v1/delegations/", limitBody(5000000, "USD"), "delegator-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 delegator_exceeds_limit, got %d %s", rr.Code, rr.Body.String())
	}
	if len(az.limitAsked) != 1 || az.limitAsked[0] != "delegator-1@50000.00 USD" {
		t.Errorf("the DELEGATOR must be asked about the ceiling in major units, asked %v", az.limitAsked)
	}
	if len(s.refusals) != 1 || s.refusals[0] != "delegator_exceeds_limit" {
		t.Errorf("refusal evidence: %v", s.refusals)
	}
}

func TestLimit_MalformedIsRefused(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, nil)
	for name, b := range map[string]map[string]any{
		"negative":       limitBody(-1, "USD"),
		"lowercase code": limitBody(100, "usd"),
		"no currency":    func() map[string]any { b := limitBody(100, "USD"); delete(b, "authority_limit_currency"); return b }(),
	} {
		if rr := doReq(r, http.MethodPost, "/v1/delegations/", b, "delegator-1"); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", name, rr.Code)
		}
	}
}

func TestFormatMinorUnits(t *testing.T) {
	for _, c := range []struct {
		minor int64
		cur   string
		want  string
	}{{50000, "USD", "500.00"}, {5, "GBP", "0.05"}, {500, "JPY", "500"}, {1234, "KWD", "1.234"}} {
		if got := domain.FormatMinorUnits(c.minor, c.cur); got != c.want {
			t.Errorf("%d %s = %s, want %s", c.minor, c.cur, got, c.want)
		}
	}
}

// ── SoD and refusal evidence ─────────────────────────────────────────────────

func TestSoDConflictIsRefusedAndRecorded(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, newStubSoD("delegate-1|PO_ISSUE"))
	rr := doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", uuid.NewString(), time.Now(), time.Now().Add(time.Hour)), "delegator-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 sod_conflict, got %d %s", rr.Code, rr.Body.String())
	}
	if len(s.refusals) != 1 || s.refusals[0] != "sod_conflict" {
		t.Errorf("refusal evidence: %v", s.refusals)
	}
}

// Delegate = delegator used to be recorded as "invalid_window".
func TestDelegateIsDelegatorIsRecordedUnderItsOwnName(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, nil)
	b := delegationBody("p1", uuid.NewString(), time.Now(), time.Now().Add(time.Hour))
	b["delegate_principal_id"] = "p1"
	if rr := doReq(r, http.MethodPost, "/v1/delegations/", b, "p1"); rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if len(s.refusals) != 1 || s.refusals[0] != "delegate_is_delegator" {
		t.Errorf("recorded as %v", s.refusals)
	}
}

func TestCreateRequiresAReason(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, nil)
	b := delegationBody("delegator-1", uuid.NewString(), time.Now(), time.Now().Add(time.Hour))
	delete(b, "reason")
	if rr := doReq(r, http.MethodPost, "/v1/delegations/", b, "delegator-1"); rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 without a reason, got %d", rr.Code)
	}
}

// ── as-of reads ──────────────────────────────────────────────────────────────

func TestListAsOf(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, nil)
	d := createActiveDelegation(t, r)
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rr := doReq(r, http.MethodGet, "/v1/delegations/?legal_entity_id=le-us&as_of="+at, nil, "viewer")
	if rr.Code != http.StatusOK {
		t.Fatalf("as_of: %d %s", rr.Code, rr.Body.String())
	}
	var list []domain.DelegationGrant
	_ = json.NewDecoder(rr.Body).Decode(&list)
	if len(list) != 1 || list[0].DelegationID != d.DelegationID {
		t.Errorf("expected the grant effective then, got %v", list)
	}
	if rr := doReq(r, http.MethodGet, "/v1/delegations/?legal_entity_id=le-us&as_of=yesterday", nil, "viewer"); rr.Code != http.StatusBadRequest {
		t.Errorf("malformed as_of: expected 400, got %d", rr.Code)
	}
	if rr := doReq(r, http.MethodGet, "/v1/delegations/?legal_entity_id=le-us&status=PROPOSED", nil, "viewer"); rr.Code != http.StatusOK {
		t.Errorf("PROPOSED is a status now and must be filterable, got %d", rr.Code)
	}
}
