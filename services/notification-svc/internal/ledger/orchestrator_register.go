package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

// ErrRegisterConflict means the register already holds a communication for this
// event. The ledger's own deduplication should have caught a replay first, so
// reaching here means the two disagree; delivering again could send twice.
var ErrRegisterConflict = errors.New("the communication register already holds this event")

// registerCommunication creates the register row for one ledger delivery, links it
// to its intent, and sets the submission marker. The row's idempotency key is
// derived from the event, so a replay of the same event can never create a second.
//
// Order matters: create, then link, then mark. The marker is the last thing before
// the provider call, so an attempt that is lost afterwards is recognised as
// "may have reached the provider" and is never blindly re-sent (Y-001 6.2).
func (o *Orchestrator) registerCommunication(ctx context.Context, tenantID string, intent *MessageIntent,
	n domain.Notification, addressSource, dedupKey, templateKey string) (*domain.Notification, error) {

	row := &domain.Notification{
		NotificationID:         uuid.NewString(),
		TenantID:               tenantID,
		LegalEntityID:          n.LegalEntityID,
		RecipientPrincipalID:   n.RecipientPrincipalID,
		RecipientAddress:       n.RecipientAddress,
		RecipientAddressSource: addressSource,
		Channel:                n.Channel,
		Subject:                n.Subject,
		Body:                   n.Body,
		Status:                 domain.StatusPending,
		SourceEventType:        n.SourceEventType,
		SourceReference:        n.SourceReference,
		CorrelationID:          n.CorrelationID,
		CreatedByPrincipalID:   n.CreatedByPrincipalID,
		CreatedAt:              time.Now().UTC(),
		PurposeContext:         templateKey,
		IdempotencyKey:         "ledger:" + dedupKey,
	}
	created, err := o.register.CreateNotification(ctx, row)
	if err != nil {
		return nil, fmt.Errorf("create register row: %w", err)
	}
	if !created {
		return nil, ErrRegisterConflict
	}
	if err := o.register.LinkIntentToNotification(ctx, intent.MessageIntentID, row.NotificationID); err != nil {
		return nil, fmt.Errorf("link intent to register row: %w", err)
	}
	if err := o.register.BeginSubmission(ctx, row.NotificationID, tenantID, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("mark submission: %w", err)
	}
	return row, nil
}

// concludeCommunication records what the provider call achieved on the register
// row. It runs on a context that outlives the request: once the provider has been
// called, the outcome is a fact about the outside world and the caller hanging up
// does not un-send an email (the direct path does the same).
//
// The ledger pipeline has no retry, so a failure CONCLUDES the row (FAILED) rather
// than scheduling one, and an ambiguous outcome becomes PENDING_UNKNOWN to be
// resolved against the provider, never FAILED and never re-sent blind.
func (o *Orchestrator) concludeCommunication(ctx context.Context, tenantID string, row *domain.Notification,
	outcome domain.DeliveryOutcome, caller, correlationID string) {

	cctx, cancel := context.WithTimeout(svcmiddleware.WithTenant(context.WithoutCancel(ctx), tenantID), 10*time.Second)
	defer cancel()

	at := time.Now().UTC()
	meta := domain.AttemptMeta{
		Origin:           domain.AttemptOriginRequest,
		ProviderName:     outcome.ProviderName,
		Retryable:        outcome.Retryable,
		ActorPrincipalID: caller,
	}
	reason := outcome.Reason
	if reason == "" {
		reason = "delivery was not accepted by the provider"
	}

	var err error
	switch {
	case outcome.Unknown:
		err = o.register.MarkOutcomeUnknown(cctx, row.NotificationID, tenantID, reason, at, correlationID, meta)
	case outcome.Delivered:
		err = o.register.CompleteDelivery(cctx, row.NotificationID, domain.StatusSent, "", outcome.ProviderResponse, &at, correlationID, meta)
	default:
		err = o.register.CompleteDelivery(cctx, row.NotificationID, domain.StatusFailed, reason, "", &at, correlationID, meta)
	}
	if err != nil {
		// The row stays PENDING with its submission marker, which the stranded
		// sweep turns into PENDING_UNKNOWN rather than re-sending.
		o.log.Error("failed to record the delivery outcome on the register row",
			zap.String("notification_id", row.NotificationID), zap.String("tenant_id", tenantID), zap.Error(err))
	}
}
