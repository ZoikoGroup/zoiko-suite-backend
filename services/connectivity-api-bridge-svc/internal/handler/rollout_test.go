package handler

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
	"zoiko.io/connectivity-api-bridge-svc/internal/rollout"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// prodView reports the harness's sandbox connection as a production connection, with a certification state the test sets.
type prodView struct {
	svc  *xicconn.Service
	cert *string
}

func (p prodView) Connection(ctx context.Context, id string) (*xicconn.Connection, error) {
	c, err := p.svc.Connection(ctx, id)
	if err != nil {
		return nil, err
	}
	c.Environment = xicconn.EnvProduction
	return c, nil
}

func (p prodView) CertificationStatusOf(context.Context, *xicconn.Connection) (*xicconn.CertStatus, error) {
	return &xicconn.CertStatus{State: *p.cert}, nil
}

type roHarness struct {
	*connHarness
	svc  *rollout.Service
	repo *rollout.MemRepo
	root chi.Router
	now  time.Time
	cert string
}

func newROHarness(t *testing.T) *roHarness {
	t.Helper()
	ch := newConnHarness(t)
	h := &roHarness{connHarness: ch, repo: rollout.NewMemRepo(), now: registrytest.T0.Add(72 * time.Hour), cert: xicconn.CertValid}
	h.svc = &rollout.Service{Repo: h.repo, Conns: prodView{svc: ch.svc, cert: &h.cert}, Clock: func() time.Time { return h.now }}
	srv := httptest.NewServer(ch.authz.handler())
	t.Cleanup(srv.Close)
	api := chi.NewRouter()
	api.Use(middleware.TenantContext)
	RegisterRolloutRoutes(api, NewRollout(h.svc, h.repo, authz.NewClient(srv.URL), zap.NewNop()))
	h.root = api
	return h
}

func (h *roHarness) call(method, path, tenant, principal string, body any) *httptest.ResponseRecorder {
	return h.do(h.root, method, path, tenant, principal, body)
}

var allAttestations = []string{"SLO_ALERTS", "RUNBOOKS", "ONCALL", "NO_P0_P1_DEFECTS"}

func TestRolloutHTTP_StagesNeedAnApproverAndRollbackIsImmediate(t *testing.T) {
	h := newROHarness(t)
	c := h.active(t, hTenantA, "acc-ro")
	create := map[string]any{"connection_id": c.ConnectionID, "pilot_operations": []string{"payment-submit"}, "pilot_daily_cap": 5, "min_soak_hours": 1, "min_success": 1, "max_failure_pct": 10}
	var ro rollout.Rollout
	w := h.call("POST", "/v1/rollouts", hTenantA, "release-manager", create)
	decodeInto(t, w, &ro)
	if w.Code != 201 || ro.Stage != "PLANNED" || ro.RolloutID == "" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	isProblem(t, h.call("POST", "/v1/rollouts", hTenantA, "release-manager", create), 409)
	isProblem(t, h.call("POST", "/v1/rollouts", hTenantA, "release-manager", map[string]any{"connection_id": c.ConnectionID, "pilot_daily_cap": 0}), 422)
	// another tenant sees a missing rollout and cannot create one on the connection
	isProblem(t, h.call("GET", "/v1/rollouts/"+ro.RolloutID, hTenantB, "reader", nil), 404)
	isProblem(t, h.call("POST", "/v1/rollouts", hTenantB, "release-manager", create), 404)
	if l := h.call("GET", "/v1/rollouts", hTenantB, "reader", nil); strings.Contains(l.Body.String(), ro.RolloutID) {
		t.Fatal("listed another tenant's rollout")
	}

	// the attestations and the evidence
	isProblem(t, h.call("POST", "/v1/rollouts/"+ro.RolloutID+":request-advance", hTenantA, "release-manager", map[string]any{"attestations": []string{"ONCALL"}}), 422)
	h.cert = xicconn.CertStale
	nr := h.call("POST", "/v1/rollouts/"+ro.RolloutID+":request-advance", hTenantA, "release-manager", map[string]any{"attestations": allAttestations})
	p := isProblem(t, nr, 409)
	if !strings.Contains(nr.Body.String(), "certification_valid") || p["readiness"] == nil {
		t.Fatalf("the refusal carries the readiness report: %s", nr.Body.String())
	}
	h.cert = xicconn.CertValid
	var q rollout.Request
	w = h.call("POST", "/v1/rollouts/"+ro.RolloutID+":request-advance", hTenantA, "release-manager", map[string]any{"attestations": allAttestations, "reason": "start the pilot"})
	decodeInto(t, w, &q)
	if w.Code != 202 || q.Status != "PENDING" || q.ToState != "PILOT" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if rd := h.call("GET", "/v1/rollouts/"+ro.RolloutID+"/readiness", hTenantA, "reader", nil); rd.Code != 200 || !strings.Contains(rd.Body.String(), `"ready":true`) {
		t.Fatalf("%d %s", rd.Code, rd.Body.String())
	}

	// a second person decides; another tenant cannot; the right permission is needed
	isProblem(t, h.call("POST", "/v1/rollout-requests/"+q.RequestID+":approve", hTenantA, "release-manager", map[string]any{}), 403)
	isProblem(t, h.call("POST", "/v1/rollout-requests/"+q.RequestID+":approve", hTenantB, "approver", map[string]any{}), 404)
	h.authz.deny("junior", ActionRolloutDecide)
	if x := h.call("POST", "/v1/rollout-requests/"+q.RequestID+":approve", hTenantA, "junior", map[string]any{}); x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}
	isProblem(t, h.call("POST", "/v1/rollout-requests/"+q.RequestID+":explode", hTenantA, "approver", map[string]any{}), 404)
	w = h.call("POST", "/v1/rollout-requests/"+q.RequestID+":approve", hTenantA, "approver", map[string]any{"note": "ok"})
	decodeInto(t, w, &q)
	if w.Code != 200 || q.Status != "USED" || q.DecidedBy != "approver" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	g := h.call("GET", "/v1/rollouts/"+ro.RolloutID, hTenantA, "reader", nil)
	if g.Code != 200 || !strings.Contains(g.Body.String(), `"stage":"PILOT"`) || !strings.Contains(g.Body.String(), "pilot_used_today") {
		t.Fatalf("%d %s", g.Code, g.Body.String())
	}
	if ev := h.call("GET", "/v1/rollouts/"+ro.RolloutID+"/events", hTenantA, "reader", nil); ev.Code != 200 || !strings.Contains(ev.Body.String(), "ADVANCED") {
		t.Fatalf("%d %s", ev.Code, ev.Body.String())
	}
	if lr := h.call("GET", "/v1/rollout-requests?subject_kind=rollout&status=used", hTenantA, "reader", nil); lr.Code != 200 || !strings.Contains(lr.Body.String(), q.RequestID) {
		t.Fatalf("%d %s", lr.Code, lr.Body.String())
	}
	if lr := h.call("GET", "/v1/rollout-requests", hTenantB, "reader", nil); strings.Contains(lr.Body.String(), q.RequestID) {
		t.Fatal("listed another tenant's request")
	}

	// rollback: needs a reason, no second person, and another tenant cannot do it
	isProblem(t, h.call("POST", "/v1/rollouts/"+ro.RolloutID+":rollback", hTenantA, "release-manager", map[string]any{}), 422)
	isProblem(t, h.call("POST", "/v1/rollouts/"+ro.RolloutID+":rollback", hTenantB, "release-manager", map[string]any{"reason": "x"}), 404)
	isProblem(t, h.call("POST", "/v1/rollouts/"+ro.RolloutID+":rollback", hTenantA, "release-manager", map[string]any{"reason": "x", "expected_version": 99}), 409)
	w = h.call("POST", "/v1/rollouts/"+ro.RolloutID+":rollback", hTenantA, "release-manager", map[string]any{"reason": "provider incident"})
	decodeInto(t, w, &ro)
	if w.Code != 200 || ro.Stage != "ROLLED_BACK" || ro.RollbackReason != "provider incident" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	isProblem(t, h.call("POST", "/v1/rollouts/"+ro.RolloutID+":rollback", hTenantA, "release-manager", map[string]any{"reason": "again"}), 409)
	// authorization is per action, and a caller with no identity is refused
	h.authz.deny("viewer", ActionRolloutCreate)
	if x := h.call("POST", "/v1/rollouts", hTenantA, "viewer", create); x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}
	if x := h.call("GET", "/v1/rollouts", hTenantA, "", nil); x.Code != 401 && x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}
}

func TestLegacyHTTP_MigrationFromDiscoveryToCutover(t *testing.T) {
	h := newROHarness(t)
	c := h.active(t, hTenantA, "acc-lg")
	var l rollout.Legacy
	w := h.call("POST", "/v1/legacy-connectors", hTenantA, "migration-lead", map[string]any{"name": "bank-x-payments", "owning_service": "bank-integration-svc", "direction": "OUTBOUND"})
	decodeInto(t, w, &l)
	if w.Code != 201 || l.State != "DISCOVERED" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	isProblem(t, h.call("POST", "/v1/legacy-connectors", hTenantA, "migration-lead", map[string]any{"name": "bank-x-payments", "owning_service": "s", "direction": "OUTBOUND"}), 409)
	isProblem(t, h.call("POST", "/v1/legacy-connectors", hTenantA, "migration-lead", map[string]any{"name": "x", "owning_service": "s", "direction": "OUTBOUND"}), 422)
	isProblem(t, h.call("GET", "/v1/legacy-connectors/"+l.LegacyID, hTenantB, "reader", nil), 404)
	isProblem(t, h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+":map", hTenantB, "migration-lead", map[string]any{"connection_id": c.ConnectionID}), 404)
	isProblem(t, h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+":start-shadow", hTenantA, "migration-lead", map[string]any{}), 409)
	if x := h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+":map", hTenantA, "migration-lead", map[string]any{"connection_id": c.ConnectionID}); x.Code != 200 {
		t.Fatalf("%d %s", x.Code, x.Body.String())
	}
	if x := h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+":start-shadow", hTenantA, "migration-lead", map[string]any{}); x.Code != 200 {
		t.Fatalf("%d %s", x.Code, x.Body.String())
	}

	// the owning service registers its attempts
	var ex []map[string]any
	for i := 0; i < 22; i++ {
		ex = append(ex, map[string]any{"external_ref": fmt.Sprintf("ext-%d", i), "domain_request_ref": fmt.Sprintf("req-%d", i), "outcome": "SETTLED",
			"payload_hash": fmt.Sprintf("%064x", i+1), "occurred_at": h.now.Add(-time.Hour)})
		h.repo.Counterpart[fmt.Sprintf("req-%d", i)] = "SETTLED"
	}
	w = h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+"/exchanges", hTenantA, "bank-integration-svc", map[string]any{"exchanges": ex})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"stored":22`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	w = h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+"/exchanges", hTenantA, "bank-integration-svc", map[string]any{"exchanges": ex})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"duplicates":22`) {
		t.Fatalf("a repeat is a no-op: %d %s", w.Code, w.Body.String())
	}
	forged := map[string]any{"external_ref": "ext-0", "outcome": "SETTLED", "payload_hash": fmt.Sprintf("%064x", 9999), "occurred_at": h.now.Add(-time.Hour)}
	isProblem(t, h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+"/exchanges", hTenantA, "bank-integration-svc", map[string]any{"exchanges": []any{forged}}), 409)
	isProblem(t, h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+"/exchanges", hTenantA, "bank-integration-svc", map[string]any{"exchanges": []any{map[string]any{"external_ref": "a b"}}}), 422)
	isProblem(t, h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+"/exchanges", hTenantB, "bank-integration-svc", map[string]any{"exchanges": ex}), 404)
	h.authz.deny("reader-only", ActionLegacyRegister)
	if x := h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+"/exchanges", hTenantA, "reader-only", map[string]any{"exchanges": ex}); x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}

	// parity, then the cutover request needs a soaked shadow and a second person
	win := map[string]any{"from": h.now.Add(-3 * time.Hour), "to": h.now}
	var p rollout.ParityRun
	w = h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+":parity-run", hTenantA, "migration-lead", win)
	decodeInto(t, w, &p)
	if w.Code != 201 || !p.Passed || p.Compared != 22 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	isProblem(t, h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+":request-advance", hTenantA, "migration-lead", map[string]any{}), 409) // not soaked
	h.now = h.now.Add(25 * time.Hour)
	if rd := h.call("GET", "/v1/legacy-connectors/"+l.LegacyID+"/readiness", hTenantA, "reader", nil); rd.Code != 200 || !strings.Contains(rd.Body.String(), `"next_state":"CUTOVER"`) {
		t.Fatalf("%d %s", rd.Code, rd.Body.String())
	}
	win = map[string]any{"from": h.now.Add(-30 * time.Hour), "to": h.now}
	if x := h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+":parity-run", hTenantA, "migration-lead", win); x.Code != 201 {
		t.Fatalf("%d %s", x.Code, x.Body.String())
	}
	var q rollout.Request
	w = h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+":request-advance", hTenantA, "migration-lead", map[string]any{"reason": "cut over"})
	decodeInto(t, w, &q)
	if w.Code != 202 || q.ToState != "CUTOVER" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	isProblem(t, h.call("POST", "/v1/rollout-requests/"+q.RequestID+":approve", hTenantA, "migration-lead", map[string]any{}), 403)
	h.authz.deny("not-legacy-approver", ActionLegacyDecide)
	if x := h.call("POST", "/v1/rollout-requests/"+q.RequestID+":approve", hTenantA, "not-legacy-approver", map[string]any{}); x.Code != 403 {
		t.Fatalf("a request is decided with its subject's permission: %d", x.Code)
	}
	if x := h.call("POST", "/v1/rollout-requests/"+q.RequestID+":approve", hTenantA, "approver", map[string]any{}); x.Code != 200 {
		t.Fatalf("%d %s", x.Code, x.Body.String())
	}
	g := h.call("GET", "/v1/legacy-connectors/"+l.LegacyID, hTenantA, "reader", nil)
	if !strings.Contains(g.Body.String(), `"state":"CUTOVER"`) {
		t.Fatalf("%s", g.Body.String())
	}
	if x := h.call("GET", "/v1/legacy-connectors/"+l.LegacyID+"/parity-runs", hTenantA, "reader", nil); x.Code != 200 || !strings.Contains(x.Body.String(), p.ParityID) {
		t.Fatalf("%d %s", x.Code, x.Body.String())
	}
	if x := h.call("GET", "/v1/legacy-connectors/"+l.LegacyID+"/parity-runs", hTenantB, "reader", nil); x.Code != 404 {
		t.Fatalf("%d", x.Code)
	}
	// after cutover registrations stop, and the cutover can be reverted with a reason
	isProblem(t, h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+"/exchanges", hTenantA, "bank-integration-svc", map[string]any{"exchanges": ex}), 409)
	isProblem(t, h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+":revert-cutover", hTenantA, "migration-lead", map[string]any{}), 422)
	if x := h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+":revert-cutover", hTenantA, "migration-lead", map[string]any{"reason": "provider rejects the mapping"}); x.Code != 200 {
		t.Fatalf("%d %s", x.Code, x.Body.String())
	}
	isProblem(t, h.call("POST", "/v1/legacy-connectors/"+l.LegacyID+":explode", hTenantA, "migration-lead", map[string]any{}), 404)
	if lst := h.call("GET", "/v1/legacy-connectors?state=shadow", hTenantA, "reader", nil); lst.Code != 200 || !strings.Contains(lst.Body.String(), l.LegacyID) {
		t.Fatalf("%d %s", lst.Code, lst.Body.String())
	}
	if lst := h.call("GET", "/v1/legacy-connectors", hTenantB, "reader", nil); strings.Contains(lst.Body.String(), l.LegacyID) {
		t.Fatal("listed another tenant's connector")
	}
}

func TestCertificationStatusHTTP(t *testing.T) {
	h := newRHarness(t)
	c := h.active(t, hTenantA, "acc-cs")
	w := h.call("GET", "/v1/integration-connections/"+c.ConnectionID+"/certification-status", hTenantA, "reader", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"NOT_REQUIRED"`) || !strings.Contains(w.Body.String(), `"retest_due":false`) {
		t.Fatalf("a sandbox connection needs no certification: %d %s", w.Code, w.Body.String())
	}
	isProblem(t, h.call("GET", "/v1/integration-connections/"+c.ConnectionID+"/certification-status", hTenantB, "reader", nil), 404)
	h.authz.deny("nobody", ActionConnRead)
	if x := h.call("GET", "/v1/integration-connections/"+c.ConnectionID+"/certification-status", hTenantA, "nobody", nil); x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}
}
