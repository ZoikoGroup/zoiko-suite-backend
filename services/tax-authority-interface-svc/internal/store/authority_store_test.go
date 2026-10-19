package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/eventing/adapter"
	"zoiko.io/tax-authority-interface-svc/internal/authority"
	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
	"zoiko.io/tax-authority-interface-svc/internal/store"
)

type fixture struct {
	t     *testing.T
	admin *pgxpool.Pool
	app   *pgxpool.Pool
	st    *store.PgStore
}

func newFixture(t *testing.T) *fixture {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	return &fixture{t: t, admin: admin, app: app, st: store.NewPgStore(app).WithEventRegion("uk")}
}

func tc(tenant string) context.Context { return middleware.WithTenant(context.Background(), tenant) }

// iface creates an authority-bound interface for a tenant.
func (f *fixture) iface(tenant string, cert *time.Time) string {
	f.t.Helper()
	id := uuid.NewString()
	_, err := f.admin.Exec(context.Background(), `INSERT INTO tax_interfaces (interface_id, tenant_id, legal_entity_id, jurisdiction, authority_name, protocol,
		provider_key, connector_id, certificate_ref, certificate_not_after, escalate_before_hours) VALUES ($1, $2, $3, 'GB', 'HMRC', 'REST', 'taxauth', $4, 'cert-1', $5, 24)`,
		id, tenant, uuid.NewString(), uuid.NewString(), cert)
	require.NoError(f.t, err)
	return id
}

func (f *fixture) filing(tenant, iface string) *authority.Filing {
	f.t.Helper()
	doc := []byte(`{"period":"2026-03","boxes":{"vat_due":125.50}}`)
	hash, err := adapter.CanonicalHash(doc)
	require.NoError(f.t, err)
	fl, created, err := f.st.CreateFiling(tc(tenant), authority.CreateFiling{InterfaceID: iface, TaxPeriod: "2026-03", FilingType: "VAT_RETURN",
		AmountText: "125.5000", SubjectRef: "subj-" + uuid.NewString(), SubjectFingerprint: hash, Document: doc, PayloadHash: hash, Rail: "VAT-RETURN@2", Actor: "maker"})
	require.NoError(f.t, err)
	require.True(f.t, created)
	return fl
}

func entry(op, disp, class string) authority.TransportEntry {
	return authority.TransportEntry{ProviderKey: "taxauth", ProfileVersion: 1, Operation: op, Disposition: disp, Class: class, HTTPStatus: 200, RequestHash: "rh", ResponseHash: "sh", ProviderRef: "RC-1", DurationMS: 5}
}

func (f *fixture) outbox(tenant, aggregate, suffix string) int {
	var n int
	require.NoError(f.t, f.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type LIKE $3`, tenant, aggregate, "%"+suffix).Scan(&n))
	return n
}

func taxSpec() adapter.Spec {
	return adapter.Spec{
		Kind: "TAX", RequiredScope: "tax.file", Rails: []string{"VAT-RETURN@2"},
		Submit:   adapter.Request{Path: "/v1/returns", Body: []adapter.Field{{Name: "return", From: "document"}}},
		Response: adapter.Response{RefPath: "receipt_id", StatusPath: "status", StatusMap: map[string]string{"RECEIVED": "ACKNOWLEDGED"}},
		Auth:     adapter.Auth{Type: "bearer"}, TimeoutSeconds: 5, Schema: json.RawMessage(`{"type":"object"}`),
	}
}

func TestAuthority_CreateIsIdempotentPerSubjectAndExact(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	ifc := f.iface(tenant, nil)
	a := f.filing(tenant, ifc)
	assert.Equal(t, authority.StatusPrepared, a.Status)
	assert.Equal(t, "125.5000", a.AmountText, "the amount is read exactly")
	assert.Equal(t, "GB", a.Jurisdiction)
	assert.Equal(t, 24*time.Hour, a.EscalateBefore)
	assert.JSONEq(t, `{"period":"2026-03","boxes":{"vat_due":125.50}}`, string(a.Document))
	assert.Equal(t, 1, f.outbox(tenant, a.SubmissionID, "authority-attempt.prepared"))

	// the same approved subject again: the one existing attempt, no new row, no new event
	again, created, err := f.st.CreateFiling(tc(tenant), authority.CreateFiling{InterfaceID: ifc, TaxPeriod: "2026-03", FilingType: "VAT_RETURN",
		SubjectRef: a.SubjectRef, SubjectFingerprint: a.SubjectFingerprint, Document: a.Document, PayloadHash: a.PayloadHash, Rail: "VAT-RETURN@2"})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, a.SubmissionID, again.SubmissionID)
	assert.Equal(t, 1, f.outbox(tenant, a.SubmissionID, "authority-attempt.prepared"))

	// the same subject with a DIFFERENT document is a conflict, never an edit
	_, _, err = f.st.CreateFiling(tc(tenant), authority.CreateFiling{InterfaceID: ifc, TaxPeriod: "2026-03", FilingType: "VAT_RETURN",
		SubjectRef: a.SubjectRef, SubjectFingerprint: strings.Repeat("a", 64), Document: []byte(`{"x":1}`), PayloadHash: strings.Repeat("a", 64), Rail: "VAT-RETURN@2"})
	assert.ErrorIs(t, err, authority.ErrSubjectConflict)

	// concurrent creates of one subject yield exactly one attempt
	var wg sync.WaitGroup
	ids := make(chan string, 10)
	sub := "race-" + uuid.NewString()
	doc := []byte(`{"period":"2026-04"}`)
	h, _ := adapter.CanonicalHash(doc)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fl, _, err := f.st.CreateFiling(tc(tenant), authority.CreateFiling{InterfaceID: ifc, TaxPeriod: "2026-04", FilingType: "VAT_RETURN",
				SubjectRef: sub, SubjectFingerprint: h, Document: doc, PayloadHash: h, Rail: "VAT-RETURN@2"})
			if err != nil {
				t.Errorf("%v", err)
				return
			}
			ids <- fl.SubmissionID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	assert.Len(t, seen, 1, "one authority attempt identity per approved subject")
}

func TestAuthority_BeginSendIsACompareAndSet(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	fl := f.filing(tenant, f.iface(tenant, nil))
	_, err := f.st.MarkValidated(tc(tenant), fl.SubmissionID, "op", 1)
	require.NoError(t, err)
	var won, lost atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.st.BeginSend(tc(tenant), fl.SubmissionID, authority.StatusValidated, 1, "hash-1", "op")
			switch {
			case err == nil:
				won.Add(1)
			case errors.Is(err, authority.ErrInFlight):
				lost.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, won.Load())
	assert.EqualValues(t, 11, lost.Load())
	got, _ := f.st.LoadFiling(tc(tenant), fl.SubmissionID)
	assert.Equal(t, authority.StatusSubmitting, got.Status)
	assert.Equal(t, 1, got.SendCount)
	assert.NotNil(t, got.InFlightSince)
}

func TestAuthority_FinishOutcomesAreAtomicWithEvidenceAndEvents(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	ctx := tc(tenant)
	ifc := f.iface(tenant, nil)

	// received → SUBMITTED; later ACCEPTED sets the acknowledgement reference the control population reads
	a := f.filing(tenant, ifc)
	_, _ = f.st.MarkValidated(ctx, a.SubmissionID, "op", 1)
	_, err := f.st.BeginSend(ctx, a.SubmissionID, authority.StatusValidated, 1, "h", "op")
	require.NoError(t, err)
	next := time.Now().Add(time.Minute)
	got, err := f.st.Finish(ctx, a.SubmissionID, authority.Outcome{Kind: authority.OutSubmitted, From: authority.StatusSubmitting,
		AuthorityRef: "RC-1", Authority: "RECEIVED", ReceiptHash: "sh", NextPoll: &next, Transport: entry("SUBMIT", "ANSWERED", "ACKNOWLEDGED")}, "op", "corr")
	require.NoError(t, err)
	assert.Equal(t, authority.StatusSubmitted, got.Status)
	assert.Equal(t, "RC-1", got.AuthorityRef)
	assert.Empty(t, f.ack(a.SubmissionID), "received is not acknowledged")
	assert.Equal(t, 1, f.outbox(tenant, a.SubmissionID, "authority-attempt.submitted"))
	log, _ := f.st.ListTransport(ctx, a.SubmissionID)
	require.Len(t, log, 1)

	got, err = f.st.Finish(ctx, a.SubmissionID, authority.Outcome{Kind: authority.OutAccepted, From: authority.StatusSubmitted,
		AuthorityRef: "RC-1", Authority: "ACCEPTED", Transport: entry("QUERY", "ANSWERED", "ACCEPTED")}, "op", "corr")
	require.NoError(t, err)
	assert.Equal(t, authority.StatusAccepted, got.Status)
	assert.Equal(t, "RC-1", f.ack(a.SubmissionID), "only an authority acceptance acknowledges the filing")
	assert.Equal(t, 1, f.outbox(tenant, a.SubmissionID, "authority-attempt.response-received"))

	// a stale outcome is refused and changes nothing
	_, err = f.st.Finish(ctx, a.SubmissionID, authority.Outcome{Kind: authority.OutUnknown, From: authority.StatusSubmitting, Transport: entry("SUBMIT", "UNKNOWN", "")}, "op", "c")
	assert.ErrorIs(t, err, authority.ErrInFlight)
	cur, _ := f.st.LoadFiling(ctx, a.SubmissionID)
	assert.Equal(t, authority.StatusAccepted, cur.Status)

	// unknown → PENDING_UNKNOWN + event; release with a not-before
	b := f.filing(tenant, ifc)
	_, _ = f.st.MarkValidated(ctx, b.SubmissionID, "op", 1)
	_, _ = f.st.BeginSend(ctx, b.SubmissionID, authority.StatusValidated, 1, "h", "op")
	_, err = f.st.Finish(ctx, b.SubmissionID, authority.Outcome{Kind: authority.OutUnknown, From: authority.StatusSubmitting, Failure: "no_answer_after_send", NextPoll: &next,
		Transport: entry("SUBMIT", "UNKNOWN", "")}, "op", "c")
	require.NoError(t, err)
	cur, _ = f.st.LoadFiling(ctx, b.SubmissionID)
	assert.Equal(t, authority.StatusPendingUnknown, cur.Status)
	assert.Equal(t, 1, f.outbox(tenant, b.SubmissionID, "authority-attempt.unknown-outcome"))

	c := f.filing(tenant, ifc)
	_, _ = f.st.MarkValidated(ctx, c.SubmissionID, "op", 1)
	_, _ = f.st.BeginSend(ctx, c.SubmissionID, authority.StatusValidated, 1, "h", "op")
	nb := time.Now().Add(time.Hour)
	_, err = f.st.Finish(ctx, c.SubmissionID, authority.Outcome{Kind: authority.OutRelease, From: authority.StatusSubmitting, ReleaseTo: authority.StatusValidated, NotBefore: &nb,
		Failure: "rate_limited", Transport: entry("SUBMIT", "ANSWERED", "RATE_LIMITED")}, "op", "c")
	require.NoError(t, err)
	_, err = f.st.BeginSend(ctx, c.SubmissionID, authority.StatusValidated, 1, "h", "op")
	assert.ErrorIs(t, err, authority.ErrNotBefore)

	// rejected is final
	d := f.filing(tenant, ifc)
	_, _ = f.st.MarkValidated(ctx, d.SubmissionID, "op", 1)
	_, _ = f.st.BeginSend(ctx, d.SubmissionID, authority.StatusValidated, 1, "h", "op")
	r, err := f.st.Finish(ctx, d.SubmissionID, authority.Outcome{Kind: authority.OutRejected, From: authority.StatusSubmitting, Reason: "bad box 3",
		Transport: entry("SUBMIT", "ANSWERED", "REJECTED")}, "op", "c")
	require.NoError(t, err)
	assert.Equal(t, authority.StatusRejected, r.Status)
	assert.Equal(t, "bad box 3", r.LastFailure)
}

func (f *fixture) ack(id string) string {
	var a *string
	require.NoError(f.t, f.admin.QueryRow(context.Background(), `SELECT ack_reference FROM tax_filing_submissions WHERE submission_id = $1`, id).Scan(&a))
	if a == nil {
		return ""
	}
	return *a
}

func TestAuthority_TheDatabaseGuardsWhatWasApproved(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	ctx := tc(tenant)
	fl := f.filing(tenant, f.iface(tenant, nil))
	su := func(sql string, args ...any) error {
		_, err := f.admin.Exec(context.Background(), sql, args...)
		return err
	}
	// the approved subject can never change (a changed document is a different subject)
	for name, sql := range map[string]string{
		"document":    `UPDATE tax_filing_submissions SET document = '{"x":1}'::jsonb WHERE submission_id = $1`,
		"hash":        `UPDATE tax_filing_submissions SET payload_hash = repeat('b', 64) WHERE submission_id = $1`,
		"fingerprint": `UPDATE tax_filing_submissions SET subject_fingerprint = 'forged' WHERE submission_id = $1`,
		"subject":     `UPDATE tax_filing_submissions SET subject_ref = 'other' WHERE submission_id = $1`,
		"amount":      `UPDATE tax_filing_submissions SET tax_amount = 1 WHERE submission_id = $1`,
		"period":      `UPDATE tax_filing_submissions SET tax_period = '2026-04' WHERE submission_id = $1`,
		"deadline":    `UPDATE tax_filing_submissions SET deadline_at = now() WHERE submission_id = $1`,
		"delete":      `DELETE FROM tax_filing_submissions WHERE submission_id = $1`,
	} {
		assert.Error(t, su(sql, fl.SubmissionID), "%s must be immutable once approved", name)
	}
	// it cannot be accepted without having been sent
	assert.Error(t, su(`UPDATE tax_filing_submissions SET status = 'ACCEPTED' WHERE submission_id = $1`, fl.SubmissionID))
	assert.Error(t, su(`UPDATE tax_filing_submissions SET status = 'SUBMITTING' WHERE submission_id = $1`, fl.SubmissionID), "SUBMITTING needs its start time")

	// pinned once sent
	_, _ = f.st.MarkValidated(ctx, fl.SubmissionID, "op", 1)
	_, err := f.st.BeginSend(ctx, fl.SubmissionID, authority.StatusValidated, 1, "hash-A", "op")
	require.NoError(t, err)
	_, err = f.st.Finish(ctx, fl.SubmissionID, authority.Outcome{Kind: authority.OutUnknown, From: authority.StatusSubmitting, Transport: entry("SUBMIT", "UNKNOWN", "")}, "op", "c")
	require.NoError(t, err)
	assert.Error(t, su(`UPDATE tax_filing_submissions SET sent_payload_hash = 'forged' WHERE submission_id = $1`, fl.SubmissionID))
	assert.Error(t, su(`UPDATE tax_filing_submissions SET profile_version = 9 WHERE submission_id = $1`, fl.SubmissionID))
	assert.Error(t, su(`UPDATE tax_filing_submissions SET rail = 'VAT-RETURN@9' WHERE submission_id = $1`, fl.SubmissionID))
	assert.Error(t, su(`UPDATE tax_filing_submissions SET send_count = 0 WHERE submission_id = $1`, fl.SubmissionID))
	_, err = f.st.BeginSend(ctx, fl.SubmissionID, authority.StatusPendingUnknown, 1, "hash-B", "op")
	assert.ErrorIs(t, err, authority.ErrPayloadChanged, "a retry must be exactly the original")
	_, err = f.st.BeginSend(ctx, fl.SubmissionID, authority.StatusPendingUnknown, 2, "hash-A", "op")
	assert.ErrorIs(t, err, authority.ErrPayloadChanged)

	// final states are final, receipts too
	_, err = f.st.Finish(ctx, fl.SubmissionID, authority.Outcome{Kind: authority.OutAccepted, From: authority.StatusPendingUnknown, AuthorityRef: "R", Transport: entry("QUERY", "ANSWERED", "ACCEPTED")}, "op", "c")
	require.NoError(t, err)
	assert.Error(t, su(`UPDATE tax_filing_submissions SET status = 'REJECTED' WHERE submission_id = $1`, fl.SubmissionID))
	assert.Error(t, su(`UPDATE tax_filing_submissions SET authority_ref = 'other' WHERE submission_id = $1`, fl.SubmissionID))

	// evidence is append-only
	var id string
	require.NoError(t, f.admin.QueryRow(context.Background(), `SELECT log_id::text FROM tax_transport_log WHERE submission_id = $1 LIMIT 1`, fl.SubmissionID).Scan(&id))
	assert.Error(t, su(`UPDATE tax_transport_log SET http_status = 1 WHERE log_id = $1`, id))
	assert.Error(t, su(`DELETE FROM tax_transport_log WHERE log_id = $1`, id))

	// a BLOCKED filing is terminal and carries its reason
	b := f.filing(tenant, f.iface(tenant, nil))
	blocked, err := f.st.Block(ctx, b.SubmissionID, "schema validation failed: $.period must match", "op", "c")
	require.NoError(t, err)
	assert.Equal(t, authority.StatusBlocked, blocked.Status)
	assert.Equal(t, 1, f.outbox(tenant, b.SubmissionID, "authority-attempt.blocked"))
	assert.Error(t, su(`UPDATE tax_filing_submissions SET status = 'VALIDATED' WHERE submission_id = $1`, b.SubmissionID))
	_, err = f.st.BeginSend(ctx, b.SubmissionID, authority.StatusBlocked, 1, "h", "op")
	assert.Error(t, err)
}

func TestAuthority_SweepAndPollDiscoveryAcrossTenantsUnderRLS(t *testing.T) {
	f := newFixture(t)
	t1, t2 := uuid.NewString(), uuid.NewString()
	a := f.filing(t1, f.iface(t1, nil))
	b := f.filing(t2, f.iface(t2, nil))
	fresh := f.filing(t1, f.iface(t1, nil))
	for _, p := range []struct{ tenant, id string }{{t1, a.SubmissionID}, {t2, b.SubmissionID}, {t1, fresh.SubmissionID}} {
		_, err := f.st.MarkValidated(tc(p.tenant), p.id, "op", 1)
		require.NoError(t, err)
		_, err = f.st.BeginSend(tc(p.tenant), p.id, authority.StatusValidated, 1, "h", "op")
		require.NoError(t, err)
	}
	_, err := f.admin.Exec(context.Background(), `UPDATE tax_filing_submissions SET in_flight_since = now() - interval '1 hour' WHERE submission_id = ANY($1)`, []string{a.SubmissionID, b.SubmissionID})
	require.NoError(t, err)
	n, err := f.st.SweepStale(context.Background(), time.Now().Add(-5*time.Minute), "sweeper")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	for _, p := range []struct {
		tenant, id, want string
	}{{t1, a.SubmissionID, authority.StatusPendingUnknown}, {t2, b.SubmissionID, authority.StatusPendingUnknown}, {t1, fresh.SubmissionID, authority.StatusSubmitting}} {
		got, _ := f.st.LoadFiling(tc(p.tenant), p.id)
		assert.Equal(t, p.want, got.Status)
	}
	assert.Equal(t, 1, f.outbox(t1, a.SubmissionID, "authority-attempt.unknown-outcome"))

	// the swept filings are due to be asked about (next_poll_at = now), across tenants
	time.Sleep(10 * time.Millisecond)
	due, err := f.st.DuePolls(context.Background(), time.Now(), 50)
	require.NoError(t, err)
	got := map[string]string{}
	for _, d := range due {
		got[d.SubmissionID] = d.TenantID
	}
	assert.Equal(t, t1, got[a.SubmissionID])
	assert.Equal(t, t2, got[b.SubmissionID])
	assert.NotContains(t, got, fresh.SubmissionID)

	// the worker flag admits SELECT only: without it an unscoped session sees nothing
	var cnt int
	require.NoError(t, f.app.QueryRow(context.Background(), `SELECT count(*) FROM tax_filing_submissions`).Scan(&cnt))
	assert.Equal(t, 0, cnt)
	_, err = f.app.Exec(context.Background(), `UPDATE tax_filing_submissions SET last_failure = 'x'`)
	require.NoError(t, err) // no rows visible, none touched
}

func TestAuthority_TenantsCannotSeeOrTouchEachOther(t *testing.T) {
	f := newFixture(t)
	owner, other := uuid.NewString(), uuid.NewString()
	fl := f.filing(owner, f.iface(owner, nil))
	_, _ = f.st.MarkValidated(tc(owner), fl.SubmissionID, "op", 1)
	_, _ = f.st.BeginSend(tc(owner), fl.SubmissionID, authority.StatusValidated, 1, "h", "op")
	_, _ = f.st.Finish(tc(owner), fl.SubmissionID, authority.Outcome{Kind: authority.OutSubmitted, From: authority.StatusSubmitting, AuthorityRef: "RC", Transport: entry("SUBMIT", "ANSWERED", "ACKNOWLEDGED")}, "op", "c")

	_, err := f.st.LoadFiling(tc(other), fl.SubmissionID)
	assert.ErrorIs(t, err, authority.ErrFilingNotFound)
	log, _ := f.st.ListTransport(tc(other), fl.SubmissionID)
	assert.Empty(t, log)
	_, err = f.st.LoadFiling(context.Background(), fl.SubmissionID)
	assert.ErrorIs(t, err, authority.ErrFilingNotFound)
	// another tenant cannot drive it: not validate, not block, not finish — and a failed attempt leaves no junk evidence
	_, err = f.st.Finish(tc(other), fl.SubmissionID, authority.Outcome{Kind: authority.OutAccepted, From: authority.StatusSubmitted, Transport: entry("QUERY", "ANSWERED", "ACCEPTED")}, "op", "c")
	assert.Error(t, err)
	_, err = f.st.Block(tc(other), fl.SubmissionID, "x", "op", "c")
	assert.Error(t, err)
	cur, _ := f.st.LoadFiling(tc(owner), fl.SubmissionID)
	assert.Equal(t, authority.StatusSubmitted, cur.Status)
	var junk int
	require.NoError(t, f.admin.QueryRow(context.Background(), `SELECT count(*) FROM tax_transport_log WHERE submission_id = $1 AND tenant_id = $2`, fl.SubmissionID, other).Scan(&junk))
	assert.Equal(t, 0, junk, "a foreign tenant's failed outcome must not write evidence against another tenant's filing")
	// a subject ref is unique per tenant, not globally: another tenant may use the same one
	ifc2 := f.iface(other, nil)
	doc := []byte(`{"p":1}`)
	h, _ := adapter.CanonicalHash(doc)
	_, created, err := f.st.CreateFiling(tc(other), authority.CreateFiling{InterfaceID: ifc2, TaxPeriod: "2026-03", FilingType: "VAT_RETURN", SubjectRef: fl.SubjectRef,
		SubjectFingerprint: h, Document: doc, PayloadHash: h, Rail: "VAT-RETURN@2"})
	require.NoError(t, err)
	assert.True(t, created)
	// and a filing cannot be created against another tenant's interface
	_, _, err = f.st.CreateFiling(tc(other), authority.CreateFiling{InterfaceID: f.iface(owner, nil), TaxPeriod: "2026-03", FilingType: "VAT_RETURN",
		SubjectRef: "x-" + uuid.NewString(), SubjectFingerprint: h, Document: doc, PayloadHash: h, Rail: "VAT-RETURN@2"})
	assert.Error(t, err)
}

func TestAuthority_ProfilesMakerCheckerImmutableAndPlatformScoped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := "tax-" + uuid.NewString()[:8]
	p1, err := f.st.PutProfile(ctx, key, taxSpec(), "author")
	require.NoError(t, err)
	assert.Equal(t, 1, p1.Version)
	assert.Equal(t, authority.ProfileDraft, p1.Status)
	_, err = f.st.GetProfile(ctx, key, 0)
	assert.ErrorIs(t, err, authority.ErrProfileNotFound, "a draft is not usable")
	_, err = f.st.CertifyProfile(ctx, key, 1, "author")
	assert.ErrorIs(t, err, authority.ErrProfileSelfCertify)
	c, err := f.st.CertifyProfile(ctx, key, 1, "reviewer")
	require.NoError(t, err)
	assert.Equal(t, authority.ProfileCertified, c.Status)
	_, err = f.st.CertifyProfile(ctx, key, 1, "reviewer2")
	assert.ErrorIs(t, err, authority.ErrProfileState)
	assert.Equal(t, 2, f.outbox(authority.PlatformScopeID, key, "authority-profile.published")+f.outbox(authority.PlatformScopeID, key, "authority-profile.certified"))

	for _, sql := range []string{
		`UPDATE tax_authority_profiles SET spec = '{}'::jsonb WHERE provider_key = $1`,
		`UPDATE tax_authority_profiles SET spec_hash = repeat('a', 64) WHERE provider_key = $1`,
		`DELETE FROM tax_authority_profiles WHERE provider_key = $1`,
	} {
		_, err := f.admin.Exec(ctx, sql, key)
		assert.Error(t, err, sql)
	}
	p2, err := f.st.PutProfile(ctx, key, taxSpec(), "author")
	require.NoError(t, err)
	assert.Equal(t, 2, p2.Version)
	got, _ := f.st.GetProfile(ctx, key, 0)
	assert.Equal(t, 1, got.Version)
	_, err = f.st.SetProfileStatus(ctx, key, 1, authority.ProfileDeprecated)
	require.NoError(t, err)
	_, err = f.st.SetProfileStatus(ctx, key, 1, authority.ProfileWithdrawn)
	require.NoError(t, err)
	_, err = f.st.SetProfileStatus(ctx, key, 1, authority.ProfileCertified)
	assert.ErrorIs(t, err, authority.ErrProfileState)

	// a tenant cannot publish platform profiles
	tx, err := f.app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, uuid.NewString())
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO tax_authority_profiles (provider_key, version, spec, spec_hash, created_by) VALUES ('rogue-auth', 1, '{}'::jsonb, repeat('a', 64), 'x')`)
	assert.Error(t, err)
}

func TestAuthority_EscalationIsRecordedOnceWithAnEvent(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	ctx := tc(tenant)
	fl := f.filing(tenant, f.iface(tenant, nil))
	require.NoError(t, f.st.Escalate(ctx, fl.SubmissionID, "outage", "op", "c"))
	require.NoError(t, f.st.Escalate(ctx, fl.SubmissionID, "outage again", "op", "c"))
	cur, _ := f.st.LoadFiling(ctx, fl.SubmissionID)
	assert.NotNil(t, cur.EscalatedAt)
	assert.Equal(t, 1, f.outbox(tenant, fl.SubmissionID, "authority-attempt.deadline-at-risk"))
	// a final filing is not escalated
	acc := f.filing(tenant, f.iface(tenant, nil))
	_, _ = f.st.MarkValidated(ctx, acc.SubmissionID, "op", 1)
	_, _ = f.st.BeginSend(ctx, acc.SubmissionID, authority.StatusValidated, 1, "h", "op")
	_, _ = f.st.Finish(ctx, acc.SubmissionID, authority.Outcome{Kind: authority.OutAccepted, From: authority.StatusSubmitting, AuthorityRef: "R", Transport: entry("SUBMIT", "ANSWERED", "ACCEPTED")}, "op", "c")
	require.NoError(t, f.st.Escalate(ctx, acc.SubmissionID, "late", "op", "c"))
	assert.Equal(t, 0, f.outbox(tenant, acc.SubmissionID, "authority-attempt.deadline-at-risk"))
}

func TestAuthority_ExistingFilingsAndControlPopulationAreUnaffected(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	ifc := f.iface(tenant, nil)
	// the legacy path: recorded intent, no document
	legacy := &domain.TaxFilingSubmission{InterfaceID: ifc, TaxPeriod: "2026-02", FilingType: "VAT_RETURN", TaxAmount: 10, Status: domain.TaxFilingPending}
	require.NoError(t, f.st.CreateSubmission(tc(tenant), legacy))
	got, err := f.st.GetSubmissionByID(tc(tenant), legacy.SubmissionID)
	require.NoError(t, err)
	assert.Equal(t, domain.TaxFilingPending, got.Status)
	// an approved-but-unaccepted attempt still counts as unacknowledged; an accepted one does not
	a := f.filing(tenant, ifc)
	assert.Empty(t, f.ack(a.SubmissionID))
}
