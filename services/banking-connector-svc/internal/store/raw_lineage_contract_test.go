package store_test

// BNK-04's three previously-missing queries (A20: GetRawLineage,
// GetNormalizationVersion, ListNormalizationExceptions), against the real
// handler, the cmd/server middleware stack and a real Postgres under
// row-level security. Skips (not fails) if TEST_DATABASE_URL isn't set — see
// list_transactions_contract_test.go for the harness this file reuses
// (listFixture, listEnv, recordingAuthz, newListEnv).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/handler"
)

func (e *listEnv) get(t *testing.T, tenant, path string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", "recon-intel-svc")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v: %s", err, rec.Body.String())
		}
	}
	return rec.Code, out, rec.Body.String()
}

// TestGetRawLineage_TracesToRawEvidence proves the response actually names
// the raw statement-line evidence (raw_reference, bank_code, the statement's
// content_hash) a canonical transaction was derived from, not just the
// canonical row's own fields GetCanonicalTransaction already returns.
func TestGetRawLineage_TracesToRawEvidence(t *testing.T) {
	e := newListEnv(t)
	tenant := uuid.NewString()
	ctx := context.Background()
	// Not e.f.statement(): bank_statements' terminal-status trigger
	// (migration 004) refuses an UPDATE once a row is past
	// RECEIVED/VALIDATING, and the default status is already ACCEPTED — so
	// content_hash/source_id must be set at INSERT time, not patched in
	// afterward.
	connID, stmtID := uuid.NewString(), uuid.NewString()
	if _, err := e.f.admin.Exec(ctx, `INSERT INTO bank_connections (connection_id, tenant_id, legal_entity_id, bank_name, account_number, currency, bank_account_id)
		VALUES ($1, $2, 'le-1', 'Test Bank', 'GB00TEST', 'USD', 'ba-1')`, connID, tenant); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	if _, err := e.f.admin.Exec(ctx, `INSERT INTO bank_statements (statement_id, connection_id, tenant_id, statement_format, statement_date, content_hash, source_id)
		VALUES ($1, $2, $3, 'BAI2', '2026-07-31', 'hash-xyz', 'src-9')`, stmtID, connID, tenant); err != nil {
		t.Fatalf("seed statement: %v", err)
	}
	lineID, txnID := uuid.NewString(), uuid.NewString()
	if _, err := e.f.admin.Exec(ctx, `INSERT INTO bank_statement_lines (line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, raw_reference, bank_code)
		VALUES ($1,$2,$3,1,'2026-07-15T12:00:00Z',10,'USD','RAW-REF-123','BC-1')`, lineID, stmtID, tenant); err != nil {
		t.Fatalf("seed line: %v", err)
	}
	if _, err := e.f.admin.Exec(ctx, `INSERT INTO bank_transactions_canonical (transaction_id, tenant_id, statement_line_id, mapping_version, transaction_date, amount, currency, status)
		VALUES ($1,$2,$3,1,'2026-07-15T12:00:00Z',10,'USD','NORMALIZED')`, txnID, tenant, lineID); err != nil {
		t.Fatalf("seed canonical txn: %v", err)
	}

	code, resp, body := e.get(t, tenant, "/v1/banking/canonical-transactions/"+txnID+"/raw-lineage")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if resp["raw_reference"] != "RAW-REF-123" || resp["bank_code"] != "BC-1" {
		t.Fatalf("raw line evidence missing: %v", resp)
	}
	if resp["statement_content_hash"] != "hash-xyz" || resp["statement_source_id"] != "src-9" {
		t.Fatalf("statement evidence missing: %v", resp)
	}
	if resp["legal_entity_id"] != "le-1" || resp["transaction_id"] != txnID {
		t.Fatalf("identity fields missing: %v", resp)
	}
	e.authz.mu.Lock()
	asked := append([][2]string(nil), e.authz.asked...)
	e.authz.mu.Unlock()
	if len(asked) == 0 || asked[len(asked)-1] != [2]string{handler.BANK_TRANSACTION_READ, "le-1"} {
		t.Fatalf("expected a bank.transaction.read decision on le-1, got %v", asked)
	}

	if code, _, _ := e.get(t, tenant, "/v1/banking/canonical-transactions/"+uuid.NewString()+"/raw-lineage"); code != http.StatusNotFound {
		t.Fatalf("unknown transaction: expected 404, got %d", code)
	}
}

// TestGetRawLineage_DeniedByAuthz proves the legal entity resolved from the
// row (not one the caller supplies) is what gets checked, and a denial
// returns nothing.
func TestGetRawLineage_DeniedByAuthz(t *testing.T) {
	e := newListEnv(t)
	tenant := uuid.NewString()
	txnID := e.f.txn(seededTxn{tenant: tenant, entity: "le-2", bankAccount: "ba-1", currency: "USD", amount: "10",
		status: domain.TxnNormalized, date: day(15, 12, 0, 0)})

	e.authz.deny[[2]string{handler.BANK_TRANSACTION_READ, "le-2"}] = true
	code, resp, _ := e.get(t, tenant, "/v1/banking/canonical-transactions/"+txnID+"/raw-lineage")
	if code != http.StatusForbidden || resp != nil {
		t.Fatalf("expected 403 and nothing returned, got %d: %v", code, resp)
	}
}

// TestGetNormalizationVersion_MapsAcrossReNormalization proves
// mapping_version advances on a re-normalization and the honest caveat
// about what it actually means (A19) rides along with every response.
func TestGetNormalizationVersion_MapsAcrossReNormalization(t *testing.T) {
	e := newListEnv(t)
	tenant := uuid.NewString()
	ctx := context.Background()
	stmtID := e.f.statement(tenant, "le-1", "ba-1")
	lineID, priorID, newID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := e.f.admin.Exec(ctx, `INSERT INTO bank_statement_lines (line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency)
		VALUES ($1,$2,$3,1,'2026-07-15T12:00:00Z',10,'USD')`, lineID, stmtID, tenant); err != nil {
		t.Fatalf("seed line: %v", err)
	}
	// The new row must exist before the prior row's superseded_by can point
	// at it: each admin.Exec is its own implicit transaction, so the FK
	// (DEFERRABLE INITIALLY DEFERRED for ReNormalizeTransaction's own
	// single-transaction write order) is still checked at this statement's
	// own commit, not deferred across these two separate calls.
	if _, err := e.f.admin.Exec(ctx, `INSERT INTO bank_transactions_canonical (transaction_id, tenant_id, statement_line_id, mapping_version, transaction_date, amount, currency, status)
		VALUES ($1,$2,$3,2,'2026-07-15T12:00:00Z',10,'USD','NORMALIZED')`, newID, tenant, lineID); err != nil {
		t.Fatalf("seed re-normalized txn: %v", err)
	}
	if _, err := e.f.admin.Exec(ctx, `INSERT INTO bank_transactions_canonical (transaction_id, tenant_id, statement_line_id, mapping_version, transaction_date, amount, currency, status, superseded_by)
		VALUES ($1,$2,$3,1,'2026-07-15T12:00:00Z',10,'USD','SUPERSEDED',$4)`, priorID, tenant, lineID, newID); err != nil {
		t.Fatalf("seed prior txn: %v", err)
	}

	code, resp, body := e.get(t, tenant, "/v1/banking/canonical-transactions/"+newID+"/normalization-version")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if mv, ok := resp["mapping_version"].(float64); !ok || int(mv) != 2 {
		t.Fatalf("expected mapping_version 2, got %v", resp["mapping_version"])
	}
	caveat, _ := resp["caveat"].(string)
	if caveat != domain.MappingVersionCaveat {
		t.Fatalf("caveat must be the fixed A19 text, got %q", caveat)
	}

	code, resp, body = e.get(t, tenant, "/v1/banking/canonical-transactions/"+priorID+"/normalization-version")
	if code != http.StatusOK {
		t.Fatalf("prior row: expected 200, got %d: %s", code, body)
	}
	if mv, ok := resp["mapping_version"].(float64); !ok || int(mv) != 1 {
		t.Fatalf("prior row must keep its own mapping_version 1, got %v", resp["mapping_version"])
	}
	if resp["status"] != "SUPERSEDED" {
		t.Fatalf("prior row status must be SUPERSEDED, got %v", resp["status"])
	}
}

type exceptionsResponse struct {
	Exceptions []map[string]any `json:"exceptions"`
	NextCursor string           `json:"next_cursor"`
}

func (e *listEnv) listExceptionsAs(t *testing.T, tenant, principal string, params url.Values) (int, exceptionsResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/banking/mapping-exceptions?"+params.Encode(), nil)
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", principal)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	var out exceptionsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v: %s", err, rec.Body.String())
		}
	}
	return rec.Code, out, rec.Body.String()
}

func (e *listEnv) listExceptions(t *testing.T, tenant string, params url.Values) (int, exceptionsResponse, string) {
	t.Helper()
	return e.listExceptionsAs(t, tenant, "ops-1", params)
}

// TestListMappingExceptions_FiltersAndIsolatesByTenant proves the status
// filter narrows the result, an unfiltered call returns every status, and
// another tenant's exceptions never appear — the platform-scoped (no
// legal_entity_id) read is still RLS-isolated by tenant.
func TestListMappingExceptions_FiltersAndIsolatesByTenant(t *testing.T) {
	e := newListEnv(t)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	ctx := context.Background()
	lineSeq := map[string]int{}
	seed := func(tenant, status string) string {
		stmtID := e.f.statement(tenant, "le-1", "ba-1")
		lineSeq[tenant]++
		lineID, excID := uuid.NewString(), uuid.NewString()
		if _, err := e.f.admin.Exec(ctx, `INSERT INTO bank_statement_lines (line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency)
			VALUES ($1,$2,$3,$4,'2026-07-15T12:00:00Z',10,'USD')`, lineID, stmtID, tenant, lineSeq[tenant]); err != nil {
			t.Fatalf("seed line: %v", err)
		}
		if _, err := e.f.admin.Exec(ctx, `INSERT INTO mapping_exceptions (exception_id, tenant_id, statement_line_id, reason, status, raised_by_principal_id)
			VALUES ($1,$2,$3,'no mapping','OPEN','maker-1')`, excID, tenant, lineID); err != nil {
			t.Fatalf("seed exception: %v", err)
		}
		if status != "OPEN" {
			if _, err := e.f.admin.Exec(ctx, `UPDATE mapping_exceptions SET status=$2, resolved_by_principal_id='checker-1', resolved_at=now() WHERE exception_id=$1`, excID, status); err != nil {
				t.Fatalf("resolve exception: %v", err)
			}
		}
		return excID
	}
	openA := seed(tenantA, "OPEN")
	approvedA := seed(tenantA, "APPROVED")
	openB := seed(tenantB, "OPEN")

	code, resp, body := e.listExceptions(t, tenantA, url.Values{})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	seen := map[string]bool{}
	for _, exc := range resp.Exceptions {
		seen[exc["exception_id"].(string)] = true
		if exc["tenant_id"] != tenantA {
			t.Fatalf("row of another tenant returned: %v", exc)
		}
	}
	if !seen[openA] || !seen[approvedA] || seen[openB] || len(seen) != 2 {
		t.Fatalf("tenant A must see exactly its own two exceptions; got %v", seen)
	}

	code, resp, body = e.listExceptions(t, tenantA, url.Values{"status": {"OPEN"}})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if len(resp.Exceptions) != 1 || resp.Exceptions[0]["exception_id"] != openA {
		t.Fatalf("status=OPEN must return only the open one; got %v", resp.Exceptions)
	}
	code, approved, body := e.listExceptions(t, tenantA, url.Values{"status": {"APPROVED"}})
	if code != http.StatusOK || len(approved.Exceptions) != 1 || approved.Exceptions[0]["exception_id"] != approvedA {
		t.Fatalf("status=APPROVED must return only the approved one; got %d %v %s", code, approved.Exceptions, body)
	}
	if approved.Exceptions[0]["resolved_by_principal_id"] != "checker-1" {
		t.Fatalf("resolution evidence must be included: %v", approved.Exceptions[0])
	}

	if code, _, _ := e.listExceptions(t, tenantA, url.Values{"status": {"BOGUS"}}); code != http.StatusBadRequest {
		t.Fatalf("invalid status: expected 400, got %d", code)
	}

	code, respB, _ := e.listExceptions(t, tenantB, url.Values{})
	if code != http.StatusOK || len(respB.Exceptions) != 1 || respB.Exceptions[0]["exception_id"] != openB {
		t.Fatalf("tenant B must see only its own exception; got %v", respB.Exceptions)
	}
}

// TestListMappingExceptions_Authorization proves the platform-level
// mapping.exception.approve decision (empty legal_entity_id) is what gates
// this route, the same posture as ApproveMappingException.
func TestListMappingExceptions_Authorization(t *testing.T) {
	e := newListEnv(t)
	tenant := uuid.NewString()

	if code, _, _ := e.listExceptions(t, tenant, url.Values{}); code != http.StatusOK {
		t.Fatalf("granted: expected 200, got %d", code)
	}
	e.authz.mu.Lock()
	asked := append([][2]string(nil), e.authz.asked...)
	e.authz.mu.Unlock()
	if len(asked) == 0 || asked[len(asked)-1] != [2]string{handler.BANKING_MAPPING_EXCEPTION_APPROVE, ""} {
		t.Fatalf("expected a mapping.exception.approve decision with no legal entity, got %v", asked)
	}

	// A different principal: CheckAllowed's cache key is
	// principal|legal_entity|action, and "ops-1"'s GRANTED decision above is
	// still within decisionCacheTTL — reusing it would read the cache, not
	// the deny this sets, and the request would stay the same key otherwise.
	e.authz.deny[[2]string{handler.BANKING_MAPPING_EXCEPTION_APPROVE, ""}] = true
	if code, resp, _ := e.listExceptionsAs(t, tenant, "ops-2", url.Values{}); code != http.StatusForbidden || len(resp.Exceptions) != 0 {
		t.Fatalf("denied: expected 403 and nothing returned, got %d", code)
	}
}
