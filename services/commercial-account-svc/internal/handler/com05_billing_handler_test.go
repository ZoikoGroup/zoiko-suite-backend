package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/store"
)

type billingStub struct {
	candidate *domain.InvoiceCandidate
	invoice   *domain.PlatformCommercialInvoice
	err       error
}

func (s *billingStub) OpenBillingAccount(_ context.Context, b *domain.BillingAccount, _ domain.IdempotencyClaim) (*domain.BillingAccount, error) {
	return b, s.err
}
func (s *billingStub) GetBillingAccount(_ context.Context, _ string) (*domain.BillingAccount, error) {
	return nil, s.err
}
func (s *billingStub) GenerateInvoiceCandidate(_ context.Context, _ domain.GenerateInvoiceCandidateRequest, _ domain.IdempotencyClaim) (*domain.InvoiceCandidate, error) {
	return s.candidate, s.err
}
func (s *billingStub) ApproveInvoiceCandidate(_ context.Context, _, _ string, _ time.Time, _ domain.IdempotencyClaim) (*domain.InvoiceCandidate, error) {
	return s.candidate, s.err
}
func (s *billingStub) IssueInvoice(_ context.Context, _, _ string, _ time.Time, _ domain.IdempotencyClaim) (*domain.PlatformCommercialInvoice, error) {
	return s.invoice, s.err
}
func (s *billingStub) GetInvoiceCandidate(_ context.Context, _ string) (*domain.InvoiceCandidate, error) {
	if s.candidate == nil {
		return nil, domain.ErrInvoiceCandidateNotFound
	}
	return s.candidate, nil
}
func (s *billingStub) GetInvoice(_ context.Context, _ string) (*domain.PlatformCommercialInvoice, error) {
	return s.invoice, s.err
}
func (s *billingStub) GetInvoiceBasis(_ context.Context, _ string) (*domain.InvoiceCandidate, error) {
	return s.candidate, s.err
}

var _ store.BillingStore = (*billingStub)(nil)

func TestCandidateAction_GeneratorSelfApproval_Blocked(t *testing.T) {
	candID := domain.PrefixInvoiceCandidate + uuid.NewString()
	generatorPrincipal := "finance-maker-01"
	cand := &domain.InvoiceCandidate{
		CandidateID:          candID,
		CreatedByPrincipalID: generatorPrincipal,
		Status:               domain.CandidateDraft,
	}

	st := &billingStub{candidate: cand}
	logger, _ := zap.NewDevelopment()
	bh := NewBillingHandler(st, &stubAuthz{}, logger)
	r := chi.NewRouter()
	RegisterBillingRoutes(r, bh)

	// Caller is the generator (same principal)
	req := httptest.NewRequest(http.MethodPost, "/v1/commercial/invoice-candidates/"+candID+":approve", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", generatorPrincipal)
	req.Header.Set("Idempotency-Key", "idemp-approve-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for invoice candidate self-approval, got HTTP %d: %s", w.Code, w.Body.String())
	}
}
