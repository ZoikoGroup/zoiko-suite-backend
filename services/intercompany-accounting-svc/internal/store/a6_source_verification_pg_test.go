package store_test

// SPEC_DEVIATIONS A6 against a real Postgres: a pair created because its
// source journal failed one of the four checks is persisted in MISMATCH with
// its structured source-side attempt, the attempt is in the same transaction,
// a replay writes no second attempt, and migration 000008's widened CHECK
// accepts the source rules. Skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

func sourceVerificationAttempt(entry *domain.IntercompanyEntry, rule, message string) *domain.MatchAttempt {
	observedAmount := domain.Decimal("5000")
	observedCurrency, observedEntity, observedStatus, observedPeriod := "USD", entry.SourceLegalEntityID, "DRAFT", "2026-09"
	return &domain.MatchAttempt{
		IntercompanyEntryID: entry.IntercompanyEntryID,
		Outcome:             domain.MatchStatusMismatch,
		FailedRule:          &rule,
		Message:             &message,
		Source: domain.MatchAttemptSide{
			JournalID:             &entry.SourceJournalID,
			ExpectedAmount:        entry.Amount,
			ExpectedCurrency:      entry.CurrencyCode,
			ObservedAmount:        &observedAmount,
			ObservedCurrency:      &observedCurrency,
			ObservedLegalEntityID: &observedEntity,
			ObservedStatus:        &observedStatus,
			ObservedFiscalPeriod:  &observedPeriod,
		},
		Target: domain.MatchAttemptSide{
			ExpectedAmount:   entry.CounterpartyAmount,
			ExpectedCurrency: entry.CounterpartyCurrencyCode,
		},
		ExaminedJournalIDs: []string{entry.SourceJournalID},
		Tolerance:          "0.001",
		AttemptedBy:        "creator",
	}
}

func TestPgStore_A6_CreateInMismatchRecordsSourceAttempt(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	entry.MatchStatus = domain.MatchStatusMismatch
	reason := "source_journal_not_finalized: status is DRAFT"
	attempt := sourceVerificationAttempt(entry, domain.MatchRuleSourceJournalNotFinalized, reason)

	created, err := s.CreateEntryWithAttempt(ctx, entry, attempt, "creator", "corr-create")
	if err != nil {
		t.Fatalf("CreateEntryWithAttempt: %v", err)
	}
	if !created {
		t.Fatal("expected the pair to be created")
	}

	got, err := s.GetEntry(ctx, entry.IntercompanyEntryID)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got.MatchStatus != domain.MatchStatusMismatch || got.MismatchReason == nil || *got.MismatchReason != reason {
		t.Fatalf("pair not created in MISMATCH with its reason: %q %v", got.MatchStatus, got.MismatchReason)
	}

	attempts, err := s.ListMatchAttempts(ctx, entry.IntercompanyEntryID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("ListMatchAttempts: %v, %d", err, len(attempts))
	}
	a := attempts[0]
	if a.Outcome != domain.MatchStatusMismatch || a.FailedRule == nil || *a.FailedRule != domain.MatchRuleSourceJournalNotFinalized ||
		a.Message == nil || *a.Message != reason {
		t.Fatalf("attempt: %+v", a)
	}
	if a.Source.JournalID == nil || *a.Source.JournalID != entry.SourceJournalID ||
		a.Source.ExpectedAmount != "5000.0000" || a.Source.ExpectedCurrency != "USD" ||
		a.Source.ObservedAmount == nil || *a.Source.ObservedAmount != "5000" ||
		a.Source.ObservedStatus == nil || *a.Source.ObservedStatus != "DRAFT" {
		t.Fatalf("source side: %+v", a.Source)
	}
	if a.Target.JournalID != nil || a.Target.ExpectedAmount != "5000.0000" || a.Target.ExpectedCurrency != "USD" ||
		a.Target.ObservedAmount != nil {
		t.Fatalf("target side: %+v", a.Target)
	}
	if len(a.ExaminedJournalIDs) != 1 || a.ExaminedJournalIDs[0] != entry.SourceJournalID || a.Tolerance != "0.001" {
		t.Fatalf("examined/tolerance: %v / %s", a.ExaminedJournalIDs, a.Tolerance)
	}
	if a.FiscalPeriod != nil || a.AttemptedBy != "creator" {
		t.Fatalf("period/actor: %+v", a)
	}

	// Both facts are real at creation: the pair was created, and a mismatch
	// was detected. Both events are in the same transaction as the row.
	events := a7Events(t, pool, tenantID, entry.IntercompanyEntryID)
	if len(events) != 2 || events[0].canonicalType != a7CreatedType || events[1].canonicalType != a7MismatchType {
		t.Fatalf("expected created + mismatch-detected, got %+v", a7CanonicalTypes(events))
	}
	for _, e := range events {
		if got := a7String(t, e.data, "match_status"); got != domain.MatchStatusMismatch {
			t.Errorf("%s match_status = %q", e.canonicalType, got)
		}
		if got := a7String(t, e.data, "mismatch_reason"); got != reason {
			t.Errorf("%s mismatch_reason = %q", e.canonicalType, got)
		}
	}
}

// A replay must not add an attempt to a pair that already exists: the
// create-time verification belongs to the creation, not to every POST.
func TestPgStore_A6_ReplayWritesNoSecondAttempt(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	sourceJournalID := uuid.New().String()
	entry := newTestEntry(tenantID, sourceJournalID)
	if _, err := s.CreateEntry(ctx, entry, "creator", "c1"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}

	replay := newTestEntry(tenantID, sourceJournalID)
	replay.MatchStatus = domain.MatchStatusMismatch
	reason := "source_journal_not_finalized: status is DRAFT"
	created, err := s.CreateEntryWithAttempt(ctx, replay, sourceVerificationAttempt(replay, domain.MatchRuleSourceJournalNotFinalized, reason), "creator", "c2")
	if err != nil {
		t.Fatalf("replayed CreateEntryWithAttempt: %v", err)
	}
	if created {
		t.Fatal("expected created=false for a replay")
	}
	if replay.IntercompanyEntryID != entry.IntercompanyEntryID || replay.MatchStatus != domain.MatchStatusUnmatched {
		t.Fatalf("replay resolved to %+v, want the original %s UNMATCHED", replay, entry.IntercompanyEntryID)
	}
	if attempts, err := s.ListMatchAttempts(ctx, entry.IntercompanyEntryID); err != nil || len(attempts) != 0 {
		t.Fatalf("replay recorded an attempt: %v, %d", err, len(attempts))
	}
}

// The pair and its create-time attempt are one transaction: an attempt the
// CHECK refuses must leave no pair behind.
func TestPgStore_A6_MismatchCreateIsAtomic(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	entry.MatchStatus = domain.MatchStatusMismatch
	attempt := sourceVerificationAttempt(entry, "NOT_A_RULE", "source_journal_not_finalized: status is DRAFT")
	if _, err := s.CreateEntryWithAttempt(ctx, entry, attempt, "creator", "c1"); err == nil {
		t.Fatal("expected the failed_rule CHECK to refuse an unknown rule")
	}
	if _, err := s.GetEntryBySourceJournal(ctx, entry.SourceJournalID); err == nil {
		t.Fatal("the pair must not survive a failed attempt insert")
	}
}
