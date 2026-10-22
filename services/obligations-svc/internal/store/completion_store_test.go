package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/store"
)

// Every test here runs the store as the runtime-shaped role (NOBYPASSRLS, not
// the owner): the defects this pass fixed were invisible to a superuser.

func pastDue(code string) domain.CreateObligationParams {
	p := validParams()
	p.ObligationCode = code
	p.DueDate = time.Now().UTC().Add(-48 * time.Hour)
	return p
}

func TestPgStore_OverdueSweepFindsTenantsUnderRLS(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())

	if _, _, err := s.CreateObligation(tenantCtx(testTenantID), pastDue("DUE-A")); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, _, err := s.CreateObligation(tenantCtx(otherTenantID), pastDue("DUE-B")); err != nil {
		t.Fatalf("create B: %v", err)
	}
	future := validParams()
	future.ObligationCode = "NOT-DUE"
	future.DueDate = time.Now().UTC().Add(72 * time.Hour)
	if _, _, err := s.CreateObligation(tenantCtx(testTenantID), future); err != nil {
		t.Fatalf("create future: %v", err)
	}

	tenants, err := s.GetTenantIDsWithObligations(context.Background())
	if err != nil {
		t.Fatalf("tenants: %v", err)
	}
	if len(tenants) != 2 {
		t.Fatalf("the sweep must see both tenants with due obligations under RLS, got %v", tenants)
	}

	overdue, err := s.FindOverdueObligations(tenantCtx(testTenantID), testTenantID)
	if err != nil || len(overdue) != 1 {
		t.Fatalf("tenant A overdue = %d, err %v; want 1", len(overdue), err)
	}
	updated, moved, err := s.TransitionOverdue(tenantCtx(testTenantID), overdue[0].ObligationID)
	if err != nil || !moved || updated.ObligationStatus != "OVERDUE" {
		t.Fatalf("transition: moved=%v status=%v err=%v", moved, updated, err)
	}

	// The sweep policy is SELECT-only: it never lets an unscoped write through.
	conn, err := admin.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, stmt := range []string{`SET ROLE obligations_rls_test`, `SELECT set_config('app.overdue_sweep', 'true', false)`} {
		if _, err := conn.Exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	tag, err := conn.Exec(context.Background(), `UPDATE obligations SET severity_level = 'X'`)
	_, _ = conn.Exec(context.Background(), `RESET ROLE`)
	_, _ = conn.Exec(context.Background(), `RESET app.overdue_sweep`)
	if err == nil && tag.RowsAffected() != 0 {
		t.Fatalf("an unscoped UPDATE under the sweep flag changed %d rows", tag.RowsAffected())
	}
}

func TestPgStore_OutboxRetriesWithBackoffInsteadOfParking(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())

	if _, _, err := s.CreateObligation(tenantCtx(testTenantID), validParams()); err != nil {
		t.Fatalf("create: %v", err)
	}
	failing := func(context.Context, store.OutboxEvent) error { return errors.New("kafka down") }

	// The old rule parked an event after five failures (~2.5 s of outage).
	for i := 0; i < 10; i++ {
		if _, err := admin.Exec(context.Background(), `UPDATE event_outbox SET next_attempt_at = NOW() - INTERVAL '1 second'`); err != nil {
			t.Fatal(err)
		}
		if _, failed, err := s.RelayOutbox(context.Background(), 10, failing); err != nil || failed != 1 {
			t.Fatalf("relay %d: failed=%d err=%v", i, failed, err)
		}
	}
	var status string
	var attempts int
	var nextIn float64
	if err := admin.QueryRow(context.Background(),
		`SELECT status, attempts, EXTRACT(EPOCH FROM next_attempt_at - NOW()) FROM event_outbox`).Scan(&status, &attempts, &nextIn); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING" || attempts != 10 {
		t.Fatalf("after 10 failures: status=%s attempts=%d; a short outage must not park the event", status, attempts)
	}
	if nextIn < 60 {
		t.Fatalf("next attempt in %.0fs; backoff must grow (2^10 s, capped at 300)", nextIn)
	}

	// Not due yet: a relay pass leaves it alone.
	if pub, failed, err := s.RelayOutbox(context.Background(), 10, failing); err != nil || pub+failed != 0 {
		t.Fatalf("a not-yet-due event was claimed: pub=%d failed=%d err=%v", pub, failed, err)
	}

	st, err := s.GetOutboxStats(context.Background())
	if err != nil || st.Pending != 1 || st.Failed != 0 {
		t.Fatalf("stats = %+v err=%v", st, err)
	}

	// Broker back: delivered once, marked published.
	if _, err := admin.Exec(context.Background(), `UPDATE event_outbox SET next_attempt_at = NOW()`); err != nil {
		t.Fatal(err)
	}
	var delivered []store.OutboxEvent
	pub, _, err := s.RelayOutbox(context.Background(), 10, func(_ context.Context, e store.OutboxEvent) error {
		delivered = append(delivered, e)
		return nil
	})
	if err != nil || pub != 1 || len(delivered) != 1 || delivered[0].TenantID != testTenantID {
		t.Fatalf("delivery: pub=%d delivered=%v err=%v", pub, delivered, err)
	}
	if err := admin.QueryRow(context.Background(), `SELECT status FROM event_outbox`).Scan(&status); err != nil || status != "PUBLISHED" {
		t.Fatalf("status after delivery = %s (%v)", status, err)
	}
}

func TestPgStore_ConcurrentRelaysPublishEachEventOnce(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())

	for i := 0; i < 20; i++ {
		p := validParams()
		p.ObligationCode = "OBL-RELAY-" + string(rune('A'+i))
		if _, _, err := s.CreateObligation(tenantCtx(testTenantID), p); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	publish := func(_ context.Context, e store.OutboxEvent) error {
		time.Sleep(5 * time.Millisecond) // hold the claim while the other relay runs
		mu.Lock()
		seen[e.OutboxID]++
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				_, _, _ = s.RelayOutbox(context.Background(), 4, publish)
			}
		}()
	}
	wg.Wait()
	if len(seen) != 20 {
		t.Fatalf("published %d distinct events, want 20", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("event %s published %d times by concurrent relays", id, n)
		}
	}
}

func TestPgStore_ApplicabilityIdempotencyKey(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)

	o, _, err := s.CreateObligation(ctx, validParams())
	if err != nil {
		t.Fatal(err)
	}
	params := domain.CreateApplicabilityDecisionParams{
		ObligationID: o.ObligationID, JurisdictionCode: "IN-KA", EntityRef: "le-1", Decision: "APPLICABLE",
		SourceRuleRef: "R-7", SourceRuleVersion: "v3", EffectiveFrom: time.Now().UTC(),
		DecidedBySystem: "rule-engine", CreatedByPrincipalID: "admin-1",
		IdempotencyKey: "key-1", RequestHash: "hash-1",
	}
	first, replayed, err := s.CreateApplicabilityDecision(ctx, params)
	if err != nil || replayed {
		t.Fatalf("first: replayed=%v err=%v", replayed, err)
	}
	second, replayed, err := s.CreateApplicabilityDecision(ctx, params)
	if err != nil || !replayed || second.ApplicabilityDecisionID != first.ApplicabilityDecisionID {
		t.Fatalf("retry must replay the recorded decision: replayed=%v id=%v err=%v", replayed, second, err)
	}
	changed := params
	changed.RequestHash = "hash-2"
	if _, _, err := s.CreateApplicabilityDecision(ctx, changed); !errors.Is(err, domain.ErrIdempotencyKeyConflict) {
		t.Fatalf("same key, different body: err=%v, want ErrIdempotencyKeyConflict", err)
	}
	list, err := s.ListApplicabilityDecisions(ctx, o.ObligationID, "IN-KA", "le-1", 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("history has %d decisions (err %v); a retry must not append a second", len(list), err)
	}

	// The same key is a different key in another tenant.
	other, _, err := s.CreateObligation(tenantCtx(otherTenantID), validParams())
	if err != nil {
		t.Fatal(err)
	}
	params.ObligationID = other.ObligationID
	if _, replayed, err := s.CreateApplicabilityDecision(tenantCtx(otherTenantID), params); err != nil || replayed {
		t.Fatalf("other tenant, same key: replayed=%v err=%v", replayed, err)
	}

	// A value the CHECK refuses is the caller's error, not an outage.
	bad := params
	bad.IdempotencyKey, bad.Decision = "", "MAYBE"
	if _, _, err := s.CreateApplicabilityDecision(tenantCtx(otherTenantID), bad); !errors.Is(err, domain.ErrInvalidField) {
		t.Fatalf("invalid decision value: err=%v, want ErrInvalidField", err)
	}
}

func TestPgStore_CallerSuppliedObligationIDCollisionIsAConflict(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())

	p := validParams()
	p.ObligationID = "dddddddd-0000-0000-0000-000000000001"
	if _, _, err := s.CreateObligation(tenantCtx(testTenantID), p); err != nil {
		t.Fatal(err)
	}
	q := validParams()
	q.ObligationID = p.ObligationID
	q.ObligationCode = "OTHER-CODE"
	if _, _, err := s.CreateObligation(tenantCtx(otherTenantID), q); !errors.Is(err, domain.ErrObligationIDConflict) {
		t.Fatalf("id clash: err=%v, want ErrObligationIDConflict (was 503 store_unavailable)", err)
	}
}

func TestPgStore_FilingLifecycleAndHistory(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)

	o, _, err := s.CreateObligation(ctx, validParams())
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFilingRequirement(ctx, domain.CreateFilingRequirementParams{
		ObligationID: o.ObligationID, FilingType: "GSTR-3B", FilingAuthority: "GSTN", SubmissionChannel: "PORTAL"})
	if err != nil {
		t.Fatal(err)
	}
	move := func(to string, version int) (*domain.FilingRequirement, bool, error) {
		return s.UpdateFilingStatus(ctx, domain.UpdateFilingStatusParams{ObligationID: o.ObligationID,
			FilingRequirementID: f.FilingRequirementID, NewStatus: to, ExpectedVersion: version,
			ActorPrincipalID: "admin-1", CorrelationID: "corr-f", Reason: "r"})
	}
	if _, _, err := move("ACCEPTED", 0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("PENDING → ACCEPTED skips submission: err=%v", err)
	}
	if _, _, err := move("SUBMITTED", 9); !errors.Is(err, domain.ErrVersionMismatch) {
		t.Fatalf("stale If-Match: err=%v", err)
	}
	got, moved, err := move("SUBMITTED", 1)
	if err != nil || !moved || got.FilingStatus != "SUBMITTED" || got.RowVersion != 2 {
		t.Fatalf("submit: %+v moved=%v err=%v", got, moved, err)
	}
	if _, moved, err := move("SUBMITTED", 0); err != nil || moved {
		t.Fatalf("repeat is a no-op: moved=%v err=%v", moved, err)
	}
	if _, _, err := move("ACCEPTED", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := move("REJECTED", 0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("ACCEPTED is terminal: err=%v", err)
	}
	hist, err := s.ListFilingTransitions(ctx, o.ObligationID, f.FilingRequirementID)
	if err != nil || len(hist) != 2 || hist[0].ToStatus != "SUBMITTED" || hist[1].ToStatus != "ACCEPTED" {
		t.Fatalf("history = %+v err=%v", hist, err)
	}
	// Append-only even for the table owner.
	if _, err := admin.Exec(context.Background(), `UPDATE filing_requirement_transitions SET to_status = 'X'`); err == nil {
		t.Fatal("filing history was rewritable")
	}
	var updated int
	if err := admin.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM event_outbox WHERE event_type = 'obligation.updated' AND event_payload->'payload'->>'filing_requirement_id' = $1`,
		f.FilingRequirementID).Scan(&updated); err != nil || updated != 2 {
		t.Fatalf("filing moves wrote %d obligation.updated events (err %v), want 2", updated, err)
	}
}

func TestPgStore_UpdateObligationAttributes(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)

	o, _, err := s.CreateObligation(ctx, validParams())
	if err != nil {
		t.Fatal(err)
	}
	newDue := o.DueDate.Add(30 * 24 * time.Hour)
	sev := "CRITICAL"
	if _, _, err := s.UpdateObligation(ctx, domain.UpdateObligationParams{ObligationID: o.ObligationID,
		ExpectedVersion: o.Version + 1, DueDate: &newDue}); !errors.Is(err, domain.ErrVersionMismatch) {
		t.Fatalf("stale version: err=%v", err)
	}
	got, changed, err := s.UpdateObligation(ctx, domain.UpdateObligationParams{ObligationID: o.ObligationID,
		ExpectedVersion: o.Version, DueDate: &newDue, SeverityLevel: &sev, ActorPrincipalID: "admin-1", CorrelationID: "corr-u"})
	if err != nil || !changed || !got.DueDate.Equal(newDue) || got.SeverityLevel != sev || got.Version != o.Version+1 {
		t.Fatalf("update: %+v changed=%v err=%v", got, changed, err)
	}
	var payload []byte
	if err := admin.QueryRow(context.Background(),
		`SELECT event_payload FROM event_outbox WHERE event_type = 'obligation.updated' ORDER BY created_at DESC LIMIT 1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var env struct {
		ActorID string `json:"actor_id"`
		Payload struct {
			ChangedFields []string `json:"changed_fields"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(payload, &env)
	if env.ActorID != "admin-1" || len(env.Payload.ChangedFields) != 2 {
		t.Fatalf("obligation.updated = %+v; want the actor and both changed fields", env)
	}
	if _, _, err := s.UpdateObligationStatus(ctx, o.ObligationID, "CLOSED", 0, "admin-1", "c"); err != nil {
		t.Fatal(err)
	}
	closed, _ := s.FindObligationByID(ctx, o.ObligationID)
	if _, _, err := s.UpdateObligation(ctx, domain.UpdateObligationParams{ObligationID: o.ObligationID,
		ExpectedVersion: closed.Version, SeverityLevel: &sev}); !errors.Is(err, domain.ErrObligationClosed) {
		t.Fatalf("a CLOSED obligation is final: err=%v", err)
	}
}

func TestPgStore_DefinitionsMappingsAndPinnedObligations(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)

	content := domain.DefinitionContent{JurisdictionID: "00000000-0000-0000-0000-000000000002",
		ObligationType: "FILING", ObligationSourceType: "JURISDICTION_RULE", ObligationSourceID: "rule-1",
		SourceReference: "IN-GST-FILING-RULE-07", SeverityLevel: "HIGH", ResponsibleFunction: "Tax"}
	d, created, err := s.CreateDefinition(ctx, domain.CreateDefinitionParams{DefinitionCode: "GSTR-3B-MONTHLY",
		Content: content, CreatedByPrincipalID: "admin-1"})
	if err != nil || !created || d.CurrentVersion != 1 {
		t.Fatalf("create definition: %+v created=%v err=%v", d, created, err)
	}
	if _, created, err := s.CreateDefinition(ctx, domain.CreateDefinitionParams{DefinitionCode: "GSTR-3B-MONTHLY",
		Content: content, CreatedByPrincipalID: "admin-1"}); err != nil || created {
		t.Fatalf("same code + content replays: created=%v err=%v", created, err)
	}
	changed := content
	changed.SeverityLevel = "LOW"
	if _, _, err := s.CreateDefinition(ctx, domain.CreateDefinitionParams{DefinitionCode: "GSTR-3B-MONTHLY",
		Content: changed, CreatedByPrincipalID: "admin-1"}); !errors.Is(err, domain.ErrDefinitionCodeConflict) {
		t.Fatalf("same code, different content: err=%v", err)
	}

	entity := validParams().LegalEntityID
	raise := func(code string, version int) (*domain.Obligation, error) {
		p := validParams()
		p.ObligationCode = code
		p.ObligationDefinitionID = d.ObligationDefinitionID
		p.DefinitionVersion = version
		o, _, err := s.CreateObligation(ctx, p)
		return o, err
	}
	if _, err := raise("GST-2026-10", 1); !errors.Is(err, domain.ErrEntityNotMapped) {
		t.Fatalf("an entity the definition does not bind: err=%v", err)
	}
	m, err := s.CreateMapping(ctx, domain.CreateMappingParams{ObligationDefinitionID: d.ObligationDefinitionID,
		LegalEntityID: entity, EffectiveFrom: time.Now().UTC().AddDate(-1, 0, 0), ActorPrincipalID: "admin-1"})
	if err != nil {
		t.Fatalf("mapping: %v", err)
	}
	if _, err := s.CreateMapping(ctx, domain.CreateMappingParams{ObligationDefinitionID: d.ObligationDefinitionID,
		LegalEntityID: entity, EffectiveFrom: time.Now().UTC(), ActorPrincipalID: "admin-1"}); !errors.Is(err, domain.ErrMappingExists) {
		t.Fatalf("second open mapping: err=%v", err)
	}
	o1, err := raise("GST-2026-10", 1)
	if err != nil || o1.DefinitionVersion == nil || *o1.DefinitionVersion != 1 {
		t.Fatalf("raise v1: %+v err=%v", o1, err)
	}

	v2, err := s.NewDefinitionVersion(ctx, domain.NewDefinitionVersionParams{ObligationDefinitionID: d.ObligationDefinitionID,
		ExpectedVersion: 1, Content: changed, ChangeReason: "severity review", ActorPrincipalID: "admin-1"})
	if err != nil || v2.CurrentVersion != 2 || v2.Current.SeverityLevel != "LOW" {
		t.Fatalf("version 2: %+v err=%v", v2, err)
	}
	if _, err := s.NewDefinitionVersion(ctx, domain.NewDefinitionVersionParams{ObligationDefinitionID: d.ObligationDefinitionID,
		ExpectedVersion: 1, Content: content, ChangeReason: "stale", ActorPrincipalID: "admin-1"}); !errors.Is(err, domain.ErrVersionMismatch) {
		t.Fatalf("stale supersede: err=%v", err)
	}
	if _, err := raise("GST-2026-11", 1); !errors.Is(err, domain.ErrVersionMismatch) {
		t.Fatalf("pinning a superseded version: err=%v", err)
	}
	still, _ := s.FindObligationByID(ctx, o1.ObligationID)
	if *still.DefinitionVersion != 1 {
		t.Fatalf("an obligation stays pinned to the version it was raised from, got %d", *still.DefinitionVersion)
	}
	versions, err := s.ListDefinitionVersions(ctx, d.ObligationDefinitionID)
	if err != nil || len(versions) != 2 {
		t.Fatalf("versions = %d err=%v", len(versions), err)
	}
	if _, err := admin.Exec(context.Background(), `UPDATE obligation_definition_versions SET severity_level = 'X'`); err == nil {
		t.Fatal("a released definition version was rewritable")
	}

	if _, err := s.EndMapping(ctx, d.ObligationDefinitionID, m.MappingID, time.Now().UTC(), "admin-1", "c"); err != nil {
		t.Fatalf("end mapping: %v", err)
	}
	if _, err := raise("GST-2026-12", 0); !errors.Is(err, domain.ErrEntityNotMapped) {
		t.Fatalf("after the mapping ended: err=%v", err)
	}
	if _, err := s.RetireDefinition(ctx, d.ObligationDefinitionID, 0, "admin-1", "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := raise("GST-2027-01", 0); !errors.Is(err, domain.ErrDefinitionRetired) {
		t.Fatalf("a retired definition raises nothing: err=%v", err)
	}

	// Another tenant cannot see the definition.
	if _, err := s.GetDefinition(tenantCtx(otherTenantID), d.ObligationDefinitionID); !errors.Is(err, domain.ErrDefinitionNotFound) {
		t.Fatalf("cross-tenant definition read: err=%v", err)
	}
	var defEvents int
	_ = admin.QueryRow(context.Background(), `SELECT COUNT(*) FROM event_outbox WHERE event_type LIKE 'obligation.definition.%' OR event_type LIKE 'obligation.entity_mapping.%'`).Scan(&defEvents)
	if defEvents != 5 {
		t.Fatalf("catalogue changes wrote %d events, want 5 (created, versioned, retired, mapping created, ended)", defEvents)
	}
}

func TestPgStore_ListObligationsCursorPages(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)

	for i := 0; i < 5; i++ {
		p := validParams()
		p.ObligationCode = "PAGE-" + string(rune('A'+i))
		if _, _, err := s.CreateObligation(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	f := domain.ListObligationsFilter{Limit: 2}
	for page := 0; page < 4; page++ {
		got, err := s.ListObligations(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range got {
			if seen[o.ObligationID] {
				t.Fatalf("obligation %s returned on two pages", o.ObligationID)
			}
			seen[o.ObligationID] = true
		}
		if len(got) < 2 {
			break
		}
		last := got[len(got)-1]
		at := last.CreatedAt
		f.AfterCreatedAt, f.AfterID = &at, last.ObligationID
	}
	if len(seen) != 5 {
		t.Fatalf("cursor pages returned %d of 5 obligations", len(seen))
	}
}
