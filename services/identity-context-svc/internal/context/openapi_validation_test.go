package context_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
)

// openapiValidationGate: openapi.yaml must load as a valid OpenAPI document
// (every $ref resolves, every schema well-formed) so a generated client can
// consume it. Mirrors the kin-openapi gate added to the other API-2 services.
func TestOpenAPIDocumentIsValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	require.NoError(t, err, "openapi.yaml must be present")

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(raw)
	require.NoError(t, err, "openapi.yaml must load")
	require.NotNil(t, doc)

	require.NoError(t, doc.Validate(context.Background()),
		"openapi.yaml must be a valid OpenAPI document")
}
