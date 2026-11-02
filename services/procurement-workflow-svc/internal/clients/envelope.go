package clients

import (
	"net/http"

	"github.com/google/uuid"
)

// setEnvelope stamps the canonical request envelope that spend-controls-svc and
// purchase-order-svc enforce on writes (write-strict by default). Sending only
// tenant/principal made both targets answer 400 envelope_incomplete, so every
// procurement case create and every issue-order failed with a 503.
//
// idempotencyKey must be stable for one logical operation so a retry replays
// the target's stored outcome instead of executing it twice.
func setEnvelope(req *http.Request, tenantID, principalID, legalEntityID, correlationID, idempotencyKey string) {
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Request-Id", uuid.NewString())
	if correlationID != "" {
		req.Header.Set("X-Correlation-ID", correlationID)
	}
	req.Header.Set("Idempotency-Key", idempotencyKey)
}
