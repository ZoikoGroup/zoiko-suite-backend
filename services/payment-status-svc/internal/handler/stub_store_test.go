package handler_test

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/middleware"
)

type stubIdempotencyClaim struct {
	command     string
	paymentID   string
	requestHash string
}

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine, ordering, and idempotency
// semantics.
type stubStore struct {
	payments         map[string]*domain.PaymentExecutionState
	appliedEvents    map[string]map[string]bool // payment_id -> provider_event_ref -> applied
	events           map[string][]domain.StatusEvent
	finalityEvidence map[string][]domain.FinalityEvidence
	idempotency      map[string]stubIdempotencyClaim // tenant + "\x00" + key
}

func newStubStore() *stubStore {
	return &stubStore{
		payments:         map[string]*domain.PaymentExecutionState{},
		appliedEvents:    map[string]map[string]bool{},
		events:           map[string][]domain.StatusEvent{},
		finalityEvidence: map[string][]domain.FinalityEvidence{},
		idempotency:      map[string]stubIdempotencyClaim{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) recordEvent(p *domain.PaymentExecutionState, eventType, from, to, ref, detail, actor string) {
	s.events[p.PaymentID] = append(s.events[p.PaymentID], domain.StatusEvent{
		EventID: uuid.New().String(), TenantID: p.TenantID, PaymentID: p.PaymentID,
		EventType: eventType, FromStatus: from, ToStatus: to, ProviderEventRef: ref, Detail: detail,
		ActorPrincipalID: actor, CreatedAt: time.Now().UTC(),
	})
}

func (s *stubStore) RecordPaymentStatus(_ context.Context, tenantID string, req domain.RecordPaymentStatusRequest, principalID, _ string) (*domain.PaymentExecutionState, error) {
	now := time.Now().UTC()
	p := &domain.PaymentExecutionState{
		PaymentID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID,
		ProviderRequestID: req.ProviderRequestID, SourceReference: req.SourceReference, ProviderKey: req.ProviderKey,
		Status: domain.StatusPrepared, CreatedByPrincipalID: principalID, CreatedAt: now, UpdatedAt: now,
	}
	s.payments[p.PaymentID] = p
	s.appliedEvents[p.PaymentID] = map[string]bool{}
	return p, nil
}

func (s *stubStore) FindPayment(_ context.Context, paymentID string) (*domain.PaymentExecutionState, error) {
	p, ok := s.payments[paymentID]
	if !ok {
		return nil, domain.ErrPaymentNotFound
	}
	return p, nil
}

func (s *stubStore) ApplyCallbackStatus(_ context.Context, paymentID string, payload domain.ProviderCallbackPayload, eventType, finalitySource, actorPrincipalID, _ string, _ bool) (*domain.PaymentExecutionState, bool, error) {
	p, ok := s.payments[paymentID]
	if !ok {
		return nil, false, domain.ErrPaymentNotFound
	}
	if p.HasOpenConflict {
		return p, false, domain.ErrOpenConflictExists
	}
	if payload.ProviderEventRef != "" {
		if s.appliedEvents[paymentID][payload.ProviderEventRef] {
			return p, false, nil
		}
		s.appliedEvents[paymentID][payload.ProviderEventRef] = true
	}
	if domain.IsFinal(p.Status) && !(p.Status == domain.StatusSettled && payload.ReportedStatus == domain.StatusReturned) {
		s.recordEvent(p, domain.EventCallbackRejectedRegression, string(p.Status), string(payload.ReportedStatus), payload.ProviderEventRef, "regression blocked", actorPrincipalID)
		return p, false, nil
	}
	p.Status = payload.ReportedStatus
	p.FinalitySource = finalitySource
	p.MappingVersion = payload.MappingVersion
	p.UpdatedAt = time.Now().UTC()
	return p, true, nil
}

func (s *stubStore) LinkStatement(_ context.Context, paymentID string, req domain.LinkStatementRequest, principalID, _ string) (*domain.PaymentExecutionState, bool, error) {
	p, ok := s.payments[paymentID]
	if !ok {
		return nil, false, domain.ErrPaymentNotFound
	}
	if p.Status != req.ReportedStatus {
		p.HasOpenConflict = true
		p.ConflictReason = "statement reports " + string(req.ReportedStatus) + " but canonical status is " + string(p.Status)
		s.recordEvent(p, domain.EventPaymentStatusConflictRaised, string(p.Status), string(req.ReportedStatus), req.StatementReference, p.ConflictReason, principalID)
		return p, true, nil
	}
	return p, false, nil
}

func (s *stubStore) ResolveConflict(_ context.Context, paymentID string, req domain.ResolveConflictRequest, principalID, _ string) (*domain.PaymentExecutionState, error) {
	if !domain.ValidResolutionStatus(req.FinalStatus) {
		return nil, domain.ErrInvalidResolution
	}
	p, ok := s.payments[paymentID]
	if !ok || !p.HasOpenConflict {
		return nil, domain.ErrInvalidTransition
	}
	from := p.Status
	p.Status = req.FinalStatus
	p.HasOpenConflict = false
	p.ConflictReason = ""
	p.FinalitySource = "MANUAL_OVERRIDE"
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventPaymentStatusConflictResolved, string(from), string(req.FinalStatus), "", req.Reason, principalID)
	return p, nil
}

func (s *stubStore) RecordReturn(_ context.Context, paymentID string, req domain.RecordReturnRequest, principalID string) (*domain.PaymentExecutionState, error) {
	p, ok := s.payments[paymentID]
	if !ok || !domain.CanReturn(p.Status) {
		return nil, domain.ErrInvalidTransition
	}
	if req.ProviderEventRef != "" {
		if s.appliedEvents[paymentID][req.ProviderEventRef] {
			return nil, domain.ErrInvalidTransition
		}
		s.appliedEvents[paymentID][req.ProviderEventRef] = true
	}
	p.Status = domain.StatusReturned
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventPaymentReturned, "SETTLED", "RETURNED", req.ProviderEventRef, req.Reason, principalID)
	return p, nil
}

func (s *stubStore) CancelPayment(_ context.Context, paymentID string, req domain.CancelRequest, principalID, _ string) (*domain.PaymentExecutionState, error) {
	p, ok := s.payments[paymentID]
	if !ok || !domain.CanCancel(p.Status) {
		return nil, domain.ErrInvalidTransition
	}
	p.Status = domain.StatusCancelled
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventPaymentCancelled, "", "CANCELLED", "", req.Reason, principalID)
	return p, nil
}

type stubFinalitySpec struct {
	command        string
	target         domain.ExecutionStatus
	eventType      string
	finalitySource string
	mappingVersion string
}

func stubFinalityHash(command, paymentID, providerReference, bankReference, reason, effectiveDate, evidenceReference, evidenceHash string) string {
	return strings.Join([]string{command, paymentID, providerReference, bankReference, reason, effectiveDate, evidenceReference, evidenceHash}, "\x1f")
}

func (s *stubStore) recordFinality(tenantID, paymentID string, spec stubFinalitySpec, providerReference, bankReference, reason, effectiveDate, evidenceReference, evidenceHash, principalID, idempotencyKey string) (*domain.PaymentExecutionState, *domain.FinalityEvidence, bool, error) {
	if idempotencyKey == "" {
		return nil, nil, false, domain.ErrIdempotencyKeyRequired
	}
	if len(idempotencyKey) > 200 {
		return nil, nil, false, domain.ErrInvalidFinalityRequest
	}
	key := tenantID + "\x00" + idempotencyKey
	hash := stubFinalityHash(spec.command, paymentID, providerReference, bankReference, reason, effectiveDate, evidenceReference, evidenceHash)
	p, ok := s.payments[paymentID]
	if !ok {
		return nil, nil, false, domain.ErrPaymentNotFound
	}
	if p.CreatedByPrincipalID != "" && p.CreatedByPrincipalID == principalID {
		return nil, nil, false, domain.ErrSelfResolutionForbidden
	}
	if claim, ok := s.idempotency[key]; ok {
		if claim.command != spec.command || claim.paymentID != paymentID || claim.requestHash != hash {
			return nil, nil, false, domain.ErrIdempotencyKeyConflict
		}
		for i := range s.finalityEvidence[paymentID] {
			if s.finalityEvidence[paymentID][i].Command == spec.command {
				ev := s.finalityEvidence[paymentID][i]
				return p, &ev, true, nil
			}
		}
		return nil, nil, false, domain.ErrStoreUnavailable
	}
	if p.HasOpenConflict {
		return nil, nil, false, domain.ErrOpenConflictExists
	}
	if !domain.CanConfirmFinality(p.Status) {
		return nil, nil, false, domain.ErrInvalidTransition
	}
	effectiveAt, err := domain.ParseEffectiveDate(effectiveDate)
	if err != nil {
		return nil, nil, false, err
	}
	from := p.Status
	p.Status = spec.target
	p.FinalitySource = spec.finalitySource
	p.MappingVersion = spec.mappingVersion
	p.UpdatedAt = time.Now().UTC()
	ev := domain.FinalityEvidence{
		EvidenceID: uuid.New().String(), TenantID: p.TenantID, PaymentID: paymentID, Command: spec.command,
		ProviderReference: providerReference, BankReference: bankReference, Reason: reason, EffectiveAt: effectiveAt,
		EvidenceReference: evidenceReference, EvidenceHash: strings.ToLower(evidenceHash),
		MappingVersion: spec.mappingVersion, FinalitySource: spec.finalitySource,
		ActorPrincipalID: principalID, IdempotencyKey: idempotencyKey, CreatedAt: time.Now().UTC(),
	}
	s.finalityEvidence[paymentID] = append(s.finalityEvidence[paymentID], ev)
	s.recordEvent(p, spec.eventType, string(from), string(spec.target), "", "manual finality evidence: "+reason, principalID)
	s.idempotency[key] = stubIdempotencyClaim{command: spec.command, paymentID: paymentID, requestHash: hash}
	return p, &ev, false, nil
}

func (s *stubStore) ConfirmSettlement(ctx context.Context, paymentID string, req domain.ConfirmSettlementRequest, principalID, idempotencyKey, _ string) (*domain.PaymentExecutionState, *domain.FinalityEvidence, bool, error) {
	if err := req.Validate(); err != nil {
		return nil, nil, false, err
	}
	return s.recordFinality(middleware.TenantFromContext(ctx), paymentID, stubFinalitySpec{
		command: domain.CommandConfirmSettlement, target: domain.StatusSettled,
		eventType: domain.EventPaymentSettled, finalitySource: domain.FinalitySourceManualConfirmation,
		mappingVersion: domain.ManualConfirmationMappingVersion,
	}, req.ProviderReference, req.BankReference, req.Reason, req.EffectiveDate, req.EvidenceReference, req.EvidenceHash, principalID, idempotencyKey)
}

func (s *stubStore) RecordRejection(ctx context.Context, paymentID string, req domain.RecordRejectionRequest, principalID, idempotencyKey, _ string) (*domain.PaymentExecutionState, *domain.FinalityEvidence, bool, error) {
	if err := req.Validate(); err != nil {
		return nil, nil, false, err
	}
	return s.recordFinality(middleware.TenantFromContext(ctx), paymentID, stubFinalitySpec{
		command: domain.CommandRecordRejection, target: domain.StatusRejected,
		eventType: domain.EventPaymentRejected, finalitySource: domain.FinalitySourceManualRejection,
		mappingVersion: domain.ManualRejectionMappingVersion,
	}, req.ProviderReference, req.BankReference, req.Reason, req.EffectiveDate, req.EvidenceReference, req.EvidenceHash, principalID, idempotencyKey)
}

func (s *stubStore) ListFinalityEvidence(_ context.Context, paymentID string) ([]domain.FinalityEvidence, error) {
	return s.finalityEvidence[paymentID], nil
}

func (s *stubStore) ListEvents(_ context.Context, paymentID string) ([]domain.StatusEvent, error) {
	return s.events[paymentID], nil
}

func (s *stubStore) ListUnresolved(_ context.Context, legalEntityID string) ([]domain.PaymentExecutionState, error) {
	var out []domain.PaymentExecutionState
	for _, p := range s.payments {
		if p.HasOpenConflict && p.LegalEntityID == legalEntityID {
			out = append(out, *p)
		}
	}
	return out, nil
}
