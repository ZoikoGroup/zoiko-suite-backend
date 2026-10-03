package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
)

func recs(n int) []domain.PopulationRecord {
	out := make([]domain.PopulationRecord, n)
	for i := range out {
		out[i] = domain.PopulationRecord{RecordID: fmt.Sprintf("r%05d", i), Reference: fmt.Sprintf("REF-%d", i),
			Amount: "1.50", Currency: "USD", Date: "2026-09-01"}
	}
	return out
}

// server pages `data` by the cursor (an offset) and lets a test tweak each page.
func server(t *testing.T, data []domain.PopulationRecord, tweak func(page int, r *pageResponse)) (*httptest.Server, *[]http.Header) {
	t.Helper()
	var seen []http.Header
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		if r.URL.Path != "/v1/control-populations/invoices" {
			http.NotFound(w, r)
			return
		}
		off, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		end := off + limit
		resp := pageResponse{Watermark: "wm-1"}
		if end < len(data) {
			resp.NextCursor = strconv.Itoa(end)
		} else {
			end = len(data)
		}
		resp.Records = data[off:end]
		if tweak != nil {
			tweak(off/limit, &resp)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(s.Close)
	return s, &seen
}

var scope = Scope{TenantID: "t1", PrincipalID: "alice", LegalEntityID: "e1", PeriodID: "2026-09", CorrelationID: "c1"}
var spec = Spec{System: "ar", Population: "invoices"}

func fetcher(s *httptest.Server) *HTTPFetcher {
	return NewHTTPFetcher(map[string]string{"ar": s.URL}, nil)
}

func TestFetch_PagesAndForwardsVerifiedScope(t *testing.T) {
	s, seen := server(t, recs(2500), nil)
	got, err := fetcher(s).Fetch(context.Background(), spec, scope)
	require.NoError(t, err)
	assert.Len(t, got.Records, 2500, "all three pages assembled")
	assert.Equal(t, "wm-1", got.Watermark)
	require.Len(t, *seen, 3)
	h := (*seen)[0]
	assert.Equal(t, "t1", h.Get("X-Tenant-Id"))
	assert.Equal(t, "alice", h.Get("X-Principal-Id"), "the source applies its own authorization to the same caller")
}

func TestFetch_RefusesUnregisteredSystemAndBadNames(t *testing.T) {
	f := NewHTTPFetcher(map[string]string{}, nil)
	_, err := f.Fetch(context.Background(), spec, scope)
	assert.True(t, errors.Is(err, ErrSourceUnknown))

	s, _ := server(t, recs(1), nil)
	_, err = fetcher(s).Fetch(context.Background(), Spec{System: "ar", Population: "../../etc/passwd"}, scope)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "the population name cannot traverse paths")
}

func TestFetch_NoWatermarkIsNotReproducible(t *testing.T) {
	s, _ := server(t, recs(3), func(_ int, r *pageResponse) { r.Watermark = "" })
	_, err := fetcher(s).Fetch(context.Background(), spec, scope)
	assert.ErrorIs(t, err, ErrSourceInconsistent)
}

func TestFetch_WatermarkMovingMidExtractionFails(t *testing.T) {
	s, _ := server(t, recs(2500), func(page int, r *pageResponse) { r.Watermark = fmt.Sprintf("wm-%d", page) })
	_, err := fetcher(s).Fetch(context.Background(), spec, scope)
	assert.ErrorIs(t, err, ErrSourceInconsistent, "a blend of two source states is never a population")
}

func TestFetch_DeclaredTotalsMustTie(t *testing.T) {
	data := recs(4)
	good := func(_ int, r *pageResponse) {
		r.DeclaredTotals = &declaredTotals{RowCount: 4, Totals: map[string]string{"USD": "6.000"}}
	}
	s, _ := server(t, data, good)
	_, err := fetcher(s).Fetch(context.Background(), spec, scope)
	assert.NoError(t, err, "6.000 and 4 x 1.50 are the same exact total")

	for name, d := range map[string]*declaredTotals{
		"row count":      {RowCount: 5, Totals: map[string]string{"USD": "6"}},
		"currency total": {RowCount: 4, Totals: map[string]string{"USD": "6.01"}},
		"missing ccy":    {RowCount: 4, Totals: map[string]string{}},
		"extra ccy":      {RowCount: 4, Totals: map[string]string{"USD": "6", "EUR": "9"}},
	} {
		d := d
		s, _ := server(t, data, func(_ int, r *pageResponse) { r.DeclaredTotals = d })
		_, err := fetcher(s).Fetch(context.Background(), spec, scope)
		assert.ErrorIs(t, err, ErrSourceInconsistent, name)
	}
}

func TestFetch_TruncatedTransferDetectedByDeclaredCount(t *testing.T) {
	data := recs(1500)
	s, _ := server(t, data, func(page int, r *pageResponse) {
		r.DeclaredTotals = &declaredTotals{RowCount: 1500, Totals: map[string]string{"USD": "2250"}}
		if page == 0 {
			r.NextCursor = "" // the source (or a proxy) stops early
		}
	})
	_, err := fetcher(s).Fetch(context.Background(), spec, scope)
	assert.ErrorIs(t, err, ErrSourceInconsistent, "a short read cannot pass as the whole population")
}

func TestFetch_InvalidRecordFailsWholePopulation(t *testing.T) {
	data := recs(3)
	data[1].Amount = "abc"
	s, _ := server(t, data, nil)
	_, err := fetcher(s).Fetch(context.Background(), spec, scope)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "a bad record is never silently skipped")
}

func TestFetch_SourceErrorsAreUnavailable(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer down.Close()
	_, err := fetcher(down).Fetch(context.Background(), spec, scope)
	assert.ErrorIs(t, err, ErrSourceUnavailable)

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, err = fetcher(closed).Fetch(context.Background(), spec, scope)
	assert.ErrorIs(t, err, ErrSourceUnavailable, "an unreachable source is an outage, not an empty population")

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer garbage.Close()
	_, err = fetcher(garbage).Fetch(context.Background(), spec, scope)
	assert.ErrorIs(t, err, ErrSourceInconsistent)
}

func TestFetch_StuckCursorDoesNotLoopForever(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(pageResponse{Watermark: "w", NextCursor: "same", Records: recs(1)})
	}))
	defer s.Close()
	_, err := fetcher(s).Fetch(context.Background(), spec, scope)
	assert.ErrorIs(t, err, ErrSourceInconsistent)
}

func TestParseEndpointsAndSpec(t *testing.T) {
	m, err := ParseEndpoints("ar=http://ar:8101/, gl=https://gl:8098")
	require.NoError(t, err)
	assert.Equal(t, "http://ar:8101", m["ar"])
	assert.Equal(t, "https://gl:8098", m["gl"])
	for _, bad := range []string{"ar", "ar=ftp://x", "=http://x", "ar=notaurl"} {
		_, err := ParseEndpoints(bad)
		assert.Error(t, err, bad)
	}
	empty, err := ParseEndpoints("")
	require.NoError(t, err)
	assert.Empty(t, empty)

	sp, err := ParseSpec(json.RawMessage(`{"system":"ar","population":"invoices"}`))
	require.NoError(t, err)
	assert.Equal(t, "ar/invoices", sp.Ref())
	_, err = ParseSpec(json.RawMessage(`{"system":"ar"}`))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestResolveScopeParams(t *testing.T) {
	sp := Spec{System: "inv", Population: "stock-count-lines", Params: map[string]string{
		"count_id": "${scope.count_id}", "side": "book"}}

	got, err := ResolveScopeParams(sp, []byte(`{"count_id":" c-123 "}`))
	require.NoError(t, err)
	assert.Equal(t, "c-123", got.Params["count_id"])
	assert.Equal(t, "book", got.Params["side"], "literals pass through")
	assert.Equal(t, "${scope.count_id}", sp.Params["count_id"], "the definition's spec is never mutated")
	assert.Equal(t, "inv/stock-count-lines?count_id=c-123&side=book", got.Ref(), "evidence names the resolved population")

	same, err := ResolveScopeParams(Spec{System: "s", Population: "p"}, nil)
	require.NoError(t, err)
	assert.Empty(t, same.Params)

	for name, scope := range map[string]string{
		"missing key":   `{}`,
		"null scope":    ``,
		"number":        `{"count_id":42}`,
		"empty":         `{"count_id":"  "}`,
		"newline":       "{\"count_id\":\"a\nb\"}",
		"too long":      `{"count_id":"` + string(make([]byte, 0)) + strings.Repeat("x", 600) + `"}`,
		"not an object": `[1]`,
		"malformed":     `{`,
	} {
		_, err := ResolveScopeParams(sp, []byte(scope))
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, name)
	}
}

func TestResolveScopeParams_CannotSmuggleReservedNamesOrUnknownReferences(t *testing.T) {
	// A param NAME can never be a reserved one, regardless of how its value is bound.
	_, err := ParseSpec(json.RawMessage(`{"system":"s","population":"p","params":{"legal_entity_id":"${scope.x}"}}`))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	// Only the exact ${scope.key} form is a reference; anything else is a literal.
	got, err := ResolveScopeParams(Spec{System: "s", Population: "p", Params: map[string]string{"a": "x${scope.k}y"}}, []byte(`{"k":"1"}`))
	require.NoError(t, err)
	assert.Equal(t, "x${scope.k}y", got.Params["a"])
}

func TestParamsAreForwardedToTheSourceAndValidated(t *testing.T) {
	var seen map[string][]string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query()
		_ = json.NewEncoder(w).Encode(pageResponse{Watermark: "w"})
	}))
	defer s.Close()
	f := NewHTTPFetcher(map[string]string{"ar": s.URL}, nil)

	_, err := f.Fetch(context.Background(), Spec{System: "ar", Population: "invoices",
		Params: map[string]string{"account_codes": "1200,1210", "normal_balance": "DEBIT"}}, scope)
	require.NoError(t, err)
	assert.Equal(t, []string{"1200,1210"}, seen["account_codes"])
	assert.Equal(t, []string{"DEBIT"}, seen["normal_balance"])
	assert.Equal(t, []string{"e1"}, seen["legal_entity_id"], "reserved names come from the verified scope only")

	// A hand-built Spec cannot bypass validation to override the entity.
	_, err = f.Fetch(context.Background(), Spec{System: "ar", Population: "invoices",
		Params: map[string]string{"legal_entity_id": "other"}}, scope)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = f.Fetch(context.Background(), Spec{System: "ar", Population: "invoices",
		Params: map[string]string{"Bad-Key": "x"}}, scope)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestNoPeriodSpecOmitsPeriodIDFromTheRequest(t *testing.T) {
	var seen map[string][]string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query()
		_ = json.NewEncoder(w).Encode(pageResponse{Watermark: "w"})
	}))
	defer s.Close()
	f := NewHTTPFetcher(map[string]string{"ar": s.URL}, nil)

	// Default: the run's period is sent.
	_, err := f.Fetch(context.Background(), Spec{System: "ar", Population: "p"}, scope)
	require.NoError(t, err)
	assert.Equal(t, []string{"2026-09"}, seen["period_id"])

	// no_period: a source that rejects period_id as an unknown parameter must not receive it,
	// even though a PERIOD_END run is required to carry one.
	_, err = f.Fetch(context.Background(), Spec{System: "ar", Population: "p", NoPeriod: true}, scope)
	require.NoError(t, err)
	_, present := seen["period_id"]
	assert.False(t, present)
	assert.Equal(t, []string{"e1"}, seen["legal_entity_id"], "the entity is still sent")

	sp, err := ParseSpec(json.RawMessage(`{"system":"ar","population":"p","no_period":true}`))
	require.NoError(t, err)
	assert.True(t, sp.NoPeriod)
	kept, err := ResolveScopeParams(Spec{System: "ar", Population: "p", NoPeriod: true, Params: map[string]string{"a": "${scope.k}"}}, []byte(`{"k":"v"}`))
	require.NoError(t, err)
	assert.True(t, kept.NoPeriod, "scope binding preserves the flag")
}
