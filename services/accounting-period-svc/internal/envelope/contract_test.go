package envelope_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounting-period-svc/internal/envelope"
)

func baseRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set(envelope.HeaderTenantID, "tenant-a")
	r.Header.Set(envelope.HeaderActorSubjectID, "steward-1")
	r.Header.Set(envelope.HeaderRequestID, "req-1")
	r.Header.Set(envelope.HeaderCorrelationID, "corr-1")
	r.Header.Set(envelope.HeaderSourceChannel, "api")
	return r
}

// The legal entity travels in the request body, so no header is required; but
// it does need an Idempotency-Key (INV-08).
func TestServicePolicy_IdempotencyRequiredOnWrite(t *testing.T) {
	p := envelope.ServicePolicy()

	w := baseRequest(http.MethodPost, "/v1/accounting-periods/x:hard-close")
	w.Header.Set(envelope.HeaderIdempotencyKey, "k-1")
	assert.Nil(t, p.Validate(envelope.Parse(w), w), "no legal_entity_id is required header for period commands")

	noKey := baseRequest(http.MethodPost, "/v1/accounting-periods/x:hard-close")
	err := p.Validate(envelope.Parse(noKey), noKey)
	require.NotNil(t, err)
	assert.Equal(t, "idempotency_key", err.Violations[0].Field)

	read := baseRequest(http.MethodGet, "/v1/accounting-periods:resolve?legal_entity_id=e&date=2026-01-01")
	assert.Nil(t, p.Validate(envelope.Parse(read), read))
}
