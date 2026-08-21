package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
	"zoiko.io/evidence-requirements-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migration from a
// clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set.
//
// WARNING, and the reason for the guard below: this DROPs evidence_requirements
// and evidence_evaluations. Point TEST_DATABASE_URL at the
// `evidence_requirements` database a running service uses and it silently
// deletes the requirement catalog AND the evaluation history — and past
// determinations are themselves evidence (§17.6), so that loss is not
// recoverable by re-running anything.
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS evidence_evaluations CASCADE;`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS evidence_requirements CASCADE;`)

	// Every *.up.sql, sorted, rather than a list written out here.
	//
	// The list here was hardcoded and had fallen behind the directory, so this
	// suite was applying a schema no deployment has -- in particular without the
	// FORCE row-level security migration, which is the one a store test most
	// needs in place. Globbing means the next migration is picked up without
	// anyone remembering to come back to this file. Same shape as
	// accounts-receivable-svc.
	migrationDir := filepath.Join(base, "../../deployments/migrations")
	migrations, err := filepath.Glob(filepath.Join(migrationDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("failed to glob migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatalf("no *.up.sql migrations found under %s", migrationDir)
	}
	sort.Strings(migrations)

	for _, migration := range migrations {
		sql, err := os.ReadFile(migration)
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", filepath.Base(migration), err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", filepath.Base(migration), err)
		}
	}

	return pool
}

// requireThrowawayDatabase fails the test unless the DSN's database name marks
// it as disposable. Two names are legitimate: CI points TEST_DATABASE_URL at
// `testdb`, and the local convention is `evidence_requirements_test`. Both
// contain "test"; the live `evidence_requirements` database does not, which is
// the case this guard exists to catch.
//
// The name is taken from the parsed DSN rather than matched against the whole
// string, so a host or password that happens to contain "test" cannot vouch for
// a live database. Anything that does not parse as a URL is refused rather than
// waved through — the suite DROPs tables, so an unreadable target is not a
// target worth guessing at. Same helper as the other store suites here.
func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably "+
			"disposable, and this suite DROPs evidence_requirements and evidence_evaluations. "+
			"Use evidence_requirements_test (or CI's testdb).", dbName)
	}
}

func strPtr(s string) *string { return &s }

func newTestRequirement(tenantID string, entityID *string) *domain.EvidenceRequirement {
	return &domain.EvidenceRequirement{
		EvidenceRequirementID: uuid.New().String(),
		TenantID:              tenantID,
		LegalEntityID:         entityID,
		DomainCode:            "PROCUREMENT",
		ActionType:            "PURCHASE_ORDER_ISSUE",
		EvidenceType:          "SUPPORTING_DOCUMENT",
		RequirementPayload:    json.RawMessage(`{"minimum_count":1}`),
		EffectiveFrom:         time.Now().Add(-time.Hour).UTC(),
		CreatedByPrincipalID:  "test-admin",
		CorrelationID:         "corr-" + uuid.New().String(),
	}
}

func TestPgStore_CreateRequirement_And_GetRequirement(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entity := uuid.New().String()
	r := newTestRequirement(tenantID, &entity)
	created, err := s.CreateRequirement(ctx, r)
	if err != nil {
		t.Fatalf("CreateRequirement: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for a first insert")
	}
	// A new requirement is in force, not retired: effective_to must be NULL.
	if r.EffectiveTo != nil {
		t.Error("a newly created requirement already carries an effective_to")
	}

	got, err := s.GetRequirement(ctx, r.EvidenceRequirementID)
	if err != nil {
		t.Fatalf("GetRequirement: %v", err)
	}
	if got == nil {
		t.Fatal("expected to read back the requirement just created")
	}
	if got.EvidenceType != r.EvidenceType || got.DomainCode != r.DomainCode {
		t.Errorf("read back %s/%s, want %s/%s",
			got.DomainCode, got.EvidenceType, r.DomainCode, r.EvidenceType)
	}
	// The payload is the extensibility seam — sufficiency parameters live here
	// as DATA so a jurisdiction-specific rule never becomes a code branch.
	// Losing it would silently drop the requirement's actual conditions.
	var payload map[string]any
	if err := json.Unmarshal(got.RequirementPayload, &payload); err != nil {
		t.Fatalf("requirement_payload did not survive the roundtrip: %v", err)
	}
	if payload["minimum_count"] != float64(1) {
		t.Errorf("requirement_payload = %v, want minimum_count 1", payload)
	}
}

// An omitted payload must persist as {} rather than NULL — a NULL payload would
// fail to unmarshal on every subsequent read of that requirement.
func TestPgStore_CreateRequirement_EmptyPayload_StoredAsObject(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	r := newTestRequirement(tenantID, nil)
	r.RequirementPayload = nil
	if _, err := s.CreateRequirement(ctx, r); err != nil {
		t.Fatalf("CreateRequirement: %v", err)
	}

	got, err := s.GetRequirement(ctx, r.EvidenceRequirementID)
	if err != nil || got == nil {
		t.Fatalf("GetRequirement: got=%v err=%v", got, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.RequirementPayload, &payload); err != nil {
		t.Fatalf("empty requirement_payload is not valid JSON on read: %v", err)
	}
}

func TestPgStore_CreateRequirement_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first := newTestRequirement(tenantID, nil)
	created, err := s.CreateRequirement(ctx, first)
	if err != nil || !created {
		t.Fatalf("first CreateRequirement: created=%v err=%v", created, err)
	}

	replay := newTestRequirement(tenantID, nil)
	replay.CorrelationID = first.CorrelationID
	replay.EvidenceType = "ATTESTATION"

	created, err = s.CreateRequirement(ctx, replay)
	if err != nil {
		t.Fatalf("replayed CreateRequirement: %v", err)
	}
	if created {
		t.Fatal("expected created=false on a replayed correlation_id — the handler " +
			"must not republish its Kafka event for a replay")
	}
	if replay.EvidenceRequirementID != first.EvidenceRequirementID {
		t.Errorf("replay resolved to %s, want the original %s",
			replay.EvidenceRequirementID, first.EvidenceRequirementID)
	}
	if replay.EvidenceType != first.EvidenceType {
		t.Errorf("replay returned evidence_type %q, want the original %q",
			replay.EvidenceType, first.EvidenceType)
	}
}

func TestPgStore_GetRequirement_OtherTenant_ReadsAsAbsent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	owner := uuid.New().String()

	r := newTestRequirement(owner, nil)
	if _, err := s.CreateRequirement(svcmiddleware.WithTenant(context.Background(), owner), r); err != nil {
		t.Fatalf("CreateRequirement: %v", err)
	}

	intruder := svcmiddleware.WithTenant(context.Background(), uuid.New().String())
	got, err := s.GetRequirement(intruder, r.EvidenceRequirementID)
	if err != nil {
		t.Fatalf("cross-tenant GetRequirement returned an error: %v", err)
	}
	if got != nil {
		t.Fatal("another tenant's requirement was readable")
	}
}

func TestPgStore_MalformedUUID_ReadsAsAbsent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	got, err := s.GetRequirement(ctx, "not-a-uuid")
	if err != nil {
		t.Fatalf("GetRequirement with a malformed id errored (%v) — it must read as "+
			"absent, not as a store failure that looks like an outage", err)
	}
	if got != nil {
		t.Fatal("malformed requirement id somehow matched a row")
	}

	ev, err := s.GetEvaluation(ctx, "not-a-uuid")
	if err != nil {
		t.Fatalf("GetEvaluation with a malformed id errored (%v)", err)
	}
	if ev != nil {
		t.Fatal("malformed evaluation id somehow matched a row")
	}

	_, err = s.EndDateRequirement(ctx, tenantID, "not-a-uuid", time.Now(), "reason", "actor")
	if !errors.Is(err, domain.ErrRequirementNotFound) {
		t.Fatalf("EndDateRequirement err = %v, want ErrRequirementNotFound — a malformed "+
			"id names no requirement, so \"already retired\" would be a claim about a "+
			"row that does not exist", err)
	}
}

// Retirement is effective end-dating, never a delete and never a flag. The row
// must survive, carry who retired it and why, and stop being effective.
func TestPgStore_EndDateRequirement_RetiresWithoutDeleting(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	r := newTestRequirement(tenantID, nil)
	if _, err := s.CreateRequirement(ctx, r); err != nil {
		t.Fatalf("CreateRequirement: %v", err)
	}

	retireAt := time.Now().UTC()
	retired, err := s.EndDateRequirement(ctx, tenantID, r.EvidenceRequirementID, retireAt, "superseded by policy", "retirer-1")
	if err != nil {
		t.Fatalf("EndDateRequirement: %v", err)
	}
	if retired.EffectiveTo == nil {
		t.Fatal("effective_to is still NULL after retirement — the requirement would keep " +
			"matching every point-in-time query")
	}

	// The row is still there: no soft-delete on material objects.
	still, err := s.GetRequirement(ctx, r.EvidenceRequirementID)
	if err != nil || still == nil {
		t.Fatalf("a retired requirement must still be readable: got=%v err=%v", still, err)
	}

	// Re-retiring is 422 already_retired, never a silent no-op.
	if _, err := s.EndDateRequirement(ctx, tenantID, r.EvidenceRequirementID, retireAt, "again", "retirer-2"); !errors.Is(err, domain.ErrAlreadyRetired) {
		t.Errorf("re-retire err = %v, want ErrAlreadyRetired", err)
	}

	// Retiring something that does not exist is a different fact from retiring
	// something already retired, and must not be collapsed into one answer.
	if _, err := s.EndDateRequirement(ctx, tenantID, uuid.New().String(), retireAt, "x", "y"); !errors.Is(err, domain.ErrRequirementNotFound) {
		t.Errorf("retire-unknown err = %v, want ErrRequirementNotFound", err)
	}
}

func TestPgStore_EndDateRequirement_OtherTenant_IsNotFound(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	owner := uuid.New().String()
	intruder := uuid.New().String()

	r := newTestRequirement(owner, nil)
	if _, err := s.CreateRequirement(svcmiddleware.WithTenant(context.Background(), owner), r); err != nil {
		t.Fatalf("CreateRequirement: %v", err)
	}

	_, err := s.EndDateRequirement(
		svcmiddleware.WithTenant(context.Background(), intruder),
		intruder, r.EvidenceRequirementID, time.Now(), "not mine", "intruder")
	if !errors.Is(err, domain.ErrRequirementNotFound) {
		t.Fatalf("cross-tenant retire err = %v, want ErrRequirementNotFound", err)
	}

	still, err := s.GetRequirement(svcmiddleware.WithTenant(context.Background(), owner), r.EvidenceRequirementID)
	if err != nil || still == nil {
		t.Fatalf("GetRequirement: got=%v err=%v", still, err)
	}
	if still.EffectiveTo != nil {
		t.Error("another tenant retired this requirement")
	}
}

// EffectiveRequirements is the read the evaluator depends on: entity-scoped rows
// PLUS tenant-wide (NULL legal_entity_id) rows, and only those in force at asOf.
// A retired requirement that keeps matching would block an action forever; one
// that stops matching too early would let an unevidenced action through.
func TestPgStore_EffectiveRequirements_ScopeAndPointInTime(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entity := uuid.New().String()
	otherEntity := uuid.New().String()

	entityScoped := newTestRequirement(tenantID, &entity)
	entityScoped.EvidenceType = "SUPPORTING_DOCUMENT"
	if _, err := s.CreateRequirement(ctx, entityScoped); err != nil {
		t.Fatalf("create entity-scoped: %v", err)
	}

	tenantWide := newTestRequirement(tenantID, nil)
	tenantWide.EvidenceType = "ATTESTATION"
	if _, err := s.CreateRequirement(ctx, tenantWide); err != nil {
		t.Fatalf("create tenant-wide: %v", err)
	}

	foreignEntity := newTestRequirement(tenantID, &otherEntity)
	foreignEntity.EvidenceType = "APPROVAL_RECORD"
	if _, err := s.CreateRequirement(ctx, foreignEntity); err != nil {
		t.Fatalf("create other-entity: %v", err)
	}

	now := time.Now().UTC()
	eff, err := s.EffectiveRequirements(ctx, tenantID, entity, "PROCUREMENT", "PURCHASE_ORDER_ISSUE", now)
	if err != nil {
		t.Fatalf("EffectiveRequirements: %v", err)
	}
	if len(eff) != 2 {
		t.Fatalf("got %d effective requirements, want 2 (this entity + tenant-wide)", len(eff))
	}
	seen := map[string]bool{}
	for _, r := range eff {
		seen[r.EvidenceType] = true
	}
	if !seen["SUPPORTING_DOCUMENT"] || !seen["ATTESTATION"] {
		t.Errorf("effective set = %v, want the entity-scoped and tenant-wide rows", seen)
	}
	if seen["APPROVAL_RECORD"] {
		t.Error("another legal entity's requirement was applied to this one")
	}

	// Before effective_from, nothing is in force yet.
	past, err := s.EffectiveRequirements(ctx, tenantID, entity, "PROCUREMENT", "PURCHASE_ORDER_ISSUE", now.Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("EffectiveRequirements in the past: %v", err)
	}
	if len(past) != 0 {
		t.Errorf("got %d requirements before effective_from, want 0", len(past))
	}

	// Retire the tenant-wide one and confirm it drops out at a later asOf while
	// remaining in force at an instant before its end date.
	retireAt := now.Add(time.Minute)
	if _, err := s.EndDateRequirement(ctx, tenantID, tenantWide.EvidenceRequirementID, retireAt, "superseded", "retirer"); err != nil {
		t.Fatalf("EndDateRequirement: %v", err)
	}

	after, err := s.EffectiveRequirements(ctx, tenantID, entity, "PROCUREMENT", "PURCHASE_ORDER_ISSUE", retireAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("EffectiveRequirements after retirement: %v", err)
	}
	if len(after) != 1 || after[0].EvidenceType != "SUPPORTING_DOCUMENT" {
		t.Fatalf("after retirement got %d requirements (%v), want only the entity-scoped one",
			len(after), after)
	}

	before, err := s.EffectiveRequirements(ctx, tenantID, entity, "PROCUREMENT", "PURCHASE_ORDER_ISSUE", now)
	if err != nil {
		t.Fatalf("EffectiveRequirements before the end date: %v", err)
	}
	if len(before) != 2 {
		t.Errorf("at an instant before effective_to got %d requirements, want 2 — a "+
			"retirement must not rewrite history", len(before))
	}
}

// ListRequirements with a zero AsOf includes retired rows, which is what an
// auditor reviewing history needs; with an AsOf it narrows to what was in force.
func TestPgStore_ListRequirements_ZeroAsOfIncludesRetired(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	live := newTestRequirement(tenantID, nil)
	if _, err := s.CreateRequirement(ctx, live); err != nil {
		t.Fatalf("create live: %v", err)
	}
	gone := newTestRequirement(tenantID, nil)
	gone.EvidenceType = "ATTESTATION"
	if _, err := s.CreateRequirement(ctx, gone); err != nil {
		t.Fatalf("create to-retire: %v", err)
	}
	retireAt := time.Now().UTC()
	if _, err := s.EndDateRequirement(ctx, tenantID, gone.EvidenceRequirementID, retireAt, "superseded", "retirer"); err != nil {
		t.Fatalf("EndDateRequirement: %v", err)
	}

	all, err := s.ListRequirements(ctx, domain.ListRequirementsFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("ListRequirements: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("zero AsOf returned %d rows, want 2 including the retired one — an "+
			"auditor reviewing history needs to see it", len(all))
	}

	current, err := s.ListRequirements(ctx, domain.ListRequirementsFilter{
		TenantID: tenantID, AsOf: retireAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("ListRequirements with AsOf: %v", err)
	}
	if len(current) != 1 || current[0].EvidenceRequirementID != live.EvidenceRequirementID {
		t.Fatalf("AsOf filter returned %d rows, want only the live one", len(current))
	}

	foreign, err := s.ListRequirements(ctx, domain.ListRequirementsFilter{TenantID: uuid.New().String()})
	if err != nil {
		t.Fatalf("ListRequirements for a foreign tenant: %v", err)
	}
	if len(foreign) != 0 {
		t.Fatalf("a foreign tenant read %d requirements", len(foreign))
	}
}

// The determination is itself evidence, so it must be retrievable afterwards and
// frozen: NO_REQUIREMENTS_DEFINED in particular must round-trip intact, because
// rendering it as SATISFIED would report an unchecked action as an approved one.
func TestPgStore_RecordEvaluation_RoundTripAndIdempotency(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	e := &domain.EvidenceEvaluation{
		EvaluationID:            uuid.New().String(),
		TenantID:                tenantID,
		LegalEntityID:           uuid.New().String(),
		DomainCode:              "PROCUREMENT",
		ActionType:              "PURCHASE_ORDER_ISSUE",
		Outcome:                 domain.OutcomeNoRequirementsDefined,
		UnmetPayload:            json.RawMessage(`[]`),
		PresentArtifactsPayload: json.RawMessage(`[{"evidence_type":"SUPPORTING_DOCUMENT"}]`),
		EvaluatedForPrincipalID: "buyer-1",
		CorrelationID:           "corr-" + uuid.New().String(),
	}

	created, err := s.RecordEvaluation(ctx, e)
	if err != nil {
		t.Fatalf("RecordEvaluation: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for a first evaluation")
	}

	got, err := s.GetEvaluation(ctx, e.EvaluationID)
	if err != nil || got == nil {
		t.Fatalf("GetEvaluation: got=%v err=%v", got, err)
	}
	if got.Outcome != domain.OutcomeNoRequirementsDefined {
		t.Errorf("outcome = %q, want NO_REQUIREMENTS_DEFINED — it is explicitly not "+
			"SATISFIED, and collapsing the two reports an unchecked action as approved",
			got.Outcome)
	}
	// Payloads are frozen at decision time so the record stays truthful after
	// the catalog changes underneath it.
	var present []map[string]any
	if err := json.Unmarshal(got.PresentArtifactsPayload, &present); err != nil {
		t.Fatalf("present_artifacts_payload did not survive the roundtrip: %v", err)
	}
	if len(present) != 1 {
		t.Errorf("present artifacts = %v, want the one recorded at decision time", present)
	}

	replay := *e
	replay.EvaluationID = uuid.New().String()
	replay.Outcome = domain.OutcomeMissing
	created, err = s.RecordEvaluation(ctx, &replay)
	if err != nil {
		t.Fatalf("replayed RecordEvaluation: %v", err)
	}
	if created {
		t.Fatal("expected created=false on a replayed correlation_id")
	}
	if replay.Outcome != domain.OutcomeNoRequirementsDefined {
		t.Errorf("replay returned outcome %q, want the ORIGINAL determination — a replay "+
			"must not restate the decision", replay.Outcome)
	}
	if replay.EvaluationID != e.EvaluationID {
		t.Errorf("replay resolved to %s, want the original %s", replay.EvaluationID, e.EvaluationID)
	}
}

func TestPgStore_GetEvaluation_OtherTenant_ReadsAsAbsent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	owner := uuid.New().String()

	e := &domain.EvidenceEvaluation{
		EvaluationID:            uuid.New().String(),
		TenantID:                owner,
		LegalEntityID:           uuid.New().String(),
		DomainCode:              "PROCUREMENT",
		ActionType:              "PURCHASE_ORDER_ISSUE",
		Outcome:                 domain.OutcomeSatisfied,
		UnmetPayload:            json.RawMessage(`[]`),
		PresentArtifactsPayload: json.RawMessage(`[]`),
		EvaluatedForPrincipalID: "buyer-1",
		CorrelationID:           "corr-" + uuid.New().String(),
	}
	if _, err := s.RecordEvaluation(svcmiddleware.WithTenant(context.Background(), owner), e); err != nil {
		t.Fatalf("RecordEvaluation: %v", err)
	}

	intruder := svcmiddleware.WithTenant(context.Background(), uuid.New().String())
	got, err := s.GetEvaluation(intruder, e.EvaluationID)
	if err != nil {
		t.Fatalf("cross-tenant GetEvaluation returned an error: %v", err)
	}
	if got != nil {
		t.Fatal("another tenant's evaluation was readable — a determination discloses " +
			"what was missing for an action in that tenant")
	}
}
