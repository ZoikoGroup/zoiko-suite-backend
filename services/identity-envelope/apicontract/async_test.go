package apicontract

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOperationStateTransitions(t *testing.T) {
	allowed := [][2]OperationState{
		{OperationQueued, OperationRunning},
		{OperationQueued, OperationCancelRequested},
		{OperationQueued, OperationFailed},
		{OperationQueued, OperationCancelled},
		{OperationRunning, OperationWaitingExternal},
		{OperationRunning, OperationSucceeded},
		{OperationRunning, OperationFailed},
		{OperationRunning, OperationCancelRequested},
		{OperationWaitingExternal, OperationRunning},
		{OperationWaitingExternal, OperationSucceeded},
		{OperationWaitingExternal, OperationCancelRequested},
		{OperationCancelRequested, OperationCancelled},
		{OperationCancelRequested, OperationSucceeded},
		{OperationCancelRequested, OperationFailed},
	}
	for _, pair := range allowed {
		if !CanTransition(pair[0], pair[1]) {
			t.Errorf("%s -> %s should be allowed", pair[0], pair[1])
		}
	}
	denied := [][2]OperationState{
		{OperationSucceeded, OperationRunning},
		{OperationFailed, OperationQueued},
		{OperationCancelled, OperationCancelRequested},
		{OperationQueued, OperationSucceeded},
		{OperationRunning, OperationQueued},
		{OperationQueued, "bogus"},
	}
	for _, pair := range denied {
		if CanTransition(pair[0], pair[1]) {
			t.Errorf("%s -> %s should be denied", pair[0], pair[1])
		}
	}
	if !CanTransition(OperationRunning, OperationRunning) {
		t.Error("same-state progress updates should be allowed")
	}
}

func TestOperationValidate(t *testing.T) {
	now := time.Now().UTC()
	valid := Operation{ID: "op_1", TenantID: "t1", Type: "export", State: OperationQueued, CreatedAt: now}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid op rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Operation){
		"no-id":     func(o *Operation) { o.ID = "" },
		"no-tenant": func(o *Operation) { o.TenantID = "" },
		"no-type":   func(o *Operation) { o.Type = "" },
		"bad-state": func(o *Operation) { o.State = "done" },
	} {
		o := valid
		mutate(&o)
		if err := o.Validate(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestWriteOperationAcceptedSetsLocation(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteOperation(rec, 202, Operation{ID: "op_1", Type: "export", State: OperationQueued,
		CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC()})
	if rec.Code != 202 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/v1/operations/op_1" {
		t.Fatalf("location = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["state"] != "queued" || body["type"] != "export" {
		t.Fatalf("body = %v", body)
	}
}

func TestNewOperationID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewOperationID()
		if len(id) < 10 || id[:3] != "op_" {
			t.Fatalf("id = %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
