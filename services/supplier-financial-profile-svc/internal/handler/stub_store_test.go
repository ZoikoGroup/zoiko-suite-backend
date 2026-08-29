package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/supplier-financial-profile-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine semantics, including the
// non-overlapping-payment-terms invariant (in-memory here; a genuine
// Postgres EXCLUDE constraint in the real store), not canned responses.
type stubStore struct {
	profiles       map[string]*domain.SupplierFinancialProfile
	paymentTerms   map[string][]domain.PaymentTermsPeriod // by profile_id
	changeRequests map[string]*domain.HighRiskChangeRequest
	events         map[string][]domain.ProfileChangeEvent // by profile_id
}

func newStubStore() *stubStore {
	return &stubStore{
		profiles:       map[string]*domain.SupplierFinancialProfile{},
		paymentTerms:   map[string][]domain.PaymentTermsPeriod{},
		changeRequests: map[string]*domain.HighRiskChangeRequest{},
		events:         map[string][]domain.ProfileChangeEvent{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) recordEvent(profile *domain.SupplierFinancialProfile, eventType, prior, new, reason, actor string) {
	s.events[profile.ProfileID] = append(s.events[profile.ProfileID], domain.ProfileChangeEvent{
		EventID: uuid.New().String(), TenantID: profile.TenantID, ProfileID: profile.ProfileID,
		EventType: eventType, PriorValue: prior, NewValue: new, Reason: reason, ActorPrincipalID: actor, CreatedAt: time.Now().UTC(),
	})
}

func (s *stubStore) CreateProfile(_ context.Context, tenantID string, req domain.CreateProfileRequest, principalID string) (*domain.SupplierFinancialProfile, error) {
	now := time.Now().UTC()
	p := &domain.SupplierFinancialProfile{
		ProfileID: uuid.New().String(), TenantID: strp(tenantID), LegalEntityID: req.LegalEntityID, SupplierRef: req.SupplierRef,
		Status: domain.StatusDraft, Category: req.Category, InvoiceChannel: req.InvoiceChannel,
		CreatedAt: now, CreatedByPrincipalID: principalID, UpdatedAt: now,
	}
	s.profiles[p.ProfileID] = p
	s.recordEvent(p, domain.EventProfileCreated, "", req.SupplierRef, "", principalID)
	return p, nil
}

func (s *stubStore) FindProfile(_ context.Context, profileID string) (*domain.SupplierFinancialProfile, error) {
	p, ok := s.profiles[profileID]
	if !ok {
		return nil, domain.ErrProfileNotFound
	}
	return p, nil
}

func (s *stubStore) ListProfiles(_ context.Context) ([]domain.SupplierFinancialProfile, error) {
	var out []domain.SupplierFinancialProfile
	for _, p := range s.profiles {
		out = append(out, *p)
	}
	return out, nil
}

func (s *stubStore) ActivateProfile(_ context.Context, profileID, principalID string) (*domain.SupplierFinancialProfile, error) {
	p, ok := s.profiles[profileID]
	if !ok || p.Status != domain.StatusDraft {
		return nil, domain.ErrInvalidTransition
	}
	p.Status = domain.StatusActive
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventProfileActivated, "DRAFT", "ACTIVE", "", principalID)
	return p, nil
}

func (s *stubStore) AmendProfile(_ context.Context, profileID string, req domain.AmendProfileRequest, principalID string) (*domain.SupplierFinancialProfile, error) {
	p, ok := s.profiles[profileID]
	if !ok {
		return nil, domain.ErrProfileNotFound
	}
	if req.Category != nil {
		p.Category = *req.Category
	}
	if req.InvoiceChannel != nil {
		p.InvoiceChannel = *req.InvoiceChannel
	}
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventProfileAmended, "", "", req.Reason, principalID)
	return p, nil
}

func (s *stubStore) PlaceHold(_ context.Context, profileID string, req domain.PlaceHoldRequest, principalID string) (*domain.SupplierFinancialProfile, error) {
	p, ok := s.profiles[profileID]
	if !ok || p.Status != domain.StatusActive {
		return nil, domain.ErrInvalidTransition
	}
	p.Status = domain.StatusOnHold
	p.HoldReason = req.Reason
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventHoldPlaced, "ACTIVE", "ON_HOLD", req.Reason, principalID)
	return p, nil
}

func (s *stubStore) ReleaseHold(_ context.Context, profileID, principalID string) (*domain.SupplierFinancialProfile, error) {
	p, ok := s.profiles[profileID]
	if !ok || p.Status != domain.StatusOnHold {
		return nil, domain.ErrInvalidTransition
	}
	p.Status = domain.StatusActive
	p.HoldReason = ""
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventHoldReleased, "ON_HOLD", "ACTIVE", "", principalID)
	return p, nil
}

func (s *stubStore) RetireProfile(_ context.Context, profileID string, req domain.RetireProfileRequest, principalID string) (*domain.SupplierFinancialProfile, error) {
	p, ok := s.profiles[profileID]
	if !ok || p.Status == domain.StatusRetired {
		return nil, domain.ErrInvalidTransition
	}
	p.Status = domain.StatusRetired
	p.UpdatedAt = time.Now().UTC()
	s.recordEvent(p, domain.EventProfileRetired, "", "RETIRED", req.Reason, principalID)
	return p, nil
}

// overlaps replicates the real Postgres EXCLUDE constraint's semantics:
// [effective_from, effective_to) — nil effective_to means open-ended.
func overlaps(aFrom time.Time, aTo *time.Time, bFrom time.Time, bTo *time.Time) bool {
	aEnd := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if aTo != nil {
		aEnd = *aTo
	}
	bEnd := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if bTo != nil {
		bEnd = *bTo
	}
	return aFrom.Before(bEnd) && bFrom.Before(aEnd)
}

func (s *stubStore) ChangePaymentTerms(_ context.Context, profileID string, req domain.ChangePaymentTermsRequest, principalID string) (*domain.PaymentTermsPeriod, error) {
	p, ok := s.profiles[profileID]
	if !ok {
		return nil, domain.ErrProfileNotFound
	}
	for _, existing := range s.paymentTerms[profileID] {
		if overlaps(req.EffectiveFrom, req.EffectiveTo, existing.EffectiveFrom, existing.EffectiveTo) {
			return nil, domain.ErrOverlappingPaymentTerms
		}
	}
	t := domain.PaymentTermsPeriod{
		PaymentTermsID: uuid.New().String(), TenantID: p.TenantID, ProfileID: profileID, TermsCode: req.TermsCode,
		EffectiveFrom: req.EffectiveFrom, EffectiveTo: req.EffectiveTo, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.paymentTerms[profileID] = append(s.paymentTerms[profileID], t)
	s.recordEvent(p, domain.EventPaymentTermsChanged, "", req.TermsCode, "", principalID)
	return &t, nil
}

func (s *stubStore) ListPaymentTerms(_ context.Context, profileID string) ([]domain.PaymentTermsPeriod, error) {
	return s.paymentTerms[profileID], nil
}

func (s *stubStore) ProposeHighRiskChange(_ context.Context, profileID string, req domain.ProposeHighRiskChangeRequest, principalID string) (*domain.HighRiskChangeRequest, error) {
	p, ok := s.profiles[profileID]
	if !ok {
		return nil, domain.ErrProfileNotFound
	}
	old := ""
	if req.Field == domain.FieldPayeeReference {
		old = p.PayeeReference
	} else {
		old = p.PaymentMethodPreference
	}
	c := &domain.HighRiskChangeRequest{
		ChangeRequestID: uuid.New().String(), TenantID: p.TenantID, ProfileID: profileID, Field: req.Field,
		OldValue: old, NewValue: req.NewValue, Reason: req.Reason, Status: domain.ChangeRequestPending,
		ProposedByPrincipalID: principalID, ProposedAt: time.Now().UTC(),
	}
	s.changeRequests[c.ChangeRequestID] = c
	s.recordEvent(p, domain.EventHighRiskProposed, old, req.NewValue, req.Reason, principalID)
	return c, nil
}

func (s *stubStore) FindChangeRequest(_ context.Context, changeRequestID string) (*domain.HighRiskChangeRequest, error) {
	c, ok := s.changeRequests[changeRequestID]
	if !ok {
		return nil, domain.ErrChangeRequestNotFound
	}
	return c, nil
}

func (s *stubStore) DecideHighRiskChange(_ context.Context, changeRequestID string, req domain.DecideHighRiskChangeRequest, principalID string) (*domain.HighRiskChangeRequest, *domain.SupplierFinancialProfile, error) {
	c, ok := s.changeRequests[changeRequestID]
	if !ok || c.Status != domain.ChangeRequestPending {
		return nil, nil, domain.ErrChangeRequestNotPending
	}
	now := time.Now().UTC()
	c.DecidedByPrincipalID = &principalID
	c.DecidedAt = &now
	c.DecisionReason = req.Reason

	p := s.profiles[c.ProfileID]
	eventType := domain.EventHighRiskRejected
	if req.Approve {
		c.Status = domain.ChangeRequestApproved
		eventType = domain.EventHighRiskApplied
		if c.Field == domain.FieldPayeeReference {
			p.PayeeReference = c.NewValue
		} else {
			p.PaymentMethodPreference = c.NewValue
		}
		p.UpdatedAt = now
	} else {
		c.Status = domain.ChangeRequestRejected
	}
	s.recordEvent(p, eventType, c.OldValue, c.NewValue, req.Reason, principalID)
	return c, p, nil
}

func (s *stubStore) ListChangeEvents(_ context.Context, profileID string) ([]domain.ProfileChangeEvent, error) {
	return s.events[profileID], nil
}
