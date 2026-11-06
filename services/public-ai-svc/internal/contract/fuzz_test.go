package contract

import (
	"encoding/json"
	"testing"
)

// SEC-T015: any request body decodes to a valid request or an error, never
// a panic, and Validate accepts only what the schema allows.
func FuzzTurnRequest(f *testing.F) {
	f.Add([]byte(`{"schema_version":"1.0","session_id":"pub_s_123456789012","client_turn_id":"6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b","message":"hi"}`))
	f.Add([]byte(`{"message":[[[[[[]]]]]]}`))
	f.Add([]byte(`{"page_context":"https://x","locale":"en_US!"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		var r TurnRequest
		if json.Unmarshal(body, &r) != nil {
			return
		}
		if r.Validate() == nil {
			if r.PageContext != "" && !PageContexts[r.PageContext] {
				t.Fatalf("accepted page_context %q", r.PageContext)
			}
			if len(r.Message) == 0 || len(r.Message) > 8000 {
				t.Fatalf("accepted message of %d bytes", len(r.Message))
			}
		}
	})
}

func FuzzAnalyticsRequest(f *testing.F) {
	f.Add([]byte(`{"schema_version":"1.0","event":"source_opened","link_id":"pricing"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		var a AnalyticsRequest
		if json.Unmarshal(body, &a) != nil || a.Validate() != nil {
			return
		}
		if a.Event != "chat_opened" && a.Event != "source_opened" {
			t.Fatalf("accepted event %q", a.Event)
		}
		if a.LinkID != "" && !linkIDRe.MatchString(a.LinkID) {
			t.Fatalf("accepted link_id %q", a.LinkID)
		}
	})
}
