package knowledge

import "testing"

// SEC-T015: an article with any front matter parses or errors; it never panics.
func FuzzParseMarkdownSource(f *testing.F) {
	f.Add("---\nsource_id: a\ntitle: T\n---\nBody")
	f.Add("---\n---\n")
	f.Add("no front matter")
	f.Fuzz(func(t *testing.T, raw string) {
		_, _ = ParseMarkdownSource(raw)
	})
}
