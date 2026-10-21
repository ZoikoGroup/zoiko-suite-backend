package domain

import "strings"

// The sensitive / ordinary data-category taxonomy mirrors the one
// privacy-purpose-registry-svc validates activities against. The registry
// exposes no endpoint for it, so it is replicated here; any category outside
// both sets is treated as unclassified and fails closed (REVIEW_REQUIRED).
// Drift between this copy and the registry is a tracked clarification item.
var sensitiveCategories = map[string]bool{
	"SPECIAL_CATEGORY": true, "HEALTH": true, "BIOMETRIC": true, "GENETIC": true,
	"RACIAL_ETHNIC": true, "POLITICAL_OPINIONS": true, "RELIGIOUS_PHILOSOPHICAL": true,
	"TRADE_UNION": true, "SEX_LIFE_ORIENTATION": true, "CRIMINAL_CONVICTIONS": true,
	"CHILD_DATA": true, "HIGH_RISK": true, "VULNERABLE_SUBJECTS": true,
}

var ordinaryCategories = map[string]bool{
	"CONTACT_INFO": true, "IDENTIFIERS": true, "FINANCIAL": true, "TRANSACTIONAL": true,
	"EMPLOYMENT": true, "BEHAVIORAL": true, "TECHNICAL": true, "LOCATION": true,
	"COMMUNICATIONS": true, "USER_CONTENT": true, "ACCOUNT_CREDENTIALS": true,
}

func norm(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// IsSensitiveCategory reports whether a category is in the sensitive taxonomy.
func IsSensitiveCategory(c string) bool { return sensitiveCategories[norm(c)] }

// IsOrdinaryCategory reports whether a category is a known non-sensitive one.
func IsOrdinaryCategory(c string) bool { return ordinaryCategories[norm(c)] }

// IsKnownCategory reports whether a category is classified either way.
func IsKnownCategory(c string) bool { return IsSensitiveCategory(c) || IsOrdinaryCategory(c) }

// IsValidSensitivityFlag: a flag is the generic SENSITIVE marker or any
// sensitive-taxonomy value.
func IsValidSensitivityFlag(f string) bool {
	f = norm(f)
	return f == "SENSITIVE" || sensitiveCategories[f]
}

func IsValidAgeBand(v string) bool {
	switch norm(v) {
	case "MINOR", "ADULT", "SENIOR", "UNKNOWN":
		return true
	}
	return false
}

func IsValidClassification(v string) bool {
	switch norm(v) {
	case "PUBLIC", "INTERNAL", "CONFIDENTIAL", "RESTRICTED", "ANONYMOUS", "UNKNOWN":
		return true
	}
	return false
}

func IsValidDataSource(v string) bool {
	switch norm(v) {
	case "DIRECT", "DERIVED", "THIRD_PARTY", "PUBLIC":
		return true
	}
	return false
}

func IsValidRecipientType(v string) bool {
	switch norm(v) {
	case "INTERNAL", "THIRD_PARTY", "PROCESSOR", "SUBPROCESSOR", "AUTHORITY":
		return true
	}
	return false
}
