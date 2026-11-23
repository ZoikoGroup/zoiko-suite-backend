package policy

import (
	"fmt"
	"strings"
	"time"

	"zoiko.io/eventing/envelope"
)

// TransferRule is one explicit permission, as a GTRM/data-transfer policy would
// grant it: events may flow From one residency region To another, up to and
// including MaxClassification (section 14: "default deny for data-bearing
// replication unless policy explicitly permits destination/purpose/
// classification"). Purpose is recorded on the evidence; it is the grant's
// stated reason, not something this package can verify.
type TransferRule struct {
	From              string
	To                string
	MaxClassification envelope.Classification
	Purpose           string
}

// Decision is the outcome of a residency check and the control evidence for it
// (EV-T10: "denies delivery and records control evidence").
type Decision struct {
	Allowed        bool
	Reason         string
	EventRegion    string
	ConsumerRegion string
	Classification envelope.Classification
	Consumer       string
	EventType      string
	TenantID       string
	Purpose        string // of the rule that allowed it, when cross-region
	At             time.Time
}

// Residency decides whether an event may be delivered to a consumer.
type Residency struct {
	rules []TransferRule
	now   func() time.Time
}

// NewResidency builds a policy from explicit transfer rules. No rules means no
// cross-region delivery at all.
func NewResidency(rules []TransferRule) (*Residency, error) {
	for _, r := range rules {
		if strings.TrimSpace(r.From) == "" || strings.TrimSpace(r.To) == "" || r.From == r.To {
			return nil, fmt.Errorf("policy: a transfer rule needs two different regions (%q -> %q)", r.From, r.To)
		}
		if rank(r.MaxClassification) < 0 {
			return nil, fmt.Errorf("policy: transfer rule %s->%s has an unknown classification %q", r.From, r.To, r.MaxClassification)
		}
		if strings.TrimSpace(r.Purpose) == "" {
			return nil, fmt.Errorf("policy: transfer rule %s->%s needs a purpose", r.From, r.To)
		}
	}
	return &Residency{rules: append([]TransferRule(nil), rules...), now: time.Now}, nil
}

func rank(c envelope.Classification) int {
	switch c {
	case envelope.Public:
		return 0
	case envelope.Internal:
		return 1
	case envelope.Confidential:
		return 2
	case envelope.Restricted:
		return 3
	}
	return -1
}

// Decide applies the policy. It fails closed: an event or consumer region that
// is not stated cannot be proven to match, and an unknown classification is
// treated as restricted.
func (r *Residency) Decide(consumer, eventType, tenantID, eventRegion, consumerRegion string, class envelope.Classification) Decision {
	if rank(class) < 0 {
		class = envelope.Restricted
	}
	d := Decision{EventRegion: eventRegion, ConsumerRegion: consumerRegion, Classification: class,
		Consumer: consumer, EventType: eventType, TenantID: tenantID, At: r.now().UTC()}
	switch {
	case strings.TrimSpace(consumerRegion) == "":
		d.Reason = "consumer region is not configured"
	case strings.TrimSpace(eventRegion) == "":
		d.Reason = "event names no residency region, so it cannot be proven to belong here"
	case eventRegion == consumerRegion:
		d.Allowed, d.Reason = true, "same region"
	default:
		d.Reason = fmt.Sprintf("no transfer rule permits %s data from %s to %s", class, eventRegion, consumerRegion)
		for _, rule := range r.rules {
			if rule.From == eventRegion && rule.To == consumerRegion && rank(class) <= rank(rule.MaxClassification) {
				d.Allowed, d.Purpose = true, rule.Purpose
				d.Reason = fmt.Sprintf("permitted by transfer rule up to %s", rule.MaxClassification)
				break
			}
		}
	}
	return d
}
