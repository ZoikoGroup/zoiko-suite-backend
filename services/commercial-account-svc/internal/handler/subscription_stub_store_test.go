package handler

import (
	"context"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
)

// Chunk 6 stub-store fields and methods, kept in a separate file from the
// Chunk-5 stubStore for the same reason the Go source is split: they're
// genuinely separate sub-domains that happen to share one process.

type subscriptionStubData struct {
	catalogs      map[string]*domain.PriceCatalog
	plans         map[string]*domain.Plan
	limits        map[string][]domain.EntitlementLimit // keyed by plan_id
	subscriptions map[string]*domain.CommercialSubscription
	evaluations   map[string]*domain.EvaluationProgram           // keyed by subscription_id
	overlays      map[string][]domain.ContractEntitlementOverlay // keyed by commercial_account_id
	usageEvents   map[string]*domain.UsageMeterEvent
	changeReqs    map[string]*domain.SubscriptionChangeRequest
	statusEvents  map[string][]domain.SubscriptionStatusEvent // keyed by subscription_id
	transfers     map[string]*domain.BillingSourceTransfer
}

func newSubscriptionStubData() *subscriptionStubData {
	return &subscriptionStubData{
		catalogs:      make(map[string]*domain.PriceCatalog),
		plans:         make(map[string]*domain.Plan),
		limits:        make(map[string][]domain.EntitlementLimit),
		subscriptions: make(map[string]*domain.CommercialSubscription),
		evaluations:   make(map[string]*domain.EvaluationProgram),
		overlays:      make(map[string][]domain.ContractEntitlementOverlay),
		usageEvents:   make(map[string]*domain.UsageMeterEvent),
		changeReqs:    make(map[string]*domain.SubscriptionChangeRequest),
		statusEvents:  make(map[string][]domain.SubscriptionStatusEvent),
		transfers:     make(map[string]*domain.BillingSourceTransfer),
	}
}

func (s *stubStore) CreatePriceCatalog(_ context.Context, c *domain.PriceCatalog) error {
	for _, existing := range s.sub.catalogs {
		if existing.CatalogCode == c.CatalogCode {
			return domain.ErrConflict
		}
	}
	s.sub.catalogs[c.CatalogVersionID] = c
	return nil
}

func (s *stubStore) GetPriceCatalog(_ context.Context, id string) (*domain.PriceCatalog, error) {
	if c, ok := s.sub.catalogs[id]; ok {
		return c, nil
	}
	return nil, domain.ErrPriceCatalogNotFound
}

func (s *stubStore) CreatePlan(_ context.Context, p *domain.Plan) error {
	s.sub.plans[p.PlanID] = p
	return nil
}

func (s *stubStore) GetPlan(_ context.Context, id string) (*domain.Plan, error) {
	p, ok := s.sub.plans[id]
	if !ok {
		return nil, domain.ErrPlanNotFound
	}
	cp := *p
	cp.Limits = s.sub.limits[id]
	return &cp, nil
}

func (s *stubStore) SetEntitlementLimit(_ context.Context, l *domain.EntitlementLimit) error {
	existing := s.sub.limits[l.PlanID]
	for i, e := range existing {
		if e.MetricType == l.MetricType {
			existing[i] = *l
			s.sub.limits[l.PlanID] = existing
			return nil
		}
	}
	s.sub.limits[l.PlanID] = append(existing, *l)
	return nil
}

func (s *stubStore) ListEntitlementLimitsByPlan(_ context.Context, planID string) ([]domain.EntitlementLimit, error) {
	return s.sub.limits[planID], nil
}

func (s *stubStore) CreateSubscription(_ context.Context, sub *domain.CommercialSubscription) error {
	for _, existing := range s.sub.subscriptions {
		if existing.CommercialAccountID == sub.CommercialAccountID &&
			existing.Status != domain.SubscriptionStatusCanceled &&
			existing.Status != domain.SubscriptionStatusTerminated {
			return domain.ErrActiveSubscriptionExists
		}
	}
	s.sub.subscriptions[sub.SubscriptionID] = sub
	return nil
}

func (s *stubStore) GetSubscription(_ context.Context, id string) (*domain.CommercialSubscription, error) {
	if sub, ok := s.sub.subscriptions[id]; ok {
		return sub, nil
	}
	return nil, domain.ErrSubscriptionNotFound
}

func (s *stubStore) UpdateSubscriptionPlan(_ context.Context, subscriptionID, planID, catalogVersionID string) error {
	sub, ok := s.sub.subscriptions[subscriptionID]
	if !ok {
		return domain.ErrSubscriptionNotFound
	}
	sub.PlanID = planID
	sub.CatalogVersionID = catalogVersionID
	return nil
}

func (s *stubStore) CreateEvaluationProgram(_ context.Context, e *domain.EvaluationProgram) error {
	if _, exists := s.sub.evaluations[e.SubscriptionID]; exists {
		return domain.ErrConflict
	}
	s.sub.evaluations[e.SubscriptionID] = e
	return nil
}

func (s *stubStore) GetEvaluationProgramBySubscription(_ context.Context, subscriptionID string) (*domain.EvaluationProgram, error) {
	if e, ok := s.sub.evaluations[subscriptionID]; ok {
		return e, nil
	}
	return nil, domain.ErrEvaluationProgramNotFound
}

func (s *stubStore) CreateOverlay(_ context.Context, o *domain.ContractEntitlementOverlay) error {
	s.sub.overlays[o.CommercialAccountID] = append(s.sub.overlays[o.CommercialAccountID], *o)
	return nil
}

func (s *stubStore) ResolveEntitlement(_ context.Context, subscriptionID, metricType string) (*domain.ResolvedEntitlement, error) {
	sub, ok := s.sub.subscriptions[subscriptionID]
	if !ok {
		return nil, domain.ErrSubscriptionNotFound
	}
	now := time.Now().UTC()
	for _, o := range s.sub.overlays[sub.CommercialAccountID] {
		if o.MetricType != metricType {
			continue
		}
		if o.EffectiveFrom.After(now) {
			continue
		}
		if o.EffectiveTo != nil && !o.EffectiveTo.After(now) {
			continue
		}
		overlayID := o.OverlayID
		return &domain.ResolvedEntitlement{MetricType: metricType, LimitValue: o.OverrideLimitValue, Source: "OVERLAY", OverlayID: &overlayID}, nil
	}
	for _, l := range s.sub.limits[sub.PlanID] {
		if l.MetricType == metricType {
			return &domain.ResolvedEntitlement{MetricType: metricType, LimitValue: l.LimitValue, Source: "PLAN"}, nil
		}
	}
	return &domain.ResolvedEntitlement{MetricType: metricType, Source: "PLAN"}, nil
}

func (s *stubStore) RecordUsageEvent(_ context.Context, e *domain.UsageMeterEvent) error {
	if _, exists := s.sub.usageEvents[e.UsageEventID]; exists {
		return domain.ErrDuplicateUsageEvent
	}
	s.sub.usageEvents[e.UsageEventID] = e
	return nil
}

func (s *stubStore) CreateChangeRequest(_ context.Context, c *domain.SubscriptionChangeRequest) error {
	s.sub.changeReqs[c.ChangeRequestID] = c
	return nil
}

func (s *stubStore) GetChangeRequest(_ context.Context, id string) (*domain.SubscriptionChangeRequest, error) {
	if c, ok := s.sub.changeReqs[id]; ok {
		return c, nil
	}
	return nil, domain.ErrChangeRequestNotFound
}

func (s *stubStore) ApplyChangeRequest(_ context.Context, changeRequestID string) (*domain.CommercialSubscription, error) {
	c, ok := s.sub.changeReqs[changeRequestID]
	if !ok {
		return nil, domain.ErrChangeRequestNotFound
	}
	if c.Status != "PREVIEWED" {
		return nil, domain.ErrInvalidChangeRequestState
	}
	sub, ok := s.sub.subscriptions[c.SubscriptionID]
	if !ok {
		return nil, domain.ErrSubscriptionNotFound
	}
	plan, ok := s.sub.plans[c.TargetPlanID]
	if !ok {
		return nil, domain.ErrPlanNotFound
	}
	sub.PlanID = plan.PlanID
	sub.CatalogVersionID = plan.CatalogVersionID
	now := time.Now().UTC()
	c.Status = "APPLIED"
	c.AppliedAt = &now
	return sub, nil
}

func (s *stubStore) TransitionSubscriptionStatus(_ context.Context, subscriptionID string, newStatus domain.SubscriptionStatus, allowedPriors []domain.SubscriptionStatus, reason *string, principalID string) error {
	sub, ok := s.sub.subscriptions[subscriptionID]
	if !ok {
		return domain.ErrSubscriptionNotFound
	}
	if sub.Status == newStatus {
		return nil
	}
	allowed := false
	for _, p := range allowedPriors {
		if sub.Status == p {
			allowed = true
			break
		}
	}
	if !allowed {
		return domain.ErrInvalidStatusTransition
	}
	previous := sub.Status
	sub.Status = newStatus
	sub.UpdatedAt = time.Now().UTC()
	s.sub.statusEvents[subscriptionID] = append(s.sub.statusEvents[subscriptionID], domain.SubscriptionStatusEvent{
		StatusEventID:        uuidNewStringForTest(),
		SubscriptionID:       subscriptionID,
		PreviousStatus:       string(previous),
		NewStatus:            string(newStatus),
		Reason:               reason,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	})
	return nil
}

func (s *stubStore) ListStatusEventsBySubscription(_ context.Context, subscriptionID string) ([]domain.SubscriptionStatusEvent, error) {
	return s.sub.statusEvents[subscriptionID], nil
}

func (s *stubStore) CreateBillingSourceTransfer(_ context.Context, transfer *domain.BillingSourceTransfer, newSub *domain.CommercialSubscription) error {
	if transfer.OldSubscriptionID != nil {
		old, ok := s.sub.subscriptions[*transfer.OldSubscriptionID]
		if !ok {
			return domain.ErrSubscriptionNotFound
		}
		if old.Status == domain.SubscriptionStatusCanceled || old.Status == domain.SubscriptionStatusTerminated {
			return domain.ErrSubscriptionNotFound
		}
		old.Status = domain.SubscriptionStatusCanceled
		now := time.Now().UTC()
		old.CanceledAt = &now
		old.UpdatedAt = now
	}
	if newSub != nil {
		for _, existing := range s.sub.subscriptions {
			if existing.CommercialAccountID == newSub.CommercialAccountID &&
				existing.SubscriptionID != newSub.SubscriptionID &&
				existing.Status != domain.SubscriptionStatusCanceled &&
				existing.Status != domain.SubscriptionStatusTerminated {
				return domain.ErrActiveSubscriptionExists
			}
		}
		s.sub.subscriptions[newSub.SubscriptionID] = newSub
	}
	s.sub.transfers[transfer.TransferID] = transfer
	return nil
}

func uuidNewStringForTest() string {
	return time.Now().UTC().Format("20060102T150405.000000000")
}
