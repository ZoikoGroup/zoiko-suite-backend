package policy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"zoiko.io/public-ai-svc/internal/signing"
)

// BundleDigest fingerprints the prompt/policy artifact (Doc 08 §06, Doc 06
// §08): every template, route mapping and menu entry, the exception
// precedence, and the rules themselves (sensitive-data detectors, exception
// and intent patterns). Any wording or rule change produces a new digest.
func BundleDigest(promptTemplateVersion string) string {
	b, _ := json.Marshal(struct {
		Prompt     string
		Exceptions map[string]Template
		Routes     map[string]Template
		Boundary   map[string]Template
		Defaults   map[string]string
		Named      []Template
		Order      []string
		Rules      []string
		Menu       []MenuCategory
		CTA        map[string]string
	}{
		promptTemplateVersion, ExceptionTemplates, RouteTemplates, BoundaryTemplates, DefaultRoute,
		[]Template{TplAbstainNoEvidence, TplAbstainStale, TplAbstainConflict, TplClarifyMarket, TplClarifyPlan, TplUnavailable,
			TplOutOfScope, TplClarifyGiveUp, TplAbstainCoverage, TplAbstainCertification, TplDataRights, TplNoApprovedInfo},
		exceptionOrder, ruleStrings(), Menu, IntentCTA,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ruleStrings lists every detector and rule pattern in a stable order.
func ruleStrings() []string {
	var out []string
	for _, d := range detectors {
		out = append(out, "detector:"+d.kind+":"+d.re.String())
	}
	for x, res := range exceptionRules {
		for _, re := range res {
			out = append(out, "exception:"+x+":"+re.String())
		}
	}
	for id, re := range intentRules {
		out = append(out, "intent:"+id+":"+re.String())
	}
	sort.Strings(out)
	return out
}

// CheckDigest enforces the approved policy (Doc 06 §08): when an approved
// digest is configured, the running policy must match it exactly.
func CheckDigest(approved, promptTemplateVersion string) error {
	if approved == "" {
		return nil // dev/test; config requires it everywhere else
	}
	if got := BundleDigest(promptTemplateVersion); got != approved {
		return fmt.Errorf("policy digest %s does not match the approved PUBLIC_AI_POLICY_DIGEST %s", got, approved)
	}
	return nil
}

// CheckSignedDigest is CheckDigest plus the signers' approval (Doc 08 §06,
// OPR-002): the approved digest must carry a valid Ed25519 signature from
// the release signing key, so a digest typed into the environment by one
// person is not enough. sig is base64; empty approved means dev/test.
func CheckSignedDigest(approved, sigB64, pubKey, promptTemplateVersion string) error {
	if err := CheckDigest(approved, promptTemplateVersion); err != nil || approved == "" {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(sig) == 0 {
		return fmt.Errorf("PUBLIC_AI_POLICY_SIGNATURE is missing or not base64")
	}
	if err := signing.Verify(pubKey, []byte(PolicySigningPayload(approved)), sig); err != nil {
		return fmt.Errorf("policy digest signature does not verify with the release signing key: %w", err)
	}
	return nil
}

// PolicySigningPayload is what the signers sign for a policy digest.
func PolicySigningPayload(digest string) string { return "zoikosuite-public-ai-policy:" + digest }
