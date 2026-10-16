package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
)

// 000033 and the B-01 session maximums on a real database (AUTHZ_IT_DSN,
// NOBYPASSRLS): step-up evidence is stored and read back for the requester
// and the approver, and an over-long request is refused, not clamped.

func TestStepUpIT_EvidenceStoredForRequesterAndApprover(t *testing.T) {
	f := newFixture(t)
	reqAt := time.Now().Add(-3 * time.Minute).Truncate(time.Second)
	ps, err := f.s.CreatePrivilegedSession(f.ctx, domain.CreatePrivilegedSessionParams{
		TenantID: f.tenant, PrincipalID: "subject-1", RequestedActions: []string{"PAYMENT_RELEASE"},
		TicketRef: "T-1", Reason: "r", DurationSeconds: 600, RequestedBy: "requester-1",
		ResourceScope: "payments/run-9", TargetEnvironment: "production", HighImpact: true,
		MaxDurationSeconds: 28800,
		StepUp:             &domain.StepUpEvidence{Assurance: "MFA", AuthnAt: reqAt, SessionContextID: "sc-req"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ps.RequesterStepUp == nil || ps.RequesterStepUp.Assurance != "MFA" || !ps.RequesterStepUp.AuthnAt.Equal(reqAt) ||
		ps.RequesterStepUp.SessionContextID != "sc-req" || ps.ApproverStepUp != nil {
		t.Fatalf("requester evidence: %+v / %+v", ps.RequesterStepUp, ps.ApproverStepUp)
	}

	apprAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	approved, err := f.s.DecidePrivilegedSession(f.ctx, ps.SessionID, f.tenant, "approver-1", true,
		&domain.StepUpEvidence{Assurance: "MFA", AuthnAt: apprAt, SessionContextID: "sc-appr"})
	if err != nil {
		t.Fatal(err)
	}
	if approved.ApproverStepUp == nil || !approved.ApproverStepUp.AuthnAt.Equal(apprAt) || approved.ApproverStepUp.SessionContextID != "sc-appr" {
		t.Fatalf("approver evidence: %+v", approved.ApproverStepUp)
	}
	got, err := f.s.FindPrivilegedSessionByID(f.ctx, ps.SessionID, f.tenant)
	if err != nil || got.RequesterStepUp == nil || got.ApproverStepUp == nil {
		t.Fatalf("read back: %+v %v", got, err)
	}

	bg, err := f.s.CreateBreakGlassSession(f.ctx, domain.CreateBreakGlassSessionParams{
		TenantID: f.tenant, PrincipalID: "subject-1", IncidentID: "INC-1", Reason: "r",
		RequestedActions: []string{"gateway.failover"}, RequestedBy: "requester-1", MaxDurationSeconds: 7200,
		StepUp: &domain.StepUpEvidence{Assurance: "MFA", AuthnAt: reqAt},
	})
	if err != nil || bg.RequesterStepUp == nil || bg.RequesterStepUp.Assurance != "MFA" || bg.DurationSeconds != 1800 {
		t.Fatalf("break-glass: %+v %v", bg, err)
	}

	// A row without evidence (pre-000033, or step-up off locally) reads back as nil.
	plain, err := f.s.CreatePrivilegedSession(f.ctx, domain.CreatePrivilegedSessionParams{
		TenantID: f.tenant, PrincipalID: "subject-2", RequestedActions: []string{"ledger.view"},
		TicketRef: "T-2", Reason: "r", DurationSeconds: 600, RequestedBy: "requester-1",
		ResourceScope: "ledger", TargetEnvironment: "production",
	})
	if err != nil || plain.RequesterStepUp != nil {
		t.Fatalf("no-evidence row: %+v %v", plain.RequesterStepUp, err)
	}
}

func TestStepUpIT_OverLongRequestsRefusedNotClamped(t *testing.T) {
	f := newFixture(t)
	_, err := f.s.CreatePrivilegedSession(f.ctx, domain.CreatePrivilegedSessionParams{
		TenantID: f.tenant, PrincipalID: "subject-1", RequestedActions: []string{"ledger.view"},
		TicketRef: "T-1", Reason: "r", DurationSeconds: 28801, RequestedBy: "r1",
		ResourceScope: "ledger", TargetEnvironment: "production", MaxDurationSeconds: 28800,
	})
	if !errors.Is(err, domain.ErrSessionDurationExceeded) {
		t.Fatalf("JIT over the max: %v", err)
	}
	// With no configured max the old ceiling refuses too, instead of clamping.
	_, err = f.s.CreateBreakGlassSession(f.ctx, domain.CreateBreakGlassSessionParams{
		TenantID: f.tenant, PrincipalID: "subject-1", IncidentID: "INC-1", Reason: "r",
		RequestedActions: []string{"gateway.failover"}, DurationSeconds: 7201, RequestedBy: "r1",
	})
	if !errors.Is(err, domain.ErrSessionDurationExceeded) {
		t.Fatalf("break-glass over the ceiling: %v", err)
	}
	ss, err := f.s.CreateSupportSession(f.ctx, domain.CreateSupportSessionParams{
		TenantID: f.tenant, SupportOperatorID: "op-1", TicketRef: "T", Purpose: "p", ReadOnly: true,
		AllowedActions: []string{"ledger.view"}, DurationSeconds: 14400, RequestedBy: "r1", MaxDurationSeconds: 14400,
	})
	if err != nil || ss.DurationSeconds != 14400 {
		t.Fatalf("support at the max: %+v %v", ss, err)
	}
}

func TestStepUpIT_CheckRefusesHalfEvidence(t *testing.T) {
	f := newFixture(t)
	ps, err := f.s.CreatePrivilegedSession(f.ctx, domain.CreatePrivilegedSessionParams{
		TenantID: f.tenant, PrincipalID: "subject-1", RequestedActions: []string{"ledger.view"},
		TicketRef: "T", Reason: "r", DurationSeconds: 60, RequestedBy: "r1", ResourceScope: "x", TargetEnvironment: "production",
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", f.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(),
		"UPDATE privileged_sessions SET requester_assurance = 'MFA' WHERE session_id = $1::uuid", ps.SessionID); err == nil {
		t.Fatal("an assurance level without its authentication time was accepted")
	}
}
