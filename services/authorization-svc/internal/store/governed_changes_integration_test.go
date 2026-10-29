package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/authorization-svc/internal/domain"
)

// 000034-000036 on a real database (AUTHZ_IT_DSN, NOBYPASSRLS): the state each
// governed change moves through, the independence the database holds on its
// own, and the events that are — and are not — emitted.

func (f *fixture) exec(t *testing.T, sql string, args ...any) error {
	t.Helper()
	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", f.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
		return err
	}
	return tx.Commit(context.Background())
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// AU-02: a request grants nothing, the quorum is distinct people, the clock
// starts at activation, and the database refuses a self-approval by itself.
func TestGovernedIT_BreakGlassQuorum(t *testing.T) {
	f := newFixture(t)
	bg, err := f.s.CreateBreakGlassSession(f.ctx, domain.CreateBreakGlassSessionParams{
		TenantID: f.tenant, PrincipalID: "responder-1", IncidentID: "INC-1", Reason: "r",
		RequestedActions: []string{"gateway.failover"}, DurationSeconds: 600, RequestedBy: "requester-1", ApprovalsRequired: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bg.Status != domain.BreakGlassSessionStatusPendingApproval || bg.ApprovalsRequired != 2 || bg.RequestExpiresAt == nil {
		t.Fatalf("new request: %+v", bg)
	}
	if types := f.outboxTypes(t, bg.SessionID); len(types) != 1 || types[0] != "security.break_glass.requested" {
		t.Errorf("events on request = %v, want only requested", types)
	}

	for _, who := range []string{"responder-1", "requester-1"} {
		if _, err := f.s.ApproveBreakGlassSession(f.ctx, bg.SessionID, f.tenant, who, nil); !errors.Is(err, domain.ErrCheckerNotIndependent) {
			t.Errorf("approval by %s: err %v", who, err)
		}
	}
	// Past the store: the trigger refuses it too.
	if err := f.exec(t, `INSERT INTO break_glass_approvals (session_id, tenant_id, approver_id) VALUES ($1::uuid, $2::uuid, 'responder-1')`,
		bg.SessionID, f.tenant); err == nil {
		t.Error("the database stored the subject's own approval")
	}

	one, err := f.s.ApproveBreakGlassSession(f.ctx, bg.SessionID, f.tenant, "approver-1",
		&domain.StepUpEvidence{Assurance: "MFA", AuthnAt: time.Now(), SessionContextID: "sc-1"})
	if err != nil || one.Status != domain.BreakGlassSessionStatusPendingApproval || len(one.Approvals) != 1 {
		t.Fatalf("first of two approvals: %+v %v", one, err)
	}
	if one.Approvals[0].StepUp == nil || one.Approvals[0].StepUp.SessionContextID != "sc-1" {
		t.Errorf("approver step-up not kept: %+v", one.Approvals[0])
	}
	if _, err := f.s.ApproveBreakGlassSession(f.ctx, bg.SessionID, f.tenant, "approver-1", nil); !errors.Is(err, domain.ErrAlreadyApproved) {
		t.Errorf("same approver twice: err %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	two, err := f.s.ApproveBreakGlassSession(f.ctx, bg.SessionID, f.tenant, "approver-2", nil)
	if err != nil || two.Status != domain.BreakGlassSessionStatusActive || two.ApprovedBy == nil || *two.ApprovedBy != "approver-2" {
		t.Fatalf("quorum met: %+v %v", two, err)
	}
	if !two.ExpiresAt.After(two.ApprovedAt.Add(599 * time.Second)) {
		t.Errorf("duration did not start at activation: approved %v expires %v", two.ApprovedAt, two.ExpiresAt)
	}
	types := f.outboxTypes(t, bg.SessionID)
	if !has(types, "security.break_glass.started") || len(types) != 4 {
		t.Errorf("events = %v, want requested, approval_recorded x2, started", types)
	}
	if _, err := f.s.ApproveBreakGlassSession(f.ctx, bg.SessionID, f.tenant, "approver-3", nil); !errors.Is(err, domain.ErrSessionNotPending) {
		t.Errorf("approval after activation: err %v", err)
	}
}

func TestGovernedIT_BreakGlassRejectAndLapse(t *testing.T) {
	f := newFixture(t)
	req := func() *domain.BreakGlassSession {
		bg, err := f.s.CreateBreakGlassSession(f.ctx, domain.CreateBreakGlassSessionParams{
			TenantID: f.tenant, PrincipalID: "responder-1", IncidentID: "INC-2", Reason: "r",
			RequestedActions: []string{"gateway.failover"}, DurationSeconds: 600, RequestedBy: "requester-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		return bg
	}
	bg := req()
	if _, err := f.s.RejectBreakGlassSession(f.ctx, bg.SessionID, f.tenant, "requester-1"); !errors.Is(err, domain.ErrCheckerNotIndependent) {
		t.Errorf("requester rejecting: err %v", err)
	}
	rej, err := f.s.RejectBreakGlassSession(f.ctx, bg.SessionID, f.tenant, "approver-1")
	if err != nil || rej.Status != domain.BreakGlassSessionStatusRejected || rej.ReviewStatus != domain.SessionReviewNotRequired {
		t.Fatalf("rejected: %+v %v", rej, err)
	}
	if types := f.outboxTypes(t, bg.SessionID); !has(types, "security.break_glass.rejected") || has(types, "security.break_glass.started") {
		t.Errorf("events = %v", types)
	}

	bg = req()
	if err := f.exec(t, `UPDATE break_glass_sessions SET request_expires_at = NOW() - interval '1 minute' WHERE session_id = $1::uuid`, bg.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApproveBreakGlassSession(f.ctx, bg.SessionID, f.tenant, "approver-1", nil); !errors.Is(err, domain.ErrSessionNotPending) {
		t.Errorf("approving a lapsed request: err %v", err)
	}
	if _, err := f.s.ExpireDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := f.s.FindBreakGlassSessionByID(f.ctx, bg.SessionID, f.tenant)
	if got.Status != domain.BreakGlassSessionStatusExpired || got.ReviewStatus != domain.SessionReviewNotRequired {
		t.Errorf("lapsed request: status %s review %s", got.Status, got.ReviewStatus)
	}
}

// AU-13: a decision names its session and the log reads by it.
func TestGovernedIT_DecisionsBySession(t *testing.T) {
	f := newFixture(t)
	session := uuid.NewString()
	entity := uuid.NewString()
	for i, sess := range []string{session, session, ""} {
		p := domain.RecordAccessDecisionParams{PrincipalID: "p-1", LegalEntityID: entity, ActionType: "x.y",
			Outcome: domain.OutcomeGranted, Basis: "b", CorrelationID: "c", TenantID: f.tenant}
		if sess != "" {
			p.SessionKind, p.SessionID = domain.SessionKindBreakGlass, sess
		}
		if i == 1 {
			p.Outcome = domain.OutcomeDenied
		}
		d, err := f.s.RecordAccessDecision(f.ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if d.SessionID != sess {
			t.Errorf("recorded session %q, want %q", d.SessionID, sess)
		}
	}
	page, err := f.s.ListAccessDecisions(f.ctx, f.tenant, domain.ListAccessDecisionsParams{SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Decisions) != 2 || page.Decisions[0].SessionKind != domain.SessionKindBreakGlass {
		t.Errorf("decisions in the session = %d (%+v), want 2", len(page.Decisions), page.Decisions)
	}
}

// AU-17: a proposed rule denies nothing until someone else publishes it;
// retiring and reactivating wait the same way; a rejected proposal is final.
func TestGovernedIT_SoDRulePublication(t *testing.T) {
	f := newFixture(t)
	tenant := f.tenant
	a, b := "A_"+uuid.NewString()[:8], "B_"+uuid.NewString()[:8]
	rule, err := f.s.CreateSoDRule(f.ctx, domain.CreateSoDRuleParams{DomainCode: "FIN", ActionA: a, ActionB: b,
		ConflictType: "MUTUALLY_EXCLUSIVE", TenantID: &tenant, RequestedBy: "maker-1", Reason: "NEW_CONTROL"})
	if err != nil {
		t.Fatal(err)
	}
	if rule.ActiveFlag || rule.PublicationStatus != domain.PublicationPending || rule.PendingChange == nil || *rule.PendingChange != domain.RuleChangePublish {
		t.Fatalf("proposed rule: %+v", rule)
	}
	conflicts := func() bool {
		_, hit, err := f.s.CheckSoDConflict(f.ctx, []string{a}, b, f.tenant)
		if err != nil {
			t.Fatal(err)
		}
		return hit
	}
	if conflicts() {
		t.Fatal("an unpublished rule denied")
	}
	if types := f.outboxTypes(t, rule.SoDRuleID); has(types, "iam.sod_policy.published") || !has(types, "iam.sod_rule.change_requested") {
		t.Errorf("events on proposal = %v", types)
	}
	if _, err := f.s.DecideSoDRuleChange(f.ctx, rule.SoDRuleID, f.tenant, false, "maker-1", true); !errors.Is(err, domain.ErrCheckerNotIndependent) {
		t.Errorf("maker publishing: err %v", err)
	}
	if err := f.exec(t, `UPDATE sod_rules SET published_by = 'maker-1', published_at = NOW() WHERE sod_rule_id = $1`, rule.SoDRuleID); err == nil {
		t.Error("the database let the maker publish their own rule")
	}
	pub, err := f.s.DecideSoDRuleChange(f.ctx, rule.SoDRuleID, f.tenant, false, "checker-1", true)
	if err != nil || !pub.ActiveFlag || pub.PublicationStatus != domain.PublicationPublished || pub.PendingChange != nil {
		t.Fatalf("published: %+v %v", pub, err)
	}
	if !conflicts() {
		t.Fatal("a published rule did not deny")
	}
	if !has(f.outboxTypes(t, rule.SoDRuleID), "iam.sod_policy.published") {
		t.Error("no iam.sod_policy.published on publication")
	}

	// Retirement is a request: the rule keeps denying until it is approved.
	if _, changed, err := f.s.RequestSoDRuleChange(f.ctx, rule.SoDRuleID, f.tenant, domain.RuleChangeRetire, "maker-2", "SUPERSEDED", 0); err != nil || !changed {
		t.Fatalf("retire request: %v %v", changed, err)
	}
	if !conflicts() {
		t.Fatal("a rule with a pending retirement stopped denying")
	}
	if _, _, err := f.s.RequestSoDRuleChange(f.ctx, rule.SoDRuleID, f.tenant, domain.RuleChangeRetire, "maker-3", "", 0); !errors.Is(err, domain.ErrChangePending) {
		t.Errorf("second request: err %v", err)
	}
	before := len(f.outboxTypes(t, rule.SoDRuleID))
	if _, err := f.s.DecideSoDRuleChange(f.ctx, rule.SoDRuleID, f.tenant, false, "checker-1", false); err != nil {
		t.Fatal(err)
	}
	after := f.outboxTypes(t, rule.SoDRuleID)
	if !conflicts() || has(after[before:], "iam.sod_policy.published") || !has(after[before:], "iam.sod_rule.change_rejected") {
		t.Errorf("rejected retirement: still denying=%v, new events %v", conflicts(), after[before:])
	}
	if _, _, err := f.s.RequestSoDRuleChange(f.ctx, rule.SoDRuleID, f.tenant, domain.RuleChangeRetire, "maker-2", "", 0); err != nil {
		t.Fatal(err)
	}
	ret, err := f.s.DecideSoDRuleChange(f.ctx, rule.SoDRuleID, f.tenant, false, "checker-2", true)
	if err != nil || ret.ActiveFlag || conflicts() {
		t.Fatalf("approved retirement: %+v %v", ret, err)
	}
	// Already retired: nothing to approve, the state holds.
	if _, changed, err := f.s.RequestSoDRuleChange(f.ctx, rule.SoDRuleID, f.tenant, domain.RuleChangeRetire, "maker-2", "", 0); err != nil || changed {
		t.Errorf("retiring a retired rule: changed=%v err=%v", changed, err)
	}

	// A rejected proposal can never take effect.
	other, _ := f.s.CreateSoDRule(f.ctx, domain.CreateSoDRuleParams{DomainCode: "FIN", ActionA: a + "X", ActionB: b + "X",
		ConflictType: "MUTUALLY_EXCLUSIVE", TenantID: &tenant, RequestedBy: "maker-1"})
	rej, err := f.s.DecideSoDRuleChange(f.ctx, other.SoDRuleID, f.tenant, false, "checker-1", false)
	if err != nil || rej.PublicationStatus != domain.PublicationRejected || rej.ActiveFlag {
		t.Fatalf("rejected proposal: %+v %v", rej, err)
	}
	if _, _, err := f.s.RequestSoDRuleChange(f.ctx, other.SoDRuleID, f.tenant, domain.RuleChangeReactivate, "maker-1", "", 0); !errors.Is(err, domain.ErrRuleRejected) {
		t.Errorf("reactivating a rejected rule: err %v", err)
	}
	if err := f.exec(t, `UPDATE sod_rules SET active_flag = TRUE WHERE sod_rule_id = $1`, other.SoDRuleID); err == nil {
		t.Error("the database activated a rejected rule")
	}
}

func TestGovernedIT_ABACRulePublication(t *testing.T) {
	f := newFixture(t)
	tenant := f.tenant
	v := "GB"
	action := "X_" + uuid.NewString()[:8]
	rule, err := f.s.CreateABACRule(f.ctx, domain.CreateABACRuleParams{TenantID: &tenant, RuleCode: "R_" + uuid.NewString()[:8],
		ActionType: action, Effect: domain.EffectRequire, AttributeKey: "country", Operator: "eq", AttributeValue: &v,
		CreatedByPrincipalID: "maker-1", Reason: "NEW"})
	if err != nil || rule.ActiveFlag || rule.PublicationStatus != domain.PublicationPending {
		t.Fatalf("proposed: %+v %v", rule, err)
	}
	if rules, _ := f.s.FindABACRules(f.ctx, action, f.tenant); len(rules) != 0 {
		t.Fatal("an unpublished ABAC rule is evaluated")
	}
	if _, err := f.s.DecideABACRuleChange(f.ctx, rule.ABACRuleID, f.tenant, false, "maker-1", true); !errors.Is(err, domain.ErrCheckerNotIndependent) {
		t.Errorf("maker: err %v", err)
	}
	if _, err := f.s.DecideABACRuleChange(f.ctx, rule.ABACRuleID, f.tenant, false, "checker-1", true); err != nil {
		t.Fatal(err)
	}
	if rules, _ := f.s.FindABACRules(f.ctx, action, f.tenant); len(rules) != 1 {
		t.Fatal("a published ABAC rule is not evaluated")
	}
}

// AU-03: create, change and retire are requests; approval is by someone else;
// a change supersedes the limit it replaces at the same instant.
func TestGovernedIT_AuthorityLimitLifecycle(t *testing.T) {
	f := newFixture(t)
	holder := "holder-" + uuid.NewString()[:8]
	al, err := f.s.CreateAuthorityLimit(f.ctx, domain.CreateAuthorityLimitParams{TenantID: f.tenant, PrincipalID: &holder,
		AuthorityType: "invoice_approval", Currency: "GBP", UpperLimit: "1000", RequestedBy: "maker-1", RequestReason: "NEW"})
	if err != nil || al.Status != domain.AuthorityLimitPending {
		t.Fatalf("requested: %+v %v", al, err)
	}
	if _, err := f.s.DecideAuthorityLimit(f.ctx, al.AuthorityLimitID, f.tenant, "maker-1", true); !errors.Is(err, domain.ErrCheckerNotIndependent) {
		t.Errorf("maker approving: err %v", err)
	}
	if err := f.exec(t, `UPDATE authority_limits SET decided_by = 'maker-1', decided_at = NOW() WHERE authority_limit_id = $1::uuid`, al.AuthorityLimitID); err == nil {
		t.Error("the database let the maker decide")
	}
	active, err := f.s.DecideAuthorityLimit(f.ctx, al.AuthorityLimitID, f.tenant, "checker-1", true)
	if err != nil || active.Status != domain.AuthorityLimitActive {
		t.Fatalf("approved: %+v %v", active, err)
	}
	if !has(f.outboxTypes(t, al.AuthorityLimitID), "iam.authority_limit.changed") {
		t.Error("no iam.authority_limit.changed on approval")
	}
	if _, err := f.s.DecideAuthorityLimit(f.ctx, al.AuthorityLimitID, f.tenant, "checker-2", true); !errors.Is(err, domain.ErrNoPendingChange) {
		t.Errorf("deciding again: err %v", err)
	}

	// A change: the maker of the first version may approve its successor.
	change, err := f.s.CreateAuthorityLimit(f.ctx, domain.CreateAuthorityLimitParams{TenantID: f.tenant, PrincipalID: &holder,
		AuthorityType: "invoice_approval", Currency: "GBP", UpperLimit: "5000", RequestedBy: "maker-2", SupersedesID: al.AuthorityLimitID})
	if err != nil || change.SupersedesID == nil {
		t.Fatalf("change: %+v %v", change, err)
	}
	if _, err := f.s.CreateAuthorityLimit(f.ctx, domain.CreateAuthorityLimitParams{TenantID: f.tenant, PrincipalID: &holder,
		AuthorityType: "invoice_approval", Currency: "GBP", UpperLimit: "9000", RequestedBy: "maker-3", SupersedesID: al.AuthorityLimitID}); !errors.Is(err, domain.ErrChangePending) {
		t.Errorf("a second open change: err %v", err)
	}
	if _, err := f.s.RequestAuthorityLimitRetire(f.ctx, al.AuthorityLimitID, f.tenant, "maker-3", "", 0); !errors.Is(err, domain.ErrChangePending) {
		t.Errorf("retiring while a change is open: err %v", err)
	}
	next, err := f.s.DecideAuthorityLimit(f.ctx, change.AuthorityLimitID, f.tenant, "maker-1", true)
	if err != nil || next.Status != domain.AuthorityLimitActive {
		t.Fatalf("change approved: %+v %v", next, err)
	}
	old, _ := f.s.FindAuthorityLimitByID(f.ctx, al.AuthorityLimitID, f.tenant)
	if old.Status != domain.AuthorityLimitSuperseded || old.EffectiveTo == nil || old.DecidedBy == nil || *old.DecidedBy != "checker-1" {
		t.Errorf("replaced version: %+v", old)
	}

	// Retirement waits for approval; a rejection keeps the limit.
	if _, err := f.s.RequestAuthorityLimitRetire(f.ctx, next.AuthorityLimitID, f.tenant, "maker-4", "LEAVER", next.Version+5); !errors.Is(err, domain.ErrVersionConflict) {
		t.Errorf("stale version: err %v", err)
	}
	pending, err := f.s.RequestAuthorityLimitRetire(f.ctx, next.AuthorityLimitID, f.tenant, "maker-4", "LEAVER", 0)
	if err != nil || pending.Status != domain.AuthorityLimitActive || pending.PendingAction == nil {
		t.Fatalf("retire requested: %+v %v", pending, err)
	}
	kept, err := f.s.DecideAuthorityLimit(f.ctx, next.AuthorityLimitID, f.tenant, "checker-1", false)
	if err != nil || kept.Status != domain.AuthorityLimitActive || kept.PendingAction != nil {
		t.Fatalf("retirement rejected: %+v %v", kept, err)
	}
	if _, err := f.s.RequestAuthorityLimitRetire(f.ctx, next.AuthorityLimitID, f.tenant, "maker-4", "LEAVER", 0); err != nil {
		t.Fatal(err)
	}
	retired, err := f.s.DecideAuthorityLimit(f.ctx, next.AuthorityLimitID, f.tenant, "checker-1", true)
	if err != nil || retired.Status != domain.AuthorityLimitRetired || retired.EffectiveTo == nil {
		t.Fatalf("retired: %+v %v", retired, err)
	}
	if _, err := f.s.CreateAuthorityLimit(f.ctx, domain.CreateAuthorityLimitParams{TenantID: f.tenant, PrincipalID: &holder,
		AuthorityType: "x", UpperLimit: "1", LowerLimit: "5", RequestedBy: "m"}); !errors.Is(err, domain.ErrAuthorityLimitInvalid) {
		t.Errorf("upper below lower: err %v", err)
	}
}
