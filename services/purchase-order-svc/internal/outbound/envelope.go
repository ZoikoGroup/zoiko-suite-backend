// Package outbound stamps the canonical request envelope on this service's
// calls to other services.
//
// Copied from procurement-workflow-svc's internal/clients/envelope.go (not
// imported: services do not share code across module boundaries). The read
// clients used to send only X-Tenant-Id and X-Principal-Id, so the callee could
// not correlate the call with the request that caused it and logged it under
// no request id and no source channel.
package outbound

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"zoiko.io/purchase-order-svc/internal/middleware"
)

// SetEnvelope sets tenant, principal (when known), legal entity (when known),
// source channel, a fresh X-Request-Id and the inbound request's
// X-Correlation-ID (a new one when the inbound request carried none).
func SetEnvelope(ctx context.Context, req *http.Request, tenantID, principalID, legalEntityID string) {
	req.Header.Set("X-Tenant-Id", tenantID)
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	if legalEntityID != "" {
		req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	}
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Request-Id", uuid.NewString())
	correlationID := middleware.CorrelationIDFromContext(ctx)
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set("X-Correlation-ID", correlationID)
}
