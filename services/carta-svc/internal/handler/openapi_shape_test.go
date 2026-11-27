package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/carta-svc/internal/handler"
	"zoiko.io/carta-svc/internal/store"
)

// The documented CartaAssessment is what /evaluate really returns: every
// field the response carries is in the schema, every required schema field is
// in the response, and the values validate.
func TestOpenAPI_CartaAssessmentMatchesResponse(t *testing.T) {
	schema := loadOpenAPI(t).Components.Schemas["CartaAssessment"].Value
	r := handler.NewRouter(handler.New(store.NewMemoryStore(), &recordingAuthz{}, zap.NewNop()))
	w := evaluateWithPrincipal(r, "alice")
	if w.Code != http.StatusCreated {
		t.Fatalf("evaluate = %d", w.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for k := range body {
		if _, ok := schema.Properties[k]; !ok {
			t.Errorf("response field %q is not documented in CartaAssessment", k)
		}
	}
	if err := schema.VisitJSON(body); err != nil {
		t.Fatalf("response does not validate against CartaAssessment: %v", err)
	}
}
