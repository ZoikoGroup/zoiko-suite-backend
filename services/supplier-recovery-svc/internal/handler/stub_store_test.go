package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/supplier-recovery-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine and idempotency semantics.
type stubStore struct {
	cases        map[string]*domain.SupplierRecoveryCase
	applications map[string][]domain.RecoveryApplication
	commitments  map[string][]domain.RecoveryCommitment
}

func newStubStore() *stubStore {
	return &stubStore{
		cases:        map[string]*domain.SupplierRecoveryCase{},
		applications: map[string][]domain.RecoveryApplication{},
		commitments:  map[string][]domain.RecoveryCommitment{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) CreateCase(_ context.Context, tenantID string, req domain.CreateCaseRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	now := time.Now().UTC()
	c := &domain.SupplierRecoveryCase{
		CaseID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID,
		SupplierRef: req.SupplierRef, RecoveryBasis: req.RecoveryBasis, SourcePayableID: req.SourcePayableID,
		TotalAmount: req.TotalAmount, Currency: req.Currency, RecoveryReason: req.RecoveryReason,
		Status: domain.StatusOpen, CreatedByPrincipalID: principalID, CreatedAt: now, UpdatedAt: now,
	}
	s.cases[c.CaseID] = c
	return c, nil
}

func (s *stubStore) FindCase(_ context.Context, caseID string) (*domain.SupplierRecoveryCase, error) {
	c, ok := s.cases[caseID]
	if !ok {
		return nil, domain.ErrCaseNotFound
	}
	return c, nil
}

func (s *stubStore) ListOpenCases(_ context.Context, legalEntityID string) ([]domain.SupplierRecoveryCase, error) {
	var out []domain.SupplierRecoveryCase
	for _, c := range s.cases {
		if c.LegalEntityID == legalEntityID && c.Status != domain.StatusClosed && c.Status != domain.StatusWrittenOff {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (s *stubStore) ApproveRecoveryPlan(_ context.Context, caseID, principalID string) (*domain.SupplierRecoveryCase, error) {
	c, ok := s.cases[caseID]
	if !ok || !domain.CanApprove(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	c.Status = domain.StatusInRecovery
	c.ApprovedByPrincipalID = principalID
	c.UpdatedAt = time.Now().UTC()
	return c, nil
}

func (s *stubStore) RecordCommitment(_ context.Context, caseID string, req domain.RecordCommitmentRequest, principalID string) (*domain.RecoveryCommitment, error) {
	c, ok := s.cases[caseID]
	if !ok {
		return nil, domain.ErrCaseNotFound
	}
	commitment := domain.RecoveryCommitment{
		CommitmentID: uuid.New().String(), TenantID: c.TenantID, CaseID: caseID,
		Detail: req.Detail, ExpectedMethod: req.ExpectedMethod, ActorPrincipalID: principalID, CreatedAt: time.Now().UTC(),
	}
	s.commitments[caseID] = append(s.commitments[caseID], commitment)
	return &commitment, nil
}

func (s *stubStore) ApplyRecovery(_ context.Context, caseID, appType string, amount float64, idempotencyRef, detail, principalID string) (*domain.SupplierRecoveryCase, bool, error) {
	c, ok := s.cases[caseID]
	if !ok {
		return nil, false, domain.ErrCaseNotFound
	}
	// The idempotency check must run BEFORE the state-transition guard —
	// see internal/store's own applyRecovery doc for why.
	for _, a := range s.applications[caseID] {
		if a.ApplicationType == appType && a.IdempotencyRef == idempotencyRef {
			return c, false, nil
		}
	}
	if !domain.CanApplyRecovery(c.Status) {
		return nil, false, domain.ErrInvalidTransition
	}
	newRecovered := c.RecoveredAmount + amount
	if newRecovered > c.TotalAmount {
		return nil, false, domain.ErrRecoveryExceedsOutstanding
	}
	s.applications[caseID] = append(s.applications[caseID], domain.RecoveryApplication{
		ApplicationID: uuid.New().String(), TenantID: c.TenantID, CaseID: caseID, ApplicationType: appType,
		Amount: amount, IdempotencyRef: idempotencyRef, Detail: detail, ActorPrincipalID: principalID, CreatedAt: time.Now().UTC(),
	})
	c.RecoveredAmount = newRecovered
	if newRecovered >= c.TotalAmount {
		c.Status = domain.StatusRecovered
	} else {
		c.Status = domain.StatusPartiallyRecovered
	}
	c.UpdatedAt = time.Now().UTC()
	return c, true, nil
}

func (s *stubStore) EscalateCase(_ context.Context, caseID string, req domain.EscalateRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	c, ok := s.cases[caseID]
	if !ok || !domain.CanEscalate(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	c.Status = domain.StatusEscalated
	c.EscalationReason = req.Reason
	c.UpdatedAt = time.Now().UTC()
	return c, nil
}

func (s *stubStore) WriteOffCase(_ context.Context, caseID string, req domain.WriteOffRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	c, ok := s.cases[caseID]
	if !ok || !domain.CanWriteOff(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	c.Status = domain.StatusWrittenOff
	c.WriteOffReason = req.Reason
	c.UpdatedAt = time.Now().UTC()
	return c, nil
}

func (s *stubStore) CloseCase(_ context.Context, caseID string, req domain.CloseCaseRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	c, ok := s.cases[caseID]
	if !ok || !domain.CanClose(c.Status) {
		return nil, domain.ErrInvalidTransition
	}
	c.Status = domain.StatusClosed
	c.CloseNote = req.Note
	c.UpdatedAt = time.Now().UTC()
	return c, nil
}

func (s *stubStore) ListApplications(_ context.Context, caseID string) ([]domain.RecoveryApplication, error) {
	return s.applications[caseID], nil
}

func (s *stubStore) ListCommitments(_ context.Context, caseID string) ([]domain.RecoveryCommitment, error) {
	return s.commitments[caseID], nil
}
