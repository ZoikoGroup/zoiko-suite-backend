//go:build integration

package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/handler"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

func governanceRun(t *testing.T, s *store.PgStore, tenantID, entity, subledger, book, status, runBy string, at time.Time) *domain.SubledgerControlRun {
	t.Helper()
	run := &domain.SubledgerControlRun{
		ControlRunID:           uuid.NewString(),
		TenantID:               tenantID,
		LegalEntityID:          entity,
		FiscalPeriod:           "2026-10",
		Subledger:              subledger,
		BookID:                 book,
		ControlAccountCode:     "CTRL-" + subledger,
		SubledgerTotalAmount:   1200,
		GLControlBalanceAmount: 1000,
		DifferenceAmount:       200,
		Status:                 status,
		RunAt:                  at,
		RunByPrincipalID:       runBy,
	}
	if status == "MATCHED" {
		run.GLControlBalanceAmount = run.SubledgerTotalAmount
		run.DifferenceAmount = 0
	}
	require.NoError(t, s.CreateControlRun(
		svcmiddleware.WithTenant(context.Background(), tenantID), run, "corr-1", runBy))
	return run
}

func governanceException(t *testing.T, s *store.PgStore, tenantID string, run *domain.SubledgerControlRun) *domain.ControlException {
	t.Helper()
	exc, err := s.GetControlExceptionByRun(
		svcmiddleware.WithTenant(context.Background(), tenantID), run.ControlRunID)
	require.NoError(t, err)
	return exc
}

func TestControlException_ExceptionRunCreatesExactlyOneOpenRow(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	entity := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

	matched := governanceRun(t, s, tenantID, entity, "AR", "", "MATCHED", "preparer", at)
	var count int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM control_exceptions WHERE tenant_id = $1 AND control_run_id = $2`,
		tenantID, matched.ControlRunID).Scan(&count))
	assert.Zero(t, count, "a MATCHED run has no exception workflow")

	exception := governanceRun(t, s, tenantID, entity, "AR", "", "EXCEPTION", "preparer", at.Add(time.Minute))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM control_exceptions WHERE tenant_id = $1 AND control_run_id = $2`,
		tenantID, exception.ControlRunID).Scan(&count))
	assert.Equal(t, 1, count, "a run landing EXCEPTION gets exactly one companion row")

	exc := governanceException(t, s, tenantID, exception)
	assert.Equal(t, domain.ControlExceptionStatusOpen, exc.Status)
	assert.Equal(t, exception.ControlRunID, exc.ControlRunID)
	assert.Equal(t, entity, exc.LegalEntityID)
	assert.Equal(t, "2026-10", exc.FiscalPeriod)
	assert.Equal(t, "AR", exc.Subledger)
	assert.Empty(t, exc.AssignedToPrincipalID)
	assert.Nil(t, exc.ResolvedAt)
}

func TestControlException_AssignResolveStateMachine(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	entity := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	run := governanceRun(t, s, tenantID, entity, "AR", "", "EXCEPTION", "preparer", time.Now().UTC())
	exc := governanceException(t, s, tenantID, run)

	assigned, err := s.AssignException(ctx, exc.ControlExceptionID, "owner-a", "resolver", "corr-1")
	require.NoError(t, err)
	assert.Equal(t, domain.ControlExceptionStatusAssigned, assigned.Status)
	assert.Equal(t, "owner-a", assigned.AssignedToPrincipalID)
	assert.NotNil(t, assigned.AssignedAt)

	// Reassignment is allowed while the exception is still being worked.
	reassigned, err := s.AssignException(ctx, exc.ControlExceptionID, "owner-b", "resolver", "corr-2")
	require.NoError(t, err)
	assert.Equal(t, "owner-b", reassigned.AssignedToPrincipalID)

	resolved, err := s.RecordResolution(ctx, exc.ControlExceptionID, "ledger corrected and re-tied", "resolver", "corr-3")
	require.NoError(t, err)
	assert.Equal(t, domain.ControlExceptionStatusResolved, resolved.Status)
	assert.Equal(t, "ledger corrected and re-tied", resolved.ResolutionNote)
	assert.Equal(t, "resolver", resolved.ResolvedByPrincipalID)
	assert.NotNil(t, resolved.ResolvedAt)

	_, err = s.AssignException(ctx, exc.ControlExceptionID, "owner-c", "resolver", "corr-4")
	assert.ErrorIs(t, err, domain.ErrControlExceptionNotOpenOrAssigned,
		"a RESOLVED exception is closed history, not reassignable")

	_, err = s.RecordResolution(ctx, exc.ControlExceptionID, "again", "resolver", "corr-5")
	assert.ErrorIs(t, err, domain.ErrControlExceptionNotOpenOrAssigned)
}

func TestRecordResolution_RequiresANonEmptyNote(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	entity := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	run := governanceRun(t, s, tenantID, entity, "AP", "", "EXCEPTION", "preparer", time.Now().UTC())
	exc := governanceException(t, s, tenantID, run)

	for _, note := range []string{"", "   ", "\n\t"} {
		_, err := s.RecordResolution(ctx, exc.ControlExceptionID, note, "resolver", "corr-1")
		assert.ErrorIs(t, err, domain.ErrResolutionNoteRequired)
	}

	still, err := s.GetControlException(ctx, exc.ControlExceptionID)
	require.NoError(t, err)
	assert.Equal(t, domain.ControlExceptionStatusOpen, still.Status, "a refused resolution must not move the exception")

	resolved, err := s.RecordResolution(ctx, exc.ControlExceptionID, "bank statement matched", "resolver", "corr-2")
	require.NoError(t, err)
	assert.Equal(t, domain.ControlExceptionStatusResolved, resolved.Status)
}

func TestCertifyControlRun_MakerCheckerAndExceptionState(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	entity := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

	run := governanceRun(t, s, tenantID, entity, "AR", "", "EXCEPTION", "preparer", at)
	exc := governanceException(t, s, tenantID, run)

	// 1. OPEN exception: CERTIFIED is refused, FAILED is allowed.
	_, err := s.CertifyControlRun(ctx, run.ControlRunID, domain.ControlCertificationCertified, "ok", "certifier", "certifier", "corr-1")
	assert.ErrorIs(t, err, domain.ErrControlHasUnresolvedException)

	// 2. ASSIGNED is still unresolved.
	_, err = s.AssignException(ctx, exc.ControlExceptionID, "owner-1", "resolver", "corr-2")
	require.NoError(t, err)
	_, err = s.CertifyControlRun(ctx, run.ControlRunID, domain.ControlCertificationCertified, "ok", "certifier", "certifier", "corr-3")
	assert.ErrorIs(t, err, domain.ErrControlHasUnresolvedException)

	// 3. RESOLVED: certification is now allowed.
	_, err = s.RecordResolution(ctx, exc.ControlExceptionID, "corrected", "resolver", "corr-4")
	require.NoError(t, err)

	// 3a. The principal who ran the control cannot certify it.
	_, err = s.CertifyControlRun(ctx, run.ControlRunID, domain.ControlCertificationCertified, "self", "preparer", "preparer", "corr-5")
	assert.ErrorIs(t, err, domain.ErrControlSelfCertification)

	// 3b. The principal who resolved the exception cannot certify it either.
	_, err = s.CertifyControlRun(ctx, run.ControlRunID, domain.ControlCertificationCertified, "self", "resolver", "resolver", "corr-6")
	assert.ErrorIs(t, err, domain.ErrControlSelfCertification)

	// 3c. An independent certifier succeeds.
	certified, err := s.CertifyControlRun(ctx, run.ControlRunID, domain.ControlCertificationCertified, "reviewed", "certifier", "certifier", "corr-7")
	require.NoError(t, err)
	require.NotNil(t, certified.CertificationStatus)
	assert.Equal(t, domain.ControlCertificationCertified, *certified.CertificationStatus)
	require.NotNil(t, certified.CertifiedAt)
	require.NotNil(t, certified.CertifiedByPrincipalID)
	assert.Equal(t, "certifier", *certified.CertifiedByPrincipalID)
	assert.Equal(t, "reviewed", certified.CertificationNote)
	assert.Equal(t, "EXCEPTION", certified.Status, "certification must not overwrite the run's own finding")

	// 4. A decision is final: no second certification or rejection.
	_, err = s.CertifyControlRun(ctx, run.ControlRunID, domain.ControlCertificationFailed, "again", "another", "another", "corr-8")
	assert.ErrorIs(t, err, domain.ErrControlAlreadyCertified)

	// 5. RejectControl (FAILED) does not need the exception resolved.
	unfixed := governanceRun(t, s, tenantID, entity, "AP", "", "EXCEPTION", "preparer", at.Add(time.Minute))
	unfixedExc := governanceException(t, s, tenantID, unfixed)
	failed, err := s.CertifyControlRun(ctx, unfixed.ControlRunID, domain.ControlCertificationFailed, "source data cannot be reconstructed", "certifier", "certifier", "corr-9")
	require.NoError(t, err)
	require.NotNil(t, failed.CertificationStatus)
	assert.Equal(t, domain.ControlCertificationFailed, *failed.CertificationStatus)
	stillOpen, err := s.GetControlException(ctx, unfixedExc.ControlExceptionID)
	require.NoError(t, err)
	assert.Equal(t, domain.ControlExceptionStatusOpen, stillOpen.Status, "failing a control must not pretend its break was resolved")
}

func TestHasUnresolvedException_OnlyTheLatestRunCounts(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	entity := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	early := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

	oldException := governanceRun(t, s, tenantID, entity, "AR", "", "EXCEPTION", "preparer", early)
	oldExc := governanceException(t, s, tenantID, oldException)

	blocking, err := s.HasUnresolvedException(ctx, entity, "AR", "")
	require.NoError(t, err)
	assert.True(t, blocking)

	// A later MATCHED run supersedes the old exception even though the old
	// exception row itself was never formally resolved.
	governanceRun(t, s, tenantID, entity, "AR", "", "MATCHED", "preparer", early.Add(time.Hour))
	blocking, err = s.HasUnresolvedException(ctx, entity, "AR", "")
	require.NoError(t, err)
	assert.False(t, blocking, "an old superseded EXCEPTION must not block forever")

	// A newer EXCEPTION is the latest run again and does block.
	newException := governanceRun(t, s, tenantID, entity, "AR", "", "EXCEPTION", "preparer", early.Add(2*time.Hour))
	newExc := governanceException(t, s, tenantID, newException)
	blocking, err = s.HasUnresolvedException(ctx, entity, "AR", "")
	require.NoError(t, err)
	assert.True(t, blocking)

	// Resolving the latest one clears it.
	_, err = s.RecordResolution(ctx, newExc.ControlExceptionID, "corrected", "resolver", "corr-1")
	require.NoError(t, err)
	blocking, err = s.HasUnresolvedException(ctx, entity, "AR", "")
	require.NoError(t, err)
	assert.False(t, blocking)

	// The old, still-unresolved exception is history and does not resurface.
	_, err = s.GetControlException(ctx, oldExc.ControlExceptionID)
	require.NoError(t, err)
}

// ── HTTP-level guard against the exact A1 finding ───────────────────────────

type governanceAuthZ struct{}

func (governanceAuthZ) CheckAllowed(context.Context, string, string, string) error { return nil }

// governanceClients embeds the handler contract as nil: these tests only
// exercise reads/writes that never touch an upstream service.
type governanceClients struct{ handler.Clients }

func governanceHTTPPost(t *testing.T, rt chi.Router, tenantID, principal, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principal)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}

func TestRemoveCloseRequirement_UnresolvedExceptionBlocks_RealStore(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	entity := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

	rt := chi.NewRouter()
	rt.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(rt, handler.New(s, governanceAuthZ{}, governanceClients{}, []byte("test-signing-key"), zap.NewNop()))

	requirement := &domain.CloseRequirement{
		RequirementID:        uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        entity,
		Kind:                 domain.CloseRequirementSubledgerControl,
		Subledger:            "INVENTORY_VALUE",
		Reason:               "inventory is material",
		CreatedAt:            at,
		CreatedByPrincipalID: "config-holder",
	}
	created, err := s.CreateCloseRequirement(ctx, requirement)
	require.NoError(t, err)
	require.True(t, created)

	exceptionRun := governanceRun(t, s, tenantID, entity, "INVENTORY_VALUE", "", "EXCEPTION", "preparer", at)
	exc := governanceException(t, s, tenantID, exceptionRun)

	removePath := "/v1/close/requirements/" + requirement.RequirementID + "/remove"
	refused := governanceHTTPPost(t, rt, tenantID, "config-holder", removePath,
		domain.RemoveCloseRequirementRequest{Reason: "no longer needed"})
	require.Equal(t, http.StatusUnprocessableEntity, refused.Code, refused.Body.String())
	var body map[string]string
	require.NoError(t, json.Unmarshal(refused.Body.Bytes(), &body))
	assert.Equal(t, "control_has_unresolved_exception", body["error_code"])

	stillThere, err := s.GetCloseRequirement(ctx, requirement.RequirementID)
	require.NoError(t, err)
	assert.Nil(t, stillThere.RemovedAt, "a refused removal must not stamp the requirement")

	_, err = s.RecordResolution(ctx, exc.ControlExceptionID, "inventory counted and corrected", "resolver", "corr-1")
	require.NoError(t, err)

	removed := governanceHTTPPost(t, rt, tenantID, "config-holder", removePath,
		domain.RemoveCloseRequirementRequest{Reason: "no longer needed"})
	require.Equal(t, http.StatusOK, removed.Code, removed.Body.String())

	gone, err := s.GetCloseRequirement(ctx, requirement.RequirementID)
	require.NoError(t, err)
	require.NotNil(t, gone.RemovedAt)

	// A superseded exception (later MATCHED run) does not block removal,
	// even though the old exception row was never formally resolved.
	superseded := &domain.CloseRequirement{
		RequirementID:        uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        entity,
		Kind:                 domain.CloseRequirementSubledgerControl,
		Subledger:            "STOCK_COUNT",
		Reason:               "stock count is material",
		CreatedAt:            at,
		CreatedByPrincipalID: "config-holder",
	}
	created, err = s.CreateCloseRequirement(ctx, superseded)
	require.NoError(t, err)
	require.True(t, created)
	governanceRun(t, s, tenantID, entity, "STOCK_COUNT", "", "EXCEPTION", "preparer", at)
	governanceRun(t, s, tenantID, entity, "STOCK_COUNT", "", "MATCHED", "preparer", at.Add(time.Hour))

	ok := governanceHTTPPost(t, rt, tenantID, "config-holder",
		"/v1/close/requirements/"+superseded.RequirementID+"/remove",
		domain.RemoveCloseRequirementRequest{Reason: "control retired"})
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
}
