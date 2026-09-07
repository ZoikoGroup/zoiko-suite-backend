package envelope

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fullEnvelope is a request carrying every §4 field, used as the baseline that
// individual tests degrade one field at a time.
func fullEnvelope(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	h := r.Header
	h.Set(HeaderTenantID, "tenant-01")
	h.Set(HeaderActorSubjectID, "user-01")
	h.Set(HeaderLegalEntityID, "entity-01")
	h.Set(HeaderBookID, "book-statutory")
	h.Set(HeaderOperation, "CREATE_JOURNAL")
	h.Set(HeaderRequestID, "req-01")
	h.Set(HeaderCorrelationID, "corr-01")
	h.Set(HeaderCausationID, "cause-01")
	h.Set(HeaderIdempotencyKey, "idem-01")
	h.Set(HeaderSourceChannel, "web")
	h.Set(HeaderOccurredAt, "2026-08-24T09:30:00Z")
	h.Set(HeaderEffectiveAt, "2026-08-31T00:00:00Z")
	h.Set(HeaderTimezone, "Europe/London")
	h.Set(HeaderJurisdictionContext, "GB")
	h.Set(HeaderPurposeContext, "STATUTORY_REPORTING")
	h.Set(HeaderExpectedVersion, "7")
	h.Set(HeaderWorkflowInstanceID, "wf-01")
	h.Set(HeaderApprovalReference, "appr-01")
	h.Set(HeaderEvidenceRefs, "doc-1, doc-2 ,, doc-3")
	return r
}

func TestParseReadsEveryCanonicalField(t *testing.T) {
	e := Parse(fullEnvelope(http.MethodPost, "/v1/journals"))

	if e.TenantID != "tenant-01" || e.ActorSubjectID != "user-01" || e.LegalEntityID != "entity-01" {
		t.Fatalf("identity fields not parsed: %+v", e)
	}
	if e.BookID != "book-statutory" || e.Operation != "CREATE_JOURNAL" || e.IdempotencyKey != "idem-01" {
		t.Fatalf("accounting/command fields not parsed: %+v", e)
	}
	if e.SourceChannel != ChannelWeb {
		t.Fatalf("source_channel = %q, want web", e.SourceChannel)
	}
	if e.OccurredAt == nil || !e.OccurredAt.Equal(time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("occurred_at = %v, want 2026-08-24T09:30:00Z", e.OccurredAt)
	}
	// Empty members are dropped, so a trailing or doubled comma cannot produce
	// an evidence reference that points at nothing.
	if got := e.EvidenceRefs; len(got) != 3 || got[0] != "doc-1" || got[2] != "doc-3" {
		t.Fatalf("evidence_refs = %v, want [doc-1 doc-2 doc-3]", got)
	}
}

func TestOperationFallsBackToRoute(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/journals", nil)
	if got := Parse(r).Operation; got != "POST /v1/journals" {
		t.Fatalf("operation = %q, want the invoked route", got)
	}
}

func TestActorPrefersSubjectOverWorkload(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/journals", nil)
	r.Header.Set(HeaderWorkloadID, "svc-poster")
	if got := Parse(r).Actor(); got != "svc-poster" {
		t.Fatalf("workload-only actor = %q, want svc-poster", got)
	}
	r.Header.Set(HeaderActorSubjectID, "user-01")
	if got := Parse(r).Actor(); got != "user-01" {
		t.Fatalf("actor = %q, want the human subject to win", got)
	}
}

// A malformed timestamp must not read as an absent one — that is the difference
// between refusing a corrupt business-event time and silently dropping it.
func TestMalformedTimestampIsRefusedNotDropped(t *testing.T) {
	r := fullEnvelope(http.MethodPost, "/v1/journals")
	r.Header.Set(HeaderOccurredAt, "24/08/2026")

	e := Parse(r)
	if e.OccurredAt != nil {
		t.Fatal("malformed occurred_at should not produce a time")
	}
	err := Policy{ServiceName: "test"}.Validate(e, r)
	if err == nil {
		t.Fatal("malformed occurred_at was accepted")
	}
	if !hasField(err, "occurred_at") {
		t.Fatalf("violations = %+v, want occurred_at", err.Violations)
	}
}

func TestValidateAcceptsFullEnvelope(t *testing.T) {
	r := fullEnvelope(http.MethodPost, "/v1/journals")
	p := Policy{
		ServiceName:   "general-ledger-svc",
		LegalEntityID: Required,
		BookID:        Required,
	}
	if err := p.Validate(Parse(r), r); err != nil {
		t.Fatalf("full envelope refused: %v (%+v)", err, err.Violations)
	}
}

func TestValidateReportsEveryMissingMandatoryFieldAtOnce(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/journals", nil)
	err := Policy{ServiceName: "general-ledger-svc"}.Validate(Parse(r), r)
	if err == nil {
		t.Fatal("empty envelope accepted")
	}
	// All five unconditional fields plus idempotency_key, in one refusal — a
	// one-at-a-time refusal turns adoption into six failed round trips.
	for _, want := range []string{"tenant_id", "actor_subject_id", "request_id", "correlation_id", "source_channel", "idempotency_key"} {
		if !hasField(err, want) {
			t.Errorf("missing violation for %s; got %+v", want, err.Violations)
		}
	}
}

func TestIdempotencyKeyCannotBeOptedOut(t *testing.T) {
	r := fullEnvelope(http.MethodPost, "/v1/journals")
	r.Header.Del(HeaderIdempotencyKey)

	// A policy that leaves IdempotencyKey at its zero value still gets INV-08.
	err := Policy{ServiceName: "test"}.Validate(Parse(r), r)
	if err == nil || !hasField(err, "idempotency_key") {
		t.Fatal("a service was able to opt out of INV-08 replay protection")
	}
}

func TestIdempotencyKeyNotRequiredOnReads(t *testing.T) {
	r := fullEnvelope(http.MethodGet, "/v1/journals")
	r.Header.Del(HeaderIdempotencyKey)
	if err := (Policy{ServiceName: "test"}).Validate(Parse(r), r); err != nil {
		t.Fatalf("read refused for missing idempotency key: %+v", err.Violations)
	}
}

// PUT and DELETE are idempotent at the HTTP level but not at the accounting
// level, so they must still carry a key.
func TestPutAndDeleteCountAsMaterialWrites(t *testing.T) {
	for _, m := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		r := fullEnvelope(m, "/v1/entity-jurisdictions/ej-1")
		r.Header.Del(HeaderIdempotencyKey)
		if err := (Policy{ServiceName: "test"}).Validate(Parse(r), r); err == nil {
			t.Errorf("%s admitted without an idempotency key", m)
		}
	}
}

func TestUnknownSourceChannelIsRefused(t *testing.T) {
	r := fullEnvelope(http.MethodPost, "/v1/journals")
	r.Header.Set(HeaderSourceChannel, "carrier-pigeon")
	err := Policy{ServiceName: "test"}.Validate(Parse(r), r)
	if err == nil || !hasField(err, "source_channel") {
		t.Fatal("unrecognised source_channel was accepted")
	}
}

// §4 makes source_system mandatory for imported/integrated records, and the
// channel is what says the record is one.
func TestExternalChannelForcesSourceSystem(t *testing.T) {
	for _, ch := range []string{"import", "integration"} {
		r := fullEnvelope(http.MethodPost, "/v1/invoices")
		r.Header.Set(HeaderSourceChannel, ch)
		err := Policy{ServiceName: "test"}.Validate(Parse(r), r)
		if err == nil || !hasField(err, "source_system") {
			t.Errorf("channel %s admitted with no source_system", ch)
		}

		r.Header.Set(HeaderSourceSystem, "sap-erp")
		if err := (Policy{ServiceName: "test"}).Validate(Parse(r), r); err != nil {
			t.Errorf("channel %s with source_system refused: %+v", ch, err.Violations)
		}
	}
}

func TestWebChannelDoesNotForceSourceSystem(t *testing.T) {
	r := fullEnvelope(http.MethodPost, "/v1/invoices")
	if err := (Policy{ServiceName: "test"}).Validate(Parse(r), r); err != nil {
		t.Fatalf("web request refused: %+v", err.Violations)
	}
}

func TestBookIDAcceptsEitherBookOrReportingBasis(t *testing.T) {
	p := Policy{ServiceName: "general-ledger-svc", BookID: Required}

	r := fullEnvelope(http.MethodPost, "/v1/journals")
	r.Header.Del(HeaderBookID)
	if err := p.Validate(Parse(r), r); err == nil || !hasField(err, "book_id") {
		t.Fatal("posting admitted with neither book_id nor reporting_basis (INV-03)")
	}

	r.Header.Set(HeaderReportingBasis, "IFRS")
	if err := p.Validate(Parse(r), r); err != nil {
		t.Fatalf("reporting_basis alone refused: %+v", err.Violations)
	}
}

// Missing gateway-set identity is an authentication failure, not a payload one.
func TestStatusIs401ForIdentityAnd400ForContract(t *testing.T) {
	r := fullEnvelope(http.MethodPost, "/v1/journals")
	r.Header.Del(HeaderTenantID)
	err := Policy{ServiceName: "test"}.Validate(Parse(r), r)
	if got := StatusFor(err); got != http.StatusUnauthorized {
		t.Fatalf("missing tenant_id -> %d, want 401", got)
	}

	r2 := fullEnvelope(http.MethodPost, "/v1/journals")
	r2.Header.Del(HeaderRequestID)
	err2 := Policy{ServiceName: "test"}.Validate(Parse(r2), r2)
	if got := StatusFor(err2); got != http.StatusBadRequest {
		t.Fatalf("missing request_id -> %d, want 400", got)
	}
}

func TestProbesAreExemptSoServicesStayLive(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz", "/health"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if err := (Policy{ServiceName: "test"}).Validate(Parse(r), r); err != nil {
			t.Errorf("%s refused: %+v", path, err.Violations)
		}
	}
}

func TestMiddlewareWriteStrictRefusesWritesAndAdmitsReads(t *testing.T) {
	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if _, ok := FromContext(r.Context()); !ok {
			t.Error("envelope not placed on context")
		}
		w.WriteHeader(http.StatusOK)
	})
	mw := MiddlewareWithMode(Policy{ServiceName: "test"}, ModeWriteStrict, nil)(next)

	reached = false
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/journals", nil))
	if w.Code != http.StatusUnauthorized || reached {
		t.Fatalf("bare write: status %d reached=%v, want 401 and no handler", w.Code, reached)
	}

	reached = false
	w = httptest.NewRecorder()
	mw.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/journals", nil))
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("bare read: status %d reached=%v, want 200 and handler reached", w.Code, reached)
	}
	if w.Header().Get("X-Envelope-Contract") != "violated" {
		t.Error("admitted read was not marked out of contract")
	}
}

func TestMiddlewareObserveNeverRefuses(t *testing.T) {
	var reported bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	mw := MiddlewareWithMode(Policy{ServiceName: "test"}, ModeObserve,
		func(*http.Request, Envelope, *ValidationError) { reported = true })(next)

	w := httptest.NewRecorder()
	mw.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/journals", nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("observe mode refused a write: %d", w.Code)
	}
	if !reported {
		t.Error("observe mode did not report the violation")
	}
}

func TestMiddlewareStrictRefusesReadsToo(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mw := MiddlewareWithMode(Policy{ServiceName: "test"}, ModeStrict, nil)(next)

	w := httptest.NewRecorder()
	mw.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/journals", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("strict mode admitted a bare read: %d", w.Code)
	}
}

// The refusal body keeps violations structured. Folding them into one sentence
// is what stops a caller telling which of five headers it is missing.
func TestRefusalBodyListsViolationsStructurally(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	mw := MiddlewareWithMode(Policy{ServiceName: "general-ledger-svc"}, ModeStrict, nil)(next)

	w := httptest.NewRecorder()
	mw.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/journals", nil))

	var body struct {
		Error      string      `json:"error"`
		Service    string      `json:"service"`
		Violations []Violation `json:"violations"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("refusal body is not JSON: %v", err)
	}
	if body.Error != "envelope_incomplete" || body.Service != "general-ledger-svc" {
		t.Fatalf("unexpected refusal envelope: %+v", body)
	}
	if len(body.Violations) < 5 {
		t.Fatalf("violations = %d, want every unmet field named", len(body.Violations))
	}
	for _, v := range body.Violations {
		if v.Field == "" || v.Header == "" || v.Reason == "" {
			t.Errorf("violation missing guidance: %+v", v)
		}
	}
}

func TestRequestIDIsEchoedEvenOnRefusal(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	mw := MiddlewareWithMode(Policy{ServiceName: "test"}, ModeStrict, nil)(next)

	r := httptest.NewRequest(http.MethodPost, "/v1/journals", nil)
	r.Header.Set(HeaderRequestID, "req-42")
	r.Header.Set(HeaderCorrelationID, "corr-42")

	w := httptest.NewRecorder()
	mw.ServeHTTP(w, r)

	if w.Header().Get(HeaderRequestID) != "req-42" || w.Header().Get(HeaderCorrelationID) != "corr-42" {
		t.Fatal("trace identifiers not echoed on a refusal, so the refused request cannot be found in logs")
	}
}

func TestResolveModeDefaultsToWriteStrictOnGarbage(t *testing.T) {
	t.Setenv(EnvVarMode, "STRICT")
	if got := ResolveMode(); got != ModeStrict {
		t.Fatalf("ResolveMode() = %q, want strict (case-insensitive)", got)
	}
	// A typo must not silently disable the control.
	t.Setenv(EnvVarMode, "strictt")
	if got := ResolveMode(); got != ModeWriteStrict {
		t.Fatalf("ResolveMode() on typo = %q, want the write-strict default", got)
	}
	t.Setenv(EnvVarMode, "observe")
	if got := ResolveMode(); got != ModeObserve {
		t.Fatalf("ResolveMode() = %q, want observe", got)
	}
}

func hasField(err *ValidationError, field string) bool {
	if err == nil {
		return false
	}
	for _, v := range err.Violations {
		if v.Field == field {
			return true
		}
	}
	return false
}
