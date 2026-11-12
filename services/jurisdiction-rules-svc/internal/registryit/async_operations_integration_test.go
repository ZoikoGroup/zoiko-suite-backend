//go:build integration

// API-5 asynchronous pack compile and test runs (migration 000036), against a real Postgres.
package registryit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"zoiko.io/identity-envelope/apicontract/asyncpgx"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
)

func newEnvAsync(signer domain.Signer) *env {
	pub := &spyPub{}
	auth := &recordingAuthz{}
	r := newRouter()
	h := handler.New(st, auth, pub, "platform-scope", zap.NewNop()).WithRegistry(st, pub).
		WithAggregateVersions(st).WithAsyncOperations(asyncpgx.New(st.OperationsDB()), st)
	if signer != nil {
		h.WithSigner(signer)
	}
	handler.RegisterRoutes(r, h)
	return &env{srv: r, pub: pub, auth: auth}
}

// await polls the operation until it is terminal.
func await(t *testing.T, e *env, location string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, op := e.do("GET", location, "poller", nil)
		require.Equal(t, 200, code, "%v", op)
		switch op["state"] {
		case "succeeded", "failed", "cancelled":
			return op
		}
		require.True(t, time.Now().Before(deadline), "operation never finished: %v", op)
		time.Sleep(50 * time.Millisecond)
	}
}

var async = map[string]string{"Prefer": "respond-async"}

func TestAPI5_CompileRunsAsAnOperation(t *testing.T) {
	keyRef := "async-key-" + strings.ToLower(uuid.NewString()[:6])
	signer, _ := newSigner(t, keyRef)
	e := newEnvAsync(signer)
	f := newFixture(t, e)

	// A compile of a version still in DRAFT fails; the failure is the command's own answer.
	code, v := e.do("POST", "/v1/admin/packs/"+f.packRef+"/versions", "author", f.manifest("2026.08.1", f.ruleID))
	mustStatus(t, 201, code, v)
	code, op, h := e.doFull("POST", f.path("2026.08.1", "/compile"), "compiler", nil, async)
	require.Equal(t, 202, code, "%v", op)
	require.Equal(t, "/v1/operations/"+op["id"].(string), h.Get("Location"))
	require.Equal(t, "jurisdiction_pack.compile", op["type"])
	done := await(t, e, h.Get("Location"))
	require.Equal(t, "failed", done["state"])
	require.Equal(t, "not_under_review", done["problem"].(map[string]any)["code"])

	// Under review it succeeds, and resource_ref names the artifact it produced.
	code, _ = e.do("POST", f.path("2026.08.1", "/submit-review"), "author", nil)
	require.Equal(t, 200, code)
	headers := map[string]string{"Prefer": "respond-async", "Idempotency-Key": "compile-" + uuid.NewString()}
	code, op, h = e.doFull("POST", f.path("2026.08.1", "/compile"), "compiler", nil, headers)
	require.Equal(t, 202, code, "%v", op)
	done = await(t, e, h.Get("Location"))
	require.Equal(t, "succeeded", done["state"], "%v", done)
	ref := done["resource_ref"].(string)
	require.Equal(t, "/v1/packs/"+f.packRef+"/versions/2026.08.1/artifact", ref)
	code, art := e.do("GET", ref, "reader", nil)
	require.Equal(t, 200, code, "%v", art)
	require.Equal(t, 1, e.pub.count("jurisdiction-pack.version-compiled"), "the background run published what the synchronous one would")

	// The same key and request answer the same operation, run once.
	code, again, _ := e.doFull("POST", f.path("2026.08.1", "/compile"), "compiler", nil, headers)
	require.Equal(t, 202, code)
	require.Equal(t, op["id"], again["id"])

	// Another tenant cannot read it.
	code, _ = e.doWithHeaders("GET", h.Get("Location"), "poller", nil, map[string]string{"X-Tenant-Id": otherTenant})
	require.Equal(t, 404, code)

	// A finished operation cannot be cancelled; a queued one can.
	code, body := e.do("POST", h.Get("Location")+"/cancel", "operator", nil)
	require.Equal(t, 409, code, "%v", body)
	require.Equal(t, "operation_not_cancelable", body["code"])
	queued := "op_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	_, err := pool.Exec(ctx, `INSERT INTO async_operations (id, tenant_id, principal_id, route, type, state, created_at, updated_at)
		VALUES ($1, $2, 'compiler', '/x', 'jurisdiction_pack.compile', 'queued', NOW(), NOW())`, queued, itTenant)
	require.NoError(t, err)
	code, body = e.do("POST", "/v1/operations/"+queued+"/cancel", "operator", nil)
	require.Equal(t, 200, code, "%v", body)
	require.Equal(t, "cancel_requested", body["state"])
	require.True(t, e.auth.pairs["jurisdiction_operation.cancel"])
	require.True(t, e.auth.pairs["jurisdiction_operation.view"])

	// Without Prefer the command stays synchronous.
	code, sync := e.do("POST", f.path("2026.08.1", "/compile"), "compiler", nil)
	require.Equal(t, 200, code, "a replay of the compile, answered inline: %v", sync)
}
