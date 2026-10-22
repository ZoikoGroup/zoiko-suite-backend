package envelope

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Decision D-06 (21 Oct 2026): this service demands X-Purpose-Context on
// reads of governed decisions, not on evidence appends. Appends were refused
// for want of a purpose that only access needs.
func TestServicePolicy_PurposeOnReadsNotAppends(t *testing.T) {
	req := func(method string) *http.Request {
		r := httptest.NewRequest(method, "/v1/decisions", nil)
		for h, v := range map[string]string{
			HeaderTenantID: "11111111-1111-4111-8111-111111111111", HeaderActorSubjectID: "svc-workflow",
			HeaderRequestID: "req-1", HeaderCorrelationID: "corr-1", HeaderSourceChannel: "system",
			HeaderLegalEntityID: "22222222-2222-4222-8222-222222222222", HeaderIdempotencyKey: "k-1",
		} {
			r.Header.Set(h, v)
		}
		return r
	}
	p := ServicePolicy()
	if err := p.Validate(Parse(req(http.MethodPost)), req(http.MethodPost)); err != nil {
		t.Fatalf("an append without purpose was refused: %+v", err.Violations)
	}
	read := req(http.MethodGet)
	err := p.Validate(Parse(read), read)
	if err == nil {
		t.Fatal("a read without purpose was admitted")
	}
	found := false
	for _, v := range err.Violations {
		if v.Header == HeaderPurposeContext {
			found = true
		}
	}
	if !found {
		t.Fatalf("violations %+v do not name X-Purpose-Context", err.Violations)
	}
}
