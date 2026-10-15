package policyclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	svcenvelope "zoiko.io/governance-decision-log-svc/internal/envelope"
	"zoiko.io/governance-decision-log-svc/internal/policyclient"
)

// Replay's lookup used to send X-Tenant-Id and nothing else: no correlation,
// request id or principal reached policy-svc (GCP §16 propagation).
func TestGetPolicyVersion_ForwardsTheEnvelopeAndPinsTheTenant(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"policy_version_id":"pv-1","rule_payload":{}}`))
	}))
	defer srv.Close()

	ctx := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{
		TenantID:       "envelope-tenant",
		ActorSubjectID: "auditor-1",
		RequestID:      "req-9",
		CorrelationID:  "corr-9",
		SourceChannel:  svcenvelope.ChannelAPI,
	})
	if _, err := policyclient.NewHTTPClient(srv.URL).GetPolicyVersion(ctx, "decision-tenant", "pv-1"); err != nil {
		t.Fatal(err)
	}
	for header, want := range map[string]string{
		"X-Correlation-ID": "corr-9",
		"X-Request-Id":     "req-9",
		"X-Principal-Id":   "auditor-1",
		"X-Tenant-Id":      "decision-tenant",
	} {
		if got.Get(header) != want {
			t.Errorf("%s = %q, want %q", header, got.Get(header), want)
		}
	}
}
