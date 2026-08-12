package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/vendor-due-diligence-svc/internal/domain"
	svcmiddleware "zoiko.io/vendor-due-diligence-svc/internal/middleware"
	"zoiko.io/vendor-due-diligence-svc/internal/store"
)

// This service shipped with no store tests, which is why the conclusion/evidence
// split survived: the handler stub is a map and cannot fail one write while
// letting the other succeed, so no test could observe a conclusion outliving its
// evidence. The claims that only a real database can settle are here — the
// transaction boundary, the STARTED guard, the 22P02 mapping, and the CHECK
// constraint from 000002.
//
// WARNING: this suite DROPs its tables. Point TEST_DATABASE_URL at the
// vendor_due_diligence database a running service uses and it deletes that data,
// which afterwards looks like a service bug rather than a test. The name is
// checked by requireThrowawayDatabase before anything is dropped.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	requireThrowawayDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	migrations := filepath.Join(filepath.Dir(filename), "../../deployments/migrations")

	// evidence first: it has an FK onto checks.
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS vendor_dd_evidence CASCADE;`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS vendor_dd_checks CASCADE;`)

	// Both migrations, in order. Applying only 000001 would leave
	// screening_source and the outcome CHECK constraint absent and quietly pass
	// the tests that exist to prove them.
	for _, name := range []string{
		"000001_initial_schema.up.sql",
		"000002_screening_source.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join(migrations, name))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", name, err)
		}
	}

	return pool
}

// requireThrowawayDatabase fails the test unless the DSN's database name marks
// it as disposable. Two names are legitimate: CI points TEST_DATABASE_URL at
// `testdb`, and the local convention is `vendor_due_diligence_test`. Both
// contain "test"; the live `vendor_due_diligence` database does not, which is
// the case this guard exists to catch.
//
// The name is taken from the parsed DSN rather than matched against the whole
// string, so a host or password that happens to contain "test" cannot vouch for
// a live database. Anything that does not parse as a URL is refused rather than
// waved through — the suite DROPs tables, so an unreadable target is not a
// target worth guessing at.
func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably "+
			"disposable, and this suite DROPs its tables. Use vendor_due_diligence_test (or CI's "+
			"testdb), not vendor_due_diligence.", dbName)
	}
}

const (
	testTenant = "tenant-store-test"
	testEntity = "22222222-2222-2222-2222-222222222222"
	testCP     = "33333333-3333-3333-3333-333333333333"
)

func tenantCtx(tenantID string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), tenantID)
}

func newCheck(correlationID, vendorName string) *domain.VendorDDCheck {
	return &domain.VendorDDCheck{
		CheckID:                uuid.NewString(),
		TenantID:               testTenant,
		LegalEntityID:          testEntity,
		CounterpartyID:         testCP,
		VendorName:             vendorName,
		Status:                 domain.StatusStarted,
		CorrelationID:          correlationID,
		InitiatedByPrincipalID: "principal-store-test",
		StartedAt:              time.Now().UTC(),
	}
}

func evidenceFor(checkID, description, docRef string) *domain.VendorDDEvidence {
	return &domain.VendorDDEvidence{
		EvidenceID:        uuid.NewString(),
		CheckID:           checkID,
		TenantID:          testTenant,
		EvidenceType:      domain.EvidenceTypeSanctionsScreening,
		Description:       description,
		DocumentReference: docRef,
		RecordedAt:        time.Now().UTC(),
	}
}

// ── the transaction boundary ──────────────────────────────────────────────────

func TestConcludeCheck_WritesOutcomeAndEvidenceTogether(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx(testTenant)

	c := newCheck("corr-conclude", "Clean Vendor Ltd")
	if created, err := s.CreateCheck(ctx, c); err != nil || !created {
		t.Fatalf("CreateCheck: created=%v err=%v", created, err)
	}

	ev := evidenceFor(c.CheckID, "no match against stub sanctions denylist", "")
	if err := s.ConcludeCheck(ctx, c.CheckID, domain.RiskClear,
		"no match against stub sanctions denylist", domain.ScreeningSourceStubDenylist, ev); err != nil {
		t.Fatalf("ConcludeCheck: %v", err)
	}

	got, err := s.GetCheck(ctx, c.CheckID)
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if got.Status != domain.StatusCompleted || got.RiskOutcome != domain.RiskClear {
		t.Errorf("expected COMPLETED/CLEAR got %q/%q", got.Status, got.RiskOutcome)
	}
	if got.ScreeningSource != domain.ScreeningSourceStubDenylist {
		t.Errorf("expected screening_source to persist, got %q", got.ScreeningSource)
	}
	if got.CompletedAt == nil {
		t.Error("expected completed_at to be set")
	}

	list, err := s.ListEvidence(ctx, c.CheckID)
	if err != nil {
		t.Fatalf("ListEvidence: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 evidence row got %d — the conclusion must not outlive its evidence", len(list))
	}
}

// The failure the split version could produce: a bad evidence row must take the
// conclusion down with it, leaving the check STARTED rather than COMPLETED with no
// evidence. An evidence_id that is not a UUID fails the insert inside the same
// transaction as the UPDATE.
func TestConcludeCheck_EvidenceFailureRollsBackTheConclusion(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx(testTenant)

	c := newCheck("corr-rollback", "Rollback Vendor Ltd")
	if _, err := s.CreateCheck(ctx, c); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}

	bad := evidenceFor(c.CheckID, "screened", "")
	bad.EvidenceID = "not-a-uuid"

	if err := s.ConcludeCheck(ctx, c.CheckID, domain.RiskClear, "screened",
		domain.ScreeningSourceStubDenylist, bad); err == nil {
		t.Fatal("expected ConcludeCheck to fail on an unusable evidence row")
	}

	got, err := s.GetCheck(ctx, c.CheckID)
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if got.Status != domain.StatusStarted {
		t.Errorf("a failed evidence write must roll back the conclusion, got status=%q", got.Status)
	}
	if got.RiskOutcome != "" {
		t.Errorf("expected no risk outcome after rollback, got %q", got.RiskOutcome)
	}
	list, _ := s.ListEvidence(ctx, c.CheckID)
	if len(list) != 0 {
		t.Errorf("expected no evidence rows, got %d", len(list))
	}
}

// ── the STARTED guard ─────────────────────────────────────────────────────────

func TestConcludeCheck_SecondConclusionRefused(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx(testTenant)

	c := newCheck("corr-twice", "Restricted Trading Corp")
	if _, err := s.CreateCheck(ctx, c); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if err := s.ConcludeCheck(ctx, c.CheckID, domain.RiskFlagged, "matched",
		domain.ScreeningSourceStubDenylist, evidenceFor(c.CheckID, "matched", "")); err != nil {
		t.Fatalf("first ConcludeCheck: %v", err)
	}

	// The unguarded UPDATE permitted this, which meant FLAGGED could be
	// overwritten with CLEAR.
	err := s.ConcludeCheck(ctx, c.CheckID, domain.RiskClear, "no match",
		domain.ScreeningSourceStubDenylist, evidenceFor(c.CheckID, "no match", ""))
	if !errors.Is(err, domain.ErrCheckAlreadyConcluded) {
		t.Fatalf("expected ErrCheckAlreadyConcluded, got %v", err)
	}

	got, _ := s.GetCheck(ctx, c.CheckID)
	if got.RiskOutcome != domain.RiskFlagged {
		t.Errorf("FLAGGED was overwritten with %q", got.RiskOutcome)
	}
	if list, _ := s.ListEvidence(ctx, c.CheckID); len(list) != 1 {
		t.Errorf("the refused conclusion must not add evidence, got %d rows", len(list))
	}
}

// Two conclusions racing on one check: exactly one wins, and the other is told so
// rather than silently overwriting.
func TestConcludeCheck_ConcurrentConclusionsExactlyOneWins(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx(testTenant)

	c := newCheck("corr-race", "Raced Vendor Ltd")
	if _, err := s.CreateCheck(ctx, c); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}

	const attempts = 8
	var wg sync.WaitGroup
	results := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.ConcludeCheck(ctx, c.CheckID, domain.RiskClear, "no match",
				domain.ScreeningSourceStubDenylist, evidenceFor(c.CheckID, "no match", ""))
		}(i)
	}
	wg.Wait()

	var won, alreadyConcluded, other int
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, domain.ErrCheckAlreadyConcluded):
			alreadyConcluded++
		default:
			other++
			t.Logf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Errorf("expected exactly 1 successful conclusion, got %d (already-concluded=%d other=%d)",
			won, alreadyConcluded, other)
	}
	if list, _ := s.ListEvidence(ctx, c.CheckID); len(list) != 1 {
		t.Errorf("expected exactly 1 evidence row for 1 conclusion, got %d", len(list))
	}
}

func TestMarkFailed_OnlyFromStarted(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx(testTenant)

	c := newCheck("corr-fail", "Failing Vendor Ltd")
	if _, err := s.CreateCheck(ctx, c); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if err := s.MarkFailed(ctx, c.CheckID, "could not record the outcome"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	got, _ := s.GetCheck(ctx, c.CheckID)
	if got.Status != domain.StatusFailed {
		t.Errorf("expected FAILED got %q", got.Status)
	}
	if got.CompletedAt == nil {
		t.Error("a FAILED check must carry completed_at — it is terminal")
	}
	if got.RiskOutcome != "" {
		t.Errorf("a FAILED check must carry no risk outcome, got %q", got.RiskOutcome)
	}

	// A concluded check must not be retrospectively marked failed.
	if err := s.MarkFailed(ctx, c.CheckID, "again"); !errors.Is(err, domain.ErrCheckNotFound) {
		t.Errorf("expected ErrCheckNotFound for a non-STARTED check, got %v", err)
	}
}

// A FAILED check cannot then be concluded: the run is over.
func TestConcludeCheck_AfterFailedIsRefused(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx(testTenant)

	c := newCheck("corr-fail-then-conclude", "Zombie Vendor Ltd")
	if _, err := s.CreateCheck(ctx, c); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if err := s.MarkFailed(ctx, c.CheckID, "lost"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	err := s.ConcludeCheck(ctx, c.CheckID, domain.RiskClear, "no match",
		domain.ScreeningSourceStubDenylist, evidenceFor(c.CheckID, "no match", ""))
	if !errors.Is(err, domain.ErrCheckAlreadyConcluded) {
		t.Fatalf("expected ErrCheckAlreadyConcluded, got %v", err)
	}
}

// ── idempotency ───────────────────────────────────────────────────────────────

func TestCreateCheck_ReplayReturnsTheStoredConclusion(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx(testTenant)

	first := newCheck("corr-idem", "Idempotent Vendor Ltd")
	if created, err := s.CreateCheck(ctx, first); err != nil || !created {
		t.Fatalf("first CreateCheck: created=%v err=%v", created, err)
	}
	if err := s.ConcludeCheck(ctx, first.CheckID, domain.RiskFlagged, "matched",
		domain.ScreeningSourceStubDenylist, evidenceFor(first.CheckID, "matched", "vault-1")); err != nil {
		t.Fatalf("ConcludeCheck: %v", err)
	}

	// A retry: a fresh struct with a new candidate id and the same correlation id.
	retry := newCheck("corr-idem", "Idempotent Vendor Ltd")
	created, err := s.CreateCheck(ctx, retry)
	if err != nil {
		t.Fatalf("retry CreateCheck: %v", err)
	}
	if created {
		t.Fatal("a retry must not create a second check")
	}
	if retry.CheckID != first.CheckID {
		t.Errorf("retry resolved to %s, expected the original %s", retry.CheckID, first.CheckID)
	}
	// The struct must carry the STORED state, not the STARTED values it arrived
	// with — otherwise the response describes a check that does not exist.
	if retry.Status != domain.StatusCompleted || retry.RiskOutcome != domain.RiskFlagged {
		t.Errorf("retry must carry the stored conclusion, got status=%q outcome=%q",
			retry.Status, retry.RiskOutcome)
	}
	if retry.ScreeningSource != domain.ScreeningSourceStubDenylist {
		t.Errorf("retry must carry the stored screening source, got %q", retry.ScreeningSource)
	}
	if retry.CompletedAt == nil {
		t.Error("retry must carry the stored completed_at")
	}
}

// ── tenant isolation and identifier handling ──────────────────────────────────

func TestGetCheck_OtherTenantReadsAsAbsent(t *testing.T) {
	s := store.New(openTestPool(t))

	c := newCheck("corr-tenant", "Private Vendor Ltd")
	if _, err := s.CreateCheck(tenantCtx(testTenant), c); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}

	if _, err := s.GetCheck(tenantCtx("tenant-someone-else"), c.CheckID); !errors.Is(err, domain.ErrCheckNotFound) {
		t.Fatalf("another tenant's check must read as absent, got %v", err)
	}
}

func TestStoreMethods_RefuseWithoutTenantScope(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := context.Background() // no tenant in context

	if _, err := s.CreateCheck(ctx, newCheck("corr-no-tenant", "X")); !errors.Is(err, domain.ErrTenantMissing) {
		t.Errorf("CreateCheck: expected ErrTenantMissing got %v", err)
	}
	if _, err := s.GetCheck(ctx, uuid.NewString()); !errors.Is(err, domain.ErrTenantMissing) {
		t.Errorf("GetCheck: expected ErrTenantMissing got %v", err)
	}
	if _, err := s.ListChecks(ctx, store.ListFilter{Limit: 10}); !errors.Is(err, domain.ErrTenantMissing) {
		t.Errorf("ListChecks: expected ErrTenantMissing got %v", err)
	}
	if _, err := s.ListEvidence(ctx, uuid.NewString()); !errors.Is(err, domain.ErrTenantMissing) {
		t.Errorf("ListEvidence: expected ErrTenantMissing got %v", err)
	}
	if err := s.MarkFailed(ctx, uuid.NewString(), "x"); !errors.Is(err, domain.ErrTenantMissing) {
		t.Errorf("MarkFailed: expected ErrTenantMissing got %v", err)
	}
}

// check_id is a uuid column, so a non-UUID path param died inside the pgx driver
// as SQLSTATE 22P02 and surfaced as 503 — a typo in a URL reading as an outage.
func TestGetCheck_NonUUIDReadsAsAbsentNotOutage(t *testing.T) {
	s := store.New(openTestPool(t))

	_, err := s.GetCheck(tenantCtx(testTenant), "definitely-not-a-uuid")
	if !errors.Is(err, domain.ErrCheckNotFound) {
		t.Fatalf("expected ErrCheckNotFound for a non-UUID id, got %v", err)
	}
}

// The list filters are NOT uuid columns — legal_entity_id and counterparty_id are
// VARCHAR(255) in 000001, unlike check_id. So a malformed filter is a valid
// comparison that matches nothing, and it must not be reported as an error: a 400
// there would claim a validation this schema does not perform. Asserted because
// the opposite was assumed while writing this, and the handler carried a dead
// `invalid_filter` branch as a result.
func TestListChecks_NonUUIDFilterMatchesNothingWithoutFailing(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx(testTenant)

	if _, err := s.CreateCheck(ctx, newCheck("corr-varchar-filter", "Filtered Vendor Ltd")); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}

	out, err := s.ListChecks(ctx, store.ListFilter{LegalEntityID: "not-a-uuid", Limit: 10})
	if err != nil {
		t.Fatalf("a non-UUID filter against a VARCHAR column must not error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected no matches, got %d", len(out))
	}
}

// ── pagination ────────────────────────────────────────────────────────────────

func TestListChecks_PaginationIsStableAcrossPages(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx(testTenant)

	// Identical timestamps on purpose: the stub screening is fast enough that
	// checks share a started_at, and ordering by it alone lets one page repeat a
	// row and skip another.
	shared := time.Now().UTC()
	const total = 7
	for i := 0; i < total; i++ {
		c := newCheck(uuid.NewString(), "Paged Vendor Ltd")
		c.StartedAt = shared
		if _, err := s.CreateCheck(ctx, c); err != nil {
			t.Fatalf("CreateCheck %d: %v", i, err)
		}
	}

	seen := map[string]bool{}
	for offset := 0; offset < total; offset += 3 {
		page, err := s.ListChecks(ctx, store.ListFilter{Limit: 3, Offset: offset})
		if err != nil {
			t.Fatalf("ListChecks offset=%d: %v", offset, err)
		}
		for _, c := range page {
			if seen[c.CheckID] {
				t.Errorf("check %s appeared on more than one page", c.CheckID)
			}
			seen[c.CheckID] = true
		}
	}
	if len(seen) != total {
		t.Errorf("paging returned %d distinct checks, expected %d", len(seen), total)
	}
}

func TestListChecks_LimitIsHonoured(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx(testTenant)

	for i := 0; i < 5; i++ {
		if _, err := s.CreateCheck(ctx, newCheck(uuid.NewString(), "Limited Vendor Ltd")); err != nil {
			t.Fatalf("CreateCheck: %v", err)
		}
	}
	page, err := s.ListChecks(ctx, store.ListFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListChecks: %v", err)
	}
	if len(page) != 2 {
		t.Errorf("expected 2 rows, got %d", len(page))
	}
}

// ── document_reference ────────────────────────────────────────────────────────

// The column shipped in 000001 with no write path, so every row held "" and
// "no document" was indistinguishable from "a document with a blank reference".
func TestConcludeCheck_DocumentReferenceIsNullWhenAbsent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenant)

	withDoc := newCheck("corr-with-doc", "Documented Vendor Ltd")
	if _, err := s.CreateCheck(ctx, withDoc); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if err := s.ConcludeCheck(ctx, withDoc.CheckID, domain.RiskClear, "no match",
		domain.ScreeningSourceStubDenylist, evidenceFor(withDoc.CheckID, "no match", "vault-doc-1")); err != nil {
		t.Fatalf("ConcludeCheck: %v", err)
	}

	without := newCheck("corr-without-doc", "Undocumented Vendor Ltd")
	if _, err := s.CreateCheck(ctx, without); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if err := s.ConcludeCheck(ctx, without.CheckID, domain.RiskClear, "no match",
		domain.ScreeningSourceStubDenylist, evidenceFor(without.CheckID, "no match", "")); err != nil {
		t.Fatalf("ConcludeCheck: %v", err)
	}

	var nulls int
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM vendor_dd_evidence
		WHERE check_id = $1 AND document_reference IS NULL
	`, without.CheckID).Scan(&nulls); err != nil {
		t.Fatalf("count: %v", err)
	}
	if nulls != 1 {
		t.Errorf("an absent document reference must be stored as NULL, not \"\": got %d NULL rows", nulls)
	}

	list, err := s.ListEvidence(ctx, withDoc.CheckID)
	if err != nil || len(list) != 1 || list[0].DocumentReference != "vault-doc-1" {
		t.Errorf("a supplied document reference must round-trip: %+v (err=%v)", list, err)
	}
}

// ── the 000002 CHECK constraint ───────────────────────────────────────────────

// The constraint is what makes a partially-applied conclusion unrepresentable —
// the state a swallowed evidence failure used to leave behind.
func TestOutcomeConstraint_RejectsAnOutcomeWithoutAConclusion(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	cases := []struct {
		name        string
		status      string
		riskOutcome any
		completedAt any
	}{
		{"STARTED with a risk outcome", domain.StatusStarted, domain.RiskClear, nil},
		{"STARTED with completed_at", domain.StatusStarted, nil, time.Now().UTC()},
		{"COMPLETED without a risk outcome", domain.StatusCompleted, nil, time.Now().UTC()},
		{"COMPLETED without completed_at", domain.StatusCompleted, domain.RiskClear, nil},
		{"FAILED with a risk outcome", domain.StatusFailed, domain.RiskClear, time.Now().UTC()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `
				INSERT INTO vendor_dd_checks (
					check_id, tenant_id, legal_entity_id, counterparty_id, vendor_name,
					status, risk_outcome, correlation_id, initiated_by_principal_id,
					started_at, completed_at
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			`, uuid.NewString(), testTenant, testEntity, testCP, "Constraint Vendor Ltd",
				tc.status, tc.riskOutcome, uuid.NewString(), "principal-store-test",
				time.Now().UTC(), tc.completedAt)
			if err == nil {
				t.Errorf("expected the outcome constraint to reject %s", tc.name)
			}
		})
	}
}
