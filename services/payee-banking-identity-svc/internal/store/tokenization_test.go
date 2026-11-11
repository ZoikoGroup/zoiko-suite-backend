//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
	"zoiko.io/payee-banking-identity-svc/internal/store"
	"zoiko.io/payee-banking-identity-svc/internal/tokenization"
)

const rawIdentifier = "GB29NWBK60161331926819"

func tokRequest(partyRef string) domain.ProposeDestinationRequest {
	return domain.ProposeDestinationRequest{
		LegalEntityID: "le-tok", PartyRef: partyRef, FinancialInstitution: "Token Bank",
		AccountIdentifier: rawIdentifier, CountryCode: "GB", Currency: "GBP",
		PayeeName: "Token Payee", SourceType: domain.SourceManualEntry,
	}
}

// storedIdentifier is payee_destinations.account_identifier as written, with
// the fingerprint and last-4 written beside it.
func storedIdentifier(t *testing.T, f *fixture, partyRef string) (identifier, fingerprint, last4 string, found bool) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(), `SELECT account_identifier, fingerprint, account_last4
		FROM payee_destinations WHERE party_ref = $1`, partyRef).Scan(&identifier, &fingerprint, &last4)
	return identifier, fingerprint, last4, err == nil
}

func count(t *testing.T, f *fixture, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// With TOKENIZATION_ENFORCED on, the token — not the raw identifier — lands
// in account_identifier and in the returned destination; the fingerprint and
// last-4 are still the raw value's, and only the same tenant detokenizes it.
func TestTokenization_EnforcedStoresTheTokenNotTheRawValue(t *testing.T) {
	f := newFixture(t)
	fake := &tokenization.Fake{}
	s := store.NewPgStore(f.pool, zap.NewNop(), store.WithEventRegion("uk"), store.WithTokenization(fake, true))
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	party := "party-tok-" + uuid.NewString()

	d, replay, err := s.ProposeDestination(ctx, f.tenant, tokRequest(party), "proposer", "corr-tok", "key-"+party)
	if err != nil || replay {
		t.Fatalf("propose: replay=%v err=%v", replay, err)
	}
	stored, fp, last4, ok := storedIdentifier(t, f, party)
	if !ok {
		t.Fatal("no destination written")
	}
	if stored == rawIdentifier {
		t.Fatal("the raw identifier was stored with tokenization enforced")
	}
	if d.AccountIdentifier != stored {
		t.Fatalf("returned destination carries %q, the database %q", d.AccountIdentifier, stored)
	}
	if fp != domain.Fingerprint("Token Bank", rawIdentifier, "GBP") || last4 != domain.Last4(rawIdentifier) {
		t.Fatalf("fingerprint/last4 not derived from the raw identifier: %s / %s", fp, last4)
	}
	if v, err := fake.Detokenize(context.Background(), tokenization.Scope{TenantID: f.tenant, PrincipalID: "auditor"}, stored); err != nil || v != rawIdentifier {
		t.Fatalf("stored token detokenizes to %q, %v", v, err)
	}
	if _, err := fake.Detokenize(context.Background(), tokenization.Scope{TenantID: uuid.NewString(), PrincipalID: "auditor"}, stored); !errors.Is(err, tokenization.ErrDetokenizationDenied) {
		t.Fatalf("another tenant detokenized the stored token: %v", err)
	}
}

// A stored token never equals the request's raw identifier, so an honest
// retry must still replay (matched through the fingerprint), and a different
// account under the same key must still conflict.
func TestTokenization_EnforcedReplayStillMatches(t *testing.T) {
	f := newFixture(t)
	s := store.NewPgStore(f.pool, zap.NewNop(), store.WithEventRegion("uk"), store.WithTokenization(&tokenization.Fake{}, true))
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	party := "party-tok-replay-" + uuid.NewString()
	key := "key-" + party

	first, _, err := s.ProposeDestination(ctx, f.tenant, tokRequest(party), "proposer", "corr", key)
	if err != nil {
		t.Fatal(err)
	}
	again, replay, err := s.ProposeDestination(ctx, f.tenant, tokRequest(party), "proposer", "corr", key)
	if err != nil || !replay || again.DestinationID != first.DestinationID {
		t.Fatalf("same request, same key: replay=%v err=%v", replay, err)
	}
	other := tokRequest(party)
	other.AccountIdentifier = "GB82WEST12345698765432"
	if _, _, err := s.ProposeDestination(ctx, f.tenant, other, "proposer", "corr", key); !errors.Is(err, domain.ErrIdempotencyKeyConflict) {
		t.Fatalf("different account under the same key: %v, want ErrIdempotencyKeyConflict", err)
	}
}

// A refused or impossible tokenization refuses the whole write: no
// destination, no command claim, no event — never a raw fallback.
func TestTokenization_FailedTokenizeWritesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	cases := map[string]struct {
		opt  store.Option
		want error
	}{
		"service unavailable": {store.WithTokenization(&tokenization.Fake{Err: tokenization.ErrTokenizationServiceUnavailable}, true),
			tokenization.ErrTokenizationServiceUnavailable},
		"enforced with no tokenizer": {store.WithTokenization(nil, true), store.ErrTokenizationNotConfigured},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := store.NewPgStore(f.pool, zap.NewNop(), store.WithEventRegion("uk"), tc.opt)
			party := "party-tok-fail-" + uuid.NewString()
			key := "key-" + party
			events := count(t, f, `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1`, f.tenant)
			if _, _, err := s.ProposeDestination(ctx, f.tenant, tokRequest(party), "proposer", "corr", key); !errors.Is(err, tc.want) {
				t.Fatalf("propose = %v, want %v", err, tc.want)
			}
			if _, _, _, ok := storedIdentifier(t, f, party); ok {
				t.Fatal("a destination was written")
			}
			if n := count(t, f, `SELECT count(*) FROM command_idempotency WHERE idempotency_key = $1`, key); n != 0 {
				t.Fatalf("the idempotency key was claimed (%d rows)", n)
			}
			if n := count(t, f, `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1`, f.tenant); n != events {
				t.Fatalf("events written: %d -> %d", events, n)
			}
		})
	}
}

// Regression: with TOKENIZATION_ENFORCED off, the write is byte-for-byte what
// it is today — the same row as a store built without any tokenization
// option — and the tokenizer is never called.
func TestTokenization_NotEnforcedIsUnchanged(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	fake := &tokenization.Fake{}
	stores := map[string]*store.PgStore{
		"no option":      store.NewPgStore(f.pool, zap.NewNop(), store.WithEventRegion("uk")),
		"enforced=false": store.NewPgStore(f.pool, zap.NewNop(), store.WithEventRegion("uk"), store.WithTokenization(fake, false)),
	}
	for name, s := range stores {
		party := "party-tok-off-" + uuid.NewString()
		d, _, err := s.ProposeDestination(ctx, f.tenant, tokRequest(party), "proposer", "corr", "key-"+party)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		stored, fp, last4, _ := storedIdentifier(t, f, party)
		if stored != rawIdentifier || d.AccountIdentifier != rawIdentifier ||
			fp != domain.Fingerprint("Token Bank", rawIdentifier, "GBP") || last4 != domain.Last4(rawIdentifier) {
			t.Fatalf("%s: stored %q returned %q fp %s last4 %s — want today's raw row", name, stored, d.AccountIdentifier, fp, last4)
		}
		if _, replay, err := s.ProposeDestination(ctx, f.tenant, tokRequest(party), "proposer", "corr", "key-"+party); err != nil || !replay {
			t.Fatalf("%s: replay=%v err=%v", name, replay, err)
		}
	}
	if fake.Calls() != 0 {
		t.Fatalf("the tokenizer was called %d times with enforcement off", fake.Calls())
	}
}
