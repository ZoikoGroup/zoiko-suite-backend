// Package policy holds the platform controls of ZS-EVENT-001 section 14 and
// E-02/E-13: who may produce an event type, and whether an event may cross a
// residency boundary to a consumer. Both are pure decisions with no I/O, so a
// service can apply them at the edge (before the outbox, before the inbox) and
// test them without a broker.
package policy

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"zoiko.io/eventing/catalogue"
	"zoiko.io/eventing/envelope"
)

// Errors returned by ProducerACL.Check.
var (
	// ErrNotOwner: the type is owned by another service (E-02, EV-T23).
	ErrNotOwner = errors.New("policy: service does not own this event type")
	// ErrUnowned: strict mode and no service owns the type.
	ErrUnowned = errors.New("policy: event type has no registered producer")
	// ErrNotCatalogued: strict mode and the type is not in the catalogue.
	ErrNotCatalogued = errors.New("policy: event type is not in the event catalogue")
	// ErrInvalidType: not a canonical event type.
	ErrInvalidType = errors.New("policy: not a canonical event type")
)

// ProducerACL answers "may this service publish this event type". Ownership is
// exclusive: one type has one producer.
type ProducerACL struct {
	exact    map[string]string // event type -> service
	prefixes []prefixGrant     // "com.zoikosuite.d.a." -> service
	strict   bool
}

type prefixGrant struct{ prefix, service string }

// NewProducerACL builds an ACL from grants: service name -> patterns. A pattern
// is a full canonical event type, or a family written
// "com.zoikosuite.<domain>.<aggregate>.*". It refuses a grant set in which two
// services would own overlapping types, because a type with two producers is
// exactly what E-02 forbids.
//
// In strict mode a type nobody owns, or that is not in the catalogue, is
// refused as well; lenient mode only enforces ownership where it is declared,
// which lets a platform adopt the ACL service by service.
func NewProducerACL(grants map[string][]string, strict bool) (*ProducerACL, error) {
	a := &ProducerACL{exact: map[string]string{}, strict: strict}
	services := make([]string, 0, len(grants))
	for s := range grants {
		services = append(services, s)
	}
	sort.Strings(services) // deterministic errors
	for _, svc := range services {
		if strings.TrimSpace(svc) == "" {
			return nil, errors.New("policy: a grant needs a service name")
		}
		for _, pat := range grants[svc] {
			if strings.HasSuffix(pat, ".*") {
				prefix := strings.TrimSuffix(pat, "*") // keeps the trailing dot
				if !envelope.ValidType(prefix + "x") {
					return nil, fmt.Errorf("policy: %q is not a valid family pattern", pat)
				}
				a.prefixes = append(a.prefixes, prefixGrant{prefix, svc})
				continue
			}
			if !envelope.ValidType(pat) {
				return nil, fmt.Errorf("policy: %q is not a canonical event type", pat)
			}
			if prev, dup := a.exact[pat]; dup && prev != svc {
				return nil, fmt.Errorf("policy: %q is granted to both %s and %s", pat, prev, svc)
			}
			a.exact[pat] = svc
		}
	}
	for t, svc := range a.exact {
		for _, p := range a.prefixes {
			if strings.HasPrefix(t, p.prefix) && p.service != svc {
				return nil, fmt.Errorf("policy: %q (%s) falls under %s*, granted to %s", t, svc, p.prefix, p.service)
			}
		}
	}
	for i, p := range a.prefixes {
		for _, q := range a.prefixes[i+1:] {
			if p.service != q.service && (strings.HasPrefix(p.prefix, q.prefix) || strings.HasPrefix(q.prefix, p.prefix)) {
				return nil, fmt.Errorf("policy: families %s* (%s) and %s* (%s) overlap", p.prefix, p.service, q.prefix, q.service)
			}
		}
	}
	return a, nil
}

// Owner returns the service that owns eventType, if one is declared.
func (a *ProducerACL) Owner(eventType string) (string, bool) {
	if s, ok := a.exact[eventType]; ok {
		return s, true
	}
	best, svc := "", ""
	for _, p := range a.prefixes {
		if strings.HasPrefix(eventType, p.prefix) && len(p.prefix) > len(best) {
			best, svc = p.prefix, p.service
		}
	}
	return svc, best != ""
}

// Check refuses a service publishing an event type it does not own.
func (a *ProducerACL) Check(service, eventType string) error {
	if !envelope.ValidType(eventType) {
		return fmt.Errorf("%w: %q", ErrInvalidType, eventType)
	}
	if a.strict {
		if _, ok := catalogue.Lookup(eventType); !ok {
			return fmt.Errorf("%w: %q", ErrNotCatalogued, eventType)
		}
	}
	owner, ok := a.Owner(eventType)
	if !ok {
		if a.strict {
			return fmt.Errorf("%w: %q", ErrUnowned, eventType)
		}
		return nil
	}
	if owner != service {
		return fmt.Errorf("%w: %q is owned by %s, not %s", ErrNotOwner, eventType, owner, service)
	}
	return nil
}

// CheckSpec checks the Type and Service of an envelope spec. Call it just
// before envelope.New in the producer's own transaction, so a service cannot
// even build an event it does not own:
//
//	if err := acl.CheckSpec(spec); err != nil { return err }
//	env, err := envelope.New(spec)
func (a *ProducerACL) CheckSpec(s envelope.Spec) error { return a.Check(s.Service, s.Type) }
