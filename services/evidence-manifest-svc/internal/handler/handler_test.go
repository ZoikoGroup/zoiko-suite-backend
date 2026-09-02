package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/aggregator"
	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/handler"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

// ── stub store ───────────────────────────────────────────────────────────────

type stubStore struct {
	manifests map[string]*domain.EvidenceManifest
	records   map[string][]domain.ManifestRecord
	createErr error
}

func newStubStore() *stubStore {
	return &stubStore{manifests: map[string]*domain.EvidenceManifest{}, records: map[string][]domain.ManifestRecord{}}
}

// Every method below scopes to the tenant on the context, mirroring
// PgStore. These previously took `_ context.Context` and ignored it, so the
// stub could not enforce a tenant boundary even in principle — any handler
// test asserting isolation would have been asserting against a fake that
// could not fail. Taking ctx is what makes those assertions mean something
// about production.
func (s *stubStore) tenantOf(ctx context.Context, manifestID string) (*domain.EvidenceManifest, bool) {
	m, ok := s.manifests[manifestID]
	if !ok || m.TenantID != svcmiddleware.TenantFromContext(ctx) {
		return nil, false
	}
	return m, true
}

func (s *stubStore) CreateManifest(ctx context.Context, m *domain.EvidenceManifest) error {
	if s.createErr != nil {
		return s.createErr
	}
	m.ManifestID = "manifest-1"
	m.Status = domain.StatusPending
	// Attribution comes from the verified context, as in PgStore — never
	// from the request body, which is what made the tenant caller-declared.
	m.TenantID = svcmiddleware.TenantFromContext(ctx)
	s.manifests[m.ManifestID] = m
	return nil
}

func (s *stubStore) AddRecord(ctx context.Context, r *domain.ManifestRecord) error {
	// Mirrors the RLS WITH CHECK on manifest_records: a record cannot be
	// appended to a manifest the caller cannot see.
	if _, ok := s.tenantOf(ctx, r.ManifestID); !ok {
		return domain.ErrManifestNotFound
	}
	r.ManifestRecordID = "record-" + r.SourceRecordID
	s.records[r.ManifestID] = append(s.records[r.ManifestID], *r)
	return nil
}

func (s *stubStore) FinalizeGenerated(ctx context.Context, manifestID, checksum string) (*domain.EvidenceManifest, error) {
	m, ok := s.tenantOf(ctx, manifestID)
	if !ok {
		return nil, domain.ErrManifestNotFound
	}
	m.Status = domain.StatusGenerated
	m.ChecksumSHA256 = &checksum
	return m, nil
}

func (s *stubStore) FinalizeFailed(ctx context.Context, manifestID, reason string) (*domain.EvidenceManifest, error) {
	m, ok := s.tenantOf(ctx, manifestID)
	if !ok {
		return nil, domain.ErrManifestNotFound
	}
	m.Status = domain.StatusFailed
	m.FailureReason = &reason
	return m, nil
}

func (s *stubStore) FindManifestByID(ctx context.Context, manifestID string) (*domain.EvidenceManifest, error) {
	m, ok := s.tenantOf(ctx, manifestID)
	if !ok {
		return nil, domain.ErrManifestNotFound
	}
	return m, nil
}

func (s *stubStore) ListRecords(ctx context.Context, manifestID string) ([]domain.ManifestRecord, error) {
	if _, ok := s.tenantOf(ctx, manifestID); !ok {
		return nil, nil
	}
	return s.records[manifestID], nil
}

// ── stub aggregator sources ──────────────────────────────────────────────────

type stubGovernance struct {
	listResult []aggregator.SourceRecord
	listErr    error
	getResult  *aggregator.SourceRecord
	getErr     error
}

func (s *stubGovernance) ListByEntityAndDateRange(_ context.Context, _ string, _, _ *time.Time) ([]aggregator.SourceRecord, error) {
	return s.listResult, s.listErr
}
func (s *stubGovernance) GetByID(_ context.Context, _ string) (*aggregator.SourceRecord, error) {
	return s.getResult, s.getErr
}

type stubAccess struct {
	result *aggregator.SourceRecord
	err    error
}

func (s *stubAccess) GetByID(_ context.Context, id string) (*aggregator.SourceRecord, error) {
	return s.result, s.err
}

type stubWorkflow struct {
	result *aggregator.SourceRecord
	err    error
}

func (s *stubWorkflow) GetByID(_ context.Context, id string) (*aggregator.SourceRecord, error) {
	return s.result, s.err
}

// stubWorkflowHistory is a test double for handler.WorkflowHistorySource —
// see aggregator.WorkflowHistoryClient's doc comment for what this closes.
// Defaults to an empty history (no error) so existing tests that never
// configure it keep exercising the real path unaffected.
type stubWorkflowHistory struct {
	result []aggregator.SourceRecord
	err    error
	calls  int
}

func (s *stubWorkflowHistory) ListByInstanceID(_ context.Context, _ string) ([]aggregator.SourceRecord, error) {
	s.calls++
	return s.result, s.err
}

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct {
	published []domain.EvidenceManifest
	err       error
}

func (p *stubPublisher) PublishManifestGenerated(_ context.Context, m *domain.EvidenceManifest, _ string) error {
	if p.err != nil {
		return p.err
	}
	p.published = append(p.published, *m)
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// stubAuthz is a test double for handler.AuthzChecker. It GRANTS by default
// so the existing behavioural tests keep exercising the real path; tests that
// need the deny or unavailable branch set err.
type stubAuthz struct {
	err error
}

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	return s.err
}

func newRouter(s *stubStore, gov *stubGovernance, acc *stubAccess, wf *stubWorkflow, pub *stubPublisher) chi.Router {
	return newRouterWithWorkflowHistory(s, gov, acc, wf, &stubWorkflowHistory{}, pub, &stubAuthz{})
}

func newRouterWithWorkflowHistory(s *stubStore, gov *stubGovernance, acc *stubAccess, wf *stubWorkflow, wfh *stubWorkflowHistory, pub *stubPublisher, az handler.AuthzChecker) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, gov, acc, wf, wfh, pub, az, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func defaultSources() (*stubGovernance, *stubAccess, *stubWorkflow, *stubPublisher) {
	return &stubGovernance{}, &stubAccess{}, &stubWorkflow{}, &stubPublisher{}
}

// ── GenerateManifest ─────────────────────────────────────────────────────────

func TestGenerateManifest_WithExplicitGovernanceID_Returns201Generated(t *testing.T) {
	gov, acc, wf, pub := defaultSources()
	gov.getResult = &aggregator.SourceRecord{
		SourceType: domain.SourceGovernanceDecision, SourceRecordID: "gd-1",
		RawJSON: []byte(`{"decision_id":"gd-1","outcome":"GRANTED"}`),
	}
	s := newStubStore()
	r := newRouter(s, gov, acc, wf, pub)

	body, _ := json.Marshal(domain.GenerateManifestRequest{
		TenantID: "t1", LegalEntityID: "e1", ScenarioType: domain.ScenarioAudit,
		GovernanceDecisionIDs: []string{"gd-1"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var got domain.EvidenceManifest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, domain.StatusGenerated, got.Status)
	require.NotNil(t, got.ChecksumSHA256)
	assert.NotEmpty(t, *got.ChecksumSHA256)

	// Published event fired.
	require.Len(t, pub.published, 1)
	assert.Equal(t, "manifest-1", pub.published[0].ManifestID)
}

func TestGenerateManifest_MissingScenarioType_Returns400(t *testing.T) {
	gov, acc, wf, pub := defaultSources()
	r := newRouter(newStubStore(), gov, acc, wf, pub)

	body, _ := json.Marshal(domain.GenerateManifestRequest{TenantID: "t1", LegalEntityID: "e1"})
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGenerateManifest_InvalidScenarioType_Returns400(t *testing.T) {
	gov, acc, wf, pub := defaultSources()
	r := newRouter(newStubStore(), gov, acc, wf, pub)

	body, _ := json.Marshal(domain.GenerateManifestRequest{
		TenantID: "t1", LegalEntityID: "e1", ScenarioType: "NOT_A_REAL_SCENARIO",
		GovernanceDecisionIDs: []string{"gd-1"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGenerateManifest_NoRecordsRequested_Returns400(t *testing.T) {
	gov, acc, wf, pub := defaultSources()
	r := newRouter(newStubStore(), gov, acc, wf, pub)

	body, _ := json.Marshal(domain.GenerateManifestRequest{
		TenantID: "t1", LegalEntityID: "e1", ScenarioType: domain.ScenarioAudit,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// This is THE key fail-closed test: if authorization-svc is unreachable while
// assembling a manifest that also references a governance decision, the WHOLE
// manifest must fail — never a partial manifest that looks complete.
func TestGenerateManifest_OneSourceUnavailable_FailsClosed_WholeManifestFails(t *testing.T) {
	gov, acc, wf, pub := defaultSources()
	gov.getResult = &aggregator.SourceRecord{SourceType: domain.SourceGovernanceDecision, SourceRecordID: "gd-1", RawJSON: []byte(`{}`)}
	acc.err = aggregator.ErrSourceUnavailable // authorization-svc down

	s := newStubStore()
	r := newRouter(s, gov, acc, wf, pub)

	body, _ := json.Marshal(domain.GenerateManifestRequest{
		TenantID: "t1", LegalEntityID: "e1", ScenarioType: domain.ScenarioAudit,
		GovernanceDecisionIDs: []string{"gd-1"},
		AccessDecisionIDs:     []string{"ad-1"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	// The manifest must be marked FAILED, not left dangling or silently GENERATED.
	m := s.manifests["manifest-1"]
	require.NotNil(t, m)
	assert.Equal(t, domain.StatusFailed, m.Status)
	assert.Empty(t, pub.published, "no event must be published for a failed manifest")
	assert.Empty(t, s.records["manifest-1"], "no partial records must be persisted for a failed manifest")
}

func TestGenerateManifest_MultipleSources_AllIncluded(t *testing.T) {
	gov, acc, wf, pub := defaultSources()
	gov.listResult = []aggregator.SourceRecord{
		{SourceType: domain.SourceGovernanceDecision, SourceRecordID: "gd-1", RawJSON: []byte(`{"a":1}`)},
	}
	acc.result = &aggregator.SourceRecord{SourceType: domain.SourceAccessDecision, SourceRecordID: "ad-1", RawJSON: []byte(`{"b":2}`)}
	wf.result = &aggregator.SourceRecord{SourceType: domain.SourceWorkflowInstance, SourceRecordID: "wf-1", RawJSON: []byte(`{"c":3}`)}

	s := newStubStore()
	r := newRouter(s, gov, acc, wf, pub)

	from := time.Now().Add(-24 * time.Hour)
	body, _ := json.Marshal(domain.GenerateManifestRequest{
		TenantID: "t1", LegalEntityID: "e1", ScenarioType: domain.ScenarioLegalDiscovery,
		GovernanceDecisionsFrom: &from,
		AccessDecisionIDs:       []string{"ad-1"},
		WorkflowInstanceIDs:     []string{"wf-1"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	require.Len(t, s.records["manifest-1"], 3, "all three source types must be included")
}

// TestGenerateManifest_WorkflowInstance_IncludesRealHistory is the first
// real caller of workflow-history-svc anywhere in this codebase — closing
// the gap workflow-history-svc's own package doc named directly. A
// requested WorkflowInstanceID must pull both the workflow-svc snapshot
// AND every real transition event, not just the former.
func TestGenerateManifest_WorkflowInstance_IncludesRealHistory(t *testing.T) {
	gov, acc, wf, pub := defaultSources()
	wf.result = &aggregator.SourceRecord{SourceType: domain.SourceWorkflowInstance, SourceRecordID: "wf-2", RawJSON: []byte(`{"c":3}`)}
	wfh := &stubWorkflowHistory{result: []aggregator.SourceRecord{
		{SourceType: domain.SourceWorkflowHistory, SourceRecordID: "evt-1", RawJSON: []byte(`{"event_id":"evt-1"}`)},
		{SourceType: domain.SourceWorkflowHistory, SourceRecordID: "evt-2", RawJSON: []byte(`{"event_id":"evt-2"}`)},
	}}

	s := newStubStore()
	r := newRouterWithWorkflowHistory(s, gov, acc, wf, wfh, pub, &stubAuthz{})

	body, _ := json.Marshal(domain.GenerateManifestRequest{
		TenantID: "t1", LegalEntityID: "e1", ScenarioType: domain.ScenarioLegalDiscovery,
		WorkflowInstanceIDs: []string{"wf-2"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	require.Len(t, s.records["manifest-1"], 3, "the workflow-svc snapshot plus both real transition events must be included")
	if wfh.calls != 1 {
		t.Fatalf("expected exactly one real ListByInstanceID call to workflow-history-svc, got %d", wfh.calls)
	}
}

// TestGenerateManifest_WorkflowHistoryUnavailable_FailsClosed verifies the
// manifest is all-or-nothing — the same fail-closed doctrine every other
// source already has.
func TestGenerateManifest_WorkflowHistoryUnavailable_FailsClosed(t *testing.T) {
	gov, acc, wf, pub := defaultSources()
	wf.result = &aggregator.SourceRecord{SourceType: domain.SourceWorkflowInstance, SourceRecordID: "wf-3", RawJSON: []byte(`{"c":3}`)}
	wfh := &stubWorkflowHistory{err: aggregator.ErrSourceUnavailable}

	s := newStubStore()
	r := newRouterWithWorkflowHistory(s, gov, acc, wf, wfh, pub, &stubAuthz{})

	body, _ := json.Marshal(domain.GenerateManifestRequest{
		TenantID: "t1", LegalEntityID: "e1", ScenarioType: domain.ScenarioLegalDiscovery,
		WorkflowInstanceIDs: []string{"wf-3"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Empty(t, s.records["manifest-1"], "no partial records must be persisted when workflow-history-svc is unavailable")
}

// ── GetManifest / ListRecords ────────────────────────────────────────────────

func TestGetManifest_NotFound_Returns404(t *testing.T) {
	gov, acc, wf, pub := defaultSources()
	r := newRouter(newStubStore(), gov, acc, wf, pub)

	req := httptest.NewRequest(http.MethodGet, "/v1/evidence-manifests/nope", nil)
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestListRecords_ReturnsAllRecordsForManifest(t *testing.T) {
	gov, acc, wf, pub := defaultSources()
	gov.getResult = &aggregator.SourceRecord{SourceType: domain.SourceGovernanceDecision, SourceRecordID: "gd-1", RawJSON: []byte(`{}`)}

	s := newStubStore()
	r := newRouter(s, gov, acc, wf, pub)

	createBody, _ := json.Marshal(domain.GenerateManifestRequest{
		TenantID: "t1", LegalEntityID: "e1", ScenarioType: domain.ScenarioAudit,
		GovernanceDecisionIDs: []string{"gd-1"},
	})
	seedReq := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(createBody))
	seedReq.Header.Set("X-Tenant-Id", "t1")
	seedReq.Header.Set("X-Principal-Id", "principal-test-01")
	r.ServeHTTP(httptest.NewRecorder(), seedReq)

	req := httptest.NewRequest(http.MethodGet, "/v1/evidence-manifests/manifest-1/records", nil)
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var records []domain.ManifestRecord
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &records))
	require.Len(t, records, 1)
	assert.Equal(t, domain.SourceGovernanceDecision, records[0].SourceType)
}
