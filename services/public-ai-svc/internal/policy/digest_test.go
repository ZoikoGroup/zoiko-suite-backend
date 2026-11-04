package policy

import (
	"regexp"
	"testing"
)

// Doc 06 §08: the digest covers the rules, not only the wording, and a
// mismatch with the approved digest is refused.
func TestPolicyDigestCoversRulesAndIsEnforced(t *testing.T) {
	before := BundleDigest("v1")
	if CheckDigest("", "v1") != nil || CheckDigest(before, "v1") != nil {
		t.Fatal("matching or unset digest must pass")
	}
	if CheckDigest("deadbeef", "v1") == nil {
		t.Fatal("a different approved digest must be refused")
	}
	saved := intentRules["I01"]
	intentRules["I01"] = regexp.MustCompile("changed")
	defer func() { intentRules["I01"] = saved }()
	if BundleDigest("v1") == before {
		t.Fatal("changing an intent rule must change the digest")
	}
}
