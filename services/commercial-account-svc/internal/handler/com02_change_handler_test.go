package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/commercial-account-svc/internal/authz"
	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

type ruleStub struct {
	err     error
	created *domain.PlanTransitionRule
	calls   []string
}

func (s *ruleStub) CreateTransitionRule(_ context.Context, r *domain.PlanTransitionRule, _ domain.IdempotencyClaim) (*domain.PlanTransitionRule, error) {
	s.calls = append(s.calls, "Create")
	s.created = r
	return r, s.err
}
func (s *ruleStub) RetireTransitionRule(_ context.Context, id, _, _ string, _ time.Time) (*domain.PlanTransitionRule, error) {
	s.calls = append(s.calls, "Retire")
	return &domain.PlanTransitionRule{RuleID: id}, s.err
}
func (s *ruleStub) ListTransitionRules(context.Context, string) ([]domain.PlanTransitionRule, error) {
	return nil, s.err
}

func newChangeRouter(st *subStub, rules *ruleStub, az *scopedAuthz) http.Handler {
	logger, _ := zap.NewDevelopment()
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterSubscriptionV2Routes(r, NewSubscriptionHandler(st, rules, az, logger).WithClock(func() time.Time { return fixedNow }))
	return r
}

const quote = "abababababababababababababababababababababababababababababababab"

func subPath(action string) string {
	return "/v1/commercial/subscriptions/" + domain.NewCommercialID(domain.PrefixSubscription) + ":" + action
}

// Preview changes nothing and needs only read authority; it maps the named
// operation to the same request the command would make.
func TestChangeHandler_PreviewMapsTheOperation(t *testing.T) {
	st := &subStub{view: subView()}
	az := &scopedAuthz{}
	w := serve(t, newChangeRouter(st, &ruleStub{}, az), req{method: http.MethodPost, path: subPath("preview-change"),
		body:    `{"operation":"schedule-downgrade","product_code":"business","accepted_terms_sha256":"` + quote + `"}`,
		headers: tenantHeaders(nil)})
	if w.Code != http.StatusOK || len(st.calls) != 1 || st.calls[0] != "PreviewChange" {
		t.Fatalf("preview: HTTP %d calls=%v %s", w.Code, st.calls, w.Body.String())
	}
	if st.change.Kind != domain.ChangeKindPlan || !st.change.ForceNextRenewal || st.change.PlanProductCode != "business" {
		t.Fatalf("preview request: %+v", st.change)
	}
	if az.checked[0] != tenantOrg+"|"+ActionSubscriptionRead {
		t.Fatalf("preview authority: %v", az.checked)
	}
}

// A change is never made without the quote that was shown.
func TestChangeHandler_CommandsNeedTheQuoteHashAndTheirOwnFields(t *testing.T) {
	cases := map[string]struct {
		action, body string
	}{
		"no quote":                  {"change-plan", `{"product_code":"enterprise","accepted_terms_sha256":"` + quote + `"}`},
		"no terms for a new plan":   {"change-plan", `{"product_code":"enterprise","expected_quote_sha256":"` + quote + `"}`},
		"quantity with a product":   {"change-quantity", `{"product_code":"enterprise","quantities":{"seats":"9"},"expected_quote_sha256":"` + quote + `"}`},
		"quantity with none":        {"change-quantity", `{"expected_quote_sha256":"` + quote + `"}`},
		"remove with quantities":    {"remove-add-on", `{"product_code":"storage","quantities":{},"expected_quote_sha256":"` + quote + `"}`},
		"operation on a command":    {"change-quantity", `{"operation":"change-plan","quantities":{"seats":"9"},"expected_quote_sha256":"` + quote + `"}`},
		"change fields on a cancel": {"cancel", `{"product_code":"enterprise"}`},
	}
	for name, tc := range cases {
		st := &subStub{view: subView()}
		w := serve(t, newChangeRouter(st, &ruleStub{}, &scopedAuthz{}), req{method: http.MethodPost, path: subPath(tc.action),
			body: tc.body, headers: tenantHeaders(map[string]string{"If-Match": `"3"`})})
		if w.Code != http.StatusBadRequest || len(st.calls) != 0 {
			t.Errorf("%s: HTTP %d, store calls %v (%s)", name, w.Code, st.calls, w.Body.String())
		}
	}

	st := &subStub{view: subView()}
	w := serve(t, newChangeRouter(st, &ruleStub{}, &scopedAuthz{}), req{method: http.MethodPost, path: subPath("change-quantity"),
		body: `{"quantities":{"seats":"9"},"expected_quote_sha256":"` + quote + `"}`, headers: tenantHeaders(map[string]string{"If-Match": `"3"`})})
	if w.Code != http.StatusOK || st.quote != quote || st.change.Kind != domain.ChangeKindQuantity || st.cmd.ExpectedVersion != 3 {
		t.Fatalf("change-quantity: HTTP %d quote=%s change=%+v", w.Code, st.quote, st.change)
	}
}

func TestChangeHandler_ErrorsMapToStableCodes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{domain.ErrQuoteChanged, http.StatusConflict, CodeQuoteChanged},
		{domain.ErrTransitionNotAllowed, http.StatusConflict, CodeTransitionNotAllowed},
		{domain.ErrTimingNotAllowed, http.StatusConflict, CodeTimingNotAllowed},
		{domain.ErrChangeAlreadyScheduled, http.StatusConflict, CodeChangeAlreadyScheduled},
		{domain.ErrNoChange, http.StatusUnprocessableEntity, CodeNoChange},
	}
	for _, tc := range cases {
		st := &subStub{view: subView(), err: tc.err}
		w := serve(t, newChangeRouter(st, &ruleStub{}, &scopedAuthz{}), req{method: http.MethodPost, path: subPath("change-quantity"),
			body: `{"quantities":{"seats":"9"},"expected_quote_sha256":"` + quote + `"}`, headers: tenantHeaders(map[string]string{"If-Match": `"3"`})})
		if w.Code != tc.status || problemCode(t, w) != tc.code {
			t.Errorf("%v: HTTP %d %s", tc.err, w.Code, w.Body.String())
		}
	}
}

// Transition rules are product policy: the price-book publish grant, a
// valid timing/proration pair, and a reason to retire.
func TestChangeHandler_TransitionRules(t *testing.T) {
	body := `{"from_product_code":"business","to_product_code":"enterprise","timing":"IMMEDIATE","proration_method":"DAILY_HALF_EVEN"}`
	rules := &ruleStub{}
	az := &scopedAuthz{}
	w := serve(t, newChangeRouter(&subStub{}, rules, az), req{method: http.MethodPost, path: "/v1/commercial/plan-transition-rules",
		body: body, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusCreated || rules.created == nil || az.checked[0] != platformScopeID+"|"+ActionPriceBookPublish {
		t.Fatalf("create rule: HTTP %d checked=%v %s", w.Code, az.checked, w.Body.String())
	}

	denied := &scopedAuthz{deny: map[string]error{platformScopeID + "|" + ActionPriceBookPublish: authzpkg.ErrAuthorizationDenied}}
	rules = &ruleStub{}
	w = serve(t, newChangeRouter(&subStub{}, rules, denied), req{method: http.MethodPost, path: "/v1/commercial/plan-transition-rules",
		body: body, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusForbidden || len(rules.calls) != 0 {
		t.Fatalf("a principal without the publish grant set product policy: HTTP %d", w.Code)
	}

	rules = &ruleStub{}
	w = serve(t, newChangeRouter(&subStub{}, rules, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/plan-transition-rules",
		body: strings.Replace(body, "IMMEDIATE", "NEXT_RENEWAL", 1), headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusBadRequest || len(rules.calls) != 0 {
		t.Fatalf("a prorated renewal-time rule was accepted: HTTP %d", w.Code)
	}

	id := domain.NewCommercialID(domain.PrefixTransitionRule)
	w = serve(t, newChangeRouter(&subStub{}, rules, &scopedAuthz{}), req{method: http.MethodPost,
		path: "/v1/commercial/plan-transition-rules/" + id + ":retire", body: `{"reason":" "}`})
	if w.Code != http.StatusBadRequest || len(rules.calls) != 0 {
		t.Fatalf("a rule was retired without a reason: HTTP %d", w.Code)
	}
}
