package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/purchase-request-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-request-svc/internal/middleware"
	"zoiko.io/purchase-request-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migrations from a
// clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set — same
// convention as every other service in this platform.
//
// WARNING, and the reason for the guard below: this DROPs purchase_requests.
// Point TEST_DATABASE_URL at the `purchase_request` database that a running
// purchase-request-svc uses and it will silently delete that register. That
// happened once to accounts-payable during this platform's console work and the
// loss looks like a service bug afterwards, not like a test — so the database
// name is checked before anything is dropped.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	requireThrowawayDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS purchase_requests CASCADE;`)

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_add_idempotency_index.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations", migration))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", migration, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", migration, err)
		}
	}

	return pool
}

// requireThrowawayDatabase fails the test unless the DSN's database name marks
// it as disposable. Two names are legitimate: CI points TEST_DATABASE_URL at
// `testdb`, and the local convention is `purchase_request_test`. Both contain
// "test"; the live `purchase_request` database does not, which is the case this
// guard exists to catch.
//
// The name is taken from the parsed DSN rather than matched against the whole
// string, so a host or password that happens to contain "test" cannot vouch for
// a live database. Anything that does not parse as a URL is refused rather than
// waved through — the suite DROPs tables, so an unreadable target is not a
// target worth guessing at. Same helper as the other store suites here.
func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably "+
			"disposable, and this suite DROPs purchase_requests. Use purchase_request_test "+
			"(or CI's testdb), not purchase_request.", dbName)
	}
}

func newTestRequest(tenantID string) *domain.PurchaseRequest {
	return &domain.PurchaseRequest{
		RequestID:              uuid.New().String(),
		TenantID:               tenantID,
		LegalEntityID:          uuid.New().String(),
		RequestedByPrincipalID: "test-requester",
		Description:            "40 laptops for the Dublin office",
		Amount:                 48000,
		CurrencyCode:           "EUR",
		Status:                 domain.RequestStatusPending,
		CorrelationID:          "corr-" + uuid.New().String(),
	}
}

func TestPgStore_CreateRequest_And_GetRequest(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	req := newTestRequest(tenantID)
	created, err := s.CreateRequest(ctx, req)
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for a first insert")
	}

	got, err := s.GetRequest(ctx, req.RequestID)
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if got == nil {
		t.Fatal("expected to read back the request just created")
	}
	if got.Status != domain.RequestStatusPending {
		t.Errorf("status = %q, want PENDING — a new request authorises nothing", got.Status)
	}
	if got.Amount != req.Amount {
		t.Errorf("amount = %v, want %v", got.Amount, req.Amount)
	}
	if got.Description != req.Description {
		t.Errorf("description = %q, want %q", got.Description, req.Description)
	}
	// Every nullable decision column must still be empty on a PENDING row.
	// A request that reads as already-decided is the failure that matters here.
	if got.ApprovedByPrincipalID != nil || got.ApprovedAt != nil {
		t.Error("a PENDING request carries approval fields")
	}
	if got.RejectedByPrincipalID != nil || got.RejectedAt != nil || got.RejectionReason != nil {
		t.Error("a PENDING request carries rejection fields")
	}
}

// A retried POST (client timeout on a call that actually succeeded) must resolve
// to the ORIGINAL request rather than raising a second requisition for the same
// spend. The partial unique index on (tenant_id, correlation_id) is what makes
// that work, so this fails if 000002 is ever dropped.
func TestPgStore_CreateRequest_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first := newTestRequest(tenantID)
	created, err := s.CreateRequest(ctx, first)
	if err != nil || !created {
		t.Fatalf("first CreateRequest: created=%v err=%v", created, err)
	}

	replay := newTestRequest(tenantID)
	replay.CorrelationID = first.CorrelationID
	replay.Description = "a different description that must not be persisted"

	created, err = s.CreateRequest(ctx, replay)
	if err != nil {
		t.Fatalf("replayed CreateRequest: %v", err)
	}
	if created {
		t.Fatal("expected created=false on a replayed correlation_id")
	}
	if replay.RequestID != first.RequestID {
		t.Errorf("replay resolved to request_id %s, want the original %s",
			replay.RequestID, first.RequestID)
	}
	if replay.Description != first.Description {
		t.Errorf("replay returned description %q, want the original %q — the replay's "+
			"body must not overwrite the stored request", replay.Description, first.Description)
	}
}

// The same correlation_id under a different tenant is a different request. The
// index is partial on (tenant_id, correlation_id), so cross-tenant collision
// would be a tenant-isolation break, not merely a dedup bug.
func TestPgStore_CreateRequest_SameCorrelationDifferentTenant_Allowed(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	shared := "corr-" + uuid.New().String()

	a := newTestRequest(tenantA)
	a.CorrelationID = shared
	if created, err := s.CreateRequest(svcmiddleware.WithTenant(context.Background(), tenantA), a); err != nil || !created {
		t.Fatalf("tenant A create: created=%v err=%v", created, err)
	}

	b := newTestRequest(tenantB)
	b.CorrelationID = shared
	created, err := s.CreateRequest(svcmiddleware.WithTenant(context.Background(), tenantB), b)
	if err != nil {
		t.Fatalf("tenant B create: %v", err)
	}
	if !created {
		t.Fatal("tenant B's request was deduplicated against tenant A's — the idempotency " +
			"index must be scoped by tenant_id")
	}
}

func TestPgStore_GetRequest_OtherTenant_ReadsAsAbsent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	owner := uuid.New().String()

	req := newTestRequest(owner)
	if _, err := s.CreateRequest(svcmiddleware.WithTenant(context.Background(), owner), req); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}

	intruder := svcmiddleware.WithTenant(context.Background(), uuid.New().String())
	got, err := s.GetRequest(intruder, req.RequestID)
	if err != nil {
		t.Fatalf("cross-tenant GetRequest returned an error: %v", err)
	}
	if got != nil {
		t.Fatal("another tenant's request was readable")
	}
}

// request_id is a uuid column, so a mistyped id dies in the driver as 22P02
// before any row is examined. Unmapped that reached the caller as 503
// store_unavailable — a typo reported as the database being down. A malformed
// id cannot name an existing request, so it must read as absent.
func TestPgStore_GetRequest_MalformedUUID_ReadsAsAbsent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	ctx := svcmiddleware.WithTenant(context.Background(), uuid.New().String())

	got, err := s.GetRequest(ctx, "not-a-uuid")
	if err != nil {
		t.Fatalf("malformed request_id returned an error (%v) — it must read as absent, "+
			"because reporting it as a store failure is indistinguishable from an outage", err)
	}
	if got != nil {
		t.Fatal("malformed request_id somehow matched a row")
	}
}

func TestPgStore_TransitionRequest_MalformedUUID_IsNotFound(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	err := s.TransitionRequest(ctx, tenantID, "not-a-uuid", domain.RequestStatusApproved, "approver", nil)
	if !errors.Is(err, domain.ErrRequestNotFound) {
		t.Fatalf("err = %v, want ErrRequestNotFound — a malformed id names no request, so "+
			"answering invalid_transition would assert it exists in the wrong state", err)
	}
}

func TestPgStore_TransitionRequest_ApproveThenSecondDecision_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	req := newTestRequest(tenantID)
	if _, err := s.CreateRequest(ctx, req); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}

	if err := s.TransitionRequest(ctx, tenantID, req.RequestID, domain.RequestStatusApproved, "approver-1", nil); err != nil {
		t.Fatalf("first approval: %v", err)
	}

	got, err := s.GetRequest(ctx, req.RequestID)
	if err != nil || got == nil {
		t.Fatalf("GetRequest after approval: got=%v err=%v", got, err)
	}
	if got.Status != domain.RequestStatusApproved {
		t.Errorf("status = %q, want APPROVED", got.Status)
	}
	if got.ApprovedByPrincipalID == nil || *got.ApprovedByPrincipalID != "approver-1" {
		t.Error("approver was not recorded — who decided a request is the audit record")
	}
	if got.ApprovedAt == nil {
		t.Error("approved_at was not recorded")
	}

	// Both branches are terminal. A second decision must be refused rather than
	// applied, so who decided a request and when cannot be overwritten.
	err = s.TransitionRequest(ctx, tenantID, req.RequestID, domain.RequestStatusRejected, "approver-2", strPtr("changed my mind"))
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("second decision err = %v, want ErrInvalidTransition", err)
	}

	after, err := s.GetRequest(ctx, req.RequestID)
	if err != nil || after == nil {
		t.Fatalf("GetRequest after refused transition: got=%v err=%v", after, err)
	}
	if after.Status != domain.RequestStatusApproved {
		t.Errorf("status = %q after a refused transition, want APPROVED unchanged", after.Status)
	}
	if after.RejectedByPrincipalID != nil || after.RejectionReason != nil {
		t.Error("a refused transition still wrote rejection fields")
	}
}

func TestPgStore_TransitionRequest_RejectRecordsReason(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	req := newTestRequest(tenantID)
	if _, err := s.CreateRequest(ctx, req); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}

	reason := "outside the approved capex envelope"
	if err := s.TransitionRequest(ctx, tenantID, req.RequestID, domain.RequestStatusRejected, "approver-1", &reason); err != nil {
		t.Fatalf("reject: %v", err)
	}

	got, err := s.GetRequest(ctx, req.RequestID)
	if err != nil || got == nil {
		t.Fatalf("GetRequest: got=%v err=%v", got, err)
	}
	if got.Status != domain.RequestStatusRejected {
		t.Errorf("status = %q, want REJECTED", got.Status)
	}
	// The reason IS the audit record for the refusal — a rejection that loses it
	// records that someone said no but not why.
	if got.RejectionReason == nil || *got.RejectionReason != reason {
		t.Errorf("rejection_reason = %v, want %q", got.RejectionReason, reason)
	}
}

// A cross-tenant transition must not apply. Tenancy is enforced in the same
// atomic UPDATE as the state-machine check, so this also proves the WHERE
// clause carries tenant_id rather than relying on RLS, which a superuser
// connection bypasses.
func TestPgStore_TransitionRequest_OtherTenant_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	owner := uuid.New().String()
	intruder := uuid.New().String()

	req := newTestRequest(owner)
	if _, err := s.CreateRequest(svcmiddleware.WithTenant(context.Background(), owner), req); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}

	err := s.TransitionRequest(
		svcmiddleware.WithTenant(context.Background(), intruder),
		intruder, req.RequestID, domain.RequestStatusApproved, "intruder", nil)
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("cross-tenant transition err = %v, want it refused", err)
	}

	got, err := s.GetRequest(svcmiddleware.WithTenant(context.Background(), owner), req.RequestID)
	if err != nil || got == nil {
		t.Fatalf("GetRequest: got=%v err=%v", got, err)
	}
	if got.Status != domain.RequestStatusPending {
		t.Errorf("status = %q, want PENDING — another tenant approved this request", got.Status)
	}
}

func TestPgStore_ListRequests_FiltersAndTenantScope(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entity := uuid.New().String()

	pending := newTestRequest(tenantID)
	pending.LegalEntityID = entity
	if _, err := s.CreateRequest(ctx, pending); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	approved := newTestRequest(tenantID)
	approved.LegalEntityID = entity
	if _, err := s.CreateRequest(ctx, approved); err != nil {
		t.Fatalf("create approved: %v", err)
	}
	if err := s.TransitionRequest(ctx, tenantID, approved.RequestID, domain.RequestStatusApproved, "approver", nil); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// A second entity in the same tenant, to prove the entity filter narrows.
	other := newTestRequest(tenantID)
	if _, err := s.CreateRequest(ctx, other); err != nil {
		t.Fatalf("create other-entity: %v", err)
	}

	all, err := s.ListRequests(ctx, domain.ListRequestsFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("ListRequests unfiltered: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered list returned %d requests, want 3", len(all))
	}

	byStatus, err := s.ListRequests(ctx, domain.ListRequestsFilter{
		TenantID: tenantID, Status: string(domain.RequestStatusApproved),
	})
	if err != nil {
		t.Fatalf("ListRequests by status: %v", err)
	}
	if len(byStatus) != 1 || byStatus[0].RequestID != approved.RequestID {
		t.Fatalf("status filter returned %d requests, want exactly the approved one", len(byStatus))
	}

	byEntity, err := s.ListRequests(ctx, domain.ListRequestsFilter{
		TenantID: tenantID, LegalEntityID: entity,
	})
	if err != nil {
		t.Fatalf("ListRequests by entity: %v", err)
	}
	if len(byEntity) != 2 {
		t.Fatalf("entity filter returned %d requests, want 2", len(byEntity))
	}

	// Another tenant must see none of them, however the filter is set.
	empty, err := s.ListRequests(ctx, domain.ListRequestsFilter{TenantID: uuid.New().String()})
	if err != nil {
		t.Fatalf("ListRequests for a foreign tenant: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("a foreign tenant read %d requests", len(empty))
	}
}

func strPtr(s string) *string { return &s }
