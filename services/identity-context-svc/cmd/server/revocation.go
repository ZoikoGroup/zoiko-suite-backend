package main

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/session"
	"zoiko.io/identity-context-svc/internal/siem"
)

// sessionRevokedPublisher is the slice of events.Publisher the cap hook uses.
type sessionRevokedPublisher interface {
	PublishSessionInvalidated(ctx context.Context, sessionContextID, principalID string,
		reason domain.InvalidationReason, correlationID string) error
}

// capRevocationHook is what the session-concurrency cap calls for every
// session it ends: the revocation leaves the service through the outbox as
// identity.session.revoked (and the legacy session.invalidated), and is
// streamed to SIEM as a HIGH security signal (IC-4).
func capRevocationHook(pub sessionRevokedPublisher, siemClient *siem.Client, log *zap.Logger) session.RevocationHook {
	return func(ctx context.Context, sessionContextID, principalID, tenantID string, reason domain.InvalidationReason, correlationID string) {
		if err := pub.PublishSessionInvalidated(ctx, sessionContextID, principalID, reason, correlationID); err != nil {
			log.Error("session revocation event not recorded", zap.String("session_context_id", sessionContextID), zap.Error(err))
		}
		siemClient.Stream(ctx, tenantID, "identity.session.revoked", siem.SeverityHigh,
			fmt.Sprintf("session %s revoked: %s", sessionContextID, reason))
	}
}
