package contract

import (
	"errors"
	"strings"
	"testing"
)

func validTurn() TurnRequest {
	return TurnRequest{SchemaVersion: "1.0", SessionID: "pub_s_0123456789ab", ClientTurnID: "6f1c2a52-3c1e-4b5e-9a7f-0c1d2e3f4a5b", Message: "What modules exist?"}
}

func TestTurnRequestValidation(t *testing.T) {
	cases := []struct {
		name  string
		mod   func(*TurnRequest)
		field string
	}{
		{"ok", func(*TurnRequest) {}, ""},
		{"bad schema", func(r *TurnRequest) { r.SchemaVersion = "2.0" }, "schema"},
		{"short session", func(r *TurnRequest) { r.SessionID = "x" }, "session_id"},
		{"bad uuid", func(r *TurnRequest) { r.ClientTurnID = "nope" }, "client_turn_id"},
		{"empty message", func(r *TurnRequest) { r.Message = "   \u0000 " }, "message"},
		{"long message", func(r *TurnRequest) { r.Message = strings.Repeat("a", 2001) }, "message"},
		{"lower market", func(r *TurnRequest) { r.MarketCode = "us" }, "market_code"},
		{"bad page", func(r *TurnRequest) { r.PageContext = "https://x" }, "page_context"},
		{"bad mode", func(r *TurnRequest) { r.RequestedResponseMode = "sse" }, "requested_response_mode"},
		{"bad locale", func(r *TurnRequest) { r.Locale = "en_US!" }, "locale"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := validTurn()
			c.mod(&r)
			err := r.Validate()
			switch {
			case c.field == "" && err != nil:
				t.Fatalf("unexpected: %v", err)
			case c.field == "schema" && !errors.Is(err, ErrUnsupportedSchema):
				t.Fatalf("want schema error, got %v", err)
			case c.field != "" && c.field != "schema":
				var fe *FieldError
				if !errors.As(err, &fe) || fe.Field != c.field {
					t.Fatalf("want field %s, got %v", c.field, err)
				}
			}
		})
	}
}

func TestNormalizeMessage(t *testing.T) {
	// NFC: e + combining acute -> é; control chars stripped; zero-width removed.
	got := NormalizeMessage("  café\u0007 price​?\n ")
	if got != "café price?" {
		t.Fatalf("got %q", got)
	}
}

func TestEnvelopeValidate(t *testing.T) {
	e := Envelope{SchemaVersion: "1.0", TurnSequence: 1, Status: Abstain, IntentID: "I05", KnowledgeReleaseID: "kb-1", PolicyVersion: "p", Citations: []Citation{}, Actions: []Action{}}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := e
	bad.Status = Answered // ANSWERED without provenance is rejected
	if bad.Validate() == nil {
		t.Fatal("ANSWERED without citations must fail")
	}
	bad = e
	bad.Actions = []Action{{Type: "POST_FORM", RouteKey: "demo"}}
	if bad.Validate() == nil {
		t.Fatal("non-route action must fail")
	}
	bad = e
	bad.IntentID = "I31"
	if bad.Validate() == nil {
		t.Fatal("unknown intent must fail")
	}
	bad = e
	bad.Text = strings.Repeat("x", 5001)
	if bad.Validate() == nil {
		t.Fatal("text over 5000 must fail")
	}
}
