package store_test

import (
	"testing"

	"github.com/google/uuid"

	"zoiko.io/authorization-svc/internal/domain"
)

// TestMergedSubjectAssignmentsRepointed: identity.subject.merged moves the
// duplicate's live authority to the survivor and ends the duplicate's, once.
func TestMergedSubjectAssignmentsRepointed(t *testing.T) {
	f := newFixture(t)
	read := f.role(t, "it.merge.read")
	write := f.role(t, "it.merge.write")
	entity := uuid.NewString()
	dup, surv := "dup-"+uuid.NewString()[:8], "surv-"+uuid.NewString()[:8]
	f.assign(t, dup, read.RoleID, domain.ApprovalApproved)
	f.assign(t, dup, write.RoleID, domain.ApprovalApproved)
	f.assign(t, surv, read.RoleID, domain.ApprovalApproved) // already held: no second copy
	f.assign(t, dup, write.RoleID, domain.ApprovalPending)  // not live authority: not moved

	grants := func(principal string) []string {
		t.Helper()
		actions, _, err := f.s.FindGrantedActions(f.ctx, principal, entity, f.tenant)
		if err != nil {
			t.Fatal(err)
		}
		return actions
	}
	if has(grants(surv), "it.merge.write") {
		t.Fatal("precondition: the survivor does not hold write yet")
	}

	p := domain.RepointMergedSubjectParams{TenantID: f.tenant, SurvivorID: surv, DuplicateID: dup, Actor: "P-ADMIN", CorrelationID: "corr-merge"}
	moved, err := f.s.RepointMergedSubject(f.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Fatalf("reissued %d assignments, want 1 (write; read was already held)", moved)
	}
	if got := grants(surv); !has(got, "it.merge.write") || !has(got, "it.merge.read") {
		t.Fatalf("survivor grants after merge: %v", got)
	}
	if got := grants(dup); len(got) != 0 {
		t.Fatalf("the retired duplicate still holds %v", got)
	}

	again, err := f.s.RepointMergedSubject(f.ctx, p)
	if err != nil || again != 0 {
		t.Fatalf("a redelivery reissued %d (err %v); want 0", again, err)
	}

	other := newFixture(t)
	if n, err := other.s.RepointMergedSubject(other.ctx, domain.RepointMergedSubjectParams{
		TenantID: other.tenant, SurvivorID: "x-" + surv, DuplicateID: surv}); err != nil || n != 0 {
		t.Fatalf("a merge in another tenant touched this tenant's assignments: n %d err %v", n, err)
	}
	if !has(grants(surv), "it.merge.write") {
		t.Fatal("another tenant's merge ended this tenant's assignment")
	}
}
