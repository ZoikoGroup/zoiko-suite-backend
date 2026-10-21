package ar01

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

func TestStateMachine(t *testing.T) {
	ok := []struct {
		from State
		tr   Transition
		to   State
	}{
		{StateDraft, TrActivate, StateActive}, {StateActive, TrSuspend, StateSuspended}, {StateSuspended, TrReinstate, StateActive},
		{StateActive, TrHold, StateOnHold}, {StateOnHold, TrRelease, StateActive},
		{StateDraft, TrRetire, StateRetired}, {StateActive, TrRetire, StateRetired}, {StateOnHold, TrRetire, StateRetired}, {StateSuspended, TrRetire, StateRetired},
	}
	for _, c := range ok {
		got, err := Next(c.from, c.tr)
		require.NoError(t, err)
		require.Equal(t, c.to, got)
	}
	bad := []struct {
		from State
		tr   Transition
	}{
		{StateDraft, TrHold}, {StateDraft, TrSuspend}, {StateActive, TrActivate}, {StateOnHold, TrSuspend}, {StateActive, TrRelease},
		{StateSuspended, TrHold}, {StateRetired, TrActivate}, {StateRetired, TrRetire}, {StateRetired, TrReinstate},
	}
	for _, c := range bad {
		_, err := Next(c.from, c.tr)
		var p *kit.Problem
		require.ErrorAs(t, err, &p)
		require.Equal(t, kit.CodeInvalidTransition, p.Code)
		require.Equal(t, http.StatusConflict, p.Status)
	}
}

func TestResolvable(t *testing.T) {
	require.True(t, StateActive.Resolvable())
	require.True(t, StateOnHold.Resolvable(), "an on-hold profile resolves, flagged, so callers can answer CREDIT_HOLD_ACTIVE")
	for _, s := range []State{StateDraft, StateSuspended, StateRetired} {
		require.False(t, s.Resolvable())
	}
}

func TestAttributeValidation(t *testing.T) {
	good := Attributes{CreditLimit: kit.MustAmount("1000"), CreditCurrency: "USD"}
	require.NoError(t, good.Validate())

	bad := []Attributes{
		{CreditLimit: kit.MustAmount("1"), CreditCurrency: "usd"},
		{CreditLimit: kit.MustAmount("-1"), CreditCurrency: "USD"},
		{CreditLimit: kit.MustAmount("1.5"), CreditCurrency: "JPY"}, // more precision than the currency allows
		{CreditLimit: 0, CreditCurrency: "USD", InvoicePreferences: InvoicePrefs{DeliveryChannel: "FAX"}},
		{CreditLimit: 0, CreditCurrency: "USD", StatementPreferences: StatementPrefs{Frequency: "DAILY"}},
		{CreditLimit: 0, CreditCurrency: "USD", DunningProfile: "has space"},
		{CreditLimit: 0, CreditCurrency: "USD", RiskAttributes: RiskAttributes{Rating: "HIGH"}}, // no provenance
		{CreditLimit: 0, CreditCurrency: "USD", RiskAttributes: RiskAttributes{Rating: "EXTREME", Source: "bureau"}},
	}
	for i, a := range bad {
		require.Error(t, a.Validate(), "case %d", i)
	}
	score := 700
	withRisk := good
	withRisk.RiskAttributes = RiskAttributes{Rating: "LOW", Score: &score, Source: "bureau", SourceRef: "REP-1", AssessedOn: "2026-01-01"}
	require.NoError(t, withRisk.Validate())
}

func TestTermsValidation(t *testing.T) {
	require.NoError(t, ValidateTerms("NET30", 30))
	require.Error(t, ValidateTerms("net 30", 30))
	require.Error(t, ValidateTerms("NET30", -1))
	require.Error(t, ValidateTerms("NET30", 366))
}

func TestPartyUsable(t *testing.T) {
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	require.NoError(t, PartyUsable(&Party{Type: "CUSTOMER", Status: "ACTIVE"}, "t1", now))
	require.NoError(t, PartyUsable(&Party{Type: "CUSTOMER", Status: "ACTIVE", TenantID: "t1", EffectiveTo: "2027-01-01"}, "t1", now))
	for name, p := range map[string]*Party{
		"nil":        nil,
		"vendor":     {Type: "VENDOR", Status: "ACTIVE"},
		"suspended":  {Type: "CUSTOMER", Status: "SUSPENDED"},
		"onboarding": {Type: "CUSTOMER", Status: "ONBOARDING"},
		"terminated": {Type: "CUSTOMER", Status: "TERMINATED"},
		"ended":      {Type: "CUSTOMER", Status: "ACTIVE", EffectiveTo: "2026-10-12T00:00:00Z"},
		"foreign":    {Type: "CUSTOMER", Status: "ACTIVE", TenantID: "other"},
	} {
		err := PartyUsable(p, "t1", now)
		var pr *kit.Problem
		require.ErrorAs(t, err, &pr, name)
		require.Equal(t, kit.CodeCustomerProfileInactive, pr.Code, name)
	}
}

func TestCounterpartyDirectory_RealWireShape(t *testing.T) {
	var gotPath, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTenant = r.URL.Path, r.Header.Get("X-Tenant-Id")
		switch {
		case strings.HasSuffix(r.URL.Path, "/missing"):
			http.Error(w, `{"error":"counterparty not found"}`, http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/broken"):
			http.Error(w, `boom`, http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/garbage"):
			_, _ = w.Write([]byte(`not json`))
		default:
			_, _ = w.Write([]byte(`{"counterparty_id":"p1","tenant_id":"t1","legal_entity_id":"e1","name":"Acme","counterparty_type":"CUSTOMER","status":"ACTIVE","effective_from":"2026-01-01"}`))
		}
	}))
	defer srv.Close()
	d := NewCounterpartyDirectory(srv.URL)
	c := kit.Caller{TenantID: "t1", PrincipalID: "u"}

	p, err := d.GetParty(context.Background(), c, "p1")
	require.NoError(t, err)
	require.Equal(t, "/v1/counterparties/p1", gotPath)
	require.Equal(t, "t1", gotTenant)
	require.Equal(t, "CUSTOMER", p.Type)
	require.Equal(t, "ACTIVE", p.Status)
	require.NoError(t, PartyUsable(p, "t1", time.Now()))

	_, err = d.GetParty(context.Background(), c, "missing")
	require.ErrorIs(t, err, ErrPartyNotFound)
	for _, id := range []string{"broken", "garbage"} {
		_, err = d.GetParty(context.Background(), c, id)
		require.ErrorIs(t, err, svcclient.ErrUnavailable, id)
	}
	srv.Close()
	_, err = d.GetParty(context.Background(), c, "p1")
	require.ErrorIs(t, err, svcclient.ErrUnavailable, "transport failure must be unavailable, never 'no problem'")
}
