package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ZS-JUR-001 Wave 1: the pack compiler (s21).
//
// Compile is a pure function: the store gathers the inputs inside one
// transaction, Compile validates and normalizes them, and the result is an
// artifact whose canonical bytes (and therefore digest) depend ONLY on the
// inputs. There is no timestamp or randomness inside the artifact, so
// recompiling unchanged inputs reproduces the same digest, and changed inputs
// under the same version are detected as a collision (JUR-NEG-20).
//
// What is deliberately not here: a rule DSL or decision-table compiler. The
// document leaves that technology to a controlled decision (s38), so rule
// modules are the existing jurisdiction_rules rows, normalized and snapshotted
// as they are. Test execution (s22) is Wave 3; the report says so.

const (
	CompilerName    = "zoiko-pack-compiler"
	CompilerVersion = "1.0.0"
	ArtifactFormat  = "zs-jur-001/pack-artifact"
	ArtifactFormatV = "1"
)

// Finding severities.
const (
	SeverityError   = "ERROR"
	SeverityWarning = "WARNING"
)

// Finding is one compiler diagnostic.
type Finding struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Subject  string `json:"subject,omitempty"`
}

// CompileReport is the diagnostic output. It is stored beside the artifact
// but is not part of the digest.
type CompileReport struct {
	CompilerName    string    `json:"compiler_name"`
	CompilerVersion string    `json:"compiler_version"`
	Errors          int       `json:"errors"`
	Warnings        int       `json:"warnings"`
	Findings        []Finding `json:"findings"`
	TestsExecuted   bool      `json:"tests_executed"`
	TestsNote       string    `json:"tests_note"`
}

// Named pairs a registry id with its code.
type Named struct {
	ID   string
	Code string
	// ParentID is the parent jurisdiction (jurisdictions only), so the artifact
	// can resolve rules up the hierarchy without reading live registry rows.
	ParentID *string
}

// CompileRule is one rule module as the store read it, with its provenance.
type CompileRule struct {
	RuleID         string
	JurisdictionID string
	RuleDomain     string
	RuleCode       string
	RuleName       string
	EffectiveFrom  time.Time
	EffectiveTo    *time.Time
	Payload        json.RawMessage
	RuleStatus     string
	// Parameters are the typed calculation parameters (ZS-JUR-001 Wave 4); nil when the rule has none.
	Parameters       json.RawMessage
	RegimeID         *string
	InterpretationID *string
	SupersedesRuleID *string
	Precedence       *int
	PublishedOn      *string
	SourceIDs        []string
}

// CompileSource is a source-register entry as read at compile time.
type CompileSource struct {
	SourceID           string
	JurisdictionID     string
	Authority          string
	SourceType         string
	AuthorityLevel     string
	Title              string
	OfficialIdentifier *string
	PublishedOn        *string
	EffectiveOn        *string
	Location           string
	SnapshotHash       string
	SnapshotRef        *string
	Language           string
	Reviewed           bool
	Superseded         bool
}

// CompileInterpretation is an interpretation record as read at compile time.
type CompileInterpretation struct {
	InterpretationID string
	JurisdictionID   string
	RegimeID         *string
	Subject          string
	Decision         string
	Rationale        string
	Approved         bool
	SourceIDs        []string
}

// CompileDependency is one manifest dependency with what the registry knows.
type CompileDependency struct {
	Ref     string
	Version string
	Group   string // "dependencies" or "schema_dependencies"

	// IsPack is true when Ref is a registered pack_ref.
	IsPack bool
	// Found is true when that exact pack version exists.
	Found          bool
	Status         string
	ArtifactDigest *string
}

// CompileInput is everything Compile needs; the store fills it from one transaction.
type CompileInput struct {
	PackRef        string
	Version        string
	Manifest       PackManifest
	ManifestDigest string

	Jurisdictions []Named
	Regimes       []Named

	Rules                []CompileRule
	MissingRuleIDs       []string
	Calendars            []CompileCalendar
	Obligations          []CompileObligation
	MissingCalendarIDs   []string
	MissingObligationIDs []string
	Sources              []CompileSource
	Interpretations      []CompileInterpretation
	Dependencies         []CompileDependency
}

// CompileOutput is the artifact (nil when the compile has errors) and its report.
type CompileOutput struct {
	ArtifactJSON   []byte
	ArtifactDigest string
	Report         CompileReport
}

// HasErrors reports whether the compile must be rejected.
func (r CompileReport) HasErrors() bool { return r.Errors > 0 }

type findings struct{ list []Finding }

func (f *findings) err(code, subject, format string, a ...any) {
	f.list = append(f.list, Finding{SeverityError, code, fmt.Sprintf(format, a...), subject})
}
func (f *findings) warn(code, subject, format string, a ...any) {
	f.list = append(f.list, Finding{SeverityWarning, code, fmt.Sprintf(format, a...), subject})
}

// Compile validates the inputs and, when they are clean, produces the artifact.
func Compile(in CompileInput) CompileOutput {
	var f findings

	if in.Manifest.PackID != in.PackRef || in.Manifest.PackVersion != in.Version {
		f.err("JUR-C001", in.PackRef, "manifest identity %s@%s does not match the pack version %s@%s",
			in.Manifest.PackID, in.Manifest.PackVersion, in.PackRef, in.Version)
	}
	if err := in.Manifest.Validate(); err != nil {
		f.err("JUR-C002", in.PackRef, "%v", err)
	}

	jurByID := map[string]string{}
	for _, j := range in.Jurisdictions {
		jurByID[j.ID] = j.Code
	}
	regByID := map[string]string{}
	for _, r := range in.Regimes {
		regByID[r.ID] = r.Code
	}
	srcByID := map[string]CompileSource{}
	for _, s := range in.Sources {
		srcByID[s.SourceID] = s
	}
	intByID := map[string]CompileInterpretation{}
	for _, i := range in.Interpretations {
		intByID[i.InterpretationID] = i
	}

	// Rule modules.
	for _, id := range in.MissingRuleIDs {
		f.err("JUR-C020", id, "rule module %s does not exist", id)
	}
	if dup, ok := firstDuplicate(in.Manifest.RuleModules); ok {
		f.err("JUR-C021", dup, "rule_modules lists %s more than once", dup)
	}
	if len(in.Rules) == 0 && len(in.MissingRuleIDs) == 0 && len(in.Calendars) == 0 && len(in.Obligations) == 0 {
		f.warn("JUR-W010", in.PackRef, "pack has no rule, calendar or obligation modules; the artifact carries scope and dependencies only")
	}

	rules := append([]CompileRule(nil), in.Rules...)
	sort.Slice(rules, func(a, b int) bool { return rules[a].RuleID < rules[b].RuleID })

	usedSources := map[string]bool{}
	usedInterps := map[string]bool{}
	for _, r := range rules {
		subj := r.RuleID
		if _, ok := jurByID[r.JurisdictionID]; !ok {
			f.err("JUR-C022", subj, "rule %s/%s belongs to a jurisdiction outside the pack scope", r.RuleDomain, r.RuleCode)
		}
		switch {
		case r.RegimeID == nil:
			f.err("JUR-C023", subj, "rule %s/%s declares no regime; every rule needs jurisdiction and regime scope", r.RuleDomain, r.RuleCode)
		default:
			if _, ok := regByID[*r.RegimeID]; !ok {
				f.err("JUR-C023", subj, "rule %s/%s regime is not in the pack's regimes", r.RuleDomain, r.RuleCode)
			}
		}
		if r.RuleStatus == "RETIRED" {
			f.err("JUR-C030", subj, "rule %s/%s is RETIRED and cannot be packaged", r.RuleDomain, r.RuleCode)
		}
		if r.RuleStatus == "DRAFT" {
			f.warn("JUR-W031", subj, "rule %s/%s is still DRAFT; the artifact snapshots its current content", r.RuleDomain, r.RuleCode)
		}
		if !packWindowOverlaps(r, in.Manifest) {
			f.warn("JUR-W032", subj, "rule %s/%s is never effective within the pack's effective window", r.RuleDomain, r.RuleCode)
		}

		// Provenance (s3 invariant 2, s7).
		if len(r.SourceIDs) == 0 {
			f.err("JUR-C024", subj, "rule %s/%s cites no authoritative source", r.RuleDomain, r.RuleCode)
		}
		allGuidance := len(r.SourceIDs) > 0
		for _, sid := range r.SourceIDs {
			usedSources[sid] = true
			s, ok := srcByID[sid]
			switch {
			case !ok:
				f.err("JUR-C027", subj, "rule %s/%s cites unknown source %s", r.RuleDomain, r.RuleCode, sid)
				allGuidance = false
			case s.Superseded:
				f.err("JUR-C027", subj, "rule %s/%s cites superseded source %s; re-point it to the replacement", r.RuleDomain, r.RuleCode, sid)
			case !s.Reviewed:
				f.err("JUR-C027", subj, "rule %s/%s cites source %s that has not had its independent review", r.RuleDomain, r.RuleCode, sid)
			}
			if ok && !strings.Contains(strings.ToUpper(s.AuthorityLevel), "GUIDANCE") {
				allGuidance = false
			}
		}
		if allGuidance {
			f.warn("JUR-W030", subj, "every source of rule %s/%s is non-binding guidance (JUR-NEG-23); the approved interpretation is the only authority basis",
				r.RuleDomain, r.RuleCode)
		}
		if r.InterpretationID == nil {
			f.err("JUR-C025", subj, "rule %s/%s has no interpretation record", r.RuleDomain, r.RuleCode)
		} else {
			usedInterps[*r.InterpretationID] = true
			if it, ok := intByID[*r.InterpretationID]; !ok || !it.Approved {
				f.err("JUR-C026", subj, "rule %s/%s interpretation is not approved (JUR-NEG-18)", r.RuleDomain, r.RuleCode)
			}
		}

		// Float lint (JUR-NEG-25): no binary floating-point in rule content.
		if why := floatInJSON(r.Payload); why != "" {
			f.err("JUR-C050", subj, "rule %s/%s payload contains %s; write rates and amounts as decimal strings", r.RuleDomain, r.RuleCode, why)
		}
		if len(r.Parameters) == 0 && ReservedDomain(r.RuleDomain) {
			f.err("JUR-C093", subj, "rule %s/%s is in a reserved parameter domain but carries no parameters", r.RuleDomain, r.RuleCode)
		}
		if len(r.Parameters) > 0 {
			if pp, perr := ParseRuleParameters(r.Parameters); perr != nil {
				f.err("JUR-C090", subj, "rule %s/%s parameters: %v", r.RuleDomain, r.RuleCode, perr)
			} else if r.RuleDomain == PayrollDomain && pp.Class == "" && FamilyDomain(pp.Family) == "" {
				f.err("JUR-C091", subj, "payroll rule %s must declare its s17 statutory parameter class (%s)", r.RuleCode, strings.Join(ParameterClasses, ", "))
			} else if FamilyDomain(pp.Family) != r.RuleDomain && (FamilyDomain(pp.Family) != "" || ReservedDomain(r.RuleDomain)) {
				f.err("JUR-C092", subj, "rule %s/%s: family %s belongs only to the %s domain, and the domains %s are reserved for their own families", r.RuleDomain, r.RuleCode, pp.Family, FamilyDomain(pp.Family), strings.Join(reservedDomains, ", "))
			}
			if why := floatInJSON(r.Parameters); why != "" {
				f.err("JUR-C050", subj, "rule %s/%s parameters contain %s; write rates and amounts as decimal strings", r.RuleDomain, r.RuleCode, why)
			}
		}
	}

	// Conflict detection (s10): overlapping rules need declared, distinct
	// precedence or an explicit supersedes link.
	for a := 0; a < len(rules); a++ {
		for b := a + 1; b < len(rules); b++ {
			ra, rb := rules[a], rules[b]
			if ra.RuleDomain != rb.RuleDomain || ra.RuleCode != rb.RuleCode || !intervalsOverlap(ra, rb) {
				continue
			}
			if resolvesConflict(ra, rb) {
				continue
			}
			f.err("JUR-C040", ra.RuleID+"|"+rb.RuleID,
				"rules %s and %s (%s/%s) overlap in time with no declared precedence or supersedes link (JUR-NEG-02)",
				ra.RuleID, rb.RuleID, ra.RuleDomain, ra.RuleCode)
		}
	}

	checkCalendarAndObligationModules(&in, &f, jurByID, regByID, srcByID, intByID, usedSources, usedInterps)

	// Dependencies: exact pins; registered packs must exist, be compiled and be usable.
	deps := append([]CompileDependency(nil), in.Dependencies...)
	sort.Slice(deps, func(a, b int) bool {
		if deps[a].Group != deps[b].Group {
			return deps[a].Group < deps[b].Group
		}
		return deps[a].Ref < deps[b].Ref
	})
	for _, d := range deps {
		subj := d.Ref + "@" + d.Version
		switch {
		case d.Ref == in.PackRef:
			f.err("JUR-C060", subj, "a pack cannot depend on itself")
		case !d.IsPack:
			f.warn("JUR-W060", subj, "external %s entry is pinned but cannot be verified: no code-list or schema registry exists yet", d.Group)
		case !d.Found:
			f.err("JUR-C061", subj, "dependency pack version does not exist")
		case d.Status == "WITHDRAWN" || d.Status == "EMERGENCY_BLOCKED":
			f.err("JUR-C062", subj, "dependency pack version is %s and cannot be depended on", d.Status)
		case d.ArtifactDigest == nil:
			f.err("JUR-C063", subj, "dependency pack version has not been compiled, so it cannot be pinned by digest")
		}
	}

	report := CompileReport{
		CompilerName: CompilerName, CompilerVersion: CompilerVersion,
		TestsExecuted: false,
		TestsNote:     "mandatory test suites run in the certification harness (ZS-JUR-001 Wave 3); none were executed by the compiler",
	}
	sort.SliceStable(f.list, func(a, b int) bool {
		if f.list[a].Severity != f.list[b].Severity {
			return f.list[a].Severity == SeverityError
		}
		if f.list[a].Code != f.list[b].Code {
			return f.list[a].Code < f.list[b].Code
		}
		return f.list[a].Subject < f.list[b].Subject
	})
	report.Findings = f.list
	for _, x := range f.list {
		if x.Severity == SeverityError {
			report.Errors++
		} else {
			report.Warnings++
		}
	}
	if report.Errors > 0 {
		return CompileOutput{Report: report}
	}

	art := buildArtifact(in, rules, deps, usedSources, usedInterps)
	raw, err := json.Marshal(art)
	if err == nil {
		raw, err = CanonicalJSON(raw)
	}
	if err != nil { // unreachable for these types, but never emit a half-built artifact
		report.Findings = append(report.Findings, Finding{SeverityError, "JUR-C099", "artifact encoding failed: " + err.Error(), ""})
		report.Errors++
		return CompileOutput{Report: report}
	}
	digest, _ := DigestOf(raw)
	return CompileOutput{ArtifactJSON: raw, ArtifactDigest: digest, Report: report}
}

func buildArtifact(in CompileInput, rules []CompileRule, deps []CompileDependency, usedSources, usedInterps map[string]bool) map[string]any {
	calOut, oblOut := buildCalendarObligationSections(in)
	jur := append([]Named(nil), in.Jurisdictions...)
	sort.Slice(jur, func(a, b int) bool { return jur[a].Code < jur[b].Code })
	reg := append([]Named(nil), in.Regimes...)
	sort.Slice(reg, func(a, b int) bool { return reg[a].Code < reg[b].Code })
	named := func(ns []Named) []map[string]any {
		out := make([]map[string]any, 0, len(ns))
		for _, n := range ns {
			m := map[string]any{"id": n.ID, "code": n.Code}
			if n.ParentID != nil {
				m["parent_id"] = *n.ParentID
			}
			out = append(out, m)
		}
		return out
	}

	ruleOut := make([]map[string]any, 0, len(rules))
	for _, r := range rules {
		m := map[string]any{
			"rule_id": r.RuleID, "jurisdiction_id": r.JurisdictionID, "rule_domain": r.RuleDomain, "rule_code": r.RuleCode,
			"rule_name": r.RuleName, "effective_from": r.EffectiveFrom.UTC().Format(time.RFC3339Nano),
			"regime_id": r.RegimeID, "interpretation_id": r.InterpretationID, "supersedes_rule_id": r.SupersedesRuleID,
			"precedence": r.Precedence, "published_on": r.PublishedOn, "payload": json.RawMessage(r.Payload), "parameters": rawOrNil(r.Parameters),
			"source_ids": sortedCopy(r.SourceIDs),
		}
		if r.EffectiveTo != nil {
			m["effective_to"] = r.EffectiveTo.UTC().Format(time.RFC3339Nano)
		} else {
			m["effective_to"] = nil
		}
		// Content digest of the rule module itself, so a consumer can pin one rule.
		if raw, err := json.Marshal(m); err == nil {
			if d, err := DigestOf(raw); err == nil {
				m["content_digest"] = d
			}
		}
		ruleOut = append(ruleOut, m)
	}

	srcs := make([]map[string]any, 0, len(usedSources))
	for _, s := range sortedSources(in.Sources) {
		if !usedSources[s.SourceID] {
			continue
		}
		srcs = append(srcs, map[string]any{
			"source_id": s.SourceID, "jurisdiction_id": s.JurisdictionID, "authority": s.Authority, "source_type": s.SourceType,
			"authority_level": s.AuthorityLevel, "title": s.Title, "official_identifier": s.OfficialIdentifier,
			"published_on": s.PublishedOn, "effective_on": s.EffectiveOn, "location": s.Location,
			"snapshot_hash": s.SnapshotHash, "snapshot_ref": s.SnapshotRef, "language": s.Language,
		})
	}
	ints := make([]map[string]any, 0, len(usedInterps))
	for _, it := range sortedInterps(in.Interpretations) {
		if !usedInterps[it.InterpretationID] {
			continue
		}
		ints = append(ints, map[string]any{
			"interpretation_id": it.InterpretationID, "jurisdiction_id": it.JurisdictionID, "regime_id": it.RegimeID,
			"subject": it.Subject, "decision": it.Decision, "rationale": it.Rationale, "source_ids": sortedCopy(it.SourceIDs),
		})
	}

	packDeps := []map[string]any{}
	external := []map[string]any{}
	for _, d := range deps {
		if d.IsPack {
			packDeps = append(packDeps, map[string]any{"ref": d.Ref, "version": d.Version, "group": d.Group, "artifact_digest": d.ArtifactDigest})
		} else {
			external = append(external, map[string]any{"ref": d.Ref, "version": d.Version, "group": d.Group})
		}
	}

	return map[string]any{
		"format": ArtifactFormat, "format_version": ArtifactFormatV,
		"pack_ref": in.PackRef, "pack_version": in.Version,
		"effective_from": in.Manifest.EffectiveFrom, "effective_to": in.Manifest.EffectiveTo,
		"manifest_digest": in.ManifestDigest, "source_register_version": in.Manifest.SourceRegisterVersion,
		"scope":              map[string]any{"jurisdictions": named(jur), "regimes": named(reg)},
		"rule_modules":       ruleOut,
		"calendar_modules":   calOut,
		"obligation_modules": oblOut,
		"sources":            srcs,
		"interpretations":    ints,
		// SBOM / dependency manifest (s21): exact pins and the toolchain that built this.
		"sbom": map[string]any{
			"pack_dependencies":     packDeps,
			"external_dependencies": external,
			"compiler":              map[string]any{"name": CompilerName, "version": CompilerVersion},
		},
	}
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func sortedSources(in []CompileSource) []CompileSource {
	out := append([]CompileSource(nil), in...)
	sort.Slice(out, func(a, b int) bool { return out[a].SourceID < out[b].SourceID })
	return out
}

func sortedInterps(in []CompileInterpretation) []CompileInterpretation {
	out := append([]CompileInterpretation(nil), in...)
	sort.Slice(out, func(a, b int) bool { return out[a].InterpretationID < out[b].InterpretationID })
	return out
}

// intervalsOverlap compares two half-open effective intervals; nil end = open.
func intervalsOverlap(a, b CompileRule) bool {
	aEndsAfterBStarts := a.EffectiveTo == nil || a.EffectiveTo.After(b.EffectiveFrom)
	bEndsAfterAStarts := b.EffectiveTo == nil || b.EffectiveTo.After(a.EffectiveFrom)
	return aEndsAfterBStarts && bEndsAfterAStarts
}

// resolvesConflict reports whether two overlapping rules carry an explicit,
// declared resolution: distinct non-nil precedence, or a supersedes link.
func resolvesConflict(a, b CompileRule) bool {
	if a.Precedence != nil && b.Precedence != nil && *a.Precedence != *b.Precedence {
		return true
	}
	return (a.SupersedesRuleID != nil && *a.SupersedesRuleID == b.RuleID) ||
		(b.SupersedesRuleID != nil && *b.SupersedesRuleID == a.RuleID)
}

// packWindowOverlaps reports whether the rule is effective at any time inside
// the pack's own effective window.
func packWindowOverlaps(r CompileRule, m PackManifest) bool {
	from, err := time.Parse("2006-01-02", m.EffectiveFrom)
	if err != nil {
		return true // the manifest error is reported separately
	}
	var to *time.Time
	if m.EffectiveTo != nil {
		if t, err := time.Parse("2006-01-02", *m.EffectiveTo); err == nil {
			to = &t
		}
	}
	pack := CompileRule{EffectiveFrom: from, EffectiveTo: to}
	return intervalsOverlap(r, pack)
}

// floatInJSON returns a description of the first non-integer or exponent
// number in raw JSON, or "" when there is none (JUR-NEG-25). Whole numbers are
// exact and allowed; anything that would round-trip through binary floating
// point must be a decimal string instead.
func floatInJSON(raw []byte) string { return FloatInJSON(raw) }

// FloatInJSON returns a description of the first non-integer or exponent number
// in raw JSON, or "" when there is none (JUR-NEG-25).
func FloatInJSON(raw []byte) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "malformed JSON"
	}
	return findFloat(v, "$")
}

func findFloat(v any, path string) string {
	switch x := v.(type) {
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			return fmt.Sprintf("a binary-float number %s at %s", s, path)
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if w := findFloat(x[k], path+"."+k); w != "" {
				return w
			}
		}
	case []any:
		for i, e := range x {
			if w := findFloat(e, fmt.Sprintf("%s[%d]", path, i)); w != "" {
				return w
			}
		}
	}
	return ""
}

// rawOrNil renders absent JSON as an explicit null in the artifact.
func rawOrNil(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
