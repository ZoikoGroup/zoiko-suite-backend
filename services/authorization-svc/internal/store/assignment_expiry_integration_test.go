package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/authorization-svc/internal/domain"
)

// ORG-06 AssignmentExpired on a real database (AUTHZ_IT_DSN, migrated to
// 000038, as a NOBYPASSRLS role): iam.assignment.expired is emitted once, only
// for assignments that were granting and ran out, and never for one that was
// ended by a person.

// drain announces everything already lapsed in the shared database, so a test
// counts only what it creates.
func (f *fixture) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 100; i++ {
		n, err := f.s.SweepExpiredAssignments(context.Background(), 1000)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("the expiry sweep never drained")
}

// assignEnding grants an assignment (already approved or pending, as status says)
// whose window closed `ago` ago.
func (f *fixture) assignEnding(t *testing.T, principal, roleID, status string, ago time.Duration) *domain.PrincipalRoleAssignment {
	t.Helper()
	end := time.Now().Add(-ago)
	p := domain.CreateRoleAssignmentParams{PrincipalID: principal, RoleID: roleID, EffectiveFrom: end.Add(-time.Hour),
		EffectiveTo: &end, AssignedBy: "it-admin", ApprovalStatus: status}
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

func (f *fixture) sweep(t *testing.T, limit int) int {
	t.Helper()
	n, err := f.s.SweepExpiredAssignments(context.Background(), limit)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) expiredEvents(t *testing.T, id string) []map[string]any {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT message_value FROM outbox_events WHERE message_key = $1 AND event_type = 'iam.assignment.expired'`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var v map[string]any
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func TestExpiry_AnAssignmentThatRanOutIsAnnouncedExactlyOnce(t *testing.T) {
	f := newFixture(t)
	f.drain(t)
	role := f.role(t, "payment.approve")
	a := f.assignEnding(t, "lapsed-p", role.RoleID, domain.ApprovalApproved, time.Minute)
	id := a.PrincipalRoleAssignmentID

	if n := f.sweep(t, 100); n != 1 {
		t.Fatalf("first sweep announced %d, want 1", n)
	}
	evs := f.expiredEvents(t, id)
	if len(evs) != 1 {
		t.Fatalf("%d expired events, want exactly 1", len(evs))
	}
	if n := f.sweep(t, 100); n != 0 {
		t.Errorf("second sweep announced %d, want 0: an expiry is announced once", n)
	}
	if len(f.expiredEvents(t, id)) != 1 {
		t.Error("a repeat sweep emitted the event again")
	}

	ev := evs[0]
	if _, has := ev["actor_id"]; has {
		t.Errorf("actor_id = %v: nobody expires an assignment, its window closes", ev["actor_id"])
	}
	if ev["tenant_id"] != f.tenant || ev["source_service"] != "authorization-svc" {
		t.Errorf("envelope = %v", ev)
	}
	payload, _ := ev["payload"].(map[string]any)
	if payload["principal_role_assignment_id"] != id || payload["principal_id"] != "lapsed-p" || payload["role_id"] != role.RoleID {
		t.Errorf("payload = %v", payload)
	}
	if payload["effective_at"] == nil || payload["effective_at"] != payload["effective_to"] {
		t.Errorf("effective_at = %v, want the instant the window closed (%v)", payload["effective_at"], payload["effective_to"])
	}
}

// A person ending an assignment already announced it as revoked; calling the
// same end an "expiry" a moment later would say it twice.
func TestExpiry_AnAssignmentEndedByAPersonIsNotAlsoAnExpiry(t *testing.T) {
	f := newFixture(t)
	f.drain(t)
	role := f.role(t, "payment.approve")
	a := f.assign(t, "revoked-p", role.RoleID, domain.ApprovalApproved)
	id := a.PrincipalRoleAssignmentID

	if _, err := f.s.RevokeRoleAssignment(f.ctx, id, f.tenant); err != nil {
		t.Fatal(err)
	}
	f.sweep(t, 100)

	if n := len(f.expiredEvents(t, id)); n != 0 {
		t.Fatalf("%d expired events for an assignment a person revoked", n)
	}
	types := f.outboxTypes(t, id)
	if types[len(types)-1] != "iam.assignment.revoked" {
		t.Errorf("events = %v, want it to end with iam.assignment.revoked", types)
	}
}

// A scheduled end is announced when it is set (as revoked) and again when it
// arrives, because the second is the fact that the access stopped.
func TestExpiry_AScheduledEndIsAnnouncedWhenItArrives(t *testing.T) {
	f := newFixture(t)
	f.drain(t)
	role := f.role(t, "payment.approve")
	a := f.assign(t, "scheduled-p", role.RoleID, domain.ApprovalApproved)
	id := a.PrincipalRoleAssignmentID

	if _, err := f.s.ScheduleRoleAssignmentEnd(f.ctx, id, f.tenant, time.Now().Add(1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	f.sweep(t, 100)
	if n := len(f.expiredEvents(t, id)); n != 0 {
		t.Fatalf("announced as expired %d time(s) before its end date", n)
	}

	time.Sleep(1700 * time.Millisecond)
	f.sweep(t, 100)
	if n := len(f.expiredEvents(t, id)); n != 1 {
		t.Fatalf("%d expired events once the end date passed, want 1", n)
	}
	types := f.outboxTypes(t, id)
	if types[len(types)-2] != "iam.assignment.revoked" || types[len(types)-1] != "iam.assignment.expired" {
		t.Errorf("events = %v, want revoked (when scheduled) then expired (when it arrived)", types)
	}
}

// Only assignments that were granting are news. A suspended one that runs out
// still stopped being held; one that never granted never was.
func TestExpiry_OnlyWhatWasHeldIsAnnounced(t *testing.T) {
	f := newFixture(t)
	f.drain(t)
	role := f.role(t, "payment.approve")

	suspended := f.assign(t, "susp-p", role.RoleID, domain.ApprovalApproved)
	if _, err := f.s.SuspendRoleAssignment(f.ctx, suspended.PrincipalRoleAssignmentID, f.tenant, "admin", "hold"); err != nil {
		t.Fatal(err)
	}
	// Let its window close without anyone ending it: set a near end through the
	// scheduling path, which a suspended assignment still allows.
	if _, err := f.s.ScheduleRoleAssignmentEnd(f.ctx, suspended.PrincipalRoleAssignmentID, f.tenant, time.Now().Add(1200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	pending := f.assignEnding(t, "pend-p", role.RoleID, domain.ApprovalPending, time.Minute)
	rejected := f.assignEnding(t, "rej-p", role.RoleID, domain.ApprovalPending, time.Minute)
	if _, err := f.s.DecideRoleAssignment(f.ctx, rejected.PrincipalRoleAssignmentID, f.tenant, domain.ApprovalRejected, "checker"); err != nil {
		t.Fatal(err)
	}
	open := f.assign(t, "open-p", role.RoleID, domain.ApprovalApproved) // no end date

	time.Sleep(1400 * time.Millisecond)
	f.sweep(t, 100)

	if n := len(f.expiredEvents(t, suspended.PrincipalRoleAssignmentID)); n != 1 {
		t.Errorf("suspended assignment that ran out: %d events, want 1", n)
	}
	for name, id := range map[string]string{"pending": pending.PrincipalRoleAssignmentID, "rejected": rejected.PrincipalRoleAssignmentID, "open-ended": open.PrincipalRoleAssignmentID} {
		if n := len(f.expiredEvents(t, id)); n != 0 {
			t.Errorf("%s assignment announced as expired %d time(s)", name, n)
		}
	}
}

// One sweep covers every tenant: it is a background loop that belongs to none.
func TestExpiry_SweepsEveryTenant(t *testing.T) {
	a := newFixture(t)
	a.drain(t)
	b := newFixture(t)
	ra, rb := a.role(t, "x.y"), b.role(t, "x.y")
	xa := a.assignEnding(t, "tenant-a-p", ra.RoleID, domain.ApprovalApproved, time.Minute)
	xb := b.assignEnding(t, "tenant-b-p", rb.RoleID, domain.ApprovalApproved, time.Minute)

	if n := a.sweep(t, 100); n != 2 {
		t.Fatalf("announced %d, want 2 (one per tenant)", n)
	}
	evA, evB := a.expiredEvents(t, xa.PrincipalRoleAssignmentID), b.expiredEvents(t, xb.PrincipalRoleAssignmentID)
	if len(evA) != 1 || len(evB) != 1 {
		t.Fatalf("events: tenant A %d, tenant B %d, want 1 each", len(evA), len(evB))
	}
	if evA[0]["tenant_id"] != a.tenant || evB[0]["tenant_id"] != b.tenant {
		t.Errorf("an event was attributed to the wrong tenant: %v / %v", evA[0]["tenant_id"], evB[0]["tenant_id"])
	}
}

func TestExpiry_TheLimitBoundsAPassAndIsValidated(t *testing.T) {
	f := newFixture(t)
	f.drain(t)
	role := f.role(t, "x.y")
	for i := 0; i < 7; i++ {
		f.assignEnding(t, "bulk-"+uuid.NewString()[:6], role.RoleID, domain.ApprovalApproved, time.Minute)
	}
	if n := f.sweep(t, 3); n != 3 {
		t.Fatalf("limit 3 announced %d", n)
	}
	if n := f.sweep(t, 3); n != 3 {
		t.Fatalf("second pass announced %d, want 3", n)
	}
	if n := f.sweep(t, 3); n != 1 {
		t.Fatalf("last pass announced %d, want the remaining 1", n)
	}
	for _, bad := range []int{0, -1, 10001} {
		if _, err := f.s.SweepExpiredAssignments(context.Background(), bad); err == nil {
			t.Errorf("limit %d was accepted", bad)
		}
	}
}

// Replicas sweep at the same time. SKIP LOCKED makes them step over each other,
// so every expiry is announced exactly once.
func TestExpiry_ConcurrentSweepsAnnounceEachExpiryOnce(t *testing.T) {
	f := newFixture(t)
	f.drain(t)
	role := f.role(t, "x.y")
	var ids []string
	for i := 0; i < 24; i++ {
		a := f.assignEnding(t, "race-"+uuid.NewString()[:6], role.RoleID, domain.ApprovalApproved, time.Minute)
		ids = append(ids, a.PrincipalRoleAssignmentID)
	}

	var wg sync.WaitGroup
	total := make([]int, 6)
	for i := range total {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := f.s.SweepExpiredAssignments(context.Background(), 4)
				if err != nil {
					t.Error(err)
					return
				}
				total[i] += n
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	sum := 0
	for _, n := range total {
		sum += n
	}
	if sum != len(ids) {
		t.Errorf("sweepers announced %d in total, want %d", sum, len(ids))
	}
	for _, id := range ids {
		if n := len(f.expiredEvents(t, id)); n != 1 {
			t.Errorf("assignment %s announced %d times, want exactly once", id, n)
		}
	}
}

// The sweep is the only thing the application role may do with every tenant's
// assignments at once, and it must not be callable by anyone who should not.
func TestExpiry_TheSweepFunctionIsNotCallableByPublic(t *testing.T) {
	f := newFixture(t)
	var public bool
	err := f.pool.QueryRow(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM information_schema.routine_privileges
		                WHERE routine_name = 'authz_sweep_expired_assignments' AND grantee = 'PUBLIC')`).Scan(&public)
	if err != nil {
		t.Fatal(err)
	}
	if public {
		t.Error("EXECUTE on authz_sweep_expired_assignments is granted to PUBLIC")
	}
}
