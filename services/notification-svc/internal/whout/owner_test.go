package whout

import (
	"encoding/json"
	"testing"
)

// INT-CTRL-034: a dead-lettered delivery names the queue that owns it; ordinary failures do not pretend to.
func TestADeadLetterNamesItsOwner(t *testing.T) {
	d := &Delivery{DeliveryID: "d1", SubscriptionID: "s1", EventID: "e1", BodyHash: "h", CorrelationID: "c1"}
	owner := func(o struct {
		Body []byte
	}) any {
		var env struct {
			Payload map[string]any `json:"payload"`
			Data    map[string]any `json:"data"`
		}
		_ = json.Unmarshal(o.Body, &env)
		if env.Payload != nil {
			return env.Payload["owner"]
		}
		return env.Data["owner"]
	}
	dl, err := DeadLetteredEvent("t1", "le1", d, 5, OutcomeHTTPError, "http_5xx", "budget exhausted")
	if err != nil {
		t.Fatal(err)
	}
	if got := owner(struct{ Body []byte }{dl.Body}); got != DeadLetterOwner {
		t.Fatalf("dead letter owner = %v, body %s", got, dl.Body)
	}
	fl, _ := FailedEvent("t1", "le1", d, 1, OutcomeHTTPError, "http_5xx")
	if got := owner(struct{ Body []byte }{fl.Body}); got != nil {
		t.Fatalf("a single failed attempt is not a dead letter: owner %v", got)
	}
}
