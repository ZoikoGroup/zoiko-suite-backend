package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/expense-claim-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine and uniqueness semantics, not
// canned responses.
type stubStore struct {
	claims       map[string]*domain.ExpenseClaim
	lines        map[string][]*domain.ExpenseLine // by claim_id
	lineByID     map[string]*domain.ExpenseLine
	receiptOwner map[string]struct{ claimID, lineID string } // by document_id
	events       map[string][]domain.ExpenseClaimEvent
}

func newStubStore() *stubStore {
	return &stubStore{
		claims:       map[string]*domain.ExpenseClaim{},
		lines:        map[string][]*domain.ExpenseLine{},
		lineByID:     map[string]*domain.ExpenseLine{},
		receiptOwner: map[string]struct{ claimID, lineID string }{},
		events:       map[string][]domain.ExpenseClaimEvent{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) recordEvent(c *domain.ExpenseClaim, eventType, detail, actor string) {
	s.events[c.ClaimID] = append(s.events[c.ClaimID], domain.ExpenseClaimEvent{
		EventID: uuid.New().String(), TenantID: c.TenantID, ClaimID: c.ClaimID,
		EventType: eventType, Detail: detail, ActorPrincipalID: actor, CreatedAt: time.Now().UTC(),
	})
}

func (s *stubStore) CreateClaim(_ context.Context, tenantID string, req domain.CreateExpenseClaimRequest, principalID string) (*domain.ExpenseClaim, error) {
	now := time.Now().UTC()
	c := &domain.ExpenseClaim{
		ClaimID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID,
		ClaimantPrincipalID: req.ClaimantPrincipalID, Currency: req.Currency, BusinessPurpose: req.BusinessPurpose,
		ProjectCostCenter: req.ProjectCostCenter, PaymentPreferenceRef: req.PaymentPreferenceRef,
		Status: domain.StatusDraft, PolicyAssessmentResult: domain.PolicyNotAssessed,
		CreatedAt: now, UpdatedAt: now,
	}
	s.claims[c.ClaimID] = c
	s.recordEvent(c, domain.EventClaimCreated, "", principalID)
	return c, nil
}

func (s *stubStore) FindClaim(_ context.Context, claimID string) (*domain.ExpenseClaim, error) {
	c, ok := s.claims[claimID]
	if !ok {
		return nil, domain.ErrClaimNotFound
	}
	return c, nil
}

func (s *stubStore) AddExpenseLine(_ context.Context, claimID string, req domain.AddExpenseLineRequest, _ string) (*domain.ExpenseLine, error) {
	c, ok := s.claims[claimID]
	if !ok {
		return nil, domain.ErrClaimNotFound
	}
	if !domain.CanAddLine(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	if req.ReceiptDocumentID != "" {
		if _, taken := s.receiptOwner[req.ReceiptDocumentID]; taken {
			return nil, domain.ErrDuplicateReceipt
		}
	}
	l := &domain.ExpenseLine{
		LineID: uuid.New().String(), TenantID: c.TenantID, ClaimID: claimID, Merchant: req.Merchant,
		ExpenseDate: req.ExpenseDate, Amount: req.Amount, Currency: req.Currency, Category: req.Category,
		ProjectCostCenter: req.ProjectCostCenter, ReceiptDocumentID: req.ReceiptDocumentID,
		ClaimTaxRecovery: req.ClaimTaxRecovery, Jurisdiction: req.Jurisdiction, TaxCategory: req.TaxCategory,
		CreatedAt: time.Now().UTC(),
	}
	s.lines[claimID] = append(s.lines[claimID], l)
	s.lineByID[l.LineID] = l
	if req.ReceiptDocumentID != "" {
		s.receiptOwner[req.ReceiptDocumentID] = struct{ claimID, lineID string }{claimID, l.LineID}
	}
	return l, nil
}

func (s *stubStore) ListLines(_ context.Context, claimID string) ([]domain.ExpenseLine, error) {
	var out []domain.ExpenseLine
	for _, l := range s.lines[claimID] {
		out = append(out, *l)
	}
	return out, nil
}

func (s *stubStore) SetLineTaxDetermination(_ context.Context, lineID, determinationID string, taxableAmount, calculatedTaxAmount float64) error {
	l, ok := s.lineByID[lineID]
	if !ok {
		return domain.ErrLineNotFound
	}
	l.TaxDeterminationID = determinationID
	l.TaxableAmount = taxableAmount
	l.CalculatedTaxAmount = calculatedTaxAmount
	return nil
}

func (s *stubStore) IsReceiptInUse(_ context.Context, documentID string) (bool, string, string, error) {
	owner, ok := s.receiptOwner[documentID]
	if !ok {
		return false, "", "", nil
	}
	return true, owner.claimID, owner.lineID, nil
}

func (s *stubStore) SubmitClaim(_ context.Context, claimID string, policyResult domain.PolicyAssessmentResult, policyVersionID string, principalID string) (*domain.ExpenseClaim, error) {
	c, ok := s.claims[claimID]
	if !ok || !domain.CanSubmit(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	c.Status = domain.StatusPendingApproval
	c.PolicyAssessmentResult = policyResult
	c.PolicyVersionID = policyVersionID
	c.UpdatedAt = time.Now().UTC()
	s.recordEvent(c, domain.EventClaimSubmitted, string(policyResult), principalID)
	return c, nil
}

func (s *stubStore) ApproveClaim(_ context.Context, claimID string, principalID string) (*domain.ExpenseClaim, error) {
	c, ok := s.claims[claimID]
	if !ok || !domain.CanDecide(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	c.Status = domain.StatusReimbursable
	c.ApprovedByPrincipalID = &principalID
	c.ApprovedAt = &now
	c.UpdatedAt = now
	s.recordEvent(c, domain.EventClaimApproved, "", principalID)
	return c, nil
}

func (s *stubStore) RejectClaim(_ context.Context, claimID string, req domain.RejectClaimRequest, principalID string) (*domain.ExpenseClaim, error) {
	c, ok := s.claims[claimID]
	if !ok || !domain.CanDecide(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	c.Status = domain.StatusRejected
	c.RejectionReason = req.Reason
	c.UpdatedAt = time.Now().UTC()
	s.recordEvent(c, domain.EventClaimRejected, req.Reason, principalID)
	return c, nil
}

func (s *stubStore) ReturnClaim(_ context.Context, claimID string, req domain.ReturnClaimRequest, principalID string) (*domain.ExpenseClaim, error) {
	c, ok := s.claims[claimID]
	if !ok || !domain.CanDecide(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	c.Status = domain.StatusReturned
	c.ReturnReason = req.Reason
	c.UpdatedAt = time.Now().UTC()
	s.recordEvent(c, domain.EventClaimReturned, req.Reason, principalID)
	return c, nil
}

func (s *stubStore) CancelClaim(_ context.Context, claimID string, req domain.CancelClaimRequest, principalID string) (*domain.ExpenseClaim, error) {
	c, ok := s.claims[claimID]
	if !ok || !domain.CanCancel(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	c.Status = domain.StatusCancelled
	c.UpdatedAt = time.Now().UTC()
	s.recordEvent(c, domain.EventClaimCancelled, req.Reason, principalID)
	return c, nil
}

func (s *stubStore) RecordPolicyException(_ context.Context, claimID string, req domain.RecordPolicyExceptionRequest, principalID string) (*domain.ExpenseClaim, error) {
	c, ok := s.claims[claimID]
	if !ok || !domain.CanDecide(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	c.HasPolicyException = true
	c.PolicyExceptionReason = req.Reason
	c.UpdatedAt = time.Now().UTC()
	s.recordEvent(c, domain.EventPolicyExceptionRecorded, req.Reason, principalID)
	return c, nil
}

func (s *stubStore) ListClaimEvents(_ context.Context, claimID string) ([]domain.ExpenseClaimEvent, error) {
	return s.events[claimID], nil
}
