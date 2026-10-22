package store_test

// SPEC_DEVIATIONS A4 against a real Postgres: both sides of a pair, its
// period and dimensions survive a round trip exactly, and migration 000005
// backfills existing pairs with the behaviour they were created under.
// Skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

func TestPgStore_A4_CrossCurrencyPairRoundTripsExactly(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	period := "2026-09"
	entry := newTestEntry(tenantID, uuid.New().String())
	entry.Amount, entry.CurrencyCode = "99999999999999.9999", "USD"
	entry.CounterpartyAmount, entry.CounterpartyCurrencyCode = "0.0001", "GBP"
	entry.FiscalPeriod = &period
	entry.Dimensions = domain.Dimensions{"cost_centre": "CC-100", "project": "P-7"}
	if _, err := s.CreateEntry(ctx, entry, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}

	got, err := s.GetEntry(ctx, entry.IntercompanyEntryID)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got.Amount != "99999999999999.9999" || got.CounterpartyAmount != "0.0001" {
		t.Fatalf("amounts did not survive exactly: %q / %q", got.Amount, got.CounterpartyAmount)
	}
	if got.CurrencyCode != "USD" || got.CounterpartyCurrencyCode != "GBP" {
		t.Fatalf("currencies: %q / %q", got.CurrencyCode, got.CounterpartyCurrencyCode)
	}
	if got.FiscalPeriod == nil || *got.FiscalPeriod != period {
		t.Fatalf("fiscal_period: %v", got.FiscalPeriod)
	}
	if len(got.Dimensions) != 2 || got.Dimensions["project"] != "P-7" {
		t.Fatalf("dimensions: %v", got.Dimensions)
	}

	// The idempotent replay reads the original row back through the same
	// column list, so it carries every new field too.
	replay := newTestEntry(tenantID, entry.SourceJournalID)
	created, err := s.CreateEntry(ctx, replay, "actor-1", "corr-2")
	if err != nil || created {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}
	if replay.CounterpartyCurrencyCode != "GBP" || replay.Dimensions["cost_centre"] != "CC-100" {
		t.Fatalf("replay did not return the original pair: %+v", replay)
	}

	// The event carries both sides, the period and the dimensions.
	var payload map[string]any
	if err := pool.QueryRow(ctx, `
		SELECT convert_from(payload, 'UTF8')::jsonb -> 'data' FROM eventing_outbox
		WHERE tenant_id = $1 AND aggregate_id = $2`, tenantID, entry.IntercompanyEntryID).Scan(&payload); err != nil {
		t.Fatalf("read created event: %v", err)
	}
	if payload["counterparty_currency_code"] != "GBP" || payload["fiscal_period"] != period || payload["dimensions"] == nil {
		t.Fatalf("created event payload missing A4 fields: %v", payload)
	}
}

// A pair without a period or dimensions (every pair created before 000005,
// and callers such as treasury-svc that do not send them yet) is still valid.
func TestPgStore_A4_PeriodAndDimensionsOptional(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	entry.Dimensions = nil
	if _, err := s.CreateEntry(ctx, entry, "actor-1", "corr-1"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	got, err := s.GetEntry(ctx, entry.IntercompanyEntryID)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got.FiscalPeriod != nil || got.Dimensions == nil || len(got.Dimensions) != 0 {
		t.Fatalf("expected no period and empty dimensions, got %v / %v", got.FiscalPeriod, got.Dimensions)
	}
}

// Migration 000005 on rows written under the old one-sided schema: each
// pair's counterparty side is its source side (what the old match compared
// the target journal against), and no period is invented.
func TestMigration000005_BackfillsCounterpartyFromSourceSide(t *testing.T) {
	pool := openEmptyPool(t)
	applyMigrations(t, pool, migrations[:4]...)

	ctx := context.Background()
	tenantID := uuid.New().String()
	id := uuid.New().String()
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `
		INSERT INTO intercompany_entries (intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
			source_journal_id, amount, currency_code, match_status, created_at, updated_at)
		VALUES ($1, $2, 'le-us', 'le-uk', $3, 1234.5678, 'EUR', 'MATCHED', $4, $4)`,
		id, tenantID, uuid.New().String(), now); err != nil {
		t.Fatalf("seed pre-000005 row: %v", err)
	}

	applyMigrations(t, pool, migrations[4:]...)

	got, err := store.New(pool).GetEntry(svcmiddleware.WithTenant(ctx, tenantID), id)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got.CounterpartyAmount != "1234.5678" || got.CounterpartyCurrencyCode != "EUR" {
		t.Fatalf("expected counterparty 1234.5678 EUR, got %q %q", got.CounterpartyAmount, got.CounterpartyCurrencyCode)
	}
	if got.FiscalPeriod != nil {
		t.Fatalf("a period was invented for a pair that never had one: %q", *got.FiscalPeriod)
	}
	if got.Dimensions == nil || len(got.Dimensions) != 0 {
		t.Fatalf("expected empty dimensions, got %v", got.Dimensions)
	}
}

// The constraints added by 000005 are real.
func TestMigration000005_Constraints(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	insert := func(cpAmount string, dims string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO intercompany_entries (intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
				source_journal_id, amount, currency_code, counterparty_amount, counterparty_currency_code, dimensions,
				match_status, created_at, updated_at)
			VALUES ($1, 't', 'a', 'b', $2, 1, 'USD', $3::numeric, 'USD', $4::jsonb, 'UNMATCHED', now(), now())`,
			uuid.New().String(), uuid.New().String(), cpAmount, dims)
		return err
	}
	if err := insert("1", `{}`); err != nil {
		t.Fatalf("valid row refused: %v", err)
	}
	if err := insert("0", `{}`); err == nil {
		t.Error("a zero counterparty amount was accepted")
	}
	if err := insert("1", `[]`); err == nil {
		t.Error("non-object dimensions were accepted")
	}
}
