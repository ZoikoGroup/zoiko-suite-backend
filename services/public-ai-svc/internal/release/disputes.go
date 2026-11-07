package release

import (
	"regexp"
	"strings"

	"zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/knowledge"
)

var amountRe = regexp.MustCompile(`\b([A-Z]{3})\s?(\d+(?:\.\d{1,2})?)`)

// PriceDisputes finds plans whose approved public page states a price that
// matches none of the plan's ACTIVE signed price claims in that currency
// (KB-003, A08). Such prices are blocked as contradicted until the owner
// resolves the conflict: the signed registry outranks website copy, but a
// disagreement is never silently resolved.
func PriceDisputes(kb *knowledge.Manifest, cm claims.Manifest) map[string]bool {
	out := map[string]bool{}
	if kb == nil {
		return out
	}
	known := map[string]map[string]bool{} // plan -> "CUR amount" set
	for _, c := range cm.Claims {
		if c.ClaimType != claims.Price || c.Status != claims.StatusActive {
			continue
		}
		if m := amountRe.FindStringSubmatch(c.NormalizedValue); m != nil {
			if known[c.SubjectID] == nil {
				known[c.SubjectID] = map[string]bool{}
			}
			known[c.SubjectID][m[1]+" "+normalise(m[2])] = true
		}
	}
	for _, ch := range kb.Chunks {
		for _, sentence := range strings.Split(ch.PlainText, ".") {
			for plan, amounts := range known {
				if !strings.Contains(sentence, plan) {
					continue
				}
				for _, m := range amountRe.FindAllStringSubmatch(sentence, -1) {
					cur := m[1]
					seenCurrency := false
					for k := range amounts {
						if strings.HasPrefix(k, cur+" ") {
							seenCurrency = true
						}
					}
					if seenCurrency && !amounts[cur+" "+normalise(m[2])] {
						out[plan] = true
					}
				}
			}
		}
	}
	return out
}

func normalise(amount string) string {
	if !strings.Contains(amount, ".") {
		return amount + ".00"
	}
	if i := strings.Index(amount, "."); len(amount)-i == 2 {
		return amount + "0"
	}
	return amount
}
