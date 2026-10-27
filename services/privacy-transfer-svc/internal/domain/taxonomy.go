package domain

import "strings"

// sensitiveCategories mirrors the sensitive/special-category taxonomy that
// privacy-purpose-registry-svc validates activities against. The registry
// exposes no endpoint for it, so it is replicated (drift is a tracked
// clarification item). It replaces a substring match that missed registered
// categories such as SEX_LIFE_ORIENTATION, TRADE_UNION or CHILD_DATA.
var sensitiveCategories = map[string]bool{
	"SPECIAL_CATEGORY": true, "HEALTH": true, "BIOMETRIC": true, "GENETIC": true,
	"RACIAL_ETHNIC": true, "POLITICAL_OPINIONS": true, "RELIGIOUS_PHILOSOPHICAL": true,
	"TRADE_UNION": true, "SEX_LIFE_ORIENTATION": true, "CRIMINAL_CONVICTIONS": true,
	"CHILD_DATA": true, "HIGH_RISK": true, "VULNERABLE_SUBJECTS": true,
}

// IsSensitiveCategory reports whether cat is in the sensitive taxonomy.
func IsSensitiveCategory(cat string) bool {
	return sensitiveCategories[strings.ToUpper(strings.TrimSpace(cat))]
}
