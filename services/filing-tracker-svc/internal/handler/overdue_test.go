package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/filing-tracker-svc/internal/domain"
)

type alreadyOverdueStore struct{ *mockStore }

func (alreadyOverdueStore) MarkOverdue(_ context.Context, _, _ string) (*domain.FilingRequirement, error) {
	return nil, domain.ErrAlreadyOverdue
}

// A second mark-overdue used to fall through to a 500.
func TestMarkOverdue_AlreadyOverdueIs409(t *testing.T) {
	st := newMockStore()
	st.items["f1"] = &domain.FilingRequirement{FilingID: "f1", LegalEntityID: "le", Status: domain.StatusOverdue}
	logger, _ := zap.NewDevelopment()
	r := chi.NewRouter()
	RegisterRoutes(r, New(alreadyOverdueStore{st}, &mockPublisher{}, &mockAuthz{}, logger))

	req := httptest.NewRequest(http.MethodPost, "/v1/filing-tracker/requirements/f1/mark-overdue", strings.NewReader(`{}`))
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "p1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d - %s", w.Code, w.Body.String())
	}
}
