package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

func getTestPool(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real PostgreSQL integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("failed to connect to TEST_DATABASE_URL: %v", err)
	}
	return pool
}

// migrationFiles is every up migration, in order. Listing them here rather
// than naming two of them inline means a migration added later is applied by
// the tests automatically — the alternative has already produced a live
// incident in this repo, where a migration was never applied and every write
// failed with a 42P10 that read like a code bug.
var migrationFiles = []string{
	"000001_initial_schema.up.sql",
	"000002_add_audit_columns.up.sql",
	"000003_add_data_classification.up.sql",
	"000004_add_rule_code_index.up.sql",
}

// setupTestDB drops and recreates the schema from the migration files.
func setupTestDB(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS jurisdiction_rule_drift_events, jurisdiction_rules, jurisdictions CASCADE;")

	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "deployments", "migrations")

	for _, name := range migrationFiles {
		sql, err := os.ReadFile(filepath.Join(migDir, name))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to execute migration %s: %v", name, err)
		}
	}
}

// newTestStore returns a store against a freshly migrated schema.
func newTestStore(t *testing.T) (*store.PgStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool := getTestPool(t)
	t.Cleanup(pool.Close)
	setupTestDB(t, pool)
	return store.New(pool, zap.NewNop()), pool, context.Background()
}

// mustCreateJurisdiction inserts a jurisdiction and fails the test if it cannot.
func mustCreateJurisdiction(t *testing.T, s *store.PgStore, ctx context.Context, code string, parent *string) *domain.Jurisdiction {
	t.Helper()
	jType := "COUNTRY"
	if parent != nil {
		jType = "STATE_PROVINCE"
	}
	j, _, err := s.CreateJurisdiction(ctx, domain.CreateJurisdictionParams{
		JurisdictionID:       uuid.New().String(),
		JurisdictionCode:     code,
		JurisdictionName:     "Jurisdiction " + code,
		JurisdictionType:     jType,
		ParentJurisdictionID: parent,
		AuthorityType:        "FEDERAL",
		EffectiveFrom:        time.Now().UTC().Add(-365 * 24 * time.Hour),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create jurisdiction %s: %v", code, err)
	}
	return j
}

func ptr[T any](v T) *T { return &v }

// ── jurisdictions ────────────────────────────────────────────────────────────

func TestPgStore_CreateJurisdiction_IdempotencyAnd409(t *testing.T) {
	s, _, ctx := newTestStore(t)

	id := uuid.New().String()
	params := domain.CreateJurisdictionParams{
		JurisdictionID:       id,
		JurisdictionCode:     "US-CA",
		JurisdictionName:     "California",
		JurisdictionType:     "STATE_PROVINCE",
		AuthorityType:        "STATE",
		EffectiveFrom:        time.Now().UTC().Truncate(time.Microsecond),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	}

	// 1. Initial creation
	j1, created, err := s.CreateJurisdiction(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on create: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on initial insert")
	}
	if j1.JurisdictionCode != "US-CA" {
		t.Errorf("expected code US-CA, got %s", j1.JurisdictionCode)
	}
	// PUBLIC is the tier data_classification_audit.md §2.11 assigns to
	// jurisdictions; it must be written without the caller supplying it.
	if j1.DataClassification != "PUBLIC" {
		t.Errorf("expected data_classification PUBLIC, got %q", j1.DataClassification)
	}

	// 2. Identical retry (idempotent 200 OK no-op)
	j2, created, err := s.CreateJurisdiction(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on identical retry")
	}
	if j2.JurisdictionID != j1.JurisdictionID {
		t.Errorf("expected ID %s, got %s", j1.JurisdictionID, j2.JurisdictionID)
	}

	// 3. Differing attribute on same dedup key (409 Conflict)
	conflictParams := params
	conflictParams.JurisdictionID = uuid.New().String()           // different ID, but same (code, type, parent)
	conflictParams.JurisdictionName = "California Republic State" // differing attribute!

	_, created, err = s.CreateJurisdiction(ctx, conflictParams)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict (409) on differing attribute, got: %v", err)
	}
	if created {
		t.Errorf("expected created=false on conflict")
	}
}

// TestPgStore_CreateJurisdiction_UnknownParent — the self-referential foreign
// key raised 23503, which the generic error path reported as
// ErrStoreUnavailable, i.e. an outage signal for a client mistake.
func TestPgStore_CreateJurisdiction_UnknownParent(t *testing.T) {
	s, _, ctx := newTestStore(t)

	_, _, err := s.CreateJurisdiction(ctx, domain.CreateJurisdictionParams{
		JurisdictionID:       uuid.New().String(),
		JurisdictionCode:     "US-CA",
		JurisdictionName:     "California",
		JurisdictionType:     "STATE_PROVINCE",
		ParentJurisdictionID: ptr(uuid.New().String()), // never inserted
		AuthorityType:        "STATE",
		EffectiveFrom:        time.Now().UTC(),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	})
	if !errors.Is(err, domain.ErrParentNotFound) {
		t.Fatalf("expected ErrParentNotFound, got %v", err)
	}
}

// TestPgStore_DeactivateJurisdiction also asserts the record is end-dated.
// The domain model states deactivation is "active_flag + effective_to", but
// only active_flag was written, so a deactivated jurisdiction kept an
// open-ended effective period.
func TestPgStore_DeactivateJurisdiction(t *testing.T) {
	s, _, ctx := newTestStore(t)

	j := mustCreateJurisdiction(t, s, ctx, "GB", nil)

	deactivated, err := s.DeactivateJurisdiction(ctx, j.JurisdictionID, "actor-deactivate")
	if err != nil {
		t.Fatalf("unexpected error on deactivate: %v", err)
	}
	if deactivated.ActiveFlag {
		t.Errorf("expected ActiveFlag=false after deactivation")
	}
	if deactivated.EffectiveTo == nil {
		t.Error("expected EffectiveTo to be set — deactivation is active_flag + effective_to, not active_flag alone")
	}
	if deactivated.UpdatedAt == nil {
		t.Fatal("expected UpdatedAt to be set")
	}
	if deactivated.UpdatedByPrincipalID == nil || *deactivated.UpdatedByPrincipalID != "actor-deactivate" {
		t.Errorf("expected UpdatedByPrincipalID=actor-deactivate, got %v", deactivated.UpdatedByPrincipalID)
	}

	// The active-only validation contract must now reject it.
	if _, err := s.FindByID(ctx, j.JurisdictionID); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("expected FindByID to 404 a deactivated jurisdiction, got %v", err)
	}
	// But it must still be readable for historical replay.
	if _, err := s.FindByIDAny(ctx, j.JurisdictionID); err != nil {
		t.Errorf("expected FindByIDAny to still find the deactivated jurisdiction, got %v", err)
	}

	// Non-existent deactivation
	_, err = s.DeactivateJurisdiction(ctx, uuid.New().String(), "actor-1")
	if !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("expected ErrJurisdictionNotFound for unknown UUID, got: %v", err)
	}
}

// TestPgStore_MalformedUUIDIsNotFoundNotOutage — a syntactically impossible
// id died in the pgx driver as SQLSTATE 22P02 and surfaced as 503
// store_unavailable, so a client typo read as a platform outage and made the
// fail-closed 503 contract meaningless.
func TestPgStore_MalformedUUIDIsNotFoundNotOutage(t *testing.T) {
	s, _, ctx := newTestStore(t)

	if _, err := s.FindByID(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("FindByID: expected ErrJurisdictionNotFound, got %v", err)
	}
	if _, err := s.FindByIDAny(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("FindByIDAny: expected ErrJurisdictionNotFound, got %v", err)
	}
	if _, err := s.FindRuleByID(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrRuleNotFound) {
		t.Errorf("FindRuleByID: expected ErrRuleNotFound, got %v", err)
	}
	if _, err := s.FindAncestors(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("FindAncestors: expected ErrJurisdictionNotFound, got %v", err)
	}
	if _, err := s.DeactivateJurisdiction(ctx, "not-a-uuid", "actor"); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("DeactivateJurisdiction: expected ErrJurisdictionNotFound, got %v", err)
	}
}

// ── hierarchy ────────────────────────────────────────────────────────────────

// TestPgStore_FindAncestors_Chain verifies a country → state → authority
// chain resolves nearest-first in a single query.
func TestPgStore_FindAncestors_Chain(t *testing.T) {
	s, _, ctx := newTestStore(t)

	country := mustCreateJurisdiction(t, s, ctx, "US", nil)
	state := mustCreateJurisdiction(t, s, ctx, "US-CA", &country.JurisdictionID)
	authority := mustCreateJurisdiction(t, s, ctx, "US-CA-FTB", &state.JurisdictionID)

	ancestors, err := s.FindAncestors(ctx, authority.JurisdictionID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ancestors) != 2 {
		t.Fatalf("expected 2 ancestors, got %d", len(ancestors))
	}
	if ancestors[0].JurisdictionID != state.JurisdictionID {
		t.Errorf("expected nearest ancestor to be the state, got %s", ancestors[0].JurisdictionCode)
	}
	if ancestors[1].JurisdictionID != country.JurisdictionID {
		t.Errorf("expected root ancestor to be the country, got %s", ancestors[1].JurisdictionCode)
	}

	// A root jurisdiction has no ancestors, but is not "not found".
	rootAncestors, err := s.FindAncestors(ctx, country.JurisdictionID)
	if err != nil {
		t.Fatalf("unexpected error for root: %v", err)
	}
	if len(rootAncestors) != 0 {
		t.Errorf("expected 0 ancestors for a root jurisdiction, got %d", len(rootAncestors))
	}

	if _, err := s.FindAncestors(ctx, uuid.New().String()); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("expected ErrJurisdictionNotFound for an unknown id, got %v", err)
	}
}

// TestPgStore_FindAncestors_CycleTerminates — the old iterative walk had no
// visited set, so a cycle returned the same jurisdictions maxAncestorDepth
// times instead of terminating. The cycle is created with direct SQL because
// CreateJurisdiction refuses to build one.
func TestPgStore_FindAncestors_CycleTerminates(t *testing.T) {
	s, pool, ctx := newTestStore(t)

	a := mustCreateJurisdiction(t, s, ctx, "CYC-A", nil)
	b := mustCreateJurisdiction(t, s, ctx, "CYC-B", &a.JurisdictionID)

	// Close the loop: A's parent becomes B.
	if _, err := pool.Exec(ctx,
		"UPDATE jurisdictions SET parent_jurisdiction_id = $1 WHERE jurisdiction_id = $2",
		b.JurisdictionID, a.JurisdictionID,
	); err != nil {
		t.Fatalf("failed to create cycle: %v", err)
	}

	ancestors, err := s.FindAncestors(ctx, a.JurisdictionID)
	if err != nil {
		t.Fatalf("unexpected error walking a cyclic hierarchy: %v", err)
	}
	// A → B, and B's parent A is already seen, so the walk stops there.
	if len(ancestors) != 1 {
		t.Fatalf("expected the walk to stop at the repeat (1 ancestor), got %d", len(ancestors))
	}
	if ancestors[0].JurisdictionID != b.JurisdictionID {
		t.Errorf("expected ancestor %s, got %s", b.JurisdictionID, ancestors[0].JurisdictionID)
	}
}

func TestPgStore_CreateJurisdiction_RejectsSelfParent(t *testing.T) {
	s, _, ctx := newTestStore(t)

	id := uuid.New().String()
	_, _, err := s.CreateJurisdiction(ctx, domain.CreateJurisdictionParams{
		JurisdictionID:       id,
		JurisdictionCode:     "SELF",
		JurisdictionName:     "Self Parent",
		JurisdictionType:     "COUNTRY",
		ParentJurisdictionID: &id,
		AuthorityType:        "FEDERAL",
		EffectiveFrom:        time.Now().UTC(),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	})
	if !errors.Is(err, domain.ErrCyclicHierarchy) {
		t.Fatalf("expected ErrCyclicHierarchy, got %v", err)
	}
}

// ── rules ────────────────────────────────────────────────────────────────────

func TestPgStore_CreateRule_IdempotencyAnd409(t *testing.T) {
	s, _, ctx := newTestStore(t)

	j := mustCreateJurisdiction(t, s, ctx, "DE", nil)

	ruleID := uuid.New().String()
	effFrom := time.Now().UTC().Truncate(time.Microsecond)
	params := domain.CreateRuleParams{
		JurisdictionRuleID:   ruleID,
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "DE_VAT_STANDARD",
		RuleName:             "Standard VAT Rate",
		EffectiveFrom:        effFrom,
		RulePayload:          []byte(`{"filing_frequency": "MONTHLY"}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	}

	// 1. Initial creation
	r1, created, err := s.CreateRule(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error creating rule: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on initial insert")
	}
	if r1.RuleCode != "DE_VAT_STANDARD" {
		t.Errorf("expected code DE_VAT_STANDARD, got %s", r1.RuleCode)
	}
	if r1.DataClassification != "INTERNAL" {
		t.Errorf("expected data_classification INTERNAL, got %q", r1.DataClassification)
	}

	// 2. Identical retry (idempotent 200 OK no-op).
	// JSONB normalises key order and whitespace on the way out, so this also
	// covers the byte-comparison bug that turned a retried POST into a 409.
	r2, created, err := s.CreateRule(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on retry")
	}
	if r2.JurisdictionRuleID != r1.JurisdictionRuleID {
		t.Errorf("expected ID %s, got %s", r1.JurisdictionRuleID, r2.JurisdictionRuleID)
	}

	// 3. Differing payload on same dedup key (409 Conflict)
	conflictParams := params
	conflictParams.JurisdictionRuleID = uuid.New().String()
	conflictParams.RulePayload = []byte(`{"filing_frequency": "QUARTERLY"}`) // differing payload!

	_, created, err = s.CreateRule(ctx, conflictParams)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict (409) on differing payload, got: %v", err)
	}
	if created {
		t.Errorf("expected created=false on conflict")
	}
}

// TestPgStore_CreateRule_UnknownJurisdiction — this hit the foreign key and
// surfaced as ErrStoreUnavailable, so "you named a jurisdiction that does not
// exist" and "the database is down" were the same response.
func TestPgStore_CreateRule_UnknownJurisdiction(t *testing.T) {
	s, _, ctx := newTestStore(t)

	_, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionID:       uuid.New().String(),
		RuleDomain:           "TAX",
		RuleCode:             "X",
		RuleName:             "X",
		EffectiveFrom:        time.Now().UTC(),
		RulePayload:          []byte(`{}`),
		RuleStatus:           "DRAFT",
		CreatedByPrincipalID: "admin-1",
	})
	if !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Fatalf("expected ErrJurisdictionNotFound, got %v", err)
	}
}

// TestPgStore_CreateRule_RejectsOverlap — two live rules with the same code
// and overlapping periods both satisfy a point-in-time query, which makes
// "the effective rule at date X" ambiguous.
func TestPgStore_CreateRule_RejectsOverlap(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "FR", nil)

	base := domain.CreateRuleParams{
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "FR_VAT",
		RuleName:             "VAT",
		EffectiveFrom:        time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		EffectiveTo:          ptr(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		RulePayload:          []byte(`{}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	}
	if _, _, err := s.CreateRule(ctx, base); err != nil {
		t.Fatalf("failed to create the incumbent rule: %v", err)
	}

	// Starts inside the incumbent's period.
	overlapping := base
	overlapping.JurisdictionRuleID = uuid.New().String()
	overlapping.EffectiveFrom = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	overlapping.EffectiveTo = nil
	if _, _, err := s.CreateRule(ctx, overlapping); !errors.Is(err, domain.ErrOverlappingRule) {
		t.Fatalf("expected ErrOverlappingRule, got %v", err)
	}

	// Starts exactly when the incumbent ends — half-open intervals do not
	// overlap, so this is the legitimate successor and must be accepted.
	successor := base
	successor.JurisdictionRuleID = uuid.New().String()
	successor.EffectiveFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	successor.EffectiveTo = nil
	if _, _, err := s.CreateRule(ctx, successor); err != nil {
		t.Fatalf("an adjacent successor must be allowed, got %v", err)
	}

	// A DRAFT replacement may be prepared while the incumbent is in force.
	draft := base
	draft.JurisdictionRuleID = uuid.New().String()
	draft.EffectiveFrom = time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	draft.EffectiveTo = nil
	draft.RuleStatus = "DRAFT"
	if _, _, err := s.CreateRule(ctx, draft); err != nil {
		t.Fatalf("a DRAFT overlapping the incumbent must be allowed, got %v", err)
	}

	// ...but activating it must not be, because that is when the ambiguity
	// would become real.
	_, _, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        draft.JurisdictionRuleID,
		NewStatus:     "ACTIVE",
		AllowedPriors: []string{"DRAFT"},
		ActorID:       "admin-1",
	})
	if !errors.Is(err, domain.ErrOverlappingRule) {
		t.Fatalf("expected activating an overlapping DRAFT to be refused, got %v", err)
	}
}

func TestPgStore_TransitionRuleStatus_StateMachineAndNoOp(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "FR", nil)

	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "PAYROLL",
		RuleCode:             "FR_SOCIAL_SEC",
		RuleName:             "Social Security Contribution",
		EffectiveFrom:        time.Now().UTC().Add(-time.Hour),
		RulePayload:          []byte(`{"applies": true}`),
		RuleStatus:           "DRAFT", // initial state
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}

	// 1. Legal transition DRAFT -> ACTIVE
	updated, transitioned, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        r.JurisdictionRuleID,
		NewStatus:     "ACTIVE",
		AllowedPriors: []string{"DRAFT"},
		ActorID:       "actor-1",
	})
	if err != nil {
		t.Fatalf("unexpected error transitioning DRAFT -> ACTIVE: %v", err)
	}
	if !transitioned {
		t.Error("expected transitioned=true for a real status change")
	}
	if updated.RuleStatus != "ACTIVE" {
		t.Errorf("expected status ACTIVE, got %s", updated.RuleStatus)
	}
	if updated.UpdatedAt == nil {
		t.Fatal("expected UpdatedAt to be set after transition")
	}
	// Activation must not close the rule.
	if updated.EffectiveTo != nil {
		t.Errorf("ACTIVE must not end-date the rule, got effective_to=%v", updated.EffectiveTo)
	}

	// 2. Idempotent network retry: call ACTIVE again when current is already ACTIVE.
	// allowedPriors is still ["DRAFT"] — without the pre-read check this fails.
	retried, transitioned, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        r.JurisdictionRuleID,
		NewStatus:     "ACTIVE",
		AllowedPriors: []string{"DRAFT"},
		ActorID:       "actor-1",
	})
	if err != nil {
		t.Fatalf("unexpected error on idempotent retry: %v", err)
	}
	if transitioned {
		t.Error("expected transitioned=false on a replay — the caller must not re-publish rule.activated")
	}
	if retried.RuleStatus != "ACTIVE" {
		t.Errorf("expected status ACTIVE on retry, got %s", retried.RuleStatus)
	}

	// 3. Illegal transition: nothing moves back to DRAFT.
	_, _, err = s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        r.JurisdictionRuleID,
		NewStatus:     "DRAFT",
		AllowedPriors: []string{},
		ActorID:       "actor-1",
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition on illegal transition, got: %v", err)
	}
}

// TestPgStore_TransitionRuleStatus_SupersedeEndDates is the regression test
// for the ambiguity a superseded rule left behind: with effective_to still
// NULL it kept matching every point-in-time query, side by side with its own
// replacement.
func TestPgStore_TransitionRuleStatus_SupersedeEndDates(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "IE", nil)

	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "IE_VAT",
		RuleName:             "VAT",
		EffectiveFrom:        time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		RulePayload:          []byte(`{}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}

	closedAt := time.Date(2025, 4, 6, 0, 0, 0, 0, time.UTC)
	superseded, _, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        r.JurisdictionRuleID,
		NewStatus:     "SUPERSEDED",
		AllowedPriors: []string{"ACTIVE"},
		EndDate:       true,
		EffectiveTo:   &closedAt,
		ActorID:       "actor-1",
	})
	if err != nil {
		t.Fatalf("unexpected error superseding: %v", err)
	}
	if superseded.EffectiveTo == nil {
		t.Fatal("a superseded rule must be end-dated")
	}
	if !superseded.EffectiveTo.Equal(closedAt) {
		t.Errorf("effective_to = %v, want the supplied %v", superseded.EffectiveTo, closedAt)
	}

	// It must now be invisible to a query after the close date...
	after, err := s.FindRules(ctx, store.FindRulesParams{
		JurisdictionID: j.JurisdictionID,
		EffectiveAt:    closedAt.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("expected the superseded rule to be excluded after its end date, got %d rules", len(after))
	}

	// ...and still visible to a historical one, which is what "historical
	// actions must always be explainable" requires.
	before, err := s.FindRules(ctx, store.FindRulesParams{
		JurisdictionID: j.JurisdictionID,
		EffectiveAt:    time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("expected the superseded rule to remain visible historically, got %d rules", len(before))
	}
}

// TestPgStore_TransitionRuleStatus_RejectsEndDateBeforeStart — a period that
// ends before it starts can never match a point-in-time query.
func TestPgStore_TransitionRuleStatus_RejectsEndDateBeforeStart(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "NL", nil)

	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "NL_VAT",
		RuleName:             "VAT",
		EffectiveFrom:        time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		RulePayload:          []byte(`{}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}

	_, _, err = s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID:        r.JurisdictionRuleID,
		NewStatus:     "RETIRED",
		AllowedPriors: []string{"ACTIVE", "SUPERSEDED"},
		EndDate:       true,
		EffectiveTo:   ptr(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
		ActorID:       "actor-1",
	})
	if !errors.Is(err, domain.ErrInvalidEffectivePeriod) {
		t.Fatalf("expected ErrInvalidEffectivePeriod, got %v", err)
	}
}

// ── legal drift ──────────────────────────────────────────────────────────────

// TestPgStore_RecordDrift covers the capability the drift_events table
// shipped for and nothing ever used: legal_drift_state had no history and no
// way to be set, despite legal.drift.detected being a published event.
func TestPgStore_RecordDrift(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "GB", nil)

	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "PAYROLL",
		RuleCode:             "GB_NI_THRESHOLD",
		RuleName:             "National Insurance Threshold",
		EffectiveFrom:        time.Now().UTC().Add(-time.Hour),
		RulePayload:          []byte(`{}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}
	if r.LegalDriftState != "CURRENT" {
		t.Fatalf("expected a new rule to start CURRENT, got %q", r.LegalDriftState)
	}

	// 1. CURRENT -> DRIFTED, with evidence.
	reason := "HMRC SI 2025/412 revised the threshold"
	drifted, event, changed, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID:    r.JurisdictionRuleID,
		ToState:               "DRIFTED",
		Reason:                &reason,
		RecordedByPrincipalID: "feed-worker-1",
		CorrelationID:         "corr-drift-1",
	})
	if err != nil {
		t.Fatalf("unexpected error recording drift: %v", err)
	}
	if !changed {
		t.Error("expected changed=true for a real drift transition")
	}
	if drifted.LegalDriftState != "DRIFTED" {
		t.Errorf("rule drift state = %q, want DRIFTED", drifted.LegalDriftState)
	}
	if event == nil {
		t.Fatal("expected a drift event to be written")
	}
	if event.FromState != "CURRENT" || event.ToState != "DRIFTED" {
		t.Errorf("event transition = %s -> %s, want CURRENT -> DRIFTED", event.FromState, event.ToState)
	}
	if event.Reason == nil || *event.Reason != reason {
		t.Errorf("event reason = %v, want %q", event.Reason, reason)
	}
	if event.CorrelationID == nil || *event.CorrelationID != "corr-drift-1" {
		t.Errorf("event correlation_id = %v, want corr-drift-1", event.CorrelationID)
	}

	// 2. Replay must be a no-op and must NOT append a second history entry.
	_, replayEvent, changed, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID:    r.JurisdictionRuleID,
		ToState:               "DRIFTED",
		RecordedByPrincipalID: "feed-worker-1",
	})
	if err != nil {
		t.Fatalf("unexpected error on replay: %v", err)
	}
	if changed {
		t.Error("expected changed=false on replay")
	}
	if replayEvent != nil {
		t.Error("a replay must not append to the append-only history")
	}

	// 3. DRIFTED -> UNDER_REVIEW -> CURRENT builds the full history.
	if _, _, _, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID:    r.JurisdictionRuleID,
		ToState:               "UNDER_REVIEW",
		RecordedByPrincipalID: "reviewer-1",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, _, _, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID:    r.JurisdictionRuleID,
		ToState:               "CURRENT",
		RecordedByPrincipalID: "reviewer-1",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	history, err := s.FindDriftEvents(ctx, r.JurisdictionRuleID, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error reading history: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("expected 3 history entries, got %d", len(history))
	}
	// Newest first.
	if history[0].ToState != "CURRENT" {
		t.Errorf("expected newest-first ordering, got %q first", history[0].ToState)
	}

	// Unknown rule.
	if _, _, _, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID:    uuid.New().String(),
		ToState:               "DRIFTED",
		RecordedByPrincipalID: "x",
	}); !errors.Is(err, domain.ErrRuleNotFound) {
		t.Errorf("expected ErrRuleNotFound, got %v", err)
	}
	if _, err := s.FindDriftEvents(ctx, uuid.New().String(), 0, 0); !errors.Is(err, domain.ErrRuleNotFound) {
		t.Errorf("expected ErrRuleNotFound from FindDriftEvents, got %v", err)
	}
}

// ── rule pack ────────────────────────────────────────────────────────────────

// TestPgStore_FindRulePack_ResolvesInheritance is the test for the capability
// that did not exist: callers had to fetch the ancestor chain and issue one
// /rules request per generation, then decide for themselves which rule wins.
func TestPgStore_FindRulePack_ResolvesInheritance(t *testing.T) {
	s, _, ctx := newTestStore(t)

	country := mustCreateJurisdiction(t, s, ctx, "US", nil)
	state := mustCreateJurisdiction(t, s, ctx, "US-CA", &country.JurisdictionID)

	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

	newRule := func(jID, domainName, code, name, status string, effFrom time.Time) {
		t.Helper()
		if _, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
			JurisdictionRuleID:   uuid.New().String(),
			JurisdictionID:       jID,
			RuleDomain:           domainName,
			RuleCode:             code,
			RuleName:             name,
			EffectiveFrom:        effFrom,
			RulePayload:          []byte(`{}`),
			RuleStatus:           status,
			CreatedByPrincipalID: "admin-1",
		}); err != nil {
			t.Fatalf("failed to create rule %s/%s: %v", code, name, err)
		}
	}

	// Federal rules, one of which the state overrides.
	newRule(country.JurisdictionID, "TAX", "FILING_FREQ", "Federal filing frequency", "ACTIVE", from)
	newRule(country.JurisdictionID, "TAX", "FEDERAL_ONLY", "Federal only", "ACTIVE", from)
	newRule(state.JurisdictionID, "TAX", "FILING_FREQ", "State filing frequency", "ACTIVE", from)
	// Excluded: a DRAFT and a RETIRED never enter a runtime pack.
	newRule(state.JurisdictionID, "TAX", "STATE_DRAFT", "Draft", "DRAFT", from)
	newRule(state.JurisdictionID, "TAX", "STATE_RETIRED", "Retired", "RETIRED", from)
	// Different domain, so it must be filtered out when domain=TAX.
	newRule(state.JurisdictionID, "PAYROLL", "STATE_PAYROLL", "State payroll", "ACTIVE", from)

	pack, err := s.FindRulePack(ctx, state.JurisdictionID, "TAX", at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pack.ResolvedFrom) != 2 || pack.ResolvedFrom[0] != state.JurisdictionID || pack.ResolvedFrom[1] != country.JurisdictionID {
		t.Errorf("resolved_from = %v, want [state country]", pack.ResolvedFrom)
	}

	byCode := map[string]*domain.JurisdictionRule{}
	for _, r := range pack.Rules {
		if _, dup := byCode[r.RuleCode]; dup {
			t.Errorf("rule_code %q appears twice — the pack must resolve to one winner per code", r.RuleCode)
		}
		byCode[r.RuleCode] = r
	}

	if len(byCode) != 2 {
		t.Fatalf("expected 2 resolved TAX rules (FILING_FREQ, FEDERAL_ONLY), got %d: %v", len(byCode), keysOf(byCode))
	}
	// The state's override must win over the country's.
	if got := byCode["FILING_FREQ"]; got == nil || got.JurisdictionID != state.JurisdictionID {
		t.Errorf("FILING_FREQ resolved to the wrong jurisdiction — the nearest one must win")
	}
	// The federal rule with no state override must still be inherited.
	if got := byCode["FEDERAL_ONLY"]; got == nil || got.JurisdictionID != country.JurisdictionID {
		t.Error("FEDERAL_ONLY should be inherited from the country")
	}
	if _, present := byCode["STATE_DRAFT"]; present {
		t.Error("a DRAFT rule must never enter a runtime rule pack")
	}
	if _, present := byCode["STATE_RETIRED"]; present {
		t.Error("a RETIRED rule must never enter a runtime rule pack")
	}
	if _, present := byCode["STATE_PAYROLL"]; present {
		t.Error("domain=TAX must exclude PAYROLL rules")
	}

	// Unfiltered, the PAYROLL rule joins the pack.
	all, err := s.FindRulePack(ctx, state.JurisdictionID, "", at)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(all.Rules) != 3 {
		t.Errorf("expected 3 rules with no domain filter, got %d", len(all.Rules))
	}
}

// TestPgStore_FindRulePack_InactiveJurisdictionFailsClosed — a pack is a
// runtime artifact, and an end-dated jurisdiction has no runtime rules.
func TestPgStore_FindRulePack_InactiveJurisdictionFailsClosed(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "GB", nil)

	if _, err := s.DeactivateJurisdiction(ctx, j.JurisdictionID, "admin-1"); err != nil {
		t.Fatalf("failed to deactivate: %v", err)
	}

	if _, err := s.FindRulePack(ctx, j.JurisdictionID, "", time.Now().UTC()); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Fatalf("expected ErrJurisdictionNotFound for a deactivated jurisdiction, got %v", err)
	}
	if _, err := s.FindRulePack(ctx, uuid.New().String(), "", time.Now().UTC()); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Fatalf("expected ErrJurisdictionNotFound for an unknown jurisdiction, got %v", err)
	}
}

func keysOf(m map[string]*domain.JurisdictionRule) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
