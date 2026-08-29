package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/goods-service-receipt-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine and aggregation semantics, not
// canned responses.
type stubStore struct {
	receipts   map[string]*domain.GoodsServiceReceipt
	evidence   map[string][]domain.ReceiptEvidence
	reversals  map[string][]domain.ReceiptReversal
	acctEvents map[string][]domain.ReceiptAccountingEvent
}

func newStubStore() *stubStore {
	return &stubStore{
		receipts:   map[string]*domain.GoodsServiceReceipt{},
		evidence:   map[string][]domain.ReceiptEvidence{},
		reversals:  map[string][]domain.ReceiptReversal{},
		acctEvents: map[string][]domain.ReceiptAccountingEvent{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) CreateReceipt(_ context.Context, tenantID string, req domain.CreateReceiptRequest, principalID string) (*domain.GoodsServiceReceipt, error) {
	now := time.Now().UTC()
	r := &domain.GoodsServiceReceipt{
		ReceiptID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID,
		PurchaseOrderID: req.PurchaseOrderID, ReceiptType: req.ReceiptType, Quantity: req.Quantity,
		UnitOfMeasure: req.UnitOfMeasure, Amount: req.Amount, CurrencyCode: req.CurrencyCode,
		ReceiptDate: req.ReceiptDate, Location: req.Location, InspectionResult: req.InspectionResult,
		RequiresIndependentAcceptance: req.RequiresIndependentAcceptance, ToleranceExceptionRef: req.ToleranceExceptionRef,
		Status: domain.StatusDraft, ReceiverPrincipalID: principalID, CreatedByPrincipalID: principalID,
		CreatedAt: now, UpdatedAt: now,
	}
	s.receipts[r.ReceiptID] = r
	return r, nil
}

func (s *stubStore) FindReceipt(_ context.Context, receiptID string) (*domain.GoodsServiceReceipt, error) {
	r, ok := s.receipts[receiptID]
	if !ok {
		return nil, domain.ErrReceiptNotFound
	}
	return r, nil
}

func (s *stubStore) ListReceiptsForPO(_ context.Context, purchaseOrderID string) ([]domain.GoodsServiceReceipt, error) {
	var out []domain.GoodsServiceReceipt
	for _, r := range s.receipts {
		if r.PurchaseOrderID == purchaseOrderID {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *stubStore) AmendReceiptDraft(_ context.Context, receiptID string, req domain.AmendReceiptDraftRequest, _ string) (*domain.GoodsServiceReceipt, error) {
	r, ok := s.receipts[receiptID]
	if !ok || r.Status != domain.StatusDraft {
		return nil, domain.ErrInvalidTransition
	}
	if req.Quantity != nil {
		r.Quantity = *req.Quantity
	}
	if req.UnitOfMeasure != nil {
		r.UnitOfMeasure = *req.UnitOfMeasure
	}
	if req.Amount != nil {
		r.Amount = *req.Amount
	}
	if req.Location != nil {
		r.Location = *req.Location
	}
	if req.InspectionResult != nil {
		r.InspectionResult = *req.InspectionResult
	}
	r.UpdatedAt = time.Now().UTC()
	return r, nil
}

func (s *stubStore) ConfirmReceipt(_ context.Context, receiptID string, principalID string) (*domain.GoodsServiceReceipt, error) {
	r, ok := s.receipts[receiptID]
	if !ok || !domain.CanConfirm(r.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	r.Status = domain.StatusConfirmed
	r.ConfirmedByPrincipalID = &principalID
	r.ConfirmedAt = &now
	r.UpdatedAt = now
	return r, nil
}

func (s *stubStore) RejectReceipt(_ context.Context, receiptID string, req domain.RejectReceiptRequest, _ string) (*domain.GoodsServiceReceipt, error) {
	r, ok := s.receipts[receiptID]
	if !ok || !domain.CanReject(r.Status) {
		return nil, domain.ErrInvalidTransition
	}
	r.Status = domain.StatusRejected
	r.RejectionReason = req.Reason
	r.UpdatedAt = time.Now().UTC()
	return r, nil
}

func (s *stubStore) ReverseReceipt(_ context.Context, receiptID string, req domain.ReverseReceiptRequest, principalID string) (*domain.GoodsServiceReceipt, error) {
	r, ok := s.receipts[receiptID]
	if !ok || !domain.CanReverse(r.Status) {
		return nil, domain.ErrInvalidTransition
	}
	newReversed := r.ReversedAmount + req.ReversedAmount
	if newReversed > r.Amount+0.0001 {
		return nil, domain.ErrOverReversal
	}
	s.reversals[receiptID] = append(s.reversals[receiptID], domain.ReceiptReversal{
		ReversalID: uuid.New().String(), TenantID: r.TenantID, ReceiptID: receiptID,
		ReversedAmount: req.ReversedAmount, Reason: req.Reason, ReversedByPrincipalID: principalID, CreatedAt: time.Now().UTC(),
	})
	r.ReversedAmount = newReversed
	if newReversed >= r.Amount-0.0001 {
		r.Status = domain.StatusFullyReversed
	} else {
		r.Status = domain.StatusPartiallyReversed
	}
	r.UpdatedAt = time.Now().UTC()
	return r, nil
}

func (s *stubStore) RecordServiceAcceptance(_ context.Context, receiptID string, req domain.RecordServiceAcceptanceRequest, principalID string) (*domain.GoodsServiceReceipt, error) {
	r, ok := s.receipts[receiptID]
	if !ok || r.Status != domain.StatusDraft {
		return nil, domain.ErrInvalidTransition
	}
	r.Status = domain.StatusPendingConfirmation
	r.UpdatedAt = time.Now().UTC()
	if req.EvidenceRef != "" {
		s.evidence[receiptID] = append(s.evidence[receiptID], domain.ReceiptEvidence{
			EvidenceID: uuid.New().String(), TenantID: r.TenantID, ReceiptID: receiptID,
			EvidenceRef: req.EvidenceRef, Description: req.Notes, RecordedByPrincipalID: principalID, CreatedAt: time.Now().UTC(),
		})
	}
	return r, nil
}

func (s *stubStore) AttachReceiptEvidence(_ context.Context, receiptID string, req domain.AttachReceiptEvidenceRequest, principalID string) (*domain.ReceiptEvidence, error) {
	r, ok := s.receipts[receiptID]
	if !ok {
		return nil, domain.ErrReceiptNotFound
	}
	e := domain.ReceiptEvidence{
		EvidenceID: uuid.New().String(), TenantID: r.TenantID, ReceiptID: receiptID,
		EvidenceRef: req.EvidenceRef, Description: req.Description, RecordedByPrincipalID: principalID, CreatedAt: time.Now().UTC(),
	}
	s.evidence[receiptID] = append(s.evidence[receiptID], e)
	return &e, nil
}

func (s *stubStore) ListReceiptEvidence(_ context.Context, receiptID string) ([]domain.ReceiptEvidence, error) {
	return s.evidence[receiptID], nil
}

func (s *stubStore) SumNetConfirmedAmountForPO(_ context.Context, purchaseOrderID string) (float64, error) {
	var total float64
	for _, r := range s.receipts {
		if r.PurchaseOrderID != purchaseOrderID {
			continue
		}
		switch r.Status {
		case domain.StatusConfirmed, domain.StatusPartiallyReversed, domain.StatusFullyReversed:
			total += r.Amount - r.ReversedAmount
		}
	}
	return total, nil
}

func (s *stubStore) RecordAccountingEvent(_ context.Context, receiptID string, status domain.AccountingEventStatus, journalID *string, failureReason string) (*domain.ReceiptAccountingEvent, error) {
	e := domain.ReceiptAccountingEvent{
		EventID: uuid.New().String(), ReceiptID: receiptID, Status: status, JournalID: journalID,
		FailureReason: failureReason, CreatedAt: time.Now().UTC(),
	}
	if r, ok := s.receipts[receiptID]; ok {
		e.TenantID = r.TenantID
	}
	s.acctEvents[receiptID] = append(s.acctEvents[receiptID], e)
	return &e, nil
}

func (s *stubStore) GetLatestAccountingEvent(_ context.Context, receiptID string) (*domain.ReceiptAccountingEvent, error) {
	events := s.acctEvents[receiptID]
	if len(events) == 0 {
		return nil, nil
	}
	return &events[len(events)-1], nil
}
