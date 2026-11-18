//go:build integration

// Unit of work (Governance audit J-11): a request's changes and the outbox rows
// announcing them commit together or not at all, against a real Postgres.
package registryit_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// outboxEnv is the production wiring: the real outbox publisher, the unit of
// work around every write.
func outboxEnv() *env {
	pub := events.NewOutboxPublisher(zap.NewNop(), st, "zoiko.jurisdiction.events.test")
	auth := &recordingAuthz{}
	r := chi.NewRouter()
	r.Use(handler.UnitOfWork(st, zap.NewNop()))
	h := handler.New(st, auth, pub, "platform-scope", zap.NewNop()).WithRegistry(st, pub).WithAggregateVersions(st)
	handler.RegisterRoutes(r, h)
	return &env{srv: r, auth: auth}
}

func countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, sql, args...).Scan(&n))
	return n
}

func TestJ11_RegistryEventCommitsWithTheWrite(t *testing.T) {
	e := outboxEnv()
	code := "J11A" + strings.ToUpper(uuid.NewString()[:5])
	status, body := e.do(http.MethodPost, "/v1/admin/regimes", "author",
		map[string]any{"regime_code": code, "regime_name": "Unit of work, committed"})
	require.Equal(t, http.StatusCreated, status, "%v", body)
	regimeID, _ := body["regime_id"].(string)
	require.NotEmpty(t, regimeID)

	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM regulatory_regimes WHERE regime_code=$1`, code))
	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM event_outbox
		WHERE event_type='regulatory-regime.created' AND partition_key=$1`, regimeID),
		"the event is in the outbox, committed with the regime")
}

// An outbox write that fails (here a trigger refuses it) must take the change
// with it: no regime without its regulatory-regime.created event.
func TestJ11_OutboxFailureRollsTheWriteBack(t *testing.T) {
	_, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION j11_refuse_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.event_type = 'regulatory-regime.created' AND NEW.event_payload::text LIKE '%J11B%' THEN
				RAISE EXCEPTION 'j11 fault injection';
			END IF;
			RETURN NEW;
		END $$;
		DROP TRIGGER IF EXISTS j11_refuse_outbox ON event_outbox;
		CREATE TRIGGER j11_refuse_outbox BEFORE INSERT ON event_outbox FOR EACH ROW EXECUTE FUNCTION j11_refuse_outbox();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS j11_refuse_outbox ON event_outbox; DROP FUNCTION IF EXISTS j11_refuse_outbox();`)
	})

	e := outboxEnv()
	code := "J11B" + strings.ToUpper(uuid.NewString()[:5])
	req := map[string]any{"regime_code": code, "regime_name": "Unit of work, refused event"}
	status, body := e.do(http.MethodPost, "/v1/admin/regimes", "author", req)
	require.Equal(t, http.StatusServiceUnavailable, status, "%v", body)
	require.Equal(t, "event_not_recorded", body["code"])
	require.Equal(t, 0, countRows(t, `SELECT count(*) FROM regulatory_regimes WHERE regime_code=$1`, code),
		"the regime was rolled back with its event")

	// The cause is fixed; the same request now commits both.
	_, err = pool.Exec(ctx, `DROP TRIGGER j11_refuse_outbox ON event_outbox`)
	require.NoError(t, err)
	status, body = e.do(http.MethodPost, "/v1/admin/regimes", "author", req)
	require.Equal(t, http.StatusCreated, status, "%v", body)
	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM regulatory_regimes WHERE regime_code=$1`, code))
	require.Equal(t, 1, countRows(t, `SELECT count(*) FROM event_outbox
		WHERE event_type='regulatory-regime.created' AND partition_key=$1`, body["regime_id"]))
}

// Inside a unit a failing statement is confined to its savepoint, as it was on
// the pool: the request goes on, and what it wrote before and after commits.
func TestUnit_AFailedStatementDoesNotPoisonTheRequest(t *testing.T) {
	uctx, unit := st.BeginUnit(ctx, "")
	a, created, err := st.CreateRegime(uctx, domain.CreateRegimeParams{RegimeCode: "UOWA" + strings.ToUpper(uuid.NewString()[:5]), RegimeName: "a", CreatedBy: "t"})
	require.NoError(t, err)
	require.True(t, created)

	// A malformed id fails in Postgres (22P02) and is answered as not found.
	_, err = st.GetSource(uctx, "not-a-uuid")
	require.Error(t, err)
	// The same code again: the idempotent create reads the existing row back.
	again, created, err := st.CreateRegime(uctx, domain.CreateRegimeParams{RegimeCode: a.RegimeCode, RegimeName: "a", CreatedBy: "t"})
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, a.RegimeID, again.RegimeID)

	b, _, err := st.CreateRegime(uctx, domain.CreateRegimeParams{RegimeCode: "UOWB" + strings.ToUpper(uuid.NewString()[:5]), RegimeName: "b", CreatedBy: "t"})
	require.NoError(t, err)
	// Nothing is visible outside the unit before it commits.
	require.Equal(t, 0, countRows(t, `SELECT count(*) FROM regulatory_regimes WHERE regime_id IN ($1::uuid,$2::uuid)`, a.RegimeID, b.RegimeID))
	require.NoError(t, unit.Finish(uctx))
	require.Equal(t, 2, countRows(t, `SELECT count(*) FROM regulatory_regimes WHERE regime_id IN ($1::uuid,$2::uuid)`, a.RegimeID, b.RegimeID))
}

func TestUnit_AbortRollsBackAndCarriesTheTenant(t *testing.T) {
	tenant := uuid.NewString()
	uctx, unit := st.BeginUnit(ctx, tenant)
	var got string
	require.NoError(t, st.DB(uctx).QueryRow(uctx, `SELECT current_setting('app.tenant_id', true)`).Scan(&got))
	require.Equal(t, tenant, got, "the unit's transaction carries the caller's tenant for RLS")

	x, _, err := st.CreateRegime(uctx, domain.CreateRegimeParams{RegimeCode: "UOWC" + strings.ToUpper(uuid.NewString()[:5]), RegimeName: "c", CreatedBy: "t"})
	require.NoError(t, err)
	store.AbortUnit(uctx, errors.New("test"))
	require.ErrorIs(t, unit.Finish(uctx), store.ErrUnitAborted)
	require.Equal(t, 0, countRows(t, `SELECT count(*) FROM regulatory_regimes WHERE regime_id=$1`, x.RegimeID))
}
