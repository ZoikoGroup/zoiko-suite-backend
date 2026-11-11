package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
	"zoiko.io/banking-connector-svc/internal/tokenization"
)

const rawAccountNumber = "GB29NWBK60161331926819"

func newConnection() *domain.BankConnection {
	return &domain.BankConnection{ConnectionID: uuid.NewString(), LegalEntityID: "le-tok", BankName: "Token Bank",
		BIC: "NWBKGB2L", AccountNumber: rawAccountNumber, Currency: "GBP", Status: "ACTIVE"}
}

// storedAccountNumber is the bank_connections.account_number actually
// written, read as the superuser so RLS cannot hide a row.
func storedAccountNumber(t *testing.T, admin *pgxpool.Pool, connectionID string) (string, bool) {
	t.Helper()
	var v string
	err := admin.QueryRow(context.Background(), `SELECT account_number FROM bank_connections WHERE connection_id = $1`, connectionID).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

// eventAccountNumber is account_number as the connection.created event
// carries it, and how many such events exist.
func eventAccountNumber(t *testing.T, admin *pgxpool.Pool, connectionID string) (string, int) {
	t.Helper()
	rows, err := admin.Query(context.Background(), `SELECT payload FROM eventing_outbox WHERE aggregate_id = $1`, connectionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var acct string
	n := 0
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var body struct {
			Data struct {
				AccountNumber string `json:"account_number"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatal(err)
		}
		acct = body.Data.AccountNumber
		n++
	}
	return acct, n
}

// With TOKENIZATION_ENFORCED on, the token — not the raw number — is what
// lands in bank_connections.account_number, in the event and in the returned
// connection; and only the same tenant can turn it back.
func TestTokenization_EnforcedStoresTheTokenNotTheRawValue(t *testing.T) {
	admin := openAdminPool(t)
	fake := &tokenization.Fake{}
	s := store.NewPgStore(appRolePool(t, admin), store.WithEventRegion("uk"), store.WithTokenization(fake, true))
	ctx := middleware.WithTenant(context.Background(), "tenant-tok-a")

	c := newConnection()
	if err := s.CreateConnection(ctx, c, "actor-1", "corr-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	stored, ok := storedAccountNumber(t, admin, c.ConnectionID)
	if !ok {
		t.Fatal("no row written")
	}
	if stored == rawAccountNumber {
		t.Fatal("the raw account number was stored with tokenization enforced")
	}
	if c.AccountNumber != stored {
		t.Fatalf("returned connection carries %q, the database %q", c.AccountNumber, stored)
	}
	if ev, n := eventAccountNumber(t, admin, c.ConnectionID); n != 1 || ev != stored {
		t.Fatalf("event carries %q (%d events), want the token", ev, n)
	}
	scope := tokenization.Scope{TenantID: "tenant-tok-a", PrincipalID: "auditor"}
	if v, err := fake.Detokenize(context.Background(), scope, stored); err != nil || v != rawAccountNumber {
		t.Fatalf("stored token detokenizes to %q, %v", v, err)
	}
	other := tokenization.Scope{TenantID: "tenant-tok-b", PrincipalID: "auditor"}
	if _, err := fake.Detokenize(context.Background(), other, stored); !errors.Is(err, tokenization.ErrDetokenizationDenied) {
		t.Fatalf("another tenant detokenized the stored token: %v", err)
	}
}

// A refused or impossible tokenization refuses the whole write: no
// connection row and no event — never a raw value written as a fallback.
func TestTokenization_FailedTokenizeWritesNothing(t *testing.T) {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	ctx := middleware.WithTenant(context.Background(), "tenant-tok-a")
	cases := map[string]struct {
		store *store.PgStore
		want  error
	}{
		"service unavailable": {store.NewPgStore(app, store.WithEventRegion("uk"),
			store.WithTokenization(&tokenization.Fake{Err: tokenization.ErrTokenizationServiceUnavailable}, true)),
			tokenization.ErrTokenizationServiceUnavailable},
		"enforced with no tokenizer": {store.NewPgStore(app, store.WithEventRegion("uk"), store.WithTokenization(nil, true)),
			store.ErrTokenizationNotConfigured},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newConnection()
			id := c.ConnectionID
			if err := tc.store.CreateConnection(ctx, c, "actor-1", "corr-1"); !errors.Is(err, tc.want) {
				t.Fatalf("create = %v, want %v", err, tc.want)
			}
			if v, ok := storedAccountNumber(t, admin, id); ok {
				t.Fatalf("a row was written (account_number %q)", v)
			}
			if _, n := eventAccountNumber(t, admin, id); n != 0 {
				t.Fatalf("%d events were written", n)
			}
		})
	}
}

// Regression: with TOKENIZATION_ENFORCED off, the write is byte-for-byte what
// it is today — the same account_number column and event as a store built
// without any tokenization option — and the tokenizer is never called.
func TestTokenization_NotEnforcedIsUnchanged(t *testing.T) {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	ctx := middleware.WithTenant(context.Background(), "tenant-tok-a")
	fake := &tokenization.Fake{}

	today := newConnection()
	if err := store.NewPgStore(app, store.WithEventRegion("uk")).CreateConnection(ctx, today, "actor-1", "corr-1"); err != nil {
		t.Fatal(err)
	}
	flagOff := newConnection()
	if err := store.NewPgStore(app, store.WithEventRegion("uk"), store.WithTokenization(fake, false)).
		CreateConnection(ctx, flagOff, "actor-1", "corr-1"); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]*domain.BankConnection{"no option": today, "enforced=false": flagOff} {
		stored, _ := storedAccountNumber(t, admin, c.ConnectionID)
		ev, n := eventAccountNumber(t, admin, c.ConnectionID)
		if stored != rawAccountNumber || ev != rawAccountNumber || n != 1 || c.AccountNumber != rawAccountNumber {
			t.Fatalf("%s: stored %q, event %q (%d), returned %q — want today's raw value everywhere", name, stored, ev, n, c.AccountNumber)
		}
	}
	if fake.Calls() != 0 {
		t.Fatalf("the tokenizer was called %d times with enforcement off", fake.Calls())
	}
}
