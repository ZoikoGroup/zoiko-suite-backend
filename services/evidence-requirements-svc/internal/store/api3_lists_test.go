package store_test

// Real-Postgres proof for ER-3 (console read models) and ER-4 (API-3), and for
// migration 000011 (the runtime role can append to the outbox).

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
	"zoiko.io/evidence-requirements-svc/internal/store"
)

func staleCurrent(t *testing.T, err error) int64 {
	t.Helper()
	var stale *domain.StaleVersionError
	if !errors.As(err, &stale) {
		t.Fatalf("err = %v, want a StaleVersionError", err)
	}
	if !errors.Is(err, domain.ErrVersionMismatch) {
		t.Fatal("a StaleVersionError must match ErrVersionMismatch")
	}
	return stale.Current
}

func TestAPI3_EndDateRequirementIsConditionalOnTheVersion(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	r := newTestRequirement(tenantID, nil)
	if _, err := s.CreateRequirement(ctx, r); err != nil {
		t.Fatalf("CreateRequirement: %v", err)
	}
	if r.RecordVersion != 1 {
		t.Fatalf("a new requirement is at version %d, want 1", r.RecordVersion)
	}

	_, err := s.EndDateRequirement(ctx, tenantID, r.EvidenceRequirementID, time.Now().UTC(), "stale", "admin", 5)
	if got := staleCurrent(t, err); got != 1 {
		t.Fatalf("current version = %d, want 1", got)
	}
	got, _ := s.GetRequirement(ctx, r.EvidenceRequirementID)
	if got.EffectiveTo != nil {
		t.Fatal("a stale end-date retired the requirement anyway")
	}

	retired, err := s.EndDateRequirement(ctx, tenantID, r.EvidenceRequirementID, time.Now().UTC(), "superseded", "admin", 1)
	if err != nil {
		t.Fatalf("EndDateRequirement at the current version: %v", err)
	}
	if retired.RecordVersion != 2 || retired.EffectiveTo == nil {
		t.Fatalf("retired = v%d effective_to=%v, want v2 retired", retired.RecordVersion, retired.EffectiveTo)
	}
	// Retiring again from the same (now stale) read is 412, not "already retired".
	_, err = s.EndDateRequirement(ctx, tenantID, r.EvidenceRequirementID, time.Now().UTC(), "again", "admin", 1)
	if got := staleCurrent(t, err); got != 2 {
		t.Fatalf("current version = %d, want 2", got)
	}
	// With the current version it is the truthful 422.
	if _, err := s.EndDateRequirement(ctx, tenantID, r.EvidenceRequirementID, time.Now().UTC(), "again", "admin", 2); !errors.Is(err, domain.ErrAlreadyRetired) {
		t.Fatalf("err = %v, want ErrAlreadyRetired", err)
	}
}

func TestAPI3_PBCCommandsBumpTheVersionAndRefuseStaleOnes(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	req, _, err := s.CreateEvidenceRequest(ctx, uuid.NewString(), domain.CreateEvidenceRequestParams{
		TenantID: tenantID, LegalEntityID: uuid.NewString(), Title: "bank confirmation", AssignedToPrincipalID: "client-1",
		CreatedByPrincipalID: "auditor-1", CorrelationID: "corr-" + uuid.NewString(), DueAt: time.Now().Add(72 * time.Hour),
	})
	if err != nil || req.RecordVersion != 1 {
		t.Fatalf("create: v%d err=%v", req.RecordVersion, err)
	}
	id := req.RequestID

	sent, err := s.SendEvidenceRequest(ctx, domain.SendEvidenceRequestParams{RequestID: id, TenantID: tenantID, CorrelationID: "c", ExpectedVersion: 1})
	if err != nil || sent.RecordVersion != 2 || sent.Status != domain.EvidenceRequestSent {
		t.Fatalf("send: %+v err=%v", sent, err)
	}
	// Send is DRAFT -> SENT only; with the current version a second send is a
	// state refusal, not a silent re-send.
	if _, err := s.SendEvidenceRequest(ctx, domain.SendEvidenceRequestParams{RequestID: id, TenantID: tenantID, CorrelationID: "c", ExpectedVersion: 2}); !errors.Is(err, domain.ErrEvidenceRequestInvalidState) {
		t.Fatalf("second send err = %v, want ErrEvidenceRequestInvalidState", err)
	}
	// A reassignment from the pre-send read is stale.
	_, err = s.ReassignEvidenceRequest(ctx, domain.ReassignEvidenceRequestParams{RequestID: id, TenantID: tenantID, NewAssigneePrincipalID: "client-2", ExpectedVersion: 1})
	if got := staleCurrent(t, err); got != 2 {
		t.Fatalf("current = %d, want 2", got)
	}
	cur, _ := s.GetEvidenceRequest(ctx, tenantID, id)
	if cur.AssignedToPrincipalID != "client-1" {
		t.Fatal("a stale reassignment changed the assignee")
	}
	extended, err := s.ExtendDueDate(ctx, domain.ExtendDueDateParams{RequestID: id, TenantID: tenantID, NewDueAt: time.Now().Add(240 * time.Hour), ExpectedVersion: 2})
	if err != nil || extended.RecordVersion != 3 {
		t.Fatalf("extend: v%d err=%v", extended.RecordVersion, err)
	}

	// View and submit are the contributor's actions: no If-Match, but each
	// moves the status and so bumps the version.
	viewed, err := s.ViewEvidenceRequest(ctx, domain.ViewEvidenceRequestParams{RequestID: id, TenantID: tenantID})
	if err != nil || viewed.RecordVersion != 4 {
		t.Fatalf("view: v%d err=%v", viewed.RecordVersion, err)
	}
	if _, _, err := s.SubmitResponse(ctx, domain.SubmitResponseParams{RequestID: id, TenantID: tenantID, SubmittedByPrincipalID: "client-1", CorrelationID: "resp-" + uuid.NewString()}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	cur, _ = s.GetEvidenceRequest(ctx, tenantID, id)
	if cur.RecordVersion != 5 || cur.Status != domain.EvidenceRequestResponseReceived {
		t.Fatalf("after submit: v%d %s, want v5 RESPONSE_RECEIVED", cur.RecordVersion, cur.Status)
	}
	// A manager's ETag from before the response is stale.
	_, err = s.RequestClarification(ctx, domain.RequestClarificationParams{RequestID: id, TenantID: tenantID, Reason: "x", ExpectedVersion: 4})
	if got := staleCurrent(t, err); got != 5 {
		t.Fatalf("current = %d, want 5", got)
	}
	clar, err := s.RequestClarification(ctx, domain.RequestClarificationParams{RequestID: id, TenantID: tenantID, Reason: "x", ExpectedVersion: 5})
	if err != nil || clar.RecordVersion != 6 || clar.Status != domain.EvidenceRequestClarificationRequired {
		t.Fatalf("clarify: %+v err=%v", clar, err)
	}

	closed, err := s.CloseEvidenceRequest(ctx, domain.CloseEvidenceRequestParams{RequestID: id, TenantID: tenantID, ExpectedVersion: 6})
	if err != nil || closed.RecordVersion != 7 || closed.Status != domain.EvidenceRequestClosed {
		t.Fatalf("close: %+v err=%v", closed, err)
	}
	again, err := s.CloseEvidenceRequest(ctx, domain.CloseEvidenceRequestParams{RequestID: id, TenantID: tenantID, ExpectedVersion: 7})
	if err != nil || again.RecordVersion != 7 {
		t.Fatalf("closing a closed request with its current version is idempotent: %+v err=%v", again, err)
	}
	_, err = s.CloseEvidenceRequest(ctx, domain.CloseEvidenceRequestParams{RequestID: id, TenantID: tenantID, ExpectedVersion: 6})
	if got := staleCurrent(t, err); got != 7 {
		t.Fatalf("current = %d, want 7", got)
	}
	if _, err := s.CloseEvidenceRequest(ctx, domain.CloseEvidenceRequestParams{RequestID: uuid.NewString(), TenantID: tenantID, ExpectedVersion: 1}); !errors.Is(err, domain.ErrEvidenceRequestNotFound) {
		t.Fatalf("closing an unknown request err = %v, want not found", err)
	}
}

func TestER03_ListPagesAreOrderedFilteredAndTenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID, otherTenantID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	entity := uuid.NewString()

	for i, action := range []string{"A_ACTION", "B_ACTION", "C_ACTION"} {
		r := newTestRequirement(tenantID, &entity)
		r.ActionType = action
		r.CorrelationID = "corr-" + uuid.NewString()
		r.EffectiveFrom = time.Now().Add(-time.Duration(i+1) * time.Hour).UTC()
		if _, err := s.CreateRequirement(ctx, r); err != nil {
			t.Fatalf("create %s: %v", action, err)
		}
	}
	foreign := newTestRequirement(otherTenantID, nil)
	if _, err := s.CreateRequirement(svcmiddleware.WithTenant(context.Background(), otherTenantID), foreign); err != nil {
		t.Fatalf("create foreign: %v", err)
	}

	first, err := s.ListRequirementsPage(ctx, tenantID, domain.ListRequirementsFilter{}, 0, 2)
	if err != nil || len(first) != 2 || first[0].ActionType != "A_ACTION" || first[1].ActionType != "B_ACTION" {
		t.Fatalf("first page = %v err=%v", first, err)
	}
	second, err := s.ListRequirementsPage(ctx, tenantID, domain.ListRequirementsFilter{}, 2, 2)
	if err != nil || len(second) != 1 || second[0].ActionType != "C_ACTION" {
		t.Fatalf("second page = %v err=%v", second, err)
	}
	filtered, _ := s.ListRequirementsPage(ctx, tenantID, domain.ListRequirementsFilter{ActionType: "B_ACTION", LegalEntityID: entity}, 0, 10)
	if len(filtered) != 1 {
		t.Fatalf("action filter = %d rows, want 1", len(filtered))
	}

	for i, outcome := range []domain.Outcome{domain.OutcomeSatisfied, domain.OutcomeMissing, domain.OutcomeNoRequirementsDefined} {
		e := &domain.EvidenceEvaluation{EvaluationID: uuid.NewString(), TenantID: tenantID, LegalEntityID: entity,
			DomainCode: "PROCUREMENT", ActionType: []string{"A_ACTION", "B_ACTION", "UNGATED"}[i], Outcome: outcome,
			UnmetPayload: json.RawMessage(`[]`), PresentArtifactsPayload: json.RawMessage(`[]`),
			EvaluatedForPrincipalID: "p", CorrelationID: "c", IdempotencyKey: "idem-" + uuid.NewString()}
		if _, err := s.RecordEvaluation(ctx, e, nil); err != nil {
			t.Fatalf("record %s: %v", outcome, err)
		}
	}
	missing, err := s.ListEvaluationsPage(ctx, tenantID, domain.ListEvaluationsFilter{Outcome: "MISSING"}, 0, 10)
	if err != nil || len(missing) != 1 || missing[0].ActionType != "B_ACTION" {
		t.Fatalf("missing filter = %v err=%v", missing, err)
	}
	all, _ := s.ListEvaluationsPage(ctx, tenantID, domain.ListEvaluationsFilter{Since: time.Now().Add(-time.Hour)}, 0, 10)
	if len(all) != 3 {
		t.Fatalf("since filter = %d rows, want 3", len(all))
	}
	none, _ := s.ListEvaluationsPage(svcmiddleware.WithTenant(context.Background(), otherTenantID), otherTenantID, domain.ListEvaluationsFilter{}, 0, 10)
	if len(none) != 0 {
		t.Fatalf("another tenant sees %d of this tenant's evaluations", len(none))
	}

	sum, err := s.GetSummary(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if sum.ActiveRequirements != 3 || sum.EvaluationsSatisfied30d != 1 || sum.EvaluationsMissing30d != 1 ||
		sum.ActionsWithNoRequirements30d != 1 || sum.RequirementsByEvidenceType["SUPPORTING_DOCUMENT"] != 3 ||
		len(sum.UncoveredActions) != 1 || sum.UncoveredActions[0].ActionType != "UNGATED" {
		t.Fatalf("summary = %+v", sum)
	}
}

// appRolePool connects as a NOSUPERUSER, NOBYPASSRLS role that does not own
// the tables -- the shape of the zoiko_app runtime role -- so row-level
// security applies to it.
func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'evreq_app_probe') THEN
			CREATE ROLE evreq_app_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS; END IF; END $$`,
		`GRANT USAGE ON SCHEMA public TO evreq_app_probe`,
		`GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA public TO evreq_app_probe`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("app role setup: %v", err)
		}
	}
	u, err := url.Parse(admin.Config().ConnString())
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword("evreq_app_probe", "probe")
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as app role: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRLS_ListsNeverCrossTenant(t *testing.T) {
	admin := openTestPool(t)
	adminStore := store.New(admin, zap.NewNop())
	a, b := uuid.New().String(), uuid.New().String()
	for _, tenant := range []string{a, b} {
		ctx := svcmiddleware.WithTenant(context.Background(), tenant)
		if _, err := adminStore.CreateRequirement(ctx, newTestRequirement(tenant, nil)); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, _, err := adminStore.CreateEvidenceRequest(ctx, uuid.NewString(), domain.CreateEvidenceRequestParams{
			TenantID: tenant, LegalEntityID: uuid.NewString(), Title: "t", AssignedToPrincipalID: "c",
			CreatedByPrincipalID: "a", CorrelationID: "corr-" + uuid.NewString(), DueAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatalf("create request: %v", err)
		}
	}

	app := appRolePool(t, admin)
	s := store.New(app, zap.NewNop())
	ctxB := svcmiddleware.WithTenant(context.Background(), b)
	reqs, err := s.ListRequirementsPage(ctxB, b, domain.ListRequirementsFilter{}, 0, 50)
	if err != nil || len(reqs) != 1 || reqs[0].TenantID != b {
		t.Fatalf("tenant B's requirement list as the app role = %v err=%v", reqs, err)
	}
	pbc, err := s.ListEvidenceRequestsPage(ctxB, b, domain.ListEvidenceRequestsFilter{OpenOnly: true}, 0, 50)
	if err != nil || len(pbc) != 1 || pbc[0].TenantID != b {
		t.Fatalf("tenant B's request list as the app role = %v err=%v", pbc, err)
	}

	// The policy itself, without the store's tenant predicate: with the scope
	// set to B the app role sees only B's rows in every list table.
	tx, err := app.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, b); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"evidence_requirements", "evidence_requests", "evidence_evaluations"} {
		var foreign int
		if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE tenant_id <> $1`, b).Scan(&foreign); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if foreign != 0 {
			t.Fatalf("RLS let the app role see %d foreign rows in %s", foreign, table)
		}
	}
}

// Migration 000011: the runtime role appends events and counts the backlog,
// but cannot read or rewrite the outbox.
func TestOutbox_AppRoleAppendsButCannotRead(t *testing.T) {
	admin := openTestPool(t)
	app := appRolePool(t, admin)
	s := store.New(app, zap.NewNop())
	tenant := uuid.New().String()
	e := &domain.EvidenceEvaluation{EvaluationID: uuid.NewString(), TenantID: tenant, LegalEntityID: uuid.NewString(),
		DomainCode: "FINANCE", ActionType: "JOURNAL_POST", Outcome: domain.OutcomeMissing,
		UnmetPayload: json.RawMessage(`[]`), PresentArtifactsPayload: json.RawMessage(`[]`),
		EvaluatedForPrincipalID: "p", CorrelationID: "c", IdempotencyKey: "idem-" + uuid.NewString()}
	ev := &domain.OutboxEvent{AggregateType: "evidence_evaluation", AggregateID: e.EvaluationID,
		EventType: "evidence.requirement.missing", Payload: []byte(`{"tenant_id":"` + tenant + `"}`)}
	if _, err := s.RecordEvaluation(svcmiddleware.WithTenant(context.Background(), tenant), e, ev); err != nil {
		t.Fatalf("the app role could not record an evaluation with its event: %v", err)
	}

	var visible int
	if err := app.QueryRow(context.Background(), `SELECT count(*) FROM outbox`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("the app role can read %d outbox rows", visible)
	}
	n, err := s.OutboxUnpublished(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("OutboxUnpublished = %d err=%v, want 1", n, err)
	}
}
