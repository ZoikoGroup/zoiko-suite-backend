package apicontract

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestSetLocationSanitizesAndIgnoresEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	SetLocation(rec, "/v1/invoices/inv_1")
	if got := rec.Header().Get("Location"); got != "/v1/invoices/inv_1" {
		t.Fatalf("location = %q", got)
	}
	rec = httptest.NewRecorder()
	SetLocation(rec, "/v1/invoices\r\nX-Injected: yes")
	if got := rec.Header().Get("Location"); got != "/v1/invoicesX-Injected: yes" {
		t.Fatalf("location = %q", got)
	}
	rec = httptest.NewRecorder()
	SetLocation(rec, "  ")
	if got := rec.Header().Get("Location"); got != "" {
		t.Fatalf("empty location was set: %q", got)
	}
}

func TestResourceAndOperationLocation(t *testing.T) {
	if got := ResourceLocation("/v1/invoices/", "inv 1"); got != "/v1/invoices/inv%201" {
		t.Fatalf("location = %q", got)
	}
	if got := OperationLocation("op_1"); got != "/v1/operations/op_1" {
		t.Fatalf("operation location = %q", got)
	}
}

func TestCreatedWrites201AndBody(t *testing.T) {
	rec := httptest.NewRecorder()
	Created(rec, "/v1/invoices/inv_1", map[string]string{"id": "inv_1"})
	if rec.Code != 201 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/v1/invoices/inv_1" {
		t.Fatalf("location = %q", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["id"] != "inv_1" {
		t.Fatalf("body = %v, err = %v", body, err)
	}
}
