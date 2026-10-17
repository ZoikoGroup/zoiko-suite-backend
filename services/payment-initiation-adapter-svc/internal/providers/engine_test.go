package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/eventing/adapter"
	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/providers"
)

// ── the provider simulator ──────────────────────────────────────────────────

type sim struct {
	srv *httptest.Server
	mu  sync.Mutex

	committed map[string]string // idempotency key -> provider ref
	submits   int
	queries   int
	keys      []string
	bodies    []string

	// behaviours for the NEXT submit (consumed one per request)
	next []string // "ok" | "commit-then-hang" | "commit-then-drop" | "drop" | "429" | "500" | "reject" | "weird" | "slow-ok"
	// the query endpoint answers from committed unless forced
	queryAlways429 bool
	hang           chan struct{}
}

func newSim(t *testing.T) *sim {
	s := &sim{committed: map[string]string{}, hang: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(func() { close(s.hang); s.srv.Close() })
	return s
}

func (s *sim) script(b ...string) { s.mu.Lock(); s.next = append(s.next, b...); s.mu.Unlock() }
func (s *sim) hits() (int, int)   { s.mu.Lock(); defer s.mu.Unlock(); return s.submits, s.queries }

func (s *sim) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if r.Method == http.MethodGet { // query by idempotency key
		s.queries++
		force := s.queryAlways429
		key := r.URL.Path[len("/v1/payments/by-key/"):]
		ref, ok := s.committed[key]
		s.mu.Unlock()
		switch {
		case force:
			w.WriteHeader(429)
		case !ok:
			w.WriteHeader(404)
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": ref, "status": "ACCEPTED"})
		}
		return
	}
	s.submits++
	key := r.Header.Get("Idempotency-Key")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	raw, _ := json.Marshal(body)
	s.keys = append(s.keys, key)
	s.bodies = append(s.bodies, string(raw))
	mode := "ok"
	if len(s.next) > 0 {
		mode, s.next = s.next[0], s.next[1:]
	}
	commit := func() string {
		if ref, ok := s.committed[key]; ok {
			return ref
		}
		ref := "PRV-" + key
		s.committed[key] = ref
		return ref
	}
	switch mode {
	case "ok":
		ref := commit()
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": ref, "status": "ACCEPTED"})
	case "slow-ok":
		ref := commit()
		s.mu.Unlock()
		time.Sleep(250 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": ref, "status": "ACCEPTED"})
	case "commit-then-hang":
		commit()
		s.mu.Unlock()
		<-s.hang
	case "commit-then-drop":
		commit()
		s.mu.Unlock()
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	case "drop":
		s.mu.Unlock()
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	case "429":
		s.mu.Unlock()
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
	case "500":
		s.mu.Unlock()
		w.WriteHeader(500)
	case "reject":
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "R-1", "status": "RJCT", "reason": "AC04"})
	case "weird":
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "W-1", "status": "SOMETHING_NEW"})
	}
}

// ── fakes for the engine's collaborators ────────────────────────────────────

type fakeStore struct {
	mu       sync.Mutex
	now      func() time.Time
	profiles map[string][]*providers.ProfileRecord
	d        map[string]*providers.Dispatch
	log      map[string][]providers.TransportEntry
	rejected map[string]string
	allLog   []providers.TransportEntry
}

func newFakeStore() *fakeStore {
	return &fakeStore{now: func() time.Time { return time.Now().UTC() }, profiles: map[string][]*providers.ProfileRecord{},
		d: map[string]*providers.Dispatch{}, log: map[string][]providers.TransportEntry{}, rejected: map[string]string{}}
}

func (f *fakeStore) PutProfile(_ context.Context, key string, spec adapter.Spec, by string) (*providers.ProfileRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &providers.ProfileRecord{ProviderKey: key, Version: len(f.profiles[key]) + 1, Kind: spec.Kind, Status: providers.ProfileCertified, Spec: spec.Normalize(), CreatedBy: by}
	f.profiles[key] = append(f.profiles[key], p)
	return p, nil
}
func (f *fakeStore) GetProfile(_ context.Context, key string, version int) (*providers.ProfileRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.profiles[key]
	for i := len(list) - 1; i >= 0; i-- {
		if (version == 0 && list[i].Status == providers.ProfileCertified) || list[i].Version == version {
			c := *list[i]
			return &c, nil
		}
	}
	return nil, providers.ErrProfileNotFound
}
func (f *fakeStore) CertifyProfile(context.Context, string, int, string) (*providers.ProfileRecord, error) {
	return nil, errors.New("unused")
}
func (f *fakeStore) SetProfileStatus(context.Context, string, int, string) (*providers.ProfileRecord, error) {
	return nil, errors.New("unused")
}
func (f *fakeStore) ListProfiles(context.Context, string) ([]providers.ProfileRecord, error) {
	return nil, nil
}
func (f *fakeStore) LoadDispatch(_ context.Context, id string) (*providers.Dispatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.d[id]
	if !ok {
		return nil, domain.ErrAttemptNotFound
	}
	c := *d
	return &c, nil
}
func (f *fakeStore) BeginSend(_ context.Context, id string, from domain.AttemptStatus, ver int, hash, _ string) (*providers.Dispatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.d[id]
	switch {
	case d.Status != from:
		return nil, providers.ErrInFlight
	case d.NotBefore != nil && d.NotBefore.After(f.now()):
		return nil, providers.ErrNotBefore
	case d.SentPayloadHash != "" && d.SentPayloadHash != hash, d.ProfileVersion != nil && *d.ProfileVersion != ver:
		return nil, providers.ErrPayloadChanged
	}
	now := f.now()
	d.Status, d.InFlightSince, d.SendCount, d.NotBefore = domain.StatusSubmitting, &now, d.SendCount+1, nil
	d.ProfileVersion, d.SentPayloadHash = &ver, hash
	c := *d
	return &c, nil
}
func (f *fakeStore) FinishSend(_ context.Context, id string, o providers.SendOutcome, actor, _ string) (*domain.PaymentInitiationAttempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.d[id]
	if d.Status != o.From {
		return nil, providers.ErrInFlight
	}
	o.Transport.AttemptID, o.Transport.Actor = id, actor
	f.log[id] = append(f.log[id], o.Transport)
	f.allLog = append(f.allLog, o.Transport)
	switch o.Kind {
	case providers.OutSubmitted:
		d.Status, d.ProviderRequestID, d.ProviderStatus, d.InFlightSince = domain.StatusSubmitted, o.ProviderRef, o.Provider, nil
	case providers.OutRejected:
		d.Status, d.InFlightSince = domain.StatusRejectedBeforeSubmission, nil
	case providers.OutUnknown:
		d.Status, d.InFlightSince, d.LastFailure = domain.StatusPendingUnknown, nil, o.Failure
	case providers.OutRelease:
		d.Status, d.InFlightSince, d.NotBefore, d.LastFailure = o.ReleaseTo, nil, o.NotBefore, o.Failure
	case providers.OutNone:
		d.LastFailure = o.Failure
	}
	return &domain.PaymentInitiationAttempt{AttemptID: id, Status: d.Status, ProviderRequestID: d.ProviderRequestID}, nil
}
func (f *fakeStore) RejectUnsent(_ context.Context, id, reason, _, _ string) (*domain.PaymentInitiationAttempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.d[id]
	d.Status = domain.StatusRejectedBeforeSubmission
	f.rejected[id] = reason
	return &domain.PaymentInitiationAttempt{AttemptID: id, Status: d.Status}, nil
}
func (f *fakeStore) ListTransport(_ context.Context, id string) ([]providers.TransportEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]providers.TransportEntry(nil), f.log[id]...), nil
}
func (f *fakeStore) RecentCalls(_ context.Context, key string, since time.Time, limit int) ([]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []bool
	for i := len(f.allLog) - 1; i >= 0 && len(out) < limit; i-- {
		e := f.allLog[i]
		if e.ProviderKey == key && e.Operation == "SUBMIT" {
			out = append(out, e.Disposition == "ANSWERED" && (e.Class == "ACKNOWLEDGED" || e.Class == "PENDING" || e.Class == "REJECTED" || e.Class == "RATE_LIMITED"))
		}
	}
	return out, nil
}
func (f *fakeStore) SweepStale(_ context.Context, before time.Time, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, d := range f.d {
		if d.Status == domain.StatusSubmitting && d.InFlightSince.Before(before) {
			d.Status, d.InFlightSince = domain.StatusPendingUnknown, nil
			n++
		}
	}
	return n, nil
}

type fakeConns struct {
	c   providers.Connector
	err error
}

func (f *fakeConns) Resolve(context.Context, string, providers.Caller) (providers.Connector, error) {
	return f.c, f.err
}

type fakeCreds struct {
	err   error
	calls atomic.Int32
}

func (f *fakeCreds) Use(context.Context, string, providers.Caller) (adapter.Credential, error) {
	f.calls.Add(1)
	if f.err != nil {
		return adapter.Credential{}, f.err
	}
	return adapter.Credential{Secret: []byte("tok")}, nil
}
func (f *fakeCreds) Sign(context.Context, string, providers.Caller) func(context.Context, []byte) (string, error) {
	return adapter.HMACSigner([]byte("k"))
}

// ── harness ─────────────────────────────────────────────────────────────────

const entity = "11111111-1111-4111-8111-111111111111"

func bankSpec(retrySafe bool) adapter.Spec {
	s := adapter.Spec{
		Kind: "BANK", RequiredScope: "payments.initiate", Rails: []string{"SEPA_CT@1"}, Currencies: []string{"EUR"}, MaxAmount: "100000.00",
		Submit: adapter.Request{Path: "/v1/payments", AmountField: "amount.value", Body: []adapter.Field{
			{Name: "amount.value", From: "canonical", Source: "amount"}, {Name: "amount.currency", From: "canonical", Source: "currency"},
			{Name: "debtor", From: "canonical", Source: "payer"}, {Name: "creditor", From: "canonical", Source: "payee"},
		}},
		Query: &adapter.Query{Method: "GET", Path: "/v1/payments/by-key/{idempotency_key}", NotFoundCodes: []int{404},
			Response: adapter.Response{RefPath: "id", StatusPath: "status", StatusMap: map[string]string{"ACCEPTED": "ACKNOWLEDGED"}}},
		Response: adapter.Response{RefPath: "id", StatusPath: "status", ReasonPath: "reason",
			StatusMap: map[string]string{"ACCEPTED": "ACKNOWLEDGED", "PENDING": "PENDING", "RJCT": "REJECTED"}},
		Auth: adapter.Auth{Type: "bearer"}, TimeoutSeconds: 5,
	}
	if retrySafe {
		s.IdempotencyHeader, s.RetrySafe = "Idempotency-Key", true
	} else {
		s.IdempotencyHeader = "Idempotency-Key"
	}
	return s
}

type env struct {
	t     *testing.T
	sim   *sim
	st    *fakeStore
	conns *fakeConns
	creds *fakeCreds
	eng   *providers.Engine
	n     int
}

func newEnv(t *testing.T, retrySafe bool) *env {
	sm := newSim(t)
	st := newFakeStore()
	_, _ = st.PutProfile(context.Background(), "simbank", bankSpec(retrySafe), "author")
	e := &env{t: t, sim: sm, st: st,
		conns: &fakeConns{c: providers.Connector{ID: "c1", LegalEntityID: entity, EndpointURL: sm.srv.URL, Status: "ACTIVE", CredentialRef: "pol",
			Scopes: []string{"payments.initiate"}, ConfigFingerprint: "fp", ApprovedFingerprint: "fp"}},
		creds: &fakeCreds{}}
	e.eng = &providers.Engine{Store: st, Connectors: e.conns, Creds: e.creds, Transport: &adapter.Transport{Client: &http.Client{Timeout: 400 * time.Millisecond}},
		Cfg: providers.Config{BreakerFailures: 3, BreakerWindow: time.Minute}, Now: func() time.Time { return st.now() }}
	return e
}

func (e *env) attempt(amount string) string {
	e.n++
	id := "attempt-" + string(rune('a'+e.n))
	e.st.d[id] = &providers.Dispatch{AttemptID: id, TenantID: "t1", LegalEntityID: entity, Status: domain.StatusPrepared, IdempotencyKey: "idem-" + id,
		AmountText: amount, Currency: "EUR", PayerAccountRef: "DE00AA", PayeeRef: "FR00BB", ExecutionDate: time.Now(),
		ProviderKey: "simbank", Rail: "SEPA_CT@1", ConnectorID: "c1"}
	return id
}

func (e *env) status(id string) domain.AttemptStatus { return e.st.d[id].Status }
func (e *env) send(id string) (*providers.Result, error) {
	return e.eng.Send(context.Background(), id, "op", providers.Caller{TenantID: "t1", PrincipalID: "op"})
}

// ── scenarios ───────────────────────────────────────────────────────────────

func TestAcknowledgedIsSubmittedNeverSettled(t *testing.T) {
	e := newEnv(t, true)
	id := e.attempt("10.50")
	res, err := e.send(id)
	if err != nil || res.Attempt.Status != domain.StatusSubmitted || res.Attempt.ProviderRequestID != "PRV-idem-"+id {
		t.Fatalf("%+v %v", res, err)
	}
	// "provider says success but bank finality is absent": the adapter's best answer is SUBMITTED. No status
	// in this service's vocabulary means settled, and nothing here can produce one.
	for _, s := range []domain.AttemptStatus{domain.StatusPrepared, domain.StatusSubmitting, domain.StatusSubmitted, domain.StatusPendingUnknown,
		domain.StatusRejectedBeforeSubmission, domain.StatusCancelled, domain.StatusQuarantined} {
		if string(s) == "SETTLED" {
			t.Fatal("an attempt status must never mean settled")
		}
	}
	log, _ := e.st.ListTransport(context.Background(), id)
	if len(log) != 1 || log[0].Operation != "SUBMIT" || log[0].Disposition != "ANSWERED" || log[0].RequestHash == "" || log[0].ResponseHash == "" {
		t.Fatalf("transport evidence: %+v", log)
	}
	d := e.st.d[id]
	if d.SendCount != 1 || d.SentPayloadHash == "" || d.ProfileVersion == nil || *d.ProfileVersion != 1 {
		t.Fatalf("pinned context: %+v", d)
	}
	if e.sim.bodies[0] != `{"amount":{"currency":"EUR","value":"10.50"},"creditor":"FR00BB","debtor":"DE00AA"}` {
		t.Fatalf("what the provider received: %s", e.sim.bodies[0])
	}
}

// the headline acceptance case: timeout after the request left, then a retry. The retry must ask first and
// must not send a second payment.
func TestTimeoutThenRetryAsksBeforeSending(t *testing.T) {
	e := newEnv(t, true)
	id := e.attempt("10.50")
	e.sim.script("commit-then-hang")
	if _, err := e.send(id); err != nil {
		t.Fatal(err)
	}
	if e.status(id) != domain.StatusPendingUnknown {
		t.Fatalf("a timeout after send is unknown, got %s", e.status(id))
	}
	res, err := e.send(id)
	if err != nil || res.Attempt.Status != domain.StatusSubmitted || res.Note != "found_at_provider" {
		t.Fatalf("%+v %v", res, err)
	}
	if s, q := e.sim.hits(); s != 1 || q != 1 {
		t.Fatalf("exactly one submit and one query expected, got submits=%d queries=%d", s, q)
	}
}

func TestRetryAfterProvenNonReceiptResendsTheSameRequest(t *testing.T) {
	e := newEnv(t, true)
	id := e.attempt("10.50")
	e.sim.script("drop", "ok") // the first request never reached the provider's ledger
	if _, err := e.send(id); err != nil || e.status(id) != domain.StatusPendingUnknown {
		t.Fatalf("%v %s", err, e.status(id))
	}
	res, err := e.send(id)
	if err != nil || res.Attempt.Status != domain.StatusSubmitted {
		t.Fatalf("%+v %v", res, err)
	}
	if len(e.sim.keys) != 2 || e.sim.keys[0] != e.sim.keys[1] || e.sim.bodies[0] != e.sim.bodies[1] {
		t.Fatalf("a retry must carry the same idempotency key and the same body: %v %v", e.sim.keys, e.sim.bodies)
	}
	if e.st.d[id].SendCount != 2 {
		t.Fatalf("send count %d", e.st.d[id].SendCount)
	}
}

func TestRetryWithoutIdempotencyGuaranteeNeverResends(t *testing.T) {
	e := newEnv(t, false) // the provider does not promise key-level deduplication
	id := e.attempt("10.50")
	e.sim.script("drop", "ok")
	_, _ = e.send(id)
	res, err := e.send(id)
	if !errors.Is(err, providers.ErrUnresolved) || e.status(id) != domain.StatusPendingUnknown {
		t.Fatalf("%+v %v %s", res, err, e.status(id))
	}
	if s, _ := e.sim.hits(); s != 1 {
		t.Fatalf("the payment must not be resent, submits=%d", s)
	}
}

func TestRetryWhileTheProviderCannotAnswerStaysPending(t *testing.T) {
	e := newEnv(t, true)
	id := e.attempt("10.50")
	e.sim.script("commit-then-drop")
	_, _ = e.send(id)
	e.sim.mu.Lock()
	e.sim.queryAlways429 = true
	e.sim.mu.Unlock()
	if _, err := e.send(id); !errors.Is(err, providers.ErrUnresolved) {
		t.Fatalf("%v", err)
	}
	if s, _ := e.sim.hits(); s != 1 || e.status(id) != domain.StatusPendingUnknown {
		t.Fatalf("submits=%d status=%s", s, e.status(id))
	}
}

func TestProviderRateLimitIsNotProcessed(t *testing.T) {
	e := newEnv(t, true)
	id := e.attempt("10.50")
	e.sim.script("429", "ok")
	if _, err := e.send(id); err != nil {
		t.Fatal(err)
	}
	d := e.st.d[id]
	if d.Status != domain.StatusPrepared || d.NotBefore == nil || d.LastFailure != "rate_limited" {
		t.Fatalf("a 429 means not processed: %+v", d)
	}
	if _, err := e.send(id); !errors.Is(err, providers.ErrNotBefore) {
		t.Fatalf("must wait: %v", err)
	}
	if s, _ := e.sim.hits(); s != 1 {
		t.Fatalf("no call while told to wait, submits=%d", s)
	}
	// after the wait the same attempt is sent and succeeds
	base := e.st.now
	e.st.now = func() time.Time { return base().Add(5 * time.Second) }
	if res, err := e.send(id); err != nil || res.Attempt.Status != domain.StatusSubmitted {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestOtherProviderAnswers(t *testing.T) {
	cases := []struct {
		mode string
		want domain.AttemptStatus
	}{
		{"500", domain.StatusPendingUnknown},   // the provider may have acted
		{"weird", domain.StatusPendingUnknown}, // a status the profile does not define is not success
		{"reject", domain.StatusRejectedBeforeSubmission},
		{"drop", domain.StatusPendingUnknown},
	}
	for _, c := range cases {
		e := newEnv(t, true)
		id := e.attempt("10.50")
		e.sim.script(c.mode)
		if _, err := e.send(id); err != nil || e.status(id) != c.want {
			t.Errorf("%s: %v status=%s want %s", c.mode, err, e.status(id), c.want)
		}
	}
}

func TestNothingIsSentWhenAPreconditionFails(t *testing.T) {
	type mut func(*env)
	cases := map[string]mut{
		"connector not active":   func(e *env) { e.conns.c.Status = "SUSPENDED" },
		"connector draft":        func(e *env) { e.conns.c.Status = "DRAFT" },
		"changed after approval": func(e *env) { e.conns.c.ConfigFingerprint = "other" },
		"never approved":         func(e *env) { e.conns.c.ConfigFingerprint, e.conns.c.ApprovedFingerprint = "", "" },
		"missing scope":          func(e *env) { e.conns.c.Scopes = []string{"read"} },
		"other legal entity":     func(e *env) { e.conns.c.LegalEntityID = "22222222-2222-4222-8222-222222222222" },
		"connector missing":      func(e *env) { e.conns.err = providers.ErrConnectorRefused },
		"connector service down": func(e *env) { e.conns.err = providers.ErrConnectorDown },
		"credential refused":     func(e *env) { e.creds.err = providers.ErrCredentialRefused },
		"vault down":             func(e *env) { e.creds.err = providers.ErrCredentialDown },
	}
	for name, mutate := range cases {
		e := newEnv(t, true)
		mutate(e)
		id := e.attempt("10.50")
		if _, err := e.send(id); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if s, _ := e.sim.hits(); s != 0 {
			t.Errorf("%s: the provider was called", name)
		}
		if e.status(id) != domain.StatusPrepared || e.st.d[id].SendCount != 0 {
			t.Errorf("%s: the attempt must be untouched: %+v", name, e.st.d[id])
		}
	}
}

func TestUnsupportedRailCurrencyAndLimitAreRefusedBeforeSending(t *testing.T) {
	for name, mut := range map[string]func(*providers.Dispatch){
		"rail version": func(d *providers.Dispatch) { d.Rail = "SEPA_CT@9" },
		"currency":     func(d *providers.Dispatch) { d.Currency = "USD" },
		"limit":        func(d *providers.Dispatch) { d.AmountText = "100000.01" },
	} {
		e := newEnv(t, true)
		id := e.attempt("10.50")
		mut(e.st.d[id])
		res, err := e.send(id)
		if err != nil || res.Attempt.Status != domain.StatusRejectedBeforeSubmission || res.Note != "rejected_by_profile" {
			t.Errorf("%s: %+v %v", name, res, err)
		}
		if s, _ := e.sim.hits(); s != 0 {
			t.Errorf("%s: the provider was called", name)
		}
	}
	e := newEnv(t, true)
	if err := e.eng.CheckBinding(context.Background(), "simbank", "SEPA_CT@9", "EUR", "1.00"); !errors.Is(err, providers.ErrProfileMismatch) {
		t.Fatalf("%v", err)
	}
	if err := e.eng.CheckBinding(context.Background(), "nobank", "SEPA_CT@1", "EUR", "1.00"); !errors.Is(err, providers.ErrProfileNotFound) {
		t.Fatalf("%v", err)
	}
	if err := e.eng.CheckBinding(context.Background(), "simbank", "SEPA_CT@1", "EUR", "1.00"); err != nil {
		t.Fatal(err)
	}
}

// a retry is exactly the original: if what would be sent differs from what was first sent, nothing is sent
func TestPayloadIsPinnedAcrossRetries(t *testing.T) {
	e := newEnv(t, true)
	id := e.attempt("10.50")
	e.sim.script("drop")
	_, _ = e.send(id)
	e.st.d[id].AmountText = "10.51" // a mutated amount (the database trigger forbids this; the pin is the second line)
	e.sim.mu.Lock()
	e.sim.committed = map[string]string{}
	e.sim.mu.Unlock()
	if _, err := e.send(id); !errors.Is(err, providers.ErrPayloadChanged) {
		t.Fatalf("%v", err)
	}
	if s, q := e.sim.hits(); s != 1 || q != 0 {
		t.Fatalf("nothing may be sent or asked when the payload changed: %d %d", s, q)
	}
}

func TestSendingPausesWhenTheProviderIsFailing(t *testing.T) {
	e := newEnv(t, true)
	for i := 0; i < 3; i++ {
		e.sim.script("500")
		id := e.attempt("1.00")
		_, _ = e.send(id)
	}
	id := e.attempt("1.00")
	before, _ := e.sim.hits()
	if _, err := e.send(id); !errors.Is(err, providers.ErrCircuitOpen) {
		t.Fatalf("%v", err)
	}
	if after, _ := e.sim.hits(); after != before || e.status(id) != domain.StatusPrepared {
		t.Fatalf("a paused provider is not called and the attempt is untouched")
	}
	// one good answer closes it: the log's newest entry is healthy
	e.st.allLog = append(e.st.allLog, providers.TransportEntry{ProviderKey: "simbank", Operation: "SUBMIT", Disposition: "ANSWERED", Class: "ACKNOWLEDGED"})
	if _, err := e.send(id); err != nil || e.status(id) != domain.StatusSubmitted {
		t.Fatalf("%v %s", err, e.status(id))
	}
}

func TestConcurrentSendsPutExactlyOneRequestInFlight(t *testing.T) {
	e := newEnv(t, true)
	id := e.attempt("10.50")
	e.sim.script("slow-ok")
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
			case errors.Is(err, providers.ErrInFlight):
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
	id := e.attempt("10.50")
	// the process died after the compare-and-set and before any outcome was recorded
	if _, err := e.st.BeginSend(context.Background(), id, domain.StatusPrepared, 1, "h", "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.send(id); !errors.Is(err, providers.ErrInFlight) {
		t.Fatalf("a request in flight cannot be sent again: %v", err)
	}
	if n, _ := e.eng.Sweep(context.Background()); n != 0 {
		t.Fatalf("not stale yet: %d", n)
	}
	base := e.st.now
	e.st.now = func() time.Time { return base().Add(10 * time.Minute) }
	if n, err := e.eng.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if e.status(id) != domain.StatusPendingUnknown {
		t.Fatalf("%s", e.status(id))
	}
	if s, _ := e.sim.hits(); s != 0 {
		t.Fatal("a sweep never sends")
	}
}

func TestQueryNeverSends(t *testing.T) {
	e := newEnv(t, true)
	id := e.attempt("10.50")
	e.sim.script("commit-then-drop")
	_, _ = e.send(id)
	res, err := e.eng.Query(context.Background(), id, "op", providers.Caller{TenantID: "t1", PrincipalID: "op"})
	if err != nil || res.Attempt == nil || res.Attempt.Status != domain.StatusSubmitted {
		t.Fatalf("%+v %v", res, err)
	}
	if s, _ := e.sim.hits(); s != 1 {
		t.Fatalf("submits=%d", s)
	}
	// a query on an attempt that is not pending is refused
	if _, err := e.eng.Query(context.Background(), id, "op", providers.Caller{TenantID: "t1"}); !errors.Is(err, providers.ErrInFlight) {
		t.Fatalf("%v", err)
	}
}

// the call is made and its outcome recorded even when the caller hangs up
func TestOutcomeIsRecordedWhenTheCallerHangsUp(t *testing.T) {
	e := newEnv(t, true)
	id := e.attempt("10.50")
	e.sim.script("slow-ok")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(60 * time.Millisecond); cancel() }()
	_, err := e.eng.Send(ctx, id, "op", providers.Caller{TenantID: "t1", PrincipalID: "op"})
	if err != nil {
		t.Fatal(err)
	}
	if e.status(id) != domain.StatusSubmitted {
		t.Fatalf("the answer that arrived after the caller left must still be recorded: %s", e.status(id))
	}
}

func TestCredentialIsUsedForOneCallAndNeverLogged(t *testing.T) {
	e := newEnv(t, true)
	id := e.attempt("10.50")
	if _, err := e.send(id); err != nil {
		t.Fatal(err)
	}
	if e.creds.calls.Load() != 1 {
		t.Fatalf("one credential release per send, got %d", e.creds.calls.Load())
	}
	b, _ := json.Marshal(e.st.log[id])
	if string(b) == "" || contains(string(b), "tok") {
		t.Fatalf("transport evidence must not contain the credential: %s", b)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
