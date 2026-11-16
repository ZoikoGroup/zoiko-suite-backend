package policy

import (
	"regexp"
	"strings"
)

// Doc 07 QA-T073: visitors misspell the words the rules and the knowledge
// base key on ("pricng", "invioces", "demmo"), so a safety rule or an answer
// would be missed. FixTypos replaces known misspellings of domain words, as
// whole words, case-insensitively. The table is fixed and reviewed: only
// non-words are listed, never a real word that means something else ("trail"
// is only corrected after "free", because "audit trail" is a real phrase).
var typoFixes = map[string]string{
	"pricng": "pricing", "prcing": "pricing", "priceing": "pricing", "pricin": "pricing", "pirce": "price", "prcie": "price",
	"certifed": "certified", "certfied": "certified", "cerified": "certified", "certificaton": "certification", "certifcation": "certification",
	"intergrate": "integrate", "intergrates": "integrates", "intergration": "integration", "intergrations": "integrations",
	"integartion": "integration", "integartions": "integrations", "intigrate": "integrate", "intigration": "integration", "integraton": "integration",
	"entitiy": "entity", "enitity": "entity", "entitiys": "entities", "entites": "entities", "entitys": "entities", "enities": "entities",
	"subsidary": "subsidiary", "subsidaries": "subsidiaries", "subsidiries": "subsidiaries", "subsideries": "subsidiaries",
	"demmo": "demo", "dmeo": "demo", "deom": "demo", "demoo": "demo",
	"moduels": "modules", "modlues": "modules", "modues": "modules", "moduls": "modules", "modul": "module", "moduel": "module",
	"invioce": "invoice", "invioces": "invoices", "invoce": "invoice", "invoces": "invoices", "invocie": "invoice", "invocies": "invoices",
	"inovice": "invoice", "inovices": "invoices", "invoic": "invoice",
	"encrytped": "encrypted", "encrpyted": "encrypted", "encypted": "encrypted", "encrpted": "encrypted", "encryted": "encrypted",
	"encrytion": "encryption", "encription": "encryption", "encyption": "encryption", "encrpytion": "encryption",
	"migrte": "migrate", "migarte": "migrate", "migraton": "migration", "migartion": "migration",
	"payrol": "payroll", "payrool": "payroll", "pyroll": "payroll",
	"complaince": "compliance", "complience": "compliance", "compliace": "compliance",
	"secuirty": "security", "securty": "security", "secutiry": "security", "secruity": "security",
	"reconcilation": "reconciliation", "reconcilliation": "reconciliation", "reconsile": "reconcile",
	"approvels": "approvals", "aproval": "approval", "aprovals": "approvals",
	"subscripton": "subscription", "susbcription": "subscription",
	"recieve": "receive", "recievable": "receivable", "recievables": "receivables", "payabel": "payable",
	// Chat shorthand.
	"pls": "please", "plz": "please", "u": "you", "ur": "your", "thx": "thanks",
}

var (
	typoRe       = regexp.MustCompile(`(?i)\b(?:` + typoAlternation() + `)\b`)
	freeTrailRe  = regexp.MustCompile(`(?i)\b(free|30[- ]day|14[- ]day|a)\s+trail\b|\btrail\s+(period|version|account|plan)\b`)
	trailWordsRe = regexp.MustCompile(`(?i)trail`)
)

func typoAlternation() string {
	words := make([]string, 0, len(typoFixes))
	for w := range typoFixes {
		words = append(words, regexp.QuoteMeta(w))
	}
	return strings.Join(words, "|")
}

// FixTypos corrects known misspellings; the rest of the text is unchanged.
// A corrected word keeps a leading capital.
func FixTypos(text string) string {
	text = typoRe.ReplaceAllStringFunc(text, func(m string) string {
		fix := typoFixes[strings.ToLower(m)]
		if m[0] >= 'A' && m[0] <= 'Z' {
			fix = strings.ToUpper(fix[:1]) + fix[1:]
		}
		return fix
	})
	return freeTrailRe.ReplaceAllStringFunc(text, func(m string) string {
		return trailWordsRe.ReplaceAllStringFunc(m, func(t string) string {
			if t[0] == 'T' {
				return "Trial"
			}
			return "trial"
		})
	})
}
