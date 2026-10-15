package store_test

import (
	"testing"
)

// 000028: the watermark the decision cache keys on moves with every policy
// write and every assignment write, read by the app role.
func TestWatermarkIT_PolicyAndAssignmentWritesMoveIt(t *testing.T) {
	f := newFixture(t)
	p0, a0, err := f.s.VersionWatermark(f.ctx)
	if err != nil {
		t.Fatalf("watermark unreadable as the app role: %v", err)
	}

	r := f.role(t, "wm.action") // a role and a bundle: config history rows
	p1, a1, err := f.s.VersionWatermark(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p1 <= p0 {
		t.Fatalf("policy_version %d -> %d: a role/bundle write must move it", p0, p1)
	}

	a := f.assign(t, "p-wm", r.RoleID, "APPROVED")
	_, a2, _ := f.s.VersionWatermark(f.ctx)
	if a2 <= a1 {
		t.Fatalf("assignment_version %d -> %d: an assignment must move it", a1, a2)
	}
	if _, err := f.s.RevokeRoleAssignment(f.ctx, a.PrincipalRoleAssignmentID, f.tenant); err != nil {
		t.Fatal(err)
	}
	_, a3, _ := f.s.VersionWatermark(f.ctx)
	if a3 <= a2 {
		t.Fatalf("assignment_version %d -> %d: a revocation must move it", a2, a3)
	}
	_ = a0
}
