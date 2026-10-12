package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/currency-registry-svc/internal/domain"
	"zoiko.io/currency-registry-svc/internal/handler"
	"zoiko.io/currency-registry-svc/internal/memstore"
	svcmiddleware "zoiko.io/currency-registry-svc/internal/middleware"
	"zoiko.io/currency-registry-svc/internal/service"
)

// ── harness ──────────────────────────────────────────────────────────────────

// stubAuthz denies per action; everything else is granted.
type stubAuthz struct {
	deny        map[string]error
	calls       []string
	calledUsers []string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, principal, _, action string) error {
	a.calls = append(a.calls, action)
	a.calledUsers = append(a.calledUsers, principal)
	return a.deny[action]
}

type testEnv struct {
	t      *testing.T
	store  *memstore.Store
	authz  *stubAuthz
	router http.Handler
	now    time.Time
	keySeq int
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{t: t, store: memstore.New(), authz: &stubAuthz{deny: map[string]error{}},
		now: time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)}
	svc := service.New(e.store).WithClock(func() time.Time { return e.now })
	h := handler.New(svc, e.authz, zap.NewNop(), nil)
	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, h)
	e.router = r
	return e
}

type opts struct {
	tenant, actor string
	noKey         bool
	key           string
}

func (e *testEnv) do(method, path string, body any, o ...opts) *httptest.ResponseRecorder {
	e.t.Helper()
	op := opts{tenant: "tenant-a", actor: "steward-1"}
	if len(o) > 0 {
		op = o[0]
		if op.tenant == "" {
			op.tenant = "tenant-a"
		}
		if op.actor == "" {
			op.actor = "steward-1"
		}
	}
	var rdr *bytes.Reader
	switch b := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		require.NoError(e.t, err)
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("X-Tenant-Id", op.tenant)
	req.Header.Set("X-Principal-Id", op.actor)
	req.Header.Set("X-Correlation-ID", "corr-test-1")
	if method == http.MethodPost && !op.noKey {
		key := op.key
		if key == "" {
			e.keySeq++
			key = fmt.Sprintf("key-%d", e.keySeq)
		}
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v), "body: %s", rec.Body.String())
	return v
}

type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *testEnv) wantErr(rec *httptest.ResponseRecorder, status int, code string) {
	e.t.Helper()
	require.Equal(e.t, status, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(e.t, code, decode[errBody](e.t, rec).Code)
}

// Test fixtures use a few ISO codes inline; non-test code has none.
func row(alpha, numeric, name string, mu int) domain.ImportRow {
	return domain.ImportRow{AlphaCode: alpha, NumericCode: numeric, Name: name, MinorUnit: json.Number(fmt.Sprint(mu))}
}

func importBody(source, version string, rows []domain.ImportRow, effective *time.Time) map[string]any {
	b := map[string]any{
		"source_name": source, "source_version": version,
		"manifest_hash": domain.ManifestHash(rows), "reason": "quarterly ISO 4217 maintenance update", "rows": rows,
	}
	if effective != nil {
		b["effective_at"] = effective.Format(time.RFC3339)
	}
	return b
}

func (e *testEnv) importRows(actor, version string, rows []domain.ImportRow) *httptest.ResponseRecorder {
	return e.do("POST", "/v1/currency-imports", importBody("iso-4217-maintenance", version, rows, nil), opts{actor: actor})
}

func (e *testEnv) mustImport(actor, version string, rows []domain.ImportRow) domain.Import {
	e.t.Helper()
	rec := e.importRows(actor, version, rows)
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	return decode[domain.Import](e.t, rec)
}

func (e *testEnv) currency(code string) domain.Currency {
	e.t.Helper()
	rec := e.do("GET", "/v1/currencies/"+code, nil)
	require.Equal(e.t, http.StatusOK, rec.Code, rec.Body.String())
	return decode[domain.Currency](e.t, rec)
}

func (e *testEnv) command(cmd string, c domain.Currency, version int64, actor string) *httptest.ResponseRecorder {
	return e.do("POST", "/v1/currencies/"+c.CurrencyID+":"+cmd,
		map[string]any{"expected_version": version, "reason": "finance approval FA-2026-001"}, opts{actor: actor})
}

// activated imports USD(2) as importer-1 and activates it as steward-2.
func (e *testEnv) activated(code, numeric string, mu int) domain.Currency {
	e.t.Helper()
	e.mustImport("importer-1", "act-"+code, []domain.ImportRow{row(code, numeric, code+" test currency", mu)})
	c := e.currency(code)
	rec := e.command("activate", c, c.Version, "steward-2")
	require.Equal(e.t, http.StatusOK, rec.Code, rec.Body.String())
	return decode[domain.Currency](e.t, rec)
}

type outboxEvent struct {
	TenantID      string `json:"tenant_id"`
	EventType     string `json:"event_type"`
	ActorID       string `json:"actor_id"`
	CorrelationID string `json:"correlation_id"`
	SourceService string `json:"source_service"`
	Payload       struct {
		Scope         string `json:"scope"`
		TenantID      string `json:"tenant_id"`
		ObjectID      string `json:"object_id"`
		ObjectVersion int64  `json:"object_version"`
		EffectiveAt   string `json:"effective_at"`
		RecordedAt    string `json:"recorded_at"`
		Actor         string `json:"actor"`
		CorrelationID string `json:"correlation_id"`
		NewStatus     string `json:"new_status"`
		ChangeKind    string `json:"change_kind"`
		Enabled       *bool  `json:"enabled"`
	} `json:"payload"`
}

func (e *testEnv) events(eventType string) []outboxEvent {
	var out []outboxEvent
	for _, ob := range e.store.Outbox() {
		if ob.EventType != eventType {
			continue
		}
		var ev outboxEvent
		require.NoError(e.t, json.Unmarshal(ob.Payload, &ev))
		out = append(out, ev)
	}
	return out
}

// ── 2. import ────────────────────────────────────────────────────────────────

func TestImport_ValidApplies(t *testing.T) {
	e := newEnv(t)
	rec := e.importRows("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2), row("JPY", "392", "Yen", 0)})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	imp := decode[domain.Import](t, rec)
	assert.Equal(t, domain.ImportApplied, imp.Status)
	assert.Equal(t, 2, imp.Summary.Created)
	assert.Equal(t, "importer-1", imp.Actor)

	cs := e.store.Currencies()
	require.Len(t, cs, 2)
	for _, c := range cs {
		assert.Equal(t, domain.StatusKnown, c.Status, "new currencies enter as KNOWN")
		assert.Equal(t, int64(1), c.Version)
		assert.Equal(t, "importer-1", c.LastImportActor)
		assert.Len(t, c.CurrencyID, 36)
		assert.Equal(t, byte('7'), c.CurrencyID[14], "UUIDv7 expected, got %s", c.CurrencyID)
	}
	assert.Len(t, e.store.MinorUnitRows(), 2)
	assert.Len(t, e.events("CurrencyUpdated"), 2)
}

func TestImport_EventCarriesTenantObjectVersionEffectiveAt(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	evs := e.events("CurrencyUpdated")
	require.Len(t, evs, 1)
	ev := evs[0]
	c := e.currency("USD")
	assert.Equal(t, "tenant-a", ev.TenantID)
	assert.Equal(t, "tenant-a", ev.Payload.TenantID)
	assert.Equal(t, "GLOBAL", ev.Payload.Scope)
	assert.Equal(t, c.CurrencyID, ev.Payload.ObjectID)
	assert.Equal(t, int64(1), ev.Payload.ObjectVersion)
	assert.Equal(t, "CREATED", ev.Payload.ChangeKind)
	assert.Equal(t, e.now.Format(time.RFC3339), ev.Payload.EffectiveAt)
	assert.NotEmpty(t, ev.Payload.RecordedAt)
	assert.Equal(t, "importer-1", ev.Payload.Actor)
	assert.Equal(t, "corr-test-1", ev.Payload.CorrelationID)
	assert.Equal(t, "corr-test-1", ev.CorrelationID)
	assert.Equal(t, "currency-registry-svc", ev.SourceService)
}

func TestImport_ManifestHashMismatch_SourceUnverified_NothingApplied(t *testing.T) {
	e := newEnv(t)
	rows := []domain.ImportRow{row("USD", "840", "US Dollar", 2)}
	body := importBody("iso-4217-maintenance", "2026-01", rows, nil)
	body["manifest_hash"] = domain.ManifestHash([]domain.ImportRow{row("USD", "840", "US Dollar", 3)}) // hash of OTHER content
	rec := e.do("POST", "/v1/currency-imports", body, opts{actor: "importer-1"})
	e.wantErr(rec, http.StatusUnprocessableEntity, "SOURCE_UNVERIFIED")
	assert.Empty(t, e.store.Currencies())
	assert.Empty(t, e.store.MinorUnitRows())
	assert.Empty(t, e.store.Imports())
	assert.Empty(t, e.store.Outbox())
}

func TestImport_InvalidRowsQuarantineWholeImport(t *testing.T) {
	good := row("USD", "840", "US Dollar", 2)
	cases := []struct {
		name  string
		setup []domain.ImportRow // pre-existing registry content
		bad   domain.ImportRow
		extra []domain.ImportRow
		want  string
	}{
		{name: "alpha too short", bad: row("US", "100", "Bad", 2), want: "alpha_code"},
		{name: "alpha lowercase", bad: row("abc", "100", "Bad", 2), want: "alpha_code"},
		{name: "numeric too long", bad: row("AAA", "1000", "Bad", 2), want: "numeric_code"},
		{name: "minor unit above range", bad: row("AAA", "100", "Bad", 7), want: "minor_unit"},
		{name: "minor unit negative", bad: row("AAA", "100", "Bad", -1), want: "minor_unit"},
		{name: "minor unit fractional", bad: domain.ImportRow{AlphaCode: "AAA", NumericCode: "100", Name: "Bad", MinorUnit: "2.5"}, want: "integer"},
		{name: "empty name", bad: row("AAA", "100", "", 2), want: "name"},
		{name: "duplicate alpha in file", bad: row("USD", "999", "Dup", 2), want: "duplicate alpha_code"},
		{name: "duplicate numeric in file", bad: row("AAA", "840", "Dup", 2), want: "duplicate numeric_code"},
		{name: "numeric conflicts with existing alpha", setup: []domain.ImportRow{row("BBB", "200", "Existing", 2)},
			bad: row("BBB", "201", "Existing", 2), want: "conflicts with registered numeric_code"},
		{name: "numeric held by another currency", setup: []domain.ImportRow{row("BBB", "200", "Existing", 2)},
			bad: row("CCC", "200", "Other", 2), want: "already registered"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.setup != nil {
				e.mustImport("importer-0", "seed", tc.setup)
			}
			baseCurrencies, baseMU, baseOutbox := len(e.store.Currencies()), len(e.store.MinorUnitRows()), len(e.store.Outbox())

			rec := e.importRows("importer-1", "2026-02", []domain.ImportRow{good, tc.bad})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			imp := decode[domain.Import](t, rec)
			assert.Equal(t, domain.ImportQuarantined, imp.Status)
			assert.NotEmpty(t, imp.QuarantineReason)
			require.NotEmpty(t, imp.RowErrors)
			assert.Contains(t, fmt.Sprint(imp.RowErrors), tc.want)

			// The valid row in the same file was NOT applied.
			assert.Len(t, e.store.Currencies(), baseCurrencies)
			assert.Len(t, e.store.MinorUnitRows(), baseMU)
			// Exactly one new event: the quarantine notice.
			assert.Len(t, e.store.Outbox(), baseOutbox+1)
			q := e.events("CurrencyImportQuarantined")
			require.Len(t, q, 1)
			assert.Equal(t, imp.ImportID, q[0].Payload.ObjectID)
		})
	}
}

func TestImport_ReplayReturnsOriginalAndDuplicatesNothing(t *testing.T) {
	e := newEnv(t)
	rows := []domain.ImportRow{row("USD", "840", "US Dollar", 2), row("JPY", "392", "Yen", 0)}
	first := e.do("POST", "/v1/currency-imports", importBody("iso-4217-maintenance", "2026-01", rows, nil), opts{actor: "importer-1", key: "k-1"})
	require.Equal(t, http.StatusCreated, first.Code)
	orig := decode[domain.Import](t, first)
	snapCurrencies, snapMU, snapOutbox := len(e.store.Currencies()), len(e.store.MinorUnitRows()), len(e.store.Outbox())

	// (a) same Idempotency-Key, same request.
	again := e.do("POST", "/v1/currency-imports", importBody("iso-4217-maintenance", "2026-01", rows, nil), opts{actor: "importer-1", key: "k-1"})
	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	assert.Equal(t, "true", again.Header().Get("Idempotent-Replay"))
	assert.Equal(t, orig.ImportID, decode[domain.Import](t, again).ImportID)

	// (b) a NEW key, same (source, version, manifest hash) - e.g. the file was re-submitted.
	resub := e.do("POST", "/v1/currency-imports", importBody("iso-4217-maintenance", "2026-01", rows, nil), opts{actor: "importer-9", key: "k-2"})
	require.Equal(t, http.StatusOK, resub.Code, resub.Body.String())
	assert.Equal(t, orig.ImportID, decode[domain.Import](t, resub).ImportID, "the ORIGINAL result, not a new import")

	assert.Len(t, e.store.Currencies(), snapCurrencies)
	assert.Len(t, e.store.MinorUnitRows(), snapMU)
	assert.Len(t, e.store.Outbox(), snapOutbox)
	assert.Len(t, e.store.Imports(), 1)
}

func TestImport_SameSourceVersionDifferentContent_DuplicateCandidate(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	rec := e.importRows("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 3)})
	e.wantErr(rec, http.StatusConflict, "DUPLICATE_CANDIDATE")
	assert.Len(t, e.store.MinorUnitRows(), 1)
}

func TestImport_SameIdempotencyKeyDifferentRequest_Refused(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/v1/currency-imports", importBody("s", "1", []domain.ImportRow{row("USD", "840", "US Dollar", 2)}, nil), opts{key: "same"})
	rec := e.do("POST", "/v1/currency-imports", importBody("s", "2", []domain.ImportRow{row("JPY", "392", "Yen", 0)}, nil), opts{key: "same"})
	e.wantErr(rec, http.StatusUnprocessableEntity, "CONTEXT_INVALID")
	assert.Len(t, e.store.Currencies(), 1)
}

func TestImport_RequiresIdempotencyKeyAndReason(t *testing.T) {
	e := newEnv(t)
	rows := []domain.ImportRow{row("USD", "840", "US Dollar", 2)}
	e.wantErr(e.do("POST", "/v1/currency-imports", importBody("s", "1", rows, nil), opts{noKey: true}), http.StatusBadRequest, "CONTEXT_INVALID")
	body := importBody("s", "1", rows, nil)
	delete(body, "reason")
	e.wantErr(e.do("POST", "/v1/currency-imports", body), http.StatusBadRequest, "CONTEXT_INVALID")
	assert.Empty(t, e.store.Currencies())
}

func TestImport_AuthzDenied_403_AndUnavailable_503(t *testing.T) {
	e := newEnv(t)
	rows := []domain.ImportRow{row("USD", "840", "US Dollar", 2)}
	e.authz.deny["CURRENCY_IMPORT"] = domain.ErrAuthorizationDenied
	e.wantErr(e.importRows("importer-1", "1", rows), http.StatusForbidden, "FORBIDDEN")
	e.authz.deny["CURRENCY_IMPORT"] = domain.ErrAuthzServiceUnavailable
	e.wantErr(e.importRows("importer-1", "1", rows), http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
	assert.Empty(t, e.store.Currencies())
	assert.Empty(t, e.store.Outbox())
}

func TestImport_MinorUnitChange_NewVersion_OldStaysResolvableAsOf(t *testing.T) {
	e := newEnv(t)
	t0 := e.now
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	before := e.store.MinorUnitRows()
	require.Len(t, before, 1)

	// A later source version changes the exponent, effective in the future relative to t0.
	t1 := t0.Add(48 * time.Hour)
	e.now = t1
	rec := e.do("POST", "/v1/currency-imports",
		importBody("iso-4217-maintenance", "2026-02", []domain.ImportRow{row("USD", "840", "US Dollar", 3)}, &t1), opts{actor: "importer-2"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	imp := decode[domain.Import](t, rec)
	assert.Equal(t, 1, imp.Summary.MinorUnitChanged)

	rows := e.store.MinorUnitRows()
	require.Len(t, rows, 2, "a change INSERTS a new minor-unit version")
	assert.Equal(t, before[0], rows[0], "the previous version row is untouched (append-only)")
	cur := e.currency("USD")
	assert.Equal(t, int64(2), cur.Version, "a change bumps the currency version")
	assert.Equal(t, "importer-2", cur.LastImportActor)

	// Now: new precision.
	assert.Equal(t, 3, cur.MinorUnit.MinorUnit)
	// Historical precision unchanged (negative path 25).
	asOfOld := e.do("GET", "/v1/currencies/USD?as_of="+t0.Add(time.Hour).Format(time.RFC3339), nil)
	require.Equal(t, http.StatusOK, asOfOld.Code)
	assert.Equal(t, 2, decode[domain.Currency](t, asOfOld).MinorUnit.MinorUnit)
	asOfNew := e.do("GET", "/v1/currencies/USD?as_of="+t1.Add(time.Hour).Format(time.RFC3339), nil)
	assert.Equal(t, 3, decode[domain.Currency](t, asOfNew).MinorUnit.MinorUnit)

	// Version history lists both with a derived validity boundary.
	vs := decode[service.MinorUnitVersions](t, e.do("GET", "/v1/currencies/USD/minor-unit-versions", nil))
	require.Len(t, vs.Versions, 2)
	assert.Equal(t, 2, vs.Versions[0].MinorUnit)
	require.NotNil(t, vs.Versions[0].ValidTo)
	assert.True(t, vs.Versions[0].ValidTo.Equal(t1))
	assert.Nil(t, vs.Versions[1].ValidTo)
	assert.Equal(t, "2026-01", vs.Versions[0].SourceVersion)
	assert.Equal(t, "2026-02", vs.Versions[1].SourceVersion)
	assert.Contains(t, vs.Versions[1].EvidenceRef, imp.ImportID)

	changed := e.events("CurrencyUpdated")
	require.Len(t, changed, 2)
	assert.Equal(t, "MINOR_UNIT_CHANGED", changed[1].Payload.ChangeKind)
	assert.Equal(t, int64(2), changed[1].Payload.ObjectVersion)
}

func TestImport_UnchangedRowCreatesNothing(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	imp := e.mustImport("importer-1", "2026-02", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	assert.Equal(t, 1, imp.Summary.Unchanged)
	assert.Len(t, e.store.MinorUnitRows(), 1)
	assert.Equal(t, int64(1), e.currency("USD").Version)
}

func TestImport_BackdatedMinorUnitChange_Quarantined(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	past := e.now.Add(-24 * time.Hour)
	rec := e.do("POST", "/v1/currency-imports", importBody("iso-4217-maintenance", "2026-02", []domain.ImportRow{row("USD", "840", "US Dollar", 3)}, &past))
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, domain.ImportQuarantined, decode[domain.Import](t, rec).Status)
	assert.Len(t, e.store.MinorUnitRows(), 1)
}

// ── 3. activate / restrict / retire ──────────────────────────────────────────

func TestActivate_HappyPath_VersionBumpHistoryAndEvent(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	c := e.currency("USD")
	rec := e.do("POST", "/v1/currencies/"+c.CurrencyID+":activate",
		map[string]any{"expected_version": c.Version, "reason": "finance approval FA-1", "approver_id": "cfo-1"}, opts{actor: "steward-2"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decode[domain.Currency](t, rec)
	assert.Equal(t, domain.StatusSupported, got.Status)
	assert.Equal(t, int64(2), got.Version)
	assert.Contains(t, e.authz.calls, "CURRENCY_ACTIVATE")

	h := e.store.History()
	require.Len(t, h, 1)
	assert.Equal(t, domain.StatusKnown, h[0].FromStatus)
	assert.Equal(t, domain.StatusSupported, h[0].ToStatus)
	assert.Equal(t, "steward-2", h[0].Actor)
	assert.Equal(t, "cfo-1", h[0].Approver)
	assert.Equal(t, "finance approval FA-1", h[0].Reason)

	evs := e.events("CurrencySupportChanged")
	require.Len(t, evs, 1)
	assert.Equal(t, c.CurrencyID, evs[0].Payload.ObjectID)
	assert.Equal(t, int64(2), evs[0].Payload.ObjectVersion)
	assert.Equal(t, "SUPPORTED", evs[0].Payload.NewStatus)
	assert.Equal(t, "tenant-a", evs[0].TenantID)
	assert.NotEmpty(t, evs[0].Payload.EffectiveAt)
}

func TestActivate_RequiresIdempotencyKeyAndExpectedVersion(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	c := e.currency("USD")
	path := "/v1/currencies/" + c.CurrencyID + ":activate"

	e.wantErr(e.do("POST", path, map[string]any{"expected_version": 1, "reason": "r"}, opts{actor: "steward-2", noKey: true}), http.StatusBadRequest, "CONTEXT_INVALID")
	e.wantErr(e.do("POST", path, map[string]any{"reason": "r"}, opts{actor: "steward-2"}), http.StatusBadRequest, "CONTEXT_INVALID")
	e.wantErr(e.do("POST", path, map[string]any{"expected_version": 1}, opts{actor: "steward-2"}), http.StatusBadRequest, "CONTEXT_INVALID")
	assert.Equal(t, domain.StatusKnown, e.currency("USD").Status)
	assert.Empty(t, e.store.History())
}

func TestActivate_StaleExpectedVersion_VersionConflict(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	c := e.currency("USD")
	e.wantErr(e.command("activate", c, 41, "steward-2"), http.StatusConflict, "VERSION_CONFLICT")
	assert.Equal(t, domain.StatusKnown, e.currency("USD").Status)
	assert.Empty(t, e.events("CurrencySupportChanged"))

	// And stale after a real change: restrict (v1->v2) then reuse v1.
	require.Equal(t, http.StatusOK, e.command("restrict", c, 1, "steward-2").Code)
	e.wantErr(e.command("activate", c, 1, "steward-2"), http.StatusConflict, "VERSION_CONFLICT")
}

func TestActivate_ImporterCannotActivate_SoDDenied(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	c := e.currency("USD")
	rec := e.command("activate", c, c.Version, "importer-1")
	e.wantErr(rec, http.StatusForbidden, "SOD_DENIED")
	assert.Equal(t, domain.StatusKnown, e.currency("USD").Status)
	assert.Empty(t, e.store.History())
	assert.Empty(t, e.events("CurrencySupportChanged"))

	// A different actor may.
	require.Equal(t, http.StatusOK, e.command("activate", c, c.Version, "steward-2").Code)
}

func TestActivate_SoDFollowsTheLatestChange(t *testing.T) {
	// steward-2 activates; importer-2 later changes the minor unit; the CHANGE's
	// author becomes the SoD-barred actor for any re-activation.
	e := newEnv(t)
	c := e.activated("USD", "840", 2)
	t1 := e.now.Add(time.Hour)
	e.now = t1
	e.do("POST", "/v1/currency-imports", importBody("iso-4217-maintenance", "2026-02", []domain.ImportRow{row("USD", "840", "US Dollar", 3)}, &t1), opts{actor: "importer-2"})
	cur := e.currency("USD")
	require.Equal(t, http.StatusOK, e.command("restrict", cur, cur.Version, "steward-2").Code)
	cur = e.currency("USD")
	e.wantErr(e.command("activate", cur, cur.Version, "importer-2"), http.StatusForbidden, "SOD_DENIED")
	_ = c
}

func TestActivate_AuthzDenied_403(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	c := e.currency("USD")
	e.authz.deny["CURRENCY_ACTIVATE"] = domain.ErrAuthorizationDenied
	e.wantErr(e.command("activate", c, c.Version, "steward-2"), http.StatusForbidden, "FORBIDDEN")
	assert.Equal(t, domain.StatusKnown, e.currency("USD").Status)
	assert.Empty(t, e.store.History())
}

func TestActivate_ReplaySameKeyReturnsOriginalAndEmitsOneEvent(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	c := e.currency("USD")
	body := map[string]any{"expected_version": 1, "reason": "finance approval"}
	first := e.do("POST", "/v1/currencies/"+c.CurrencyID+":activate", body, opts{actor: "steward-2", key: "act-1"})
	second := e.do("POST", "/v1/currencies/"+c.CurrencyID+":activate", body, opts{actor: "steward-2", key: "act-1"})
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	assert.Equal(t, "true", second.Header().Get("Idempotent-Replay"))
	assert.Equal(t, int64(2), decode[domain.Currency](t, second).Version)
	assert.Equal(t, int64(2), e.currency("USD").Version, "no second bump")
	assert.Len(t, e.events("CurrencySupportChanged"), 1)
	assert.Len(t, e.store.History(), 1)
}

func TestLifecycle_IllegalTransitionsViaAPI_InvalidTransition(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	c := e.currency("USD")
	// KNOWN -> RETIRED is not a legal move.
	e.wantErr(e.command("retire", c, 1, "steward-2"), http.StatusConflict, "INVALID_TRANSITION")
	require.Equal(t, http.StatusOK, e.command("restrict", c, 1, "steward-2").Code) // KNOWN -> RESTRICTED (v2)
	// RESTRICTED -> RESTRICTED
	e.wantErr(e.command("restrict", c, 2, "steward-2"), http.StatusConflict, "INVALID_TRANSITION")
	require.Equal(t, http.StatusOK, e.command("activate", c, 2, "steward-2").Code) // RESTRICTED -> SUPPORTED (v3)
	require.Equal(t, http.StatusOK, e.command("retire", c, 3, "steward-2").Code)   // SUPPORTED -> RETIRED (v4)
	// RETIRED is terminal.
	for _, cmd := range []string{"activate", "restrict", "retire"} {
		e.wantErr(e.command(cmd, c, 4, "steward-2"), http.StatusConflict, "INVALID_TRANSITION")
	}
	assert.Equal(t, domain.StatusRetired, e.currency("USD").Status)
}

func TestCommands_UnknownCommandAndUnknownCurrency(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	c := e.currency("USD")
	e.wantErr(e.do("POST", "/v1/currencies/"+c.CurrencyID+":delete", map[string]any{"expected_version": 1, "reason": "x"}), http.StatusNotFound, "NOT_FOUND")
	e.wantErr(e.do("POST", "/v1/currencies/00000000-0000-7000-8000-000000000000:activate", map[string]any{"expected_version": 1, "reason": "x"}), http.StatusNotFound, "NOT_FOUND")
	e.wantErr(e.do("POST", "/v1/currencies/not-a-uuid:activate", map[string]any{"expected_version": 1, "reason": "x"}), http.StatusNotFound, "NOT_FOUND")
}

func TestEvents_AtomicWithStateChange(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	c := e.currency("USD")

	e.store.FailEnqueue = true
	rec := e.command("activate", c, 1, "steward-2")
	e.wantErr(rec, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
	e.store.FailEnqueue = false

	assert.Equal(t, domain.StatusKnown, e.currency("USD").Status, "state change rolled back with the failed outbox write")
	assert.Equal(t, int64(1), e.currency("USD").Version)
	assert.Empty(t, e.store.History())
	// ...and the same key is not poisoned: the retry succeeds.
	require.Equal(t, http.StatusOK, e.command("activate", c, 1, "steward-2").Code)
}

func TestEvents_OneProducedPerCommand(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)}) // CurrencyUpdated
	c := e.currency("USD")
	require.Equal(t, http.StatusOK, e.command("activate", c, 1, "steward-2").Code) // CurrencySupportChanged
	require.Equal(t, http.StatusOK, e.command("restrict", c, 2, "steward-2").Code) // CurrencySupportChanged
	require.Equal(t, http.StatusOK, e.command("activate", c, 3, "steward-3").Code) // CurrencySupportChanged
	require.Equal(t, http.StatusOK, e.command("retire", c, 4, "steward-2").Code)   // CurrencyRetired
	e.importRows("importer-1", "bad", []domain.ImportRow{row("zz", "1", "x", 9)})  // CurrencyImportQuarantined

	assert.Len(t, e.events("CurrencyUpdated"), 1)
	assert.Len(t, e.events("CurrencySupportChanged"), 3)
	retired := e.events("CurrencyRetired")
	require.Len(t, retired, 1)
	assert.Equal(t, int64(5), retired[0].Payload.ObjectVersion)
	assert.Equal(t, c.CurrencyID, retired[0].Payload.ObjectID)
	assert.Len(t, e.events("CurrencyImportQuarantined"), 1)
	assert.Len(t, e.store.Outbox(), 6)
}

// ── 4/5. queries, validate, as_of ────────────────────────────────────────────

func TestValidate_UnknownCurrency_NotSupported(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/v1/currencies:validate?code=QQQ&operation=post", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	res := decode[map[string]any](t, rec)
	assert.Equal(t, false, res["supported"])
	assert.Equal(t, "UNKNOWN_CURRENCY", res["reason"])
	v, present := res["minor_unit"]
	assert.True(t, present)
	assert.Nil(t, v, "never guess a precision for an unknown currency")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestValidate_StatusMatrix(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{
		row("AAA", "101", "Known only", 2), row("BBB", "102", "To support", 3), row("CCC", "103", "To restrict", 0), row("DDD", "104", "To retire", 2)})
	act := func(code, cmd string) {
		c := e.currency(code)
		require.Equal(t, http.StatusOK, e.command(cmd, c, c.Version, "steward-2").Code)
	}
	act("BBB", "activate")
	act("CCC", "restrict")
	act("DDD", "activate")
	act("DDD", "retire")

	type vr struct {
		Supported bool    `json:"supported"`
		MinorUnit *int    `json:"minor_unit"`
		Reason    string  `json:"reason"`
		Status    string  `json:"status"`
		Version   float64 `json:"currency_version"`
	}
	cases := []struct {
		code, op, reason string
		supported        bool
		mu               int
	}{
		{"AAA", "post", "STATUS_KNOWN", false, 2},
		{"BBB", "post", "OK", true, 3},
		{"CCC", "post", "STATUS_RESTRICTED", false, 0},
		{"DDD", "post", "STATUS_RETIRED", false, 2},
		{"DDD", "read", "OK", true, 2}, // retired currency remains usable for history
		{"AAA", "read", "OK", true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.code+"/"+tc.op, func(t *testing.T) {
			rec := e.do("GET", "/v1/currencies:validate?code="+tc.code+"&operation="+tc.op, nil)
			require.Equal(t, http.StatusOK, rec.Code)
			res := decode[vr](t, rec)
			assert.Equal(t, tc.supported, res.Supported)
			assert.Equal(t, tc.reason, res.Reason)
			require.NotNil(t, res.MinorUnit)
			assert.Equal(t, tc.mu, *res.MinorUnit)
			assert.NotZero(t, res.Version, "consumers pin the applied version")
		})
	}
}

func TestValidate_BadRequests(t *testing.T) {
	e := newEnv(t)
	e.wantErr(e.do("GET", "/v1/currencies:validate", nil), http.StatusBadRequest, "CONTEXT_INVALID")
	e.wantErr(e.do("GET", "/v1/currencies:validate?code=USD&operation=teleport", nil), http.StatusUnprocessableEntity, "CONTEXT_INVALID")
}

func TestRetiredCurrency_ReadableForHistory_NotForNewPostings(t *testing.T) {
	e := newEnv(t)
	c := e.activated("USD", "840", 2)
	tActive := e.now.Add(time.Minute)
	e.now = e.now.Add(24 * time.Hour)
	require.Equal(t, http.StatusOK, e.command("retire", c, c.Version, "steward-2").Code)

	rec := e.do("GET", "/v1/currencies/USD", nil)
	require.Equal(t, http.StatusOK, rec.Code, "retired currencies remain readable")
	got := decode[domain.Currency](t, rec)
	assert.Equal(t, domain.StatusRetired, got.Status)
	require.NotNil(t, got.ValidTo)
	require.NotNil(t, got.MinorUnit)
	assert.Equal(t, 2, got.MinorUnit.MinorUnit)

	// Historical read as of while it was live.
	asOf := e.do("GET", "/v1/currencies/USD?as_of="+tActive.Format(time.RFC3339), nil)
	require.Equal(t, http.StatusOK, asOf.Code)

	val := decode[map[string]any](t, e.do("GET", "/v1/currencies:validate?code=USD&operation=post", nil))
	assert.Equal(t, false, val["supported"])
	assert.Equal(t, "STATUS_RETIRED", val["reason"])
	assert.Equal(t, float64(2), val["minor_unit"])

	// And it appears in the retired list, not the supported one.
	sup := decode[struct{ Items []domain.Currency }](t, e.do("GET", "/v1/currencies?status=SUPPORTED", nil))
	assert.Empty(t, sup.Items)
	ret := decode[struct{ Items []domain.Currency }](t, e.do("GET", "/v1/currencies?status=RETIRED", nil))
	assert.Len(t, ret.Items, 1)
}

func TestGetCurrency_UnknownIs404NotFound(t *testing.T) {
	e := newEnv(t)
	e.wantErr(e.do("GET", "/v1/currencies/QQQ", nil), http.StatusNotFound, "NOT_FOUND")
	e.wantErr(e.do("GET", "/v1/currencies/QQQ/minor-unit-versions", nil), http.StatusNotFound, "NOT_FOUND")
	e.wantErr(e.do("GET", "/v1/currencies:resolve-numeric?code=999", nil), http.StatusNotFound, "NOT_FOUND")
	e.wantErr(e.do("GET", "/v1/currencies:resolve-numeric", nil), http.StatusBadRequest, "CONTEXT_INVALID")
	e.wantErr(e.do("GET", "/v1/currencies/USD?as_of=yesterday", nil), http.StatusBadRequest, "CONTEXT_INVALID")
}

func TestGetCurrency_AsOfBeforeItExisted_404(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2)})
	before := e.now.Add(-time.Hour).Format(time.RFC3339)
	e.wantErr(e.do("GET", "/v1/currencies/USD?as_of="+before, nil), http.StatusNotFound, "NOT_FOUND")
}

func TestResolveNumericAndListAndETag(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("USD", "840", "US Dollar", 2), row("JPY", "392", "Yen", 0)})
	r := decode[domain.Currency](t, e.do("GET", "/v1/currencies:resolve-numeric?code=392", nil))
	assert.Equal(t, "JPY", r.AlphaCode)
	assert.Equal(t, 0, r.MinorUnit.MinorUnit)

	all := decode[struct{ Items []domain.Currency }](t, e.do("GET", "/v1/currencies", nil))
	assert.Len(t, all.Items, 2)
	e.wantErr(e.do("GET", "/v1/currencies?status=BOGUS", nil), http.StatusUnprocessableEntity, "CONTEXT_INVALID")
	e.wantErr(e.do("GET", "/v1/currencies?limit=0", nil), http.StatusBadRequest, "CONTEXT_INVALID")

	rec := e.do("GET", "/v1/currencies/USD", nil)
	tag := rec.Header().Get("ETag")
	require.NotEmpty(t, tag)
	req := httptest.NewRequest("GET", "/v1/currencies/USD", nil)
	req.Header.Set("If-None-Match", tag)
	out := httptest.NewRecorder()
	e.router.ServeHTTP(out, req)
	assert.Equal(t, http.StatusNotModified, out.Code)
}

// ── 6. tenant overlay ────────────────────────────────────────────────────────

func (e *testEnv) tenantCmd(tenant, code, cmd string, expected int, actor string) *httptest.ResponseRecorder {
	return e.do("POST", "/v1/tenants/"+tenant+"/currency-support/"+code+":"+cmd,
		map[string]any{"expected_version": expected, "reason": "tenant treasury policy"}, opts{tenant: tenant, actor: actor})
}

func TestTenantOverlay_CannotEnableNonSupported(t *testing.T) {
	e := newEnv(t)
	e.mustImport("importer-1", "2026-01", []domain.ImportRow{row("AAA", "101", "Known", 2), row("BBB", "102", "Restricted", 2)})
	b := e.currency("BBB")
	require.Equal(t, http.StatusOK, e.command("restrict", b, b.Version, "steward-2").Code)

	e.wantErr(e.tenantCmd("tenant-a", "AAA", "enable", 0, "tadmin"), http.StatusUnprocessableEntity, "CONTEXT_INVALID")
	e.wantErr(e.tenantCmd("tenant-a", "BBB", "enable", 0, "tadmin"), http.StatusUnprocessableEntity, "CONTEXT_INVALID")
	e.wantErr(e.tenantCmd("tenant-a", "QQQ", "enable", 0, "tadmin"), http.StatusNotFound, "NOT_FOUND")
	items := decode[struct{ Items []domain.TenantSupport }](t, e.do("GET", "/v1/tenants/tenant-a/currency-support", nil))
	assert.Empty(t, items.Items)
	assert.Empty(t, e.events("CurrencySupportChanged")[1:], "only the restrict event exists; no overlay event")
}

func TestTenantOverlay_RetiredCurrencyCannotBeEnabled(t *testing.T) {
	e := newEnv(t)
	c := e.activated("USD", "840", 2)
	require.Equal(t, http.StatusOK, e.command("retire", c, c.Version, "steward-2").Code)
	e.wantErr(e.tenantCmd("tenant-a", "USD", "enable", 0, "tadmin"), http.StatusConflict, "REFERENCE_RETIRED")
}

func TestTenantOverlay_EnableDisableAndCrossTenantIsolation(t *testing.T) {
	e := newEnv(t)
	e.activated("USD", "840", 2)

	rec := e.tenantCmd("tenant-a", "USD", "enable", 0, "tadmin-a")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	ts := decode[domain.TenantSupport](t, rec)
	assert.True(t, ts.Enabled)
	assert.Equal(t, int64(1), ts.Version)
	assert.Equal(t, "tenant-a", ts.TenantID)

	// Overlay event: TENANT scope, tenant, object, version.
	var tenantEv *outboxEvent
	for _, ev := range e.events("CurrencySupportChanged") {
		ev := ev
		if ev.Payload.Scope == "TENANT" {
			tenantEv = &ev
		}
	}
	require.NotNil(t, tenantEv)
	assert.Equal(t, "tenant-a", tenantEv.TenantID)
	assert.Equal(t, ts.CurrencyID, tenantEv.Payload.ObjectID)
	assert.Equal(t, int64(1), tenantEv.Payload.ObjectVersion)
	require.NotNil(t, tenantEv.Payload.Enabled)
	assert.True(t, *tenantEv.Payload.Enabled)

	// Tenant B cannot see tenant A's overlay.
	listB := decode[struct{ Items []domain.TenantSupport }](t, e.do("GET", "/v1/tenants/tenant-b/currency-support", nil, opts{tenant: "tenant-b"}))
	assert.Empty(t, listB.Items)
	listA := decode[struct{ Items []domain.TenantSupport }](t, e.do("GET", "/v1/tenants/tenant-a/currency-support", nil, opts{tenant: "tenant-a"}))
	require.Len(t, listA.Items, 1)

	// Tenant B cannot read or write tenant A's overlay by path.
	e.wantErr(e.do("GET", "/v1/tenants/tenant-a/currency-support", nil, opts{tenant: "tenant-b"}), http.StatusForbidden, "CONTEXT_INVALID")
	cross := e.do("POST", "/v1/tenants/tenant-a/currency-support/USD:disable",
		map[string]any{"expected_version": 1, "reason": "x"}, opts{tenant: "tenant-b", actor: "mallory"})
	e.wantErr(cross, http.StatusForbidden, "CONTEXT_INVALID")
	assert.True(t, decode[struct{ Items []domain.TenantSupport }](t, e.do("GET", "/v1/tenants/tenant-a/currency-support", nil, opts{tenant: "tenant-a"})).Items[0].Enabled)

	// Tenant-scoped validation: A enabled it, B did not.
	va := decode[map[string]any](t, e.do("GET", "/v1/currencies:validate?code=USD&operation=post&tenant_scoped=true", nil, opts{tenant: "tenant-a"}))
	assert.Equal(t, true, va["supported"])
	vb := decode[map[string]any](t, e.do("GET", "/v1/currencies:validate?code=USD&operation=post&tenant_scoped=true", nil, opts{tenant: "tenant-b"}))
	assert.Equal(t, false, vb["supported"])
	assert.Equal(t, "NOT_ENABLED_FOR_TENANT", vb["reason"])
	// Global validation is unaffected by overlays.
	vg := decode[map[string]any](t, e.do("GET", "/v1/currencies:validate?code=USD&operation=post", nil, opts{tenant: "tenant-b"}))
	assert.Equal(t, true, vg["supported"])

	// Tenant B enabling is independent and starts from version 0.
	require.Equal(t, http.StatusOK, e.tenantCmd("tenant-b", "USD", "enable", 0, "tadmin-b").Code)

	// Concurrency/state rules on the overlay.
	e.wantErr(e.tenantCmd("tenant-a", "USD", "enable", 1, "tadmin-a"), http.StatusConflict, "INVALID_TRANSITION") // already enabled
	e.wantErr(e.tenantCmd("tenant-a", "USD", "disable", 0, "tadmin-a"), http.StatusConflict, "VERSION_CONFLICT")  // stale
	dis := e.tenantCmd("tenant-a", "USD", "disable", 1, "tadmin-a")
	require.Equal(t, http.StatusOK, dis.Code, dis.Body.String())
	d := decode[domain.TenantSupport](t, dis)
	assert.False(t, d.Enabled)
	assert.Equal(t, int64(2), d.Version)
	e.wantErr(e.tenantCmd("tenant-a", "USD", "disable", 2, "tadmin-a"), http.StatusConflict, "INVALID_TRANSITION") // already disabled
}

func TestTenantOverlay_AuthzAndIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	e.activated("USD", "840", 2)
	e.authz.deny["CURRENCY_TENANT_ENABLE"] = domain.ErrAuthorizationDenied
	e.wantErr(e.tenantCmd("tenant-a", "USD", "enable", 0, "tadmin"), http.StatusForbidden, "FORBIDDEN")
	e.authz.deny = map[string]error{}
	e.wantErr(e.do("POST", "/v1/tenants/tenant-a/currency-support/USD:enable",
		map[string]any{"expected_version": 0, "reason": "r"}, opts{noKey: true}), http.StatusBadRequest, "CONTEXT_INVALID")
}

// ── context ──────────────────────────────────────────────────────────────────

func TestCommands_MissingTenantOrActor_401(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest("POST", "/v1/currency-imports", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Idempotency-Key", "k")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	e.wantErr(rec, http.StatusUnauthorized, "CONTEXT_INVALID")

	req = httptest.NewRequest("POST", "/v1/currency-imports", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Idempotency-Key", "k")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	rec = httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	e.wantErr(rec, http.StatusUnauthorized, "CONTEXT_INVALID")
}

func TestImport_UnknownFieldRejected(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/v1/currency-imports", `{"source_name":"s","source_version":"1","manifest_hash":"x","reason":"r","rows":[],"surprise":1}`)
	e.wantErr(rec, http.StatusBadRequest, "CONTEXT_INVALID")
}
