package store_test

// R-001 WFC-05 deadline clocks on a real Postgres, as a NOSUPERUSER,
// NOBYPASSRLS role that does not own the tables (the runtime role's shape).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/store"
)

type clockEnv struct {
	admin *pgxpool.Pool
	s     *store.PgStore
	ctx   context.Context
	obl   *domain.Obligation
}

func newClockEnv(t *testing.T) *clockEnv {
	t.Helper()
	admin := getTestPool(t)
	t.Cleanup(admin.Close)
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)
	o, _, err := s.CreateObligation(ctx, validParams())
	if err != nil {
		t.Fatalf("create obligation: %v", err)
	}
	return &clockEnv{admin: admin, s: s, ctx: ctx, obl: o}
}

func calcAt(due time.Time, calVersion int, calVersionID string) domain.Calculation {
	return domain.Calculation{DecisionID: "dec-1", Outcome: domain.CalculationOutcomeCalculated, ObligationRuleID: "6f1e8a52-1d7b-4d7a-9a3c-2b0c6c9e0a11",
		RuleVersion: 3, ContentDigest: "sha256:rule", AnchorDate: "2026-11-30", UnadjustedDate: "2027-01-07", DueDate: due.Format("2006-01-02"),
		DueAt: &due, Timezone: "UTC", CalendarCode: "gb-hmrc", CalendarVersion: calVersion, CalendarVersionID: calVersionID,
		BusinessDayAdjustment: "NEXT", ExtensionGranted: 0, PackRef: "gb-tax", PackVersion: "1.4.0", PackVersionID: "pv-1",
		Explanation: json.RawMessage(`["anchor 2026-11-30","due at 2027-01-07T23:59:59Z"]`)}
}

func (e *clockEnv) start(t *testing.T, key string, due time.Time, class domain.ClockClass, extra ...domain.MilestoneSpec) *domain.Clock {
	t.Helper()
	dueSoon := due.Add(-7 * 24 * time.Hour)
	c, created, err := e.s.StartClock(e.ctx, domain.StartClockParams{ObligationID: e.obl.ObligationID, ClockKey: key, RequestHash: "h-" + key,
		ClockClass: class, JurisdictionCode: "GB", RuleObligationCode: "VAT_RETURN", Calc: calcAt(due, 1, "cv-1"),
		Trigger: domain.TriggerEvidence{EventID: "evt-" + key, EventType: "period.closed", EventTime: time.Now().UTC().Add(-time.Hour),
			SourceObjectType: "ACCOUNTING_PERIOD", SourceObjectID: "p-2026-11"},
		DueSoonAt: &dueSoon, ExtraMilestones: extra, CalculationRequest: json.RawMessage(`{"jurisdiction":"GB","obligation_code":"VAT_RETURN","period_end":"2026-11-30"}`),
		Actor: "maker-1", CorrelationID: "corr-" + key})
	if err != nil {
		t.Fatalf("start clock: %v", err)
	}
	if !created {
		t.Fatalf("start clock %s: created=false on first start", key)
	}
	return c
}

func (e *clockEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// OB-S6 / R-INV-16: the calculation is stored verbatim; the key is idempotent.
func TestWFC05_StartClock_ProvenanceRoundTripsAndKeyIsIdempotent(t *testing.T) {
	e := newClockEnv(t)
	due := time.Date(2027, 1, 7, 23, 59, 59, 0, time.UTC)
	c := e.start(t, "k1", due, domain.ClockClassHardLegal)
	got, err := e.s.GetClock(e.ctx, c.ClockID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.DueAt.Equal(due) || got.LocalDueDate != "2027-01-07" || got.Timezone != "UTC" || *got.CalendarVersionID != "cv-1" ||
		*got.CalendarVersion != 1 || got.RuleVersion != 3 || got.RuleContentDigest != "sha256:rule" || *got.BusinessDayAdjustment != "NEXT" ||
		*got.PackVersionID != "pv-1" || got.CalculationRef != "dec-1" || got.Trigger.EventID != "evt-k1" || got.Trigger.SourceObjectID != "p-2026-11" {
		t.Fatalf("provenance did not round-trip: %+v", got)
	}
	if got.Trigger.IngestionLatencyMS == nil || *got.Trigger.IngestionLatencyMS < 3_500_000 {
		t.Fatalf("NP-12: ingestion latency not recorded: %v", got.Trigger.IngestionLatencyMS)
	}
	if got.StoredState != domain.ClockStateActive || got.RowVersion != 1 {
		t.Fatalf("state %s v%d", got.StoredState, got.RowVersion)
	}
	// Replay with the same key and request: the same clock, not a second one.
	again, created, err := e.s.StartClock(e.ctx, domain.StartClockParams{ObligationID: e.obl.ObligationID, ClockKey: "k1", RequestHash: "h-k1",
		ClockClass: domain.ClockClassHardLegal, Calc: calcAt(due, 1, "cv-1"), Actor: "maker-1"})
	if err != nil || created || again.ClockID != c.ClockID {
		t.Fatalf("replay: created=%v id=%v err=%v", created, again, err)
	}
	_, _, err = e.s.StartClock(e.ctx, domain.StartClockParams{ObligationID: e.obl.ObligationID, ClockKey: "k1", RequestHash: "different",
		ClockClass: domain.ClockClassHardLegal, Calc: calcAt(due, 1, "cv-1"), Actor: "maker-1"})
	if !errors.Is(err, domain.ErrClockKeyConflict) {
		t.Fatalf("same key, different request: want ErrClockKeyConflict, got %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM obligation_clocks`); n != 1 {
		t.Fatalf("%d clocks after a replay and a conflict", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = 'obligation.clock.started'`); n != 1 {
		t.Fatalf("%d started events", n)
	}
	// A refused calculation never makes a clock.
	_, _, err = e.s.StartClock(e.ctx, domain.StartClockParams{ObligationID: e.obl.ObligationID, ClockKey: "k2", RequestHash: "h",
		ClockClass: domain.ClockClassHardLegal, Calc: domain.Calculation{Outcome: "AMBIGUOUS"}, Actor: "maker-1"})
	if !errors.Is(err, domain.ErrCalculationRefused) {
		t.Fatalf("AMBIGUOUS outcome: %v", err)
	}
}

// R-INV-17: the database refuses an in-place change of due_at; clocks and
// their history are never deleted.
func TestWFC05_DueAtAndProvenanceAreImmutable_AndNothingIsDeleted(t *testing.T) {
	e := newClockEnv(t)
	c := e.start(t, "k1", time.Now().UTC().AddDate(0, 1, 0), domain.ClockClassHardLegal)
	for name, q := range map[string]string{
		"due_at":              `UPDATE obligation_clocks SET due_at = due_at + INTERVAL '1 day' WHERE clock_id = $1`,
		"calendar_version_id": `UPDATE obligation_clocks SET calendar_version_id = 'cv-9' WHERE clock_id = $1`,
		"clock_class":         `UPDATE obligation_clocks SET clock_class = 'ADVISORY' WHERE clock_id = $1`,
		"trigger":             `UPDATE obligation_clocks SET trigger_event_time = NOW() WHERE clock_id = $1`,
		"delete clock":        `DELETE FROM obligation_clocks WHERE clock_id = $1`,
		"edit transition":     `UPDATE obligation_clock_transitions SET actor = 'x' WHERE clock_id = $1`,
		"delete transition":   `DELETE FROM obligation_clock_transitions WHERE clock_id = $1`,
	} {
		if _, err := e.admin.Exec(context.Background(), q, c.ClockID); err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
}

// Tenant isolation as the runtime role: another tenant reads nothing.
func TestWFC05_RLS_AnotherTenantCannotSeeTheClock(t *testing.T) {
	e := newClockEnv(t)
	c := e.start(t, "k1", time.Now().UTC().AddDate(0, 1, 0), domain.ClockClassHardLegal)
	other := tenantCtx(otherTenantID)
	if _, err := e.s.GetClock(other, c.ClockID); !errors.Is(err, domain.ErrClockNotFound) {
		t.Fatalf("another tenant read the clock: %v", err)
	}
	if l, err := e.s.ListClocks(other, domain.ListClocksFilter{ObligationID: e.obl.ObligationID}); err != nil || len(l) != 0 {
		t.Fatalf("another tenant listed %d clocks (%v)", len(l), err)
	}
	if _, err := e.s.ApplyClockCommand(other, domain.ClockCommandParams{ClockID: c.ClockID, Command: domain.ClockCommandSatisfy,
		Actor: "x", EvidenceRefs: []string{"DOC:1"}}); !errors.Is(err, domain.ErrClockNotFound) {
		t.Fatalf("another tenant moved the clock: %v", err)
	}
}

// OB-S7 / NP-16 / NP-37: a read is OVERDUE with no sweep; the sweep persists
// the move once, even when it runs twice.
func TestWFC05_FrozenClockOverdueOnRead_SweepPersistsOnce(t *testing.T) {
	e := newClockEnv(t)
	c := e.start(t, "k1", time.Now().UTC().Add(-2*time.Hour), domain.ClockClassHardLegal)
	got, err := e.s.GetClock(e.ctx, c.ClockID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StoredState != domain.ClockStateActive || got.ClockState != domain.ClockStateOverdue {
		t.Fatalf("frozen clock: stored %s, read %s; want ACTIVE stored and OVERDUE read", got.StoredState, got.ClockState)
	}
	now := time.Now().UTC()
	items, err := e.s.ClocksNeedingSweep(context.Background(), now, 100)
	if err != nil || len(items) != 1 || items[0].ID != c.ClockID || items[0].TenantID != testTenantID {
		t.Fatalf("sweep (cross-tenant, as the runtime role) listed %+v (%v)", items, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := e.s.SweepClock(context.Background(), items[0], now); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if n := e.count(t, `SELECT COUNT(*) FROM obligation_clock_transitions WHERE clock_id = $1 AND to_state = 'OVERDUE'`, c.ClockID); n != 1 {
		t.Fatalf("NP-37: %d OVERDUE transitions after two sweeps", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = 'obligation.clock.overdue'`); n != 1 {
		t.Fatalf("NP-37: %d overdue events after two sweeps", n)
	}
	var effective time.Time
	if err := e.admin.QueryRow(context.Background(), `SELECT effective_at FROM obligation_clock_transitions WHERE clock_id = $1 AND to_state = 'OVERDUE'`, c.ClockID).Scan(&effective); err != nil {
		t.Fatal(err)
	}
	if !effective.Equal(c.DueAt) {
		t.Fatalf("R-INV-26: OVERDUE recorded at %s, want due_at %s (a late sweep must not move it)", effective, c.DueAt)
	}
	if again, _ := e.s.ClocksNeedingSweep(context.Background(), now, 100); len(again) != 0 {
		t.Fatalf("clock still listed for sweeping: %+v", again)
	}
}

// NP-38: fulfilling a late clock records both the overdue period and the
// fulfilment.
func TestWFC05_SatisfyAfterOverdue_KeepsTheOverduePeriod(t *testing.T) {
	e := newClockEnv(t)
	c := e.start(t, "k1", time.Now().UTC().Add(-3*time.Hour), domain.ClockClassRegulatory)
	done, err := e.s.ApplyClockCommand(e.ctx, domain.ClockCommandParams{ClockID: c.ClockID, Command: domain.ClockCommandSatisfy,
		ExpectedVersion: c.RowVersion, Actor: "maker-1", EvidenceRefs: []string{"SUPPORTING_DOCUMENT:doc-1"}, Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if done.StoredState != domain.ClockStateFulfilled || done.OverdueSince == nil || !done.OverdueSince.Equal(c.DueAt) {
		t.Fatalf("NP-38: state %s overdue_since %v (due %s)", done.StoredState, done.OverdueSince, c.DueAt)
	}
	trs, err := e.s.ListClockTransitions(e.ctx, c.ClockID)
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, tr := range trs {
		seq = append(seq, tr.ToState)
	}
	if strings.Join(seq, ",") != "ACTIVE,OVERDUE,FULFILLED" {
		t.Fatalf("history %v, want ACTIVE,OVERDUE,FULFILLED", seq)
	}
	// Terminal: no command moves it again, and the DB refuses a direct edit.
	if _, err := e.s.ApplyClockCommand(e.ctx, domain.ClockCommandParams{ClockID: c.ClockID, Command: domain.ClockCommandSatisfy,
		Actor: "maker-1", EvidenceRefs: []string{"SUPPORTING_DOCUMENT:doc-2"}}); !errors.Is(err, domain.ErrClockCommandNotAllowed) {
		t.Fatalf("second satisfy: %v", err)
	}
	if _, err := e.admin.Exec(context.Background(), `UPDATE obligation_clocks SET clock_state = 'ACTIVE' WHERE clock_id = $1`, c.ClockID); err == nil {
		t.Fatal("the database reopened a FULFILLED clock")
	}
	if _, err := e.admin.Exec(context.Background(), `UPDATE obligation_clocks SET overdue_since = NULL WHERE clock_id = $1`, c.ClockID); err == nil {
		t.Fatal("NP-38: the database cleared overdue_since")
	}
}

// R-INV-14 in the database: FULFILLED needs evidence or an exemption.
func TestWFC05_FulfilledWithoutEvidenceIsRefusedByTheDatabase(t *testing.T) {
	e := newClockEnv(t)
	c := e.start(t, "k1", time.Now().UTC().AddDate(0, 1, 0), domain.ClockClassHardLegal)
	if _, err := e.s.ApplyClockCommand(e.ctx, domain.ClockCommandParams{ClockID: c.ClockID, Command: domain.ClockCommandSatisfy,
		Actor: "maker-1"}); err == nil {
		t.Fatal("FULFILLED with no evidence and no exemption was stored")
	}
	exempt := "STATUTE_REQUIRES_NONE"
	ref := "wf-1"
	if _, err := e.s.ApplyClockCommand(e.ctx, domain.ClockCommandParams{ClockID: c.ClockID, Command: domain.ClockCommandSatisfy,
		Actor: "maker-1", EvidenceExemptReason: &exempt, ApprovalReference: &ref}); err != nil {
		t.Fatalf("an approved exemption was refused: %v", err)
	}
}

// s7.1 in the database: a waiver names a requester, a different approver, a
// reason and the approval; N/A also links an applicability decision.
func TestWFC05_WaiverNeedsADistinctApprover_AndNALinksADecision(t *testing.T) {
	e := newClockEnv(t)
	c := e.start(t, "k1", time.Now().UTC().AddDate(0, 1, 0), domain.ClockClassHardLegal)
	reason, ref, self, other := "COURT_ORDER", "wf-1", "maker-1", "checker-2"
	if _, err := e.s.ApplyClockCommand(e.ctx, domain.ClockCommandParams{ClockID: c.ClockID, Command: domain.ClockCommandWaive,
		Actor: self, ReasonCode: &reason, ApprovalReference: &ref, ApprovedBy: &self, EvidenceRefs: []string{"DOC:1"}}); err == nil {
		t.Fatal("a self-approved waiver was stored")
	}
	if _, err := e.s.ApplyClockCommand(e.ctx, domain.ClockCommandParams{ClockID: c.ClockID, Command: domain.ClockCommandNotApplicable,
		Actor: self, ReasonCode: &reason, ApprovalReference: &ref, ApprovedBy: &other, EvidenceRefs: []string{"DOC:1"}}); err == nil {
		t.Fatal("NOT_APPLICABLE without an applicability decision was stored")
	}
	w, err := e.s.ApplyClockCommand(e.ctx, domain.ClockCommandParams{ClockID: c.ClockID, Command: domain.ClockCommandWaive,
		Actor: self, ReasonCode: &reason, ApprovalReference: &ref, ApprovedBy: &other, EvidenceRefs: []string{"DOC:1"}})
	if err != nil {
		t.Fatalf("a waiver approved by another principal was refused: %v", err)
	}
	if w.StoredState != domain.ClockStateWaived || *w.RequestedBy != self || *w.ApprovedBy != other || *w.AuthorityAction != "WAIVE" {
		t.Fatalf("waiver record: %+v", w)
	}
}

// R-INV-17 / NP-14 / NP-32: a correction supersedes; the old clock keeps its
// due_at and both are linked.
func TestWFC05_Supersede_KeepsTheOldClockAndLinksBothWays(t *testing.T) {
	e := newClockEnv(t)
	due := time.Now().UTC().AddDate(0, 1, 0).Truncate(time.Second)
	c := e.start(t, "k1", due, domain.ClockClassHardLegal)
	reason := "CALENDAR_CORRECTED"
	newDue := due.Add(24 * time.Hour)
	old, next, superseded, err := e.s.SupersedeClock(e.ctx, c.ClockID, c.RowVersion, domain.StartClockParams{ClockKey: "recalc-1", RequestHash: "r",
		Calc: calcAt(newDue, 2, "cv-2"), SupersessionReason: &reason, Actor: "maker-1", CorrelationID: "corr"})
	if err != nil || !superseded {
		t.Fatalf("supersede: %v %v", superseded, err)
	}
	if old.StoredState != domain.ClockStateSuperseded || !old.DueAt.Equal(due) || *old.SupersededByClockID != next.ClockID {
		t.Fatalf("old clock: %+v", old)
	}
	if *next.SupersedesClockID != c.ClockID || !next.DueAt.Equal(newDue) || *next.CalendarVersionID != "cv-2" || next.Trigger.EventID != c.Trigger.EventID {
		t.Fatalf("successor: %+v", next)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = 'obligation.clock.superseded'`); n != 1 {
		t.Fatalf("%d superseded events", n)
	}
	// Same answer again: nothing to supersede.
	_, _, superseded, err = e.s.SupersedeClock(e.ctx, next.ClockID, next.RowVersion, domain.StartClockParams{ClockKey: "recalc-2", RequestHash: "r2",
		Calc: calcAt(newDue, 2, "cv-2"), SupersessionReason: &reason, Actor: "maker-1"})
	if err != nil || superseded {
		t.Fatalf("an unchanged recalculation superseded: %v %v", superseded, err)
	}
	// The calendar consumer finds clocks on older versions across tenants.
	items, err := e.s.ClocksOnOlderCalendar(context.Background(), "gb-hmrc", 2, 100)
	if err != nil || len(items) != 0 {
		t.Fatalf("clocks on gb-hmrc < 2 after the correction: %+v %v", items, err)
	}
	items, _ = e.s.ClocksOnOlderCalendar(context.Background(), "gb-hmrc", 3, 100)
	if len(items) != 1 || items[0].ID != next.ClockID {
		t.Fatalf("clocks on gb-hmrc < 3: %+v", items)
	}
}

// s7.4 / R-INV-19: a milestone fires once and escalation never changes the
// clock's state or due_at. OPEN_CASE queues a workflow case in the outbox.
func TestWFC05_MilestoneFiresOnce_EscalationNeverChangesTheClock(t *testing.T) {
	e := newClockEnv(t)
	due := time.Now().UTC().Add(-time.Hour)
	c := e.start(t, "k1", due, domain.ClockClassHardLegal,
		domain.MilestoneSpec{Kind: domain.MilestoneEscalation, OffsetSeconds: 60, Action: domain.ActionOpenCase},
		domain.MilestoneSpec{Kind: domain.MilestoneEscalation, OffsetSeconds: 120, Action: domain.ActionRaisePriority, Params: map[string]any{"priority": "CRITICAL"}},
		domain.MilestoneSpec{Kind: domain.MilestoneEscalation, OffsetSeconds: 180, Action: domain.ActionNotify})
	now := time.Now().UTC()
	due1, err := e.s.DueMilestones(context.Background(), now, 100)
	if err != nil || len(due1) != 3 {
		t.Fatalf("due milestones: %d (%v)", len(due1), err)
	}
	for _, it := range due1 {
		for i := 0; i < 2; i++ {
			if _, err := e.s.FireMilestone(context.Background(), it, now); err != nil {
				t.Fatalf("fire: %v", err)
			}
		}
	}
	if n := e.count(t, `SELECT COUNT(*) FROM event_outbox WHERE destination = 'WORKFLOW_CASE'`); n != 1 {
		t.Fatalf("%d case requests queued (want 1 after two fires)", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = 'obligation.escalated'`); n != 3 {
		t.Fatalf("%d escalated events", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM escalation_events WHERE clock_id = $1`, c.ClockID); n != 3 {
		t.Fatalf("%d escalation records", n)
	}
	got, _ := e.s.GetClock(e.ctx, c.ClockID)
	if !got.DueAt.Equal(c.DueAt) || got.Priority != "CRITICAL" || got.ClockState != domain.ClockStateOverdue {
		t.Fatalf("after escalation: due %s priority %s state %s", got.DueAt, got.Priority, got.ClockState)
	}
	if got.StoredState.Terminal() {
		t.Fatalf("escalation fulfilled the clock: %s", got.StoredState)
	}
	var payload []byte
	if err := e.admin.QueryRow(context.Background(), `SELECT event_payload FROM event_outbox WHERE destination = 'WORKFLOW_CASE'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var req store.CaseRequest
	_ = json.Unmarshal(payload, &req)
	if req.SourceType != "OBLIGATION_CLOCK" || req.SourceID != c.ClockID || req.Confidentiality != "RESTRICTED" || req.IdempotencyKey == "" {
		t.Fatalf("case request: %+v", req)
	}
	// The Kafka relay never picks up a WORKFLOW_CASE row.
	var kinds []string
	_, _, _ = e.s.RelayOutbox(context.Background(), 100, func(_ context.Context, ev store.OutboxEvent) error {
		kinds = append(kinds, ev.EventType)
		return nil
	})
	for _, k := range kinds {
		if k == "workflow.case.open_requested" {
			t.Fatal("the Kafka relay published a case request")
		}
	}
	if err := e.s.RecordCaseOpened(context.Background(), req, "case-77"); err != nil {
		t.Fatal(err)
	}
	evs, _ := e.s.ListEscalationEvents(e.ctx, c.ClockID)
	found := false
	for _, ev := range evs {
		if ev.Outcome == "COMPLETED" && ev.ResultRef != nil && *ev.ResultRef == "case-77" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the opened case was not recorded: %+v", evs)
	}
}

// Escalation policies are immutable versions.
func TestWFC05_EscalationPolicyVersionsAreImmutable(t *testing.T) {
	e := newClockEnv(t)
	m := []domain.MilestoneSpec{{Kind: domain.MilestoneEscalation, OffsetSeconds: 0, Action: domain.ActionNotify}}
	p1, err := e.s.CreateEscalationPolicy(e.ctx, "VAT_LATE", m, "admin-1", "c")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := e.s.CreateEscalationPolicy(e.ctx, "VAT_LATE", append(m, domain.MilestoneSpec{Kind: domain.MilestoneEscalation, OffsetSeconds: 3600, Action: domain.ActionOpenCase}), "admin-1", "c")
	if err != nil || p1.Version != 1 || p2.Version != 2 {
		t.Fatalf("versions %d, %d (%v)", p1.Version, p2.Version, err)
	}
	if _, err := e.admin.Exec(context.Background(), `UPDATE escalation_policies SET milestones = '[]' WHERE escalation_policy_id = $1`, p1.EscalationPolicyID); err == nil {
		t.Fatal("a policy version was edited in place")
	}
	r, err := e.s.RetireEscalationPolicy(e.ctx, p1.EscalationPolicyID, p1.RowVersion, "admin-1")
	if err != nil || r.Status != "RETIRED" {
		t.Fatalf("retire: %+v %v", r, err)
	}
	// A clock cannot start on a retired policy.
	due := time.Now().UTC().AddDate(0, 1, 0)
	_, _, err = e.s.StartClock(e.ctx, domain.StartClockParams{ObligationID: e.obl.ObligationID, ClockKey: "kp", RequestHash: "h",
		ClockClass: domain.ClockClassHardLegal, Calc: calcAt(due, 1, "cv-1"), EscalationPolicyID: &p1.EscalationPolicyID, Actor: "maker-1"})
	if !errors.Is(err, domain.ErrEscalationPolicyNotFound) {
		t.Fatalf("start on a retired policy: %v", err)
	}
	c, _, err := e.s.StartClock(e.ctx, domain.StartClockParams{ObligationID: e.obl.ObligationID, ClockKey: "kp2", RequestHash: "h",
		ClockClass: domain.ClockClassHardLegal, Calc: calcAt(due, 1, "cv-1"), EscalationPolicyID: &p2.EscalationPolicyID, Actor: "maker-1"})
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := e.s.ListClockMilestones(e.ctx, c.ClockID)
	if len(ms) != 2 || ms[0].EscalationPolicyVersion == nil || *ms[0].EscalationPolicyVersion != 2 {
		t.Fatalf("milestones from policy v2: %+v", ms)
	}
}

// Migration 000016 up/down/up over rows in the 19 Nov 000014 shape.
func TestMigration000016_UpDownUp_KeepsOld000014Rows(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	ctx := context.Background()
	if _, err := admin.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); err != nil {
		t.Fatal(err)
	}
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")
	files, _ := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	apply := func(path string) {
		t.Helper()
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
	}
	for _, f := range files {
		if strings.Contains(filepath.Base(f), "000016_") {
			break
		}
		apply(f)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO obligations (obligation_id, tenant_id, legal_entity_id, jurisdiction_id, obligation_source_type,
		obligation_source_id, obligation_code, obligation_type, due_date, severity_level, responsible_function, source_reference, created_by_principal_id)
		VALUES ('11111111-0000-0000-0000-000000000001', $1, '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002',
		'RULE', 'r', 'OBL-1', 'FILING', '2026-12-31', 'HIGH', 'Tax', 'ref', 'a')`, testTenantID); err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"SOFT_INTERNAL", "EXTENSION", "HARD_LEGAL"} {
		if _, err := admin.Exec(ctx, `INSERT INTO obligation_clocks (obligation_id, tenant_id, legal_entity_id, clock_class, clock_state,
			calendar_code, calendar_rule_id, calendar_rule_version, computed_deadline)
			VALUES ('11111111-0000-0000-0000-000000000001', $1, '00000000-0000-0000-0000-000000000001', $2, 'ACTIVE',
			'gb', '6f1e8a52-1d7b-4d7a-9a3c-2b0c6c9e0a11', 1, '2026-12-31T23:59:59Z')`, testTenantID, class); err != nil {
			t.Fatal(err)
		}
	}
	up := filepath.Join(dir, "000016_deadline_clocks_wfc05.up.sql")
	down := filepath.Join(dir, "000016_deadline_clocks_wfc05.down.sql")
	apply(up)
	classes := func() string {
		rows, err := admin.Query(ctx, `SELECT clock_class FROM obligation_clocks ORDER BY clock_class`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var c string
			_ = rows.Scan(&c)
			out = append(out, c)
		}
		return strings.Join(out, ",")
	}
	if got := classes(); got != "HARD_LEGAL,HARD_LEGAL,INTERNAL_SLA" {
		t.Fatalf("after up: %s", got)
	}
	apply(down)
	if got := classes(); got != "EXTENSION,HARD_LEGAL,SOFT_INTERNAL" {
		t.Fatalf("after down: %s", got)
	}
	apply(up)
	if got := classes(); got != "HARD_LEGAL,HARD_LEGAL,INTERNAL_SLA" {
		t.Fatalf("after up again: %s", got)
	}
	var keys int
	if err := admin.QueryRow(ctx, `SELECT COUNT(DISTINCT clock_key) FROM obligation_clocks`).Scan(&keys); err != nil || keys != 3 {
		t.Fatalf("clock keys after up: %d (%v)", keys, err)
	}
}

// NP-37: ten concurrent sweeps of one clock persist one move and one event.
// The guard is layered (row lock, state-and-version predicate, a unique
// (clock_id, row_version) index on the transitions); any one of them holds it.
func TestWFC05_ConcurrentSweepsPersistOneMove(t *testing.T) {
	e := newClockEnv(t)
	c := e.start(t, "k1", time.Now().UTC().Add(-time.Hour), domain.ClockClassHardLegal)
	now := time.Now().UTC()
	item := store.WorkItem{TenantID: testTenantID, ID: c.ClockID}
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_, err := e.s.SweepClock(context.Background(), item, now)
			errs <- err
		}()
	}
	for i := 0; i < 10; i++ {
		if err := <-errs; err != nil {
			t.Logf("sweep error (allowed: a loser may be refused): %v", err)
		}
	}
	if n := e.count(t, `SELECT COUNT(*) FROM obligation_clock_transitions WHERE clock_id = $1 AND to_state = 'OVERDUE'`, c.ClockID); n != 1 {
		t.Fatalf("NP-37: %d OVERDUE transitions after 10 concurrent sweeps", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = 'obligation.clock.overdue'`); n != 1 {
		t.Fatalf("NP-37: %d overdue events after 10 concurrent sweeps", n)
	}
}

// OB-S14 / R-INV-18: across both 2026 Europe/London transitions the stored
// due_at and time zone are exactly jurisdiction-rules' answer (JR-DST-1:
// 01:30 on 29 Mar does not exist and resolves to 01:00 UTC; 01:30 on 25 Oct
// occurs twice and resolves to the earlier, 00:30 UTC), and the due day
// starts at local midnight.
func TestWFC05_DST_StoredInstantAndZoneAreJurisdictionsAnswer(t *testing.T) {
	e := newClockEnv(t)
	london, _ := time.LoadLocation("Europe/London")
	for i, c := range []struct {
		local    string
		due      time.Time
		dayStart time.Time
	}{
		{"2026-03-29", time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC), time.Date(2026, 3, 29, 0, 0, 0, 0, london)},
		{"2026-10-25", time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), time.Date(2026, 10, 25, 0, 0, 0, 0, london)},
	} {
		calc := calcAt(c.due, 1, "cv-1")
		calc.Timezone, calc.DueDate = "Europe/London", c.local
		clk, _, err := e.s.StartClock(e.ctx, domain.StartClockParams{ObligationID: e.obl.ObligationID, ClockKey: fmt.Sprintf("dst-%d", i),
			RequestHash: "h", ClockClass: domain.ClockClassHardLegal, Calc: calc, Actor: "maker-1"})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := e.s.GetClock(e.ctx, clk.ClockID)
		if !got.DueAt.Equal(c.due) || got.Timezone != "Europe/London" || got.LocalDueDate != c.local {
			t.Fatalf("%s: stored %s %s %s", c.local, got.DueAt, got.Timezone, got.LocalDueDate)
		}
		if !got.DueDayStart().Equal(c.dayStart) {
			t.Fatalf("%s: due day starts %s, want %s", c.local, got.DueDayStart(), c.dayStart)
		}
	}
}
