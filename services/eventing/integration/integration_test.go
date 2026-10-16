package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/eventing/envelope"
)

var t0 = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

func trusted() Trusted {
	return Trusted{TenantID: "tenant-a", LegalEntityID: "le-1", WorkloadIdentity: "svc-x",
		HomeRegion: "uk", ProcessingRegion: "uk"}
}

func validCtx(t *testing.T) Context {
	t.Helper()
	c, err := FromRequest(http.Header{}, trusted(), t0)
	if err != nil {
		t.Fatal(err)
	}
	c.Purpose = "bank-statement-import"
	c.ContractVersion = "1.0.0"
	c.DataClassification = envelope.Confidential
	c.SourceSystem = "bank-x"
	return c
}

// ---- taxonomy (§2.2) ---------------------------------------------------------

func TestTaxonomyIsCompleteAndStable(t *testing.T) {
	want := map[Code]bool{
		"INT-CTX-001": true, "INT-REG-002": true, "INT-AUTH-003": true, "INT-SCHEMA-004": true,
		"INT-IDEM-005": true, "INT-EXT-006": true, "INT-RATE-007": true, "INT-QUAR-008": true,
		"INT-SIG-009": true, "INT-CRED-010": true, "INT-RECON-011": true, "INT-VERS-012": true,
		"INT-SCOPE-013": true, "INT-FILE-014": true, "INT-CB-015": true, "INT-BP-016": true,
	}
	got := Codes()
	if len(got) != len(want) {
		t.Fatalf("taxonomy has %d codes, spec defines %d", len(got), len(want))
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected code %s", c)
		}
		i := c.Info()
		if i.Title == "" || i.Status < 200 || i.Status > 599 {
			t.Errorf("%s has incomplete info %+v", c, i)
		}
	}
}

func TestPendingUnknownIsNotReportedAsFailure(t *testing.T) {
	i := CodeExternalPending.Info()
	if i.Status != http.StatusAccepted {
		t.Fatalf("INT-EXT-006 status = %d, want 202: timeout must not read as failed", i.Status)
	}
	if i.Retryable {
		t.Fatal("pending/unknown must not be blindly retryable (INT-NP-032)")
	}
}

func TestQuarantineCodesAreFlagged(t *testing.T) {
	for _, c := range []Code{CodeQuarantined, CodeFileIntegrity} {
		if !c.Info().Quarantine {
			t.Errorf("%s must be a quarantine code", c)
		}
	}
	if CodeAuthDenied.Info().Quarantine {
		t.Error("auth denial is not a quarantine")
	}
}

func TestUndefinedCodeIsServerError(t *testing.T) {
	if Code("INT-NOPE-999").Valid() || Code("INT-NOPE-999").Info().Status != 500 {
		t.Fatal("an undefined code must never look like a client error")
	}
}

func TestErrorIsAsAndUnwrap(t *testing.T) {
	cause := errors.New("db down")
	err := error(Wrap(CodeBackpressure, "queue full", cause))
	if !errors.Is(err, New(CodeBackpressure, "")) {
		t.Error("errors.Is should match on code")
	}
	if errors.Is(err, New(CodeAuthDenied, "")) {
		t.Error("errors.Is must not match a different code")
	}
	if !errors.Is(err, cause) {
		t.Error("cause must be reachable")
	}
	if e, ok := As(err); !ok || e.Code != CodeBackpressure {
		t.Error("As failed")
	}
}

// ---- RFC 9457 --------------------------------------------------------------

func TestWriteProblemShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, "/v1/things/1", New(CodeRateLimited, "slow down").WithCorrelation("c-1").WithRetryAfter(7))
	if rec.Code != 429 {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Fatalf("content type %q", ct)
	}
	if rec.Header().Get("Retry-After") != "7" {
		t.Fatalf("Retry-After %q", rec.Header().Get("Retry-After"))
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "urn:zoikosuite:problem:INT-RATE-007" || p.Code != CodeRateLimited || !p.Retryable ||
		p.Status != 429 || p.Instance != "/v1/things/1" || p.CorrelationID != "c-1" || p.Title == "" {
		t.Fatalf("problem = %+v", p)
	}
}

func TestWriteProblemNeverLeaksCauseOrUnclassifiedText(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, "/x", Wrap(CodeAuthDenied, "denied", errors.New("password=hunter2 in dsn")))
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatal("cause leaked into problem body")
	}
	rec = httptest.NewRecorder()
	WriteProblem(rec, "/x", errors.New("pq: connection to 10.0.0.5 refused"))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("unclassified error must be opaque 500, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestRetryAfterOnlyWhenAdvised(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, "", New(CodeBackpressure, "busy"))
	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("no Retry-After advised, none should be sent")
	}
}

// ---- context (§2.1) --------------------------------------------------------

func TestForgedTenantHeaderRejected_INT_NP_001(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderTenantID, "tenant-b")
	_, err := FromRequest(h, trusted(), t0)
	e, ok := As(err)
	if !ok || e.Code != CodeContextInvalid {
		t.Fatalf("want INT-CTX-001, got %v", err)
	}
}

func TestForgedEntityHeaderRejected(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderLegalEntityID, "le-other")
	if _, err := FromRequest(h, trusted(), t0); !errors.Is(err, New(CodeContextInvalid, "")) {
		t.Fatalf("want INT-CTX-001, got %v", err)
	}
}

func TestMatchingOrAbsentClaimAccepted(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderTenantID, "tenant-a")
	h.Set(HeaderLegalEntityID, "le-1")
	h.Set(HeaderIdempotencyKey, " k-1 ")
	c, err := FromRequest(h, trusted(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if c.TenantID != "tenant-a" || c.IdempotencyKey != "k-1" || c.CorrelationID == "" {
		t.Fatalf("ctx = %+v", c)
	}
}

func TestUnresolvedTenantFailsClosed(t *testing.T) {
	_, err := FromRequest(http.Header{}, Trusted{}, t0)
	if !errors.Is(err, New(CodeContextInvalid, "")) {
		t.Fatalf("want INT-CTX-001, got %v", err)
	}
	h := http.Header{}
	h.Set(HeaderTenantID, "tenant-a")
	// a claim cannot substitute for an unresolved trusted tenant
	if _, err := FromRequest(h, Trusted{}, t0); err == nil {
		t.Fatal("header alone must never establish the tenant")
	}
}

func TestCorrelationPreservedOrGenerated(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderCorrelationID, "corr-9")
	c, _ := FromRequest(h, trusted(), t0)
	if c.CorrelationID != "corr-9" {
		t.Fatal("caller correlation id must be preserved")
	}
	a, _ := FromRequest(http.Header{}, trusted(), t0)
	b, _ := FromRequest(http.Header{}, trusted(), t0)
	if a.CorrelationID == "" || a.CorrelationID == b.CorrelationID {
		t.Fatal("missing correlation id must be generated uniquely")
	}
}

func TestValidateAcceptsCompleteContext(t *testing.T) {
	if err := validCtx(t).Validate(Rules{}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReportsEveryViolationWithoutValues(t *testing.T) {
	c := Context{TenantID: "tenant-secret-value"}
	err := c.Validate(Rules{RequireEntity: true, RequireIdempotency: true, RequireDestination: true})
	e, ok := As(err)
	if !ok || e.Code != CodeContextInvalid {
		t.Fatalf("got %v", err)
	}
	for _, f := range []string{"legal_entity_id", "actor_subject", "purpose", "home_region", "processing_region",
		"correlation_id", "idempotency_key", "contract_version", "data_classification", "source_system", "destination_system", "request_time"} {
		if !strings.Contains(e.Detail, f) {
			t.Errorf("violation list missing %s: %s", f, e.Detail)
		}
	}
	if strings.Contains(e.Detail, "tenant-secret-value") {
		t.Fatal("detail must name fields, not echo values")
	}
}

func TestValidateConditionalRules(t *testing.T) {
	c := validCtx(t)
	c.LegalEntityID = ""
	if err := c.Validate(Rules{}); err != nil {
		t.Fatalf("entity optional when not required: %v", err)
	}
	if err := c.Validate(Rules{RequireEntity: true}); err == nil {
		t.Fatal("entity required but absent must fail")
	}
	c.IdempotencyKey = ""
	if err := c.Validate(Rules{RequireIdempotency: true}); err == nil {
		t.Fatal("idempotency key required but absent must fail")
	}
}

func TestValidateRejectsFreeFormSystemAndBadValues(t *testing.T) {
	for name, mut := range map[string]func(*Context){
		"free-text source":     func(c *Context) { c.SourceSystem = "Bank X (prod)!" },
		"upper destination":    func(c *Context) { c.DestinationSystem = "Provider" },
		"bad classification":   func(c *Context) { c.DataClassification = "secret" },
		"non-UTC request time": func(c *Context) { c.RequestTime = c.RequestTime.In(time.FixedZone("x", 3600)) },
		"bad business date":    func(c *Context) { c.BusinessDate = "16/10/2026" },
		"bad region":           func(c *Context) { c.ProcessingRegion = "UK West!" },
		"blank purpose":        func(c *Context) { c.Purpose = "  " },
		"no principal":         func(c *Context) { c.WorkloadIdentity = "" },
	} {
		c := validCtx(t)
		mut(&c)
		if err := c.Validate(Rules{}); err == nil {
			t.Errorf("%s: expected violation", name)
		}
	}
}

func TestCheckResidency(t *testing.T) {
	c := validCtx(t)
	if err := c.CheckResidency([]string{"uk", "eu-west"}); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckResidency([]string{"us-east"}); !errors.Is(err, New(CodeResidencyDenied, "")) {
		t.Fatalf("want INT-REG-002, got %v", err)
	}
	if err := c.CheckResidency(nil); !errors.Is(err, New(CodeResidencyDenied, "")) {
		t.Fatal("no policy decision must deny, not allow")
	}
}

func TestLogFieldsCarryIdentifiersOnly(t *testing.T) {
	c := validCtx(t)
	c.ActorSubject = "alice@example.com"
	c.IdempotencyKey = "idem-secret"
	c.AuthzDecisionRef = "authz-1"
	f := c.LogFields()
	for k, v := range f {
		if strings.Contains(v, "alice") || strings.Contains(v, "idem-secret") {
			t.Errorf("log field %s leaks %q", k, v)
		}
	}
	if f["tenant_id"] != "tenant-a" || f["correlation_id"] == "" {
		t.Fatalf("fields = %v", f)
	}
}

func TestContextRoundTripAndInject(t *testing.T) {
	c := validCtx(t)
	c.IdempotencyKey = "k1"
	ctx := WithContext(context.Background(), c)
	got, ok := FromContext(ctx)
	if !ok || got.TenantID != c.TenantID {
		t.Fatal("context lost")
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("empty context must report absent")
	}
	h := http.Header{}
	c.Inject(h)
	if h.Get(HeaderTenantID) != "tenant-a" || h.Get(HeaderIdempotencyKey) != "k1" || h.Get(HeaderCorrelationID) != c.CorrelationID {
		t.Fatalf("headers = %v", h)
	}
}

// ---- outcome / evidence ----------------------------------------------------

func ok(s State) State { return s }

func TestOutcomeDimensionsAreSeparateFacts(t *testing.T) {
	// transport OK only: the provider has not accepted anything yet.
	o := Outcome{StateSucceeded, StateSucceeded, StateSucceeded, StatePending, StateNotApplicable, StateNotApplicable}
	if o.Certified() || o.Overall() != "PENDING" {
		t.Fatalf("transport success must not certify: %s", o.Overall())
	}
}

func TestOutcomeCannotHideReconciliationBreak_INT_NP_048(t *testing.T) {
	o := Outcome{StateSucceeded, StateSucceeded, StateSucceeded, StateSucceeded, StateSucceeded, StateFailed}
	if o.Certified() || o.Overall() != "FAILED" {
		t.Fatal("reconciliation failure must dominate")
	}
}

func TestOutcomePrecedence(t *testing.T) {
	s := StateSucceeded
	cases := []struct {
		o    Outcome
		want string
	}{
		{Outcome{s, s, s, s, s, s}, "CERTIFIED"},
		{Outcome{s, StateNotApplicable, StateNotApplicable, s, s, s}, "CERTIFIED"},
		{Outcome{s, s, s, StateUnknown, s, StatePending}, "UNKNOWN"},
		{Outcome{s, StateFailed, s, StateUnknown, s, s}, "FAILED"},
		{Outcome{s, s, s, s, StatePending, s}, "PENDING"},
		{Outcome{StateNotApplicable, StateNotApplicable, StateNotApplicable, StateNotApplicable, StateNotApplicable, StateNotApplicable}, "PENDING"},
	}
	for i, c := range cases {
		if got := c.o.Overall(); got != c.want {
			t.Errorf("case %d: %s, want %s", i, got, c.want)
		}
	}
}

func TestOutcomeRejectsUnsetDimension(t *testing.T) {
	if err := (Outcome{}).Validate(); err == nil {
		t.Fatal("blank dimensions must be explicit, not silently 'nothing to do'")
	}
}

func evSpec() EvidenceSpec {
	s := StateSucceeded
	h, _ := HashJSON(map[string]any{"a": 1})
	return EvidenceSpec{Kind: "provider_attempt", SubjectType: "attempt", SubjectID: "att-1", PayloadHash: h,
		Outcome: Outcome{s, s, s, StatePending, StateNotApplicable, StateNotApplicable}, Refs: map[string]string{"provider_ref": "P-123"}}
}

func TestNewEvidenceDerivesFromContext(t *testing.T) {
	c := validCtx(t)
	c.CausationID = "cause-1"
	ev, err := NewEvidence(c, evSpec(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if ev.TenantID != "tenant-a" || ev.CorrelationID != c.CorrelationID || ev.CausationID != "cause-1" ||
		ev.SourceSystem != "bank-x" || ev.ContractVersion != "1.0.0" || ev.Overall != "PENDING" ||
		ev.EvidenceID == "" || !ev.RecordedAt.Equal(t0) {
		t.Fatalf("evidence = %+v", ev)
	}
}

func TestNewEvidenceRefusals(t *testing.T) {
	c := validCtx(t)
	bad := map[string]func(*EvidenceSpec){
		"no subject":     func(s *EvidenceSpec) { s.SubjectID = "" },
		"bad hash":       func(s *EvidenceSpec) { s.PayloadHash = "md5:abc" },
		"unset outcome":  func(s *EvidenceSpec) { s.Outcome = Outcome{} },
		"secret ref key": func(s *EvidenceSpec) { s.Refs = map[string]string{"api_key": "x"} },
		"bearer in ref":  func(s *EvidenceSpec) { s.Refs = map[string]string{"note": "Bearer abcdefghijklmnop"} },
		"oversized ref":  func(s *EvidenceSpec) { s.Refs = map[string]string{"body": strings.Repeat("a", 300)} },
	}
	for name, mut := range bad {
		s := evSpec()
		mut(&s)
		if _, err := NewEvidence(c, s, t0); err == nil {
			t.Errorf("%s: expected refusal", name)
		}
	}
	noTenant := c
	noTenant.TenantID = ""
	if _, err := NewEvidence(noTenant, evSpec(), t0); err == nil {
		t.Error("evidence without tenant must be refused")
	}
}

func TestEvidenceJSONHasNoPayloadBody(t *testing.T) {
	ev, _ := NewEvidence(validCtx(t), evSpec(), t0)
	b, _ := json.Marshal(ev)
	if strings.Contains(string(b), `"payload":`) || strings.Contains(string(b), `"body"`) {
		t.Fatalf("evidence must hold hashes and refs, not bodies: %s", b)
	}
}

func TestHashing(t *testing.T) {
	a, _ := HashJSON(map[string]any{"b": 2, "a": 1})
	b, _ := HashJSON(map[string]any{"a": 1, "b": 2})
	if a != b || !hashPattern.MatchString(a) {
		t.Fatalf("hash unstable or malformed: %s %s", a, b)
	}
	if HashBytes([]byte("x")) == HashBytes([]byte("y")) {
		t.Fatal("different content must hash differently")
	}
	if _, err := HashJSON(make(chan int)); err == nil {
		t.Fatal("unmarshalable value must error")
	}
}

// ---- redaction (INT-CTRL-035, INT-NP-019/047) ------------------------------

func TestRedactNestedSecrets(t *testing.T) {
	in := map[string]any{
		"amount": 12.5, "currency": "GBP",
		"auth":  map[string]any{"client_secret": "s3cr3t", "scope": "read"},
		"items": []any{map[string]any{"iban": "GB82WEST12345698765432", "ref": "r1"}},
		"note":  "call failed: Bearer abcdefghijklmnop was rejected",
		"jwt":   "tok eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig",
	}
	out := Redact(in).(map[string]any)
	b, _ := json.Marshal(out)
	s := string(b)
	for _, leak := range []string{"s3cr3t", "GB82WEST", "abcdefghijklmnop", "eyJhbGci"} {
		if strings.Contains(s, leak) {
			t.Errorf("leaked %q in %s", leak, s)
		}
	}
	if out["currency"] != "GBP" || out["amount"] != 12.5 {
		t.Error("non-sensitive fields must survive")
	}
	if in["auth"].(map[string]any)["client_secret"] != "s3cr3t" {
		t.Error("Redact must not mutate its input")
	}
}

func TestScrubStringPrivateKeyAndKeyValue(t *testing.T) {
	pem := "x -----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY----- y" // gitleaks:allow -- truncated sample PEM header: the test proves private keys are scrubbed from telemetry
	if got := ScrubString(pem); strings.Contains(got, "MIIabc") {
		t.Errorf("private key leaked: %q", got)
	}
	if got := ScrubString("dsn password=hunter2 host=db"); strings.Contains(got, "hunter2") {
		t.Errorf("kv secret leaked: %q", got)
	}
	if got := ScrubString("plain status ok"); got != "plain status ok" {
		t.Errorf("clean text altered: %q", got)
	}
}

func TestRedactJSONHandlesInvalidJSONSafely(t *testing.T) {
	out := string(RedactJSON([]byte(`{"password":"p","x":1}`)))
	if strings.Contains(out, `"p"`) && !strings.Contains(out, Redacted) {
		t.Fatalf("not redacted: %s", out)
	}
	raw := "not json token=abc123 " + strings.Repeat("z", 500)
	out = string(RedactJSON([]byte(raw)))
	if strings.Contains(out, "abc123") || len(out) > 300 {
		t.Fatalf("invalid JSON must be scrubbed and truncated: %d %s", len(out), out)
	}
}

// ---- contract registry -----------------------------------------------------

const eventSchema = `{"type":"object","required":["id","amount","lines"],
 "properties":{"id":{"type":"string"},"amount":{"type":"integer"},
 "lines":{"type":"array","items":{"type":"object","required":["sku"],"properties":{"sku":{"type":"string"}}}}}}`

func TestValidatePayload(t *testing.T) {
	s := json.RawMessage(eventSchema)
	good := json.RawMessage(`{"id":"a","amount":3,"lines":[{"sku":"s"}],"extra":true}`)
	if v := ValidatePayload(s, good); len(v) != 0 {
		t.Fatalf("valid payload rejected: %v", v)
	}
	cases := map[string]string{
		"missing required":  `{"id":"a","lines":[]}`,
		"wrong type":        `{"id":"a","amount":"3","lines":[]}`,
		"non-integer":       `{"id":"a","amount":3.5,"lines":[]}`,
		"nested required":   `{"id":"a","amount":1,"lines":[{}]}`,
		"nested type":       `{"id":"a","amount":1,"lines":[{"sku":9}]}`,
		"not an object":     `[1]`,
		"null for required": `{"id":null,"amount":1,"lines":[]}`,
	}
	for name, p := range cases {
		if v := ValidatePayload(s, json.RawMessage(p)); len(v) == 0 {
			t.Errorf("%s: expected violation", name)
		}
	}
}

func TestValidatePayloadFailsClosedOnUnreadableInputs(t *testing.T) {
	if v := ValidatePayload(json.RawMessage(`nope`), json.RawMessage(`{}`)); len(v) == 0 {
		t.Error("unreadable schema must not pass")
	}
	if v := ValidatePayload(json.RawMessage(eventSchema), json.RawMessage(`{`)); len(v) == 0 {
		t.Error("unreadable payload must not pass")
	}
	if v := ValidatePayload(json.RawMessage(`{"type":"weird"}`), json.RawMessage(`{}`)); len(v) == 0 {
		t.Error("unknown declared type cannot be satisfied")
	}
}

func registryServer(t *testing.T, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if r.Header.Get("X-Principal-Id") == "" {
			http.Error(w, "no principal", 401)
			return
		}
		switch r.URL.Path {
		case "/v1/schemas/ledger.posted/versions/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"event_name": "ledger.posted", "version": 2, "json_schema": json.RawMessage(eventSchema), "compatibility_mode": "BACKWARD"})
		case "/v1/schemas/ledger.posted/versions/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"event_name": "ledger.posted", "version": 1, "json_schema": json.RawMessage(eventSchema)})
		case "/v1/schemas/ledger.posted/versions/3":
			_ = json.NewEncoder(w).Encode(map[string]any{"event_name": "ledger.posted", "version": 9, "json_schema": json.RawMessage(eventSchema)})
		case "/v1/schemas/broken.event/versions/latest":
			_, _ = w.Write([]byte(`{"version":0}`))
		case "/v1/schemas/down.event/versions/latest":
			http.Error(w, "boom", 503)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestHTTPRegistryLatestAndPinnedVersion(t *testing.T) {
	var n int32
	srv := registryServer(t, &n)
	defer srv.Close()
	reg, err := NewHTTPRegistry(srv.URL+"/", "svc-x", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s, err := reg.Schema(context.Background(), "ledger.posted", 0)
	if err != nil || s.Version != 2 {
		t.Fatalf("latest: %v %+v", err, s)
	}
	s, err = reg.Schema(context.Background(), "ledger.posted", 1)
	if err != nil || s.Version != 1 {
		t.Fatalf("pinned: %v %+v", err, s)
	}
}

func TestHTTPRegistryErrorMapping(t *testing.T) {
	var n int32
	srv := registryServer(t, &n)
	defer srv.Close()
	reg, _ := NewHTTPRegistry(srv.URL, "svc-x", time.Second)
	ctx := context.Background()

	if _, err := reg.Schema(ctx, "no.such", 0); !errors.Is(err, New(CodeSchemaMismatch, "")) {
		t.Errorf("404 must be INT-SCHEMA-004, got %v", err)
	}
	for name, ev := range map[string]string{"5xx": "down.event", "malformed": "broken.event"} {
		if _, err := reg.Schema(ctx, ev, 0); !errors.Is(err, ErrRegistryUnavailable) {
			t.Errorf("%s must be ErrRegistryUnavailable, got %v", name, err)
		}
	}
	// asked for v3, registry answered v9: never accept a different version silently (no fallback).
	if _, err := reg.Schema(ctx, "ledger.posted", 3); !errors.Is(err, ErrRegistryUnavailable) {
		t.Errorf("version substitution must be refused, got %v", err)
	}
	for _, bad := range []string{"", "a/b", "a?x=1", "a#b"} {
		before := atomic.LoadInt32(&n)
		if _, err := reg.Schema(ctx, bad, 0); err == nil {
			t.Errorf("%q must be refused", bad)
		}
		if atomic.LoadInt32(&n) != before {
			t.Errorf("%q must not reach the network", bad)
		}
	}
}

func TestHTTPRegistryUnreachableAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	reg, _ := NewHTTPRegistry(srv.URL, "svc-x", 50*time.Millisecond)
	start := time.Now()
	_, err := reg.Schema(context.Background(), "ledger.posted", 0)
	elapsed := time.Since(start)
	srv.Close()
	if !errors.Is(err, ErrRegistryUnavailable) || elapsed > 250*time.Millisecond {
		t.Fatalf("timeout must fail closed quickly: %v after %v", err, elapsed)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	reg, _ = NewHTTPRegistry(url, "svc-x", time.Second)
	if _, err := reg.Schema(context.Background(), "ledger.posted", 0); !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("connection refused must be ErrRegistryUnavailable: %v", err)
	}
}

func TestNewHTTPRegistryValidation(t *testing.T) {
	for name, args := range map[string][3]any{
		"relative url": {"/v1", "p", time.Second},
		"no principal": {"http://x", " ", time.Second},
		"no timeout":   {"http://x", "p", time.Duration(0)},
	} {
		if _, err := NewHTTPRegistry(args[0].(string), args[1].(string), args[2].(time.Duration)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

type fakeRegistry struct {
	s   *Schema
	err error
}

func (f fakeRegistry) Schema(context.Context, string, int) (*Schema, error) { return f.s, f.err }

func TestCheckPayload(t *testing.T) {
	reg := fakeRegistry{s: &Schema{EventName: "ledger.posted", Version: 2, JSONSchema: json.RawMessage(eventSchema)}}
	if _, err := CheckPayload(context.Background(), reg, "ledger.posted", 0, json.RawMessage(`{"id":"a","amount":1,"lines":[]}`)); err != nil {
		t.Fatal(err)
	}
	_, err := CheckPayload(context.Background(), reg, "ledger.posted", 0, json.RawMessage(`{"id":"a"}`))
	e, ok := As(err)
	if !ok || e.Code != CodeSchemaMismatch || !strings.Contains(e.Detail, "ledger.posted v2") {
		t.Fatalf("want INT-SCHEMA-004 naming the contract, got %v", err)
	}
	_, err = CheckPayload(context.Background(), fakeRegistry{err: ErrRegistryUnavailable}, "ledger.posted", 0, json.RawMessage(`{}`))
	if !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("unavailable registry must surface, not pass: %v", err)
	}
}

func TestRegistryPropagatesContextHeaders(t *testing.T) {
	var gotTenant, gotCorr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant, gotCorr = r.Header.Get(HeaderTenantID), r.Header.Get(HeaderCorrelationID)
		_ = json.NewEncoder(w).Encode(map[string]any{"event_name": "a.b", "version": 1, "json_schema": json.RawMessage(`{"type":"object"}`)})
	}))
	defer srv.Close()
	reg, _ := NewHTTPRegistry(srv.URL, "svc-x", time.Second)
	c := validCtx(t)
	if _, err := reg.Schema(WithContext(context.Background(), c), "a.b", 0); err != nil {
		t.Fatal(err)
	}
	if gotTenant != "tenant-a" || gotCorr != c.CorrelationID {
		t.Fatalf("headers tenant=%q corr=%q", gotTenant, gotCorr)
	}
}
