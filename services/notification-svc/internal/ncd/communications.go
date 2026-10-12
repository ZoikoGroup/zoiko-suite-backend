package ncd

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CommunicationInput is POST /v1/communications.
type CommunicationInput struct {
	IntentID             string             `json:"intent_id"`
	LegalEntityID        string             `json:"legal_entity_id"`
	RecipientPrincipalID string             `json:"recipient_principal_id"`
	RecipientTenantID    string             `json:"recipient_tenant_id,omitempty"`
	FreeTextEndpoint     *FreeTextEndpoint  `json:"free_text_endpoint,omitempty"`
	Locale               string             `json:"locale"`
	Variables            map[string]string  `json:"variables,omitempty"`
	Attachments          []Attachment       `json:"attachments,omitempty"`
	SourceEventID        string             `json:"source_event_id"`
	SourceEventType      string             `json:"source_event_type,omitempty"`
	WorkflowID           string             `json:"workflow_id,omitempty"`
	PrivacyPermission    PermissionDecision `json:"privacy_permission"`
	MarketingPermission  PermissionDecision `json:"marketing_permission"`
	PDCDecisionRef       string             `json:"pdc_decision_ref,omitempty"`
	ResidencyRegions     []string           `json:"residency_regions,omitempty"`
	NotBefore            *time.Time         `json:"not_before,omitempty"`
	ExpiresAt            *time.Time         `json:"expires_at,omitempty"`
	// Corrections (§6.6, §8.4, INV-19).
	SupersedesCommunicationID string `json:"supersedes_communication_id,omitempty"`
	CorrectionReason          string `json:"correction_reason,omitempty"`

	bulkID string
}

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CommunicationKey is the §3.4 purpose-scoped idempotency key: derived from
// the originating business event, the intent (which fixes the purpose) and
// the recipient. A replayed source event maps to the same communication
// (NP-21); the same event raising a DIFFERENT intent for the same person is a
// different communication.
func CommunicationKey(tenantID, legalEntityID, intentID, sourceEventID, recipient string) string {
	return "ncd-" + SHA256Hex("communication", tenantID, legalEntityID, intentID, sourceEventID, recipient)
}

// CreateCommunication records a logical communication. Returns created=false
// with the existing communication when the idempotency key was seen before.
func (s *Service) CreateCommunication(ctx context.Context, a Actor, in CommunicationInput) (*Communication, bool, error) {
	switch {
	case in.IntentID == "" || in.LegalEntityID == "" || in.RecipientPrincipalID == "" || in.SourceEventID == "":
		return nil, false, Invalid("missing_fields", "intent_id, legal_entity_id, recipient_principal_id and source_event_id are required")
	case in.Locale == "" || !ValidLocale(in.Locale):
		return nil, false, Invalid("invalid_locale", "locale is required and must be a locale tag like en or en-GB")
	case (in.SupersedesCommunicationID == "") != (strings.TrimSpace(in.CorrectionReason) == ""):
		return nil, false, Invalid("correction_needs_reason", "supersedes_communication_id and correction_reason are supplied together")
	}
	for _, att := range in.Attachments {
		if att.Slot == "" || att.DRCRecordID == "" || att.DRCVersion == "" || !sha256RE.MatchString(att.SHA256) {
			return nil, false, Invalid("invalid_attachment",
				"each attachment pins slot, drc_record_id, drc_version and a lowercase sha256; \"latest\" is never evidence (INV-18)")
		}
		if strings.EqualFold(att.DRCVersion, "latest") {
			return nil, false, Invalid("invalid_attachment", "drc_version \"latest\" is a mutable pointer; pin the exact version (NP-10)")
		}
	}
	now := s.now()
	c := &Communication{
		CommunicationID: uuid.NewString(), TenantID: a.TenantID, LegalEntityID: in.LegalEntityID, IntentID: in.IntentID,
		IdempotencyKey: CommunicationKey(a.TenantID, in.LegalEntityID, in.IntentID, in.SourceEventID, in.RecipientPrincipalID),
		SourceEventID:  in.SourceEventID, SourceEventType: in.SourceEventType, WorkflowID: in.WorkflowID,
		RecipientPrincipalID: in.RecipientPrincipalID, RecipientTenantID: in.RecipientTenantID, FreeTextEndpoint: in.FreeTextEndpoint,
		Locale: in.Locale, Variables: in.Variables, Attachments: nonNilAttachments(in.Attachments),
		PrivacyPermission: in.PrivacyPermission, MarketingPermission: in.MarketingPermission, PDCDecisionRef: in.PDCDecisionRef,
		ResidencyRegions: nonNilStrings(in.ResidencyRegions), LifecycleState: CommCreated, NotBefore: in.NotBefore,
		SupersedesCommunicationID: in.SupersedesCommunicationID, CorrectionReason: in.CorrectionReason, BulkID: in.bulkID,
		CreatedByPrincipalID: a.PrincipalID, CreatedAt: now,
	}
	if c.Variables == nil {
		c.Variables = map[string]string{}
	}
	var created bool
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		intent, err := tx.GetIntent(in.IntentID, 0)
		if err != nil {
			return mapNotFound(err, "intent_not_found", "intent %s", in.IntentID)
		}
		if intent.LegalEntityID != in.LegalEntityID {
			return Invalid("legal_entity_mismatch", "intent %s belongs to another legal entity", in.IntentID)
		}
		exp := now.Add(time.Duration(intent.DefaultExpirySeconds) * time.Second)
		if in.ExpiresAt != nil {
			if !in.ExpiresAt.After(now) {
				return Invalid("expires_at_in_past", "expires_at must be in the future")
			}
			exp = in.ExpiresAt.UTC()
		}
		c.ExpiresAt = exp
		c.Priority = intent.PurposeClass.Priority()
		if in.SupersedesCommunicationID != "" {
			prior, err := tx.GetCommunication(in.SupersedesCommunicationID, false)
			if err != nil {
				return mapNotFound(err, "superseded_not_found", "superseded communication %s", in.SupersedesCommunicationID)
			}
			if prior.RecipientPrincipalID != in.RecipientPrincipalID {
				return Invalid("correction_recipient_mismatch", "a correction goes to the recipient of the communication it corrects")
			}
		}
		created, err = tx.InsertCommunication(c)
		if err != nil || !created {
			return err
		}
		if c.SupersedesCommunicationID != "" {
			return emit(tx, a, EvtCorrection, c.LegalEntityID, c.CommunicationID, map[string]any{
				"communication_id": c.CommunicationID, "superseded_communication_id": c.SupersedesCommunicationID,
				"reason": c.CorrectionReason,
			})
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return c, created, nil
}

func nonNilAttachments(v []Attachment) []Attachment {
	if v == nil {
		return []Attachment{}
	}
	return v
}

// PrepareResult is POST /v1/communications/{id}/prepare.
type PrepareResult struct {
	Communication *Communication    `json:"communication"`
	Renders       []RenderedContent `json:"renders"`
	Plan          *RecipientPlan    `json:"recipient_plan,omitempty"`
	Decision      *ChannelDecision  `json:"channel_decision,omitempty"`
	Refusal       *Refusal          `json:"refusal,omitempty"`
}

// Prepare resolves template/content/recipient plan without any delivery side
// effect and returns blocking/review reasons (§10.1). Content is rendered and
// pinned once; a re-prepare reuses the pinned renders (INV-04).
func (s *Service) Prepare(ctx context.Context, a Actor, id string) (*PrepareResult, error) {
	now := s.now()
	var comm *Communication
	var intent *Intent
	var pref *Preference
	var renders []RenderedContent
	var refusal *Refusal

	// Phase 1 (tx): intent, content, attachments.
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		if comm, err = tx.GetCommunication(id, true); err != nil {
			return mapNotFound(err, "communication_not_found", "communication %s", id)
		}
		switch comm.LifecycleState {
		case CommCreated, CommBlocked, CommReviewRequired:
		default:
			return Conflict("not_preparable", "communication %s is %s", id, comm.LifecycleState)
		}
		if renders, err = tx.ListRenders(id); err != nil {
			return err
		}
		if comm.IntentVersion > 0 {
			intent, err = tx.GetIntent(comm.IntentID, comm.IntentVersion)
		} else {
			intent, err = effectiveIntentNow(tx, comm.IntentID, now)
		}
		if err != nil {
			var e *Error
			if errors.As(err, &e) && e.Refusal != nil {
				refusal = e.Refusal
				return s.block(tx, a, comm, refusal)
			}
			return err
		}
		if pref, err = tx.GetPreference(comm.RecipientPrincipalID); err != nil {
			return err
		}
		if len(renders) > 0 {
			return nil
		}
		if refusal = checkAttachments(*intent, comm.Attachments); refusal != nil {
			return s.block(tx, a, comm, refusal)
		}
		if refusal = ValidateVariables(*intent, comm.Variables); refusal != nil {
			return s.block(tx, a, comm, refusal)
		}
		ts, err := tx.EffectiveTemplates(comm.IntentID, now, now)
		if err != nil {
			return err
		}
		rs, ref := renderAll(*intent, ts, comm, now)
		if ref != nil {
			refusal = ref
			return s.block(tx, a, comm, refusal)
		}
		for i := range rs {
			rs[i].RenderID = uuid.NewString()
			if err := tx.InsertRender(&rs[i]); err != nil {
				return err
			}
		}
		renders = rs
		comm.IntentVersion, comm.PurposeClass = intent.Version, intent.PurposeClass
		return tx.UpdateCommunication(comm)
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return &PrepareResult{Communication: comm, Renders: nonNilRenders(renders), Refusal: refusal}, nil
	}

	// Phase 2 (no tx): recipient resolution calls the contact authority.
	plan, err := s.buildPlan(ctx, a, *intent, RecipientInput{IntentID: comm.IntentID, RecipientPrincipalID: comm.RecipientPrincipalID,
		RecipientTenantID: comm.RecipientTenantID, FreeTextEndpoint: comm.FreeTextEndpoint, Locale: comm.Locale}, pref)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Refusal != nil {
			refusal = e.Refusal
			if berr := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
				c, err := tx.GetCommunication(id, true)
				if err != nil {
					return err
				}
				comm = c
				return s.block(tx, a, c, refusal)
			}); berr != nil {
				return nil, berr
			}
			return &PrepareResult{Communication: comm, Renders: renders, Refusal: refusal}, nil
		}
		return nil, err
	}

	// Phase 3 (tx): plan, decision, state, event.
	var dec *ChannelDecision
	err = s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		c, err := tx.GetCommunication(id, true)
		if err != nil {
			return err
		}
		if err := tx.InsertPlan(plan); err != nil {
			return err
		}
		dec, err = s.decide(ctx, tx, a, restrictToRendered(*intent, renders), plan, c.PrivacyPermission, c.MarketingPermission, c.ResidencyRegions, c.CommunicationID)
		if err != nil {
			return err
		}
		c.RecipientPlanID, c.ChannelDecisionID = plan.PlanID, dec.DecisionID
		c.IntentVersion, c.PurposeClass = intent.Version, intent.PurposeClass
		applyDecisionToComm(c, dec)
		if c.NotBefore == nil && comm.NotBefore != nil {
			c.NotBefore = comm.NotBefore
		}
		c.PreparedAt = ptr(s.now())
		if err := tx.UpdateCommunication(c); err != nil {
			return err
		}
		comm = c
		if c.LifecycleState == CommPrepared {
			return emit(tx, a, EvtPrepared, c.LegalEntityID, c.CommunicationID, map[string]any{
				"communication_id": c.CommunicationID, "intent_id": c.IntentID, "intent_version": c.IntentVersion,
				"rendered_content_hash": combinedContentHash(renders, c.Attachments), "recipient_plan_id": plan.PlanID,
				"decision_refs": map[string]any{"channel_decision_id": dec.DecisionID, "privacy": c.PrivacyPermission.DecisionID,
					"marketing": c.MarketingPermission.DecisionID, "pdc": c.PDCDecisionRef},
			})
		}
		return emit(tx, a, EvtBlocked, c.LegalEntityID, c.CommunicationID, map[string]any{
			"communication_id": c.CommunicationID, "reason_code": c.BlockedReasonCode, "outcome": c.LifecycleState,
			"reference": dec.DecisionID,
		})
	})
	if err != nil {
		return nil, err
	}
	res := &PrepareResult{Communication: comm, Renders: renders, Plan: plan, Decision: dec}
	if comm.LifecycleState != CommPrepared {
		res.Refusal = Refuse(comm.BlockedReasonCode, comm.BlockedDetail)
	}
	return res, nil
}

func nonNilRenders(v []RenderedContent) []RenderedContent {
	if v == nil {
		return []RenderedContent{}
	}
	return v
}

// block records a refusal on the communication and emits communication.blocked.
func (s *Service) block(tx Tx, a Actor, c *Communication, r *Refusal) error {
	c.LifecycleState, c.BlockedReasonCode, c.BlockedDetail = CommBlocked, r.Code, r.Detail
	if r.Code == NCD018RegulatedReviewRequired || r.Code == NCD005LocaleNotApproved && c.PurposeClass == PurposeRegulated {
		c.LifecycleState = CommReviewRequired
	}
	if err := tx.UpdateCommunication(c); err != nil {
		return err
	}
	return emit(tx, a, EvtBlocked, c.LegalEntityID, c.CommunicationID, map[string]any{
		"communication_id": c.CommunicationID, "reason_code": r.Code, "reason": r.Name, "detail": r.Detail,
	})
}

// checkAttachments enforces the intent's attachment contract (§4.1, INV-18).
func checkAttachments(intent Intent, atts []Attachment) *Refusal {
	have := map[string]bool{}
	for _, a := range atts {
		found := false
		for _, slot := range intent.AttachmentContract {
			if slot.Slot == a.Slot {
				found = true
			}
		}
		if !found {
			return Refuse(NCD004TemplateVariableInvalid, "attachment slot "+a.Slot+" is not in the intent's attachment contract")
		}
		if have[a.Slot] {
			return Refuse(NCD004TemplateVariableInvalid, "attachment slot "+a.Slot+" supplied twice")
		}
		have[a.Slot] = true
	}
	for _, slot := range intent.AttachmentContract {
		if slot.Required && !have[slot.Slot] {
			return Refuse(NCD004TemplateVariableInvalid, "required attachment slot "+slot.Slot+" is missing")
		}
	}
	return nil
}

// renderAll pins one render per allowed channel that has an effective
// template for the requested locale, or an explicitly compatible one.
func renderAll(intent Intent, ts []TemplateVersion, c *Communication, now time.Time) ([]RenderedContent, *Refusal) {
	if len(ts) == 0 {
		return nil, Refuse(NCD003TemplateNotPublished, "the intent has no published, effective template for any channel")
	}
	var out []RenderedContent
	localesSeen := map[string]bool{}
	for _, ch := range intent.AllowedChannels {
		var pick *TemplateVersion
		fallbackFrom := ""
		for i := range ts {
			t := ts[i]
			if t.Channel != ch {
				continue
			}
			localesSeen[t.Locale] = true
			if t.Locale == c.Locale {
				pick = &ts[i]
				fallbackFrom = ""
				break
			}
			for _, l := range t.CompatibleLocales {
				if l == c.Locale && pick == nil {
					pick = &ts[i]
					fallbackFrom = c.Locale
				}
			}
		}
		if pick == nil {
			continue
		}
		r, ref := Render(intent, *pick, c.Variables)
		if ref != nil {
			return nil, ref
		}
		rc := RenderedContent{
			CommunicationID: c.CommunicationID, Channel: ch, TemplateVersionID: pick.TemplateVersionID,
			TemplateVersion: pick.Version, Locale: pick.Locale, LocaleFallbackFrom: fallbackFrom,
			Subject: r.Subject, Body: r.Body, SubjectHash: r.SubjectHash, BodyHash: r.BodyHash,
			ContentHash: ContentHashWithAttachments(r.ContentHash, c.Attachments), VariableHashes: r.VariableHashes,
			AttachmentManifest: nonNilAttachments(c.Attachments), RenderedAt: now,
		}
		out = append(out, rc)
	}
	if len(out) == 0 {
		// NP-09: no silent English/global fallback.
		return nil, Refuse(NCD005LocaleNotApproved, fmt.Sprintf(
			"no approved template for locale %s and no template declares it compatible (published locales: %s)", c.Locale, keys(localesSeen)))
	}
	return out, nil
}

func keys(m map[string]bool) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// combinedContentHash identifies the full as-issued package of a communication.
func combinedContentHash(renders []RenderedContent, atts []Attachment) string {
	var hs []string
	for _, r := range renders {
		hs = append(hs, r.Channel+":"+r.ContentHash)
	}
	sort.Strings(hs)
	return ContentHashWithAttachments(SHA256Hex(append([]string{"package"}, hs...)...), atts)
}

// Dispatch performs the atomic final permission/suppression recheck, creates
// the delivery job and queues it (§10.1).
func (s *Service) Dispatch(ctx context.Context, a Actor, id string) (*Communication, *DeliveryJob, error) {
	var comm *Communication
	var job *DeliveryJob
	var refusal *Refusal
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		c, err := tx.GetCommunication(id, true)
		if err != nil {
			return mapNotFound(err, "communication_not_found", "communication %s", id)
		}
		comm = c
		if c.LifecycleState != CommPrepared {
			return Conflict("not_prepared", "communication %s is %s; dispatch needs PREPARED", id, c.LifecycleState)
		}
		now := s.now()
		if !c.ExpiresAt.After(now) {
			refusal = Refuse(NCD015DeliveryExpired, "the communication expired before dispatch")
			c.LifecycleState, c.BlockedReasonCode, c.BlockedDetail, c.ConcludedAt = CommExpired, refusal.Code, refusal.Detail, &now
			return tx.UpdateCommunication(c)
		}
		intent, err := tx.GetIntent(c.IntentID, c.IntentVersion)
		if err != nil {
			return err
		}
		if intent.PurposeClass == PurposeRegulated {
			n, err := tx.GetNoticeByCommunication(c.CommunicationID)
			if errors.Is(err, ErrNotFound) {
				refusal = Refuse(NCD018RegulatedReviewRequired,
					"a regulated communication needs its notice package (POST /v1/regulated-notices) before dispatch")
				return s.block(tx, a, c, refusal)
			}
			if err != nil {
				return err
			}
			if n.State == NoticePrepared {
				n.State = NoticeReady
				if err := tx.UpdateNotice(n); err != nil {
					return err
				}
			}
		}
		plan, err := tx.GetPlan(c.RecipientPlanID)
		if err != nil {
			return err
		}
		renders, err := tx.ListRenders(c.CommunicationID)
		if err != nil {
			return err
		}
		// The final gate: a fresh decision against suppressions and
		// preferences as they are NOW, in the transaction that creates the
		// job (§10.1 dispatch, INV-24).
		dec, err := s.decide(ctx, tx, a, restrictToRendered(*intent, renders), plan, c.PrivacyPermission, c.MarketingPermission, c.ResidencyRegions, c.CommunicationID)
		if err != nil {
			return err
		}
		c.ChannelDecisionID = dec.DecisionID
		if dec.Outcome == DecisionBlocked || dec.Outcome == DecisionReviewRequired {
			refusal = Refuse(firstCode(dec), restrictionSummary(dec))
			return s.block(tx, a, c, refusal)
		}
		nb := c.NotBefore
		if dec.NotBefore != nil && (nb == nil || dec.NotBefore.After(*nb)) {
			nb = dec.NotBefore
		}
		next := now
		if nb != nil && nb.After(now) {
			next = *nb
		}
		job = &DeliveryJob{
			JobID: uuid.NewString(), TenantID: a.TenantID, CommunicationID: c.CommunicationID, Origin: "INITIAL",
			Routes: dec.Routes, MaxAttemptsPerRoute: s.limits.MaxAttemptsPerRoute, State: JobQueued,
			Stream: intent.PurposeClass.Stream(), Priority: intent.PurposeClass.Priority(), NextRunAt: next,
			NotBefore: nb, ExpiresAt: c.ExpiresAt, CreatedBy: a.PrincipalID, CreatedAt: now,
		}
		if c.BulkID != "" {
			job.Origin = "BULK"
		}
		if err := tx.InsertJob(job); err != nil {
			return err
		}
		c.LifecycleState, c.DispatchedAt = CommDispatched, &now
		return tx.UpdateCommunication(c)
	})
	if err != nil {
		return nil, nil, err
	}
	if refusal != nil {
		return comm, nil, Refused(refusal)
	}
	s.Kick()
	return comm, job, nil
}

// Cancel cancels eligible unsent jobs and preserves the evidence (§6.6).
// Attempts already submitted are history and are not touched.
func (s *Service) Cancel(ctx context.Context, a Actor, id, reason string) (*Communication, int, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, 0, Invalid("missing_fields", "reason is required")
	}
	var comm *Communication
	cancelled := 0
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		c, err := tx.GetCommunication(id, true)
		if err != nil {
			return mapNotFound(err, "communication_not_found", "communication %s", id)
		}
		comm = c
		now := s.now()
		switch c.LifecycleState {
		case CommCreated, CommPrepared, CommBlocked, CommReviewRequired:
			c.LifecycleState, c.ConcludedAt = CommCancelled, &now
			cancelled = 1
		case CommDispatched:
			jobs, err := tx.ListJobs(id)
			if err != nil {
				return err
			}
			for i := range jobs {
				j := jobs[i]
				if j.State == JobQueued {
					j.State, j.ExceptionReason, j.ConcludedAt = JobCancelled, "cancelled: "+reason, &now
					if err := tx.UpdateJob(&j); err != nil {
						return err
					}
					cancelled++
				}
			}
			if cancelled == 0 {
				return Conflict("nothing_cancellable", "every job of communication %s has already submitted or concluded", id)
			}
			attempts, err := tx.ListAttempts(id)
			if err != nil {
				return err
			}
			if len(attempts) == 0 {
				c.LifecycleState, c.ConcludedAt = CommCancelled, &now
			}
		default:
			return Conflict("not_cancellable", "communication %s is %s", id, c.LifecycleState)
		}
		if err := tx.UpdateCommunication(c); err != nil {
			return err
		}
		return s.recordEvidence(tx, a, c, &Evidence{EvidenceType: "CANCELLED", NormalizedState: "CANCELLED", Source: "OPERATOR",
			Confidence: "HIGH", DoesNotProve: "that no earlier attempt reached the recipient", ActorPrincipalID: a.PrincipalID,
			ObservedAt: now, Details: map[string]any{"reason": reason, "jobs_cancelled": cancelled}})
	})
	return comm, cancelled, err
}

// ResendInput is POST /v1/communications/{id}/resend.
type ResendInput struct {
	Reason string `json:"reason"`
}

// Resend creates a governed new job under the same communication with an
// explicit reason; the original attempts and evidence are untouched (§3.4).
// For a regulated, mandatory or security communication it becomes a
// maker-checker approval instead (§11.3 "subject to maker-checker for
// high-impact notices").
func (s *Service) Resend(ctx context.Context, a Actor, id string, in ResendInput) (*DeliveryJob, *Approval, error) {
	if strings.TrimSpace(in.Reason) == "" {
		return nil, nil, Invalid("missing_fields", "a resend needs an explicit reason (§3.4)")
	}
	var job *DeliveryJob
	var appr *Approval
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		c, err := tx.GetCommunication(id, true)
		if err != nil {
			return mapNotFound(err, "communication_not_found", "communication %s", id)
		}
		intent, err := tx.GetIntent(c.IntentID, c.IntentVersion)
		if err != nil {
			return mapNotFound(err, "not_prepared", "communication %s was never prepared", id)
		}
		if err := resendable(tx, c); err != nil {
			return err
		}
		if intent.Mandatory || intent.PurposeClass == PurposeRegulated || intent.PurposeClass == PurposeSecurityCritical {
			appr = &Approval{ApprovalID: uuid.NewString(), TenantID: a.TenantID, LegalEntityID: c.LegalEntityID, Kind: ApprovalResend,
				TargetID: id, Payload: map[string]any{}, Reason: in.Reason, Status: "PENDING", RequestedBy: a.PrincipalID, RequestedAt: s.now()}
			return tx.InsertApproval(appr)
		}
		job, err = s.createResendJob(ctx, tx, a, c, intent, in.Reason)
		return err
	})
	if err == nil && job != nil {
		s.Kick()
	}
	return job, appr, err
}

func resendable(tx Tx, c *Communication) error {
	switch c.LifecycleState {
	case CommDispatched, CommCompleted, CommException, CommExpired:
	default:
		return Conflict("not_resendable", "communication %s is %s; only a dispatched communication is resent", c.CommunicationID, c.LifecycleState)
	}
	unknown, err := tx.HasUnknownAttempt(c.CommunicationID)
	if err != nil {
		return err
	}
	if unknown {
		return Refused(Refuse(NCD014DeliveryAttemptUnknown,
			"an attempt of this communication is UNKNOWN; reconcile the original attempt before any resend (INV-13)"))
	}
	jobs, err := tx.ListJobs(c.CommunicationID)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.State == JobQueued || j.State == JobAwaitingResolution {
			return Conflict("delivery_in_progress", "communication %s still has a live job (%s)", c.CommunicationID, j.State)
		}
	}
	return nil
}

func (s *Service) createResendJob(ctx context.Context, tx Tx, a Actor, c *Communication, intent *Intent, reason string) (*DeliveryJob, error) {
	plan, err := tx.GetPlan(c.RecipientPlanID)
	if err != nil {
		return nil, err
	}
	renders, err := tx.ListRenders(c.CommunicationID)
	if err != nil {
		return nil, err
	}
	dec, err := s.decide(ctx, tx, a, restrictToRendered(*intent, renders), plan, c.PrivacyPermission, c.MarketingPermission, c.ResidencyRegions, c.CommunicationID)
	if err != nil {
		return nil, err
	}
	if dec.Outcome == DecisionBlocked || dec.Outcome == DecisionReviewRequired {
		return nil, Refused(Refuse(firstCode(dec), restrictionSummary(dec)))
	}
	now := s.now()
	exp := c.ExpiresAt
	if !exp.After(now) {
		exp = now.Add(time.Duration(intent.DefaultExpirySeconds) * time.Second)
	}
	job := &DeliveryJob{
		JobID: uuid.NewString(), TenantID: a.TenantID, CommunicationID: c.CommunicationID, Origin: "RESEND",
		ResendReason: reason, Routes: dec.Routes, MaxAttemptsPerRoute: s.limits.MaxAttemptsPerRoute, State: JobQueued,
		Stream: intent.PurposeClass.Stream(), Priority: intent.PurposeClass.Priority(), NextRunAt: now,
		ExpiresAt: exp, CreatedBy: a.PrincipalID, CreatedAt: now,
	}
	if err := tx.InsertJob(job); err != nil {
		return nil, err
	}
	c.LifecycleState, c.ConcludedAt = CommDispatched, nil
	c.ExpiresAt = exp
	return job, tx.UpdateCommunication(c)
}

// ResolveInput is POST /v1/communications/{id}/attempts/{attempt_id}/resolve.
type ResolveInput struct {
	ResolvedState string `json:"resolved_state"` // ACCEPTED, DELIVERED or FAILED
	EvidenceRef   string `json:"evidence_ref"`
	Note          string `json:"note"`
}

// ResolveUnknown reconciles an UNKNOWN attempt against evidence about the
// ORIGINAL submission — a provider query result, an export row, a ticket.
// Only after that may a failed attempt be retried or fall back (§6.2).
func (s *Service) ResolveUnknown(ctx context.Context, a Actor, commID, attemptID string, in ResolveInput) (*Attempt, error) {
	switch in.ResolvedState {
	case AttemptAccepted, AttemptDelivered, AttemptFailed:
	default:
		return nil, Invalid("invalid_resolved_state", "resolved_state must be ACCEPTED, DELIVERED or FAILED")
	}
	if strings.TrimSpace(in.EvidenceRef) == "" || strings.TrimSpace(in.Note) == "" {
		return nil, Invalid("missing_fields", "evidence_ref and note are required: an ambiguous attempt is resolved on evidence, never on a guess")
	}
	var out *Attempt
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		att, err := tx.GetAttempt(attemptID)
		if err != nil || att.CommunicationID != commID {
			return NotFound("attempt_not_found", "attempt %s of communication %s", attemptID, commID)
		}
		if att.State != AttemptUnknown {
			return Conflict("not_unknown", "attempt %s is %s, not UNKNOWN", attemptID, att.State)
		}
		now := s.now()
		att.ResolvedAt, att.ResolvedBy, att.ResolutionNote = &now, a.PrincipalID, in.Note
		norm := Normalized{EvidenceType: "UNKNOWN_RESOLVED", NormalizedState: in.ResolvedState, Confidence: "MEDIUM",
			DoesNotProve: "more than the cited reconciliation evidence shows", AttemptState: in.ResolvedState}
		if in.ResolvedState == AttemptFailed {
			att.FailureReason = "resolved FAILED by reconciliation: " + in.Note
		}
		if err := s.applyFact(ctx, tx, a, att, norm, factMeta{source: "OPERATOR", actor: a.PrincipalID,
			details: map[string]any{"evidence_ref": in.EvidenceRef, "note": in.Note}, observedAt: now}); err != nil {
			return err
		}
		if x, err := tx.ListExceptions(true, 500); err == nil {
			for _, e := range x {
				if e.AttemptID == attemptID && e.Kind == "UNKNOWN_UNRESOLVED" {
					_ = tx.ResolveException(e.ExceptionID, a.PrincipalID, "resolved "+in.ResolvedState+": "+in.EvidenceRef, now)
				}
			}
		}
		out, err = tx.GetAttempt(attemptID)
		return err
	})
	if err == nil {
		s.Kick()
	}
	return out, err
}

// Misdelivery records a mistaken-recipient incident (NP-44, §8.4): further
// sends stop, the endpoint is placed on a security hold, and nothing is
// deleted — the evidence of the event is preserved, never concealed.
func (s *Service) Misdelivery(ctx context.Context, a Actor, id, reason string) (*Communication, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, Invalid("missing_fields", "reason is required")
	}
	var comm *Communication
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		c, err := tx.GetCommunication(id, true)
		if err != nil {
			return mapNotFound(err, "communication_not_found", "communication %s", id)
		}
		now := s.now()
		jobs, err := tx.ListJobs(id)
		if err != nil {
			return err
		}
		for i := range jobs {
			j := jobs[i]
			if j.State == JobQueued || j.State == JobAwaitingEvidence {
				j.State, j.ExceptionReason, j.ConcludedAt = JobCancelled, "misdelivery incident: "+reason, &now
				if err := tx.UpdateJob(&j); err != nil {
					return err
				}
			}
		}
		attempts, err := tx.ListAttempts(id)
		if err != nil {
			return err
		}
		for _, at := range attempts {
			hash, _ := at.RecipientSnapshot["endpoint_hash"].(string)
			masked, _ := at.RecipientSnapshot["endpoint_masked"].(string)
			if hash == "" {
				continue
			}
			sup := &Suppression{SuppressionID: uuid.NewString(), TenantID: a.TenantID, EndpointHash: hash, EndpointMasked: masked,
				ChannelScope: "ALL", PurposeScope: "ALL", Reason: SuppSecurityHold, Source: "SECURITY",
				SourceEvidenceRef: "misdelivery:" + id, EffectiveFrom: now, CreatedByPrincipal: a.PrincipalID, CreatedAt: now}
			created, err := tx.InsertSuppression(sup)
			if err != nil {
				return err
			}
			if created {
				if err := emitSuppressed(tx, a, c.LegalEntityID, sup); err != nil {
					return err
				}
			}
		}
		if _, err := tx.InsertException(&Exception{ExceptionID: uuid.NewString(), TenantID: a.TenantID, CommunicationID: id,
			Kind: "MISDELIVERY_INCIDENT", Detail: "privacy/security incident assessment required: " + reason, CreatedAt: now}); err != nil {
			return err
		}
		if c.LifecycleState == CommDispatched {
			c.LifecycleState, c.ConcludedAt = CommException, &now
			if err := tx.UpdateCommunication(c); err != nil {
				return err
			}
		}
		comm = c
		return nil
	})
	return comm, err
}

// CommunicationView is GET /v1/communications/{id}: precise state dimensions
// and an evidence summary — no overloaded "sent" (§3.3, §10.1).
type CommunicationView struct {
	Communication *Communication   `json:"communication"`
	Claims        Claims           `json:"claims"`
	Jobs          []DeliveryJob    `json:"jobs"`
	Attempts      []Attempt        `json:"attempts"`
	Notice        *RegulatedNotice `json:"regulated_notice,omitempty"`
}

// GetCommunication reads one communication with its claims.
func (s *Service) GetCommunication(ctx context.Context, a Actor, id string) (*CommunicationView, error) {
	var v CommunicationView
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		c, err := tx.GetCommunication(id, false)
		if err != nil {
			return mapNotFound(err, "communication_not_found", "communication %s", id)
		}
		v.Communication = c
		if v.Jobs, err = tx.ListJobs(id); err != nil {
			return err
		}
		if v.Attempts, err = tx.ListAttempts(id); err != nil {
			return err
		}
		ev, err := tx.ListEvidence(id)
		if err != nil {
			return err
		}
		ack := "NOT_REQUIRED"
		if n, err := tx.GetNoticeByCommunication(id); err == nil {
			n.LegalSufficiency = LegalSufficiencyNotDetermined
			v.Notice = n
			ack = ackState(n)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		v.Claims = Summarize(v.Attempts, ev, ack)
		return nil
	})
	if v.Jobs == nil {
		v.Jobs = []DeliveryJob{}
	}
	if v.Attempts == nil {
		v.Attempts = []Attempt{}
	}
	return &v, err
}

// Communication returns the bare communication (for authorization).
func (s *Service) Communication(ctx context.Context, a Actor, id string) (*Communication, error) {
	var c *Communication
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		c, err = tx.GetCommunication(id, false)
		return mapNotFound(err, "communication_not_found", "communication %s", id)
	})
	return c, err
}

func ackState(n *RegulatedNotice) string {
	if n.AckRequirement == AckNone {
		return "NOT_REQUIRED"
	}
	switch n.State {
	case NoticeAcknowledged:
		return "ACKNOWLEDGED"
	case NoticeDeclined:
		return "DECLINED"
	case NoticeDisputed:
		return "DISPUTED"
	case NoticeExpired:
		return "EXPIRED"
	}
	return "PENDING"
}

// EvidenceBundle is GET /v1/evidence/{communication_id} (§3.2, §21.2): the
// answer to "why was it sent, what exactly, to whom, what did the provider
// prove, what was acknowledged, and what survives today".
type EvidenceBundle struct {
	Communication   *Communication    `json:"communication"`
	Intent          *Intent           `json:"intent,omitempty"`
	Renders         []RenderedContent `json:"renders"`
	Plan            *RecipientPlan    `json:"recipient_plan,omitempty"`
	Decisions       []ChannelDecision `json:"channel_decisions"`
	Jobs            []DeliveryJob     `json:"jobs"`
	Attempts        []Attempt         `json:"attempts"`
	Evidence        []Evidence        `json:"evidence"`
	Notices         []RegulatedNotice `json:"regulated_notices"`
	Acknowledgments []Acknowledgment  `json:"acknowledgments"`
	Approvals       []Approval        `json:"approvals"`
	Exceptions      []Exception       `json:"exceptions"`
	Claims          Claims            `json:"claims"`
	PackageHash     string            `json:"package_content_hash"`
}

// Evidence assembles the bundle.
func (s *Service) Evidence(ctx context.Context, a Actor, id string) (*EvidenceBundle, error) {
	b := &EvidenceBundle{}
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		c, err := tx.GetCommunication(id, false)
		if err != nil {
			return mapNotFound(err, "communication_not_found", "communication %s", id)
		}
		b.Communication = c
		if c.IntentVersion > 0 {
			if b.Intent, err = tx.GetIntent(c.IntentID, c.IntentVersion); err != nil {
				return err
			}
		}
		if b.Renders, err = tx.ListRenders(id); err != nil {
			return err
		}
		if c.RecipientPlanID != "" {
			if b.Plan, err = tx.GetPlan(c.RecipientPlanID); err != nil {
				return err
			}
		}
		if b.Decisions, err = tx.ListDecisions(id); err != nil {
			return err
		}
		if b.Jobs, err = tx.ListJobs(id); err != nil {
			return err
		}
		if b.Attempts, err = tx.ListAttempts(id); err != nil {
			return err
		}
		if b.Evidence, err = tx.ListEvidence(id); err != nil {
			return err
		}
		ack := "NOT_REQUIRED"
		if n, err := tx.GetNoticeByCommunication(id); err == nil {
			if b.Notices, err = tx.ListNoticeVersions(n.NoticeID); err != nil {
				return err
			}
			if b.Acknowledgments, err = tx.ListAcks(n.NoticeID); err != nil {
				return err
			}
			ack = ackState(n)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		for i := range b.Notices {
			b.Notices[i].LegalSufficiency = LegalSufficiencyNotDetermined
		}
		if b.Approvals, err = tx.ListApprovals(id); err != nil {
			return err
		}
		all, err := tx.ListExceptions(false, 1000)
		if err != nil {
			return err
		}
		for _, x := range all {
			if x.CommunicationID == id {
				b.Exceptions = append(b.Exceptions, x)
			}
		}
		b.Claims = Summarize(b.Attempts, b.Evidence, ack)
		b.PackageHash = combinedContentHash(b.Renders, c.Attachments)
		return nil
	})
	fillBundle(b)
	return b, err
}

func fillBundle(b *EvidenceBundle) {
	if b.Renders == nil {
		b.Renders = []RenderedContent{}
	}
	if b.Decisions == nil {
		b.Decisions = []ChannelDecision{}
	}
	if b.Jobs == nil {
		b.Jobs = []DeliveryJob{}
	}
	if b.Attempts == nil {
		b.Attempts = []Attempt{}
	}
	if b.Evidence == nil {
		b.Evidence = []Evidence{}
	}
	if b.Notices == nil {
		b.Notices = []RegulatedNotice{}
	}
	if b.Acknowledgments == nil {
		b.Acknowledgments = []Acknowledgment{}
	}
	if b.Approvals == nil {
		b.Approvals = []Approval{}
	}
	if b.Exceptions == nil {
		b.Exceptions = []Exception{}
	}
}

// ListExceptions returns the human-path queue.
func (s *Service) ListExceptions(ctx context.Context, a Actor, openOnly bool) ([]Exception, error) {
	var out []Exception
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		out, err = tx.ListExceptions(openOnly, 500)
		return err
	})
	if out == nil {
		out = []Exception{}
	}
	return out, err
}
