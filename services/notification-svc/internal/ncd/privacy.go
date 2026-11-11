package ncd

import (
	"context"
	"strings"

	"zoiko.io/notification-svc/internal/privacy"
)

// S7-B1 (ZS-SVC-Y-001 NCD-02 s5.3, INV-09, INV-30, NP-17): the privacy permission a channel decision rests on is
// asked of privacy-decision-svc, for the recipient and the activity and purpose the intent is governed under. A
// caller no longer supplies it: a value in the request is ignored and the ignoring is recorded.
//
// Everything errs towards not sending. An intent that names no binding, an unreachable service, an unreadable
// answer, and a restriction this plane cannot enforce are all refusals, never permission.

// PrivacyDecider asks the privacy decision service. *privacy.Client implements it.
type PrivacyDecider interface {
	Decide(ctx context.Context, r privacy.Request) (*privacy.Decision, error)
}

// SetPrivacyDecider turns on server-side privacy decisions. Without one the plane behaves as before and reads the
// caller-supplied permission, which is development only: main.go wires it whenever privacy enforcement is on.
func (s *Service) SetPrivacyDecider(d PrivacyDecider) { s.privacyDecider = d }

// privacyResolution is the permission a decision rests on and where it came from.
type privacyResolution struct {
	Permission PermissionDecision
	Source     string // "CALLER" (no decider wired) or "PRV"
	Note       string
}

// resolvePrivacy returns the privacy permission for a channel decision.
func (s *Service) resolvePrivacy(ctx context.Context, a Actor, intent Intent, plan *RecipientPlan, supplied PermissionDecision) privacyResolution {
	if s.privacyDecider == nil {
		r := privacyResolution{Permission: supplied, Source: "CALLER"}
		if supplied.Decision == "" {
			r.Note = "no PRV decision supplied; privacy enforcement is off. Marketing and INDETERMINATE fail closed; other purposes proceed on the recorded absence"
		}
		return r
	}
	r := privacyResolution{Source: "PRV"}
	if supplied.Decision != "" || supplied.DecisionID != "" {
		r.Note = "a caller-supplied privacy permission was ignored: the decision is asked of privacy-decision-svc. "
	}
	if intent.PrivacyActivityID == "" || intent.PrivacyPurposeID == "" {
		r.Permission = PermissionDecision{Decision: "INDETERMINATE"}
		r.Note += "the intent names no privacy processing activity and purpose, so there is nothing to ask; a person-directed message is never sent on an assumption"
		return r
	}
	d, err := s.privacyDecider.Decide(ctx, privacy.Request{TenantID: a.TenantID, PrincipalID: a.PrincipalID, CorrelationID: a.CorrelationID,
		SubjectRef: plan.RecipientPrincipalID, ActivityID: intent.PrivacyActivityID, PurposeID: intent.PrivacyPurposeID})
	if err != nil || d == nil {
		r.Permission = PermissionDecision{Decision: "INDETERMINATE"}
		r.Note += "no privacy decision could be obtained; failing closed"
		return r
	}
	r.Permission = PermissionDecision{DecisionID: d.DecisionID}
	switch d.Result {
	case privacy.ResultPermit:
		r.Permission.Decision = "PERMIT"
	case privacy.ResultRestrict:
		// Decide enforces channel exclusions written NO_<CHANNEL>. Any other duty (minimise, redact, limit the
		// recipient) cannot be satisfied here, and sending anyway would pretend it was: refuse.
		var restrictions []string
		for _, c := range d.Constraints {
			t := strings.ToUpper(strings.TrimSpace(c.Type))
			if !strings.HasPrefix(t, "NO_") || !ValidChannel(strings.TrimPrefix(t, "NO_")) {
				r.Permission.Decision = "DENY"
				r.Note += "the privacy decision restricts this use with a duty notification-svc cannot enforce (" + c.Type + "); delivery withheld"
				return r
			}
			restrictions = append(restrictions, t)
		}
		r.Permission.Decision, r.Permission.Restrictions = "RESTRICT", restrictions
	case privacy.ResultBlock:
		r.Permission.Decision = "DENY"
	default: // REVIEW_REQUIRED, INDETERMINATE: a person decides, nothing is assumed
		r.Permission.Decision = "INDETERMINATE"
	}
	return r
}

// privacyDecisionID is the id of the privacy decision a fresh channel decision rests on, whether it was asked of
// privacy-decision-svc or supplied by a caller (development only).
func privacyDecisionID(d *ChannelDecision) string {
	if d == nil {
		return ""
	}
	if p, ok := d.Inputs["privacy_permission"].(PermissionDecision); ok {
		return p.DecisionID
	}
	return ""
}
