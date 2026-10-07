package domain

import (
	"strings"
	"testing"
)

func TestNormalizeEvidence(t *testing.T) {
	cases := []struct {
		event, bounce, fact, strength string
		ok                            bool
	}{
		{"DELIVERED", "", EvidenceMailboxAccepted, StrengthMailboxLevel, true},
		{"BOUNCE", "HARD", EvidenceBounced, StrengthMailboxLevel, true},
		{"BOUNCE", "", EvidenceBounced, StrengthMailboxLevel, true}, // an unclassified bounce is treated as permanent: never assume it will be delivered
		{"BOUNCE", "SOFT", EvidenceDeferred, StrengthMailboxLevel, true},
		{"BOUNCE", "TRANSIENT", EvidenceDeferred, StrengthMailboxLevel, true},
		{"DROPPED", "", EvidenceRejected, StrengthProviderLevel, true},
		{"COMPLAINT", "", EvidenceComplaint, StrengthRecipientSignal, true},
		{"UNSUBSCRIBE", "", EvidenceUnsubscribed, StrengthRecipientSignal, true},
		{"OPENED", "", "", "", false}, // no open or click telemetry is ever accepted as evidence here
		{"", "", "", "", false},
	}
	for _, c := range cases {
		fact, strength, ok := NormalizeEvidence(c.event, c.bounce)
		if fact != c.fact || strength != c.strength || ok != c.ok {
			t.Errorf("%s/%s = %q %q %v, want %q %q %v", c.event, c.bounce, fact, strength, ok, c.fact, c.strength, c.ok)
		}
	}
}

// Provider telemetry never claims a person saw or acknowledged a message.
func TestEvidenceVocabularyNeverClaimsHumanNotice(t *testing.T) {
	for _, f := range EvidenceFacts {
		if EvidenceLimits(f) == "" {
			t.Errorf("%s has no stated limits", f)
		}
		for _, banned := range []string{"READ", "OPENED", "ACKNOWLEDGED", "SEEN", "VIEWED"} {
			if strings.Contains(f, banned) {
				t.Errorf("fact %s claims human notice", f)
			}
		}
	}
	if !strings.Contains(EvidenceLimits(EvidenceMailboxAccepted), "does not prove") {
		t.Error("MAILBOX_ACCEPTED must say what it does not prove")
	}
	// Every fact a callback can produce is in the closed set the database CHECK mirrors.
	for _, ev := range []string{"DELIVERED", "BOUNCE", "DROPPED", "COMPLAINT", "UNSUBSCRIBE"} {
		f, s, _ := NormalizeEvidence(ev, "HARD")
		if !inSet(EvidenceFacts, f) || !inSet(EvidenceStrengths, s) {
			t.Errorf("%s produces %q/%q outside the closed sets", ev, f, s)
		}
	}
}
