package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payment-proposal-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine and uniqueness semantics.
type stubStore struct {
	proposals     map[string]*domain.PaymentProposal
	items         map[string]*domain.ProposalItem
	activePayable map[string]string // "source|payableID" -> itemID, only while active
	events        map[string][]domain.ProposalEvent
}

func newStubStore() *stubStore {
	return &stubStore{
		proposals:     map[string]*domain.PaymentProposal{},
		items:         map[string]*domain.ProposalItem{},
		activePayable: map[string]string{},
		events:        map[string][]domain.ProposalEvent{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func payableKey(source domain.PayableSource, id string) string { return string(source) + "|" + id }

func (s *stubStore) recordEvent(p *domain.PaymentProposal, eventType, detail, actor string) {
	s.events[p.ProposalID] = append(s.events[p.ProposalID], domain.ProposalEvent{
		EventID: uuid.New().String(), TenantID: p.TenantID, ProposalID: p.ProposalID,
		EventType: eventType, Detail: detail, ActorPrincipalID: actor, CreatedAt: time.Now().UTC(),
	})
}

func (s *stubStore) CreateProposal(_ context.Context, tenantID string, req domain.CreateProposalRequest, principalID string) (*domain.PaymentProposal, error) {
	now := time.Now().UTC()
	p := &domain.PaymentProposal{
		ProposalID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID,
		PayingBankAccountRef: req.PayingBankAccountRef, Currency: req.Currency, PaymentDate: req.PaymentDate,
		PaymentMethod: req.PaymentMethod, Status: domain.StatusDraft, CreatedByPrincipalID: principalID,
		CreatedAt: now, UpdatedAt: now,
	}
	s.proposals[p.ProposalID] = p
	s.recordEvent(p, domain.EventProposalCreated, "", principalID)
	return p, nil
}

func (s *stubStore) FindProposal(_ context.Context, proposalID string) (*domain.PaymentProposal, error) {
	p, ok := s.proposals[proposalID]
	if !ok {
		return nil, domain.ErrProposalNotFound
	}
	return p, nil
}

func (s *stubStore) AddItem(_ context.Context, item domain.ProposalItem) (*domain.ProposalItem, error) {
	p, ok := s.proposals[item.ProposalID]
	if !ok {
		return nil, domain.ErrProposalNotFound
	}
	if !domain.CanMutateItems(p.Status) {
		return nil, domain.ErrInvalidTransition
	}
	key := payableKey(item.PayableSource, item.PayableID)
	if _, taken := s.activePayable[key]; taken {
		return nil, domain.ErrPayableAlreadyInProposal
	}
	item.ItemID = uuid.New().String()
	item.TenantID = p.TenantID
	item.IsActive = true
	item.CreatedAt = time.Now().UTC()
	s.items[item.ItemID] = &item
	s.activePayable[key] = item.ItemID
	return &item, nil
}

func (s *stubStore) FindItem(_ context.Context, itemID string) (*domain.ProposalItem, error) {
	i, ok := s.items[itemID]
	if !ok {
		return nil, domain.ErrItemNotFound
	}
	return i, nil
}

func (s *stubStore) RemoveItem(_ context.Context, itemID string) error {
	i, ok := s.items[itemID]
	if !ok || !i.IsActive {
		return domain.ErrItemNotFound
	}
	i.IsActive = false
	delete(s.activePayable, payableKey(i.PayableSource, i.PayableID))
	return nil
}

func (s *stubStore) ListItems(_ context.Context, proposalID string) ([]domain.ProposalItem, error) {
	var out []domain.ProposalItem
	for _, i := range s.items {
		if i.ProposalID == proposalID && i.IsActive {
			out = append(out, *i)
		}
	}
	return out, nil
}

func (s *stubStore) RecalculateProposal(_ context.Context, proposalID string, gross, withholding, net float64, principalID string) (*domain.PaymentProposal, error) {
	p, ok := s.proposals[proposalID]
	if !ok || !domain.CanRecalculate(p.Status) {
		return nil, domain.ErrInvalidTransition
	}
	p.Status = domain.StatusCalculated
	p.GrossAmount, p.WithholdingAmount, p.NetAmount = gross, withholding, net
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventProposalCalculated, "", principalID)
	return p, nil
}

func (s *stubStore) SubmitForReview(_ context.Context, proposalID string, principalID string) (*domain.PaymentProposal, error) {
	p, ok := s.proposals[proposalID]
	if !ok || !domain.CanSubmitForReview(p.Status) {
		return nil, domain.ErrInvalidTransition
	}
	p.Status = domain.StatusReview
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventProposalChanged, "submitted for review", principalID)
	return p, nil
}

func (s *stubStore) FreezeProposal(_ context.Context, proposalID string, principalID string) (*domain.PaymentProposal, error) {
	p, ok := s.proposals[proposalID]
	if !ok || !domain.CanFreeze(p.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	p.Status = domain.StatusFrozen
	p.FrozenByPrincipalID = &principalID
	p.FrozenAt = &now
	p.UpdatedAt = now
	s.recordEvent(p, domain.EventProposalFrozen, "", principalID)
	return p, nil
}

func (s *stubStore) CancelProposal(_ context.Context, proposalID string, req domain.CancelProposalRequest, principalID string) (*domain.PaymentProposal, error) {
	p, ok := s.proposals[proposalID]
	if !ok || !domain.CanCancel(p.Status) {
		return nil, domain.ErrInvalidTransition
	}
	p.Status = domain.StatusCancelled
	p.UpdatedAt = time.Now().UTC()
	for _, i := range s.items {
		if i.ProposalID == proposalID && i.IsActive {
			i.IsActive = false
			delete(s.activePayable, payableKey(i.PayableSource, i.PayableID))
		}
	}
	s.recordEvent(p, domain.EventProposalCancelled, req.Reason, principalID)
	return p, nil
}

func (s *stubStore) ListEvents(_ context.Context, proposalID string) ([]domain.ProposalEvent, error) {
	return s.events[proposalID], nil
}
