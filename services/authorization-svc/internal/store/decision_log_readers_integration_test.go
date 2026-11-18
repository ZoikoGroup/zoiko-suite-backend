package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// AC-2 / D2-2.F07: migration 000042 on a real database (AUTHZ_IT_DSN, migrated
// to 000042, as a NOBYPASSRLS role). The governance decision log's two service
// readers hold GOVERNANCE_DECISION_READ in a tenant from its first role on,
// nothing more, and a tenant administrator's revocation stands.

const (
	actionDecisionRead   = "GOVERNANCE_DECISION_READ"
	actionDecisionRecord = "GOVERNANCE_DECISION_RECORD"
)

var decisionLogReaders = []string{"svc-contract-lifecycle", "svc-decision-support"}

func (f *fixture) granted(t *testing.T, principal, entity, action string) bool {
	t.Helper()
	actions, _, err := f.s.FindGrantedActionsScoped(f.ctx, principal, entity, f.tenant, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a == action {
			return true
		}
	}
	return false
}

func TestAC2_DecisionLogReadersProvisionedWithATenantsFirstRole(t *testing.T) {
	f := newFixture(t)
	entity := uuid.NewString()
	for _, p := range decisionLogReaders {
		if f.granted(t, p, entity, actionDecisionRead) {
			t.Fatalf("precondition: %s holds READ in a tenant that has no role yet", p)
		}
	}

	f.role(t, actionPay) // the tenant's first role, created through the store

	for _, p := range decisionLogReaders {
		if !f.granted(t, p, entity, actionDecisionRead) {
			t.Fatalf("%s must hold %s on any entity of a provisioned tenant (tenant-wide assignment)", p, actionDecisionRead)
		}
		if f.granted(t, p, entity, actionDecisionRecord) {
			t.Fatalf("%s is a reader: it must not hold %s", p, actionDecisionRecord)
		}
		if f.granted(t, p, entity, actionPay) {
			t.Fatalf("%s must hold only the ledger read", p)
		}
	}
	if f.granted(t, "someone-else", entity, actionDecisionRead) {
		t.Fatal("only the two reader principals are provisioned")
	}

	// Another tenant's grant never leaks: a fresh tenant with no role is
	// still empty although this one is provisioned.
	other := newFixture(t)
	if other.granted(t, decisionLogReaders[0], entity, actionDecisionRead) {
		t.Fatal("a grant in one tenant answered for another")
	}
}

// Provisioning happens once per tenant: an administrator who ends a reader's
// assignment is not overridden when the tenant later creates more roles.
func TestAC2_AnEndedReaderAssignmentIsNotRegranted(t *testing.T) {
	f := newFixture(t)
	entity := uuid.NewString()
	f.role(t)

	assignments, err := f.s.ListRoleAssignments(f.ctx, f.tenant, "svc-decision-support", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected the one provisioned assignment, got %d", len(assignments))
	}
	if _, err := f.s.RevokeRoleAssignment(f.ctx, assignments[0].PrincipalRoleAssignmentID, f.tenant); err != nil {
		t.Fatal(err)
	}
	if f.granted(t, "svc-decision-support", entity, actionDecisionRead) {
		t.Fatal("the revoked reader still holds READ")
	}

	f.role(t)
	f.role(t, actionPay)

	if f.granted(t, "svc-decision-support", entity, actionDecisionRead) {
		t.Fatal("creating more roles re-granted a reader an administrator had revoked")
	}
	if !f.granted(t, "svc-contract-lifecycle", entity, actionDecisionRead) {
		t.Fatal("the other reader's grant must be untouched")
	}
}

// AC-2: migration 000043 provisions the platform's other service identities
// in a tenant from its first role on, each with exactly its own actions.
func TestAC2_PlatformServiceIdentitiesProvisionedWithATenantsFirstRole(t *testing.T) {
	f := newFixture(t)
	entity := uuid.NewString()
	if f.granted(t, "svc-policy", entity, actionDecisionRecord) {
		t.Fatal("precondition: svc-policy holds RECORD in a tenant that has no role yet")
	}

	f.role(t)

	for _, p := range []string{"svc-policy", "svc-workflow"} {
		if !f.granted(t, p, entity, actionDecisionRecord) {
			t.Fatalf("%s must hold %s on any entity of the tenant", p, actionDecisionRecord)
		}
		if f.granted(t, p, entity, actionDecisionRead) || f.granted(t, p, f.tenant, "iam.assignment.revoke") {
			t.Fatalf("%s is a ledger writer only", p)
		}
	}
	// access-control-svc's review executor: the iam.* checks evaluate on the
	// tenant itself as the legal entity.
	for _, a := range []string{"iam.assignment.revoke", "iam.assignment.read"} {
		if !f.granted(t, "svc-access-control", f.tenant, a) {
			t.Fatalf("svc-access-control must hold %s on the tenant scope", a)
		}
	}
	if f.granted(t, "svc-access-control", f.tenant, "iam.assignment.grant") || f.granted(t, "svc-access-control", entity, actionDecisionRecord) {
		t.Fatal("svc-access-control may revoke and read, nothing more")
	}

	// Once per tenant: a revoked writer is not re-granted by later roles.
	assignments, err := f.s.ListRoleAssignments(f.ctx, f.tenant, "svc-workflow", "", true)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("expected one svc-workflow assignment, got %d (%v)", len(assignments), err)
	}
	if _, err := f.s.RevokeRoleAssignment(f.ctx, assignments[0].PrincipalRoleAssignmentID, f.tenant); err != nil {
		t.Fatal(err)
	}
	f.role(t)
	if f.granted(t, "svc-workflow", entity, actionDecisionRecord) {
		t.Fatal("creating another role re-granted a writer an administrator had revoked")
	}
	if !f.granted(t, "svc-policy", entity, actionDecisionRecord) {
		t.Fatal("the other writer's grant must be untouched")
	}
}
