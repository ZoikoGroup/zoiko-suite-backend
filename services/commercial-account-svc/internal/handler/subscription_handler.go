// Chunk 6 — Plans, Pricing & Entitlements HTTP handlers.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/events"
)

const (
	PriceCatalogCreate        = "PRICE_CATALOG_CREATE"
	PlanCreate                = "PLAN_CREATE"
	SubscriptionCreate        = "SUBSCRIPTION_CREATE"
	EvaluationProgramCreate   = "EVALUATION_PROGRAM_CREATE"
	OverlayCreate             = "CONTRACT_OVERLAY_CREATE"
	SubscriptionChangePreview = "SUBSCRIPTION_CHANGE_PREVIEW"
	SubscriptionChangeConfirm = "SUBSCRIPTION_CHANGE_CONFIRM"
	SubscriptionStatusSet     = "SUBSCRIPTION_STATUS_SET"
	BillingSourceTransferSet  = "BILLING_SOURCE_TRANSFER_CREATE"
)

func RegisterSubscriptionRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/price-catalogs", func(r chi.Router) {
		r.Post("/", h.CreatePriceCatalog)
		r.Get("/{id}", h.GetPriceCatalog)
	})
	r.Route("/v1/plans", func(r chi.Router) {
		r.Post("/", h.CreatePlan)
		r.Get("/{id}", h.GetPlan)
		r.Put("/{id}/entitlement-limits", h.SetEntitlementLimit)
	})
	r.Route("/v1/subscriptions", func(r chi.Router) {
		r.Post("/", h.CreateSubscription)
		r.Get("/{id}", h.GetSubscription)
		r.Get("/{id}/entitlements/{metricType}", h.ResolveEntitlement)
		r.Post("/{id}/evaluation-program", h.CreateEvaluationProgram)
		r.Post("/{id}/usage-events", h.RecordUsageEvent)
		r.Post("/{id}/status", h.SetSubscriptionStatus)
		r.Get("/{id}/status-events", h.ListStatusEvents)
	})
	r.Route("/v1/subscription-change-requests", func(r chi.Router) {
		r.Post("/", h.PreviewSubscriptionChange)
		r.Post("/{id}/confirm", h.ConfirmSubscriptionChange)
	})
	r.Post("/v1/contract-entitlement-overlays", h.CreateOverlay)
	r.Post("/v1/billing-source-transfers", h.TransferBillingSource)
}

func (h *Handler) CreatePriceCatalog(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePriceCatalogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CatalogCode == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "catalog_code and effective_from are required")
		return
	}
	effectiveFrom, err := time.Parse(time.RFC3339, req.EffectiveFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "effective_from must be RFC3339")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, platformScopeID, PriceCatalogCreate) {
		return
	}

	c := &domain.PriceCatalog{
		CatalogVersionID:     uuid.NewString(),
		CatalogCode:          req.CatalogCode,
		Status:               domain.CatalogStatusPublished,
		EffectiveFrom:        effectiveFrom,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreatePriceCatalog(r.Context(), c); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "catalog_code already exists")
			return
		}
		h.logger.Error("create price catalog failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create price catalog")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) GetPriceCatalog(w http.ResponseWriter, r *http.Request) {
	c, err := h.store.GetPriceCatalog(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrPriceCatalogNotFound) {
			writeError(w, http.StatusNotFound, "price catalog not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get price catalog")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) CreatePlan(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CatalogVersionID == "" || req.PlanCode == "" || req.DisplayName == "" || req.BillingInterval == "" || req.BasePriceCurrencyCode == "" {
		writeError(w, http.StatusBadRequest, "catalog_version_id, plan_code, display_name, billing_interval, and base_price_currency_code are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, platformScopeID, PlanCreate) {
		return
	}

	p := &domain.Plan{
		PlanID:                uuid.NewString(),
		CatalogVersionID:      req.CatalogVersionID,
		PlanCode:              req.PlanCode,
		DisplayName:           req.DisplayName,
		BillingInterval:       req.BillingInterval,
		BasePriceAmount:       req.BasePriceAmount,
		BasePriceCurrencyCode: req.BasePriceCurrencyCode,
		CreatedAt:             time.Now().UTC(),
		CreatedByPrincipalID:  principalID,
	}
	if req.MarketScope != "" {
		p.MarketScope = &req.MarketScope
	}
	if err := h.store.CreatePlan(r.Context(), p); err != nil {
		h.logger.Error("create plan failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create plan")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) GetPlan(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.GetPlan(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrPlanNotFound) {
			writeError(w, http.StatusNotFound, "plan not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get plan")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) SetEntitlementLimit(w http.ResponseWriter, r *http.Request) {
	planID := chi.URLParam(r, "id")
	var req domain.SetEntitlementLimitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.MetricType == "" {
		writeError(w, http.StatusBadRequest, "metric_type is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, platformScopeID, PlanCreate) {
		return
	}

	l := &domain.EntitlementLimit{
		EntitlementLimitID: uuid.NewString(),
		PlanID:             planID,
		MetricType:         req.MetricType,
		LimitValue:         req.LimitValue,
	}
	if err := h.store.SetEntitlementLimit(r.Context(), l); err != nil {
		h.logger.Error("set entitlement limit failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to set entitlement limit")
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (h *Handler) CreateSubscription(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CommercialAccountID == "" || req.PlanID == "" {
		writeError(w, http.StatusBadRequest, "commercial_account_id and plan_id are required")
		return
	}

	plan, err := h.store.GetPlan(r.Context(), req.PlanID)
	if err != nil {
		if errors.Is(err, domain.ErrPlanNotFound) {
			writeError(w, http.StatusBadRequest, "plan not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch plan")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.CommercialAccountID, SubscriptionCreate) {
		return
	}

	status := domain.SubscriptionStatusActive
	if req.StartAsEvaluation {
		status = domain.SubscriptionStatusEvaluation
	}
	billingSource := domain.BillingSourceDirect
	if req.BillingSource != "" {
		billingSource = domain.BillingSource(req.BillingSource)
	}
	now := time.Now().UTC()
	sub := &domain.CommercialSubscription{
		SubscriptionID:       uuid.NewString(),
		CommercialAccountID:  req.CommercialAccountID,
		PlanID:               plan.PlanID,
		CatalogVersionID:     plan.CatalogVersionID,
		BillingInterval:      plan.BillingInterval,
		BillingSource:        billingSource,
		Status:               status,
		CreatedAt:            now,
		UpdatedAt:            now,
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateSubscription(r.Context(), sub); err != nil {
		if errors.Is(err, domain.ErrActiveSubscriptionExists) {
			writeError(w, http.StatusConflict, "commercial account already has a non-terminal subscription")
			return
		}
		h.logger.Error("create subscription failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create subscription")
		return
	}

	// commercial_subscription.created is published via the transactional
	// outbox (internal/outbox), written in the same DB transaction as the
	// INSERT above — see PgStore.CreateSubscription. No direct publish
	// call here: that would either double-publish or, if removed without
	// the outbox in place, silently drop the event on a crash between
	// commit and publish (doc7 backlog item 32).
	writeJSON(w, http.StatusCreated, sub)
}

func (h *Handler) GetSubscription(w http.ResponseWriter, r *http.Request) {
	sub, err := h.store.GetSubscription(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrSubscriptionNotFound) {
			writeError(w, http.StatusNotFound, "subscription not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get subscription")
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

func (h *Handler) ResolveEntitlement(w http.ResponseWriter, r *http.Request) {
	subscriptionID := chi.URLParam(r, "id")
	metricType := chi.URLParam(r, "metricType")
	resolved, err := h.store.ResolveEntitlement(r.Context(), subscriptionID, metricType)
	if err != nil {
		if errors.Is(err, domain.ErrSubscriptionNotFound) {
			writeError(w, http.StatusNotFound, "subscription not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to resolve entitlement")
		return
	}
	writeJSON(w, http.StatusOK, resolved)
}

func (h *Handler) CreateEvaluationProgram(w http.ResponseWriter, r *http.Request) {
	subscriptionID := chi.URLParam(r, "id")
	var req domain.CreateEvaluationProgramRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DurationDays <= 0 || req.ConversionPolicy == "" || req.ExpiryAction == "" {
		writeError(w, http.StatusBadRequest, "duration_days, conversion_policy, and expiry_action are required")
		return
	}

	sub, err := h.store.GetSubscription(r.Context(), subscriptionID)
	if err != nil {
		if errors.Is(err, domain.ErrSubscriptionNotFound) {
			writeError(w, http.StatusNotFound, "subscription not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch subscription")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, sub.CommercialAccountID, EvaluationProgramCreate) {
		return
	}

	now := time.Now().UTC()
	e := &domain.EvaluationProgram{
		EvaluationProgramID:  uuid.NewString(),
		SubscriptionID:       subscriptionID,
		DurationDays:         req.DurationDays,
		PaymentRequired:      req.PaymentRequired,
		ConversionPolicy:     req.ConversionPolicy,
		ExpiryAction:         req.ExpiryAction,
		StartedAt:            now,
		ExpiresAt:            now.AddDate(0, 0, req.DurationDays),
		CreatedAt:            now,
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateEvaluationProgram(r.Context(), e); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "subscription already has an evaluation program")
			return
		}
		h.logger.Error("create evaluation program failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create evaluation program")
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (h *Handler) CreateOverlay(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateOverlayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CommercialAccountID == "" || req.MetricType == "" || req.ApprovedByPrincipalID == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "commercial_account_id, metric_type, approved_by_principal_id, and effective_from are required")
		return
	}
	effectiveFrom, err := time.Parse(time.RFC3339, req.EffectiveFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "effective_from must be RFC3339")
		return
	}
	var effectiveTo *time.Time
	if req.EffectiveTo != "" {
		t, err := time.Parse(time.RFC3339, req.EffectiveTo)
		if err != nil {
			writeError(w, http.StatusBadRequest, "effective_to must be RFC3339")
			return
		}
		effectiveTo = &t
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.CommercialAccountID, OverlayCreate) {
		return
	}

	o := &domain.ContractEntitlementOverlay{
		OverlayID:             uuid.NewString(),
		CommercialAccountID:   req.CommercialAccountID,
		MetricType:            req.MetricType,
		OverrideLimitValue:    req.OverrideLimitValue,
		EffectiveFrom:         effectiveFrom,
		EffectiveTo:           effectiveTo,
		ApprovedByPrincipalID: req.ApprovedByPrincipalID,
		CreatedAt:             time.Now().UTC(),
		CreatedByPrincipalID:  principalID,
	}
	if req.LegalReference != "" {
		o.LegalReference = &req.LegalReference
	}
	if err := h.store.CreateOverlay(r.Context(), o); err != nil {
		h.logger.Error("create overlay failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create overlay")
		return
	}
	writeJSON(w, http.StatusCreated, o)
}

func (h *Handler) RecordUsageEvent(w http.ResponseWriter, r *http.Request) {
	subscriptionID := chi.URLParam(r, "id")
	var req domain.RecordUsageEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.UsageEventID == "" || req.MetricType == "" || req.SourceService == "" {
		writeError(w, http.StatusBadRequest, "usage_event_id, metric_type, and source_service are required")
		return
	}

	e := &domain.UsageMeterEvent{
		UsageEventID:   req.UsageEventID,
		SubscriptionID: subscriptionID,
		MetricType:     req.MetricType,
		Quantity:       req.Quantity,
		OccurredAt:     time.Now().UTC(),
		SourceService:  req.SourceService,
		BillableState:  "PENDING",
		CreatedAt:      time.Now().UTC(),
	}
	if err := h.store.RecordUsageEvent(r.Context(), e); err != nil {
		if errors.Is(err, domain.ErrDuplicateUsageEvent) {
			// Idempotent: a retried metering call is not an error — it must
			// never double-count (doc7 §L1). Report success either way.
			writeJSON(w, http.StatusOK, map[string]string{"status": "already_recorded"})
			return
		}
		h.logger.Error("record usage event failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to record usage event")
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (h *Handler) PreviewSubscriptionChange(w http.ResponseWriter, r *http.Request) {
	var req domain.PreviewChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SubscriptionID == "" || req.TargetPlanID == "" {
		writeError(w, http.StatusBadRequest, "subscription_id and target_plan_id are required")
		return
	}

	sub, err := h.store.GetSubscription(r.Context(), req.SubscriptionID)
	if err != nil {
		if errors.Is(err, domain.ErrSubscriptionNotFound) {
			writeError(w, http.StatusNotFound, "subscription not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch subscription")
		return
	}
	if _, err := h.store.GetPlan(r.Context(), req.TargetPlanID); err != nil {
		if errors.Is(err, domain.ErrPlanNotFound) {
			writeError(w, http.StatusBadRequest, "target plan not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch target plan")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, sub.CommercialAccountID, SubscriptionChangePreview) {
		return
	}

	effectiveAt := time.Now().UTC()
	if req.EffectiveAt != "" {
		t, err := time.Parse(time.RFC3339, req.EffectiveAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "effective_at must be RFC3339")
			return
		}
		effectiveAt = t
	}

	c := &domain.SubscriptionChangeRequest{
		ChangeRequestID:        uuid.NewString(),
		SubscriptionID:         req.SubscriptionID,
		TargetPlanID:           req.TargetPlanID,
		EffectiveAt:            effectiveAt,
		Status:                 "PREVIEWED",
		RequestedByPrincipalID: principalID,
		CreatedAt:              time.Now().UTC(),
	}
	if err := h.store.CreateChangeRequest(r.Context(), c); err != nil {
		h.logger.Error("create change request failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create change request")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) ConfirmSubscriptionChange(w http.ResponseWriter, r *http.Request) {
	changeRequestID := chi.URLParam(r, "id")

	existing, err := h.store.GetChangeRequest(r.Context(), changeRequestID)
	if err != nil {
		if errors.Is(err, domain.ErrChangeRequestNotFound) {
			writeError(w, http.StatusNotFound, "change request not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch change request")
		return
	}
	sub, err := h.store.GetSubscription(r.Context(), existing.SubscriptionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fetch subscription")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, sub.CommercialAccountID, SubscriptionChangeConfirm) {
		return
	}

	updated, err := h.store.ApplyChangeRequest(r.Context(), changeRequestID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrInvalidChangeRequestState):
			writeError(w, http.StatusConflict, "change request already applied or canceled")
		case errors.Is(err, domain.ErrChangeRequestNotFound):
			writeError(w, http.StatusNotFound, "change request not found")
		default:
			h.logger.Error("apply change request failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "failed to apply change request")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "commercial_subscription.plan_changed", EntityID: updated.SubscriptionID, TenantID: updated.CommercialAccountID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// SetSubscriptionStatus drives the doc7 §O1-O3 dunning state machine — e.g.
// a payment webhook handler posts ACTIVE -> PAST_DUE on failure, or PAST_DUE
// -> ACTIVE on a successful retry. Rejects any transition not in
// domain.ValidSubscriptionStatusTransitions; a same-status request succeeds
// idempotently without re-running tenant workflows (§O3).
func (h *Handler) SetSubscriptionStatus(w http.ResponseWriter, r *http.Request) {
	subscriptionID := chi.URLParam(r, "id")
	var req domain.SetSubscriptionStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.NewStatus == "" {
		writeError(w, http.StatusBadRequest, "new_status is required")
		return
	}
	newStatus := domain.SubscriptionStatus(req.NewStatus)
	// ValidSubscriptionStatusTransitions is keyed fromStatus -> []toStatus;
	// invert it here to get every status allowed to transition INTO newStatus.
	var fromStatuses []domain.SubscriptionStatus
	for from, tos := range domain.ValidSubscriptionStatusTransitions {
		for _, to := range tos {
			if to == newStatus {
				fromStatuses = append(fromStatuses, from)
				break
			}
		}
	}
	if len(fromStatuses) == 0 {
		writeError(w, http.StatusBadRequest, "new_status is not a recognized subscription status")
		return
	}

	sub, err := h.store.GetSubscription(r.Context(), subscriptionID)
	if err != nil {
		if errors.Is(err, domain.ErrSubscriptionNotFound) {
			writeError(w, http.StatusNotFound, "subscription not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch subscription")
		return
	}

	principalID, ok2 := h.requirePrincipal(w, r)
	if !ok2 {
		return
	}
	if !h.authorize(w, r, principalID, sub.CommercialAccountID, SubscriptionStatusSet) {
		return
	}

	var reason *string
	if req.Reason != "" {
		reason = &req.Reason
	}
	if err := h.store.TransitionSubscriptionStatus(r.Context(), subscriptionID, newStatus, fromStatuses, reason, principalID); err != nil {
		switch {
		case errors.Is(err, domain.ErrSubscriptionNotFound):
			writeError(w, http.StatusNotFound, "subscription not found")
		case errors.Is(err, domain.ErrInvalidStatusTransition):
			writeError(w, http.StatusConflict, "subscription status transition not allowed")
		default:
			h.logger.Error("set subscription status failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "failed to set subscription status")
		}
		return
	}

	updated, err := h.store.GetSubscription(r.Context(), subscriptionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fetch updated subscription")
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "commercial_subscription.status_changed", EntityID: updated.SubscriptionID, TenantID: updated.CommercialAccountID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) ListStatusEvents(w http.ResponseWriter, r *http.Request) {
	subscriptionID := chi.URLParam(r, "id")
	events, err := h.store.ListStatusEventsBySubscription(r.Context(), subscriptionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list status events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status_events": events})
}

// TransferBillingSource implements doc7 §P3's standalone <-> Zoiko One
// migration: an effective-dated commercial transfer record, never a silent
// swap. Cancelling the old subscription and creating the new one happen in
// one atomic transaction; the "one non-terminal subscription per account"
// constraint from migration 000002 structurally prevents the account from
// ending up double-billed across both.
func (h *Handler) TransferBillingSource(w http.ResponseWriter, r *http.Request) {
	var req domain.TransferBillingSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CommercialAccountID == "" || req.NewBillingSource == "" {
		writeError(w, http.StatusBadRequest, "commercial_account_id and new_billing_source are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.CommercialAccountID, BillingSourceTransferSet) {
		return
	}

	var oldSub *domain.CommercialSubscription
	var oldBillingSource *string
	if req.OldSubscriptionID != "" {
		s, err := h.store.GetSubscription(r.Context(), req.OldSubscriptionID)
		if err != nil {
			if errors.Is(err, domain.ErrSubscriptionNotFound) {
				writeError(w, http.StatusBadRequest, "old subscription not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to fetch old subscription")
			return
		}
		oldSub = s
		bs := string(s.BillingSource)
		oldBillingSource = &bs
	}

	targetPlanID := req.TargetPlanID
	if targetPlanID == "" {
		if oldSub == nil {
			writeError(w, http.StatusBadRequest, "target_plan_id is required when old_subscription_id is not given")
			return
		}
		targetPlanID = oldSub.PlanID
	}
	plan, err := h.store.GetPlan(r.Context(), targetPlanID)
	if err != nil {
		if errors.Is(err, domain.ErrPlanNotFound) {
			writeError(w, http.StatusBadRequest, "target plan not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch target plan")
		return
	}

	now := time.Now().UTC()
	newSubID := uuid.NewString()
	newSub := &domain.CommercialSubscription{
		SubscriptionID:       newSubID,
		CommercialAccountID:  req.CommercialAccountID,
		PlanID:               plan.PlanID,
		CatalogVersionID:     plan.CatalogVersionID,
		BillingInterval:      plan.BillingInterval,
		BillingSource:        domain.BillingSource(req.NewBillingSource),
		Status:               domain.SubscriptionStatusActive,
		CreatedAt:            now,
		UpdatedAt:            now,
		CreatedByPrincipalID: principalID,
	}

	transfer := &domain.BillingSourceTransfer{
		TransferID:            uuid.NewString(),
		CommercialAccountID:   req.CommercialAccountID,
		OldBillingSource:      oldBillingSource,
		NewBillingSource:      req.NewBillingSource,
		NewSubscriptionID:     &newSubID,
		EntitlementContinuity: true,
		CreditAmount:          req.CreditAmount,
		ReconciliationStatus:  "PENDING",
		CreatedAt:             now,
		CreatedByPrincipalID:  principalID,
	}
	if req.OldSubscriptionID != "" {
		transfer.OldSubscriptionID = &req.OldSubscriptionID
	}

	if err := h.store.CreateBillingSourceTransfer(r.Context(), transfer, newSub); err != nil {
		if errors.Is(err, domain.ErrActiveSubscriptionExists) {
			writeError(w, http.StatusConflict, "commercial account already has a non-terminal subscription")
			return
		}
		if errors.Is(err, domain.ErrSubscriptionNotFound) {
			writeError(w, http.StatusBadRequest, "old subscription not found or already terminal")
			return
		}
		h.logger.Error("billing source transfer failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to transfer billing source")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "commercial_subscription.billing_source_transferred", EntityID: transfer.TransferID, TenantID: transfer.CommercialAccountID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: transfer,
	})
	writeJSON(w, http.StatusCreated, transfer)
}
