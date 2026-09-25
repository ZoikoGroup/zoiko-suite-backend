package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/commercial-account-svc/internal/authz"
	"zoiko.io/commercial-account-svc/internal/domain"
)

// ── Test doubles ─────────────────────────────────────────────────────────────

// actionAuthz grants everything except the actions listed in deny, and
// records every action it was asked about.
type actionAuthz struct {
	mu      sync.Mutex
	deny    map[string]error
	checked []string
}

func (a *actionAuthz) CheckAllowed(_ context.Context, _, _, action string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checked = append(a.checked, action)
	return a.deny[action]
}

// pbStub records what the handler passed and returns canned results.
type pbStub struct {
	err        error
	version    *domain.PriceVersion
	product    *domain.Product
	lastClaim  domain.IdempotencyClaim
	lastIfMat  int
	lastSeller *bool
	lastReason string
	calls      []string
}

func (s *pbStub) rec(name string, claim domain.IdempotencyClaim, ifMatch int) {
	s.calls = append(s.calls, name)
	s.lastClaim, s.lastIfMat = claim, ifMatch
}

func (s *pbStub) UpsertCurrency(_ context.Context, c *domain.CommercialCurrency) (*domain.CommercialCurrency, error) {
	s.calls = append(s.calls, "UpsertCurrency")
	return c, s.err
}
func (s *pbStub) ListCurrencies(context.Context) ([]domain.CommercialCurrency, error) {
	return nil, s.err
}
func (s *pbStub) CreateProduct(_ context.Context, p *domain.Product, c domain.IdempotencyClaim) (*domain.Product, error) {
	s.rec("CreateProduct", c, 0)
	if s.err != nil {
		return nil, s.err
	}
	return p, nil
}
func (s *pbStub) GetProduct(_ context.Context, _ string, seller bool) (*domain.Product, error) {
	s.lastSeller = &seller
	return s.product, nil
}
func (s *pbStub) ListPublishedProducts(context.Context, time.Time) ([]domain.ProductSummary, error) {
	return nil, s.err
}
func (s *pbStub) CreateDraftVersion(_ context.Context, v *domain.PriceVersion, _ bool, c domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	s.rec("CreateDraftVersion", c, 0)
	if s.err != nil {
		return nil, s.err
	}
	v.RowVersion = 1
	return v, nil
}
func (s *pbStub) PutPriceComponent(_ context.Context, _ string, ifMatch int, _ *domain.PriceComponent, c domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	s.rec("PutPriceComponent", c, ifMatch)
	return s.version, s.err
}
func (s *pbStub) RemovePriceComponent(_ context.Context, _ string, ifMatch int, _, _ string, c domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	s.rec("RemovePriceComponent", c, ifMatch)
	return s.version, s.err
}
func (s *pbStub) SetCommercialTerms(_ context.Context, _ string, ifMatch int, _ *domain.CommercialTerms, c domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	s.rec("SetCommercialTerms", c, ifMatch)
	return s.version, s.err
}
func (s *pbStub) SetCapabilities(_ context.Context, _ string, ifMatch int, _ []domain.PlanCapability, _ string, c domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	s.rec("SetCapabilities", c, ifMatch)
	return s.version, s.err
}
func (s *pbStub) SubmitForApproval(_ context.Context, _ string, ifMatch int, _ string, _ time.Time, c domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	s.rec("SubmitForApproval", c, ifMatch)
	return s.version, s.err
}
func (s *pbStub) ApprovePriceVersion(_ context.Context, _ string, ifMatch int, _ string, _ time.Time, c domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	s.rec("ApprovePriceVersion", c, ifMatch)
	return s.version, s.err
}
func (s *pbStub) RejectPriceVersion(_ context.Context, _ string, ifMatch int, _, reason string, _ time.Time, c domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	s.rec("RejectPriceVersion", c, ifMatch)
	s.lastReason = reason
	return s.version, s.err
}
func (s *pbStub) PublishPriceVersion(_ context.Context, _ string, ifMatch int, _ string, _ time.Time, c domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	s.rec("PublishPriceVersion", c, ifMatch)
	return s.version, s.err
}
func (s *pbStub) RetirePriceVersion(_ context.Context, _ string, ifMatch int, _, reason string, _ time.Time, c domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	s.rec("RetirePriceVersion", c, ifMatch)
	s.lastReason = reason
	return s.version, s.err
}
func (s *pbStub) GetPriceVersion(_ context.Context, _ string, seller bool) (*domain.PriceVersion, error) {
	s.lastSeller = &seller
	if s.version == nil {
		return nil, domain.ErrPriceVersionNotFound
	}
	return s.version, nil
}
func (s *pbStub) ListPriceHistory(_ context.Context, _ string, seller bool) ([]domain.PriceVersion, error) {
	s.lastSeller = &seller
	return nil, s.err
}
func (s *pbStub) ResolveSellableOffers(context.Context, domain.SellableOfferFilter, time.Time) ([]domain.PriceVersion, error) {
	s.calls = append(s.calls, "ResolveSellableOffers")
	return nil, s.err
}

func newPBRouter(st *pbStub, az *actionAuthz) http.Handler {
	logger, _ := zap.NewDevelopment()
	r := chi.NewRouter()
	RegisterPriceBookRoutes(r, NewPriceBookHandler(st, az, logger))
	return r
}

type req struct {
	method, path, body string
	headers            map[string]string
}

func serve(t *testing.T, h http.Handler, rq req) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(rq.method, rq.path, strings.NewReader(rq.body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Principal-Id", "alice")
	for k, v := range rq.headers {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func problemCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("error response Content-Type = %q, want application/problem+json (RFC 9457); body=%s", ct, w.Body.String())
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Status != w.Code || p.Type == "" {
		t.Fatalf("problem body inconsistent with response: %+v (HTTP %d)", p, w.Code)
	}
	return p.Code
}

func sampleVersion() *domain.PriceVersion {
	return &domain.PriceVersion{PriceVersionID: domain.NewCommercialID(domain.PrefixPriceVersion), RowVersion: 4,
		Status: domain.PriceVersionReview, Components: []domain.PriceComponent{}, Capabilities: []domain.PlanCapability{}}
}

func cmdHeaders(extra map[string]string) map[string]string {
	h := map[string]string{"Idempotency-Key": "k-1", "If-Match": `"4"`}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// ── Tests ────────────────────────────────────────────────────────────────────

func TestPriceBookHandler_CustomMethodsDispatchWithTheirOwnPermission(t *testing.T) {
	cases := map[string]struct{ store, action string }{
		"submit":  {"SubmitForApproval", ActionPriceBookPropose},
		"approve": {"ApprovePriceVersion", ActionPriceBookApprove},
		"reject":  {"RejectPriceVersion", ActionPriceBookApprove},
		"publish": {"PublishPriceVersion", ActionPriceBookPublish},
		"retire":  {"RetirePriceVersion", ActionPriceBookRetire},
	}
	for action, want := range cases {
		st := &pbStub{version: sampleVersion()}
		az := &actionAuthz{}
		w := serve(t, newPBRouter(st, az), req{method: http.MethodPost,
			path: "/v1/commercial/price-versions/" + st.version.PriceVersionID + ":" + action,
			body: `{"reason":"because"}`, headers: cmdHeaders(nil)})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d %s", action, w.Code, w.Body.String())
		}
		if len(st.calls) != 1 || st.calls[0] != want.store || st.lastIfMat != 4 {
			t.Fatalf("%s: dispatched to %v with If-Match %d", action, st.calls, st.lastIfMat)
		}
		if len(az.checked) != 1 || az.checked[0] != want.action {
			t.Fatalf("%s: checked %v, want exactly %s", action, az.checked, want.action)
		}
		if w.Header().Get("ETag") != `"4"` {
			t.Fatalf("%s: ETag = %q", action, w.Header().Get("ETag"))
		}
	}
}

// Negative path #01 at the API boundary.
func TestPriceBookHandler_BareUUIDIsRefusedAsCrossPlane(t *testing.T) {
	st := &pbStub{version: sampleVersion()}
	for _, rq := range []req{
		{method: http.MethodGet, path: "/v1/commercial/price-versions/3f2504e0-4f89-11d3-9a0c-0305e82c3301"},
		{method: http.MethodPost, path: "/v1/commercial/price-versions/3f2504e0-4f89-11d3-9a0c-0305e82c3301:approve", headers: cmdHeaders(nil)},
		{method: http.MethodGet, path: "/v1/commercial/products/3f2504e0-4f89-11d3-9a0c-0305e82c3301/price-history"},
	} {
		w := serve(t, newPBRouter(st, &actionAuthz{}), rq)
		if w.Code != http.StatusUnprocessableEntity || problemCode(t, w) != CodeCrossPlaneAccessDenied {
			t.Fatalf("%s %s: HTTP %d %s", rq.method, rq.path, w.Code, w.Body.String())
		}
	}
	if len(st.calls) != 0 {
		t.Fatalf("a cross-plane identifier reached the store: %v", st.calls)
	}
}

func TestPriceBookHandler_CommandsRequireIdempotencyKeyAndIfMatch(t *testing.T) {
	st := &pbStub{version: sampleVersion()}
	path := "/v1/commercial/price-versions/" + st.version.PriceVersionID + ":submit"

	w := serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodPost, path: path, headers: map[string]string{"If-Match": `"4"`}})
	if w.Code != http.StatusBadRequest || problemCode(t, w) != CodeIdempotencyKeyRequired {
		t.Fatalf("missing Idempotency-Key: HTTP %d %s", w.Code, w.Body.String())
	}
	w = serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodPost, path: path, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusPreconditionRequired || problemCode(t, w) != CodePreconditionRequired {
		t.Fatalf("missing If-Match: HTTP %d %s", w.Code, w.Body.String())
	}
	w = serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/products",
		body: `{"product_code":"teams","product_kind":"PLAN"}`})
	if w.Code != http.StatusBadRequest || problemCode(t, w) != CodeIdempotencyKeyRequired {
		t.Fatalf("CreateProduct without Idempotency-Key: HTTP %d", w.Code)
	}
	if len(st.calls) != 0 {
		t.Fatalf("an unprotected command reached the store: %v", st.calls)
	}
}

// A client cannot smuggle state the server does not accept from it.
func TestPriceBookHandler_UnknownFieldsAreRefused(t *testing.T) {
	st := &pbStub{}
	w := serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/products",
		body: `{"product_code":"teams","product_kind":"PLAN","status":"PUBLISHED"}`, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusBadRequest || problemCode(t, w) != CodeInvalidCommercialContext {
		t.Fatalf("unknown field accepted: HTTP %d %s", w.Code, w.Body.String())
	}
	if len(st.calls) != 0 {
		t.Fatal("request with an unknown field reached the store")
	}
}

func TestPriceBookHandler_StoreErrorsMapToStableCodes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{domain.ErrSoDViolation, http.StatusForbidden, CodeSoDViolation},
		{domain.ErrVersionConflict, http.StatusPreconditionFailed, CodeVersionConflict},
		{domain.ErrPriceVersionImmutable, http.StatusConflict, CodePriceVersionImmutable},
		{domain.ErrMeterNotRegistered, http.StatusUnprocessableEntity, CodeMeterNotRegistered},
		{domain.ErrContentHashMismatch, http.StatusConflict, CodePriceBasisChanged},
		{domain.ErrIdempotencyKeyReused, http.StatusUnprocessableEntity, CodeIdempotencyKeyReused},
		{&domain.PublicationBlockedError{Reasons: []string{"a", "b"}}, http.StatusUnprocessableEntity, CodePublicationBlocked},
		{errors.New("boom"), http.StatusInternalServerError, CodeInternal},
	}
	for _, tc := range cases {
		st := &pbStub{version: sampleVersion(), err: tc.err}
		w := serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodPost,
			path: "/v1/commercial/price-versions/" + st.version.PriceVersionID + ":approve", headers: cmdHeaders(nil)})
		if w.Code != tc.status || problemCode(t, w) != tc.code {
			t.Errorf("%v: HTTP %d %s, want %d %s", tc.err, w.Code, w.Body.String(), tc.status, tc.code)
		}
	}
	st := &pbStub{version: sampleVersion(), err: &domain.PublicationBlockedError{Reasons: []string{"terms", "currency"}}}
	w := serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodPost,
		path: "/v1/commercial/price-versions/" + st.version.PriceVersionID + ":submit", headers: cmdHeaders(nil)})
	var p Problem
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if len(p.Reasons) != 2 {
		t.Fatalf("publication blockers must all be listed: %+v", p)
	}
}

func TestPriceBookHandler_AuthorizationFailsClosed(t *testing.T) {
	st := &pbStub{version: sampleVersion()}
	path := "/v1/commercial/price-versions/" + st.version.PriceVersionID + ":approve"

	denied := &actionAuthz{deny: map[string]error{ActionPriceBookApprove: authzpkg.ErrAuthorizationDenied}}
	w := serve(t, newPBRouter(st, denied), req{method: http.MethodPost, path: path, headers: cmdHeaders(nil)})
	if w.Code != http.StatusForbidden || problemCode(t, w) != CodeAuthorizationDenied {
		t.Fatalf("denied approver: HTTP %d", w.Code)
	}
	down := &actionAuthz{deny: map[string]error{ActionPriceBookApprove: errors.New("connection refused")}}
	w = serve(t, newPBRouter(st, down), req{method: http.MethodPost, path: path, headers: cmdHeaders(nil)})
	if w.Code != http.StatusServiceUnavailable || problemCode(t, w) != CodeAuthorizationUnavailable {
		t.Fatalf("authorization outage: HTTP %d", w.Code)
	}
	if len(st.calls) != 0 {
		t.Fatalf("an unauthorized command reached the store: %v", st.calls)
	}
}

// A replayed command answers with the original status and the existing
// resource, and says so.
func TestPriceBookHandler_ReplayReturnsTheOriginalResult(t *testing.T) {
	existing := &domain.Product{ProductID: domain.NewCommercialID(domain.PrefixProduct), ProductCode: "teams"}
	st := &pbStub{err: &domain.IdempotentReplayError{ResourceID: existing.ProductID}, product: existing}
	w := serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/products",
		body: `{"product_code":"teams","product_kind":"PLAN"}`, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: HTTP %d, Idempotent-Replayed=%q", w.Code, w.Header().Get("Idempotent-Replayed"))
	}
	var got domain.Product
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ProductID != existing.ProductID {
		t.Fatalf("replay returned %s, want the original %s", got.ProductID, existing.ProductID)
	}
}

// The request hash binds a key to one exact request.
func TestPriceBookHandler_RequestHashCoversTheBody(t *testing.T) {
	hashOf := func(body string) string {
		st := &pbStub{}
		serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/products",
			body: body, headers: map[string]string{"Idempotency-Key": "same-key"}})
		return st.lastClaim.RequestSHA256
	}
	a := hashOf(`{"product_code":"teams","product_kind":"PLAN"}`)
	if a == "" || a != hashOf(`{"product_code":"teams","product_kind":"PLAN"}`) {
		t.Fatal("the same request must hash the same")
	}
	if a == hashOf(`{"product_code":"teams","product_kind":"ADD_ON"}`) {
		t.Fatal("a different request under the same key hashed the same; it would be replayed instead of refused")
	}
}

// Drafts are visible only with the read grant, and an authorization outage
// narrows visibility instead of widening it.
func TestPriceBookHandler_SellerViewFailsToPublic(t *testing.T) {
	cases := map[string]struct {
		deny error
		want bool
	}{
		"granted": {nil, true},
		"denied":  {authzpkg.ErrAuthorizationDenied, false},
		"outage":  {errors.New("timeout"), false},
	}
	for name, tc := range cases {
		st := &pbStub{version: sampleVersion()}
		az := &actionAuthz{deny: map[string]error{ActionPriceBookRead: tc.deny}}
		w := serve(t, newPBRouter(st, az), req{method: http.MethodGet, path: "/v1/commercial/price-versions/" + st.version.PriceVersionID})
		if w.Code != http.StatusOK || st.lastSeller == nil || *st.lastSeller != tc.want {
			t.Fatalf("%s: HTTP %d, seller view = %v, want %v", name, w.Code, st.lastSeller, tc.want)
		}
	}
}

func TestPriceBookHandler_RejectAndRetireRequireAReason(t *testing.T) {
	for _, action := range []string{"reject", "retire"} {
		st := &pbStub{version: sampleVersion()}
		w := serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodPost,
			path: "/v1/commercial/price-versions/" + st.version.PriceVersionID + ":" + action, body: `{"reason":"  "}`, headers: cmdHeaders(nil)})
		if w.Code != http.StatusBadRequest || problemCode(t, w) != CodeInvalidCommercialContext || len(st.calls) != 0 {
			t.Fatalf("%s without a reason: HTTP %d, store calls %v", action, w.Code, st.calls)
		}
	}
	st := &pbStub{version: sampleVersion()}
	w := serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodPost,
		path: "/v1/commercial/price-versions/" + st.version.PriceVersionID + ":delete", headers: cmdHeaders(nil)})
	if w.Code != http.StatusNotFound || problemCode(t, w) != CodeNotFound {
		t.Fatalf("unknown custom method: HTTP %d", w.Code)
	}
}

func TestPriceBookHandler_SellableOffersValidateFilters(t *testing.T) {
	st := &pbStub{}
	w := serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodGet, path: "/v1/commercial/sellable-offers?market=gb"})
	if w.Code != http.StatusBadRequest || problemCode(t, w) != CodeInvalidCommercialContext {
		t.Fatalf("lowercase market accepted: HTTP %d", w.Code)
	}
	w = serve(t, newPBRouter(st, &actionAuthz{}), req{method: http.MethodGet, path: "/v1/commercial/sellable-offers?product_code=business&currency=USD&market=GB"})
	if w.Code != http.StatusOK || len(st.calls) != 1 {
		t.Fatalf("valid filters: HTTP %d calls=%v", w.Code, st.calls)
	}
}
