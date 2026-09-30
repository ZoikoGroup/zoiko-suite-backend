package ncd

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/notification-svc/internal/domain"
)

// ── NCD-02 recipient resolution (§5.1, §5.2) ────────────────────────────────

// RecipientInput is POST /v1/recipient-resolution.
type RecipientInput struct {
	IntentID             string            `json:"intent_id"`
	RecipientPrincipalID string            `json:"recipient_principal_id"`
	RecipientTenantID    string            `json:"recipient_tenant_id,omitempty"`
	FreeTextEndpoint     *FreeTextEndpoint `json:"free_text_endpoint,omitempty"`
	Locale               string            `json:"locale,omitempty"`
}

// resolveEndpoints asks the contact authority for the recipient's routes.
// Network I/O, so it runs OUTSIDE any database transaction.
func (s *Service) resolveEndpoints(ctx context.Context, a Actor, intent Intent, in RecipientInput) ([]Endpoint, map[string]any, error) {
	now := s.now()
	evidence := map[string]any{"resolved_at": now, "caller": a.PrincipalID}

	// NP-12 / INV-16: a recipient in another tenant is refused. There is no
	// external-relationship registry (MDM) to authorize one, so none is.
	if in.RecipientTenantID != "" && in.RecipientTenantID != a.TenantID {
		return nil, evidence, Refused(Refuse(NCD020CrossTenantRecipientBlock,
			"the recipient belongs to another tenant and no authorized external-party relationship exists; tenant ids alone never infer one"))
	}
	if in.RecipientPrincipalID == "" {
		return nil, evidence, Invalid("missing_fields", "recipient_principal_id is required")
	}

	protected := intent.Mandatory || intent.PurposeClass == PurposeRegulated || intent.PurposeClass == PurposeSecurityCritical
	var eps []Endpoint

	// Free-text endpoints (§5.2): prohibited for regulated/legal notices
	// unless a controlled exception captures authority, verification and a
	// reviewer (NP-11). The reviewer is a different principal from the caller.
	var free *Endpoint
	if f := in.FreeTextEndpoint; f != nil {
		if f.Channel != ChannelEmail {
			return nil, evidence, Invalid("unsupported_free_text_channel", "only an EMAIL free-text endpoint is supported")
		}
		addr, err := mail.ParseAddress(f.Address)
		if err != nil {
			return nil, evidence, Invalid("invalid_endpoint", "free_text_endpoint.address is not an email address")
		}
		ep := Endpoint{Channel: ChannelEmail, Address: addr.Address, ResolvedAt: now}
		if protected {
			if f.ExceptionRef == "" || f.VerificationRef == "" || f.ReviewerPrincipalID == "" {
				return nil, evidence, Refused(Refuse(NCD007EndpointUnverified,
					"a free-text endpoint for a mandatory, regulated or security communication needs exception_ref, verification_ref and reviewer_principal_id (NP-11)"))
			}
			if f.ReviewerPrincipalID == a.PrincipalID {
				return nil, evidence, Forbidden("self_review_forbidden", "the reviewer of a controlled endpoint exception cannot be the caller")
			}
			ep.Provenance, ep.Verified = ProvenanceControlledInput, true
			ep.ProvenanceRef = "exception:" + f.ExceptionRef + ";verification:" + f.VerificationRef + ";reviewer:" + f.ReviewerPrincipalID
		} else {
			ep.Provenance, ep.Verified, ep.ProvenanceRef = ProvenanceRequest, false, "caller:"+a.PrincipalID
		}
		free = &ep
	}

	// The IAM contact authority: identity-context-svc. It also establishes
	// that the principal exists at all, which is what makes the in-app inbox
	// a verified route.
	principalExists := false
	var identityEmail string
	if s.recipient != nil {
		addr, err := s.recipient.ResolveEmail(ctx, a.TenantID, a.PrincipalID, in.RecipientPrincipalID)
		switch {
		case err == nil:
			principalExists, identityEmail = true, addr
		case errors.Is(err, domain.ErrPrincipalHasNoAddress):
			principalExists = true
			evidence["email"] = "principal has no email on record"
		case errors.Is(err, domain.ErrPrincipalNotFound):
			return nil, evidence, Refused(Refuse(NCD006RecipientUnresolved,
				"identity-context-svc has no principal "+in.RecipientPrincipalID+" in this tenant"))
		default:
			return nil, evidence, Unavailable("identity_unavailable", "recipient resolution could not reach the contact authority: %v", err)
		}
	} else {
		evidence["identity"] = "no contact authority configured"
	}
	evidence["principal_verified"] = principalExists

	for _, ch := range intent.AllowedChannels {
		switch ch {
		case ChannelEmail:
			if free != nil {
				eps = append(eps, *free)
			} else if identityEmail != "" {
				eps = append(eps, Endpoint{Channel: ChannelEmail, Address: identityEmail, Provenance: ProvenanceIdentityContext,
					ProvenanceRef: "identity-context-svc:principal/" + in.RecipientPrincipalID, Verified: true, ResolvedAt: now})
			}
		case ChannelInApp:
			eps = append(eps, Endpoint{Channel: ChannelInApp, Address: in.RecipientPrincipalID, Provenance: ProvenancePlatformInbox,
				ProvenanceRef: "tenant:" + a.TenantID + "/principal:" + in.RecipientPrincipalID, Verified: principalExists, ResolvedAt: now})
		}
		// SMS and PUSH: no contact master holds phone numbers or device
		// tokens yet (OD-03/OD-04), so no endpoint resolves.
	}
	for i := range eps {
		eps[i].EndpointHash = EndpointHash(eps[i].Channel, eps[i].Address)
		eps[i].EndpointMasked = MaskEndpoint(eps[i].Channel, eps[i].Address)
	}
	if len(eps) == 0 {
		return nil, evidence, Refused(Refuse(NCD006RecipientUnresolved, "no endpoint resolved for any channel the intent allows"))
	}
	return eps, evidence, nil
}

// buildPlan resolves and assembles (not yet persists) a recipient plan.
func (s *Service) buildPlan(ctx context.Context, a Actor, intent Intent, in RecipientInput, pref *Preference) (*RecipientPlan, error) {
	eps, ev, err := s.resolveEndpoints(ctx, a, intent, in)
	if err != nil {
		return nil, err
	}
	p := &RecipientPlan{
		PlanID: uuid.NewString(), LegalEntityID: intent.LegalEntityID, IntentID: intent.IntentID,
		IntentVersion: intent.Version, RecipientPrincipalID: in.RecipientPrincipalID, Endpoints: eps,
		Locale: in.Locale, DecisionEvidence: ev, CreatedByPrincipalID: a.PrincipalID, CreatedAt: s.now(),
	}
	if pref != nil {
		if pref.TimeZone != "" {
			// The recipient's own governed profile; never inferred (NP-20).
			p.TimeZone, p.TimeZoneSource = pref.TimeZone, "RECIPIENT_PREFERENCE"
		}
		if p.Locale == "" {
			p.Locale = pref.Locale
		}
	}
	return p, nil
}

// effectiveIntentNow resolves the intent version in force now (NCD-001/002).
func effectiveIntentNow(tx Tx, intentID string, now time.Time) (*Intent, error) {
	i, err := tx.EffectiveIntent(intentID, now, now)
	if errors.Is(err, ErrNotFound) {
		if _, e2 := tx.GetIntent(intentID, 0); e2 != nil {
			return nil, Refused(Refuse(NCD001IntentNotFound, "no intent "+intentID))
		}
		return nil, Refused(Refuse(NCD002IntentNotEffective, "intent "+intentID+" has no version effective now"))
	}
	return i, err
}

func (s *Service) loadIntentAndPreference(ctx context.Context, a Actor, intentID, recipient string) (*Intent, *Preference, error) {
	var intent *Intent
	var pref *Preference
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		if intent, err = effectiveIntentNow(tx, intentID, s.now()); err != nil {
			return err
		}
		pref, err = tx.GetPreference(recipient)
		return err
	})
	return intent, pref, err
}

// ResolveRecipient is POST /v1/recipient-resolution.
func (s *Service) ResolveRecipient(ctx context.Context, a Actor, in RecipientInput) (*RecipientPlan, error) {
	if in.IntentID == "" {
		return nil, Invalid("missing_fields", "intent_id is required")
	}
	intent, pref, err := s.loadIntentAndPreference(ctx, a, in.IntentID, in.RecipientPrincipalID)
	if err != nil {
		return nil, err
	}
	p, err := s.buildPlan(ctx, a, *intent, in, pref)
	if err != nil {
		return nil, err
	}
	if err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error { return tx.InsertPlan(p) }); err != nil {
		return nil, err
	}
	p.TenantID = a.TenantID
	return p, nil
}

// DecisionRequest is POST /v1/channel-decision.
type DecisionRequest struct {
	RecipientPlanID     string             `json:"recipient_plan_id"`
	PrivacyPermission   PermissionDecision `json:"privacy_permission"`
	MarketingPermission PermissionDecision `json:"marketing_permission"`
	ResidencyRegions    []string           `json:"residency_regions,omitempty"`
}

// decide runs Decide with fresh suppressions, preference and bindings, and
// persists the decision on tx.
func (s *Service) decide(ctx context.Context, tx Tx, a Actor, intent Intent, plan *RecipientPlan, privacy, marketing PermissionDecision,
	regions []string, communicationID string) (*ChannelDecision, error) {
	now := s.now()
	pref, err := tx.GetPreference(plan.RecipientPrincipalID)
	if err != nil {
		return nil, err
	}
	sups, err := activeSuppressionsFor(tx, plan, now)
	if err != nil {
		return nil, err
	}
	bindings, err := s.store.Bindings(ctx, now)
	if err != nil {
		return nil, err
	}
	res := Decide(DecisionInput{Intent: intent, Plan: *plan, Preference: pref, Suppressions: sups, Bindings: bindings,
		Privacy: privacy, Marketing: marketing, ResidencyRegions: regions, Now: now})
	inputs := map[string]any{
		"privacy_permission":      privacy,
		"marketing_permission":    marketing,
		"residency_regions":       regions,
		"suppressions_considered": len(sups),
		"preference_version":      0,
		"overrides":               res.Overrides,
	}
	if privacy.Decision == "" {
		inputs["privacy_permission_note"] = "no PRV decision supplied; PRV is not deployed. Marketing and INDETERMINATE fail closed; other purposes proceed on the recorded absence"
	}
	if pref != nil {
		inputs["preference_version"] = pref.Version
	}
	d := &ChannelDecision{
		DecisionID: uuid.NewString(), TenantID: a.TenantID, PlanID: plan.PlanID, IntentID: intent.IntentID,
		IntentVersion: intent.Version, CommunicationID: communicationID, Outcome: res.Outcome,
		Routes: nonNilRoutes(res.Routes), Restrictions: nonNilRestrictions(res.Restrictions),
		ReasonCodes: nonNilCodes(res.ReasonCodes), NotBefore: res.NotBefore, EvidenceRequirement: res.EvidenceRequirement,
		FallbackRules: res.FallbackRules, Inputs: inputs, DecidedBy: a.PrincipalID, DecidedAt: now,
	}
	if err := tx.InsertDecision(d); err != nil {
		return nil, err
	}
	return d, nil
}

func activeSuppressionsFor(tx Tx, plan *RecipientPlan, now time.Time) ([]Suppression, error) {
	var hashes, emails []string
	for _, e := range plan.Endpoints {
		hashes = append(hashes, e.EndpointHash)
		if e.Channel == ChannelEmail {
			emails = append(emails, NormalizeEndpoint(ChannelEmail, e.Address))
		}
	}
	return tx.ActiveSuppressions(plan.RecipientPrincipalID, hashes, emails, now)
}

func nonNilRoutes(v []Route) []Route {
	if v == nil {
		return []Route{}
	}
	return v
}
func nonNilRestrictions(v []Restriction) []Restriction {
	if v == nil {
		return []Restriction{}
	}
	return v
}
func nonNilCodes(v []ReasonCode) []ReasonCode {
	if v == nil {
		return []ReasonCode{}
	}
	return v
}

// ChannelDecision is POST /v1/channel-decision.
func (s *Service) ChannelDecision(ctx context.Context, a Actor, in DecisionRequest) (*ChannelDecision, error) {
	if in.RecipientPlanID == "" {
		return nil, Invalid("missing_fields", "recipient_plan_id is required")
	}
	var out *ChannelDecision
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		plan, err := tx.GetPlan(in.RecipientPlanID)
		if err != nil {
			return mapNotFound(err, "plan_not_found", "recipient plan %s", in.RecipientPlanID)
		}
		intent, err := tx.GetIntent(plan.IntentID, plan.IntentVersion)
		if err != nil {
			return err
		}
		out, err = s.decide(ctx, tx, a, *intent, plan, in.PrivacyPermission, in.MarketingPermission, in.ResidencyRegions, "")
		return err
	})
	return out, err
}

// ── preferences (§5.3) ──────────────────────────────────────────────────────

// PreferenceInput is POST /v1/preferences. It has no consent field and cannot
// acquire one: the decoder refuses unknown fields, so "marketing_consent" or
// "disable_unsubscribe" is a 400, not a silently ignored key (§5.5, NP-60).
type PreferenceInput struct {
	MutedChannels   []string `json:"muted_channels"`
	ChannelOrder    []string `json:"channel_order"`
	QuietHoursStart string   `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd   string   `json:"quiet_hours_end,omitempty"`
	TimeZone        string   `json:"time_zone,omitempty"`
	Locale          string   `json:"locale,omitempty"`
	ExpectedVersion int      `json:"expected_version"`
}

// SetPreference is a governed change to the caller's OWN preference profile.
func (s *Service) SetPreference(ctx context.Context, a Actor, in PreferenceInput) (*Preference, error) {
	for _, c := range append(append([]string{}, in.MutedChannels...), in.ChannelOrder...) {
		if !ValidChannel(c) {
			return nil, Invalid("invalid_channel", "channel %q is not EMAIL, IN_APP, SMS or PUSH", c)
		}
	}
	if (in.QuietHoursStart == "") != (in.QuietHoursEnd == "") {
		return nil, Invalid("invalid_quiet_hours", "quiet_hours_start and quiet_hours_end are set together")
	}
	if in.QuietHoursStart != "" {
		if _, _, ok := parseHHMM(in.QuietHoursStart); !ok {
			return nil, Invalid("invalid_quiet_hours", "quiet_hours_start must be HH:MM")
		}
		if _, _, ok := parseHHMM(in.QuietHoursEnd); !ok {
			return nil, Invalid("invalid_quiet_hours", "quiet_hours_end must be HH:MM")
		}
		if in.TimeZone == "" {
			return nil, Invalid("time_zone_required", "quiet hours are civil time and need an IANA time_zone (INV-23)")
		}
	}
	if in.TimeZone != "" {
		if _, err := time.LoadLocation(in.TimeZone); err != nil || !strings.Contains(in.TimeZone, "/") && in.TimeZone != "UTC" {
			return nil, Invalid("invalid_time_zone", "time_zone %q is not an IANA zone such as Europe/London", in.TimeZone)
		}
	}
	if in.Locale != "" && !ValidLocale(in.Locale) {
		return nil, Invalid("invalid_locale", "locale %q is not a locale tag", in.Locale)
	}
	p := &Preference{
		TenantID: a.TenantID, PrincipalID: a.PrincipalID, MutedChannels: nonNilStrings(in.MutedChannels),
		ChannelOrder: nonNilStrings(in.ChannelOrder), QuietHoursStart: in.QuietHoursStart, QuietHoursEnd: in.QuietHoursEnd,
		TimeZone: in.TimeZone, Locale: in.Locale, UpdatedAt: s.now(), UpdatedBy: a.PrincipalID,
	}
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		err := tx.SavePreference(p, in.ExpectedVersion)
		if errors.Is(err, ErrStaleVersion) {
			return Conflict("stale_preference_version", "expected_version %d is not the current version", in.ExpectedVersion)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// GetPreference returns the caller's profile (an empty one if never set).
func (s *Service) GetPreference(ctx context.Context, a Actor) (*Preference, error) {
	var p *Preference
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		p, err = tx.GetPreference(a.PrincipalID)
		return err
	})
	if err == nil && p == nil {
		p = &Preference{TenantID: a.TenantID, PrincipalID: a.PrincipalID, MutedChannels: []string{}, ChannelOrder: []string{}}
	}
	return p, err
}

// ── suppressions (§5.4, §7.3, §10.1 POST /suppressions) ─────────────────────

// SuppressionInput is POST /v1/suppressions.
type SuppressionInput struct {
	SubjectPrincipalID string `json:"subject_principal_id,omitempty"`
	Endpoint           *struct {
		Channel string `json:"channel"`
		Address string `json:"address"`
	} `json:"endpoint,omitempty"`
	ChannelScope      string     `json:"channel_scope"`
	PurposeScope      string     `json:"purpose_scope"`
	Reason            string     `json:"reason"`
	Source            string     `json:"source"`
	SourceEvidenceRef string     `json:"source_evidence_ref"`
	EffectiveUntil    *time.Time `json:"effective_until,omitempty"`
}

// SelfServiceSuppression reports whether a caller may record this suppression
// about themselves without the operator grant: opting out of marketing or
// muting a channel is the recipient's own right (NP-46 — the direct endpoint
// writes canonical suppression durably; provider sync is secondary).
func (in SuppressionInput) SelfServiceSuppression(caller string) bool {
	return in.SubjectPrincipalID == caller && in.Endpoint == nil &&
		(in.Reason == SuppMarketingOptOut || in.Reason == SuppChannelMute)
}

// AddSuppression records one canonical suppression, idempotently.
func (s *Service) AddSuppression(ctx context.Context, a Actor, in SuppressionInput) (*Suppression, bool, error) {
	if in.ChannelScope == "" {
		in.ChannelScope = "ALL"
	}
	if in.PurposeScope == "" {
		in.PurposeScope = "ALL"
	}
	if in.Reason == SuppMarketingOptOut {
		in.PurposeScope = string(PurposeMarketing)
	}
	switch {
	case !ValidSuppressionReason(in.Reason):
		return nil, false, Invalid("invalid_reason", "reason %q is not a §5.4 suppression", in.Reason)
	case in.ChannelScope != "ALL" && !ValidChannel(in.ChannelScope):
		return nil, false, Invalid("invalid_scope", "channel_scope must be ALL or a channel")
	case in.PurposeScope != "ALL" && !PurposeClass(in.PurposeScope).Valid():
		return nil, false, Invalid("invalid_scope", "purpose_scope must be ALL or a purpose class")
	case in.Source == "" || strings.TrimSpace(in.SourceEvidenceRef) == "":
		return nil, false, Invalid("missing_fields", "source and source_evidence_ref are mandatory (§10.1)")
	case in.SubjectPrincipalID == "" && in.Endpoint == nil:
		return nil, false, Invalid("missing_fields", "a suppression names subject_principal_id or an endpoint")
	case in.Reason == SuppChannelMute && in.ChannelScope == "ALL":
		return nil, false, Invalid("invalid_scope", "a channel mute names the channel it mutes")
	}
	sup := &Suppression{
		SuppressionID: uuid.NewString(), TenantID: a.TenantID, SubjectPrincipalID: in.SubjectPrincipalID,
		ChannelScope: in.ChannelScope, PurposeScope: in.PurposeScope, Reason: in.Reason, Source: in.Source,
		SourceEvidenceRef: in.SourceEvidenceRef, EffectiveFrom: s.now(), EffectiveUntil: in.EffectiveUntil,
		CreatedByPrincipal: a.PrincipalID, CreatedAt: s.now(),
	}
	if in.Endpoint != nil {
		if !ValidChannel(in.Endpoint.Channel) || strings.TrimSpace(in.Endpoint.Address) == "" {
			return nil, false, Invalid("invalid_endpoint", "endpoint needs a channel and an address")
		}
		sup.EndpointHash = EndpointHash(in.Endpoint.Channel, in.Endpoint.Address)
		sup.EndpointMasked = MaskEndpoint(in.Endpoint.Channel, in.Endpoint.Address)
	}
	var created bool
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		created, err = tx.InsertSuppression(sup)
		if err != nil {
			return err
		}
		if created {
			return emitSuppressed(tx, a, "", sup)
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return sup, created, nil
}

func emitSuppressed(tx Tx, a Actor, legalEntityID string, s *Suppression) error {
	ref := s.EndpointHash
	if ref == "" {
		ref = "principal:" + s.SubjectPrincipalID
	}
	return emit(tx, a, EvtEndpointSupp, legalEntityID, ref, map[string]any{
		"suppression_id": s.SuppressionID, "endpoint_ref": ref, "subject_principal_id": s.SubjectPrincipalID,
		"scope":  map[string]string{"channel": s.ChannelScope, "purpose": s.PurposeScope},
		"reason": s.Reason, "source": s.Source, "source_evidence": s.SourceEvidenceRef, "effective_at": s.EffectiveFrom,
	})
}

// SuppressionQuery is GET /v1/suppressions.
type SuppressionQuery struct {
	PrincipalID string
	Channel     string
	Address     string
	Purpose     string
	ActiveOnly  bool
	Limit       int
}

// ListSuppressions returns purpose/channel-scoped effective suppression facts.
func (s *Service) ListSuppressions(ctx context.Context, a Actor, q SuppressionQuery) ([]Suppression, error) {
	hash := ""
	if q.Address != "" {
		ch := q.Channel
		if ch == "" {
			ch = ChannelEmail
		}
		hash = EndpointHash(ch, q.Address)
	}
	var out []Suppression
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		all, err := tx.ListSuppressions(q.PrincipalID, hash, q.ActiveOnly, q.Limit)
		if err != nil {
			return err
		}
		now := s.now()
		for _, sup := range all {
			if q.Channel != "" && sup.ChannelScope != "ALL" && sup.ChannelScope != q.Channel {
				continue
			}
			if q.Purpose != "" && sup.PurposeScope != "ALL" && sup.PurposeScope != q.Purpose {
				continue
			}
			if q.ActiveOnly && !sup.ActiveAt(now) {
				continue
			}
			out = append(out, sup)
		}
		return nil
	})
	if out == nil {
		out = []Suppression{}
	}
	return out, err
}

// LiftInput is POST /v1/suppressions/{id}/lift.
type LiftInput struct {
	EvidenceRef string `json:"evidence_ref"`
	Reason      string `json:"reason"`
}

// LiftSuppression lifts a suppression with evidence. Governed reasons (hard
// bounce, complaint, legal, security) become a maker-checker approval: the
// lift happens only when a second principal approves it (§7.3).
func (s *Service) LiftSuppression(ctx context.Context, a Actor, id string, in LiftInput) (*Suppression, *Approval, error) {
	if strings.TrimSpace(in.EvidenceRef) == "" || strings.TrimSpace(in.Reason) == "" {
		return nil, nil, Invalid("missing_fields", "evidence_ref and reason are required: a suppression is lifted on evidence of correction or re-consent (§7.3)")
	}
	var sup *Suppression
	var appr *Approval
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		sup, err = tx.GetSuppression(id)
		if err != nil {
			return mapNotFound(err, "suppression_not_found", "suppression %s", id)
		}
		if sup.LiftedAt != nil {
			return Conflict("already_lifted", "suppression %s is already lifted", id)
		}
		if GovernedReactivation(sup.Reason) {
			appr = &Approval{ApprovalID: uuid.NewString(), TenantID: a.TenantID, LegalEntityID: "-", Kind: ApprovalSuppressionLift,
				TargetID: id, Payload: map[string]any{"evidence_ref": in.EvidenceRef}, Reason: in.Reason, Status: "PENDING",
				RequestedBy: a.PrincipalID, RequestedAt: s.now()}
			return tx.InsertApproval(appr)
		}
		now := s.now()
		if err := tx.LiftSuppression(id, a.PrincipalID, in.EvidenceRef, "", now); err != nil {
			return err
		}
		sup, err = tx.GetSuppression(id)
		return err
	})
	return sup, appr, err
}

// ── revalidate (§5.5) ───────────────────────────────────────────────────────

// Revalidate re-resolves the recipient and re-decides channels for a
// communication that has not yet had a provider submission (§5.2 "endpoint
// changed after job creation — re-resolve before first submission").
func (s *Service) Revalidate(ctx context.Context, a Actor, communicationID string) (*Communication, *ChannelDecision, error) {
	var comm *Communication
	var intent *Intent
	var pref *Preference
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		comm, err = tx.GetCommunication(communicationID, false)
		if err != nil {
			return mapNotFound(err, "communication_not_found", "communication %s", communicationID)
		}
		if comm.IntentVersion == 0 {
			return Conflict("not_prepared", "communication %s has not been prepared", communicationID)
		}
		intent, err = tx.GetIntent(comm.IntentID, comm.IntentVersion)
		if err != nil {
			return err
		}
		pref, err = tx.GetPreference(comm.RecipientPrincipalID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	switch comm.LifecycleState {
	case CommPrepared, CommBlocked, CommReviewRequired, CommDispatched:
	default:
		return nil, nil, Conflict("not_revalidatable", "communication %s is %s", communicationID, comm.LifecycleState)
	}
	plan, err := s.buildPlan(ctx, a, *intent, RecipientInput{IntentID: comm.IntentID, RecipientPrincipalID: comm.RecipientPrincipalID,
		RecipientTenantID: comm.RecipientTenantID, FreeTextEndpoint: comm.FreeTextEndpoint, Locale: comm.Locale}, pref)
	if err != nil {
		return nil, nil, err
	}
	var dec *ChannelDecision
	err = s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		c, err := tx.GetCommunication(communicationID, true)
		if err != nil {
			return err
		}
		renders, err := tx.ListRenders(communicationID)
		if err != nil {
			return err
		}
		if err := tx.InsertPlan(plan); err != nil {
			return err
		}
		scoped := restrictToRendered(*intent, renders)
		dec, err = s.decide(ctx, tx, a, scoped, plan, c.PrivacyPermission, c.MarketingPermission, c.ResidencyRegions, c.CommunicationID)
		if err != nil {
			return err
		}
		c.RecipientPlanID, c.ChannelDecisionID = plan.PlanID, dec.DecisionID
		if c.LifecycleState == CommDispatched {
			// Only a job that has not submitted yet takes the new routes; a
			// submitted attempt is history and is never mutated (§6.6).
			jobs, err := tx.ListJobs(communicationID)
			if err != nil {
				return err
			}
			for i := range jobs {
				j := jobs[i]
				if j.State == JobQueued && j.AttemptsOnRoute == 0 && j.RouteIndex == 0 {
					if dec.Outcome == DecisionBlocked || dec.Outcome == DecisionReviewRequired {
						j.State, j.ExceptionReason = JobCancelled, "revalidation: "+string(firstCode(dec))
						now := s.now()
						j.ConcludedAt = &now
					} else if len(dec.Routes) > 0 {
						j.Routes = dec.Routes
					}
					if err := tx.UpdateJob(&j); err != nil {
						return err
					}
				}
			}
		} else {
			applyDecisionToComm(c, dec)
		}
		comm = c
		return tx.UpdateCommunication(c)
	})
	if err != nil {
		return nil, nil, err
	}
	return comm, dec, nil
}

func firstCode(d *ChannelDecision) ReasonCode {
	if len(d.ReasonCodes) > 0 {
		return d.ReasonCodes[0]
	}
	return ""
}

func applyDecisionToComm(c *Communication, d *ChannelDecision) {
	switch d.Outcome {
	case DecisionPermitted, DecisionDeferred:
		c.LifecycleState, c.BlockedReasonCode, c.BlockedDetail = CommPrepared, "", ""
		c.NotBefore = d.NotBefore
	case DecisionBlocked:
		c.LifecycleState, c.BlockedReasonCode = CommBlocked, firstCode(d)
		c.BlockedDetail = restrictionSummary(d)
	case DecisionReviewRequired:
		c.LifecycleState, c.BlockedReasonCode = CommReviewRequired, firstCode(d)
		c.BlockedDetail = restrictionSummary(d)
	}
}

func restrictionSummary(d *ChannelDecision) string {
	var parts []string
	for _, r := range d.Restrictions {
		p := string(r.Code) + " " + r.Detail
		if r.Channel != "" {
			p = r.Channel + ": " + p
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "no compliant channel"
	}
	return strings.Join(parts, "; ")
}

// restrictToRendered narrows an intent's channels to those with pinned
// content, so the decision never routes a channel with nothing to send.
func restrictToRendered(intent Intent, renders []RenderedContent) Intent {
	have := map[string]bool{}
	for _, r := range renders {
		have[r.Channel] = true
	}
	var chs []string
	for _, c := range intent.AllowedChannels {
		if have[c] {
			chs = append(chs, c)
		}
	}
	intent.AllowedChannels = chs
	return intent
}
