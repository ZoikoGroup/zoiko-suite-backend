package notifyclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/workflow-svc/internal/notify"
	"zoiko.io/workflow-svc/internal/outbox"
)

const (
	tenant = "11111111-1111-1111-1111-111111111111"
	entity = "22222222-2222-2222-2222-222222222222"
	wfID   = "33333333-3333-4333-8333-333333333333"
	rowID  = "44444444-4444-4444-8444-444444444444"
)

func storedNotice(t *testing.T, n notify.Notice, corr *string) outbox.StoredEvent {
	t.Helper()
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	return outbox.StoredEvent{OutboxEventID: rowID, AggregateType: "workflow_instance", AggregateID: wfID,
		EventType: n.SourceEventType, TenantID: tenant, LegalEntityID: entity, CorrelationID: corr, Payload: raw}
}

func notice() notify.Notice {
	return notify.Notice{RecipientPrincipalID: "approver-a", Channel: notify.ChannelInApp,
		Subject: "Approval needed: Purchase approval", Body: "A request is waiting for your decision.",
		SourceEventType: "workflow.approval_requested", SourceReference: wfID}
}

func TestDeliver_SendsTheNoticeAsTheServicePrincipal(t *testing.T) {
	var gotHeaders http.Header
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/notifications" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotHeaders = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	corr := "corr-1"
	if err := New(srv.URL+"/", "svc-workflow").Deliver(context.Background(), storedNotice(t, notice(), &corr)); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	for h, want := range map[string]string{
		"X-Tenant-Id": tenant, "X-Principal-Id": "svc-workflow", "X-Workload-Id": "workflow-svc",
		"X-Legal-Entity-Id": entity, "X-Correlation-ID": corr, "X-Request-Id": rowID,
		"X-Source-Channel": "system", "X-Workflow-Instance-Id": wfID,
		"Idempotency-Key": "workflow-notice:" + rowID, "Content-Type": "application/json",
	} {
		if gotHeaders.Get(h) != want {
			t.Errorf("header %s = %q, want %q", h, gotHeaders.Get(h), want)
		}
	}
	for k, want := range map[string]string{
		"recipient_principal_id": "approver-a", "legal_entity_id": entity, "channel": "IN_APP",
		"subject": "Approval needed: Purchase approval", "body": "A request is waiting for your decision.",
		"source_event_type": "workflow.approval_requested", "source_reference": wfID,
		"correlation_id": corr, "idempotency_key": "workflow-notice:" + rowID, "communication_class": "T0",
	} {
		if got[k] != want {
			t.Errorf("body %s = %v, want %q", k, got[k], want)
		}
	}
}

func TestDeliver_NoCorrelationUsesTheWorkflow(t *testing.T) {
	var corr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		corr = r.Header.Get("X-Correlation-ID")
		w.WriteHeader(http.StatusOK) // an idempotent replay
	}))
	defer srv.Close()
	if err := New(srv.URL, "svc-workflow").Deliver(context.Background(), storedNotice(t, notice(), nil)); err != nil {
		t.Fatalf("a replay (200) is delivered: %v", err)
	}
	if corr != wfID {
		t.Fatalf("correlation falls back to the workflow id, got %q", corr)
	}
}

func TestDeliver_Outcomes(t *testing.T) {
	cases := []struct {
		status    int
		ok        bool
		permanent bool
	}{
		{http.StatusCreated, true, false},
		{http.StatusOK, true, false},
		{http.StatusConflict, false, true},
		{http.StatusBadRequest, false, false},
		{http.StatusForbidden, false, false},
		{http.StatusTooManyRequests, false, false},
		{http.StatusServiceUnavailable, false, false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		err := New(srv.URL, "svc-workflow").Deliver(context.Background(), storedNotice(t, notice(), nil))
		srv.Close()
		if tc.ok != (err == nil) {
			t.Fatalf("%d: ok=%v, err=%v", tc.status, tc.ok, err)
		}
		if errors.Is(err, outbox.ErrPermanent) != tc.permanent {
			t.Fatalf("%d: permanent=%v, err=%v", tc.status, tc.permanent, err)
		}
	}
}

func TestDeliver_UnreachableRetries(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	err := New(url, "svc-workflow").Deliver(context.Background(), storedNotice(t, notice(), nil))
	if err == nil || errors.Is(err, outbox.ErrPermanent) {
		t.Fatalf("an unreachable notification-svc is retried, got %v", err)
	}
}

func TestDeliver_BadRowsParkWithoutACall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	d := New(srv.URL, "svc-workflow")

	bad := storedNotice(t, notice(), nil)
	bad.Payload = []byte("{not json")
	if err := d.Deliver(context.Background(), bad); !errors.Is(err, outbox.ErrPermanent) {
		t.Fatalf("an undecodable row parks, got %v", err)
	}
	empty := notice()
	empty.RecipientPrincipalID = ""
	if err := d.Deliver(context.Background(), storedNotice(t, empty, nil)); !errors.Is(err, outbox.ErrPermanent) {
		t.Fatalf("a notice without a recipient parks, got %v", err)
	}
	if called {
		t.Fatal("a bad row must not reach notification-svc")
	}
}

func TestDeliver_NoPrincipalStaysPending(t *testing.T) {
	err := New("http://unused", "").Deliver(context.Background(), storedNotice(t, notice(), nil))
	if err == nil || errors.Is(err, outbox.ErrPermanent) {
		t.Fatalf("without a principal the notice waits (retryable), got %v", err)
	}
}
