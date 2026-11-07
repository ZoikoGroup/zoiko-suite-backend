// Package isolation enforces the R0 public-only boundary in code (INV-01,
// SEC-P0-01, Doc 04 §01): public-ai-svc may not depend on any other ZoikoSuite
// module, and its configuration may not point at another service's data.
package isolation

import "strings"

// Module is this service's module path.
const Module = "zoiko.io/public-ai-svc"

// ForbiddenDeps returns every package path that belongs to another zoiko.io
// module. Paths of this module itself are allowed.
func ForbiddenDeps(pkgs []string) []string {
	var bad []string
	for _, p := range pkgs {
		p = strings.TrimSpace(p)
		if !strings.HasPrefix(p, "zoiko.io/") {
			continue
		}
		if p == Module || strings.HasPrefix(p, Module+"/") {
			continue
		}
		bad = append(bad, p)
	}
	return bad
}
