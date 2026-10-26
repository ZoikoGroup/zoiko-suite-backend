package store

// Real-PostgreSQL tests for PreferenceAssertion.effective_at (§10.1 "source and
// effective time"), migration 000008.

import (
	"context"
	"testing"
	"time"

	"zoiko.io/privacy-consent-svc/internal/domain"
)

const prefTenant = "00000000-0000-0000-0000-0000000000e1"

func setPrefAt(t *testing.T, s *PgStore, subject, value string, at *time.Time) *domain.PreferenceAssertion {
	t.Helper()
	p, err := s.SetPreference(tctx(prefTenant), prefTenant, domain.SetPreferenceRequest{SubjectRef: subject, ChannelOrPurpose: "email", Value: value, Source: "UI", EffectiveAt: at})
	if err != nil {
		t.Fatalf("SetPreference: %v", err)
	}
	return p
}

func resolvePref(t *testing.T, s *PgStore, subject string) *domain.PreferenceAssertion {
	t.Helper()
	p, err := s.ResolvePreference(tctx(prefTenant), subject, "email")
	if err != nil {
		t.Fatalf("ResolvePreference: %v", err)
	}
	return p
}

func TestPreferenceEffectiveAt_PersistedExactly_InUTC_Microseconds(t *testing.T) {
	s, db := newStore(t)
	loc := time.FixedZone("IST", 5*3600+1800)
	at := time.Date(2026, 1, 2, 10, 0, 0, 123456789, loc) // sub-µs digits, non-UTC zone
	p := setPrefAt(t, s, "s1", "ENABLED", &at)

	want := time.Date(2026, 1, 2, 4, 30, 0, 123456000, time.UTC)
	if !p.EffectiveAt.Equal(want) || p.EffectiveAt.Location() != time.UTC {
		t.Fatalf("returned effective_at %v, want %v UTC", p.EffectiveAt, want)
	}
	var stored time.Time
	if err := db.Admin.QueryRow(context.Background(), `SELECT effective_at FROM preference_assertions WHERE preference_assertion_id=$1`, p.PreferenceAssertionID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(want) {
		t.Fatalf("persisted effective_at %v, want %v", stored, want)
	}
	if !p.CreatedAt.After(p.EffectiveAt) && !p.CreatedAt.Equal(p.EffectiveAt) {
		// created_at is "now" (2026-10 clock or later); effective_at is the stated past instant.
		t.Fatalf("effective_at must be independent of created_at: %v vs %v", p.EffectiveAt, p.CreatedAt)
	}
	if got := resolvePref(t, s, "s1"); got.PreferenceAssertionID != p.PreferenceAssertionID || !got.EffectiveAt.Equal(want) {
		t.Fatalf("resolution must return the assertion with its effective time: %+v", got)
	}
}

func TestPreferenceEffectiveAt_DefaultsToRecordingInstant(t *testing.T) {
	s, _ := newStore(t)
	p := setPrefAt(t, s, "s2", "DISABLED", nil)
	if !p.EffectiveAt.Equal(p.CreatedAt) {
		t.Fatalf("default effective_at must equal the recording instant: %v vs %v", p.EffectiveAt, p.CreatedAt)
	}
}

func TestPreferenceEffectiveAt_ResolutionOrdersByEffectiveTime(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now().UTC()
	d := func(days int) *time.Time { x := now.AddDate(0, 0, days); return &x }
	setPrefAt(t, s, "s3", "ENABLED", d(-1)) // recorded first
	setPrefAt(t, s, "s3", "DISABLED", d(0)) // later effective time -> governs
	setPrefAt(t, s, "s3", "UNSET", d(-10))  // recorded last, effective earliest -> must not override
	if got := resolvePref(t, s, "s3"); got.Value != domain.PreferenceDisabled {
		t.Fatalf("latest effective time must govern, got %s", got.Value)
	}
	// Tie on effective time: the one recorded last governs.
	same := now.AddDate(0, 0, -2)
	setPrefAt(t, s, "s3b", "ENABLED", &same)
	setPrefAt(t, s, "s3b", "DISABLED", &same)
	if got := resolvePref(t, s, "s3b"); got.Value != domain.PreferenceDisabled {
		t.Fatalf("on equal effective time the later-recorded assertion governs, got %s", got.Value)
	}
}

func TestPreferenceEffectiveAt_FutureAssertionNotYetInForce(t *testing.T) {
	s, _ := newStore(t)
	past := time.Now().UTC().AddDate(0, 0, -1)
	future := time.Now().UTC().AddDate(0, 0, 5)
	setPrefAt(t, s, "s4", "ENABLED", &past)
	setPrefAt(t, s, "s4", "DISABLED", &future)
	if got := resolvePref(t, s, "s4"); got.Value != domain.PreferenceEnabled {
		t.Fatalf("a future-dated assertion must not govern yet, got %s", got.Value)
	}
	setPrefAt(t, s, "s5", "ENABLED", &future)
	if got := resolvePref(t, s, "s5"); got.Value != domain.PreferenceNotApplicable || got.PreferenceAssertionID != "" {
		t.Fatalf("nothing in force must resolve NOT_APPLICABLE, got %+v", got)
	}
}

func TestPreferenceEffectiveAt_LegacyRowsWithoutAStoredValueReadAsCreatedAt(t *testing.T) {
	s, db := newStore(t)
	ctx := context.Background()
	// A row exactly as written before migration 000008: no effective_at.
	var id string
	var created time.Time
	if err := db.Admin.QueryRow(ctx, `
		INSERT INTO preference_assertions (tenant_id, subject_ref, channel_or_purpose, value, source)
		VALUES ($1, 'legacy', 'email', 'ENABLED', 'UI') RETURNING preference_assertion_id, created_at`, prefTenant).Scan(&id, &created); err != nil {
		t.Fatal(err)
	}
	got := resolvePref(t, s, "legacy")
	if got.PreferenceAssertionID != id || !got.EffectiveAt.Equal(created) {
		t.Fatalf("a legacy row must read as effective from created_at: %+v (created %v)", got, created)
	}
	// A newer-effective explicit assertion overrides it; an older-effective one does not.
	older := created.AddDate(0, 0, -3)
	setPrefAt(t, s, "legacy", "DISABLED", &older)
	if got := resolvePref(t, s, "legacy"); got.Value != domain.PreferenceEnabled {
		t.Fatalf("an assertion effective before the legacy row must not override it, got %s", got.Value)
	}
	// Append-only evidence is untouched: the table still rejects UPDATE and DELETE.
	if _, err := db.Admin.Exec(ctx, `UPDATE preference_assertions SET effective_at = NOW() WHERE preference_assertion_id=$1`, id); err == nil {
		t.Fatal("preference evidence must remain append-only (UPDATE)")
	}
	if _, err := db.Admin.Exec(ctx, `DELETE FROM preference_assertions WHERE preference_assertion_id=$1`, id); err == nil {
		t.Fatal("preference evidence must remain append-only (DELETE)")
	}
}

func TestPreferenceEffectiveAt_TenantIsolationUnchanged(t *testing.T) {
	s, _ := newStore(t)
	p := setPrefAt(t, s, "iso", "ENABLED", nil)
	other := "00000000-0000-0000-0000-0000000000e2"
	got, err := s.ResolvePreference(tctx(other), "iso", "email")
	if err != nil || got.PreferenceAssertionID != "" || got.Value != domain.PreferenceNotApplicable {
		t.Fatalf("another tenant must see nothing: %+v %v (own id %s)", got, err, p.PreferenceAssertionID)
	}
}
