package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/commercial-account-svc/internal/authz"
	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

type usageStub struct {
	err      error
	dedupErr error // GetDedupStatus's own error, separate from err (RegisterUsageEvent's replay uses both)
	event    *domain.UsageEventRecord
	stmt     *domain.UsageStatement
	calls    []string
	tenant   string
	regArgs  struct {
		org, sub, meterKey string
		version            int
		eventID            string
		in                 domain.EventInput
	}
}

func (s *usageStub) rec(ctx context.Context, name string) {
	s.calls = append(s.calls, name)
	s.tenant = svcmiddleware.TenantFromContext(ctx)
}
func (s *usageStub) RegisterMeterDefinition(ctx context.Context, m *domain.MeterDefinition, _ domain.IdempotencyClaim) (*domain.MeterDefinition, error) {
	s.rec(ctx, "RegisterMeterDefinition")
	return m, s.err
}
func (s *usageStub) RetireMeterDefinition(ctx context.Context, key string, v int, _, _ string, _ time.Time) (*domain.MeterDefinition, error) {
	s.rec(ctx, "RetireMeterDefinition")
	return &domain.MeterDefinition{MeterKey: key, MeterVersion: v}, s.err
}
func (s *usageStub) GetMeterDefinition(ctx context.Context, key string, v int) (*domain.MeterDefinition, error) {
	s.rec(ctx, "GetMeterDefinition")
	return &domain.MeterDefinition{MeterKey: key, MeterVersion: v}, s.err
}
func (s *usageStub) ListMeterDefinitions(ctx context.Context) ([]domain.MeterDefinition, error) {
	s.rec(ctx, "ListMeterDefinitions")
	return nil, s.err
}
func (s *usageStub) RegisterUsageEvent(ctx context.Context, org, sub, meterKey string, version int, eventID string, in domain.EventInput, _ string, _ time.Time, _ domain.IdempotencyClaim) (*domain.UsageEventRecord, error) {
	s.rec(ctx, "RegisterUsageEvent")
	s.regArgs.org, s.regArgs.sub, s.regArgs.meterKey, s.regArgs.version, s.regArgs.eventID, s.regArgs.in = org, sub, meterKey, version, eventID, in
	return s.event, s.err
}
func (s *usageStub) CorrectUsageEvent(ctx context.Context, meterKey, orig, newID string, in domain.EventInput, reason, actor string, now time.Time, _ domain.IdempotencyClaim) (*domain.UsageEventRecord, error) {
	s.rec(ctx, "CorrectUsageEvent")
	return s.event, s.err
}
func (s *usageStub) CloseUsageWindow(ctx context.Context, sub string, term int, meterKey, actor string, now time.Time, _ domain.IdempotencyClaim) (*domain.UsageStatement, error) {
	s.rec(ctx, "CloseUsageWindow")
	return s.stmt, s.err
}
func (s *usageStub) CertifyUsageStatement(ctx context.Context, id, actor string, now time.Time, _ domain.IdempotencyClaim) (*domain.UsageStatement, error) {
	s.rec(ctx, "CertifyUsageStatement")
	return s.stmt, s.err
}
func (s *usageStub) ReopenWindow(ctx context.Context, id, actor, reason string, now time.Time, _ domain.IdempotencyClaim) (*domain.UsageStatement, error) {
	s.rec(ctx, "ReopenWindow")
	return s.stmt, s.err
}
func (s *usageStub) CreateUsageAdjustment(ctx context.Context, targetStatementID, meterKey, sourceUsageEventID, reason, actor string, now time.Time, _ domain.IdempotencyClaim) (*domain.UsageAdjustment, error) {
	s.rec(ctx, "CreateUsageAdjustment")
	return &domain.UsageAdjustment{}, s.err
}
func (s *usageStub) GetUsage(ctx context.Context, sub string, term int, meterKey string) (*domain.UsageStatement, error) {
	s.rec(ctx, "GetUsage")
	return s.stmt, s.err
}
func (s *usageStub) GetUsageStatement(ctx context.Context, id string) (*domain.UsageStatement, error) {
	s.rec(ctx, "GetUsageStatement")
	return s.stmt, s.err
}
func (s *usageStub) ExplainAggregation(ctx context.Context, id string) ([]domain.UsageEventRecord, error) {
	s.rec(ctx, "ExplainAggregation")
	return nil, s.err
}
func (s *usageStub) GetLateEvents(ctx context.Context, sub string) ([]domain.UsageEventRecord, error) {
	s.rec(ctx, "GetLateEvents")
	return nil, s.err
}
func (s *usageStub) GetDedupStatus(ctx context.Context, meterKey, id string) (*domain.UsageEventRecord, error) {
	s.rec(ctx, "GetDedupStatus")
	return s.event, s.dedupErr
}

func newUsageRouter(st *usageStub, az *scopedAuthz) http.Handler {
	logger, _ := zap.NewDevelopment()
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterUsageRoutes(r, NewUsageHandler(st, az, logger).WithClock(func() time.Time { return fixedNow }))
	return r
}

const usageEventBody = `{"organization_id":"` + customerOrg + `","subscription_id":"` + "csub_00000000-0000-4000-8000-000000000001" +
	`","meter_key":"api.calls","meter_version":1,"usage_event_id":"evt-1","quantity":"5","occurred_at":"2030-01-01T00:00:00Z","source_service":"api-gateway-svc"}`

func TestUsageHandler_RegisterMeterDefinitionNeedsTheManageGrant(t *testing.T) {
	st := &usageStub{}
	az := &scopedAuthz{}
	body := `{"meter_key":"api.calls","meter_version":1,"display_name":"API calls","unit":"call","aggregation_method":"SUM"}`
	w := serve(t, newUsageRouter(st, az), req{method: http.MethodPost, path: "/v1/commercial/meter-definitions",
		body: body, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusCreated || az.checked[0] != platformScopeID+"|"+ActionMeterDefinitionManage {
		t.Fatalf("HTTP %d checked=%v %s", w.Code, az.checked, w.Body.String())
	}
	denied := &scopedAuthz{deny: map[string]error{platformScopeID + "|" + ActionMeterDefinitionManage: authzpkg.ErrAuthorizationDenied}}
	st = &usageStub{}
	w = serve(t, newUsageRouter(st, denied), req{method: http.MethodPost, path: "/v1/commercial/meter-definitions",
		body: body, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusForbidden || len(st.calls) != 0 {
		t.Fatalf("a principal without the manage grant registered a meter: HTTP %d", w.Code)
	}
}

func TestUsageHandler_RegisterMeterDefinitionValidatesUniqueDimension(t *testing.T) {
	st := &usageStub{}
	body := `{"meter_key":"api.calls","meter_version":1,"display_name":"x","unit":"call","aggregation_method":"UNIQUE_COUNT"}`
	w := serve(t, newUsageRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/meter-definitions",
		body: body, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusBadRequest || len(st.calls) != 0 {
		t.Fatalf("a UNIQUE_COUNT meter with no unique_dimension was registered: HTTP %d", w.Code)
	}
}

// Ingestion needs the ingest grant (checked the same way as every other
// command in this service), not the caller's own tenant membership — the
// organization is named in the body because the caller is a workload, not
// the tenant itself.
func TestUsageHandler_RegisterUsageEventNeedsTheIngestGrant(t *testing.T) {
	st := &usageStub{event: &domain.UsageEventRecord{MeterKey: "api.calls", UsageEventID: "evt-1"}}
	az := &scopedAuthz{}
	w := serve(t, newUsageRouter(st, az), req{method: http.MethodPost, path: "/v1/commercial/usage-events",
		body: usageEventBody, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusCreated || az.checked[0] != platformScopeID+"|"+ActionUsageIngest {
		t.Fatalf("HTTP %d checked=%v %s", w.Code, az.checked, w.Body.String())
	}
	if st.regArgs.org != customerOrg || st.regArgs.meterKey != "api.calls" || st.regArgs.version != 1 || st.regArgs.eventID != "evt-1" {
		t.Fatalf("registration args: %+v", st.regArgs)
	}

	denied := &scopedAuthz{deny: map[string]error{platformScopeID + "|" + ActionUsageIngest: authzpkg.ErrAuthorizationDenied}}
	st = &usageStub{}
	w = serve(t, newUsageRouter(st, denied), req{method: http.MethodPost, path: "/v1/commercial/usage-events",
		body: usageEventBody, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusForbidden || len(st.calls) != 0 {
		t.Fatalf("a caller without the ingest grant registered usage: HTTP %d", w.Code)
	}
}

func TestUsageHandler_RegisterUsageEventValidatesRequiredFields(t *testing.T) {
	cases := map[string]string{
		"missing organization_id": `{"subscription_id":"csub_00000000-0000-4000-8000-000000000001","meter_key":"api.calls","meter_version":1,"usage_event_id":"e","quantity":"1","occurred_at":"2030-01-01T00:00:00Z","source_service":"s"}`,
		"bare uuid subscription":  `{"organization_id":"` + customerOrg + `","subscription_id":"3f2504e0-4f89-11d3-9a0c-0305e82c3301","meter_key":"api.calls","meter_version":1,"usage_event_id":"e","quantity":"1","occurred_at":"2030-01-01T00:00:00Z","source_service":"s"}`,
		"missing quantity":        `{"organization_id":"` + customerOrg + `","subscription_id":"csub_00000000-0000-4000-8000-000000000001","meter_key":"api.calls","meter_version":1,"usage_event_id":"e","occurred_at":"2030-01-01T00:00:00Z","source_service":"s"}`,
		"missing occurred_at":     `{"organization_id":"` + customerOrg + `","subscription_id":"csub_00000000-0000-4000-8000-000000000001","meter_key":"api.calls","meter_version":1,"usage_event_id":"e","quantity":"1","source_service":"s"}`,
	}
	for name, body := range cases {
		st := &usageStub{}
		w := serve(t, newUsageRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/usage-events",
			body: body, headers: map[string]string{"Idempotency-Key": "k"}})
		if w.Code != http.StatusBadRequest || len(st.calls) != 0 {
			t.Errorf("%s: HTTP %d, calls %v", name, w.Code, st.calls)
		}
	}
}

func TestUsageHandler_ReplayReturnsTheDedupedEvent(t *testing.T) {
	existing := &domain.UsageEventRecord{MeterKey: "api.calls", UsageEventID: "evt-1", Status: domain.UsageAccepted}
	st := &usageStub{err: &domain.IdempotentReplayError{ResourceID: "api.calls/evt-1"}, event: existing}
	w := serve(t, newUsageRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/usage-events",
		body: usageEventBody, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusCreated || w.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: HTTP %d replayed=%q %s", w.Code, w.Header().Get("Idempotent-Replayed"), w.Body.String())
	}
}

// Statement certify/reopen is a billing-operations grant, separate from the
// ingest grant a workload holds.
func TestUsageHandler_StatementActionsNeedTheCertifyGrant(t *testing.T) {
	id := domain.NewCommercialID(domain.PrefixUsageStatement)
	st := &usageStub{stmt: &domain.UsageStatement{StatementID: id}}
	az := &scopedAuthz{}
	w := serve(t, newUsageRouter(st, az), req{method: http.MethodPost, path: "/v1/commercial/usage-statements/" + id + ":certify",
		headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusOK || az.checked[0] != platformScopeID+"|"+ActionUsageCertify || st.calls[0] != "CertifyUsageStatement" {
		t.Fatalf("certify: HTTP %d checked=%v calls=%v", w.Code, az.checked, st.calls)
	}

	st = &usageStub{stmt: &domain.UsageStatement{StatementID: id}}
	w = serve(t, newUsageRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/usage-statements/" + id + ":reopen",
		headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusBadRequest || len(st.calls) != 0 {
		t.Fatalf("a reopen without a reason: HTTP %d", w.Code)
	}
	w = serve(t, newUsageRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/usage-statements/" + id + ":reopen",
		body: `{"reason":"dispute resolved"}`, headers: map[string]string{"Idempotency-Key": "k"}})
	if w.Code != http.StatusOK || st.calls[0] != "ReopenWindow" {
		t.Fatalf("reopen with a reason: HTTP %d calls=%v", w.Code, st.calls)
	}
}

func TestUsageHandler_ReadsAreScoped(t *testing.T) {
	st := &usageStub{stmt: &domain.UsageStatement{}}
	az := &scopedAuthz{}
	w := serve(t, newUsageRouter(st, az), req{method: http.MethodGet,
		path:    "/v1/commercial/usage?subscription_id=csub_00000000-0000-4000-8000-000000000001&term_no=1&meter_key=api.calls",
		headers: entHeaders()})
	if w.Code != http.StatusOK || st.tenant != tenantOrg || az.checked[0] != tenantOrg+"|"+ActionUsageRead {
		t.Fatalf("self read: HTTP %d tenant=%s checked=%v %s", w.Code, st.tenant, az.checked, w.Body.String())
	}

	st = &usageStub{stmt: &domain.UsageStatement{}}
	az = &scopedAuthz{}
	w = serve(t, newUsageRouter(st, az), req{method: http.MethodGet,
		path:    "/v1/commercial/usage?organization_id=" + customerOrg + "&subscription_id=csub_00000000-0000-4000-8000-000000000001&term_no=1&meter_key=api.calls",
		headers: entHeaders()})
	if w.Code != http.StatusOK || st.tenant != customerOrg || az.checked[0] != platformScopeID+"|"+ActionUsageRead {
		t.Fatalf("operator read: HTTP %d tenant=%s checked=%v", w.Code, st.tenant, az.checked)
	}
}

func TestUsageHandler_ErrorsMapToStableCodes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{domain.ErrMeterDefinitionNotFound, http.StatusNotFound, CodeMeterDefinitionNotFound},
		{domain.ErrUsageEventExists, http.StatusConflict, CodeUsageEventExists},
		{domain.ErrStatementInvalidState, http.StatusConflict, CodeStatementInvalidState},
		{domain.ErrStatementNotOpenForWindow, http.StatusUnprocessableEntity, CodeNoOpenTermForUsage},
		{domain.ErrReopenNeedsIndependentActor, http.StatusForbidden, CodeSoDViolation},
	}
	for _, tc := range cases {
		st := &usageStub{event: &domain.UsageEventRecord{}, err: tc.err}
		w := serve(t, newUsageRouter(st, &scopedAuthz{}), req{method: http.MethodPost, path: "/v1/commercial/usage-events",
			body: usageEventBody, headers: map[string]string{"Idempotency-Key": "k"}})
		if w.Code != tc.status || problemCode(t, w) != tc.code {
			t.Errorf("%v: HTTP %d %s", tc.err, w.Code, w.Body.String())
		}
	}
}
