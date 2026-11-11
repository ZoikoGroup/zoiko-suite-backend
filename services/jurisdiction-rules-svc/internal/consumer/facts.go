package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// JUR-1: establishment, registration and address events of tenant-entity-registry-svc, on the same topic as the
// entity events. They feed the facts a jurisdiction decision rests on.
const (
	EventEstablishmentCreated     = "establishment.created"
	EventEstablishmentActivated   = "establishment.activated"
	EventEstablishmentDeactivated = "establishment.deactivated"
	EventEstablishmentClosed      = "establishment.closed"
	EventRegistrationCreated      = "registration.created"
	EventRegistrationVerified     = "registration.verified"
	EventRegistrationRenewed      = "registration.renewed"
	EventRegistrationSuspended    = "registration.suspended"
	EventRegistrationExpired      = "registration.expired"
	EventRegistrationRevoked      = "registration.revoked"
	EventRegistrationSuperseded   = "registration.superseded"
	EventAddressCreated           = "address.created"
	EventAddressSuperseded        = "address.superseded"
)

// FactStore is the consumer's view of the facts projection.
type FactStore interface {
	ApplyRegistryFact(ctx context.Context, eventID, eventType string, e domain.RegistryFactEvent) (bool, error)
	ApplyAddressCountry(ctx context.Context, eventID, eventType string, e domain.AddressCountryEvent) (bool, error)
}

// WithFacts enables the establishment, registration and address projection.
func (c *EntityConsumer) WithFacts(s FactStore) *EntityConsumer {
	c.facts = s
	return c
}

type factPayload struct {
	TenantID            string `json:"tenant_id"`
	LegalEntityID       string `json:"legal_entity_id"`
	EstablishmentID     string `json:"establishment_id"`
	EstablishmentType   string `json:"establishment_type"`
	AddressID           string `json:"address_id"`
	RegistrationID      string `json:"registration_id"`
	RegistrationType    string `json:"registration_type"`
	Jurisdiction        string `json:"jurisdiction"`
	HolderLegalEntityID string `json:"holder_legal_entity_id"`
	Status              string `json:"status"`
	ExpiryDate          string `json:"expiry_date"`
	EffectiveDate       string `json:"effective_date"`
	Country             string `json:"country"`
}

func isFactEvent(t string) bool {
	switch t {
	case EventEstablishmentCreated, EventEstablishmentActivated, EventEstablishmentDeactivated, EventEstablishmentClosed,
		EventRegistrationCreated, EventRegistrationVerified, EventRegistrationRenewed, EventRegistrationSuspended,
		EventRegistrationExpired, EventRegistrationRevoked, EventRegistrationSuperseded,
		EventAddressCreated, EventAddressSuperseded:
		return true
	}
	return false
}

// datePart returns the YYYY-MM-DD of a date or RFC 3339 instant.
func datePart(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) < 10 {
		return "", fmt.Errorf("date %q", s)
	}
	if _, err := time.Parse("2006-01-02", s[:10]); err != nil {
		return "", fmt.Errorf("date %q", s)
	}
	return s[:10], nil
}

func (c *EntityConsumer) handleFact(ctx context.Context, env inbound) error {
	if c.facts == nil {
		return nil
	}
	skip := func(err error) error {
		c.log.Error("registry fact event: cannot be applied — skipped",
			zap.String("event_id", env.EventID), zap.String("event_type", env.EventType), zap.Error(err))
		c.count(env.EventType, "skipped")
		return nil
	}
	if env.EventID == "" || strings.TrimSpace(env.TenantID) == "" {
		return skip(fmt.Errorf("%w: event_id and tenant_id are required", domain.ErrInvalidEvent))
	}
	var p factPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return skip(fmt.Errorf("%w: payload: %v", domain.ErrInvalidEvent, err))
	}
	if p.TenantID != "" && p.TenantID != env.TenantID {
		return skip(fmt.Errorf("%w: payload tenant differs from envelope tenant", domain.ErrInvalidEvent))
	}
	version := env.ObjectVersion
	if version < 1 {
		version = 1
	}

	var applied bool
	var err error
	switch env.EventType {
	case EventAddressCreated, EventAddressSuperseded:
		addr := firstNonEmpty(p.AddressID, env.ObjectID)
		if addr == "" {
			return skip(fmt.Errorf("%w: no address_id", domain.ErrInvalidEvent))
		}
		country := ""
		if env.EventType == EventAddressCreated {
			country = strings.ToUpper(strings.TrimSpace(p.Country))
		}
		applied, err = c.facts.ApplyAddressCountry(ctx, env.EventID, env.EventType,
			domain.AddressCountryEvent{TenantID: env.TenantID, AddressID: addr, Country: country, Version: version})
	default:
		row := domain.FactRow{EventID: env.EventID, Status: strings.TrimSpace(p.Status), ObjectVersion: version}
		entity := ""
		switch {
		case strings.HasPrefix(env.EventType, "establishment."):
			row.FactKind, row.FactID = domain.FactEstablishment, firstNonEmpty(p.EstablishmentID, env.ObjectID)
			row.Subtype, row.AddressID = p.EstablishmentType, p.AddressID
			entity = firstNonEmpty(p.LegalEntityID, env.LegalEntityID)
		default:
			row.FactKind, row.FactID = domain.FactRegistration, firstNonEmpty(p.RegistrationID, env.ObjectID)
			row.Subtype, row.Jurisdiction, row.ExpiryDate = p.RegistrationType, strings.ToUpper(strings.TrimSpace(p.Jurisdiction)), strings.TrimSpace(p.ExpiryDate)
			entity = strings.TrimSpace(p.HolderLegalEntityID)
			if entity == "" {
				return nil // held by a party, not a legal entity: not a fact about an entity's jurisdiction
			}
		}
		if row.FactID == "" || entity == "" || row.Status == "" {
			return skip(fmt.Errorf("%w: fact id, legal entity and status are required", domain.ErrInvalidEvent))
		}
		d, derr := datePart(p.EffectiveDate)
		if derr != nil {
			return skip(fmt.Errorf("%w: effective_date: %v", domain.ErrInvalidEvent, derr))
		}
		row.EffectiveDate = d
		if row.ExpiryDate != "" {
			if row.ExpiryDate, derr = datePart(row.ExpiryDate); derr != nil {
				return skip(fmt.Errorf("%w: expiry_date: %v", domain.ErrInvalidEvent, derr))
			}
		}
		applied, err = c.facts.ApplyRegistryFact(ctx, env.EventID, env.EventType, domain.RegistryFactEvent{TenantID: env.TenantID, LegalEntityID: entity, Row: row})
	}
	switch {
	case errors.Is(err, domain.ErrInvalidEvent):
		return skip(err)
	case err != nil:
		c.count(env.EventType, "retry")
		return err
	}
	if applied {
		c.count(env.EventType, "applied")
	} else {
		c.count(env.EventType, "duplicate")
	}
	return nil
}
