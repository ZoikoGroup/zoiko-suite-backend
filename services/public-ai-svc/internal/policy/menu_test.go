package policy

import (
	"strings"
	"testing"
)

// Every menu message must classify to the intent it is listed under, so a
// click is handled exactly like the same question typed.
func TestMenuMessagesClassifyToTheirIntent(t *testing.T) {
	for _, c := range Menu {
		if len(c.Items) == 0 || len(c.Items) > 4 {
			t.Errorf("%s: %d items, want 1-4 (Doc 01 §06)", c.ID, len(c.Items))
		}
		for _, it := range c.Items {
			d := Classify(it.Message)
			got := d.Primary
			if strings.HasPrefix(it.IntentID, "X") {
				got = d.Exception
			}
			if got != it.IntentID {
				t.Errorf("%s %q classified as %q/%q, want %s", it.ID, it.Message, d.Exception, d.Primary, it.IntentID)
			}
			if it.IntentID == "I05" && !AsksPrice(it.Message) {
				t.Errorf("%s %q must ask about price so it goes through the market and signed claims", it.ID, it.Message)
			}
		}
	}
}

func TestFollowUpsAreBoundedAndDeterministic(t *testing.T) {
	got := FollowUps("I05")
	if len(got) == 0 || len(got) > MaxFollowUps {
		t.Fatalf("I05 follow-ups: %v", got)
	}
	if got[0].IntentID != "I06" {
		t.Errorf("Doc 02 §08: pricing is followed by trial first, got %s", got[0].IntentID)
	}
	seen := map[string]bool{}
	for _, it := range got {
		if it.IntentID == "I05" || seen[it.Message] {
			t.Errorf("follow-up repeats the intent or a message: %+v", it)
		}
		seen[it.Message] = true
	}
	again := FollowUps("I05")
	for i := range got {
		if got[i] != again[i] {
			t.Fatal("follow-ups are not deterministic")
		}
	}
	if len(FollowUps("I29")) != 0 {
		t.Error("an intent outside the menu has no follow-ups")
	}
}

func TestEveryIntentHasADefaultCTA(t *testing.T) {
	for i := 1; i <= 30; i++ {
		id := "I" + string(rune('0'+i/10)) + string(rune('0'+i%10))
		if IntentCTA[id] == "" {
			t.Errorf("%s has no Doc 02 §06 default CTA", id)
		}
	}
}
