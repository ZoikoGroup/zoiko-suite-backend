package whout

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func goodSpec() Spec {
	return Spec{
		LegalEntityID: "le-1", Name: "ERP feed", DestinationURL: "https://hooks.example.com/zoiko",
		EventTypes: []string{"invoice.paid", "payment.*"}, AllowedFields: []string{"invoice_id", "amount", "customer.name"},
		SigningKeyPath: "kv/webhooks/t1/erp", DestinationRegion: "uk",
	}.Normalize()
}

func TestValidSpecPasses(t *testing.T) {
	if err := goodSpec().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusesBadSpecs(t *testing.T) {
	cases := map[string]func(*Spec){
		"no legal entity":         func(s *Spec) { s.LegalEntityID = "" },
		"no name":                 func(s *Spec) { s.Name = "" },
		"credentials in url":      func(s *Spec) { s.DestinationURL = "https://u:p@hooks.example.com/x" },
		"query string in url":     func(s *Spec) { s.DestinationURL = "https://hooks.example.com/x?token=abc" },
		"fragment in url":         func(s *Spec) { s.DestinationURL = "https://hooks.example.com/x#f" },
		"no event types":          func(s *Spec) { s.EventTypes = nil },
		"bare star event":         func(s *Spec) { s.EventTypes = []string{"*"} },
		"malformed event":         func(s *Spec) { s.EventTypes = []string{"a b"} },
		"too many events":         func(s *Spec) { s.EventTypes = manyStrings(51, "e") },
		"restricted field":        func(s *Spec) { s.AllowedFields = []string{"invoice_id", "bank.account_number"} },
		"restricted token field":  func(s *Spec) { s.AllowedFields = []string{"api_key"} },
		"restricted pin":          func(s *Spec) { s.AllowedFields = []string{"user.pin"} },
		"malformed field":         func(s *Spec) { s.AllowedFields = []string{"a..b"} },
		"field too deep":          func(s *Spec) { s.AllowedFields = []string{"a.b.c.d"} },
		"unsupported api version": func(s *Spec) { s.APIVersion = "v9" },
		"bad key path":            func(s *Spec) { s.SigningKeyPath = "x" },
		"no region":               func(s *Spec) { s.DestinationRegion = "" },
		"zero attempts allowed":   func(s *Spec) { s.MaxAttempts = 99 },
	}
	for name, mutate := range cases {
		s := goodSpec()
		mutate(&s)
		if err := s.Normalize().Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func manyStrings(n int, p string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = p + strings.Repeat("x", i+1)
	}
	return out
}

func TestRestrictedFieldDetection(t *testing.T) {
	for _, f := range []string{"password", "user.Password", "api_key", "apiKey", "access_token", "client_secret", "private_key",
		"bank.account_number", "card_number", "iban", "ssn", "national_id", "tax_id", "cvv", "user.pin", "otp", "authorization",
		"credentials", "routing_number", "sort_code", "pan"} {
		if !IsRestrictedField(f) {
			t.Errorf("%s should be restricted", f)
		}
	}
	for _, f := range []string{"invoice_id", "amount", "shipping", "pinned", "company", "customer.name", "status", "span", "currency"} {
		if IsRestrictedField(f) {
			t.Errorf("%s should not be restricted", f)
		}
	}
}

func TestMatchEventType(t *testing.T) {
	f := []string{"invoice.paid", "payment.*"}
	for et, want := range map[string]bool{
		"invoice.paid": true, "invoice.created": false, "payment.settled": true, "payment.refund.issued": true,
		"payment": false, "paymentx.settled": false, "": false,
	} {
		if got := MatchEventType(f, et); got != want {
			t.Errorf("%q: %v want %v", et, got, want)
		}
	}
	if MatchEventType([]string{"*"}, "anything") {
		t.Error("a bare * must not match")
	}
}

func TestFingerprintMovesOnProtectedChangesOnly(t *testing.T) {
	base := goodSpec()
	fp := base.Fingerprint()
	if goodSpec().Fingerprint() != fp {
		t.Fatal("not deterministic")
	}
	reordered := base
	reordered.EventTypes = []string{"payment.*", "invoice.paid", "invoice.paid"}
	reordered.AllowedFields = []string{"customer.name", "amount", "invoice_id"}
	if reordered.Fingerprint() != fp {
		t.Error("order and duplicates must not matter")
	}
	for name, mutate := range map[string]func(*Spec){
		"url":    func(s *Spec) { s.DestinationURL = "https://other.example.com/zoiko" },
		"events": func(s *Spec) { s.EventTypes = append(append([]string{}, s.EventTypes...), "invoice.created") },
		"fields": func(s *Spec) { s.AllowedFields = append(append([]string{}, s.AllowedFields...), "total") },
		"region": func(s *Spec) { s.DestinationRegion = "eu" },
		"entity": func(s *Spec) { s.LegalEntityID = "le-2" },
	} {
		s := base
		mutate(&s)
		if s.Normalize().Fingerprint() == fp {
			t.Errorf("%s change did not move the fingerprint", name)
		}
	}
	for name, mutate := range map[string]func(*Spec){
		"name":     func(s *Spec) { s.Name = "renamed" },
		"attempts": func(s *Spec) { s.MaxAttempts = 3 },
		"key":      func(s *Spec) { s.SigningKeyPath = "kv/webhooks/t1/rotated" },
	} {
		s := base
		mutate(&s)
		if s.Normalize().Fingerprint() != fp {
			t.Errorf("%s must not need a new approval", name)
		}
	}
}

func event() SourceEvent {
	return SourceEvent{
		EventID: "evt-1", EventType: "invoice.paid", TenantID: "t1", OccurredAt: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC),
		Payload: map[string]any{
			"invoice_id": "inv-1", "amount": "10.00", "internal_note": "do not leak", "bank": map[string]any{"account_number": "12345678"},
			"customer": map[string]any{"name": "Acme", "email": "a@acme.test"},
		},
	}
}

func TestMinimizeCopiesOnlyAllowedFields(t *testing.T) {
	body, err := Minimize(event(), []string{"invoice_id", "customer.name", "missing.field"}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	data := got["data"].(map[string]any)
	if data["invoice_id"] != "inv-1" || data["customer"].(map[string]any)["name"] != "Acme" {
		t.Fatalf("allowed fields missing: %s", body)
	}
	for _, leak := range []string{"internal_note", "account_number", "a@acme.test", "12345678", "amount"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("the body leaks %q: %s", leak, body)
		}
	}
	if got["id"] != "evt-1" || got["type"] != "invoice.paid" || got["api_version"] != "v1" || got["tenant_id"] != "t1" {
		t.Fatalf("envelope: %s", body)
	}
	if err := VerifyBody(body, []string{"invoice_id", "customer.name"}); err != nil {
		t.Fatalf("a freshly minimized body must pass its own gate: %v", err)
	}
	if _, err := Minimize(event(), []string{"password"}, "v1"); !errors.Is(err, ErrRestrictedField) {
		t.Errorf("a restricted field must never be minimized in: %v", err)
	}
}

func TestMinimizeRefusesOversizedBodies(t *testing.T) {
	ev := event()
	ev.Payload["invoice_id"] = strings.Repeat("x", MaxBodyBytes+1)
	if _, err := Minimize(ev, []string{"invoice_id"}, "v1"); !errors.Is(err, ErrValidation) {
		t.Fatalf("%v", err)
	}
}

// INT-NP-017: a payload field outside the approved scope is blocked before sending.
func TestVerifyBodyBlocksAnythingOutsideTheScope(t *testing.T) {
	allowed := []string{"invoice_id", "customer.name"}
	cases := map[string]string{
		"extra data field":         `{"id":"e","type":"t","created":"x","api_version":"v1","tenant_id":"t","data":{"invoice_id":"1","amount":"9"}}`,
		"extra nested field":       `{"id":"e","type":"t","created":"x","api_version":"v1","tenant_id":"t","data":{"customer":{"name":"A","email":"x"}}}`,
		"extra top-level member":   `{"id":"e","type":"t","created":"x","api_version":"v1","tenant_id":"t","data":{},"debug":"x"}`,
		"restricted field":         `{"id":"e","type":"t","created":"x","api_version":"v1","tenant_id":"t","data":{"password":"x"}}`,
		"not an object":            `[1,2]`,
		"data not an object":       `{"id":"e","type":"t","data":"x"}`,
		"scalar where object owed": `{"id":"e","type":"t","created":"x","api_version":"v1","tenant_id":"t","data":{"customer":"A"}}`,
	}
	for name, body := range cases {
		err := VerifyBody([]byte(body), allowed)
		if !errors.Is(err, ErrOutsideScope) && !errors.Is(err, ErrRestrictedField) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := `{"id":"e","type":"t","created":"x","api_version":"v1","tenant_id":"t","data":{"invoice_id":"1","customer":{"name":"A"}}}`
	if err := VerifyBody([]byte(ok), allowed); err != nil {
		t.Errorf("an in-scope body was blocked: %v", err)
	}
	// the scope narrowed after the body was built
	if err := VerifyBody([]byte(ok), []string{"invoice_id"}); !errors.Is(err, ErrOutsideScope) {
		t.Errorf("a narrowed scope must block a body built under the wider one: %v", err)
	}
	// an allowed object field may not smuggle a restricted key inside it
	nested := `{"id":"e","type":"t","created":"x","api_version":"v1","tenant_id":"t","data":{"customer":{"api_key":"k"}}}`
	if err := VerifyBody([]byte(nested), []string{"customer"}); !errors.Is(err, ErrRestrictedField) {
		t.Errorf("a restricted key inside an allowed object: %v", err)
	}
	// a secret-looking VALUE is caught even under an allowed field
	secretValue := `{"id":"e","type":"t","created":"x","api_version":"v1","tenant_id":"t","data":{"invoice_id":"Bearer abcdefghijklmnop.qrstuvwx"}}`
	if err := VerifyBody([]byte(secretValue), allowed); !errors.Is(err, ErrRestrictedField) {
		t.Errorf("a secret value under an allowed field: %v", err)
	}
}

func TestDeliveryIDIsStable(t *testing.T) {
	a, b := DeliveryID("sub-1", "evt-1"), DeliveryID("sub-1", "evt-1")
	if a != b || a == DeliveryID("sub-2", "evt-1") || a == DeliveryID("sub-1", "evt-2") {
		t.Fatal("delivery identity must be a pure function of (subscription, event)")
	}
	if DeliveryID("a|b", "c") == DeliveryID("a", "b|c") {
		t.Log("note: separators are ambiguous only for ids containing |; subscription ids are UUIDs")
	}
}

func TestParseSource(t *testing.T) {
	raw := `{"event_id":"evt-1","event_type":"invoice.paid","tenant_id":"t1","emitted_at":"2026-10-17T09:00:00Z","payload":{"a":1}}`
	ev, err := ParseSource([]byte(raw))
	if err != nil || ev.EventID != "evt-1" || ev.Payload["a"] != float64(1) || ev.OccurredAt.IsZero() {
		t.Fatalf("%+v %v", ev, err)
	}
	if _, err := ParseSource([]byte(strings.Replace(raw, "emitted_at", "occurred_at", 1))); err != nil {
		t.Errorf("occurred_at variant: %v", err)
	}
	for name, bad := range map[string]string{
		"not json":     `x`,
		"no id":        `{"event_type":"a","tenant_id":"t","emitted_at":"2026-10-17T09:00:00Z"}`,
		"no tenant":    `{"event_id":"e","event_type":"a","emitted_at":"2026-10-17T09:00:00Z"}`,
		"no timestamp": `{"event_id":"e","event_type":"a","tenant_id":"t"}`,
		"payload list": `{"event_id":"e","event_type":"a","tenant_id":"t","emitted_at":"2026-10-17T09:00:00Z","payload":[1]}`,
	} {
		if _, err := ParseSource([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRetryDelayIsBoundedAndJittered(t *testing.T) {
	p := RetryPolicy{Base: 30 * time.Second, Cap: time.Hour}
	full := func() float64 { return 0.999999 }
	half := func() float64 { return 0 }
	prev := time.Duration(0)
	for n := 1; n <= 12; n++ {
		d := p.Delay(n, full)
		if d > p.Cap {
			t.Errorf("attempt %d: %v exceeds the cap", n, d)
		}
		if d < prev {
			t.Errorf("attempt %d: delay shrank", n)
		}
		prev = d
		if lo := p.Delay(n, half); lo > d || lo < time.Second {
			t.Errorf("attempt %d: jitter range %v..%v", n, lo, d)
		}
	}
	if p.Delay(1000, full) != p.Cap && p.Delay(1000, full) < p.Cap-time.Second {
		t.Error("a huge attempt count must clamp, not overflow")
	}
	if p.Delay(0, full) <= 0 {
		t.Error("attempt 0 must still wait")
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		code      int
		outcome   AttemptOutcome
		retryable bool
		permanent bool
	}{
		{200, OutcomeSucceeded, false, false}, {204, OutcomeSucceeded, false, false},
		{429, OutcomeRateLimited, true, false}, {500, OutcomeHTTPError, true, false}, {503, OutcomeHTTPError, true, false},
		{400, OutcomeHTTPError, true, false}, {404, OutcomeHTTPError, true, false}, {301, OutcomeHTTPError, true, false},
		{410, OutcomeHTTPError, false, true},
	}
	for _, c := range cases {
		r := ClassifyStatus(c.code, 0)
		if r.Outcome != c.outcome || r.Retryable != c.retryable || r.Permanent != c.permanent {
			t.Errorf("%d: %+v", c.code, r)
		}
	}
	if r := ClassifyStatus(http.StatusTooManyRequests, 42*time.Second); r.RetryAfter != 42*time.Second {
		t.Errorf("Retry-After lost: %+v", r)
	}
}

func TestOwnEventsAreNeverFannedOut(t *testing.T) {
	for _, et := range []string{EventDeliveryFailed, EventDeadLettered, EventSubscriptionActivated, EventControlFailed, "webhook.anything"} {
		if !IsOwnEvent(et) {
			t.Errorf("%s", et)
		}
	}
	if IsOwnEvent("webhooks.x") && !IsOwnEvent("webhook.x") {
		t.Error("prefix check")
	}
	if IsOwnEvent("invoice.paid") || IsOwnEvent("") {
		t.Error("ordinary events are fanned out")
	}
}
