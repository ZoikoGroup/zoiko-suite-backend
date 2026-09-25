// COM-02 configuration changes, transition rules and proration evidence
// (migration 000008).
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/outbox"
)

// TransitionRuleStore manages plan transition rules on the seller plane.
type TransitionRuleStore interface {
	CreateTransitionRule(ctx context.Context, r *domain.PlanTransitionRule, claim domain.IdempotencyClaim) (*domain.PlanTransitionRule, error)
	RetireTransitionRule(ctx context.Context, ruleID, actor, reason string, now time.Time) (*domain.PlanTransitionRule, error)
	ListTransitionRules(ctx context.Context, fromProductCode string) ([]domain.PlanTransitionRule, error)
}

var _ TransitionRuleStore = (*PgStore)(nil)

// ── Transition rules ─────────────────────────────────────────────────────────

const transitionRuleSelect = `
	SELECT r.rule_id, r.from_product_id, pf.product_code, r.to_product_id, pt.product_code, r.timing,
	       r.proration_method, r.created_at, r.created_by_principal_id, r.retired_at, r.retired_by_principal_id,
	       r.retire_reason
	FROM plan_transition_rules r
	JOIN commercial_products pf ON pf.product_id = r.from_product_id
	JOIN commercial_products pt ON pt.product_id = r.to_product_id`

func scanTransitionRule(row pgx.Row) (*domain.PlanTransitionRule, error) {
	var r domain.PlanTransitionRule
	if err := row.Scan(&r.RuleID, &r.FromProductID, &r.FromProductCode, &r.ToProductID, &r.ToProductCode, &r.Timing,
		&r.ProrationMethod, &r.CreatedAt, &r.CreatedByPrincipalID, &r.RetiredAt, &r.RetiredByPrincipalID,
		&r.RetireReason); err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateTransitionRule records one allowed change. r.FromProductCode and
// r.ToProductCode name the plans; their ids are resolved here.
func (s *PgStore) CreateTransitionRule(ctx context.Context, r *domain.PlanTransitionRule, claim domain.IdempotencyClaim) (*domain.PlanTransitionRule, error) {
	var out *domain.PlanTransitionRule
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		for _, code := range []string{r.FromProductCode, r.ToProductCode} {
			var kind domain.ProductKind
			err := tx.QueryRow(ctx, `SELECT product_kind FROM commercial_products WHERE product_code = $1`, code).Scan(&kind)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: %s", domain.ErrProductNotFound, code)
			}
			if err != nil {
				return err
			}
			if kind != domain.ProductKindPlan {
				return fmt.Errorf("%w: transition rules are between plans; %s is %s", domain.ErrWrongProductKind, code, kind)
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO plan_transition_rules (rule_id, from_product_id, to_product_id, timing, proration_method,
				created_at, created_by_principal_id)
			SELECT $1, pf.product_id, pt.product_id, $4, $5, $6, $7
			FROM commercial_products pf, commercial_products pt
			WHERE pf.product_code = $2 AND pt.product_code = $3`,
			r.RuleID, r.FromProductCode, r.ToProductCode, r.Timing, r.ProrationMethod, r.CreatedAt, r.CreatedByPrincipalID); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.ErrTransitionRuleExists
			}
			return err
		}
		got, err := scanTransitionRule(tx.QueryRow(ctx, transitionRuleSelect+` WHERE r.rule_id = $1`, r.RuleID))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "plan_transition_rule", AggregateID: got.RuleID,
			EventType: "plan_transition_rule.created", Payload: got})
	})
	return out, err
}

// RetireTransitionRule ends a rule. It is naturally idempotent in effect —
// a retired rule stays retired — so a repeat is refused rather than replayed.
func (s *PgStore) RetireTransitionRule(ctx context.Context, ruleID, actor, reason string, now time.Time) (*domain.PlanTransitionRule, error) {
	var out *domain.PlanTransitionRule
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		cur, err := scanTransitionRule(tx.QueryRow(ctx, transitionRuleSelect+` WHERE r.rule_id = $1 FOR UPDATE OF r`, ruleID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrTransitionRuleNotFound
		}
		if err != nil {
			return err
		}
		if cur.RetiredAt != nil {
			return domain.ErrTransitionRuleRetired
		}
		if _, err := tx.Exec(ctx, `
			UPDATE plan_transition_rules SET retired_at = $2, retired_by_principal_id = $3, retire_reason = $4
			WHERE rule_id = $1`, ruleID, now, actor, reason); err != nil {
			return err
		}
		out, err = scanTransitionRule(tx.QueryRow(ctx, transitionRuleSelect+` WHERE r.rule_id = $1`, ruleID))
		if err != nil {
			return err
		}
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "plan_transition_rule", AggregateID: ruleID,
			EventType: "plan_transition_rule.retired", Payload: out})
	})
	return out, err
}

func (s *PgStore) ListTransitionRules(ctx context.Context, fromProductCode string) ([]domain.PlanTransitionRule, error) {
	var out []domain.PlanTransitionRule
	err := s.catalogTx(ctx, false, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, transitionRuleSelect+` WHERE ($1 = '' OR pf.product_code = $1)
			ORDER BY pf.product_code, pt.product_code, r.created_at`, fromProductCode)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanTransitionRule(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// ── Quotes ───────────────────────────────────────────────────────────────────

func productCodeOf(pvs map[string]*domain.PriceVersion, it domain.SubscriptionItem) string {
	if pv := pvs[it.PriceVersionID]; pv != nil {
		return pv.ProductCode
	}
	return ""
}

func renumber(items []domain.SubscriptionItem) []domain.SubscriptionItem {
	out := make([]domain.SubscriptionItem, len(items))
	copy(out, items)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ItemRole == "PLAN" && out[j].ItemRole != "PLAN" })
	for i := range out {
		out[i].ItemNo = i + 1
	}
	return out
}

// buildQuote works out what a change would do, from the locked aggregate.
// Existing items keep their bound price versions; only a product being added
// binds to the current sellable offer in the subscription's own currency and
// market. The same function serves preview and confirmation, so what is
// confirmed can never differ from what was shown.
func buildQuote(ctx context.Context, tx pgx.Tx, a *subscriptionAggregate, req domain.ChangeRequest, now time.Time) (*domain.ChangeQuote, []domain.SubscriptionItem, error) {
	cur := effectiveAt(a.versions, now)
	if cur == nil || cur.LifecycleStatus != domain.LifecycleActive {
		return nil, nil, fmt.Errorf("%w: only an ACTIVE subscription can change its configuration", domain.ErrSubscriptionInvalidState)
	}
	if len(scheduledAfter(a.versions, now)) > 0 {
		return nil, nil, domain.ErrChangeAlreadyScheduled
	}
	pvs := make(map[string]*domain.PriceVersion, len(a.pvs))
	for k, v := range a.pvs {
		pvs[k] = v
	}
	curPlan := a.planOf(cur)
	resolve := func(code string, kind domain.ProductKind) (*domain.PriceVersion, error) {
		vs, err := resolveOffers(ctx, tx, domain.SellableOfferFilter{ProductCode: code, CurrencyCode: a.sub.CurrencyCode, MarketCode: a.sub.MarketCode}, now)
		if err != nil {
			return nil, err
		}
		if len(vs) != 1 {
			return nil, fmt.Errorf("%w: no sellable offer for %s in %s/%s", domain.ErrPriceVersionNotSellable, code, a.sub.CurrencyCode, a.sub.MarketCode)
		}
		if vs[0].ProductKind != kind {
			return nil, domain.ErrWrongProductKind
		}
		pvs[vs[0].PriceVersionID] = vs[0]
		return vs[0], nil
	}

	var planItem domain.SubscriptionItem
	var addOns []domain.SubscriptionItem
	for _, it := range cur.Items {
		if it.ItemRole == "PLAN" {
			planItem = it
		} else {
			addOns = append(addOns, it)
		}
	}
	target := curPlan

	switch req.Kind {
	case domain.ChangeKindPlan:
		np, err := resolve(req.PlanProductCode, domain.ProductKindPlan)
		if err != nil {
			return nil, nil, err
		}
		if np.ProductID == curPlan.ProductID {
			return nil, nil, &domain.ValidationError{Field: "product_code", Reason: "this is the current plan; change its quantities instead"}
		}
		if req.PlanAcceptedTermsSHA256 != np.Terms.TermsDocumentSHA256 {
			return nil, nil, domain.ErrTermsNotAccepted
		}
		q, err := domain.ValidateQuantities(np, req.PlanQuantities)
		if err != nil {
			return nil, nil, err
		}
		planItem = domain.SubscriptionItem{ItemRole: "PLAN", PriceVersionID: np.PriceVersionID,
			PriceContentSHA256: *np.ContentSHA256, AcceptedTermsSHA256: req.PlanAcceptedTermsSHA256, Quantities: q}
		target = np
	case domain.ChangeKindQuantity:
		q, err := domain.ValidateQuantities(curPlan, req.PlanQuantities)
		if err != nil {
			return nil, nil, err
		}
		planItem.Quantities = q
	case domain.ChangeKindAddOn:
		if req.AddOn != nil {
			for _, it := range addOns {
				if productCodeOf(pvs, it) == req.AddOn.ProductCode {
					return nil, nil, &domain.ValidationError{Field: "product_code", Reason: "this add-on is already on the subscription"}
				}
			}
			ao, err := resolve(req.AddOn.ProductCode, domain.ProductKindAddOn)
			if err != nil {
				return nil, nil, err
			}
			if req.AddOn.AcceptedTermsSHA256 != ao.Terms.TermsDocumentSHA256 {
				return nil, nil, domain.ErrTermsNotAccepted
			}
			q, err := domain.ValidateQuantities(ao, req.AddOn.Quantities)
			if err != nil {
				return nil, nil, err
			}
			addOns = append(addOns, domain.SubscriptionItem{ItemRole: "ADD_ON", PriceVersionID: ao.PriceVersionID,
				PriceContentSHA256: *ao.ContentSHA256, AcceptedTermsSHA256: req.AddOn.AcceptedTermsSHA256, Quantities: q})
		} else {
			kept := addOns[:0:0]
			for _, it := range addOns {
				if productCodeOf(pvs, it) != req.RemoveAddOnCode {
					kept = append(kept, it)
				}
			}
			if len(kept) == len(addOns) {
				return nil, nil, &domain.ValidationError{Field: "product_code", Reason: "this add-on is not on the subscription"}
			}
			addOns = kept
		}
	case domain.ChangeKindMigration:
		offer, err := eligibleOffer(ctx, tx, req.MigrationOfferID, a.sub.SubscriptionID, now)
		if err != nil {
			return nil, nil, err
		}
		if offer.FromPriceVersionID != curPlan.PriceVersionID {
			return nil, nil, fmt.Errorf("%w: the subscription is not on the offer's price version", domain.ErrNotEligibleForMigration)
		}
		np, err := loadVersion(ctx, tx, offer.ToPriceVersionID, false)
		if err != nil {
			return nil, nil, err
		}
		pvs[np.PriceVersionID] = np
		if req.PlanAcceptedTermsSHA256 != np.Terms.TermsDocumentSHA256 {
			return nil, nil, domain.ErrTermsNotAccepted
		}
		keep := map[string]string{}
		for _, x := range planItem.Quantities {
			keep[x.ComponentKey] = x.Quantity
		}
		q, err := domain.ValidateQuantities(np, keep)
		if err != nil {
			return nil, nil, err
		}
		planItem = domain.SubscriptionItem{ItemRole: "PLAN", PriceVersionID: np.PriceVersionID,
			PriceContentSHA256: *np.ContentSHA256, AcceptedTermsSHA256: req.PlanAcceptedTermsSHA256, Quantities: q}
		target = np
	default:
		return nil, nil, fmt.Errorf("unknown change kind %q", req.Kind)
	}

	items := renumber(append([]domain.SubscriptionItem{planItem}, addOns...))
	if domain.ItemsEqual(cur.Items, items) {
		return nil, nil, domain.ErrNoChange
	}

	// A migration is governed by its published offer and always lands at
	// the next renewal; every other change needs an active transition rule.
	var ruleID string
	timing, method := domain.TimingNextRenewal, domain.ProrationNone
	if req.Kind != domain.ChangeKindMigration {
		rule, err := scanTransitionRule(tx.QueryRow(ctx, transitionRuleSelect+`
			WHERE r.from_product_id = $1 AND r.to_product_id = $2 AND r.retired_at IS NULL`, curPlan.ProductID, target.ProductID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fmt.Errorf("%w: %s -> %s", domain.ErrTransitionNotAllowed, curPlan.ProductCode, target.ProductCode)
		}
		if err != nil {
			return nil, nil, err
		}
		ruleID, timing, method = rule.RuleID, rule.Timing, rule.ProrationMethod
		if req.ForceNextRenewal {
			timing, method = domain.TimingNextRenewal, domain.ProrationNone
		}
	}

	spans, lt := a.termSpans()
	i := termIndexAt(lt, now)
	if i < 0 {
		return nil, nil, fmt.Errorf("%w: the subscription has no current term", domain.ErrSubscriptionInvalidState)
	}
	term := lt[i]
	q := &domain.ChangeQuote{SubscriptionID: a.sub.SubscriptionID, Kind: req.Kind, RuleID: ruleID, MigrationOfferID: req.MigrationOfferID, Timing: timing,
		ProrationMethod: method, FromPlanPriceVersionID: curPlan.PriceVersionID, ToPlanPriceVersionID: target.PriceVersionID,
		CurrencyCode: a.sub.CurrencyCode, Items: items}
	switch timing {
	case domain.TimingImmediate:
		if spans[i].Interval != target.BillingInterval || spans[i].IntervalCount != target.BillingIntervalCount {
			return nil, nil, domain.ErrTimingNotAllowed
		}
		q.EffectiveAt = now
	case domain.TimingNextRenewal:
		if a.sub.EndsAt != nil && !a.sub.EndsAt.After(term.EndsAt) {
			return nil, nil, fmt.Errorf("%w: the subscription ends at this term's end", domain.ErrSubscriptionInvalidState)
		}
		q.EffectiveAt = term.EndsAt
	}
	if req.Kind == domain.ChangeKindMigration && q.EffectiveAt.Before(target.EffectiveFrom) {
		return nil, nil, fmt.Errorf("%w: the new price is not in effect by this renewal", domain.ErrNotEligibleForMigration)
	}

	oldCharge, err := domain.ConfigurationPeriodCharge(cur.Items, pvs)
	if err != nil {
		return nil, nil, err
	}
	newCharge, err := domain.ConfigurationPeriodCharge(items, pvs)
	if err != nil {
		return nil, nil, err
	}
	q.OldPeriodCharge, q.NewPeriodCharge = domain.EvidenceAmount(oldCharge), domain.EvidenceAmount(newCharge)
	if timing == domain.TimingImmediate && method == domain.ProrationDailyHalfEven {
		currency, err := loadCurrency(ctx, tx, a.sub.CurrencyCode)
		if err != nil {
			return nil, nil, err
		}
		p := domain.Prorate(oldCharge, newCharge, term.StartsAt, term.EndsAt, now, currency.MinorUnits)
		q.Proration = &p
	}
	q.Seal()
	return q, items, nil
}

// PreviewChange answers PreviewPlanChange: the quote a customer must see,
// and confirm by its hash, before the change can be made.
func (s *PgStore) PreviewChange(ctx context.Context, subscriptionID string, req domain.ChangeRequest, now time.Time) (*domain.ChangeQuote, error) {
	var out *domain.ChangeQuote
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		a, err := loadAggregate(ctx, tx, subscriptionID, false)
		if err != nil {
			return err
		}
		out, _, err = buildQuote(ctx, tx, a, req, now)
		return err
	})
	return out, err
}

// RequestChange applies a configuration change atomically with its
// effective time (COM-CTRL-008): one new version, its evidence row and its
// event commit together or not at all. The confirmation must carry the hash
// of the quote the customer saw; if anything changed since — a new day of
// proration, a new offer, a new rule — it is refused, not silently repriced.
func (s *PgStore) RequestChange(ctx context.Context, c domain.SubscriptionCommand, req domain.ChangeRequest, expectedQuoteSHA256 string, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	return s.subscriptionCommand(ctx, c, claim, func(tx pgx.Tx, a *subscriptionAggregate, m changeMeta) error {
		q, items, err := buildQuote(ctx, tx, a, req, c.Now)
		if err != nil {
			return err
		}
		if q.QuoteSHA256 != expectedQuoteSHA256 {
			return domain.ErrQuoteChanged
		}
		changeType := map[domain.ChangeKind]domain.ChangeType{
			domain.ChangeKindPlan: domain.ChangePlanChanged, domain.ChangeKindQuantity: domain.ChangeQuantityChanged,
			domain.ChangeKindAddOn: domain.ChangeAddOnChanged, domain.ChangeKindMigration: domain.ChangePriceMigrated,
		}[req.Kind]
		lp := domain.LifecyclePlan{Versions: []domain.PlannedVersion{
			{Status: domain.LifecycleActive, ChangeType: changeType, EffectiveFrom: q.EffectiveAt},
		}}
		if err := a.applyPlan(ctx, tx, lp, items, nil, m); err != nil {
			return err
		}

		_, lt := a.termSpans()
		term := lt[termIndexAt(lt, c.Now)]
		var days, remaining *int
		var credit, charge, net, ruleID, offerID *string
		if q.RuleID != "" {
			ruleID = &q.RuleID
		}
		if q.MigrationOfferID != "" {
			offerID = &q.MigrationOfferID
		}
		if q.Proration != nil {
			days, remaining = &q.Proration.DaysInTerm, &q.Proration.DaysRemaining
			credit, charge, net = &q.Proration.Credit, &q.Proration.Charge, &q.Proration.Net
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO subscription_changes (change_id, subscription_id, change_kind, rule_id, timing, proration_method,
				effective_at, from_price_version_id, to_price_version_id, currency_code, old_period_charge, new_period_charge,
				term_starts_at, term_ends_at, days_in_term, days_remaining, proration_credit, proration_charge, proration_net,
				quote_sha256, channel, customer_basis_ref, requested_by_principal_id, created_at, migration_offer_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric, $12::numeric, $13, $14, $15, $16,
			        $17::numeric, $18::numeric, $19::numeric, $20, $21, $22, $23, $24, $25)`,
			c.ChangeID, a.sub.SubscriptionID, req.Kind, ruleID, q.Timing, q.ProrationMethod, q.EffectiveAt,
			q.FromPlanPriceVersionID, q.ToPlanPriceVersionID, q.CurrencyCode, q.OldPeriodCharge, q.NewPeriodCharge,
			term.StartsAt, term.EndsAt, days, remaining, credit, charge, net, q.QuoteSHA256, c.Channel,
			c.CustomerBasisRef, c.Actor, c.Now, offerID); err != nil {
			return err
		}
		if err := setEndsAt(ctx, tx, a.sub.SubscriptionID, a.sub.EndsAt); err != nil {
			return err
		}
		eventType := map[domain.ChangeKind]string{
			domain.ChangeKindPlan: "subscription.changed", domain.ChangeKindQuantity: "subscription.quantity_changed",
			domain.ChangeKindAddOn: "subscription.add_on_changed", domain.ChangeKindMigration: "subscription.changed",
		}[req.Kind]
		return emitSubscriptionEvent(ctx, tx, eventType, a.sub, m, subscriptionEvent{
			ChangeType: changeType, Status: domain.LifecycleActive, EffectiveAt: q.EffectiveAt,
			PriceVersionID: q.ToPlanPriceVersionID,
		})
	})
}

// GetChanges returns the evidence for every configuration change.
func (s *PgStore) GetChanges(ctx context.Context, subscriptionID string) ([]domain.SubscriptionChange, error) {
	var out []domain.SubscriptionChange
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		if _, err := loadSubscriptionRow(ctx, tx, subscriptionID, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT change_id, subscription_id, change_kind, rule_id, timing, proration_method, effective_at,
			       from_price_version_id, to_price_version_id, currency_code, old_period_charge::text,
			       new_period_charge::text, term_starts_at, term_ends_at, days_in_term, days_remaining,
			       proration_credit::text, proration_charge::text, proration_net::text, quote_sha256, channel,
			       customer_basis_ref, requested_by_principal_id, created_at, migration_offer_id
			FROM subscription_changes WHERE subscription_id = $1 ORDER BY created_at, change_id`, subscriptionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ch domain.SubscriptionChange
			var days, remaining *int
			var credit, charge, net *string
			if err := rows.Scan(&ch.ChangeID, &ch.SubscriptionID, &ch.Kind, &ch.RuleID, &ch.Timing, &ch.ProrationMethod,
				&ch.EffectiveAt, &ch.FromPriceVersionID, &ch.ToPriceVersionID, &ch.CurrencyCode, &ch.OldPeriodCharge,
				&ch.NewPeriodCharge, &ch.TermStartsAt, &ch.TermEndsAt, &days, &remaining, &credit, &charge, &net,
				&ch.QuoteSHA256, &ch.Channel, &ch.CustomerBasisRef, &ch.RequestedByPrincipalID, &ch.CreatedAt,
				&ch.MigrationOfferID); err != nil {
				return err
			}
			if net != nil {
				ch.Proration = &domain.Proration{Method: ch.ProrationMethod, DaysInTerm: *days, DaysRemaining: *remaining,
					Credit: *credit, Charge: *charge, Net: *net}
			}
			out = append(out, ch)
		}
		return rows.Err()
	})
	return out, err
}
