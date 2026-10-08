package domain

import (
	"encoding/json"
	"sort"
	"time"
)

// ZS-JUR-001 Wave 4: calendar and obligation modules in the pack compiler.
//
// They follow the same invariants as rule modules (s3): every module has a
// stable id, jurisdiction scope, effective interval and provenance (reviewed,
// non-superseded sources and, for obligation rules, an APPROVED interpretation),
// and the compiler snapshots them into the artifact with a content digest so a
// resolver never reads live registry rows. Unlike rule modules, calendar
// versions and obligation rules must already be PUBLISHED (immutable), so a
// compiled artifact can never drift from the rows it was built from.

// CompileCalendar is a calendar version as read at compile time.
type CompileCalendar struct {
	CalendarVersion
	JurisdictionID string
	Published      bool
}

// CompileObligation is an obligation rule as read at compile time.
type CompileObligation struct {
	ObligationRule
	Published bool
}

func (c CompileCalendar) moduleMap() map[string]any {
	hol := make([]map[string]any, 0, len(c.Holidays))
	hs := append([]Holiday(nil), c.Holidays...)
	sort.Slice(hs, func(a, b int) bool { return hs[a].Date < hs[b].Date })
	for _, h := range hs {
		hol = append(hol, map[string]any{"date": h.Date, "name": h.Name})
	}
	wk := append([]int{}, c.WeekendDays...)
	sort.Ints(wk)
	m := map[string]any{
		"calendar_version_id": c.CalendarVersionID, "calendar_code": c.CalendarCode, "jurisdiction_id": c.JurisdictionID, "version": c.Version,
		"effective_from": c.EffectiveFrom.Format(dateLayout), "timezone": c.Timezone, "weekend_days": wk, "cutoff_time": c.CutoffTime,
		"holidays": hol, "source_ids": sortedCopy(c.SourceIDs),
	}
	if raw, err := json.Marshal(m); err == nil {
		if d, err := DigestOf(raw); err == nil {
			m["content_digest"] = d
		}
	}
	return m
}

func (o CompileObligation) moduleMap() map[string]any {
	m := map[string]any{
		"obligation_rule_id": o.ObligationRuleID, "jurisdiction_id": o.JurisdictionID, "regime_id": o.RegimeID, "interpretation_id": o.InterpretationID,
		"obligation_code": o.ObligationCode, "rule_version": o.RuleVersion, "name": o.Name, "period_basis": o.PeriodBasis, "anchor": o.Anchor,
		"offset_months": o.OffsetMonths, "offset_days": o.OffsetDays, "offset_to_month_end": o.OffsetToMonthEnd,
		"business_day_adjustment": o.BusinessDayAdjustment, "calendar_code": nilIfEmpty(o.CalendarCode),
		"effective_from": o.EffectiveFrom.Format(dateLayout), "extension_allowed": o.ExtensionAllowed, "max_extension_days": o.MaxExtensionDays,
		"extension_requires_evidence": o.ExtensionRequiresEvidence, "cutoff_applies": o.CutoffApplies,
		"escalation_owner": o.EscalationOwner, "escalation_sla_hours": o.EscalationSLAHours, "source_ids": sortedCopy(o.SourceIDs),
	}
	if o.EffectiveTo != nil {
		m["effective_to"] = o.EffectiveTo.Format(dateLayout)
	} else {
		m["effective_to"] = nil
	}
	if raw, err := json.Marshal(m); err == nil {
		if d, err := DigestOf(raw); err == nil {
			m["content_digest"] = d
		}
	}
	return m
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// checkSourcesFor reports the standard provenance errors for one module.
func checkSourcesFor(f *findings, code, subject, label string, ids []string, srcByID map[string]CompileSource, used map[string]bool) {
	if len(ids) == 0 {
		f.err(code, subject, "%s cites no authoritative source", label)
		return
	}
	for _, sid := range ids {
		used[sid] = true
		s, ok := srcByID[sid]
		switch {
		case !ok:
			f.err(code, subject, "%s cites unknown source %s", label, sid)
		case s.Superseded:
			f.err(code, subject, "%s cites superseded source %s; re-point it to the replacement", label, sid)
		case !s.Reviewed:
			f.err(code, subject, "%s cites source %s that has not had its independent review", label, sid)
		}
	}
}

// checkCalendarAndObligationModules validates the Wave 4 modules of a pack.
func checkCalendarAndObligationModules(in *CompileInput, f *findings, jurByID, regByID map[string]string,
	srcByID map[string]CompileSource, intByID map[string]CompileInterpretation, usedSources, usedInterps map[string]bool) {

	for _, id := range in.MissingCalendarIDs {
		f.err("JUR-C070", id, "calendar module %s does not exist", id)
	}
	for _, id := range in.MissingObligationIDs {
		f.err("JUR-C080", id, "obligation module %s does not exist", id)
	}
	if dup, ok := firstDuplicate(in.Manifest.CalendarModules); ok {
		f.err("JUR-C070", dup, "calendar_modules lists %s more than once", dup)
	}
	if dup, ok := firstDuplicate(in.Manifest.ObligationModules); ok {
		f.err("JUR-C080", dup, "obligation_modules lists %s more than once", dup)
	}

	calCodes := map[string]bool{}
	for _, c := range in.Calendars {
		subj := c.CalendarVersionID
		label := "calendar " + c.CalendarCode + " v" + itoa(c.Version)
		calCodes[c.CalendarCode] = true
		if !c.Published {
			f.err("JUR-C071", subj, "%s is not PUBLISHED; only immutable calendar versions can be packaged", label)
		}
		if _, ok := jurByID[c.JurisdictionID]; !ok {
			f.err("JUR-C072", subj, "%s belongs to a jurisdiction outside the pack scope", label)
		}
		if err := ValidateCalendarVersion(c.CalendarVersion); err != nil {
			f.err("JUR-C073", subj, "%s: %v", label, err)
		}
		checkSourcesFor(f, "JUR-C074", subj, label, c.SourceIDs, srcByID, usedSources)
	}

	byCode := map[string][]CompileObligation{}
	for _, o := range in.Obligations {
		subj := o.ObligationRuleID
		label := "obligation " + o.ObligationCode + " v" + itoa(o.RuleVersion)
		byCode[o.ObligationCode] = append(byCode[o.ObligationCode], o)
		if !o.Published {
			f.err("JUR-C081", subj, "%s is not PUBLISHED; only immutable obligation rules can be packaged", label)
		}
		if _, ok := jurByID[o.JurisdictionID]; !ok {
			f.err("JUR-C082", subj, "%s belongs to a jurisdiction outside the pack scope", label)
		}
		switch {
		case o.RegimeID == nil:
			f.err("JUR-C083", subj, "%s declares no regime", label)
		default:
			if _, ok := regByID[*o.RegimeID]; !ok {
				f.err("JUR-C083", subj, "%s regime is not in the pack's regimes", label)
			}
		}
		if err := ValidateObligationRule(o.ObligationRule); err != nil {
			f.err("JUR-C084", subj, "%s: %v", label, err)
		}
		checkSourcesFor(f, "JUR-C085", subj, label, o.SourceIDs, srcByID, usedSources)
		if o.InterpretationID == nil {
			f.err("JUR-C086", subj, "%s has no interpretation record", label)
		} else {
			usedInterps[*o.InterpretationID] = true
			if it, ok := intByID[*o.InterpretationID]; !ok || !it.Approved {
				f.err("JUR-C086", subj, "%s interpretation is not approved (JUR-NEG-18)", label)
			}
		}
		// The calendar an obligation names must travel in the same pack: the
		// artifact must be self-contained, and no live lookup is allowed.
		if o.CalendarCode != "" && !calCodes[o.CalendarCode] {
			f.err("JUR-C087", subj, "%s names calendar %q, which is not among the pack's calendar modules", label, o.CalendarCode)
		}
	}
	// Two obligation rules of the same code whose windows overlap are an
	// unresolvable conflict: obligation rules carry no precedence (s10).
	for code, list := range byCode {
		for a := 0; a < len(list); a++ {
			for b := a + 1; b < len(list); b++ {
				if windowsOverlap(list[a].EffectiveFrom, list[a].EffectiveTo, list[b].EffectiveFrom, list[b].EffectiveTo) {
					f.err("JUR-C088", list[a].ObligationRuleID+"|"+list[b].ObligationRuleID,
						"obligation rules %s and %s (%s) overlap in time (JUR-NEG-02)", list[a].ObligationRuleID, list[b].ObligationRuleID, code)
				}
			}
		}
	}
}

func windowsOverlap(aFrom time.Time, aTo *time.Time, bFrom time.Time, bTo *time.Time) bool {
	return (aTo == nil || aTo.After(bFrom)) && (bTo == nil || bTo.After(aFrom))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// buildCalendarObligationSections renders the sorted artifact sections.
func buildCalendarObligationSections(in CompileInput) (cals, obls []map[string]any) {
	cs := append([]CompileCalendar(nil), in.Calendars...)
	sort.Slice(cs, func(a, b int) bool { return cs[a].CalendarVersionID < cs[b].CalendarVersionID })
	cals = make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		cals = append(cals, c.moduleMap())
	}
	os := append([]CompileObligation(nil), in.Obligations...)
	sort.Slice(os, func(a, b int) bool { return os[a].ObligationRuleID < os[b].ObligationRuleID })
	obls = make([]map[string]any, 0, len(os))
	for _, o := range os {
		obls = append(obls, o.moduleMap())
	}
	return cals, obls
}
