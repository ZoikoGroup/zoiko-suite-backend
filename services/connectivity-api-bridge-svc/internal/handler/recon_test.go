package handler

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/recon"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
	"zoiko.io/connectivity-api-bridge-svc/internal/signals"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

type rHarness struct {
	*connHarness
	svc  *recon.Service
	repo *recon.MemRepo
	root chi.Router
	now  time.Time
}

type fakeSignals struct{}

func (fakeSignals) Snapshot(ctx context.Context, _ time.Time) (*signals.Snapshot, error) {
	return &signals.Snapshot{WindowSeconds: 86400, Inbound: signals.Inbound{Total: 7}}, nil
}
func (fakeSignals) Platform(ctx context.Context, _ time.Time) (*signals.Snapshot, error) {
	return &signals.Snapshot{Connections: signals.Connections{ByStatus: map[string]int{"ACTIVE": 3}}, Inbound: signals.Inbound{ByState: map[string]int{"HANDED_OFF": 9}}}, nil
}

func newRHarness(t *testing.T) *rHarness {
	t.Helper()
	ch := newConnHarness(t)
	h := &rHarness{connHarness: ch, repo: recon.NewMemRepo(), now: registrytest.T0.Add(72 * time.Hour)}
	h.svc = &recon.Service{Repo: h.repo, Conns: ch.svc, Connectors: ch.repo, Clock: func() time.Time { return h.now }}
	srv := httptest.NewServer(ch.authz.handler())
	t.Cleanup(srv.Close)
	api := chi.NewRouter()
	api.Use(middleware.TenantContext)
	RegisterReconRoutes(api, NewRecon(h.svc, h.repo, fakeSignals{}, authz.NewClient(srv.URL), zap.NewNop()))
	RegisterConnectionRoutes(api, NewConnection(ch.svc, ch.conns, authz.NewClient(srv.URL), zap.NewNop()))
	h.root = api
	return h
}

func (h *rHarness) call(method, path, tenant, principal string, body any) *httptest.ResponseRecorder {
	return h.do(h.root, method, path, tenant, principal, body)
}

func (h *rHarness) body(c xicconn.Connection, basis []string, ext map[string]any) map[string]any {
	m := map[string]any{"connection_id": c.ConnectionID, "period_from": h.now.Add(-24 * time.Hour), "period_to": h.now.Add(-time.Hour), "basis": basis}
	if ext != nil {
		m["external"] = ext
	}
	return m
}

func TestReconHTTP_RequestIsAsynchronousIdempotentAndIndependent(t *testing.T) {
	h := newRHarness(t)
	c := h.active(t, hTenantA, "acc-r")
	ext := map[string]any{"source": "PROVIDER_STATEMENT", "counts": map[string]int{"received": 3}}
	w := h.call("POST", "/v1/reconciliation-runs", hTenantA, "ops", h.body(c, []string{"COUNT"}, ext))
	var run recon.Run
	decodeInto(t, w, &run)
	if w.Code != 202 || run.State != recon.RunOpen || run.RunID == "" || run.ExternalSuppliedBy != "ops" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// the same request again is the same run
	w2 := h.call("POST", "/v1/reconciliation-runs", hTenantA, "ops", h.body(c, []string{"COUNT"}, ext))
	var again recon.Run
	decodeInto(t, w2, &again)
	if w2.Code != 200 || again.RunID != run.RunID {
		t.Fatalf("%d %s", w2.Code, w2.Body.String())
	}
	// NP-44: no provider declaration, or one that comes from the platform itself
	for name, b := range map[string]map[string]any{
		"none":     h.body(c, []string{"COUNT"}, nil),
		"internal": h.body(c, []string{"COUNT"}, map[string]any{"source": "INTERNAL", "counts": map[string]int{"received": 3}}),
	} {
		p := isProblem(t, h.call("POST", "/v1/reconciliation-runs", hTenantA, "ops", b), 422)
		if !strings.Contains(p["detail"].(string), "independent") {
			t.Errorf("%s: %v", name, p)
		}
	}
	isProblem(t, h.call("POST", "/v1/reconciliation-runs", hTenantA, "ops", h.body(c, []string{"VIBES"}, nil)), 422)
	// NP-52: another tenant's connection: 404 and a boundary signal, nothing stored
	isProblem(t, h.call("POST", "/v1/reconciliation-runs", hTenantB, "ops", h.body(c, []string{"STATE"}, nil)), 404)
	if len(h.repo.Signals) != 1 {
		t.Fatalf("%d signals", len(h.repo.Signals))
	}
	// reads are tenant scoped
	if g := h.call("GET", "/v1/reconciliation-runs/"+run.RunID, hTenantA, "reader", nil); g.Code != 200 {
		t.Fatalf("%d", g.Code)
	}
	isProblem(t, h.call("GET", "/v1/reconciliation-runs/"+run.RunID, hTenantB, "reader", nil), 404)
	isProblem(t, h.call("GET", "/v1/reconciliation-runs/"+run.RunID+"/events", hTenantB, "reader", nil), 404)
	if l := h.call("GET", "/v1/reconciliation-runs?state=open", hTenantA, "reader", nil); l.Code != 200 || !strings.Contains(l.Body.String(), run.RunID) {
		t.Fatalf("%d %s", l.Code, l.Body.String())
	}
	if l := h.call("GET", "/v1/reconciliation-runs", hTenantB, "reader", nil); strings.Contains(l.Body.String(), run.RunID) {
		t.Fatal("listed another tenant's run")
	}
	// the authorization is its own: reading is not requesting
	h.authz.deny("reader", ActionReconRequest)
	if x := h.call("POST", "/v1/reconciliation-runs", hTenantA, "reader", h.body(c, []string{"STATE"}, nil)); x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}
	if x := h.call("GET", "/v1/reconciliation-runs", hTenantA, "", nil); x.Code != 401 && x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}
}

func TestReconHTTP_DomainTotalsVariancesAndTheCasePath(t *testing.T) {
	h := newRHarness(t)
	c := h.active(t, hTenantA, "acc-r")
	h.repo.Inbound = map[string]int{"received": 2}
	w := h.call("POST", "/v1/reconciliation-runs", hTenantA, "ops", h.body(c, []string{"COUNT", "AMOUNT"}, map[string]any{"source": "PROVIDER_API", "counts": map[string]int{"received": 40}, "total_amount": "100.00", "currency": "GBP"}))
	var run recon.Run
	decodeInto(t, w, &run)
	if !run.AwaitingDomain {
		t.Fatalf("%s", w.Body.String())
	}
	// the person who supplied the provider's side cannot supply the domain's
	isProblem(t, h.call("POST", "/v1/reconciliation-runs/"+run.RunID+":domain-totals", hTenantA, "ops", map[string]any{"total_amount": "100.00", "currency": "GBP"}), 403)
	isProblem(t, h.call("POST", "/v1/reconciliation-runs/"+run.RunID+":domain-totals", hTenantA, "domain", map[string]any{"total_amount": "x", "currency": "GBP"}), 422)
	if x := h.call("POST", "/v1/reconciliation-runs/"+run.RunID+":domain-totals", hTenantA, "domain", map[string]any{"total_amount": "90.00", "currency": "GBP", "reference": "ledger"}); x.Code != 200 {
		t.Fatalf("%d %s", x.Code, x.Body.String())
	}
	isProblem(t, h.call("POST", "/v1/reconciliation-runs/"+run.RunID+":domain-totals", hTenantA, "domain2", map[string]any{"total_amount": "1.00", "currency": "GBP"}), 409)
	isProblem(t, h.call("POST", "/v1/reconciliation-runs/"+run.RunID+":explode", hTenantA, "domain", map[string]any{}), 404)
	if _, err := h.svc.Evaluate(middleware.WithTenant(context.Background(), hTenantA), hTenantA, run.RunID); err != nil {
		t.Fatal(err)
	}
	vl := h.call("GET", "/v1/reconciliation-runs/"+run.RunID+"/variances", hTenantA, "reader", nil)
	if vl.Code != 200 || !strings.Contains(vl.Body.String(), "COUNT_MISMATCH") || !strings.Contains(vl.Body.String(), "AMOUNT_MISMATCH") {
		t.Fatalf("%d %s", vl.Code, vl.Body.String())
	}
	vars, _ := h.repo.ListVariances(middleware.WithTenant(context.Background(), hTenantA), recon.VarianceFilter{})
	if len(vars) != 2 {
		t.Fatalf("%d", len(vars))
	}
	v := vars[0]
	// the run cannot close, and another tenant cannot see or touch the variance
	isProblem(t, h.call("POST", "/v1/reconciliation-runs/"+run.RunID+":close", hTenantA, "closer", map[string]any{"note": "x"}), 409)
	isProblem(t, h.call("GET", "/v1/reconciliation-variances/"+v.VarianceID, hTenantB, "reader", nil), 404)
	isProblem(t, h.call("POST", "/v1/reconciliation-variances/"+v.VarianceID+":resolve", hTenantB, "r", map[string]any{"resolution": "DOMAIN_CORRECTED", "note": "note text"}), 404)
	// the case path
	if x := h.call("POST", "/v1/reconciliation-variances/"+v.VarianceID+":case", hTenantA, "r", map[string]any{"case_ref": "CASE-5", "expected_version": v.Version}); x.Code != 200 {
		t.Fatalf("%d %s", x.Code, x.Body.String())
	}
	isProblem(t, h.call("POST", "/v1/reconciliation-variances/"+v.VarianceID+":case", hTenantA, "r", map[string]any{"case_ref": "CASE-6", "expected_version": v.Version}), 409)
	isProblem(t, h.call("POST", "/v1/reconciliation-variances/"+v.VarianceID+":resolve", hTenantA, "r", map[string]any{"resolution": "NOPE", "note": "note text"}), 422)
	// a HIGH variance cannot be accepted by whoever supplied the provider's side
	var high string
	for _, x := range vars {
		if x.Severity == recon.SevHigh {
			high = x.VarianceID
		}
	}
	if high == "" {
		t.Fatalf("%+v", vars)
	}
	isProblem(t, h.call("POST", "/v1/reconciliation-variances/"+high+":accept", hTenantA, "ops", map[string]any{"case_ref": "C-1", "note": "waved through"}), 403)
	if x := h.call("POST", "/v1/reconciliation-variances/"+high+":accept", hTenantA, "reviewer", map[string]any{"case_ref": "C-1", "note": "provider test batch"}); x.Code != 200 || !strings.Contains(x.Body.String(), "ACCEPTED") {
		t.Fatalf("%d %s", x.Code, x.Body.String())
	}
	isProblem(t, h.call("POST", "/v1/reconciliation-variances/"+high+":resolve", hTenantA, "reviewer", map[string]any{"resolution": "DOMAIN_CORRECTED", "note": "again"}), 409)
	other := v.VarianceID
	if other == high {
		other = vars[1].VarianceID
	}
	if x := h.call("POST", "/v1/reconciliation-variances/"+other+":resolve", hTenantA, "reviewer", map[string]any{"resolution": "DOMAIN_CORRECTED", "note": "ledger corrected"}); x.Code != 200 {
		t.Fatalf("%d %s", x.Code, x.Body.String())
	}
	if g := h.call("GET", "/v1/reconciliation-runs/"+run.RunID, hTenantA, "reader", nil); !strings.Contains(g.Body.String(), `"state":"CLOSED"`) {
		t.Fatalf("%s", g.Body.String())
	}
	if l := h.call("GET", "/v1/reconciliation-variances?state=accepted&severity=high", hTenantA, "reader", nil); l.Code != 200 || !strings.Contains(l.Body.String(), high) {
		t.Fatalf("%d %s", l.Code, l.Body.String())
	}
	// each command has its own authorization
	h.authz.deny("limited", ActionVarianceAccept)
	isProblem(t, h.call("POST", "/v1/reconciliation-variances/"+other+":accept", hTenantA, "limited", map[string]any{"case_ref": "C", "note": "note text"}), 403)
}

func TestSignalsHTTP_TenantSnapshotAndTheMetricsEndpoint(t *testing.T) {
	h := newRHarness(t)
	if x := h.call("GET", "/v1/integration-signals", hTenantA, "reader", nil); x.Code != 200 || !strings.Contains(x.Body.String(), `"total":7`) {
		t.Fatalf("%d %s", x.Code, x.Body.String())
	}
	isProblem(t, h.call("GET", "/v1/integration-signals?window=1s", hTenantA, "reader", nil), 422)
	isProblem(t, h.call("GET", "/v1/integration-signals?window=9999h", hTenantA, "reader", nil), 422)
	h.authz.deny("nobody", ActionSignalsRead)
	if x := h.call("GET", "/v1/integration-signals", hTenantA, "nobody", nil); x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}
	// the platform metrics need the scrape token, carry no tenant data, and sanitise what they label
	m := signals.NewMetrics()
	m.Hook("outbound")("submit_refused", "EGRESS_DENIED")
	m.Hook("x\"y")("a b", "c\nd")
	mh := NewMetrics(m, fakeSignals{}, "tok", time.Hour, zap.NewNop())
	get := func(tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/metrics", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		mh.ServeHTTP(w, req)
		return w
	}
	if get("").Code != 401 || get("bad").Code != 401 {
		t.Fatal("the endpoint answered without the token")
	}
	w := get("tok")
	b := w.Body.String()
	if w.Code != 200 || !strings.Contains(b, `xic_events_total{service="outbound",kind="submit_refused",reason="EGRESS_DENIED"} 1`) || !strings.Contains(b, `xic_connections{status="ACTIVE"} 3`) ||
		!strings.Contains(b, `service="x_y"`) || strings.Contains(b, "c\nd") || !strings.Contains(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("%d\n%s", w.Code, b)
	}
}
