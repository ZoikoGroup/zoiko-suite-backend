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

func (s *stubStore) CreateManifest(_ context.Context, m *domain.EvidenceManifest) error {
	if s.createErr != nil {
		return s.createErr
	}
	m.ManifestID = "manifest-1"
	m.Status = domain.StatusPending
	s.manifests[m.ManifestID] = m
	return nil
}

func (s *stubStore) AddRecord(_ context.Context, r *domain.ManifestRecord) error {
	r.ManifestRecordID = "record-" + r.SourceRecordID
	s.records[r.ManifestID] = append(s.records[r.ManifestID], *r)
	return nil
}

func (s *stubStore) FinalizeGenerated(_ context.Context, manifestID, checksum string) (*domain.EvidenceManifest, error) {
	m, ok := s.manifests[manifestID]
	if !ok {
		return nil, domain.ErrManifestNotFound
	}
	m.Status = domain.StatusGenerated
	m.ChecksumSHA256 = &checksum
	return m, nil
}

func (s *stubStore) FinalizeFailed(_ context.Context, manifestID, reason string) (*domain.EvidenceManifest, error) {
	m, ok := s.manifests[manifestID]
	if !ok {
		return nil, domain.ErrManifestNotFound
	}
	m.Status = domain.StatusFailed
	m.FailureReason = &reason
	return m, nil
}

func (s *stubStore) FindManifestByID(_ context.Context, manifestID string) (*domain.EvidenceManifest, error) {
	m, ok := s.manifests[manifestID]
	if !ok {
		return nil, domain.ErrManifestNotFound
	}
	return m, nil
}

func (s *stubStore) ListRecords(_ context.Context, manifestID string) ([]domain.ManifestRecord, error) {
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

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct {
	published []domain.EvidenceManifest
	err       error
}

func (p *stubPublisher) PublishManifestGenerated(_ context.Context, m *domain.EvidenceManifest) error {
	if p.err != nil {
		return p.err
	}
	p.published = append(p.published, *m)
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func newRouter(s *stubStore, gov *stubGovernance, acc *stubAccess, wf *stubWorkflow, pub *stubPublisher) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, gov, acc, wf, pub, zap.NewNop())
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
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	require.Len(t, s.records["manifest-1"], 3, "all three source types must be included")
}

// ── GetManifest / ListRecords ────────────────────────────────────────────────

func TestGetManifest_NotFound_Returns404(t *testing.T) {
	gov, acc, wf, pub := defaultSources()
	r := newRouter(newStubStore(), gov, acc, wf, pub)

	req := httptest.NewRequest(http.MethodGet, "/v1/evidence-manifests/nope", nil)
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
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(createBody)))

	req := httptest.NewRequest(http.MethodGet, "/v1/evidence-manifests/manifest-1/records", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var records []domain.ManifestRecord
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &records))
	require.Len(t, records, 1)
	assert.Equal(t, domain.SourceGovernanceDecision, records[0].SourceType)
}
