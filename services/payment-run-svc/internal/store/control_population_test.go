package store

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payment-run-svc/internal/domain"
	"zoiko.io/payment-run-svc/internal/middleware"
)

// FIN-CTRL-030 population: needs a migrated database (TEST_DATABASE_URL), like the other store tests.

// entityOf reads the legal entity of a run (the helper that created it generated one).
func entityOf(t *testing.T, s *PgStore, tenant, runID string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	var entity string
	if err := tx.QueryRow(ctx, `SELECT legal_entity_id::text FROM payment_runs WHERE run_id = $1::uuid`, runID).Scan(&entity); err != nil {
		t.Fatal(err)
	}
	return entity
}

func dbDate(t *testing.T, s *PgStore, offsetDays int) string {
	t.Helper()
	var d string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT to_char((now() AT TIME ZONE 'UTC')::date + $1::int, 'YYYY-MM-DD')`, offsetDays).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestUnappliedSupplierPayments_OnlySettledUnappliedPayablesBeforeTheCutOffAreInScope(t *testing.T) {
	s, pool := openStore(t)
	tenant := uuid.NewString()
	runID, instructionID := settledInstruction(t, s, tenant)
	entity := entityOf(t, s, tenant, runID)
	ctx := context.Background()

	q := func(tenantID, entityID, before string) *domain.ControlPopulationPage {
		t.Helper()
		pg, err := s.QueryUnappliedSupplierPayments(ctx, tenantID, domain.UnappliedPaymentsQuery{LegalEntityID: entityID, UnappliedBefore: before, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return pg
	}

	pg := q(tenant, entity, dbDate(t, s, 1))
	if len(pg.Records) != 1 || pg.DeclaredTotals.RowCount != 1 {
		t.Fatalf("settled, unapplied payable must be in scope: %+v", pg)
	}
	r := pg.Records[0]
	if r.Amount != "90.00" || r.Currency != "USD" || r.Date != dbDate(t, s, 0) || pg.DeclaredTotals.Totals["USD"] != "90.00" {
		t.Fatalf("record %+v totals %+v", r, pg.DeclaredTotals.Totals)
	}
	if r.Attributes["instruction_id"] != instructionID || r.Attributes["gross_amount"] != "100.00" ||
		r.Attributes["withholding_amount"] != "10.00" || r.Attributes["age_days"] != "1" || r.Attributes["settled_at_source"] != "EVENT" {
		t.Fatalf("attributes %+v", r.Attributes)
	}
	if pg.Watermark == "" {
		t.Fatal("watermark is required")
	}

	if got := q(tenant, entity, dbDate(t, s, 0)); len(got.Records) != 0 {
		t.Fatalf("the cut-off is strict: settled today is not before today: %+v", got.Records)
	}
	if got := q(uuid.NewString(), entity, dbDate(t, s, 1)); len(got.Records) != 0 {
		t.Fatalf("another tenant must see nothing: %+v", got.Records)
	}
	if got := q(tenant, uuid.NewString(), dbDate(t, s, 1)); len(got.Records) != 0 {
		t.Fatalf("another entity must see nothing: %+v", got.Records)
	}

	// Once AP-08 has accepted the settlement application the payable is no longer unapplied.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if tag, err := tx.Exec(ctx, `UPDATE run_instruction_payables SET payable_applied_at = now() WHERE instruction_id = $1::uuid`, instructionID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("mark applied: rows=%d err=%v", tag.RowsAffected(), err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after := q(tenant, entity, dbDate(t, s, 1))
	if len(after.Records) != 0 || after.Watermark == pg.Watermark {
		t.Fatalf("an applied payable leaves the population and changes the watermark: %+v", after)
	}
}

func TestUnappliedSupplierPayments_UnsettledInstructionsAreNotInScope(t *testing.T) {
	s, _ := openStore(t)
	tenant := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenant)
	entity := uuid.NewString()
	run, _, err := s.CreateRun(ctx, tenant, domain.CreateRunRequest{
		LegalEntityID: entity, PayingBankAccountRef: "acct", Currency: "USD", ValueDate: time.Now(), PaymentMethod: "ACH",
	}, []domain.RunInstruction{{
		AuthorizationID: "auth-" + uuid.NewString(), AuthorizationFingerprint: "fp", PayeeRef: "payee", NetAmount: 50, Currency: "USD",
		Payables: []domain.InstructionPayable{{PayableSource: "AP_INVOICE", SourceReference: "inv-" + uuid.NewString(), GrossAmount: 50, NetAmount: 50}},
	}}, "p1")
	if err != nil || run == nil {
		t.Fatalf("CreateRun: %v", err)
	}
	pg, err := s.QueryUnappliedSupplierPayments(context.Background(), tenant, domain.UnappliedPaymentsQuery{
		LegalEntityID: entity, UnappliedBefore: dbDate(t, s, 1), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(pg.Records) != 0 || pg.DeclaredTotals.RowCount != 0 {
		t.Fatalf("a PENDING instruction has not moved cash: %+v", pg.Records)
	}
}

func TestUnappliedSupplierPayments_KeysetPagingIsCompleteWithOneWatermark(t *testing.T) {
	s, _ := openStore(t)
	tenant := uuid.NewString()
	ctx := middleware.WithTenant(context.Background(), tenant)
	entity := uuid.NewString()
	var instructions []domain.RunInstruction
	for i := 0; i < 5; i++ {
		instructions = append(instructions, domain.RunInstruction{
			AuthorizationID: "auth-" + uuid.NewString(), AuthorizationFingerprint: "fp", PayeeRef: "payee-" + uuid.NewString(),
			NetAmount: 10, Currency: "USD",
			Payables: []domain.InstructionPayable{{PayableSource: "AP_INVOICE", SourceReference: "inv-" + uuid.NewString(), GrossAmount: 10, NetAmount: 10}},
		})
	}
	_, created, err := s.CreateRun(ctx, tenant, domain.CreateRunRequest{
		LegalEntityID: entity, PayingBankAccountRef: "acct", Currency: "USD", ValueDate: time.Now(), PaymentMethod: "ACH",
	}, instructions, "p1")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for _, ins := range created {
		if _, applied, err := s.ReconcileInstruction(ctx, domain.ReconcileInstructionRequest{
			InstructionID: ins.InstructionID, ExternalStatus: domain.InstructionSettled, ProviderEventRef: "bnk07:" + ins.InstructionID + ":SETTLED",
		}, "p1"); err != nil || !applied {
			t.Fatalf("settle: applied=%v err=%v", applied, err)
		}
	}

	before := dbDate(t, s, 1)
	for _, limit := range []int{1, 2, 5, 100} {
		var ids []string
		watermarks := map[string]bool{}
		after := ""
		for guard := 0; ; guard++ {
			if guard > 20 {
				t.Fatal("paging did not terminate")
			}
			pg, err := s.QueryUnappliedSupplierPayments(context.Background(), tenant, domain.UnappliedPaymentsQuery{
				LegalEntityID: entity, UnappliedBefore: before, Limit: limit, AfterRecordID: after})
			if err != nil {
				t.Fatal(err)
			}
			watermarks[pg.Watermark] = true
			if pg.DeclaredTotals.RowCount != 5 || pg.DeclaredTotals.Totals["USD"] != "50.00" {
				t.Fatalf("declared totals cover the whole population, not the page: %+v", pg.DeclaredTotals)
			}
			for _, r := range pg.Records {
				ids = append(ids, r.RecordID)
			}
			if pg.NextRecordID == "" {
				break
			}
			after = pg.NextRecordID
		}
		if len(ids) != 5 || len(watermarks) != 1 {
			t.Fatalf("limit=%d: got %d records, %d watermarks", limit, len(ids), len(watermarks))
		}
		if !sort.StringsAreSorted(ids) {
			t.Fatalf("limit=%d: records must be ordered by record_id: %v", limit, ids)
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("limit=%d: repeated record %s", limit, id)
			}
			seen[id] = true
		}
	}
}
