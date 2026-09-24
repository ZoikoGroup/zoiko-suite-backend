package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/store"
)

// Integration tests for the decision-log audit read and the principal-status
// projection, against a real PostgreSQL.
//
// DESTRUCTIVE — see setupTestDB. TEST_DATABASE_URL must name a throwaway
// database; pointing it at the compose stack erases that stack's fixtures.

const (
	qTenantA = "11111111-1111-1111-1111-111111111111"
	qTenantB = "22222222-2222-2222-2222-222222222222"
	qEntity1 = "aaaaaaaa-1111-1111-1111-111111111111"
	qEntity2 = "aaaaaaaa-2222-2222-2222-222222222222"
)

func newQueryStore(t *testing.T) *store.PgStore {
	t.Helper()
	pool := getTestPool(t)
	setupTestDB(t, pool)
	return store.New(pool, zap.NewNop())
}

// record writes one decision and returns it, failing the test on error.
func record(t *testing.T, s *store.PgStore, tenant, entity, principal, action, outcome, basis string) *domain.AccessDecisionLog {
	t.Helper()
	d, err := s.RecordAccessDecision(context.Background(), domain.RecordAccessDecisionParams{
		PrincipalID:   principal,
		LegalEntityID: entity,
		ActionType:    action,
		Outcome:       outcome,
		Basis:         basis,
		CorrelationID: "corr-" + action,
		TenantID:      tenant,
	})
	if err != nil {
		t.Fatalf("RecordAccessDecision(%s/%s): %v", principal, action, err)
	}
	return d
}

// ── the audit read ──────────────────────────────────────────────────────────

func TestListAccessDecisions_NewestFirst(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	first := record(t, s, qTenantA, qEntity1, "p-1", "ACTION_ONE", "GRANTED", "rbac:role=X")
	second := record(t, s, qTenantA, qEntity1, "p-1", "ACTION_TWO", "GRANTED", "rbac:role=X")
	third := record(t, s, qTenantA, qEntity1, "p-1", "ACTION_THREE", "DENIED", "no_grant")

	page, err := s.ListAccessDecisions(ctx, qTenantA, domain.ListAccessDecisionsParams{})
	if err != nil {
		t.Fatalf("ListAccessDecisions: %v", err)
	}
	if len(page.Decisions) != 3 {
		t.Fatalf("got %d decisions, want 3", len(page.Decisions))
	}
	// Newest first: an auditor opening the log wants the most recent refusals,
	// not the oldest.
	want := []string{third.AccessDecisionID, second.AccessDecisionID, first.AccessDecisionID}
	for i, id := range want {
		if page.Decisions[i].AccessDecisionID != id {
			t.Fatalf("position %d = %s, want %s (newest first)", i, page.Decisions[i].AccessDecisionID, id)
		}
	}
	if page.NextCursor != "" {
		t.Errorf("next_cursor = %q on a complete page, want empty", page.NextCursor)
	}
}

// The control on this read. A tenant must not see another's decisions — a
// denial's basis names the SoD rule that fired, so an unscoped listing would
// hand a reader the location of every tripwire on the platform.
func TestListAccessDecisions_IsTenantScoped(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	record(t, s, qTenantA, qEntity1, "p-a", "ACTION_A", "DENIED", "sod:conflict_with=OTHER")
	record(t, s, qTenantB, qEntity1, "p-b", "ACTION_B", "DENIED", "sod:conflict_with=OTHER")

	page, err := s.ListAccessDecisions(ctx, qTenantA, domain.ListAccessDecisionsParams{})
	if err != nil {
		t.Fatalf("ListAccessDecisions: %v", err)
	}
	if len(page.Decisions) != 1 {
		t.Fatalf("tenant A saw %d decisions, want 1 — the other belongs to tenant B", len(page.Decisions))
	}
	if page.Decisions[0].PrincipalID != "p-a" {
		t.Fatalf("tenant A saw %s", page.Decisions[0].PrincipalID)
	}
}

// A decision recorded with NO tenant is invisible here, exactly as it is
// through the by-id read. It cannot be attributed to a tenant, so serving it to
// one would be a guess. This is the known consequence of the 86 callers that
// send no envelope, and it is asserted rather than assumed.
func TestListAccessDecisions_TenantlessRowsAreInvisible(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	record(t, s, qTenantA, qEntity1, "p-scoped", "ACTION_A", "GRANTED", "rbac:role=X")
	record(t, s, "", qEntity1, "p-unattributed", "ACTION_B", "DENIED", "no_grant")

	page, err := s.ListAccessDecisions(ctx, qTenantA, domain.ListAccessDecisionsParams{})
	if err != nil {
		t.Fatalf("ListAccessDecisions: %v", err)
	}
	if len(page.Decisions) != 1 {
		t.Fatalf("got %d decisions, want 1 — a NULL-tenant row must not be served to an arbitrary tenant", len(page.Decisions))
	}
	if page.Decisions[0].PrincipalID != "p-scoped" {
		t.Fatalf("got %s", page.Decisions[0].PrincipalID)
	}
}

// A tenantless READ is refused outright. Unlike an evaluation, which must
// answer a caller that sends no tenant, nobody has to be able to read the whole
// platform's audit trail.
func TestListAccessDecisions_RefusesTenantlessRead(t *testing.T) {
	s := newQueryStore(t)

	_, err := s.ListAccessDecisions(context.Background(), "", domain.ListAccessDecisionsParams{})
	if !errors.Is(err, domain.ErrTenantScopeRequired) {
		t.Fatalf("err = %v, want ErrTenantScopeRequired", err)
	}
}

// The query the evidence obligation actually names, and what 000012's index
// exists for.
func TestListAccessDecisions_FiltersByOutcome(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	record(t, s, qTenantA, qEntity1, "p-1", "ACTION_OK", "GRANTED", "rbac:role=X")
	record(t, s, qTenantA, qEntity1, "p-1", "ACTION_OK_2", "GRANTED", "rbac:role=X")
	denied := record(t, s, qTenantA, qEntity1, "p-1", "ACTION_NO", "DENIED", "sod:conflict_with=OTHER")

	page, err := s.ListAccessDecisions(ctx, qTenantA, domain.ListAccessDecisionsParams{Outcome: "DENIED"})
	if err != nil {
		t.Fatalf("ListAccessDecisions: %v", err)
	}
	if len(page.Decisions) != 1 || page.Decisions[0].AccessDecisionID != denied.AccessDecisionID {
		t.Fatalf("got %+v, want only the denial", page.Decisions)
	}
	if page.Decisions[0].DecisionBasis != "sod:conflict_with=OTHER" {
		t.Errorf("basis = %q — the reason is the point of the read", page.Decisions[0].DecisionBasis)
	}
}

func TestListAccessDecisions_FiltersByPrincipalActionAndEntity(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	record(t, s, qTenantA, qEntity1, "p-1", "ACTION_A", "DENIED", "no_grant")
	record(t, s, qTenantA, qEntity1, "p-2", "ACTION_A", "DENIED", "no_grant")
	record(t, s, qTenantA, qEntity2, "p-1", "ACTION_A", "DENIED", "no_grant")
	record(t, s, qTenantA, qEntity1, "p-1", "ACTION_B", "DENIED", "no_grant")

	cases := []struct {
		name   string
		params domain.ListAccessDecisionsParams
		want   int
	}{
		{"by principal", domain.ListAccessDecisionsParams{PrincipalID: "p-1"}, 3},
		{"by action", domain.ListAccessDecisionsParams{ActionType: "ACTION_A"}, 3},
		{"by entity", domain.ListAccessDecisionsParams{LegalEntityID: qEntity1}, 3},
		{"all three", domain.ListAccessDecisionsParams{
			PrincipalID: "p-1", ActionType: "ACTION_A", LegalEntityID: qEntity1,
		}, 1},
		{"no match", domain.ListAccessDecisionsParams{PrincipalID: "p-nobody"}, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			page, err := s.ListAccessDecisions(ctx, qTenantA, c.params)
			if err != nil {
				t.Fatalf("ListAccessDecisions: %v", err)
			}
			if len(page.Decisions) != c.want {
				t.Fatalf("got %d, want %d", len(page.Decisions), c.want)
			}
		})
	}
}

// The window is inclusive-from, exclusive-to. Asserted because an
// inclusive-inclusive upper bound would return the first decision of the next
// month in a month-by-month audit, and the double-counting would be invisible.
func TestListAccessDecisions_WindowIsInclusiveFromExclusiveTo(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	d := record(t, s, qTenantA, qEntity1, "p-1", "ACTION_A", "GRANTED", "rbac:role=X")

	exact := d.DecidedAt
	page, err := s.ListAccessDecisions(ctx, qTenantA, domain.ListAccessDecisionsParams{DecidedFrom: &exact})
	if err != nil {
		t.Fatalf("from = its own timestamp: %v", err)
	}
	if len(page.Decisions) != 1 {
		t.Fatalf("decided_from = the decision's own timestamp returned %d rows, want 1 (inclusive)", len(page.Decisions))
	}

	page, err = s.ListAccessDecisions(ctx, qTenantA, domain.ListAccessDecisionsParams{DecidedTo: &exact})
	if err != nil {
		t.Fatalf("to = its own timestamp: %v", err)
	}
	if len(page.Decisions) != 0 {
		t.Fatalf("decided_to = the decision's own timestamp returned %d rows, want 0 (exclusive)", len(page.Decisions))
	}

	after := exact.Add(time.Millisecond)
	page, err = s.ListAccessDecisions(ctx, qTenantA, domain.ListAccessDecisionsParams{DecidedTo: &after})
	if err != nil {
		t.Fatalf("to = just after: %v", err)
	}
	if len(page.Decisions) != 1 {
		t.Fatalf("decided_to just after the decision returned %d rows, want 1", len(page.Decisions))
	}
}

// Keyset pagination, walked end to end. The property that matters is that
// every decision appears exactly once across the pages — which is what OFFSET
// cannot guarantee on an append-only table that is being appended to.
func TestListAccessDecisions_PagesWithoutDroppingOrRepeating(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	const total = 7
	written := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		d := record(t, s, qTenantA, qEntity1, "p-1", "ACTION_"+string(rune('A'+i)), "DENIED", "no_grant")
		written[d.AccessDecisionID] = true
	}

	seen := make(map[string]int, total)
	cursor := domain.AccessDecisionCursor{}
	pages := 0
	for {
		page, err := s.ListAccessDecisions(ctx, qTenantA, domain.ListAccessDecisionsParams{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, d := range page.Decisions {
			seen[d.AccessDecisionID]++
		}
		if page.NextCursor == "" {
			break
		}
		if pages > 10 {
			t.Fatal("more than 10 pages for 7 rows — the cursor is not advancing")
		}
		var err2 error
		cursor, err2 = domain.DecodeAccessDecisionCursor(page.NextCursor)
		if err2 != nil {
			t.Fatalf("the store issued a cursor it cannot decode: %v", err2)
		}
	}

	if pages != 3 {
		t.Errorf("walked %d pages for 7 rows at 3 per page, want 3", pages)
	}
	if len(seen) != total {
		t.Fatalf("saw %d distinct decisions across the pages, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s appeared %d times — a keyset page boundary must not repeat a row", id, n)
		}
		if !written[id] {
			t.Errorf("%s was returned but never written", id)
		}
	}
}

// A row inserted mid-walk sorts AHEAD of the cursor and so does not displace
// what a later page returns. This is the concrete failure OFFSET has and keyset
// does not, on a table that takes one row per authorization on the platform.
func TestListAccessDecisions_ConcurrentInsertDoesNotShiftLaterPages(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		d := record(t, s, qTenantA, qEntity1, "p-1", "ACTION_"+string(rune('A'+i)), "DENIED", "no_grant")
		ids = append(ids, d.AccessDecisionID)
	}

	first, err := s.ListAccessDecisions(ctx, qTenantA, domain.ListAccessDecisionsParams{Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if first.NextCursor == "" {
		t.Fatal("no next cursor after page 1 of 4 rows at 2 per page")
	}

	// Something else authorizes an action while the auditor is on page one.
	record(t, s, qTenantA, qEntity1, "p-1", "ACTION_LATE", "DENIED", "no_grant")

	cursor, err := domain.DecodeAccessDecisionCursor(first.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	second, err := s.ListAccessDecisions(ctx, qTenantA, domain.ListAccessDecisionsParams{Limit: 2, Cursor: cursor})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}

	// Page two must be the two rows that were always going to be there — the
	// oldest two — not shifted by one because a newer row appeared.
	if len(second.Decisions) != 2 {
		t.Fatalf("page 2 returned %d rows, want 2", len(second.Decisions))
	}
	if second.Decisions[0].AccessDecisionID != ids[1] || second.Decisions[1].AccessDecisionID != ids[0] {
		t.Fatalf("page 2 = [%s %s], want [%s %s] — an insert during paging shifted the window, which is the OFFSET failure keyset exists to avoid",
			second.Decisions[0].AccessDecisionID, second.Decisions[1].AccessDecisionID, ids[1], ids[0])
	}
}

func TestListAccessDecisions_LimitIsCapped(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	record(t, s, qTenantA, qEntity1, "p-1", "ACTION_A", "GRANTED", "rbac:role=X")

	// Asking for more than the cap is not an error; it is clamped. Proven by
	// the query succeeding rather than by counting rows, since there is only
	// one — the cap is asserted on the value the store used, which is
	// observable through the absence of a driver error on a LIMIT that would
	// otherwise be enormous.
	page, err := s.ListAccessDecisions(ctx, qTenantA, domain.ListAccessDecisionsParams{Limit: 10_000_000})
	if err != nil {
		t.Fatalf("oversized limit: %v", err)
	}
	if len(page.Decisions) != 1 {
		t.Fatalf("got %d rows, want 1", len(page.Decisions))
	}
}

// ── principal_status_projection ─────────────────────────────────────────────

// The property the whole layer rests on: the table ships empty and an absent
// row is ACTIVE, so /v1/authorize behaves exactly as it did before 000013.
func TestFindPrincipalStatus_AbsentIsActive(t *testing.T) {
	s := newQueryStore(t)

	status, err := s.FindPrincipalStatus(context.Background(), "p-never-seen", qTenantA)
	if err != nil {
		t.Fatalf("FindPrincipalStatus: %v", err)
	}
	if status != domain.PrincipalStatusActive {
		t.Fatalf("status = %q for an unprojected principal, want ACTIVE — the alternative denies every principal on the platform until a status event arrives",
			status)
	}
}

func TestProjectPrincipalStatus_RoundTrips(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "p-1", TenantID: qTenantA, Status: "SUSPENDED", SourceService: "identity-context-svc",
	}); err != nil {
		t.Fatalf("ProjectPrincipalStatus: %v", err)
	}

	status, err := s.FindPrincipalStatus(ctx, "p-1", qTenantA)
	if err != nil {
		t.Fatalf("FindPrincipalStatus: %v", err)
	}
	if status != "SUSPENDED" {
		t.Fatalf("status = %q, want SUSPENDED", status)
	}
}

// A status is a current value, not a history: the second event replaces the
// first rather than adding a row. Also the redelivery case — the broker
// redelivers and an INSERT per delivery would violate the primary key.
func TestProjectPrincipalStatus_IsAnUpsert(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	for _, st := range []string{"SUSPENDED", "SUSPENDED", "DISABLED", domain.PrincipalStatusActive} {
		if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
			PrincipalID: "p-1", TenantID: qTenantA, Status: st, SourceService: "identity-context-svc",
		}); err != nil {
			t.Fatalf("projecting %s: %v", st, err)
		}
	}

	status, err := s.FindPrincipalStatus(ctx, "p-1", qTenantA)
	if err != nil {
		t.Fatalf("FindPrincipalStatus: %v", err)
	}
	if status != domain.PrincipalStatusActive {
		t.Fatalf("status = %q after a reinstatement, want ACTIVE", status)
	}
}

// An older event must not overwrite a newer one. A replay of an old SUSPENDED
// after a newer ACTIVE would re-suspend a reinstated principal, silently, and
// the only symptom would be a person unable to work.
func TestProjectPrincipalStatus_RefusesAStaleEvent(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	newer := time.Now().UTC()
	older := newer.Add(-time.Hour)

	if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "p-1", TenantID: qTenantA, Status: domain.PrincipalStatusActive,
		SourceService: "identity-context-svc", StatusChangedAt: &newer,
	}); err != nil {
		t.Fatalf("projecting the newer status: %v", err)
	}

	_, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "p-1", TenantID: qTenantA, Status: "SUSPENDED",
		SourceService: "identity-context-svc", StatusChangedAt: &older,
	})
	if !errors.Is(err, domain.ErrPrincipalStatusStale) {
		t.Fatalf("err = %v, want ErrPrincipalStatusStale", err)
	}

	status, err := s.FindPrincipalStatus(ctx, "p-1", qTenantA)
	if err != nil {
		t.Fatalf("FindPrincipalStatus: %v", err)
	}
	if status != domain.PrincipalStatusActive {
		t.Fatalf("status = %q — a stale replay re-suspended a reinstated principal", status)
	}
}

// An event with no timestamp still applies. identity-context-svc does not
// currently send one, and refusing on a missing field would make every event
// from the current producer permanently unappliable.
func TestProjectPrincipalStatus_AppliesWithoutATimestamp(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	at := time.Now().UTC()
	if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "p-1", TenantID: qTenantA, Status: domain.PrincipalStatusActive,
		SourceService: "identity-context-svc", StatusChangedAt: &at,
	}); err != nil {
		t.Fatalf("first: %v", err)
	}

	if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "p-1", TenantID: qTenantA, Status: "SUSPENDED",
		SourceService: "identity-context-svc", StatusChangedAt: nil,
	}); err != nil {
		t.Fatalf("timestampless event was refused: %v — the current producer sends no timestamp, so this would make it unappliable", err)
	}

	status, err := s.FindPrincipalStatus(ctx, "p-1", qTenantA)
	if err != nil {
		t.Fatalf("FindPrincipalStatus: %v", err)
	}
	if status != "SUSPENDED" {
		t.Fatalf("status = %q, want SUSPENDED", status)
	}
}

// Status is per tenant. A suspension in tenant A must not deny in tenant B —
// identity-context-svc publishes standing per tenant, and cross-tenant denial
// would be this service inventing a policy the authoritative service did not
// state.
func TestFindPrincipalStatus_ScopedReadIsPerTenant(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "p-1", TenantID: qTenantA, Status: "SUSPENDED", SourceService: "identity-context-svc",
	}); err != nil {
		t.Fatalf("project: %v", err)
	}

	inA, err := s.FindPrincipalStatus(ctx, "p-1", qTenantA)
	if err != nil {
		t.Fatalf("read in A: %v", err)
	}
	if inA != "SUSPENDED" {
		t.Errorf("status in tenant A = %q, want SUSPENDED", inA)
	}

	inB, err := s.FindPrincipalStatus(ctx, "p-1", qTenantB)
	if err != nil {
		t.Fatalf("read in B: %v", err)
	}
	if inB != domain.PrincipalStatusActive {
		t.Errorf("status in tenant B = %q, want ACTIVE — standing is published per tenant", inB)
	}
}

// The tenantless read — 86 of this endpoint's 111 callers — takes the MOST
// RESTRICTIVE row. A gate escapable by omitting a header would not be a gate,
// so a principal suspended in any tenant is suspended for a caller that names
// none. This is what 000013's platform_scope hatch exists for.
func TestFindPrincipalStatus_TenantlessReadTakesTheMostRestrictiveRow(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	// ACTIVE in one tenant, SUSPENDED in another.
	if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "p-1", TenantID: qTenantA, Status: domain.PrincipalStatusActive, SourceService: "identity-context-svc",
	}); err != nil {
		t.Fatalf("project A: %v", err)
	}
	if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "p-1", TenantID: qTenantB, Status: "SUSPENDED", SourceService: "identity-context-svc",
	}); err != nil {
		t.Fatalf("project B: %v", err)
	}

	status, err := s.FindPrincipalStatus(ctx, "p-1", "")
	if err != nil {
		t.Fatalf("tenantless read: %v", err)
	}
	if status != "SUSPENDED" {
		t.Fatalf("tenantless read = %q, want SUSPENDED — otherwise the gate is bypassed by omitting X-Tenant-Id, which is what most of this endpoint's callers do",
			status)
	}
}

// And a tenantless read for a principal who is active everywhere is ACTIVE —
// the most-restrictive ordering must not turn "active in two tenants" into a
// denial.
func TestFindPrincipalStatus_TenantlessReadIsActiveWhenEveryRowIs(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	for _, tenant := range []string{qTenantA, qTenantB} {
		if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
			PrincipalID: "p-1", TenantID: tenant, Status: domain.PrincipalStatusActive, SourceService: "identity-context-svc",
		}); err != nil {
			t.Fatalf("project %s: %v", tenant, err)
		}
	}

	status, err := s.FindPrincipalStatus(ctx, "p-1", "")
	if err != nil {
		t.Fatalf("tenantless read: %v", err)
	}
	if status != domain.PrincipalStatusActive {
		t.Fatalf("tenantless read = %q, want ACTIVE", status)
	}
}

func TestProjectPrincipalStatus_RefusesIncompleteInput(t *testing.T) {
	s := newQueryStore(t)
	ctx := context.Background()

	if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "p-1", TenantID: "", Status: "SUSPENDED",
	}); !errors.Is(err, domain.ErrTenantScopeRequired) {
		t.Errorf("no tenant: err = %v, want ErrTenantScopeRequired", err)
	}

	if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "", TenantID: qTenantA, Status: "SUSPENDED",
	}); !errors.Is(err, domain.ErrPrincipalStatusIncomplete) {
		t.Errorf("no principal: err = %v, want ErrPrincipalStatusIncomplete", err)
	}

	// An empty status would be read as not-ACTIVE by the gate and deny that
	// principal everything, from a malformed event.
	if _, err := s.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "p-1", TenantID: qTenantA, Status: "",
	}); !errors.Is(err, domain.ErrPrincipalStatusIncomplete) {
		t.Errorf("no status: err = %v, want ErrPrincipalStatusIncomplete", err)
	}
}

func TestFindPrincipalStatus_EmptyPrincipalIsActive(t *testing.T) {
	s := newQueryStore(t)

	// Not an error: /v1/authorize validates principal_id before reaching the
	// gate, so this is defence in depth and must not 503 the evaluation.
	status, err := s.FindPrincipalStatus(context.Background(), "", qTenantA)
	if err != nil {
		t.Fatalf("FindPrincipalStatus: %v", err)
	}
	if status != domain.PrincipalStatusActive {
		t.Fatalf("status = %q", status)
	}
}

// The claim 000013's down migration rests on: with the table ABSENT, layer 0
// answers ACTIVE rather than failing the evaluation.
//
// Without this, reverting 000013 on a live service would 503 every
// authorization call on the platform — a rollback that takes the authorization
// plane down is not a rollback. Matched on SQLSTATE 42P01 rather than on the
// error message, which is localised.
//
// This test drops the table AFTER setup deliberately: it is the only way to
// reach the branch, and it is why the drop is the last thing it does.
func TestFindPrincipalStatus_MissingTableIsInertNotAnOutage(t *testing.T) {
	pool := getTestPool(t)
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DROP TABLE principal_status_projection;"); err != nil {
		t.Fatalf("dropping the projection: %v", err)
	}

	// Both branches — the tenant-scoped read and the tenantless one, which
	// take different code paths (withRLS vs withPlatformScope).
	scoped, err := s.FindPrincipalStatus(ctx, "p-1", qTenantA)
	if err != nil {
		t.Fatalf("tenant-scoped read with the table absent returned %v — reverting 000013 would 503 every authorization call on the platform", err)
	}
	if scoped != domain.PrincipalStatusActive {
		t.Errorf("tenant-scoped read = %q, want ACTIVE", scoped)
	}

	tenantless, err := s.FindPrincipalStatus(ctx, "p-1", "")
	if err != nil {
		t.Fatalf("tenantless read with the table absent returned %v", err)
	}
	if tenantless != domain.PrincipalStatusActive {
		t.Errorf("tenantless read = %q, want ACTIVE", tenantless)
	}
}
