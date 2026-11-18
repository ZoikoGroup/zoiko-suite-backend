package handler_test

// Pins behaviour that openapi.yaml now documents: deterministic 409 violations,
// the size limit, and the compatibility-checker subset (a `type` given as an
// array is refused at registration).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/schema-registry-svc/internal/domain"
)

func TestRegisterVersion_ManyViolations_ComeBackInTheSameOrderEveryTime(t *testing.T) {
	var oldProps, newProps, oldReq []string
	for _, n := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"} {
		oldProps = append(oldProps, `"`+n+`":{"type":"string"}`)
		newProps = append(newProps, `"`+n+`":{"type":"integer"}`)
		oldReq = append(oldReq, `"`+n+`"`)
	}
	latest := &domain.EventSchema{EventName: "order.probe.event", Version: 1,
		JSONSchema: json.RawMessage(`{"properties":{` + strings.Join(oldProps, ",") + `},"required":[` + strings.Join(oldReq, ",") + `]}`)}
	body := `{"json_schema":{"properties":{` + strings.Join(newProps, ",") + `},"required":[` + strings.Join(oldReq, ",") + `]}}`

	violations := func() []any {
		r := newRouter(&stubStore{latest: latest})
		req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/order.probe.event/versions", bytes.NewBufferString(body)))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusConflict, rec.Code)
		var got map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got["violations"].([]any)
	}
	first := violations()
	require.Len(t, first, 8)
	for i := 0; i < 50; i++ {
		require.Equal(t, first, violations(), "the 409 body must be stable across identical requests")
	}
}

// A `type` declared as an array is valid JSON Schema, but the registry's shape
// holds a single string, so it cannot be compared. It is refused at
// registration rather than stored and then failing every later compatibility
// check (documented in openapi.yaml as an unsupported construct).
func TestRegisterVersion_TypeGivenAsArray_IsRefusedAtRegistration(t *testing.T) {
	s := &stubStore{}
	r := newRouter(s)
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/array.type.probe/versions",
		bytes.NewBufferString(`{"json_schema":{"properties":{"a":{"type":["string","null"]}}}}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Nil(t, s.insertedArg, "nothing is stored")
}

// Keywords the checker does not analyse (here allOf) are accepted and simply not compared.
func TestRegisterVersion_UnanalysedKeywords_AreAcceptedAndNotCompared(t *testing.T) {
	s := &stubStore{latest: &domain.EventSchema{EventName: "ref.probe.event", Version: 1,
		JSONSchema: json.RawMessage(`{"properties":{"status":{"type":"string","allOf":[{"minLength":1}]}}}`)}}
	r := newRouter(s)
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/ref.probe.event/versions",
		bytes.NewBufferString(`{"json_schema":{"properties":{"status":{"type":"string","allOf":[{"minLength":9}]}}}}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusCreated, rec.Code, "allOf is outside the compared keywords")
}

// ZS-EVENT-001 section 12: narrowing an enum used to pass unseen (this suite
// once asserted 201 here and called it "a real-world breaking change the
// checker does not see"). It is now refused, so a contract cannot silently
// reject values a consumer was sending.
func TestRegisterVersion_NarrowedEnum_IsRefused(t *testing.T) {
	s := &stubStore{latest: &domain.EventSchema{EventName: "enum.probe.event", Version: 1,
		JSONSchema: json.RawMessage(`{"properties":{"status":{"type":"string","enum":["A","B"]}}}`)}}
	r := newRouter(s)
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/enum.probe.event/versions",
		bytes.NewBufferString(`{"json_schema":{"properties":{"status":{"type":"string","enum":["A"]}}}}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "enum value")
	assert.Nil(t, s.insertedArg)
}

func TestRegisterVersion_OversizedBody_Is413_AndNothingIsStored(t *testing.T) {
	s := &stubStore{}
	r := newRouter(s)
	big := `{"json_schema":{"properties":{"a":{"type":"string","description":"` + strings.Repeat("x", 1<<20+10) + `"}}}}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/big.body.probe/versions", bytes.NewBufferString(big)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Nil(t, s.insertedArg)
}

func TestRegisterVersion_UnauthenticatedCallerLearnsNothingAboutValidation(t *testing.T) {
	// A malformed event name and an unauthenticated caller: 401, not 400.
	r := newRouter(&stubStore{})
	req := httptest.NewRequest(http.MethodPost, "/v1/schemas/NOT_A_VALID_NAME/versions", bytes.NewBufferString(`{`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// An Idempotency-Key wider than its column used to reach Postgres as 22001 and
// come back as 503 — an outage status for a caller's over-long header.
func TestRegisterVersion_OverlongIdempotencyKey_Is400Not503(t *testing.T) {
	s := &stubStore{insertErr: fmt.Errorf("insert schema version: %w", domain.ErrFieldTooLong)}
	r := newRouter(s)
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/long.key.probe/versions", bytes.NewBufferString(validSchemaBody)))
	req.Header.Set("Idempotency-Key", strings.Repeat("k", 300))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
