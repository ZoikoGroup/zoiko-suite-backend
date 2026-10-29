package handler_test

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// openapi.yaml is checked with a real OpenAPI implementation, not only for
// route parity: TestOpenAPIMatchesTheRouter reads lines and passed while the
// document did not load.
func loadOpenAPI(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../openapi.yaml")
	if err != nil {
		t.Fatalf("openapi.yaml does not load: %v", err)
	}
	return doc
}

func TestOpenAPIDocumentIsValid(t *testing.T) {
	if err := loadOpenAPI(t).Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI document: %v", err)
	}
}

// Errors are problem documents (ZS-API-001 §12); requests and successes are
// not. The verify-release 409 is the release decision itself
// (can_release: false), not an error report, and stays application/json.
func TestOpenAPI_ProblemJSONOnlyOnErrors(t *testing.T) {
	decision := map[string]bool{"POST /v1/workflows/{workflow_instance_id}/verify-release 409": true}
	for path, item := range loadOpenAPI(t).Paths.Map() {
		for method, op := range item.Operations() {
			if op.RequestBody != nil && op.RequestBody.Value != nil &&
				op.RequestBody.Value.Content.Get("application/problem+json") != nil {
				t.Errorf("%s %s: a request body is not a problem", method, path)
			}
			for code, resp := range op.Responses.Map() {
				mt := resp.Value.Content.Get("application/problem+json")
				switch {
				case decision[method+" "+path+" "+code], path == "/healthz", path == "/readyz":
				case code[0] == '4' || code[0] == '5':
					if mt == nil || mt.Schema == nil || mt.Schema.Ref != "#/components/schemas/Problem" {
						t.Errorf("%s %s %s must be application/problem+json with the Problem schema", method, path, code)
					}
				case mt != nil:
					t.Errorf("%s %s %s is a success, not a problem", method, path, code)
				}
			}
		}
	}
}
