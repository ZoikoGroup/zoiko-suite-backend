package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
)

// Doc 02 Appendix C: intent_id + intent_version, template_id + version.
// The versions are content hashes of the rules and the approved wording, so
// any change to a pattern or a template changes the version recorded on
// every decision (no manual bump to forget).
// They are computed on first use, after every package init has filled the
// rule and template tables.
var (
	IntentVersion   = sync.OnceValue(func() string { return hashOf(intentRuleText()) })
	TemplateVersion = sync.OnceValue(func() string { return hashOf(templateText()) })
)

func hashOf(parts []string) string {
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

func intentRuleText() []string {
	var out []string
	for id, re := range intentRules {
		out = append(out, id+"="+re.String())
	}
	for id, res := range exceptionRules {
		for _, re := range res {
			out = append(out, id+"="+re.String())
		}
	}
	return out
}

func templateText() []string {
	var out []string
	for text, id := range templateIDs {
		out = append(out, id+"="+text)
	}
	for intent, q := range GroundedQualifiers {
		out = append(out, intent+"="+q.ID+"|"+q.Text+"|"+q.Question)
	}
	return out
}
