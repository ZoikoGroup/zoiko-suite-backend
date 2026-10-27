//go:build integration

package store_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

// Control populations of treasury-svc (FIN-CTRL-026 and FIN-CTRL-044) against a real Postgres.

func recordRate(t *testing.T, ctx context.Context, tenant, pair string, rate float64, at time.Time) {
	t.Helper()
	if _, err := testStore.RecordFXRate(ctx, domain.RecordFXRateParams{
		TenantID: tenant, CurrencyPair: pair, Rate: rate, EffectiveAt: at, RecordedByPrincipalID: "fx-admin-1"}); err != nil {
		t.Fatalf("record %s: %v", pair, err)
	}
}

func TestControlPopulation_FXRateObservations_MonthScopeBoundariesAndPaging(t *testing.T) {
	cleanTables(t)
	tenant := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)

	recordRate(t, ctx, tenant, "EUR/USD", 1.08, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) // first instant of the month
	recordRate(t, ctx, tenant, "EUR/USD", 1.09, time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC))
	recordRate(t, ctx, tenant, "GBP/USD", 1.25, time.Date(2026, 9, 30, 23, 30, 0, 0, time.UTC)) // last day, late
	recordRate(t, ctx, tenant, "EUR/USD", 1.10, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))   // next month: out
	recordRate(t, ctx, tenant, "EUR/USD", 1.07, time.Date(2026, 8, 31, 23, 59, 0, 0, time.UTC)) // previous month: out
	other := uuid.NewString()
	recordRate(t, svcmiddleware.WithTenant(context.Background(), other), other, "EUR/USD", 9.99, time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))

	page, err := testStore.QueryFXRateObservations(ctx, tenant, domain.FXRateObservationsQuery{PeriodID: "2026-09", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 3 || page.DeclaredTotals.RowCount != 3 {
		t.Fatalf("only the three September observations of this tenant are in scope: %+v", page.Records)
	}
	dates := map[string]bool{}
	for _, r := range page.Records {
		dates[r.Date+"/"+r.Attributes["currency_pair"]] = true
		if r.Currency != "XXX" || r.Reference != r.Attributes["currency_pair"] || r.Attributes["recorded_by"] != "fx-admin-1" {
			t.Fatalf("record %+v", r)
		}
	}
	for _, want := range []string{"2026-09-01/EUR/USD", "2026-09-02/EUR/USD", "2026-09-30/GBP/USD"} {
		if !dates[want] {
			t.Fatalf("missing %s in %v", want, dates)
		}
	}
	if page.DeclaredTotals.Totals["XXX"] != "3.42000000" || !strings.HasPrefix(page.Watermark, "tr1:n=3;md5=") {
		t.Fatalf("totals %+v watermark %q", page.DeclaredTotals, page.Watermark)
	}

	// another tenant sees only its own observation, and an empty month is an empty population
	foreign, err := testStore.QueryFXRateObservations(ctx, other, domain.FXRateObservationsQuery{PeriodID: "2026-09", Limit: 10})
	if err != nil || len(foreign.Records) != 1 || foreign.Records[0].Amount != "9.99000000" {
		t.Fatalf("tenant isolation: %v %+v", err, foreign)
	}
	if empty, err := testStore.QueryFXRateObservations(ctx, tenant, domain.FXRateObservationsQuery{PeriodID: "2026-07", Limit: 10}); err != nil || len(empty.Records) != 0 {
		t.Fatalf("empty month: %v %+v", err, empty)
	}
	if _, err := testStore.QueryFXRateObservations(ctx, tenant, domain.FXRateObservationsQuery{PeriodID: "2026-9", Limit: 10}); err == nil {
		t.Fatal("a malformed period is refused, never defaulted")
	}

	// keyset paging is complete, ordered, repeat-free and under one watermark
	for _, limit := range []int{1, 2, 100} {
		var ids []string
		marks := map[string]bool{}
		after := ""
		for guard := 0; ; guard++ {
			if guard > 10 {
				t.Fatal("paging did not terminate")
			}
			p, err := testStore.QueryFXRateObservations(ctx, tenant, domain.FXRateObservationsQuery{PeriodID: "2026-09", Limit: limit, AfterRecordID: after})
			if err != nil {
				t.Fatal(err)
			}
			marks[p.Watermark] = true
			for _, r := range p.Records {
				ids = append(ids, r.RecordID)
			}
			if p.NextRecordID == "" {
				break
			}
			after = p.NextRecordID
		}
		if len(ids) != 3 || len(marks) != 1 || !sort.StringsAreSorted(ids) {
			t.Fatalf("limit=%d: ids %v, %d watermarks", limit, ids, len(marks))
		}
	}
}

func TestControlPopulation_BankAccountOwnership_GapsAndPrivacy(t *testing.T) {
	cleanTables(t)
	tenant := uuid.NewString()
	entity := "le-own-1"
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)

	create := func(status, creator string) string {
		a := newTestAccount(tenant, entity)
		a.AccountStatus = status
		a.CreatedByPrincipalID = creator
		a.CorrelationID = uuid.NewString()
		if _, err := testStore.CreateBankAccount(ctx, a); err != nil {
			t.Fatalf("create %s: %v", status, err)
		}
		return a.BankAccountID
	}
	verify := func(id, by string) {
		if _, err := testStore.VerifyBankAccountOwnership(ctx, domain.VerifyOwnershipParams{
			BankAccountID: id, TenantID: tenant, VerificationMethod: "MICRO_DEPOSIT", EvidenceRef: "ev-" + by, VerifiedByPrincipalID: by}); err != nil {
			t.Fatalf("verify: %v", err)
		}
	}

	unverified := create("ACTIVE", "maker-1") // no evidence at all
	good := create("ACTIVE", "maker-1")       // independently verified
	verify(good, "checker-1")
	self := create("ACTIVE", "maker-1") // verified by whoever created it
	verify(self, "maker-1")
	pending := create("PENDING_VERIFICATION", "maker-1") // not in use yet: no gap
	reverified := create("ACTIVE", "maker-1")            // first evidence by the creator, superseded by an independent one
	verify(reverified, "maker-1")
	verify(reverified, "checker-2")
	// accounts of another entity and another tenant are never in scope
	foreign := newTestAccount(tenant, "le-other")
	foreign.CorrelationID = uuid.NewString()
	if _, err := testStore.CreateBankAccount(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	page, err := testStore.QueryBankAccountOwnership(ctx, tenant, domain.BankAccountOwnershipQuery{LegalEntityID: entity, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 5 || page.DeclaredTotals.RowCount != 5 {
		t.Fatalf("every status of this entity's accounts is reported (5), got %d", len(page.Records))
	}
	gap := map[string]string{}
	for _, r := range page.Records {
		gap[r.RecordID] = r.Attributes["ownership_gap"]
		if r.Currency != "XXX" || r.Amount != "0" || r.Reference != r.RecordID {
			t.Fatalf("record %+v", r)
		}
		// strict privacy: nothing identifying the account itself leaves the service
		for k, v := range r.Attributes {
			if strings.Contains(v, "1234") || strings.Contains(v, "SWIFT") || strings.Contains(v, "Corporate") {
				t.Fatalf("attribute %s=%q leaks account detail", k, v)
			}
		}
	}
	want := map[string]string{unverified: "OWNERSHIP_UNVERIFIED", good: "", self: "SELF_VERIFIED", pending: "", reverified: ""}
	for id, w := range want {
		if gap[id] != w {
			t.Errorf("account %s: gap %q, want %q", id, gap[id], w)
		}
	}
	for _, r := range page.Records {
		if r.RecordID == good && (r.Attributes["ownership_verified"] != "true" || r.Attributes["verified_by"] != "checker-1") {
			t.Fatalf("good %+v", r)
		}
		if r.RecordID == reverified && r.Attributes["verified_by"] != "checker-2" {
			t.Fatalf("the CURRENT evidence counts, not the superseded one: %+v", r)
		}
	}

	if other, err := testStore.QueryBankAccountOwnership(ctx, uuid.NewString(), domain.BankAccountOwnershipQuery{LegalEntityID: entity, Limit: 10}); err != nil || len(other.Records) != 0 {
		t.Fatalf("another tenant must see nothing: %v %+v", err, other)
	}
	// paging
	var ids []string
	marks := map[string]bool{}
	after := ""
	for guard := 0; ; guard++ {
		if guard > 10 {
			t.Fatal("paging did not terminate")
		}
		p, err := testStore.QueryBankAccountOwnership(ctx, tenant, domain.BankAccountOwnershipQuery{LegalEntityID: entity, Limit: 2, AfterRecordID: after})
		if err != nil {
			t.Fatal(err)
		}
		marks[p.Watermark] = true
		for _, r := range p.Records {
			ids = append(ids, r.RecordID)
		}
		if p.NextRecordID == "" {
			break
		}
		after = p.NextRecordID
	}
	if len(ids) != 5 || len(marks) != 1 || !sort.StringsAreSorted(ids) {
		t.Fatalf("ids %v, %d watermarks", ids, len(marks))
	}
}
