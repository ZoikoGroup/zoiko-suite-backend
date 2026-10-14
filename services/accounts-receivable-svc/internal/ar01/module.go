package ar01

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// Module is AR-01. It implements kit.ProfileResolver for AR-02/03/04.
type Module struct {
	deps      kit.Deps
	parties   PartyDirectory
	openItems kit.OpenItems // optional: credit exposure for GetCreditStatus (AR-07)
}

var _ kit.ProfileResolver = (*Module)(nil)

// New builds the module. parties is required (the party master check fails
// closed); openItems may be nil until AR-07 is wired, in which case
// GetCreditStatus reports exposure as unavailable rather than guessing.
func New(deps kit.Deps, parties PartyDirectory, openItems kit.OpenItems) *Module {
	return &Module{deps: deps, parties: parties, openItems: openItems}
}

// Register mounts the spec paths verbatim at the router root.
func (m *Module) Register(r chi.Router) {
	for _, q := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"getFinancialProfile", m.getFinancialProfile},
		{"getFinancialProfileAsOf", m.getFinancialProfileAsOf},
		{"getAvailableActions", m.getAvailableActions},
		{"getCreditStatus", m.getCreditStatus},
		{"listProfileHistory", m.listProfileHistory},
	} {
		h := m.deps.Query(q.h)
		r.Get("/internal/v1/ar01/"+q.name, h)
		r.Post("/internal/v1/ar01/"+q.name, h)
	}
	for _, c := range []struct {
		path, op string
		h        http.HandlerFunc
	}{
		{"createFinancialProfile", "AR01.CreateFinancialProfile", m.createFinancialProfile},
		{"amendFinancialProfile", "AR01.AmendFinancialProfile", m.amendFinancialProfile},
		{"setCreditLimit", "AR01.SetCreditLimit", m.setCreditLimit},
		{"approveCreditLimitChange", "AR01.ApproveCreditLimitChange", m.approveCreditLimitChange},
		{"placeCreditHold", "AR01.PlaceCreditHold", m.placeCreditHold},
		{"releaseCreditHold", "AR01.ReleaseCreditHold", m.releaseCreditHold},
		{"changePaymentTerms", "AR01.ChangePaymentTerms", m.changePaymentTerms},
		{"retireFinancialProfile", "AR01.RetireFinancialProfile", m.retireFinancialProfile},
	} {
		r.Post("/internal/v1/ar01/commands/"+c.path, m.deps.Cmd(c.op, c.h))
	}
}

// read runs fn in a tenant-scoped transaction (sets app.tenant_id so FORCE'd RLS binds).
func (m *Module) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return m.deps.DB.WithTx(ctx, tenantID, fn)
}

// ActiveAt implements kit.ProfileResolver: the single effective, ACTIVE profile
// at a date, or CUSTOMER_PROFILE_INACTIVE. A future-dated, expired, draft,
// suspended or retired profile never resolves, and there is no fallback to a
// stale one. Overlapping versions (impossible by schema) would be VALIDATION_FAILED.
func (m *Module) ActiveAt(ctx context.Context, tenantID, legalEntityID, customerPartyID string, at time.Time) (*kit.ProfileView, error) {
	if at.IsZero() {
		at = m.deps.Now()
	}
	date := day(at)
	var view *kit.ProfileView
	err := m.read(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+headerCols+` FROM ar01_financial_profiles
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND customer_party_id = $3 AND state <> 'RETIRED'`,
			tenantID, legalEntityID, customerPartyID)
		if err != nil {
			return kit.MapPgErr(err)
		}
		var hs []*Header
		for rows.Next() {
			h, err := scanHeader(rows)
			if err != nil {
				rows.Close()
				return err
			}
			hs = append(hs, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		switch {
		case len(hs) == 0:
			return errProfileInactive("no live financial profile for this customer")
		case len(hs) > 1:
			return kit.Invalid("ambiguous financial profile: more than one live profile for this customer")
		}
		h := hs[0]
		if !h.State.Resolvable() {
			return errProfileInactive("financial profile is " + string(h.State))
		}
		a, err := attrAt(ctx, tx, tenantID, h.ProfileID, date)
		if err != nil {
			return err
		}
		if a == nil {
			return errProfileInactive("financial profile is not yet effective on " + date)
		}
		ts, err := termsAt(ctx, tx, tenantID, h.ProfileID, date)
		if err != nil {
			return err
		}
		switch {
		case len(ts) == 0:
			return errProfileInactive("no payment terms are effective on " + date)
		case len(ts) > 1:
			return kit.Invalid("ambiguous payment terms: overlapping effective versions on " + date)
		}
		from := ts[0].EffectiveFrom
		if a.EffectiveFrom > from {
			from = a.EffectiveFrom
		}
		ef, _ := time.Parse(dateLayout, from)
		view = &kit.ProfileView{
			ProfileID: h.ProfileID, CustomerPartyID: h.CustomerPartyID, LegalEntityID: h.LegalEntityID, Version: h.Version,
			PaymentTermsCode: ts[0].Code, PaymentTermsDays: ts[0].Days, CreditLimit: a.CreditLimit, CreditCurrency: a.CreditCurrency,
			CreditHold: h.CreditHold, DunningProfile: a.DunningProfile, EffectiveFrom: ef,
		}
		return nil
	})
	if err != nil {
		var p *kit.Problem
		if errors.As(err, &p) {
			return nil, err
		}
		m.deps.Log.Error("ar01 profile resolution failed", zap.Error(err))
		return nil, kit.Unavailable("financial profile store unavailable")
	}
	return view, nil
}

// requireParty asks the party master whether the customer may carry financial
// changes. A definite "no" is CUSTOMER_PROFILE_INACTIVE (422); an unanswerable
// master is 503 (fail closed).
func (m *Module) requireParty(ctx context.Context, c kit.Caller, partyID string) error {
	p, err := m.parties.GetParty(ctx, c, partyID)
	switch {
	case errors.Is(err, ErrPartyNotFound):
		return errProfileInactive("customer party not found")
	case err != nil:
		if !errors.Is(err, svcclient.ErrUnavailable) {
			m.deps.Log.Error("party master lookup failed", zap.Error(err))
		}
		return kit.Unavailable("party master unavailable")
	}
	return PartyUsable(p, c.TenantID, m.deps.Now())
}

func (m *Module) today() string { return day(m.deps.Now()) }

func (m *Module) fail(w http.ResponseWriter, err error) {
	var p *kit.Problem
	if !errors.As(err, &p) {
		m.deps.Log.Error("ar01 request failed", zap.Error(err))
	}
	kit.WriteErr(w, err)
}
