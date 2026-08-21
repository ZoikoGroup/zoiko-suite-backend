package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/board-resolutions-svc/internal/domain"
	"zoiko.io/board-resolutions-svc/internal/middleware"
	"zoiko.io/board-resolutions-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migrations from a
// clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set — same
// convention as every other service in this platform.
//
// Both migrations are applied, in order: 000002 is what makes row-level
// security actually apply to this connection (see its own comment), so a suite
// that applied only 000001 would be testing a schema no deployment runs.
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
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS board_resolutions, board_meetings CASCADE;`)

	for _, name := range []string{
		"000001_initial_schema.up.sql",
		"000002_force_rls_and_constraints.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations", name))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", name, err)
		}
	}

	return pool
}

// scoped runs a verification query with app.tenant_id installed, exactly as the
// store installs it for every statement it makes.
//
// The assertions here deliberately query the tables directly rather than
// through the store: a write must not be verified by the code that performed
// it. But a raw query carries no tenant scope, and once the row-level security
// policy actually applies -- which it does the moment the service connects as
// something other than a superuser -- an unscoped statement matches NO rows.
// A SELECT then reports "this row was never written" about a row that was, and
// an UPDATE reports no error while changing nothing at all, which is worse: the
// test proceeds against state it believes it arranged.
//
// Transaction-local, so no stale tenant is left on the pooled connection to
// silently scope a later query to the wrong tenant.
func scoped(t *testing.T, pool *pgxpool.Pool, tenantID string, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("verification tx begin failed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		t.Fatalf("installing the verification tenant scope failed: %v", err)
	}
	fn(tx)
}

// scopedWrite is scoped's committing sibling, for a scoped statement that is
// test SETUP rather than verification.
//
// Keeping them separate rather than adding a bool: a rollback where a commit was
// meant leaves the test running against state it believes it arranged, and the
// failure surfaces as a wrong assertion about the SERVICE rather than about the
// harness. That is precisely what happened here before this existed.
func scopedWrite(t *testing.T, pool *pgxpool.Pool, tenantID string, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("setup tx begin failed: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		t.Fatalf("installing the setup tenant scope failed: %v", err)
	}
	fn(tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("setup tx commit failed: %v", err)
	}
	committed = true
}

// requireThrowawayDatabase refuses to run against anything not recognisably
// disposable. This suite DROPs board_meetings and board_resolutions, and those
// rows are the record of what a board decided and who put it into force. Only
// the database NAME vouches for it; a password that happens to contain "test"
// must not.
func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably "+
			"disposable, and this suite DROPs board_meetings and board_resolutions. Use "+
			"board_resolutions_test (or CI's testdb), not board_resolutions.", dbName)
	}
}

func tenantCtx(tenantID string) context.Context {
	return middleware.WithTenant(context.Background(), tenantID)
}

func newResolution(legalEntityID, createdBy string) *domain.BoardResolution {
	return &domain.BoardResolution{
		LegalEntityID:    legalEntityID,
		ResolutionNumber: "RES-2026-001",
		Title:            "Approve Annual Budget",
		Content:          "Resolved that the 2026 budget be approved.",
		Category:         domain.ResolutionCategoryFinancial,
		EffectiveFrom:    "2026-01-01",
		CreatedBy:        createdBy,
	}
}

func newMeeting(legalEntityID, createdBy string) *domain.BoardMeeting {
	return &domain.BoardMeeting{
		LegalEntityID: legalEntityID,
		Title:         "Q1 Board Meeting",
		ScheduledAt:   time.Now().UTC().Add(24 * time.Hour),
		Location:      "Boardroom",
		EffectiveFrom: "2026-01-01",
		CreatedBy:     createdBy,
	}
}

// The headline defect: every read carried no tenant predicate and relied on a
// row-level security policy that did not apply, because the service connects
// as the table owner and the tables were not FORCE ROW LEVEL SECURITY. One
// tenant could read another's board resolutions by id.
func TestPgStore_GetResolution_IsTenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	r := newResolution("le-us", "drafter-1")
	if err := s.CreateResolution(tenantCtx("tenant-a"), r); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetResolution(tenantCtx("tenant-a"), r.ResolutionID)
	if err != nil {
		t.Fatalf("own-tenant read: %v", err)
	}
	if got.Title != r.Title {
		t.Fatalf("own-tenant read returned %q, want %q", got.Title, r.Title)
	}

	_, err = s.GetResolution(tenantCtx("tenant-b"), r.ResolutionID)
	if !errors.Is(err, domain.ErrResolutionNotFound) {
		t.Fatalf("cross-tenant read returned %v, want ErrResolutionNotFound", err)
	}
}

func TestPgStore_GetMeeting_IsTenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	m := newMeeting("le-us", "secretary-1")
	if err := s.CreateMeeting(tenantCtx("tenant-a"), m); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.GetMeeting(tenantCtx("tenant-b"), m.MeetingID); !errors.Is(err, domain.ErrMeetingNotFound) {
		t.Fatalf("cross-tenant read returned %v, want ErrMeetingNotFound", err)
	}
}

func TestPgStore_ListsAreTenantScopedAndPaged(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	for i := 0; i < 3; i++ {
		r := newResolution("le-us", "drafter-1")
		r.ResolutionNumber = "RES-" + strconv.Itoa(i)
		if err := s.CreateResolution(tenantCtx("tenant-a"), r); err != nil {
			t.Fatalf("seed tenant-a: %v", err)
		}
	}
	if err := s.CreateResolution(tenantCtx("tenant-b"), newResolution("le-us", "drafter-9")); err != nil {
		t.Fatalf("seed tenant-b: %v", err)
	}

	all, err := s.ListResolutions(tenantCtx("tenant-a"), domain.ResolutionFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("tenant-a sees %d resolutions, want exactly its own 3", len(all))
	}
	for _, r := range all {
		if r.TenantID != "tenant-a" {
			t.Fatalf("tenant-a's list contained a %s row", r.TenantID)
		}
	}

	page, err := s.ListResolutions(tenantCtx("tenant-a"), domain.ResolutionFilter{Limit: 2})
	if err != nil {
		t.Fatalf("paged list: %v", err)
	}
	rest, err := s.ListResolutions(tenantCtx("tenant-a"), domain.ResolutionFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("offset list: %v", err)
	}
	if len(page) != 2 || len(rest) != 1 {
		t.Fatalf("paging returned %d then %d rows, want 2 then 1", len(page), len(rest))
	}
	for _, a := range page {
		for _, b := range rest {
			if a.ResolutionID == b.ResolutionID {
				t.Fatalf("resolution %s appeared on both pages", a.ResolutionID)
			}
		}
	}
}

// A resolution may cite a meeting, but only one of this tenant's meetings.
// meeting_id used to be written verbatim with no check at all.
func TestPgStore_CreateResolution_RejectsAnotherTenantsMeeting(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	m := newMeeting("le-us", "secretary-1")
	if err := s.CreateMeeting(tenantCtx("tenant-a"), m); err != nil {
		t.Fatalf("create meeting: %v", err)
	}

	r := newResolution("le-uk", "drafter-9")
	r.MeetingID = m.MeetingID
	if err := s.CreateResolution(tenantCtx("tenant-b"), r); !errors.Is(err, domain.ErrMeetingNotFound) {
		t.Fatalf("filing a resolution against another tenant's meeting returned %v, want ErrMeetingNotFound", err)
	}

	// The tenant that owns the meeting may of course cite it.
	own := newResolution("le-us", "drafter-1")
	own.MeetingID = m.MeetingID
	if err := s.CreateResolution(tenantCtx("tenant-a"), own); err != nil {
		t.Fatalf("own-tenant meeting reference rejected: %v", err)
	}
}

// The closing action's finalized check listed only PASSED and RESCINDED, so a
// resolution the board had REJECTED could still be passed into force.
func TestPgStore_PassResolution_RejectedCannotBePassed(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	r := newResolution("le-us", "drafter-1")
	if err := s.CreateResolution(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	// scopedWrite, not scoped: this statement is test SETUP the assertion below
	// depends on, so it has to commit. The read-only helper rolls back, which
	// left the resolution PENDING and made PassResolution correct to allow it —
	// the test then reported a service defect that did not exist. Unscoped it
	// was worse still: the UPDATE matched no rows and returned no error at all.
	// Hence the row count is asserted rather than assumed.
	scopedWrite(t, pool, "tenant-a", func(tx pgx.Tx) {
		tag, err := tx.Exec(context.Background(),
			`UPDATE board_resolutions SET status='REJECTED' WHERE resolution_id=$1`, r.ResolutionID)
		if err != nil {
			t.Fatalf("set REJECTED: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("set REJECTED changed %d rows, want 1", tag.RowsAffected())
		}
	})

	_, err := s.PassResolution(ctx, r.ResolutionID, "chairperson-1", &domain.PassResolutionRequest{})
	if !errors.Is(err, domain.ErrResolutionAlreadyFinalized) {
		t.Fatalf("passing a REJECTED resolution returned %v, want ErrResolutionAlreadyFinalized", err)
	}
}

// Passing twice must be refused the second time. The status check and the
// write used to sit in two separate transactions.
func TestPgStore_PassResolution_IsIdempotentlyRefusedOnceFinal(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	r := newResolution("le-us", "drafter-1")
	if err := s.CreateResolution(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}

	passed, err := s.PassResolution(ctx, r.ResolutionID, "chairperson-1", &domain.PassResolutionRequest{})
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if passed.Status != domain.ResolutionStatusPassed {
		t.Fatalf("status is %s, want PASSED", passed.Status)
	}
	if passed.PassedBy == nil || *passed.PassedBy != "chairperson-1" {
		t.Fatalf("passed_by is %v, want the principal the handler established", passed.PassedBy)
	}

	if _, err := s.PassResolution(ctx, r.ResolutionID, "chairperson-2", &domain.PassResolutionRequest{}); !errors.Is(err, domain.ErrResolutionAlreadyFinalized) {
		t.Fatalf("second pass returned %v, want ErrResolutionAlreadyFinalized", err)
	}
}

// Segregation of duties is re-checked against the locked row, because the
// handler's own check runs against a read that is stale by the time the write
// happens.
func TestPgStore_PassResolution_SelfApprovalIsRefusedAtTheWrite(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	r := newResolution("le-us", "drafter-1")
	if err := s.CreateResolution(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.PassResolution(ctx, r.ResolutionID, "drafter-1", &domain.PassResolutionRequest{}); !errors.Is(err, domain.ErrSelfApprovalNotAllowed) {
		t.Fatalf("self-pass returned %v, want ErrSelfApprovalNotAllowed", err)
	}
}

// A finalized resolution cannot be re-tallied.
func TestPgStore_RecordVotes_RefusedOnceFinal(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	r := newResolution("le-us", "drafter-1")
	if err := s.CreateResolution(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.RecordVotes(ctx, r.ResolutionID, &domain.RecordVotesRequest{VotesFor: 5, VotesAgainst: 1}); err != nil {
		t.Fatalf("tally: %v", err)
	}
	if _, err := s.PassResolution(ctx, r.ResolutionID, "chairperson-1", &domain.PassResolutionRequest{}); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if _, err := s.RecordVotes(ctx, r.ResolutionID, &domain.RecordVotesRequest{VotesFor: 99}); !errors.Is(err, domain.ErrResolutionAlreadyFinalized) {
		t.Fatalf("re-tallying a PASSED resolution returned %v, want ErrResolutionAlreadyFinalized", err)
	}
}

// A write must not be able to conclude another tenant's resolution.
func TestPgStore_Transitions_AreTenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	r := newResolution("le-us", "drafter-1")
	if err := s.CreateResolution(tenantCtx("tenant-a"), r); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.RecordVotes(tenantCtx("tenant-b"), r.ResolutionID, &domain.RecordVotesRequest{VotesFor: 1}); !errors.Is(err, domain.ErrResolutionNotFound) {
		t.Fatalf("cross-tenant vote returned %v, want ErrResolutionNotFound", err)
	}
	if _, err := s.PassResolution(tenantCtx("tenant-b"), r.ResolutionID, "chairperson-1", &domain.PassResolutionRequest{}); !errors.Is(err, domain.ErrResolutionNotFound) {
		t.Fatalf("cross-tenant pass returned %v, want ErrResolutionNotFound", err)
	}
}

// The tenant id arrives on a request header. It used to be interpolated into
// the SQL that installs the RLS context: `SET LOCAL app.tenant_id = '<header>'`.
func TestPgStore_TenantIDIsNotInterpolatedIntoSQL(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	// Seed a row that must survive.
	if err := s.CreateResolution(tenantCtx("tenant-a"), newResolution("le-us", "drafter-1")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	injected := "x'; DROP TABLE board_resolutions; --"
	// The read is expected to find nothing (no such tenant); what matters is
	// that the statement does not execute and the table is still there.
	if _, err := s.ListResolutions(tenantCtx(injected), domain.ResolutionFilter{Limit: 10}); err != nil {
		t.Fatalf("injected tenant produced an error rather than an empty result: %v", err)
	}

	rows, err := s.ListResolutions(tenantCtx("tenant-a"), domain.ResolutionFilter{Limit: 10})
	if err != nil {
		t.Fatalf("the table did not survive an injected tenant id: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the seeded resolution to still be there, found %d", len(rows))
	}
}

// A store call with no tenant must be refused, not run with an empty scope
// that reads as "this tenant has nothing".
func TestPgStore_WithoutTenant_IsRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := context.Background()

	if _, err := s.ListResolutions(ctx, domain.ResolutionFilter{Limit: 10}); !errors.Is(err, domain.ErrTenantMissing) {
		t.Fatalf("list without tenant returned %v, want ErrTenantMissing", err)
	}
	if _, err := s.GetMeeting(ctx, "mtg-1"); !errors.Is(err, domain.ErrTenantMissing) {
		t.Fatalf("get without tenant returned %v, want ErrTenantMissing", err)
	}
	if err := s.CreateResolution(ctx, newResolution("le-us", "drafter-1")); !errors.Is(err, domain.ErrTenantMissing) {
		t.Fatalf("create without tenant returned %v, want ErrTenantMissing", err)
	}
}

// A malformed effective_from dies inside the driver on the DATE column and
// used to answer 500 — an outage status for a bad date in a form.
func TestPgStore_MalformedDate_IsAFieldErrorNotAnOutage(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	r := newResolution("le-us", "drafter-1")
	r.EffectiveFrom = "next tuesday"
	if err := s.CreateResolution(tenantCtx("tenant-a"), r); !errors.Is(err, domain.ErrInvalidField) {
		t.Fatalf("malformed date returned %v, want ErrInvalidField", err)
	}
}

// Negative vote counts were accepted and stored.
func TestPgStore_NegativeVotesAreRefusedByTheSchema(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	r := newResolution("le-us", "drafter-1")
	if err := s.CreateResolution(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.RecordVotes(ctx, r.ResolutionID, &domain.RecordVotesRequest{VotesAgainst: -5}); err == nil {
		t.Fatal("the schema accepted a negative vote count")
	}
}
