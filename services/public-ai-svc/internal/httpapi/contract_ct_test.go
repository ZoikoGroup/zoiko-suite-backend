package httpapi

// Doc 05 §27-28 contract tests not already covered elsewhere (see
// docs/suivo/contract-test-map.md for the full CT-001..CT-052 map).

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// CT-002: a missing or forged session grant gets 401 and discloses nothing.
func TestDoc05CT002_MissingOrInvalidGrant(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-00000021")
	for name, grant := range map[string]string{"missing": "", "forged": "pgr_forged000000000000000000", "malformed": "not a grant"} {
		hdr := map[string]string{"Idempotency-Key": turnUUID}
		if grant != "" {
			hdr[sessionHeader] = grant
		}
		w := h.req(http.MethodPost, "/public-ai/v1/turns", turnBody(s.SessionID, turnUUID, "hello"), hdr)
		if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Errorf("%s grant: got %d", name, w.Code)
		}
		if strings.Contains(w.Body.String(), s.SessionID) {
			t.Errorf("%s grant: response discloses the session id", name)
		}
	}
	if h.ans.calls.Load() != 0 {
		t.Fatal("no turn may be answered without a valid grant")
	}
}

// CT-006: an unknown schema_version on a turn is 400 unsupported_schema.
func TestDoc05CT006_UnknownSchemaVersion(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-00000022")
	body := strings.Replace(turnBody(s.SessionID, turnUUID, "hello"), `"schema_version":"1.0"`, `"schema_version":"9.9"`, 1)
	w := h.req(http.MethodPost, "/public-ai/v1/turns", body, map[string]string{sessionHeader: s.SessionGrant, "Idempotency-Key": turnUUID})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unsupported_schema") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

// CT-046: the browser cannot post internal events: there is no public
// event-ingest route, and internal paths are not served under /public-ai.
func TestDoc05CT046_BrowserCannotPostEvents(t *testing.T) {
	h := newHarness(t, time.Hour)
	for _, p := range []string{"/public-ai/v1/events", "/public-ai/v1/outbox", "/public-ai/v1/internal/events", "/events"} {
		w := h.req(http.MethodPost, p, `{"event_type":"public_ai.source.revoked.v1"}`, nil)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: got %d", p, w.Code)
		}
	}
}
