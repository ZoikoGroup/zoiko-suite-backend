package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payable-open-item-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine and uniqueness semantics.
type stubStore struct {
	payables       map[string]*domain.PayableOpenItem
	sourceIndex    map[string]string          // "sourceType:sourceReference" -> payable_id
	paymentApplied map[string]map[string]bool // payable_id -> provider_payment_ref -> applied
	applications   map[string][]domain.SettlementApplication
}

func newStubStore() *stubStore {
	return &stubStore{
		payables:       map[string]*domain.PayableOpenItem{},
		sourceIndex:    map[string]string{},
		paymentApplied: map[string]map[string]bool{},
		applications:   map[string][]domain.SettlementApplication{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sourceKey(sourceType domain.SourceType, sourceReference string) string {
	return string(sourceType) + ":" + sourceReference
}

func (s *stubStore) CreatePayable(_ context.Context, tenantID string, req domain.CreatePayableRequest, principalID string) (*domain.PayableOpenItem, error) {
	key := sourceKey(req.SourceType, req.SourceReference)
	if _, taken := s.sourceIndex[key]; taken {
		return nil, domain.ErrDuplicateSource
	}
	now := time.Now().UTC()
	p := &domain.PayableOpenItem{
		PayableID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID,
		SourceType: req.SourceType, SourceReference: req.SourceReference, PayeeRef: req.PayeeRef,
		OriginalAmount: req.OriginalAmount, ResidualAmount: req.OriginalAmount, Currency: req.Currency,
		DueDate: req.DueDate, Status: domain.StatusOpen, CreatedByPrincipalID: principalID,
		CreatedAt: now, UpdatedAt: now,
	}
	s.payables[p.PayableID] = p
	s.sourceIndex[key] = p.PayableID
	s.paymentApplied[p.PayableID] = map[string]bool{}
	return p, nil
}

func (s *stubStore) FindPayable(_ context.Context, payableID string) (*domain.PayableOpenItem, error) {
	p, ok := s.payables[payableID]
	if !ok {
		return nil, domain.ErrPayableNotFound
	}
	return p, nil
}

func (s *stubStore) FindBySource(_ context.Context, sourceType domain.SourceType, sourceReference string) (*domain.PayableOpenItem, error) {
	id, ok := s.sourceIndex[sourceKey(sourceType, sourceReference)]
	if !ok {
		return nil, domain.ErrPayableNotFound
	}
	return s.payables[id], nil
}

func (s *stubStore) ListOpenPayables(_ context.Context, legalEntityID string) ([]domain.PayableOpenItem, error) {
	var out []domain.PayableOpenItem
	for _, p := range s.payables {
		if p.LegalEntityID == legalEntityID && p.ClosedAt == nil && (p.Status == domain.StatusOpen || p.Status == domain.StatusPartiallySettled) {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (s *stubStore) GetSupplierBalance(_ context.Context, payeeRef string) (float64, error) {
	var total float64
	for _, p := range s.payables {
		if p.PayeeRef == payeeRef && p.ClosedAt == nil && (p.Status == domain.StatusOpen || p.Status == domain.StatusPartiallySettled) {
			total += p.ResidualAmount
		}
	}
	return total, nil
}

func (s *stubStore) applyDelta(payableID, appType string, delta float64, idempotencyRef, detail, principalID string, allowNegative bool) (*domain.PayableOpenItem, bool, error) {
	p, ok := s.payables[payableID]
	if !ok {
		return nil, false, domain.ErrPayableNotFound
	}
	// The idempotency check must run BEFORE the state-transition guard: a
	// replayed payment that fully settled the payable on its first
	// application would otherwise be rejected as "already settled" on
	// replay instead of returning the idempotent no-op it actually is.
	if appType == "PAYMENT" && idempotencyRef != "" {
		if s.paymentApplied[payableID][idempotencyRef] {
			return p, false, nil
		}
	}
	if p.Status != domain.StatusOpen && p.Status != domain.StatusPartiallySettled {
		return nil, false, domain.ErrInvalidTransition
	}
	newResidual := p.ResidualAmount + delta
	if newResidual < 0 && !allowNegative {
		return nil, false, domain.ErrResidualWouldGoNegative
	}
	if appType == "PAYMENT" && idempotencyRef != "" {
		s.paymentApplied[payableID][idempotencyRef] = true
	}
	p.ResidualAmount = newResidual
	if newResidual <= 0 {
		p.Status = domain.StatusSettled
	} else {
		p.Status = domain.StatusPartiallySettled
	}
	p.UpdatedAt = time.Now().UTC()
	s.applications[payableID] = append(s.applications[payableID], domain.SettlementApplication{
		ApplicationID: uuid.New().String(), TenantID: p.TenantID, PayableID: payableID, ApplicationType: appType,
		Amount: delta, IdempotencyRef: idempotencyRef, Detail: detail, ActorPrincipalID: principalID, CreatedAt: time.Now().UTC(),
	})
	return p, true, nil
}

func (s *stubStore) ApplySupplierCredit(_ context.Context, payableID string, req domain.ApplySupplierCreditRequest, principalID string) (*domain.PayableOpenItem, error) {
	p, _, err := s.applyDelta(payableID, "SUPPLIER_CREDIT", -req.Amount, req.CreditRef, req.Reason, principalID, true)
	return p, err
}

func (s *stubStore) ApplyConfirmedPayment(_ context.Context, payableID string, req domain.ApplyConfirmedPaymentRequest, principalID string) (*domain.PayableOpenItem, bool, error) {
	return s.applyDelta(payableID, "PAYMENT", -req.Amount, req.ProviderPaymentRef, "", principalID, false)
}

func (s *stubStore) ApplyRecovery(_ context.Context, payableID string, req domain.ApplyRecoveryRequest, principalID string) (*domain.PayableOpenItem, error) {
	p, _, err := s.applyDelta(payableID, "RECOVERY", -req.Amount, req.RecoveryRef, req.Reason, principalID, false)
	return p, err
}

func (s *stubStore) PlaceHold(_ context.Context, payableID string, req domain.PlaceHoldRequest, principalID string) (*domain.PayableOpenItem, error) {
	p, ok := s.payables[payableID]
	if !ok || p.ClosedAt != nil {
		return nil, domain.ErrPayableNotFound
	}
	p.IsHeld = true
	p.HoldReason = req.Reason
	p.UpdatedAt = time.Now().UTC()
	return p, nil
}

func (s *stubStore) ReleaseHold(_ context.Context, payableID, principalID string) (*domain.PayableOpenItem, error) {
	p, ok := s.payables[payableID]
	if !ok || p.ClosedAt != nil {
		return nil, domain.ErrPayableNotFound
	}
	p.IsHeld = false
	p.HoldReason = ""
	p.UpdatedAt = time.Now().UTC()
	return p, nil
}

func (s *stubStore) OpenDispute(_ context.Context, payableID string, req domain.OpenDisputeRequest, principalID string) (*domain.PayableOpenItem, error) {
	p, ok := s.payables[payableID]
	if !ok || p.ClosedAt != nil {
		return nil, domain.ErrPayableNotFound
	}
	p.IsDisputed = true
	p.DisputeReason = req.Reason
	now := time.Now().UTC()
	p.DisputeOpenedAt = &now
	p.UpdatedAt = now
	return p, nil
}

func (s *stubStore) ResolveDispute(_ context.Context, payableID string, req domain.ResolveDisputeRequest, principalID string) (*domain.PayableOpenItem, error) {
	p, ok := s.payables[payableID]
	if !ok || p.ClosedAt != nil || !p.IsDisputed {
		return nil, domain.ErrInvalidTransition
	}
	p.IsDisputed = false
	p.DisputeReason = "resolved: " + req.Resolution
	p.DisputeOpenedAt = nil
	p.UpdatedAt = time.Now().UTC()
	return p, nil
}

func (s *stubStore) ClosePayable(_ context.Context, payableID, principalID string) (*domain.PayableOpenItem, error) {
	p, ok := s.payables[payableID]
	if !ok {
		return nil, domain.ErrPayableNotFound
	}
	if !domain.CanClose(p.Status, p.IsHeld, p.IsDisputed) {
		if p.IsHeld || p.IsDisputed {
			return nil, domain.ErrPayableHeldOrDisputed
		}
		return nil, domain.ErrPayableNotFullySettled
	}
	now := time.Now().UTC()
	p.ClosedAt = &now
	p.UpdatedAt = now
	return p, nil
}

func (s *stubStore) ListApplications(_ context.Context, payableID string) ([]domain.SettlementApplication, error) {
	return s.applications[payableID], nil
}
