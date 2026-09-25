// COM-01 Product & Price Book persistence (migration 000006).
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

// PriceBookStore is the COM-01 persistence contract. Every mutating method
// takes the caller's IdempotencyClaim and records it in the same transaction
// as the change, so a replayed command can never apply twice.
type PriceBookStore interface {
	UpsertCurrency(ctx context.Context, c *domain.CommercialCurrency) (*domain.CommercialCurrency, error)
	ListCurrencies(ctx context.Context) ([]domain.CommercialCurrency, error)

	CreateProduct(ctx context.Context, p *domain.Product, claim domain.IdempotencyClaim) (*domain.Product, error)
	GetProduct(ctx context.Context, productID string, seller bool) (*domain.Product, error)
	ListPublishedProducts(ctx context.Context, now time.Time) ([]domain.ProductSummary, error)

	CreateDraftVersion(ctx context.Context, v *domain.PriceVersion, clonePrevious bool, claim domain.IdempotencyClaim) (*domain.PriceVersion, error)
	PutPriceComponent(ctx context.Context, versionID string, expectedVersion int, c *domain.PriceComponent, claim domain.IdempotencyClaim) (*domain.PriceVersion, error)
	RemovePriceComponent(ctx context.Context, versionID string, expectedVersion int, componentKey, actor string, claim domain.IdempotencyClaim) (*domain.PriceVersion, error)
	SetCommercialTerms(ctx context.Context, versionID string, expectedVersion int, terms *domain.CommercialTerms, claim domain.IdempotencyClaim) (*domain.PriceVersion, error)
	SetCapabilities(ctx context.Context, versionID string, expectedVersion int, caps []domain.PlanCapability, actor string, claim domain.IdempotencyClaim) (*domain.PriceVersion, error)

	SubmitForApproval(ctx context.Context, versionID string, expectedVersion int, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.PriceVersion, error)
	ApprovePriceVersion(ctx context.Context, versionID string, expectedVersion int, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.PriceVersion, error)
	RejectPriceVersion(ctx context.Context, versionID string, expectedVersion int, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.PriceVersion, error)
	PublishPriceVersion(ctx context.Context, versionID string, expectedVersion int, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.PriceVersion, error)
	RetirePriceVersion(ctx context.Context, versionID string, expectedVersion int, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.PriceVersion, error)

	GetPriceVersion(ctx context.Context, versionID string, seller bool) (*domain.PriceVersion, error)
	ListPriceHistory(ctx context.Context, productID string, seller bool) ([]domain.PriceVersion, error)
	ResolveSellableOffers(ctx context.Context, f domain.SellableOfferFilter, now time.Time) ([]domain.PriceVersion, error)
}

var _ PriceBookStore = (*PgStore)(nil)

// ErrCurrencyMinorUnitsFixed is returned when an upsert tries to change the
// minor units of a currency that already exists.
var ErrCurrencyMinorUnitsFixed = errors.New("currency minor units are fixed once written")

const aggregatePriceVersion = "product_price_version"

// ── Transactions ─────────────────────────────────────────────────────────────

// withSellerPlane runs fn in a transaction that has declared the ZoikoSuite
// seller plane (app.commercial_plane = 'seller'), the only context in which
// the price book's RLS policies allow writes or expose unpublished versions.
// Transaction-local for the same reason withTenant is: a session-wide setting
// would leak seller visibility to whichever request next borrows the
// connection.
func (s *PgStore) withSellerPlane(ctx context.Context, fn func(pgx.Tx) error) error {
	return s.catalogTx(ctx, true, fn)
}

// catalogTx runs fn with or without the seller plane declared. Without it,
// RLS limits every price-book read to published and retired content.
func (s *PgStore) catalogTx(ctx context.Context, seller bool, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if seller {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
			return err
		}
	}
	if err := fn(tx); err != nil {
		return mapPriceBookErr(err)
	}
	return mapPriceBookErr(tx.Commit(ctx))
}

func mapPriceBookErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "CP001":
		return fmt.Errorf("%w: %s", domain.ErrPriceVersionImmutable, pgErr.Message)
	case "23505":
		switch pgErr.ConstraintName {
		case "commercial_products_product_code_key":
			return domain.ErrProductCodeTaken
		case "idx_price_versions_one_in_flight":
			return domain.ErrInFlightVersionExists
		}
	case "23514":
		if pgErr.ConstraintName == "price_versions_approver_is_independent" {
			return domain.ErrSoDViolation
		}
	}
	return err
}

// claimIdempotency records claim before the change it guards. A concurrent
// request with the same key blocks on the primary key until this transaction
// ends; if it commits, the other sees the claim and replays.
func claimIdempotency(ctx context.Context, tx pgx.Tx, c domain.IdempotencyClaim) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO commercial_idempotency_keys
			(owner_scope, principal_id, idempotency_key, operation, request_sha256, resource_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (owner_scope, principal_id, idempotency_key) DO NOTHING`,
		c.OwnerScope, c.PrincipalID, c.Key, c.Operation, c.RequestSHA256, c.ResourceID)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var reqHash, resourceID string
	if err := tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id FROM commercial_idempotency_keys
		WHERE owner_scope = $1 AND principal_id = $2 AND idempotency_key = $3`,
		c.OwnerScope, c.PrincipalID, c.Key).Scan(&reqHash, &resourceID); err != nil {
		return fmt.Errorf("read idempotency claim: %w", err)
	}
	if reqHash != c.RequestSHA256 {
		return domain.ErrIdempotencyKeyReused
	}
	return &domain.IdempotentReplayError{ResourceID: resourceID}
}

type priceVersionEvent struct {
	PriceVersionID string                    `json:"price_version_id"`
	ProductID      string                    `json:"product_id"`
	ProductCode    string                    `json:"product_code"`
	VersionNumber  int                       `json:"version_number"`
	Status         domain.PriceVersionStatus `json:"status"`
	RowVersion     int                       `json:"row_version"`
	ContentSHA256  *string                   `json:"content_sha256,omitempty"`
	ActorID        string                    `json:"actor_id"`
	Reason         string                    `json:"reason,omitempty"`
	OccurredAt     time.Time                 `json:"occurred_at"`
}

func emitVersionEvent(ctx context.Context, tx pgx.Tx, eventType string, v *domain.PriceVersion, actor, reason string, at time.Time) error {
	return outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: aggregatePriceVersion,
		AggregateID:   v.PriceVersionID,
		EventType:     eventType,
		Payload: priceVersionEvent{
			PriceVersionID: v.PriceVersionID, ProductID: v.ProductID, ProductCode: v.ProductCode,
			VersionNumber: v.VersionNumber, Status: v.Status, RowVersion: v.RowVersion,
			ContentSHA256: v.ContentSHA256, ActorID: actor, Reason: reason, OccurredAt: at.UTC(),
		},
	})
}

// ── Currencies ───────────────────────────────────────────────────────────────

const currencyColumns = `currency_code, minor_units, sale_enabled, created_at, created_by_principal_id, updated_at, updated_by_principal_id`

func scanCurrency(row pgx.Row) (*domain.CommercialCurrency, error) {
	var c domain.CommercialCurrency
	if err := row.Scan(&c.CurrencyCode, &c.MinorUnits, &c.SaleEnabled, &c.CreatedAt,
		&c.CreatedByPrincipalID, &c.UpdatedAt, &c.UpdatedByPrincipalID); err != nil {
		return nil, err
	}
	return &c, nil
}

// UpsertCurrency is a full-representation PUT, idempotent by nature: sending
// the same body twice leaves the same state. minor_units may be written once
// and never changed.
func (s *PgStore) UpsertCurrency(ctx context.Context, c *domain.CommercialCurrency) (*domain.CommercialCurrency, error) {
	var out *domain.CommercialCurrency
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		got, err := scanCurrency(tx.QueryRow(ctx, `
			INSERT INTO commercial_currencies
				(currency_code, minor_units, sale_enabled, created_by_principal_id, updated_by_principal_id)
			VALUES ($1, $2, $3, $4, $4)
			ON CONFLICT (currency_code) DO UPDATE
				SET sale_enabled = EXCLUDED.sale_enabled,
				    updated_at = NOW(),
				    updated_by_principal_id = EXCLUDED.updated_by_principal_id
				WHERE commercial_currencies.minor_units = EXCLUDED.minor_units
			RETURNING `+currencyColumns,
			c.CurrencyCode, c.MinorUnits, c.SaleEnabled, c.UpdatedByPrincipalID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCurrencyMinorUnitsFixed
		}
		out = got
		return err
	})
	return out, err
}

func (s *PgStore) ListCurrencies(ctx context.Context) ([]domain.CommercialCurrency, error) {
	var out []domain.CommercialCurrency
	err := s.catalogTx(ctx, false, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+currencyColumns+` FROM commercial_currencies ORDER BY currency_code`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCurrency(rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	return out, err
}

func loadCurrency(ctx context.Context, tx pgx.Tx, code string) (*domain.CommercialCurrency, error) {
	c, err := scanCurrency(tx.QueryRow(ctx, `SELECT `+currencyColumns+` FROM commercial_currencies WHERE currency_code = $1`, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCurrencyNotFound
	}
	return c, err
}

// ── Products ─────────────────────────────────────────────────────────────────

func (s *PgStore) CreateProduct(ctx context.Context, p *domain.Product, claim domain.IdempotencyClaim) (*domain.Product, error) {
	var out *domain.Product
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := scanProduct(tx.QueryRow(ctx, `
			INSERT INTO commercial_products (product_id, product_code, product_kind, created_by_principal_id)
			VALUES ($1, $2, $3, $4)
			RETURNING product_id, product_code, product_kind, created_at, created_by_principal_id`,
			p.ProductID, p.ProductCode, p.ProductKind, p.CreatedByPrincipalID))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{
			AggregateType: "commercial_product", AggregateID: got.ProductID, EventType: "product.created", Payload: got,
		})
	})
	return out, err
}

func scanProduct(row pgx.Row) (*domain.Product, error) {
	var p domain.Product
	if err := row.Scan(&p.ProductID, &p.ProductCode, &p.ProductKind, &p.CreatedAt, &p.CreatedByPrincipalID); err != nil {
		return nil, err
	}
	return &p, nil
}

func loadProduct(ctx context.Context, tx pgx.Tx, productID string, forUpdate bool) (*domain.Product, error) {
	q := `SELECT product_id, product_code, product_kind, created_at, created_by_principal_id
	      FROM commercial_products WHERE product_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	p, err := scanProduct(tx.QueryRow(ctx, q, productID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrProductNotFound
	}
	return p, err
}

func (s *PgStore) GetProduct(ctx context.Context, productID string, seller bool) (*domain.Product, error) {
	var out *domain.Product
	err := s.catalogTx(ctx, seller, func(tx pgx.Tx) error {
		p, err := loadProduct(ctx, tx, productID, false)
		out = p
		return err
	})
	return out, err
}

// ListPublishedProducts returns every product with at least one version that
// is published, in effect at now, and priced in a currency enabled for sale.
func (s *PgStore) ListPublishedProducts(ctx context.Context, now time.Time) ([]domain.ProductSummary, error) {
	var out []domain.ProductSummary
	err := s.catalogTx(ctx, false, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ON (p.product_id, v.currency_code)
			       p.product_id, p.product_code, p.product_kind, v.display_name, v.currency_code, v.effective_from
			FROM product_price_versions v
			JOIN commercial_products p ON p.product_id = v.product_id
			JOIN commercial_currencies c ON c.currency_code = v.currency_code
			WHERE v.status = 'PUBLISHED'
			  AND v.effective_from <= $1
			  AND (v.effective_to IS NULL OR v.effective_to > $1)
			  AND c.sale_enabled
			ORDER BY p.product_id, v.currency_code, v.effective_from DESC, v.version_number DESC`, now)
		if err != nil {
			return err
		}
		defer rows.Close()

		type latest struct {
			summary domain.ProductSummary
			at      time.Time
		}
		byProduct := map[string]*latest{}
		var order []string
		for rows.Next() {
			var id, code, name, currency string
			var kind domain.ProductKind
			var effFrom time.Time
			if err := rows.Scan(&id, &code, &kind, &name, &currency, &effFrom); err != nil {
				return err
			}
			l, ok := byProduct[id]
			if !ok {
				l = &latest{summary: domain.ProductSummary{ProductID: id, ProductCode: code, ProductKind: kind}}
				byProduct[id] = l
				order = append(order, id)
			}
			l.summary.Currencies = append(l.summary.Currencies, currency)
			if effFrom.After(l.at) || l.summary.DisplayName == "" {
				l.summary.DisplayName, l.at = name, effFrom
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range order {
			out = append(out, byProduct[id].summary)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ProductCode < out[j].ProductCode })
		return nil
	})
	return out, err
}

// ── Price version reads ──────────────────────────────────────────────────────

const priceVersionColumns = `
	v.price_version_id, v.product_id, p.product_code, p.product_kind, v.version_number,
	v.supersedes_price_version_id, v.display_name, v.billing_interval, v.billing_interval_count,
	v.currency_code, v.market_codes, v.effective_from, v.effective_to, v.change_reason,
	v.terms_document_ref, v.terms_document_sha256, v.auto_renew, v.renewal_notice_days,
	v.minimum_term_intervals, v.trial_duration_days, v.trial_conversion,
	v.trial_payment_method_required, v.terms_set_at, v.terms_set_by_principal_id,
	v.status, v.row_version, v.content_sha256, v.created_at, v.created_by_principal_id,
	v.submitted_at, v.submitted_by_principal_id, v.approved_at, v.approved_by_principal_id,
	v.approved_content_sha256, v.published_at, v.published_by_principal_id,
	v.retired_at, v.retired_by_principal_id, v.retire_reason,
	v.last_rejected_at, v.last_rejected_by_principal_id, v.last_rejection_reason`

const priceVersionFrom = `
	FROM product_price_versions v
	JOIN commercial_products p ON p.product_id = v.product_id`

func scanPriceVersion(row pgx.Row) (*domain.PriceVersion, error) {
	var v domain.PriceVersion
	var (
		termsRef, termsHash, trialConversion, termsSetBy *string
		autoRenew, trialPaymentRequired                  *bool
		noticeDays, minTerm, trialDays                   *int
		termsSetAt                                       *time.Time
	)
	if err := row.Scan(
		&v.PriceVersionID, &v.ProductID, &v.ProductCode, &v.ProductKind, &v.VersionNumber,
		&v.SupersedesPriceVersionID, &v.DisplayName, &v.BillingInterval, &v.BillingIntervalCount,
		&v.CurrencyCode, &v.MarketCodes, &v.EffectiveFrom, &v.EffectiveTo, &v.ChangeReason,
		&termsRef, &termsHash, &autoRenew, &noticeDays,
		&minTerm, &trialDays, &trialConversion,
		&trialPaymentRequired, &termsSetAt, &termsSetBy,
		&v.Status, &v.RowVersion, &v.ContentSHA256, &v.CreatedAt, &v.CreatedByPrincipalID,
		&v.SubmittedAt, &v.SubmittedByPrincipalID, &v.ApprovedAt, &v.ApprovedByPrincipalID,
		&v.ApprovedContentSHA256, &v.PublishedAt, &v.PublishedByPrincipalID,
		&v.RetiredAt, &v.RetiredByPrincipalID, &v.RetireReason,
		&v.LastRejectedAt, &v.LastRejectedByPrincipalID, &v.LastRejectionReason,
	); err != nil {
		return nil, err
	}
	if termsRef != nil {
		v.Terms = &domain.CommercialTerms{
			TermsDocumentRef: *termsRef, TermsDocumentSHA256: *termsHash, AutoRenew: *autoRenew,
			RenewalNoticeDays: *noticeDays, MinimumTermIntervals: *minTerm,
			SetAt: *termsSetAt, SetByPrincipalID: *termsSetBy,
		}
		if trialDays != nil {
			v.Terms.Trial = &domain.TrialPolicy{
				DurationDays: *trialDays, Conversion: *trialConversion, PaymentMethodRequired: *trialPaymentRequired,
			}
		}
	}
	v.Components = []domain.PriceComponent{}
	v.Capabilities = []domain.PlanCapability{}
	return &v, nil
}

// loadVersion reads one version with its components and capabilities. With
// forUpdate it row-locks the version, which also blocks any concurrent child
// insert (the draft-only trigger takes FOR SHARE on the same row).
func loadVersion(ctx context.Context, tx pgx.Tx, versionID string, forUpdate bool) (*domain.PriceVersion, error) {
	q := `SELECT ` + priceVersionColumns + priceVersionFrom + ` WHERE v.price_version_id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF v`
	}
	v, err := scanPriceVersion(tx.QueryRow(ctx, q, versionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPriceVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := loadChildren(ctx, tx, []*domain.PriceVersion{v}); err != nil {
		return nil, err
	}
	return v, nil
}

func queryVersions(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]*domain.PriceVersion, error) {
	rows, err := tx.Query(ctx, `SELECT `+priceVersionColumns+priceVersionFrom+` `+where, args...)
	if err != nil {
		return nil, err
	}
	var out []*domain.PriceVersion
	for rows.Next() {
		v, err := scanPriceVersion(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := loadChildren(ctx, tx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// loadChildren attaches components, tiers and capabilities to every version
// in vs with three queries, whatever the number of versions.
func loadChildren(ctx context.Context, tx pgx.Tx, vs []*domain.PriceVersion) error {
	if len(vs) == 0 {
		return nil
	}
	ids := make([]string, len(vs))
	byID := make(map[string]*domain.PriceVersion, len(vs))
	for i, v := range vs {
		ids[i] = v.PriceVersionID
		byID[v.PriceVersionID] = v
	}

	rows, err := tx.Query(ctx, `
		SELECT price_component_id, price_version_id, component_key, component_type, amount::text,
		       billing_timing, unit_name, included_quantity::text, minimum_quantity::text,
		       maximum_quantity::text, quantity_rounding, tier_mode, meter_key, meter_version,
		       aggregation_method, trigger_event, eligibility_code, expires_after_days,
		       discount_type, discount_value::text, discount_cap_amount::text, duration_intervals,
		       requires_approval, created_at, created_by_principal_id
		FROM price_components WHERE price_version_id = ANY($1)
		ORDER BY price_version_id, component_key`, ids)
	if err != nil {
		return err
	}
	componentIdx := map[string]*domain.PriceComponent{}
	for rows.Next() {
		var c domain.PriceComponent
		if err := rows.Scan(&c.PriceComponentID, &c.PriceVersionID, &c.ComponentKey, &c.ComponentType, &c.Amount,
			&c.BillingTiming, &c.UnitName, &c.IncludedQuantity, &c.MinimumQuantity,
			&c.MaximumQuantity, &c.QuantityRounding, &c.TierMode, &c.MeterKey, &c.MeterVersion,
			&c.AggregationMethod, &c.TriggerEvent, &c.EligibilityCode, &c.ExpiresAfterDays,
			&c.DiscountType, &c.DiscountValue, &c.DiscountCapAmount, &c.DurationIntervals,
			&c.RequiresApproval, &c.CreatedAt, &c.CreatedByPrincipalID); err != nil {
			rows.Close()
			return err
		}
		v := byID[c.PriceVersionID]
		v.Components = append(v.Components, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Pointers are taken only after the slices stop growing.
	for _, v := range vs {
		for i := range v.Components {
			componentIdx[v.Components[i].PriceComponentID] = &v.Components[i]
		}
	}

	tierRows, err := tx.Query(ctx, `
		SELECT price_component_id, tier_index, up_to_quantity::text, unit_amount::text, flat_amount::text
		FROM price_component_tiers WHERE price_version_id = ANY($1)
		ORDER BY price_component_id, tier_index`, ids)
	if err != nil {
		return err
	}
	for tierRows.Next() {
		var componentID string
		var t domain.PriceTier
		if err := tierRows.Scan(&componentID, &t.TierIndex, &t.UpToQuantity, &t.UnitAmount, &t.FlatAmount); err != nil {
			tierRows.Close()
			return err
		}
		if c := componentIdx[componentID]; c != nil {
			c.Tiers = append(c.Tiers, t)
		}
	}
	tierRows.Close()
	if err := tierRows.Err(); err != nil {
		return err
	}

	capRows, err := tx.Query(ctx, `
		SELECT price_version_id, capability_key, limit_value, limit_unit
		FROM price_version_capabilities WHERE price_version_id = ANY($1)
		ORDER BY price_version_id, capability_key`, ids)
	if err != nil {
		return err
	}
	defer capRows.Close()
	for capRows.Next() {
		var versionID string
		var c domain.PlanCapability
		if err := capRows.Scan(&versionID, &c.CapabilityKey, &c.LimitValue, &c.LimitUnit); err != nil {
			return err
		}
		v := byID[versionID]
		v.Capabilities = append(v.Capabilities, c)
	}
	return capRows.Err()
}

func (s *PgStore) GetPriceVersion(ctx context.Context, versionID string, seller bool) (*domain.PriceVersion, error) {
	var out *domain.PriceVersion
	err := s.catalogTx(ctx, seller, func(tx pgx.Tx) error {
		v, err := loadVersion(ctx, tx, versionID, false)
		out = v
		return err
	})
	return out, err
}

// ListPriceHistory returns a product's versions in version order. Outside the
// seller plane RLS reduces it to published and retired versions.
func (s *PgStore) ListPriceHistory(ctx context.Context, productID string, seller bool) ([]domain.PriceVersion, error) {
	var out []domain.PriceVersion
	err := s.catalogTx(ctx, seller, func(tx pgx.Tx) error {
		if _, err := loadProduct(ctx, tx, productID, false); err != nil {
			return err
		}
		vs, err := queryVersions(ctx, tx, `WHERE v.product_id = $1 ORDER BY v.version_number`, productID)
		if err != nil {
			return err
		}
		out = derefVersions(vs)
		return nil
	})
	return out, err
}

// ResolveSellableOffers returns, per product and currency, the one version a
// new customer can buy at now: published, in effect, in a currency enabled
// for sale, and — when a market is given — offered in that market. Among
// several such versions the latest effective_from wins. An older version
// stops being sold by being retired, never by being edited.
func (s *PgStore) ResolveSellableOffers(ctx context.Context, f domain.SellableOfferFilter, now time.Time) ([]domain.PriceVersion, error) {
	var out []domain.PriceVersion
	err := s.catalogTx(ctx, false, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ON (v.product_id, v.currency_code) v.price_version_id
			FROM product_price_versions v
			JOIN commercial_products p ON p.product_id = v.product_id
			JOIN commercial_currencies c ON c.currency_code = v.currency_code
			WHERE v.status = 'PUBLISHED'
			  AND v.effective_from <= $1
			  AND (v.effective_to IS NULL OR v.effective_to > $1)
			  AND c.sale_enabled
			  AND ($2 = '' OR p.product_code = $2)
			  AND ($3 = '' OR v.currency_code = $3)
			  AND ($4 = '' OR $4 = ANY (v.market_codes))
			ORDER BY v.product_id, v.currency_code, v.effective_from DESC, v.version_number DESC`,
			now, f.ProductCode, f.CurrencyCode, f.MarketCode)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		vs, err := queryVersions(ctx, tx, `WHERE v.price_version_id = ANY($1) ORDER BY p.product_code, v.currency_code`, ids)
		if err != nil {
			return err
		}
		out = derefVersions(vs)
		return nil
	})
	return out, err
}

func derefVersions(vs []*domain.PriceVersion) []domain.PriceVersion {
	out := make([]domain.PriceVersion, len(vs))
	for i, v := range vs {
		out[i] = *v
	}
	return out
}

// ── Drafting ─────────────────────────────────────────────────────────────────

// CreateDraftVersion opens the next version of a product. The version number
// and the version it supersedes are resolved here under a product row lock,
// never taken from the caller. With clonePrevious the new draft starts as a
// copy of the latest version's terms, components and capabilities, and is
// then independent of it.
func (s *PgStore) CreateDraftVersion(ctx context.Context, v *domain.PriceVersion, clonePrevious bool, claim domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	var out *domain.PriceVersion
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if _, err := loadProduct(ctx, tx, v.ProductID, true); err != nil {
			return err
		}
		if _, err := loadCurrency(ctx, tx, v.CurrencyCode); err != nil {
			return err
		}

		var prevID *string
		next := 1
		var prevNumber int
		var id string
		err := tx.QueryRow(ctx, `
			SELECT price_version_id, version_number FROM product_price_versions
			WHERE product_id = $1 ORDER BY version_number DESC LIMIT 1`, v.ProductID).Scan(&id, &prevNumber)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			prevID, next = &id, prevNumber+1
		}

		var prev *domain.PriceVersion
		if clonePrevious {
			if prevID == nil {
				return &domain.ValidationError{Field: "clone_previous", Reason: "product has no previous version to clone"}
			}
			if prev, err = loadVersion(ctx, tx, *prevID, false); err != nil {
				return err
			}
		}

		var termsRef, termsHash, trialConversion, termsSetBy *string
		var autoRenew, trialPayment *bool
		var noticeDays, minTerm, trialDays *int
		var termsSetAt *time.Time
		if prev != nil && prev.Terms != nil {
			t := prev.Terms
			termsRef, termsHash, autoRenew = &t.TermsDocumentRef, &t.TermsDocumentSHA256, &t.AutoRenew
			noticeDays, minTerm = &t.RenewalNoticeDays, &t.MinimumTermIntervals
			termsSetAt, termsSetBy = &v.CreatedAt, &v.CreatedByPrincipalID
			if t.Trial != nil {
				trialDays, trialConversion, trialPayment = &t.Trial.DurationDays, &t.Trial.Conversion, &t.Trial.PaymentMethodRequired
			}
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO product_price_versions (
				price_version_id, product_id, version_number, supersedes_price_version_id,
				display_name, billing_interval, billing_interval_count, currency_code, market_codes,
				effective_from, effective_to, change_reason,
				terms_document_ref, terms_document_sha256, auto_renew, renewal_notice_days,
				minimum_term_intervals, trial_duration_days, trial_conversion,
				trial_payment_method_required, terms_set_at, terms_set_by_principal_id,
				status, row_version, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			        $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
			        'DRAFT', 1, $23, $24)`,
			v.PriceVersionID, v.ProductID, next, prevID,
			v.DisplayName, v.BillingInterval, v.BillingIntervalCount, v.CurrencyCode, v.MarketCodes,
			v.EffectiveFrom, v.EffectiveTo, v.ChangeReason,
			termsRef, termsHash, autoRenew, noticeDays,
			minTerm, trialDays, trialConversion,
			trialPayment, termsSetAt, termsSetBy,
			v.CreatedAt, v.CreatedByPrincipalID); err != nil {
			return err
		}

		if prev != nil {
			for i := range prev.Components {
				c := prev.Components[i]
				c.PriceComponentID = domain.NewCommercialID(domain.PrefixPriceComponent)
				c.PriceVersionID = v.PriceVersionID
				c.CreatedAt, c.CreatedByPrincipalID = v.CreatedAt, v.CreatedByPrincipalID
				if err := insertComponent(ctx, tx, &c); err != nil {
					return err
				}
			}
			if err := insertCapabilities(ctx, tx, v.PriceVersionID, prev.Capabilities); err != nil {
				return err
			}
		}

		created, err := loadVersion(ctx, tx, v.PriceVersionID, false)
		if err != nil {
			return err
		}
		out = created
		return emitVersionEvent(ctx, tx, "price_version.created", created, v.CreatedByPrincipalID, v.ChangeReason, v.CreatedAt)
	})
	return out, err
}

func insertComponent(ctx context.Context, tx pgx.Tx, c *domain.PriceComponent) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO price_components (
			price_component_id, price_version_id, component_key, component_type, amount, billing_timing,
			unit_name, included_quantity, minimum_quantity, maximum_quantity, quantity_rounding, tier_mode,
			meter_key, meter_version, aggregation_method, trigger_event, eligibility_code, expires_after_days,
			discount_type, discount_value, discount_cap_amount, duration_intervals, requires_approval,
			created_at, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5::numeric, $6, $7, $8::numeric, $9::numeric, $10::numeric, $11, $12,
		        $13, $14, $15, $16, $17, $18, $19, $20::numeric, $21::numeric, $22, $23, $24, $25)`,
		c.PriceComponentID, c.PriceVersionID, c.ComponentKey, c.ComponentType, c.Amount, c.BillingTiming,
		c.UnitName, c.IncludedQuantity, c.MinimumQuantity, c.MaximumQuantity, c.QuantityRounding, c.TierMode,
		c.MeterKey, c.MeterVersion, c.AggregationMethod, c.TriggerEvent, c.EligibilityCode, c.ExpiresAfterDays,
		c.DiscountType, c.DiscountValue, c.DiscountCapAmount, c.DurationIntervals, c.RequiresApproval,
		c.CreatedAt, c.CreatedByPrincipalID); err != nil {
		return err
	}
	for _, t := range c.Tiers {
		if _, err := tx.Exec(ctx, `
			INSERT INTO price_component_tiers
				(price_component_id, price_version_id, tier_index, up_to_quantity, unit_amount, flat_amount)
			VALUES ($1, $2, $3, $4::numeric, $5::numeric, $6::numeric)`,
			c.PriceComponentID, c.PriceVersionID, t.TierIndex, t.UpToQuantity, t.UnitAmount, t.FlatAmount); err != nil {
			return err
		}
	}
	return nil
}

func insertCapabilities(ctx context.Context, tx pgx.Tx, versionID string, caps []domain.PlanCapability) error {
	for _, c := range caps {
		if _, err := tx.Exec(ctx, `
			INSERT INTO price_version_capabilities (price_version_id, capability_key, limit_value, limit_unit)
			VALUES ($1, $2, $3, $4)`, versionID, c.CapabilityKey, c.LimitValue, c.LimitUnit); err != nil {
			return err
		}
	}
	return nil
}

func deleteComponent(ctx context.Context, tx pgx.Tx, versionID, componentKey string) (deleted *domain.ComponentType, err error) {
	var componentID string
	var ctype domain.ComponentType
	err = tx.QueryRow(ctx, `
		SELECT price_component_id, component_type FROM price_components
		WHERE price_version_id = $1 AND component_key = $2`, versionID, componentKey).Scan(&componentID, &ctype)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM price_component_tiers WHERE price_component_id = $1`, componentID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM price_components WHERE price_component_id = $1`, componentID); err != nil {
		return nil, err
	}
	return &ctype, nil
}

// mutateDraft is the shared frame for every draft edit: claim, lock, check
// the version is still a DRAFT at the expected row_version, apply, advance
// row_version, and return the new state — all in one transaction. fn reports
// whether it already advanced row_version itself (the trigger requires each
// UPDATE to advance it by exactly one).
func (s *PgStore) mutateDraft(ctx context.Context, versionID string, expectedVersion int, claim domain.IdempotencyClaim,
	fn func(tx pgx.Tx, v *domain.PriceVersion) (bumped bool, err error)) (*domain.PriceVersion, error) {
	var out *domain.PriceVersion
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		v, err := loadVersion(ctx, tx, versionID, true)
		if err != nil {
			return err
		}
		if v.Status != domain.PriceVersionDraft {
			return domain.ErrPriceVersionImmutable
		}
		if v.RowVersion != expectedVersion {
			return domain.ErrVersionConflict
		}
		bumped, err := fn(tx, v)
		if err != nil {
			return err
		}
		if !bumped {
			if _, err := tx.Exec(ctx, `
				UPDATE product_price_versions SET row_version = row_version + 1
				WHERE price_version_id = $1`, versionID); err != nil {
				return err
			}
		}
		out, err = loadVersion(ctx, tx, versionID, false)
		return err
	})
	return out, err
}

// PutPriceComponent adds a component to a draft, replacing any existing
// component with the same key. Components are never updated in place.
func (s *PgStore) PutPriceComponent(ctx context.Context, versionID string, expectedVersion int, c *domain.PriceComponent, claim domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	var meterChanged bool
	v, err := s.mutateDraft(ctx, versionID, expectedVersion, claim, func(tx pgx.Tx, v *domain.PriceVersion) (bool, error) {
		cur, err := loadCurrency(ctx, tx, v.CurrencyCode)
		if err != nil {
			return false, err
		}
		if err := domain.ValidateComponent(c, *cur); err != nil {
			return false, err
		}
		replaced, err := deleteComponent(ctx, tx, versionID, c.ComponentKey)
		if err != nil {
			return false, err
		}
		c.PriceComponentID = domain.NewCommercialID(domain.PrefixPriceComponent)
		c.PriceVersionID = versionID
		if err := insertComponent(ctx, tx, c); err != nil {
			return false, err
		}
		meterChanged = c.ComponentType == domain.ComponentMetered ||
			(replaced != nil && *replaced == domain.ComponentMetered)
		if meterChanged {
			if err := emitVersionEvent(ctx, tx, "price_version.meter_binding_changed", v, c.CreatedByPrincipalID, c.ComponentKey, c.CreatedAt); err != nil {
				return false, err
			}
		}
		return false, nil
	})
	return v, err
}

// RemovePriceComponent drops one component from a draft.
func (s *PgStore) RemovePriceComponent(ctx context.Context, versionID string, expectedVersion int, componentKey, actor string, claim domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	return s.mutateDraft(ctx, versionID, expectedVersion, claim, func(tx pgx.Tx, v *domain.PriceVersion) (bool, error) {
		removed, err := deleteComponent(ctx, tx, versionID, componentKey)
		if err != nil {
			return false, err
		}
		if removed == nil {
			return false, domain.ErrPriceComponentNotFound
		}
		if *removed == domain.ComponentMetered {
			if err := emitVersionEvent(ctx, tx, "price_version.meter_binding_changed", v, actor, componentKey, time.Now().UTC()); err != nil {
				return false, err
			}
		}
		return false, nil
	})
}

// SetCommercialTerms replaces the draft's CommercialTermVersion and trial.
func (s *PgStore) SetCommercialTerms(ctx context.Context, versionID string, expectedVersion int, t *domain.CommercialTerms, claim domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	return s.mutateDraft(ctx, versionID, expectedVersion, claim, func(tx pgx.Tx, v *domain.PriceVersion) (bool, error) {
		var trialDays *int
		var trialConversion *string
		var trialPayment *bool
		if t.Trial != nil {
			trialDays, trialConversion, trialPayment = &t.Trial.DurationDays, &t.Trial.Conversion, &t.Trial.PaymentMethodRequired
		}
		if _, err := tx.Exec(ctx, `
			UPDATE product_price_versions SET
				row_version = row_version + 1,
				terms_document_ref = $2, terms_document_sha256 = $3, auto_renew = $4,
				renewal_notice_days = $5, minimum_term_intervals = $6,
				trial_duration_days = $7, trial_conversion = $8, trial_payment_method_required = $9,
				terms_set_at = $10, terms_set_by_principal_id = $11
			WHERE price_version_id = $1`,
			versionID, t.TermsDocumentRef, t.TermsDocumentSHA256, t.AutoRenew,
			t.RenewalNoticeDays, t.MinimumTermIntervals,
			trialDays, trialConversion, trialPayment, t.SetAt, t.SetByPrincipalID); err != nil {
			return false, err
		}
		v.RowVersion++
		return true, emitVersionEvent(ctx, tx, "price_version.commercial_terms_changed", v, t.SetByPrincipalID, "", t.SetAt)
	})
}

// SetCapabilities replaces the draft's plan capability matrix.
func (s *PgStore) SetCapabilities(ctx context.Context, versionID string, expectedVersion int, caps []domain.PlanCapability, actor string, claim domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	return s.mutateDraft(ctx, versionID, expectedVersion, claim, func(tx pgx.Tx, v *domain.PriceVersion) (bool, error) {
		if err := domain.ValidateCapabilities(caps); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM price_version_capabilities WHERE price_version_id = $1`, versionID); err != nil {
			return false, err
		}
		return false, insertCapabilities(ctx, tx, versionID, caps)
	})
}

// ── Lifecycle transitions ────────────────────────────────────────────────────

// transition is the shared frame for lifecycle commands: claim, lock, check
// the source state and expected row_version, then apply.
func (s *PgStore) transition(ctx context.Context, versionID string, expectedVersion int, from domain.PriceVersionStatus,
	claim domain.IdempotencyClaim, apply func(tx pgx.Tx, v *domain.PriceVersion) error) (*domain.PriceVersion, error) {
	var out *domain.PriceVersion
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		v, err := loadVersion(ctx, tx, versionID, true)
		if err != nil {
			return err
		}
		if v.Status != from {
			return fmt.Errorf("%w: version is %s, this action requires %s", domain.ErrPriceVersionInvalidState, v.Status, from)
		}
		if v.RowVersion != expectedVersion {
			return domain.ErrVersionConflict
		}
		if err := apply(tx, v); err != nil {
			return err
		}
		out, err = loadVersion(ctx, tx, versionID, false)
		return err
	})
	return out, err
}

func (s *PgStore) SubmitForApproval(ctx context.Context, versionID string, expectedVersion int, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	return s.transition(ctx, versionID, expectedVersion, domain.PriceVersionDraft, claim, func(tx pgx.Tx, v *domain.PriceVersion) error {
		cur, err := loadCurrency(ctx, tx, v.CurrencyCode)
		if err != nil {
			return err
		}
		if err := domain.CheckSubmittable(v, *cur, now); err != nil {
			return err
		}
		hash := domain.ContentHash(v)
		if _, err := tx.Exec(ctx, `
			UPDATE product_price_versions SET
				status = 'REVIEW', row_version = row_version + 1,
				submitted_at = $2, submitted_by_principal_id = $3, content_sha256 = $4
			WHERE price_version_id = $1`, versionID, now, actor, hash); err != nil {
			return err
		}
		v.Status, v.RowVersion, v.ContentSHA256 = domain.PriceVersionReview, v.RowVersion+1, &hash
		return emitVersionEvent(ctx, tx, "price_version.submitted", v, actor, "", now)
	})
}

// ApprovePriceVersion is the checker step (COM-CTRL-004). The approver may be
// neither the version's creator nor its submitter, and approves the exact
// content hash that was submitted.
func (s *PgStore) ApprovePriceVersion(ctx context.Context, versionID string, expectedVersion int, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	return s.transition(ctx, versionID, expectedVersion, domain.PriceVersionReview, claim, func(tx pgx.Tx, v *domain.PriceVersion) error {
		if actor == v.CreatedByPrincipalID || (v.SubmittedByPrincipalID != nil && actor == *v.SubmittedByPrincipalID) {
			return domain.ErrSoDViolation
		}
		if v.ContentSHA256 == nil || domain.ContentHash(v) != *v.ContentSHA256 {
			return domain.ErrContentHashMismatch
		}
		if _, err := tx.Exec(ctx, `
			UPDATE product_price_versions SET
				status = 'APPROVED', row_version = row_version + 1,
				approved_at = $2, approved_by_principal_id = $3, approved_content_sha256 = content_sha256
			WHERE price_version_id = $1`, versionID, now, actor); err != nil {
			return err
		}
		v.Status, v.RowVersion = domain.PriceVersionApproved, v.RowVersion+1
		return emitVersionEvent(ctx, tx, "price_version.approved", v, actor, "", now)
	})
}

// RejectPriceVersion returns a version under review to DRAFT for correction.
// The rejection is kept on the row and published as an event.
func (s *PgStore) RejectPriceVersion(ctx context.Context, versionID string, expectedVersion int, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	return s.transition(ctx, versionID, expectedVersion, domain.PriceVersionReview, claim, func(tx pgx.Tx, v *domain.PriceVersion) error {
		if _, err := tx.Exec(ctx, `
			UPDATE product_price_versions SET
				status = 'DRAFT', row_version = row_version + 1,
				submitted_at = NULL, submitted_by_principal_id = NULL, content_sha256 = NULL,
				last_rejected_at = $2, last_rejected_by_principal_id = $3, last_rejection_reason = $4
			WHERE price_version_id = $1`, versionID, now, actor, reason); err != nil {
			return err
		}
		v.Status, v.RowVersion, v.ContentSHA256 = domain.PriceVersionDraft, v.RowVersion+1, nil
		return emitVersionEvent(ctx, tx, "price_version.rejected", v, actor, reason, now)
	})
}

// PublishPriceVersion makes an approved version sellable from its
// effective_from. The content is re-hashed against the approved hash first:
// what is published is exactly what was approved.
func (s *PgStore) PublishPriceVersion(ctx context.Context, versionID string, expectedVersion int, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	return s.transition(ctx, versionID, expectedVersion, domain.PriceVersionApproved, claim, func(tx pgx.Tx, v *domain.PriceVersion) error {
		if v.ApprovedContentSHA256 == nil || domain.ContentHash(v) != *v.ApprovedContentSHA256 {
			return domain.ErrContentHashMismatch
		}
		cur, err := loadCurrency(ctx, tx, v.CurrencyCode)
		if err != nil {
			return err
		}
		if err := domain.CheckPublishable(v, *cur, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE product_price_versions SET
				status = 'PUBLISHED', row_version = row_version + 1,
				published_at = $2, published_by_principal_id = $3
			WHERE price_version_id = $1`, versionID, now, actor); err != nil {
			return err
		}
		v.Status, v.RowVersion = domain.PriceVersionPublished, v.RowVersion+1
		return emitVersionEvent(ctx, tx, "price_version.published", v, actor, "", now)
	})
}

// RetirePriceVersion stops new sales of a published version. Subscriptions
// already bound to it keep it, and it stays readable for history and invoice
// reconstruction.
func (s *PgStore) RetirePriceVersion(ctx context.Context, versionID string, expectedVersion int, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.PriceVersion, error) {
	return s.transition(ctx, versionID, expectedVersion, domain.PriceVersionPublished, claim, func(tx pgx.Tx, v *domain.PriceVersion) error {
		if _, err := tx.Exec(ctx, `
			UPDATE product_price_versions SET
				status = 'RETIRED', row_version = row_version + 1,
				retired_at = $2, retired_by_principal_id = $3, retire_reason = $4
			WHERE price_version_id = $1`, versionID, now, actor, reason); err != nil {
			return err
		}
		v.Status, v.RowVersion = domain.PriceVersionRetired, v.RowVersion+1
		return emitVersionEvent(ctx, tx, "price_version.retired", v, actor, reason, now)
	})
}
