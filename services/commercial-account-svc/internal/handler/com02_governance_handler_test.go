package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

type govStub struct {
	err      error
	calls    []string
	tenant   string
	decision string
	propose  domain.ProposeDiscountParams
}

func (g *govStub) rec(ctx context.Context, name string) {
	g.calls = append(g.calls, name)
	g.tenant = svcmiddleware.TenantFromContext(ctx)
}
func (g *govStub) ProposeDiscount(ctx context.Context, p domain.ProposeDiscountParams, _ domain.IdempotencyClaim) (*domain.DiscountApplication, error) {
	g.rec(ctx, "ProposeDiscount")
	g.propose = p
	return &domain.DiscountApplication{DiscountApplicationID: p.DiscountApplicationID, RowVersion: 1}, g.err
}
func (g *govStub) DecideDiscount(ctx context.Context, _, id string, _ int, decision, _, _ string, _ time.Time, _ domain.IdempotencyClaim) (*domain.DiscountApplication, error) {
	g.rec(ctx, "DecideDiscount")
	g.decision = decision
	return &domain.DiscountApplication{DiscountApplicationID: id, RowVersion: 2}, g.err
}
func (g *govStub) ListDiscounts(ctx context.Context, _ string) ([]domain.DiscountApplication, error) {
	g.rec(ctx, "ListDiscounts")
	return nil, g.err
}
func (g *govStub) CreateMigrationOffer(ctx context.Context, o *domain.MigrationOffer, _ domain.IdempotencyClaim) (*domain.MigrationOffer, error) {
	g.rec(ctx, "CreateMigrationOffer")
	return o, g.err
}
func (g *govStub) PublishMigrationOffer(ctx context.Context, id string, _ int, _ string, _ time.Time, _ domain.IdempotencyClaim) (*domain.MigrationOffer, error) {
	g.rec(ctx, "PublishMigrationOffer")
	return &domain.MigrationOffer{MigrationOfferID: id}, g.err
}
func (g *govStub) WithdrawMigrationOffer(ctx context.Context, id string, _ int, _, _ string, _ time.Time, _ domain.IdempotencyClaim) (*domain.MigrationOffer, error) {
	g.rec(ctx, "WithdrawMigrationOffer")
	return &domain.MigrationOffer{MigrationOfferID: id}, g.err
}
func (g *govStub) GetMigrationOffer(ctx context.Context, id string) (*domain.MigrationOffer, error) {
	g.rec(ctx, "GetMigrationOffer")
	return &domain.MigrationOffer{MigrationOfferID: id}, g.err
}
func (g *govStub) ListMigrationOffersFor(ctx context.Context, _ string, _ time.Time) ([]domain.MigrationOffer, error) {
	g.rec(ctx, "ListMigrationOffersFor")
	return nil, g.err
}
func (g *govStub) ProcessNextBoundary(context.Context, time.Time) (bool, error) { return false, nil }

func newGovRouter(st *subStub, gov *govStub, az *scopedAuthz) http.Handler {
	logger, _ := zap.NewDevelopment()
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterSubscriptionV2Routes(r, NewSubscriptionHandler(st, &ruleStub{}, az, logger).WithGovernance(gov).
		WithClock(func() time.Time { return fixedNow }))
	return r
}

const assisted = `"assisted":{"organization_id":"` + customerOrg + `","customer_basis_ref":"order form SO-77"}`

func TestGovernanceHandler_DiscountsAreSalesActionsWithSeparateApproval(t *testing.T) {
	subID := domain.NewCommercialID(domain.PrefixSubscription)
	gov := &govStub{}
	az := &scopedAuthz{}
	w := serve(t, newGovRouter(&subStub{}, gov, az), req{method: http.MethodPost, path: "/v1/commercial/subscriptions/" + subID + "/discounts",
		body: `{"product_code":"business","component_key":"commit_15","reason":"3-year deal",` + assisted + `}`, headers: tenantHeaders(nil)})
	if w.Code != http.StatusCreated || gov.tenant != customerOrg || gov.propose.CustomerBasisRef != "order form SO-77" ||
		az.checked[0] != platformScopeID+"|"+ActionDiscountPropose {
		t.Fatalf("propose: HTTP %d tenant=%s checked=%v %s", w.Code, gov.tenant, az.checked, w.Body.String())
	}

	gov = &govStub{}
	w = serve(t, newGovRouter(&subStub{}, gov, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/subscriptions/" + subID + "/discounts",
		body: `{"product_code":"business","component_key":"commit_15","reason":"self-serve"}`, headers: tenantHeaders(nil)})
	if w.Code != http.StatusBadRequest || len(gov.calls) != 0 {
		t.Fatalf("a customer proposed its own discount: HTTP %d", w.Code)
	}

	did := domain.NewCommercialID(domain.PrefixDiscountApplication)
	for action, perm := range map[string]string{"approve": ActionDiscountApprove, "reject": ActionDiscountApprove, "withdraw": ActionDiscountPropose} {
		gov = &govStub{}
		az = &scopedAuthz{}
		w = serve(t, newGovRouter(&subStub{}, gov, az), req{method: http.MethodPost,
			path: "/v1/commercial/subscriptions/" + subID + "/discounts/" + did + ":" + action,
			body: `{"reason":"because",` + assisted + `}`, headers: tenantHeaders(map[string]string{"If-Match": `"1"`})})
		if w.Code != http.StatusOK || gov.decision != action || az.checked[0] != platformScopeID+"|"+perm {
			t.Errorf("%s: HTTP %d decision=%s checked=%v", action, w.Code, gov.decision, az.checked)
		}
	}
	gov = &govStub{err: domain.ErrSoDViolation}
	w = serve(t, newGovRouter(&subStub{}, gov, &scopedAuthz{}), req{method: http.MethodPost,
		path: "/v1/commercial/subscriptions/" + subID + "/discounts/" + did + ":approve",
		body: `{` + assisted + `}`, headers: tenantHeaders(map[string]string{"If-Match": `"1"`})})
	if w.Code != http.StatusForbidden || problemCode(t, w) != CodeSoDViolation {
		t.Fatalf("self-approval: HTTP %d", w.Code)
	}
}

func TestGovernanceHandler_MigrationOffers(t *testing.T) {
	gov := &govStub{}
	az := &scopedAuthz{}
	body := `{"from_price_version_id":"` + domain.NewCommercialID(domain.PrefixPriceVersion) + `","to_price_version_id":"` +
		domain.NewCommercialID(domain.PrefixPriceVersion) + `","reason":"2030 repricing"}`
	w := serve(t, newGovRouter(&subStub{}, gov, az), req{method: http.MethodPost, path: "/v1/commercial/migration-offers",
		body: body, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusCreated || az.checked[0] != platformScopeID+"|"+ActionPriceBookPropose {
		t.Fatalf("create offer: HTTP %d checked=%v %s", w.Code, az.checked, w.Body.String())
	}

	id := domain.NewCommercialID(domain.PrefixMigrationOffer)
	gov = &govStub{err: domain.ErrMigrationEligibilityMissing}
	az = &scopedAuthz{}
	w = serve(t, newGovRouter(&subStub{}, gov, az), req{method: http.MethodPost, path: "/v1/commercial/migration-offers/" + id + ":publish",
		headers: map[string]string{"Idempotency-Key": "k", "If-Match": `"1"`}})
	if w.Code != http.StatusUnprocessableEntity || problemCode(t, w) != CodeMigrationEligibilityMissing ||
		az.checked[0] != platformScopeID+"|"+ActionPriceBookPublish {
		t.Fatalf("publish without eligibility: HTTP %d checked=%v", w.Code, az.checked)
	}

	st := &subStub{view: subView()}
	w = serve(t, newGovRouter(st, &govStub{}, &scopedAuthz{}), req{method: http.MethodPost, path: subPath("accept-migration"),
		body:    `{"migration_offer_id":"` + id + `","accepted_terms_sha256":"` + quote + `","expected_quote_sha256":"` + quote + `"}`,
		headers: tenantHeaders(map[string]string{"If-Match": `"3"`})})
	if w.Code != http.StatusOK || st.change.Kind != domain.ChangeKindMigration || st.change.MigrationOfferID != id {
		t.Fatalf("accept-migration: HTTP %d change=%+v %s", w.Code, st.change, w.Body.String())
	}
	st = &subStub{view: subView()}
	w = serve(t, newGovRouter(st, &govStub{}, &scopedAuthz{}), req{method: http.MethodPost, path: subPath("accept-migration"),
		body:    `{"migration_offer_id":"` + id + `","accepted_terms_sha256":"` + quote + `","quantities":{"seats":"99"},"expected_quote_sha256":"` + quote + `"}`,
		headers: tenantHeaders(map[string]string{"If-Match": `"3"`})})
	if w.Code != http.StatusBadRequest || len(st.calls) != 0 {
		t.Fatalf("a migration that also changed quantities was accepted: HTTP %d", w.Code)
	}
}
