package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/adapter/ext"
	"zoiko.io/tax-authority-interface-svc/internal/authority"
)

// ── the authority simulator ─────────────────────────────────────────────────

type sim struct {
	srv       *httptest.Server
	mu        sync.Mutex
	committed map[string]string // submission ref -> receipt id
	status    map[string]string // submission ref -> authority status
	submits   int
	queries   int
	bodies    []string
	next      []string
	hang      chan struct{}
	wrongEcho bool
}

func newSim(t *testing.T) *sim {
	s := &sim{committed: map[string]string{}, status: map[string]string{}, hang: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(func() { close(s.hang); s.srv.Close() })
	return s
}

func (s *sim) script(m ...string) { s.mu.Lock(); s.next = append(s.next, m...); s.mu.Unlock() }
func (s *sim) hits() (int, int)   { s.mu.Lock(); defer s.mu.Unlock(); return s.submits, s.queries }
func (s *sim) setStatus(ref, st string) {
	s.mu.Lock()
	s.status[ref] = st
	s.mu.Unlock()
}

func (s *sim) reply(w http.ResponseWriter, ref, receipt, status string) {
	echo := ref
	if s.wrongEcho {
		echo = "someone-elses-filing"
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"receipt_id": receipt, "status": status, "submission_ref": echo})
}

func (s *sim) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if r.Method == http.MethodGet {
		s.queries++
		ref := strings.TrimPrefix(r.URL.Path, "/v1/returns/by-ref/")
		rc, ok := s.committed[ref]
		st := s.status[ref]
		if st == "" {
			st = "RECEIVED"
		}
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		s.reply(w, ref, rc, st)
		return
	}
	s.submits++
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	raw, _ := json.Marshal(body)
	s.bodies = append(s.bodies, string(raw))
	ref, _ := body["submission_ref"].(string)
	mode := "ok"
	if len(s.next) > 0 {
		mode, s.next = s.next[0], s.next[1:]
	}
	commit := func() string {
		if rc, ok := s.committed[ref]; ok {
			return rc
		}
		rc := "RC-" + ref
		s.committed[ref] = rc
		return rc
	}
	switch mode {
	case "ok":
		rc := commit()
		s.mu.Unlock()
		s.reply(w, ref, rc, "RECEIVED")
	case "commit-then-hang":
		commit()
		s.mu.Unlock()
		<-s.hang
	case "drop":
		s.mu.Unlock()
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	case "500":
		s.mu.Unlock()
		w.WriteHeader(500)
	case "reject":
		s.mu.Unlock()
		s.reply(w, ref, "RC-R", "REJECTED")
	default:
		s.mu.Unlock()
		w.WriteHeader(500)
	}
}

// ── fakes ───────────────────────────────────────────────────────────────────

type fakeStore struct {
	mu        sync.Mutex
	now       func() time.Time
	profiles  []*authority.ProfileRecord
	f         map[string]*authority.Filing
	allLog    []authority.TransportEntry
	escalated map[string]int
	blocked   map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{now: func() time.Time { return time.Now().UTC() }, f: map[string]*authority.Filing{}, escalated: map[string]int{}, blocked: map[string]string{}}
}

func (s *fakeStore) PutProfile(_ context.Context, key string, spec adapter.Spec, by string) (*authority.ProfileRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &authority.ProfileRecord{ProviderKey: key, Version: len(s.profiles) + 1, Status: authority.ProfileCertified, Spec: spec.Normalize(), CreatedBy: by}
	s.profiles = append(s.profiles, p)
	return p, nil
}
func (s *fakeStore) GetProfile(_ context.Context, key string, version int) (*authority.ProfileRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.profiles) - 1; i >= 0; i-- {
		p := s.profiles[i]
		if p.ProviderKey == key && ((version == 0 && p.Status == authority.ProfileCertified) || p.Version == version) {
			c := *p
			return &c, nil
		}
	}
	return nil, authority.ErrProfileNotFound
}
func (s *fakeStore) CertifyProfile(context.Context, string, int, string) (*authority.ProfileRecord, error) {
	return nil, errors.New("unused")
}
func (s *fakeStore) SetProfileStatus(context.Context, string, int, string) (*authority.ProfileRecord, error) {
	return nil, errors.New("unused")
}
func (s *fakeStore) ListProfiles(context.Context, string) ([]authority.ProfileRecord, error) {
	return nil, nil
}
func (s *fakeStore) CreateFiling(context.Context, authority.CreateFiling) (*authority.Filing, bool, error) {
	return nil, false, errors.New("unused")
}
func (s *fakeStore) LoadFiling(_ context.Context, id string) (*authority.Filing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.f[id]
	if !ok {
		return nil, authority.ErrFilingNotFound
	}
	c := *f
	return &c, nil
}
func (s *fakeStore) MarkValidated(_ context.Context, id, _ string) (*authority.Filing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.f[id]
	if f.Status != authority.StatusPrepared {
		return nil, authority.ErrInFlight
	}
	now := s.now()
	f.Status, f.ValidatedAt = authority.StatusValidated, &now
	c := *f
	return &c, nil
}
func (s *fakeStore) Block(_ context.Context, id, reason, _, _ string) (*authority.Filing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.f[id]
	f.Status, f.LastFailure = authority.StatusBlocked, reason
	s.blocked[id] = reason
	c := *f
	return &c, nil
}
func (s *fakeStore) BeginSend(_ context.Context, id, from string, ver int, hash, _ string) (*authority.Filing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.f[id]
	switch {
	case f.Status != from:
		return nil, authority.ErrInFlight
	case f.NotBefore != nil && f.NotBefore.After(s.now()):
		return nil, authority.ErrNotBefore
	case f.SentPayloadHash != "" && f.SentPayloadHash != hash, f.ProfileVersion != nil && *f.ProfileVersion != ver:
		return nil, authority.ErrPayloadChanged
	}
	now := s.now()
	f.Status, f.InFlightSince, f.SendCount, f.NotBefore = authority.StatusSubmitting, &now, f.SendCount+1, nil
	f.ProfileVersion, f.SentPayloadHash = &ver, hash
	c := *f
	return &c, nil
}
func (s *fakeStore) Finish(_ context.Context, id string, o authority.Outcome, actor, _ string) (*authority.Filing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.f[id]
	if f.Status != o.From {
		return nil, authority.ErrInFlight
	}
	o.Transport.SubmissionID, o.Transport.Actor = id, actor
	s.allLog = append(s.allLog, o.Transport)
	switch o.Kind {
	case authority.OutSubmitted:
		f.Status, f.AuthorityRef, f.AuthorityStatus, f.InFlightSince, f.NextPollAt = authority.StatusSubmitted, o.AuthorityRef, o.Authority, nil, o.NextPoll
	case authority.OutAccepted:
		f.Status, f.AuthorityRef, f.AuthorityStatus, f.InFlightSince, f.NextPollAt = authority.StatusAccepted, o.AuthorityRef, o.Authority, nil, nil
	case authority.OutRejected:
		f.Status, f.AuthorityStatus, f.InFlightSince, f.LastFailure = authority.StatusRejected, o.Authority, nil, o.Reason
	case authority.OutUnknown:
		f.Status, f.InFlightSince, f.LastFailure, f.NextPollAt = authority.StatusPendingUnknown, nil, o.Failure, o.NextPoll
	case authority.OutRelease:
		f.Status, f.InFlightSince, f.NotBefore, f.LastFailure = o.ReleaseTo, nil, o.NotBefore, o.Failure
	case authority.OutNone:
		f.LastFailure = o.Failure
		if o.NextPoll != nil {
			f.NextPollAt = o.NextPoll
		}
	}
	c := *f
	return &c, nil
}
func (s *fakeStore) Escalate(_ context.Context, id, _, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.f[id]
	if f.EscalatedAt == nil {
		now := s.now()
		f.EscalatedAt = &now
		s.escalated[id]++
	}
	return nil
}
func (s *fakeStore) ListTransport(_ context.Context, id string) ([]authority.TransportEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []authority.TransportEntry
	for _, e := range s.allLog {
		if e.SubmissionID == id {
			out = append(out, e)
		}
	}
	return out, nil
}
func (s *fakeStore) RecentCalls(_ context.Context, key string, _ time.Time, limit int) ([]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []bool
	for i := len(s.allLog) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.allLog[i]
		if e.ProviderKey == key && e.Operation == "SUBMIT" {
			out = append(out, e.Disposition == "ANSWERED" && (e.Class == "ACKNOWLEDGED" || e.Class == "ACCEPTED" || e.Class == "REJECTED"))
		}
	}
	return out, nil
}
func (s *fakeStore) SweepStale(_ context.Context, before time.Time, _ string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.f {
		if f.Status == authority.StatusSubmitting && f.InFlightSince.Before(before) {
			f.Status, f.InFlightSince = authority.StatusPendingUnknown, nil
			n++
		}
	}
	return n, nil
}
func (s *fakeStore) DuePolls(_ context.Context, now time.Time, _ int) ([]authority.Due, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []authority.Due
	for _, f := range s.f {
		if (f.Status == authority.StatusSubmitted || f.Status == authority.StatusPendingUnknown) && f.NextPollAt != nil && !f.NextPollAt.After(now) {
			out = append(out, authority.Due{SubmissionID: f.SubmissionID, TenantID: f.TenantID})
		}
	}
	return out, nil
}

type fakeConns struct {
	c   ext.Connector
	err error
}

func (f *fakeConns) Resolve(context.Context, string, ext.Caller) (ext.Connector, error) {
	return f.c, f.err
}

type fakeCreds struct {
	err   error
	calls atomic.Int32
}

func (f *fakeCreds) Use(context.Context, string, ext.Caller) (adapter.Credential, error) {
	f.calls.Add(1)
	if f.err != nil {
		return adapter.Credential{}, f.err
	}
	return adapter.Credential{Secret: []byte("tok")}, nil
}
func (f *fakeCreds) Sign(context.Context, string, ext.Caller) func(context.Context, []byte) (string, error) {
	return adapter.HMACSigner([]byte("k"))
}

// ── harness ─────────────────────────────────────────────────────────────────

const (
	entity = "11111111-1111-4111-8111-111111111111"
	schema = `{"type":"object","required":["period","boxes"],"additionalProperties":false,"properties":{
	  "period":{"type":"string","pattern":"^[0-9]{4}-(0[1-9]|1[0-2])$"},
	  "boxes":{"type":"object","required":["vat_due"],"properties":{"vat_due":{"type":"number","minimum":0}}}}}`
	goodDoc = `{"period":"2026-03","boxes":{"vat_due":125.50}}`
)

func taxSpec(retrySafe bool) adapter.Spec {
	s := adapter.Spec{
		Kind: "TAX", RequiredScope: "tax.file", Rails: []string{"VAT-RETURN@2"},
		Submit: adapter.Request{Path: "/v1/returns", Body: []adapter.Field{
			{Name: "return", From: "document"}, {Name: "submission_ref", From: "canonical", Source: "reference"},
		}},
		Query: &adapter.Query{Method: "GET", Path: "/v1/returns/by-ref/{idempotency_key}", NotFoundCodes: []int{404},
			Response: adapter.Response{RefPath: "receipt_id", StatusPath: "status", EchoPath: "submission_ref",
				StatusMap: map[string]string{"RECEIVED": "ACKNOWLEDGED", "ACCEPTED": "ACCEPTED", "REJECTED": "REJECTED"}}},
		Response: adapter.Response{RefPath: "receipt_id", StatusPath: "status", ReasonPath: "reason", EchoPath: "submission_ref",
			StatusMap: map[string]string{"RECEIVED": "ACKNOWLEDGED", "ACCEPTED": "ACCEPTED", "REJECTED": "REJECTED"}},
		Auth: adapter.Auth{Type: "bearer"}, IdempotencyHeader: "Idempotency-Key", RetrySafe: retrySafe, TimeoutSeconds: 5,
		Schema: json.RawMessage(schema),
	}
	return s
}

type env struct {
	t     *testing.T
	sim   *sim
	st    *fakeStore
	conns *fakeConns
	creds *fakeCreds
	eng   *authority.Engine
	n     int
}

func newEnv(t *testing.T, retrySafe bool) *env {
	sm := newSim(t)
	st := newFakeStore()
	_, _ = st.PutProfile(context.Background(), "taxauth", taxSpec(retrySafe), "author")
	e := &env{t: t, sim: sm, st: st,
		conns: &fakeConns{c: ext.Connector{ID: "c1", LegalEntityID: entity, EndpointURL: sm.srv.URL, Status: "ACTIVE", CredentialRef: "pol",
			Scopes: []string{"tax.file"}, ConfigFingerprint: "fp", ApprovedFingerprint: "fp"}},
		creds: &fakeCreds{}}
	e.eng = &authority.Engine{Store: st, Connectors: e.conns, Creds: e.creds, Transport: &adapter.Transport{Client: &http.Client{Timeout: 400 * time.Millisecond}},
		Cfg: authority.Config{BreakerFailures: 3, BreakerWindow: time.Minute, PollInterval: time.Minute, UrgentPollInterval: 10 * time.Second},
		Now: func() time.Time { return st.now() }}
	return e
}

func (e *env) filing(doc string) string {
	e.n++
	id := "filing-" + string(rune('a'+e.n))
	e.st.f[id] = &authority.Filing{SubmissionID: id, TenantID: "t1", InterfaceID: "if1", LegalEntityID: entity, Jurisdiction: "GB",
		ProviderKey: "taxauth", ConnectorID: "c1", EscalateBefore: 24 * time.Hour, TaxPeriod: "2026-03", FilingType: "VAT_RETURN", AmountText: "125.5000",
		SubjectRef: "subj-" + id, Document: []byte(doc), Rail: "VAT-RETURN@2", Status: authority.StatusPrepared, SubmittedAt: time.Now()}
	return id
}

func (e *env) status(id string) string { return e.st.f[id].Status }
func (e *env) caller() ext.Caller      { return ext.Caller{TenantID: "t1", PrincipalID: "op"} }
func (e *env) send(id string) (*authority.Result, error) {
	return e.eng.Send(context.Background(), id, "op", e.caller())
}
func (e *env) poll(id string) (*authority.Result, error) {
	return e.eng.Poll(context.Background(), id, "op", e.caller())
}

// ── scenarios ───────────────────────────────────────────────────────────────

func TestTransportSuccessIsReceivedNotAccepted(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(goodDoc)
	res, err := e.send(id)
	if err != nil || res.Filing.Status != authority.StatusSubmitted || res.Note != "received_by_authority" {
		t.Fatalf("%+v %v", res, err)
	}
	f := e.st.f[id]
	if f.AuthorityRef != "RC-"+id || f.ValidatedAt == nil || f.NextPollAt == nil || f.SendCount != 1 || f.SentPayloadHash == "" {
		t.Fatalf("%+v", f)
	}
	if e.sim.bodies[0] != `{"return":{"boxes":{"vat_due":125.5},"period":"2026-03"},"submission_ref":"`+id+`"}` {
		t.Fatalf("the authority received: %s", e.sim.bodies[0])
	}
	log, _ := e.st.ListTransport(context.Background(), id)
	if len(log) != 1 || log[0].Disposition != "ANSWERED" || log[0].RequestHash == "" || log[0].ResponseHash == "" {
		t.Fatalf("%+v", log)
	}
}

func TestAcceptanceAndRejectionComeFromTheAuthorityByPoll(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(goodDoc)
	_, _ = e.send(id)
	res, err := e.poll(id)
	if err != nil || res.Filing.Status != authority.StatusSubmitted || res.Note != "still_pending_at_authority" {
		t.Fatalf("%+v %v", res, err)
	}
	e.sim.setStatus(id, "ACCEPTED")
	res, err = e.poll(id)
	if err != nil || res.Filing.Status != authority.StatusAccepted || res.Filing.AuthorityRef != "RC-"+id {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := e.poll(id); !errors.Is(err, authority.ErrInFlight) {
		t.Fatalf("a final filing is not polled: %v", err)
	}
	// rejection
	id2 := e.filing(goodDoc)
	_, _ = e.send(id2)
	e.sim.setStatus(id2, "REJECTED")
	res, err = e.poll(id2)
	if err != nil || res.Filing.Status != authority.StatusRejected {
		t.Fatalf("%+v %v", res, err)
	}
	// an authority that rejects at submission
	id3 := e.filing(goodDoc)
	e.sim.script("reject")
	res, err = e.send(id3)
	if err != nil || res.Filing.Status != authority.StatusRejected || res.Note != "rejected_by_authority" {
		t.Fatalf("%+v %v", res, err)
	}
}

// acceptance: unsupported schema
func TestUnsupportedSchemaVersionIsBlockedNotSent(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(goodDoc)
	e.st.f[id].Rail = "VAT-RETURN@9"
	res, err := e.send(id)
	if !errors.Is(err, authority.ErrBlocked) || res.Filing.Status != authority.StatusBlocked {
		t.Fatalf("%+v %v", res, err)
	}
	if s, _ := e.sim.hits(); s != 0 {
		t.Fatal("nothing may be sent")
	}
	if err := e.eng.CheckBinding(context.Background(), "taxauth", "VAT-RETURN@9"); !errors.Is(err, authority.ErrBlocked) {
		t.Fatalf("%v", err)
	}
	if err := e.eng.CheckBinding(context.Background(), "taxauth", "VAT-RETURN@2"); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.CheckBinding(context.Background(), "nobody", "VAT-RETURN@2"); !errors.Is(err, authority.ErrProfileNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestSchemaFailureBlocksAndNamesPathsNeverValues(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(`{"period":"TAXPAYER-SECRET","boxes":{"vat_due":-5},"extra":1}`)
	res, err := e.eng.Validate(context.Background(), id, "op", "c")
	if !errors.Is(err, authority.ErrBlocked) || res.Filing.Status != authority.StatusBlocked {
		t.Fatalf("%+v %v", res, err)
	}
	for _, want := range []string{"$.period", "$.boxes.vat_due", "$.extra"} {
		if !strings.Contains(res.Note, want) {
			t.Errorf("the failure must name %s: %s", want, res.Note)
		}
	}
	if strings.Contains(res.Note, "TAXPAYER-SECRET") || strings.Contains(e.st.blocked[id], "TAXPAYER-SECRET") {
		t.Fatal("a value leaked into the failure")
	}
	// BLOCKED is final: it cannot be sent, and the document is not adapted to fit
	if _, err := e.send(id); !errors.Is(err, authority.ErrBlocked) {
		t.Fatalf("%v", err)
	}
	if s, _ := e.sim.hits(); s != 0 {
		t.Fatal("nothing may be sent")
	}
	// validating a good document records it
	ok := e.filing(goodDoc)
	res, err = e.eng.Validate(context.Background(), ok, "op", "c")
	if err != nil || res.Filing.Status != authority.StatusValidated {
		t.Fatalf("%+v %v", res, err)
	}
	if res, err = e.eng.Validate(context.Background(), ok, "op", "c"); err != nil || res.Note != "already_validated" {
		t.Fatalf("%+v %v", res, err)
	}
}

// acceptance: expired certificate
func TestExpiredCertificateStopsTheSendBeforeItStarts(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(goodDoc)
	past := e.st.now().Add(-time.Hour)
	e.st.f[id].CertificateNotAt = &past
	if _, err := e.send(id); !errors.Is(err, authority.ErrCertificateExpired) {
		t.Fatalf("%v", err)
	}
	if s, _ := e.sim.hits(); s != 0 || e.status(id) != authority.StatusPrepared || e.creds.calls.Load() != 0 {
		t.Fatalf("nothing sent, nothing changed, no credential released: %d %s %d", s, e.status(id), e.creds.calls.Load())
	}
	future := e.st.now().Add(time.Hour)
	e.st.f[id].CertificateNotAt = &future
	if res, err := e.send(id); err != nil || res.Filing.Status != authority.StatusSubmitted {
		t.Fatalf("%+v %v", res, err)
	}
}

// acceptance: authority timeout then duplicate filing
func TestTimeoutThenRetryAsksTheAuthorityAndNeverFilesTwice(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(goodDoc)
	e.sim.script("commit-then-hang")
	if _, err := e.send(id); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != authority.StatusPendingUnknown {
		t.Fatalf("a timeout after the request left is UNKNOWN: %s", e.status(id))
	}
	res, err := e.send(id)
	if err != nil || res.Filing.Status != authority.StatusSubmitted || res.Note != "found_at_authority" {
		t.Fatalf("%+v %v", res, err)
	}
	if s, q := e.sim.hits(); s != 1 || q != 1 {
		t.Fatalf("one filing and one question: submits=%d queries=%d", s, q)
	}
	// and the authority's final answer, if it already has one, settles it on the retry
	id2 := e.filing(goodDoc)
	e.sim.script("commit-then-hang")
	_, _ = e.send(id2)
	e.sim.setStatus(id2, "ACCEPTED")
	if res, err := e.send(id2); err != nil || res.Filing.Status != authority.StatusAccepted {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRetryAfterProvenNonReceiptResendsTheSameFiling(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(goodDoc)
	e.sim.script("drop", "ok")
	_, _ = e.send(id)
	res, err := e.send(id)
	if err != nil || res.Filing.Status != authority.StatusSubmitted {
		t.Fatalf("%+v %v", res, err)
	}
	if len(e.sim.bodies) != 2 || e.sim.bodies[0] != e.sim.bodies[1] {
		t.Fatalf("a retry is exactly the original: %v", e.sim.bodies)
	}
}

func TestRetryWithoutIdempotencyGuaranteeNeverResends(t *testing.T) {
	e := newEnv(t, false)
	id := e.filing(goodDoc)
	e.sim.script("drop", "ok")
	_, _ = e.send(id)
	if _, err := e.send(id); !errors.Is(err, authority.ErrUnresolved) || e.status(id) != authority.StatusPendingUnknown {
		t.Fatalf("%v %s", err, e.status(id))
	}
	if s, _ := e.sim.hits(); s != 1 {
		t.Fatalf("must not refile: %d", s)
	}
}

// acceptance: authority receipt mismatch
func TestAReceiptThatIsNotAboutOurFilingProvesNothing(t *testing.T) {
	e := newEnv(t, true)
	e.sim.wrongEcho = true
	id := e.filing(goodDoc)
	res, err := e.send(id)
	if err != nil || res.Filing.Status != authority.StatusPendingUnknown || res.Filing.LastFailure != "receipt_mismatch" {
		t.Fatalf("%+v %v", res, err)
	}
	if e.st.f[id].AuthorityRef != "" {
		t.Fatal("a mismatched receipt must not be recorded as the filing's receipt")
	}
	// polling a mismatched answer changes nothing either, and a mismatched ACCEPTED is not an acceptance
	e.sim.setStatus(id, "ACCEPTED")
	res, err = e.poll(id)
	if err != nil || res.Filing.Status != authority.StatusPendingUnknown {
		t.Fatalf("%+v %v", res, err)
	}
}

// acceptance: payload changed after approval
func TestAChangedPayloadIsNeverSent(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(goodDoc)
	e.sim.script("drop")
	_, _ = e.send(id)
	e.st.f[id].Document = []byte(`{"period":"2026-03","boxes":{"vat_due":0.01}}`) // a mutated document (the database forbids it; the pin is the second line)
	if _, err := e.send(id); !errors.Is(err, authority.ErrPayloadChanged) {
		t.Fatalf("%v", err)
	}
	if s, q := e.sim.hits(); s != 1 || q != 0 {
		t.Fatalf("nothing sent or asked once the payload changed: %d %d", s, q)
	}
}

// acceptance: endpoint outage near deadline
func TestOutageNearTheDeadlineIsEscalatedNotLeftToWait(t *testing.T) {
	e := newEnv(t, true)
	deadline := e.st.now().Add(6 * time.Hour) // inside the 24h escalation window
	id := e.filing(goodDoc)
	e.st.f[id].DeadlineAt = &deadline
	e.sim.script("500")
	res, err := e.send(id)
	if err != nil || res.Filing.Status != authority.StatusPendingUnknown {
		t.Fatalf("%+v %v", res, err)
	}
	if e.st.escalated[id] != 1 {
		t.Fatal("an unresolved filing near its deadline must be escalated")
	}
	// polling is urgent, not routine
	if np := e.st.f[id].NextPollAt; np == nil || np.Sub(e.st.now()) > 11*time.Second {
		t.Fatalf("near the deadline the poll interval is the urgent one: %v", np)
	}
	// a second failure does not escalate again
	_, _ = e.poll(id)
	if e.st.escalated[id] != 1 {
		t.Fatalf("escalated %d times", e.st.escalated[id])
	}
	// far from the deadline there is no escalation
	far := e.st.now().Add(30 * 24 * time.Hour)
	id2 := e.filing(goodDoc)
	e.st.f[id2].DeadlineAt = &far
	e.sim.script("500")
	_, _ = e.send(id2)
	if e.st.escalated[id2] != 0 {
		t.Fatal("no deadline pressure, no escalation")
	}
	if np := e.st.f[id2].NextPollAt; np == nil || np.Sub(e.st.now()) < 50*time.Second {
		t.Fatalf("routine poll interval expected: %v", np)
	}
}

func TestAFailingAuthorityPausesSendingExceptWhenTheDeadlineIsAtRisk(t *testing.T) {
	e := newEnv(t, true)
	for i := 0; i < 3; i++ {
		e.sim.script("500")
		_, _ = e.send(e.filing(goodDoc))
	}
	calm := e.filing(goodDoc)
	before, _ := e.sim.hits()
	if _, err := e.send(calm); !errors.Is(err, authority.ErrCircuitOpen) {
		t.Fatalf("%v", err)
	}
	if after, _ := e.sim.hits(); after != before || e.status(calm) != authority.StatusValidated {
		t.Fatalf("a paused authority is not called (the filing was validated, nothing more)")
	}
	urgent := e.filing(goodDoc)
	d := e.st.now().Add(2 * time.Hour)
	e.st.f[urgent].DeadlineAt = &d
	res, err := e.send(urgent)
	if err != nil || res.Filing.Status != authority.StatusSubmitted {
		t.Fatalf("near the deadline the filing is tried anyway: %+v %v", res, err)
	}
}

func TestNothingIsSentWhenAPreconditionFails(t *testing.T) {
	cases := map[string]func(*env){
		"connector suspended":    func(e *env) { e.conns.c.Status = "SUSPENDED" },
		"changed after approval": func(e *env) { e.conns.c.ConfigFingerprint = "other" },
		"missing scope":          func(e *env) { e.conns.c.Scopes = []string{"read"} },
		"other legal entity":     func(e *env) { e.conns.c.LegalEntityID = "22222222-2222-4222-8222-222222222222" },
		"connector missing":      func(e *env) { e.conns.err = ext.ErrConnectorRefused },
		"connector down":         func(e *env) { e.conns.err = ext.ErrConnectorDown },
		"credential refused":     func(e *env) { e.creds.err = ext.ErrCredentialRefused },
		"vault down":             func(e *env) { e.creds.err = ext.ErrCredentialDown },
	}
	for name, mut := range cases {
		e := newEnv(t, true)
		mut(e)
		id := e.filing(goodDoc)
		if _, err := e.send(id); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if s, _ := e.sim.hits(); s != 0 || e.st.f[id].SendCount != 0 {
			t.Errorf("%s: the authority was called", name)
		}
	}
}

func TestConcurrentSubmitsPutExactlyOneFilingInFlight(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(goodDoc)
	var wg sync.WaitGroup
	var ok, busy atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.send(id)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, authority.ErrInFlight):
				busy.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if s, _ := e.sim.hits(); s != 1 || ok.Load() != 1 || busy.Load() != 7 {
		t.Fatalf("submits=%d ok=%d busy=%d", s, ok.Load(), busy.Load())
	}
}

func TestStaleInFlightIsSweptToUnknownNeverResent(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(goodDoc)
	e.st.f[id].Status = authority.StatusValidated
	if _, err := e.st.BeginSend(context.Background(), id, authority.StatusValidated, 1, "h", "op"); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.eng.Sweep(context.Background()); n != 0 {
		t.Fatal("not stale yet")
	}
	base := e.st.now
	e.st.now = func() time.Time { return base().Add(10 * time.Minute) }
	if n, err := e.eng.Sweep(context.Background()); err != nil || n != 1 || e.status(id) != authority.StatusPendingUnknown {
		t.Fatalf("%d %v %s", n, err, e.status(id))
	}
	if s, _ := e.sim.hits(); s != 0 {
		t.Fatal("a sweep never sends")
	}
}

func TestPollDueAsksAboutEveryFilingWhoseTimeHasCome(t *testing.T) {
	e := newEnv(t, true)
	a, b := e.filing(goodDoc), e.filing(goodDoc)
	_, _ = e.send(a)
	_, _ = e.send(b)
	e.sim.setStatus(a, "ACCEPTED")
	if n, _ := e.eng.PollDue(context.Background(), func(c context.Context, _ string) context.Context { return c }, "poller"); n != 0 {
		t.Fatalf("nothing is due yet: %d", n)
	}
	base := e.st.now
	e.st.now = func() time.Time { return base().Add(2 * time.Minute) }
	if n, err := e.eng.PollDue(context.Background(), func(c context.Context, _ string) context.Context { return c }, "poller"); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if e.status(a) != authority.StatusAccepted || e.status(b) != authority.StatusSubmitted {
		t.Fatalf("%s %s", e.status(a), e.status(b))
	}
}

func TestPollNeverSendsAndNeedsAQuery(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(goodDoc)
	if _, err := e.poll(id); !errors.Is(err, authority.ErrInFlight) {
		t.Fatalf("an unsent filing is not polled: %v", err)
	}
	if s, _ := e.sim.hits(); s != 0 {
		t.Fatal("poll must not send")
	}
	spec := taxSpec(true)
	spec.Query = nil
	e.st.profiles = nil
	_, _ = e.st.PutProfile(context.Background(), "taxauth", spec, "a")
	e.st.f[id].Status = authority.StatusSubmitted
	if _, err := e.poll(id); !errors.Is(err, authority.ErrNoQuery) {
		t.Fatalf("%v", err)
	}
}

func TestCredentialIsReleasedPerCallAndNeverInEvidence(t *testing.T) {
	e := newEnv(t, true)
	id := e.filing(goodDoc)
	if _, err := e.send(id); err != nil {
		t.Fatal(err)
	}
	if e.creds.calls.Load() != 1 {
		t.Fatalf("calls %d", e.creds.calls.Load())
	}
	b, _ := json.Marshal(e.st.allLog)
	if strings.Contains(string(b), "tok") {
		t.Fatalf("the credential is in the evidence: %s", b)
	}
}
