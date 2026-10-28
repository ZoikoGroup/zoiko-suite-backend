package store_test

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// 000030 on a real database (AUTHZ_IT_DSN, NOBYPASSRLS): approval and review
// independence held by CHECKs, the review opening when a session ends, the
// §23 events from the outbox triggers, and the expiry sweep ending sessions.

func (f *fixture) backdate(t *testing.T, table, sessionID string) {
	t.Helper()
	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", f.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), "UPDATE "+table+" SET expires_at = NOW() - interval '1 minute' WHERE session_id = $1::uuid", sessionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSessionControlsIT_JITApprovalReviewAndEvents(t *testing.T) {
	f := newFixture(t)
	ps, err := f.s.CreatePrivilegedSession(f.ctx, domain.CreatePrivilegedSessionParams{
		TenantID: f.tenant, PrincipalID: "subject-1", RequestedActions: []string{"PAYMENT_RELEASE"},
		TicketRef: "T-1", Reason: "r", DurationSeconds: 600, RequestedBy: "requester-1",
		ResourceScope: "payments/run-9", TargetEnvironment: "production", HighImpact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ps.Status != domain.PrivilegedSessionStatusPendingApproval {
		t.Fatalf("high-impact session status = %s, want PENDING_APPROVAL", ps.Status)
	}
	if types := f.outboxTypes(t, ps.SessionID); len(types) != 0 {
		t.Errorf("a pending session announced %v; it has not started", types)
	}

	// The database refuses a non-independent approver even past the handler.
	for _, approver := range []string{"subject-1", "requester-1"} {
		if _, err := f.s.DecidePrivilegedSession(f.ctx, ps.SessionID, f.tenant, approver, true, nil); err == nil {
			t.Errorf("approval by %s was stored", approver)
		}
	}
	approved, err := f.s.DecidePrivilegedSession(f.ctx, ps.SessionID, f.tenant, "approver-1", true, nil)
	if err != nil || approved.Status != domain.PrivilegedSessionStatusActive || approved.ApprovedBy == nil || *approved.ApprovedBy != "approver-1" {
		t.Fatalf("independent approval: %+v %v", approved, err)
	}
	if _, err := f.s.DecidePrivilegedSession(f.ctx, ps.SessionID, f.tenant, "approver-2", true, nil); !errors.Is(err, domain.ErrSessionNotPending) {
		t.Errorf("second decision: err %v, want ErrSessionNotPending", err)
	}

	if _, err := f.s.RevokePrivilegedSession(f.ctx, ps.SessionID, f.tenant, "requester-1"); err != nil {
		t.Fatal(err)
	}
	ended, _ := f.s.FindPrivilegedSessionByID(f.ctx, ps.SessionID, f.tenant)
	if ended.ReviewStatus != domain.SessionReviewPending {
		t.Errorf("ended high-impact session review_status = %s, want PENDING", ended.ReviewStatus)
	}
	if types := f.outboxTypes(t, ps.SessionID); len(types) != 2 || types[0] != "security.privileged_session.started" || types[1] != "security.privileged_session.ended" {
		t.Errorf("events = %v", types)
	}

	if err := f.s.ReviewSession(f.ctx, "privileged", ps.SessionID, f.tenant, "subject-1", domain.SessionReviewAttested, ""); err == nil {
		t.Error("the subject reviewed their own session")
	}
	if err := f.s.ReviewSession(f.ctx, "privileged", ps.SessionID, f.tenant, "reviewer-1", domain.SessionReviewAttested, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReviewSession(f.ctx, "privileged", ps.SessionID, f.tenant, "reviewer-2", domain.SessionReviewAttested, ""); !errors.Is(err, domain.ErrSessionReviewNotPending) {
		t.Errorf("second review: err %v, want ErrSessionReviewNotPending", err)
	}
}

// A low-risk JIT session is active at once and needs no review.
func TestSessionControlsIT_LowRiskJITNeedsNoReview(t *testing.T) {
	f := newFixture(t)
	ps, err := f.s.CreatePrivilegedSession(f.ctx, domain.CreatePrivilegedSessionParams{
		TenantID: f.tenant, PrincipalID: "subject-2", RequestedActions: []string{"ledger.view"},
		TicketRef: "T", Reason: "r", DurationSeconds: 600, RequestedBy: "subject-2", ResourceScope: "x", TargetEnvironment: "prod",
	})
	if err != nil || ps.Status != domain.PrivilegedSessionStatusActive {
		t.Fatalf("low-risk session: %+v %v", ps, err)
	}
	if _, err := f.s.RevokePrivilegedSession(f.ctx, ps.SessionID, f.tenant, "subject-2"); err != nil {
		t.Fatal(err)
	}
	ended, _ := f.s.FindPrivilegedSessionByID(f.ctx, ps.SessionID, f.tenant)
	if ended.ReviewStatus != domain.SessionReviewNotRequired {
		t.Errorf("low-risk review_status = %s, want NOT_REQUIRED", ended.ReviewStatus)
	}
}

// Break-glass: expiry is recorded by the sweep, which fires .ended and opens
// the mandatory review; support sessions announce themselves too.
func TestSessionControlsIT_BreakGlassExpiryAndSupportEvents(t *testing.T) {
	f := newFixture(t)
	bg, err := f.s.CreateBreakGlassSession(f.ctx, domain.CreateBreakGlassSessionParams{
		TenantID: f.tenant, PrincipalID: "responder-1", IncidentID: "INC-7", Reason: "r",
		RequestedActions: []string{"gateway.failover"}, DurationSeconds: 600, RequestedBy: "responder-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// AU-02 (000034): active only once an independent approver approves it.
	if _, err := f.s.ApproveBreakGlassSession(f.ctx, bg.SessionID, f.tenant, "approver-1", nil); err != nil {
		t.Fatal(err)
	}
	f.backdate(t, "break_glass_sessions", bg.SessionID)
	if _, err := f.s.ExpireDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := f.s.FindBreakGlassSessionByID(f.ctx, bg.SessionID, f.tenant)
	if got.Status != domain.BreakGlassSessionStatusExpired || got.ReviewStatus != domain.SessionReviewPending {
		t.Errorf("after the sweep: status %s, review %s; want EXPIRED / PENDING", got.Status, got.ReviewStatus)
	}
	// requested, approval_recorded, started, ended.
	if types := f.outboxTypes(t, bg.SessionID); len(types) != 4 || types[3] != "security.break_glass.ended" {
		t.Errorf("break-glass events = %v", types)
	}

	ss, err := f.s.CreateSupportSession(f.ctx, domain.CreateSupportSessionParams{
		TenantID: f.tenant, SupportOperatorID: "support-1", TicketRef: "S-1", Purpose: "p",
		ReadOnly: true, DurationSeconds: 600, TenantConsentObtained: true, RequestedBy: "support-lead",
	})
	if err != nil {
		t.Fatal(err)
	}
	if types := f.outboxTypes(t, ss.SessionID); len(types) != 1 || types[0] != "support.session.started" {
		t.Errorf("support events = %v", types)
	}

}
