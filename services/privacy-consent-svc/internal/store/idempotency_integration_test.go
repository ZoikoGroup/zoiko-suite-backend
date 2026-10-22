package store

// Real-PostgreSQL tests for the idempotency remediation: the claim, the
// evidence row(s), the outbox event and the stored replay response commit in
// ONE transaction (RunIdempotent), scoped to (tenant, principal, key).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"zoiko.io/privacy-consent-svc/internal/domain"
)

func claimFor(tenant, principal, key string) domain.IdempotencyRecord {
	return domain.IdempotencyRecord{Key: key, TenantID: tenant, PrincipalID: principal, Endpoint: "op", RequestHash: "h-" + key}
}

func setPref(s *PgStore, tenant, subject string) func(ctx context.Context) (int, any, error) {
	return func(ctx context.Context) (int, any, error) {
		p, err := s.SetPreference(ctx, tenant, domain.SetPreferenceRequest{SubjectRef: subject, ChannelOrPurpose: "email", Value: "ENABLED", Source: "UI"})
		return 201, p, err
	}
}

func TestRunIdempotent_ClaimEvidenceEventAndResponseCommitTogether(t *testing.T) {
	s, db := newStore(t)
	const tenant = "00000000-0000-0000-0000-0000000000c1"
	nv := publishedNotice(t, s, tenant)

	existing, status, _, err := s.RunIdempotent(tctx(tenant), claimFor(tenant, "p1", "k1"), func(ctx context.Context) (int, any, error) {
		r, err := s.RecordConsent(ctx, tenant, creq("subj-1", "purpose-x", nv, "GRANTED"), "p1", "corr-1")
		return 201, r, err
	})
	if err != nil || existing != nil || status != 201 {
		t.Fatalf("first: existing=%v status=%d err=%v", existing, status, err)
	}
	if countWhere(t, db, "consent_receipts", "subject_ref='subj-1'") != 1 ||
		countWhere(t, db, "event_outbox", "event_type='privacy.consent.changed' AND entity_id='subj-1'") != 1 ||
		countWhere(t, db, "consent_idempotency_keys", "idempotency_key='k1' AND response_code=201 AND length(response_body) > 0") != 1 {
		t.Fatal("receipt, event and idempotency record must all be present")
	}

	// A duplicate runs nothing and returns the stored record.
	existing, _, _, err = s.RunIdempotent(tctx(tenant), claimFor(tenant, "p1", "k1"), func(ctx context.Context) (int, any, error) {
		t.Fatal("a duplicate must not execute")
		return 0, nil, nil
	})
	if err != nil || existing == nil || existing.ResponseCode != 201 || existing.RequestHash != "h-k1" {
		t.Fatalf("duplicate: %+v %v", existing, err)
	}
}

func TestRunIdempotent_SaveFailureRollsBackEvidenceEventAndClaim_RetryCreatesOne(t *testing.T) {
	s, db := newStore(t)
	const tenant = "00000000-0000-0000-0000-0000000000c2"
	nv := publishedNotice(t, s, tenant)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE FUNCTION fail_idem_update() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected idempotency save failure'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER fail_idem_update BEFORE UPDATE ON consent_idempotency_keys FOR EACH ROW EXECUTE FUNCTION fail_idem_update()`,
	} {
		if _, err := db.Admin.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	op := func(c context.Context) (int, any, error) {
		r, err := s.RecordConsent(c, tenant, creq("subj-2", "purpose-x", nv, "GRANTED"), "p1", "corr-2")
		return 201, r, err
	}
	if _, _, _, err := s.RunIdempotent(tctx(tenant), claimFor(tenant, "p1", "k-fail"), op); err == nil {
		t.Fatal("a failed idempotency save must fail the operation")
	}
	if countWhere(t, db, "consent_receipts", "subject_ref='subj-2'") != 0 ||
		countWhere(t, db, "event_outbox", "entity_id='subj-2'") != 0 ||
		countWhere(t, db, "consent_idempotency_keys", "idempotency_key='k-fail'") != 0 {
		t.Fatal("receipt, event and claim must roll back together")
	}
	if _, err := db.Admin.Exec(ctx, `DROP TRIGGER fail_idem_update ON consent_idempotency_keys`); err != nil {
		t.Fatal(err)
	}
	existing, _, _, err := s.RunIdempotent(tctx(tenant), claimFor(tenant, "p1", "k-fail"), op)
	if err != nil || existing != nil {
		t.Fatalf("retry: %v %v", existing, err)
	}
	if countWhere(t, db, "consent_receipts", "subject_ref='subj-2'") != 1 || countWhere(t, db, "event_outbox", "entity_id='subj-2'") != 1 {
		t.Fatal("the retry must produce exactly one receipt and one event")
	}
}

func TestRunIdempotent_OperationErrorRollsBackTheClaim(t *testing.T) {
	s, db := newStore(t)
	const tenant = "00000000-0000-0000-0000-0000000000c3"
	// An unpublished/unknown notice version is refused with a domain error.
	_, _, _, err := s.RunIdempotent(tctx(tenant), claimFor(tenant, "p1", "k-err"), func(ctx context.Context) (int, any, error) {
		r, err := s.RecordConsent(ctx, tenant, creq("subj-3", "purpose-x", "00000000-0000-0000-0000-00000000dead", "GRANTED"), "p1", "c")
		return 201, r, err
	})
	if !errors.Is(err, domain.ErrNoticeVersionInvalid) {
		t.Fatalf("the operation's domain error must reach the caller, got %v", err)
	}
	if countWhere(t, db, "consent_idempotency_keys", "idempotency_key='k-err'") != 0 {
		t.Fatal("a refused operation must not consume the idempotency key")
	}
}

func TestRunIdempotent_ConcurrentIdenticalRequests_ExecuteOnce(t *testing.T) {
	s, db := newStore(t)
	const tenant = "00000000-0000-0000-0000-0000000000c4"
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners, replays := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			existing, _, _, err := s.RunIdempotent(tctx(tenant), claimFor(tenant, "p1", "race"), setPref(s, tenant, "subj-race"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("RunIdempotent: %v", err)
			case existing != nil:
				replays++
			default:
				winners++
			}
		}()
	}
	wg.Wait()
	if winners != 1 || replays != n-1 {
		t.Fatalf("winners=%d replays=%d", winners, replays)
	}
	if countWhere(t, db, "preference_assertions", "subject_ref='subj-race'") != 1 {
		t.Fatal("concurrent identical requests duplicated the evidence")
	}
}

func TestRunIdempotent_ScopedByTenantAndPrincipal(t *testing.T) {
	s, db := newStore(t)
	const tA, tB = "00000000-0000-0000-0000-0000000000c5", "00000000-0000-0000-0000-0000000000c6"
	for _, c := range []struct{ tenant, principal string }{{tA, "p1"}, {tA, "p2"}, {tB, "p1"}} {
		existing, _, _, err := s.RunIdempotent(tctx(c.tenant), claimFor(c.tenant, c.principal, "same-key"), setPref(s, c.tenant, "subj-"+c.tenant[len(c.tenant)-1:]+c.principal))
		if err != nil || existing != nil {
			t.Fatalf("%v: %v %v", c, existing, err)
		}
	}
	if countWhere(t, db, "consent_idempotency_keys", "idempotency_key='same-key'") != 3 {
		t.Fatal("the same key under a different principal or tenant is a different claim")
	}
	got, err := s.GetIdempotency(tctx(tA), tA, "p2", "same-key")
	if err != nil || got == nil || got.PrincipalID != "p2" {
		t.Fatalf("GetIdempotency must be principal-scoped: %v %v", got, err)
	}
	if got, _ := s.GetIdempotency(tctx(tA), tA, "p3", "same-key"); got != nil {
		t.Fatal("another principal must not see the record")
	}
	if got, _ := s.GetIdempotency(tctx(tB), tA, "p1", "same-key"); got != nil {
		t.Fatal("another tenant must not see the record (RLS)")
	}
}

func TestRunIdempotent_RefusesAClaimThatIsNotTheVerifiedTenant(t *testing.T) {
	s, _ := newStore(t)
	const tA, tB = "00000000-0000-0000-0000-0000000000c7", "00000000-0000-0000-0000-0000000000c8"
	if _, _, _, err := s.RunIdempotent(tctx(tA), claimFor(tB, "p1", "k"), setPref(s, tB, "x")); err == nil {
		t.Fatal("a claim for another tenant must be refused")
	}
	if _, _, _, err := s.RunIdempotent(context.Background(), claimFor(tA, "p1", "k"), setPref(s, tA, "x")); err == nil {
		t.Fatal("a claim with no verified tenant must be refused")
	}
}
