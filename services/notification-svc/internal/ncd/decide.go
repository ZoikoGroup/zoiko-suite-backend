package ncd

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DecisionInput is everything a channel decision reads. The decision is a
// pure function of it, so a stored decision's inputs replay to the same
// answer (§5.5, "decision evidence").
type DecisionInput struct {
	Intent       Intent
	Plan         RecipientPlan
	Preference   *Preference
	Suppressions []Suppression
	Bindings     []Binding
	Privacy      PermissionDecision
	Marketing    PermissionDecision
	// ResidencyRegions, when non-empty, is the set of processing regions a
	// route must be certified for (§6.3, NP-30). A GLOBAL binding does not
	// satisfy an explicit constraint: "network availability is not permission".
	ResidencyRegions []string
	Now              time.Time
}

// DecisionResult is the decision minus its identity fields.
type DecisionResult struct {
	Outcome             string
	Routes              []Route
	Restrictions        []Restriction
	ReasonCodes         []ReasonCode
	NotBefore           *time.Time
	EvidenceRequirement Level
	FallbackRules       map[string]any
	// Overrides records each convenience preference a mandatory notice
	// overrode, with the policy that permitted it (NP-15: "evidence recorded").
	Overrides []string
}

// Platform default quiet window for routine promotional traffic when the
// recipient has set none (§5.3: quiet hours are NCD + PDC + MDM time zone).
const (
	defaultQuietStart = "21:00"
	defaultQuietEnd   = "08:00"
)

// RequiredRouteEvidence is the minimum capability a route must have for an
// intent. E4 is a formal acknowledgment package: the route must support
// authenticated interaction (E3) and NCD-05 supplies the acknowledgment.
func RequiredRouteEvidence(class Level) int {
	r := class.Rank()
	if r > 3 {
		return 3
	}
	return r
}

// Decide computes the ordered eligible channel plan (§5.1 figure 4).
func Decide(in DecisionInput) DecisionResult {
	intent := in.Intent
	res := DecisionResult{
		EvidenceRequirement: intent.EvidenceClass,
		FallbackRules: map[string]any{
			"fallback_allowed":             intent.FallbackAllowed,
			"minimum_evidence":             string(intent.EvidenceClass),
			"may_lower_evidence":           false,
			"residency_regions":            in.ResidencyRegions,
			"requires_unknown_resolved":    true,
			"alternate_route_certified_in": "same failover group",
		},
	}
	block := func(code ReasonCode, detail string) DecisionResult {
		res.Outcome = DecisionBlocked
		res.Routes = nil
		res.ReasonCodes = append(res.ReasonCodes, code)
		res.Restrictions = append(res.Restrictions, Restriction{Code: code, Detail: detail})
		return res
	}

	// 1. PRV permission (§5.3). DENY blocks everything, mandatory or not
	//    (INV-09, NP-18); INDETERMINATE fails closed to review (NP-17).
	switch strings.ToUpper(in.Privacy.Decision) {
	case "DENY":
		return block(NCD008PrivacyPermissionBlocked, "PRV denied personal-data use for this purpose ("+in.Privacy.DecisionID+")")
	case "INDETERMINATE":
		res.Outcome = DecisionReviewRequired
		res.ReasonCodes = append(res.ReasonCodes, NCD008PrivacyPermissionBlocked)
		res.Restrictions = append(res.Restrictions, Restriction{Code: NCD008PrivacyPermissionBlocked,
			Detail: "PRV permission is INDETERMINATE; never assumed to be consent (NP-17)"})
		return res
	case "", "PERMIT", "RESTRICT":
	default:
		return block(NCD008PrivacyPermissionBlocked, "unrecognised PRV decision "+in.Privacy.Decision)
	}

	// 2. Marketing permission (§11.4 anti-circumvention, NP-47, INV-07/08).
	if intent.ClassifiedAsMarketing() && strings.ToUpper(in.Marketing.Decision) != "PERMIT" {
		return block(NCD009MarketingPermissionBlock,
			"the message is classified as marketing and has no current PRV/PDC marketing PERMIT")
	}

	restricted := map[string]bool{}
	if strings.ToUpper(in.Privacy.Decision) == "RESTRICT" {
		for _, r := range in.Privacy.Restrictions {
			if strings.HasPrefix(r, "NO_") {
				restricted[strings.TrimPrefix(r, "NO_")] = true
			}
		}
	}

	muted := map[string]bool{}
	var order []string
	if in.Preference != nil {
		for _, c := range in.Preference.MutedChannels {
			muted[c] = true
		}
		order = in.Preference.ChannelOrder
	}

	// Candidate channel order: the recipient's preferred order among the
	// intent's allowed channels, then the intent's own order. A preference
	// SELECTS among permitted channels; it never adds one (§5.3).
	var candidates []string
	seen := map[string]bool{}
	for _, c := range order {
		if intent.Allows(c) && !seen[c] {
			candidates = append(candidates, c)
			seen[c] = true
		}
	}
	for _, c := range intent.AllowedChannels {
		if !seen[c] {
			candidates = append(candidates, c)
			seen[c] = true
		}
	}

	endpointFor := map[string]Endpoint{}
	for _, e := range in.Plan.Endpoints {
		if _, ok := endpointFor[e.Channel]; !ok {
			endpointFor[e.Channel] = e
		}
	}

	protected := intent.Mandatory || intent.PurposeClass == PurposeRegulated || intent.PurposeClass == PurposeSecurityCritical
	var deferUntil *time.Time
	required := RequiredRouteEvidence(intent.EvidenceClass)

	for _, ch := range candidates {
		restrict := func(code ReasonCode, hash, detail string) {
			res.Restrictions = append(res.Restrictions, Restriction{Channel: ch, EndpointHash: hash, Code: code, Detail: detail})
		}
		if restricted[ch] {
			restrict(NCD008PrivacyPermissionBlocked, "", "PRV RESTRICT excludes "+ch)
			continue
		}
		ep, ok := endpointFor[ch]
		if !ok {
			restrict(NCD006RecipientUnresolved, "", "no resolved "+ch+" endpoint for the recipient")
			continue
		}
		if !ep.Verified && protected {
			restrict(NCD007EndpointUnverified, ep.EndpointHash,
				"a mandatory, regulated or security communication needs a verified endpoint; this one's provenance is "+ep.Provenance)
			continue
		}

		// 3. Suppression precedence (§5.4). Checked per endpoint AND per
		//    subject, for this channel and purpose.
		v := SuppressionVerdict(intent, in.Plan.RecipientPrincipalID, ch, ep.EndpointHash, in.Suppressions, in.Now)
		res.Overrides = append(res.Overrides, v.Overrides...)
		if v.Restriction != nil {
			res.Restrictions = append(res.Restrictions, *v.Restriction)
			if v.DeferUntil != nil && (deferUntil == nil || v.DeferUntil.After(*deferUntil)) {
				deferUntil = v.DeferUntil
			}
			continue
		}
		overridden := v.Overridden
		if muted[ch] && !overridden {
			if intent.Mandatory && intent.PreferenceOverrideAllowed {
				res.Overrides = append(res.Overrides, fmt.Sprintf("preference mute of %s overridden: intent %s/%d is mandatory with preference_override_allowed",
					ch, intent.IntentID, intent.Version))
			} else {
				restrict(NCD010ChannelSuppressed, ep.EndpointHash, "recipient's preference mutes "+ch+" for non-mandatory communications (NP-16)")
				continue
			}
		}

		// 4. Provider route (§6.3): certified, active, healthy, residency
		//    compliant, evidence capable. Cost orders only among compliant.
		b, code, why := selectBinding(in.Bindings, ch, required, in.ResidencyRegions)
		if b == nil {
			restrict(code, ep.EndpointHash, why)
			continue
		}
		res.Routes = append(res.Routes, Route{
			Channel: ch, BindingID: b.BindingID, EvidenceCapability: b.EvidenceCapability,
			FailoverGroup: b.FailoverGroup, EndpointHash: ep.EndpointHash, EndpointMasked: ep.EndpointMasked,
			Provenance: ep.Provenance, Address: ep.Address,
		})
	}

	if len(res.Routes) == 0 {
		if deferUntil != nil {
			res.Outcome = DecisionDeferred
			res.NotBefore = deferUntil
			res.ReasonCodes = append(res.ReasonCodes, NCD010ChannelSuppressed)
			return res
		}
		res.Outcome = DecisionBlocked
		code := NCD011NoCompliantChannel
		if len(res.Restrictions) > 0 && allSame(res.Restrictions) {
			code = res.Restrictions[0].Code
		}
		res.ReasonCodes = append(res.ReasonCodes, code)
		return res
	}
	if !intent.FallbackAllowed && len(res.Routes) > 1 {
		res.Routes = res.Routes[:1]
	}

	// 5. Quiet hours in the recipient's civil time (§5.3, INV-23).
	if nb, review := quietHours(in, intent); review != "" {
		res.Outcome = DecisionReviewRequired
		res.ReasonCodes = append(res.ReasonCodes, NCD012QuietHourDeferred)
		res.Restrictions = append(res.Restrictions, Restriction{Code: NCD012QuietHourDeferred, Detail: review})
		return res
	} else if nb != nil {
		res.Outcome = DecisionDeferred
		res.NotBefore = nb
		res.ReasonCodes = append(res.ReasonCodes, NCD012QuietHourDeferred)
		return res
	}

	res.Outcome = DecisionPermitted
	return res
}

func allSame(rs []Restriction) bool {
	for _, r := range rs[1:] {
		if r.Code != rs[0].Code {
			return false
		}
	}
	return true
}

// selectBinding picks the best compliant binding for a channel.
func selectBinding(bindings []Binding, channel string, requiredEvidence int, regions []string) (*Binding, ReasonCode, string) {
	var ok []Binding
	var reasons []string
	lowEvidence := false
	for _, b := range bindings {
		if b.Channel != channel {
			continue
		}
		switch {
		case b.Status != "ACTIVE":
			reasons = append(reasons, b.BindingID+" disabled")
		case !b.Certified:
			reasons = append(reasons, b.BindingID+" not certified")
		case b.Health == "CIRCUIT_OPEN":
			reasons = append(reasons, b.BindingID+" circuit open: "+b.HealthReason)
		case b.EvidenceCapability.Rank() < requiredEvidence:
			lowEvidence = true
			reasons = append(reasons, fmt.Sprintf("%s evidence capability %s is below the required E%d", b.BindingID, b.EvidenceCapability, requiredEvidence))
		case !residencyOK(b.Regions, regions):
			reasons = append(reasons, fmt.Sprintf("%s processes in %v, outside the residency constraint %v", b.BindingID, b.Regions, regions))
		default:
			ok = append(ok, b)
		}
	}
	if len(ok) == 0 {
		if len(reasons) == 0 {
			return nil, NCD013ProviderRouteUnavailable, "no provider binding exists for " + channel
		}
		code := NCD013ProviderRouteUnavailable
		if lowEvidence && len(reasons) == 1 {
			code = NCD016EvidenceInsufficient
		}
		return nil, code, strings.Join(reasons, "; ")
	}
	sort.SliceStable(ok, func(i, j int) bool {
		if ok[i].Priority != ok[j].Priority {
			return ok[i].Priority < ok[j].Priority
		}
		return ok[i].CostRank < ok[j].CostRank
	})
	b := ok[0]
	return &b, "", ""
}

func residencyOK(bindingRegions, required []string) bool {
	if len(required) == 0 {
		return true
	}
	for _, r := range required {
		for _, b := range bindingRegions {
			if strings.EqualFold(r, b) {
				return true
			}
		}
	}
	return false
}

// quietHours returns a deferral time, or a review reason when the rule
// applies but the recipient's civil time is unknown (NP-20: do not guess).
func quietHours(in DecisionInput, intent Intent) (*time.Time, string) {
	if intent.QuietHoursPolicy == "EXEMPT" || intent.PurposeClass == PurposeSecurityCritical ||
		intent.Urgency.Rank() >= 3 || (intent.Mandatory && intent.Urgency.Rank() >= 2) {
		return nil, ""
	}
	start, end, zone := "", "", in.Plan.TimeZone
	if in.Preference != nil && in.Preference.QuietHoursStart != "" {
		start, end = in.Preference.QuietHoursStart, in.Preference.QuietHoursEnd
		if in.Preference.TimeZone != "" {
			zone = in.Preference.TimeZone
		}
	} else if intent.PurposeClass == PurposeMarketing {
		start, end = defaultQuietStart, defaultQuietEnd
	}
	if start == "" {
		return nil, ""
	}
	if zone == "" {
		return nil, "quiet hours apply to this routine message and the recipient's time zone is unknown; not guessed (NP-20)"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, "recipient time zone " + zone + " is not a known IANA zone"
	}
	return QuietWindowEnd(in.Now, loc, start, end), ""
}

// QuietWindowEnd returns the end of the quiet window containing now, in UTC,
// or nil when now is outside it. Computed with the zone's own rules, so a DST
// transition moves the instant rather than the local clock (TC-08).
func QuietWindowEnd(now time.Time, loc *time.Location, start, end string) *time.Time {
	sh, sm, ok1 := parseHHMM(start)
	eh, em, ok2 := parseHHMM(end)
	if !ok1 || !ok2 {
		return nil
	}
	local := now.In(loc)
	mins := local.Hour()*60 + local.Minute()
	s, e := sh*60+sm, eh*60+em
	var inWindow bool
	if s <= e {
		inWindow = mins >= s && mins < e
	} else {
		inWindow = mins >= s || mins < e
	}
	if !inWindow {
		return nil
	}
	day := local
	if s > e && mins >= s {
		day = local.AddDate(0, 0, 1)
	}
	out := time.Date(day.Year(), day.Month(), day.Day(), eh, em, 0, 0, loc).UTC()
	return &out
}

func parseHHMM(s string) (int, int, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

// Verdict is the suppression outcome for one route.
type Verdict struct {
	Restriction *Restriction
	DeferUntil  *time.Time
	Overridden  bool
	Overrides   []string
}

// SuppressionVerdict applies §5.4 precedence to one channel/endpoint. It is
// the same function the channel decision and the final submit gate call
// (INV-24: suppression is rechecked immediately before submission).
func SuppressionVerdict(intent Intent, recipientPrincipalID, channel, endpointHash string, sups []Suppression, now time.Time) Verdict {
	var v Verdict
	for _, s := range sups {
		if !s.ActiveAt(now) || !s.Covers(channel, intent.PurposeClass) {
			continue
		}
		if s.EndpointHash != "" && s.EndpointHash != endpointHash {
			continue
		}
		if s.EndpointHash == "" && s.SubjectPrincipalID != "" && s.SubjectPrincipalID != recipientPrincipalID {
			continue
		}
		switch s.Reason {
		case SuppChannelMute:
			if intent.Mandatory && intent.PreferenceOverrideAllowed {
				v.Overridden = true
				v.Overrides = append(v.Overrides, fmt.Sprintf("CHANNEL_MUTE on %s overridden: intent %s/%d is mandatory with preference_override_allowed",
					channel, intent.IntentID, intent.Version))
				continue
			}
			v.Restriction = &Restriction{Channel: channel, EndpointHash: endpointHash, Code: NCD010ChannelSuppressed,
				Detail: "recipient muted " + channel + " (convenience suppression)"}
		case SuppSoftBounce:
			until := now.Add(time.Hour)
			if s.EffectiveUntil != nil {
				until = *s.EffectiveUntil
			}
			v.DeferUntil = &until
			v.Restriction = &Restriction{Channel: channel, EndpointHash: endpointHash, Code: NCD010ChannelSuppressed,
				Detail: "temporary soft bounce; backing off until " + until.Format(time.RFC3339)}
		default:
			// HARD_BOUNCE, ENDPOINT_INVALID, COMPLAINT_ABUSE (marketing
			// scoped), MARKETING_OPTOUT (marketing scoped), SECURITY_HOLD and
			// LEGAL_RESTRICTION. None is a convenience preference, so a
			// mandatory notice cannot override any of them (INV-09).
			v.Restriction = &Restriction{Channel: channel, EndpointHash: endpointHash, Code: NCD010ChannelSuppressed,
				Detail: fmt.Sprintf("%s suppression (%s, purpose scope %s) from %s", s.Reason, s.ChannelScope, s.PurposeScope, s.Source)}
		}
		if v.Restriction != nil {
			return v
		}
	}
	return v
}
