package specs

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var updateSchemas = flag.Bool("update-schemas", false, "rewrite api/schemas from api/openapi.yaml")

// rewriteRefs points component refs at the sibling schema files.
func rewriteRefs(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if s, ok := val.(string); k == "$ref" && ok && strings.HasPrefix(s, "#/components/schemas/") {
				out[k] = strings.TrimPrefix(s, "#/components/schemas/") + ".v1.schema.json"
				continue
			}
			out[k] = rewriteRefs(val)
		}
		return out
	case []any:
		for i := range x {
			x[i] = rewriteRefs(x[i])
		}
		return x
	}
	return v
}

// Doc 05 §29: every public schema is published as a versioned JSON Schema
// (2020-12) file, generated from the OpenAPI components and kept in step.
// Regenerate with: go test ./internal/specs -run TestJSONSchemas -update-schemas
func TestJSONSchemasMatchOpenAPI(t *testing.T) {
	doc := load(t, "../../api/openapi.yaml")
	schemas := at(t, doc, "components", "schemas").(map[string]any)
	dir := "../../api/schemas"
	var names []string
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := rewriteRefs(schemas[name]).(map[string]any)
		s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
		s["$id"] = "https://schemas.zoikosuite.com/public-ai/" + name + ".v1.schema.json"
		want, _ := json.MarshalIndent(s, "", "  ")
		want = append(want, '\n')
		path := filepath.Join(dir, name+".v1.schema.json")
		if *updateSchemas {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: missing; run with -update-schemas", path)
			continue
		}
		if strings.ReplaceAll(string(got), "\r\n", "\n") != string(want) {
			t.Errorf("%s is out of date with openapi.yaml; run with -update-schemas", path)
		}
	}
}
