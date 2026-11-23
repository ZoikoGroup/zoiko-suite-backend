package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/authorization-svc/internal/domain"
)

// ORG-06 SuspendAssignment and GetAssignmentsAsOf on a real database
// (AUTHZ_IT_DSN, migrated to 000037, as a NOBYPASSRLS role).

const actionPay = "payment.approve"

// holds reports whether principal is currently granted actionPay by a role
// assignment, asking the two queries the decision path uses.
func (f *fixture) holds(t *testing.T, principal, entity string) bool {
	t.Helper()
	actions, _, err := f.s.FindGrantedActionsScoped(f.ctx, principal, entity, f.tenant, "", "")
	if err != nil {
		t.Fatal(err)
	}
	granting, err := f.s.FindGrantingAssignments(f.ctx, principal, entity, f.tenant, "", "", actionPay)
	if err != nil {
		t.Fatal(err)
	}
	byActions := false
	for _, a := range actions {
		if a == actionPay {
			byActions = true
		}
	}
	if byActions != (len(granting) > 0) {
		t.Fatalf("the two decision queries disagree: actions=%v granting=%d", actions, len(granting))
	}
	return byActions
}

func (f *fixture) status(t *testing.T, id string) string {
	t.Helper()
	a, err := f.s.FindRoleAssignmentByID(f.ctx, id, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	return a.ApprovalStatus
}

// asTenant runs fn in a transaction scoped to the fixture's tenant, as the
// application role: row-level security applies.
func (f *fixture) asTenant(t *testing.T, fn func(tx pgx.Tx) error) error {
	t.Helper()
	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", f.tenant); err != nil {
		t.Fatal(err)
	}
	return fn(tx)
}

func TestSuspend_StopsEveryGrantAndResumeRestoresIt(t *testing.T) {
	f := newFixture(t)
	entity := uuid.NewString()
	role := f.role(t, actionPay)
	a := f.assign(t, "alice", role.RoleID, domain.ApprovalApproved)
	if !f.holds(t, "alice", entity) {
		t.Fatal("precondition: an approved assignment must grant")
	}

	got, err := f.s.SuspendRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "admin-1", "under investigation")
	if err != nil {
		t.Fatal(err)
	}
	if got.ApprovalStatus != domain.ApprovalSuspended {
		t.Fatalf("status = %s, want SUSPENDED", got.ApprovalStatus)
	}
	if f.holds(t, "alice", entity) {
		t.Fatal("a suspended assignment must grant nothing on any decision query")
	}

	if _, err := f.s.ResumeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "admin-2", "cleared"); err != nil {
		t.Fatal(err)
	}
	if !f.holds(t, "alice", entity) {
		t.Fatal("a resumed assignment must grant again")
	}
}

// Authority delegated FROM a suspended holder stops flowing, like one from a
// suspended principal (N-02), and flows again on resume.
func TestSuspend_StopsAuthorityDelegatedFromTheHolder(t *testing.T) {
	f := newFixture(t)
	entity := uuid.NewString()
	role := f.role(t, actionPay)
	a := f.assign(t, "boss", role.RoleID, domain.ApprovalApproved)
	end := time.Now().Add(24 * time.Hour)
	if _, err := f.s.CreateDelegatedAuthority(f.ctx, domain.CreateDelegatedAuthorityParams{TenantID: f.tenant, DelegatorPrincipalID: "boss",
		DelegatePrincipalID: "cover", ScopeType: "FULL", EffectiveFrom: time.Now().Add(-time.Minute), EffectiveTo: &end}); err != nil {
		t.Fatal(err)
	}
	conferred := func() int {
		actions, _, err := f.s.FindDelegatedActionsScoped(f.ctx, "cover", entity, f.tenant, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return len(actions)
	}
	if conferred() != 1 {
		t.Fatal("precondition: the delegation must confer")
	}
	if _, err := f.s.SuspendRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "admin-1", "hold"); err != nil {
		t.Fatal(err)
	}
	if n := conferred(); n != 0 {
		t.Fatalf("delegated from a suspended assignment: %d action(s) still conferred", n)
	}
	if _, err := f.s.ResumeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "admin-2", ""); err != nil {
		t.Fatal(err)
	}
	if conferred() != 1 {
		t.Fatal("the delegation must confer again after resume")
	}
}

func TestSuspend_Refusals(t *testing.T) {
	f := newFixture(t)
	role := f.role(t, actionPay)

	t.Run("a pending assignment cannot be suspended", func(t *testing.T) {
		p := f.assign(t, "pending-p", role.RoleID, domain.ApprovalPending)
		_, err := f.s.SuspendRoleAssignment(f.ctx, p.PrincipalRoleAssignmentID, f.tenant, "admin", "r")
		if !errors.Is(err, domain.ErrAssignmentNotSuspendable) {
			t.Fatalf("got %v, want ErrAssignmentNotSuspendable", err)
		}
		if f.status(t, p.PrincipalRoleAssignmentID) != domain.ApprovalPending {
			t.Fatal("the refusal changed the status")
		}
	})
	t.Run("an unknown assignment", func(t *testing.T) {
		_, err := f.s.SuspendRoleAssignment(f.ctx, uuid.NewString(), f.tenant, "admin", "r")
		if !errors.Is(err, domain.ErrRoleAssignmentNotFound) {
			t.Fatalf("got %v, want ErrRoleAssignmentNotFound", err)
		}
	})
	t.Run("already suspended", func(t *testing.T) {
		a := f.assign(t, "twice", role.RoleID, domain.ApprovalApproved)
		if _, err := f.s.SuspendRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "admin", "r"); err != nil {
			t.Fatal(err)
		}
		_, err := f.s.SuspendRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "admin", "again")
		if !errors.Is(err, domain.ErrAssignmentNotSuspendable) {
			t.Fatalf("got %v, want ErrAssignmentNotSuspendable", err)
		}
		susp, _ := f.s.ListAssignmentSuspensions(f.ctx, a.PrincipalRoleAssignmentID, f.tenant)
		if len(susp) != 1 {
			t.Fatalf("%d suspension rows, want exactly 1", len(susp))
		}
	})
	t.Run("an ended assignment", func(t *testing.T) {
		a := f.assign(t, "ended", role.RoleID, domain.ApprovalApproved)
		if _, err := f.s.RevokeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant); err != nil {
			t.Fatal(err)
		}
		_, err := f.s.SuspendRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "admin", "r")
		if !errors.Is(err, domain.ErrAssignmentNotSuspendable) {
			t.Fatalf("got %v, want ErrAssignmentNotSuspendable", err)
		}
	})
	t.Run("another tenant cannot suspend it, nor learn it exists", func(t *testing.T) {
		a := f.assign(t, "mine", role.RoleID, domain.ApprovalApproved)
		_, err := f.s.SuspendRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, uuid.NewString(), "admin", "r")
		if !errors.Is(err, domain.ErrRoleAssignmentNotFound) {
			t.Fatalf("got %v, want ErrRoleAssignmentNotFound", err)
		}
		if f.status(t, a.PrincipalRoleAssignmentID) != domain.ApprovalApproved {
			t.Fatal("a cross-tenant attempt changed the assignment")
		}
	})
}

func TestResume_Refusals(t *testing.T) {
	f := newFixture(t)
	role := f.role(t, actionPay)

	t.Run("not suspended", func(t *testing.T) {
		a := f.assign(t, "active", role.RoleID, domain.ApprovalApproved)
		_, err := f.s.ResumeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "admin", "r")
		if !errors.Is(err, domain.ErrAssignmentNotSuspended) {
			t.Fatalf("got %v, want ErrAssignmentNotSuspended", err)
		}
	})
	t.Run("nobody lifts their own suspension", func(t *testing.T) {
		a := f.assign(t, "self-p", role.RoleID, domain.ApprovalApproved)
		if _, err := f.s.SuspendRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "admin", "r"); err != nil {
			t.Fatal(err)
		}
		_, err := f.s.ResumeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "self-p", "let me back in")
		if !errors.Is(err, domain.ErrSelfResume) {
			t.Fatalf("got %v, want ErrSelfResume", err)
		}
		if f.status(t, a.PrincipalRoleAssignmentID) != domain.ApprovalSuspended {
			t.Fatal("a refused self-resume changed the status")
		}
	})
	t.Run("a window that closed while suspended", func(t *testing.T) {
		a := f.assign(t, "lapsed", role.RoleID, domain.ApprovalApproved)
		if _, err := f.s.SuspendRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "admin", "r"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.RevokeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant); err != nil {
			t.Fatal(err)
		}
		_, err := f.s.ResumeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "admin", "r")
		if !errors.Is(err, domain.ErrAssignmentEnded) {
			t.Fatalf("got %v, want ErrAssignmentEnded", err)
		}
	})
	t.Run("unknown and cross-tenant", func(t *testing.T) {
		if _, err := f.s.ResumeRoleAssignment(f.ctx, uuid.NewString(), f.tenant, "admin", "r"); !errors.Is(err, domain.ErrRoleAssignmentNotFound) {
			t.Fatalf("unknown: got %v", err)
		}
	})
}

func TestSuspend_RecordsEvidenceAndEmitsItsOwnEvents(t *testing.T) {
	f := newFixture(t)
	role := f.role(t, actionPay)
	a := f.assign(t, "evid", role.RoleID, domain.ApprovalApproved)
	id := a.PrincipalRoleAssignmentID
	var before int64
	if err := f.pool.QueryRow(context.Background(), "SELECT assignment_version FROM authz_versions").Scan(&before); err != nil {
		t.Fatal(err)
	}

	if _, err := f.s.SuspendRoleAssignment(f.ctx, id, f.tenant, "admin-1", "access review pending"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ResumeRoleAssignment(f.ctx, id, f.tenant, "admin-2", "review passed"); err != nil {
		t.Fatal(err)
	}

	susp, err := f.s.ListAssignmentSuspensions(f.ctx, id, f.tenant)
	if err != nil || len(susp) != 1 {
		t.Fatalf("suspensions = %v, err = %v", susp, err)
	}
	s := susp[0]
	if s.SuspendedBy != "admin-1" || s.SuspensionReason != "access review pending" {
		t.Errorf("suspension evidence = %+v", s)
	}
	if s.ResumedBy == nil || *s.ResumedBy != "admin-2" || s.ResumeReason == nil || *s.ResumeReason != "review passed" || s.ResumedAt == nil {
		t.Errorf("resume evidence = %+v", s)
	}
	if !s.ResumedAt.After(s.SuspendedAt) {
		t.Errorf("resumed %v is not after suspended %v", s.ResumedAt, s.SuspendedAt)
	}

	// granted once, at creation; suspended and resumed are their own events, so
	// a resume is not mistaken for a new grant.
	types := f.outboxTypes(t, id)
	want := []string{"iam.assignment.granted", "iam.assignment.suspended", "iam.assignment.resumed"}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}

	// Cached decisions on every replica are invalidated by the watermark.
	var after int64
	if err := f.pool.QueryRow(context.Background(), "SELECT assignment_version FROM authz_versions").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after < before+2 {
		t.Errorf("assignment_version %d -> %d: a suspend and a resume must each bump it", before, after)
	}
}

// Ending a suspended assignment is still a revocation, and says so.
func TestSuspend_RevokingASuspendedAssignmentStillEmitsRevoked(t *testing.T) {
	f := newFixture(t)
	role := f.role(t, actionPay)
	a := f.assign(t, "rev", role.RoleID, domain.ApprovalApproved)
	id := a.PrincipalRoleAssignmentID
	if _, err := f.s.SuspendRoleAssignment(f.ctx, id, f.tenant, "admin", "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RevokeRoleAssignment(f.ctx, id, f.tenant); err != nil {
		t.Fatal(err)
	}
	types := f.outboxTypes(t, id)
	if len(types) == 0 || types[len(types)-1] != "iam.assignment.revoked" {
		t.Fatalf("events = %v, want a final iam.assignment.revoked", types)
	}
}

// The database refuses what the store refuses, so a writer that skips the store
// cannot turn a non-granting assignment into a granting one.
func TestSuspend_TheDatabaseEnforcesTheStateMachineAndTheEvidence(t *testing.T) {
	f := newFixture(t)
	role := f.role(t, actionPay)
	pending := f.assign(t, "db-pending", role.RoleID, domain.ApprovalPending)
	rejected := f.assign(t, "db-rejected", role.RoleID, domain.ApprovalPending)
	if _, err := f.s.DecideRoleAssignment(f.ctx, rejected.PrincipalRoleAssignmentID, f.tenant, domain.ApprovalRejected, "checker"); err != nil {
		t.Fatal(err)
	}
	active := f.assign(t, "db-active", role.RoleID, domain.ApprovalApproved)
	if _, err := f.s.SuspendRoleAssignment(f.ctx, active.PrincipalRoleAssignmentID, f.tenant, "admin", "r"); err != nil {
		t.Fatal(err)
	}

	update := func(id, status string) error {
		return f.asTenant(t, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				"UPDATE principal_role_assignments SET approval_status = $2 WHERE principal_role_assignment_id = $1", id, status)
			return err
		})
	}
	if err := update(pending.PrincipalRoleAssignmentID, "SUSPENDED"); err == nil {
		t.Error("a PENDING assignment was suspended by raw SQL")
	}
	if err := update(rejected.PrincipalRoleAssignmentID, "SUSPENDED"); err == nil {
		t.Error("a REJECTED assignment was suspended by raw SQL")
	}
	for _, bad := range []string{"REJECTED", "PENDING_APPROVAL", "EXPIRED"} {
		if err := update(active.PrincipalRoleAssignmentID, bad); err == nil {
			t.Errorf("a SUSPENDED assignment was moved to %s by raw SQL", bad)
		}
	}

	suspensionSQL := map[string]string{
		"edit who":            "UPDATE assignment_suspensions SET suspended_by = 'someone-else' WHERE principal_role_assignment_id = $1",
		"edit the reason":     "UPDATE assignment_suspensions SET suspension_reason = 'nothing to see' WHERE principal_role_assignment_id = $1",
		"back-date the start": "UPDATE assignment_suspensions SET suspended_at = suspended_at - interval '1 day' WHERE principal_role_assignment_id = $1",
		"delete the record":   "DELETE FROM assignment_suspensions WHERE principal_role_assignment_id = $1",
	}
	for name, sql := range suspensionSQL {
		err := f.asTenant(t, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), sql, active.PrincipalRoleAssignmentID)
			return err
		})
		if err == nil {
			t.Errorf("%s: the append-only suspension record was changed", name)
		}
	}
	// A second open suspension, and one with no reason.
	for name, sql := range map[string]string{
		"second open suspension": "INSERT INTO assignment_suspensions (principal_role_assignment_id, tenant_id, suspended_by, suspension_reason) VALUES ($1, '" + f.tenant + "', 'x', 'again')",
		"blank reason":           "INSERT INTO assignment_suspensions (principal_role_assignment_id, tenant_id, suspended_by, suspension_reason, resumed_by, resumed_at) VALUES ($1, '" + f.tenant + "', 'x', '  ', 'y', now())",
	} {
		err := f.asTenant(t, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), sql, active.PrincipalRoleAssignmentID)
			return err
		})
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestSuspend_SuspensionRecordsAreTenantIsolated(t *testing.T) {
	a := newFixture(t)
	roleA := a.role(t, actionPay)
	asg := a.assign(t, "iso", roleA.RoleID, domain.ApprovalApproved)
	if _, err := a.s.SuspendRoleAssignment(a.ctx, asg.PrincipalRoleAssignmentID, a.tenant, "admin", "r"); err != nil {
		t.Fatal(err)
	}
	b := newFixture(t)
	var n int
	err := b.asTenant(t, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "SELECT count(*) FROM assignment_suspensions").Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	var mine int
	_ = b.asTenant(t, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "SELECT count(*) FROM assignment_suspensions WHERE principal_role_assignment_id = $1",
			asg.PrincipalRoleAssignmentID).Scan(&mine)
	})
	if mine != 0 {
		t.Fatalf("tenant B can read tenant A's suspension record (%d visible, %d total)", mine, n)
	}
	if _, err := b.s.ListAssignmentSuspensions(b.ctx, asg.PrincipalRoleAssignmentID, b.tenant); !errors.Is(err, domain.ErrRoleAssignmentNotFound) {
		t.Fatalf("tenant B listing A's suspensions: got %v, want not found", err)
	}
}

// ── GetAssignmentsAsOf ──────────────────────────────────────────────────────

func (f *fixture) asOf(t *testing.T, principal string, at time.Time) []string {
	t.Helper()
	got, err := f.s.QueryAssignmentsAsOf(f.ctx, domain.AssignmentsAsOfQuery{TenantID: f.tenant, PrincipalID: principal, AsOf: at})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, a := range got {
		ids = append(ids, a.PrincipalRoleAssignmentID)
	}
	return ids
}

func idsInclude(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// The same rows answer differently at different instants: before it existed,
// while it authorized, while suspended, after resuming, after revocation.
func TestAsOf_ReconstructsEachPhaseOfAnAssignment(t *testing.T) {
	f := newFixture(t)
	role := f.role(t, actionPay)
	a := f.assign(t, "phased", role.RoleID, domain.ApprovalApproved)
	id := a.PrincipalRoleAssignmentID

	if _, err := f.s.SuspendRoleAssignment(f.ctx, id, f.tenant, "admin-1", "hold"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ResumeRoleAssignment(f.ctx, id, f.tenant, "admin-2", "released"); err != nil {
		t.Fatal(err)
	}
	revoked, err := f.s.RevokeRoleAssignment(f.ctx, id, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	susp, _ := f.s.ListAssignmentSuspensions(f.ctx, id, f.tenant)
	suspendedAt, resumedAt, endedAt := susp[0].SuspendedAt, *susp[0].ResumedAt, *revoked.EffectiveTo

	tick := time.Microsecond
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before it was recorded", a.CreatedAt.Add(-time.Second), false},
		{"once recorded and approved", a.CreatedAt, true},
		{"just before the suspension", suspendedAt.Add(-tick), true},
		{"at the moment of suspension", suspendedAt, false},
		{"during the suspension", suspendedAt.Add(resumedAt.Sub(suspendedAt) / 2), false},
		{"at the moment of resumption", resumedAt, true},
		{"between resumption and revocation", resumedAt.Add(endedAt.Sub(resumedAt) / 2), true},
		{"at revocation (the window is half open)", endedAt, false},
		{"long after", endedAt.Add(24 * time.Hour), false},
	}
	for _, c := range cases {
		if got := idsInclude(f.asOf(t, "phased", c.at), id); got != c.want {
			t.Errorf("%s: in force = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAsOf_NeverShowsWhatNeverAuthorized(t *testing.T) {
	f := newFixture(t)
	role := f.role(t, actionPay)
	pending := f.assign(t, "never", role.RoleID, domain.ApprovalPending)
	rejected := f.assign(t, "never", role.RoleID, domain.ApprovalPending)
	if _, err := f.s.DecideRoleAssignment(f.ctx, rejected.PrincipalRoleAssignmentID, f.tenant, domain.ApprovalRejected, "checker"); err != nil {
		t.Fatal(err)
	}
	got := f.asOf(t, "never", time.Now().Add(time.Minute))
	if idsInclude(got, pending.PrincipalRoleAssignmentID) || idsInclude(got, rejected.PrincipalRoleAssignmentID) {
		t.Fatalf("an assignment that never granted appears as authorizing: %v", got)
	}
}

// A pending assignment authorizes only from when it is approved, whatever
// effective_from says: as-of answers "what could they do", and nothing was
// granted before the checker acted.
func TestAsOf_AnApprovalAuthorizesFromTheMomentItIsDecided(t *testing.T) {
	f := newFixture(t)
	role := f.role(t, actionPay)
	a := f.assign(t, "later", role.RoleID, domain.ApprovalPending) // effective_from is a minute ago
	time.Sleep(20 * time.Millisecond)
	decided, err := f.s.DecideRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, domain.ApprovalApproved, "checker")
	if err != nil {
		t.Fatal(err)
	}
	id := a.PrincipalRoleAssignmentID
	if idsInclude(f.asOf(t, "later", decided.ApprovalDecidedAt.Add(-time.Microsecond)), id) {
		t.Error("shown as authorizing before it was approved")
	}
	if !idsInclude(f.asOf(t, "later", *decided.ApprovalDecidedAt), id) {
		t.Error("not shown once approved")
	}
}

func TestAsOf_FiltersByPrincipalAndTenant(t *testing.T) {
	f := newFixture(t)
	role := f.role(t, actionPay)
	x := f.assign(t, "who-x", role.RoleID, domain.ApprovalApproved)
	y := f.assign(t, "who-y", role.RoleID, domain.ApprovalApproved)
	now := time.Now().Add(time.Second)

	onlyX := f.asOf(t, "who-x", now)
	if !idsInclude(onlyX, x.PrincipalRoleAssignmentID) || idsInclude(onlyX, y.PrincipalRoleAssignmentID) {
		t.Errorf("principal filter: %v", onlyX)
	}
	all := f.asOf(t, "", now)
	if !idsInclude(all, x.PrincipalRoleAssignmentID) || !idsInclude(all, y.PrincipalRoleAssignmentID) {
		t.Errorf("no principal filter: %v", all)
	}

	other := newFixture(t)
	got, err := other.s.QueryAssignmentsAsOf(other.ctx, domain.AssignmentsAsOfQuery{TenantID: other.tenant, AsOf: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.PrincipalRoleAssignmentID == x.PrincipalRoleAssignmentID {
			t.Fatal("tenant B's as-of read returned tenant A's assignment")
		}
	}
}
