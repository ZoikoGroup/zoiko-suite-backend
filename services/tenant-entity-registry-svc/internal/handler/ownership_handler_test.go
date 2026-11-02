package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

var ownershipReadPaths = []string{
	"/v1/ownership-graph",
	"/v1/entities/" + entityID + "/group",
	"/v1/entities/" + entityID + "/parents",
	"/v1/entities/" + entityID + "/control-chain",
}

// A date the caller cannot have meant is refused, not silently read as "now":
// answering today's structure to a question about last March would be a wrong
// answer that looks right.
func TestOwnershipReads_MalformedAsOfIs400(t *testing.T) {
	for _, path := range ownershipReadPaths {
		stub := &stubSvc{}
		rec := do(newRouter(stub), http.MethodGet, path+"?as_of=last-tuesday", "")
		assert.Equal(t, http.StatusBadRequest, rec.Code, path)
		assert.True(t, stub.gotAsOf.IsZero(), "%s: the service must not have been called", path)
	}
}

func TestOwnershipReads_AsOfReachesTheService(t *testing.T) {
	for _, path := range ownershipReadPaths {
		for raw, want := range map[string]time.Time{
			"2026-03-31":           time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
			"2026-03-31T10:30:00Z": time.Date(2026, 3, 31, 10, 30, 0, 0, time.UTC),
		} {
			stub := &stubSvc{}
			rec := do(newRouter(stub), http.MethodGet, path+"?as_of="+raw, "")
			require.Equal(t, http.StatusOK, rec.Code, path)
			assert.True(t, stub.gotAsOf.Equal(want), "%s as_of=%s: got %v", path, raw, stub.gotAsOf)
		}
	}
}

func TestOwnershipReads_AbsentAsOfMeansNow(t *testing.T) {
	for _, path := range ownershipReadPaths {
		stub := &stubSvc{}
		before := time.Now().UTC()
		rec := do(newRouter(stub), http.MethodGet, path, "")
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.WithinDuration(t, before, stub.gotAsOf, 5*time.Second, path)
	}
}

func TestOwnershipGraph_RootReachesTheService(t *testing.T) {
	stub := &stubSvc{}
	rec := do(newRouter(stub), http.MethodGet, "/v1/ownership-graph?root_entity_id="+entityID, "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, entityID, stub.gotID)
}

// Empty answers are empty lists, not null: a client iterating the body must not
// have to special-case a missing array.
func TestOwnershipReads_EmptyAnswersAreEmptyLists(t *testing.T) {
	rec := do(newRouter(&stubSvc{}), http.MethodGet, "/v1/ownership-graph", "")
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, []any{}, body["relationships"])
	assert.Equal(t, []any{}, body["entities"])
}

func TestOwnershipReads_ServiceRefusalsAreMapped(t *testing.T) {
	for _, path := range ownershipReadPaths {
		stub := &stubSvc{err: registry.ErrNotFound}
		rec := do(newRouter(stub), http.MethodGet, path, "")
		assert.Equal(t, http.StatusNotFound, rec.Code, path)
	}
}
