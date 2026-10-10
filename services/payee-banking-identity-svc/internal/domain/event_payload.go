package domain

import "time"

// DestinationEventPayload is the ONLY shape a PayeeDestination may take on the
// event bus.
//
// PayeeDestination itself must never be published: it carries the full
// AccountIdentifier, and it has no JSON tags, so marshalling it writes that
// value to Kafka verbatim. HTTP-response masking (maskIfNotPrivileged) is a
// read-path control and cannot protect an event, which is serialized before the
// response is built.
//
// The fields are deliberately the minimum a consumer needs to react to a change
// (identify the destination and party, see its state, and detect that the
// underlying account changed): no account identifier, payee name or
// institution. Fingerprint is a digest of institution|account|currency, kept
// because the spec's fraud controls consume it; AccountLast4 is the masked
// display value. A consumer that needs more must call this service's
// purpose-limited read.
type DestinationEventPayload struct {
	DestinationID string            `json:"destination_id"`
	TenantID      string            `json:"tenant_id"`
	LegalEntityID string            `json:"legal_entity_id"`
	PartyRef      string            `json:"party_ref"`
	Scope         string            `json:"scope"`
	Status        DestinationStatus `json:"status"`
	SourceType    SourceType        `json:"source_type"`
	CountryCode   string            `json:"country_code"`
	Currency      string            `json:"currency"`
	AccountLast4  string            `json:"account_last4"`
	Fingerprint   string            `json:"fingerprint"`

	SupersededByDestinationID string `json:"superseded_by_destination_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewDestinationEventPayload projects a destination onto its safe event shape.
// tenantID is passed explicitly (the verified request tenant) rather than read
// from d.TenantID, which is a nullable column pointer and has been absent on
// returned rows.
func NewDestinationEventPayload(d *PayeeDestination, tenantID string) DestinationEventPayload {
	return DestinationEventPayload{
		DestinationID: d.DestinationID, TenantID: tenantID, LegalEntityID: d.LegalEntityID,
		PartyRef: d.PartyRef, Scope: d.Scope, Status: d.Status, SourceType: d.SourceType,
		CountryCode: d.CountryCode, Currency: d.Currency,
		AccountLast4: d.AccountLast4, Fingerprint: d.Fingerprint,
		SupersededByDestinationID: d.SupersededByDestinationID,
		CreatedAt:                 d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}
