package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/authorization-svc/internal/domain"
)

// 000038-000040 on a real database (AUTHZ_IT_DSN, NOBYPASSRLS): suspension
// stops a grant and resuming restores it, the history reconstructs what was
// held at any past instant (IAM-13 "done when"), the version moves with each
// real change, and QUANTITY limits are held to their shape by the database.

// dbNow is the database clock: history is stamped with it, and the host's
// clock can drift from the Docker VM's.
func (f *fixture) dbNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := f.pool.QueryRow(context.Background(), "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

func (f *fixture) heldAsOf(t *testing.T, principal, assignmentID string, at time.Time) bool {
	t.Helper()
	list, err := f.s.FindRoleAssignmentsAsOf(f.ctx, f.tenant, principal, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.PrincipalRoleAssignmentID == assignmentID {
			return true
		}
	}
	return false
}

func TestIAM13IT_SuspendResumeAndAsOf(t *testing.T) {
	f := newFixture(t)
	r := f.role(t, "it.read")
	entity := uuid.NewString()
	granted := func() bool {
		t.Helper()
		actions, _, err := f.s.FindGrantedActions(f.ctx, "holder-1", entity, f.tenant)
		if err != nil {
			t.Fatal(err)
		}
		return has(actions, "it.read")
	}

	before := f.dbNow(t)
	a := f.assign(t, "holder-1", r.RoleID, domain.ApprovalApproved)
	held := f.dbNow(t)
	if !granted() {
		t.Fatal("a fresh approved assignment grants nothing")
	}

	s, err := f.s.SuspendRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "it-admin", "extended leave")
	if err != nil {
		t.Fatal(err)
	}
	if s.SuspendedAt == nil || s.SuspendedBy == nil || *s.SuspendedBy != "it-admin" || s.SuspensionReason == nil || s.Version != a.Version+1 {
		t.Fatalf("suspended row: %+v", s)
	}
	suspended := f.dbNow(t)
	if granted() {
		t.Fatal("a suspended assignment still grants")
	}
	if s.ApprovalStatus != domain.ApprovalSuspended {
		t.Fatalf("suspended row status %s, want %s", s.ApprovalStatus, domain.ApprovalSuspended)
	}
	if _, err := f.s.SuspendRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "it-admin", "again"); !errors.Is(err, domain.ErrAssignmentNotSuspendable) {
		t.Errorf("second suspend: err %v, want ErrAssignmentNotSuspendable", err)
	}
	other := newFixture(t)
	if _, err := other.s.SuspendRoleAssignment(other.ctx, a.PrincipalRoleAssignmentID, other.tenant, "x", ""); !errors.Is(err, domain.ErrRoleAssignmentNotFound) {
		t.Errorf("suspend from another tenant: err %v, want ErrRoleAssignmentNotFound", err)
	}

	resumed, err := f.s.ResumeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "it-approver", "back from leave")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.SuspendedAt != nil || resumed.ApprovalStatus != domain.ApprovalApproved || resumed.Version != s.Version+1 {
		t.Fatalf("resumed row: %+v", resumed)
	}
	back := f.dbNow(t)
	if !granted() {
		t.Fatal("a resumed assignment grants nothing")
	}
	if _, err := f.s.ResumeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, "it-approver", ""); !errors.Is(err, domain.ErrAssignmentNotSuspended) {
		t.Errorf("second resume: err %v, want ErrAssignmentNotSuspended", err)
	}

	if _, err := f.s.RevokeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant); err != nil {
		t.Fatal(err)
	}
	ended := f.dbNow(t)

	// The as-of read answers each instant from what was recorded by then.
	for _, c := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before it was made", before, false},
		{"while held", held, true},
		{"while suspended", suspended, false},
		{"after resuming", back, true},
		{"after revoking", ended, false},
	} {
		if got := f.heldAsOf(t, "holder-1", a.PrincipalRoleAssignmentID, c.at); got != c.want {
			t.Errorf("as of %s: held = %v, want %v", c.name, got, c.want)
		}
	}
	if other.heldAsOf(t, "holder-1", a.PrincipalRoleAssignmentID, back) {
		t.Error("another tenant's as-of read sees this assignment")
	}

	want := []string{"iam.assignment.granted", "iam.assignment.suspended", "iam.assignment.resumed", "iam.assignment.revoked"}
	if got := f.outboxTypes(t, a.PrincipalRoleAssignmentID); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Errorf("events = %v, want %v", got, want)
	}

	var rows int
	var changedBy string
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*), max(changed_by) FROM principal_role_assignment_history
		WHERE principal_role_assignment_id = $1::uuid`, a.PrincipalRoleAssignmentID).Scan(&rows, &changedBy); err != nil {
		t.Fatal(err)
	}
	if rows != 4 || changedBy != "it-admin" {
		t.Errorf("history rows = %d changed_by = %q, want 4 by it-admin", rows, changedBy)
	}
}

// A pending privileged assignment held nothing until its approval; the as-of
// read places the grant at the decision, not at the request.
func TestIAM13IT_AsOfFollowsApproval(t *testing.T) {
	f := newFixture(t)
	r := f.role(t, "it.write")
	a := f.assign(t, "holder-2", r.RoleID, domain.ApprovalPending)
	pending := f.dbNow(t)
	if _, err := f.s.DecideRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant, domain.ApprovalApproved, "checker-1"); err != nil {
		t.Fatal(err)
	}
	approved := f.dbNow(t)
	if f.heldAsOf(t, "holder-2", a.PrincipalRoleAssignmentID, pending) {
		t.Error("held as of while it was pending approval")
	}
	if !f.heldAsOf(t, "holder-2", a.PrincipalRoleAssignmentID, approved) {
		t.Error("not held as of after it was approved")
	}
}

func TestIAM13IT_QuantityLimitShape(t *testing.T) {
	f := newFixture(t)
	p := "approver-q"
	create := func(kind, currency, upper string) (*domain.AuthorityLimit, error) {
		return f.s.CreateAuthorityLimit(f.ctx, domain.CreateAuthorityLimitParams{
			TenantID: f.tenant, PrincipalID: &p, AuthorityType: "po_line_approval", LimitKind: kind,
			Currency: currency, UpperLimit: upper, RequestedBy: "maker-1",
		})
	}
	al, err := create(domain.AuthorityLimitQuantity, "", "50")
	if err != nil {
		t.Fatal(err)
	}
	if al.LimitKind != domain.AuthorityLimitQuantity || al.Currency != "" {
		t.Fatalf("stored QUANTITY limit: kind=%q currency=%q", al.LimitKind, al.Currency)
	}
	if got, err := f.s.FindAuthorityLimitByID(f.ctx, al.AuthorityLimitID, f.tenant); err != nil || got.LimitKind != domain.AuthorityLimitQuantity {
		t.Fatalf("read back: %+v %v", got, err)
	}
	for name, c := range map[string][2]string{
		"currency on a count": {"GBP", "50"},
		"fractional bound":    {"", "2.5"},
	} {
		if _, err := create(domain.AuthorityLimitQuantity, c[0], c[1]); !errors.Is(err, domain.ErrAuthorityLimitInvalid) {
			t.Errorf("%s: err %v, want ErrAuthorityLimitInvalid", name, err)
		}
	}
	m, err := create("", "", "1000.00")
	if err != nil || m.LimitKind != domain.AuthorityLimitMonetary || m.Currency != "GBP" {
		t.Fatalf("default limit: %+v %v", m, err)
	}
}

func TestIAM13IT_WorkloadBindingVersion(t *testing.T) {
	f := newFixture(t)
	wb, err := f.s.SaveWorkloadBinding(f.ctx, domain.WorkloadBinding{WorkloadID: "svc-" + uuid.NewString()[:8], TenantID: f.tenant,
		AllowedAudience: "authz", AllowedActions: []string{"x.read"}}, domain.OutboxMessage{EventType: "it", Key: "it", Value: []byte(`{}`), TenantID: f.tenant})
	if err != nil {
		t.Fatal(err)
	}
	ev := domain.OutboxMessage{EventType: "it", Key: wb.WorkloadID, Value: []byte(`{}`), TenantID: f.tenant}
	off, err := f.s.SetWorkloadBindingActive(f.ctx, wb.WorkloadID, f.tenant, false, ev)
	if err != nil || off.Version != wb.Version+1 {
		t.Fatalf("deactivate: version %d -> %+v %v", wb.Version, off, err)
	}
	again, err := f.s.SetWorkloadBindingActive(f.ctx, wb.WorkloadID, f.tenant, false, ev)
	if err != nil || again.Version != off.Version {
		t.Fatalf("a no-op deactivate moved the version: %d -> %+v %v", off.Version, again, err)
	}
}
