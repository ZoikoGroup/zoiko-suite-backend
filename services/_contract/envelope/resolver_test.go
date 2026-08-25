package envelope

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubRegistry stands in for tenant-entity-registry-svc, answering the two reads
// Resolver makes. Bodies mirror that service's real Tenant and LegalEntity JSON.
type stubRegistry struct {
	tenant     string
	entity     string
	tenantCode int
	entityCode int
	seenTenant string
	seenActor  string
	seenCorr   string
}

func (s *stubRegistry) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.seenTenant = r.Header.Get(HeaderTenantID)
		s.seenActor = r.Header.Get(HeaderActorSubjectID)
		s.seenCorr = r.Header.Get(HeaderCorrelationID)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/v1/tenants/tenant-01":
			if s.tenantCode != 0 {
				w.WriteHeader(s.tenantCode)
				return
			}
			_, _ = w.Write([]byte(s.tenant))
		case r.URL.Path == "/v1/entities/entity-01":
			if s.entityCode != 0 {
				w.WriteHeader(s.entityCode)
				return
			}
			_, _ = w.Write([]byte(s.entity))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const activeTenant = `{"tenant_id":"tenant-01","status":"ACTIVE","lifecycle_state":"ACTIVE",
	"primary_timezone":"Europe/London","default_currency_code":"GBP",
	"default_data_residency_policy_id":"res-eu"}`

const gbEntity = `{"legal_entity_id":"entity-01","tenant_id":"tenant-01","entity_status":"ACTIVE",
	"primary_jurisdiction_id":"GB","fiscal_calendar_id":"fc-uk","data_residency_policy_id":"res-uk"}`

func envFor(tenant, entity string) Envelope {
	return Envelope{TenantID: tenant, LegalEntityID: entity, ActorSubjectID: "user-01", CorrelationID: "corr-01"}
}

// The point of the resolver: jurisdiction_context and timezone are facts an
// existing service owns, so they are read rather than trusted from the caller.
func TestResolvePullsJurisdictionAndTimezoneFromRegistry(t *testing.T) {
	s := &stubRegistry{tenant: activeTenant, entity: gbEntity}
	rs := NewResolver(s.server(t).URL)

	got, err := rs.Resolve(context.Background(), envFor("tenant-01", "entity-01"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.JurisdictionContext != "GB" {
		t.Errorf("jurisdiction_context = %q, want GB from primary_jurisdiction_id", got.JurisdictionContext)
	}
	if got.Timezone != "Europe/London" {
		t.Errorf("timezone = %q, want Europe/London from primary_timezone", got.Timezone)
	}
	if got.DefaultCurrency != "GBP" || got.FiscalCalendarID != "fc-uk" {
		t.Errorf("unexpected resolved context: %+v", got)
	}
	// The entity's own residency policy is more specific than the tenant default.
	if got.ResidencyPolicyID != "res-uk" {
		t.Errorf("residency = %q, want the entity policy to win over the tenant default", got.ResidencyPolicyID)
	}
}

// §5 class S: the caller may request context but cannot override the result.
func TestCallerCannotOverrideResolvedJurisdiction(t *testing.T) {
	s := &stubRegistry{tenant: activeTenant, entity: gbEntity}
	rs := NewResolver(s.server(t).URL)

	e := envFor("tenant-01", "entity-01")
	e.JurisdictionContext = "AE" // caller claims a lower-tax jurisdiction

	got, err := rs.Resolve(context.Background(), e)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if applied := e.Apply(got); applied.JurisdictionContext != "GB" {
		t.Fatalf("caller claim survived: jurisdiction_context = %q, want GB", applied.JurisdictionContext)
	}
	// Overridden, and recorded as overridden rather than silently dropped.
	if len(got.Conflicts) == 0 || got.Conflicts[0].Field != "jurisdiction_context" {
		t.Fatalf("conflict not recorded: %+v", got.Conflicts)
	}
	if got.Conflicts[0].Claimed != "AE" || got.Conflicts[0].Resolved != "GB" {
		t.Fatalf("conflict does not preserve both values: %+v", got.Conflicts[0])
	}
}

// A user's own timezone legitimately differs from the tenant default, so it is
// the one resolved value Apply does not overwrite.
func TestCallerTimezoneNarrowsTenantDefault(t *testing.T) {
	s := &stubRegistry{tenant: activeTenant, entity: gbEntity}
	rs := NewResolver(s.server(t).URL)

	e := envFor("tenant-01", "entity-01")
	e.Timezone = "Asia/Dubai"

	got, _ := rs.Resolve(context.Background(), e)
	if applied := e.Apply(got); applied.Timezone != "Asia/Dubai" {
		t.Fatalf("timezone = %q, want the caller's own zone preserved", applied.Timezone)
	}
}

func TestSuspendedTenantIsRefusedDespiteA200(t *testing.T) {
	s := &stubRegistry{
		tenant: `{"tenant_id":"tenant-01","status":"SUSPENDED","lifecycle_state":"ACTIVE"}`,
		entity: gbEntity,
	}
	rs := NewResolver(s.server(t).URL)

	if _, err := rs.Resolve(context.Background(), envFor("tenant-01", "entity-01")); !errors.Is(err, ErrTenantNotResolvable) {
		t.Fatalf("err = %v, want ErrTenantNotResolvable (INV-01)", err)
	}
}

// Every tenant tenant-entity-registry-svc provisions starts as status=ACTIVE +
// lifecycle=ONBOARDING, and the registry's own enum calls that pair legitimate.
// Refusing it locks out every newly provisioned tenant — including from
// POST /v1/tenants/{id}/lifecycle, the one call that could move it on.
func TestOnboardingTenantIsOperable(t *testing.T) {
	s := &stubRegistry{
		tenant: `{"tenant_id":"tenant-01","status":"ACTIVE","lifecycle_state":"ONBOARDING",
			"primary_timezone":"Europe/London"}`,
		entity: gbEntity,
	}
	rs := NewResolver(s.server(t).URL)

	got, err := rs.Resolve(context.Background(), envFor("tenant-01", "entity-01"))
	if err != nil {
		t.Fatalf("err = %v, want a freshly provisioned tenant to resolve", err)
	}
	if got.TenantLifecycle != "ONBOARDING" {
		t.Fatalf("lifecycle = %q, want ONBOARDING carried through", got.TenantLifecycle)
	}
}

// The remaining two registry lifecycle values both mean "not transacting".
func TestNonTransactingLifecyclesAreRefused(t *testing.T) {
	for _, lifecycle := range []string{"SUSPENDED", "OFFBOARDING"} {
		t.Run(lifecycle, func(t *testing.T) {
			s := &stubRegistry{
				tenant: `{"tenant_id":"tenant-01","status":"ACTIVE","lifecycle_state":"` + lifecycle + `"}`,
				entity: gbEntity,
			}
			rs := NewResolver(s.server(t).URL)

			_, err := rs.Resolve(context.Background(), envFor("tenant-01", "entity-01"))
			if !errors.Is(err, ErrTenantNotResolvable) {
				t.Fatalf("err = %v, want %s to be refused", err, lifecycle)
			}
		})
	}
}

// An unrecognised lifecycle state — one added after this code was written —
// must read as not operable rather than being admitted by default.
func TestUnknownLifecycleIsNotOperable(t *testing.T) {
	s := &stubRegistry{
		tenant: `{"tenant_id":"tenant-01","status":"ACTIVE","lifecycle_state":"PENDING_LIQUIDATION"}`,
		entity: gbEntity,
	}
	rs := NewResolver(s.server(t).URL)

	if _, err := rs.Resolve(context.Background(), envFor("tenant-01", "entity-01")); !errors.Is(err, ErrTenantNotResolvable) {
		t.Fatalf("err = %v, want an unknown lifecycle to be refused", err)
	}
}

// INV-01/INV-02: naming another tenant's entity must not make it usable.
func TestCrossTenantEntityIsRefused(t *testing.T) {
	s := &stubRegistry{
		tenant: activeTenant,
		entity: `{"legal_entity_id":"entity-01","tenant_id":"tenant-99","entity_status":"ACTIVE","primary_jurisdiction_id":"GB"}`,
	}
	rs := NewResolver(s.server(t).URL)

	if _, err := rs.Resolve(context.Background(), envFor("tenant-01", "entity-01")); !errors.Is(err, ErrEntityNotResolvable) {
		t.Fatalf("err = %v, want a cross-tenant entity to be refused", err)
	}
}

// "Cannot answer" must never read as "no" — the contract jurisdiction-rules-svc
// already set for the platform.
func TestRegistry5xxIsUnavailableNotDenial(t *testing.T) {
	s := &stubRegistry{tenantCode: http.StatusServiceUnavailable}
	rs := NewResolver(s.server(t).URL)

	_, err := rs.Resolve(context.Background(), envFor("tenant-01", "entity-01"))
	if !errors.Is(err, ErrResolverUnavailable) {
		t.Fatalf("err = %v, want ErrResolverUnavailable", err)
	}
	if errors.Is(err, ErrTenantNotResolvable) {
		t.Fatal("an unreachable registry was reported as a resolved refusal")
	}
}

func TestUnknownTenantIsNotResolvable(t *testing.T) {
	s := &stubRegistry{tenantCode: http.StatusNotFound}
	rs := NewResolver(s.server(t).URL)

	if _, err := rs.Resolve(context.Background(), envFor("tenant-01", "entity-01")); !errors.Is(err, ErrTenantNotResolvable) {
		t.Fatalf("err = %v, want ErrTenantNotResolvable", err)
	}
}

// §4 makes legal_entity_id conditional, so a tenant-scoped call must resolve
// without one rather than being forced to invent an entity.
func TestResolveWithoutLegalEntity(t *testing.T) {
	s := &stubRegistry{tenant: activeTenant}
	rs := NewResolver(s.server(t).URL)

	got, err := rs.Resolve(context.Background(), envFor("tenant-01", ""))
	if err != nil {
		t.Fatalf("tenant-scoped resolve failed: %v", err)
	}
	if got.Timezone != "Europe/London" {
		t.Errorf("timezone = %q, want the tenant default", got.Timezone)
	}
	if got.JurisdictionContext != "" {
		t.Errorf("jurisdiction_context = %q, want empty with no entity in scope", got.JurisdictionContext)
	}
}

// Without propagation the registry's row-level security has no tenant to scope
// by, and the validating read is unattributable in the audit trail.
func TestResolverPropagatesEnvelopeOntoItsOwnCall(t *testing.T) {
	s := &stubRegistry{tenant: activeTenant}
	rs := NewResolver(s.server(t).URL)

	if _, err := rs.Resolve(context.Background(), envFor("tenant-01", "")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if s.seenTenant != "tenant-01" || s.seenActor != "user-01" || s.seenCorr != "corr-01" {
		t.Fatalf("envelope not propagated: tenant=%q actor=%q corr=%q", s.seenTenant, s.seenActor, s.seenCorr)
	}
}

func TestResolveRefusesEmptyTenant(t *testing.T) {
	rs := NewResolver("http://127.0.0.1:1")
	if _, err := rs.Resolve(context.Background(), Envelope{}); !errors.Is(err, ErrTenantNotResolvable) {
		t.Fatalf("err = %v, want ErrTenantNotResolvable before any network call", err)
	}
}
