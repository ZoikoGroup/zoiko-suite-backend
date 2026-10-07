package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/commercial-account-svc/internal/authz"
	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

const (
	tenantOrg   = "11111111-1111-1111-1111-111111111111"
	customerOrg = "22222222-2222-2222-2222-222222222222"
)

// scopedAuthz records every (scope, action) it is asked about and denies the
// pairs listed in deny.
type scopedAuthz struct {
	mu      sync.Mutex
	deny    map[string]error
	checked []string
}

func (a *scopedAuthz) CheckAllowed(_ context.Context, _, scope, action string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checked = append(a.checked, scope+"|"+action)
	return a.deny[scope+"|"+action]
}

type subStub struct {
	err          error
	view         *domain.SubscriptionView
	calls        []string
	tenant       string
	claim        domain.IdempotencyClaim
	start        domain.StartSubscriptionParams
	cmd          domain.SubscriptionCommand
	confirmation *bool
	change       domain.ChangeRequest
	quote        string
}

func (s *subStub) rec(ctx context.Context, name string, claim domain.IdempotencyClaim) {
	s.calls = append(s.calls, name)
	s.tenant, s.claim = svcmiddleware.TenantFromContext(ctx), claim
}

func (s *subStub) SetAccountMarket(ctx context.Context, _, _, _ string, _ time.Time) error {
	s.rec(ctx, "SetAccountMarket", domain.IdempotencyClaim{})
	return s.err
}
func (s *subStub) StartSubscription(ctx context.Context, p domain.StartSubscriptionParams, c domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	s.rec(ctx, "StartSubscription", c)
	s.start = p
	return s.view, s.err
}
func (s *subStub) ActivateSubscription(ctx context.Context, cmd domain.SubscriptionCommand, confirmation bool, c domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	s.rec(ctx, "ActivateSubscription", c)
	s.cmd, s.confirmation = cmd, &confirmation
	return s.view, s.err
}
func (s *subStub) ScheduleCancellation(ctx context.Context, cmd domain.SubscriptionCommand, c domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	s.rec(ctx, "ScheduleCancellation", c)
	s.cmd = cmd
	return s.view, s.err
}
func (s *subStub) CancelNow(ctx context.Context, cmd domain.SubscriptionCommand, c domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	s.rec(ctx, "CancelNow", c)
	s.cmd = cmd
	return s.view, s.err
}
func (s *subStub) Reactivate(ctx context.Context, cmd domain.SubscriptionCommand, c domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	s.rec(ctx, "Reactivate", c)
	return s.view, s.err
}
func (s *subStub) Renew(ctx context.Context, cmd domain.SubscriptionCommand, c domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	s.rec(ctx, "Renew", c)
	return s.view, s.err
}
func (s *subStub) GetSubscriptionAsOf(ctx context.Context, _ string, _ time.Time) (*domain.SubscriptionView, error) {
	s.rec(ctx, "GetSubscriptionAsOf", domain.IdempotencyClaim{})
	return s.view, nil
}
func (s *subStub) GetEffectiveVersion(ctx context.Context, _ string, _ time.Time) (*domain.SubscriptionVersion, error) {
	s.rec(ctx, "GetEffectiveVersion", domain.IdempotencyClaim{})
	return &domain.SubscriptionVersion{}, s.err
}
func (s *subStub) GetChangeHistory(ctx context.Context, _ string) ([]domain.SubscriptionVersion, error) {
	s.rec(ctx, "GetChangeHistory", domain.IdempotencyClaim{})
	return nil, s.err
}
func (s *subStub) GetRenewalState(ctx context.Context, _ string, _ time.Time) (*domain.RenewalState, error) {
	s.rec(ctx, "GetRenewalState", domain.IdempotencyClaim{})
	return &domain.RenewalState{}, s.err
}
func (s *subStub) PreviewChange(ctx context.Context, _ string, req domain.ChangeRequest, _ time.Time) (*domain.ChangeQuote, error) {
	s.rec(ctx, "PreviewChange", domain.IdempotencyClaim{})
	s.change = req
	return &domain.ChangeQuote{QuoteSHA256: "q"}, s.err
}
func (s *subStub) RequestChange(ctx context.Context, cmd domain.SubscriptionCommand, req domain.ChangeRequest, quote string, c domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	s.rec(ctx, "RequestChange", c)
	s.cmd, s.change, s.quote = cmd, req, quote
	return s.view, s.err
}
func (s *subStub) GetChanges(ctx context.Context, _ string) ([]domain.SubscriptionChange, error) {
	s.rec(ctx, "GetChanges", domain.IdempotencyClaim{})
	return nil, s.err
}

var fixedNow = time.Date(2030, 3, 1, 12, 0, 0, 0, time.UTC)

func newSubRouter(st *subStub, az *scopedAuthz) http.Handler {
	logger, _ := zap.NewDevelopment()
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterSubscriptionV2Routes(r, NewSubscriptionHandler(st, nil, az, logger).WithClock(func() time.Time { return fixedNow }))
	return r
}

func subView() *domain.SubscriptionView {
	return &domain.SubscriptionView{Subscription: domain.Subscription{
		SubscriptionID: domain.NewCommercialID(domain.PrefixSubscription), OrganizationID: customerOrg, RowVersion: 3}}
}

func tenantHeaders(extra map[string]string) map[string]string {
	h := map[string]string{"X-Tenant-Id": tenantOrg, "Idempotency-Key": "k-1"}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

const startBody = `{"commercial_account_id":"33333333-3333-3333-3333-333333333333","product_code":"business",
	"quantities":{"seats":"5"},"accepted_terms_sha256":"` + "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd" + `"}`

func TestSubscriptionHandler_SelfServiceStartActsOnTheVerifiedTenantOnly(t *testing.T) {
	st := &subStub{view: subView()}
	az := &scopedAuthz{}
	w := serve(t, newSubRouter(st, az), req{method: http.MethodPost, path: "/v1/commercial/subscriptions:start",
		body: startBody, headers: tenantHeaders(nil)})
	if w.Code != http.StatusCreated || w.Header().Get("ETag") != `"3"` {
		t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
	}
	if len(az.checked) != 1 || az.checked[0] != tenantOrg+"|"+ActionSubscriptionManage {
		t.Fatalf("authorization: %v, want the customer's own manage grant", az.checked)
	}
	if st.tenant != tenantOrg || st.claim.OwnerScope != tenantOrg || st.start.Channel != domain.ChannelSelfService ||
		st.start.CustomerBasisRef != nil || !st.start.StartsAt.Equal(fixedNow) || st.start.Quantities["seats"] != "5" {
		t.Fatalf("store received: tenant=%s claimScope=%s params=%+v", st.tenant, st.claim.OwnerScope, st.start)
	}

	st = &subStub{view: subView()}
	w = serve(t, newSubRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/subscriptions:start",
		body: startBody, headers: map[string]string{"Idempotency-Key": "k-1"}})
	if w.Code != http.StatusUnauthorized || len(st.calls) != 0 {
		t.Fatalf("a start without a verified tenant: HTTP %d, calls %v", w.Code, st.calls)
	}
}

// §4.2: an assisted change records the operator and the customer's basis,
// and needs the operator's platform grant — not the customer's.
func TestSubscriptionHandler_AssistedStartNeedsBasisAndPlatformGrant(t *testing.T) {
	assisted := strings.TrimSuffix(startBody, "}") + `,"assisted":{"organization_id":"` + customerOrg + `","customer_basis_ref":"signed order form SO-1182"}}`
	st := &subStub{view: subView()}
	az := &scopedAuthz{}
	w := serve(t, newSubRouter(st, az), req{method: http.MethodPost, path: "/v1/commercial/subscriptions:start",
		body: assisted, headers: tenantHeaders(nil)})
	if w.Code != http.StatusCreated {
		t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
	}
	if len(az.checked) != 1 || az.checked[0] != platformScopeID+"|"+ActionSubscriptionAssist {
		t.Fatalf("authorization: %v", az.checked)
	}
	if st.tenant != customerOrg || st.claim.OwnerScope != customerOrg || st.start.Channel != domain.ChannelAssisted ||
		st.start.CustomerBasisRef == nil || *st.start.CustomerBasisRef != "signed order form SO-1182" {
		t.Fatalf("assisted scope: tenant=%s params=%+v", st.tenant, st.start)
	}

	noBasis := strings.Replace(assisted, "signed order form SO-1182", " ", 1)
	st = &subStub{view: subView()}
	w = serve(t, newSubRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/subscriptions:start",
		body: noBasis, headers: tenantHeaders(nil)})
	if w.Code != http.StatusBadRequest || len(st.calls) != 0 {
		t.Fatalf("an assisted start without a customer basis: HTTP %d", w.Code)
	}

	denied := &scopedAuthz{deny: map[string]error{platformScopeID + "|" + ActionSubscriptionAssist: authzpkg.ErrAuthorizationDenied}}
	st = &subStub{view: subView()}
	w = serve(t, newSubRouter(st, denied), req{method: http.MethodPost, path: "/v1/commercial/subscriptions:start",
		body: assisted, headers: tenantHeaders(nil)})
	if w.Code != http.StatusForbidden || len(st.calls) != 0 {
		t.Fatalf("an operator without the assist grant: HTTP %d, calls %v", w.Code, st.calls)
	}
}

// A customer's activate is only ever a trial confirmation; paid activation
// needs the operator's activation grant.
func TestSubscriptionHandler_ActivationAuthority(t *testing.T) {
	id := domain.NewCommercialID(domain.PrefixSubscription)
	path := "/v1/commercial/subscriptions/" + id + ":activate"

	st := &subStub{view: subView()}
	serve(t, newSubRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: path, headers: tenantHeaders(map[string]string{"If-Match": `"3"`})})
	if st.confirmation == nil || !*st.confirmation || st.cmd.ExpectedVersion != 3 {
		t.Fatalf("self-service activate must reach the store as a customer confirmation: %v", st.confirmation)
	}

	st = &subStub{view: subView()}
	az := &scopedAuthz{}
	body := `{"assisted":{"organization_id":"` + customerOrg + `","customer_basis_ref":"invoice INV-9 paid"}}`
	serve(t, newSubRouter(st, az), req{method: http.MethodPost, path: path, body: body, headers: tenantHeaders(map[string]string{"If-Match": `"3"`})})
	if st.confirmation == nil || *st.confirmation || az.checked[0] != platformScopeID+"|"+ActionSubscriptionActivate {
		t.Fatalf("operator activate: confirmation=%v checked=%v", st.confirmation, az.checked)
	}
}

func TestSubscriptionHandler_RefusesUnsafeInput(t *testing.T) {
	cases := map[string]string{
		"card number as payment method": strings.TrimSuffix(startBody, "}") + `,"payment_method_ref":"4111 1111 1111 1111"}`,
		"start in the past":             strings.TrimSuffix(startBody, "}") + `,"starts_at":"2030-02-01T00:00:00Z"}`,
		"client-chosen price":           strings.TrimSuffix(startBody, "}") + `,"price_version_id":"cpv_00000000-0000-4000-8000-000000000000"}`,
		"client-chosen currency":        strings.TrimSuffix(startBody, "}") + `,"currency_code":"USD"}`,
		"tenant uuid as expected price": strings.TrimSuffix(startBody, "}") + `,"expected_price_version_id":"3f2504e0-4f89-11d3-9a0c-0305e82c3301"}`,
	}
	for name, body := range cases {
		st := &subStub{view: subView()}
		w := serve(t, newSubRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/subscriptions:start",
			body: body, headers: tenantHeaders(nil)})
		if w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: HTTP %d %s", name, w.Code, w.Body.String())
		}
		if len(st.calls) != 0 {
			t.Errorf("%s: reached the store", name)
		}
	}
	st := &subStub{view: subView()}
	w := serve(t, newSubRouter(st, &scopedAuthz{}), req{method: http.MethodGet,
		path: "/v1/commercial/subscriptions/3f2504e0-4f89-11d3-9a0c-0305e82c3301", headers: tenantHeaders(nil)})
	if w.Code != http.StatusUnprocessableEntity || problemCode(t, w) != CodeCrossPlaneAccessDenied {
		t.Fatalf("a tenant-plane uuid as a subscription id: HTTP %d", w.Code)
	}
}

func TestSubscriptionHandler_ErrorsMapToStableCodes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{domain.ErrSellerActivationRequired, http.StatusForbidden, CodeSellerActivationRequired},
		{domain.ErrMinimumTermNotMet, http.StatusConflict, CodeMinimumTermNotMet},
		{domain.ErrSubscriptionOverlap, http.StatusConflict, CodeSubscriptionOverlap},
		{domain.ErrLegacySubscriptionLive, http.StatusConflict, CodeSubscriptionOverlap},
		{domain.ErrTermsNotAccepted, http.StatusConflict, CodeTermsNotAccepted},
		{domain.ErrOfferChanged, http.StatusConflict, CodeOfferChanged},
		{domain.ErrAccountMarketNotSet, http.StatusUnprocessableEntity, CodeInvalidCommercialContext},
		{errors.Join(domain.ErrPriceVersionNotSellable), http.StatusUnprocessableEntity, CodePriceVersionInactive},
		{domain.ErrVersionConflict, http.StatusPreconditionFailed, CodeVersionConflict},
		{domain.ErrSubscriptionNotFound, http.StatusNotFound, CodeNotFound},
	}
	for _, tc := range cases {
		st := &subStub{view: subView(), err: tc.err}
		w := serve(t, newSubRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/subscriptions:start",
			body: startBody, headers: tenantHeaders(nil)})
		if w.Code != tc.status || problemCode(t, w) != tc.code {
			t.Errorf("%v: HTTP %d %s, want %d %s", tc.err, w.Code, w.Body.String(), tc.status, tc.code)
		}
	}
}

func TestSubscriptionHandler_ReplayAnswersWithTheOriginalSubscription(t *testing.T) {
	v := subView()
	st := &subStub{view: v, err: &domain.IdempotentReplayError{ResourceID: v.SubscriptionID}}
	w := serve(t, newSubRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/subscriptions:start",
		body: startBody, headers: tenantHeaders(nil)})
	var got domain.SubscriptionView
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "true" || got.SubscriptionID != v.SubscriptionID {
		t.Fatalf("replay: HTTP %d replayed=%q id=%s", w.Code, w.Header().Get("Idempotent-Replayed"), got.SubscriptionID)
	}
}

func TestSubscriptionHandler_ReadsAreScoped(t *testing.T) {
	id := domain.NewCommercialID(domain.PrefixSubscription)
	st := &subStub{view: subView()}
	az := &scopedAuthz{}
	w := serve(t, newSubRouter(st, az), req{method: http.MethodGet, path: "/v1/commercial/subscriptions/" + id, headers: tenantHeaders(nil)})
	if w.Code != http.StatusOK || az.checked[0] != tenantOrg+"|"+ActionSubscriptionRead || st.tenant != tenantOrg {
		t.Fatalf("customer read: HTTP %d checked=%v tenant=%s", w.Code, az.checked, st.tenant)
	}

	st = &subStub{view: subView()}
	az = &scopedAuthz{}
	w = serve(t, newSubRouter(st, az), req{method: http.MethodGet,
		path: "/v1/commercial/subscriptions/" + id + "/history?organization_id=" + customerOrg, headers: tenantHeaders(nil)})
	if w.Code != http.StatusOK || az.checked[0] != platformScopeID+"|"+ActionSubscriptionAssist || st.tenant != customerOrg {
		t.Fatalf("operator read: HTTP %d checked=%v tenant=%s", w.Code, az.checked, st.tenant)
	}

	w = serve(t, newSubRouter(&subStub{view: subView()}, &scopedAuthz{}), req{method: http.MethodGet,
		path: "/v1/commercial/subscriptions/" + id + "/effective-version?at=yesterday", headers: tenantHeaders(nil)})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a malformed as-of time: HTTP %d", w.Code)
	}
}
