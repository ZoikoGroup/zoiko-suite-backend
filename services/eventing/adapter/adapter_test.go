package adapter

import (
	"context"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/eventing/signature"
)

func bankSpec() Spec {
	return Spec{
		Kind: "BANK", RequiredScope: "payments.initiate",
		Rails:      []string{"SEPA_CT@1", "FPS@1"},
		Currencies: []string{"EUR", "GBP", "JPY"},
		MaxAmount:  "100000.00",
		Submit: Request{
			Method: "POST", Path: "/v1/payments", AmountField: "instructedAmount.value", AmountFormat: "decimal",
			Body: []Field{
				{Name: "instructedAmount.value", From: "canonical", Source: "amount"},
				{Name: "instructedAmount.currency", From: "canonical", Source: "currency"},
				{Name: "debtor.account", From: "canonical", Source: "payer"},
				{Name: "creditor.account", From: "canonical", Source: "payee"},
				{Name: "scheme", From: "const", Const: "SCT"},
			},
		},
		Query: &Query{Method: "GET", Path: "/v1/payments/by-key/{idempotency_key}", NotFoundCodes: []int{404},
			Response: Response{RefPath: "id", StatusPath: "status", StatusMap: map[string]string{"ACCEPTED": StatusAcknowledged, "PENDING": StatusPending, "RJCT": StatusRejected}}},
		Response: Response{RefPath: "id", StatusPath: "status", ReasonPath: "reason",
			StatusMap: map[string]string{"ACCEPTED": StatusAcknowledged, "PENDING": StatusPending, "RJCT": StatusRejected}},
		Auth: Auth{Type: "bearer"}, IdempotencyHeader: "Idempotency-Key", RetrySafe: true, TimeoutSeconds: 2,
		Callback: &Callback{Scheme: "hmac-sha256-hex", EventRefPath: "event_id", PaymentRefPath: "payment_id", StatusPath: "status",
			EventTimePath: "at", StatusMap: map[string]string{"BOOKED": "SETTLED", "RJCT": StatusRejected, "PDNG": StatusPending}},
	}
}

func canon() Canonical {
	return Canonical{"rail": "SEPA_CT@1", "amount": "10.50", "currency": "EUR", "payer": "DE00AA", "payee": "FR00BB"}
}

func TestSpecValidate(t *testing.T) {
	if err := bankSpec().Normalize().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Spec){
		"kind":                  func(s *Spec) { s.Kind = "X" },
		"no rails":              func(s *Spec) { s.Rails = nil },
		"retry-safe w/o header": func(s *Spec) { s.IdempotencyHeader = "" },
		"no not-found":          func(s *Spec) { s.Query.NotFoundCodes = nil },
		"2xx not-found":         func(s *Spec) { s.Query.NotFoundCodes = []int{200} },
		"abs url path":          func(s *Spec) { s.Submit.Path = "http://evil/x" },
		"traversal":             func(s *Spec) { s.Submit.Path = "/a/../b" },
		"amount elsewhere":      func(s *Spec) { s.Submit.AmountField = "other" },
		"bad status map":        func(s *Spec) { s.Response.StatusMap["X"] = "SETTLED" },
		"zoiko tolerance":       func(s *Spec) { s.Callback.Scheme = "zoiko-v1"; s.Callback.ToleranceSeconds = 1 },
		"bad limit":             func(s *Spec) { s.MaxAmount = "1e9" },
		"timeout":               func(s *Spec) { s.TimeoutSeconds = 500 },
	}
	for name, mut := range cases {
		s := bankSpec()
		mut(&s)
		if err := s.Normalize().Validate(); !errors.Is(err, ErrSpecInvalid) {
			t.Errorf("%s: want ErrSpecInvalid, got %v", name, err)
		}
	}
}

func TestBuildRequestAndGuards(t *testing.T) {
	s := bankSpec().Normalize()
	raw, err := s.BuildRequest(canon())
	if err != nil || !strings.Contains(string(raw), `"value":"10.50"`) {
		t.Fatalf("%s %v", raw, err)
	}
	// unsupported rail version
	c := canon()
	c["rail"] = "SEPA_CT@9"
	if _, err := s.BuildRequest(c); !errors.Is(err, ErrRailUnsupported) {
		t.Fatalf("rail: %v", err)
	}
	c = canon()
	c["currency"] = "USD"
	if _, err := s.BuildRequest(c); !errors.Is(err, ErrCurrency) {
		t.Fatalf("currency: %v", err)
	}
	c = canon()
	c["amount"] = "100000.01"
	if _, err := s.BuildRequest(c); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("limit: %v", err)
	}
	for _, bad := range []string{"0", "-5", "abc", "1e3", ""} {
		c = canon()
		c["amount"] = bad
		if _, err := s.BuildRequest(c); err == nil {
			t.Errorf("amount %q accepted", bad)
		}
	}
	c = canon()
	delete(c, "payee")
	if _, err := s.BuildRequest(c); !errors.Is(err, ErrCanonicalMissing) {
		t.Fatalf("missing: %v", err)
	}
}

func TestAmountFormats(t *testing.T) {
	cases := []struct {
		format, cur, amt, want string
	}{
		{"decimal", "EUR", "10", `"value":"10.00"`},
		{"number", "EUR", "10.5", `"value":10.50`},
		{"minor", "EUR", "10.50", `"value":1050`},
		{"minor", "JPY", "1050", `"value":1050`},
		{"minor", "KWD", "1.234", `"value":1234`},
	}
	for _, tc := range cases {
		s := bankSpec()
		s.Currencies = nil
		s.Submit.AmountFormat = tc.format
		raw, err := s.Normalize().BuildRequest(Canonical{"rail": "SEPA_CT@1", "amount": tc.amt, "currency": tc.cur, "payer": "a", "payee": "b"})
		if err != nil || !strings.Contains(string(raw), tc.want) {
			t.Errorf("%v: %s %v", tc, raw, err)
		}
	}
	// fractional minor units are refused, not rounded: a JPY amount with decimals would change the money
	s := bankSpec()
	s.Currencies = nil
	s.Submit.AmountFormat = "minor"
	if _, err := s.Normalize().BuildRequest(Canonical{"rail": "SEPA_CT@1", "amount": "10.5", "currency": "JPY", "payer": "a", "payee": "b"}); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("fractional JPY: %v", err)
	}
}

// the amount re-check catches a profile or bug that changes the money
func TestAmountMutationIsCaught(t *testing.T) {
	s := bankSpec().Normalize()
	want, _ := new(big.Rat).SetString("10.50")
	if err := s.verifyAmount([]byte(`{"instructedAmount":{"value":"10.51"}}`), want, "EUR"); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("mutated: %v", err)
	}
	if err := s.verifyAmount([]byte(`{"instructedAmount":{}}`), want, "EUR"); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.verifyAmount([]byte(`{"instructedAmount":{"value":"10.5"}}`), want, "EUR"); err != nil {
		t.Fatalf("equal value in another spelling: %v", err)
	}
}

func transport(t *testing.T, h http.HandlerFunc) (*Transport, string, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	return &Transport{Client: srv.Client()}, srv.URL, srv.Close
}

func call(spec Spec, base string, body []byte) Call {
	return Call{Spec: spec, BaseURL: base, Body: body, IdempotencyKey: "key-1", Credential: Credential{Secret: []byte("tok")}}
}

func TestSubmitClasses(t *testing.T) {
	spec := bankSpec().Normalize()
	body, _ := spec.BuildRequest(canon())
	run := func(h http.HandlerFunc) Result {
		tr, url, done := transport(t, h)
		defer done()
		return tr.Submit(context.Background(), call(spec, url, body))
	}
	json := func(w http.ResponseWriter, code int, b string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(b))
	}

	r := run(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Idempotency-Key") != "key-1" || q.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("headers: %v", q.Header)
		}
		json(w, 201, `{"id":"P-1","status":"ACCEPTED"}`)
	})
	if r.Disposition != Answered || r.Class != ClassAcknowledged || r.ProviderRef != "P-1" || r.RequestHash == "" || r.ResponseHash == "" {
		t.Fatalf("ack: %+v", r)
	}
	// provider says "success" (HTTP 200 / ACCEPTED) is an acknowledgement, never settlement: the class has no finality
	if r.Class == Class("SETTLED") {
		t.Fatal("an adapter acknowledgement must not imply finality")
	}

	r = run(func(w http.ResponseWriter, _ *http.Request) { json(w, 200, `{"id":"P-2","status":"PENDING"}`) })
	if r.Class != ClassPending {
		t.Fatalf("pending: %+v", r)
	}
	r = run(func(w http.ResponseWriter, _ *http.Request) {
		json(w, 200, `{"id":"P-3","status":"RJCT","reason":"AC04"}`)
	})
	if r.Class != ClassRejected || r.Reason != "AC04" {
		t.Fatalf("rjct: %+v", r)
	}
	r = run(func(w http.ResponseWriter, _ *http.Request) { json(w, 422, `{"reason":"bad iban"}`) })
	if r.Class != ClassRejected || r.Reason != "bad iban" {
		t.Fatalf("422: %+v", r)
	}
	r = run(func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Retry-After", "7"); json(w, 429, `{}`) })
	if r.Class != ClassRateLimited || r.RetryAfter != 7*time.Second {
		t.Fatalf("429: %+v", r)
	}
	r = run(func(w http.ResponseWriter, _ *http.Request) { json(w, 503, `{}`) })
	if r.Class != ClassUnavailable {
		t.Fatalf("503: %+v", r)
	}
	r = run(func(w http.ResponseWriter, _ *http.Request) { json(w, 401, `{}`) })
	if r.Class != ClassAuth {
		t.Fatalf("401: %+v", r)
	}
	r = run(func(w http.ResponseWriter, _ *http.Request) { json(w, 302, ``) })
	if r.Class != ClassUnavailable || r.ErrCode != "redirect" {
		t.Fatalf("302: %+v", r)
	}
	// 2xx we cannot read, or a status the profile does not define: unknown, not success
	r = run(func(w http.ResponseWriter, _ *http.Request) { json(w, 200, `<html>`) })
	if r.Class != ClassUnparseable {
		t.Fatalf("html: %+v", r)
	}
	r = run(func(w http.ResponseWriter, _ *http.Request) { json(w, 200, `{"id":"P","status":"WEIRD"}`) })
	if r.Class != ClassUnparseable || r.ErrCode != "unmapped_status" {
		t.Fatalf("weird: %+v", r)
	}
	r = run(func(w http.ResponseWriter, _ *http.Request) { json(w, 200, `{"status":"ACCEPTED"}`) })
	if r.Class != ClassUnparseable {
		t.Fatalf("no ref: %+v", r)
	}
}

// timeout after the request left: UNKNOWN, never NOT_SENT
func TestTimeoutAfterSendIsUnknown(t *testing.T) {
	spec := bankSpec().Normalize()
	body, _ := spec.BuildRequest(canon())
	block := make(chan struct{})
	tr, url, done := transport(t, func(w http.ResponseWriter, _ *http.Request) { <-block })
	defer done()
	defer close(block)
	tr.Client.Timeout = 200 * time.Millisecond
	r := tr.Submit(context.Background(), call(spec, url, body))
	if r.Disposition != Unknown {
		t.Fatalf("a timeout after the request was written is UNKNOWN: %+v", r)
	}
}

func TestDroppedConnectionIsUnknown(t *testing.T) {
	spec := bankSpec().Normalize()
	body, _ := spec.BuildRequest(canon())
	tr, url, done := transport(t, func(w http.ResponseWriter, _ *http.Request) {
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	})
	defer done()
	r := tr.Submit(context.Background(), call(spec, url, body))
	if r.Disposition != Unknown {
		t.Fatalf("%+v", r)
	}
}

func TestRefusedConnectionIsNotSent(t *testing.T) {
	spec := bankSpec().Normalize()
	body, _ := spec.BuildRequest(canon())
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	tr := &Transport{Client: &http.Client{Timeout: time.Second}}
	r := tr.Submit(context.Background(), call(spec, "http://"+addr, body))
	if r.Disposition != NotSent {
		t.Fatalf("a refused connection never carried the request: %+v", r)
	}
}

func TestSetupFailuresAreNotSent(t *testing.T) {
	spec := bankSpec().Normalize()
	body, _ := spec.BuildRequest(canon())
	tr := &Transport{Client: http.DefaultClient}
	if r := tr.Submit(context.Background(), call(spec, "ftp://x", body)); r.Disposition != NotSent {
		t.Fatalf("%+v", r)
	}
	c := call(spec, "http://127.0.0.1:1", body)
	c.Credential = Credential{}
	if r := tr.Submit(context.Background(), c); r.Disposition != NotSent || r.ErrCode != "auth_setup" {
		t.Fatalf("no credential must not send: %+v", r)
	}
}

func TestFetchNotFoundIsProof(t *testing.T) {
	spec := bankSpec().Normalize()
	tr, url, done := transport(t, func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path != "/v1/payments/by-key/key-1" || q.Method != "GET" {
			t.Errorf("path %s %s", q.Method, q.URL.Path)
		}
		w.WriteHeader(404)
	})
	defer done()
	r := tr.Fetch(context.Background(), call(spec, url, nil), "")
	if r.Class != ClassNotFound {
		t.Fatalf("%+v", r)
	}
	tr2, url2, done2 := transport(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"P-9","status":"PENDING"}`))
	})
	defer done2()
	r = tr2.Fetch(context.Background(), call(spec, url2, nil), "")
	if r.Class != ClassPending || r.ProviderRef != "P-9" {
		t.Fatalf("%+v", r)
	}
	spec.Query = nil
	if r := tr.Fetch(context.Background(), call(spec, url, nil), ""); r.Disposition != NotSent {
		t.Fatalf("%+v", r)
	}
}

func TestHMACAuthAndBodyBound(t *testing.T) {
	spec := bankSpec()
	spec.Auth = Auth{Type: "hmac", SignatureHeader: "X-Sig"}
	spec = spec.Normalize()
	body, _ := spec.BuildRequest(canon())
	tr, url, done := transport(t, func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("X-Sig") == "" {
			t.Error("no signature header")
		}
		_, _ = w.Write([]byte(strings.Repeat("a", 1000)))
	})
	defer done()
	tr.MaxBytes = 100
	c := call(spec, url, body)
	c.Signer = HMACSigner([]byte("k"))
	if r := tr.Submit(context.Background(), c); r.Class != ClassUnparseable {
		t.Fatalf("an oversized answer must not be read in full: %+v", r)
	}
	c.Signer = nil
	if r := tr.Submit(context.Background(), c); r.Disposition != NotSent {
		t.Fatalf("%+v", r)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	if parseRetryAfter("5") != 5*time.Second || parseRetryAfter("99999") != time.Hour || parseRetryAfter("x") != 0 || parseRetryAfter("-1") != 0 {
		t.Fatal("retry-after")
	}
}

func TestCallbackSchemes(t *testing.T) {
	cb := *bankSpec().Callback
	body := []byte(`{"event_id":"E1","payment_id":"P1","status":"BOOKED","at":"2026-01-02T03:04:05Z"}`)
	now := time.Now()

	// legacy hex HMAC
	sig, _ := HMACSigner([]byte("s1"))(context.Background(), body)
	ev, err := cb.VerifyCallback(CallbackHeaders{Signature: sig}, body, [][]byte{[]byte("s1")}, now)
	if err != nil || ev.EventRef != "E1" || ev.PaymentRef != "P1" || ev.Status != "SETTLED" || ev.EventTime == nil {
		t.Fatalf("%+v %v", ev, err)
	}
	// rotation: either secret verifies
	if _, err := cb.VerifyCallback(CallbackHeaders{Signature: sig}, body, [][]byte{[]byte("old"), []byte("s1")}, now); err != nil {
		t.Fatal(err)
	}
	// signature failure / tampered body / wrong length / empty secrets
	if _, err := cb.VerifyCallback(CallbackHeaders{Signature: sig}, body, [][]byte{[]byte("nope")}, now); !errors.Is(err, ErrCallbackSignature) {
		t.Fatal(err)
	}
	if _, err := cb.VerifyCallback(CallbackHeaders{Signature: sig}, append([]byte{' '}, body...), [][]byte{[]byte("s1")}, now); !errors.Is(err, ErrCallbackSignature) {
		t.Fatal(err)
	}
	if _, err := cb.VerifyCallback(CallbackHeaders{Signature: "abc"}, body, [][]byte{[]byte("s1")}, now); !errors.Is(err, ErrCallbackSignature) {
		t.Fatal(err)
	}
	if _, err := cb.VerifyCallback(CallbackHeaders{Signature: sig}, body, nil, now); !errors.Is(err, ErrCallbackSignature) {
		t.Fatal(err)
	}

	// shared timestamped scheme
	z := cb
	z.Scheme, z.ToleranceSeconds = "zoiko-v1", 300
	hdr, _ := signature.Sign(context.Background(), now, "ev-1", body, signature.LocalSigner([]byte("s1")))
	if _, err := z.VerifyCallback(CallbackHeaders{Signature: hdr, EventID: "ev-1"}, body, [][]byte{[]byte("s1")}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := z.VerifyCallback(CallbackHeaders{Signature: hdr, EventID: "ev-1"}, body, [][]byte{[]byte("s1")}, now.Add(time.Hour)); !errors.Is(err, ErrCallbackStale) {
		t.Fatalf("replay window: %v", err)
	}
	if _, err := z.VerifyCallback(CallbackHeaders{Signature: hdr, EventID: "ev-2"}, body, [][]byte{[]byte("s1")}, now); !errors.Is(err, ErrCallbackSignature) {
		t.Fatalf("event id is signed: %v", err)
	}
	if _, err := z.VerifyCallback(CallbackHeaders{Signature: hdr}, body, [][]byte{[]byte("s1")}, now); !errors.Is(err, ErrCallbackSignature) {
		t.Fatal(err)
	}
}

func TestCallbackExtraction(t *testing.T) {
	cb := *bankSpec().Callback
	for name, body := range map[string]string{
		"not json":   `nope`,
		"no event":   `{"payment_id":"P","status":"BOOKED"}`,
		"no payment": `{"event_id":"E","status":"BOOKED"}`,
		"long ref":   `{"event_id":"` + strings.Repeat("x", 200) + `","payment_id":"P","status":"BOOKED"}`,
	} {
		if _, err := cb.Extract([]byte(body)); !errors.Is(err, ErrCallbackMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := cb.Extract([]byte(`{"event_id":"E","payment_id":"P","status":"???"}`)); !errors.Is(err, ErrCallbackStatus) {
		t.Fatal(err)
	}
}

func TestSpecHashIsStable(t *testing.T) {
	a, b := bankSpec(), bankSpec()
	if a.Hash() != b.Hash() || a.Hash() == "" {
		t.Fatal("hash")
	}
	b.MaxAmount = "1.00"
	if a.Hash() == b.Hash() {
		t.Fatal("a change must change the hash")
	}
}

// found by the live run: net/http re-sends a request that carries an Idempotency-Key when the server hangs up
// on a reused connection after the request was written. The adapter must make exactly one HTTP attempt.
func TestTheLibraryNeverBlindlyRetriesARequest(t *testing.T) {
	spec := bankSpec().Normalize()
	body, _ := spec.BuildRequest(canon())
	var hits int
	var mu sync.Mutex
	tr, url, done := transport(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n == 1 { // the first request establishes a keep-alive connection
			_, _ = w.Write([]byte(`{"id":"P-1","status":"ACCEPTED"}`))
			return
		}
		c, _, _ := w.(http.Hijacker).Hijack() // later requests: the server hangs up after reading them
		_ = c.Close()
	})
	defer done()
	if r := tr.Submit(context.Background(), call(spec, url, body)); r.Class != ClassAcknowledged {
		t.Fatalf("%+v", r)
	}
	r := tr.Submit(context.Background(), call(spec, url, body))
	if r.Disposition != Unknown {
		t.Fatalf("a hang-up after the request was written is unknown: %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 2 {
		t.Fatalf("the library re-sent the request on its own: the server saw %d requests, want 2", hits)
	}
}
