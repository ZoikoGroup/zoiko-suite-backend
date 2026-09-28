// COM-02 part 2c persistence: discount applications, price migration offers
// and the subscription boundary queue (migration 000009).
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/outbox"
)

// GovernanceStore is the COM-02 part 2c contract.
type GovernanceStore interface {
	ProposeDiscount(ctx context.Context, p domain.ProposeDiscountParams, claim domain.IdempotencyClaim) (*domain.DiscountApplication, error)
	DecideDiscount(ctx context.Context, subscriptionID, discountID string, expectedVersion int, decision, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.DiscountApplication, error)
	ListDiscounts(ctx context.Context, subscriptionID string) ([]domain.DiscountApplication, error)

	CreateMigrationOffer(ctx context.Context, o *domain.MigrationOffer, claim domain.IdempotencyClaim) (*domain.MigrationOffer, error)
	PublishMigrationOffer(ctx context.Context, offerID string, expectedVersion int, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.MigrationOffer, error)
	WithdrawMigrationOffer(ctx context.Context, offerID string, expectedVersion int, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.MigrationOffer, error)
	GetMigrationOffer(ctx context.Context, offerID string) (*domain.MigrationOffer, error)
	ListMigrationOffersFor(ctx context.Context, subscriptionID string, now time.Time) ([]domain.MigrationOffer, error)

	ProcessNextBoundary(ctx context.Context, now time.Time) (bool, error)
}

var _ GovernanceStore = (*PgStore)(nil)

// Discount decisions.
const (
	DecisionApprove  = "approve"
	DecisionReject   = "reject"
	DecisionWithdraw = "withdraw"
)

// ── Discounts ────────────────────────────────────────────────────────────────

const discountColumns = `discount_application_id, subscription_id, price_version_id, component_key, status, reason,
	customer_basis_ref, requested_by_principal_id, requested_at, approval_basis, approved_by_principal_id, approved_at,
	decided_reason, decided_by_principal_id, decided_at, row_version`

func scanDiscount(row pgx.Row) (*domain.DiscountApplication, error) {
	var d domain.DiscountApplication
	if err := row.Scan(&d.DiscountApplicationID, &d.SubscriptionID, &d.PriceVersionID, &d.ComponentKey, &d.Status, &d.Reason,
		&d.CustomerBasisRef, &d.RequestedByPrincipalID, &d.RequestedAt, &d.ApprovalBasis, &d.ApprovedByPrincipalID,
		&d.ApprovedAt, &d.DecidedReason, &d.DecidedByPrincipalID, &d.DecidedAt, &d.RowVersion); err != nil {
		return nil, err
	}
	return &d, nil
}

func (a *subscriptionAggregate) currentOrUpcomingItems(now time.Time) []domain.SubscriptionItem {
	if v, _ := a.statusNowOrUpcoming(now); v != nil {
		return v.Items
	}
	return nil
}

// discountComponent finds a DISCOUNT component on a price version the
// subscription is bound to now.
func (a *subscriptionAggregate) discountComponent(now time.Time, productCode, componentKey string) (*domain.PriceVersion, *domain.PriceComponent, error) {
	for _, it := range a.currentOrUpcomingItems(now) {
		pv := a.pvs[it.PriceVersionID]
		if pv == nil || (productCode != "" && pv.ProductCode != productCode) {
			continue
		}
		for i := range pv.Components {
			c := &pv.Components[i]
			if c.ComponentKey == componentKey && c.ComponentType == domain.ComponentDiscount {
				return pv, c, nil
			}
		}
	}
	return nil, nil, domain.ErrDiscountNotApplicable
}

func discountEvent(ctx context.Context, tx pgx.Tx, eventType, org string, d *domain.DiscountApplication, actor string, at time.Time) error {
	return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "subscription_discount", AggregateID: d.DiscountApplicationID,
		EventType: eventType, TenantID: &org, Payload: map[string]any{
			"discount_application_id": d.DiscountApplicationID, "subscription_id": d.SubscriptionID,
			"price_version_id": d.PriceVersionID, "component_key": d.ComponentKey, "status": d.Status,
			"approval_basis": d.ApprovalBasis, "actor_id": actor, "occurred_at": at.UTC(),
		}})
}

// ProposeDiscount applies a catalogue discount component to a subscription.
// A component the catalogue marks requires_approval waits for a second
// person; one it does not is approved on catalogue policy at once, and the
// record says which.
func (s *PgStore) ProposeDiscount(ctx context.Context, p domain.ProposeDiscountParams, claim domain.IdempotencyClaim) (*domain.DiscountApplication, error) {
	var out *domain.DiscountApplication
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		a, err := loadAggregate(ctx, tx, p.SubscriptionID, true)
		if err != nil {
			return err
		}
		cur, _ := a.statusNowOrUpcoming(p.Now)
		if cur == nil || cur.LifecycleStatus.Terminal() || cur.LifecycleStatus == domain.LifecycleCancelPending {
			return fmt.Errorf("%w: discounts apply to a pending, trialing or active subscription", domain.ErrSubscriptionInvalidState)
		}
		pv, comp, err := a.discountComponent(p.Now, p.ProductCode, p.ComponentKey)
		if err != nil {
			return err
		}
		status, basis := domain.DiscountProposed, (*string)(nil)
		var approvedAt *time.Time
		if !*comp.RequiresApproval {
			b := domain.ApprovalCatalogPolicy
			status, basis, approvedAt = domain.DiscountApproved, &b, &p.Now
		}
		d, err := scanDiscount(tx.QueryRow(ctx, `
			INSERT INTO subscription_discounts (discount_application_id, subscription_id, price_version_id, component_key,
				status, reason, customer_basis_ref, requested_by_principal_id, requested_at, approval_basis, approved_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING `+discountColumns,
			p.DiscountApplicationID, p.SubscriptionID, pv.PriceVersionID, p.ComponentKey, status, p.Reason,
			p.CustomerBasisRef, p.Actor, p.Now, basis, approvedAt))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.ErrDiscountExists
			}
			return err
		}
		out = d
		eventType := "subscription.discount_proposed"
		if status == domain.DiscountApproved {
			eventType = "subscription.discount_approved"
		}
		return discountEvent(ctx, tx, eventType, a.sub.OrganizationID, d, p.Actor, p.Now)
	})
	return out, err
}

// DecideDiscount approves, rejects or withdraws a proposed discount. The
// approver must not be the proposer (negative path #06), and the component
// must still be bound: a plan change since the proposal leaves nothing to
// approve.
func (s *PgStore) DecideDiscount(ctx context.Context, subscriptionID, discountID string, expectedVersion int, decision, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.DiscountApplication, error) {
	var out *domain.DiscountApplication
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		a, err := loadAggregate(ctx, tx, subscriptionID, true)
		if err != nil {
			return err
		}
		d, err := scanDiscount(tx.QueryRow(ctx, `SELECT `+discountColumns+` FROM subscription_discounts
			WHERE discount_application_id = $1 AND subscription_id = $2 FOR UPDATE`, discountID, subscriptionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDiscountNotFound
		}
		if err != nil {
			return err
		}
		if d.Status != domain.DiscountProposed {
			return fmt.Errorf("%w: discount is %s", domain.ErrDiscountInvalidState, d.Status)
		}
		if d.RowVersion != expectedVersion {
			return domain.ErrVersionConflict
		}
		eventType := "subscription.discount_" + map[string]string{DecisionApprove: "approved", DecisionReject: "rejected", DecisionWithdraw: "withdrawn"}[decision]
		switch decision {
		case DecisionApprove:
			if actor == d.RequestedByPrincipalID {
				return domain.ErrSoDViolation
			}
			bound := false
			for _, it := range a.currentOrUpcomingItems(now) {
				bound = bound || it.PriceVersionID == d.PriceVersionID
			}
			if !bound {
				return fmt.Errorf("%w: the price version it discounts is no longer bound", domain.ErrDiscountNotApplicable)
			}
			d, err = scanDiscount(tx.QueryRow(ctx, `
				UPDATE subscription_discounts SET status = 'APPROVED', row_version = row_version + 1,
					approval_basis = 'APPROVER', approved_by_principal_id = $2, approved_at = $3
				WHERE discount_application_id = $1 RETURNING `+discountColumns, discountID, actor, now))
		case DecisionReject, DecisionWithdraw:
			if decision == DecisionWithdraw && actor != d.RequestedByPrincipalID {
				return domain.ErrDiscountNotRequester
			}
			status := map[string]domain.DiscountStatus{DecisionReject: domain.DiscountRejected, DecisionWithdraw: domain.DiscountWithdrawn}[decision]
			d, err = scanDiscount(tx.QueryRow(ctx, `
				UPDATE subscription_discounts SET status = $2, row_version = row_version + 1,
					decided_reason = $3, decided_by_principal_id = $4, decided_at = $5
				WHERE discount_application_id = $1 RETURNING `+discountColumns, discountID, status, reason, actor, now))
		default:
			return fmt.Errorf("unknown discount decision %q", decision)
		}
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.ConstraintName == "discounts_approver_is_independent" {
				return domain.ErrSoDViolation
			}
			return err
		}
		out = d
		return discountEvent(ctx, tx, eventType, a.sub.OrganizationID, d, actor, now)
	})
	return out, err
}

func (s *PgStore) ListDiscounts(ctx context.Context, subscriptionID string) ([]domain.DiscountApplication, error) {
	var out []domain.DiscountApplication
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		if _, err := loadSubscriptionRow(ctx, tx, subscriptionID, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+discountColumns+` FROM subscription_discounts
			WHERE subscription_id = $1 ORDER BY requested_at, discount_application_id`, subscriptionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDiscount(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	return out, err
}

// ── Migration offers ─────────────────────────────────────────────────────────

const migrationOfferColumns = `migration_offer_id, product_id, from_price_version_id, to_price_version_id, eligibility_mode,
	accept_by, reason, status, row_version, created_at, created_by_principal_id, published_at, published_by_principal_id,
	withdrawn_at, withdraw_reason`

func scanMigrationOffer(row pgx.Row) (*domain.MigrationOffer, error) {
	var o domain.MigrationOffer
	if err := row.Scan(&o.MigrationOfferID, &o.ProductID, &o.FromPriceVersionID, &o.ToPriceVersionID, &o.EligibilityMode,
		&o.AcceptBy, &o.Reason, &o.Status, &o.RowVersion, &o.CreatedAt, &o.CreatedByPrincipalID, &o.PublishedAt,
		&o.PublishedByPrincipalID, &o.WithdrawnAt, &o.WithdrawReason); err != nil {
		return nil, err
	}
	return &o, nil
}

func loadMigrationOffer(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (*domain.MigrationOffer, error) {
	q := `SELECT ` + migrationOfferColumns + ` FROM price_migration_offers WHERE migration_offer_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	o, err := scanMigrationOffer(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMigrationOfferNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT subscription_id FROM price_migration_offer_targets WHERE migration_offer_id = $1 ORDER BY subscription_id`, id)
	if err != nil {
		return nil, err
	}
	if o.TargetSubscriptionIDs, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return nil, err
	}
	return o, nil
}

func migrationEvent(ctx context.Context, tx pgx.Tx, eventType string, o *domain.MigrationOffer) error {
	return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "price_migration_offer", AggregateID: o.MigrationOfferID,
		EventType: eventType, Payload: o})
}

// CreateMigrationOffer drafts an offer from one price version to a later
// published version of the same product. Listed targets must be existing
// subscriptions (the foreign key checks it; the seller plane cannot read
// tenant subscriptions, and does not need to).
func (s *PgStore) CreateMigrationOffer(ctx context.Context, o *domain.MigrationOffer, claim domain.IdempotencyClaim) (*domain.MigrationOffer, error) {
	var out *domain.MigrationOffer
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		from, err := loadVersion(ctx, tx, o.FromPriceVersionID, false)
		if err != nil {
			return err
		}
		to, err := loadVersion(ctx, tx, o.ToPriceVersionID, false)
		if err != nil {
			return err
		}
		if from.ProductID != to.ProductID || to.VersionNumber <= from.VersionNumber || to.Status != domain.PriceVersionPublished ||
			(from.Status != domain.PriceVersionPublished && from.Status != domain.PriceVersionRetired) {
			return domain.ErrMigrationTargetInvalid
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO price_migration_offers (migration_offer_id, product_id, from_price_version_id, to_price_version_id,
				eligibility_mode, accept_by, reason, status, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'DRAFT', $8, $9)`,
			o.MigrationOfferID, from.ProductID, from.PriceVersionID, to.PriceVersionID, o.EligibilityMode, o.AcceptBy,
			o.Reason, o.CreatedAt, o.CreatedByPrincipalID); err != nil {
			return err
		}
		for _, sub := range o.TargetSubscriptionIDs {
			if _, err := tx.Exec(ctx, `INSERT INTO price_migration_offer_targets (migration_offer_id, subscription_id) VALUES ($1, $2)`,
				o.MigrationOfferID, sub); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23503" {
					return &domain.ValidationError{Field: "target_subscription_ids", Reason: sub + " is not a subscription"}
				}
				return err
			}
		}
		if out, err = loadMigrationOffer(ctx, tx, o.MigrationOfferID, false); err != nil {
			return err
		}
		return migrationEvent(ctx, tx, "migration_offer.created", out)
	})
	return out, err
}

// PublishMigrationOffer makes an offer available to its eligible
// subscribers. It needs someone other than its creator, and an eligibility
// rule that names who the offer is for (negative path #38).
func (s *PgStore) PublishMigrationOffer(ctx context.Context, offerID string, expectedVersion int, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.MigrationOffer, error) {
	var out *domain.MigrationOffer
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		o, err := loadMigrationOffer(ctx, tx, offerID, true)
		if err != nil {
			return err
		}
		if o.Status != domain.MigrationOfferDraft {
			return fmt.Errorf("%w: offer is %s", domain.ErrMigrationOfferInvalidState, o.Status)
		}
		if o.RowVersion != expectedVersion {
			return domain.ErrVersionConflict
		}
		if actor == o.CreatedByPrincipalID {
			return domain.ErrSoDViolation
		}
		if o.EligibilityMode == nil || (*o.EligibilityMode == domain.EligibilityListedSubscriptions && len(o.TargetSubscriptionIDs) == 0) {
			return domain.ErrMigrationEligibilityMissing
		}
		if o.AcceptBy != nil && !o.AcceptBy.After(now) {
			return &domain.PublicationBlockedError{Reasons: []string{"accept_by has already passed"}}
		}
		to, err := loadVersion(ctx, tx, o.ToPriceVersionID, false)
		if err != nil {
			return err
		}
		if to.Status != domain.PriceVersionPublished {
			return domain.ErrMigrationTargetInvalid
		}
		if _, err := tx.Exec(ctx, `
			UPDATE price_migration_offers SET status = 'PUBLISHED', row_version = row_version + 1,
				published_at = $2, published_by_principal_id = $3 WHERE migration_offer_id = $1`, offerID, now, actor); err != nil {
			return err
		}
		if out, err = loadMigrationOffer(ctx, tx, offerID, false); err != nil {
			return err
		}
		return migrationEvent(ctx, tx, "migration_offer.published", out)
	})
	return out, err
}

func (s *PgStore) WithdrawMigrationOffer(ctx context.Context, offerID string, expectedVersion int, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.MigrationOffer, error) {
	var out *domain.MigrationOffer
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		o, err := loadMigrationOffer(ctx, tx, offerID, true)
		if err != nil {
			return err
		}
		if o.Status == domain.MigrationOfferWithdrawn {
			return fmt.Errorf("%w: offer is already withdrawn", domain.ErrMigrationOfferInvalidState)
		}
		if o.RowVersion != expectedVersion {
			return domain.ErrVersionConflict
		}
		if _, err := tx.Exec(ctx, `
			UPDATE price_migration_offers SET status = 'WITHDRAWN', row_version = row_version + 1,
				withdrawn_at = $2, withdrawn_by_principal_id = $3, withdraw_reason = $4 WHERE migration_offer_id = $1`,
			offerID, now, actor, reason); err != nil {
			return err
		}
		if out, err = loadMigrationOffer(ctx, tx, offerID, false); err != nil {
			return err
		}
		return migrationEvent(ctx, tx, "migration_offer.withdrawn", out)
	})
	return out, err
}

func (s *PgStore) GetMigrationOffer(ctx context.Context, offerID string) (*domain.MigrationOffer, error) {
	var out *domain.MigrationOffer
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		o, err := loadMigrationOffer(ctx, tx, offerID, false)
		out = o
		return err
	})
	return out, err
}

// eligibleOffer loads a published offer the subscription may accept now.
// From a tenant transaction only published offers, and only target rows for
// the tenant's own subscriptions, are visible.
func eligibleOffer(ctx context.Context, tx pgx.Tx, offerID, subscriptionID string, now time.Time) (*domain.MigrationOffer, error) {
	o, err := loadMigrationOffer(ctx, tx, offerID, false)
	if err != nil {
		return nil, err
	}
	if o.Status != domain.MigrationOfferPublished || (o.AcceptBy != nil && !o.AcceptBy.After(now)) {
		return nil, fmt.Errorf("%w: the offer is not open", domain.ErrNotEligibleForMigration)
	}
	if *o.EligibilityMode == domain.EligibilityListedSubscriptions {
		listed := false
		for _, id := range o.TargetSubscriptionIDs {
			listed = listed || id == subscriptionID
		}
		if !listed {
			return nil, domain.ErrNotEligibleForMigration
		}
	}
	return o, nil
}

// ListMigrationOffersFor returns the open offers a subscription can accept.
func (s *PgStore) ListMigrationOffersFor(ctx context.Context, subscriptionID string, now time.Time) ([]domain.MigrationOffer, error) {
	out := []domain.MigrationOffer{}
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		a, err := loadAggregate(ctx, tx, subscriptionID, false)
		if err != nil {
			return err
		}
		cur := effectiveAt(a.versions, now)
		if cur == nil {
			return nil
		}
		plan := a.planOf(cur)
		rows, err := tx.Query(ctx, `SELECT migration_offer_id FROM price_migration_offers
			WHERE status = 'PUBLISHED' AND from_price_version_id = $1 ORDER BY published_at`, plan.PriceVersionID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			o, err := eligibleOffer(ctx, tx, id, subscriptionID, now)
			if errors.Is(err, domain.ErrNotEligibleForMigration) {
				continue
			}
			if err != nil {
				return err
			}
			o.TargetSubscriptionIDs = nil // a customer sees whether it is eligible, not who else is
			out = append(out, *o)
		}
		return nil
	})
	return out, err
}

// ── Boundary queue ───────────────────────────────────────────────────────────

const (
	boundaryWorkerActor = "system:boundary-worker"
	boundaryMaxAttempts = 10
)

type boundaryItem struct {
	id        string
	org       string
	subID     string
	kind      string
	versionID *string
	termNo    *int
	attempts  int
}

// boundaryBackoff doubles from one minute up to an hour.
func boundaryBackoff(attempts int) time.Duration {
	d := time.Minute << attempts
	if d > time.Hour || d <= 0 {
		return time.Hour
	}
	return d
}

var versionEventTypes = map[domain.ChangeType]string{
	domain.ChangeTrialConversion:       "subscription.activated",
	domain.ChangeActivated:             "subscription.activated",
	domain.ChangeTrialExpiry:           "subscription.expired",
	domain.ChangeTermExpiry:            "subscription.expired",
	domain.ChangeCancellationEffective: "subscription.canceled",
	domain.ChangePlanChanged:           "subscription.changed",
	domain.ChangePriceMigrated:         "subscription.changed",
	domain.ChangeQuantityChanged:       "subscription.quantity_changed",
	domain.ChangeAddOnChanged:          "subscription.add_on_changed",
}

// ProcessNextBoundary claims one due boundary and processes it, all in one
// transaction: SKIP LOCKED lets any number of workers run side by side
// without two ever taking the same item. It reports whether an item was
// found; a failure is recorded against the item with a backoff (and parked
// as FAILED after boundaryMaxAttempts) so one bad item never blocks the
// queue behind it.
func (s *PgStore) ProcessNextBoundary(ctx context.Context, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.commercial_plane', 'boundary_worker', true)"); err != nil {
		return false, err
	}
	var it boundaryItem
	err = tx.QueryRow(ctx, `
		SELECT boundary_id::text, organization_id::text, subscription_id, kind, subscription_version_id, term_no, attempts
		FROM subscription_boundary_queue
		WHERE status = 'PENDING' AND next_attempt_at <= $1
		ORDER BY next_attempt_at, boundary_id
		LIMIT 1 FOR UPDATE SKIP LOCKED`, now).Scan(&it.id, &it.org, &it.subID, &it.kind, &it.versionID, &it.termNo, &it.attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", it.org); err != nil {
		return false, err
	}

	outcome, perr := processBoundary(ctx, tx, it, now)
	if perr == nil {
		_, perr = tx.Exec(ctx, `UPDATE subscription_boundary_queue SET status = 'DONE', processed_at = $2, outcome = $3
			WHERE boundary_id = $1::uuid`, it.id, now, outcome)
	}
	if perr == nil {
		if perr = tx.Commit(ctx); perr == nil {
			return true, nil
		}
	}
	_ = tx.Rollback(ctx)
	if rerr := s.recordBoundaryFailure(ctx, it, perr, now); rerr != nil {
		return true, fmt.Errorf("boundary %s failed (%v) and the failure could not be recorded: %w", it.id, perr, rerr)
	}
	return true, fmt.Errorf("boundary %s: %w", it.id, mapSubscriptionErr(perr))
}

func (s *PgStore) recordBoundaryFailure(ctx context.Context, it boundaryItem, cause error, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.commercial_plane', 'boundary_worker', true)"); err != nil {
		return err
	}
	attempts := it.attempts + 1
	if attempts >= boundaryMaxAttempts {
		_, err = tx.Exec(ctx, `UPDATE subscription_boundary_queue SET attempts = $2, last_error = $3, status = 'FAILED',
			processed_at = $4, outcome = 'FAILED' WHERE boundary_id = $1::uuid`, it.id, attempts, cause.Error(), now)
	} else {
		_, err = tx.Exec(ctx, `UPDATE subscription_boundary_queue SET attempts = $2, last_error = $3, next_attempt_at = $4
			WHERE boundary_id = $1::uuid`, it.id, attempts, cause.Error(), now.Add(boundaryBackoff(attempts)))
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// processBoundary decides what a due boundary means now. Nothing here trusts
// the state at scheduling time: a version voided since, a term already
// renewed, a subscription that has since ended — each is skipped, with the
// reason recorded as the outcome.
func processBoundary(ctx context.Context, tx pgx.Tx, it boundaryItem, now time.Time) (string, error) {
	a, err := loadAggregate(ctx, tx, it.subID, true)
	if err != nil {
		return "", err
	}
	m := changeMeta{changeID: domain.NewCommercialID(domain.PrefixCommercialChange), actor: boundaryWorkerActor,
		channel: domain.ChannelSystem, now: now}

	switch it.kind {
	case "VERSION_EFFECTIVE":
		for i := range a.versions {
			v := &a.versions[i]
			if v.SubscriptionVersionID != *it.versionID {
				continue
			}
			if v.VoidedAt != nil {
				return "SKIPPED_VOIDED", nil
			}
			eventType, ok := versionEventTypes[v.ChangeType]
			if !ok {
				return "SKIPPED_NO_EVENT", nil
			}
			m.changeID = v.ChangeID
			return "EVENT_PUBLISHED", emitSubscriptionEvent(ctx, tx, eventType, a.sub, m, subscriptionEvent{
				ChangeType: v.ChangeType, Status: v.LifecycleStatus, EffectiveAt: v.EffectiveFrom, EndsAt: a.sub.EndsAt,
			})
		}
		return "", fmt.Errorf("version %s not found on %s", *it.versionID, it.subID)

	case "TERM_END":
		lt := liveTerms(a.terms)
		var term *domain.SubscriptionTerm
		for i := range a.terms {
			if a.terms[i].TermNo == *it.termNo {
				term = &a.terms[i]
			}
		}
		switch {
		case term == nil:
			return "", fmt.Errorf("term %d not found on %s", *it.termNo, it.subID)
		case term.VoidedAt != nil:
			return "SKIPPED_VOIDED", nil
		case !term.AutoRenew:
			return "SKIPPED_NOT_AUTO_RENEW", nil
		case len(lt) > 0 && lt[len(lt)-1].TermNo != term.TermNo:
			return "SKIPPED_ALREADY_RENEWED", nil
		case a.sub.EndsAt != nil && !a.sub.EndsAt.After(term.EndsAt):
			return "SKIPPED_ENDED", nil
		}
		return "RENEWED", renewApply(ctx, tx, a, m)
	}
	return "", fmt.Errorf("unknown boundary kind %q", it.kind)
}
