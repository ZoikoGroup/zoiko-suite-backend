package specs

import (
	"fmt"
	"sort"
	"testing"
)

// Doc 04 §25, Doc 05 §26/§29: the published contract only grows. Against
// the frozen 1.0 snapshot (api/compat), the current spec may add paths,
// optional fields and enum values, but never remove a path, an operation, a
// response field or an enum value, and never make a request field newly
// required. A breaking change needs /public-ai/v2 and a new snapshot.

// walk collects comparable facts about a schema tree under prefix.
func walk(prefix string, v any, out map[string]bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for name, p := range props {
			out[prefix+".prop:"+name] = true
			walk(prefix+"."+name, p, out)
		}
	}
	if enum, ok := m["enum"].([]any); ok {
		for _, e := range enum {
			out[fmt.Sprintf("%s.enum:%v", prefix, e)] = true
		}
	}
	if items, ok := m["items"]; ok {
		walk(prefix+"[]", items, out)
	}
	for _, k := range []string{"allOf", "oneOf", "anyOf"} {
		if xs, ok := m[k].([]any); ok {
			for i, x := range xs {
				walk(fmt.Sprintf("%s.%s%d", prefix, k, i), x, out)
			}
		}
	}
}

func required(prefix string, v any, out map[string]bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if rs, ok := m["required"].([]any); ok {
		for _, r := range rs {
			out[fmt.Sprintf("%s.required:%v", prefix, r)] = true
		}
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for name, p := range props {
			required(prefix+"."+name, p, out)
		}
	}
}

func openapiFacts(doc map[string]any) (have, requestRequired map[string]bool) {
	have, requestRequired = map[string]bool{}, map[string]bool{}
	for path, item := range doc["paths"].(map[string]any) {
		for method, op := range item.(map[string]any) {
			key := method + " " + path
			have["op:"+key] = true
			o := op.(map[string]any)
			if rb, ok := o["requestBody"].(map[string]any); ok {
				if c, ok := rb["content"].(map[string]any); ok {
					for _, mt := range c {
						if s, ok := mt.(map[string]any)["schema"]; ok {
							required("req:"+key, s, requestRequired)
						}
					}
				}
			}
			for code := range o["responses"].(map[string]any) {
				have["resp:"+key+" "+code] = true
			}
		}
	}
	for name, s := range doc["components"].(map[string]any)["schemas"].(map[string]any) {
		walk("schema:"+name, s, have)
		required("schema:"+name, s, requestRequired)
	}
	return have, requestRequired
}

func missing(base, head map[string]bool) []string {
	var out []string
	for k := range base {
		if !head[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func TestOpenAPIBackwardCompatible(t *testing.T) {
	baseHave, baseReq := openapiFacts(load(t, "../../api/compat/openapi-1.0.yaml"))
	headHave, headReq := openapiFacts(load(t, "../../api/openapi.yaml"))
	for _, k := range missing(baseHave, headHave) {
		t.Errorf("breaking change: %s was removed", k)
	}
	// A request field that becomes required breaks existing clients.
	for _, k := range missing(headReq, baseReq) {
		if len(k) > 4 && k[:4] == "req:" {
			t.Errorf("breaking change: %s is newly required", k)
		}
	}
}

func TestAsyncAPIBackwardCompatible(t *testing.T) {
	facts := func(doc map[string]any) map[string]bool {
		out := map[string]bool{}
		for name, m := range at(t, doc, "components", "messages").(map[string]any) {
			out["msg:"+name] = true
			walk("msg:"+name, at(t, m, "payload"), out)
		}
		return out
	}
	for _, k := range missing(facts(load(t, "../../api/compat/asyncapi-1.0.yaml")), facts(load(t, "../../api/asyncapi.yaml"))) {
		t.Errorf("breaking event change: %s was removed", k)
	}
}
