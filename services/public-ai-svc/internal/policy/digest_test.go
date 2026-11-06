package policy

import (
	"encoding/base64"
	"regexp"
	"testing"

	"zoiko.io/public-ai-svc/internal/signing"
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

// Doc 08 OPR-002: the approved digest must carry the release signers'
// signature; an unsigned or wrongly signed digest is refused.
func TestSignedPolicyDigest(t *testing.T) {
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	digest := BundleDigest("pt-1")
	sig, _ := signing.Sign(priv, []byte(PolicySigningPayload(digest)))
	good := base64.StdEncoding.EncodeToString(sig)
	if err := CheckSignedDigest(digest, good, pub, "pt-1"); err != nil {
		t.Fatalf("signed digest refused: %v", err)
	}
	if CheckSignedDigest(digest, "", pub, "pt-1") == nil {
		t.Fatal("an unsigned digest must be refused")
	}
	otherPub, _, _ := signing.GenerateKey()
	if CheckSignedDigest(digest, good, otherPub, "pt-1") == nil {
		t.Fatal("a signature from another key must be refused")
	}
	rawSig, _ := signing.Sign(priv, []byte(digest)) // the bare digest, not the policy payload
	if CheckSignedDigest(digest, base64.StdEncoding.EncodeToString(rawSig), pub, "pt-1") == nil {
		t.Fatal("a signature over a different payload must be refused")
	}
	if CheckSignedDigest("", "", "", "pt-1") != nil {
		t.Fatal("dev/test without a digest must start")
	}
}
