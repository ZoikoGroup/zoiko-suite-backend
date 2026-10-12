package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/store"
)

// Second pass, on a real database (AUTHZ_IT_DSN, migrated to 000027, as a
// NOBYPASSRLS role). Each test works under a fresh tenant and only adds rows.

type fixture struct {
	pool   *pgxpool.Pool
	s      *store.PgStore
	tenant string
	ctx    context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := outboxPool(t)
	tenant := uuid.NewString()
	return &fixture{pool: pool, s: store.New(pool, zap.NewNop()), tenant: tenant,
		ctx: domain.WithAudit(context.Background(), "it-admin", "corr-"+tenant[:8])}
}

func (f *fixture) role(t *testing.T, actions ...string) *domain.Role {
	t.Helper()
	r, _, err := f.s.CreateRole(f.ctx, domain.CreateRoleParams{TenantID: f.tenant, RoleCode: "R_" + uuid.NewString()[:8], RoleName: "it",
		RoleScopeType: "TENANT", CreatedByPrincipalID: "it-admin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) > 0 {
		if _, _, err := f.s.CreatePermissionBundle(f.ctx, domain.CreatePermissionBundleParams{RoleID: r.RoleID, BundleCode: "B", PermittedActions: actions}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (f *fixture) assign(t *testing.T, principal, roleID, status string) *domain.PrincipalRoleAssignment {
	t.Helper()
	p := domain.CreateRoleAssignmentParams{PrincipalID: principal, RoleID: roleID, EffectiveFrom: time.Now().Add(-time.Minute), AssignedBy: "it-admin", ApprovalStatus: status}
	if status == domain.ApprovalPending {
		exp := time.Now().Add(time.Hour)
		p.ApprovalExpiresAt = &exp
	}
	a, err := f.s.CreateRoleAssignment(f.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fixture) outboxTypes(t *testing.T, key string) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), "SELECT event_type FROM outbox_events WHERE message_key = $1 ORDER BY created_at, outbox_event_id", key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// GOV-03 / §20 evidence round-trips, and policy_set_version is the watermark.
func TestSecondPassIT_DecisionEvidence(t *testing.T) {
	f := newFixture(t)
	f.role(t, "x.y") // a config change, so the watermark moves
	var watermark int64
	if err := f.pool.QueryRow(context.Background(), "SELECT max(history_id) FROM authz_config_history").Scan(&watermark); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(5 * time.Minute)
	d, err := f.s.RecordAccessDecision(f.ctx, domain.RecordAccessDecisionParams{
		PrincipalID: "p-1", LegalEntityID: uuid.NewString(), ActionType: "payment.release", Outcome: "GRANTED", Basis: "delegated:from=boss",
		CorrelationID: "c-ev", TenantID: f.tenant, Decision: "PERMIT", Obligations: []string{"RECORD_HIGH_RISK_EVIDENCE"},
		ReasonCodes: []string{"PERMISSION_GRANTED"}, MatchedGrants: []string{"rbac:role=X"}, ResourceType: "payment_instruction",
		ResourceID: "pi-1", ResourceVersion: "18", AttributesDigest: "sha256:ab", SessionAssurance: "PHISHING_RESISTANT",
		OnBehalfOf: "boss", DelegationID: "da-1", ExpiresAt: &exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.PolicySetVersion == "" || !strings.HasPrefix(d.PolicySetVersion, "cfg.") {
		t.Fatalf("policy_set_version = %q", d.PolicySetVersion)
	}
	got, err := f.s.FindAccessDecisionByID(f.ctx, d.AccessDecisionID, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != "PERMIT" || got.ResourceID != "pi-1" || got.OnBehalfOf != "boss" || got.DelegationID != "da-1" ||
		len(got.Obligations) != 1 || got.AttributesDigest != "sha256:ab" || got.ExpiresAt == nil || got.SessionAssurance != "PHISHING_RESISTANT" {
		t.Errorf("evidence did not round-trip: %+v", got)
	}
	// The exact configuration watermark in force when the decision was made.
	if want := fmt.Sprintf("cfg.%d", watermark); got.PolicySetVersion != want {
		t.Errorf("policy_set_version = %q, want %q", got.PolicySetVersion, want)
	}
}

// ZS-IAM-001 §32: decision history cannot be rewritten or deleted.
func TestSecondPassIT_DecisionLogAppendOnly(t *testing.T) {
	f := newFixture(t)
	d, err := f.s.RecordAccessDecision(f.ctx, domain.RecordAccessDecisionParams{PrincipalID: "p-1", LegalEntityID: uuid.NewString(),
		ActionType: "a", Outcome: "DENIED", Basis: "no_grant", CorrelationID: "c-imm", TenantID: f.tenant})
	if err != nil {
		t.Fatal(err)
	}
	// Under the tenant, where RLS makes the row visible: the point is that a
	// connection which CAN see the row still cannot change or remove it.
	for _, q := range []string{
		"UPDATE access_decision_log SET decision_outcome = 'GRANTED' WHERE access_decision_id = $1",
		"DELETE FROM access_decision_log WHERE access_decision_id = $1",
	} {
		tx, err := f.pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", f.tenant); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(context.Background(), q, d.AccessDecisionID)
		_ = tx.Rollback(context.Background())
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q was not refused: %v", q, err)
		}
	}
}

// GOV-12: a pending privileged assignment grants nothing until approved; a
// late approval expires it instead.
func TestSecondPassIT_MakerChecker(t *testing.T) {
	f := newFixture(t)
	r := f.role(t, "iam.role.manage")
	a := f.assign(t, "target-1", r.RoleID, domain.ApprovalPending)
	actions, _, err := f.s.FindGrantedActions(f.ctx, "target-1", f.tenant, f.tenant)
	if err != nil || len(actions) != 0 {
		t.Fatalf("pending assignment granted %v (err %v)", actions, err)
	}
	if types := f.outboxTypes(t, a.PrincipalRoleAssignmentID); len(types) != 0 {
		t.Errorf("a pending assignment announced a grant: %v", types)
	}
	decided, err := f.s.DecideRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, domain.ApprovalApproved, "checker-1")
	if err != nil || decided.ApprovalStatus != domain.ApprovalApproved || decided.ApprovedBy == nil || *decided.ApprovedBy != "checker-1" {
		t.Fatalf("approve: %+v %v", decided, err)
	}
	actions, _, _ = f.s.FindGrantedActions(f.ctx, "target-1", f.tenant, f.tenant)
	if len(actions) != 1 {
		t.Fatalf("approved assignment grants %v", actions)
	}
	if types := f.outboxTypes(t, a.PrincipalRoleAssignmentID); len(types) != 1 || types[0] != "iam.assignment.granted" {
		t.Errorf("approval should announce iam.assignment.granted once: %v", types)
	}
	if _, err := f.s.DecideRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, domain.ApprovalApproved, "checker-2"); !errors.Is(err, domain.ErrApprovalNotPending) {
		t.Errorf("second decision: want ErrApprovalNotPending, got %v", err)
	}

	// A decision arriving after the window expires the request.
	past := time.Now().Add(-time.Second)
	lateP := domain.CreateRoleAssignmentParams{PrincipalID: "target-3", RoleID: r.RoleID, EffectiveFrom: time.Now().Add(-time.Minute),
		AssignedBy: "it-admin", ApprovalStatus: domain.ApprovalPending, ApprovalExpiresAt: &past}
	expired, err := f.s.CreateRoleAssignment(f.ctx, lateP)
	if err != nil {
		t.Fatal(err)
	}
	if a, err := f.s.DecideRoleAssignment(f.ctx, expired.PrincipalRoleAssignmentID, f.tenant, domain.ApprovalApproved, "checker-1"); !errors.Is(err, domain.ErrApprovalExpired) || a.ApprovalStatus != domain.ApprovalExpired {
		t.Fatalf("late approval: want EXPIRED, got %+v %v", a, err)
	}
	actions, _, _ = f.s.FindGrantedActions(f.ctx, "target-3", f.tenant, f.tenant)
	if len(actions) != 0 {
		t.Errorf("an expired request grants %v", actions)
	}
}

// §16 / 000025: who changed configuration and why is recorded with the change;
// 000026: the change is announced through the outbox.
func TestSecondPassIT_AuditAndEvents(t *testing.T) {
	f := newFixture(t)
	r := f.role(t, "x.y")
	ctx := domain.WithReason(f.ctx, "ROLE_DECOMMISSIONED")
	if _, err := f.s.SetRoleActive(ctx, r.RoleID, f.tenant, false, 0); err != nil {
		t.Fatal(err)
	}
	var by, reason, corr string
	if err := f.pool.QueryRow(context.Background(), `SELECT changed_by, reason, correlation_id FROM authz_config_history
		WHERE object_type = 'roles' AND object_id = $1 ORDER BY version DESC LIMIT 1`, r.RoleID).Scan(&by, &reason, &corr); err != nil {
		t.Fatal(err)
	}
	if by != "it-admin" || reason != "ROLE_DECOMMISSIONED" || !strings.HasPrefix(corr, "corr-") {
		t.Errorf("history attribution: by=%q reason=%q corr=%q", by, reason, corr)
	}
	types := f.outboxTypes(t, r.RoleID)
	if len(types) < 2 || types[len(types)-1] != "iam.role.published" {
		t.Errorf("role change not announced as iam.role.published: %v", types)
	}
	var env []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT message_value FROM outbox_events WHERE message_key = $1
		ORDER BY outbox_event_id DESC LIMIT 1`, r.RoleID).Scan(&env); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(env, &m)
	if m["tenant_id"] != f.tenant || m["actor_id"] != "it-admin" || m["source_service"] != "authorization-svc" {
		t.Errorf("event envelope = %v", m)
	}

	a := f.assign(t, "holder-1", r.RoleID, domain.ApprovalApproved)
	if _, err := f.s.RevokeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant); err != nil {
		t.Fatal(err)
	}
	if types := f.outboxTypes(t, a.PrincipalRoleAssignmentID); len(types) != 2 || types[0] != "iam.assignment.granted" || types[1] != "iam.assignment.revoked" {
		t.Errorf("assignment lifecycle events = %v", types)
	}
}

// entity.status.changed projection: latest wins; an older event changes nothing.
func TestSecondPassIT_EntityStatus(t *testing.T) {
	f := newFixture(t)
	entity := uuid.NewString()
	now := time.Now().UTC()
	if err := f.s.ProjectEntityStatus(f.ctx, domain.ProjectEntityStatusParams{LegalEntityID: entity, TenantID: f.tenant, Status: "DISSOLVED", StatusChangedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ProjectEntityStatus(f.ctx, domain.ProjectEntityStatusParams{LegalEntityID: entity, TenantID: f.tenant, Status: "ACTIVE", StatusChangedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if st, err := f.s.FindEntityStatus(f.ctx, entity, f.tenant); err != nil || st != "DISSOLVED" {
		t.Fatalf("status = %q %v, want DISSOLVED (older event ignored)", st, err)
	}
	if st, _ := f.s.FindEntityStatus(f.ctx, uuid.NewString(), f.tenant); st != "" {
		t.Errorf("unknown entity: want no status, got %q", st)
	}
}

// §11: delegations do not carry protected privileges, and the delegation a
// grant rests on can be named.
func TestSecondPassIT_DelegationAttributionAndProtectedPrivileges(t *testing.T) {
	f := newFixture(t)
	entity := uuid.NewString()
	r := f.role(t, "payment.approve", "iam.assignment.grant")
	f.assign(t, "boss-1", r.RoleID, domain.ApprovalApproved)
	end := time.Now().Add(24 * time.Hour)
	d, err := f.s.CreateDelegatedAuthority(f.ctx, domain.CreateDelegatedAuthorityParams{TenantID: f.tenant, DelegatorPrincipalID: "boss-1",
		DelegatePrincipalID: "cover-1", ScopeType: "FULL", EffectiveFrom: time.Now().Add(-time.Minute), EffectiveTo: &end})
	if err != nil {
		t.Fatal(err)
	}
	actions, _, err := f.s.FindDelegatedActions(f.ctx, "cover-1", entity, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0] != "payment.approve" {
		t.Fatalf("delegated actions = %v; iam.assignment.grant must not flow through a delegation", actions)
	}
	delegator, id, err := f.s.FindDelegationSource(f.ctx, "cover-1", entity, f.tenant, "", "", "payment.approve")
	if err != nil || delegator != "boss-1" || id != d.DelegatedAuthorityID {
		t.Fatalf("delegation source = %q %q %v", delegator, id, err)
	}
	if types := f.outboxTypes(t, d.DelegatedAuthorityID); len(types) != 1 || types[0] != "iam.delegation.granted" {
		t.Errorf("delegation events = %v", types)
	}
}

// GOV-04: an exception is never self-approved (enforced by the database too),
// applies only while active, and its expiry is recorded and announced.
func TestSecondPassIT_SoDExceptions(t *testing.T) {
	f := newFixture(t)
	tenant := f.tenant
	rule, err := f.s.CreateSoDRule(f.ctx, domain.CreateSoDRuleParams{DomainCode: "TREASURY", ActionA: "payment.prepare", ActionB: "payment.release",
		ConflictType: "STATIC", TenantID: &tenant})
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(3 * time.Second)
	e, err := f.s.CreateSoDException(f.ctx, domain.CreateSoDExceptionParams{TenantID: tenant, SoDRuleID: rule.SoDRuleID, PrincipalID: "p-1",
		CompensatingControl: "daily review", Reason: "single-person entity", RequestedBy: "ctl-1", ExpiresAt: exp})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.TransitionSoDException(f.ctx, e.SoDExceptionID, tenant, "REQUESTED", "APPROVED", "ctl-1"); err == nil {
		t.Fatal("the database accepted an exception approved by its requester")
	}
	if active, _ := f.s.FindActiveSoDException(f.ctx, "p-1", tenant, "payment.release", "payment.prepare"); active != nil {
		t.Fatal("a REQUESTED exception is active")
	}
	if _, err := f.s.TransitionSoDException(f.ctx, e.SoDExceptionID, tenant, "REQUESTED", "APPROVED", "cro-1"); err != nil {
		t.Fatal(err)
	}
	if active, err := f.s.FindActiveSoDException(f.ctx, "p-1", tenant, "payment.release", "payment.prepare"); err != nil || active == nil {
		t.Fatalf("approved exception not active (either action order): %v %v", active, err)
	}
	time.Sleep(4 * time.Second)
	if active, _ := f.s.FindActiveSoDException(f.ctx, "p-1", tenant, "payment.release", "payment.prepare"); active != nil {
		t.Fatal("an expired exception still authorizes the conflict (GOV-04 #3)")
	}
	if _, err := f.s.ExpireDue(f.ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := f.s.FindSoDException(f.ctx, e.SoDExceptionID, tenant)
	if got.Status != "EXPIRED" {
		t.Errorf("sweeper did not record expiry: %s", got.Status)
	}
	if types := f.outboxTypes(t, e.SoDExceptionID); len(types) != 2 || types[0] != "sod.compensating_control.approved" || types[1] != "sod.exception.expired" {
		t.Errorf("exception events = %v", types)
	}
}

// Invariant #3: a missing principal-status table is a refusal, not ACTIVE.
// Runs only when the harness has renamed the table (AUTHZ_IT_PRINCIPAL_TABLE_GONE=1).
func TestSecondPassIT_PrincipalStatusTableMissingFailsClosed(t *testing.T) {
	if os.Getenv("AUTHZ_IT_PRINCIPAL_TABLE_GONE") != "1" {
		t.Skip("set by the harness after renaming principal_status_projection")
	}
	f := newFixture(t)
	if _, err := f.s.FindPrincipalStatus(f.ctx, "p-1", f.tenant); !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Fatalf("missing table: want ErrStoreUnavailable, got %v", err)
	}
}

// S9-1 (Group 1 audit): a decision granted through one of a principal's roles
// counts as use of THAT assignment only. The basis string names every role
// the principal holds, which used to mark all of them used and none DORMANT.
func TestSecondPassIT_UsageAttributedToGrantingAssignment(t *testing.T) {
	f := newFixture(t)
	ra := f.role(t, "x.used")
	rb := f.role(t, "x.idle")
	aa := f.assign(t, "p-1", ra.RoleID, domain.ApprovalApproved)
	ab := f.assign(t, "p-1", rb.RoleID, domain.ApprovalApproved)

	granting, err := f.s.FindGrantingAssignments(f.ctx, "p-1", f.tenant, f.tenant, "", "", "x.used")
	if err != nil || len(granting) != 1 || granting[0].AssignmentID != aa.PrincipalRoleAssignmentID {
		t.Fatalf("granting assignments for x.used = %+v %v; want only role A's", granting, err)
	}
	if _, err := f.s.RecordAccessDecision(f.ctx, domain.RecordAccessDecisionParams{
		PrincipalID: "p-1", LegalEntityID: f.tenant, ActionType: "x.used", Outcome: "GRANTED",
		Basis:         "rbac:role=" + ra.RoleCode + "," + rb.RoleCode, // what the basis has always said
		MatchedGrants: []string{"assignment:" + aa.PrincipalRoleAssignmentID, "role:" + ra.RoleCode},
		CorrelationID: "c-usage", TenantID: f.tenant,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.QueryRoleAssignments(f.ctx, domain.AssignmentQuery{TenantID: f.tenant, PrincipalID: "p-1", IncludeUsage: true, ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, a := range rows {
		used[a.PrincipalRoleAssignmentID] = a.LastGrantedAt != nil
	}
	if !used[aa.PrincipalRoleAssignmentID] || used[ab.PrincipalRoleAssignmentID] {
		t.Fatalf("usage: role A used=%v (want true), role B used=%v (want false — it granted nothing)", used[aa.PrincipalRoleAssignmentID], used[ab.PrincipalRoleAssignmentID])
	}
}
