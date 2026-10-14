//go:build integration

// Real-Postgres tests for AR-05. Run (serially):
//
//	go test -tags=integration -count=1 -p 1 -timeout=600s ./internal/ar05/...
//
// The embedded superuser bypasses RLS, so isolation tests prove the explicit tenant_id predicates, not the policies.
// Spec section 18 scenarios carry their number: TestNeg14 (wrong e-invoice jurisdiction/network profile), TestNeg15
// (forged e-invoice callback accepted), TestNeg16 (e-invoice timeout treated as failed and duplicate resubmitted). The four
// rows of the AR-05 table are TestAR05Row1..4.
package ar05

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/kit/kittest"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	var stop func()
	pool, stop = kittest.Start("ar05_test", 17650)
	if _, err := pool.Exec(context.Background(), `CREATE TABLE ar05_test_dimension (invoice_id text NOT NULL, state text NOT NULL)`); err != nil {
		panic(err)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

type env struct {
	t      *testing.T
	m      *Module
	h      http.Handler
	tenant string
	entity string
	inv    *fakeInvoices
	prof   *fakeProfiles
	ent    *fakeEntJur
	reg    *fakeRegistry
	tr     *FakeTransport
	clk    *testClock
	secret []byte
	doc    *kit.IssuedInvoice
}

type envOpt func(*envCfg)

type envCfg struct {
	noTransport bool
	noRegistry  bool
	writeTx     bool
}

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	var cfg envCfg
	for _, o := range opts {
		o(&cfg)
	}
	e := &env{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), inv: newFakeInvoices(), prof: &fakeProfiles{profile: defaultProfile()}, ent: &fakeEntJur{code: "GB"},
		reg: &fakeRegistry{}, tr: NewFakeTransport("acme"), clk: newClock(), secret: []byte("acme-callback-secret")}
	e.inv.writeTx = cfg.writeTx
	deps := testDeps(&fakeAuthz{grants: standardGrants()}, e.clk.Now)
	deps.DB = kit.NewDB(pool)
	col := Collaborators{Invoices: e.inv, Profiles: e.prof, Catalog: StaticProfileCatalog{"GB|INVOICE": "PEPPOL_INVOICE"}, Entities: e.ent,
		Secrets: MapSecrets{"acme": e.secret, "evil": []byte("evil-secret")}}
	if !cfg.noRegistry {
		col.Registry = e.reg
	}
	if !cfg.noTransport {
		col.Transports = NetworkTransports{"PEPPOL": e.tr}
	}
	e.m = New(deps, col)
	e.h = testRouter(e.m)
	e.doc = sampleInvoice(e.entity)
	e.inv.put(e.tenant, e.doc)
	return e
}

func (e *env) cmd(principal, name string, body map[string]any) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodPost, base+"commands/"+name, e.tenant, principal, "", body)
	return code, b
}

func (e *env) cmdKey(principal, name, key string, body map[string]any) (int, []byte, http.Header) {
	return call(e.t, e.h, http.MethodPost, base+"commands/"+name, e.tenant, principal, key, body)
}

func (e *env) query(name, qs string) (int, []byte) {
	code, b, _ := call(e.t, e.h, http.MethodGet, base+name+"?"+qs, e.tenant, pNobody, "", nil)
	return code, b
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (e *env) exec(q string, args ...any) error {
	_, err := pool.Exec(context.Background(), q, args...)
	return err
}

func (e *env) events(subID, eventType string) int {
	return e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3`, e.tenant, subID, eventType)
}

func (e *env) audits(aggregateID, command string) int {
	return e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = $3`, e.tenant, aggregateID, command)
}

func (e *env) subCount() int {
	return e.count(`SELECT count(*) FROM ar05_einvoice_submissions WHERE tenant_id = $1`, e.tenant)
}

func (e *env) attemptCount(subID string) int {
	return e.count(`SELECT count(*) FROM ar05_submission_attempts WHERE tenant_id = $1 AND submission_id = $2`, e.tenant, subID)
}

func (e *env) ackCount(subID string) int {
	return e.count(`SELECT count(*) FROM ar05_acknowledgements WHERE tenant_id = $1 AND submission_id = $2`, e.tenant, subID)
}

func (e *env) securityEvents(reason string) int {
	return e.count(`SELECT count(*) FROM ar05_security_events WHERE reason = $1 AND tenant_id = $2`, reason, e.tenant)
}

func (e *env) body(s *Submission, extra map[string]any) map[string]any {
	b := map[string]any{"submission_id": s.SubmissionID, "expected_version": s.Version}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// get re-reads a submission through the query API.
func (e *env) get(id string) *Submission {
	e.t.Helper()
	code, b := e.query("getEInvoiceStatus", "submission_id="+id)
	require.Equal(e.t, http.StatusOK, code, string(b))
	var out struct {
		Submission *Submission `json:"submission"`
	}
	require.NoError(e.t, json.Unmarshal(b, &out))
	return out.Submission
}

func (e *env) prepare() *Submission {
	e.t.Helper()
	code, b := e.cmd(pSubmitter, "prepareEInvoice", map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB", "endpoint_id": "0088:5060000000001", "participant_id": "0088:seller", "purpose": "EXCHANGE"})
	require.Equal(e.t, http.StatusCreated, code, string(b))
	s := asResp(e.t, b).Submission
	require.Equal(e.t, StatePrepared, s.State)
	return s
}

func (e *env) validate(s *Submission) *Submission {
	e.t.Helper()
	code, b := e.cmd(pSubmitter, "validateEInvoice", e.body(s, nil))
	require.Equal(e.t, http.StatusOK, code, string(b))
	v := asResp(e.t, b).Submission
	require.Equal(e.t, StateValidated, v.State)
	return v
}

// ready is a VALIDATED submission.
func (e *env) ready() *Submission { return e.validate(e.prepare()) }

func (e *env) submit(principal string, s *Submission) (int, []byte) {
	return e.cmd(principal, "submitEInvoice", e.body(s, nil))
}

func (e *env) mustSubmit(s *Submission) respBody {
	e.t.Helper()
	code, b := e.submit(pSubmitter, s)
	require.Less(e.t, code, 300, string(b))
	return asResp(e.t, b)
}

type attemptView struct {
	ExternalAttemptID string   `json:"external_attempt_id"`
	AttemptNo         int      `json:"attempt_no"`
	ParentAttemptID   string   `json:"parent_attempt_id"`
	ReconciliationID  string   `json:"reconciliation_id"`
	Provider          string   `json:"provider"`
	RequestDigest     string   `json:"request_digest"`
	Outcome           string   `json:"outcome"`
	Results           []Result `json:"results"`
	StartedBy         string   `json:"started_by"`
}

func (e *env) attempts(subID string) []attemptView {
	e.t.Helper()
	code, b := e.query("getSubmissionAttempt", "submission_id="+subID)
	require.Equal(e.t, http.StatusOK, code, string(b))
	var out struct {
		Attempts []attemptView `json:"attempts"`
	}
	require.NoError(e.t, json.Unmarshal(b, &out))
	return out.Attempts
}

func outcomes(a attemptView) []string {
	var out []string
	for _, r := range a.Results {
		out = append(out, r.Outcome)
	}
	return out
}

type cbOpts struct {
	provider string
	secret   []byte
	ts       time.Time
	nonce    string
	rawBody  []byte // when set, signed as given instead of the JSON of body
	send     []byte // when set, SENT instead of the signed bytes (tampering)
}

func (e *env) cbBody(attemptID string, s *Submission, eventID, status string, extra map[string]any) map[string]any {
	b := map[string]any{"provider_event_id": eventID, "external_attempt_id": attemptID, "document_id": s.DocumentID, "status": status,
		"occurred_at": e.clk.Now().Format(time.RFC3339)}
	if status == "ACCEPTED" {
		b["receipt_id"] = "rcpt-" + eventID
		b["provider_reference"] = "prov-" + eventID
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// callback delivers a provider callback exactly as a provider would: no principal, no tenant header, no Idempotency-Key.
func (e *env) callback(body map[string]any, o cbOpts) (int, []byte) {
	e.t.Helper()
	if o.provider == "" {
		o.provider = "acme"
	}
	if o.secret == nil {
		o.secret = e.secret
	}
	if o.ts.IsZero() {
		o.ts = e.clk.Now()
	}
	if o.nonce == "" {
		o.nonce = uuid.NewString()
	}
	raw := o.rawBody
	if raw == nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(e.t, err)
	}
	ts := strconv.FormatInt(o.ts.Unix(), 10)
	sendBody := raw
	if o.send != nil {
		sendBody = o.send
	}
	req := httptest.NewRequest(http.MethodPost, CallbackPath, bytesReader(sendBody))
	req.Header.Set(HeaderProvider, o.provider)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, o.nonce)
	req.Header.Set(HeaderSignature, Sign(o.secret, ts, o.nonce, raw))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func requireRefused(t *testing.T, err error, sqlstate, fragment string) {
	t.Helper()
	require.Error(t, err)
	var pe *pgconn.PgError
	require.True(t, errors.As(err, &pe), err.Error())
	require.Equal(t, sqlstate, pe.Code, err.Error())
	require.Contains(t, pe.Message, fragment)
}

var errTimeout = errors.New("context deadline exceeded (client timeout)")

// ── PrepareEInvoice ──────────────────────────────────────────────────────────

func TestPrepare_BindsToTheIssuedDocument(t *testing.T) {
	e := newEnv(t)
	code, body, _ := e.cmdKey(pSubmitter, "prepareEInvoice", "k-prep-1", map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB", "endpoint_id": "EP-1", "participant_id": "P-1", "purpose": "EXCHANGE"})
	require.Equal(t, http.StatusCreated, code, string(body))
	r := asResp(t, body)
	s := r.Submission
	require.Equal(t, StatePrepared, s.State)
	require.Equal(t, 1, s.Version)
	require.Equal(t, e.doc.ContentHash, s.ContentHash, "bound to the invoice's immutable content hash")
	require.Equal(t, "INV-000042", s.DocumentNumber)
	require.Equal(t, "PEPPOL_INVOICE", s.ProfileCode)
	require.Equal(t, "1.0", s.ProfileVersion)
	require.Equal(t, "PEPPOL", s.Network)
	require.Equal(t, "JSON", s.Syntax)
	require.Equal(t, "EXCHANGE", s.Mode)
	require.Equal(t, "pv-1", s.PackVersionID, "pack provenance is retained")
	require.Equal(t, "EP-1", s.EndpointID)
	require.Len(t, s.ArtifactHash, 64)
	require.Equal(t, e.entity, s.LegalEntityID, "the entity is derived from the invoice, never from the body")

	// the artifact is stored with its hash
	var content []byte
	var sha string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT content, sha256 FROM ar05_artifacts WHERE submission_id = $1`, s.SubmissionID).Scan(&content, &sha))
	require.Equal(t, s.ArtifactHash, sha)
	require.Equal(t, sha, Sha256Hex(content))
	require.True(t, json.Valid(content))

	require.Equal(t, []string{"PREPARED"}, e.inv.states(e.doc.InvoiceID), "mirrored onto the e-invoice dimension")
	require.Equal(t, 1, e.events(s.SubmissionID, EventPrepared))
	require.Equal(t, 1, e.audits(s.SubmissionID, "prepareEInvoice"))
	require.Equal(t, 0, e.count(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1`, e.tenant), "AR-05 has no accounting consequence")
	require.Empty(t, e.inv.otherCalls(), "AR-05 writes nothing to the invoice owner except the dimension")

	// idempotent replay: the stored answer is returned and nothing is created twice
	code, body2, hdr := e.cmdKey(pSubmitter, "prepareEInvoice", "k-prep-1", map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB", "endpoint_id": "EP-1", "participant_id": "P-1", "purpose": "EXCHANGE"})
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, "true", hdr.Get("Idempotent-Replay"))
	require.JSONEq(t, string(body), string(body2))
	require.Equal(t, 1, e.subCount())

	// the same key with a different request is refused
	code, _, _ = e.cmdKey(pSubmitter, "prepareEInvoice", "k-prep-1", map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB", "purpose": "OTHER"})
	require.Equal(t, http.StatusUnprocessableEntity, code)

	// a second prepare under a different key continues the existing submission: one logical submission per (document, profile)
	code, body3 := e.cmd(pSubmitter, "prepareEInvoice", map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB"})
	require.Equal(t, http.StatusOK, code, string(body3))
	r3 := asResp(t, body3)
	require.True(t, r3.Existing)
	require.Equal(t, s.SubmissionID, r3.Submission.SubmissionID)
	require.Equal(t, 1, e.subCount())
	require.Equal(t, 1, e.prof.calls, "the existing submission is returned without a second registry resolution")
}

func TestPrepare_ConcurrentPreparesYieldOneSubmission(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, b := e.cmd(pSubmitter, "prepareEInvoice", map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB"})
			if code == http.StatusCreated || code == http.StatusOK {
				ids <- asResp(t, b).Submission.SubmissionID
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	require.Len(t, seen, 1, "every concurrent prepare ends with the same single submission")
	require.Equal(t, 1, e.subCount())
	require.Equal(t, 1, e.inv.statesCount(e.doc.InvoiceID, "PREPARED"), "the dimension was set once")
}

func TestPrepare_OnlyIssuedInvoices(t *testing.T) {
	e := newEnv(t)
	code, b := e.cmd(pSubmitter, "prepareEInvoice", map[string]any{"document_id": uuid.NewString(), "jurisdiction_id": "GB"})
	require.Equal(t, http.StatusNotFound, code, "a draft / unissued / unknown invoice is NOT_FOUND: nothing authoritative to transmit")
	require.Equal(t, kit.CodeNotFound, errCode(t, b))
	require.Equal(t, 0, e.subCount())
}

func TestPrepare_ResubmissionChainAfterRejection(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	r := e.mustSubmit(s)
	att := r.ExternalAttemptID
	code, b := e.callback(e.cbBody(att, s, "ev-rej", "REJECTED", map[string]any{"reason_code": "SCHEMA_ERROR", "message": "BR-CO-15 failed"}), cbOpts{})
	require.Equal(t, http.StatusOK, code, string(b))
	rejected := e.get(s.SubmissionID)
	require.Equal(t, StateRejected, rejected.State)
	require.Equal(t, []string{"REJECTED"}, available2(e, rejected))

	// a chained resubmission: new submission, parent = the rejected one; the rejected one is preserved
	e.clk.Advance(time.Minute) // the test clock is frozen: give the new submission a later created_at
	s2 := e.prepare()
	require.NotEqual(t, s.SubmissionID, s2.SubmissionID)
	require.Equal(t, s.SubmissionID, s2.ParentSubmissionID)
	require.Equal(t, StateRejected, e.get(s.SubmissionID).State, "superseded attempts and submissions are preserved")
	require.Equal(t, 2, e.subCount())
	require.Equal(t, "PREPARED", e.inv.current(e.doc.InvoiceID))

	code, b = e.query("listSubmissionHistory", "document_id="+e.doc.InvoiceID)
	require.Equal(t, http.StatusOK, code, string(b))
	var hist struct {
		Submissions []Submission `json:"submissions"`
	}
	require.NoError(t, json.Unmarshal(b, &hist))
	require.Len(t, hist.Submissions, 2)
	require.Equal(t, s2.SubmissionID, hist.Submissions[0].SubmissionID, "newest first")
}

func available2(e *env, s *Submission) []string {
	// the final state of a rejected submission is reflected on the dimension
	return []string{e.inv.current(s.DocumentID)}
}

// ── TestNeg14: wrong e-invoice jurisdiction / network profile ────────────────

func TestNeg14_WrongJurisdictionOrProfileIsRefused(t *testing.T) {
	e := newEnv(t)
	prepare := func(extra map[string]any) (int, []byte) {
		b := map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB"}
		for k, v := range extra {
			b[k] = v
		}
		return e.cmd(pSubmitter, "prepareEInvoice", b)
	}
	type tc struct {
		name   string
		setup  func()
		extra  map[string]any
		status int
		code   string
	}
	cases := []tc{
		{"jurisdiction is not the entity's jurisdiction", nil, map[string]any{"jurisdiction_id": "DE", "profile_code": "XRECHNUNG"}, 422, kit.CodeEInvoiceValidationFailed},
		{"profile is not the one configured for the jurisdiction", nil, map[string]any{"profile_code": "XRECHNUNG"}, 422, kit.CodeEInvoiceValidationFailed},
		{"network does not match the profile's", nil, map[string]any{"network": "SDI"}, 422, kit.CodeEInvoiceValidationFailed},
		{"profile is for another document type", func() { e.prof.set(func(p *Profile) { p.DocumentType = DocCreditNote }) }, nil, 422, kit.CodeEInvoiceValidationFailed},
		{"retired or unknown profile", func() { e.prof.setErr(ErrNoProfile) }, nil, 422, kit.CodeEInvoiceValidationFailed},
		{"profile unusable (dependency expired)", func() { e.prof.setErr(ErrProfileInvalid) }, nil, 422, kit.CodeEInvoiceValidationFailed},
		{"syntax with no renderer (UBL)", func() { e.prof.set(func(p *Profile) { p.Syntax = SyntaxUBL }) }, nil, 422, kit.CodeEInvoiceValidationFailed},
		{"syntax with no renderer (authority specific)", func() { e.prof.set(func(p *Profile) { p.Syntax = SyntaxOther }) }, nil, 422, kit.CodeEInvoiceValidationFailed},
		{"incomplete profile (no transmission mode)", func() { e.prof.set(func(p *Profile) { p.Mode = "" }) }, nil, 422, kit.CodeEInvoiceValidationFailed},
		{"profile registry unavailable: fail closed", func() { e.prof.setErr(ErrProfileUnavailable) }, nil, 503, kit.CodeDependencyUnavailable},
		{"entity jurisdiction unavailable: fail closed", func() { e.ent.err = errors.New("registry down") }, nil, 503, kit.CodeDependencyUnavailable},
	}
	denied := 0
	for _, c := range cases {
		e.prof.set(func(p *Profile) { *p = defaultProfile() })
		e.prof.setErr(nil)
		e.ent.err = nil
		if c.setup != nil {
			c.setup()
		}
		code, b := prepare(c.extra)
		require.Equal(t, c.status, code, c.name+": "+string(b))
		require.Equal(t, c.code, errCode(t, b), c.name)
		if c.status == 422 {
			denied++
		}
	}
	require.Equal(t, 0, e.subCount(), "a refused prepare records no submission")
	require.Empty(t, e.inv.calls, "and touches nothing on the invoice")
	require.Equal(t, 0, e.tr.SubmitCount(), "and nothing is ever sent")
	require.Equal(t, denied, e.audits(e.doc.InvoiceID, "prepareEInvoice.denied"), "every refusal leaves evidence")

	// with the correct jurisdiction and profile it works
	e.prof.set(func(p *Profile) { *p = defaultProfile() })
	e.prof.setErr(nil)
	e.ent.err = nil
	code, b := prepare(map[string]any{"network": "peppol"})
	require.Equal(t, http.StatusCreated, code, string(b))
}

func TestNeg14_ProfileCannotBeRetargetedAfterPrepare(t *testing.T) {
	e := newEnv(t)
	s := e.prepare()
	// a later pack release / different registry answer cannot change the in-flight submission: it keeps its snapshot
	e.prof.set(func(p *Profile) { p.ProfileVersion = "2.0"; p.Network = "SDI" })
	s = e.validate(s)
	require.Equal(t, "1.0", s.ProfileVersion)
	require.Equal(t, "PEPPOL", s.Network)
	require.Equal(t, 1, e.prof.calls)
	requireRefused(t, e.exec(`UPDATE ar05_einvoice_submissions SET profile_code = 'OTHER' WHERE submission_id = $1`, s.SubmissionID), "P0001", "AR05_SUBMISSION_IMMUTABLE")
	requireRefused(t, e.exec(`UPDATE ar05_einvoice_submissions SET network = 'SDI' WHERE submission_id = $1`, s.SubmissionID), "P0001", "AR05_SUBMISSION_IMMUTABLE")
}

// ── ValidateEInvoice ─────────────────────────────────────────────────────────

func TestValidate_PassAndFail(t *testing.T) {
	e := newEnv(t)
	s := e.prepare()
	v := e.validate(s)
	require.Equal(t, 2, v.Version)
	require.Equal(t, "PASSED", v.ValidationStatus)
	require.Equal(t, []string{"PREPARED"}, e.inv.states(e.doc.InvoiceID), "VALIDATED is internal: the dimension stays PREPARED")
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar05_validation_results WHERE submission_id = $1 AND passed`, s.SubmissionID))

	code, b := e.query("getValidationErrors", "submission_id="+s.SubmissionID)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(b), `"validation_status":"PASSED"`)

	// a profile that needs a field AR cannot supply fails validation and the failure is stored
	e2 := newEnv(t)
	e2.prof.set(func(p *Profile) {
		p.SemanticMapping = append(p.SemanticMapping, MappingEntry{From: "invoice.buyer.vat_id", To: "cbc:CompanyID"})
	})
	s2 := e2.prepare()
	code, b = e2.cmd(pSubmitter, "validateEInvoice", e2.body(s2, nil))
	require.Equal(t, http.StatusUnprocessableEntity, code, string(b))
	require.Equal(t, kit.CodeEInvoiceValidationFailed, errCode(t, b))
	require.Contains(t, string(b), "REQUIRED_FIELD_UNSUPPORTED")
	failed := e2.get(s2.SubmissionID)
	require.Equal(t, StatePrepared, failed.State, "a failed validation never advances the state")
	require.Equal(t, "FAILED", failed.ValidationStatus)

	code, b = e2.query("getValidationErrors", "submission_id="+s2.SubmissionID)
	require.Equal(t, http.StatusOK, code)
	var ve struct {
		Errors []Issue `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(b, &ve))
	require.NotEmpty(t, ve.Errors)
	require.Equal(t, "invoice.buyer.vat_id", ve.Errors[0].Field)

	// the failed submission cannot be submitted; nothing reaches the provider
	code, b = e2.submit(pSubmitter, failed)
	require.Equal(t, http.StatusConflict, code, string(b))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, b))
	require.Equal(t, 0, e2.tr.SubmitCount())

	// every run is kept
	code, _ = e2.cmd(pSubmitter, "validateEInvoice", e2.body(failed, nil))
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, 2, e2.count(`SELECT count(*) FROM ar05_validation_results WHERE submission_id = $1 AND NOT passed`, s2.SubmissionID))
	require.Equal(t, 0, e.count(`SELECT count(*) FROM ar05_validation_results WHERE submission_id = $1 AND NOT passed`, s.SubmissionID))
}

func TestContentHashBinding_ChangedDocumentIsNeverTransmitted(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	e.inv.mu.Lock()
	e.inv.issued[e.tenant+"|"+e.doc.InvoiceID].ContentHash = "0123456789abcdef"
	e.inv.mu.Unlock()
	code, b := e.submit(pSubmitter, s)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(b))
	require.Equal(t, kit.CodeEInvoiceValidationFailed, errCode(t, b))
	require.Equal(t, 0, e.tr.SubmitCount())
	require.Equal(t, StateValidated, e.get(s.SubmissionID).State)
	require.Equal(t, 0, e.attemptCount(s.SubmissionID))
	require.Equal(t, 1, e.audits(s.SubmissionID, "contentBinding.denied"))

	p := newEnv(t)
	ps := p.prepare()
	p.inv.mu.Lock()
	p.inv.issued[p.tenant+"|"+p.doc.InvoiceID].ContentHash = "feedface"
	p.inv.mu.Unlock()
	code, _ = p.cmd(pSubmitter, "validateEInvoice", p.body(ps, nil))
	require.Equal(t, http.StatusUnprocessableEntity, code, "validation is bound to the content hash too")
}

// ── SubmitEInvoice ───────────────────────────────────────────────────────────

func TestSubmit_AcknowledgedByProvider(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	r := e.mustSubmit(s)
	require.Equal(t, OutAcknowledged, r.Outcome)
	require.Equal(t, StateSubmitted, r.Submission.State)
	require.Equal(t, "acme", r.Submission.Provider)
	require.Equal(t, 1, r.Submission.AttemptCount)
	require.NotEmpty(t, r.Submission.RegistrySubmissionID)
	require.Equal(t, []string{"PREPARED", "PENDING_UNKNOWN", "SUBMITTED"}, e.inv.states(e.doc.InvoiceID), "PENDING_UNKNOWN is entered BEFORE the call, then SUBMITTED")
	require.Empty(t, e.inv.otherCalls())
	require.Equal(t, 1, e.events(s.SubmissionID, EventSubmitted))
	require.Equal(t, 0, e.events(s.SubmissionID, EventStateUnknown))

	// the durable external_attempt_id is what the provider saw and what is stored
	require.Equal(t, 1, e.tr.SubmitCount())
	req := e.tr.Submits[0]
	require.Equal(t, r.ExternalAttemptID, req.ExternalAttemptID)
	require.Equal(t, s.ArtifactHash, req.ArtifactHash)
	require.Equal(t, Sha256Hex(req.Artifact), req.ArtifactHash)
	require.Equal(t, "0088:5060000000001", req.EndpointID)
	require.Equal(t, "PEPPOL", req.Network)
	as := e.attempts(s.SubmissionID)
	require.Len(t, as, 1)
	require.Equal(t, r.ExternalAttemptID, as[0].ExternalAttemptID)
	require.Equal(t, 1, as[0].AttemptNo)
	require.Len(t, as[0].RequestDigest, 64)
	require.Equal(t, OutAcknowledged, as[0].Outcome)
	require.Equal(t, "pv-1", s.PackVersionID)

	// regulatory registry: registered once, keyed per submission, with the artifact hash
	require.Len(t, e.reg.regs, 1)
	require.Equal(t, "ar05-reg-"+s.SubmissionID, e.reg.regs[0].Key)
	require.Equal(t, "sha256:"+s.ArtifactHash, e.reg.regs[0].PayloadHash)
	require.Equal(t, "INVOICE:"+e.doc.InvoiceID, e.reg.regs[0].SubjectRef)
	require.Equal(t, "GB", e.reg.regs[0].Jurisdiction)

	// a second submit (new key, current version) is suppressed: it returns the existing state
	cur := e.get(s.SubmissionID)
	code, b := e.submit(pSubmitter, cur)
	require.Equal(t, http.StatusOK, code, string(b))
	require.True(t, asResp(t, b).DuplicateSuppressed)
	require.Equal(t, 1, e.tr.SubmitCount(), "the provider saw the document exactly once")
}

// prepareFresh makes an independent invoice+submission in the same tenant (used where a second document is needed).
func (e *env) prepareFresh() *Submission {
	inv := sampleInvoice(e.entity)
	e.inv.put(e.tenant, inv)
	code, b := e.cmd(pSubmitter, "prepareEInvoice", map[string]any{"document_id": inv.InvoiceID, "jurisdiction_id": "GB"})
	require.Equal(e.t, http.StatusCreated, code, string(b))
	return asResp(e.t, b).Submission
}

func TestSubmit_IdempotentReplayReturnsStoredResult(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	code, b, _ := e.cmdKey(pSubmitter, "submitEInvoice", "k-submit", e.body(s, nil))
	require.Equal(t, http.StatusOK, code, string(b))
	code2, b2, hdr := e.cmdKey(pSubmitter, "submitEInvoice", "k-submit", e.body(s, nil))
	require.Equal(t, http.StatusOK, code2)
	require.Equal(t, "true", hdr.Get("Idempotent-Replay"))
	require.JSONEq(t, string(b), string(b2))
	require.Equal(t, 1, e.tr.SubmitCount(), "a replayed command never reaches the provider again")
	require.Equal(t, 1, e.attemptCount(s.SubmissionID))
}

func TestSubmit_ProviderNotConfiguredFailsCleanly(t *testing.T) {
	e := newEnv(t, func(c *envCfg) { c.noTransport = true })
	s := e.ready()
	code, b := e.submit(pSubmitter, s)
	require.Equal(t, http.StatusServiceUnavailable, code, string(b))
	require.Equal(t, CodeProviderNotConfigured, errCode(t, b), "a typed refusal, never a fake success")
	after := e.get(s.SubmissionID)
	require.Equal(t, StateValidated, after.State, "nothing changed: not PENDING_UNKNOWN, not SUBMITTED")
	require.Equal(t, s.Version, after.Version)
	require.Equal(t, 0, e.attemptCount(s.SubmissionID))
	require.Empty(t, e.reg.regs, "nothing is registered with the regulator before a provider exists")
	require.Equal(t, []string{"PREPARED"}, e.inv.states(e.doc.InvoiceID))
}

func TestSubmit_DefinitiveNotSentMayBeSubmittedAgain(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	e.tr.Script(FakeSubmit{Err: ErrNotSent})
	code, b := e.submit(pSubmitter, s)
	require.Equal(t, http.StatusServiceUnavailable, code, string(b))
	after := e.get(s.SubmissionID)
	require.Equal(t, StateValidated, after.State, "the transport guaranteed nothing left: back to VALIDATED")
	as := e.attempts(s.SubmissionID)
	require.Len(t, as, 1)
	require.Equal(t, OutNotSent, as[0].Outcome)
	require.Equal(t, "PREPARED", e.inv.current(e.doc.InvoiceID))

	r := e.mustSubmit(after)
	require.Equal(t, StateSubmitted, r.Submission.State)
	require.Equal(t, 2, len(e.attempts(s.SubmissionID)))
	require.NotEqual(t, as[0].ExternalAttemptID, r.ExternalAttemptID)
}

func TestSubmit_SynchronousProviderAnswers(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	e.tr.Script(FakeSubmit{Result: &SubmitResult{Disposition: DispAccepted, ProviderRef: "clr-1", ReceiptID: "RCPT-1", ResponseDigest: Sha256Hex([]byte("x"))}})
	r := e.mustSubmit(s)
	require.Equal(t, StateAccepted, r.Submission.State, "a clearance answer with a receipt is final")
	require.Equal(t, "ACCEPTED", e.inv.current(e.doc.InvoiceID))
	require.Equal(t, 1, e.events(s.SubmissionID, EventAccepted))
	require.Len(t, e.reg.events, 1, "the acceptance is mirrored to the regulatory registry")
	require.Equal(t, "ACCEPTED", e.reg.events[0].Status)
	require.Equal(t, "RCPT-1", e.reg.events[0].ReceiptID)

	// ACCEPTED without a receipt is NOT recorded as accepted
	e2 := newEnv(t)
	s2 := e2.ready()
	e2.tr.Script(FakeSubmit{Result: &SubmitResult{Disposition: DispAccepted, ProviderRef: "clr-2"}})
	r2 := e2.mustSubmit(s2)
	require.Equal(t, StateSubmitted, r2.Submission.State)
	as := e2.attempts(s2.SubmissionID)
	require.Equal(t, OutAcknowledged, as[0].Outcome)

	// a synchronous rejection
	e3 := newEnv(t)
	s3 := e3.ready()
	e3.tr.Script(FakeSubmit{Result: &SubmitResult{Disposition: DispRejected, ReasonCode: "BR-01", Message: "bad", ResponseDigest: Sha256Hex([]byte("y"))}})
	r3 := e3.mustSubmit(s3)
	require.Equal(t, StateRejected, r3.Submission.State)
	require.Equal(t, "REJECTED", e3.inv.current(e3.doc.InvoiceID))
	require.Equal(t, 1, e3.events(s3.SubmissionID, EventRejected))
	require.Empty(t, e3.inv.otherCalls())
}

func TestSubmit_RegistryOutageStopsBeforeAnythingIsSent(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	e.reg.err = errors.New("registry down")
	code, b := e.submit(pSubmitter, s)
	require.Equal(t, http.StatusServiceUnavailable, code, string(b))
	require.Equal(t, 0, e.tr.SubmitCount())
	require.Equal(t, 0, e.attemptCount(s.SubmissionID))
	require.Equal(t, StateValidated, e.get(s.SubmissionID).State)
	e.reg.err = nil
	require.Equal(t, StateSubmitted, e.mustSubmit(e.get(s.SubmissionID)).Submission.State)

	// local-only mode: no registry wired, everything else works
	l := newEnv(t, func(c *envCfg) { c.noRegistry = true })
	ls := l.ready()
	lr := l.mustSubmit(ls)
	require.Equal(t, StateSubmitted, lr.Submission.State)
	require.Empty(t, lr.Submission.RegistrySubmissionID)
}

func TestSubmit_NoAttemptWithoutItsDimensionMirror(t *testing.T) {
	e := newEnv(t, func(c *envCfg) { c.writeTx = true })
	s := e.ready()
	e.inv.mu.Lock()
	e.inv.failDim = errors.New("invoice owner unavailable")
	e.inv.mu.Unlock()
	code, _ := e.submit(pSubmitter, s)
	require.GreaterOrEqual(t, code, 500)
	require.Equal(t, 0, e.tr.SubmitCount(), "nothing is sent when the attempt could not be committed atomically with its mirror")
	require.Equal(t, 0, e.attemptCount(s.SubmissionID), "the attempt rolled back with the state")
	require.Equal(t, StateValidated, e.get(s.SubmissionID).State)
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar05_test_dimension WHERE invoice_id = $1`, e.doc.InvoiceID), "only the committed PREPARED mirror exists")
}

func TestPrepare_RollsBackAtomicallyWhenTheMirrorFails(t *testing.T) {
	e := newEnv(t, func(c *envCfg) { c.writeTx = true })
	e.inv.failDim = errors.New("invoice owner unavailable")
	code, _ := e.cmd(pSubmitter, "prepareEInvoice", map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB"})
	require.GreaterOrEqual(t, code, 500)
	require.Equal(t, 0, e.subCount())
	require.Equal(t, 0, e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, e.tenant, EventPrepared), "no event without the state change")
	require.Equal(t, 0, e.count(`SELECT count(*) FROM ar05_artifacts WHERE tenant_id = $1`, e.tenant))
	require.Equal(t, 0, e.count(`SELECT count(*) FROM ar05_test_dimension WHERE invoice_id = $1`, e.doc.InvoiceID))
}

// ── TestNeg16 / AR-05 row 1: timeout is PENDING_UNKNOWN, never failed; no duplicate resubmission ───────

func TestNeg16_TimeoutBecomesPendingUnknownAndIsNeverResubmitted(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	// the provider RECEIVED the document but the answer timed out: the dangerous case
	e.tr.Script(FakeSubmit{Err: errTimeout, Delivered: true})
	code, b, _ := e.cmdKey(pSubmitter, "submitEInvoice", "k-timeout", e.body(s, nil))
	require.Equal(t, http.StatusAccepted, code, string(b))
	r := asResp(t, b)
	require.Equal(t, StateUnknown, r.Submission.State, "a timeout is PENDING_UNKNOWN, not failed")
	require.Equal(t, kit.CodeExternalStateUnknown, r.Code)
	require.Equal(t, "PENDING_UNKNOWN", e.inv.current(e.doc.InvoiceID))
	require.Equal(t, 1, e.events(s.SubmissionID, EventStateUnknown))
	require.Equal(t, 0, e.events(s.SubmissionID, EventSubmitted))
	as := e.attempts(s.SubmissionID)
	require.Equal(t, OutAmbiguous, as[0].Outcome)
	att1 := as[0].ExternalAttemptID
	require.Equal(t, r.ExternalAttemptID, att1)

	// a duplicate submit is refused with the stable class and sends nothing
	cur := e.get(s.SubmissionID)
	code, b = e.submit(pSubmitter, cur)
	require.Equal(t, http.StatusConflict, code, string(b))
	require.Equal(t, kit.CodeExternalStateUnknown, errCode(t, b))
	require.Equal(t, 1, e.tr.SubmitCount(), "no duplicate send")
	// ... and so is a stale-version one
	code, b = e.submit(pSubmitter, s)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeStaleVersion, errCode(t, b))
	// the same Idempotency-Key replays the stored 202: it cannot cause a send either
	code, _, hdr := e.cmdKey(pSubmitter, "submitEInvoice", "k-timeout", e.body(s, nil))
	require.Equal(t, http.StatusAccepted, code)
	require.Equal(t, "true", hdr.Get("Idempotent-Replay"))
	require.Equal(t, 1, e.tr.SubmitCount())

	// RetrySubmission reconciles FIRST: the provider holds the document, so nothing is re-sent
	code, b = e.cmd(pSubmitter, "retrySubmission", e.body(cur, map[string]any{"reason_code": "TIMEOUT_FOLLOW_UP"}))
	require.Equal(t, http.StatusOK, code, string(b))
	rr := asResp(t, b)
	require.NotNil(t, rr.Retried)
	require.False(t, *rr.Retried)
	require.Equal(t, StateSubmitted, rr.Submission.State, "reconciled to what the provider confirmed")
	require.Equal(t, 1, e.tr.SubmitCount(), "still exactly one send")
	require.Len(t, e.tr.Queries, 1)
	require.Equal(t, att1, e.tr.Queries[0].ExternalAttemptID, "reconciliation asks about the durable attempt id")
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar05_reconciliations WHERE submission_id = $1 AND provider_state = 'RECEIVED' AND trigger = 'RETRY' AND method = 'QUERY_PROVIDER'`, s.SubmissionID))
	require.Equal(t, 1, e.events(s.SubmissionID, EventReconciled))
	require.Equal(t, "SUBMITTED", e.inv.current(e.doc.InvoiceID))

	// later the provider finishes: reconcile settles the final state
	e.tr.SetProviderState(att1, ProviderAccepted)
	code, b = e.cmd(pReconciler, "reconcileSubmissionState", e.body(rr.Submission, nil))
	require.Equal(t, http.StatusOK, code, string(b))
	fin := asResp(t, b)
	require.Equal(t, StateAccepted, fin.Submission.State)
	require.Equal(t, "ACCEPTED", e.inv.current(e.doc.InvoiceID))
	require.Equal(t, 1, e.events(s.SubmissionID, EventAccepted))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar05_acknowledgements WHERE submission_id = $1 AND source = 'RECONCILIATION' AND status = 'ACCEPTED' AND NOT signature_verified AND receipt_id IS NOT NULL`, s.SubmissionID))
	require.Equal(t, 1, e.tr.SubmitCount())
	require.Empty(t, e.inv.otherCalls())
}

func TestNeg16_ResubmitOnlyAfterProviderConfirmsNonReceipt(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	e.tr.Script(FakeSubmit{Err: errTimeout}) // timed out and the provider never got it
	code, b := e.submit(pSubmitter, s)
	require.Equal(t, http.StatusAccepted, code, string(b))
	cur := e.get(s.SubmissionID)
	require.Equal(t, StateUnknown, cur.State)
	att1 := e.attempts(s.SubmissionID)[0].ExternalAttemptID

	code, b = e.cmd(pSubmitter, "retrySubmission", e.body(cur, nil))
	require.Equal(t, http.StatusOK, code, string(b))
	r := asResp(t, b)
	require.Equal(t, StateSubmitted, r.Submission.State)
	require.Equal(t, 2, e.tr.SubmitCount(), "a new attempt exists only because the provider confirmed the first was not received")
	as := e.attempts(s.SubmissionID)
	require.Len(t, as, 2)
	require.Equal(t, att1, as[0].ExternalAttemptID)
	require.Equal(t, []string{OutAmbiguous, OutNotReceived, OutSuperseded}, outcomes(as[0]), "the first attempt is preserved, with its history")
	require.Equal(t, att1, as[1].ParentAttemptID, "the resubmission chain")
	require.NotEmpty(t, as[1].ReconciliationID, "and the reconciliation that licensed it")
	require.NotEqual(t, att1, as[1].ExternalAttemptID)
	require.Equal(t, 2, as[1].AttemptNo)
	require.Equal(t, OutAcknowledged, as[1].Outcome)
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar05_reconciliations WHERE submission_id = $1 AND provider_state = 'NOT_RECEIVED' AND trigger = 'RETRY'`, s.SubmissionID))
	require.Equal(t, []string{"PREPARED", "PENDING_UNKNOWN", "SUBMITTED"}, e.inv.states(e.doc.InvoiceID), "the dimension never flapped to a failed state")
}

func TestNeg16_RetryRefusedWhenTheProviderCannotSayWhatItHolds(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	e.tr.Script(FakeSubmit{Err: errTimeout})
	e.submit(pSubmitter, s)
	cur := e.get(s.SubmissionID)

	for name, setup := range map[string]func(){
		"provider answers UNKNOWN":    func() { e.tr.QueryAnswer = ProviderUnknown },
		"provider query itself fails": func() { e.tr.QueryAnswer = ""; e.tr.QueryErr = errTimeout },
	} {
		setup()
		code, b := e.cmd(pSubmitter, "retrySubmission", e.body(cur, nil))
		require.Equal(t, http.StatusConflict, code, name+" "+string(b))
		require.Equal(t, kit.CodeExternalStateUnknown, errCode(t, b), name)
		require.Equal(t, 1, e.tr.SubmitCount(), name+": nothing is re-sent on a guess")
		require.Equal(t, 1, e.attemptCount(s.SubmissionID), name)
		require.Equal(t, StateUnknown, e.get(s.SubmissionID).State, name)
	}
	require.Equal(t, 2, e.count(`SELECT count(*) FROM ar05_reconciliations WHERE submission_id = $1 AND provider_state IN ('UNKNOWN','ERROR')`, s.SubmissionID), "every unresolved reconciliation is evidence")
	cur = e.get(s.SubmissionID)

	// the provider recovers and confirms non-receipt: now (and only now) the retry proceeds
	e.tr.QueryErr, e.tr.QueryAnswer = nil, ""
	code, b := e.cmd(pSubmitter, "retrySubmission", e.body(cur, nil))
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, 2, e.tr.SubmitCount())
}

func TestNeg16_AttemptStillInFlightIsNeitherQueriedNorResent(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	// Simulate a crash between "attempt recorded" and "outcome recorded": PENDING_UNKNOWN with an attempt that has no result.
	att := uuid.NewString()
	require.NoError(t, e.exec(`UPDATE ar05_einvoice_submissions SET state = 'PENDING_UNKNOWN', attempt_count = 1, provider = 'acme', version = version + 1 WHERE submission_id = $1`, s.SubmissionID))
	require.NoError(t, e.exec(`INSERT INTO ar05_submission_attempts (external_attempt_id, tenant_id, legal_entity_id, submission_id, attempt_no, provider, artifact_hash, request_digest, started_by, started_at)
		VALUES ($1,$2,$3,$4,1,'acme',$5,$6,$7,$8)`, att, e.tenant, e.entity, s.SubmissionID, s.ArtifactHash, Sha256Hex([]byte("rd")), pSubmitter, e.clk.Now()))
	cur := e.get(s.SubmissionID)
	require.Equal(t, StateUnknown, cur.State)
	require.Equal(t, OutInFlight, e.attempts(s.SubmissionID)[0].Outcome)

	code, b := e.cmd(pSubmitter, "retrySubmission", e.body(cur, nil))
	require.Equal(t, http.StatusConflict, code, string(b))
	require.Equal(t, kit.CodeExternalStateUnknown, errCode(t, b))
	require.Empty(t, e.tr.Queries, "inside its call window the provider is not even asked (it could answer 'not received' while the request is on the wire)")
	require.Equal(t, 0, e.tr.SubmitCount())

	// once the call window has passed, the attempt is treated as crashed and reconciled
	e.clk.Advance(2*e.m.SubmitTimeout + time.Minute)
	code, b = e.cmd(pSubmitter, "retrySubmission", e.body(cur, nil))
	require.Equal(t, http.StatusOK, code, string(b))
	require.Len(t, e.tr.Queries, 1)
	require.Equal(t, 1, e.tr.SubmitCount())
	as := e.attempts(s.SubmissionID)
	require.Len(t, as, 2)
	require.Equal(t, att, as[1].ParentAttemptID)
}

func TestAR05Row1_ProviderTimeoutFollowedByDuplicateResubmission(t *testing.T) {
	// the scenario as the AR-05 table words it: concurrent resubmissions around a timeout never produce a second send
	e := newEnv(t)
	s := e.ready()
	e.tr.Script(FakeSubmit{Err: errTimeout, Delivered: true})
	code, _ := e.submit(pSubmitter, s)
	require.Equal(t, http.StatusAccepted, code)
	cur := e.get(s.SubmissionID)

	var wg sync.WaitGroup
	codes := make(chan int, 12)
	for i := 0; i < 6; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); c, _ := e.submit(pSubmitter, cur); codes <- c }()
		go func() { defer wg.Done(); c, _ := e.cmd(pSubmitter, "retrySubmission", e.body(cur, nil)); codes <- c }()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		require.Contains(t, []int{http.StatusOK, http.StatusConflict}, c)
	}
	require.Equal(t, 1, e.tr.SubmitCount(), "the provider received the document once")
	require.Equal(t, 1, e.attemptCount(s.SubmissionID))
	require.NotEqual(t, StateRejected, e.get(s.SubmissionID).State)
}

func TestDuplicateSubmissionSuppression_ConcurrentSubmits(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	e.tr.Script(FakeSubmit{Hook: func(SubmitRequest) { time.Sleep(200 * time.Millisecond) }})
	var wg sync.WaitGroup
	type res struct {
		code int
		r    respBody
	}
	out := make(chan res, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, b := e.submit(pSubmitter, s)
			var r respBody
			_ = json.Unmarshal(b, &r)
			out <- res{c, r}
		}()
	}
	wg.Wait()
	close(out)
	sent := 0
	for r := range out {
		require.Contains(t, []int{http.StatusOK, http.StatusConflict}, r.code)
		if r.code == http.StatusOK && r.r.Outcome == OutAcknowledged {
			sent++
		}
	}
	require.Equal(t, 1, sent)
	require.Equal(t, 1, e.tr.SubmitCount(), "eight concurrent submits, one transmission")
	require.Equal(t, 1, e.attemptCount(s.SubmissionID))
	require.Equal(t, StateSubmitted, e.get(s.SubmissionID).State)
}

// ── TestNeg15: forged callback ───────────────────────────────────────────────

func TestNeg15_ForgedCallbackIsRefusedAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	r := e.mustSubmit(s)
	att := r.ExternalAttemptID
	before := e.get(s.SubmissionID)
	dimBefore := e.inv.states(e.doc.InvoiceID)
	auditBefore := e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1`, e.tenant)
	outboxBefore := e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, e.tenant)

	forgedBody := e.cbBody(att, s, "ev-forged", "ACCEPTED", nil)
	raw, _ := json.Marshal(forgedBody)
	type attack struct {
		name   string
		opts   cbOpts
		body   map[string]any
		status int
		code   string
		reason string
	}
	attacks := []attack{
		{"signed with the attacker's own secret", cbOpts{secret: []byte("guess")}, forgedBody, 401, CodeCallbackUnauthenticated, "bad_signature"},
		{"body altered after signing (REJECTED -> ACCEPTED)", cbOpts{rawBody: mustJSON(e.cbBody(att, s, "ev-forged", "REJECTED", nil)), send: raw}, forgedBody, 401, CodeCallbackUnauthenticated, "bad_signature"},
		{"another provider's valid secret", cbOpts{provider: "evil", secret: []byte("evil-secret")}, forgedBody, 403, CodeCallbackMismatch, "provider_mismatch"},
		{"unknown provider", cbOpts{provider: "mallory"}, forgedBody, 401, CodeCallbackUnauthenticated, "no_secret_for_provider"},
		{"stale timestamp", cbOpts{ts: e.clk.Now().Add(-time.Hour)}, forgedBody, 401, CodeCallbackStale, "stale_timestamp"},
		{"future timestamp", cbOpts{ts: e.clk.Now().Add(time.Hour)}, forgedBody, 401, CodeCallbackStale, "stale_timestamp"},
	}
	for _, a := range attacks {
		code, b := e.callback(a.body, a.opts)
		require.Equal(t, a.status, code, a.name+" "+string(b))
		require.Equal(t, a.code, errCode(t, b), a.name)
		require.GreaterOrEqual(t, e.securityEvents(a.reason), 1, a.name+": recorded as a security event")
	}
	// an unsigned request at all
	req := httptest.NewRequest(http.MethodPost, CallbackPath, bytesReader(raw))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	after := e.get(s.SubmissionID)
	require.Equal(t, before.State, after.State, "a forged callback accepted nothing")
	require.Equal(t, before.Version, after.Version)
	require.Equal(t, StateSubmitted, after.State)
	require.Equal(t, 0, e.ackCount(s.SubmissionID), "and stored no acknowledgement")
	require.Equal(t, dimBefore, e.inv.states(e.doc.InvoiceID), "and did not touch the invoice's e-invoice dimension")
	require.Equal(t, outboxBefore, e.count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, e.tenant), "and emitted no event")
	require.Equal(t, 0, e.events(s.SubmissionID, EventAccepted))
	require.Greater(t, e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1`, e.tenant), auditBefore, "the attempt is in the tenant's audit trail")
	require.GreaterOrEqual(t, e.audits(s.SubmissionID, "recordProviderCallback.refused"), 1)
	require.Empty(t, e.inv.otherCalls())

	// the genuine callback, signed correctly, is accepted
	code, b := e.callback(e.cbBody(att, s, "ev-genuine", "ACCEPTED", nil), cbOpts{})
	require.Equal(t, http.StatusOK, code, string(b))
	fin := e.get(s.SubmissionID)
	require.Equal(t, StateAccepted, fin.State)
	require.Equal(t, "ACCEPTED", e.inv.current(e.doc.InvoiceID))
	require.Equal(t, 1, e.events(s.SubmissionID, EventAccepted))

	// signature-verification evidence is stored with the acknowledgement
	code, b = e.query("getAcknowledgement", "submission_id="+s.SubmissionID)
	require.Equal(t, http.StatusOK, code, string(b))
	var ack struct {
		Latest Ack `json:"latest"`
	}
	require.NoError(t, json.Unmarshal(b, &ack))
	require.True(t, ack.Latest.SignatureVerified)
	require.Equal(t, SigAlg, ack.Latest.SignatureAlg)
	require.Len(t, ack.Latest.SignatureDigest, 64)
	require.Len(t, ack.Latest.BodyDigest, 64)
	require.Equal(t, "CALLBACK", ack.Latest.Source)
	require.Equal(t, "APPLIED", ack.Latest.Disposition)
	require.Equal(t, "rcpt-ev-genuine", ack.Latest.ReceiptID)
	require.NotNil(t, ack.Latest.CallbackTimestamp)
	require.Len(t, e.reg.events, 1)
}

func TestNeg15_ReplayedMismatchedAndUnknownCallbacks(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	att := e.mustSubmit(s).ExternalAttemptID

	// a genuine PROCESSING report is applied once...
	code, b := e.callback(e.cbBody(att, s, "ev-1", "PROCESSING", nil), cbOpts{nonce: "nonce-A"})
	require.Equal(t, http.StatusOK, code, string(b))
	// ... its nonce cannot be reused with a different event (captured-and-replayed signature on a new body is impossible; the
	// same nonce on a genuinely signed new event is a replay)
	v := e.get(s.SubmissionID).Version
	code, b = e.callback(e.cbBody(att, s, "ev-2", "ACCEPTED", nil), cbOpts{nonce: "nonce-A"})
	require.Equal(t, http.StatusUnauthorized, code, string(b))
	require.Equal(t, CodeCallbackReplayed, errCode(t, b))
	require.Equal(t, v, e.get(s.SubmissionID).Version)
	require.Equal(t, StateSubmitted, e.get(s.SubmissionID).State)
	require.GreaterOrEqual(t, e.securityEvents("nonce_replayed"), 1)

	// valid signature, but the document is not the one the attempt carried
	code, b = e.callback(e.cbBody(att, s, "ev-3", "ACCEPTED", map[string]any{"document_id": uuid.NewString()}), cbOpts{})
	require.Equal(t, http.StatusUnprocessableEntity, code, string(b))
	require.Equal(t, CodeCallbackMismatch, errCode(t, b))
	require.Equal(t, StateSubmitted, e.get(s.SubmissionID).State)

	// valid signature, attempt that does not exist
	code, b = e.callback(e.cbBody(uuid.NewString(), s, "ev-4", "ACCEPTED", nil), cbOpts{})
	require.Equal(t, http.StatusNotFound, code, string(b))
	require.GreaterOrEqual(t, e.count(`SELECT count(*) FROM ar05_security_events WHERE reason = 'unknown_attempt'`), 1)

	// an attempt of ANOTHER submission cannot be used to move this one
	other := e.prepareFresh()
	other = e.validate(other)
	oatt := e.mustSubmit(other).ExternalAttemptID
	code, b = e.callback(e.cbBody(oatt, s, "ev-5", "ACCEPTED", nil), cbOpts{}) // document of s, attempt of other
	require.Equal(t, http.StatusUnprocessableEntity, code, string(b))
	require.Equal(t, StateSubmitted, e.get(s.SubmissionID).State)
	require.Equal(t, StateSubmitted, e.get(other.SubmissionID).State)
}

func TestNeg15_DuplicateLateAndReceiptlessCallbacks(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	att := e.mustSubmit(s).ExternalAttemptID

	// ACCEPTED with no receipt is not recorded as accepted
	code, b := e.callback(e.cbBody(att, s, "ev-norcpt", "ACCEPTED", map[string]any{"receipt_id": "", "provider_reference": ""}), cbOpts{})
	require.Equal(t, http.StatusOK, code, string(b))
	require.Contains(t, string(b), `"disposition":"MISSING_RECEIPT"`)
	require.Equal(t, StateSubmitted, e.get(s.SubmissionID).State)
	require.Equal(t, 1, e.ackCount(s.SubmissionID), "the authentic report is stored as evidence, without effect")

	code, b = e.callback(e.cbBody(att, s, "ev-ok", "ACCEPTED", nil), cbOpts{})
	require.Equal(t, http.StatusOK, code, string(b))
	acc := e.get(s.SubmissionID)
	require.Equal(t, StateAccepted, acc.State)

	// the same event delivered again (new nonce): idempotent, nothing changes
	code, b = e.callback(e.cbBody(att, s, "ev-ok", "ACCEPTED", nil), cbOpts{})
	require.Equal(t, http.StatusOK, code, string(b))
	require.Contains(t, string(b), `"duplicate":true`)
	require.Equal(t, acc.Version, e.get(s.SubmissionID).Version)
	require.Equal(t, 2, e.ackCount(s.SubmissionID))
	require.Equal(t, 1, e.events(s.SubmissionID, EventAccepted))

	// a late, conflicting REJECTED after the final ACCEPTED cannot regress it
	code, b = e.callback(e.cbBody(att, s, "ev-late", "REJECTED", nil), cbOpts{})
	require.Equal(t, http.StatusOK, code, string(b))
	require.Contains(t, string(b), `"disposition":"AFTER_TERMINAL"`)
	require.Equal(t, StateAccepted, e.get(s.SubmissionID).State)
	require.Equal(t, "ACCEPTED", e.inv.current(e.doc.InvoiceID))
	require.Equal(t, 3, e.ackCount(s.SubmissionID))
}

func TestNeg15_CallbackNeedsNoPrincipalNorTenantHeader(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	att := e.mustSubmit(s).ExternalAttemptID
	// e.callback sends neither X-Tenant-Id nor X-Principal-Id nor Idempotency-Key: tenant comes from the attempt the signed body names
	code, b := e.callback(e.cbBody(att, s, "ev-x", "ACCEPTED", nil), cbOpts{})
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, StateAccepted, e.get(s.SubmissionID).State)
	var actor string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT actor_id FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = 'recordProviderCallback'`, e.tenant, s.SubmissionID).Scan(&actor))
	require.Equal(t, "provider:acme", actor, "the audit actor is the authenticated provider")
}

// ── AR-05 row 2: a rejected e-invoice never mutates the source invoice ───────

func TestAR05Row2_RejectedEInvoiceLeavesTheIssuedInvoiceUntouched(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	att := e.mustSubmit(s).ExternalAttemptID
	code, b := e.callback(e.cbBody(att, s, "ev-rej", "REJECTED", map[string]any{"reason_code": "BR-CO-15", "message": "totals mismatch"}), cbOpts{})
	require.Equal(t, http.StatusOK, code, string(b))

	rej := e.get(s.SubmissionID)
	require.Equal(t, StateRejected, rej.State)
	require.Equal(t, "REJECTED", e.inv.current(e.doc.InvoiceID), "ONLY the e-invoice dimension records the rejection")
	require.Equal(t, []string{"PREPARED", "PENDING_UNKNOWN", "SUBMITTED", "REJECTED"}, e.inv.states(e.doc.InvoiceID))
	require.Empty(t, e.inv.otherCalls(), "no correction, no settlement, no dispute, no lifecycle call: the invoice owner is only told the dimension")
	e.inv.issuedUntouched(t, e.tenant, e.doc.InvoiceID)
	require.Equal(t, 1, e.events(s.SubmissionID, EventRejected))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar05_acknowledgements WHERE submission_id = $1 AND status = 'REJECTED' AND reason_code = 'BR-CO-15' AND signature_verified`, s.SubmissionID))
	require.Equal(t, 0, e.count(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1`, e.tenant), "no financial consequence")

	// the rejected submission is final; the only way forward is a chained resubmission
	code, b = e.submit(pSubmitter, rej)
	require.Equal(t, http.StatusConflict, code, string(b))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, b))
	requireRefused(t, e.exec(`UPDATE ar05_einvoice_submissions SET state = 'SUBMITTED' WHERE submission_id = $1`, s.SubmissionID), "P0001", "AR05_TERMINAL_STATE")
}

// ── manual reconciliation (controlled admin evidence path) ───────────────────

func TestReconcile_ManualOverrideOnlyThroughTheControlledAdminPath(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	e.tr.Script(FakeSubmit{Err: errTimeout})
	code, _ := e.submit(pAdmin2, s) // pAdmin2 starts the attempt
	require.Equal(t, http.StatusAccepted, code)
	cur := e.get(s.SubmissionID)
	require.Equal(t, StateUnknown, cur.State)

	adminBody := func(extra map[string]any) map[string]any {
		b := map[string]any{"method": MethodAdmin, "resolved_state": "ACCEPTED", "receipt_id": "PORTAL-RCPT-9", "reason_code": "PROVIDER_PORTAL_CONFIRMED", "evidence_id": "EV-2026-0042"}
		for k, v := range extra {
			b[k] = v
		}
		return e.body(cur, b)
	}
	// without the privileged action
	for _, who := range []string{pSubmitter, pReconciler, pNobody} {
		code, b := e.cmd(who, "reconcileSubmissionState", adminBody(nil))
		require.Equal(t, http.StatusForbidden, code, who+" "+string(b))
	}
	// segregation of duties: whoever started the attempt cannot resolve it by hand
	code, b := e.cmd(pAdmin2, "reconcileSubmissionState", adminBody(nil))
	require.Equal(t, http.StatusForbidden, code, string(b))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, b))
	require.Equal(t, 1, e.audits(s.SubmissionID, "reconcileSubmissionState.denied"))
	require.Equal(t, StateUnknown, e.get(s.SubmissionID).State)
	require.Equal(t, 0, e.count(`SELECT count(*) FROM ar05_reconciliations WHERE submission_id = $1`, s.SubmissionID))

	// reason / evidence / receipt are mandatory
	code, b = e.cmd(pAdmin, "reconcileSubmissionState", adminBody(map[string]any{"evidence_id": ""}))
	require.Equal(t, kit.CodeEvidenceRequired, errCode(t, b))
	code, b = e.cmd(pAdmin, "reconcileSubmissionState", adminBody(map[string]any{"reason_code": ""}))
	require.Equal(t, kit.CodeReasonRequired, errCode(t, b))
	code, b = e.cmd(pAdmin, "reconcileSubmissionState", adminBody(map[string]any{"receipt_id": ""}))
	require.Equal(t, http.StatusBadRequest, code, string(b))

	// stale version
	code, b = e.cmd(pAdmin, "reconcileSubmissionState", e.body(s, map[string]any{"method": MethodAdmin, "resolved_state": "ACCEPTED", "receipt_id": "R", "reason_code": "X", "evidence_id": "E"}))
	require.Equal(t, http.StatusConflict, code, string(b))
	require.Equal(t, kit.CodeStaleVersion, errCode(t, b))

	// the controlled path works and records its evidence
	code, b = e.cmd(pAdmin, "reconcileSubmissionState", adminBody(nil))
	require.Equal(t, http.StatusOK, code, string(b))
	r := asResp(t, b)
	require.Equal(t, StateAccepted, r.Submission.State)
	require.NotNil(t, r.Reconciliation)
	require.Equal(t, MethodAdmin, r.Reconciliation.Method)
	require.Equal(t, "EV-2026-0042", r.Reconciliation.EvidenceID)
	require.Equal(t, "PROVIDER_PORTAL_CONFIRMED", r.Reconciliation.ReasonCode)
	require.Equal(t, "PENDING_UNKNOWN", r.Reconciliation.StateBefore)
	require.Equal(t, "ACCEPTED", r.Reconciliation.StateAfter)
	require.Equal(t, "ACCEPTED", e.inv.current(e.doc.InvoiceID))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar05_acknowledgements WHERE submission_id = $1 AND source = 'ADMIN' AND receipt_id = 'PORTAL-RCPT-9' AND NOT signature_verified`, s.SubmissionID))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar_audit_events WHERE tenant_id = $1 AND aggregate_id = $2 AND command = 'reconcileSubmissionState' AND evidence_id = 'EV-2026-0042' AND reason_code = 'PROVIDER_PORTAL_CONFIRMED'`, e.tenant, s.SubmissionID))
	as := e.attempts(s.SubmissionID)
	require.Equal(t, OutAdminAccept, as[0].Outcome)
	require.Equal(t, 1, e.events(s.SubmissionID, EventReconciled))

	// a final submission cannot be overridden again
	code, b = e.cmd(pAdmin, "reconcileSubmissionState", adminBody(map[string]any{"resolved_state": "REJECTED"}))
	require.Equal(t, http.StatusConflict, code)
	_ = b

	// the database refuses an evidence-less admin reconciliation outright
	requireRefused(t, e.exec(`INSERT INTO ar05_reconciliations (reconciliation_id, tenant_id, legal_entity_id, submission_id, method, trigger, provider_state, state_before, state_after, performed_by, performed_at)
		VALUES (gen_random_uuid(), $1, $2, $3, 'ADMIN_EVIDENCE', 'RECONCILE', 'ACCEPTED', 'PENDING_UNKNOWN', 'ACCEPTED', 'x', now())`, e.tenant, e.entity, s.SubmissionID), "23514", "ar05_admin_needs_evidence")
}

func TestReconcile_QueryAdminNotReceivedKeepsStateAndLicensesRetry(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	e.tr.Script(FakeSubmit{Err: errTimeout})
	e.submit(pSubmitter, s)
	cur := e.get(s.SubmissionID)
	code, b := e.cmd(pReconciler, "reconcileSubmissionState", e.body(cur, nil))
	require.Equal(t, http.StatusOK, code, string(b))
	r := asResp(t, b)
	require.Equal(t, StateUnknown, r.Submission.State, "NOT_RECEIVED does not move the state; retry follows")
	require.Equal(t, ProviderNotReceived, r.Reconciliation.ProviderState)
	require.Equal(t, OutNotReceived, e.attempts(s.SubmissionID)[0].Outcome)
	require.Equal(t, cur.Version, r.Submission.Version, "no state change, no version bump; the evidence rows are appended")
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar05_reconciliations WHERE submission_id = $1`, s.SubmissionID))
	require.Equal(t, 1, e.audits(s.SubmissionID, "reconcileSubmissionState"))

	// the reconciler (who has no submit permission) cannot send; the submitter can retry
	code, _ = e.cmd(pReconciler, "retrySubmission", e.body(r.Submission, nil))
	require.Equal(t, http.StatusForbidden, code)
	code, b = e.cmd(pSubmitter, "retrySubmission", e.body(r.Submission, nil))
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, 2, e.tr.SubmitCount())
}

// ── cancel ───────────────────────────────────────────────────────────────────

func TestCancel_OnlyBeforeTransmission(t *testing.T) {
	e := newEnv(t)
	s := e.prepare()
	code, b := e.cmd(pSubmitter, "cancelSubmissionWherePermitted", e.body(s, nil))
	require.Equal(t, kit.CodeReasonRequired, errCode(t, b), string(b))
	code, b = e.cmd(pSubmitter, "cancelSubmissionWherePermitted", e.body(s, map[string]any{"reason_code": "WRONG_PROFILE_CHOSEN"}))
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, StateCancelled, asResp(t, b).Submission.State)
	require.Equal(t, "REQUIRED", e.inv.current(e.doc.InvoiceID), "the document still needs an e-invoice")
	require.Equal(t, 0, e.tr.SubmitCount())

	// a cancelled submission does not block a fresh one
	s2 := e.prepare()
	require.NotEqual(t, s.SubmissionID, s2.SubmissionID)
	require.Equal(t, "", s2.ParentSubmissionID, "cancelled submissions do not chain (only rejected ones do)")
	s2 = e.validate(s2)
	e.mustSubmit(s2)
	cur := e.get(s2.SubmissionID)
	code, b = e.cmd(pSubmitter, "cancelSubmissionWherePermitted", e.body(cur, map[string]any{"reason_code": "TOO_LATE"}))
	require.Equal(t, http.StatusConflict, code, string(b))
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, b), "after transmission a correction document applies, not cancellation")

	// not required any more once no profile is in force
	n := newEnv(t)
	ns := n.prepare()
	n.prof.setErr(ErrNoProfile)
	code, b = n.cmd(pSubmitter, "cancelSubmissionWherePermitted", n.body(ns, map[string]any{"reason_code": "NOT_NEEDED"}))
	require.Equal(t, http.StatusOK, code, string(b))
	require.Equal(t, "NOT_REQUIRED", n.inv.current(n.doc.InvoiceID))
}

// ── authorization, concurrency, isolation ─────────────────────────────────────

func TestCommandsAreAuthorizedPerAction(t *testing.T) {
	e := newEnv(t)
	s := e.prepare()
	for _, name := range []string{"validateEInvoice", "submitEInvoice", "retrySubmission"} {
		for _, who := range []string{pNobody, pReconciler} {
			code, b := e.cmd(who, name, e.body(s, nil))
			require.Equal(t, http.StatusForbidden, code, name+" "+who+" "+string(b))
		}
	}
	code, _ := e.cmd(pSubmitter, "reconcileSubmissionState", e.body(s, nil))
	require.Equal(t, http.StatusForbidden, code, "submitting does not include reconciling")
	code, _ = e.cmd(pNobody, "prepareEInvoice", map[string]any{"document_id": e.doc.InvoiceID, "jurisdiction_id": "GB"})
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, 0, e.tr.SubmitCount())
}

func TestOptimisticConcurrency(t *testing.T) {
	e := newEnv(t)
	s := e.prepare()
	code, b := e.cmd(pSubmitter, "validateEInvoice", map[string]any{"submission_id": s.SubmissionID})
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, kit.CodeExpectedVersionMissing, errCode(t, b))
	code, b = e.cmd(pSubmitter, "validateEInvoice", map[string]any{"submission_id": s.SubmissionID, "expected_version": 99})
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, kit.CodeStaleVersion, errCode(t, b))
	v := e.validate(s)
	code, b = e.cmd(pSubmitter, "validateEInvoice", e.body(s, nil)) // old version
	require.Equal(t, http.StatusConflict, code, string(b))
	require.Equal(t, kit.CodeStaleVersion, errCode(t, b))
	require.Equal(t, v.Version, e.get(s.SubmissionID).Version)

	// a stale body entity is refused too
	code, b = e.cmd(pSubmitter, "submitEInvoice", e.body(v, map[string]any{"legal_entity_id": uuid.NewString()}))
	require.Equal(t, http.StatusForbidden, code, string(b))
	require.Equal(t, kit.CodeTenantScope, errCode(t, b))
}

func TestTenantIsolation(t *testing.T) {
	a, b := newEnv(t), newEnv(t)
	s := a.ready()
	att := a.mustSubmit(s).ExternalAttemptID
	foreign := func(method, path string, body map[string]any) int {
		code, _, _ := call(t, a.h, method, path, b.tenant, pSubmitter, "", body)
		return code
	}
	require.Equal(t, http.StatusNotFound, foreign(http.MethodGet, base+"getEInvoiceStatus?submission_id="+s.SubmissionID, nil))
	require.Equal(t, http.StatusNotFound, foreign(http.MethodGet, base+"getSubmissionAttempt?external_attempt_id="+att, nil))
	require.Equal(t, http.StatusNotFound, foreign(http.MethodGet, base+"getValidationErrors?submission_id="+s.SubmissionID, nil))
	require.Equal(t, http.StatusNotFound, foreign(http.MethodGet, base+"getAcknowledgement?submission_id="+s.SubmissionID, nil))
	require.Equal(t, http.StatusNotFound, foreign(http.MethodGet, base+"getEInvoiceStatus?document_id="+a.doc.InvoiceID, nil))
	require.Equal(t, http.StatusNotFound, foreign(http.MethodPost, base+"commands/validateEInvoice", map[string]any{"submission_id": s.SubmissionID, "expected_version": 1}))
	require.Equal(t, http.StatusNotFound, foreign(http.MethodPost, base+"commands/prepareEInvoice", map[string]any{"document_id": a.doc.InvoiceID, "jurisdiction_id": "GB"}))
	code, hb := b.query("listSubmissionHistory", "document_id="+a.doc.InvoiceID)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(hb), `"submissions":[]`)
	require.Equal(t, 0, b.subCount())
	require.Equal(t, StateSubmitted, a.get(s.SubmissionID).State)
}

// ── queries ──────────────────────────────────────────────────────────────────

func TestQueries(t *testing.T) {
	e := newEnv(t)
	s := e.prepare()

	code, _ := e.query("getValidationErrors", "submission_id="+s.SubmissionID)
	require.Equal(t, http.StatusNotFound, code, "not validated yet")
	code, _ = e.query("getAcknowledgement", "submission_id="+s.SubmissionID)
	require.Equal(t, http.StatusNotFound, code, "no acknowledgement yet")
	code, _ = e.query("getEInvoiceStatus", "")
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = e.query("getEInvoiceStatus", "submission_id=not-a-uuid")
	require.Equal(t, http.StatusNotFound, code)

	s = e.validate(s)
	att := e.mustSubmit(s).ExternalAttemptID
	e.callback(e.cbBody(att, s, "ev-p", "PROCESSING", nil), cbOpts{})

	// by submission id and by document id (POST body also accepted)
	code, b := e.query("getEInvoiceStatus", "document_id="+e.doc.InvoiceID)
	require.Equal(t, http.StatusOK, code, string(b))
	var st struct {
		Submission *Submission    `json:"submission"`
		Actions    []string       `json:"available_actions"`
		Latest     map[string]any `json:"latest_attempt"`
		Dimension  string         `json:"einvoice_dimension"`
	}
	require.NoError(t, json.Unmarshal(b, &st))
	require.Equal(t, s.SubmissionID, st.Submission.SubmissionID)
	require.Equal(t, []string{"reconcileSubmissionState"}, st.Actions, "available actions are derived from the state")
	require.Equal(t, "SUBMITTED", st.Dimension)
	pc, pb, _ := call(t, e.h, http.MethodPost, base+"getEInvoiceStatus", e.tenant, pNobody, "", map[string]any{"submission_id": s.SubmissionID})
	require.Equal(t, http.StatusOK, pc, string(pb))

	code, b = e.query("getSubmissionAttempt", "external_attempt_id="+att)
	require.Equal(t, http.StatusOK, code, string(b))
	var av attemptView
	require.NoError(t, json.Unmarshal(b, &av))
	require.Equal(t, att, av.ExternalAttemptID)
	require.Equal(t, []string{OutAcknowledged}, outcomes(av))

	code, b = e.query("getAcknowledgement", "submission_id="+s.SubmissionID)
	require.Equal(t, http.StatusOK, code, string(b))
	require.Contains(t, string(b), `"status":"PROCESSING"`)

	code, b = e.query("listSubmissionHistory", "submission_id="+s.SubmissionID)
	require.Equal(t, http.StatusOK, code, string(b))
	var h struct {
		History         []HistoryEntry   `json:"history"`
		Attempts        []attemptView    `json:"attempts"`
		Acks            []Ack            `json:"acknowledgements"`
		Reconciliations []Reconciliation `json:"reconciliations"`
	}
	require.NoError(t, json.Unmarshal(b, &h))
	var cmds []string
	for _, x := range h.History {
		cmds = append(cmds, x.Command)
	}
	require.Equal(t, []string{"prepareEInvoice", "validateEInvoice", "submitEInvoice", "attemptOutcome"}, cmds)
	require.Len(t, h.Attempts, 1)
	require.Len(t, h.Acks, 1)
	require.Empty(t, h.Reconciliations)

	code, _ = e.query("listSubmissionHistory", "document_id="+e.doc.InvoiceID+"&limit=0")
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = e.query("listSubmissionHistory", "document_id="+e.doc.InvoiceID+"&limit=9999")
	require.Equal(t, http.StatusOK, code, "the limit is capped, not refused")
}

// ── append-only evidence ─────────────────────────────────────────────────────

func TestEvidenceIsAppendOnly(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	e.tr.Script(FakeSubmit{Err: errTimeout})
	e.submit(pSubmitter, s)
	att := e.attempts(s.SubmissionID)[0].ExternalAttemptID
	cur := e.get(s.SubmissionID)
	e.cmd(pReconciler, "reconcileSubmissionState", e.body(cur, nil))
	e.callback(e.cbBody(att, s, "ev-ap", "PROCESSING", nil), cbOpts{})
	e.callback(e.cbBody(att, s, "ev-ap2", "ACCEPTED", nil), cbOpts{provider: "acme", secret: []byte("bad")}) // security event row

	for _, tbl := range []string{"ar05_artifacts", "ar05_validation_results", "ar05_submission_attempts", "ar05_attempt_results", "ar05_attempt_index",
		"ar05_acknowledgements", "ar05_callback_nonces", "ar05_reconciliations", "ar05_submission_history", "ar05_security_events"} {
		require.GreaterOrEqual(t, e.count(`SELECT count(*) FROM `+tbl), 1, tbl+" has evidence")
		requireRefused(t, e.exec(`DELETE FROM `+tbl), "P0001", "append-only")
	}
	requireRefused(t, e.exec(`UPDATE ar05_submission_attempts SET provider = 'x'`), "P0001", "append-only")
	requireRefused(t, e.exec(`UPDATE ar05_attempt_results SET outcome = 'ACCEPTED'`), "P0001", "append-only")
	requireRefused(t, e.exec(`UPDATE ar05_acknowledgements SET status = 'ACCEPTED'`), "P0001", "append-only")
	requireRefused(t, e.exec(`UPDATE ar05_validation_results SET passed = false`), "P0001", "append-only")
	requireRefused(t, e.exec(`UPDATE ar05_artifacts SET content = 'x'::bytea`), "P0001", "append-only")
	requireRefused(t, e.exec(`DELETE FROM ar05_einvoice_submissions`), "P0001", "AR05_SUBMISSION_NOT_DELETABLE")
	requireRefused(t, e.exec(`UPDATE ar05_einvoice_submissions SET content_hash = 'tampered' WHERE submission_id = $1`, s.SubmissionID), "P0001", "AR05_SUBMISSION_IMMUTABLE")
	requireRefused(t, e.exec(`UPDATE ar05_einvoice_submissions SET artifact_hash = $2 WHERE submission_id = $1`, s.SubmissionID, Sha256Hex([]byte("other"))), "P0001", "AR05_SUBMISSION_IMMUTABLE")
	requireRefused(t, e.exec(`UPDATE ar05_einvoice_submissions SET state = 'BOGUS' WHERE submission_id = $1`, s.SubmissionID), "23514", "ar05_state_valid")

	// an unsigned CALLBACK acknowledgement cannot be stored at all
	requireRefused(t, e.exec(`INSERT INTO ar05_acknowledgements (ack_id, tenant_id, legal_entity_id, submission_id, external_attempt_id, provider, provider_event_id, status, occurred_at,
			received_at, source, signature_verified, disposition, state_after, recorded_by)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, 'acme', 'forged', 'ACCEPTED', now(), now(), 'CALLBACK', false, 'APPLIED', 'ACCEPTED', 'x')`, e.tenant, e.entity, s.SubmissionID, att),
		"23514", "ar05_callback_is_signed")
	// a retry attempt must name the reconciliation that licensed it
	requireRefused(t, e.exec(`INSERT INTO ar05_submission_attempts (external_attempt_id, tenant_id, legal_entity_id, submission_id, attempt_no, provider, artifact_hash, request_digest, parent_attempt_id, started_by, started_at)
		VALUES (gen_random_uuid(), $1, $2, $3, 9, 'acme', 'h', $4, $5, 'x', now())`, e.tenant, e.entity, s.SubmissionID, Sha256Hex([]byte("z")), att), "23514", "ar05_retry_follows_reconciliation")
	// one active submission per (document, profile)
	requireRefused(t, e.exec(`INSERT INTO ar05_einvoice_submissions (submission_id, tenant_id, legal_entity_id, document_type, document_id, content_hash, jurisdiction_code, profile_code,
			profile_version, network, syntax, transmission_mode, profile_snapshot, artifact_hash, artifact_ref, prepared_by)
		VALUES (gen_random_uuid(), $1, $2, 'INVOICE', $3, 'h', 'GB', 'PEPPOL_INVOICE', '1.0', 'PEPPOL', 'JSON', 'EXCHANGE', '{}', $4, 'r', 'x')`, e.tenant, e.entity, e.doc.InvoiceID, Sha256Hex([]byte("q"))),
		"23505", "uq_ar05_active_submission")
}

// ── events: the catalogue, and no accounting consequence ─────────────────────

func TestEventsAndNoPostings(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	att := e.mustSubmit(s).ExternalAttemptID
	e.callback(e.cbBody(att, s, "ev-fin", "ACCEPTED", nil), cbOpts{})
	for _, ev := range []string{EventPrepared, EventSubmitted, EventAccepted} {
		require.Equal(t, 1, e.events(s.SubmissionID, ev), ev)
	}
	require.Equal(t, 0, e.events(s.SubmissionID, EventRejected))
	require.Equal(t, 0, e.count(`SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1`, e.tenant), "AR-05 queues no posting")
	var payload string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT payload::text FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3`, e.tenant, s.SubmissionID, EventAccepted).Scan(&payload))
	require.Contains(t, payload, s.ArtifactHash)
	require.Contains(t, payload, e.doc.ContentHash)
	require.Contains(t, payload, att)
}

// compile-time check that the integration helpers use the same kit types the production code does.
var _ = pgx.ErrNoRows

// The four rows of the AR-05 negative-path table.
func TestAR05Row3_ForgedCallbackAccepted(t *testing.T) {
	TestNeg15_ForgedCallbackIsRefusedAndChangesNothing(t)
}
func TestAR05Row4_WrongJurisdictionSyntaxOrProfile(t *testing.T) {
	TestNeg14_WrongJurisdictionOrProfileIsRefused(t)
}
