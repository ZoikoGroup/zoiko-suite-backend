// COM-02 Subscription persistence (migration 000007).
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

// SubscriptionStore is the COM-02 persistence contract. Every method runs in
// the tenant scope carried by ctx; an assisted (operator) request has that
// scope set to the customer's organization by the handler, after the
// operator's platform permission has been checked.
type SubscriptionStore interface {
	SetAccountMarket(ctx context.Context, accountID, marketCode, actor string, now time.Time) error
	StartSubscription(ctx context.Context, p domain.StartSubscriptionParams, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error)
	ActivateSubscription(ctx context.Context, c domain.SubscriptionCommand, customerConfirmation bool, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error)
	ScheduleCancellation(ctx context.Context, c domain.SubscriptionCommand, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error)
	CancelNow(ctx context.Context, c domain.SubscriptionCommand, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error)
	Reactivate(ctx context.Context, c domain.SubscriptionCommand, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error)
	Renew(ctx context.Context, c domain.SubscriptionCommand, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error)
	GetSubscriptionAsOf(ctx context.Context, subscriptionID string, at time.Time) (*domain.SubscriptionView, error)
	GetEffectiveVersion(ctx context.Context, subscriptionID string, at time.Time) (*domain.SubscriptionVersion, error)
	GetChangeHistory(ctx context.Context, subscriptionID string) ([]domain.SubscriptionVersion, error)
	GetRenewalState(ctx context.Context, subscriptionID string, now time.Time) (*domain.RenewalState, error)
	PreviewChange(ctx context.Context, subscriptionID string, req domain.ChangeRequest, now time.Time) (*domain.ChangeQuote, error)
	RequestChange(ctx context.Context, c domain.SubscriptionCommand, req domain.ChangeRequest, expectedQuoteSHA256 string, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error)
	GetChanges(ctx context.Context, subscriptionID string) ([]domain.SubscriptionChange, error)
}

var _ SubscriptionStore = (*PgStore)(nil)

// ErrNoEffectiveVersion is returned when a subscription had not started at
// the requested instant.
var ErrNoEffectiveVersion = errors.New("the subscription had no effective version at that time")

func mapSubscriptionErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23P01":
		if pgErr.ConstraintName == "subscriptions_no_overlap" {
			return domain.ErrSubscriptionOverlap
		}
	case "CP002":
		return domain.ErrCommercialAccountNotFound
	case "CP003":
		return domain.ErrLegacySubscriptionLive
	case "CP001":
		return fmt.Errorf("%w: %s", domain.ErrSubscriptionInvalidState, pgErr.Message)
	}
	return err
}

func (s *PgStore) subscriptionTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return mapSubscriptionErr(s.withTenant(ctx, fn))
}

// SetAccountMarket records the market a customer buys in. It is seller-side
// data: it decides which offers the account is sold, never the account's own
// tax or business settings.
func (s *PgStore) SetAccountMarket(ctx context.Context, accountID, marketCode, actor string, now time.Time) error {
	return s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE commercial_accounts
			   SET market_code = $2, market_set_at = $3, market_set_by_principal_id = $4, updated_at = $3
			 WHERE commercial_account_id = $1`, accountID, marketCode, now, actor)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrCommercialAccountNotFound
		}
		return nil
	})
}

// ── Aggregate loading ────────────────────────────────────────────────────────

type subscriptionAggregate struct {
	sub      *domain.Subscription
	versions []domain.SubscriptionVersion // every version, voided included, by version_number
	terms    []domain.SubscriptionTerm    // every term, voided included, by term_no
	plan     *domain.PriceVersion         // plan of the latest live version
	pvs      map[string]*domain.PriceVersion
}

const subscriptionColumns = `subscription_id, organization_id::text, commercial_account_id::text, product_id,
	currency_code, market_code, billing_source, channel, customer_basis_ref, payment_method_ref,
	starts_at, ends_at, row_version, created_at, created_by_principal_id`

func loadSubscriptionRow(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (*domain.Subscription, error) {
	q := `SELECT ` + subscriptionColumns + ` FROM subscriptions WHERE subscription_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var s domain.Subscription
	err := tx.QueryRow(ctx, q, id).Scan(&s.SubscriptionID, &s.OrganizationID, &s.CommercialAccountID, &s.ProductID,
		&s.CurrencyCode, &s.MarketCode, &s.BillingSource, &s.Channel, &s.CustomerBasisRef, &s.PaymentMethodRef,
		&s.StartsAt, &s.EndsAt, &s.RowVersion, &s.CreatedAt, &s.CreatedByPrincipalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrSubscriptionNotFound
	}
	return &s, err
}

func loadAggregate(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (*subscriptionAggregate, error) {
	sub, err := loadSubscriptionRow(ctx, tx, id, forUpdate)
	if err != nil {
		return nil, err
	}
	agg := &subscriptionAggregate{sub: sub}

	rows, err := tx.Query(ctx, `
		SELECT subscription_version_id, subscription_id, version_number, lifecycle_status, change_type,
		       effective_from, trial_ends_at, change_id, reason, channel, customer_basis_ref, created_at,
		       created_by_principal_id, voided_at, voided_by_principal_id, void_reason, voided_by_change_id
		FROM subscription_versions WHERE subscription_id = $1 ORDER BY version_number`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v domain.SubscriptionVersion
		if err := rows.Scan(&v.SubscriptionVersionID, &v.SubscriptionID, &v.VersionNumber, &v.LifecycleStatus, &v.ChangeType,
			&v.EffectiveFrom, &v.TrialEndsAt, &v.ChangeID, &v.Reason, &v.Channel, &v.CustomerBasisRef, &v.CreatedAt,
			&v.CreatedByPrincipalID, &v.VoidedAt, &v.VoidedByPrincipalID, &v.VoidReason, &v.VoidedByChangeID); err != nil {
			rows.Close()
			return nil, err
		}
		v.Items = []domain.SubscriptionItem{}
		agg.versions = append(agg.versions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	byVersion := map[string]*domain.SubscriptionVersion{}
	for i := range agg.versions {
		byVersion[agg.versions[i].SubscriptionVersionID] = &agg.versions[i]
	}
	itemRows, err := tx.Query(ctx, `
		SELECT i.subscription_version_id, i.item_no, i.item_role, i.price_version_id, i.price_content_sha256,
		       i.accepted_terms_sha256
		FROM subscription_items i JOIN subscription_versions v USING (subscription_version_id)
		WHERE v.subscription_id = $1 ORDER BY i.subscription_version_id, i.item_no`, id)
	if err != nil {
		return nil, err
	}
	for itemRows.Next() {
		var versionID string
		var it domain.SubscriptionItem
		if err := itemRows.Scan(&versionID, &it.ItemNo, &it.ItemRole, &it.PriceVersionID, &it.PriceContentSHA256,
			&it.AcceptedTermsSHA256); err != nil {
			itemRows.Close()
			return nil, err
		}
		it.Quantities = []domain.SubscriptionItemQuantity{}
		v := byVersion[versionID]
		v.Items = append(v.Items, it)
	}
	itemRows.Close()
	if err := itemRows.Err(); err != nil {
		return nil, err
	}

	qRows, err := tx.Query(ctx, `
		SELECT q.subscription_version_id, q.item_no, q.component_key, q.quantity::text
		FROM subscription_item_quantities q JOIN subscription_versions v USING (subscription_version_id)
		WHERE v.subscription_id = $1 ORDER BY q.subscription_version_id, q.item_no, q.component_key`, id)
	if err != nil {
		return nil, err
	}
	for qRows.Next() {
		var versionID string
		var itemNo int
		var q domain.SubscriptionItemQuantity
		if err := qRows.Scan(&versionID, &itemNo, &q.ComponentKey, &q.Quantity); err != nil {
			qRows.Close()
			return nil, err
		}
		v := byVersion[versionID]
		for i := range v.Items {
			if v.Items[i].ItemNo == itemNo {
				v.Items[i].Quantities = append(v.Items[i].Quantities, q)
			}
		}
	}
	qRows.Close()
	if err := qRows.Err(); err != nil {
		return nil, err
	}

	termRows, err := tx.Query(ctx, `
		SELECT term_no, starts_at, ends_at, price_version_id, auto_renew, renewal_notice_days,
		       minimum_term_intervals, change_id, created_at, created_by_principal_id, voided_at, void_reason
		FROM subscription_terms WHERE subscription_id = $1 ORDER BY term_no`, id)
	if err != nil {
		return nil, err
	}
	for termRows.Next() {
		var t domain.SubscriptionTerm
		if err := termRows.Scan(&t.TermNo, &t.StartsAt, &t.EndsAt, &t.PriceVersionID, &t.AutoRenew, &t.RenewalNoticeDays,
			&t.MinimumTermIntervals, &t.ChangeID, &t.CreatedAt, &t.CreatedByPrincipalID, &t.VoidedAt, &t.VoidReason); err != nil {
			termRows.Close()
			return nil, err
		}
		agg.terms = append(agg.terms, t)
	}
	termRows.Close()
	if err := termRows.Err(); err != nil {
		return nil, err
	}

	deriveEffectiveTo(agg.versions)

	// Every price version the subscription has ever referenced, from its
	// items and its terms. Published and retired versions are readable from
	// the tenant plane, and a subscription is only ever bound to those.
	agg.pvs = map[string]*domain.PriceVersion{}
	var ids []string
	seen := map[string]bool{}
	for _, v := range agg.versions {
		for _, it := range v.Items {
			if !seen[it.PriceVersionID] {
				seen[it.PriceVersionID] = true
				ids = append(ids, it.PriceVersionID)
			}
		}
	}
	for _, t := range agg.terms {
		if !seen[t.PriceVersionID] {
			seen[t.PriceVersionID] = true
			ids = append(ids, t.PriceVersionID)
		}
	}
	if len(ids) > 0 {
		pvs, err := queryVersions(ctx, tx, `WHERE v.price_version_id = ANY($1)`, ids)
		if err != nil {
			return nil, err
		}
		for _, pv := range pvs {
			agg.pvs[pv.PriceVersionID] = pv
		}
	}
	if l := live(agg.versions); len(l) > 0 {
		agg.plan = agg.planOf(l[len(l)-1])
	}
	return agg, nil
}

func (a *subscriptionAggregate) planOf(v *domain.SubscriptionVersion) *domain.PriceVersion {
	for _, it := range v.Items {
		if it.ItemRole == "PLAN" {
			return a.pvs[it.PriceVersionID]
		}
	}
	return nil
}

// planAt is the plan price version in force at t — or, before the
// subscription starts, the one it will start on. A change scheduled for a
// term end is therefore the plan that term's renewal is priced under.
func (a *subscriptionAggregate) planAt(t time.Time) *domain.PriceVersion {
	if v, _ := a.statusNowOrUpcoming(t); v != nil {
		return a.planOf(v)
	}
	return a.plan
}

// termSpans is the live terms with the interval each was taken under.
func (a *subscriptionAggregate) termSpans() ([]domain.TermSpan, []domain.SubscriptionTerm) {
	lt := liveTerms(a.terms)
	spans := make([]domain.TermSpan, len(lt))
	for i, t := range lt {
		pv := a.pvs[t.PriceVersionID]
		spans[i] = domain.TermSpan{StartsAt: t.StartsAt, EndsAt: t.EndsAt, Interval: pv.BillingInterval, IntervalCount: pv.BillingIntervalCount}
	}
	return spans, lt
}

// termIndexAt is the index of the live term containing t, or -1.
func termIndexAt(lt []domain.SubscriptionTerm, t time.Time) int {
	for i, term := range lt {
		if !term.StartsAt.After(t) && term.EndsAt.After(t) {
			return i
		}
	}
	return -1
}

// minimumTermEnd is when the first term's minimum commitment is served.
func (a *subscriptionAggregate) minimumTermEnd() *time.Time {
	spans, lt := a.termSpans()
	if len(lt) == 0 {
		return nil
	}
	e := domain.MinimumTermEnd(spans[0], lt[0].MinimumTermIntervals)
	return &e
}

// cancellationBoundary applies the current term's notice and the minimum
// term to the current run of terms.
func (a *subscriptionAggregate) cancellationBoundary(now time.Time) (time.Time, bool) {
	spans, lt := a.termSpans()
	i := termIndexAt(lt, now)
	if i < 0 {
		return time.Time{}, false
	}
	notBefore := now.Add(time.Duration(lt[i].RenewalNoticeDays) * 24 * time.Hour)
	if minEnd := a.minimumTermEnd(); minEnd != nil && minEnd.After(notBefore) {
		notBefore = *minEnd
	}
	return domain.CancellationBoundary(spans, i, notBefore, now), true
}

// live returns the non-voided versions ordered by effective time.
func live(vs []domain.SubscriptionVersion) []*domain.SubscriptionVersion {
	var out []*domain.SubscriptionVersion
	for i := range vs {
		if vs[i].VoidedAt == nil {
			out = append(out, &vs[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].EffectiveFrom.Equal(out[j].EffectiveFrom) {
			return out[i].EffectiveFrom.Before(out[j].EffectiveFrom)
		}
		return out[i].VersionNumber < out[j].VersionNumber
	})
	return out
}

func deriveEffectiveTo(vs []domain.SubscriptionVersion) {
	l := live(vs)
	for i := 0; i < len(l)-1; i++ {
		next := l[i+1].EffectiveFrom
		l[i].EffectiveTo = &next
	}
}

// effectiveAt is the version in force at t: the latest non-voided version
// whose effective_from is not after t.
func effectiveAt(vs []domain.SubscriptionVersion, t time.Time) *domain.SubscriptionVersion {
	var out *domain.SubscriptionVersion
	for _, v := range live(vs) {
		if !v.EffectiveFrom.After(t) {
			out = v
		}
	}
	return out
}

func scheduledAfter(vs []domain.SubscriptionVersion, t time.Time) []domain.SubscriptionVersion {
	out := []domain.SubscriptionVersion{}
	for _, v := range live(vs) {
		if v.EffectiveFrom.After(t) {
			out = append(out, *v)
		}
	}
	return out
}

func liveTerms(ts []domain.SubscriptionTerm) []domain.SubscriptionTerm {
	var out []domain.SubscriptionTerm
	for _, t := range ts {
		if t.VoidedAt == nil {
			out = append(out, t)
		}
	}
	return out
}

func termAt(ts []domain.SubscriptionTerm, t time.Time) *domain.SubscriptionTerm {
	for _, term := range liveTerms(ts) {
		if !term.StartsAt.After(t) && term.EndsAt.After(t) {
			term := term
			return &term
		}
	}
	return nil
}

func (a *subscriptionAggregate) view(at time.Time) *domain.SubscriptionView {
	v := &domain.SubscriptionView{Subscription: *a.sub, AsOf: at, Scheduled: scheduledAfter(a.versions, at)}
	if e := effectiveAt(a.versions, at); e != nil {
		v.Effective = e
		st := e.LifecycleStatus
		v.Status = &st
	}
	v.CurrentTerm = termAt(a.terms, at)
	return v
}

// statusNowOrUpcoming is the status in force at now, or — for a subscription
// whose start is still in the future — the status it will start in.
func (a *subscriptionAggregate) statusNowOrUpcoming(now time.Time) (*domain.SubscriptionVersion, bool) {
	if e := effectiveAt(a.versions, now); e != nil {
		return e, true
	}
	if l := live(a.versions); len(l) > 0 {
		return l[0], false
	}
	return nil, false
}

func (a *subscriptionAggregate) nextVersionNumber() int {
	n := 0
	for _, v := range a.versions {
		if v.VersionNumber > n {
			n = v.VersionNumber
		}
	}
	return n + 1
}

func (a *subscriptionAggregate) nextTermNo() int {
	n := 0
	for _, t := range a.terms {
		if t.TermNo > n {
			n = t.TermNo
		}
	}
	return n + 1
}

func (a *subscriptionAggregate) currentItems() []domain.SubscriptionItem {
	l := live(a.versions)
	if len(l) == 0 {
		return nil
	}
	return l[len(l)-1].Items
}

// ── Writers ──────────────────────────────────────────────────────────────────

type changeMeta struct {
	changeID string
	actor    string
	channel  domain.Channel
	basis    *string
	reason   *string
	now      time.Time
}

func insertSubscriptionVersion(ctx context.Context, tx pgx.Tx, subID string, number int, pv domain.PlannedVersion,
	items []domain.SubscriptionItem, m changeMeta) error {
	versionID := domain.NewCommercialID(domain.PrefixSubscriptionVersion)
	if _, err := tx.Exec(ctx, `
		INSERT INTO subscription_versions (subscription_version_id, subscription_id, version_number, lifecycle_status,
			change_type, effective_from, trial_ends_at, change_id, reason, channel, customer_basis_ref,
			created_at, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		versionID, subID, number, pv.Status, pv.ChangeType, pv.EffectiveFrom, pv.TrialEndsAt, m.changeID, m.reason,
		m.channel, m.basis, m.now, m.actor); err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO subscription_items (subscription_version_id, item_no, item_role, price_version_id,
				price_content_sha256, accepted_terms_sha256)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			versionID, it.ItemNo, it.ItemRole, it.PriceVersionID, it.PriceContentSHA256, it.AcceptedTermsSHA256); err != nil {
			return err
		}
		for _, q := range it.Quantities {
			if _, err := tx.Exec(ctx, `
				INSERT INTO subscription_item_quantities (subscription_version_id, item_no, component_key, quantity)
				VALUES ($1, $2, $3, $4::numeric)`, versionID, it.ItemNo, q.ComponentKey, q.Quantity); err != nil {
				return err
			}
		}
	}
	return nil
}

func insertSubscriptionTerm(ctx context.Context, tx pgx.Tx, subID string, termNo int, t domain.PlannedTerm,
	plan *domain.PriceVersion, m changeMeta) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO subscription_terms (subscription_id, term_no, starts_at, ends_at, price_version_id, auto_renew,
			renewal_notice_days, minimum_term_intervals, change_id, created_at, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		subID, termNo, t.StartsAt, t.EndsAt, plan.PriceVersionID, plan.Terms.AutoRenew, plan.Terms.RenewalNoticeDays,
		plan.Terms.MinimumTermIntervals, m.changeID, m.now, m.actor)
	return err
}

// applyPlan writes a lifecycle plan's versions and terms after the
// aggregate's existing ones.
func (a *subscriptionAggregate) applyPlan(ctx context.Context, tx pgx.Tx, p domain.LifecyclePlan, items []domain.SubscriptionItem, termPlan *domain.PriceVersion, m changeMeta) error {
	n := a.nextVersionNumber()
	for i, pv := range p.Versions {
		if err := insertSubscriptionVersion(ctx, tx, a.sub.SubscriptionID, n+i, pv, items, m); err != nil {
			return err
		}
	}
	t := a.nextTermNo()
	for i, term := range p.Terms {
		if err := insertSubscriptionTerm(ctx, tx, a.sub.SubscriptionID, t+i, term, termPlan, m); err != nil {
			return err
		}
	}
	return nil
}

// voidFuture voids every version and term that has not yet taken effect.
func voidFuture(ctx context.Context, tx pgx.Tx, subID string, m changeMeta, why string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE subscription_versions
		   SET voided_at = $2, voided_by_principal_id = $3, void_reason = $4, voided_by_change_id = $5
		 WHERE subscription_id = $1 AND voided_at IS NULL AND effective_from > $2`,
		subID, m.now, m.actor, why, m.changeID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE subscription_terms
		   SET voided_at = $2, voided_by_principal_id = $3, void_reason = $4, voided_by_change_id = $5
		 WHERE subscription_id = $1 AND voided_at IS NULL AND starts_at > $2`,
		subID, m.now, m.actor, why, m.changeID)
	return err
}

func setEndsAt(ctx context.Context, tx pgx.Tx, subID string, endsAt *time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE subscriptions SET ends_at = $2, row_version = row_version + 1 WHERE subscription_id = $1`, subID, endsAt)
	return err
}

type subscriptionEvent struct {
	SubscriptionID string                  `json:"subscription_id"`
	OrganizationID string                  `json:"organization_id"`
	ChangeID       string                  `json:"change_id"`
	ChangeType     domain.ChangeType       `json:"change_type,omitempty"`
	Status         domain.LifecycleStatus  `json:"status,omitempty"`
	EffectiveAt    time.Time               `json:"effective_at"`
	EndsAt         *time.Time              `json:"ends_at,omitempty"`
	PriceVersionID string                  `json:"price_version_id,omitempty"`
	Schedule       []domain.PlannedVersion `json:"schedule,omitempty"`
	TermNo         int                     `json:"term_no,omitempty"`
	ActorID        string                  `json:"actor_id"`
	Channel        domain.Channel          `json:"channel"`
	OccurredAt     time.Time               `json:"occurred_at"`
}

func emitSubscriptionEvent(ctx context.Context, tx pgx.Tx, eventType string, sub *domain.Subscription, m changeMeta, e subscriptionEvent) error {
	e.SubscriptionID, e.OrganizationID, e.ChangeID = sub.SubscriptionID, sub.OrganizationID, m.changeID
	e.ActorID, e.Channel, e.OccurredAt = m.actor, m.channel, m.now
	org := sub.OrganizationID
	return outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: "subscription", AggregateID: sub.SubscriptionID, EventType: eventType, Payload: e, TenantID: &org,
	})
}

// ── StartSubscription ────────────────────────────────────────────────────────

// StartSubscription binds a new subscription to the server-resolved sellable
// offer for the account's own currency and market. The caller names only the
// product; it cannot name a price, a price version or a currency (§4.2
// server-resolved context; negative path #05).
func (s *PgStore) StartSubscription(ctx context.Context, p domain.StartSubscriptionParams, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	var out *domain.SubscriptionView
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		var org, currency, status string
		var market *string
		err := tx.QueryRow(ctx, `
			SELECT organization_id::text, billing_currency_code, status, market_code
			FROM commercial_accounts WHERE commercial_account_id = $1 FOR UPDATE`, p.CommercialAccountID).
			Scan(&org, &currency, &status, &market)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCommercialAccountNotFound
		}
		if err != nil {
			return err
		}
		if status != string(domain.CommercialAccountStatusActive) {
			return domain.ErrAccountNotActive
		}
		if market == nil {
			return domain.ErrAccountMarketNotSet
		}

		resolve := func(code string) (*domain.PriceVersion, error) {
			vs, err := resolveOffers(ctx, tx, domain.SellableOfferFilter{ProductCode: code, CurrencyCode: currency, MarketCode: *market}, p.Now)
			if err != nil {
				return nil, err
			}
			if len(vs) != 1 {
				return nil, fmt.Errorf("%w: no sellable offer for %s in %s/%s", domain.ErrPriceVersionNotSellable, code, currency, *market)
			}
			return vs[0], nil
		}

		plan, err := resolve(p.PlanProductCode)
		if err != nil {
			return err
		}
		if plan.ProductKind != domain.ProductKindPlan {
			return domain.ErrWrongProductKind
		}
		if p.ExpectedPriceVersionID != nil && *p.ExpectedPriceVersionID != plan.PriceVersionID {
			return domain.ErrOfferChanged
		}
		if p.AcceptedTermsSHA256 != plan.Terms.TermsDocumentSHA256 {
			return domain.ErrTermsNotAccepted
		}
		if plan.Terms.Trial != nil && plan.Terms.Trial.PaymentMethodRequired && p.PaymentMethodRef == nil {
			return domain.ErrPaymentMethodRequired
		}
		qty, err := domain.ValidateQuantities(plan, p.Quantities)
		if err != nil {
			return err
		}
		items := []domain.SubscriptionItem{{ItemNo: 1, ItemRole: "PLAN", PriceVersionID: plan.PriceVersionID,
			PriceContentSHA256: *plan.ContentSHA256, AcceptedTermsSHA256: p.AcceptedTermsSHA256, Quantities: qty}}

		seen := map[string]bool{p.PlanProductCode: true}
		for i, a := range p.AddOns {
			field := fmt.Sprintf("add_ons[%d]", i)
			if seen[a.ProductCode] {
				return &domain.ValidationError{Field: field + ".product_code", Reason: "each product may appear once"}
			}
			seen[a.ProductCode] = true
			addOn, err := resolve(a.ProductCode)
			if err != nil {
				return err
			}
			if addOn.ProductKind != domain.ProductKindAddOn {
				return domain.ErrWrongProductKind
			}
			if a.AcceptedTermsSHA256 != addOn.Terms.TermsDocumentSHA256 {
				return fmt.Errorf("%w (%s)", domain.ErrTermsNotAccepted, field)
			}
			aq, err := domain.ValidateQuantities(addOn, a.Quantities)
			if err != nil {
				var ve *domain.ValidationError
				if errors.As(err, &ve) {
					ve.Field = field + "." + ve.Field
				}
				return err
			}
			items = append(items, domain.SubscriptionItem{ItemNo: i + 2, ItemRole: "ADD_ON", PriceVersionID: addOn.PriceVersionID,
				PriceContentSHA256: *addOn.ContentSHA256, AcceptedTermsSHA256: a.AcceptedTermsSHA256, Quantities: aq})
		}

		lp := domain.PlanStart(plan, p.StartsAt)
		if _, err := tx.Exec(ctx, `
			INSERT INTO subscriptions (subscription_id, organization_id, commercial_account_id, product_id, currency_code,
				market_code, channel, customer_basis_ref, payment_method_ref, starts_at, ends_at, created_at,
				created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			p.SubscriptionID, org, p.CommercialAccountID, plan.ProductID, currency, *market, p.Channel,
			p.CustomerBasisRef, p.PaymentMethodRef, p.StartsAt, lp.EndsAt, p.Now, p.Actor); err != nil {
			return err
		}

		agg, err := loadAggregate(ctx, tx, p.SubscriptionID, true)
		if err != nil {
			return err
		}
		m := changeMeta{changeID: p.ChangeID, actor: p.Actor, channel: p.Channel, basis: p.CustomerBasisRef, now: p.Now}
		if err := agg.applyPlan(ctx, tx, lp, items, plan, m); err != nil {
			return err
		}
		if err := emitSubscriptionEvent(ctx, tx, "subscription.started", agg.sub, m, subscriptionEvent{
			ChangeType: domain.ChangeStarted, Status: lp.Versions[0].Status, EffectiveAt: p.StartsAt,
			EndsAt: lp.EndsAt, PriceVersionID: plan.PriceVersionID, Schedule: lp.Versions,
		}); err != nil {
			return err
		}
		if agg, err = loadAggregate(ctx, tx, p.SubscriptionID, false); err != nil {
			return err
		}
		out = agg.view(p.Now)
		return nil
	})
	return out, err
}

// ── Lifecycle commands ───────────────────────────────────────────────────────

// subscriptionCommand is the frame every lifecycle command shares: claim the
// idempotency key, lock the subscription, check the ETag, apply, and return
// the resulting state — all in one transaction.
func (s *PgStore) subscriptionCommand(ctx context.Context, c domain.SubscriptionCommand, claim domain.IdempotencyClaim,
	apply func(tx pgx.Tx, a *subscriptionAggregate, m changeMeta) error) (*domain.SubscriptionView, error) {
	var out *domain.SubscriptionView
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		a, err := loadAggregate(ctx, tx, c.SubscriptionID, true)
		if err != nil {
			return err
		}
		if a.sub.RowVersion != c.ExpectedVersion {
			return domain.ErrVersionConflict
		}
		m := changeMeta{changeID: c.ChangeID, actor: c.Actor, channel: c.Channel, basis: c.CustomerBasisRef, now: c.Now}
		if c.Reason != "" {
			r := c.Reason
			m.reason = &r
		}
		if err := apply(tx, a, m); err != nil {
			return err
		}
		if a, err = loadAggregate(ctx, tx, c.SubscriptionID, false); err != nil {
			return err
		}
		out = a.view(c.Now)
		return nil
	})
	return out, err
}

func lastLiveTerm(ts []domain.SubscriptionTerm) *domain.SubscriptionTerm {
	l := liveTerms(ts)
	if len(l) == 0 {
		return nil
	}
	return &l[len(l)-1]
}

// ActivateSubscription makes a subscription paid-active. A PENDING
// subscription is activated by ZoikoSuite only (customerConfirmation must be
// false): paid access is never self-granted. A TRIALING subscription whose
// policy needs confirmation may be confirmed by the customer; it then
// converts at the end of the trial it already has.
func (s *PgStore) ActivateSubscription(ctx context.Context, c domain.SubscriptionCommand, customerConfirmation bool, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	return s.subscriptionCommand(ctx, c, claim, func(tx pgx.Tx, a *subscriptionAggregate, m changeMeta) error {
		cur, _ := a.statusNowOrUpcoming(c.Now)
		if cur == nil {
			return domain.ErrSubscriptionInvalidState
		}
		var lp domain.LifecyclePlan
		var plan *domain.PriceVersion
		switch cur.LifecycleStatus {
		case domain.LifecyclePending:
			if customerConfirmation {
				return domain.ErrSellerActivationRequired
			}
			at := c.Now
			if a.sub.StartsAt.After(at) {
				at = a.sub.StartsAt
			}
			plan = a.planAt(at)
			lp = domain.PlanActivation(plan, at, domain.ChangeActivated)
		case domain.LifecycleTrialing:
			for _, v := range scheduledAfter(a.versions, c.Now) {
				if v.LifecycleStatus == domain.LifecycleActive {
					return domain.ErrConversionAlreadyScheduled
				}
			}
			if err := voidFuture(ctx, tx, a.sub.SubscriptionID, m, "trial conversion confirmed"); err != nil {
				return err
			}
			plan = a.planAt(c.Now)
			lp = domain.PlanActivation(plan, *cur.TrialEndsAt, domain.ChangeTrialConversion)
		default:
			return fmt.Errorf("%w: cannot activate a %s subscription", domain.ErrSubscriptionInvalidState, cur.LifecycleStatus)
		}
		if err := a.applyPlan(ctx, tx, lp, a.currentItems(), plan, m); err != nil {
			return err
		}
		if err := setEndsAt(ctx, tx, a.sub.SubscriptionID, lp.EndsAt); err != nil {
			return err
		}
		return emitSubscriptionEvent(ctx, tx, "subscription.activated", a.sub, m, subscriptionEvent{
			ChangeType: lp.Versions[0].ChangeType, Status: domain.LifecycleActive,
			EffectiveAt: lp.Versions[0].EffectiveFrom, EndsAt: lp.EndsAt, Schedule: lp.Versions,
		})
	})
}

// ScheduleCancellation ends an active subscription at the earliest term end
// the commercial terms allow: after the minimum term, with the notice the
// terms require. Until then it is CANCEL_PENDING and fully in service.
func (s *PgStore) ScheduleCancellation(ctx context.Context, c domain.SubscriptionCommand, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	return s.subscriptionCommand(ctx, c, claim, func(tx pgx.Tx, a *subscriptionAggregate, m changeMeta) error {
		cur := effectiveAt(a.versions, c.Now)
		if cur == nil || cur.LifecycleStatus != domain.LifecycleActive {
			return fmt.Errorf("%w: only an ACTIVE subscription can schedule a cancellation", domain.ErrSubscriptionInvalidState)
		}
		// A scheduled configuration change would take effect inside the
		// cancel-pending window and mask it; resolve one before the other.
		if len(scheduledAfter(a.versions, c.Now)) > 0 {
			return domain.ErrChangeAlreadyScheduled
		}
		boundary, ok := a.cancellationBoundary(c.Now)
		if !ok {
			return domain.ErrSubscriptionInvalidState
		}
		if a.sub.EndsAt != nil && !a.sub.EndsAt.After(boundary) {
			return fmt.Errorf("%w: the subscription already ends at %s", domain.ErrSubscriptionInvalidState, a.sub.EndsAt.Format(time.RFC3339))
		}
		lp := domain.LifecyclePlan{Versions: []domain.PlannedVersion{
			{Status: domain.LifecycleCancelPending, ChangeType: domain.ChangeCancellationScheduled, EffectiveFrom: c.Now},
			{Status: domain.LifecycleCanceled, ChangeType: domain.ChangeCancellationEffective, EffectiveFrom: boundary},
		}}
		if err := a.applyPlan(ctx, tx, lp, a.currentItems(), nil, m); err != nil {
			return err
		}
		if err := setEndsAt(ctx, tx, a.sub.SubscriptionID, &boundary); err != nil {
			return err
		}
		return emitSubscriptionEvent(ctx, tx, "subscription.cancellation_scheduled", a.sub, m, subscriptionEvent{
			ChangeType: domain.ChangeCancellationScheduled, Status: domain.LifecycleCanceled, EffectiveAt: boundary, EndsAt: &boundary,
		})
	})
}

// CancelNow ends the subscription immediately where the terms permit it: at
// any time before paid service starts, and afterwards only once the minimum
// term has been served. Everything scheduled for later is voided.
func (s *PgStore) CancelNow(ctx context.Context, c domain.SubscriptionCommand, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	return s.subscriptionCommand(ctx, c, claim, func(tx pgx.Tx, a *subscriptionAggregate, m changeMeta) error {
		cur, started := a.statusNowOrUpcoming(c.Now)
		if cur == nil || (started && cur.LifecycleStatus.Terminal()) {
			return domain.ErrSubscriptionEnded
		}
		if started && (cur.LifecycleStatus == domain.LifecycleActive || cur.LifecycleStatus == domain.LifecycleCancelPending) {
			if minEnd := a.minimumTermEnd(); minEnd != nil && c.Now.Before(*minEnd) {
				return domain.ErrMinimumTermNotMet
			}
		}
		if err := voidFuture(ctx, tx, a.sub.SubscriptionID, m, "canceled now"); err != nil {
			return err
		}
		endsAt := c.Now
		if a.sub.StartsAt.After(endsAt) {
			endsAt = a.sub.StartsAt
		}
		lp := domain.LifecyclePlan{Versions: []domain.PlannedVersion{
			{Status: domain.LifecycleCanceled, ChangeType: domain.ChangeCanceledNow, EffectiveFrom: c.Now},
		}}
		if err := a.applyPlan(ctx, tx, lp, a.currentItems(), nil, m); err != nil {
			return err
		}
		if err := setEndsAt(ctx, tx, a.sub.SubscriptionID, &endsAt); err != nil {
			return err
		}
		return emitSubscriptionEvent(ctx, tx, "subscription.canceled", a.sub, m, subscriptionEvent{
			ChangeType: domain.ChangeCanceledNow, Status: domain.LifecycleCanceled, EffectiveAt: c.Now, EndsAt: &endsAt,
		})
	})
}

// Reactivate withdraws a scheduled cancellation before it takes effect.
func (s *PgStore) Reactivate(ctx context.Context, c domain.SubscriptionCommand, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	return s.subscriptionCommand(ctx, c, claim, func(tx pgx.Tx, a *subscriptionAggregate, m changeMeta) error {
		cur := effectiveAt(a.versions, c.Now)
		if cur == nil || cur.LifecycleStatus != domain.LifecycleCancelPending {
			return fmt.Errorf("%w: only a CANCEL_PENDING subscription can be reactivated", domain.ErrSubscriptionInvalidState)
		}
		if err := voidFuture(ctx, tx, a.sub.SubscriptionID, m, "cancellation withdrawn"); err != nil {
			return err
		}
		lp := domain.LifecyclePlan{Versions: []domain.PlannedVersion{
			{Status: domain.LifecycleActive, ChangeType: domain.ChangeReactivated, EffectiveFrom: c.Now},
		}}
		if err := a.applyPlan(ctx, tx, lp, a.currentItems(), nil, m); err != nil {
			return err
		}
		if err := setEndsAt(ctx, tx, a.sub.SubscriptionID, nil); err != nil {
			return err
		}
		return emitSubscriptionEvent(ctx, tx, "subscription.changed", a.sub, m, subscriptionEvent{
			ChangeType: domain.ChangeReactivated, Status: domain.LifecycleActive, EffectiveAt: c.Now,
		})
	})
}

// Renew adds the next term. An auto-renewing subscription renews once its
// current term has ended (the boundary processor calls this at term end).
// A subscription that does not auto-renew can be renewed before it expires,
// which withdraws the scheduled expiry. The new term is priced under the
// plan in force at the term end: the bound price version, or a change
// scheduled for that boundary — never a newer published price
// (COM-CTRL-003).
func (s *PgStore) Renew(ctx context.Context, c domain.SubscriptionCommand, claim domain.IdempotencyClaim) (*domain.SubscriptionView, error) {
	return s.subscriptionCommand(ctx, c, claim, func(tx pgx.Tx, a *subscriptionAggregate, m changeMeta) error {
		spans, lt := a.termSpans()
		if len(lt) == 0 {
			return fmt.Errorf("%w: the subscription has no term to renew", domain.ErrSubscriptionInvalidState)
		}
		last := lt[len(lt)-1]
		plan := a.planAt(last.EndsAt)
		start, end := domain.NextTermWindow(spans, plan.BillingInterval, plan.BillingIntervalCount)
		next := domain.PlannedTerm{StartsAt: start, EndsAt: end}
		lp := domain.LifecyclePlan{Terms: []domain.PlannedTerm{next}}
		endsAt := a.sub.EndsAt

		endsAtTermEnd := a.sub.EndsAt != nil && !a.sub.EndsAt.After(last.EndsAt)
		switch {
		case endsAtTermEnd:
			expiry := effectiveAt(a.versions, last.EndsAt)
			if last.AutoRenew || !c.Now.Before(last.EndsAt) || expiry == nil || expiry.ChangeType != domain.ChangeTermExpiry {
				return domain.ErrSubscriptionEnded
			}
			if err := voidFuture(ctx, tx, a.sub.SubscriptionID, m, "renewed before expiry"); err != nil {
				return err
			}
			lp.Versions = []domain.PlannedVersion{{Status: domain.LifecycleExpired, ChangeType: domain.ChangeTermExpiry, EffectiveFrom: next.EndsAt}}
			endsAt = &next.EndsAt
		case c.Now.Before(last.EndsAt):
			return domain.ErrRenewalNotDue
		}
		if err := a.applyPlan(ctx, tx, lp, a.currentItems(), plan, m); err != nil {
			return err
		}
		if err := setEndsAt(ctx, tx, a.sub.SubscriptionID, endsAt); err != nil {
			return err
		}
		return emitSubscriptionEvent(ctx, tx, "subscription.renewed", a.sub, m, subscriptionEvent{
			EffectiveAt: next.StartsAt, EndsAt: endsAt, TermNo: a.nextTermNo(),
		})
	})
}

// ── Queries ──────────────────────────────────────────────────────────────────

func (s *PgStore) GetSubscriptionAsOf(ctx context.Context, id string, at time.Time) (*domain.SubscriptionView, error) {
	var out *domain.SubscriptionView
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		a, err := loadAggregate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out = a.view(at)
		return nil
	})
	return out, err
}

// GetEffectiveVersion reconstructs the agreement in force at any instant.
func (s *PgStore) GetEffectiveVersion(ctx context.Context, id string, at time.Time) (*domain.SubscriptionVersion, error) {
	var out *domain.SubscriptionVersion
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		a, err := loadAggregate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if out = effectiveAt(a.versions, at); out == nil {
			return ErrNoEffectiveVersion
		}
		return nil
	})
	return out, err
}

// GetChangeHistory returns every version ever written, voided ones included.
func (s *PgStore) GetChangeHistory(ctx context.Context, id string) ([]domain.SubscriptionVersion, error) {
	var out []domain.SubscriptionVersion
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		a, err := loadAggregate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out = a.versions
		return nil
	})
	return out, err
}

func (s *PgStore) GetRenewalState(ctx context.Context, id string, now time.Time) (*domain.RenewalState, error) {
	var out *domain.RenewalState
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		a, err := loadAggregate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		v := a.view(now)
		rs := &domain.RenewalState{SubscriptionID: id, AsOf: now, Status: v.Status, CurrentTerm: v.CurrentTerm,
			AutoRenew: a.planAt(now).Terms.AutoRenew, EndsAt: a.sub.EndsAt}
		if last := lastLiveTerm(a.terms); last != nil {
			rs.AutoRenew = last.AutoRenew
			rs.MinimumTermEndsAt = a.minimumTermEnd()
			continues := a.sub.EndsAt == nil || a.sub.EndsAt.After(last.EndsAt)
			if last.AutoRenew && continues {
				next := last.EndsAt
				rs.NextTermStartsAt = &next
				rs.RenewalDue = !now.Before(last.EndsAt)
			}
			if v.Status != nil && *v.Status == domain.LifecycleActive {
				if b, ok := a.cancellationBoundary(now); ok {
					rs.EarliestCancellationBoundary = &b
				}
			}
		}
		out = rs
		return nil
	})
	return out, err
}
