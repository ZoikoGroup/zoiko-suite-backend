package ncd

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Accessors the HTTP layer uses to authorize against a stored object's own
// legal entity before acting on it (fetch → authorize → act).

// IntentEntity returns the legal entity an intent belongs to.
func (s *Service) IntentEntity(ctx context.Context, a Actor, intentID string) (string, error) {
	var le string
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		i, err := tx.GetIntent(intentID, 0)
		if err != nil {
			return mapNotFound(err, "intent_not_found", "intent %s", intentID)
		}
		le = i.LegalEntityID
		return nil
	})
	return le, err
}

// TemplateEntity returns the legal entity a template version belongs to.
func (s *Service) TemplateEntity(ctx context.Context, a Actor, id string) (string, error) {
	tv, err := s.GetTemplate(ctx, a, id)
	if err != nil {
		return "", err
	}
	return tv.LegalEntityID, nil
}

// GetPlan returns a recipient plan.
func (s *Service) GetPlan(ctx context.Context, a Actor, id string) (*RecipientPlan, error) {
	var p *RecipientPlan
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		p, err = tx.GetPlan(id)
		return mapNotFound(err, "plan_not_found", "recipient plan %s", id)
	})
	return p, err
}

// GetBulk returns a bulk send.
func (s *Service) GetBulk(ctx context.Context, a Actor, id string) (*BulkSend, error) {
	var b *BulkSend
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		b, err = tx.GetBulk(id, false)
		return mapNotFound(err, "bulk_not_found", "bulk send %s", id)
	})
	return b, err
}

// ── legacy integration ──────────────────────────────────────────────────────

// RecordInAppOpened records that the recipient opened an NCD in-app notice in
// their inbox (§7.1 "In-app event: OPENED"). The inbox row's id is the attempt
// id. Opening is display evidence, never an acknowledgment (§8.3).
func (s *Service) RecordInAppOpened(ctx context.Context, a Actor, communicationID, attemptID string, at time.Time) error {
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		att, err := tx.GetAttempt(attemptID)
		if err != nil {
			return err
		}
		if att.CommunicationID != communicationID || att.RecipientPrincipalID != a.PrincipalID {
			return nil
		}
		c, err := tx.GetCommunication(communicationID, false)
		if err != nil {
			return err
		}
		return s.recordEvidence(tx, a, c, &Evidence{EvidenceID: uuid.NewString(), AttemptID: attemptID, EvidenceType: "OPENED",
			NormalizedState: "OPENED", Source: "IN_APP", Confidence: "HIGH", ActorPrincipalID: a.PrincipalID, ObservedAt: at,
			DoesNotProve:    "comprehension, agreement or acknowledgment (§8.3); only that the authenticated recipient opened it",
			ProviderEventID: "", BindingID: att.BindingID})
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// LegacySendSuppressed is the suppression gate for the pre-NCD send path
// (POST /v1/notifications): that path names no intent, so it is treated as a
// TRANSACTIONAL communication — a hard bounce, security hold or legal
// restriction blocks it; a marketing opt-out does not (§5.3, INV-24).
func (s *Service) LegacySendSuppressed(ctx context.Context, tenantID, recipientPrincipalID, channel, address string) (*Restriction, error) {
	var r *Restriction
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		hash := EndpointHash(channel, address)
		if channel == ChannelInApp {
			hash = EndpointHash(ChannelInApp, recipientPrincipalID)
		}
		sups, err := tx.ActiveSuppressions(recipientPrincipalID, []string{hash}, nil, s.now())
		if err != nil {
			return err
		}
		v := SuppressionVerdict(Intent{PurposeClass: PurposeTransactional}, recipientPrincipalID, channel, hash, sups, s.now())
		r = v.Restriction
		return nil
	})
	return r, err
}
