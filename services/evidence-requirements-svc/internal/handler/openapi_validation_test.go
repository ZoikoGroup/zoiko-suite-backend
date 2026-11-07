package handler_test

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
)

// openapi.yaml is checked with a real OpenAPI implementation, not only for
// route parity: TestOpenAPIMatchesTheRouter reads lines and passed while the
// document did not load.
func loadOpenAPI(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../openapi.yaml")
	require.NoError(t, err, "openapi.yaml must load")
	return doc
}

func TestOpenAPIDocumentIsValid(t *testing.T) {
	require.NoError(t, loadOpenAPI(t).Validate(context.Background()), "openapi.yaml must be a valid OpenAPI document")
}

// Errors are problem documents (ZS-API-001 §12); requests and successes are not.
func TestOpenAPI_ProblemJSONOnlyOnErrors(t *testing.T) {
	for path, item := range loadOpenAPI(t).Paths.Map() {
		for method, op := range item.Operations() {
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				require.Nil(t, op.RequestBody.Value.Content.Get("application/problem+json"), "%s %s request body is not a problem", method, path)
			}
			for code, resp := range op.Responses.Map() {
				mt := resp.Value.Content.Get("application/problem+json")
				switch {
				case code[0] == '4' || code[0] == '5':
					if path == "/readyz" || path == "/healthz" {
						continue
					}
					require.NotNil(t, mt, "%s %s %s must be application/problem+json", method, path, code)
					require.Equal(t, "#/components/schemas/Problem", mt.Schema.Ref, "%s %s %s", method, path, code)
				default:
					require.Nil(t, mt, "%s %s %s is a success, not a problem", method, path, code)
				}
			}
		}
	}
}
