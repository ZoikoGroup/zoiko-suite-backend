//go:build integration

package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
	"zoiko.io/payee-banking-identity-svc/internal/store"
	"zoiko.io/payee-banking-identity-svc/internal/tokenization"
)

// proposeLegacy proposes a destination the way every one was proposed before
// enforcement: raw, through a store with no tokenization.
func proposeLegacy(t *testing.T, f *fixture, party, identifier string) *domain.PayeeDestination {
	t.Helper()
	req := tokRequest(party)
	req.AccountIdentifier = identifier
	d, _, err := f.s.ProposeDestination(middleware.WithTenant(context.Background(), f.tenant), f.tenant, req, "legacy", "corr", "key-"+party)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func identifierTokenRows(t *testing.T, f *fixture) int {
	return count(t, f, `SELECT count(*) FROM payee_destination_identifier_tokens`)
}

func storedIdentifierColumn(t *testing.T, f *fixture, id string) string {
	t.Helper()
	var v string
	if err := f.pool.QueryRow(context.Background(), `SELECT account_identifier FROM payee_destinations WHERE destination_id = $1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// corrupting wraps the Fake so one identifier does not round-trip.
type corrupting struct {
	*tokenization.Fake
	bad string
}

func (c corrupting) Detokenize(ctx context.Context, s tokenization.Scope, tok string) (string, error) {
	v, err := c.Fake.Detokenize(ctx, s, tok)
	if err == nil && v == c.bad {
		return v + "0", nil
	}
	return v, err
}

// Every legacy destination — including SUSPENDED and SUPERSEDED ones, which
// 000002's trigger refuses to update at all — gets a verified token row;
// payee_destinations is not touched; each then reads back tokenized.
func TestBackfill_TokenizesEveryLegacyDestination(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := proposeLegacy(t, f, "party-bf-a-"+uuid.NewString(), "GB29NWBK60161331926819")
	b := proposeLegacy(t, f, "party-bf-b-"+uuid.NewString(), "DE89370400440532013000")
	suspended := proposeLegacy(t, f, "party-bf-c-"+uuid.NewString(), "FR1420041010050500013M02606")
	if _, err := f.pool.Exec(ctx, `UPDATE payee_destinations SET status = 'SUSPENDED' WHERE destination_id = $1`, suspended.DestinationID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE payee_destinations SET status = 'ACTIVE' WHERE destination_id = $1`, suspended.DestinationID); err == nil {
		t.Fatal("fixture: 000002 let a terminal destination be updated")
	}

	fake := &tokenization.Fake{}
	report, err := store.BackfillIdentifierTokens(ctx, f.pool, fake, store.BackfillOptions{PrincipalID: "svc-tokenization-backfill"})
	if err != nil || report.Candidates != 3 || report.Tokenized != 3 {
		t.Fatalf("backfill: %+v, %v", report, err)
	}
	tenantCtx := middleware.WithTenant(ctx, f.tenant)
	for _, d := range []*domain.PayeeDestination{a, b, suspended} {
		raw := storedIdentifierColumn(t, f, d.DestinationID)
		if raw != d.AccountIdentifier {
			t.Fatalf("account_identifier changed: %q", raw)
		}
		got, err := f.s.FindDestination(tenantCtx, d.DestinationID)
		if err != nil || !got.Tokenized() {
			t.Fatalf("%s after backfill: %+v, %v", d.DestinationID, got, err)
		}
		back, err := fake.Detokenize(ctx, tokenization.Scope{TenantID: f.tenant, PrincipalID: "p", LegalEntityID: "le-tok"}, got.AccountIdentifierToken)
		if err != nil || back != raw {
			t.Fatalf("token detokenizes to %q, %v", back, err)
		}
	}
	if again, err := store.BackfillIdentifierTokens(ctx, f.pool, fake, store.BackfillOptions{PrincipalID: "p"}); err != nil || again.Candidates != 0 {
		t.Fatalf("re-run: %+v, %v", again, err)
	}
}

// One mismatch stops everything; nothing is written, not even for the rows
// that verified before it.
func TestBackfill_RoundTripMismatchWritesNothing(t *testing.T) {
	f := newFixture(t)
	proposeLegacy(t, f, "party-mm-a-"+uuid.NewString(), "GB29NWBK60161331926819")
	proposeLegacy(t, f, "party-mm-b-"+uuid.NewString(), "DE89370400440532013000")
	_, err := store.BackfillIdentifierTokens(context.Background(), f.pool,
		corrupting{Fake: &tokenization.Fake{}, bad: "DE89370400440532013000"}, store.BackfillOptions{PrincipalID: "p"})
	if !errors.Is(err, store.ErrBackfillRoundTrip) || identifierTokenRows(t, f) != 0 {
		t.Fatalf("err = %v, rows = %d", err, identifierTokenRows(t, f))
	}
}

// A destination with no tenant cannot be bound to a token: the backfill
// stops with nothing written rather than leave that row raw unnoticed.
func TestBackfill_NoTenantStopsTheBackfill(t *testing.T) {
	f := newFixture(t)
	proposeLegacy(t, f, "party-nt-"+uuid.NewString(), "GB29NWBK60161331926819")
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO payee_destinations (tenant_id, legal_entity_id, party_ref, financial_institution, account_identifier,
			account_last4, country_code, currency, payee_name, source_type, fingerprint, proposed_by_principal_id)
		VALUES (NULL, 'le-tok', 'party-platform', 'Bank', 'GB82WEST12345698765432', '5432', 'GB', 'GBP', 'P', 'MANUAL_ENTRY', 'fp-nt', 'p')`); err != nil {
		t.Fatal(err)
	}
	_, err := store.BackfillIdentifierTokens(context.Background(), f.pool, &tokenization.Fake{}, store.BackfillOptions{PrincipalID: "p"})
	if !errors.Is(err, store.ErrBackfillNoTenant) || identifierTokenRows(t, f) != 0 {
		t.Fatalf("err = %v, rows = %d", err, identifierTokenRows(t, f))
	}
}

func TestBackfill_TokenizeFailureAndDryRunWriteNothing(t *testing.T) {
	f := newFixture(t)
	proposeLegacy(t, f, "party-fail-"+uuid.NewString(), "GB29NWBK60161331926819")
	if _, err := store.BackfillIdentifierTokens(context.Background(), f.pool,
		&tokenization.Fake{Err: tokenization.ErrTokenizationServiceUnavailable}, store.BackfillOptions{PrincipalID: "p"}); !errors.Is(err, tokenization.ErrTokenizationServiceUnavailable) {
		t.Fatalf("failing vault: %v", err)
	}
	fake := &tokenization.Fake{}
	report, err := store.BackfillIdentifierTokens(context.Background(), f.pool, fake, store.BackfillOptions{PrincipalID: "p", DryRun: true})
	if err != nil || report.Tokenized != 0 || fake.Calls() != 2 || identifierTokenRows(t, f) != 0 {
		t.Fatalf("dry run: %+v, %v, calls %d, rows %d", report, err, fake.Calls(), identifierTokenRows(t, f))
	}
}

// A role subject to RLS would see no rows and "succeed"; refused outright.
func TestBackfill_RefusesAnRLSBoundRole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `DO $do$ BEGIN
		CREATE ROLE payee_rls_bound LOGIN PASSWORD 'pw' NOSUPERUSER NOBYPASSRLS;
	EXCEPTION WHEN duplicate_object THEN NULL; END $do$`); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	u.User = url.UserPassword("payee_rls_bound", "pw")
	bound, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if _, err := store.BackfillIdentifierTokens(ctx, bound, &tokenization.Fake{}, store.BackfillOptions{PrincipalID: "p"}); !errors.Is(err, store.ErrBackfillNeedsAllRows) {
		t.Fatalf("err = %v, want ErrBackfillNeedsAllRows", err)
	}
}

// Whether a destination is tokenized is the ROW's fact, not the current
// flag: a destination proposed with enforcement on still replays (matched by
// fingerprint) after enforcement is turned off, instead of being refused as a
// key conflict because its stored token differs from the raw request.
func TestReplay_TokenizedRowMatchesAfterEnforcementIsTurnedOff(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	party := "party-flag-" + uuid.NewString()
	key := "key-" + party
	on := store.NewPgStore(f.pool, zap.NewNop(), store.WithEventRegion("uk"), store.WithTokenization(&tokenization.Fake{}, true))
	first, _, err := on.ProposeDestination(ctx, f.tenant, tokRequest(party), "proposer", "corr", key)
	if err != nil || !first.Tokenized() {
		t.Fatalf("enforced propose: %+v, %v", first, err)
	}
	off := store.NewPgStore(f.pool, zap.NewNop(), store.WithEventRegion("uk"))
	again, replay, err := off.ProposeDestination(ctx, f.tenant, tokRequest(party), "proposer", "corr", key)
	if err != nil || !replay || again.DestinationID != first.DestinationID {
		t.Fatalf("replay with enforcement off: replay=%v err=%v", replay, err)
	}
	var source string
	_ = f.pool.QueryRow(context.Background(), `SELECT source FROM payee_destination_identifier_tokens WHERE destination_id = $1`, first.DestinationID).Scan(&source)
	if source != "WRITE" || strings.Contains(storedIdentifierColumn(t, f, first.DestinationID), "GB29") {
		t.Fatalf("enforced propose: source %q, stored %q", source, storedIdentifierColumn(t, f, first.DestinationID))
	}
}
