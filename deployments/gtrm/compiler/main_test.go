package main

import "testing"

func TestConfigBodyIgnoresProvenanceAndLineEndings(t *testing.T) {
	body := "http:\n  routers:\n    r1:\n      rule: Host(`a.example`)\n"
	withHeaderLF := "# generated_at: 2026-10-19T00:00:00Z\n# commit: abc\n\n" + body
	withHeaderCRLF := "# generated_at: 2026-10-19T00:00:00Z\r\n# commit: def\r\n\r\n" +
		"http:\r\n  routers:\r\n    r1:\r\n      rule: Host(`a.example`)\r\n"

	if got, want := configBody(withHeaderCRLF), configBody(withHeaderLF); got != want {
		t.Fatalf("CRLF checkout changed the drift body:\n got %q\nwant %q", got, want)
	}
	if got, want := configBody(withHeaderLF), body; got != want {
		t.Fatalf("provenance header not stripped:\n got %q\nwant %q", got, want)
	}
	if configBody(withHeaderLF) == configBody(body+"# hand edit\n") {
		t.Fatal("a real content change must still be detected as drift")
	}
}
