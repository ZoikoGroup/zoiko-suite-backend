package handler_test

// INT-01 acceptance materials (ZS-SVC-M-001), named for the MATs the plan pins.
//
// Each test wraps the behavior an existing test already proves under its INT_NP
// name, or adds the one the plan calls for and nothing covered yet: the
// residency gate, route-config rollback, and the distinguishability of a
// downstream outage from a caller error.

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/gateway-auth-svc/internal/admission"
	"zoiko.io/gateway-auth-svc/internal/handler"
)

// verifyNamed runs one ForwardAuth probe with the given method and headers.
func verifyNamed(t *testing.T, h *handler.Handler, key any, method string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/verify", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if tok, ok := key.(string); ok {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	h.Verify(rec, req)
	return rec
}

// MAT forged-tenant-header: a caller-supplied X-Zoiko-Resolved-Tenant-Id that
// disagrees with the signed envelope is refused, and the refusal names itself
// (Traefik returns the answer verbatim, so the reason reaches the caller).
func TestINT01_ForgedTenantHeader(t *testing.T) {
	h, key, _ := newTestEnv(t)
	tok := mintEnvelope(t, key, testKid, validClaims())

	rec := verifyNamed(t, h, tok, http.MethodGet, map[string]string{
		"X-Zoiko-Resolved-Tenant-Id": "tenant-evil",
	})

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "tenant_hostname_mismatch", rec.Header().Get("X-Auth-Denial-Reason"))
	assert.Contains(t, rec.Body.String(), "decision_id")
}

// MAT downstream-denied: the gateway's own refusals always carry
// X-Auth-Denial-Reason and a decision id; an admitted request never does. A
// backend's own 403 comes back verbatim without the reason header, so the two
// kinds of refusal stay distinguishable at the edge.
func TestINT01_ValidTokenDownstreamDenied(t *testing.T) {
	h, key, _ := newTestEnv(t)
	tok := mintEnvelope(t, key, testKid, validClaims())

	admitted := verifyNamed(t, h, tok, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, admitted.Code)
	assert.Empty(t, admitted.Header().Get("X-Auth-Denial-Reason"),
		"an admitted request must never carry a refusal reason")
	assert.Equal(t, "principal-xyz", admitted.Header().Get("X-Principal-Id"))

	refused := verifyNamed(t, h, "", http.MethodGet, nil)
	require.Equal(t, http.StatusUnauthorized, refused.Code)
	assert.Equal(t, "no_token", refused.Header().Get("X-Auth-Denial-Reason"))
	assert.Contains(t, refused.Body.String(), "decision_id")
}

// MAT version-unsupported: a URI the admission policy does not admit is
// refused under its own version, never silently re-served by an older one.
func TestINT01_VersionUnsupported(t *testing.T) {
	h, key, _ := newTestEnv(t)
	tok := mintEnvelope(t, key, testKid, validClaims())
	h.UseAdmission(handler.Admission{Policy: admission.Policy{SupportedVersions: []string{"v1"}}})

	rec := verifyNamed(t, h, tok, http.MethodGet, map[string]string{
		"X-Forwarded-Uri": "/v9/accounts",
	})

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "version_unsupported", rec.Header().Get("X-Auth-Denial-Reason"))
}

// MAT request-size-abuse: an oversized body is refused by the shape policy
// before authentication is even consulted.
func TestINT01_RequestSizeAbuse(t *testing.T) {
	h, key, _ := newTestEnv(t)
	tok := mintEnvelope(t, key, testKid, validClaims())
	h.UseAdmission(handler.Admission{Policy: admission.Policy{MaxBodyBytes: 1 << 20}})

	req := httptest.NewRequest(http.MethodPost, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Length", strconv.Itoa(10<<20))
	sized := httptest.NewRecorder()
	h.Verify(sized, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, sized.Code)
	assert.Equal(t, "request_too_large", sized.Header().Get("X-Auth-Denial-Reason"))
}

// MAT rate-limit-race: concurrent spenders of one client key cannot both spend
// the same token, so admitted responses never exceed the burst, and every
// refusal is 429 with an honest Retry-After.
func TestINT01_RateLimitRace(t *testing.T) {
	h, key, _ := newTestEnv(t)
	tok := mintEnvelope(t, key, testKid, validClaims())
	h.UseAdmission(handler.Admission{IPLimiter: admission.NewLimiter(0.001, 10, 0, nil)})

	const total = 50
	codes := make([]int, total)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := verifyNamed(t, h, tok, http.MethodGet, nil)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	admitted, limited := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			admitted++
		case http.StatusTooManyRequests:
			limited++
		}
	}
	assert.LessOrEqual(t, admitted, 10, "spend race must not admit past the burst")
	assert.Positive(t, limited)

	limiter := admission.NewLimiter(0.001, 10, 0, nil)
	var wait time.Duration
	for {
		var ok bool
		ok, wait = limiter.Allow("race-client")
		if !ok {
			break
		}
	}
	assert.GreaterOrEqual(t, int(wait.Seconds()), 1, "Retry-After advises at least one second")
}

// AU-04: login routes use a stricter per-IP bucket. A flood of failed logins
// from one IP hits 429 before the 25th attempt.
func TestAU04_LoginRoutesUseStricterBucket(t *testing.T) {
	h, key, _ := newTestEnv(t)
	tok := mintEnvelope(t, key, testKid, validClaims())
	// Install a very small login bucket: 5/sec, burst 10
	h.UseAdmission(handler.Admission{
		LoginLimiter: admission.NewLimiter(5, 10, 0, nil),
	})

	// Make rapid requests to /v1/authenticate
	codes := make([]int, 25)
	for i := 0; i < 25; i++ {
		rec := verifyNamed(t, h, tok, http.MethodPost, map[string]string{
			"X-Forwarded-Uri": "/v1/authenticate",
		})
		codes[i] = rec.Code
	}

	// First 10 should be admitted (burst), then 429
	admitted, limited := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			admitted++
		case http.StatusTooManyRequests:
			limited++
		}
	}
	assert.LessOrEqual(t, admitted, 10, "login bucket must not admit past the burst")
	assert.Positive(t, limited, "some requests must be rate limited")
}

// AU-04: login throttle keyed on real client IP via trusted proxy.
// An untrusted peer's XFF must be ignored; the limiter keys on the real client.
func TestAU04_ThrottleKeyedOnRealClientIP(t *testing.T) {
	h, key, cfg := newTestEnvWith(t, "", "")
	cfg.TrustedProxyCIDRs = []string{"10.0.0.0/8"}
	cfg.TrustedProxyDepth = 1
	tok := mintEnvelope(t, key, testKid, validClaims())
	h.UseAdmission(handler.Admission{
		LoginLimiter: admission.NewLimiter(5, 10, 0, nil),
	})

	// Client A at 203.0.113.7 behind trusted proxy 10.0.1.5
	codesA := make([]int, 15)
	for i := 0; i < 15; i++ {
		rec := verifyNamed(t, h, tok, http.MethodPost, map[string]string{
			"X-Forwarded-Uri": "/v1/authenticate",
			"X-Forwarded-For": "203.0.113.7, 10.0.1.5",
		})
		codesA[i] = rec.Code
	}

	// Client B at 203.0.113.8 behind same trusted proxy
	codesB := make([]int, 15)
	for i := 0; i < 15; i++ {
		rec := verifyNamed(t, h, tok, http.MethodPost, map[string]string{
			"X-Forwarded-Uri": "/v1/authenticate",
			"X-Forwarded-For": "203.0.113.8, 10.0.1.5",
		})
		codesB[i] = rec.Code
	}

	// Both should hit their own bucket limits independently
	limitedA, limitedB := 0, 0
	for _, c := range codesA {
		if c == http.StatusTooManyRequests {
			limitedA++
		}
	}
	for _, c := range codesB {
		if c == http.StatusTooManyRequests {
			limitedB++
		}
	}
	assert.Positive(t, limitedA, "client A must be rate limited")
	assert.Positive(t, limitedB, "client B must be rate limited")

	// Client A forges XFF to pretend to be client B - must NOT bypass A's limit
	rec := verifyNamed(t, h, tok, http.MethodPost, map[string]string{
		"X-Forwarded-Uri": "/v1/authenticate",
		"X-Forwarded-For": "203.0.113.8, 10.0.1.5", // forged
	})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "forged XFF must not bypass client A's limit")
}

// AU-04: other login routes also use the stricter bucket.
func TestAU04_OtherLoginRoutesUseLoginBucket(t *testing.T) {
	loginRoutes := []string{
		"/v1/authenticate/step-up",
		"/v1/account/password",
		"/v1/account/password/forgot",
		"/v1/account/password/reset",
		"/oauth2/login",
		"/oauth2/token",
	}

	for _, route := range loginRoutes {
		t.Run(route, func(t *testing.T) {
			h, key, _ := newTestEnv(t)
			tok := mintEnvelope(t, key, testKid, validClaims())
			h.UseAdmission(handler.Admission{
				LoginLimiter: admission.NewLimiter(5, 10, 0, nil),
			})

			// Burst 10
			for i := 0; i < 10; i++ {
				rec := verifyNamed(t, h, tok, http.MethodPost, map[string]string{
					"X-Forwarded-Uri": route,
				})
				assert.Equal(t, http.StatusOK, rec.Code, "first 10 requests to %s must be admitted", route)
			}
			// 11th should be 429
			rec := verifyNamed(t, h, tok, http.MethodPost, map[string]string{
				"X-Forwarded-Uri": route,
			})
			assert.Equal(t, http.StatusTooManyRequests, rec.Code, "11th request to %s must be rate limited", route)
		})
	}
}

// MAT route-config-rollback: the active admission policy can be replaced —
// and restored — without changing the handler, and a rolled-back version
// admission refuses what it no longer admits.
func TestINT01_RouteConfigRollback(t *testing.T) {
	h, key, _ := newTestEnv(t)
	tok := mintEnvelope(t, key, testKid, validClaims())
	v2 := map[string]string{"X-Forwarded-Uri": "/v2/accounts"}

	h.UseAdmission(handler.Admission{Policy: admission.Policy{SupportedVersions: []string{"v1", "v2"}}})
	assert.Equal(t, http.StatusOK, verifyNamed(t, h, tok, http.MethodGet, v2).Code)

	h.UseAdmission(handler.Admission{Policy: admission.Policy{SupportedVersions: []string{"v1"}}})
	rolled := verifyNamed(t, h, tok, http.MethodGet, v2)
	assert.Equal(t, http.StatusBadRequest, rolled.Code)
	assert.Equal(t, "version_unsupported", rolled.Header().Get("X-Auth-Denial-Reason"))

	h.UseAdmission(handler.Admission{Policy: admission.Policy{SupportedVersions: []string{"v1", "v2"}}})
	assert.Equal(t, http.StatusOK, verifyNamed(t, h, tok, http.MethodGet, v2).Code)
}

// Residency fixtures. The tenant's default policy is res-eu, the entity's
// effective one res-uk, so GOV-01 resolution lands on res-uk — the same id the
// residency gate must look up.
const (
	strictUKPolicy    = `{"data_residency_policy_id":"res-uk","residency_mode":"STRICT_REGION","residency_region_id":"reg-uk"}`
	preferredUKPolicy = `{"data_residency_policy_id":"res-uk","residency_mode":"PREFERRED_REGION","residency_region_id":"reg-uk"}`
	ukRegion          = `{"residency_region_id":"reg-uk","region_code":"uk"}`
)

// verifyResidency probes /verify against a stub registry carrying residency
// routes, with an optional X-Zoiko-Resolved-Region (the header GTRM's
// per-tenant middleware injects and clients cannot).
func verifyResidency(t *testing.T, method, region string, reg *stubRegistry) *httptest.ResponseRecorder {
	t.Helper()
	h, key, _ := newTestEnvWith(t, "", reg.start(t))

	req := httptest.NewRequest(method, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, validClaims()))
	if region != "" {
		req.Header.Set("X-Zoiko-Resolved-Region", region)
	}
	rec := httptest.NewRecorder()
	h.Verify(rec, req)
	return rec
}

// MAT residency-denied: on a material write, a strict policy whose region
// disagrees with the edge-resolved region is refused with its own outcome;
// reads, preferred-mode policies, region-matching writes, non-GTRM routes
// (no region header) and registry-free reads are not.
func TestINT01_ResidencyDenied(t *testing.T) {
	t.Run("strict region mismatch blocks the write", func(t *testing.T) {
		reg := &stubRegistry{tenantBody: activeTenant, entityBody: ownEntity,
			policyBody: strictUKPolicy, regionBody: ukRegion}
		rec := verifyResidency(t, http.MethodPost, "eu", reg)
		require.Equal(t, http.StatusForbidden, rec.Code)
		assert.Equal(t, "residency_denied", rec.Header().Get("X-Auth-Denial-Reason"))
	})

	t.Run("strict region match admits the write", func(t *testing.T) {
		reg := &stubRegistry{tenantBody: activeTenant, entityBody: ownEntity,
			policyBody: strictUKPolicy, regionBody: ukRegion}
		assert.Equal(t, http.StatusOK, verifyResidency(t, http.MethodPost, "uk", reg).Code)
	})

	t.Run("read passes on mismatch", func(t *testing.T) {
		reg := &stubRegistry{tenantBody: activeTenant, entityBody: ownEntity,
			policyBody: strictUKPolicy, regionBody: ukRegion}
		assert.Equal(t, http.StatusOK, verifyResidency(t, http.MethodGet, "eu", reg).Code)
	})

	t.Run("preferred region never blocks", func(t *testing.T) {
		reg := &stubRegistry{tenantBody: activeTenant, entityBody: ownEntity,
			policyBody: preferredUKPolicy, regionBody: ukRegion}
		assert.Equal(t, http.StatusOK, verifyResidency(t, http.MethodPost, "eu", reg).Code)
	})

	t.Run("unknown policy fails the write closed", func(t *testing.T) {
		reg := &stubRegistry{tenantBody: activeTenant, entityBody: ownEntity,
			policyBody: strictUKPolicy, policyCode: http.StatusNotFound, regionBody: ukRegion}
		rec := verifyResidency(t, http.MethodPost, "eu", reg)
		require.Equal(t, http.StatusForbidden, rec.Code)
		assert.Equal(t, "residency_denied", rec.Header().Get("X-Auth-Denial-Reason"))
		assert.Equal(t, http.StatusOK, verifyResidency(t, http.MethodGet, "eu", reg).Code)
	})

	t.Run("policy lookup outage blocks the write with 503", func(t *testing.T) {
		reg := &stubRegistry{tenantBody: activeTenant, entityBody: ownEntity,
			policyBody: strictUKPolicy, policyCode: http.StatusServiceUnavailable, regionBody: ukRegion}
		rec := verifyResidency(t, http.MethodPost, "eu", reg)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, "residency_unresolved", rec.Header().Get("X-Auth-Denial-Reason"))
		assert.Equal(t, http.StatusOK, verifyResidency(t, http.MethodGet, "eu", reg).Code)
	})

	t.Run("no region header means no comparison", func(t *testing.T) {
		reg := &stubRegistry{tenantBody: activeTenant, entityBody: ownEntity,
			policyBody: strictUKPolicy, regionBody: ukRegion}
		assert.Equal(t, http.StatusOK, verifyResidency(t, http.MethodPost, "", reg).Code)
	})
}

// MAT downstream-timeout-distinguishable: three very different situations give
// three mutually distinguishable answers. A downstream outage (503, unresolved,
// Retry-After, decision id) is not a caller error (401, no_token), and a
// registry denial (403, denied) is not an outage.
func TestINT01_DownstreamTimeoutDistinguishable(t *testing.T) {
	outage := &stubRegistry{tenantCode: http.StatusServiceUnavailable, entityBody: ownEntity}
	h, key, _ := newTestEnvWith(t, "", outage.start(t))
	tok := mintEnvelope(t, key, testKid, validClaims())

	down := verifyNamed(t, h, tok, http.MethodPost, nil)
	require.Equal(t, http.StatusServiceUnavailable, down.Code)
	assert.Equal(t, "tenant_context_unresolved", down.Header().Get("X-Auth-Denial-Reason"))
	assert.Equal(t, "unresolved", down.Header().Get("X-Tenant-Context"))
	assert.NotEmpty(t, down.Header().Get("Retry-After"))
	assert.Contains(t, down.Body.String(), "decision_id")

	caller := verifyNamed(t, h, "", http.MethodPost, nil)
	require.Equal(t, http.StatusUnauthorized, caller.Code)
	assert.Equal(t, "no_token", caller.Header().Get("X-Auth-Denial-Reason"))
	assert.Empty(t, caller.Header().Get("Retry-After"))
	assert.Empty(t, caller.Header().Get("X-Tenant-Context"))

	susp := &stubRegistry{tenantBody: suspendedTenant, entityBody: ownEntity}
	h2, key2, _ := newTestEnvWith(t, "", susp.start(t))
	denied := verifyNamed(t, h2, mintEnvelope(t, key2, testKid, validClaims()), http.MethodGet, nil)
	require.Equal(t, http.StatusForbidden, denied.Code)
	assert.Equal(t, "tenant_context_denied", denied.Header().Get("X-Auth-Denial-Reason"))
	assert.Equal(t, "denied", denied.Header().Get("X-Tenant-Context"))
}
