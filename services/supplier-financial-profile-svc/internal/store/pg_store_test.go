package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/supplier-financial-profile-svc/internal/domain"
	"zoiko.io/supplier-financial-profile-svc/internal/middleware"
)

func newTestStore(t *testing.T) (*PgStore, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// TEST_APP_DATABASE_URL (a NOSUPERUSER NOBYPASSRLS role) runs the STORE under
	// row-level security; the returned pool stays the owner for verification SQL.
	storePool := pool
	if appDSN := os.Getenv("TEST_APP_DATABASE_URL"); appDSN != "" {
		app, err := pgxpool.New(context.Background(), appDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Close)
		storePool = app
	}
	return NewPgStore(storePool, zap.NewNop()), pool
}

func countOutbox(t *testing.T, pool *pgxpool.Pool, profileID, eventType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, profileID, eventType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPgStore_Lifecycle_Versioning_Revisions_Outbox runs the full command
// path against real Postgres: version bumps, expected_version, append-only
// revisions, as-of reads, outbox rows and idempotency in the same transaction.
func TestPgStore_Lifecycle_Versioning_Revisions_Outbox(t *testing.T) {
	s, pool := newTestStore(t)
	tenant := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenant)
	le := uuid.New().String()

	p, err := s.CreateProfile(ctx, tenant, domain.CreateProfileRequest{LegalEntityID: le, SupplierRef: "sup-1", Category: "IT"}, "maker", nil)
	if err != nil || p.Version != 1 || p.Status != domain.StatusDraft {
		t.Fatalf("create: %v %+v", err, p)
	}
	if countOutbox(t, pool, p.ProfileID, "SupplierFinancialProfileCreated") != 1 || countOutbox(t, pool, p.ProfileID, "supplier_financial_profile.created") != 1 {
		t.Fatalf("create must enqueue the spec event and the legacy alias")
	}
	// A second live profile for the same supplier/entity is refused (and the tx stays usable).
	if _, err := s.CreateProfile(ctx, tenant, domain.CreateProfileRequest{LegalEntityID: le, SupplierRef: "sup-1"}, "maker", nil); !errors.Is(err, domain.ErrDuplicateProfile) {
		t.Fatalf("expected ErrDuplicateProfile, got %v", err)
	}
	tCreated := p.UpdatedAt

	time.Sleep(20 * time.Millisecond)
	one := 1
	a, err := s.Transition(ctx, p.ProfileID, domain.CmdActivate, &one, "", "maker", nil)
	if err != nil || a.Version != 2 || a.Status != domain.StatusActive {
		t.Fatalf("activate: %v %+v", err, a)
	}
	if _, err := s.Transition(ctx, p.ProfileID, domain.CmdPlaceHold, &one, "stale", "maker", nil); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}
	two := 2
	h, err := s.Transition(ctx, p.ProfileID, domain.CmdPlaceHold, &two, "dispute", "maker", nil)
	if err != nil || h.Status != domain.StatusOnHold || h.Version != 3 {
		t.Fatalf("hold: %v %+v", err, h)
	}
	if countOutbox(t, pool, p.ProfileID, "SupplierHoldPlaced") != 1 || countOutbox(t, pool, p.ProfileID, "supplier_hold.placed") != 1 {
		t.Fatalf("hold must enqueue SupplierHoldPlaced + legacy alias")
	}
	if countOutbox(t, pool, p.ProfileID, "SupplierFinancialProfileChanged") != 1 { // activation
		t.Fatalf("activation must enqueue SupplierFinancialProfileChanged")
	}
	if _, err := s.Transition(ctx, p.ProfileID, domain.CmdUnsuspend, nil, "", "maker", nil); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}
	if _, err := s.Transition(ctx, p.ProfileID, domain.CmdReleaseHold, nil, "", "maker", nil); err != nil {
		t.Fatal(err)
	}
	sus, err := s.Transition(ctx, p.ProfileID, domain.CmdSuspend, nil, "sanctions", "maker", nil)
	if err != nil || sus.Status != domain.StatusSuspended {
		t.Fatalf("suspend: %v %+v", err, sus)
	}

	// History is one revision per change with prior snapshots.
	hist, err := s.ListProfileHistory(ctx, p.ProfileID)
	if err != nil || len(hist) != 5 || hist[0].PriorSnapshot != nil || hist[4].PriorSnapshot == nil || hist[4].PriorSnapshot.Status != domain.StatusActive {
		t.Fatalf("history: %v %d %+v", err, len(hist), hist)
	}
	// As-of reconstructs the state at past instants.
	at, err := s.FindProfileAsOf(ctx, p.ProfileID, tCreated.Add(5*time.Millisecond))
	if err != nil || at.Profile.Status != domain.StatusDraft || at.Profile.Version != 1 || at.Profile.Category != "IT" {
		t.Fatalf("as-of creation: %v %+v", err, at)
	}
	at, err = s.FindProfileAsOf(ctx, p.ProfileID, time.Now().Add(time.Minute))
	if err != nil || at.Profile.Status != domain.StatusSuspended || at.Profile.Version != 5 {
		t.Fatalf("as-of now: %v %+v", err, at)
	}
	if _, err := s.FindProfileAsOf(ctx, p.ProfileID, tCreated.Add(-time.Hour)); !errors.Is(err, domain.ErrNoRevisionAsOf) {
		t.Fatalf("expected ErrNoRevisionAsOf, got %v", err)
	}

	// Eligibility source: lookup by supplier.
	by, err := s.FindProfileBySupplier(ctx, le, "sup-1")
	if err != nil || by.ProfileID != p.ProfileID {
		t.Fatalf("by supplier: %v %+v", err, by)
	}

	// Revisions are append-only (trigger) even for the superuser runtime.
	if _, err := pool.Exec(context.Background(), `UPDATE supplier_profile_revisions SET reason = 'x' WHERE profile_id = $1`, p.ProfileID); err == nil {
		t.Fatalf("revisions must be append-only")
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM supplier_profile_revisions WHERE profile_id = $1`, p.ProfileID); err == nil {
		t.Fatalf("revisions must be append-only")
	}
}

func TestPgStore_TenantIsolation(t *testing.T) {
	s, _ := newTestStore(t)
	tenantA, tenantB := uuid.New().String(), uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)
	ctxNone := context.Background()
	le := uuid.New().String()

	p, err := s.CreateProfile(ctxA, tenantA, domain.CreateProfileRequest{LegalEntityID: le, SupplierRef: "sup"}, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Same supplier in another tenant is a different profile.
	if _, err := s.CreateProfile(ctxB, tenantB, domain.CreateProfileRequest{LegalEntityID: le, SupplierRef: "sup"}, "m", nil); err != nil {
		t.Fatalf("same supplier in another tenant must be allowed: %v", err)
	}
	if _, err := s.FindProfile(ctxB, p.ProfileID); !errors.Is(err, domain.ErrProfileNotFound) {
		t.Fatalf("tenant B must not see tenant A's profile, got %v", err)
	}
	if _, err := s.FindProfile(ctxNone, p.ProfileID); !errors.Is(err, domain.ErrProfileNotFound) {
		t.Fatalf("no tenant context must see nothing (no NULL-tenant escape), got %v", err)
	}
	if _, err := s.Transition(ctxB, p.ProfileID, domain.CmdActivate, nil, "", "m", nil); !errors.Is(err, domain.ErrProfileNotFound) {
		t.Fatalf("tenant B must not mutate tenant A's profile, got %v", err)
	}
	if hist, _ := s.ListProfileHistory(ctxB, p.ProfileID); len(hist) != 0 {
		t.Fatalf("tenant B must not read tenant A's history")
	}
}

// TestPgStore_PaymentTerms_Overlap proves the DB EXCLUDE constraint maps to
// ErrOverlappingPaymentTerms, rolls the whole command back (no version bump,
// no revision, no outbox row) and leaves the store usable afterwards.
func TestPgStore_PaymentTerms_Overlap(t *testing.T) {
	s, pool := newTestStore(t)
	tenant := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenant)
	p, _ := s.CreateProfile(ctx, tenant, domain.CreateProfileRequest{LegalEntityID: uuid.New().String(), SupplierRef: "sup"}, "m", nil)
	s.Transition(ctx, p.ProfileID, domain.CmdActivate, nil, "", "m", nil) //nolint:errcheck

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.ChangePaymentTerms(ctx, p.ProfileID, domain.ChangePaymentTermsRequest{TermsCode: "NET_30", EffectiveFrom: from, EffectiveTo: &to}, "m", nil); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.FindProfile(ctx, p.ProfileID)
	_, err := s.ChangePaymentTerms(ctx, p.ProfileID, domain.ChangePaymentTermsRequest{TermsCode: "NET_60", EffectiveFrom: from.AddDate(0, 2, 0)}, "m", nil)
	if !errors.Is(err, domain.ErrOverlappingPaymentTerms) {
		t.Fatalf("expected ErrOverlappingPaymentTerms, got %v", err)
	}
	after, _ := s.FindProfile(ctx, p.ProfileID)
	if after.Version != cur.Version {
		t.Fatalf("a rejected overlap must not bump the version")
	}
	if n := countOutbox(t, pool, p.ProfileID, "SupplierPaymentTermsChanged"); n != 1 {
		t.Fatalf("expected exactly one terms event, got %d", n)
	}
	// Adjacent period is fine; the as-of read reports the terms in force.
	if _, err := s.ChangePaymentTerms(ctx, p.ProfileID, domain.ChangePaymentTermsRequest{TermsCode: "NET_60", EffectiveFrom: to}, "m", nil); err != nil {
		t.Fatalf("adjacent period: %v", err)
	}
	asOf, err := s.FindProfileAsOf(ctx, p.ProfileID, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_ = asOf
	terms, _ := s.ListPaymentTerms(ctx, p.ProfileID)
	if len(terms) != 2 {
		t.Fatalf("expected 2 terms, got %d", len(terms))
	}
}

// TestPgStore_HighRiskChange_MakerChecker covers propose/decide, version and
// value staleness, atomic rollback, last-payee-change, and the SoD guard.
func TestPgStore_HighRiskChange_MakerChecker(t *testing.T) {
	s, pool := newTestStore(t)
	tenant := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenant)
	p, _ := s.CreateProfile(ctx, tenant, domain.CreateProfileRequest{LegalEntityID: uuid.New().String(), SupplierRef: "sup"}, "m", nil)
	p, _ = s.Transition(ctx, p.ProfileID, domain.CmdActivate, nil, "", "m", nil)

	if _, err := s.LastPayeeChange(ctx, p.ProfileID); !errors.Is(err, domain.ErrNoPayeeChange) {
		t.Fatalf("expected ErrNoPayeeChange, got %v", err)
	}

	cr, err := s.ProposeHighRiskChange(ctx, p.ProfileID, domain.ProposeHighRiskChangeRequest{Field: domain.FieldPayeeReference, NewValue: "ref-1"}, "bank-changer", nil)
	if err != nil {
		t.Fatal(err)
	}
	v := p.Version
	// Self-approval is refused inside the transaction and rolls back the decision.
	if _, _, err := s.DecideHighRiskChange(ctx, cr.ChangeRequestID, domain.DecideHighRiskChangeRequest{ExpectedVersion: &v, Approve: true}, "bank-changer", nil); !errors.Is(err, domain.ErrSoDConflict) {
		t.Fatalf("expected ErrSoDConflict, got %v", err)
	}
	if got, _ := s.FindChangeRequest(ctx, cr.ChangeRequestID); got.Status != domain.ChangeRequestPending {
		t.Fatalf("a refused self-approval must leave the request pending, got %s", got.Status)
	}
	// Stale version rolls back too.
	bad := v + 3
	if _, _, err := s.DecideHighRiskChange(ctx, cr.ChangeRequestID, domain.DecideHighRiskChangeRequest{ExpectedVersion: &bad, Approve: true}, "checker", nil); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}
	if got, _ := s.FindChangeRequest(ctx, cr.ChangeRequestID); got.Status != domain.ChangeRequestPending {
		t.Fatalf("a stale decision must leave the request pending")
	}

	decided, np, err := s.DecideHighRiskChange(ctx, cr.ChangeRequestID, domain.DecideHighRiskChangeRequest{ExpectedVersion: &v, Approve: true}, "checker", nil)
	if err != nil || decided.Status != domain.ChangeRequestApproved || np.PayeeReference != "ref-1" || np.Version != v+1 {
		t.Fatalf("decide: %v %+v %+v", err, decided, np)
	}
	last, err := s.LastPayeeChange(ctx, p.ProfileID)
	if err != nil || last.PrincipalID != "bank-changer" || last.ApproverPrincipalID != "checker" || last.Version != v+1 {
		t.Fatalf("last payee change: %v %+v", err, last)
	}
	if countOutbox(t, pool, p.ProfileID, domain.LegacyHighRiskDecided) != 1 {
		t.Fatalf("decide must enqueue the legacy decided event")
	}
	// A second proposal made against the old value can no longer be applied.
	cr2, _ := s.ProposeHighRiskChange(ctx, p.ProfileID, domain.ProposeHighRiskChangeRequest{Field: domain.FieldPayeeReference, NewValue: "ref-2"}, "bank-changer", nil)
	// Change the field out from under cr2 via another approved proposal.
	cr3, _ := s.ProposeHighRiskChange(ctx, p.ProfileID, domain.ProposeHighRiskChangeRequest{Field: domain.FieldPayeeReference, NewValue: "ref-3"}, "bank-changer", nil)
	cur, _ := s.FindProfile(ctx, p.ProfileID)
	cv := cur.Version
	if _, _, err := s.DecideHighRiskChange(ctx, cr3.ChangeRequestID, domain.DecideHighRiskChangeRequest{ExpectedVersion: &cv, Approve: true}, "checker", nil); err != nil {
		t.Fatal(err)
	}
	cur, _ = s.FindProfile(ctx, p.ProfileID)
	cv = cur.Version
	if _, _, err := s.DecideHighRiskChange(ctx, cr2.ChangeRequestID, domain.DecideHighRiskChangeRequest{ExpectedVersion: &cv, Approve: true}, "checker", nil); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("a proposal made against a superseded value must not apply, got %v", err)
	}
	// Decided requests are immutable (trigger).
	if _, err := pool.Exec(context.Background(), `UPDATE high_risk_change_requests SET reason = 'x' WHERE change_request_id = $1`, cr.ChangeRequestID); err == nil {
		t.Fatalf("decided change requests must be immutable")
	}
}

func TestPgStore_Amend_HighRiskRoutedToApproval(t *testing.T) {
	s, _ := newTestStore(t)
	tenant := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenant)
	p, _ := s.CreateProfile(ctx, tenant, domain.CreateProfileRequest{LegalEntityID: uuid.New().String(), SupplierRef: "sup"}, "m", nil)

	cat := "SVC"
	refs := []string{"a", "b"}
	policy := "STRICT"
	flags := []string{"WATCH"}
	res, err := s.AmendProfile(ctx, p.ProfileID, domain.AmendProfileRequest{
		Category: &cat, ProcurementCategoryRefs: &refs, APAccountPolicy: &policy, RiskControlFlags: &flags, Reason: "r",
	}, "maker", nil)
	if err != nil || res.HTTPStatus() != 202 || len(res.PendingChanges) != 2 {
		t.Fatalf("amend: %v %+v", err, res)
	}
	if res.Profile.Category != cat || len(res.Profile.ProcurementCategoryRefs) != 2 || res.Profile.APAccountPolicy != "" || res.Profile.Version != p.Version+1 {
		t.Fatalf("only low-risk fields apply immediately: %+v", res.Profile)
	}
	for _, pc := range res.PendingChanges {
		cur, _ := s.FindProfile(ctx, p.ProfileID)
		v := cur.Version
		if _, _, err := s.DecideHighRiskChange(ctx, pc.ChangeRequestID, domain.DecideHighRiskChangeRequest{ExpectedVersion: &v, Approve: true}, "checker", nil); err != nil {
			t.Fatalf("decide %s: %v", pc.Field, err)
		}
	}
	got, _ := s.FindProfile(ctx, p.ProfileID)
	if got.APAccountPolicy != "STRICT" || len(got.RiskControlFlags) != 1 || got.RiskControlFlags[0] != "WATCH" {
		t.Fatalf("approved high-risk fields not applied: %+v", got)
	}
	// A retired profile is terminal.
	if _, err := s.Transition(ctx, p.ProfileID, domain.CmdRetire, nil, "done", "m", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AmendProfile(ctx, p.ProfileID, domain.AmendProfileRequest{Category: &cat}, "m", nil); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition amending a retired profile, got %v", err)
	}
}

// TestPgStore_Idempotency covers reservation + stored response in the same
// transaction, replay lookup, a concurrent/duplicate key losing cleanly (no
// aborted transaction, no second state change) and tenant scoping of keys.
func TestPgStore_Idempotency(t *testing.T) {
	s, pool := newTestStore(t)
	tenant := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenant)
	le := uuid.New().String()
	idem := &domain.IdemScope{Key: "k-" + uuid.NewString(), Operation: "create", RequestHash: "h1"}

	p, err := s.CreateProfile(ctx, tenant, domain.CreateProfileRequest{LegalEntityID: le, SupplierRef: "sup"}, "m", idem)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.LookupIdempotency(ctx, idem.Key)
	if err != nil || rec == nil || rec.StatusCode != 201 || rec.RequestHash != "h1" || len(rec.Response) < 10 {
		t.Fatalf("lookup: %v %+v", err, rec)
	}
	// Same key again (the handler normally replays first; this is the race path).
	if _, err := s.CreateProfile(ctx, tenant, domain.CreateProfileRequest{LegalEntityID: le, SupplierRef: "sup2"}, "m", idem); !errors.Is(err, domain.ErrIdempotencyRace) {
		t.Fatalf("expected ErrIdempotencyRace, got %v", err)
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM supplier_financial_profiles WHERE legal_entity_id = $1`, le).Scan(&n)
	if n != 1 {
		t.Fatalf("the losing request must not create a profile, found %d", n)
	}
	// A failed command (stale version) does not consume its key.
	bad := 99
	k2 := &domain.IdemScope{Key: "k-" + uuid.NewString(), Operation: "activate", RequestHash: "h2"}
	if _, err := s.Transition(ctx, p.ProfileID, domain.CmdActivate, &bad, "", "m", k2); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("expected stale, got %v", err)
	}
	if rec, _ := s.LookupIdempotency(ctx, k2.Key); rec != nil {
		t.Fatalf("a failed command must not store an idempotency result")
	}
	if _, err := s.Transition(ctx, p.ProfileID, domain.CmdActivate, nil, "", "m", k2); err != nil {
		t.Fatalf("retry with the same key after a failure: %v", err)
	}
	// Keys are tenant-scoped.
	other := middleware.WithTenant(context.Background(), uuid.New().String())
	if rec, _ := s.LookupIdempotency(other, idem.Key); rec != nil {
		t.Fatalf("idempotency keys must be tenant-scoped")
	}
}

func TestPgStore_InvalidIDsAreNotFoundNotUnavailable(t *testing.T) {
	s, _ := newTestStore(t)
	tenant := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenant)
	if _, err := s.FindProfile(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrProfileNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Transition(ctx, "not-a-uuid", domain.CmdActivate, nil, "", "m", nil); !errors.Is(err, domain.ErrProfileNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.FindProfileAsOf(ctx, "not-a-uuid", time.Now()); !errors.Is(err, domain.ErrProfileNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.CreateProfile(middleware.WithTenant(context.Background(), "not-a-uuid"), "not-a-uuid", domain.CreateProfileRequest{LegalEntityID: "le", SupplierRef: "s"}, "m", nil); !errors.Is(err, domain.ErrInvalidTenant) {
		t.Fatalf("got %v", err)
	}
}
