// Package catalogue is the machine-readable form of the ZS-EVENT-001 §19
// "Enterprise Domain Event Catalogue — Canonical Initial Inventory".
//
// The catalogue says which event types exist, which domain owns each one and
// who consumes it. It is a contract inventory, not permission to emit: an
// event still has to pass the §23 Definition of Ready. Its purpose here is to
// make the §21 "semantic lint" and the E-02 owner rule checkable in CI instead
// of living only in a Word document.
//
// catalogue.json is the single source of truth. Add a type there (and a test
// will tell you what is missing); never hard-code event type strings that are
// absent from it.
package catalogue

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

//go:embed catalogue.json
var raw []byte

// Prefix is the fixed namespace of every canonical event type (§5.1).
const Prefix = "com.zoikosuite."

// Entry is one catalogued event type.
type Entry struct {
	// Type is the full canonical type, e.g. com.zoikosuite.accounting.journal.posted.
	Type string `json:"type"`
	// Family is the §19 section heading the type is listed under.
	Family string `json:"family"`
	// Producer is the owning service/role named by the catalogue. Only this
	// producer may publish the type (E-02).
	Producer string `json:"producer"`
	// Subject is the aggregate the fact is about.
	Subject string `json:"subject"`
	// Consumers is the catalogue's primary-consumer / intent column.
	Consumers string `json:"consumers"`
	// Origin is empty for a type listed in ZS-EVENT-001 section 19, and "extension" for a type a service already emits
	// that section 19 does not list. Extensions are real, owned facts recorded so they are governed; they still
	// need their consumers registered (section 23) before they count as Ready.
	Origin string `json:"origin,omitempty"`
}

// Domain is the first segment after the prefix (accounting, ar, tax, ...).
func (e Entry) Domain() string { return segments(e.Type)[0] }

// Aggregate is the second segment (journal, customer-invoice, ...).
func (e Entry) Aggregate() string { return segments(e.Type)[1] }

// Fact is the final segment (posted, issued, ...).
func (e Entry) Fact() string {
	s := segments(e.Type)
	return s[len(s)-1]
}

func segments(t string) []string {
	return strings.Split(strings.TrimPrefix(t, Prefix), ".")
}

// Domains are the §19 domain prefixes. A type outside them is not in the
// catalogue's vocabulary.
var Domains = []string{
	"governance", "org", "accounting", "ar", "ap", "banking", "tax", "assets",
	"inventory", "projects", "finance", "reporting", "audit", "legal",
	"business", "workforce", "data", "ai", "commercial", "security", "ops",
}

// ExtensionDomains are domain prefixes services already emit under that section 19
// does not define. Only an origin=extension entry may use one, so a spec entry can
// never drift into them. Each is an open architecture question (section 19's
// "Data, AI & Integrations" family uses the data.* domain for connector and import
// facts): adopt the domain into section 19, or migrate the producer to an existing one.
var ExtensionDomains = []string{"integration", "connectivity", "treasury", "reconciliation", "einvoice"}

var (
	once    sync.Once
	entries []Entry
	byType  map[string]Entry
	loadErr error
)

func load() {
	if err := json.Unmarshal(raw, &entries); err != nil {
		loadErr = fmt.Errorf("catalogue: parse catalogue.json: %w", err)
		return
	}
	byType = make(map[string]Entry, len(entries))
	for _, e := range entries {
		if _, dup := byType[e.Type]; dup {
			loadErr = fmt.Errorf("catalogue: duplicate event type %q", e.Type)
			return
		}
		byType[e.Type] = e
	}
}

// All returns every catalogued entry, sorted by type.
func All() ([]Entry, error) {
	once.Do(load)
	if loadErr != nil {
		return nil, loadErr
	}
	out := append([]Entry(nil), entries...)
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out, nil
}

// Lookup returns the entry for a canonical event type.
func Lookup(eventType string) (Entry, bool) {
	once.Do(load)
	if loadErr != nil {
		return Entry{}, false
	}
	e, ok := byType[eventType]
	return e, ok
}

// pastTense reports whether a fact segment reads as a past-tense fact.
// It is a heuristic on purpose (§21: "Past-tense name"); legitimate
// exceptions are listed in nonPastTense with the reason they are accepted.
var pastTense = regexp.MustCompile(`((ed|en|wn|set|off|paid|made|sent|held|built|run|given|taken|bound|back)|^became-.+)$`)

// nonPastTense are catalogue entries the standard itself lists whose last
// segment is not a past participle. They are accepted explicitly so that a
// new one has to be a conscious decision, not a lint gap.
var nonPastTense = map[string]string{
	"com.zoikosuite.tax.tax-payment.due":                             "§19 lists it as a state-reached signal: the payment became due",
	"com.zoikosuite.legal.obligation.due":                            "§19 lists it as a state-reached signal: the obligation became due",
	"com.zoikosuite.business.deadline.due":                           "§19 lists it as a state-reached signal: the deadline became due",
	"com.zoikosuite.legal.license.expiring":                          "§19 lists it as a lead-time signal ahead of expiry",
	"com.zoikosuite.banking.payment-execution-state.pending":         "extension: provider state fact, the payment is pending",
	"com.zoikosuite.banking.payment-execution-state.pending-unknown": "extension: provider result cannot be proven yet (section 15 status.unknown)",
	"com.zoikosuite.banking.payment-initiation-attempt.ambiguous":    "extension: provider outcome is ambiguous (section 15 status.unknown)",
}

var typeShape = regexp.MustCompile(`^com\.zoikosuite\.[a-z0-9-]+\.[a-z0-9-]+(\.[a-z0-9-]+)+$`)
var versionSuffix = regexp.MustCompile(`.v[0-9]+$`)

// Lint checks one entry against the §21 semantic-lint gate and returns every
// violation (empty when it passes).
func Lint(e Entry) []string {
	var v []string
	if !typeShape.MatchString(e.Type) {
		v = append(v, "type must match com.zoikosuite.<domain>.<aggregate>.<fact>")
		return v
	}
	if m := regexp.MustCompile(`\.v[0-9]+$`).FindString(e.Type); m != "" {
		v = append(v, "type must not carry a version suffix")
	}
	known := false
	for _, d := range Domains {
		if e.Domain() == d {
			known = true
		}
	}
	if !known && e.Origin == "extension" {
		for _, d := range ExtensionDomains {
			if e.Domain() == d {
				known = true
			}
		}
	}
	if !known {
		v = append(v, fmt.Sprintf("domain %q is not a §19 domain", e.Domain()))
	}
	if !pastTense.MatchString(e.Fact()) {
		if _, ok := nonPastTense[e.Type]; !ok && !(e.Origin == "extension" && extensionNamingDebt[e.Type]) {
			v = append(v, fmt.Sprintf("fact %q is not past tense (§5.1, E-01) and has no recorded exemption", e.Fact()))
		}
	}
	switch last := e.Fact(); last {
	case "updated", "changed", "modified", "deleted":
		// §0/§27: a generic CRUD verb for a material fact is prohibited. The
		// §19 catalogue has a few (org.legal-entity.updated, subscription.changed)
		// and they are listed in genericAllowed rather than waved through.
		if _, ok := genericAllowed[e.Type]; !ok {
			v = append(v, fmt.Sprintf("generic verb %q is a prohibited anti-pattern (§27)", last))
		}
	}
	for name, val := range map[string]string{"producer": e.Producer, "subject": e.Subject, "consumers": e.Consumers, "family": e.Family} {
		if strings.TrimSpace(val) == "" {
			v = append(v, name+" is required")
		}
	}
	sort.Strings(v)
	return v
}

// genericAllowed are generic-verb types that §19 itself lists. They are kept
// because the standard catalogues them; the lint refuses any NEW one.
var genericAllowed = map[string]string{
	"com.zoikosuite.org.legal-entity.updated":                   "listed in §19 (dependent read models)",
	"com.zoikosuite.org.establishment.changed":                  "listed in §19 (tax, reporting)",
	"com.zoikosuite.org.ownership.changed":                      "listed in §19 (consolidation)",
	"com.zoikosuite.commercial.subscription.changed":            "listed in §19 (entitlement, billing)",
	"com.zoikosuite.workforce.worker-financial-profile.changed": "listed in §19 (payroll dimensions)",
	"com.zoikosuite.security.key-policy.changed":                "listed in §19 (encryption services)",
	"com.zoikosuite.governance.residency-policy.changed":        "listed in §19 (routing, data platform)",
}

// Spec returns the entries listed in ZS-EVENT-001 section 19 (not extensions).
func Spec() ([]Entry, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range all {
		if e.Origin == "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// FactPath is the part of the type that identifies the message within its
// domain. Most types are domain.aggregate.fact; some have a sub-aggregate
// (accounting.kernel.migration-batch.approved), and keying by the last segment
// alone would make "kernel.approved" collide across them.
//
// withAggregate=true returns "aggregate.<path>", unique within the domain (a
// component key); false returns just the path below the aggregate (the message
// name within its channel).
func (e Entry) FactPath(withAggregate bool) string {
	s := segments(e.Type)
	if withAggregate {
		return strings.Join(s[1:], ".")
	}
	return strings.Join(s[2:], ".")
}
