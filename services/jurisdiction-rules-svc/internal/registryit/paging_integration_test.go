//go:build integration

// API-4 cursor paging on the list routes, against a real Postgres.
package registryit_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// walk follows next_cursor from path until the last page and returns every item.
func walk(t *testing.T, e *env, path string, headers map[string]string) []map[string]any {
	t.Helper()
	var all []map[string]any
	next := path
	for pages := 0; next != ""; pages++ {
		require.Less(t, pages, 100, "paging does not terminate")
		code, page := e.doWithHeaders("GET", next, "reader", nil, headers)
		require.Equal(t, 200, code, "%v", page)
		items := page["items"].([]any)
		require.LessOrEqual(t, len(items), int(page["page_size"].(float64)))
		for _, it := range items {
			all = append(all, it.(map[string]any))
		}
		next = ""
		if c, _ := page["next_cursor"].(string); c != "" {
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			next = path + sep + "cursor=" + url.QueryEscape(c)
		}
	}
	return all
}

func TestAPI4_PagesCoverTheListOnceAndCursorsAreBound(t *testing.T) {
	e := newEnv()
	prefix := "PG" + strings.ToUpper(uuid.NewString()[:4])
	for i := 0; i < 7; i++ {
		code, body := e.do("POST", "/v1/admin/regimes", "author", map[string]any{"regime_code": prefix + string(rune('A'+i)), "regime_name": "paging " + prefix})
		require.Equal(t, 201, code, "%v", body)
	}

	// Every regime appears exactly once across the pages.
	all := walk(t, e, "/v1/regimes?page_size=3", nil)
	seen := map[string]int{}
	for _, it := range all {
		seen[it["regime_code"].(string)]++
	}
	for i := 0; i < 7; i++ {
		require.Equal(t, 1, seen[prefix+string(rune('A'+i))], "regime %d", i)
	}

	code, first := e.do("GET", "/v1/regimes?page_size=3", "reader", nil)
	require.Equal(t, 200, code)
	cursor := first["next_cursor"].(string)
	require.NotEmpty(t, cursor)

	// A tampered cursor, another tenant's, or one used on another route: 400.
	tampered := cursor[:len(cursor)-2] + "xx"
	code, body := e.do("GET", "/v1/regimes?page_size=3&cursor="+url.QueryEscape(tampered), "reader", nil)
	require.Equal(t, 400, code, "%v", body)
	require.Equal(t, "invalid_cursor", body["code"])
	code, body = e.doWithHeaders("GET", "/v1/regimes?page_size=3&cursor="+url.QueryEscape(cursor), "reader", nil, map[string]string{"X-Tenant-Id": otherTenant})
	require.Equal(t, 400, code, "a cursor is bound to its tenant: %v", body)
	code, body = e.do("GET", "/v1/packs?page_size=3&cursor="+url.QueryEscape(cursor), "reader", nil)
	require.Equal(t, 400, code, "a cursor is bound to its route: %v", body)

	// Out-of-range page_size, and limit/offset mixed with paging.
	code, body = e.do("GET", "/v1/regimes?page_size=201", "reader", nil)
	require.Equal(t, 400, code, "%v", body)
	code, body = e.do("GET", "/v1/regimes?page_size=2&offset=4", "reader", nil)
	require.Equal(t, 400, code, "%v", body)
	require.Equal(t, "invalid_paging", body["code"])

	// No paging parameter: the legacy answer, unchanged.
	code, legacy := e.do("GET", "/v1/regimes", "reader", nil)
	require.Equal(t, 200, code)
	_, wrapped := legacy["regimes"]
	require.True(t, wrapped, "legacy callers keep {\"regimes\": [...]}")
}

func TestAPI4_FiltersAreFilterBracketsInPagedMode(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "PGF")
	for i := 0; i < 3; i++ {
		code, src := e.do("POST", "/v1/admin/sources", "author", sourceBody(j.JurisdictionID))
		mustStatus(t, 201, code, src)
	}
	got := walk(t, e, "/v1/sources?page_size=2&filter[jurisdiction_id]="+j.JurisdictionID, nil)
	require.Len(t, got, 3)
	for _, s := range got {
		require.Equal(t, j.JurisdictionID, s["jurisdiction_id"])
	}

	code, body := e.do("GET", "/v1/sources?page_size=2&jurisdiction_id="+j.JurisdictionID, "reader", nil)
	require.Equal(t, 400, code, "%v", body)
	require.Equal(t, "unknown_filter", body["code"])
	require.Contains(t, body["detail"], "filter[jurisdiction_id]")
	code, body = e.do("GET", "/v1/sources?filter[colour]=red", "reader", nil)
	require.Equal(t, 400, code, "%v", body)
	require.Equal(t, "unknown_filter", body["code"])

	// The cursor is bound to the filter set: reusing it with another filter is refused.
	code, page := e.do("GET", "/v1/sources?page_size=2&filter[jurisdiction_id]="+j.JurisdictionID, "reader", nil)
	require.Equal(t, 200, code)
	c := page["next_cursor"].(string)
	code, body = e.do("GET", "/v1/sources?page_size=2&filter[authority]=HMRC&cursor="+url.QueryEscape(c), "reader", nil)
	require.Equal(t, 400, code, "%v", body)
	require.Equal(t, "invalid_cursor", body["code"])

	// Legacy bare filter, no paging: the original wrapped list.
	code, legacy := e.do("GET", "/v1/sources?jurisdiction_id="+j.JurisdictionID, "reader", nil)
	require.Equal(t, 200, code)
	require.Len(t, legacy["sources"].([]any), 3)
}

func TestAPI4_JurisdictionsPageWithTypeFilter(t *testing.T) {
	e := newEnv()
	for i := 0; i < 3; i++ {
		seedJurisdiction(t, "PGJ")
	}
	got := walk(t, e, "/v1/jurisdictions?page_size=50&filter[type]=COUNTRY&filter[active]=true", nil)
	require.GreaterOrEqual(t, len(got), 3)
	for _, j := range got {
		require.Equal(t, "COUNTRY", j["jurisdiction_type"])
	}
	code, arr := e.doRaw("GET", "/v1/jurisdictions?type=COUNTRY")
	require.Equal(t, 200, code)
	require.True(t, strings.HasPrefix(strings.TrimSpace(arr), "["), "legacy callers keep the bare array")
}
