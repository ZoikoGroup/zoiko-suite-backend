//go:build integration

package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/eventing/adapter"
	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/middleware"
	"zoiko.io/payment-initiation-adapter-svc/internal/providers"
	"zoiko.io/payment-initiation-adapter-svc/internal/store"
)

// appStore connects as an UNPRIVILEGED role, so row-level security and the sweeper flag are really enforced
// (the superuser the harness uses bypasses them).
var (
	appOnce  sync.Once
	appPool  *pgxpool.Pool
	appStore *store.PgStore
)

func app(t *testing.T) *store.PgStore {
	t.Helper()
	appOnce.Do(func() {
		ctx := context.Background()
		_, err := testPool.Exec(ctx, `
			DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'zoiko_app') THEN
			  CREATE ROLE zoiko_app LOGIN PASSWORD 'app' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS; END IF; END $$;
			GRANT USAGE ON SCHEMA public TO zoiko_app;
			GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO zoiko_app;
			GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO zoiko_app;`)
		require.NoError(t, err)
		cfg, err := pgxpool.ParseConfig(testDSN)
		require.NoError(t, err)
		cfg.ConnConfig.User, cfg.ConnConfig.Password = "zoiko_app", "app"
		appPool, err = pgxpool.NewWithConfig(ctx, cfg)
		require.NoError(t, err)
		appStore = store.NewPgStore(appPool, zap.NewNop(), store.WithEventRegion("uk"))
	})
	require.NotNil(t, appStore)
	return appStore
}

func tctx(tenant string) context.Context { return middleware.WithTenant(context.Background(), tenant) }

func newBound(t *testing.T, st *store.PgStore, tenant string) *domain.PaymentInitiationAttempt {
	t.Helper()
	key := "k-" + uuid.NewString()
	a, err := st.PrepareAttempt(tctx(tenant), tenant, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.NewString(), SourceReference: "src-" + key, PayerAccountRef: "DE00AA", PayeeRef: "FR00BB", Amount: 10.50,
		Currency: "EUR", ExecutionDate: time.Now().Add(24 * time.Hour), PayerAccountVerified: true, IdempotencyKey: key,
		ProviderKey: "simbank", Rail: "SEPA_CT@1", ConnectorID: uuid.NewString(),
	}, "maker", "corr-"+key)
	require.NoError(t, err)
	return a
}

func entry(op, disp, class string) providers.TransportEntry {
	return providers.TransportEntry{ProviderKey: "simbank", ProfileVersion: 1, Operation: op, Disposition: disp, Class: class,
		HTTPStatus: 201, RequestHash: "rh", ResponseHash: "sh", ProviderRef: "PRV-1", DurationMS: 12}
}

func spec() adapter.Spec {
	return adapter.Spec{
		Kind: "BANK", RequiredScope: "payments.initiate", Rails: []string{"SEPA_CT@1"}, Currencies: []string{"EUR"},
		Submit: adapter.Request{Path: "/v1/payments", AmountField: "amount.value", Body: []adapter.Field{
			{Name: "amount.value", From: "canonical", Source: "amount"}, {Name: "creditor", From: "canonical", Source: "payee"}}},
		Response: adapter.Response{RefPath: "id", StatusPath: "status", StatusMap: map[string]string{"ACCEPTED": "ACKNOWLEDGED"}},
		Auth:     adapter.Auth{Type: "bearer"}, TimeoutSeconds: 5,
	}
}

func TestDispatch_PrepareBindsTheProviderAndTheExactAmount(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	a := newBound(t, st, tenant)
	d, err := st.LoadDispatch(tctx(tenant), a.AttemptID)
	require.NoError(t, err)
	assert.Equal(t, "simbank", d.ProviderKey)
	assert.Equal(t, "SEPA_CT@1", d.Rail)
	assert.Equal(t, "10.50", d.AmountText, "the amount is read exactly, never through a float")
	assert.Equal(t, domain.StatusPrepared, d.Status)
	assert.Equal(t, 0, d.SendCount)

	// an unresolved attempt blocks another for the same source reference, including one that is in flight
	_, err = st.BeginSend(tctx(tenant), a.AttemptID, domain.StatusPrepared, 1, "h", "op")
	require.NoError(t, err)
	_, err = st.PrepareAttempt(tctx(tenant), tenant, domain.PrepareAttemptRequest{
		LegalEntityID: a.LegalEntityID, SourceReference: a.SourceReference, PayerAccountRef: "x", PayeeRef: "y", Amount: 1, Currency: "EUR",
		ExecutionDate: time.Now(), PayerAccountVerified: true, IdempotencyKey: "other-" + uuid.NewString(),
	}, "maker", "c")
	assert.ErrorIs(t, err, domain.ErrUnresolvedAttemptExists)
}

func TestDispatch_BeginSendIsACompareAndSet(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	a := newBound(t, st, tenant)
	var won, lost, other atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.BeginSend(tctx(tenant), a.AttemptID, domain.StatusPrepared, 1, "hash-1", "op")
			switch {
			case err == nil:
				won.Add(1)
			case errors.Is(err, providers.ErrInFlight):
				lost.Add(1)
			default:
				other.Add(1)
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, won.Load(), "exactly one caller may put the request in flight")
	assert.EqualValues(t, 11, lost.Load())
	d, _ := st.LoadDispatch(tctx(tenant), a.AttemptID)
	assert.Equal(t, domain.StatusSubmitting, d.Status)
	assert.Equal(t, 1, d.SendCount)
	assert.NotNil(t, d.InFlightSince)
	assert.NotNil(t, d.ProfileVersion)
	assert.Equal(t, "hash-1", d.SentPayloadHash)
}

func TestDispatch_FinishSendOutcomesAreAtomicWithEvidenceAndEvents(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	ctx := tctx(tenant)

	// acknowledged → SUBMITTED, with transport evidence and a submitted event, in one transaction
	a := newBound(t, st, tenant)
	_, err := st.BeginSend(ctx, a.AttemptID, domain.StatusPrepared, 1, "h", "op")
	require.NoError(t, err)
	got, err := st.FinishSend(ctx, a.AttemptID, providers.SendOutcome{Kind: providers.OutSubmitted, From: domain.StatusSubmitting,
		ProviderRef: "PRV-1", Provider: "ACCEPTED", Transport: entry("SUBMIT", "ANSWERED", "ACKNOWLEDGED")}, "op", "corr")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSubmitted, got.Status)
	d, _ := st.LoadDispatch(ctx, a.AttemptID)
	assert.Equal(t, "PRV-1", d.ProviderRequestID)
	assert.Equal(t, "ACCEPTED", d.ProviderStatus)
	assert.Nil(t, d.InFlightSince)
	log, err := st.ListTransport(ctx, a.AttemptID)
	require.NoError(t, err)
	require.Len(t, log, 1)
	assert.Equal(t, "ANSWERED", log[0].Disposition)
	assert.Equal(t, "op", log[0].Actor)
	assert.Equal(t, 1, outboxCount(t, tenant, a.AttemptID, "payment-initiation-attempt.submitted"))

	// a second outcome for the same call is refused: the attempt is no longer in the state it was for
	_, err = st.FinishSend(ctx, a.AttemptID, providers.SendOutcome{Kind: providers.OutUnknown, From: domain.StatusSubmitting,
		Transport: entry("SUBMIT", "UNKNOWN", "")}, "op", "corr")
	assert.ErrorIs(t, err, providers.ErrInFlight)
	d, _ = st.LoadDispatch(ctx, a.AttemptID)
	assert.Equal(t, domain.StatusSubmitted, d.Status, "SUBMITTED is terminal here")

	// unknown → PENDING_UNKNOWN + ambiguous event
	b := newBound(t, st, tenant)
	_, _ = st.BeginSend(ctx, b.AttemptID, domain.StatusPrepared, 1, "h", "op")
	_, err = st.FinishSend(ctx, b.AttemptID, providers.SendOutcome{Kind: providers.OutUnknown, From: domain.StatusSubmitting,
		Failure: "no_answer_after_send", Transport: entry("SUBMIT", "UNKNOWN", "")}, "op", "corr")
	require.NoError(t, err)
	d, _ = st.LoadDispatch(ctx, b.AttemptID)
	assert.Equal(t, domain.StatusPendingUnknown, d.Status)
	assert.Equal(t, "no_answer_after_send", d.LastFailure)
	assert.Equal(t, 1, outboxCount(t, tenant, b.AttemptID, "payment-initiation-attempt.ambiguous"))

	// release with a not-before: back to where it was, and not sendable until then
	c := newBound(t, st, tenant)
	_, _ = st.BeginSend(ctx, c.AttemptID, domain.StatusPrepared, 1, "h", "op")
	nb := time.Now().Add(time.Hour)
	_, err = st.FinishSend(ctx, c.AttemptID, providers.SendOutcome{Kind: providers.OutRelease, From: domain.StatusSubmitting, ReleaseTo: domain.StatusPrepared,
		NotBefore: &nb, Failure: "rate_limited", Transport: entry("SUBMIT", "ANSWERED", "RATE_LIMITED")}, "op", "corr")
	require.NoError(t, err)
	_, err = st.BeginSend(ctx, c.AttemptID, domain.StatusPrepared, 1, "h", "op")
	assert.ErrorIs(t, err, providers.ErrNotBefore)
	_, err = testPool.Exec(context.Background(), `UPDATE payment_initiation_attempts SET not_before = now() - interval '1 second' WHERE attempt_id = $1`, c.AttemptID)
	require.NoError(t, err)
	_, err = st.BeginSend(ctx, c.AttemptID, domain.StatusPrepared, 1, "h", "op")
	assert.NoError(t, err, "after the wait it can be sent")

	// rejected → REJECTED_BEFORE_SUBMISSION with the reason
	r := newBound(t, st, tenant)
	_, _ = st.BeginSend(ctx, r.AttemptID, domain.StatusPrepared, 1, "h", "op")
	rj, err := st.FinishSend(ctx, r.AttemptID, providers.SendOutcome{Kind: providers.OutRejected, From: domain.StatusSubmitting, Reason: "AC04",
		Provider: "RJCT", Transport: entry("SUBMIT", "ANSWERED", "REJECTED")}, "op", "corr")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejectedBeforeSubmission, rj.Status)
	assert.Equal(t, "AC04", rj.RejectionReason)
}

func TestDispatch_RetryMustBeSentAsTheOriginal(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	ctx := tctx(tenant)
	a := newBound(t, st, tenant)
	_, err := st.BeginSend(ctx, a.AttemptID, domain.StatusPrepared, 1, "hash-A", "op")
	require.NoError(t, err)
	_, err = st.FinishSend(ctx, a.AttemptID, providers.SendOutcome{Kind: providers.OutUnknown, From: domain.StatusSubmitting, Transport: entry("SUBMIT", "UNKNOWN", "")}, "op", "c")
	require.NoError(t, err)
	// a different payload, or a different profile version, cannot be sent for this attempt
	_, err = st.BeginSend(ctx, a.AttemptID, domain.StatusPendingUnknown, 1, "hash-B", "op")
	assert.ErrorIs(t, err, providers.ErrPayloadChanged)
	_, err = st.BeginSend(ctx, a.AttemptID, domain.StatusPendingUnknown, 2, "hash-A", "op")
	assert.ErrorIs(t, err, providers.ErrPayloadChanged)
	d, err := st.BeginSend(ctx, a.AttemptID, domain.StatusPendingUnknown, 1, "hash-A", "op")
	require.NoError(t, err)
	assert.Equal(t, 2, d.SendCount)
}

func TestDispatch_TriggersPinTheContextAndGuardInFlight(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	ctx := tctx(tenant)
	a := newBound(t, st, tenant)
	_, err := st.BeginSend(ctx, a.AttemptID, domain.StatusPrepared, 1, "hash-A", "op")
	require.NoError(t, err)
	_, err = st.FinishSend(ctx, a.AttemptID, providers.SendOutcome{Kind: providers.OutUnknown, From: domain.StatusSubmitting, Transport: entry("SUBMIT", "UNKNOWN", "")}, "op", "c")
	require.NoError(t, err)

	su := func(sql string, args ...any) error {
		_, err := testPool.Exec(context.Background(), sql, args...)
		return err
	}
	// the superuser bypasses RLS, so only the triggers stand in the way
	for name, sql := range map[string]string{
		"provider":        `UPDATE payment_initiation_attempts SET provider_key = 'other' WHERE attempt_id = $1`,
		"rail":            `UPDATE payment_initiation_attempts SET rail = 'FPS@1' WHERE attempt_id = $1`,
		"connector":       `UPDATE payment_initiation_attempts SET connector_id = 'x' WHERE attempt_id = $1`,
		"profile version": `UPDATE payment_initiation_attempts SET profile_version = 9 WHERE attempt_id = $1`,
		"payload hash":    `UPDATE payment_initiation_attempts SET sent_payload_hash = 'forged' WHERE attempt_id = $1`,
		"send count down": `UPDATE payment_initiation_attempts SET send_count = 0 WHERE attempt_id = $1`,
		"amount":          `UPDATE payment_initiation_attempts SET amount = 10.51 WHERE attempt_id = $1`,
	} {
		assert.Error(t, su(sql, a.AttemptID), "%s must be immutable once pinned", name)
	}

	// an in-flight request is resolved by the provider's answer, never by an operator action
	b := newBound(t, st, tenant)
	_, _ = st.BeginSend(ctx, b.AttemptID, domain.StatusPrepared, 1, "h", "op")
	assert.Error(t, su(`UPDATE payment_initiation_attempts SET status = 'CANCELLED', in_flight_since = NULL WHERE attempt_id = $1`, b.AttemptID))
	assert.Error(t, su(`UPDATE payment_initiation_attempts SET status = 'QUARANTINED', in_flight_since = NULL WHERE attempt_id = $1`, b.AttemptID))
	_, err = st.CancelAttempt(ctx, b.AttemptID, "op")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition, "cancel through the store is also refused while in flight")
	_, err = st.QuarantineAttempt(ctx, b.AttemptID, domain.QuarantineRequest{Reason: "x"}, "op", "corr-q")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition)

	// SUBMITTING and its timestamp are one fact
	c := newBound(t, st, tenant)
	assert.Error(t, su(`UPDATE payment_initiation_attempts SET status = 'SUBMITTING' WHERE attempt_id = $1`, c.AttemptID), "no start time")
	assert.Error(t, su(`UPDATE payment_initiation_attempts SET in_flight_since = now() WHERE attempt_id = $1`, c.AttemptID), "a time without the state")
	// and sending can start only from a state where it is allowed
	_, _ = st.BeginSend(ctx, c.AttemptID, domain.StatusPrepared, 1, "h", "op")
	_, _ = st.FinishSend(ctx, c.AttemptID, providers.SendOutcome{Kind: providers.OutSubmitted, From: domain.StatusSubmitting, Transport: entry("SUBMIT", "ANSWERED", "ACKNOWLEDGED")}, "op", "c")
	assert.Error(t, su(`UPDATE payment_initiation_attempts SET status = 'SUBMITTING', in_flight_since = now() WHERE attempt_id = $1`, c.AttemptID), "terminal")

	// the transport log is append-only
	var logID string
	require.NoError(t, testPool.QueryRow(context.Background(), `SELECT log_id::text FROM provider_transport_log WHERE attempt_id = $1 LIMIT 1`, a.AttemptID).Scan(&logID))
	assert.Error(t, su(`UPDATE provider_transport_log SET http_status = 200 WHERE log_id = $1`, logID))
	assert.Error(t, su(`DELETE FROM provider_transport_log WHERE log_id = $1`, logID))
}

func TestDispatch_SweepReleasesStaleInFlightPerTenantUnderRLS(t *testing.T) {
	st := app(t)
	t1, t2 := uuid.NewString(), uuid.NewString()
	a1, a2 := newBound(t, st, t1), newBound(t, st, t2)
	fresh := newBound(t, st, t1)
	for _, p := range []struct {
		tenant string
		id     string
	}{{t1, a1.AttemptID}, {t2, a2.AttemptID}, {t1, fresh.AttemptID}} {
		_, err := st.BeginSend(tctx(p.tenant), p.id, domain.StatusPrepared, 1, "h", "op")
		require.NoError(t, err)
	}
	// two of them died long ago
	_, err := testPool.Exec(context.Background(), `UPDATE payment_initiation_attempts SET in_flight_since = now() - interval '1 hour' WHERE attempt_id = ANY($1)`,
		[]string{a1.AttemptID, a2.AttemptID})
	require.NoError(t, err)

	n, err := st.SweepStale(context.Background(), time.Now().Add(-5*time.Minute), "sweeper")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "only the stale ones, across tenants")
	for _, p := range []struct {
		tenant, id string
		want       domain.AttemptStatus
	}{{t1, a1.AttemptID, domain.StatusPendingUnknown}, {t2, a2.AttemptID, domain.StatusPendingUnknown}, {t1, fresh.AttemptID, domain.StatusSubmitting}} {
		d, err := st.LoadDispatch(tctx(p.tenant), p.id)
		require.NoError(t, err)
		assert.Equal(t, p.want, d.Status)
	}
	assert.Equal(t, 1, outboxCount(t, t1, a1.AttemptID, "payment-initiation-attempt.ambiguous"))
	// a second sweep finds nothing
	n, _ = st.SweepStale(context.Background(), time.Now().Add(-5*time.Minute), "sweeper")
	assert.Equal(t, 0, n)
}

func TestDispatch_TenantsCannotSeeOrTouchEachOther(t *testing.T) {
	st := app(t)
	owner, other := uuid.NewString(), uuid.NewString()
	a := newBound(t, st, owner)
	_, err := st.BeginSend(tctx(owner), a.AttemptID, domain.StatusPrepared, 1, "h", "op")
	require.NoError(t, err)
	_, _ = st.FinishSend(tctx(owner), a.AttemptID, providers.SendOutcome{Kind: providers.OutSubmitted, From: domain.StatusSubmitting, Transport: entry("SUBMIT", "ANSWERED", "ACKNOWLEDGED")}, "op", "c")

	_, err = st.LoadDispatch(tctx(other), a.AttemptID)
	assert.ErrorIs(t, err, domain.ErrAttemptNotFound)
	log, _ := st.ListTransport(tctx(other), a.AttemptID)
	assert.Empty(t, log)
	// no tenant in context sees nothing either
	_, err = st.LoadDispatch(context.Background(), a.AttemptID)
	assert.ErrorIs(t, err, domain.ErrAttemptNotFound)

	// the other tenant cannot move it: not begin, not finish
	b := newBound(t, st, owner)
	_, err = st.BeginSend(tctx(other), b.AttemptID, domain.StatusPrepared, 1, "h", "op")
	assert.Error(t, err)
	d, _ := st.LoadDispatch(tctx(owner), b.AttemptID)
	assert.Equal(t, domain.StatusPrepared, d.Status)
	_, _ = st.BeginSend(tctx(owner), b.AttemptID, domain.StatusPrepared, 1, "h", "op")
	_, err = st.FinishSend(tctx(other), b.AttemptID, providers.SendOutcome{Kind: providers.OutSubmitted, From: domain.StatusSubmitting, Transport: entry("SUBMIT", "ANSWERED", "ACKNOWLEDGED")}, "op", "c")
	assert.Error(t, err)
	d, _ = st.LoadDispatch(tctx(owner), b.AttemptID)
	assert.Equal(t, domain.StatusSubmitting, d.Status, "a foreign tenant's outcome changed nothing")

	// the transport log can only be written for one's own tenant (RLS WITH CHECK), even by raw SQL
	_, err = appPool.Exec(tctx(other), `INSERT INTO provider_transport_log (tenant_id, attempt_id, provider_key, profile_version, operation, disposition)
		VALUES ($1, $2, 'simbank', 1, 'SUBMIT', 'ANSWERED')`, owner, a.AttemptID)
	assert.Error(t, err, "an unscoped session cannot write another tenant's evidence")

	// health is per tenant: another tenant's failures do not pause this one
	calls, err := st.RecentCalls(tctx(other), "simbank", time.Now().Add(-time.Hour), 5)
	require.NoError(t, err)
	assert.Empty(t, calls)
	calls, _ = st.RecentCalls(tctx(owner), "simbank", time.Now().Add(-time.Hour), 5)
	assert.NotEmpty(t, calls)
}

func TestProfiles_MakerCheckerImmutabilityAndPlatformScope(t *testing.T) {
	st := app(t)
	ctx := context.Background()
	key := "bank-" + uuid.NewString()[:8]

	p1, err := st.PutProfile(ctx, key, spec(), "author")
	require.NoError(t, err)
	assert.Equal(t, 1, p1.Version)
	assert.Equal(t, providers.ProfileDraft, p1.Status)
	assert.Len(t, p1.SpecHash, 64)

	// a draft is not usable as "latest certified"
	_, err = st.GetProfile(ctx, key, 0)
	assert.ErrorIs(t, err, providers.ErrProfileNotFound)

	// the author cannot certify their own profile
	_, err = st.CertifyProfile(ctx, key, 1, "author")
	assert.ErrorIs(t, err, providers.ErrProfileSelfCertify)
	c, err := st.CertifyProfile(ctx, key, 1, "reviewer")
	require.NoError(t, err)
	assert.Equal(t, providers.ProfileCertified, c.Status)
	assert.Equal(t, "reviewer", c.CertifiedBy)
	_, err = st.CertifyProfile(ctx, key, 1, "reviewer2")
	assert.ErrorIs(t, err, providers.ErrProfileState, "already certified")

	got, err := st.GetProfile(ctx, key, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, got.Version)
	assert.Equal(t, "payments.initiate", got.Spec.RequiredScope)

	// a certified profile is immutable: the spec, its hash, its identity; and it is never deleted
	for _, sql := range []string{
		`UPDATE provider_profiles SET spec = '{}'::jsonb WHERE provider_key = $1`,
		`UPDATE provider_profiles SET spec_hash = repeat('a', 64) WHERE provider_key = $1`,
		`UPDATE provider_profiles SET kind = 'TAX' WHERE provider_key = $1`,
		`UPDATE provider_profiles SET created_by = 'someone' WHERE provider_key = $1`,
		`DELETE FROM provider_profiles WHERE provider_key = $1`,
	} {
		_, err := testPool.Exec(ctx, sql, key)
		assert.Error(t, err, sql)
	}

	// a change is a new version, which retires nothing until certified
	p2, err := st.PutProfile(ctx, key, spec(), "author")
	require.NoError(t, err)
	assert.Equal(t, 2, p2.Version)
	got, _ = st.GetProfile(ctx, key, 0)
	assert.Equal(t, 1, got.Version, "latest CERTIFIED stays v1 until v2 is certified")
	_, err = st.CertifyProfile(ctx, key, 2, "reviewer")
	require.NoError(t, err)
	got, _ = st.GetProfile(ctx, key, 0)
	assert.Equal(t, 2, got.Version)

	// lifecycle: deprecate stays readable by version; withdraw is final; illegal moves are refused
	_, err = st.SetProfileStatus(ctx, key, 1, providers.ProfileDeprecated)
	require.NoError(t, err)
	v1, err := st.GetProfile(ctx, key, 1)
	require.NoError(t, err)
	assert.Equal(t, providers.ProfileDeprecated, v1.Status)
	_, err = st.SetProfileStatus(ctx, key, 1, providers.ProfileWithdrawn)
	require.NoError(t, err)
	_, err = st.SetProfileStatus(ctx, key, 1, providers.ProfileCertified)
	assert.ErrorIs(t, err, providers.ErrProfileState)
	_, err = st.SetProfileStatus(ctx, key, 99, providers.ProfileWithdrawn)
	assert.ErrorIs(t, err, providers.ErrProfileNotFound)

	list, err := st.ListProfiles(ctx, key)
	require.NoError(t, err)
	assert.Len(t, list, 2)

	// platform data: any tenant can read, none can write
	tenantCtx := tctx(uuid.NewString())
	rows, err := appPool.Query(tenantCtx, `SELECT 1 FROM provider_profiles WHERE provider_key = $1`, key)
	require.NoError(t, err)
	rows.Close()
	tx, err := appPool.Begin(context.Background())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, uuid.NewString())
	require.NoError(t, err)
	_, err = tx.Exec(context.Background(), `INSERT INTO provider_profiles (provider_key, version, kind, spec, spec_hash, created_by)
		VALUES ('rogue-bank', 1, 'BANK', '{}'::jsonb, repeat('a', 64), 'x')`)
	assert.Error(t, err, "a tenant cannot publish platform profiles")
}

func TestOutboxEventsOfTheSendPathCarryTheTenant(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	a := newBound(t, st, tenant)
	_, _ = st.BeginSend(tctx(tenant), a.AttemptID, domain.StatusPrepared, 1, "h", "op")
	_, err := st.FinishSend(tctx(tenant), a.AttemptID, providers.SendOutcome{Kind: providers.OutSubmitted, From: domain.StatusSubmitting, ProviderRef: "P",
		Transport: entry("SUBMIT", "ANSWERED", "ACKNOWLEDGED")}, "op", "corr-9")
	require.NoError(t, err)
	var n int
	require.NoError(t, testPool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1 AND aggregate_id = $2`, tenant, a.AttemptID).Scan(&n))
	assert.Equal(t, 2, n, "prepared + submitted")
}

func outboxCount(t *testing.T, tenant, aggregate, suffix string) int {
	t.Helper()
	var n int
	require.NoError(t, testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type LIKE $3`, tenant, aggregate, "%"+suffix).Scan(&n))
	return n
}
