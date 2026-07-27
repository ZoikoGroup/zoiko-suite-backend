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
	"go.uber.org/zap"

	"zoiko.io/consolidation-svc/internal/clients"
	"zoiko.io/consolidation-svc/internal/domain"
	"zoiko.io/consolidation-svc/internal/handler"
	"zoiko.io/consolidation-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	runs      map[string]*domain.ConsolidationRun
	snapshots map[string][]domain.BalanceSnapshot
}

func newStubStore() *stubStore {
	return &stubStore{
		runs:      make(map[string]*domain.ConsolidationRun),
		snapshots: make(map[string][]domain.BalanceSnapshot),
	}
}

func (s *stubStore) CreateRun(_ context.Context, run *domain.ConsolidationRun) error {
	s.runs[run.ConsolidationRunID] = run
	return nil
}

func (s *stubStore) GetRun(_ context.Context, id string) (*domain.ConsolidationRun, error) {
	run, ok := s.runs[id]
	if !ok {
		return nil, domain.ErrRunNotFound
	}
	return run, nil
}

func (s *stubStore) ListRuns(_ context.Context, groupLegalEntityID string) ([]domain.ConsolidationRun, error) {
	var out []domain.ConsolidationRun
	for _, run := range s.runs {
		if groupLegalEntityID != "" && run.GroupLegalEntityID != groupLegalEntityID {
			continue
		}
		out = append(out, *run)
	}
	return out, nil
}

func (s *stubStore) CompleteRun(_ context.Context, id, status string, exceptionCount int, completedAt time.Time) error {
	run, ok := s.runs[id]
	if !ok {
		return domain.ErrRunNotFound
	}
	run.Status = status
	run.ExceptionCount = exceptionCount
	t := completedAt
	run.CompletedAt = &t
	return nil
}

func (s *stubStore) CreateBalanceSnapshots(_ context.Context, snapshots []domain.BalanceSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}
	runID := snapshots[0].ConsolidationRunID
	s.snapshots[runID] = append(s.snapshots[runID], snapshots...)
	return nil
}

func (s *stubStore) ListSnapshotsByRun(_ context.Context, runID string) ([]domain.BalanceSnapshot, error) {
	snaps, ok := s.snapshots[runID]
	if !ok {
		return []domain.BalanceSnapshot{}, nil
	}
	return snaps, nil
}

type stubPublisher struct {
	started, completed, exceptions int
}

func (p *stubPublisher) PublishRunStarted(_ context.Context, _ string, _ domain.ConsolidationRun) {
	p.started++
}
func (p *stubPublisher) PublishCompleted(_ context.Context, _ string, _ domain.ConsolidationRun, _ int) {
	p.completed++
}
func (p *stubPublisher) PublishExceptionDetected(_ context.Context, _ string, _ domain.ConsolidationRun, _ []string) {
	p.exceptions++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubClients struct {
	glBalances map[string]map[string]float64
	glErr      error
}

func (c *stubClients) FetchTrialBalance(_ context.Context, _, legalEntityID, _ string) (map[string]float64, error) {
	if c.glErr != nil {
		return nil, c.glErr
	}
	if c.glBalances != nil {
		if bal, ok := c.glBalances[legalEntityID]; ok {
			return bal, nil
		}
	}
	return map[string]float64{"1000-Cash": 10000.0}, nil
}

func (c *stubClients) FetchMatchedIntercompanyEntries(_ context.Context, _ string) ([]clients.IntercompanyEntry, error) {
	return []clients.IntercompanyEntry{}, nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, cl *stubClients) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, cl, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── StartRun Tests ────────────────────────────────────────────────────────────

func TestStartRun_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id": "le-group",
		"child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period":         "2024-Q1",
		"target_currency":       "USD",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestStartRun_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id": "le-group",
		"child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period":         "2024-Q1",
		"target_currency":       "USD",
	}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

func TestStartRun_MissingFields(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id": "le-group",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestStartRun_HappyPath(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	cl := &stubClients{
		glBalances: map[string]map[string]float64{
			"le-us": {"1000-Cash": 5000.0, "2000-AP": -2000.0},
			"le-uk": {"1000-Cash": 3000.0, "2000-AP": -1000.0},
		},
	}
	r := newRouter(s, pub, &stubAuthZ{}, cl)
	rr := doReq(r, http.MethodPost, "/v1/consolidation/runs/", map[string]any{
		"group_legal_entity_id":  "le-group",
		"child_legal_entity_ids": []string{"le-us", "le-uk"},
		"fiscal_period":          "2024-Q1",
		"target_currency":        "USD",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var resp domain.ConsolidationRunResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Status != "COMPLETED" {
		t.Errorf("expected COMPLETED status, got %q", resp.Status)
	}
	if len(resp.Snapshots) != 2 {
		t.Fatalf("expected 2 balance snapshots, got %d", len(resp.Snapshots))
	}
	if pub.started != 1 {
		t.Errorf("expected 1 started event got %d", pub.started)
	}
	if pub.completed != 1 {
		t.Errorf("expected 1 completed event got %d", pub.completed)
	}
}

// ── ListRuns & Snapshots Tests ────────────────────────────────────────────────

func TestGetRun_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/consolidation/runs/nonexistent-id", nil, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d", rr.Code)
	}
}