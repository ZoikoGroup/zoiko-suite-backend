//go:build integration

// J-10: the entity-jurisdiction projection is rebuilt from tenant-entity-registry-svc
// for entities whose events left Kafka retention, against a real Postgres.
package registryit_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/entityregistry"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
)

// fakeRegistry answers the two registry reads the backfill makes.
type fakeRegistry struct {
	entities    []map[string]any
	assignments map[string][]map[string]any
	status      atomic.Int32 // non-zero: answer every request with it
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s := f.status.Load(); s != 0 {
		w.WriteHeader(int(s))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/tenants/") && strings.HasSuffix(r.URL.Path, "/entities"):
		_ = json.NewEncoder(w).Encode(f.entities)
	case strings.HasPrefix(r.URL.Path, "/v1/entities/") && strings.HasSuffix(r.URL.Path, "/jurisdictions"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/entities/"), "/jurisdictions")
		as := f.assignments[id]
		if as == nil {
			as = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(as)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func backfillEnv(t *testing.T, reg *fakeRegistry) (*env, *spyPub) {
	t.Helper()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	pub := &spyPub{}
	auth := &recordingAuthz{}
	r := newRouter()
	h := handler.New(st, auth, pub, "platform-scope", zap.NewNop()).WithRegistry(st, pub).
		WithEntityApplicability(st).WithEntityBackfill(entityregistry.New(srv.URL), st)
	handler.RegisterRoutes(r, h)
	return &env{srv: r, pub: pub, auth: auth}, pub
}

func TestJ10_BackfillProjectsEntitiesWhoseEventsAreGone(t *testing.T) {
	primary := seedJurisdiction(t, "J10P")
	extra := seedJurisdiction(t, "J10X")
	entity, foreign := uuid.NewString(), uuid.NewString()
	assignment := uuid.NewString()
	since := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	reg := &fakeRegistry{
		entities: []map[string]any{
			{"legal_entity_id": entity, "tenant_id": itTenant, "primary_jurisdiction_id": primary.JurisdictionID, "record_version": 2, "created_at": since},
			{"legal_entity_id": foreign, "tenant_id": otherTenant, "primary_jurisdiction_id": primary.JurisdictionID, "record_version": 1, "created_at": since},
		},
		assignments: map[string][]map[string]any{entity: {{
			"assignment_id": assignment, "tenant_id": itTenant, "legal_entity_id": entity, "jurisdiction_id": extra.JurisdictionID,
			"assignment_type": "TAX_ONLY", "effective_from": since, "effective_to": nil, "record_version": 1,
		}}},
	}
	e, pub := backfillEnv(t, reg)

	// Before: no event ever reached this service, so nothing is projected.
	code, got := e.do("GET", "/v1/entities/"+entity+"/jurisdictions", "reader", nil)
	require.Equal(t, 200, code, "%v", got)
	require.Empty(t, got["jurisdictions"])

	code, res := e.do("POST", "/v1/admin/entity-applicability:backfill", "steward", nil)
	require.Equal(t, 200, code, "%v", res)
	require.Equal(t, float64(1), res["entities_scanned"])
	require.Equal(t, float64(2), res["applied"], "the primary jurisdiction and the assignment")
	require.Len(t, res["skipped"], 1, "the other tenant's entity is never projected here")
	require.True(t, e.auth.pairs["jurisdiction_entity.backfill"])
	require.Equal(t, 1, pub.count("jurisdiction-entity-applicability.backfilled"))

	code, got = e.do("GET", "/v1/entities/"+entity+"/jurisdictions", "reader", nil)
	require.Equal(t, 200, code)
	require.Len(t, got["jurisdictions"], 2)

	// Running it again changes nothing.
	code, res = e.do("POST", "/v1/admin/entity-applicability:backfill", "steward", nil)
	require.Equal(t, 200, code, "%v", res)
	require.Equal(t, float64(0), res["applied"])
	require.Equal(t, float64(2), res["unchanged"])

	// A newer live event wins over the backfill's older version.
	applied, err := st.ApplyEntityApplicability(ctx, "evt-"+uuid.NewString(), "entity.jurisdiction.changed", domain.EntityJurisdictionApplicability{
		ApplicabilityID: assignment, TenantID: itTenant, LegalEntityID: entity, JurisdictionID: primary.JurisdictionID,
		ApplicabilityType: "TAX_ONLY", EffectiveFrom: since, SourceObjectVersion: 5})
	require.NoError(t, err)
	require.True(t, applied)
	reg.assignments[entity][0]["record_version"] = 3
	code, res = e.do("POST", "/v1/admin/entity-applicability:backfill", "steward", nil)
	require.Equal(t, 200, code, "%v", res)
	var jur string
	require.NoError(t, pool.QueryRow(ctx, `SELECT jurisdiction_id::text FROM entity_jurisdiction_applicability WHERE applicability_id=$1`, assignment).Scan(&jur))
	require.Equal(t, primary.JurisdictionID, jur, "the live event's version 5 is newer than the registry row's 3")
}

func TestJ10_BackfillRefusalsAndOutages(t *testing.T) {
	reg := &fakeRegistry{entities: []map[string]any{}}
	e, _ := backfillEnv(t, reg)

	code, body := e.doWithHeaders("POST", "/v1/admin/entity-applicability:backfill", "steward", nil, map[string]string{"X-Tenant-Id": ""})
	require.Equal(t, 401, code, "%v", body)
	require.Equal(t, "missing_tenant", body["code"])

	code, body = e.do("POST", "/v1/admin/entity-applicability:backfill", "steward", map[string]any{"legal_entity_id": "nope"})
	require.Equal(t, 400, code, "%v", body)

	reg.status.Store(http.StatusForbidden)
	code, body = e.do("POST", "/v1/admin/entity-applicability:backfill", "steward", nil)
	require.Equal(t, 502, code, "%v", body)
	require.Equal(t, "registry_refused", body["code"])

	reg.status.Store(http.StatusInternalServerError)
	code, body = e.do("POST", "/v1/admin/entity-applicability:backfill", "steward", nil)
	require.Equal(t, 503, code, "%v", body)
	require.Equal(t, "registry_unavailable", body["code"])

	auth := &recordingAuthz{deny: "jurisdiction_entity.backfill"}
	srv := httptest.NewServer(reg)
	defer srv.Close()
	r := newRouter()
	handler.RegisterRoutes(r, handler.New(st, auth, &spyPub{}, "s", zap.NewNop()).WithEntityApplicability(st).WithEntityBackfill(entityregistry.New(srv.URL), st))
	denied := &env{srv: r, auth: auth}
	code, _ = denied.do("POST", "/v1/admin/entity-applicability:backfill", "intruder", nil)
	require.Equal(t, 403, code)
}
