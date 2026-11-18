package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/schema-registry-svc/internal/domain"
)

func ownedLatest() *stubStore {
	return &stubStore{latest: &domain.EventSchema{
		EventName:     "accounting.journal.posted",
		Version:       1,
		JSONSchema:    json.RawMessage(`{"properties":{"id":{"type":"string"}},"required":["id"]}`),
		OwningService: "accounting-kernel",
	}}
}

func register(t *testing.T, s *stubStore, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/accounting.journal.posted/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	newRouter(s).ServeHTTP(rec, req)
	return rec
}

// EV-T23: a second producer cannot take over an event by registering a version.
func TestRegisterVersion_DifferentOwnerRefused_EV_T23(t *testing.T) {
	s := ownedLatest()
	rec := register(t, s, `{"json_schema":{"properties":{"id":{"type":"string"}},"required":["id"]},"owning_service":"rogue-svc"}`)
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "ownership")
	assert.Nil(t, s.insertedArg, "nothing may be stored")
}

// NONE relaxes schema evolution only; it is not a way to change the owner.
func TestRegisterVersion_OwnerChangeRefusedEvenUnderNone(t *testing.T) {
	s := ownedLatest()
	rec := register(t, s, `{"json_schema":{"properties":{"x":{"type":"string"}}},"compatibility_mode":"NONE","owning_service":"rogue-svc"}`)
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Nil(t, s.insertedArg)
}

func TestRegisterVersion_OmittedOwnerInheritsRegisteredOwner(t *testing.T) {
	s := ownedLatest()
	rec := register(t, s, `{"json_schema":{"properties":{"id":{"type":"string"},"memo":{"type":"string"}},"required":["id"]}}`)
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "accounting-kernel", s.insertedArg.OwningService)
}

func TestRegisterVersion_SameOwnerAccepted(t *testing.T) {
	s := ownedLatest()
	rec := register(t, s, `{"json_schema":{"properties":{"id":{"type":"string"}},"required":["id"]},"owning_service":"accounting-kernel"}`)
	require.Equal(t, http.StatusCreated, rec.Code)
}

// A legacy event with no recorded owner (rows predate the column) may gain one.
func TestRegisterVersion_LegacyUnownedEventMayGainOwner(t *testing.T) {
	s := &stubStore{latest: &domain.EventSchema{
		EventName: "accounting.journal.posted", Version: 1,
		JSONSchema: json.RawMessage(`{"properties":{"id":{"type":"string"}},"required":["id"]}`),
	}}
	rec := register(t, s, `{"json_schema":{"properties":{"id":{"type":"string"}},"required":["id"]},"owning_service":"accounting-kernel"}`)
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "accounting-kernel", s.insertedArg.OwningService)
}

// The catalogue's kebab-case names are registrable; malformed ones are not.
func TestValidEventName_CanonicalKebabNames(t *testing.T) {
	assert.True(t, domain.ValidEventName("com.zoikosuite.ar.customer-invoice.issued"))
	assert.True(t, domain.ValidEventName("jurisdiction.rule.updated"))
	for _, bad := range []string{"com.zoikosuite.-x.y", "a.b-", "a.b--c", "Com.x.y", "a..b", "a.b-.c"} {
		assert.False(t, domain.ValidEventName(bad), bad)
	}
}
