package ncd

import "testing"

// NP-37/38/39, INV-20: engagement signals are evidence, never a state change
// and never an acknowledgment. A click proves an interaction, not who made it.
func TestEngagementSignalsAreLowConfidenceAndChangeNothing(t *testing.T) {
	for _, kind := range []string{"opened", "open", "clicked", "click"} {
		n, ok := NormalizeProviderEvent(ChannelEmail, kind)
		if !ok {
			t.Fatalf("%s should normalize", kind)
		}
		if n.Confidence != "LOW" || n.AttemptState != "" || n.Suppression != "" {
			t.Errorf("%s: want LOW confidence with no attempt transition and no suppression, got %+v", kind, n)
		}
		if n.NormalizedState == "ACKNOWLEDGED" || n.EvidenceType == "ACKNOWLEDGMENT" {
			t.Errorf("%s must never normalize to an acknowledgment, got %+v", kind, n)
		}
		if n.DoesNotProve == "" {
			t.Errorf("%s must state what it does not prove", kind)
		}
	}
	if n, _ := NormalizeProviderEvent(ChannelEmail, "clicked"); n.EvidenceType != "LINK_ACTION" {
		t.Errorf("a click is LINK_ACTION interaction evidence, got %s", n.EvidenceType)
	}
}
