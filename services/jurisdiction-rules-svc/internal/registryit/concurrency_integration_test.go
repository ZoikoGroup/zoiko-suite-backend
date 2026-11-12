//go:build integration

// Optimistic concurrency on aggregates (API-3, migration 000034) and the
// aggregate_version of registry events (J-12), against a real Postgres.
package registryit_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// A source's read answers its version; a command naming an older one is
// refused with 412 and changes nothing; the current one runs and answers the
// new version.
func TestAPI3_SourceCommandsCheckIfMatch(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "IFM")
	code, src := e.do("POST", "/v1/admin/sources", "author", sourceBody(j.JurisdictionID))
	mustStatus(t, 201, code, src)
	id := src["source_id"].(string)

	code, _, h := e.doFull("GET", "/v1/sources/"+id, "reader", nil, nil)
	require.Equal(t, 200, code)
	require.Equal(t, `"1"`, h.Get("ETag"))

	code, body, h := e.doFull("POST", "/v1/admin/sources/"+id+"/review", "reviewer", nil, map[string]string{"If-Match": `"7"`})
	require.Equal(t, http.StatusPreconditionFailed, code, "%v", body)
	require.Equal(t, "precondition_failed", body["code"])
	require.Equal(t, `"1"`, h.Get("ETag"), "a 412 names the current version")
	var reviewed *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT reviewed_by_principal_id FROM regulatory_sources WHERE source_id=$1`, id).Scan(&reviewed))
	require.Nil(t, reviewed, "a refused command changes nothing")

	code, body, h = e.doFull("POST", "/v1/admin/sources/"+id+"/review", "reviewer", nil, map[string]string{"If-Match": `"1"`})
	require.Equal(t, 200, code, "%v", body)
	require.Equal(t, `"2"`, h.Get("ETag"), "the command answers the version it produced")

	code, _, _ = e.doFull("POST", "/v1/admin/sources/"+id+"/supersede", "author", map[string]any{"replacement_source_id": id}, map[string]string{"If-Match": `"1"`})
	require.Equal(t, http.StatusPreconditionFailed, code, "the old version is stale now")

	code, body, _ = e.doFull("POST", "/v1/admin/sources/"+id+"/review", "reviewer", nil, map[string]string{"If-Match": `"x"`})
	require.Equal(t, 400, code, "%v", body)
	require.Equal(t, "invalid_if_match", body["code"])
}

// A no-op write (an idempotent replay) does not move the version.
func TestAPI3_AReplayDoesNotMoveTheVersion(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "IFR")
	_, src := e.do("POST", "/v1/admin/sources", "author", sourceBody(j.JurisdictionID))
	id := src["source_id"].(string)
	code, _ := e.do("POST", "/v1/admin/sources/"+id+"/review", "reviewer", nil)
	require.Equal(t, 200, code)
	_, err := pool.Exec(ctx, `UPDATE regulatory_sources SET authority = authority WHERE source_id=$1`, id)
	require.NoError(t, err)
	require.Equal(t, 2, countRows(t, `SELECT row_version FROM regulatory_sources WHERE source_id=$1`, id))
}

// J-12: a registry event's envelope carries the aggregate's version: 1 for
// the create, the row_version after each command.
func TestJ12_RegistryEventsCarryTheAggregateVersion(t *testing.T) {
	e := outboxEnv()
	j := seedJurisdiction(t, "J12")
	code, src := e.do("POST", "/v1/admin/sources", "author", sourceBody(j.JurisdictionID))
	mustStatus(t, 201, code, src)
	id := src["source_id"].(string)
	code, _ = e.do("POST", "/v1/admin/sources/"+id+"/review", "reviewer", nil)
	require.Equal(t, 200, code)

	version := func(eventType string) float64 {
		var raw []byte
		require.NoError(t, pool.QueryRow(ctx, `SELECT event_payload FROM event_outbox WHERE event_type=$1 AND partition_key=$2
			ORDER BY created_at DESC LIMIT 1`, eventType, id).Scan(&raw))
		var env map[string]any
		require.NoError(t, json.Unmarshal(raw, &env))
		_, inPayload := env["payload"].(map[string]any)["aggregate_version"]
		require.False(t, inPayload, "the version travels in the envelope, not the payload")
		v, _ := env["aggregate_version"].(float64)
		return v
	}
	require.Equal(t, float64(1), version("regulatory-source.captured"))
	require.Equal(t, float64(2), version("regulatory-source.reviewed"))
}
