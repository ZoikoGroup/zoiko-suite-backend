package ncd

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// GetApproval reads one approval (for authorization and display).
func (s *Service) GetApproval(ctx context.Context, a Actor, id string) (*Approval, error) {
	var out *Approval
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		out, err = tx.GetApproval(id, false)
		return mapNotFound(err, "approval_not_found", "approval %s", id)
	})
	return out, err
}

// DecideApproval approves or rejects a maker-checker request. The decider is
// the caller and may not be the requester; an approval EXECUTES its action in
// the same transaction, so "approved" and "done" cannot drift apart.
func (s *Service) DecideApproval(ctx context.Context, a Actor, id string, approve bool, note string) (*Approval, any, error) {
	var out *Approval
	var result any
	var kick bool
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		appr, err := tx.GetApproval(id, true)
		if err != nil {
			return mapNotFound(err, "approval_not_found", "approval %s", id)
		}
		if appr.Status != "PENDING" {
			return Conflict("already_decided", "approval %s is %s", id, appr.Status)
		}
		if appr.RequestedBy == a.PrincipalID {
			return Forbidden("self_approval_forbidden", "the requester of an approval cannot decide it")
		}
		now := s.now()
		appr.DecidedBy, appr.DecidedAt, appr.DecisionNote = a.PrincipalID, &now, note
		if !approve {
			appr.Status = "REJECTED"
			out = appr
			return tx.UpdateApproval(appr)
		}
		appr.Status = "APPROVED"
		if err := tx.UpdateApproval(appr); err != nil {
			return err
		}
		out = appr
		switch appr.Kind {
		case ApprovalResend:
			c, err := tx.GetCommunication(appr.TargetID, true)
			if err != nil {
				return err
			}
			intent, err := tx.GetIntent(c.IntentID, c.IntentVersion)
			if err != nil {
				return err
			}
			if err := resendable(tx, c); err != nil {
				return err
			}
			job, err := s.createResendJob(ctx, tx, a, c, intent, appr.Reason+" (approved by "+a.PrincipalID+")")
			if err != nil {
				return err
			}
			result, kick = job, true
		case ApprovalManualEvidence:
			if err := s.applyManualEvidence(tx, a, appr); err != nil {
				return err
			}
		case ApprovalSuppressionLift:
			ev, _ := appr.Payload["evidence_ref"].(string)
			if err := tx.LiftSuppression(appr.TargetID, appr.RequestedBy, ev, a.PrincipalID, now); err != nil {
				return err
			}
			sup, err := tx.GetSuppression(appr.TargetID)
			if err != nil {
				return err
			}
			result = sup
		case ApprovalBulkSend:
			// Approval only; dispatch presents the audience hash separately.
		}
		return nil
	})
	if kick {
		s.Kick()
	}
	return out, result, err
}

// ── governed bulk sends (§6.5) ──────────────────────────────────────────────

// BulkInput is POST /v1/bulk-sends.
type BulkInput struct {
	IntentID      string            `json:"intent_id"`
	LegalEntityID string            `json:"legal_entity_id"`
	SourceEventID string            `json:"source_event_id"`
	Recipients    []string          `json:"recipients"`
	Variables     map[string]string `json:"variables,omitempty"`
	Locale        string            `json:"locale"`
	Reason        string            `json:"reason,omitempty"`
}

// AudienceHash identifies an exact recipient set, order-independent.
func AudienceHash(recipients []string) string {
	rs := append([]string{}, recipients...)
	sort.Strings(rs)
	return SHA256Hex(append([]string{"audience"}, rs...)...)
}

// PreviewBulk records the audience snapshot and its count. Above the approval
// threshold — and always for S3 content — dispatch needs a second principal's
// approval of exactly this audience (NP-50, OD-13).
func (s *Service) PreviewBulk(ctx context.Context, a Actor, in BulkInput) (*BulkSend, *Approval, error) {
	if in.IntentID == "" || in.LegalEntityID == "" || in.SourceEventID == "" || len(in.Recipients) == 0 || in.Locale == "" {
		return nil, nil, Invalid("missing_fields", "intent_id, legal_entity_id, source_event_id, locale and recipients are required")
	}
	seen := map[string]bool{}
	var rs []string
	for _, r := range in.Recipients {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		// NP-49: a recipient reference that names another tenant is refused;
		// expansion is tenant-scoped by construction.
		if strings.Contains(r, ":") {
			return nil, nil, Refused(Refuse(NCD020CrossTenantRecipientBlock, "recipient "+r+" is a qualified reference; bulk audiences are principals of the caller's tenant only"))
		}
		if !seen[r] {
			seen[r] = true
			rs = append(rs, r)
		}
	}
	sort.Strings(rs)
	var b *BulkSend
	var appr *Approval
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		intent, err := effectiveIntentNow(tx, in.IntentID, s.now())
		if err != nil {
			return err
		}
		if !intent.BulkAllowed {
			return Conflict("bulk_not_allowed", "intent %s does not allow bulk sends", in.IntentID)
		}
		if intent.LegalEntityID != in.LegalEntityID {
			return Invalid("legal_entity_mismatch", "intent %s belongs to another legal entity", in.IntentID)
		}
		b = &BulkSend{BulkID: uuid.NewString(), TenantID: a.TenantID, LegalEntityID: in.LegalEntityID, IntentID: in.IntentID,
			SourceEventID: in.SourceEventID, Recipients: rs, AudienceCount: len(rs), AudienceHash: AudienceHash(rs),
			Variables: in.Variables, Locale: in.Locale, Status: "PREVIEWED", RequestedBy: a.PrincipalID, CreatedAt: s.now()}
		b.RequiresApproval = len(rs) >= s.limits.BulkApprovalThreshold || intent.Sensitivity.Rank() >= 3
		if b.RequiresApproval {
			reason := in.Reason
			if reason == "" {
				reason = "bulk send of " + itoa(len(rs)) + " recipients"
			}
			appr = &Approval{ApprovalID: uuid.NewString(), TenantID: a.TenantID, LegalEntityID: in.LegalEntityID, Kind: ApprovalBulkSend,
				TargetID: b.BulkID, Payload: map[string]any{"audience_count": b.AudienceCount, "audience_hash": b.AudienceHash},
				Reason: reason, Status: "PENDING", RequestedBy: a.PrincipalID, RequestedAt: s.now()}
			if err := tx.InsertApproval(appr); err != nil {
				return err
			}
			b.ApprovalID = appr.ApprovalID
		}
		return tx.InsertBulk(b)
	})
	return b, appr, err
}

// BulkResult reports each recipient's communication.
type BulkResult struct {
	RecipientPrincipalID string   `json:"recipient_principal_id"`
	CommunicationID      string   `json:"communication_id,omitempty"`
	State                string   `json:"lifecycle_state,omitempty"`
	Refusal              *Refusal `json:"refusal,omitempty"`
}

// DispatchBulk dispatches a previewed audience. The caller presents the
// audience hash it approved; any other hash is refused (NP-50).
func (s *Service) DispatchBulk(ctx context.Context, a Actor, id, audienceHash string) (*BulkSend, []BulkResult, error) {
	var b *BulkSend
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		b, err = tx.GetBulk(id, true)
		if err != nil {
			return mapNotFound(err, "bulk_not_found", "bulk send %s", id)
		}
		if b.Status != "PREVIEWED" {
			return Conflict("already_dispatched", "bulk send %s was already dispatched", id)
		}
		if audienceHash != b.AudienceHash {
			return Conflict("audience_changed", "the audience hash does not match the previewed audience; preview again (NP-50)")
		}
		if b.RequiresApproval {
			appr, err := tx.GetApproval(b.ApprovalID, false)
			if err != nil {
				return err
			}
			if appr.Status != "APPROVED" {
				return Conflict("approval_required", "bulk send %s needs approval %s (%s)", id, appr.ApprovalID, appr.Status)
			}
		}
		now := s.now()
		b.Status, b.DispatchedAt = "DISPATCHED", &now
		return tx.UpdateBulk(b)
	})
	if err != nil {
		return nil, nil, err
	}
	results := make([]BulkResult, 0, len(b.Recipients))
	for _, r := range b.Recipients {
		res := BulkResult{RecipientPrincipalID: r}
		c, _, err := s.CreateCommunication(ctx, a, CommunicationInput{IntentID: b.IntentID, LegalEntityID: b.LegalEntityID,
			RecipientPrincipalID: r, Locale: b.Locale, Variables: b.Variables, SourceEventID: b.SourceEventID, bulkID: b.BulkID})
		if err != nil {
			res.Refusal = Refuse(NCD006RecipientUnresolved, AsError(err).Detail)
			results = append(results, res)
			continue
		}
		res.CommunicationID = c.CommunicationID
		p, err := s.Prepare(ctx, a, c.CommunicationID)
		if err != nil {
			res.Refusal = Refuse(NCD006RecipientUnresolved, AsError(err).Detail)
		} else if p.Refusal != nil {
			res.Refusal, res.State = p.Refusal, p.Communication.LifecycleState
		} else if cc, _, err := s.Dispatch(ctx, a, c.CommunicationID); err != nil {
			res.Refusal = AsError(err).Refusal
		} else {
			res.State = cc.LifecycleState
		}
		results = append(results, res)
	}
	return b, results, nil
}

// ── worker ──────────────────────────────────────────────────────────────────

// Run drives the plane until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	lastReputation := time.Time{}
	for {
		s.RunOnce(ctx)
		if s.now().Sub(lastReputation) >= 5*time.Minute {
			s.logErr("reputation evaluation failed", s.EvaluateReputation(ctx))
			lastReputation = s.now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

// RunOnce performs one pass of every sweep. Exported for tests.
func (s *Service) RunOnce(ctx context.Context) {
	now := s.now()
	if refs, err := s.store.StaleSubmitting(ctx, now.Add(-jobLease), 50); err == nil {
		for _, r := range refs {
			s.logErr("stranded attempt sweep failed", s.MarkStranded(ctx, r))
		}
	}
	if refs, err := s.store.DueJobs(ctx, now, 50); err == nil {
		for _, r := range refs {
			s.logErr("job processing failed", s.ProcessJob(ctx, r))
		}
	} else {
		s.logErr("due job discovery failed", err)
	}
	if refs, err := s.store.UnknownPastDue(ctx, now, 50); err == nil {
		for _, r := range refs {
			s.logErr("unknown sweep failed", s.SweepUnknown(ctx, r))
		}
	}
	if refs, err := s.store.NoticesDue(ctx, now.Add(s.limits.DeadlineAtRiskWindow), 50); err == nil {
		for _, r := range refs {
			s.logErr("notice sweep failed", s.SweepNotice(ctx, r))
		}
	}
	if now.Sub(s.lastBacklog) >= backlogEvery {
		if b, err := s.store.Backlog(ctx, now); err == nil {
			s.metrics.Backlog(b)
			s.lastBacklog = now
		} else {
			s.logErr("backlog snapshot failed", err)
		}
	}
}

// backlogEvery bounds how often the §13.1 backlog is re-read: often enough to
// alert on, cheap enough to run beside a two-second worker.
const backlogEvery = 15 * time.Second
