package catalogue

import (
	"regexp"
	"strings"
)

// serviceProducers maps the catalogue's producer names that do not end in
// "Service" to the service that is meant. A producer absent from this mapping
// and not written as "<Name> Service" is a role or a domain (AR, Legal, Period
// Service...) that no single deployed service owns yet; it is deliberately left
// unowned rather than guessed.
var serviceProducers = map[string]string{
	"Accounting Kernel":          "general-ledger-svc",
	"Payment Initiation Adapter": "payment-initiation-adapter-svc",
}

// ambiguousProducers are written "<Name> Service" but name a capability that more
// than one deployed service emits, so they must not be turned into a service
// name. The period facts are the known case (accounting-period-svc and
// financial-close-svc both emit them).
var ambiguousProducers = map[string]bool{
	"Period Service":       true,
	"Party Service":        true,
	"Legal Entity Service": true,
	"Import Service":       true,
	"Expense Service":      true,
	"Document Service":     true,
	"Connector Service":    true,
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// ServiceName returns the deployed service that the entry's producer names, or
// "" when the producer is a role, a domain or ambiguous (see above).
func (e Entry) ServiceName() string {
	p := strings.TrimSpace(e.Producer)
	if s, ok := serviceProducers[p]; ok {
		return s
	}
	if ambiguousProducers[p] || !strings.HasSuffix(p, " Service") {
		return ""
	}
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(strings.TrimSuffix(p, " Service")), "-"), "-")
	if slug == "" {
		return ""
	}
	return slug + "-svc"
}

// ServiceGrants returns service name -> the event types the catalogue assigns to
// it, for entries whose producer names a deployed service.
func ServiceGrants() (map[string][]string, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, e := range all {
		if s := e.ServiceName(); s != "" {
			out[s] = append(out[s], e.Type)
		}
	}
	return out, nil
}
