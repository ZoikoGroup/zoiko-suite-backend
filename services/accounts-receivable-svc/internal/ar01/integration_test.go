//go:build integration

// Real-Postgres tests for AR-01. Run:
//
//	go test -tags=integration -count=1 -timeout=600s ./internal/ar01/...
//
// The embedded superuser bypasses RLS, so isolation tests prove the explicit
// tenant_id predicates, not the policies.
package ar01

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/kit/kittest"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	var stop func()
	pool, stop = kittest.Start("ar01_test", 17400)
	code := m.Run()
	stop()
	os.Exit(code)
}

const base = "/internal/v1/ar01/"

type env struct {
	t       *testing.T
	m       *Module
	h       http.Handler
	tenant  string
	entity  string
	parties *fakeParties
	open    *fakeOpen
	offset  time.Duration // simulated clock skew
}

func (e *env) now() time.Time { return time.Now().Add(e.offset).UTC() }

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), parties: &fakeParties{party: activeCustomer()}, open: &fakeOpen{}}
	deps := testDeps(&fakeAuthz{grants: standardGrants()})
	deps.DB = kit.NewDB(pool)
	deps.Clock = e.now
	e.m = New(deps, e.parties, e.open)
	e.h = testRouter(e.m)
	return e
}

func (e *env) dayOf(offsetDays int) string { return day(e.now().AddDate(0, 0, offsetDays)) }

func (e *env) cmd(principal, name string, body map[string]any) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodPost, base+"commands/"+name, e.tenant, principal, "", body)
	return code, b
}

func (e *env) query(principal, name, qs string) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodGet, base+name+"?"+qs, e.tenant, principal, "", nil)
	return code, b
}

func (e *env) ok(code int, body []byte) *Profile {
	e.t.Helper()
	require.Less(e.t, code, 300, string(body))
	return asProfile(e.t, body)
}

func (e *env) create(principal string, extra map[string]any) *Profile {
	e.t.Helper()
	b := createBody(e.entity, uuid.NewString())
	for k, v := range extra {
		b[k] = v
	}
	code, body := e.cmd(principal, "createFinancialProfile", b)
	require.Equal(e.t, http.StatusCreated, code, string(body))
	return asProfile(e.t, body)
}

func (e *env) activate(p *Profile) *Profile {
	e.t.Helper()
	code, body := e.cmd(pMaker, "amendFinancialProfile", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "transition": "ACTIVATE"})
	return e.ok(code, body)
}

func (e *env) active(extra map[string]any) *Profile { return e.activate(e.create(pMaker, extra)) }

func (e *env) get(id string) *Profile {
	e.t.Helper()
	code, body := e.query(pNobody, "getFinancialProfile", "profile_id="+id)
	return e.ok(code, body)
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (e *env) events(profileID, eventType string) int {
	return e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3`, e.tenant, profileID, eventType)
}

func (e *env) audits(profileID string) int {
	return e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2`, e.tenant, profileID)
}

func (e *env) resolve(p *Profile, at time.Time) (*kit.ProfileView, error) {
	return e.m.ActiveAt(context.Background(), e.tenant, p.LegalEntityID, p.CustomerPartyID, at)
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var p *kit.Problem
	require.ErrorAs(t, err, &p)
	require.Equal(t, code, p.Code)
}

// ── lifecycle, evidence, events ──────────────────────────────────────────────

func TestLifecycle_EvidenceAndEvents(t *testing.T) {
	e := newEnv(t)
	p := e.create(pMaker, map[string]any{"dunning_profile": "STD_DUNNING", "ar_account_policy": "STANDARD",
		"invoice_preferences":   map[string]any{"delivery_channel": "EMAIL", "po_required": true},
		"statement_preferences": map[string]any{"frequency": "MONTHLY"},
		"risk_attributes":       map[string]any{"rating": "LOW", "source": "bureau", "source_ref": "REP-9"}, "reason_code": "ONBOARD", "evidence_id": "EV-1"})
	require.Equal(t, StateDraft, p.State)
	require.Equal(t, 1, p.Version)
	require.Equal(t, "NET30", p.PaymentTerms.Code)
	require.Equal(t, "bureau", p.Attributes.RiskAttributes.Source)
	require.Equal(t, 1, e.events(p.ProfileID, EventProfileCreated))

	// DRAFT profile is not usable and cannot be held.
	_, err := e.resolve(p, e.now())
	requireCode(t, err, kit.CodeCustomerProfileInactive)
	code, body := e.cmd(pMaker, "placeCreditHold", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "R"})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body))

	p = e.activate(p)
	require.Equal(t, StateActive, p.State)
	require.Equal(t, 2, p.Version)
	v, err := e.resolve(p, e.now())
	require.NoError(t, err)
	require.False(t, v.CreditHold)
	require.Equal(t, "NET30", v.PaymentTermsCode)
	require.Equal(t, 30, v.PaymentTermsDays)
	require.Equal(t, "STD_DUNNING", v.DunningProfile)
	require.Equal(t, 2, v.Version)

	// Amend attributes (versioned) with a risk update needing provenance and a reason.
	code, body = e.cmd(pMaker, "amendFinancialProfile", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version,
		"dunning_profile": "STRICT", "risk_attributes": map[string]any{"rating": "HIGH", "source": "internal", "source_ref": "CASE-4"}, "reason_code": "REASSESS"})
	p = e.ok(code, body)
	require.Equal(t, "STRICT", p.Attributes.DunningProfile)
	require.Equal(t, 3, p.Version)
	code, body = e.cmd(pMaker, "amendFinancialProfile", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "dunning_profile": "STRICT"})
	require.Equal(t, http.StatusBadRequest, code, "an amendment that changes nothing is refused: "+string(body))

	// Hold placement / release (reason mandatory) and the resolver's CreditHold flag.
	code, body = e.cmd(pMaker, "placeCreditHold", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "OVERDUE_90", "evidence_id": "EV-2"})
	p = e.ok(code, body)
	require.Equal(t, StateOnHold, p.State)
	require.True(t, p.CreditHold)
	require.Equal(t, "OVERDUE_90", p.HoldReasonCode)
	require.Equal(t, pMaker, p.HoldPlacedBy)
	v, err = e.resolve(p, e.now())
	require.NoError(t, err)
	require.True(t, v.CreditHold, "an on-hold profile resolves with CreditHold=true")
	code, body = e.cmd(pMaker, "placeCreditHold", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "AGAIN"})
	require.Equal(t, http.StatusConflict, code)
	code, body = e.cmd(pMaker, "amendFinancialProfile", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "transition": "SUSPEND", "reason_code": "R"})
	require.Equal(t, http.StatusConflict, code, "release the hold before suspending: "+string(body))
	code, body = e.cmd(pNobody, "releaseCreditHold", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "PAID"})
	require.Equal(t, http.StatusForbidden, code)
	code, body = e.cmd(pMaker, "releaseCreditHold", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "PAID"})
	p = e.ok(code, body)
	require.Equal(t, StateActive, p.State)
	require.False(t, p.CreditHold)
	require.Empty(t, p.HoldPlacedBy)

	// Suspend -> not usable; reinstate -> usable; retire -> not usable and frees the slot.
	code, body = e.cmd(pMaker, "amendFinancialProfile", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "transition": "SUSPEND", "reason_code": "REVIEW"})
	p = e.ok(code, body)
	_, err = e.resolve(p, e.now())
	requireCode(t, err, kit.CodeCustomerProfileInactive)
	code, body = e.cmd(pMaker, "amendFinancialProfile", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "transition": "REINSTATE", "reason_code": "CLEARED"})
	p = e.ok(code, body)
	_, err = e.resolve(p, e.now())
	require.NoError(t, err)
	code, body = e.cmd(pMaker, "retireFinancialProfile", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "CHURN"})
	p = e.ok(code, body)
	require.Equal(t, StateRetired, p.State)
	_, err = e.resolve(p, e.now())
	requireCode(t, err, kit.CodeCustomerProfileInactive)
	code, body = e.cmd(pMaker, "amendFinancialProfile", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "dunning_profile": "X"})
	require.Equal(t, http.StatusConflict, code, "a retired profile cannot be amended: "+string(body))

	// Evidence: events + audit + history all line up with the commands (8 successful ones).
	require.Equal(t, 1, e.events(p.ProfileID, EventProfileCreated))
	require.Equal(t, 5, e.events(p.ProfileID, EventProfileChanged)) // activate, amend, suspend, reinstate, retire
	require.Equal(t, 1, e.events(p.ProfileID, EventCreditHoldPlaced))
	require.Equal(t, 1, e.events(p.ProfileID, EventCreditHoldReleased))
	require.Equal(t, 8, e.audits(p.ProfileID))
	code, body = e.query(pNobody, "listProfileHistory", "profile_id="+p.ProfileID)
	require.Equal(t, http.StatusOK, code, string(body))
	var hs struct {
		Items    []HistoryEntry `json:"items"`
		Versions []AttrVersion  `json:"attribute_versions"`
		Terms    []TermVersion  `json:"payment_term_versions"`
	}
	require.NoError(t, jsonDecode(body, &hs))
	require.Len(t, hs.Items, 8)
	require.Equal(t, "createFinancialProfile", hs.Items[0].Command)
	require.Equal(t, "placeCreditHold", hs.Items[3].Command)
	require.Equal(t, "OVERDUE_90", hs.Items[3].ReasonCode)
	require.Equal(t, "EV-2", hs.Items[3].EvidenceID)
	require.Equal(t, pMaker, hs.Items[3].ActorID)
	require.Equal(t, "HIGH", hs.Items[2].NewValues["risk_attributes"].(map[string]any)["rating"])
	require.Equal(t, "LOW", hs.Items[2].PriorValues["risk_attributes"].(map[string]any)["rating"])
	require.Len(t, hs.Versions, 2)
	require.Len(t, hs.Terms, 1)

	// Pagination.
	code, body = e.query(pNobody, "listProfileHistory", "profile_id="+p.ProfileID+"&limit=4")
	require.Equal(t, http.StatusOK, code)
	var pg struct {
		Items []HistoryEntry `json:"items"`
		Next  int            `json:"next_after_version"`
	}
	require.NoError(t, jsonDecode(body, &pg))
	require.Len(t, pg.Items, 4)
	require.Equal(t, 4, pg.Next)

	// The slot is free again: a new live profile for the same customer can be created.
	nb := createBody(e.entity, p.CustomerPartyID)
	code, body = e.cmd(pMaker, "createFinancialProfile", nb)
	require.Equal(t, http.StatusCreated, code, string(body))
}

// ── Neg #1: party record inactive ────────────────────────────────────────────

func TestNeg01_PartyInactive(t *testing.T) {
	e := newEnv(t)
	for _, party := range []*fakeParties{
		{party: &Party{Type: "CUSTOMER", Status: "SUSPENDED"}},
		{party: &Party{Type: "VENDOR", Status: "ACTIVE"}},
		{err: ErrPartyNotFound},
	} {
		e.m.parties = party
		code, body := e.cmd(pMaker, "createFinancialProfile", createBody(e.entity, uuid.NewString()))
		require.Equal(t, http.StatusUnprocessableEntity, code)
		require.Equal(t, kit.CodeCustomerProfileInactive, errCode(t, body))
	}
	e.m.parties = &fakeParties{err: svcclient.ErrUnavailable}
	code, body := e.cmd(pMaker, "createFinancialProfile", createBody(e.entity, uuid.NewString()))
	require.Equal(t, http.StatusServiceUnavailable, code, "unanswerable party master fails closed")
	require.Equal(t, kit.CodeDependencyUnavailable, errCode(t, body))
	require.Zero(t, e.count(`SELECT count(*) FROM ar01_financial_profiles WHERE tenant_id = $1`, e.tenant), "no profile on a blocked create")
	require.Zero(t, e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, e.tenant))

	// A profile exists; then the party goes inactive: every value-bearing mutation is blocked...
	e.m.parties = &fakeParties{party: activeCustomer()}
	p := e.active(nil)
	e.m.parties = &fakeParties{party: &Party{Type: "CUSTOMER", Status: "TERMINATED"}}
	mutations := map[string]map[string]any{
		"setCreditLimit":        {"profile_id": p.ProfileID, "expected_version": p.Version, "credit_limit": "10", "credit_currency": "USD", "reason_code": "R"},
		"changePaymentTerms":    {"profile_id": p.ProfileID, "expected_version": p.Version, "payment_terms_code": "NET60", "payment_terms_days": 60, "effective_from": e.dayOf(5)},
		"amendFinancialProfile": {"profile_id": p.ProfileID, "expected_version": p.Version, "dunning_profile": "X"},
		"releaseCreditHold":     {"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "R"},
	}
	for name, b := range mutations {
		code, body := e.cmd(pMaker, name, b)
		require.Equal(t, http.StatusUnprocessableEntity, code, name+string(body))
		require.Equal(t, kit.CodeCustomerProfileInactive, errCode(t, body), name)
	}
	require.Equal(t, p.Version, e.get(p.ProfileID).Version, "blocked mutations change nothing")
	// ...while the protective actions remain possible: place a hold, then retire.
	code, body = e.cmd(pMaker, "placeCreditHold", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "PARTY_TERMINATED"})
	p = e.ok(code, body)
	code, body = e.cmd(pMaker, "retireFinancialProfile", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "PARTY_TERMINATED"})
	e.ok(code, body)
}

// ── Neg #2: credit limit raised by an unauthorised user / SoD ────────────────

func TestNeg02_CreditLimitIncreaseNeedsDifferentApprover(t *testing.T) {
	e := newEnv(t)
	p := e.active(nil)
	setLimit := func(principal string, p *Profile, limit string) (int, []byte) {
		return e.cmd(principal, "setCreditLimit", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version,
			"credit_limit": limit, "credit_currency": "USD", "reason_code": "REVIEW", "evidence_id": "EV-L"})
	}

	// Unauthorised principal: 403, nothing changes.
	code, body := setLimit(pNobody, p, "5000")
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, kit.CodeForbidden, errCode(t, body))
	require.Equal(t, p.Version, e.get(p.ProfileID).Version)

	// An authorised manager raising above the policy ceiling (default 0): PENDING, not applied.
	code, body = setLimit(pMaker, p, "5000")
	require.Equal(t, http.StatusAccepted, code, string(body))
	p = asProfile(t, body)
	require.NotNil(t, p.PendingCreditRequest)
	require.Equal(t, kit.MustAmount("5000"), p.PendingCreditRequest.RequestedLimit)
	require.Equal(t, pMaker, p.PendingCreditRequest.RequestedBy)
	require.True(t, p.Attributes.CreditLimit.IsZero(), "the limit must not take effect before approval")
	v, err := e.resolve(p, e.now())
	require.NoError(t, err)
	require.True(t, v.CreditLimit.IsZero())
	require.Equal(t, 1, e.events(p.ProfileID, EventCreditLimitRequested))
	require.Equal(t, 0, e.events(p.ProfileID, EventCreditLimitChanged))

	// Only one change may be pending.
	code, body = setLimit(pMaker, p, "6000")
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, CodeLimitPending, errCode(t, body))

	approve := func(principal string, p *Profile, decision string) (int, []byte) {
		return e.cmd(principal, "approveCreditLimitChange", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version,
			"request_id": p.PendingCreditRequest.RequestID, "decision": decision, "reason_code": "WITHIN_APPETITE"})
	}
	// The requester holds no approve action: 403.
	code, body = approve(pMaker, p, "APPROVE")
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, kit.CodeForbidden, errCode(t, body))

	// A principal holding BOTH actions requests, then tries to approve their own: SoD.
	q := e.active(nil)
	code, body = setLimit(pBoth, q, "7000")
	require.Equal(t, http.StatusAccepted, code, string(body))
	q = asProfile(t, body)
	code, body = approve(pBoth, q, "APPROVE")
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = 'approveCreditLimitChange.denied'`, e.tenant, q.ProfileID),
		"the refused self-approval leaves evidence")
	q2 := e.get(q.ProfileID)
	require.True(t, q2.Attributes.CreditLimit.IsZero())
	require.Equal(t, q.Version, q2.Version)
	// The data refuses it too (belt and braces).
	_, err = pool.Exec(context.Background(), `UPDATE ar01_credit_limit_requests SET status = 'APPROVED', decided_by = requested_by WHERE profile_id = $1`, q.ProfileID)
	require.Error(t, err)

	// A different principal with the approve action approves: it takes effect.
	code, body = approve(pApprover, q, "APPROVE")
	q = e.ok(code, body)
	require.Equal(t, kit.MustAmount("7000"), q.Attributes.CreditLimit)
	require.Equal(t, pApprover, q.Attributes.ApprovedBy)
	require.Equal(t, pBoth, q.Attributes.CreatedBy)
	require.Nil(t, q.PendingCreditRequest)
	require.Equal(t, 1, e.events(q.ProfileID, EventCreditLimitChanged))
	v, err = e.resolve(q, e.now())
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("7000"), v.CreditLimit)
	require.Equal(t, "USD", v.CreditCurrency)

	// Reject path on the first profile: limit stays, event emitted, pending cleared.
	code, body = approve(pApprover, p, "REJECT")
	p = e.ok(code, body)
	require.Nil(t, p.PendingCreditRequest)
	require.True(t, p.Attributes.CreditLimit.IsZero())
	require.Equal(t, 1, e.events(p.ProfileID, EventCreditLimitRejected))

	// A decrease is immediate and needs no approval.
	code, body = setLimit(pMaker, q, "3000")
	q = e.ok(code, body)
	require.Equal(t, kit.MustAmount("3000"), q.Attributes.CreditLimit)
	require.Equal(t, 2, e.events(q.ProfileID, EventCreditLimitChanged))
	require.Empty(t, q.Attributes.ApprovedBy)
}

func TestCreditLimitPolicyCeiling(t *testing.T) {
	e := newEnv(t)
	// Only a holder of the approve action may set a ceiling.
	code, _ := e.cmd(pMaker, "createFinancialProfile", withExtra(createBody(e.entity, uuid.NewString()), map[string]any{"credit_limit_policy_max": "2000"}))
	require.Equal(t, http.StatusForbidden, code)

	p := e.activate(e.create(pBoth, map[string]any{"credit_limit_policy_max": "2000", "credit_limit": "1500", "reason_code": "ONBOARD"}))
	require.Equal(t, kit.MustAmount("1500"), p.Attributes.CreditLimit, "initial limit within policy applies directly")
	require.Equal(t, kit.MustAmount("2000"), p.PolicyMax)
	require.Nil(t, p.PendingCreditRequest)

	code, body := e.cmd(pMaker, "setCreditLimit", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "credit_limit": "2000", "credit_currency": "USD", "reason_code": "R"})
	p = e.ok(code, body)
	require.Equal(t, http.StatusOK, code, "an increase within the ceiling is immediate")
	require.Equal(t, kit.MustAmount("2000"), p.Attributes.CreditLimit)
	code, body = e.cmd(pMaker, "setCreditLimit", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "credit_limit": "2000.01", "credit_currency": "USD", "reason_code": "R"})
	require.Equal(t, http.StatusAccepted, code, "above the ceiling waits: "+string(body))

	// An initial limit above the ceiling opens a pending request and starts at zero.
	r := e.create(pMaker, map[string]any{"credit_limit": "900", "reason_code": "ONBOARD"})
	require.True(t, r.Attributes.CreditLimit.IsZero())
	require.NotNil(t, r.PendingCreditRequest)
	require.Equal(t, kit.MustAmount("900"), r.PendingCreditRequest.RequestedLimit)
	require.Equal(t, 1, e.events(r.ProfileID, EventCreditLimitRequested))
}

func withExtra(b map[string]any, extra map[string]any) map[string]any {
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// ── Neg #3: overlapping payment-term versions are impossible ─────────────────

func TestNeg03_OverlappingPaymentTerms(t *testing.T) {
	e := newEnv(t)
	p := e.active(nil)
	change := func(p *Profile, code string, days int, eff string) (int, []byte) {
		b := map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "payment_terms_code": code, "payment_terms_days": days, "reason_code": "RENEGOTIATED"}
		if eff != "" {
			b["effective_from"] = eff
		}
		return e.cmd(pMaker, "changePaymentTerms", b)
	}

	// Same start as the version in force, or in the past: refused.
	c, body := change(p, "NET45", 45, e.dayOf(0))
	require.Equal(t, http.StatusConflict, c, string(body))
	require.Equal(t, CodeVersionOverlap, errCode(t, body))
	c, body = change(p, "NET45", 45, e.dayOf(-3))
	require.Equal(t, http.StatusBadRequest, c, string(body))
	c, body = change(p, "NET30", 30, e.dayOf(5))
	require.Equal(t, http.StatusBadRequest, c, "unchanged terms are refused")

	// Two future-dated versions build a clean timeline.
	c, body = change(p, "NET60", 60, e.dayOf(10))
	p = e.ok(c, body)
	c, body = change(p, "NET90", 90, e.dayOf(20))
	p = e.ok(c, body)
	// A version that would start inside an existing interval is refused (409).
	for _, d := range []int{10, 15, 20, 12} {
		c, body = change(p, "NET75", 75, e.dayOf(d))
		require.Equal(t, http.StatusConflict, c, fmt.Sprintf("day +%d: %s", d, body))
		require.Equal(t, CodeVersionOverlap, errCode(t, body))
	}
	rows, err := pool.Query(context.Background(), `SELECT payment_terms_code, effective_from::text, COALESCE(effective_to::text,'') FROM ar01_payment_term_versions
		WHERE profile_id = $1 ORDER BY effective_from`, p.ProfileID)
	require.NoError(t, err)
	type iv struct{ code, from, to string }
	var got []iv
	for rows.Next() {
		var x iv
		require.NoError(t, rows.Scan(&x.code, &x.from, &x.to))
		got = append(got, x)
	}
	rows.Close()
	require.Equal(t, []iv{{"NET30", e.dayOf(0), e.dayOf(10)}, {"NET60", e.dayOf(10), e.dayOf(20)}, {"NET90", e.dayOf(20), ""}}, got)

	// The schema itself refuses overlap, immutability and removal (direct SQL, bypassing the service).
	ctx := context.Background()
	ins := func(seq int, from, to string) error {
		_, err := pool.Exec(ctx, `INSERT INTO ar01_payment_term_versions (term_version_id, tenant_id, legal_entity_id, profile_id, seq, payment_terms_code, payment_terms_days, effective_from, effective_to, created_by)
			VALUES ($1,$2,$3,$4,$5,'X',1,$6::date,NULLIF($7,'')::date,'sql')`, uuid.NewString(), e.tenant, e.entity, p.ProfileID, seq, from, to)
		return err
	}
	require.ErrorContains(t, ins(10, e.dayOf(5), e.dayOf(15)), "AR01_PAYMENT_TERMS_OVERLAP")
	require.ErrorContains(t, ins(11, e.dayOf(30), ""), "AR01_PAYMENT_TERMS_OVERLAP") // a second open-ended version overlaps the open one
	require.ErrorContains(t, ins(12, e.dayOf(30), e.dayOf(30)), "ar01_term_interval")
	_, err = pool.Exec(ctx, `UPDATE ar01_payment_term_versions SET payment_terms_days = 1 WHERE profile_id = $1`, p.ProfileID)
	require.ErrorContains(t, err, "AR01_PAYMENT_TERMS_IMMUTABLE")
	_, err = pool.Exec(ctx, `UPDATE ar01_payment_term_versions SET effective_to = $2::date WHERE profile_id = $1 AND payment_terms_code = 'NET30'`, p.ProfileID, e.dayOf(5))
	require.Error(t, err, "an already-closed interval cannot be moved")
	_, err = pool.Exec(ctx, `DELETE FROM ar01_payment_term_versions WHERE profile_id = $1`, p.ProfileID)
	require.ErrorContains(t, err, "not permitted")

	// Resolution always sees exactly one version per date.
	for d, want := range map[int]string{0: "NET30", 9: "NET30", 10: "NET60", 19: "NET60", 20: "NET90", 400: "NET90"} {
		v, err := e.resolve(p, e.now().AddDate(0, 0, d))
		require.NoError(t, err, d)
		require.Equal(t, want, v.PaymentTermsCode, "day +%d", d)
	}
}

func TestNeg03_ConcurrentTermChangesNeverOverlap(t *testing.T) {
	e := newEnv(t)
	p := e.active(nil)
	const n = 10
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _, _ := call(t, e.h, http.MethodPost, base+"commands/changePaymentTerms", e.tenant, pMaker, "", map[string]any{
				"profile_id": p.ProfileID, "expected_version": p.Version, "payment_terms_code": fmt.Sprintf("NET%d", 40+i), "payment_terms_days": 40 + i,
				"effective_from": e.dayOf(30), "reason_code": "RACE"})
			codes[i] = code
		}(i)
	}
	wg.Wait()
	wins, stale := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			wins++
		case http.StatusConflict:
			stale++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	require.Equal(t, 1, wins, "exactly one concurrent change wins")
	require.Equal(t, n-1, stale)
	require.Equal(t, 2, e.count(`SELECT count(*) FROM ar01_payment_term_versions WHERE profile_id = $1`, p.ProfileID))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar01_payment_term_versions WHERE profile_id = $1 AND effective_to IS NULL`, p.ProfileID))

	// Concurrent writers that bypass the service (no row lock) still cannot both
	// insert overlapping intervals: the trigger serialises them per profile.
	q := e.active(nil)
	var wg2 sync.WaitGroup
	errs := make([]error, 2)
	spans := [][2]string{{"2020-01-01", "2020-06-01"}, {"2020-03-01", "2020-09-01"}}
	for i := range spans {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			errs[i] = kit.NewDB(pool).WithTx(context.Background(), e.tenant, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(), `INSERT INTO ar01_payment_term_versions (term_version_id, tenant_id, legal_entity_id, profile_id, seq, payment_terms_code, payment_terms_days, effective_from, effective_to, created_by)
					VALUES ($1,$2,$3,$4,$5,'RAW',1,$6::date,$7::date,'sql')`, uuid.NewString(), e.tenant, e.entity, q.ProfileID, 10+i, spans[i][0], spans[i][1])
				time.Sleep(150 * time.Millisecond) // hold the transaction open so the other one really races
				return err
			})
		}(i)
	}
	wg2.Wait()
	failures := 0
	for _, err := range errs {
		if err != nil {
			failures++
		}
	}
	require.Equal(t, 1, failures, "exactly one of two overlapping raw inserts may commit: %v", errs)
}

// ── Neg #4: invoice attempts to use a future / expired profile ───────────────

func TestNeg04_FutureExpiredAndAbsentProfilesNeverResolve(t *testing.T) {
	e := newEnv(t)

	// Absent.
	_, err := e.m.ActiveAt(context.Background(), e.tenant, e.entity, uuid.NewString(), e.now())
	requireCode(t, err, kit.CodeCustomerProfileInactive)

	// Future-dated profile: active state, but not effective yet.
	p := e.active(map[string]any{"effective_from": e.dayOf(7)})
	_, err = e.resolve(p, e.now())
	requireCode(t, err, kit.CodeCustomerProfileInactive)
	_, err = e.resolve(p, e.now().AddDate(0, 0, 6))
	requireCode(t, err, kit.CodeCustomerProfileInactive)
	v, err := e.resolve(p, e.now().AddDate(0, 0, 7))
	require.NoError(t, err)
	require.Equal(t, e.dayOf(7), day(v.EffectiveFrom))
	require.Equal(t, "NET30", v.PaymentTermsCode)

	// Scheduled terms change: the old terms until the switch date, the new after, never a mix.
	q := e.active(nil)
	code, body := e.cmd(pMaker, "changePaymentTerms", map[string]any{"profile_id": q.ProfileID, "expected_version": q.Version,
		"payment_terms_code": "NET60", "payment_terms_days": 60, "effective_from": e.dayOf(10)})
	q = e.ok(code, body)
	v, _ = e.resolve(q, e.now().AddDate(0, 0, 9))
	require.Equal(t, 30, v.PaymentTermsDays)
	v, _ = e.resolve(q, e.now().AddDate(0, 0, 10))
	require.Equal(t, 60, v.PaymentTermsDays)

	// Expired/ended states.
	code, body = e.cmd(pMaker, "retireFinancialProfile", map[string]any{"profile_id": q.ProfileID, "expected_version": q.Version, "reason_code": "END"})
	e.ok(code, body)
	for _, d := range []int{0, 10, 100} {
		_, err = e.resolve(q, e.now().AddDate(0, 0, d))
		requireCode(t, err, kit.CodeCustomerProfileInactive)
	}

	// Schema: at most one live profile per customer/entity (no ambiguity possible).
	_, err = pool.Exec(context.Background(), `INSERT INTO ar01_financial_profiles (profile_id, tenant_id, legal_entity_id, customer_party_id, state, created_by)
		VALUES ($1,$2,$3,$4,'ACTIVE','sql')`, uuid.NewString(), e.tenant, e.entity, p.CustomerPartyID)
	require.ErrorContains(t, err, "uq_ar01_profile_live")

	// Invoice-side view: the resolver output via the kit port type.
	var _ kit.ProfileResolver = e.m
}

func TestNeg04_ConcurrentCreatesYieldOneProfile(t *testing.T) {
	e := newEnv(t)
	party := uuid.NewString()
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, _ = call(t, e.h, http.MethodPost, base+"commands/createFinancialProfile", e.tenant, pMaker, "", createBody(e.entity, party))
		}(i)
	}
	wg.Wait()
	created, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, n-1, conflict)
	require.Equal(t, 1, e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, e.tenant, EventProfileCreated))
}

// ── historical reconstruction ────────────────────────────────────────────────

func TestAsOf_ReconstructsExactlyWhatWasEffective(t *testing.T) {
	e := newEnv(t)
	e.offset = -10 * 24 * time.Hour // day -10: create with limit 1000 / NET30
	p := e.create(pBoth, map[string]any{"credit_limit_policy_max": "5000", "credit_limit": "1000", "reason_code": "ONBOARD"})
	p = e.activate(p)
	// day -10: schedule NET60 from day -4
	code, body := e.cmd(pMaker, "changePaymentTerms", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version,
		"payment_terms_code": "NET60", "payment_terms_days": 60, "effective_from": day(time.Now().AddDate(0, 0, -4))})
	p = e.ok(code, body)
	// day -5: credit limit decreased to 600 and a hold placed
	e.offset = -5 * 24 * time.Hour
	code, body = e.cmd(pMaker, "setCreditLimit", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "credit_limit": "600", "credit_currency": "USD", "reason_code": "RISK"})
	p = e.ok(code, body)
	code, body = e.cmd(pMaker, "placeCreditHold", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "OVERDUE"})
	p = e.ok(code, body)
	// day -2: hold released
	e.offset = -2 * 24 * time.Hour
	code, body = e.cmd(pMaker, "releaseCreditHold", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "PAID"})
	p = e.ok(code, body)
	e.offset = 0

	asOf := func(date string) (int, *Profile) {
		code, body := e.query(pNobody, "getFinancialProfileAsOf", "profile_id="+p.ProfileID+"&as_of="+date)
		if code != http.StatusOK {
			return code, nil
		}
		return code, asProfile(t, body)
	}
	d := func(n int) string { return day(time.Now().AddDate(0, 0, n)) }

	code, _ = asOf(d(-11))
	require.Equal(t, http.StatusNotFound, code, "the profile did not exist yet")

	_, a := asOf(d(-10))
	require.Equal(t, StateActive, a.State)
	require.Equal(t, kit.MustAmount("1000"), a.Attributes.CreditLimit)
	require.Equal(t, "NET30", a.PaymentTerms.Code)
	require.False(t, a.CreditHold)
	require.Equal(t, d(-10), a.AsOf)

	_, a = asOf(d(-5)) // limit already cut and hold placed that day
	require.Equal(t, kit.MustAmount("600"), a.Attributes.CreditLimit)
	require.Equal(t, "NET30", a.PaymentTerms.Code)
	require.True(t, a.CreditHold)
	require.Equal(t, StateOnHold, a.State)

	_, a = asOf(d(-4)) // NET60 starts; hold still on
	require.Equal(t, "NET60", a.PaymentTerms.Code)
	require.Equal(t, 60, a.PaymentTerms.Days)
	require.True(t, a.CreditHold)
	require.Equal(t, d(-4), a.PaymentTerms.EffectiveFrom)

	_, a = asOf(d(-2)) // released that day
	require.False(t, a.CreditHold)
	require.Equal(t, StateActive, a.State)
	_, a = asOf(d(0))
	require.Equal(t, kit.MustAmount("600"), a.Attributes.CreditLimit)
	require.Equal(t, p.Version, a.Version)

	// A precise instant: just after the hold (day -5) and before the release.
	code, body = e.query(pNobody, "getFinancialProfileAsOf", "profile_id="+p.ProfileID+"&as_of="+time.Now().AddDate(0, 0, -3).UTC().Format(time.RFC3339))
	require.Equal(t, http.StatusOK, code, string(body))
	require.True(t, asProfile(t, body).CreditHold)

	// Lookup by customer instead of profile id.
	code, body = e.query(pNobody, "getFinancialProfileAsOf", "legal_entity_id="+e.entity+"&customer_party_id="+p.CustomerPartyID+"&as_of="+d(-10))
	require.Equal(t, http.StatusOK, code, string(body))
	require.Equal(t, p.ProfileID, asProfile(t, body).ProfileID)

	// The version history keeps every recorded version with derived intervals, and is immutable.
	code, body = e.query(pNobody, "listProfileHistory", "profile_id="+p.ProfileID)
	require.Equal(t, http.StatusOK, code)
	var h struct {
		Items []HistoryEntry `json:"items"`
		Attrs []AttrVersion  `json:"attribute_versions"`
		Terms []TermVersion  `json:"payment_term_versions"`
	}
	require.NoError(t, jsonDecode(body, &h))
	require.Len(t, h.Terms, 2)
	require.Equal(t, d(-4), h.Terms[0].EffectiveTo)
	require.Len(t, h.Attrs, 2) // create + the limit cut
}

// ── command plumbing: idempotency, concurrency, isolation, atomicity ─────────

func TestIdempotentReplayAndKeyReuse(t *testing.T) {
	e := newEnv(t)
	body := createBody(e.entity, uuid.NewString())
	key := uuid.NewString()
	c1, b1, _ := call(t, e.h, http.MethodPost, base+"commands/createFinancialProfile", e.tenant, pMaker, key, body)
	require.Equal(t, http.StatusCreated, c1, string(b1))
	c2, b2, h2 := call(t, e.h, http.MethodPost, base+"commands/createFinancialProfile", e.tenant, pMaker, key, body)
	require.Equal(t, http.StatusCreated, c2)
	require.Equal(t, "true", h2.Get("Idempotent-Replay"))
	require.JSONEq(t, string(b1), string(b2))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar01_financial_profiles WHERE tenant_id = $1`, e.tenant))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, e.tenant))
	other := createBody(e.entity, uuid.NewString())
	c3, b3, _ := call(t, e.h, http.MethodPost, base+"commands/createFinancialProfile", e.tenant, pMaker, key, other)
	require.Equal(t, http.StatusUnprocessableEntity, c3)
	require.Equal(t, kit.CodeIdempotencyReused, errCode(t, b3))

	// A replayed state change does not apply twice.
	p := asProfile(t, b1)
	act := map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "transition": "ACTIVATE"}
	k2 := uuid.NewString()
	call(t, e.h, http.MethodPost, base+"commands/amendFinancialProfile", e.tenant, pMaker, k2, act)
	c4, _, _ := call(t, e.h, http.MethodPost, base+"commands/amendFinancialProfile", e.tenant, pMaker, k2, act)
	require.Equal(t, http.StatusOK, c4)
	require.Equal(t, 2, e.get(p.ProfileID).Version)
	require.Equal(t, 1, e.events(p.ProfileID, EventProfileChanged))

	// No key at all is refused (call() adds one for POST, so build the request by hand).
	rec := httptest.NewRecorder()
	r := newReq(http.MethodPost, base+"commands/placeCreditHold", e.tenant, pMaker, map[string]any{})
	e.h.ServeHTTP(rec, r)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, kit.CodeIdempotencyRequired, errCode(t, rec.Body.Bytes()))
}

func TestOptimisticConcurrency(t *testing.T) {
	e := newEnv(t)
	p := e.active(nil)
	hold := map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version - 1, "reason_code": "R"}
	code, body := e.cmd(pMaker, "placeCreditHold", hold)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeStaleVersion, errCode(t, body))
	delete(hold, "expected_version")
	code, body = e.cmd(pMaker, "placeCreditHold", hold)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, kit.CodeExpectedVersionMissing, errCode(t, body))
	require.Equal(t, 0, e.events(p.ProfileID, EventCreditHoldPlaced))

	// A racing pair of identical holds: one wins, one is stale.
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, _ = call(t, e.h, http.MethodPost, base+"commands/placeCreditHold", e.tenant, pMaker, "", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "R"})
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, c := range codes {
		if c == http.StatusOK {
			wins++
		}
	}
	require.Equal(t, 1, wins)
	require.Equal(t, 1, e.events(p.ProfileID, EventCreditHoldPlaced))
}

func TestTenantAndEntityIsolation(t *testing.T) {
	e := newEnv(t)
	p := e.active(nil)

	other := *e
	other.tenant = uuid.NewString()
	for _, name := range []string{"getFinancialProfile", "getAvailableActions", "getCreditStatus", "listProfileHistory"} {
		code, body := other.query(pMaker, name, "profile_id="+p.ProfileID)
		require.Equal(t, http.StatusNotFound, code, name)
		require.Equal(t, kit.CodeNotFound, errCode(t, body), name)
	}
	code, _ := other.query(pMaker, "getFinancialProfileAsOf", "profile_id="+p.ProfileID+"&as_of="+e.dayOf(0))
	require.Equal(t, http.StatusNotFound, code)
	code, body := other.cmd(pMaker, "placeCreditHold", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "R"})
	require.Equal(t, http.StatusNotFound, code, string(body))
	code, _ = other.query(pMaker, "getFinancialProfile", "legal_entity_id="+e.entity+"&customer_party_id="+p.CustomerPartyID)
	require.Equal(t, http.StatusNotFound, code)
	_, err := e.m.ActiveAt(context.Background(), other.tenant, p.LegalEntityID, p.CustomerPartyID, e.now())
	requireCode(t, err, kit.CodeCustomerProfileInactive)
	require.Equal(t, p.Version, e.get(p.ProfileID).Version)

	// A body entity that disagrees with the stored profile is refused.
	code, body = e.cmd(pMaker, "placeCreditHold", map[string]any{"profile_id": p.ProfileID, "legal_entity_id": uuid.NewString(), "expected_version": p.Version, "reason_code": "R"})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeTenantScope, errCode(t, body))
	// Same customer in another legal entity is a different profile.
	b := createBody(uuid.NewString(), p.CustomerPartyID)
	code, body = e.cmd(pMaker, "createFinancialProfile", b)
	require.Equal(t, http.StatusCreated, code, string(body))
}

func TestAtomicity_RolledBackCommandLeavesNoEvidence(t *testing.T) {
	e := newEnv(t)
	p := e.active(nil)
	ctx := context.Background()
	before := [3]int{e.audits(p.ProfileID), e.events(p.ProfileID, EventCreditHoldPlaced), e.count(`SELECT count(*) FROM ar01_profile_history WHERE profile_id = $1`, p.ProfileID)}
	c := kit.Caller{TenantID: e.tenant, PrincipalID: pMaker, LegalEntityID: e.entity}
	err := kit.NewDB(pool).WithTx(ctx, e.tenant, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, e.tenant, p.ProfileID, true)
		require.NoError(t, err)
		_, err = commit(ctx, tx, c, cur, StateOnHold, hist{At: time.Now(), Command: "placeCreditHold", Reason: "R"}, func(h *Header) []kit.Event {
			return []kit.Event{{AggregateType: AggregateType, AggregateID: h.ProfileID, EventType: EventCreditHoldPlaced, Payload: map[string]any{}}}
		})
		require.NoError(t, err)
		return errors.New("simulated failure after the writes")
	})
	require.Error(t, err)
	after := [3]int{e.audits(p.ProfileID), e.events(p.ProfileID, EventCreditHoldPlaced), e.count(`SELECT count(*) FROM ar01_profile_history WHERE profile_id = $1`, p.ProfileID)}
	require.Equal(t, before, after)
	require.Equal(t, StateActive, e.get(p.ProfileID).State)
}

func TestAppendOnlyHistoryAndVersions(t *testing.T) {
	e := newEnv(t)
	p := e.active(nil)
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE ar01_profile_history SET command = 'x' WHERE profile_id = $1`,
		`DELETE FROM ar01_profile_history WHERE profile_id = $1`,
		`UPDATE ar01_profile_versions SET credit_limit = 99 WHERE profile_id = $1`,
		`DELETE FROM ar01_profile_versions WHERE profile_id = $1`,
	} {
		_, err := pool.Exec(ctx, q, p.ProfileID)
		require.ErrorContains(t, err, "append-only", q)
	}
	// Versions must be recorded in sequence with non-decreasing effective dates.
	ins := func(seq int, from string) error {
		_, err := pool.Exec(ctx, `INSERT INTO ar01_profile_versions (version_id, tenant_id, legal_entity_id, profile_id, seq, effective_from, credit_limit, credit_currency, change_kind, created_by)
			VALUES ($1,$2,$3,$4,$5,$6::date,0,'USD','AMEND','sql')`, uuid.NewString(), e.tenant, e.entity, p.ProfileID, seq, from)
		return err
	}
	require.ErrorContains(t, ins(5, e.dayOf(0)), "AR01_VERSION_SEQUENCE")
	require.ErrorContains(t, ins(2, e.dayOf(-30)), "AR01_VERSION_OVERLAP")
	require.NoError(t, ins(2, e.dayOf(0)))
	// A hold must match the state (data-level invariant).
	_, err := pool.Exec(ctx, `UPDATE ar01_financial_profiles SET credit_hold = true WHERE profile_id = $1`, p.ProfileID)
	require.ErrorContains(t, err, "ar01_hold_matches_state")
}

// ── queries ──────────────────────────────────────────────────────────────────

func TestCreditStatusAndAvailableActions(t *testing.T) {
	e := newEnv(t)
	p := e.activate(e.create(pBoth, map[string]any{"credit_limit_policy_max": "5000", "credit_limit": "1000", "reason_code": "ONBOARD"}))

	e.open.exposure = kit.MustAmount("300.25")
	code, body := e.query(pNobody, "getCreditStatus", "profile_id="+p.ProfileID)
	require.Equal(t, http.StatusOK, code, string(body))
	var cs creditStatus
	require.NoError(t, jsonDecode(body, &cs))
	require.Equal(t, kit.MustAmount("1000"), cs.CreditLimit)
	require.Equal(t, kit.MustAmount("300.25"), *cs.CurrentExposure)
	require.Equal(t, kit.MustAmount("699.75"), *cs.AvailableCredit)
	require.True(t, cs.LimitConfigured)
	require.False(t, cs.OverLimit)
	require.False(t, cs.CreditHold)

	e.open.exposure = kit.MustAmount("1200")
	_, body = e.query(pNobody, "getCreditStatus", "profile_id="+p.ProfileID)
	require.NoError(t, jsonDecode(body, &cs))
	require.True(t, cs.OverLimit)
	require.Equal(t, kit.MustAmount("-200"), *cs.AvailableCredit)

	e.open.err = errors.New("ar07 down")
	code, body = e.query(pNobody, "getCreditStatus", "profile_id="+p.ProfileID)
	require.Equal(t, http.StatusServiceUnavailable, code, "exposure unknown must fail closed: "+string(body))
	e.open.err = nil

	// Without an exposure source the answer says so rather than guessing.
	e.m.openItems = nil
	_, body = e.query(pNobody, "getCreditStatus", "profile_id="+p.ProfileID)
	cs = creditStatus{}
	require.NoError(t, jsonDecode(body, &cs))
	require.False(t, cs.ExposureAvailable)
	require.Nil(t, cs.AvailableCredit)
	e.m.openItems = e.open

	// Hold shows in the credit status.
	code, body = e.cmd(pMaker, "placeCreditHold", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "OVERDUE"})
	p = e.ok(code, body)
	_, body = e.query(pNobody, "getCreditStatus", "profile_id="+p.ProfileID)
	cs = creditStatus{}
	require.NoError(t, jsonDecode(body, &cs))
	require.True(t, cs.CreditHold)
	require.Equal(t, "OVERDUE", cs.HoldReasonCode)

	// Available actions: derived from state and permissions.
	actions := func(principal string) map[string]bool {
		code, body := e.query(principal, "getAvailableActions", "profile_id="+p.ProfileID)
		require.Equal(t, http.StatusOK, code, string(body))
		var out struct {
			Actions []actionState `json:"actions"`
		}
		require.NoError(t, jsonDecode(body, &out))
		m := map[string]bool{}
		for _, a := range out.Actions {
			m[a.Action] = a.Available
		}
		return m
	}
	a := actions(pMaker) // ON_HOLD, manager
	require.True(t, a["ReleaseCreditHold"])
	require.False(t, a["PlaceCreditHold"])
	require.True(t, a["RetireFinancialProfile"])
	require.False(t, a["ActivateProfile"])
	require.False(t, a["ApproveCreditLimitChange"], "nothing pending")
	a = actions(pNobody)
	for name, ok := range a {
		require.False(t, ok, name)
	}
	// With a pending increase: the approver may approve, the requester may not.
	code, body = e.cmd(pBoth, "releaseCreditHold", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "reason_code": "PAID"})
	p = e.ok(code, body)
	code, body = e.cmd(pBoth, "setCreditLimit", map[string]any{"profile_id": p.ProfileID, "expected_version": p.Version, "credit_limit": "9000", "credit_currency": "USD", "reason_code": "GROWTH"})
	require.Equal(t, http.StatusAccepted, code, string(body))
	require.True(t, actions(pApprover)["ApproveCreditLimitChange"])
	require.False(t, actions(pBoth)["ApproveCreditLimitChange"], "SoD: the requester cannot approve")
	require.False(t, actions(pBoth)["SetCreditLimit"], "one pending change at a time")
}

func TestQueriesAcceptPostAndLookupByCustomer(t *testing.T) {
	e := newEnv(t)
	p := e.active(nil)
	code, body, _ := call(t, e.h, http.MethodPost, base+"getFinancialProfile", e.tenant, pNobody, "", map[string]any{"profile_id": p.ProfileID})
	require.Equal(t, http.StatusOK, code, string(body))
	require.Equal(t, p.ProfileID, asProfile(t, body).ProfileID)
	code, body = e.query(pNobody, "getFinancialProfile", "legal_entity_id="+e.entity+"&customer_party_id="+p.CustomerPartyID)
	require.Equal(t, http.StatusOK, code, string(body))
	code, _ = e.query(pNobody, "getFinancialProfile", "")
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = e.query(pNobody, "getFinancialProfile", "profile_id="+uuid.NewString())
	require.Equal(t, http.StatusNotFound, code)
	code, _ = e.query(pNobody, "getFinancialProfileAsOf", "profile_id="+p.ProfileID)
	require.Equal(t, http.StatusBadRequest, code, "as_of is required")
}
