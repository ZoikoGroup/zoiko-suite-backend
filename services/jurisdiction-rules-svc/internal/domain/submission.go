package domain

import (
	"fmt"
	"strings"
	"time"
)

// ZS-JUR-001 Wave 5: electronic invoicing and digital reporting profiles (s13),
// statutory filing and authority submission (s14), as two more closed
// parameter families on the governed rule module.
//
// A profile says WHAT a jurisdiction or network requires: identity, syntax,
// transmission mode, timing, corrections, the versioned schemas and code lists it
// depends on (with validity windows), and the submission LIFECYCLE (which states
// exist, which transitions are legal, which states need an authority receipt).
// It carries no endpoint, credential or certificate: those are provider and secret
// store concerns (s39: no filing credentials inside pack artifacts).

const (
	FamilyEInvoiceProfile = "EINVOICE_PROFILE"
	FamilyFilingProfile   = "FILING_PROFILE"

	EInvoiceDomain = "EINVOICE_PROFILE"
	FilingDomain   = "STATUTORY_FILING"

	OutcomeProfileResolved = "PROFILE_RESOLVED"
	DependencyUnknown      = "DEPENDENCY_UNKNOWN" // JUR-NEG-08, JUR-NEG-27
	DependencyExpired      = "DEPENDENCY_EXPIRED" // JUR-NEG-08
	ApprovalMissing        = "APPROVAL_MISSING"

	// Submission event dispositions (JUR-NEG-09, 10, 11).
	DispApplied           = "APPLIED"
	DispDuplicate         = "DUPLICATE"
	DispStale             = "STALE"
	DispAfterTerminal     = "AFTER_TERMINAL"
	DispIllegalTransition = "ILLEGAL_TRANSITION"
	DispMissingReceipt    = "MISSING_RECEIPT"
	DispUnknownStatus     = "UNKNOWN_STATUS"

	maxLifecycleStates = 20
)

var (
	profileSyntaxes     = []string{"UBL_XML", "JSON", "AUTHORITY_SPECIFIC"}
	einvoiceModes       = []string{"EXCHANGE", "CLEARANCE", "REPORTING_REALTIME", "REPORTING_POST_ISSUANCE"}
	filingModes         = []string{"FILING_DIRECT", "FILING_VIA_PROVIDER"}
	einvoiceCorrections = []string{"CREDIT_NOTE", "CANCEL", "AMEND"}
	filingCorrections   = []string{"AMENDED_FILING", "CANCEL"}
	einvoiceDocTypes    = []string{"INVOICE", "CREDIT_NOTE", "DEBIT_NOTE"}
	dependencyKinds     = []string{"SCHEMA", "CODE_LIST", "BUSINESS_RULES"}
)

// Dependency is a versioned external schema, code list or business-rule package
// with the dates it is valid for (s13, s14, s33).
type Dependency struct {
	Kind      string  `json:"kind"`
	Ref       string  `json:"ref"`
	Version   string  `json:"version"`
	ValidFrom string  `json:"valid_from"`
	ValidTo   *string `json:"valid_to"`
}

// Timing is the profile's timing block. Zero means "no window".
type Timing struct {
	IssueDeadlineHours   *int `json:"issue_deadline_hours,omitempty"`
	RetryWindowHours     int  `json:"retry_window_hours"`
	CorrectionWindowDays int  `json:"correction_window_days"`
}

// Lifecycle is the submission state machine. Transitions are explicit pairs;
// nothing not listed is legal. A state in ReceiptRequired can only be entered with
// an authority or provider receipt id, so a false "accepted" cannot be recorded.
type Lifecycle struct {
	Initial         string     `json:"initial"`
	Terminal        []string   `json:"terminal"`
	Transitions     [][]string `json:"transitions"`
	ReceiptRequired []string   `json:"receipt_required"`
}

// SubmissionProfile is the payload of an EINVOICE_PROFILE or FILING_PROFILE rule.
type SubmissionProfile struct {
	Channel          string         `json:"channel"`
	DocumentType     string         `json:"document_type"`
	ProfileVersion   string         `json:"profile_version"`
	Syntax           string         `json:"syntax,omitempty"`
	TransmissionMode string         `json:"transmission_mode"`
	Timing           *Timing        `json:"timing"`
	Corrections      []string       `json:"corrections"`
	Dependencies     []Dependency   `json:"dependencies"`
	SemanticMapping  []MappingEntry `json:"semantic_mapping,omitempty"`
	Lifecycle        *Lifecycle     `json:"lifecycle"`
	// Filing only.
	ObligationCode string   `json:"obligation_code,omitempty"`
	ApprovalRoles  []string `json:"approval_roles,omitempty"`
	RetentionClass string   `json:"retention_class,omitempty"`
}

func (p RuleParameters) hasOtherFamilyFields(except string) bool {
	return p.Class != "" || p.Rate != nil || len(p.Bands) != 0 || p.Amount != nil || p.Unit != "" || p.Rounding.Mode != "" || p.Rounding.Scale != 0 ||
		p.RecordClass != "" || p.Trigger != "" || p.Minimum != nil || p.FormatRequirement != "" || p.LegalHoldOverride != nil || p.DestructionRule != "" ||
		p.MappingType != "" || len(p.Entries) != 0
}

func (p RuleParameters) validateSubmissionProfile(filing bool) error {
	name := FamilyEInvoiceProfile
	if filing {
		name = FamilyFilingProfile
	}
	if p.hasOtherFamilyFields(name) || p.Profile == nil {
		return paramBad("%s takes only a profile object", name)
	}
	sp := p.Profile
	if !recordClassRe.MatchString(sp.Channel) {
		return paramBad("profile.channel %q must be UPPER_SNAKE", sp.Channel)
	}
	if !recordClassRe.MatchString(sp.DocumentType) || (!filing && !inList(einvoiceDocTypes, sp.DocumentType)) {
		return paramBad("profile.document_type %q is not valid for %s", sp.DocumentType, name)
	}
	if _, ok := parseDecimal(sp.ProfileVersion); !ok {
		return paramBad("profile.profile_version %q must be a decimal string such as \"1.0\"", sp.ProfileVersion)
	}
	if sp.Timing == nil || sp.Timing.RetryWindowHours < 0 || sp.Timing.RetryWindowHours > 720 || sp.Timing.CorrectionWindowDays < 0 || sp.Timing.CorrectionWindowDays > 3650 ||
		(sp.Timing.IssueDeadlineHours != nil && (*sp.Timing.IssueDeadlineHours < 0 || *sp.Timing.IssueDeadlineHours > 87600)) {
		return paramBad("profile.timing needs retry_window_hours 0-720 and correction_window_days 0-3650 (issue_deadline_hours optional, 0-87600)")
	}
	allowed := einvoiceCorrections
	modes := einvoiceModes
	if filing {
		allowed, modes = filingCorrections, filingModes
	}
	if !inList(modes, sp.TransmissionMode) {
		return paramBad("profile.transmission_mode must be one of %s", strings.Join(modes, ", "))
	}
	seenCorr := map[string]bool{}
	for _, c := range sp.Corrections {
		if !inList(allowed, c) || seenCorr[c] {
			return paramBad("profile.corrections entries must be distinct and one of %s", strings.Join(allowed, ", "))
		}
		seenCorr[c] = true
	}
	if err := validateDependencies(sp.Dependencies); err != nil {
		return err
	}
	if err := sp.Lifecycle.validate(); err != nil {
		return err
	}
	if filing {
		if sp.Syntax != "" && !inList(profileSyntaxes, sp.Syntax) {
			return paramBad("profile.syntax must be one of %s", strings.Join(profileSyntaxes, ", "))
		}
		if len(sp.SemanticMapping) != 0 {
			return paramBad("a filing profile does not carry an invoice semantic mapping")
		}
		if !recordClassRe.MatchString(sp.ObligationCode) {
			return paramBad("profile.obligation_code must be UPPER_SNAKE")
		}
		if !recordClassRe.MatchString(sp.RetentionClass) {
			return paramBad("profile.retention_class must be an UPPER_SNAKE record class (s14 retention)")
		}
		if len(sp.ApprovalRoles) > 10 {
			return paramBad("profile.approval_roles has too many roles")
		}
		seenRole := map[string]bool{}
		for _, r := range sp.ApprovalRoles {
			if !recordClassRe.MatchString(r) || seenRole[r] {
				return paramBad("profile.approval_roles must be distinct UPPER_SNAKE roles")
			}
			seenRole[r] = true
		}
		return nil
	}
	if !inList(profileSyntaxes, sp.Syntax) {
		return paramBad("profile.syntax must be one of %s", strings.Join(profileSyntaxes, ", "))
	}
	if sp.ObligationCode != "" || len(sp.ApprovalRoles) != 0 || sp.RetentionClass != "" {
		return paramBad("obligation_code, approval_roles and retention_class belong to a filing profile")
	}
	if len(sp.SemanticMapping) == 0 || len(sp.SemanticMapping) > maxMappingEntries {
		return paramBad("profile.semantic_mapping needs 1 to %d canonical-to-target entries", maxMappingEntries)
	}
	seen := map[string]bool{}
	for i, e := range sp.SemanticMapping {
		if e.From == "" || e.To == "" || len(e.From) > 200 || len(e.To) > 200 {
			return paramBad("profile.semantic_mapping[%d] needs from and to (at most 200 characters)", i)
		}
		if seen[e.From] {
			return paramBad("profile.semantic_mapping[%d]: %q is mapped twice", i, e.From)
		}
		seen[e.From] = true
	}
	return nil
}

func parseDay(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	return t, err == nil
}

func validateDependencies(deps []Dependency) error {
	if len(deps) == 0 || len(deps) > 50 {
		return paramBad("profile.dependencies needs 1 to 50 pinned schema, code-list or business-rule versions (no schema is assumed)")
	}
	seen := map[string]bool{}
	for i, d := range deps {
		if !inList(dependencyKinds, d.Kind) || !mappingCodeRe.MatchString(d.Ref) || !mappingCodeRe.MatchString(d.Version) {
			return paramBad("profile.dependencies[%d] needs kind (%s), ref and version", i, strings.Join(dependencyKinds, ", "))
		}
		from, ok := parseDay(d.ValidFrom)
		if !ok {
			return paramBad("profile.dependencies[%d].valid_from must be a date, YYYY-MM-DD", i)
		}
		if d.ValidTo != nil {
			to, ok := parseDay(*d.ValidTo)
			if !ok || to.Before(from) {
				return paramBad("profile.dependencies[%d].valid_to must be a date on or after valid_from, or null", i)
			}
		}
		k := d.Kind + "|" + d.Ref + "|" + d.Version
		if seen[k] {
			return paramBad("profile.dependencies[%d] repeats %s %s", i, d.Ref, d.Version)
		}
		seen[k] = true
	}
	return nil
}

func (l *Lifecycle) states() map[string]bool {
	s := map[string]bool{}
	if l == nil {
		return s
	}
	s[l.Initial] = true
	for _, t := range l.Transitions {
		for _, x := range t {
			s[x] = true
		}
	}
	for _, x := range l.Terminal {
		s[x] = true
	}
	return s
}

func (l *Lifecycle) validate() error {
	if l == nil {
		return paramBad("profile.lifecycle is required")
	}
	if !recordClassRe.MatchString(l.Initial) {
		return paramBad("lifecycle.initial must be an UPPER_SNAKE state")
	}
	if len(l.Terminal) == 0 || len(l.Transitions) == 0 {
		return paramBad("lifecycle needs at least one terminal state and one transition")
	}
	terminal := map[string]bool{}
	for _, t := range l.Terminal {
		if !recordClassRe.MatchString(t) || terminal[t] {
			return paramBad("lifecycle.terminal entries must be distinct UPPER_SNAKE states")
		}
		terminal[t] = true
	}
	if terminal[l.Initial] {
		return paramBad("the initial state cannot be terminal")
	}
	next := map[string][]string{}
	pairs := map[string]bool{}
	for i, t := range l.Transitions {
		if len(t) != 2 || !recordClassRe.MatchString(t[0]) || !recordClassRe.MatchString(t[1]) {
			return paramBad("lifecycle.transitions[%d] must be a [from, to] pair of UPPER_SNAKE states", i)
		}
		if terminal[t[0]] {
			return paramBad("lifecycle.transitions[%d]: %s is terminal and cannot be left (no illegal regression)", i, t[0])
		}
		if t[1] == l.Initial {
			return paramBad("lifecycle.transitions[%d]: nothing may return to the initial state", i)
		}
		if pairs[t[0]+">"+t[1]] {
			return paramBad("lifecycle.transitions[%d] repeats %s to %s", i, t[0], t[1])
		}
		pairs[t[0]+">"+t[1]] = true
		next[t[0]] = append(next[t[0]], t[1])
	}
	states := l.states()
	if len(states) > maxLifecycleStates {
		return paramBad("lifecycle has too many states")
	}
	// Every state must be reachable from the initial state, and every non-terminal state must lead somewhere.
	reach := map[string]bool{l.Initial: true}
	queue := []string{l.Initial}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, n := range next[cur] {
			if !reach[n] {
				reach[n] = true
				queue = append(queue, n)
			}
		}
	}
	for s := range states {
		if !reach[s] {
			return paramBad("lifecycle state %s is not reachable from %s", s, l.Initial)
		}
		if !terminal[s] && len(next[s]) == 0 {
			return paramBad("lifecycle state %s is not terminal and has no way out", s)
		}
	}
	for _, r := range l.ReceiptRequired {
		if !states[r] || r == l.Initial {
			return paramBad("lifecycle.receipt_required names %s, which is not a state that can be entered", r)
		}
	}
	// JUR-NEG-09: an acceptance can never be recorded without the authority's receipt.
	for _, r := range []string{"ACCEPTED", "CLEARED", "FILED"} {
		if states[r] && !containsStr(l.ReceiptRequired, r) {
			return paramBad("lifecycle state %s must require a receipt (no false accepted state)", r)
		}
	}
	return nil
}

func containsStr(list []string, v string) bool { return inList(list, v) }

func (l *Lifecycle) isTerminal(s string) bool { return l != nil && inList(l.Terminal, s) }

func (l *Lifecycle) allowed(from, to string) bool {
	if l == nil {
		return false
	}
	for _, t := range l.Transitions {
		if len(t) == 2 && t[0] == from && t[1] == to {
			return true
		}
	}
	return false
}

// IsTerminal reports whether s is a terminal state of the lifecycle.
func (p *SubmissionProfile) IsTerminal(s string) bool { return p != nil && p.Lifecycle.isTerminal(s) }

// DependencyUse is one schema, code list or rule package a submission was built with.
type DependencyUse struct {
	Ref     string `json:"ref"`
	Version string `json:"version"`
}

// DependencyCheck says whether every dependency a submission used is declared by
// the profile and valid on the submission date. The failure names the exact
// dependency and version (JUR-NEG-08, JUR-NEG-27).
type DependencyCheck struct {
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
}

// CheckDependencies validates the dependencies a submission used at a date.
func (sp *SubmissionProfile) CheckDependencies(used []DependencyUse, on time.Time) DependencyCheck {
	day := time.Date(on.Year(), on.Month(), on.Day(), 0, 0, 0, 0, time.UTC)
	for _, u := range used {
		var found *Dependency
		for i := range sp.Dependencies {
			if sp.Dependencies[i].Ref == u.Ref && sp.Dependencies[i].Version == u.Version {
				found = &sp.Dependencies[i]
				break
			}
		}
		if found == nil {
			return DependencyCheck{DependencyUnknown, fmt.Sprintf("%s version %s is not a dependency of this profile; declared versions: %s", u.Ref, u.Version, declaredVersions(sp.Dependencies, u.Ref))}
		}
		from, _ := parseDay(found.ValidFrom)
		if day.Before(from) {
			return DependencyCheck{DependencyExpired, fmt.Sprintf("%s version %s is not valid until %s (submission date %s)", u.Ref, u.Version, found.ValidFrom, day.Format("2006-01-02"))}
		}
		if found.ValidTo != nil {
			if to, _ := parseDay(*found.ValidTo); day.After(to) {
				return DependencyCheck{DependencyExpired, fmt.Sprintf("%s version %s expired on %s (submission date %s)", u.Ref, u.Version, *found.ValidTo, day.Format("2006-01-02"))}
			}
		}
	}
	return DependencyCheck{Outcome: OutcomeProfileResolved}
}

func declaredVersions(deps []Dependency, ref string) string {
	var v []string
	for _, d := range deps {
		if d.Ref == ref {
			v = append(v, d.Version)
		}
	}
	if len(v) == 0 {
		return "none for " + ref
	}
	return strings.Join(v, ", ")
}

// CheckApprovals verifies a filing's required approvals before submission (s14):
// every required role is held by a principal who is not the preparer, and one
// principal cannot cover two roles.
func (sp *SubmissionProfile) CheckApprovals(preparer string, approvals []Approval) DependencyCheck {
	used := map[string]bool{}
	for _, role := range sp.ApprovalRoles {
		ok := false
		for _, a := range approvals {
			if a.Role == role && a.PrincipalID != "" && a.PrincipalID != preparer && !used[a.PrincipalID] {
				used[a.PrincipalID] = true
				ok = true
				break
			}
		}
		if !ok {
			return DependencyCheck{ApprovalMissing, fmt.Sprintf("the role %s has no approval from a principal other than the preparer and the other approvers", role)}
		}
	}
	return DependencyCheck{Outcome: OutcomeProfileResolved}
}

// Approval is a role sign-off that a filing needs before submission.
type Approval struct {
	Role        string `json:"role"`
	PrincipalID string `json:"principal_id"`
}

// SubmissionEvent is a status report from an authority or provider.
type SubmissionEvent struct {
	ProviderEventID string
	Status          string
	ReceiptID       string
	OccurredAt      time.Time
}

// EvaluateEvent decides what a status report does to a submission. Pure: the store
// applies the result under a row lock.
//
//	UNKNOWN_STATUS      a status the lifecycle does not define
//	AFTER_TERMINAL      the submission already reached a final outcome (late or out-of-order callbacks cannot change it)
//	MISSING_RECEIPT     a state that needs an authority receipt was reported without one
//	STALE               the report is older than the last applied one
//	ILLEGAL_TRANSITION  the lifecycle does not allow current to status
//	APPLIED             the status becomes current
func (sp *SubmissionProfile) EvaluateEvent(current string, lastOccurred *time.Time, ev SubmissionEvent) string {
	l := sp.Lifecycle
	if !l.states()[ev.Status] || ev.Status == l.Initial {
		return DispUnknownStatus
	}
	if l.isTerminal(current) {
		return DispAfterTerminal
	}
	if inList(l.ReceiptRequired, ev.Status) && strings.TrimSpace(ev.ReceiptID) == "" {
		return DispMissingReceipt
	}
	if lastOccurred != nil && ev.OccurredAt.Before(*lastOccurred) {
		return DispStale
	}
	if !l.allowed(current, ev.Status) {
		return DispIllegalTransition
	}
	return DispApplied
}
