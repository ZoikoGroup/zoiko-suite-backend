// Chunk 6 — Plans, Pricing & Entitlements store methods.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/commercial-account-svc/internal/domain"
)

func (s *PgStore) CreatePriceCatalog(ctx context.Context, c *domain.PriceCatalog) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO price_catalogs (
			catalog_version_id, catalog_code, status, effective_from, effective_to,
			created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, c.CatalogVersionID, c.CatalogCode, string(c.Status), c.EffectiveFrom, c.EffectiveTo,
		c.CreatedAt, c.CreatedByPrincipalID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: catalog_code %s", domain.ErrConflict, c.CatalogCode)
		}
		return fmt.Errorf("insert price catalog: %w", err)
	}
	return nil
}

func (s *PgStore) GetPriceCatalog(ctx context.Context, catalogVersionID string) (*domain.PriceCatalog, error) {
	var c domain.PriceCatalog
	var status string
	err := s.pool.QueryRow(ctx, `
		SELECT catalog_version_id, catalog_code, status, effective_from, effective_to,
		       created_at, created_by_principal_id
		FROM price_catalogs WHERE catalog_version_id = $1
	`, catalogVersionID).Scan(
		&c.CatalogVersionID, &c.CatalogCode, &status, &c.EffectiveFrom, &c.EffectiveTo,
		&c.CreatedAt, &c.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPriceCatalogNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Status = domain.CatalogStatus(status)
	return &c, nil
}

func (s *PgStore) CreatePlan(ctx context.Context, p *domain.Plan) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO plans (
			plan_id, catalog_version_id, plan_code, display_name, billing_interval,
			base_price_amount, base_price_currency_code, market_scope,
			created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, p.PlanID, p.CatalogVersionID, p.PlanCode, p.DisplayName, p.BillingInterval,
		p.BasePriceAmount, p.BasePriceCurrencyCode, p.MarketScope,
		p.CreatedAt, p.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("insert plan: %w", err)
	}
	return nil
}

func (s *PgStore) GetPlan(ctx context.Context, planID string) (*domain.Plan, error) {
	var p domain.Plan
	err := s.pool.QueryRow(ctx, `
		SELECT plan_id, catalog_version_id, plan_code, display_name, billing_interval,
		       base_price_amount, base_price_currency_code, market_scope,
		       created_at, created_by_principal_id
		FROM plans WHERE plan_id = $1
	`, planID).Scan(
		&p.PlanID, &p.CatalogVersionID, &p.PlanCode, &p.DisplayName, &p.BillingInterval,
		&p.BasePriceAmount, &p.BasePriceCurrencyCode, &p.MarketScope,
		&p.CreatedAt, &p.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPlanNotFound
	}
	if err != nil {
		return nil, err
	}
	limits, err := s.ListEntitlementLimitsByPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	p.Limits = limits
	return &p, nil
}

func (s *PgStore) SetEntitlementLimit(ctx context.Context, l *domain.EntitlementLimit) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO entitlement_limits (entitlement_limit_id, plan_id, metric_type, limit_value)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (plan_id, metric_type) DO UPDATE SET limit_value = EXCLUDED.limit_value
	`, l.EntitlementLimitID, l.PlanID, l.MetricType, l.LimitValue)
	if err != nil {
		return fmt.Errorf("set entitlement limit: %w", err)
	}
	return nil
}

func (s *PgStore) ListEntitlementLimitsByPlan(ctx context.Context, planID string) ([]domain.EntitlementLimit, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT entitlement_limit_id, plan_id, metric_type, limit_value
		FROM entitlement_limits WHERE plan_id = $1
	`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.EntitlementLimit
	for rows.Next() {
		var l domain.EntitlementLimit
		if err := rows.Scan(&l.EntitlementLimitID, &l.PlanID, &l.MetricType, &l.LimitValue); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *PgStore) CreateSubscription(ctx context.Context, sub *domain.CommercialSubscription) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO commercial_subscriptions (
			subscription_id, commercial_account_id, plan_id, catalog_version_id, billing_interval,
			status, renewal_date, canceled_at, processor_subscription_ref,
			created_at, updated_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, sub.SubscriptionID, sub.CommercialAccountID, sub.PlanID, sub.CatalogVersionID, sub.BillingInterval,
		string(sub.Status), sub.RenewalDate, sub.CanceledAt, sub.ProcessorSubscriptionRef,
		sub.CreatedAt, sub.UpdatedAt, sub.CreatedByPrincipalID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: commercial_account_id %s", domain.ErrActiveSubscriptionExists, sub.CommercialAccountID)
		}
		return fmt.Errorf("insert subscription: %w", err)
	}
	return nil
}

func (s *PgStore) GetSubscription(ctx context.Context, subscriptionID string) (*domain.CommercialSubscription, error) {
	var sub domain.CommercialSubscription
	var status string
	err := s.pool.QueryRow(ctx, `
		SELECT subscription_id, commercial_account_id, plan_id, catalog_version_id, billing_interval,
		       status, renewal_date, canceled_at, processor_subscription_ref,
		       created_at, updated_at, created_by_principal_id
		FROM commercial_subscriptions WHERE subscription_id = $1
	`, subscriptionID).Scan(
		&sub.SubscriptionID, &sub.CommercialAccountID, &sub.PlanID, &sub.CatalogVersionID, &sub.BillingInterval,
		&status, &sub.RenewalDate, &sub.CanceledAt, &sub.ProcessorSubscriptionRef,
		&sub.CreatedAt, &sub.UpdatedAt, &sub.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, err
	}
	sub.Status = domain.SubscriptionStatus(status)
	return &sub, nil
}

// UpdateSubscriptionPlan is used by ApplyChangeRequest — an upgrade/downgrade
// never edits catalog/plan rows in place (§U1); it repoints the subscription
// at a new plan_id/catalog_version_id.
func (s *PgStore) UpdateSubscriptionPlan(ctx context.Context, subscriptionID, planID, catalogVersionID string) error {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
		UPDATE commercial_subscriptions
		SET plan_id = $1, catalog_version_id = $2, updated_at = $3
		WHERE subscription_id = $4
	`, planID, catalogVersionID, now, subscriptionID)
	if err != nil {
		return fmt.Errorf("update subscription plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrSubscriptionNotFound
	}
	return nil
}

func (s *PgStore) CreateEvaluationProgram(ctx context.Context, e *domain.EvaluationProgram) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO evaluation_programs (
			evaluation_program_id, subscription_id, duration_days, payment_required,
			conversion_policy, expiry_action, started_at, expires_at,
			created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, e.EvaluationProgramID, e.SubscriptionID, e.DurationDays, e.PaymentRequired,
		e.ConversionPolicy, e.ExpiryAction, e.StartedAt, e.ExpiresAt,
		e.CreatedAt, e.CreatedByPrincipalID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: subscription_id %s", domain.ErrConflict, e.SubscriptionID)
		}
		return fmt.Errorf("insert evaluation program: %w", err)
	}
	return nil
}

func (s *PgStore) GetEvaluationProgramBySubscription(ctx context.Context, subscriptionID string) (*domain.EvaluationProgram, error) {
	var e domain.EvaluationProgram
	err := s.pool.QueryRow(ctx, `
		SELECT evaluation_program_id, subscription_id, duration_days, payment_required,
		       conversion_policy, expiry_action, started_at, expires_at,
		       created_at, created_by_principal_id
		FROM evaluation_programs WHERE subscription_id = $1
	`, subscriptionID).Scan(
		&e.EvaluationProgramID, &e.SubscriptionID, &e.DurationDays, &e.PaymentRequired,
		&e.ConversionPolicy, &e.ExpiryAction, &e.StartedAt, &e.ExpiresAt,
		&e.CreatedAt, &e.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEvaluationProgramNotFound
	}
	return &e, err
}

func (s *PgStore) CreateOverlay(ctx context.Context, o *domain.ContractEntitlementOverlay) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO contract_entitlement_overlays (
			overlay_id, commercial_account_id, metric_type, override_limit_value,
			legal_reference, effective_from, effective_to, approved_by_principal_id,
			created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, o.OverlayID, o.CommercialAccountID, o.MetricType, o.OverrideLimitValue,
		o.LegalReference, o.EffectiveFrom, o.EffectiveTo, o.ApprovedByPrincipalID,
		o.CreatedAt, o.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("insert overlay: %w", err)
	}
	return nil
}

// activeOverlayForMetric returns the overlay currently in effect for
// (commercialAccountID, metricType), or nil if none applies right now.
func (s *PgStore) activeOverlayForMetric(ctx context.Context, commercialAccountID, metricType string) (*domain.ContractEntitlementOverlay, error) {
	now := time.Now().UTC()
	var o domain.ContractEntitlementOverlay
	err := s.pool.QueryRow(ctx, `
		SELECT overlay_id, commercial_account_id, metric_type, override_limit_value,
		       legal_reference, effective_from, effective_to, approved_by_principal_id,
		       created_at, created_by_principal_id
		FROM contract_entitlement_overlays
		WHERE commercial_account_id = $1 AND metric_type = $2
		  AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		ORDER BY effective_from DESC
		LIMIT 1
	`, commercialAccountID, metricType, now).Scan(
		&o.OverlayID, &o.CommercialAccountID, &o.MetricType, &o.OverrideLimitValue,
		&o.LegalReference, &o.EffectiveFrom, &o.EffectiveTo, &o.ApprovedByPrincipalID,
		&o.CreatedAt, &o.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ResolveEntitlement answers "what may this subscription's account actually
// do for metricType right now": an active contract overlay wins over the
// plan's own limit, since an overlay exists precisely to override it (§B6).
func (s *PgStore) ResolveEntitlement(ctx context.Context, subscriptionID, metricType string) (*domain.ResolvedEntitlement, error) {
	sub, err := s.GetSubscription(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}

	overlay, err := s.activeOverlayForMetric(ctx, sub.CommercialAccountID, metricType)
	if err != nil {
		return nil, err
	}
	if overlay != nil {
		overlayID := overlay.OverlayID
		return &domain.ResolvedEntitlement{
			MetricType: metricType,
			LimitValue: overlay.OverrideLimitValue,
			Source:     "OVERLAY",
			OverlayID:  &overlayID,
		}, nil
	}

	limits, err := s.ListEntitlementLimitsByPlan(ctx, sub.PlanID)
	if err != nil {
		return nil, err
	}
	for _, l := range limits {
		if l.MetricType == metricType {
			return &domain.ResolvedEntitlement{
				MetricType: metricType,
				LimitValue: l.LimitValue,
				Source:     "PLAN",
			}, nil
		}
	}
	// No limit row for this metric on this plan at all — distinct from an
	// explicit unlimited (LimitValue nil but a row exists). Reported the
	// same way (nil LimitValue) since a caller checking entitlement can't
	// safely tell "plan doesn't model this metric" from "unlimited" without
	// a Source that says PLAN and finding no row — that ambiguity is real
	// and left visible rather than papered over with an invented default.
	return &domain.ResolvedEntitlement{MetricType: metricType, LimitValue: nil, Source: "PLAN"}, nil
}

func (s *PgStore) RecordUsageEvent(ctx context.Context, e *domain.UsageMeterEvent) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO commercial_usage_meter_events (
			usage_event_id, subscription_id, metric_type, quantity, occurred_at,
			source_service, billable_state, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (usage_event_id) DO NOTHING
	`, e.UsageEventID, e.SubscriptionID, e.MetricType, e.Quantity, e.OccurredAt,
		e.SourceService, e.BillableState, e.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert usage event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrDuplicateUsageEvent
	}
	return nil
}

func (s *PgStore) CreateChangeRequest(ctx context.Context, c *domain.SubscriptionChangeRequest) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO subscription_change_requests (
			change_request_id, subscription_id, target_plan_id, effective_at,
			status, requested_by_principal_id, created_at, applied_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, c.ChangeRequestID, c.SubscriptionID, c.TargetPlanID, c.EffectiveAt,
		c.Status, c.RequestedByPrincipalID, c.CreatedAt, c.AppliedAt,
	)
	if err != nil {
		return fmt.Errorf("insert change request: %w", err)
	}
	return nil
}

func (s *PgStore) GetChangeRequest(ctx context.Context, changeRequestID string) (*domain.SubscriptionChangeRequest, error) {
	var c domain.SubscriptionChangeRequest
	err := s.pool.QueryRow(ctx, `
		SELECT change_request_id, subscription_id, target_plan_id, effective_at,
		       status, requested_by_principal_id, created_at, applied_at
		FROM subscription_change_requests WHERE change_request_id = $1
	`, changeRequestID).Scan(
		&c.ChangeRequestID, &c.SubscriptionID, &c.TargetPlanID, &c.EffectiveAt,
		&c.Status, &c.RequestedByPrincipalID, &c.CreatedAt, &c.AppliedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrChangeRequestNotFound
	}
	return &c, err
}

// ApplyChangeRequest atomically repoints the subscription at the change
// request's target plan and marks the request APPLIED — the two writes
// happen in one transaction so a crash between them can never leave a
// request marked APPLIED against a subscription that was never actually
// moved, or vice versa.
func (s *PgStore) ApplyChangeRequest(ctx context.Context, changeRequestID string) (*domain.CommercialSubscription, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var subscriptionID, targetPlanID, status string
	err = tx.QueryRow(ctx, `
		SELECT subscription_id, target_plan_id, status FROM subscription_change_requests
		WHERE change_request_id = $1
	`, changeRequestID).Scan(&subscriptionID, &targetPlanID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrChangeRequestNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != "PREVIEWED" {
		return nil, domain.ErrInvalidChangeRequestState
	}

	var catalogVersionID string
	if err := tx.QueryRow(ctx, `SELECT catalog_version_id FROM plans WHERE plan_id = $1`, targetPlanID).Scan(&catalogVersionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPlanNotFound
		}
		return nil, err
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE commercial_subscriptions SET plan_id = $1, catalog_version_id = $2, updated_at = $3
		WHERE subscription_id = $4
	`, targetPlanID, catalogVersionID, now, subscriptionID); err != nil {
		return nil, fmt.Errorf("update subscription: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE subscription_change_requests SET status = 'APPLIED', applied_at = $1
		WHERE change_request_id = $2
	`, now, changeRequestID); err != nil {
		return nil, fmt.Errorf("update change request: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return s.GetSubscription(ctx, subscriptionID)
}
