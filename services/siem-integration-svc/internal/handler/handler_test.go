package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/siem-integration-svc/internal/domain"
	"zoiko.io/siem-integration-svc/internal/handler"
	"zoiko.io/siem-integration-svc/internal/store"
)

func newRouter() http.Handler {
	return handler.NewRouter(handler.New(store.NewMemoryStore(), zap.NewNop()))
}

func TestHealthCheck(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	newRouter().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestExporterAndStreamLifecycle(t *testing.T) {
	router := newRouter()

	// Create Exporter
	expBody, _ := json.Marshal(domain.CreateExporterRequest{
		LegalEntityID: "LE-88",
		Name:          "Splunk Production HEC",
		Platform:      domain.PlatformSplunk,
		EndpointURL:   "https://splunk.corp:8088/services/collector",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/siem/exporters", bytes.NewBuffer(expBody))
	req.Header.Set("X-Tenant-ID", "t1")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body)
	}
	var exp domain.SIEMExporter
	json.Unmarshal(w.Body.Bytes(), &exp)

	// Get Exporter
	req2 := httptest.NewRequest(http.MethodGet, "/v1/siem/exporters/"+exp.ID, nil)
	req2.Header.Set("X-Tenant-ID", "t1")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("expected 200, got %d", w2.Code)
	}

	// Stream Event
	evtBody, _ := json.Marshal(domain.StreamEventRequest{
		ExporterID: exp.ID,
		SourceSvc:  "anomaly-detection-svc",
		EventType:  "ANOMALY_DETECTED",
		Severity:   domain.SeverityHigh,
		Message:    "Unusual transaction spike detected on account ACC-999",
	})
	req3 := httptest.NewRequest(http.MethodPost, "/v1/siem/stream", bytes.NewBuffer(evtBody))
	req3.Header.Set("X-Tenant-ID", "t1")
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	if w3.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w3.Code, w3.Body)
	}

	// List Events
	req4 := httptest.NewRequest(http.MethodGet, "/v1/siem/events?exporter_id="+exp.ID, nil)
	req4.Header.Set("X-Tenant-ID", "t1")
	w4 := httptest.NewRecorder()
	router.ServeHTTP(w4, req4)
	if w4.Code != 200 {
		t.Fatalf("expected 200, got %d", w4.Code)
	}
}

func TestValidationErrors(t *testing.T) {
	expBody, _ := json.Marshal(domain.CreateExporterRequest{Name: "Missing LE"})
	req := httptest.NewRequest(http.MethodPost, "/v1/siem/exporters", bytes.NewBuffer(expBody))
	req.Header.Set("X-Tenant-ID", "t1")
	w := httptest.NewRecorder()
	newRouter().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
