package store_test

import (
	"sort"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// AZ-1 keyset paging and the §26 operational snapshot on a real database
// (AUTHZ_IT_DSN, NOBYPASSRLS).

func TestAZ1IT_AssignmentKeysetTraversal(t *testing.T) {
	f := newFixture(t)
	r := f.role(t, "it.read")
	var want []string
	for _, p := range []string{"p-a", "p-b", "p-c", "p-d", "p-e"} {
		want = append(want, f.assign(t, p, r.RoleID, domain.ApprovalApproved).PrincipalRoleAssignmentID)
	}
	sort.Strings(want)

	var got []string
	after := ""
	for pages := 0; pages < 10; pages++ {
		// Scoped to this test's role: since 000042 a tenant's first role
		// also provisions the decision-log service readers' assignments.
		page, err := f.s.QueryRoleAssignments(f.ctx, domain.AssignmentQuery{
			TenantID: f.tenant, RoleID: r.RoleID, ActiveOnly: true, Limit: 3, Keyset: true, AfterID: after,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			got = append(got, a.PrincipalRoleAssignmentID)
		}
		after = page[len(page)-1].PrincipalRoleAssignmentID
	}
	if len(got) != len(want) {
		t.Fatalf("keyset traversal returned %d rows, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keyset order %v, want %v", got, want)
		}
	}

	// Another tenant's cursor position reads nothing of this tenant's (RLS
	// plus the tenant predicate).
	other := newFixture(t)
	page, err := other.s.QueryRoleAssignments(other.ctx, domain.AssignmentQuery{TenantID: other.tenant, Limit: 10, Keyset: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 0 {
		t.Fatalf("another tenant read %d of this tenant's assignments", len(page))
	}
}

// The snapshot reads every tenant's sessions under platform scope as the
// NOBYPASSRLS role — an unscoped read would count zero — and returns counts.
func TestOpsIT_OperationalSnapshotReads(t *testing.T) {
	f := newFixture(t)
	snap, err := f.s.OperationalSnapshot(f.ctx)
	if err != nil {
		t.Fatalf("snapshot as the app role: %v", err)
	}
	for _, kind := range []string{"jit", "break_glass", "support"} {
		if _, ok := snap.Sessions[kind]; !ok {
			t.Errorf("snapshot has no %s session counts", kind)
		}
	}
	if snap.OutboxPending < 0 || snap.OutboxStuck < 0 || snap.OutboxOldestPendingSeconds < 0 {
		t.Fatalf("negative outbox figures: %+v", snap)
	}
}
