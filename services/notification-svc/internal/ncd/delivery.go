package ncd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	workerPrincipal = "system:ncd-worker"
	jobLease        = 2 * time.Minute
)

// factMeta carries the provenance of one fact applied to an attempt.
type factMeta struct {
	source            string
	actor             string
	bindingID         string
	providerEventID   string
	providerMessageID string
	payloadHash       string
	observedAt        time.Time
	details           map[string]any
}

// recordEvidence appends one evidence row and announces it.
func (s *Service) recordEvidence(tx Tx, a Actor, c *Communication, e *Evidence) error {
	if e.EvidenceID == "" {
		e.EvidenceID = uuid.NewString()
	}
	e.TenantID, e.CommunicationID = tx.TenantID(), c.CommunicationID
	e.ReceivedAt = s.now()
	if e.ObservedAt.IsZero() {
		e.ObservedAt = e.ReceivedAt
	}
	if err := tx.InsertEvidence(e); err != nil {
		return err
	}
	return emit(tx, a, EvtEvidenceRecorded, c.LegalEntityID, c.CommunicationID, map[string]any{
		"evidence_id": e.EvidenceID, "communication_id": c.CommunicationID, "attempt_id": e.AttemptID,
		"evidence_type": e.EvidenceType, "normalized_state": e.NormalizedState, "observed_at": e.ObservedAt,
		"provider_ref": e.ProviderEventID, "confidence": e.Confidence, "does_not_prove": e.DoesNotProve,
	})
}

// applyFact applies one normalized fact to an attempt: evidence first (so a
// duplicate callback stops here, NP-24), then the §6.2 transition if it is a
// legal one — a fact that would move the attempt backwards is kept as
// evidence and changes nothing (NP-25) — then the consequences for the job,
// the communication and any regulated notice.
func (s *Service) applyFact(ctx context.Context, tx Tx, a Actor, att *Attempt, norm Normalized, meta factMeta) error {
	now := s.now()
	c, err := tx.GetCommunication(att.CommunicationID, true)
	if err != nil {
		return err
	}
	moved := norm.AttemptState != "" && norm.AttemptState != att.State && CanTransition(att.State, norm.AttemptState)
	details := meta.details
	if details == nil {
		details = map[string]any{}
	}
	if norm.AttemptState != "" && !moved && norm.AttemptState != att.State {
		details["transition_applied"] = false
		details["transition_note"] = fmt.Sprintf("attempt already %s; %s would not be a §6.2 transition, recorded as evidence only", att.State, norm.AttemptState)
	}
	ev := &Evidence{AttemptID: att.AttemptID, EvidenceType: norm.EvidenceType, NormalizedState: norm.NormalizedState,
		Source: meta.source, Confidence: norm.Confidence, DoesNotProve: norm.DoesNotProve, BindingID: meta.bindingID,
		ProviderEventID: meta.providerEventID, PayloadHash: meta.payloadHash, ObservedAt: meta.observedAt,
		ActorPrincipalID: meta.actor, Details: details}
	if ev.BindingID == "" {
		ev.BindingID = att.BindingID
	}
	if err := s.recordEvidence(tx, a, c, ev); err != nil {
		return err
	}

	if meta.providerMessageID != "" && att.ProviderMessageID == "" {
		err := tx.SetProviderMessageID(att.AttemptID, meta.providerMessageID)
		switch {
		case errors.Is(err, ErrProviderIDCollision):
			// NP-27 / §7.5: quarantine and reconcile, never merge silently.
			if _, err := tx.InsertException(&Exception{ExceptionID: uuid.NewString(), TenantID: tx.TenantID(),
				CommunicationID: c.CommunicationID, AttemptID: att.AttemptID, Kind: "PROVIDER_ID_COLLISION",
				Detail:    "provider message id " + meta.providerMessageID + " already maps to another attempt; quarantined for reconciliation",
				CreatedAt: now}); err != nil {
				return err
			}
			if err := s.recordEvidence(tx, a, c, &Evidence{AttemptID: att.AttemptID, EvidenceType: "RECONCILIATION_EXCEPTION",
				NormalizedState: "QUARANTINED", Source: "RECONCILIATION", Confidence: "HIGH",
				DoesNotProve: "which attempt the provider record belongs to", BindingID: att.BindingID,
				Details: map[string]any{"provider_message_id": meta.providerMessageID}}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			att.ProviderMessageID = meta.providerMessageID
		}
	}

	if moved {
		att.State = norm.AttemptState
		if att.State == AttemptUnknown {
			att.ResolutionDueAt = ptr(now.Add(ResolutionDue))
		}
		if (att.State == AttemptFailed || att.State == AttemptBounced) && att.FailureReason == "" {
			att.FailureReason = norm.NormalizedState
			if r, ok := details["reason"].(string); ok && r != "" {
				att.FailureReason = r
			}
		}
		if err := tx.UpdateAttempt(att); err != nil {
			return err
		}
		if att.State == AttemptUnknown {
			if err := emit(tx, a, EvtAttemptUnknown, c.LegalEntityID, c.CommunicationID, map[string]any{
				"attempt_id": att.AttemptID, "communication_id": c.CommunicationID, "ambiguity_cause": att.FailureReason,
				"resolution_due_at": att.ResolutionDueAt, "reason_code": NCD014DeliveryAttemptUnknown,
			}); err != nil {
				return err
			}
		}
	}

	if norm.Suppression != "" {
		if err := s.suppressFromEvidence(tx, a, c, att, norm, now); err != nil {
			return err
		}
	}
	if !moved {
		return nil
	}
	job, err := tx.GetJob(att.JobID)
	if err != nil {
		return err
	}
	intent, err := tx.GetIntent(att.IntentID, att.IntentVersion)
	if err != nil {
		return err
	}
	return s.progress(tx, a, c, job, att, intent)
}

// suppressFromEvidence turns a bounce, complaint or invalid endpoint into
// canonical suppression, idempotently (§7.2, §7.3).
func (s *Service) suppressFromEvidence(tx Tx, a Actor, c *Communication, att *Attempt, norm Normalized, now time.Time) error {
	hash, _ := att.RecipientSnapshot["endpoint_hash"].(string)
	masked, _ := att.RecipientSnapshot["endpoint_masked"].(string)
	if hash == "" {
		return nil
	}
	mk := func(reason, purpose string, until *time.Time, ref string) *Suppression {
		return &Suppression{SuppressionID: uuid.NewString(), TenantID: tx.TenantID(), EndpointHash: hash, EndpointMasked: masked,
			ChannelScope: att.Channel, PurposeScope: purpose, Reason: reason, Source: "PROVIDER_EVENT",
			SourceEvidenceRef: ref, EffectiveFrom: now, EffectiveUntil: until, CreatedByPrincipal: workerPrincipal, CreatedAt: now}
	}
	ref := "attempt:" + att.AttemptID + ":" + norm.NormalizedState
	var sups []*Suppression
	switch norm.Suppression {
	case SuppSoftBounce:
		sups = append(sups, mk(SuppSoftBounce, "ALL", ptr(now.Add(24*time.Hour)), ref))
		// Repeated soft bounces promote to endpoint remediation (§5.4).
		n, err := tx.CountSoftBounces(hash, now.Add(-72*time.Hour))
		if err != nil {
			return err
		}
		if n+1 >= 3 {
			sups = append(sups, mk(SuppHardBounce, "ALL", nil, "promoted:3 soft bounces in 72h:"+hash))
		}
	default:
		sups = append(sups, mk(norm.Suppression, norm.SuppressionScope, nil, ref))
	}
	for _, sup := range sups {
		created, err := tx.InsertSuppression(sup)
		if err != nil {
			return err
		}
		if !created {
			continue
		}
		if err := emitSuppressed(tx, a, c.LegalEntityID, sup); err != nil {
			return err
		}
		kind, detail := "", ""
		switch sup.Reason {
		case SuppHardBounce, SuppEndpointInvalid:
			kind, detail = "ENDPOINT_REMEDIATION", "endpoint "+masked+" suppressed ("+sup.Reason+"); re-resolve or correct the recipient's contact"
		case SuppComplaint:
			kind, detail = "REPUTATION_ALERT", "complaint on "+masked+"; review template, audience and consent provenance (§7.4)"
		}
		if kind != "" {
			if _, err := tx.InsertException(&Exception{ExceptionID: uuid.NewString(), TenantID: tx.TenantID(),
				CommunicationID: c.CommunicationID, AttemptID: att.AttemptID, Kind: kind, Detail: detail, CreatedAt: now}); err != nil {
				return err
			}
		}
	}
	return nil
}

// progress moves the job, communication and notice after an attempt changed.
func (s *Service) progress(tx Tx, a Actor, c *Communication, job *DeliveryJob, att *Attempt, intent *Intent) error {
	now := s.now()
	job.LeasedUntil = nil
	switch att.State {
	case AttemptDelivered:
		job.State, job.ConcludedAt = JobCompleted, &now
		if err := tx.UpdateJob(job); err != nil {
			return err
		}
		c.LifecycleState, c.ConcludedAt = CommCompleted, &now
		if err := tx.UpdateCommunication(c); err != nil {
			return err
		}
		return s.noticeEvidenced(tx, a, c, job.Routes[att.RouteIndex].EvidenceCapability)
	case AttemptAccepted, AttemptPending:
		if job.State == JobCompleted {
			return nil
		}
		job.State, job.NextRunAt = JobAwaitingEvidence, now.Add(s.limits.EvidenceWindow)
		if err := tx.UpdateJob(job); err != nil {
			return err
		}
		if att.State == AttemptAccepted {
			return s.noticeEvidenced(tx, a, c, "E1")
		}
		return nil
	case AttemptUnknown:
		job.State = JobAwaitingResolution
		return tx.UpdateJob(job)
	case AttemptFailed, AttemptBounced:
		if att.State == AttemptFailed && att.Retryable && job.AttemptsOnRoute < job.MaxAttemptsPerRoute {
			backoff := 30 * time.Second << uint(maxInt(job.AttemptsOnRoute-1, 0))
			if backoff > 30*time.Minute {
				backoff = 30 * time.Minute
			}
			job.State, job.NextRunAt = JobQueued, now.Add(backoff)
			return tx.UpdateJob(job)
		}
		return s.advanceRoute(tx, a, c, job, intent, fmt.Sprintf("route %d (%s) ended %s: %s",
			job.RouteIndex, job.Routes[job.RouteIndex].Channel, att.State, att.FailureReason))
	}
	return tx.UpdateJob(job)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// advanceRoute is the §6.4 governed fallback: a new attempt on the next route
// of the same communication, only if policy allows it, the route keeps the
// required evidence class (NP-29) and residency (NP-30), and nothing is
// UNKNOWN (enforced again by the attempt trigger). Otherwise an exception.
func (s *Service) advanceRoute(tx Tx, a Actor, c *Communication, job *DeliveryJob, intent *Intent, why string) error {
	now := s.now()
	next := job.RouteIndex + 1
	kind, code, detail := "NO_ROUTE", NCD011NoCompliantChannel, why+"; no further route"
	if next < len(job.Routes) {
		r := job.Routes[next]
		switch {
		case !intent.FallbackAllowed:
			kind, code, detail = "FALLBACK_BLOCKED", NCD011NoCompliantChannel, why+"; the intent does not allow fallback"
		case r.EvidenceCapability.Rank() < RequiredRouteEvidence(intent.EvidenceClass):
			kind, code, detail = "FALLBACK_BLOCKED", NCD016EvidenceInsufficient,
				fmt.Sprintf("%s; fallback to %s would lower evidence below %s (NP-29)", why, r.Channel, intent.EvidenceClass)
		case !now.Before(job.ExpiresAt):
			kind, code, detail = "DELIVERY_EXPIRED", NCD015DeliveryExpired, why+"; the job expired before fallback"
		default:
			job.RouteIndex, job.AttemptsOnRoute = next, 0
			job.State, job.NextRunAt, job.ConcludedAt = JobQueued, now, nil
			job.LastDeferralReason = "fallback: " + why
			if err := tx.UpdateJob(job); err != nil {
				return err
			}
			if c.LifecycleState != CommDispatched {
				c.LifecycleState, c.ConcludedAt = CommDispatched, nil
				return tx.UpdateCommunication(c)
			}
			return nil
		}
	}
	job.State, job.ExceptionReason, job.ConcludedAt = JobException, string(code)+" "+detail, &now
	if err := tx.UpdateJob(job); err != nil {
		return err
	}
	if _, err := tx.InsertException(&Exception{ExceptionID: uuid.NewString(), TenantID: tx.TenantID(), CommunicationID: c.CommunicationID,
		Kind: kind, ReasonCode: code, Detail: detail, CreatedAt: now}); err != nil {
		return err
	}
	if c.LifecycleState != CommCompleted {
		c.LifecycleState, c.ConcludedAt = CommException, &now
		if err := tx.UpdateCommunication(c); err != nil {
			return err
		}
	}
	return s.noticeException(tx, a, c, detail)
}

// ── notice progression (§8.2) ───────────────────────────────────────────────

func (s *Service) noticeEvidenced(tx Tx, a Actor, c *Communication, capability Level) error {
	n, err := tx.GetNoticeByCommunication(c.CommunicationID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch n.State {
	case NoticePrepared, NoticeReady, NoticeInProgress, NoticeException:
	default:
		return nil
	}
	// Evidence must meet the notice's requirement; a provider acceptance
	// alone never evidences a high-evidence notice (§8.2, INV-11).
	if capability.Rank() < RequiredRouteEvidence(n.EvidenceRequirement) {
		if n.State != NoticeInProgress {
			n.State = NoticeInProgress
			return tx.UpdateNotice(n)
		}
		return nil
	}
	if n.AckRequirement == AckNone {
		n.State = NoticeSatisfiedByPolicy
		if n.RecordRequirement {
			n.RecordStatus = "PENDING"
		}
	} else {
		n.State = NoticeAckPending
	}
	return tx.UpdateNotice(n)
}

func (s *Service) noticeInProgress(tx Tx, c *Communication) error {
	n, err := tx.GetNoticeByCommunication(c.CommunicationID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if n.State == NoticePrepared || n.State == NoticeReady {
		n.State = NoticeInProgress
		return tx.UpdateNotice(n)
	}
	return nil
}

func (s *Service) noticeException(tx Tx, a Actor, c *Communication, detail string) error {
	n, err := tx.GetNoticeByCommunication(c.CommunicationID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch n.State {
	case NoticeAcknowledged, NoticeDeclined, NoticeDisputed, NoticeExpired, NoticeSatisfiedByPolicy, NoticeSuperseded:
		return nil
	}
	n.State = NoticeException
	if err := tx.UpdateNotice(n); err != nil {
		return err
	}
	return s.atRisk(tx, a, n, "delivery failed: "+detail)
}

func (s *Service) atRisk(tx Tx, a Actor, n *RegulatedNotice, reason string) error {
	now := s.now()
	n.AtRiskNotifiedAt = &now
	if err := tx.UpdateNotice(n); err != nil {
		return err
	}
	if _, err := tx.InsertException(&Exception{ExceptionID: uuid.NewString(), TenantID: tx.TenantID(), CommunicationID: n.CommunicationID,
		NoticeID: n.NoticeID, Kind: "DEADLINE_AT_RISK", ReasonCode: NCD016EvidenceInsufficient, Detail: reason, CreatedAt: now}); err != nil {
		return err
	}
	return emit(tx, a, EvtDeadlineAtRisk, n.LegalEntityID, n.CommunicationID, map[string]any{
		"notice_id": n.NoticeID, "notice_version": n.NoticeVersion, "communication_id": n.CommunicationID,
		"workflow_ref": n.WFCObligationRef, "deadline_at": n.DeadlineAt, "state": n.State,
		"deficiency": reason, "escalation_reason": reason,
	})
}

// ── job processing ──────────────────────────────────────────────────────────

func (s *Service) workerActor(tenantID, id string) Actor {
	return Actor{TenantID: tenantID, PrincipalID: workerPrincipal, CorrelationID: "ncd-worker-" + id}
}

// defer pushes a job back without dropping it (§6.5 backpressure). A deferral
// past expiry expires the job instead — "no stale send" (NP-52).
func (s *Service) deferJob(tx Tx, a Actor, c *Communication, job *DeliveryJob, until time.Time, reason string) error {
	if !until.Before(job.ExpiresAt) {
		return s.expireJob(tx, a, c, job, "deferred past expiry: "+reason)
	}
	job.NextRunAt, job.LastDeferralReason, job.LeasedUntil = until, reason, nil
	return tx.UpdateJob(job)
}

func (s *Service) expireJob(tx Tx, a Actor, c *Communication, job *DeliveryJob, why string) error {
	now := s.now()
	job.State, job.ExceptionReason, job.ConcludedAt, job.LeasedUntil = JobExpired, string(NCD015DeliveryExpired)+" "+why, &now, nil
	if err := tx.UpdateJob(job); err != nil {
		return err
	}
	if _, err := tx.InsertException(&Exception{ExceptionID: uuid.NewString(), TenantID: tx.TenantID(), CommunicationID: c.CommunicationID,
		Kind: "DELIVERY_EXPIRED", ReasonCode: NCD015DeliveryExpired,
		Detail: "the job expired before a provider submission; WFC/domain decides what happens next (NP-52): " + why, CreatedAt: now}); err != nil {
		return err
	}
	if c.LifecycleState == CommDispatched {
		c.LifecycleState, c.ConcludedAt = CommExpired, &now
		if err := tx.UpdateCommunication(c); err != nil {
			return err
		}
	}
	return s.noticeException(tx, a, c, "delivery expired")
}

type submission struct {
	attempt *Attempt
	binding Binding
	msg     Message
}

// ProcessJob runs one due job: gates, one attempt, its outcome.
func (s *Service) ProcessJob(ctx context.Context, ref WorkRef) error {
	a := s.workerActor(ref.TenantID, ref.ID)
	var sub *submission
	now := s.now()
	bindings, err := s.store.Bindings(ctx, now)
	if err != nil {
		return err
	}
	err = s.store.InTx(ctx, ref.TenantID, func(tx Tx) error {
		job, err := tx.ClaimJob(ref.ID, now, jobLease)
		if err != nil || job == nil {
			return err
		}
		c, err := tx.GetCommunication(job.CommunicationID, true)
		if err != nil {
			return err
		}
		if job.State == JobAwaitingEvidence {
			// The evidence window closed with no negative fact: the delivery
			// process is complete. The claim is still only what the evidence
			// supports (usually "provider accepted") — §3.3.
			job.State, job.ConcludedAt, job.LeasedUntil = JobCompleted, &now, nil
			if err := tx.UpdateJob(job); err != nil {
				return err
			}
			if c.LifecycleState == CommDispatched {
				c.LifecycleState, c.ConcludedAt = CommCompleted, &now
				return tx.UpdateCommunication(c)
			}
			return nil
		}
		if !now.Before(job.ExpiresAt) {
			return s.expireJob(tx, a, c, job, "expired while queued")
		}
		if job.NotBefore != nil && job.NotBefore.After(now) {
			return s.deferJob(tx, a, c, job, *job.NotBefore, "not_before")
		}
		intent, err := tx.GetIntent(c.IntentID, c.IntentVersion)
		if err != nil {
			return err
		}

		// Stream circuit breaker (§7.4). CRITICAL is never paused.
		if job.Stream != "CRITICAL" {
			st, reason, err := tx.GetStreamControl(job.Stream)
			if err != nil {
				return err
			}
			if st == "PAUSED" {
				return s.deferJob(tx, a, c, job, now.Add(s.limits.DeferStep), "stream "+job.Stream+" paused: "+reason)
			}
		}

		// Quotas (§6.5): deferred, never dropped. Security traffic has
		// protected capacity: exempt from tenant/intent quotas, bounded per
		// recipient.
		since := now.Add(-time.Hour)
		route := job.Routes[job.RouteIndex]
		if job.Stream == "CRITICAL" {
			n, err := tx.CountAttempts(since, c.RecipientPrincipalID, route.Channel, "")
			if err != nil {
				return err
			}
			if n >= s.limits.CriticalRecipientPerHour {
				return s.deferJob(tx, a, c, job, now.Add(s.limits.DeferStep), "per-recipient critical quota reached")
			}
		} else {
			for _, q := range []struct {
				n     int
				recip string
				ch    string
				in    string
				name  string
			}{
				{s.limits.TenantPerHour, "", "", "", "tenant"},
				{s.limits.IntentPerHour, "", "", c.IntentID, "intent"},
				{s.limits.RecipientPerHour, c.RecipientPrincipalID, route.Channel, "", "recipient/channel"},
			} {
				n, err := tx.CountAttempts(since, q.recip, q.ch, q.in)
				if err != nil {
					return err
				}
				if n >= q.n {
					return s.deferJob(tx, a, c, job, now.Add(s.limits.DeferStep), q.name+" hourly quota reached")
				}
			}
		}

		// Provider route (§6.3): the planned binding, or a certified
		// equivalent in the same failover group; otherwise wait (NP-28).
		b, why := routeBinding(bindings, route, intent, c.ResidencyRegions)
		if b == nil {
			return s.deferJob(tx, a, c, job, now.Add(s.limits.DeferStep), string(NCD013ProviderRouteUnavailable)+" "+why+"; queue preserved")
		}

		// The final submit gate (INV-24): suppression and preference as they
		// are at this instant, atomically with the attempt that follows.
		var emails []string
		if route.Channel == ChannelEmail {
			emails = []string{NormalizeEndpoint(ChannelEmail, route.Address)}
		}
		sups, err := tx.ActiveSuppressions(c.RecipientPrincipalID, []string{route.EndpointHash}, emails, now)
		if err != nil {
			return err
		}
		v := SuppressionVerdict(*intent, c.RecipientPrincipalID, route.Channel, route.EndpointHash, sups, now)
		if v.Restriction == nil {
			pref, err := tx.GetPreference(c.RecipientPrincipalID)
			if err != nil {
				return err
			}
			if pref != nil && contains(pref.MutedChannels, route.Channel) && !v.Overridden && !(intent.Mandatory && intent.PreferenceOverrideAllowed) {
				v.Restriction = &Restriction{Channel: route.Channel, Code: NCD010ChannelSuppressed, Detail: "muted by the recipient's current preference"}
			}
		}
		if v.Restriction != nil {
			if v.DeferUntil != nil {
				return s.deferJob(tx, a, c, job, *v.DeferUntil, v.Restriction.Detail)
			}
			return s.advanceRoute(tx, a, c, job, intent, "final gate: "+string(v.Restriction.Code)+" "+v.Restriction.Detail)
		}

		renders, err := tx.ListRenders(c.CommunicationID)
		if err != nil {
			return err
		}
		var render *RenderedContent
		for i := range renders {
			if renders[i].Channel == route.Channel {
				render = &renders[i]
			}
		}
		if render == nil {
			return s.advanceRoute(tx, a, c, job, intent, "no pinned content for "+route.Channel)
		}

		origin := job.Origin
		switch {
		case job.AttemptsOnRoute > 0:
			origin = "RETRY"
		case job.RouteIndex > 0:
			origin = "FALLBACK"
		}
		id := uuid.NewString()
		att := &Attempt{
			AttemptID: id, JobID: job.JobID, CommunicationID: c.CommunicationID, RouteIndex: job.RouteIndex,
			Channel: route.Channel, BindingID: b.BindingID, IntentID: intent.IntentID, IntentVersion: intent.Version,
			PurposeClass: intent.PurposeClass, RecipientPrincipalID: c.RecipientPrincipalID, Origin: origin,
			IdempotencyToken: "ncd-att-" + id, ContentHash: render.ContentHash, RenderID: render.RenderID,
			RecipientSnapshot: map[string]any{"channel": route.Channel, "endpoint_hash": route.EndpointHash,
				"endpoint_masked": route.EndpointMasked, "provenance": route.Provenance, "binding_id": b.BindingID},
			State: AttemptCreated, CreatedAt: now,
		}
		if err := tx.InsertAttempt(att); err != nil {
			if errors.Is(err, ErrUnknownOutstanding) {
				job.State, job.LeasedUntil = JobAwaitingResolution, nil
				return tx.UpdateJob(job)
			}
			return err
		}
		att.State, att.SubmittedAt = AttemptSubmitting, &now
		if err := tx.UpdateAttempt(att); err != nil {
			return err
		}
		job.AttemptsOnRoute++
		if err := tx.UpdateJob(job); err != nil {
			return err
		}
		if err := emit(tx, a, EvtAttemptCreated, c.LegalEntityID, c.CommunicationID, map[string]any{
			"attempt_id": att.AttemptID, "job_id": job.JobID, "communication_id": c.CommunicationID,
			"provider_binding": b.BindingID, "channel": route.Channel, "payload_hash": att.ContentHash,
			"recipient_endpoint_ref": route.EndpointHash, "origin": origin, "idempotency_token": att.IdempotencyToken,
		}); err != nil {
			return err
		}
		if err := s.noticeInProgress(tx, c); err != nil {
			return err
		}
		to := route.Address
		if route.Channel == ChannelInApp {
			to = c.RecipientPrincipalID
		}
		sub = &submission{attempt: att, binding: *b, msg: Message{
			TenantID: ref.TenantID, LegalEntityID: c.LegalEntityID, CommunicationID: c.CommunicationID, AttemptID: att.AttemptID,
			IdempotencyToken: att.IdempotencyToken, Channel: route.Channel, BindingID: b.BindingID, To: to,
			RecipientPrincipalID: c.RecipientPrincipalID, Subject: render.Subject, Body: render.Body,
			CorrelationID: a.CorrelationID, SenderIdentity: b.SenderIdentity, PurposeClass: att.PurposeClass,
		}}
		return nil
	})
	if err != nil || sub == nil {
		return err
	}

	// The provider call happens between two transactions, never inside one.
	// The attempt is already committed SUBMITTING, so a crash here leaves a
	// row the stranded sweep turns UNKNOWN — never one that is silently
	// resent.
	started := time.Now()
	outcome := s.transport.Submit(context.WithoutCancel(ctx), sub.binding, sub.msg)
	norm := NormalizeSubmit(sub.msg.Channel, outcome)
	s.metrics.AttemptSubmitted(sub.msg.Channel, sub.binding.BindingID, string(norm.AttemptState), time.Since(started))
	success := outcome.Accepted || outcome.DeliveredNow
	if !success && (outcome.Retryable || outcome.Unknown) {
		_ = s.store.RecordBindingResult(ctx, sub.binding.BindingID, false, s.limits.CircuitFailureThreshold)
	} else if success {
		_ = s.store.RecordBindingResult(ctx, sub.binding.BindingID, true, s.limits.CircuitFailureThreshold)
	}
	return s.store.InTx(ctx, ref.TenantID, func(tx Tx) error {
		att, err := tx.GetAttempt(sub.attempt.AttemptID)
		if err != nil {
			return err
		}
		att.Retryable = outcome.Retryable
		if !success {
			att.FailureReason = outcome.Reason
		}
		if outcome.Unknown {
			att.FailureReason = outcome.Reason
		}
		return s.applyFact(ctx, tx, a, att, norm, factMeta{source: "PROVIDER_API", actor: workerPrincipal,
			bindingID: sub.binding.BindingID, providerMessageID: outcome.ProviderMessageID, observedAt: s.now(),
			details: map[string]any{"provider": outcome.ProviderName, "reason": outcome.Reason}})
	})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// routeBinding returns the route's binding when usable, else a certified
// equivalent in the same failover group (§6.3 "Failover: only to
// pre-certified equivalent route").
func routeBinding(bindings []Binding, r Route, intent *Intent, regions []string) (*Binding, string) {
	var group []Binding
	for _, b := range bindings {
		if b.BindingID == r.BindingID && b.Status == "ACTIVE" && b.Certified && b.Health != "CIRCUIT_OPEN" {
			return &b, ""
		}
		if b.FailoverGroup == r.FailoverGroup && b.Channel == r.Channel && b.BindingID != r.BindingID {
			group = append(group, b)
		}
	}
	b, _, why := selectBinding(group, r.Channel, RequiredRouteEvidence(intent.EvidenceClass), regions)
	if b == nil {
		return nil, "binding " + r.BindingID + " unavailable and no certified equivalent: " + why
	}
	return b, ""
}

// ── sweeps ──────────────────────────────────────────────────────────────────

// SweepUnknown turns UNKNOWN attempts past their resolution deadline into a
// human exception. It never resends (§6.2, INV-13).
func (s *Service) SweepUnknown(ctx context.Context, ref WorkRef) error {
	a := s.workerActor(ref.TenantID, ref.ID)
	return s.store.InTx(ctx, ref.TenantID, func(tx Tx) error {
		att, err := tx.GetAttempt(ref.ID)
		if err != nil || att.State != AttemptUnknown {
			return err
		}
		_, err = tx.InsertException(&Exception{ExceptionID: uuid.NewString(), TenantID: ref.TenantID,
			CommunicationID: att.CommunicationID, AttemptID: att.AttemptID, Kind: "UNKNOWN_UNRESOLVED", ReasonCode: NCD014DeliveryAttemptUnknown,
			Detail: "the attempt's outcome is still UNKNOWN past its resolution deadline; query the provider by attempt token " +
				att.IdempotencyToken + " and resolve the ORIGINAL attempt (§13.2 stuck UNKNOWN)", CreatedAt: s.now()})
		_ = a
		return err
	})
}

// MarkStranded turns an attempt left SUBMITTING by a lost process into
// UNKNOWN: it may have reached the provider, so it must be reconciled, never
// silently resent.
func (s *Service) MarkStranded(ctx context.Context, ref WorkRef) error {
	a := s.workerActor(ref.TenantID, ref.ID)
	return s.store.InTx(ctx, ref.TenantID, func(tx Tx) error {
		att, err := tx.GetAttempt(ref.ID)
		if err != nil || att.State != AttemptSubmitting {
			return err
		}
		att.FailureReason = "the submitting process ended before recording the provider's answer"
		norm := NormalizeSubmit(att.Channel, SubmitOutcome{Unknown: true})
		return s.applyFact(ctx, tx, a, att, norm, factMeta{source: "SYSTEM", actor: workerPrincipal, observedAt: s.now(),
			details: map[string]any{"reason": att.FailureReason}})
	})
}

func (s *Service) logErr(msg string, err error, fields ...zap.Field) {
	if err != nil {
		s.log.Warn(msg, append(fields, zap.Error(err))...)
	}
}
