package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// BundleDigest fingerprints the prompt/policy artifact (Doc 08 §06): every
// template, route mapping and the exception precedence. Any wording or rule
// change produces a new digest for the release manifest.
func BundleDigest(promptTemplateVersion string) string {
	b, _ := json.Marshal(struct {
		Prompt     string
		Exceptions map[string]Template
		Routes     map[string]Template
		Defaults   map[string]string
		Named      []Template
		Order      []string
	}{
		promptTemplateVersion, ExceptionTemplates, RouteTemplates, DefaultRoute,
		[]Template{TplAbstainNoEvidence, TplAbstainStale, TplAbstainConflict, TplClarifyMarket, TplClarifyPlan, TplUnavailable, TplOutOfScope, TplClarifyGiveUp},
		exceptionOrder,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
