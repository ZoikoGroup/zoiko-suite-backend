package resolver

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 Wave 4 (calendar half): runtime obligation due-date calculation.
// It uses the same pack selection, verification and fail-closed rules as rule
// resolution (see pick), and answers from the signed artifact alone.

// DueRequest asks for the due date of an obligation.
type DueRequest struct {
	Jurisdiction   string
	ObligationCode string
	Facts          domain.DueFacts
	PackRef        string
	PackVersion    string
}

// DueDecision is the explainable answer: which pack release, which obligation
// rule version and calendar version, the sources behind it, and every step.
type DueDecision struct {
	Outcome          string            `json:"outcome"`
	Jurisdiction     string            `json:"jurisdiction"`
	ObligationCode   string            `json:"obligation_code"`
	Pack             *PackInfo         `json:"pack,omitempty"`
	Due              *domain.DueResult `json:"calculation,omitempty"`
	InterpretationID *string           `json:"interpretation_id,omitempty"`
	Sources          []SourceInfo      `json:"sources,omitempty"`
	PacksConsulted   []PackConsulted   `json:"packs_consulted"`
	Explanation      []string          `json:"explanation"`
}

// dueRank orders the non-calculated outcomes so the most informative one wins
// when several packs answer differently.
var dueRank = map[string]int{
	domain.DueExtensionNotAllowed: 1, domain.DueInvalidFacts: 2, domain.DueNoCalendar: 3, domain.DueAmbiguous: 4, domain.DueNoRule: 5,
}

// CalculateDue computes an obligation due date from verified released packs.
func (r *Resolver) CalculateDue(ctx context.Context, req DueRequest) (*DueDecision, error) {
	if strings.TrimSpace(req.Jurisdiction) == "" || strings.TrimSpace(req.ObligationCode) == "" {
		return nil, fmt.Errorf("%w: jurisdiction and obligation_code are required", ErrBadRequest)
	}
	if req.PackVersion != "" && req.PackRef == "" {
		return nil, fmt.Errorf("%w: pack_version needs pack_ref", ErrBadRequest)
	}
	at, ok := domain.SelectionDate(req.Facts)
	if !ok {
		return nil, fmt.Errorf("%w: one anchor fact (period_end, event_date, registration_date, or anniversary_date with anchor_year) is required", ErrBadRequest)
	}
	d := &DueDecision{Jurisdiction: req.Jurisdiction, ObligationCode: req.ObligationCode, PacksConsulted: []PackConsulted{}, Explanation: []string{}}
	step := func(format string, a ...any) { d.Explanation = append(d.Explanation, fmt.Sprintf(format, a...)) }

	entries, early, err := r.pick(ctx, req.Jurisdiction, req.PackRef, req.PackVersion, at, step)
	if err != nil {
		return nil, err
	}
	if early != "" {
		// pick speaks in rule-resolution terms; an obligation has its own name for "nothing applies".
		if early == domain.OutcomeNoRule {
			early = domain.DueNoRule
		}
		d.Outcome = early
		return d, nil
	}

	type hit struct {
		e   *entry
		res domain.DueResult
	}
	var calculated, others []hit
	for _, e := range entries {
		res := e.doc.CalculateDue(req.Jurisdiction, req.ObligationCode, req.Facts)
		d.PacksConsulted = append(d.PacksConsulted, PackConsulted{PackRef: e.pack.PackRef, Version: e.pack.Version, Outcome: res.Outcome, RuleID: res.ObligationRuleID})
		step("pack %s@%s: %s", e.pack.PackRef, e.pack.Version, res.Outcome)
		if res.Outcome == domain.DueCalculated {
			calculated = append(calculated, hit{e, res})
		} else {
			others = append(others, hit{e, res})
		}
	}

	var chosen *hit
	switch {
	case len(calculated) > 1:
		// Two packs both define this obligation for this jurisdiction and date.
		d.Outcome = domain.DueAmbiguous
		step("%d packs each define obligation %s for %s: cross-pack conflict, blocked", len(calculated), req.ObligationCode, req.Jurisdiction)
		return d, nil
	case len(calculated) == 1:
		chosen = &calculated[0]
	case len(others) > 0:
		sort.SliceStable(others, func(a, b int) bool {
			ra, rb := dueRank[others[a].res.Outcome], dueRank[others[b].res.Outcome]
			if ra == 0 {
				ra = 9
			}
			if rb == 0 {
				rb = 9
			}
			return ra < rb
		})
		chosen = &others[0]
	}
	if chosen == nil {
		d.Outcome = domain.DueNoRule
		return d, nil
	}

	d.Outcome = chosen.res.Outcome
	res := chosen.res
	d.Due = &res
	if res.ObligationRuleID != "" {
		pack := chosen.e.pack
		d.Pack = &pack
		for _, o := range chosen.e.doc.Obligations {
			if o.ObligationRuleID != res.ObligationRuleID {
				continue
			}
			d.InterpretationID = o.InterpretationID
			for _, sid := range o.SourceIDs {
				if s, ok := chosen.e.doc.Sources[sid]; ok {
					d.Sources = append(d.Sources, SourceInfo{SourceID: s.SourceID, Authority: s.Authority, SourceType: s.SourceType,
						AuthorityLevel: s.AuthorityLevel, Title: s.Title, SnapshotHash: s.SnapshotHash})
				}
			}
			sort.Slice(d.Sources, func(a, b int) bool { return d.Sources[a].SourceID < d.Sources[b].SourceID })
		}
	}
	d.Explanation = append(d.Explanation, res.Explanation...)
	return d, nil
}
