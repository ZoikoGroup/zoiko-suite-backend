//go:build integration

package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/bank-reconciliation-svc/internal/banking"
	"zoiko.io/bank-reconciliation-svc/internal/domain"
	"zoiko.io/bank-reconciliation-svc/internal/envelope"
)

// TestCanonicalMatch_AgainstRealBankingConnector is the cross-process proof
// of SPEC_DEVIATIONS C2: this service's real handler and real
// banking.HTTPClient, calling a real banking-connector-svc process whose
// GET /v1/banking/canonical-transactions/{id} genuinely refuses a request
// with no principal. Before the fix the match below answered 503
// banking_connector_unavailable.
//
// It needs a running banking-connector-svc and that service's database, so
// it runs only when both are supplied:
//
//	BANKING_CONNECTOR_E2E_URL           e.g. http://127.0.0.1:18145
//	BANKING_CONNECTOR_E2E_DATABASE_URL  the connector's DATABASE_URL, migrated
//
// The test seeds its own canonical transaction (under a fresh tenant), so it
// is repeatable against the same database.
func TestCanonicalMatch_AgainstRealBankingConnector(t *testing.T) {
	baseURL := os.Getenv("BANKING_CONNECTOR_E2E_URL")
	dsn := os.Getenv("BANKING_CONNECTOR_E2E_DATABASE_URL")
	if baseURL == "" || dsn == "" {
		t.Skip("BANKING_CONNECTOR_E2E_URL / BANKING_CONNECTOR_E2E_DATABASE_URL not set")
	}
	ctx := context.Background()
	tenant := fmt.Sprintf("tenant-c2-e2e-%d", time.Now().UnixNano())
	txnID := seedConnectorCanonical(t, ctx, dsn, tenant, 125.50, "USD")

	t.Run("connector refuses a principal-less read", func(t *testing.T) {
		// What banking.HTTPClient sent before the fix: tenant only.
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/banking/canonical-transactions/"+txnID, nil)
		req.Header.Set("X-Tenant-Id", tenant)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("call connector: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected the real connector to answer 401 without X-Principal-Id, got %d", resp.StatusCode)
		}
	})

	t.Run("client reads the canonical transaction", func(t *testing.T) {
		// The envelope this service's own middleware puts on every request
		// context; the client propagates its correlation and request ids,
		// which a connector running ZS_ENVELOPE_ENFORCEMENT=strict demands.
		ectx := envelope.WithEnvelope(ctx, envelope.Envelope{CorrelationID: "corr-c2-e2e", RequestID: "req-c2-e2e"})
		got, err := banking.NewHTTPClient(baseURL).GetCanonicalTransaction(ectx, tenant, "recon-matcher", txnID)
		if err != nil {
			t.Fatalf("GetCanonicalTransaction: %v", err)
		}
		if got.TransactionID != txnID || got.TenantID != tenant || got.Amount != 125.50 ||
			got.Currency != "USD" || got.Status != "NORMALIZED" {
			t.Fatalf("unexpected canonical transaction: %+v", got)
		}
	})

	t.Run("statement-line match verifies through the connector", func(t *testing.T) {
		s := newStubStore()
		s.lines["sl-e2e"] = &domain.StatementLine{
			StatementLineID: "sl-e2e", TenantID: tenant, LegalEntityID: "e1",
			Amount: 125.50, CurrencyCode: "USD", Status: domain.StatementLineStatusUnmatched,
		}
		r := newRouterWithBanking(s, banking.NewHTTPClient(baseURL))
		// The headers this service's own envelope policy already requires
		// on a write, so every real match request carries them.
		body, _ := json.Marshal(domain.MatchStatementLineRequest{TransactionID: txnID})
		req := httptest.NewRequest(http.MethodPost, "/v1/statement-lines/sl-e2e/match", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant-Id", tenant)
		req.Header.Set("X-Principal-Id", "recon-matcher")
		req.Header.Set("X-Correlation-ID", "corr-c2-e2e")
		req.Header.Set("X-Request-Id", "req-c2-e2e")
		req.Header.Set("X-Source-Channel", "api")
		req.Header.Set("Idempotency-Key", "idem-c2-e2e")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		l := s.lines["sl-e2e"]
		if l.Status != domain.StatementLineStatusMatched || l.MatchedTransactionID == nil || *l.MatchedTransactionID != txnID {
			t.Fatalf("expected the line MATCHED to %s, got %+v", txnID, l)
		}
	})
}

// seedConnectorCanonical writes the minimum BNK-02/03/04 chain the
// connector's canonical read needs — connection, statement, line, NORMALIZED
// canonical transaction — directly into its database.
func seedConnectorCanonical(t *testing.T, ctx context.Context, dsn, tenant string, amount float64, currency string) string {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to connector database: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	connID, stmtID, lineID := tenant+"-conn", tenant+"-stmt", tenant+"-line"
	if _, err := tx.Exec(ctx, `INSERT INTO bank_connections (connection_id, tenant_id, legal_entity_id, bank_name, account_number, currency)
		VALUES ($1, $2, 'e1', 'Test Bank', 'GB00TEST0000000001', $3)`, connID, tenant, currency); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bank_statements (statement_id, connection_id, tenant_id, statement_format, statement_date)
		VALUES ($1, $2, $3, 'BAI2', now())`, stmtID, connID, tenant); err != nil {
		t.Fatalf("seed statement: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bank_statement_lines (line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency)
		VALUES ($1, $2, $3, 1, now(), $4, $5)`, lineID, stmtID, tenant, amount, currency); err != nil {
		t.Fatalf("seed statement line: %v", err)
	}
	var txnID string
	if err := tx.QueryRow(ctx, `INSERT INTO bank_transactions_canonical (tenant_id, statement_line_id, transaction_date, amount, currency, status)
		VALUES ($1, $2, now(), $3, $4, 'NORMALIZED') RETURNING transaction_id`, tenant, lineID, amount, currency).Scan(&txnID); err != nil {
		t.Fatalf("seed canonical transaction: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return txnID
}
