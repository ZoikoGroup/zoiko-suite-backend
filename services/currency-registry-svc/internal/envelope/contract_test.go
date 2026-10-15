package envelope_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/currency-registry-svc/internal/envelope"
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

// Currencies are global reference data: a command needs no legal entity, but
// it does need an Idempotency-Key (INV-08).
func TestServicePolicy_NoLegalEntityRequired_IdempotencyRequiredOnWrite(t *testing.T) {
	p := envelope.ServicePolicy()

	w := baseRequest(http.MethodPost, "/v1/currencies/x:activate")
	w.Header.Set(envelope.HeaderIdempotencyKey, "k-1")
	assert.Nil(t, p.Validate(envelope.Parse(w), w), "no legal_entity_id is required for a global registry")

	noKey := baseRequest(http.MethodPost, "/v1/currencies/x:activate")
	err := p.Validate(envelope.Parse(noKey), noKey)
	require.NotNil(t, err)
	assert.Equal(t, "idempotency_key", err.Violations[0].Field)

	read := baseRequest(http.MethodGet, "/v1/currencies:validate?code=AAA")
	assert.Nil(t, p.Validate(envelope.Parse(read), read))
}
