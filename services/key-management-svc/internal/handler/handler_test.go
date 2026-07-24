package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/key-management-svc/internal/domain"
	"zoiko.io/key-management-svc/internal/handler"
	"zoiko.io/key-management-svc/internal/store"
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

func TestKeyLifecycle(t *testing.T) {
	router := newRouter()

	// Register BYOK Key
	regBody, _ := json.Marshal(domain.RegisterKeyRequest{
		LegalEntityID:  "LE-300",
		KeyAlias:       "finance-master-key",
		KeyModel:       domain.ModelBYOK,
		KeyProvider:    domain.ProviderAWSKMS,
		ExternalKeyARN: "arn:aws:kms:eu-west-1:123456789012:key/abc-123",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/keys", bytes.NewBuffer(regBody))
	req.Header.Set("X-Tenant-ID", "t1")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body)
	}

	var key domain.CustomerKey
	json.Unmarshal(w.Body.Bytes(), &key)
	if key.ID == "" {
		t.Fatal("expected key ID")
	}
	if key.KeyVersion != 1 {
		t.Fatalf("expected version 1, got %d", key.KeyVersion)
	}

	// Rotate Key
	req2 := httptest.NewRequest(http.MethodPost, "/v1/keys/"+key.ID+"/rotate", nil)
	req2.Header.Set("X-Tenant-ID", "t1")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("expected 200 on rotate, got %d", w2.Code)
	}
	var rotated domain.CustomerKey
	json.Unmarshal(w2.Body.Bytes(), &rotated)
	if rotated.KeyVersion != 2 {
		t.Fatalf("expected version 2 after rotation, got %d", rotated.KeyVersion)
	}

	// Disable Key
	req3 := httptest.NewRequest(http.MethodPost, "/v1/keys/"+key.ID+"/disable", nil)
	req3.Header.Set("X-Tenant-ID", "t1")
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	if w3.Code != 200 {
		t.Fatalf("expected 200 on disable, got %d", w3.Code)
	}
}

func TestValidationErrors(t *testing.T) {
	// Missing external_key_arn for BYOK
	regBody, _ := json.Marshal(domain.RegisterKeyRequest{
		LegalEntityID: "LE-300",
		KeyAlias:      "bad-key",
		KeyModel:      domain.ModelBYOK,
		KeyProvider:   domain.ProviderAWSKMS,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/keys", bytes.NewBuffer(regBody))
	req.Header.Set("X-Tenant-ID", "t1")
	w := httptest.NewRecorder()
	newRouter().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
