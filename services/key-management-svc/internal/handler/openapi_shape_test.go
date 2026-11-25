package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/key-management-svc/internal/domain"
)

// The documented CustomerKey is what the service really returns (state,
// key_version, rotation_count; not status/version): every response field is
// in the schema and the response validates against it, after register and
// after rotate.
func TestOpenAPI_CustomerKeyMatchesResponse(t *testing.T) {
	schema := loadOpenAPI(t).Components.Schemas["CustomerKey"].Value
	router := newRouter()
	check := func(raw []byte) {
		t.Helper()
		var body map[string]interface{}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for k := range body {
			if _, ok := schema.Properties[k]; !ok {
				t.Errorf("response field %q is not documented in CustomerKey", k)
			}
		}
		if err := schema.VisitJSON(body); err != nil {
			t.Fatalf("response does not validate against CustomerKey: %v", err)
		}
	}
	regBody, _ := json.Marshal(domain.RegisterKeyRequest{
		LegalEntityID: "LE-1", KeyAlias: "a", KeyModel: domain.ModelBYOK,
		KeyProvider: domain.ProviderAWSKMS, ExternalKeyARN: "arn:aws:kms:eu-west-1:1:key/x",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/keys", bytes.NewBuffer(regBody))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "p-1")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", w.Code, w.Body.String())
	}
	check(w.Body.Bytes())
	var key domain.CustomerKey
	_ = json.Unmarshal(w.Body.Bytes(), &key)

	req = httptest.NewRequest(http.MethodPost, "/v1/keys/"+key.ID+"/rotate", nil)
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "p-1")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate = %d: %s", w.Code, w.Body.String())
	}
	check(w.Body.Bytes())
}
