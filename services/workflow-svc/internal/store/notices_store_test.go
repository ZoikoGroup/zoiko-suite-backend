package store_test

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/notify"
	"zoiko.io/workflow-svc/internal/outbox"
	"zoiko.io/workflow-svc/internal/store"
)

// noticeRow is one outbox row of destination "notification".
type noticeRow struct {
	notify.Notice
	eventType string
}

func noticesOf(t *testing.T, pool *pgxpool.Pool, instanceID string) []noticeRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT event_type, payload FROM outbox_events
		WHERE destination = $1 AND aggregate_id = $2 ORDER BY created_at, outbox_event_id`,
		outbox.DestinationNotification, instanceID)
	if err != nil {
		t.Fatalf("read notices: %v", err)
	}
	defer rows.Close()
	var out []noticeRow
	for rows.Next() {
		var r noticeRow
		var payload []byte
		if err := rows.Scan(&r.eventType, &payload); err != nil {
			t.Fatalf("scan notice: %v", err)
		}
		if err := json.Unmarshal(payload, &r.Notice); err != nil {
			t.Fatalf("decode notice: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// told lists "recipient/event" once per recipient (both channels collapsed).
func told(rows []noticeRow) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		k := r.RecipientPrincipalID + "/" + r.SourceEventType
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func wantTold(t *testing.T, step string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: told\n got  %v\n want %v", step, got, want)
	}
}

func newNoticeStore(t *testing.T) (*store.PgStore, *pgxpool.Pool) {
	t.Helper()
	pool := getTestPool(t)
	t.Cleanup(pool.Close)
	setupTestDB(t, pool)
	return store.New(pool, zap.NewNop()).WithNotices("https://console.test"), pool
}

func strp(s string) *string { return &s }

func TestPgStore_Notices_OffByDefault(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)
	s := store.New(pool, zap.NewNop())
	inst, _, _, err := s.CreateWorkflow(tenantCtx(testTenantID), twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if n := len(noticesOf(t, pool, inst.WorkflowInstanceID)); n != 0 {
		t.Fatalf("without WithNotices no notice is written, got %d", n)
	}
}

// The whole route: each transition tells the right people, in the decision's
// own transaction, and never the person who acted.
func TestPgStore_Notices_TwoStageRoute(t *testing.T) {
	s, pool := newNoticeStore(t)
	ctx := tenantCtx(testTenantID)

	inst, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := inst.WorkflowInstanceID
	rows := noticesOf(t, pool, id)
	wantTold(t, "created", told(rows), "approver-1/workflow.approval_requested")
	if len(rows) != 2 {
		t.Fatalf("one recipient on IN_APP and EMAIL = 2 rows, got %d", len(rows))
	}
	if !strings.Contains(rows[0].Body, "https://console.test/admin/workflows?id="+id) {
		t.Fatalf("the notice links to the workflow: %q", rows[0].Body)
	}

	if _, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{WorkflowInstanceID: id, ActorPrincipalID: "approver-1",
		Action: "APPROVE", Rationale: strp("Budget confirmed")}); err != nil {
		t.Fatalf("approve stage 1: %v", err)
	}
	rows = noticesOf(t, pool, id)
	wantTold(t, "stage 1 approved", told(rows[2:]), "approver-2/workflow.approval_requested", "requester-1/workflow.stage_approved")

	reason := "The quote is older than 30 days."
	if _, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{WorkflowInstanceID: id, ActorPrincipalID: "approver-2",
		Action: "REJECT", Rationale: &reason}); err != nil {
		t.Fatalf("reject stage 2: %v", err)
	}
	rows = noticesOf(t, pool, id)
	last := rows[6:]
	wantTold(t, "rejected", told(last), "requester-1/workflow.rejected")
	for _, r := range last {
		if !strings.Contains(r.Body, "Reason: "+reason) {
			t.Fatalf("the requester's rejection notice quotes the reason: %q", r.Body)
		}
	}
	for _, r := range rows {
		if r.eventType != r.SourceEventType {
			t.Fatalf("outbox event_type %q must match the notice's %q", r.eventType, r.SourceEventType)
		}
	}
}

// A replayed command (same Idempotency-Key, same body) is not a new decision
// and tells nobody again.
func TestPgStore_Notices_ReplayTellsNobodyTwice(t *testing.T) {
	s, pool := newNoticeStore(t)
	ctx := tenantCtx(testTenantID)
	inst, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	approve := domain.SubmitActionParams{WorkflowInstanceID: inst.WorkflowInstanceID, ActorPrincipalID: "approver-1",
		Action: "APPROVE", Meta: domain.CommandMeta{IdempotencyKey: "approve-once", RequestHash: "h1"}}
	if _, _, _, err := s.SubmitAction(ctx, approve); err != nil {
		t.Fatalf("approve: %v", err)
	}
	before := len(noticesOf(t, pool, inst.WorkflowInstanceID))
	if _, _, _, err := s.SubmitAction(ctx, approve); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if after := len(noticesOf(t, pool, inst.WorkflowInstanceID)); after != before {
		t.Fatalf("a replay wrote %d more notices", after-before)
	}
}

// A refused decision writes nothing: the notices live and die with it.
func TestPgStore_Notices_RefusedDecisionWritesNone(t *testing.T) {
	s, pool := newNoticeStore(t)
	ctx := tenantCtx(testTenantID)
	inst, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before := len(noticesOf(t, pool, inst.WorkflowInstanceID))
	if _, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "approver-2", Action: "APPROVE"}); err == nil {
		t.Fatal("stage 2's approver cannot decide stage 1")
	}
	if after := len(noticesOf(t, pool, inst.WorkflowInstanceID)); after != before {
		t.Fatalf("a refused decision wrote %d notices", after-before)
	}
}

func TestPgStore_Notices_QuorumPoolIsTold(t *testing.T) {
	s, pool := newNoticeStore(t)
	p := twoStageParams()
	p.Stages = []domain.CreateWorkflowStageInput{{StageType: domain.StageTypeQuorum, RequiredApprovals: 2,
		QuorumApprovers: []string{"voter-b", "voter-a", "voter-c"}}}
	inst, _, _, err := s.CreateWorkflow(tenantCtx(testTenantID), p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wantTold(t, "quorum created", told(noticesOf(t, pool, inst.WorkflowInstanceID)),
		"voter-a/workflow.approval_requested", "voter-b/workflow.approval_requested", "voter-c/workflow.approval_requested")
}

func TestPgStore_Notices_RoleStageNamesNobody(t *testing.T) {
	s, pool := newNoticeStore(t)
	p := twoStageParams()
	p.Stages = []domain.CreateWorkflowStageInput{{StageType: domain.StageTypeRole, ApproverRole: "TAX_OFFICER"}}
	inst, _, _, err := s.CreateWorkflow(tenantCtx(testTenantID), p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if n := len(noticesOf(t, pool, inst.WorkflowInstanceID)); n != 0 {
		t.Fatalf("a ROLE stage has no named approver to tell, got %d notices", n)
	}
}

func TestPgStore_Notices_WithdrawTellsTheWaitingApprover(t *testing.T) {
	s, pool := newNoticeStore(t)
	ctx := tenantCtx(testTenantID)
	inst, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := s.CancelWorkflow(ctx, inst.WorkflowInstanceID, "requester-1",
		domain.LifecycleCommand{Rationale: strp("No longer needed")}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	rows := noticesOf(t, pool, inst.WorkflowInstanceID)
	wantTold(t, "withdrawn", told(rows[2:]), "approver-1/workflow.cancelled")
	if !strings.Contains(rows[2].Body, "Reason: No longer needed") {
		t.Fatalf("the cancellation quotes the reason: %q", rows[2].Body)
	}
}

func TestPgStore_Notices_EscalateToTellsTargetAndRequester(t *testing.T) {
	s, pool := newNoticeStore(t)
	ctx := tenantCtx(testTenantID)
	inst, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := s.EscalateToWorkflow(ctx, domain.EscalateToWorkflowParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "admin-1", EscalationTarget: "manager-1", Narrative: strp("Approver on leave")}); err != nil {
		t.Fatalf("escalate-to: %v", err)
	}
	wantTold(t, "escalated", told(noticesOf(t, pool, inst.WorkflowInstanceID)[2:]),
		"manager-1/workflow.escalated_to_you", "requester-1/workflow.escalated")
}

func TestPgStore_Notices_ReopenTellsStageOneAndRequester(t *testing.T) {
	s, pool := newNoticeStore(t)
	ctx := tenantCtx(testTenantID)
	inst, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, err := s.SubmitAction(ctx, domain.SubmitActionParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "approver-1", Action: "REJECT", Rationale: strp("Wrong cost centre")}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	before := len(noticesOf(t, pool, inst.WorkflowInstanceID))
	if err := s.RequestChanges(ctx, inst.WorkflowInstanceID, "admin-1", "Fix the cost centre and resubmit", nil); err != nil {
		t.Fatalf("request changes: %v", err)
	}
	rows := noticesOf(t, pool, inst.WorkflowInstanceID)[before:]
	wantTold(t, "reopened", told(rows), "approver-1/workflow.resubmitted", "requester-1/workflow.changes_requested")
	for _, r := range rows {
		if r.RecipientPrincipalID == "requester-1" && !strings.Contains(r.Body, "Reason: Fix the cost centre and resubmit") {
			t.Fatalf("the requester hears the reason: %q", r.Body)
		}
	}
}

func TestPgStore_Notices_InvalidateTellsTheRequester(t *testing.T) {
	s, pool := newNoticeStore(t)
	ctx := tenantCtx(testTenantID)
	inst, _, _, err := s.CreateWorkflow(ctx, twoStageParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := s.InvalidateWorkflow(ctx, domain.InvalidateWorkflowParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "owner-1", ReasonCode: "SUBJECT_CHANGED", Narrative: strp("The amount changed")}); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	rows := noticesOf(t, pool, inst.WorkflowInstanceID)[2:]
	wantTold(t, "invalidated", told(rows), "approver-1/workflow.invalidated", "requester-1/workflow.invalidated")
	for _, r := range rows {
		if !strings.Contains(r.Body, "Reason: The amount changed") {
			t.Fatalf("the invalidation quotes its narrative: %q", r.Body)
		}
	}
}

// As the runtime role (NOSUPERUSER NOBYPASSRLS): the stage and quorum-pool
// reads and the outbox writes all run under the instance's tenant, so FORCE
// RLS on those tables must not hide the approvers from the notice step.
func TestPgStore_RLS_NoticesAsTheRuntimeRole(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(appRolePool(t, admin), zap.NewNop()).WithNotices("")
	ctx := tenantCtx(testTenantID)

	p := twoStageParams()
	p.Stages = []domain.CreateWorkflowStageInput{
		{StageType: domain.StageTypeQuorum, RequiredApprovals: 1, QuorumApprovers: []string{"voter-a", "voter-b"}},
		{ApproverPrincipalID: "approver-2"},
	}
	inst, _, _, err := s.CreateWorkflow(ctx, p)
	if err != nil {
		t.Fatalf("create as runtime role: %v", err)
	}
	wantTold(t, "created", told(noticesOf(t, admin, inst.WorkflowInstanceID)),
		"voter-a/workflow.approval_requested", "voter-b/workflow.approval_requested")

	if _, _, _, err := s.SubmitQuorumVote(ctx, domain.SubmitQuorumVoteParams{WorkflowInstanceID: inst.WorkflowInstanceID,
		ActorPrincipalID: "voter-a", Action: "APPROVE"}); err != nil {
		t.Fatalf("quorum vote as runtime role: %v", err)
	}
	rows := noticesOf(t, admin, inst.WorkflowInstanceID)
	wantTold(t, "quorum met", told(rows[4:]), "approver-2/workflow.approval_requested", "requester-1/workflow.stage_approved")
	if !strings.HasPrefix(rows[4].Body, "Stage 1 was approved.") &&
		!strings.HasPrefix(rows[4].Body, "Stage 1 of your request") &&
		!strings.HasPrefix(rows[4].Body, "<p>Stage 1 of your request") {
		t.Fatalf("unexpected body: %q", rows[4].Body)
	}
	if strings.Contains(rows[4].Body+rows[6].Body, "quorum vote") {
		t.Fatal("the synthetic quorum tally is not a comment and must not be quoted")
	}
}
