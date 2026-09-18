package envelope_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/envelope"
)

// The canonical service input contract (ZS-ARCH-SVC-001 §4) had no tests at
// all, and it is the middleware in front of every route on this service.
//
// The parts worth pinning are the ones whose failure is SILENT rather than
// loud: the /v1/authenticate exemption (without it the entry point to the whole
// platform is unreachable, and reachable only by a caller asserting the
// identity it has not yet proven), the 401-vs-400 split (which tells a caller
// whether to fix its payload or its authentication), and observe mode never
// refusing (a migration state that must not gate anything).

func request(method, path string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// fullEnvelope is every unconditionally-mandatory §4 header.
func fullEnvelope() map[string]string {
	return map[string]string{
		envelope.HeaderTenantID:       "tenant-a",
		envelope.HeaderActorSubjectID: "principal-1",
		envelope.HeaderRequestID:      "req-1",
		envelope.HeaderCorrelationID:  "corr-1",
		envelope.HeaderSourceChannel:  string(envelope.ChannelWeb),
	}
}

// ── Parse ────────────────────────────────────────────────────────────────────

func TestParseReadsTheMandatoryFields(t *testing.T) {
	e := envelope.Parse(request(http.MethodPost, "/v1/context/resolve", fullEnvelope()))

	assert.Equal(t, "tenant-a", e.TenantID)
	assert.Equal(t, "principal-1", e.ActorSubjectID)
	assert.Equal(t, "req-1", e.RequestID)
	assert.Equal(t, "corr-1", e.CorrelationID)
}

// TestActorAcceptsAWorkloadInPlaceOfAHuman.
//
// §4 permits either. A service-to-service call has no human subject, and
// refusing one for the absence of a principal would make every scheduled job
// and every internal call uncontractable.
func TestActorAcceptsAWorkloadInPlaceOfAHuman(t *testing.T) {
	h := fullEnvelope()
	delete(h, envelope.HeaderActorSubjectID)
	h[envelope.HeaderWorkloadID] = "retention-worker"

	e := envelope.Parse(request(http.MethodPost, "/v1/context/resolve", h))
	assert.Equal(t, "retention-worker", e.Actor())
}

func TestSourceChannelRejectsUnknownValues(t *testing.T) {
	assert.True(t, envelope.ChannelWeb.Valid())
	assert.True(t, envelope.ChannelScheduledJob.Valid())

	// An unknown channel must not be coerced onto "api": source_channel drives
	// provenance class, and import/integration additionally force
	// source_system, so silently remapping erases the obligation the field
	// exists to create.
	assert.False(t, envelope.SourceChannel("carrier_pigeon").Valid())
	assert.False(t, envelope.SourceChannel("").Valid())
}

func TestExternalChannelsAreTheOnesThatOriginateOutside(t *testing.T) {
	assert.True(t, envelope.ChannelImport.External())
	assert.True(t, envelope.ChannelIntegration.External())

	assert.False(t, envelope.ChannelWeb.External())
	assert.False(t, envelope.ChannelAPI.External())
	assert.False(t, envelope.ChannelSystem.External())
}

// ── Validate ─────────────────────────────────────────────────────────────────

func TestValidateAcceptsACompleteEnvelope(t *testing.T) {
	policy := envelope.ServicePolicy()
	r := request(http.MethodGet, "/v1/principals/p-1", fullEnvelope())

	assert.Nil(t, policy.Validate(envelope.Parse(r), r))
}

// TestValidateReportsEveryViolationAtOnce.
//
// A caller adopting the envelope typically misses several fields together, and
// a one-at-a-time refusal turns that into as many failed round trips.
func TestValidateReportsEveryViolationAtOnce(t *testing.T) {
	policy := envelope.ServicePolicy()
	r := request(http.MethodGet, "/v1/principals/p-1", nil)

	err := policy.Validate(envelope.Parse(r), r)
	require.NotNil(t, err)
	assert.GreaterOrEqual(t, len(err.Violations), 4,
		"a bare request violates tenant, actor, request_id and correlation_id at minimum")

	fields := map[string]bool{}
	for _, v := range err.Violations {
		fields[v.Field] = true
		assert.NotEmpty(t, v.Header, "every violation must name the header to set")
		assert.NotEmpty(t, v.Reason, "every violation must say why")
	}
	assert.True(t, fields["tenant_id"])
	assert.True(t, fields["actor_subject_id"])
	assert.True(t, fields["request_id"])
	assert.True(t, fields["correlation_id"])
}

// TestStatusForSplits401From400 is the distinction that decides what a caller
// does next.
//
// tenant_id and actor are set by gateway-auth-svc AFTER it verifies a signed
// identity envelope, so their absence means the request never passed that
// verification. Reporting it as 400 tells the caller to fix its payload when
// what it has to fix is authentication.
func TestStatusForSplits401From400(t *testing.T) {
	policy := envelope.ServicePolicy()

	bare := request(http.MethodGet, "/v1/principals/p-1", nil)
	err := policy.Validate(envelope.Parse(bare), bare)
	require.NotNil(t, err)
	assert.Equal(t, http.StatusUnauthorized, envelope.StatusFor(err),
		"a missing tenant is an authentication failure, not a malformed payload")

	// Authenticated, but missing a traceability field.
	h := fullEnvelope()
	delete(h, envelope.HeaderRequestID)
	traced := request(http.MethodGet, "/v1/principals/p-1", h)
	err = policy.Validate(envelope.Parse(traced), traced)
	require.NotNil(t, err)
	assert.Equal(t, http.StatusBadRequest, envelope.StatusFor(err))
}

func TestValidationErrorNamesItsFields(t *testing.T) {
	policy := envelope.ServicePolicy()
	r := request(http.MethodGet, "/v1/principals/p-1", nil)

	err := policy.Validate(envelope.Parse(r), r)
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "canonical input contract violated")
	assert.Contains(t, err.Error(), "tenant_id")
}

// ── The /v1/authenticate exemption ───────────────────────────────────────────

// TestAuthenticateIsExempt is the one that keeps the platform reachable.
//
// The contract's mandatory tenant_id and actor are set by gateway-auth-svc only
// after it verifies a signed envelope — which is exactly what /v1/authenticate
// exists to make obtainable. Enforcing them here would be circular and
// satisfiable only by a caller asserting the identity it has not yet proven.
func TestAuthenticateIsExemptFromTheContract(t *testing.T) {
	policy := envelope.ServicePolicy()
	r := request(http.MethodPost, "/v1/authenticate", nil)

	assert.Nil(t, policy.Validate(envelope.Parse(r), r),
		"the login endpoint must be reachable without the identity it issues")
}

// TestOnlyAuthenticateIsExempt guards the blast radius of that exemption.
func TestOnlyAuthenticateIsExempt(t *testing.T) {
	policy := envelope.ServicePolicy()

	for _, path := range []string{
		"/v1/context/resolve",
		"/v1/context/support",
		"/v1/principals/p-1",
		"/v1/context/tenant/invalidate",
	} {
		r := request(http.MethodPost, path, nil)
		assert.NotNil(t, policy.Validate(envelope.Parse(r), r),
			"%s must not be exempt — only the login endpoint is", path)
	}
}

// ── Service policy ───────────────────────────────────────────────────────────

func TestServicePolicyMatchesThisServicesObligations(t *testing.T) {
	p := envelope.ServicePolicy()

	assert.Equal(t, "identity-context-svc", p.ServiceName,
		"the name appears in refusals so an aggregated log says which service refused")

	// §4: legal entity is mandatory for entity-specific records, and a resolved
	// session is scoped to one.
	assert.Equal(t, envelope.RequiredOnWrite, p.LegalEntityID)

	// This service posts to no accounting book.
	assert.Equal(t, envelope.NotRequired, p.BookID)
}

// TestIdempotencyCannotBeDisabled.
//
// Policy defaults IdempotencyKey to RequiredOnWrite when left at NotRequired,
// because a service that changes material state without replay protection
// violates INV-08. A service may RAISE it; disabling it is deliberately not
// expressible, and this pins that the default holds for this service.
func TestIdempotencyIsRequiredOnMaterialWrites(t *testing.T) {
	policy := envelope.ServicePolicy()

	h := fullEnvelope()
	h[envelope.HeaderLegalEntityID] = "entity-1"
	// No Idempotency-Key.
	r := request(http.MethodPost, "/v1/context/tenant/invalidate", h)

	err := policy.Validate(envelope.Parse(r), r)
	require.NotNil(t, err, "a material write without an idempotency key must be refused")

	var found bool
	for _, v := range err.Violations {
		if v.Field == "idempotency_key" {
			found = true
		}
	}
	assert.True(t, found, "the refusal must name idempotency_key")
}

func TestReadsDoNotNeedAnIdempotencyKey(t *testing.T) {
	policy := envelope.ServicePolicy()
	r := request(http.MethodGet, "/v1/principals/p-1", fullEnvelope())

	assert.Nil(t, policy.Validate(envelope.Parse(r), r),
		"a read changes nothing, so replay protection is meaningless for it")
}

// ── Mode resolution ──────────────────────────────────────────────────────────

func TestResolveModeDefaultsToWriteStrict(t *testing.T) {
	t.Setenv(envelope.EnvVarMode, "")
	assert.Equal(t, envelope.ModeWriteStrict, envelope.ResolveMode())
}

// TestATypoDoesNotSilentlyDisableTheControl.
//
// An unrecognised value falls back to the DEFAULT, not to observe. Falling back
// to observe would mean one misspelled deployment variable turns enforcement
// off everywhere with no signal.
func TestUnrecognisedModeFallsBackToTheDefaultNotObserve(t *testing.T) {
	t.Setenv(envelope.EnvVarMode, "strictt")
	assert.Equal(t, envelope.ModeWriteStrict, envelope.ResolveMode())

	t.Setenv(envelope.EnvVarMode, "off")
	assert.Equal(t, envelope.ModeWriteStrict, envelope.ResolveMode())
}

func TestModeIsCaseAndSpaceInsensitive(t *testing.T) {
	t.Setenv(envelope.EnvVarMode, "  STRICT ")
	assert.Equal(t, envelope.ModeStrict, envelope.ResolveMode())
}

// ── Middleware ───────────────────────────────────────────────────────────────

func middlewareUnder(t *testing.T, mode envelope.Mode) http.Handler {
	t.Helper()
	t.Setenv(envelope.EnvVarMode, string(mode))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Proves the envelope reached the handler's context, which is what
		// downstream propagation depends on.
		e, ok := envelope.FromContext(r.Context())
		if !ok {
			http.Error(w, "no envelope on context", http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Seen-Tenant", e.TenantID)
		w.WriteHeader(http.StatusOK)
	})
	return envelope.Middleware(envelope.ServicePolicy(), nil)(next)
}

func TestMiddlewarePlacesTheEnvelopeOnContext(t *testing.T) {
	h := middlewareUnder(t, envelope.ModeStrict)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, request(http.MethodGet, "/v1/principals/p-1", fullEnvelope()))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "tenant-a", w.Header().Get("X-Seen-Tenant"))
}

func TestStrictModeRefusesAnIncompleteRead(t *testing.T) {
	h := middlewareUnder(t, envelope.ModeStrict)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, request(http.MethodGet, "/v1/principals/p-1", nil))

	assert.Equal(t, http.StatusUnauthorized, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.NotEmpty(t, body, "a refusal must say what was missing, not just refuse")
}

// TestWriteStrictAdmitsReadsAndRefusesWrites is the default mode, and the line
// it draws is where the invariants actually bite: replay protection and
// evidence-before-completion are properties of writes.
func TestWriteStrictAdmitsReadsAndRefusesWrites(t *testing.T) {
	h := middlewareUnder(t, envelope.ModeWriteStrict)

	readW := httptest.NewRecorder()
	h.ServeHTTP(readW, request(http.MethodGet, "/v1/principals/p-1", nil))
	assert.Equal(t, http.StatusOK, readW.Code,
		"an incomplete read is a traceability gap, not a correctness one")

	writeW := httptest.NewRecorder()
	h.ServeHTTP(writeW, request(http.MethodPost, "/v1/context/tenant/invalidate", nil))
	assert.NotEqual(t, http.StatusOK, writeW.Code,
		"a material write without an envelope must be refused")
}

// TestObserveModeNeverRefuses.
//
// It is a migration state, not a resting state: the envelope is still parsed
// and propagated so adoption is measurable, but nothing is gated.
func TestObserveModeNeverRefuses(t *testing.T) {
	h := middlewareUnder(t, envelope.ModeObserve)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request(method, "/v1/context/tenant/invalidate", nil))
		assert.Equal(t, http.StatusOK, w.Code, "%s must be admitted in observe mode", method)
	}
}

// TestMiddlewareReportsAdmittedViolations: observe-mode adoption has to be
// measurable, or it is indistinguishable from having no contract.
func TestMiddlewareReportsViolationsItAdmits(t *testing.T) {
	t.Setenv(envelope.EnvVarMode, string(envelope.ModeObserve))

	var reported int
	reporter := func(_ *http.Request, _ envelope.Envelope, err *envelope.ValidationError) {
		if err != nil && len(err.Violations) > 0 {
			reported++
		}
	}

	h := envelope.Middleware(envelope.ServicePolicy(), reporter)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, request(http.MethodPost, "/v1/context/tenant/invalidate", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, reported, "an admitted violation must still be counted")
}

func TestAuthenticateIsAdmittedInEveryMode(t *testing.T) {
	for _, mode := range []envelope.Mode{envelope.ModeStrict, envelope.ModeWriteStrict, envelope.ModeObserve} {
		h := middlewareUnder(t, mode)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request(http.MethodPost, "/v1/authenticate", nil))
		assert.Equal(t, http.StatusOK, w.Code,
			"the login endpoint must stay reachable under %s", mode)
	}
}
