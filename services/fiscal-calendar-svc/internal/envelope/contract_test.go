package envelope_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/fiscal-calendar-svc/internal/envelope"
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

// Fiscal calendars are entity-scoped records: a command needs a legal entity
// (spec section 3) and an Idempotency-Key (INV-08).
func TestServicePolicy_LegalEntityRequired_IdempotencyRequiredOnWrite(t *testing.T) {
	p := envelope.ServicePolicy()

	w := baseRequest(http.MethodPost, "/v1/fiscal-calendars")
	w.Header.Set(envelope.HeaderIdempotencyKey, "k-1")
	err := p.Validate(envelope.Parse(w), w)
	require.NotNil(t, err)
	assert.Equal(t, "legal_entity_id", err.Violations[0].Field)

	w.Header.Set(envelope.HeaderLegalEntityID, "entity-1")
	assert.Nil(t, p.Validate(envelope.Parse(w), w))

	noKey := baseRequest(http.MethodPost, "/v1/fiscal-calendars")
	noKey.Header.Set(envelope.HeaderLegalEntityID, "entity-1")
	err = p.Validate(envelope.Parse(noKey), noKey)
	require.NotNil(t, err)
	assert.Equal(t, "idempotency_key", err.Violations[0].Field)
}
