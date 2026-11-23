package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/exception-escalation-svc/internal/authz"
	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/store"
)

// fakeDeadlines serves one deadline and counts the calls that reach the store, so a test can
// show a refused request never got as far as the data.
type fakeDeadlines struct {
	store.DeadlineStore
	d          *domain.Deadline
	recalcs    int
	reads      int
	lastReason string
}

func (f *fakeDeadlines) GetDeadline(ctx context.Context, id string) (*domain.Deadline, error) {
	if f.d == nil || f.d.DeadlineID != id {
		return nil, domain.ErrDeadlineNotFound
	}
	return f.d, nil
}
func (f *fakeDeadlines) GetSourceDeadline(ctx context.Context, tenantID, id string) (*domain.DeadlineSourceInfo, error) {
	f.reads++
	return &domain.DeadlineSourceInfo{DeadlineID: id}, nil
}
func (f *fakeDeadlines) ExplainCalculation(ctx context.Context, tenantID, id string) (*domain.ExplainCalculationResult, error) {
	f.reads++
	return &domain.ExplainCalculationResult{DeadlineID: id}, nil
}
func (f *fakeDeadlines) Recalculate(ctx context.Context, p domain.RecalculateParams) (*domain.Deadline, error) {
	f.recalcs++
	f.lastReason = p.Reason
	return f.d, nil
}

type recordingAuthz struct {
	err     error
	actions []string
	entity  []string
}

func (a *recordingAuthz) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	a.actions = append(a.actions, actionType)
	a.entity = append(a.entity, legalEntityID)
	return a.err
}

func deadlineRouter(az AuthZClient, fd *fakeDeadlines) *chi.Mux {
	logger, _ := zap.NewDevelopment()
	h := New(newMockStore(), az, logger)
	r := chi.NewRouter()
	RegisterDeadlineRoutes(r, h, fd)
	return r
}

func newFakeDeadline() *fakeDeadlines {
	return &fakeDeadlines{d: &domain.Deadline{DeadlineID: "dl-1", TenantID: "default", LegalEntityID: "le-9", Title: "t", DueAt: time.Now().Add(time.Hour), Status: domain.DeadlineStatusScheduled}}
}

func do(r http.Handler, method, path, principal, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// The three deadline read endpoints used to have no authorization check: any caller in the
// tenant could read any deadline, its source lock and how it was calculated.
func TestDeadlineReads_RequireAuthorizationOnTheDeadlinesEntity(t *testing.T) {
	for _, path := range []string{"/v1/deadlines/dl-1", "/v1/deadlines/dl-1/source", "/v1/deadlines/dl-1/explain"} {
		fd := newFakeDeadline()

		// No principal: 401.
		if rr := do(deadlineRouter(&recordingAuthz{}, fd), http.MethodGet, path, "", ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s without a principal: got %d, want 401", path, rr.Code)
		}

		// Denied: 403, and the source/explain data was never read.
		az := &recordingAuthz{err: authz.ErrAuthorizationDenied}
		if rr := do(deadlineRouter(az, fd), http.MethodGet, path, "p-1", ""); rr.Code != http.StatusForbidden {
			t.Errorf("%s denied: got %d, want 403", path, rr.Code)
		}
		if fd.reads != 0 {
			t.Errorf("%s: a denied caller reached the store read", path)
		}

		// Authz unavailable: fail closed (503).
		if rr := do(deadlineRouter(&recordingAuthz{err: context.DeadlineExceeded}, fd), http.MethodGet, path, "p-1", ""); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s authz outage: got %d, want 503", path, rr.Code)
		}

		// Allowed: 200, checked against the deadline's own legal entity with DEADLINE_MANAGE.
		az = &recordingAuthz{}
		if rr := do(deadlineRouter(az, fd), http.MethodGet, path, "p-1", ""); rr.Code != http.StatusOK {
			t.Errorf("%s allowed: got %d, want 200", path, rr.Code)
		}
		if len(az.actions) != 1 || az.actions[0] != actionDeadlineManage || az.entity[0] != "le-9" {
			t.Errorf("%s: authz checked %v on %v, want DEADLINE_MANAGE on le-9", path, az.actions, az.entity)
		}
	}
}

// A manual recalculation without a reason is refused before it reaches the store.
func TestRecalculate_RequiresAReason(t *testing.T) {
	fd := newFakeDeadline()
	r := deadlineRouter(&recordingAuthz{}, fd)
	due := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)

	if rr := do(r, http.MethodPost, "/v1/deadlines/dl-1/recalculate", "p-1", `{"due_at":"`+due+`"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("no reason: got %d, want 400", rr.Code)
	}
	if fd.recalcs != 0 {
		t.Fatal("a recalculation without a reason reached the store")
	}
	if rr := do(r, http.MethodPost, "/v1/deadlines/dl-1/recalculate", "p-1", `{"due_at":"`+due+`","reason":"vendor delay"}`); rr.Code != http.StatusOK {
		t.Fatalf("with reason: got %d, want 200", rr.Code)
	}
	if fd.recalcs != 1 || fd.lastReason != "vendor delay" {
		t.Fatalf("store saw recalcs=%d reason=%q", fd.recalcs, fd.lastReason)
	}
}
