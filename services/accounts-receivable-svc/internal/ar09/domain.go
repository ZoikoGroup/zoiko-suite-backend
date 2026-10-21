// Package ar09 implements AR-09 Collections & Dunning (spec ZS-SVC-C-001 section 12): collection cases, dunning stages,
// notices, promises to pay, case holds and escalation. Collections owns communication and follow-up, NEVER a balance
// (invariant 10): this package has no write path into AR-07 or the posting queue. It reads open items through the narrow
// read-only Receivables interface; overdue is derived from AR-07 facts and a case resolves only when AR-07 shows the items
// settled / written off / credited. Section 18 #27 (dunning during dispute/hold), #28 (promise history overwritten) and #29
// (collector self-approves write-off: there is no approval or write-off authority here at all).
package ar09

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Authorization actions. Reads are tenant-scoped only (settled AR read posture).
const (
	ActionCollectionsManage   = "AR09_COLLECTIONS_MANAGE"   // collections.manage: OpenCollectionCase, RecordPromiseToPay, CloseCollectionCase
	ActionDunningSend         = "AR09_DUNNING_SEND"         // dunning.send: AdvanceDunningStage, SendDunningNotice
	ActionCollectionsEscalate = "AR09_COLLECTIONS_ESCALATE" // collections.escalate: EscalateCollectionCase (records a referral reason only)
	ActionHoldManage          = "AR09_HOLD_MANAGE"          // hold manage: PlaceCollectionHold, ResumeCollections
	ActionPolicyManage        = "AR09_POLICY_MANAGE"        // PublishDunningPolicy (policy data)
)

// Actions lists every AR-09 authorization action.
var Actions = []string{ActionCollectionsManage, ActionDunningSend, ActionCollectionsEscalate, ActionHoldManage, ActionPolicyManage}

// SystemPrincipal is the actor recorded for scheduler (automated) dunning.
const SystemPrincipal = "system:ar09-scheduler"

// Events (kit.Emit EventType).
const (
	AggregateCase           = "AR_COLLECTION_CASE"
	EvCaseOpened            = "ar.collection.case_opened"
	EvNoticeSent            = "ar.collection.dunning_notice_sent"
	EvPromiseRecorded       = "ar.collection.promise_to_pay_recorded"
	EvHoldPlaced            = "ar.collection.hold_placed"
	EvEscalated             = "ar.collection.escalated"
	EvCaseResolved          = "ar.collection.case_resolved"
	StateEligible           = "ELIGIBLE"
	StateActive             = "ACTIVE"
	StatePromiseToPay       = "PROMISE_TO_PAY"
	StateHeld               = "HELD"
	StateDisputed           = "DISPUTED"
	StateEscalated          = "ESCALATED"
	StateResolved           = "RESOLVED"
	StateClosed             = "CLOSED"
	ResultSent              = "SENT"
	ResultDelivered         = "DELIVERED"
	ResultFailed            = "DELIVERY_FAILED"
	ResultSuppressed        = "SUPPRESSED"
	OutcomeResolved         = "RESOLVED"  // CloseCollectionCase outcome: AR-07 shows every item settled / written off / credited
	OutcomeAbandoned        = "ABANDONED" // CloseCollectionCase outcome: administrative close with a reason
	SuppCaseHold            = "CASE_HOLD"
	SuppProfileInactive     = "PROFILE_INACTIVE"
	SuppItemDisputeOrHold   = "ITEM_DISPUTE_OR_HOLD"
	SuppCollectionBlocked   = "COLLECTION_BLOCKED"
	SuppPromiseGrace        = "PROMISE_GRACE"
	SuppContactBlackout     = "CONTACT_BLACKOUT"
	SuppRateLimited         = "RATE_LIMITED"
	defaultAgingBucketCount = 5
)

var terminal = map[string]bool{StateResolved: true, StateClosed: true}

// IsTerminal reports a resolved or closed case.
func IsTerminal(state string) bool { return terminal[state] }

// transitions is the case state machine. Overdue is NOT a state: it is derived from AR-07 facts.
var transitions = map[string][]string{
	StateEligible:     {StateActive, StatePromiseToPay, StateHeld, StateDisputed, StateResolved, StateClosed},
	StateActive:       {StatePromiseToPay, StateHeld, StateDisputed, StateEscalated, StateResolved, StateClosed},
	StatePromiseToPay: {StateActive, StateHeld, StateDisputed, StateResolved, StateClosed},
	StateHeld:         {StateEligible, StateActive, StatePromiseToPay, StateDisputed, StateEscalated, StateResolved, StateClosed},
	StateDisputed:     {StateEligible, StateActive, StatePromiseToPay, StateHeld, StateResolved, StateClosed},
	StateEscalated:    {StateHeld, StateResolved, StateClosed},
}

// CanTransition reports whether from -> to is a legal move.
func CanTransition(from, to string) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

func invalidTransition(from, to string) *kit.Problem {
	return kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("a %s collection case cannot move to %s", from, to))
}

// Stage is one dunning step of a policy.
type Stage struct {
	StageNo             int        `json:"stage_no"`
	DaysPastDue         int        `json:"days_past_due"`
	GraceDays           int        `json:"grace_days"`
	Channel             string     `json:"channel"`
	TemplateKey         string     `json:"template_key"`
	EscalationThreshold kit.Amount `json:"escalation_threshold"` // 0 = no threshold; escalation needs the live case residual >= threshold
}

// TriggerDay is the days-past-due at which the stage becomes due (policy days + grace).
func (s Stage) TriggerDay() int { return s.DaysPastDue + s.GraceDays }

// Policy is one immutable published version of a dunning profile: stages plus the contact policy. Contact restrictions are
// data (no jurisdiction assumption is coded).
type Policy struct {
	PolicyID            string    `json:"policy_id"`
	TenantID            string    `json:"tenant_id"`
	LegalEntityID       string    `json:"legal_entity_id"`
	DunningProfile      string    `json:"dunning_profile"`
	Version             int       `json:"version"`
	EffectiveFrom       string    `json:"effective_from"`
	Stages              []Stage   `json:"stages"`
	BlackoutWeekdays    []int     `json:"blackout_weekdays"` // 0 = Sunday .. 6 = Saturday: no contact on these days
	BlackoutDates       []string  `json:"blackout_dates"`    // YYYY-MM-DD: no contact on these dates
	MaxNoticesPerWindow int       `json:"max_notices_per_window"`
	WindowDays          int       `json:"window_days"`
	PromiseGraceDays    int       `json:"promise_grace_days"`
	PublishedBy         string    `json:"published_by"`
	PublishedAt         time.Time `json:"published_at"`
}

var channels = map[string]bool{"EMAIL": true, "LETTER": true, "SMS": true, "PORTAL": true, "PHONE": true}

// ValidatePolicy checks a policy before it is published.
func ValidatePolicy(p *Policy) error {
	if strings.TrimSpace(p.DunningProfile) == "" {
		return kit.Invalid("dunning_profile is required")
	}
	if _, err := parseDate("effective_from", p.EffectiveFrom); err != nil {
		return err
	}
	if len(p.Stages) == 0 {
		return kit.Invalid("a policy needs at least one stage")
	}
	last := -1
	for i, s := range p.Stages {
		if s.StageNo != i+1 {
			return kit.Invalid("stage_no must run 1..n without gaps")
		}
		if s.DaysPastDue < 0 || s.GraceDays < 0 {
			return kit.Invalid("days_past_due and grace_days cannot be negative")
		}
		if s.TriggerDay() <= last {
			return kit.Invalid("each stage must trigger later (days_past_due + grace_days) than the previous one")
		}
		last = s.TriggerDay()
		if !channels[s.Channel] {
			return kit.Invalid("stage channel must be one of EMAIL, LETTER, SMS, PORTAL, PHONE")
		}
		if strings.TrimSpace(s.TemplateKey) == "" {
			return kit.Invalid("stage template_key is required")
		}
		if s.EscalationThreshold.IsNegative() {
			return kit.Invalid("escalation_threshold cannot be negative")
		}
	}
	for _, d := range p.BlackoutWeekdays {
		if d < 0 || d > 6 {
			return kit.Invalid("blackout_weekdays are 0 (Sunday) .. 6 (Saturday)")
		}
	}
	for _, d := range p.BlackoutDates {
		if _, err := parseDate("blackout_dates", d); err != nil {
			return err
		}
	}
	if p.MaxNoticesPerWindow < 0 || p.WindowDays < 0 || p.PromiseGraceDays < 0 {
		return kit.Invalid("rate limit and grace values cannot be negative")
	}
	if (p.MaxNoticesPerWindow == 0) != (p.WindowDays == 0) {
		return kit.Invalid("max_notices_per_window and window_days must be set together (0 = no rate limit)")
	}
	return nil
}

// stage returns the stage with number n.
func (p *Policy) stage(n int) *Stage {
	for i := range p.Stages {
		if p.Stages[i].StageNo == n {
			return &p.Stages[i]
		}
	}
	return nil
}

// nextDueStage is the first stage after current whose trigger day the case has reached. nil = none due.
func (p *Policy) nextDueStage(current, daysPastDue int) *Stage {
	for i := range p.Stages {
		s := &p.Stages[i]
		if s.StageNo > current && daysPastDue >= s.TriggerDay() {
			return s
		}
	}
	return nil
}

// BlackoutReason reports a contact-policy blackout on the calendar day of now (UTC).
func (p *Policy) BlackoutReason(now time.Time) string {
	now = now.UTC()
	for _, d := range p.BlackoutWeekdays {
		if int(now.Weekday()) == d {
			return SuppContactBlackout
		}
	}
	day := now.Format("2006-01-02")
	for _, d := range p.BlackoutDates {
		if d == day {
			return SuppContactBlackout
		}
	}
	return ""
}

// Case is a collection case. Its aging snapshot is evidence captured at open, never a balance.
type Case struct {
	CaseID          string     `json:"case_id"`
	TenantID        string     `json:"tenant_id"`
	LegalEntityID   string     `json:"legal_entity_id"`
	CustomerPartyID string     `json:"customer_party_id"`
	Currency        string     `json:"currency"`
	State           string     `json:"state"`
	StateBefore     string     `json:"state_before_block,omitempty"`
	OwnerPrincipal  string     `json:"owner_principal"`
	DunningProfile  string     `json:"dunning_profile"`
	PolicyID        string     `json:"policy_id"`
	DunningStage    int        `json:"dunning_stage"`
	AgingSnapshot   *Aging     `json:"aging_snapshot"`
	ReferralReason  string     `json:"referral_reason,omitempty"`
	EscalatedBy     string     `json:"escalated_by,omitempty"`
	EscalatedAt     *time.Time `json:"escalated_at,omitempty"`
	Resolution      any        `json:"resolution,omitempty"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	ClosedBy        string     `json:"closed_by,omitempty"`
	CloseReason     string     `json:"close_reason,omitempty"`
	Version         int        `json:"version"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// Promise is one version of a customer's promise to pay (append-only; a change is a new version).
type Promise struct {
	PromiseID      string     `json:"promise_id"`
	CaseID         string     `json:"case_id"`
	VersionNo      int        `json:"version_no"`
	PromisedDate   string     `json:"promised_date"`
	PromisedAmount kit.Amount `json:"promised_amount"`
	Currency       string     `json:"currency"`
	Supersedes     string     `json:"supersedes,omitempty"`
	ReasonCode     string     `json:"reason_code,omitempty"`
	RecordedBy     string     `json:"recorded_by"`
	RecordedAt     time.Time  `json:"recorded_at"`
}

// Hold is a case-level collection hold.
type Hold struct {
	HoldID        string     `json:"hold_id"`
	CaseID        string     `json:"case_id"`
	ReasonCode    string     `json:"reason_code"`
	EvidenceID    string     `json:"evidence_id,omitempty"`
	PlacedBy      string     `json:"placed_by"`
	PlacedAt      time.Time  `json:"placed_at"`
	ReleasedBy    string     `json:"released_by,omitempty"`
	ReleasedAt    *time.Time `json:"released_at,omitempty"`
	ReleaseReason string     `json:"release_reason,omitempty"`
}

// Action is one append-only communication / suppression record.
type Action struct {
	ActionID           string    `json:"action_id"`
	CaseID             string    `json:"case_id"`
	StageNo            int       `json:"stage_no"`
	Channel            string    `json:"channel"`
	TemplateKey        string    `json:"template_key"`
	DeliveryResult     string    `json:"delivery_result"`
	SuppressionReason  string    `json:"suppression_reason,omitempty"`
	FailureDetail      string    `json:"failure_detail,omitempty"`
	ProviderRef        string    `json:"provider_ref,omitempty"`
	NoticeArtifactHash string    `json:"notice_artifact_hash,omitempty"`
	AgingSnapshot      *Aging    `json:"aging_snapshot,omitempty"`
	ActorID            string    `json:"actor_id"`
	Automated          bool      `json:"automated"`
	RecordedAt         time.Time `json:"recorded_at"`
}

// HistoryEvent is one append-only case history row.
type HistoryEvent struct {
	Seq          int64          `json:"seq"`
	Event        string         `json:"event"`
	FromState    string         `json:"from_state,omitempty"`
	ToState      string         `json:"to_state,omitempty"`
	VersionAfter int            `json:"version_after"`
	ActorID      string         `json:"actor_id"`
	ReasonCode   string         `json:"reason_code,omitempty"`
	Detail       map[string]any `json:"detail,omitempty"`
	At           time.Time      `json:"at"`
}

// ---- aging (derived from AR-07 live views) ----

// Aging is a point-in-time evidence snapshot derived from open items. It is a read model: never persisted as a balance and
// never read back as truth.
type Aging struct {
	AsOf     string                `json:"as_of"`
	Currency string                `json:"currency"`
	Total    kit.Amount            `json:"total_residual"`
	Buckets  map[string]kit.Amount `json:"buckets"`
	Items    []AgingItem           `json:"items"`
}

// AgingItem is one open item in a snapshot.
type AgingItem struct {
	OpenItemID  string     `json:"open_item_id"`
	DueDate     string     `json:"due_date"`
	DaysPastDue int        `json:"days_past_due"`
	Residual    kit.Amount `json:"residual"`
	Disputed    bool       `json:"disputed"`
	Held        bool       `json:"collection_hold"`
}

// BucketNames is the bucket order of an aging snapshot.
var BucketNames = []string{"CURRENT", "1-30", "31-60", "61-90", "91+"}

func bucketOf(dpd int) string {
	switch {
	case dpd <= 0:
		return "CURRENT"
	case dpd <= 30:
		return "1-30"
	case dpd <= 60:
		return "31-60"
	case dpd <= 90:
		return "61-90"
	}
	return "91+"
}

// DaysPastDue is whole calendar days (UTC) between the due date and asOf; negative before the due date.
func DaysPastDue(due, asOf time.Time) int {
	d := time.Date(due.Year(), due.Month(), due.Day(), 0, 0, 0, 0, time.UTC)
	a := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, time.UTC)
	return int(a.Sub(d).Hours() / 24)
}

// BuildAging derives the snapshot of the views with a residual.
func BuildAging(views []kit.OpenItemView, currency string, asOf time.Time) *Aging {
	a := &Aging{AsOf: asOf.UTC().Format("2006-01-02"), Currency: currency, Buckets: map[string]kit.Amount{}, Items: []AgingItem{}}
	for _, b := range BucketNames {
		a.Buckets[b] = 0
	}
	sorted := append([]kit.OpenItemView(nil), views...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].OpenItemID < sorted[j].OpenItemID })
	for _, v := range sorted {
		if !v.Residual.IsPositive() {
			continue
		}
		dpd := DaysPastDue(v.DueDate, asOf)
		a.Total = a.Total.Add(v.Residual)
		a.Buckets[bucketOf(dpd)] = a.Buckets[bucketOf(dpd)].Add(v.Residual)
		a.Items = append(a.Items, AgingItem{OpenItemID: v.OpenItemID, DueDate: v.DueDate.Format("2006-01-02"), DaysPastDue: dpd,
			Residual: v.Residual, Disputed: v.Disputed, Held: v.CollectionHold})
	}
	return a
}

// MaxDaysPastDue is the oldest overdue age among items that still carry a residual (0 when none is overdue).
func MaxDaysPastDue(a *Aging) int {
	m := 0
	for _, i := range a.Items {
		if i.DaysPastDue > m {
			m = i.DaysPastDue
		}
	}
	return m
}

// ItemsSettled reports that no listed item carries a residual (the case's resolution condition, read live).
func ItemsSettled(views []kit.OpenItemView) bool {
	for _, v := range views {
		if v.Residual.IsPositive() {
			return false
		}
	}
	return true
}

// ---- notice artifact ----

// RenderNotice builds the deterministic notice text and its SHA-256 artifact hash. The body states what is owed as read from
// AR-07 at render time; it is communication, not a ledger.
func RenderNotice(c *Case, st *Stage, aging *Aging) (body, hash string) {
	var b strings.Builder
	fmt.Fprintf(&b, "template=%s\nchannel=%s\ncase=%s\ncustomer=%s\nstage=%d\nas_of=%s\ncurrency=%s\n",
		st.TemplateKey, st.Channel, c.CaseID, c.CustomerPartyID, st.StageNo, aging.AsOf, aging.Currency)
	for _, i := range aging.Items {
		if i.DaysPastDue > 0 {
			fmt.Fprintf(&b, "item=%s due=%s days_past_due=%d outstanding=%s\n", i.OpenItemID, i.DueDate, i.DaysPastDue, i.Residual)
		}
	}
	fmt.Fprintf(&b, "total_outstanding=%s\n", aging.Total)
	body = b.String()
	sum := sha256.Sum256([]byte(body))
	return body, hex.EncodeToString(sum[:])
}

// ---- helpers ----

func parseDate(field, s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, kit.Invalid(field + " must be a date (YYYY-MM-DD)")
	}
	return t, nil
}

func notFound() *kit.Problem { return kit.NotFound("collection case not found") }

func suppressed(reason string) *kit.Problem {
	return kit.Conflict(kit.CodeCollectionHoldActive, "collection contact is suppressed: "+reason)
}

func startOfDayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// AvailableForState lists the commands the state machine allows from a state, before permissions.
func AvailableForState(state string) []string {
	switch state {
	case StateEligible:
		return []string{"AdvanceDunningStage", "RecordPromiseToPay", "PlaceCollectionHold", "CloseCollectionCase"}
	case StateActive:
		return []string{"AdvanceDunningStage", "SendDunningNotice", "RecordPromiseToPay", "PlaceCollectionHold", "EscalateCollectionCase", "CloseCollectionCase"}
	case StatePromiseToPay:
		return []string{"AdvanceDunningStage", "SendDunningNotice", "RecordPromiseToPay", "PlaceCollectionHold", "CloseCollectionCase"}
	case StateHeld:
		return []string{"ResumeCollections", "CloseCollectionCase"}
	case StateDisputed:
		return []string{"PlaceCollectionHold", "CloseCollectionCase"}
	case StateEscalated:
		return []string{"PlaceCollectionHold", "CloseCollectionCase"}
	}
	return []string{}
}

// ActionFor maps a command to its authorization action.
var ActionFor = map[string]string{
	"OpenCollectionCase":     ActionCollectionsManage,
	"RecordPromiseToPay":     ActionCollectionsManage,
	"CloseCollectionCase":    ActionCollectionsManage,
	"AdvanceDunningStage":    ActionDunningSend,
	"SendDunningNotice":      ActionDunningSend,
	"EscalateCollectionCase": ActionCollectionsEscalate,
	"PlaceCollectionHold":    ActionHoldManage,
	"ResumeCollections":      ActionHoldManage,
}
