package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/store"
)

// COM-01 Product & Price Book against real Postgres (migration 000006), run as
// the NOSUPERUSER NOBYPASSRLS role so every RLS policy is live. Negative
// controls go around the store with raw SQL: the point is that the database
// itself refuses, not merely that the Go code declines to ask.

const (
	maker     = "alice"
	checker   = "bob"
	publisher = "carol"
)

var t0 = time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)

func day(n int) time.Time { return t0.Add(time.Duration(n) * 24 * time.Hour) }

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }
func boolp(b bool) *bool    { return &b }

type pbFixture struct {
	t     *testing.T
	ctx   context.Context
	admin *pgxpool.Pool
	app   *pgxpool.Pool
	s     *store.PgStore
	mu    sync.Mutex
	keys  int
}

func newPB(t *testing.T) *pbFixture {
	t.Helper()
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	return &pbFixture{t: t, ctx: context.Background(), admin: admin, app: app, s: store.NewPgStore(app)}
}

func (f *pbFixture) claim(principal, op, resource string) domain.IdempotencyClaim {
	f.mu.Lock()
	f.keys++
	n := f.keys
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", op, resource, n)))
	return domain.IdempotencyClaim{
		OwnerScope: domain.SellerScope, PrincipalID: principal, Key: fmt.Sprintf("key-%d", n),
		Operation: op, RequestSHA256: hex.EncodeToString(sum[:]), ResourceID: resource,
	}
}

func (f *pbFixture) currency(code string, minor int, enabled bool) {
	f.t.Helper()
	if _, err := f.s.UpsertCurrency(f.ctx, &domain.CommercialCurrency{
		CurrencyCode: code, MinorUnits: minor, SaleEnabled: enabled, UpdatedByPrincipalID: maker,
	}); err != nil {
		f.t.Fatalf("upsert currency %s: %v", code, err)
	}
}

func (f *pbFixture) product(code string) *domain.Product {
	f.t.Helper()
	p := &domain.Product{ProductID: domain.NewCommercialID(domain.PrefixProduct), ProductCode: code,
		ProductKind: domain.ProductKindPlan, CreatedByPrincipalID: maker}
	got, err := f.s.CreateProduct(f.ctx, p, f.claim(maker, "CreateProduct", p.ProductID))
	if err != nil {
		f.t.Fatalf("create product %s: %v", code, err)
	}
	return got
}

func (f *pbFixture) draft(productID string, effFrom time.Time, clone bool, by string) *domain.PriceVersion {
	f.t.Helper()
	v := &domain.PriceVersion{
		PriceVersionID: domain.NewCommercialID(domain.PrefixPriceVersion), ProductID: productID,
		DisplayName: "Business", BillingInterval: "MONTH", BillingIntervalCount: 1, CurrencyCode: "USD",
		MarketCodes: []string{"GB", "US"}, EffectiveFrom: effFrom, ChangeReason: "2030 price book",
		CreatedAt: t0, CreatedByPrincipalID: by,
	}
	got, err := f.s.CreateDraftVersion(f.ctx, v, clone, f.claim(by, "CreateDraftVersion", v.PriceVersionID))
	if err != nil {
		f.t.Fatalf("create draft: %v", err)
	}
	return got
}

func (f *pbFixture) put(v *domain.PriceVersion, c domain.PriceComponent) *domain.PriceVersion {
	f.t.Helper()
	c.CreatedAt, c.CreatedByPrincipalID = t0, maker
	got, err := f.s.PutPriceComponent(f.ctx, v.PriceVersionID, v.RowVersion, &c, f.claim(maker, "AddPriceComponent", v.PriceVersionID))
	if err != nil {
		f.t.Fatalf("put component %s: %v", c.ComponentKey, err)
	}
	return got
}

func baseComponent(amount string) domain.PriceComponent {
	return domain.PriceComponent{ComponentKey: "base", ComponentType: domain.ComponentRecurringFixed,
		Amount: strp(amount), BillingTiming: strp("IN_ADVANCE")}
}

func seatsComponent() domain.PriceComponent {
	return domain.PriceComponent{ComponentKey: "seats", ComponentType: domain.ComponentPerUnit, UnitName: strp("seat"),
		BillingTiming: strp("IN_ADVANCE"), IncludedQuantity: strp("5"), QuantityRounding: strp("UP"), TierMode: strp("GRADUATED"),
		Tiers: []domain.PriceTier{
			{TierIndex: 1, UpToQuantity: strp("50"), UnitAmount: "12.00"},
			{TierIndex: 2, UnitAmount: "9.5000"},
		}}
}

func (f *pbFixture) terms(v *domain.PriceVersion, trial bool) *domain.PriceVersion {
	f.t.Helper()
	t := &domain.CommercialTerms{TermsDocumentRef: "legal/terms/2030-01", TermsDocumentSHA256: strings.Repeat("ab", 32),
		AutoRenew: true, RenewalNoticeDays: 30, MinimumTermIntervals: 1, SetAt: t0, SetByPrincipalID: maker}
	if trial {
		t.Trial = &domain.TrialPolicy{DurationDays: 14, Conversion: "REQUIRE_CONFIRMATION", PaymentMethodRequired: false}
	}
	got, err := f.s.SetCommercialTerms(f.ctx, v.PriceVersionID, v.RowVersion, t, f.claim(maker, "SetCommercialTerms", v.PriceVersionID))
	if err != nil {
		f.t.Fatalf("set terms: %v", err)
	}
	return got
}

func (f *pbFixture) submit(v *domain.PriceVersion, by string, at time.Time) (*domain.PriceVersion, error) {
	return f.s.SubmitForApproval(f.ctx, v.PriceVersionID, v.RowVersion, by, at, f.claim(by, "submit", v.PriceVersionID))
}

func (f *pbFixture) approve(v *domain.PriceVersion, by string, at time.Time) (*domain.PriceVersion, error) {
	return f.s.ApprovePriceVersion(f.ctx, v.PriceVersionID, v.RowVersion, by, at, f.claim(by, "approve", v.PriceVersionID))
}

func (f *pbFixture) publish(v *domain.PriceVersion, by string, at time.Time) (*domain.PriceVersion, error) {
	return f.s.PublishPriceVersion(f.ctx, v.PriceVersionID, v.RowVersion, by, at, f.claim(by, "publish", v.PriceVersionID))
}

func (f *pbFixture) ok(v *domain.PriceVersion, err error) *domain.PriceVersion {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("unexpected error: %v", err)
	}
	return v
}

// publishedVersion runs the whole maker-checker lifecycle: alice drafts and
// submits, bob approves, carol publishes two days after t0.
func (f *pbFixture) publishedVersion(productID string, effFrom time.Time, baseAmount string) *domain.PriceVersion {
	f.t.Helper()
	v := f.draft(productID, effFrom, false, maker)
	v = f.put(v, baseComponent(baseAmount))
	v = f.put(v, seatsComponent())
	v = f.terms(v, false)
	v = f.ok(f.submit(v, maker, t0))
	v = f.ok(f.approve(v, checker, day(1)))
	return f.ok(f.publish(v, publisher, day(2)))
}

// sellerExec runs one raw statement as the seller plane, around the store.
func (f *pbFixture) sellerExec(sql string, args ...any) (pgconn.CommandTag, error) {
	return f.rawExec(true, "", sql, args...)
}

// tenantExec runs one raw statement as a tenant-scoped transaction.
func (f *pbFixture) tenantExec(org, sql string, args ...any) (pgconn.CommandTag, error) {
	return f.rawExec(false, org, sql, args...)
}

func (f *pbFixture) rawExec(seller bool, org, sql string, args ...any) (pgconn.CommandTag, error) {
	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if seller {
		if _, err := tx.Exec(f.ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
			f.t.Fatalf("set seller plane: %v", err)
		}
	}
	if org != "" {
		if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", org); err != nil {
			f.t.Fatalf("set tenant: %v", err)
		}
	}
	tag, err := tx.Exec(f.ctx, sql, args...)
	if err != nil {
		return tag, err
	}
	return tag, tx.Commit(f.ctx)
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func (f *pbFixture) outboxCount(aggregateID, eventType string) int {
	f.t.Helper()
	var n int
	if err := f.admin.QueryRow(f.ctx,
		`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, aggregateID, eventType,
	).Scan(&n); err != nil {
		f.t.Fatalf("count outbox: %v", err)
	}
	return n
}

// ── Lifecycle ────────────────────────────────────────────────────────────────

func TestPriceBook_FullLifecycle_PublishesAndResolves(t *testing.T) {
	f := newPB(t)
	f.currency("USD", 2, true)
	p := f.product("business")

	v := f.draft(p.ProductID, day(10), false, maker)
	if v.Status != domain.PriceVersionDraft || v.VersionNumber != 1 || v.SupersedesPriceVersionID != nil {
		t.Fatalf("new draft: status=%s number=%d supersedes=%v", v.Status, v.VersionNumber, v.SupersedesPriceVersionID)
	}
	v = f.put(v, baseComponent("49.00"))
	v = f.put(v, seatsComponent())
	v = f.put(v, domain.PriceComponent{ComponentKey: "setup", ComponentType: domain.ComponentOneTime, Amount: strp("199.00"),
		TriggerEvent: strp("SUBSCRIPTION_START")})
	v = f.put(v, domain.PriceComponent{ComponentKey: "launch_promo", ComponentType: domain.ComponentDiscount,
		DiscountType: strp("PERCENT"), DiscountValue: strp("20"), DurationIntervals: intp(3),
		EligibilityCode: strp("NEW_CUSTOMER"), RequiresApproval: boolp(true)})
	v = f.terms(v, true)
	limit := int64(10)
	v = f.ok(f.s.SetCapabilities(f.ctx, v.PriceVersionID, v.RowVersion, []domain.PlanCapability{
		{CapabilityKey: "users", LimitValue: &limit, LimitUnit: strp("user")},
		{CapabilityKey: "storage", LimitValue: nil},
	}, maker, f.claim(maker, "SetPlanCapabilities", v.PriceVersionID)))

	v = f.ok(f.submit(v, maker, t0))
	if v.Status != domain.PriceVersionReview || v.ContentSHA256 == nil || *v.ContentSHA256 != domain.ContentHash(v) {
		t.Fatalf("after submit: status=%s hash=%v", v.Status, v.ContentSHA256)
	}
	v = f.ok(f.approve(v, checker, day(1)))
	if v.Status != domain.PriceVersionApproved || *v.ApprovedContentSHA256 != *v.ContentSHA256 || *v.ApprovedByPrincipalID != checker {
		t.Fatalf("after approve: %+v", v)
	}
	v = f.ok(f.publish(v, publisher, day(2)))
	if v.Status != domain.PriceVersionPublished || *v.PublishedByPrincipalID != publisher {
		t.Fatalf("after publish: status=%s", v.Status)
	}

	early, err := f.s.ResolveSellableOffers(f.ctx, domain.SellableOfferFilter{ProductCode: "business"}, day(5))
	if err != nil || len(early) != 0 {
		t.Fatalf("a price was sellable before its effective_from: %d offers, err=%v", len(early), err)
	}
	offers, err := f.s.ResolveSellableOffers(f.ctx, domain.SellableOfferFilter{ProductCode: "business", CurrencyCode: "USD", MarketCode: "GB"}, day(11))
	if err != nil || len(offers) != 1 {
		t.Fatalf("want one sellable offer, got %d (err=%v)", len(offers), err)
	}
	o := offers[0]
	if o.PriceVersionID != v.PriceVersionID || len(o.Components) != 4 || len(o.Capabilities) != 2 {
		t.Fatalf("offer content: id=%s components=%d capabilities=%d", o.PriceVersionID, len(o.Components), len(o.Capabilities))
	}
	if o.Terms == nil || o.Terms.Trial == nil || o.Terms.Trial.Conversion != "REQUIRE_CONFIRMATION" {
		t.Fatalf("offer must disclose its trial policy: %+v", o.Terms)
	}
	for _, c := range o.Components {
		if c.ComponentKey == "seats" && (len(c.Tiers) != 2 || c.Tiers[1].UnitAmount != "9.5000") {
			t.Fatalf("tiers must round-trip exactly, scale included: %+v", c.Tiers)
		}
	}
	if none, _ := f.s.ResolveSellableOffers(f.ctx, domain.SellableOfferFilter{MarketCode: "FR"}, day(11)); len(none) != 0 {
		t.Fatalf("offer resolved in a market it is not offered in")
	}
	products, err := f.s.ListPublishedProducts(f.ctx, day(11))
	if err != nil || len(products) != 1 || products[0].ProductCode != "business" {
		t.Fatalf("published products: %+v (err=%v)", products, err)
	}

	for _, ev := range []string{"price_version.created", "price_version.commercial_terms_changed",
		"price_version.submitted", "price_version.approved", "price_version.published"} {
		if n := f.outboxCount(v.PriceVersionID, ev); n != 1 {
			t.Errorf("outbox %s: %d rows, want exactly 1 (written in the same transaction as the change)", ev, n)
		}
	}
}

// COM-CTRL-004 / negative path #06: the checker is never the maker.
func TestPriceBook_ApproverMustBeIndependent(t *testing.T) {
	f := newPB(t)
	f.currency("USD", 2, true)

	v := f.draft(f.product("solo").ProductID, day(10), false, maker)
	v = f.terms(f.put(v, baseComponent("10.00")), false)
	v = f.ok(f.submit(v, maker, t0))
	if _, err := f.approve(v, maker, day(1)); !errors.Is(err, domain.ErrSoDViolation) {
		t.Fatalf("creator approved their own price: %v", err)
	}

	// Created by alice, submitted by dave: neither may approve.
	w := f.draft(f.product("relay").ProductID, day(10), false, maker)
	w = f.terms(f.put(w, baseComponent("10.00")), false)
	w = f.ok(f.submit(w, "dave", t0))
	if _, err := f.approve(w, "dave", day(1)); !errors.Is(err, domain.ErrSoDViolation) {
		t.Fatalf("submitter approved the version they submitted: %v", err)
	}
	if _, err := f.approve(w, checker, day(1)); err != nil {
		t.Fatalf("an independent checker must be able to approve: %v", err)
	}

	// Backstop: even a raw UPDATE cannot record a self-approval.
	_, err := f.sellerExec(`
		UPDATE product_price_versions
		   SET status = 'APPROVED', row_version = row_version + 1, approved_at = NOW(),
		       approved_by_principal_id = created_by_principal_id, approved_content_sha256 = content_sha256
		 WHERE price_version_id = $1`, v.PriceVersionID)
	if pgCode(err) != "23514" || !strings.Contains(err.Error(), "price_versions_approver_is_independent") {
		t.Fatalf("raw self-approval was not refused by the schema: %v", err)
	}
	still, _ := f.s.GetPriceVersion(f.ctx, v.PriceVersionID, true)
	if still.Status != domain.PriceVersionReview {
		t.Fatalf("version left REVIEW after refused approvals: %s", still.Status)
	}
}

// COM-CTRL-002 / negative path #03: a published price is never edited in
// place — not through the store, and not through raw SQL either.
func TestPriceBook_PublishedPriceCannotBeEditedInPlace(t *testing.T) {
	f := newPB(t)
	f.currency("USD", 2, true)
	v := f.publishedVersion(f.product("business").ProductID, day(10), "49.00")
	hashBefore := *v.ContentSHA256

	if _, err := f.s.PutPriceComponent(f.ctx, v.PriceVersionID, v.RowVersion,
		&domain.PriceComponent{ComponentKey: "base", ComponentType: domain.ComponentRecurringFixed, Amount: strp("1.00"),
			BillingTiming: strp("IN_ADVANCE"), CreatedAt: t0, CreatedByPrincipalID: maker},
		f.claim(maker, "AddPriceComponent", v.PriceVersionID)); !errors.Is(err, domain.ErrPriceVersionImmutable) {
		t.Fatalf("store accepted a component change on a published version: %v", err)
	}

	raw := map[string]string{
		"update component amount": `UPDATE price_components SET amount = 1.00 WHERE price_version_id = $1`,
		"delete components":       `DELETE FROM price_components WHERE price_version_id = $1`,
		"update tier":             `UPDATE price_component_tiers SET unit_amount = 0.01 WHERE price_version_id = $1`,
		"insert component": `INSERT INTO price_components (price_component_id, price_version_id, component_key, component_type,
			amount, billing_timing, created_by_principal_id)
			VALUES ('cpc_00000000-0000-4000-8000-000000000001', $1, 'sneaky', 'RECURRING_FIXED', 0.01, 'IN_ADVANCE', 'mallory')`,
		"relabel version":    `UPDATE product_price_versions SET display_name = 'Cheap', row_version = row_version + 1 WHERE price_version_id = $1`,
		"back to draft":      `UPDATE product_price_versions SET status = 'DRAFT', row_version = row_version + 1 WHERE price_version_id = $1`,
		"shift effective":    `UPDATE product_price_versions SET effective_from = effective_from - interval '1 day', row_version = row_version + 1 WHERE price_version_id = $1`,
		"delete version":     `DELETE FROM product_price_versions WHERE price_version_id = $1`,
		"capability changed": `DELETE FROM price_version_capabilities WHERE price_version_id = $1`,
	}
	// The published version has no capabilities, so a DELETE would target
	// nothing and prove nothing; an INSERT is the real attempt.
	raw["capability changed"] = `INSERT INTO price_version_capabilities (price_version_id, capability_key) VALUES ($1, 'extra')`

	// Two independent layers, each proven on its own:
	//   1. As the application role on the seller plane, the statement is
	//      refused or reaches no row (tables with no UPDATE/DELETE policy are
	//      invisible to those commands under FORCE RLS).
	//   2. As the superuser, which bypasses RLS entirely — an operator, a
	//      migration, a restore — the trigger still refuses with CP001.
	for name, sql := range raw {
		tag, err := f.sellerExec(sql, v.PriceVersionID)
		if err == nil && tag.RowsAffected() != 0 {
			t.Errorf("%s: application role changed %d published rows", name, tag.RowsAffected())
		} else if err != nil && pgCode(err) != "CP001" {
			t.Errorf("%s: application role got an unexpected error: %v", name, err)
		}
		if _, err := f.admin.Exec(f.ctx, sql, v.PriceVersionID); pgCode(err) != "CP001" {
			t.Errorf("%s: with RLS bypassed, the trigger did not refuse with CP001: %v", name, err)
		}
	}

	retired, err := f.s.RetirePriceVersion(f.ctx, v.PriceVersionID, v.RowVersion, publisher, "replaced by 2031 book", day(20),
		f.claim(publisher, "retire", v.PriceVersionID))
	if err != nil || retired.Status != domain.PriceVersionRetired {
		t.Fatalf("retirement is the one change a published version allows: %v", err)
	}
	if _, err := f.sellerExec(`UPDATE product_price_versions SET retire_reason = 'rewritten', row_version = row_version + 1 WHERE price_version_id = $1`,
		v.PriceVersionID); pgCode(err) != "CP001" {
		t.Fatalf("a retired version accepted an update: %v", err)
	}
	if _, err := f.s.RetirePriceVersion(f.ctx, v.PriceVersionID, retired.RowVersion, publisher, "again", day(21),
		f.claim(publisher, "retire", v.PriceVersionID)); !errors.Is(err, domain.ErrPriceVersionInvalidState) {
		t.Fatalf("retiring a retired version: %v", err)
	}

	after, _ := f.s.GetPriceVersion(f.ctx, v.PriceVersionID, true)
	if domain.ContentHash(after) != hashBefore || *after.Components[0].Amount != "49.00" {
		t.Fatal("published content changed despite every refusal")
	}
}

// COM-CTRL-003 / negative path #04: publishing a newer price leaves the old
// version exactly as it was — the basis existing subscriptions stay bound to.
func TestPriceBook_NewVersionDoesNotAlterPublishedOne(t *testing.T) {
	f := newPB(t)
	f.currency("USD", 2, true)
	p := f.product("business")
	v1 := f.publishedVersion(p.ProductID, day(10), "49.00")

	v2 := f.draft(p.ProductID, day(40), true, maker)
	if v2.VersionNumber != 2 || v2.SupersedesPriceVersionID == nil || *v2.SupersedesPriceVersionID != v1.PriceVersionID {
		t.Fatalf("v2 lineage: number=%d supersedes=%v", v2.VersionNumber, v2.SupersedesPriceVersionID)
	}
	if len(v2.Components) != 2 || v2.Terms == nil {
		t.Fatalf("clone_previous must copy components and terms: %d components, terms=%v", len(v2.Components), v2.Terms)
	}
	for i := range v2.Components {
		if v2.Components[i].PriceComponentID == v1.Components[i].PriceComponentID {
			t.Fatal("cloned components must be new rows, not shared with the published version")
		}
	}
	v2 = f.put(v2, baseComponent("59.00"))
	v2 = f.ok(f.submit(v2, maker, day(20)))
	v2 = f.ok(f.approve(v2, checker, day(21)))
	v2 = f.ok(f.publish(v2, publisher, day(22)))

	after, err := f.s.GetPriceVersion(f.ctx, v1.PriceVersionID, false)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != domain.PriceVersionPublished || *after.ContentSHA256 != *v1.ContentSHA256 || domain.ContentHash(after) != *v1.ContentSHA256 {
		t.Fatal("publishing v2 altered v1")
	}
	for _, c := range after.Components {
		if c.ComponentKey == "base" && *c.Amount != "49.00" {
			t.Fatalf("v1 base price became %s after v2 was published", *c.Amount)
		}
	}

	resolve := func(at time.Time) string {
		offers, err := f.s.ResolveSellableOffers(f.ctx, domain.SellableOfferFilter{ProductCode: "business"}, at)
		if err != nil || len(offers) != 1 {
			t.Fatalf("resolve at %s: %d offers, err=%v", at, len(offers), err)
		}
		return offers[0].PriceVersionID
	}
	if got := resolve(day(30)); got != v1.PriceVersionID {
		t.Fatalf("before v2 takes effect the sellable offer must still be v1, got %s", got)
	}
	if got := resolve(day(41)); got != v2.PriceVersionID {
		t.Fatalf("after v2 takes effect the sellable offer must be v2, got %s", got)
	}

	history, err := f.s.ListPriceHistory(f.ctx, p.ProductID, false)
	if err != nil || len(history) != 2 || history[0].VersionNumber != 1 || history[1].VersionNumber != 2 {
		t.Fatalf("history: %d versions (err=%v)", len(history), err)
	}
}

// ── Idempotency and concurrency ──────────────────────────────────────────────

func TestPriceBook_IdempotentCommands(t *testing.T) {
	f := newPB(t)
	f.currency("USD", 2, true)

	first := &domain.Product{ProductID: domain.NewCommercialID(domain.PrefixProduct), ProductCode: "teams",
		ProductKind: domain.ProductKindPlan, CreatedByPrincipalID: maker}
	claim := f.claim(maker, "CreateProduct", first.ProductID)
	if _, err := f.s.CreateProduct(f.ctx, first, claim); err != nil {
		t.Fatal(err)
	}

	retry := *first
	retry.ProductID = domain.NewCommercialID(domain.PrefixProduct)
	retryClaim := claim
	retryClaim.ResourceID = retry.ProductID
	_, err := f.s.CreateProduct(f.ctx, &retry, retryClaim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != first.ProductID {
		t.Fatalf("a retried CreateProduct must replay the first result, got %v", err)
	}

	reused := claim
	reused.RequestSHA256 = strings.Repeat("0", 64)
	if _, err := f.s.CreateProduct(f.ctx, &retry, reused); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
		t.Fatalf("the same key with a different request must be refused: %v", err)
	}

	// Keys are scoped per principal: bob's "key-1" is not alice's.
	other := &domain.Product{ProductID: domain.NewCommercialID(domain.PrefixProduct), ProductCode: "enterprise",
		ProductKind: domain.ProductKindPlan, CreatedByPrincipalID: checker}
	bobClaim := claim
	bobClaim.PrincipalID, bobClaim.ResourceID = checker, other.ProductID
	if _, err := f.s.CreateProduct(f.ctx, other, bobClaim); err != nil {
		t.Fatalf("another principal's identical key string collided: %v", err)
	}

	// Eight concurrent retries of one command: exactly one applies.
	concurrent := f.claim(maker, "CreateProduct", "")
	var wg sync.WaitGroup
	results := make([]error, 8)
	ids := make([]string, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := &domain.Product{ProductID: domain.NewCommercialID(domain.PrefixProduct), ProductCode: "racing",
				ProductKind: domain.ProductKindPlan, CreatedByPrincipalID: maker}
			c := concurrent
			c.ResourceID = p.ProductID
			ids[i] = p.ProductID
			_, results[i] = f.s.CreateProduct(f.ctx, p, c)
		}(i)
	}
	wg.Wait()
	var winners int
	var winner string
	for i, err := range results {
		if err == nil {
			winners++
			winner = ids[i]
		}
	}
	if winners != 1 {
		t.Fatalf("%d concurrent retries applied; exactly one must (errors: %v)", winners, results)
	}
	for _, err := range results {
		if err != nil && (!errors.As(err, &replay) || replay.ResourceID != winner) {
			t.Fatalf("a losing retry must replay the winner %s, got %v", winner, err)
		}
	}
	var n int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM commercial_products WHERE product_code = 'racing'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d products created by one idempotent command", n)
	}

	// A replayed transition replays; it does not report the state it caused.
	v := f.draft(first.ProductID, day(10), false, maker)
	v = f.terms(f.put(v, baseComponent("10.00")), false)
	submitClaim := f.claim(maker, "submit", v.PriceVersionID)
	if _, err := f.s.SubmitForApproval(f.ctx, v.PriceVersionID, v.RowVersion, maker, t0, submitClaim); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SubmitForApproval(f.ctx, v.PriceVersionID, v.RowVersion, maker, t0, submitClaim); !errors.As(err, &replay) {
		t.Fatalf("a retried submit must replay, not fail on the new state: %v", err)
	}
	if n := f.outboxCount(v.PriceVersionID, "price_version.submitted"); n != 1 {
		t.Fatalf("a replayed submit emitted %d events", n)
	}
}

func TestPriceBook_StaleETagIsRefused(t *testing.T) {
	f := newPB(t)
	f.currency("USD", 2, true)
	v := f.draft(f.product("business").ProductID, day(10), false, maker)
	stale := v.RowVersion
	v = f.put(v, baseComponent("10.00"))

	c := seatsComponent()
	c.CreatedAt, c.CreatedByPrincipalID = t0, maker
	if _, err := f.s.PutPriceComponent(f.ctx, v.PriceVersionID, stale, &c, f.claim(maker, "AddPriceComponent", v.PriceVersionID)); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("an edit against a stale ETag was applied: %v", err)
	}
	v = f.terms(v, false)
	v = f.ok(f.submit(v, maker, t0))
	if _, err := f.s.ApprovePriceVersion(f.ctx, v.PriceVersionID, v.RowVersion-1, checker, day(1),
		f.claim(checker, "approve", v.PriceVersionID)); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("an approval against a stale ETag was applied: %v", err)
	}
	if _, err := f.sellerExec(`UPDATE product_price_versions SET row_version = row_version WHERE price_version_id = $1`, v.PriceVersionID); pgCode(err) != "CP001" {
		t.Fatalf("an UPDATE that does not advance row_version was accepted: %v", err)
	}
}

// ── Seller plane isolation (COM-CTRL-001) ────────────────────────────────────

func TestPriceBook_RLS_EnabledAndForced(t *testing.T) {
	f := newPB(t)
	for _, table := range []string{"commercial_currencies", "commercial_products", "product_price_versions",
		"price_components", "price_component_tiers", "price_version_capabilities", "commercial_idempotency_keys"} {
		var enabled, forced bool
		if err := f.admin.QueryRow(f.ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).
			Scan(&enabled, &forced); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if !enabled || !forced {
			t.Errorf("%s: RLS enabled=%v forced=%v; both are required", table, enabled, forced)
		}
	}
}

func TestPriceBook_RLS_TenantPlaneSeesOnlyPublishedAndCannotWrite(t *testing.T) {
	f := newPB(t)
	f.currency("USD", 2, true)
	published := f.publishedVersion(f.product("business").ProductID, day(10), "49.00")
	secret := f.product("unannounced")
	draft := f.put(f.draft(secret.ProductID, day(60), false, maker), baseComponent("999.00"))

	if _, err := f.s.GetPriceVersion(f.ctx, draft.PriceVersionID, false); !errors.Is(err, domain.ErrPriceVersionNotFound) {
		t.Fatalf("a draft price reached a non-seller read: %v", err)
	}
	if _, err := f.s.GetPriceVersion(f.ctx, draft.PriceVersionID, true); err != nil {
		t.Fatalf("the seller plane must see its own drafts: %v", err)
	}
	if _, err := f.s.GetProduct(f.ctx, secret.ProductID, false); !errors.Is(err, domain.ErrProductNotFound) {
		t.Fatalf("an unannounced product's code reached a non-seller read: %v", err)
	}
	if _, err := f.s.ListPriceHistory(f.ctx, secret.ProductID, false); !errors.Is(err, domain.ErrProductNotFound) {
		t.Fatalf("an unannounced product's history reached a non-seller read: %v", err)
	}
	if got, err := f.s.GetPriceVersion(f.ctx, published.PriceVersionID, false); err != nil || len(got.Components) != 2 {
		t.Fatalf("published prices must be readable by everyone: %v", err)
	}

	var draftComponents int
	_ = f.app.QueryRow(f.ctx, `SELECT count(*) FROM price_components WHERE price_version_id = $1`, draft.PriceVersionID).Scan(&draftComponents)
	if draftComponents != 0 {
		t.Fatalf("a draft's components were visible without the seller plane: %d rows", draftComponents)
	}

	writes := map[string]string{
		"insert product": `INSERT INTO commercial_products (product_id, product_code, product_kind, created_by_principal_id)
			VALUES ('cprod_00000000-0000-4000-8000-000000000009', 'tenant_made', 'PLAN', 'tenant-user')`,
		"insert currency": `INSERT INTO commercial_currencies (currency_code, minor_units, sale_enabled, created_by_principal_id, updated_by_principal_id)
			VALUES ('XTS', 2, true, 'tenant-user', 'tenant-user')`,
		"forge seller claim": `INSERT INTO commercial_idempotency_keys (owner_scope, principal_id, idempotency_key, operation, request_sha256, resource_id)
			VALUES ('seller', 'tenant-user', 'k', 'CreateProduct', '` + strings.Repeat("a", 64) + `', 'x')`,
	}
	for name, sql := range writes {
		if _, err := f.tenantExec(orgA, sql); pgCode(err) != "42501" {
			t.Errorf("%s from a tenant-scoped transaction: got %v, want an RLS refusal (42501)", name, err)
		}
	}
	tag, err := f.tenantExec(orgA, `UPDATE product_price_versions SET row_version = row_version + 1 WHERE price_version_id = $1`, published.PriceVersionID)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("a tenant-scoped UPDATE reached a catalogue row: rows=%d err=%v", tag.RowsAffected(), err)
	}
}

// A commercial identifier is structurally distinct from a tenant-plane UUID,
// in the schema as well as at the API boundary (negative path #01).
func TestPriceBook_SchemaRefusesBareUUIDIdentifiers(t *testing.T) {
	f := newPB(t)
	_, err := f.sellerExec(`INSERT INTO commercial_products (product_id, product_code, product_kind, created_by_principal_id)
		VALUES ('3f2504e0-4f89-11d3-9a0c-0305e82c3301', 'bare', 'PLAN', 'alice')`)
	if pgCode(err) != "23514" {
		t.Fatalf("a bare UUID was accepted as a commercial product id: %v", err)
	}
}

// ── Draft discipline ─────────────────────────────────────────────────────────

func TestPriceBook_ContentFreezesAtSubmissionAndRejectReopensIt(t *testing.T) {
	f := newPB(t)
	f.currency("USD", 2, true)
	v := f.draft(f.product("business").ProductID, day(10), false, maker)
	v = f.terms(f.put(v, baseComponent("49.00")), false)
	v = f.ok(f.submit(v, maker, t0))
	firstHash := *v.ContentSHA256

	if _, err := f.s.RemovePriceComponent(f.ctx, v.PriceVersionID, v.RowVersion, "base", maker,
		f.claim(maker, "RemovePriceComponent", v.PriceVersionID)); !errors.Is(err, domain.ErrPriceVersionImmutable) {
		t.Fatalf("a component was removed from a version under review: %v", err)
	}
	if _, err := f.sellerExec(`INSERT INTO price_components (price_component_id, price_version_id, component_key, component_type,
		amount, billing_timing, created_by_principal_id)
		VALUES ('cpc_00000000-0000-4000-8000-000000000002', $1, 'late', 'RECURRING_FIXED', 1.00, 'IN_ADVANCE', 'alice')`,
		v.PriceVersionID); pgCode(err) != "CP001" {
		t.Fatalf("a component was added after submission, outside the approved hash: %v", err)
	}

	v, err := f.s.RejectPriceVersion(f.ctx, v.PriceVersionID, v.RowVersion, checker, "base price above approved band", day(1),
		f.claim(checker, "reject", v.PriceVersionID))
	if err != nil || v.Status != domain.PriceVersionDraft || v.ContentSHA256 != nil ||
		v.LastRejectionReason == nil || *v.LastRejectionReason != "base price above approved band" {
		t.Fatalf("reject: %+v, err=%v", v, err)
	}
	if n := f.outboxCount(v.PriceVersionID, "price_version.rejected"); n != 1 {
		t.Fatalf("rejection events: %d", n)
	}
	v = f.put(v, baseComponent("45.00"))
	v = f.ok(f.submit(v, maker, day(2)))
	if *v.ContentSHA256 == firstHash {
		t.Fatal("resubmitted content hashed the same as the rejected content")
	}
	if _, err := f.approve(v, checker, day(3)); err != nil {
		t.Fatalf("the corrected version must be approvable: %v", err)
	}
}

func TestPriceBook_SubmissionAndPublicationGates(t *testing.T) {
	f := newPB(t)
	f.currency("USD", 2, true)
	p := f.product("business")

	v := f.draft(p.ProductID, day(10), false, maker)
	v = f.put(v, baseComponent("49.00"))
	if _, err := f.submit(v, maker, t0); err == nil {
		t.Fatal("submitted without commercial terms")
	}
	v = f.put(v, domain.PriceComponent{ComponentKey: "api_calls", ComponentType: domain.ComponentMetered,
		MeterKey: strp("api.calls"), MeterVersion: intp(1), AggregationMethod: strp("SUM"), IncludedQuantity: strp("1000"),
		BillingTiming: strp("IN_ARREARS"), Amount: strp("0.0015")})
	v = f.terms(v, false)
	if _, err := f.submit(v, maker, t0); !errors.Is(err, domain.ErrMeterNotRegistered) {
		t.Fatalf("a metered price was submitted before any meter registry exists: %v", err)
	}
	v, err := f.s.RemovePriceComponent(f.ctx, v.PriceVersionID, v.RowVersion, "api_calls", maker,
		f.claim(maker, "RemovePriceComponent", v.PriceVersionID))
	if err != nil {
		t.Fatal(err)
	}
	if n := f.outboxCount(v.PriceVersionID, "price_version.meter_binding_changed"); n != 2 {
		t.Fatalf("meter binding events (added, removed): %d, want 2", n)
	}

	if _, err := f.submit(v, maker, day(11)); err == nil {
		t.Fatal("submitted a version whose effective_from had already passed")
	}

	f.currency("USD", 2, false)
	if _, err := f.submit(v, maker, t0); err == nil {
		t.Fatal("submitted a version priced in a currency not enabled for sale")
	}
	f.currency("USD", 2, true)

	v = f.ok(f.submit(v, maker, t0))
	v = f.ok(f.approve(v, checker, day(1)))
	var blocked *domain.PublicationBlockedError
	if _, err := f.publish(v, publisher, day(11)); !errors.As(err, &blocked) {
		t.Fatalf("published a price after its effective_from, making it retroactive: %v", err)
	}
	if _, err := f.publish(v, publisher, day(9)); err != nil {
		t.Fatalf("a timely publication must succeed: %v", err)
	}
}

func TestPriceBook_CurrencyScaleAndMinorUnits(t *testing.T) {
	f := newPB(t)
	f.currency("USD", 2, true)
	v := f.draft(f.product("business").ProductID, day(10), false, maker)

	c := baseComponent("10.005")
	c.CreatedAt, c.CreatedByPrincipalID = t0, maker
	var ve *domain.ValidationError
	if _, err := f.s.PutPriceComponent(f.ctx, v.PriceVersionID, v.RowVersion, &c, f.claim(maker, "AddPriceComponent", v.PriceVersionID)); !errors.As(err, &ve) {
		t.Fatalf("a 3-place USD fixed charge was accepted: %v", err)
	}
	v = f.put(v, domain.PriceComponent{ComponentKey: "calls", ComponentType: domain.ComponentPerUnit, UnitName: strp("call"),
		BillingTiming: strp("IN_ARREARS"), IncludedQuantity: strp("0"), QuantityRounding: strp("NONE"), Amount: strp("0.0015")})
	if *v.Components[0].Amount != "0.0015" {
		t.Fatalf("a four-place unit price did not round-trip exactly: %s", *v.Components[0].Amount)
	}

	// The schema refuses what it cannot store exactly rather than rounding it.
	if _, err := f.sellerExec(`INSERT INTO price_components (price_component_id, price_version_id, component_key, component_type,
		amount, billing_timing, created_by_principal_id)
		VALUES ('cpc_00000000-0000-4000-8000-000000000003', $1, 'tiny', 'RECURRING_FIXED', 0.00015, 'IN_ADVANCE', 'alice')`,
		v.PriceVersionID); pgCode(err) != "23514" {
		t.Fatalf("a 5-place amount was stored (and so silently rounded or kept) instead of refused: %v", err)
	}

	if _, err := f.s.UpsertCurrency(f.ctx, &domain.CommercialCurrency{CurrencyCode: "USD", MinorUnits: 3, SaleEnabled: true,
		UpdatedByPrincipalID: maker}); !errors.Is(err, store.ErrCurrencyMinorUnitsFixed) {
		t.Fatalf("minor units were changed under existing prices: %v", err)
	}
	if _, err := f.sellerExec(`UPDATE commercial_currencies SET minor_units = 3 WHERE currency_code = 'USD'`); pgCode(err) != "CP001" {
		t.Fatalf("raw minor-unit change was accepted: %v", err)
	}
}

func TestPriceBook_OneVersionInFlightPerProduct(t *testing.T) {
	f := newPB(t)
	f.currency("USD", 2, true)
	p := f.product("business")
	f.draft(p.ProductID, day(10), false, maker)
	v := &domain.PriceVersion{PriceVersionID: domain.NewCommercialID(domain.PrefixPriceVersion), ProductID: p.ProductID,
		DisplayName: "Rival", BillingInterval: "MONTH", BillingIntervalCount: 1, CurrencyCode: "USD", MarketCodes: []string{"US"},
		EffectiveFrom: day(10), ChangeReason: "competing draft", CreatedAt: t0, CreatedByPrincipalID: checker}
	if _, err := f.s.CreateDraftVersion(f.ctx, v, false, f.claim(checker, "CreateDraftVersion", v.PriceVersionID)); !errors.Is(err, domain.ErrInFlightVersionExists) {
		t.Fatalf("a second in-flight version was opened for one product: %v", err)
	}
}
