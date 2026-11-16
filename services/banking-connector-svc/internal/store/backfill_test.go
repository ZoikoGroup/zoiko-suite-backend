package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
	"zoiko.io/banking-connector-svc/internal/tokenization"
)

const bfTenant = "tenant-backfill"

// legacyConnection writes a connection the way every connection was written
// before enforcement: raw, through a store with no tokenization.
func legacyConnection(t *testing.T, app *pgxpool.Pool, raw string) *domain.BankConnection {
	t.Helper()
	c := &domain.BankConnection{ConnectionID: uuid.NewString(), LegalEntityID: "le-bf", BankName: "Legacy Bank",
		BIC: "NWBKGB2L", AccountNumber: raw, IBAN: raw, Currency: "GBP", Status: "ACTIVE"}
	if err := store.NewPgStore(app, store.WithEventRegion("uk")).CreateConnection(
		middleware.WithTenant(context.Background(), bfTenant), c, "legacy-writer", "corr"); err != nil {
		t.Fatal(err)
	}
	return c
}

func tokenRows(t *testing.T, admin *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM bank_connection_account_tokens`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rawColumn(t *testing.T, admin *pgxpool.Pool, id string) string {
	t.Helper()
	var v string
	if err := admin.QueryRow(context.Background(), `SELECT account_number FROM bank_connections WHERE connection_id = $1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// scopeRecorder wraps the Fake, recording tokenize scopes, and corrupts the
// detokenized value for one raw number (a round-trip mismatch).
type scopeRecorder struct {
	*tokenization.Fake
	mu       sync.Mutex
	scopes   []tokenization.Scope
	corrupts string
}

func (s *scopeRecorder) Tokenize(ctx context.Context, scope tokenization.Scope, class, value string) (string, error) {
	s.mu.Lock()
	s.scopes = append(s.scopes, scope)
	s.mu.Unlock()
	return s.Fake.Tokenize(ctx, scope, class, value)
}

func (s *scopeRecorder) Detokenize(ctx context.Context, scope tokenization.Scope, token string) (string, error) {
	v, err := s.Fake.Detokenize(ctx, scope, token)
	if err == nil && s.corrupts != "" && v == s.corrupts {
		return v + "0", nil // one digit too many: close, but not the same number
	}
	return v, err
}

// Every legacy row — including a REVOKED one, which migration 003's trigger
// would refuse to UPDATE — gets a verified token row; bank_connections is not
// touched; and each row then reads back tokenized, with the real number's
// masked form, exactly as a connection created tokenized does.
func TestBackfill_TokenizesEveryLegacyRow(t *testing.T) {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	ctx := context.Background()
	a := legacyConnection(t, app, "GB29NWBK60161331926819")
	b := legacyConnection(t, app, "DE89370400440532013000")
	revoked := legacyConnection(t, app, "FR1420041010050500013M02606")
	if _, err := admin.Exec(ctx, `UPDATE bank_connections SET status = 'REVOKED' WHERE connection_id = $1`, revoked.ConnectionID); err != nil {
		t.Fatal(err)
	}
	empty := uuid.NewString()
	if _, err := admin.Exec(ctx, `INSERT INTO bank_connections (connection_id, tenant_id, legal_entity_id, bank_name, account_number)
		VALUES ($1, $2, 'le-bf', 'BNK-02 request', '')`, empty, bfTenant); err != nil {
		t.Fatal(err)
	}

	tok := &scopeRecorder{Fake: &tokenization.Fake{}}
	report, err := store.BackfillAccountTokens(ctx, admin, tok, store.BackfillOptions{PrincipalID: "svc-tokenization-backfill"})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.Candidates != 4 || report.SkippedEmpty != 1 || report.Tokenized != 3 {
		t.Fatalf("report: %+v", report)
	}

	reader := store.NewPgStore(app)
	tenantCtx := middleware.WithTenant(ctx, bfTenant)
	for _, c := range []*domain.BankConnection{a, b, revoked} {
		raw := c.AccountNumber
		if got := rawColumn(t, admin, c.ConnectionID); got != raw {
			t.Fatalf("bank_connections.account_number changed for %s: %q", c.ConnectionID, got)
		}
		got, err := reader.GetConnectionByID(tenantCtx, c.ConnectionID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Tokenized() || got.AccountNumberMasked != domain.MaskAccountNumber(raw) {
			t.Fatalf("%s after backfill: tokenized=%v masked=%q", c.ConnectionID, got.Tokenized(), got.AccountNumberMasked)
		}
		back, err := tok.Fake.Detokenize(ctx, tokenization.Scope{TenantID: bfTenant, PrincipalID: "p", LegalEntityID: "le-bf"}, got.AccountNumberToken)
		if err != nil || back != raw {
			t.Fatalf("stored token detokenizes to %q, %v", back, err)
		}
		var source string
		_ = admin.QueryRow(ctx, `SELECT source FROM bank_connection_account_tokens WHERE connection_id = $1`, c.ConnectionID).Scan(&source)
		if source != "BACKFILL" {
			t.Fatalf("source %q", source)
		}
	}
	if got, _ := reader.GetConnectionByID(tenantCtx, empty); got.Tokenized() {
		t.Fatal("a connection with no account number was tokenized")
	}
	for _, s := range tok.scopes {
		if !strings.HasPrefix(s.IdempotencyKey, "tokenize-backfill:bank_connections:") ||
			!strings.HasSuffix(s.IdempotencyKey, ":svc-tokenization-backfill") || s.PrincipalID != "svc-tokenization-backfill" ||
			s.LegalEntityID != "le-bf" || s.TenantID != bfTenant {
			t.Fatalf("tokenize scope %+v", s)
		}
	}

	// Phase 5.4: a backfilled row reads exactly like one created tokenized.
	created := &domain.BankConnection{ConnectionID: uuid.NewString(), LegalEntityID: "le-bf", BankName: "New Bank",
		AccountNumber: "GB29NWBK60161331926819", IBAN: "GB29NWBK60161331926819", Currency: "GBP", Status: "ACTIVE"}
	if err := store.NewPgStore(app, store.WithEventRegion("uk"), store.WithTokenization(&tokenization.Fake{}, true)).
		CreateConnection(tenantCtx, created, "writer", "corr"); err != nil {
		t.Fatal(err)
	}
	fresh, _ := reader.GetConnectionByID(tenantCtx, created.ConnectionID)
	old, _ := reader.GetConnectionByID(tenantCtx, a.ConnectionID)
	if !fresh.Tokenized() || fresh.AccountNumberMasked != old.AccountNumberMasked {
		t.Fatalf("new vs backfilled read differ: %+v / %+v", fresh, old)
	}

	// Re-run: nothing left to do.
	again, err := store.BackfillAccountTokens(ctx, admin, tok, store.BackfillOptions{PrincipalID: "svc-tokenization-backfill"})
	if err != nil || again.Candidates != 1 || again.Tokenized != 0 { // only the empty one remains a candidate
		t.Fatalf("re-run: %+v, %v", again, err)
	}
}

// One row that does not round-trip stops the whole backfill — including
// rows before it that verified fine. Nothing is written.
func TestBackfill_RoundTripMismatchWritesNothing(t *testing.T) {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	legacyConnection(t, app, "GB29NWBK60161331926819")
	legacyConnection(t, app, "DE89370400440532013000")

	tok := &scopeRecorder{Fake: &tokenization.Fake{}, corrupts: "DE89370400440532013000"}
	_, err := store.BackfillAccountTokens(context.Background(), admin, tok, store.BackfillOptions{PrincipalID: "p"})
	if !errors.Is(err, store.ErrBackfillRoundTrip) {
		t.Fatalf("err = %v, want ErrBackfillRoundTrip", err)
	}
	if n := tokenRows(t, admin); n != 0 {
		t.Fatalf("%d token rows written by a failed backfill", n)
	}
}

// A refused tokenize (here: the vault is down) also stops it with nothing
// written.
func TestBackfill_TokenizeFailureWritesNothing(t *testing.T) {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	legacyConnection(t, app, "GB29NWBK60161331926819")
	_, err := store.BackfillAccountTokens(context.Background(), admin,
		&tokenization.Fake{Err: tokenization.ErrTokenizationServiceUnavailable}, store.BackfillOptions{PrincipalID: "p"})
	if !errors.Is(err, tokenization.ErrTokenizationServiceUnavailable) || tokenRows(t, admin) != 0 {
		t.Fatalf("err = %v, rows = %d", err, tokenRows(t, admin))
	}
}

// A role subject to RLS would see no tenant's rows and "succeed" having done
// nothing; the backfill refuses it outright.
func TestBackfill_RefusesAnRLSBoundRole(t *testing.T) {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	legacyConnection(t, app, "GB29NWBK60161331926819")
	if _, err := store.BackfillAccountTokens(context.Background(), app, &tokenization.Fake{},
		store.BackfillOptions{PrincipalID: "p"}); !errors.Is(err, store.ErrBackfillNeedsAllRows) {
		t.Fatalf("err = %v, want ErrBackfillNeedsAllRows", err)
	}
}

func TestBackfill_DryRunVerifiesAndWritesNothing(t *testing.T) {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	legacyConnection(t, app, "GB29NWBK60161331926819")
	fake := &tokenization.Fake{}
	report, err := store.BackfillAccountTokens(context.Background(), admin, fake, store.BackfillOptions{PrincipalID: "p", DryRun: true})
	if err != nil || report.Candidates != 1 || report.Tokenized != 0 || fake.Calls() != 2 || tokenRows(t, admin) != 0 {
		t.Fatalf("dry run: %+v, %v, calls %d, rows %d", report, err, fake.Calls(), tokenRows(t, admin))
	}
}

// An enforced create writes the token row in the same transaction, and no
// raw value reaches the event — account_number OR its iban alias.
func TestEnforcedCreate_WritesTokenRowAndNoRawAnywhere(t *testing.T) {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	ctx := middleware.WithTenant(context.Background(), bfTenant)
	c := &domain.BankConnection{ConnectionID: uuid.NewString(), LegalEntityID: "le-bf", BankName: "New Bank",
		AccountNumber: "GB29NWBK60161331926819", IBAN: "GB29NWBK60161331926819", Currency: "GBP", Status: "ACTIVE"}
	if err := store.NewPgStore(app, store.WithEventRegion("uk"), store.WithTokenization(&tokenization.Fake{}, true)).
		CreateConnection(ctx, c, "writer", "corr"); err != nil {
		t.Fatal(err)
	}
	var token, masked, source string
	if err := admin.QueryRow(context.Background(), `SELECT account_number_token, account_number_masked, source
		FROM bank_connection_account_tokens WHERE connection_id = $1`, c.ConnectionID).Scan(&token, &masked, &source); err != nil {
		t.Fatalf("no token row: %v", err)
	}
	if token == "" || token != rawColumn(t, admin, c.ConnectionID) || masked != "******************6819" || source != "WRITE" {
		t.Fatalf("token row: %q %q %q (account_number %q)", token, masked, source, rawColumn(t, admin, c.ConnectionID))
	}
	var payload []byte
	if err := admin.QueryRow(context.Background(), `SELECT payload FROM eventing_outbox WHERE aggregate_id = $1`, c.ConnectionID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(payload, &ev)
	if strings.Contains(string(payload), "GB29NWBK60161331926819") || ev.Data["iban"] != token || ev.Data["account_number"] != token {
		t.Fatalf("event payload carries a raw value: %s", payload)
	}
}

// A token row is set once: neither the app role nor the table owner can
// change or remove it.
func TestAccountTokenRowsAreAppendOnly(t *testing.T) {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	c := legacyConnection(t, app, "GB29NWBK60161331926819")
	if _, err := store.BackfillAccountTokens(context.Background(), admin, &tokenization.Fake{}, store.BackfillOptions{PrincipalID: "p"}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE bank_connection_account_tokens SET account_number_token = 'faketok_other' WHERE connection_id = $1`,
		`DELETE FROM bank_connection_account_tokens WHERE connection_id = $1`,
	} {
		if _, err := admin.Exec(context.Background(), stmt, c.ConnectionID); err == nil {
			t.Fatalf("%q succeeded", stmt)
		}
	}
}
