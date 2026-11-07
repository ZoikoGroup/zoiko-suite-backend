package handler_test

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// access-control-svc API-2 gate: openapi.yaml must load as a valid OpenAPI
// document (every $ref resolves, every schema well-formed) so a generated
// client can consume it, and every served route must be declared.
func loadValidatedSpec(t *testing.T) *openapi3.T {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(filepath.Join(filepath.Dir(thisFile), "../../openapi.yaml"))
	if err != nil {
		t.Fatalf("openapi.yaml does not load: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI document: %v", err)
	}
	return doc
}

func TestOpenAPI_DocumentIsValid(t *testing.T) {
	loadValidatedSpec(t)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}
