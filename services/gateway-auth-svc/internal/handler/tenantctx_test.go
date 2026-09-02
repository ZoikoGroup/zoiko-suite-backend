package handler_test

// GOV-01 tenant context resolution (ZS-SVC-A-001 §4), exercised through the
// ForwardAuth endpoint with a stub tenant-entity-registry-svc.
//
// A valid token proves who is asking. It does not prove the tenant may
// transact, and it does not prove the legal entity named in the token belongs
// to that tenant — the registry owns both facts, and until this wiring existed
// nothing on the platform consulted it.

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRegistry answers the two reads envelope.Resolver makes. Bodies mirror
// tenant-entity-registry-svc's real Tenant and LegalEntity JSON, including the
// lifecycle_state value ProvisionTenant actually writes.
type stubRegistry struct {
	tenantBody string
	entityBody string
	tenantCode int
	entityCode int
	calls      atomic.Int32
}

func (s *stubRegistry) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/tenants/tenant-abc":
			if s.tenantCode != 0 {
				w.WriteHeader(s.tenantCode)
				return
			}
			_, _ = w.Write([]byte(s.tenantBody))
		case "/v1/entities/entity-001":
			if s.entityCode != 0 {
				w.WriteHeader(s.entityCode)
				return
			}
			_, _ = w.Write([]byte(s.entityBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// onboardingTenant is what ProvisionTenant creates: status ACTIVE, lifecycle
// ONBOARDING. The registry's own enum documents the pair as legitimate.
const onboardingTenant = `{"tenant_id":"tenant-abc","status":"ACTIVE","lifecycle_state":"ONBOARDING",
	"primary_timezone":"Europe/London","default_currency_code":"GBP",
	"default_data_residency_policy_id":"res-eu"}`

const activeTenant = `{"tenant_id":"tenant-abc","status":"ACTIVE","lifecycle_state":"ACTIVE",
	"primary_timezone":"Europe/London","default_currency_code":"GBP",
	"default_data_residency_policy_id":"res-eu"}`

const suspendedTenant = `{"tenant_id":"tenant-abc","status":"SUSPENDED","lifecycle_state":"SUSPENDED",
	"primary_timezone":"Europe/London"}`

const ownEntity = `{"legal_entity_id":"entity-001","tenant_id":"tenant-abc","entity_status":"ACTIVE",
	"primary_jurisdiction_id":"GB","fiscal_calendar_id":"fc-uk","data_residency_policy_id":"res-uk"}`

const foreignEntity = `{"legal_entity_id":"entity-001","tenant_id":"tenant-OTHER","entity_status":"ACTIVE",
	"primary_jurisdiction_id":"FR"}`

func verifyWithRegistry(t *testing.T, method string, reg *stubRegistry) *httptest.ResponseRecorder {
	t.Helper()
	h, key, _ := newTestEnvWith(t, "", reg.start(t))

	req := httptest.NewRequest(method, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, validClaims()))
	rec := httptest.NewRecorder()
	h.Verify(rec, req)
	return rec
}

// A freshly provisioned tenant must be able to transact.
//
// ProvisionTenant writes lifecycle ONBOARDING, and the only route out of it is
// POST /v1/tenants/{id}/lifecycle — itself a write against this same registry.
// Treating ONBOARDING as non-operable would refuse the one call that could end
// it, so no tenant this platform provisions could ever become usable.
func TestVerify_OnboardingTenantIsOperable(t *testing.T) {
	rec := verifyWithRegistry(t, http.MethodPost, &stubRegistry{
		tenantBody: onboardingTenant, entityBody: ownEntity,
	})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "GB", rec.Header().Get("X-Jurisdiction-Context"))
	assert.Equal(t, "res-uk", rec.Header().Get("X-Residency-Policy-Id"))
}

func TestVerify_ActiveTenantResolvesServerContext(t *testing.T) {
	rec := verifyWithRegistry(t, http.MethodGet, &stubRegistry{
		tenantBody: activeTenant, entityBody: ownEntity,
	})

	require.Equal(t, http.StatusOK, rec.Code)
	// Entity-level residency overrides the tenant default (res-uk, not res-eu).
	assert.Equal(t, "res-uk", rec.Header().Get("X-Residency-Policy-Id"))
	assert.Equal(t, "Europe/London", rec.Header().Get("X-Timezone"))
	assert.Empty(t, rec.Header().Get("X-Tenant-Context-Stale"))
}

// A suspended tenant holds a perfectly valid token. Authentication is not the
// question being asked here, so re-authenticating cannot help: 403, not 401.
func TestVerify_SuspendedTenantDenied(t *testing.T) {
	rec := verifyWithRegistry(t, http.MethodGet, &stubRegistry{
		tenantBody: suspendedTenant, entityBody: ownEntity,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "denied", rec.Header().Get("X-Tenant-Context"))
}

func TestVerify_UnknownTenantDenied(t *testing.T) {
	rec := verifyWithRegistry(t, http.MethodGet, &stubRegistry{
		tenantCode: http.StatusNotFound, entityBody: ownEntity,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// The token names an entity; the registry says it belongs to someone else.
// Nothing downstream would have caught this — the entity ID travels as a header
// every service trusts.
func TestVerify_CrossTenantEntityDenied(t *testing.T) {
	rec := verifyWithRegistry(t, http.MethodGet, &stubRegistry{
		tenantBody: activeTenant, entityBody: foreignEntity,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "denied", rec.Header().Get("X-Tenant-Context"))
}

// Registry unreachable is the absence of a decision, not a refusal. A write
// must not proceed on a context nobody could confirm, and answering 403 would
// claim a denial that never happened.
func TestVerify_RegistryUnavailableBlocksWriteWith503(t *testing.T) {
	rec := verifyWithRegistry(t, http.MethodPost, &stubRegistry{
		tenantCode: http.StatusInternalServerError,
	})

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "5", rec.Header().Get("Retry-After"))
	// Distinguishable from a backend service's own 503, which means something
	// else entirely and has a different fix.
	assert.Equal(t, "unresolved", rec.Header().Get("X-Tenant-Context"))
}

// The same failure on a read with nothing cached is still fail-closed: the
// stale-read allowance needs a prior successful resolution to be stale from.
func TestVerify_RegistryUnavailableBlocksColdReadWith503(t *testing.T) {
	rec := verifyWithRegistry(t, http.MethodGet, &stubRegistry{
		tenantCode: http.StatusInternalServerError,
	})

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// Resolution must not put the registry in the hot path of every gated request.
func TestVerify_ResolutionIsCached(t *testing.T) {
	reg := &stubRegistry{tenantBody: activeTenant, entityBody: ownEntity}
	h, key, _ := newTestEnvWith(t, "", reg.start(t))
	tok := "Bearer " + mintEnvelope(t, key, testKid, validClaims())

	for range 5 {
		req := httptest.NewRequest(http.MethodGet, "/verify", nil)
		req.Header.Set("Authorization", tok)
		rec := httptest.NewRecorder()
		h.Verify(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	}

	// One tenant read + one entity read, then served from cache.
	assert.Equal(t, int32(2), reg.calls.Load(),
		"resolution should be cached for the configured TTL, not repeated per request")
}

// With no registry configured the gateway behaves exactly as it did before this
// wiring existed. An unconfigured dependency degrades to "not consulted", never
// to a fabricated allow or a blanket denial.
func TestVerify_ResolutionDisabledWhenRegistryUnset(t *testing.T) {
	h, key, _ := newTestEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, validClaims()))
	rec := httptest.NewRecorder()
	h.Verify(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("X-Jurisdiction-Context"),
		"a disabled resolver must not assert context it never resolved")
}

// Traefik sets X-Forwarded-Method; it decides the write/read posture even when
// the probe itself arrives as something else.
func TestVerify_ForwardedMethodDecidesWritePosture(t *testing.T) {
	reg := &stubRegistry{tenantCode: http.StatusInternalServerError}
	h, key, _ := newTestEnvWith(t, "", reg.start(t))

	req := httptest.NewRequest(http.MethodGet, "/verify", nil)
	req.Header.Set("Authorization", "Bearer "+mintEnvelope(t, key, testKid, validClaims()))
	req.Header.Set("X-Forwarded-Method", http.MethodPost)
	rec := httptest.NewRecorder()
	h.Verify(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
