package acceptance

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/public-ai-svc/internal/events"
)

// analyticsNames returns the §27 events in the outbox, in order, with their data.
func analyticsData(t *testing.T, x *h) []map[string]any {
	t.Helper()
	evs, err := x.env.Store.UnpublishedEvents(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ev := range evs {
		if ev.EventType != events.TypeAnalytics {
			continue
		}
		var env struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(ev.Payload, &env); err != nil {
			t.Fatal(err)
		}
		out = append(out, env.Data)
	}
	return out
}

func names(ds []map[string]any) string {
	var n []string
	for _, d := range ds {
		n = append(n, d["name"].(string))
	}
	return strings.Join(n, ",")
}

// Doc 02 §27: each turn records intent_classified and answer_delivered (or
// clarification_shown); the browser-only events go through POST /analytics;
// payloads are ids and enums, never the visitor's words.
func TestAnalyticsEventContract(t *testing.T) {
	x := publicKBServer(t)
	w := x.req(http.MethodPost, "/public-ai/v1/analytics", `{"schema_version":"1.0","event":"chat_opened","page_context":"pricing","locale":"en-US"}`, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("chat_opened without a session: %d %s", w.Code, w.Body.String())
	}
	for body, code := range map[string]int{
		`{"schema_version":"1.0","event":"page_view"}`:                                      422,
		`{"schema_version":"1.0","event":"source_opened","link_id":"https://evil.example"}`: 422,
		`{"schema_version":"1.0","event":"chat_opened","question":"my card 4111"}`:          400,
	} {
		if w := x.req(http.MethodPost, "/public-ai/v1/analytics", body, nil); w.Code != code {
			t.Errorf("%s: %d, want %d", body, w.Code, code)
		}
	}

	s := x.session("analytics-1")
	question := "What is ZoikoSuite?"
	e := envelope(t, x.turn(s, "2a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", question))
	w = x.req(http.MethodPost, "/public-ai/v1/analytics",
		`{"schema_version":"1.0","event":"source_opened","session_id":"`+s.SessionID+`","link_id":"`+e.Citations[0].URLKey+`","intent_id":"`+e.IntentID+`"}`,
		map[string]string{"X-Public-Chat-Session": s.SessionGrant})
	if w.Code != http.StatusNoContent {
		t.Fatalf("source_opened: %d %s", w.Code, w.Body.String())
	}
	fb := x.req(http.MethodPost, "/public-ai/v1/turns/"+e.TurnID+"/feedback", `{"schema_version":"1.0","session_id":"`+s.SessionID+`","rating":"useful"}`,
		map[string]string{"X-Public-Chat-Session": s.SessionGrant})
	if fb.Code != http.StatusNoContent {
		t.Fatalf("feedback: %d", fb.Code)
	}
	ds := analyticsData(t, x)
	if got := names(ds); got != "chat_opened,intent_classified,answer_delivered,source_opened,feedback_submitted" {
		t.Fatalf("events %s", got)
	}
	if ds[0]["route_id"] != "pricing" || ds[1]["intent_id"] != "I01" || ds[1]["safety_disposition"] != "ALLOW" || ds[1]["confidence_bucket"] == "" {
		t.Errorf("fields: %v %v", ds[0], ds[1])
	}
	if ds[2]["source_count"] != float64(len(e.Citations)) || ds[2]["answer_class"] == "" {
		t.Errorf("answer_delivered: %v", ds[2])
	}
	raw, _ := json.Marshal(ds)
	if strings.Contains(strings.ToLower(string(raw)), "what is zoikosuite") {
		t.Fatal("an analytics event carried the visitor's words")
	}
}
