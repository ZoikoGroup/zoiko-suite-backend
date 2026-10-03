package domain

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ZS-JUR-001 Wave 8: jurisdiction rollout (s32, s35, s36, s37).
//
// The standard defines the architecture, not the substantive local law. A pack
// family named in the portfolio does NOT mean certified rules exist: production
// support begins only after local expert review and certification. This file is
// the rollout GOVERNANCE: the portfolio, the Definition of Ready and Definition
// of Done checklists, and the launch gate. It holds no regulatory content.

// Rollout statuses.
const (
	RolloutPlanned   = "PLANNED"
	RolloutAuthoring = "AUTHORING"
	RolloutReady     = "READY"
	RolloutLaunched  = "LAUNCHED"
	RolloutSuspended = "SUSPENDED"
	RolloutRetired   = "RETIRED"

	// UnassignedOwner marks a seeded portfolio entry with no accountable owner yet.
	UnassignedOwner = "UNASSIGNED"

	// Support outcomes: explicit, never a guess (JUR-NEG-22).
	SupportSupported    = "SUPPORTED"
	SupportNotYet       = "NOT_YET_SUPPORTED"
	SupportSuspended    = "SUSPENDED"
	SupportNoRollout    = "NO_ROLLOUT"
	SupportNoReleasedPk = "NO_RELEASED_PACK"
)

// RolloutLayers are the s32 portfolio layers.
var RolloutLayers = []string{"GLOBAL_REFERENCE", "GLOBAL_CORE", "REGIONAL_FRAMEWORK", "COUNTRY", "SUBDIVISION"}

var familyRefRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidFamilyRef reports whether s is a well-formed pack family reference.
func ValidFamilyRef(s string) bool { return familyRefRe.MatchString(s) }

// ChecklistItem is one Definition of Ready or Definition of Done item.
type ChecklistItem struct {
	Code      string `json:"code"`
	Checklist string `json:"checklist"`
	Text      string `json:"text"`
}

// ReadyChecklist is the s35 Definition of Ready for a jurisdiction pack.
var ReadyChecklist = []ChecklistItem{
	{"DOR_01", "READY", "Jurisdiction and regime IDs exist in the canonical registry."},
	{"DOR_02", "READY", "Authoritative sources are captured and reviewed."},
	{"DOR_03", "READY", "Interpretation decisions and assumptions are documented."},
	{"DOR_04", "READY", "Effective-date model is unambiguous."},
	{"DOR_05", "READY", "Dependencies, code lists and schemas are pinned."},
	{"DOR_06", "READY", "Rule modules have stable IDs and declared precedence."},
	{"DOR_07", "READY", "Golden, boundary, negative and historical cases exist."},
	{"DOR_08", "READY", "Security/data classification and residency impacts are reviewed."},
	{"DOR_09", "READY", "Operational owner and support model are assigned."},
	{"DOR_10", "READY", "Rollback target and migration/remediation notes are defined."},
}

// DoneChecklist is the s36 Definition of Done for a production release.
var DoneChecklist = []ChecklistItem{
	{"DOD_01", "DONE", "Compiler, schema lint, dependency and conflict checks pass."},
	{"DOD_02", "DONE", "All mandatory tests pass with captured digest."},
	{"DOD_03", "DONE", "Qualified independent review is complete."},
	{"DOD_04", "DONE", "Certification record is approved and immutable."},
	{"DOD_05", "DONE", "Artifact digest and signature verify."},
	{"DOD_06", "DONE", "Deployment rings and region eligibility are configured."},
	{"DOD_07", "DONE", "Runtime services can resolve pack/rule version deterministically."},
	{"DOD_08", "DONE", "Observability dashboards/alerts are active."},
	{"DOD_09", "DONE", "Source-to-outcome evidence is demonstrably reconstructable."},
	{"DOD_10", "DONE", "Rollback and emergency support procedures are exercised or evidenced."},
	{"DOD_11", "DONE", "Documentation and user-facing regulatory disclosures are updated where required."},
}

// ChecklistFor returns the checklist items for "READY" or "DONE", or nil.
func ChecklistFor(checklist string) []ChecklistItem {
	switch checklist {
	case "READY":
		return ReadyChecklist
	case "DONE":
		return DoneChecklist
	}
	return nil
}

// ChecklistHasItem reports whether code is an item of the checklist.
func ChecklistHasItem(checklist, code string) bool {
	for _, it := range ChecklistFor(checklist) {
		if it.Code == code {
			return true
		}
	}
	return false
}

// PortfolioEntry is one s32 initial portfolio family.
type PortfolioEntry struct {
	FamilyRef        string
	DisplayName      string
	Layer            string
	JurisdictionCode string
	Note             string
}

// InitialPortfolio is the s32 recommended initial structure. Families that
// repeat per state or province (us.state.<state>, ca.province.<province>) are
// created one at a time as they are taken on, not seeded.
var InitialPortfolio = []PortfolioEntry{
	{"global.reference", "Global reference data", "GLOBAL_REFERENCE", "", "ISO/canonical currencies, countries/subdivisions, generic date/money semantics"},
	{"global.tax.core", "Global tax core", "GLOBAL_CORE", "", "Shared tax calculation contract, provenance, rounding framework"},
	{"global.invoice.core", "Global invoice core", "GLOBAL_CORE", "", "Canonical invoice semantics and common validation contract"},
	{"eu.vat.framework", "EU VAT framework", "REGIONAL_FRAMEWORK", "", "Shared EU-layer VAT/e-invoicing/digital-reporting framework where applicable"},
	{"gb", "United Kingdom", "COUNTRY", "GB", "UK tax, invoicing, filing, calendar and retention pack families"},
	{"us.federal", "United States federal", "COUNTRY", "US", "US federal tax/filing rule families"},
	{"in", "India", "COUNTRY", "IN", "India national/state GST and related compliance pack families"},
	{"au", "Australia", "COUNTRY", "AU", "Australia tax/reporting pack families"},
	{"de", "Germany", "COUNTRY", "DE", "Germany national/EU-aligned pack families"},
	{"ca.federal", "Canada federal", "COUNTRY", "CA", "Canada federal pack families"},
}

// RolloutSnapshot is the state a transition is judged on.
type RolloutSnapshot struct {
	Status string
	Owner  string
	// Met maps an item code to whether its LATEST attestation is met.
	Met             map[string]bool
	ApprovedExperts int
	// Experts are the principals who have recorded an expert decision (they cannot launch).
	Experts        []string
	LinkedPacks    int
	ReleasedLinked int
}

var rolloutEdges = map[string][]string{
	RolloutPlanned:   {RolloutAuthoring, RolloutRetired},
	RolloutAuthoring: {RolloutReady, RolloutRetired},
	RolloutReady:     {RolloutAuthoring, RolloutLaunched, RolloutRetired},
	RolloutLaunched:  {RolloutSuspended, RolloutRetired},
	RolloutSuspended: {RolloutLaunched, RolloutRetired},
	RolloutRetired:   {},
}

// ValidRolloutStatus reports whether s is a rollout status.
func ValidRolloutStatus(s string) bool { _, ok := rolloutEdges[s]; return ok }

func missingItems(items []ChecklistItem, met map[string]bool) []string {
	var out []string
	for _, it := range items {
		if !met[it.Code] {
			out = append(out, it.Code)
		}
	}
	sort.Strings(out)
	return out
}

// TransitionBlockers lists, in plain words, everything that stops the rollout moving
// to the target status. An empty list means the transition may proceed. The database
// enforces the same gates, so these are explanations, not the only line of defence.
func TransitionBlockers(s RolloutSnapshot, to, actor, reason string) []string {
	var b []string
	allowed := false
	for _, n := range rolloutEdges[s.Status] {
		allowed = allowed || n == to
	}
	if !allowed {
		return []string{fmt.Sprintf("a rollout in %s cannot move to %s", s.Status, to)}
	}
	if (to == RolloutSuspended || to == RolloutRetired || (s.Status == RolloutReady && to == RolloutAuthoring)) && strings.TrimSpace(reason) == "" {
		b = append(b, "a reason is required for this transition")
	}
	switch to {
	case RolloutAuthoring:
		if s.Status == RolloutPlanned && (s.Owner == "" || s.Owner == UnassignedOwner) {
			b = append(b, "an accountable owner must be assigned before authoring starts")
		}
	case RolloutReady:
		if m := missingItems(ReadyChecklist, s.Met); len(m) > 0 {
			b = append(b, "Definition of Ready is incomplete: "+strings.Join(m, ", "))
		}
	case RolloutLaunched:
		if m := missingItems(ReadyChecklist, s.Met); len(m) > 0 {
			b = append(b, "Definition of Ready is incomplete: "+strings.Join(m, ", "))
		}
		if m := missingItems(DoneChecklist, s.Met); len(m) > 0 {
			b = append(b, "Definition of Done is incomplete: "+strings.Join(m, ", "))
		}
		if s.ApprovedExperts < 1 {
			b = append(b, "no qualified local expert has approved this rollout (production support begins only after local expert review)")
		}
		if s.LinkedPacks < 1 {
			b = append(b, "no pack is linked to this rollout")
		} else if s.ReleasedLinked < 1 {
			b = append(b, "none of the linked packs has a RELEASED version")
		}
		if actor == s.Owner {
			b = append(b, "the rollout owner cannot launch their own rollout")
		}
		for _, e := range s.Experts {
			if e == actor {
				b = append(b, "an approving or rejecting expert cannot launch the rollout")
				break
			}
		}
	}
	return b
}

// RolloutBlockedError carries the reasons a transition was refused.
type RolloutBlockedError struct{ Blockers []string }

func (e *RolloutBlockedError) Error() string {
	return "the rollout cannot make this transition: " + strings.Join(e.Blockers, "; ")
}
