package decisionlog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/workflow-svc/internal/decisionlog"
	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/outbox"
)

func ledgerEvent(t *testing.T, rec domain.LedgerRecord) outbox.StoredEvent {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return outbox.StoredEvent{OutboxEventID: rec.DecisionID, EventType: "governance.decision.record", Payload: b}
}

func sampleRecord() domain.LedgerRecord {
	return domain.LedgerRecord{
		DecisionID: "11111111-1111-4111-8111-000000000001", TenantID: "t-1",
		LegalEntityID: "22222222-2222-4222-8222-000000000002", ActionType: "WORKFLOW_APPROVE",
		Outcome: "APPROVED", RuleBasis: "workflow:PAYMENT access_decision:ad-1", CorrelationID: "corr-1",
		WorkflowInstanceID: "w-1", JurisdictionContext: "IN",
		EvaluationContext: map[string]any{"actor_principal_id": "approver-1", "access_decision_id": "ad-1"},
		DecidedAt:         time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), ActorPrincipalID: "approver-1",
	}
}

// The record reaches the ledger in its wire shape, written by workflow-svc's
// own principal, with the decider in evaluation_context and a deterministic
// Idempotency-Key so a redelivery replays.
func TestLedgerDeliverer_SendsTheRecordAsTheServicePrincipal(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/decisions" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		hdr = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	rec := sampleRecord()
	if err := decisionlog.NewLedgerDeliverer(srv.URL, "svc-workflow").Deliver(context.Background(), ledgerEvent(t, rec)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	for h, want := range map[string]string{
		"X-Principal-Id": "svc-workflow", "X-Tenant-Id": "t-1", "X-Legal-Entity-Id": rec.LegalEntityID,
		"X-Correlation-ID": "corr-1", "X-Source-Channel": "system", "X-Purpose-Context": decisionlog.LedgerPurposeContext,
		"Idempotency-Key": "workflow-ledger:" + rec.DecisionID, "X-Jurisdiction-Context": "IN",
	} {
		if hdr.Get(h) != want {
			t.Errorf("%s = %q, want %q", h, hdr.Get(h), want)
		}
	}
	if got["decision_id"] != rec.DecisionID || got["outcome"] != "APPROVED" || got["workflow_instance_id"] != "w-1" {
		t.Errorf("body = %v", got)
	}
	if _, ok := got["tenant_id"]; ok {
		t.Error("body names tenant_id; the ledger files under the verified X-Tenant-Id")
	}
	ev, _ := got["evaluation_context"].(map[string]any)
	if ev["actor_principal_id"] != "approver-1" || ev["access_decision_id"] != "ad-1" {
		t.Errorf("evaluation_context = %v", ev)
	}
	if got["decided_at"] != "2026-10-21T09:00:00Z" {
		t.Errorf("decided_at = %v, want the decision's own time (a replay must match)", got["decided_at"])
	}
}

func TestLedgerDeliverer_ReplayIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	if err := decisionlog.NewLedgerDeliverer(srv.URL, "svc-workflow").Deliver(context.Background(), ledgerEvent(t, sampleRecord())); err != nil {
		t.Fatalf("a 200 replay is delivered, got %v", err)
	}
}

func TestLedgerDeliverer_ConflictIsPermanent_OthersRetry(t *testing.T) {
	for status, permanent := range map[int]bool{
		http.StatusConflict: true, http.StatusForbidden: false, http.StatusBadRequest: false, http.StatusServiceUnavailable: false,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		err := decisionlog.NewLedgerDeliverer(srv.URL, "svc-workflow").Deliver(context.Background(), ledgerEvent(t, sampleRecord()))
		srv.Close()
		if err == nil {
			t.Errorf("%d: delivered", status)
			continue
		}
		if errors.Is(err, outbox.ErrPermanent) != permanent {
			t.Errorf("%d: permanent = %v, want %v (%v)", status, !permanent, permanent, err)
		}
	}
}

// Without a configured writer the record waits (retry), it is not dropped.
func TestLedgerDeliverer_NoPrincipalWaits(t *testing.T) {
	err := decisionlog.NewLedgerDeliverer("http://127.0.0.1:1", "").Deliver(context.Background(), ledgerEvent(t, sampleRecord()))
	if err == nil || errors.Is(err, outbox.ErrPermanent) {
		t.Fatalf("err = %v, want a retryable error", err)
	}
}

// The ledger requires a correlation id; a decision without one is filed under
// its workflow instance rather than refused 400 forever.
func TestLedgerDeliverer_MissingCorrelationUsesTheInstance(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	rec := sampleRecord()
	rec.CorrelationID = ""
	if err := decisionlog.NewLedgerDeliverer(srv.URL, "svc-workflow").Deliver(context.Background(), ledgerEvent(t, rec)); err != nil {
		t.Fatal(err)
	}
	if got["correlation_id"] != "w-1" {
		t.Errorf("correlation_id = %v, want w-1", got["correlation_id"])
	}
}
