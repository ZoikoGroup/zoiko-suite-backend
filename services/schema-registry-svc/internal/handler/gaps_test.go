package handler_test

// Coverage for the gaps closed in the 17 Aug 2026 pass. Kept in its own file
// so each test sits next to the reason it exists rather than being appended to
// the original suite in registration order.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/schema-registry-svc/internal/domain"
)

const validSchemaBody = `{"json_schema":{"properties":{"a":{"type":"string"}}}}`

// json.Valid answers "is this well-formed JSON", which is not the question.
// `123`, `"a string"`, `null` and `[]` all passed it and were stored as event
// contracts — under an error message that already claimed json_schema "must be
// a valid JSON object".
//
// The damage is not cosmetic: a first version stored as `123` can never be
// evolved. The next registration runs the compatibility check, the stored
// baseline fails to parse into a shape, and every future version of that event
// answers 400 forever. The registry accepted a value that permanently bricked
// the contract it was recording.
func TestRegisterVersion_NonObjectSchema_IsRefused(t *testing.T) {
	for _, body := range []string{
		`{"json_schema":123}`,
		`{"json_schema":"a string"}`,
		`{"json_schema":null}`,
		`{"json_schema":[]}`,
		`{"json_schema":true}`,
	} {
		s := &stubStore{latest: nil}
		r := newRouter(s)
		req := withIdentity(httptest.NewRequest(http.MethodPost,
			"/v1/schemas/probe.non.object/versions", bytes.NewBufferString(body)))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code, "body %s should be refused", body)
		assert.Nil(t, s.insertedArg, "body %s must not be stored", body)
	}
}

// `{}` is an object, and constrains nothing. A contract that permits every
// payload is not a contract — recording one lets a producer claim its events
// are governed when nothing about them is specified.
func TestRegisterVersion_EmptyObjectSchema_IsRefused(t *testing.T) {
	s := &stubStore{latest: nil}
	r := newRouter(s)
	req := withIdentity(httptest.NewRequest(http.MethodPost,
		"/v1/schemas/probe.empty.object/versions", bytes.NewBufferString(`{"json_schema":{}}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Nil(t, s.insertedArg)
}

// A `properties` member the compatibility checker cannot read is refused at
// registration, while there is still a caller to tell — otherwise every future
// version of the event fails its compatibility check against this one.
func TestRegisterVersion_UnparseableShape_IsRefusedAtRegistration(t *testing.T) {
	s := &stubStore{latest: nil}
	r := newRouter(s)
	req := withIdentity(httptest.NewRequest(http.MethodPost,
		"/v1/schemas/probe.bad.shape/versions",
		bytes.NewBufferString(`{"json_schema":{"properties":["not","an","object"]}}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Nil(t, s.insertedArg)
}

// The event name is this registry's primary key and is echoed back in every
// response. It used to be accepted verbatim — any non-empty string — so the
// key of a canonical registry was a free-text field.
func TestRegisterVersion_InvalidEventName_IsRefused(t *testing.T) {
	for _, name := range []string{
		"NotLowercase.event",
		"nodotsatall",
		"trailing.",
		"has%20spaces.event",
		"..",
	} {
		s := &stubStore{latest: nil}
		r := newRouter(s)
		req := withIdentity(httptest.NewRequest(http.MethodPost,
			"/v1/schemas/"+name+"/versions", bytes.NewBufferString(validSchemaBody)))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.NotEqual(t, http.StatusCreated, rec.Code, "event name %q should be refused", name)
		assert.Nil(t, s.insertedArg, "event name %q must not be stored", name)
	}
}

// An over-long name used to reach Postgres, die as SQLSTATE 22001, and answer
// 503 — an outage status for a name that was simply too long.
func TestRegisterVersion_OverlongEventName_Is400Not503(t *testing.T) {
	long := "a." + strings.Repeat("b", 300)
	s := &stubStore{latest: nil}
	r := newRouter(s)
	req := withIdentity(httptest.NewRequest(http.MethodPost,
		"/v1/schemas/"+long+"/versions", bytes.NewBufferString(validSchemaBody)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Nil(t, s.insertedArg)
}

func TestRegisterVersion_OverlongOwningService_Is400(t *testing.T) {
	s := &stubStore{latest: nil}
	r := newRouter(s)
	body := `{"json_schema":{"properties":{"a":{"type":"string"}}},"owning_service":"` +
		strings.Repeat("s", 300) + `"}`
	req := withIdentity(httptest.NewRequest(http.MethodPost,
		"/v1/schemas/probe.long.owner/versions", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Nil(t, s.insertedArg)
}

// A misspelled field used to be discarded silently, so the caller got
// "json_schema is required" for a field they believed they had sent — or, for
// compatibility_mode, a discipline they did not ask for recorded as BACKWARD.
func TestRegisterVersion_UnknownField_IsRefused(t *testing.T) {
	s := &stubStore{latest: nil}
	r := newRouter(s)
	req := withIdentity(httptest.NewRequest(http.MethodPost,
		"/v1/schemas/probe.unknown.field/versions",
		bytes.NewBufferString(`{"json_schema":{"properties":{"a":{"type":"string"}}},"compatibility_mode_":"NONE"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Nil(t, s.insertedArg)
}

// An event contract belongs to the platform, not to a legal entity. The header
// used to be passed through verbatim, and authorization-svc rejects an empty
// legal_entity_id — so an entity-less caller got a 503 blaming infrastructure
// for a scope the request was never going to carry.
func TestRegisterVersion_NoLegalEntity_AuthorizesAgainstThePlatformScope(t *testing.T) {
	s := &stubStore{latest: nil}
	a := &stubAuthz{}
	r := newRouterWithAuthz(s, a)

	req := httptest.NewRequest(http.MethodPost, "/v1/schemas/probe.platform.scope/versions",
		bytes.NewBufferString(validSchemaBody))
	req.Header.Set("X-Principal-Id", "principal-admin-001") // deliberately no X-Legal-Entity-Id
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, testPlatformScope, a.gotScope,
		"an entity-less registration must be authorized against the platform scope, never an empty string")
}

func TestRegisterVersion_WithLegalEntity_AuthorizesAgainstIt(t *testing.T) {
	s := &stubStore{latest: nil}
	a := &stubAuthz{}
	r := newRouterWithAuthz(s, a)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/schemas/probe.entity.scope/versions",
		bytes.NewBufferString(validSchemaBody)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "entity-001", a.gotScope)
}

// Every read used to be open: anything that could reach the port could
// enumerate every event name, every payload field, and which service owns it.
func TestReads_WithoutIdentity_Are401(t *testing.T) {
	for _, path := range []string{
		"/v1/schemas/",
		"/v1/schemas/probe.event.read/versions",
		"/v1/schemas/probe.event.read/versions/latest",
		"/v1/schemas/probe.event.read/versions/1",
	} {
		s := &stubStore{
			names:   []string{"a.b"},
			latest:  &domain.EventSchema{EventName: "probe.event.read", Version: 1},
			version: &domain.EventSchema{EventName: "probe.event.read", Version: 1},
		}
		r := newRouter(s)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s must require an identified caller", path)
	}
}

func TestReads_ArePagedAndValidated(t *testing.T) {
	for _, path := range []string{"/v1/schemas/", "/v1/schemas/probe.event.read/versions"} {
		for _, q := range []string{"?limit=abc", "?limit=0", "?limit=99999", "?offset=-1"} {
			s := &stubStore{names: []string{"a.b"}, versions: []*domain.EventSchema{{Version: 1}}}
			r := newRouter(s)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, getWithIdentity(path+q))
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s%s", path, q)
		}
	}

	// The default is bounded, not unlimited.
	s := &stubStore{names: []string{"a.b"}}
	r := newRouter(s)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, getWithIdentity("/v1/schemas/"))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 100, s.gotLimit)

	s2 := &stubStore{versions: []*domain.EventSchema{{Version: 2}}}
	r2 := newRouter(s2)
	rec2 := httptest.NewRecorder()
	r2.ServeHTTP(rec2, getWithIdentity("/v1/schemas/probe.event.read/versions?limit=5&offset=10"))
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, 5, s2.gotLimit)
	assert.Equal(t, 10, s2.gotOffset)
}

// An empty page past the end of a real event's history is not a missing event.
// Only offset 0 can tell the two apart, and answering 404 for a page beyond the
// end would tell a paging reader the contract had been deleted.
func TestListVersions_EmptyPageBeyondEnd_Is200NotNotFound(t *testing.T) {
	s := &stubStore{versions: nil}
	r := newRouter(s)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, getWithIdentity("/v1/schemas/probe.event.read/versions?offset=50"))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

// A version number is assigned by the registry and starts at 1. A zero or
// negative one used to reach Postgres as a perfectly valid comparison that
// matched nothing, so it answered 404 and read as "that version was deleted".
func TestGetVersion_NonPositiveVersion_Is400(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		s := &stubStore{}
		r := newRouter(s)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, getWithIdentity("/v1/schemas/probe.event.read/versions/"+v))
		assert.Equal(t, http.StatusBadRequest, rec.Code, "version %s", v)
	}
}

// A name that could never be registered names no contract, so a read of it is
// not-found rather than a validation error.
func TestReads_InvalidEventName_Are404(t *testing.T) {
	s := &stubStore{}
	r := newRouter(s)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, getWithIdentity("/v1/schemas/NOT_AN_EVENT/versions/latest"))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
