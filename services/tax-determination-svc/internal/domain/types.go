package domain

import (
	"errors"
	"time"
)

var (
	ErrTaxDeterminationNotFound = errors.New("tax determination not found")
	ErrAlreadyOverridden        = errors.New("tax determination is already overridden")

	// ── TAX-03 input contract ────────────────────────────────────────────

	ErrInvalidSupplyKind = errors.New("supply_kind must be one of GOODS, SERVICES, DIGITAL_SERVICES")
	ErrInvalidSupplyType = errors.New("supply_type must be one of B2B, B2C, B2G")

	// ErrB2BNeedsBuyerRegistration guards the fact that decides reverse charge.
	// A buyer described as a business but carrying no registration in the place
	// of supply is a B2C supply in substance, and treating it as B2B is how a
	// cross-border supply goes untaxed on both sides.
	ErrB2BNeedsBuyerRegistration = errors.New("supply_type B2B requires buyer_tax_registration_id — a business buyer with no registration in the place of supply is a B2C supply in substance")

	// ErrExemptionNeedsReason enforces INV-10 for the one number on a
	// determination that reduces tax without any rule having said so.
	ErrExemptionNeedsReason = errors.New("exempt_amount greater than zero requires exemption_reason")

	// ErrInvalidCurrency covers shape only — REF-02 Currency Registry does not
	// exist, so nothing can say whether a well-formed code is one this entity
	// transacts in.
	ErrInvalidCurrency = errors.New("currency must be a 3-letter ISO 4217 code, e.g. GBP")

	ErrInvalidSupplyDate = errors.New("supply_date must be an ISO calendar date, e.g. 2026-08-24")

	// ErrJurisdictionUnknown is returned when jurisdiction-rules-svc does not
	// recognise a jurisdiction named on the request.
	ErrJurisdictionUnknown = errors.New("jurisdiction is not recognised by jurisdiction-rules-svc")

	// ErrJurisdictionUnverifiable means jurisdiction-rules-svc could not be
	// reached. Distinct from ErrJurisdictionUnknown on purpose: "cannot answer"
	// is never "no", and a governed determination fails closed rather than
	// proceeding against an unvalidated jurisdiction.
	ErrJurisdictionUnverifiable = errors.New("jurisdiction could not be verified — jurisdiction-rules-svc unreachable")

	// ErrRegistryUnavailable means the seller's tax registration could not be
	// resolved. Also fail-closed: a determination that silently omits the
	// seller's registration status is one whose central fact is unknown.
	ErrRegistryUnavailable = errors.New("seller tax registration could not be resolved — tenant-entity-registry-svc unreachable")
)

// ValidCurrencyCode reports whether s has the shape of an ISO 4217 alphabetic
// code. Shape only: embedding a currency list would hardcode reference data
// that doctrine puts in a registry service, consumed as versioned data.
func ValidCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

type DeterminationStatus string

const (
	StatusCalculated DeterminationStatus = "CALCULATED"
	StatusApplied    DeterminationStatus = "APPLIED"
	StatusOverridden DeterminationStatus = "OVERRIDDEN"
	StatusReversed   DeterminationStatus = "REVERSED"
)

// SupplyKind is TAX-03's product/service classification at the level that
// changes the treatment. Every VAT and GST system distinguishes these three:
// goods follow where they move, services follow where the customer belongs, and
// digital services carry their own destination rules.
type SupplyKind string

const (
	SupplyKindGoods           SupplyKind = "GOODS"
	SupplyKindServices        SupplyKind = "SERVICES"
	SupplyKindDigitalServices SupplyKind = "DIGITAL_SERVICES"

	// SupplyKindUnspecified marks rows written before migration 000003 added
	// the field. Never acceptable on a new determination.
	SupplyKindUnspecified SupplyKind = "UNSPECIFIED"
)

// SupplyType is TAX-03's B2B/B2C fact. B2G is separate from B2B because public
// bodies attract distinct e-invoicing and withholding obligations in a number
// of jurisdictions.
type SupplyType string

const (
	SupplyTypeB2B SupplyType = "B2B"
	SupplyTypeB2C SupplyType = "B2C"
	SupplyTypeB2G SupplyType = "B2G"

	SupplyTypeUnspecified SupplyType = "UNSPECIFIED"
)

// PlaceOfSupplyBasis records how the place of supply was arrived at.
//
// Today only CallerAsserted occurs. §9.J expects place-of-supply RULES to
// decide it from establishments, supply kind and B2B/B2C facts, and those rules
// are jurisdiction-pack data (TAX-02) that no pack currently carries. Recording
// the basis keeps that limitation in the evidence rather than letting a
// caller-supplied jurisdiction read as something this engine determined. When
// packs carry the rules the basis becomes RULE_DERIVED, and a disagreement
// between what the caller asserted and what the rules say becomes auditable.
type PlaceOfSupplyBasis string

const (
	PlaceOfSupplyCallerAsserted PlaceOfSupplyBasis = "CALLER_ASSERTED"
	PlaceOfSupplyRuleDerived    PlaceOfSupplyBasis = "RULE_DERIVED"
)

var validSupplyKinds = map[SupplyKind]bool{
	SupplyKindGoods: true, SupplyKindServices: true, SupplyKindDigitalServices: true,
}

var validSupplyTypes = map[SupplyType]bool{
	SupplyTypeB2B: true, SupplyTypeB2C: true, SupplyTypeB2G: true,
}

// ValidSupplyKind reports whether k may be supplied on a new determination.
// UNSPECIFIED is excluded: it is a backfill marker, not a classification.
func ValidSupplyKind(k SupplyKind) bool { return validSupplyKinds[k] }

// ValidSupplyType reports whether t may be supplied on a new determination.
func ValidSupplyType(t SupplyType) bool { return validSupplyTypes[t] }

type TaxDetermination struct {
	DeterminationID string `json:"determination_id"`
	TenantID        string `json:"tenant_id"`
	TransactionID   string `json:"transaction_id"`
	SourceModule    string `json:"source_module"` // INVOICE, PAYROLL, PURCHASE_ORDER, AP, AR
	LegalEntityID   string `json:"legal_entity_id"`
	JurisdictionID  string `json:"jurisdiction_id"`
	RuleID          string `json:"rule_id,omitempty"`
	// TaxLogicSnapshotID is a content-addressed (SHA-256) reference over the
	// actual rule fields applied at determination time — pins this
	// determination to the exact rule content used, independent of later
	// edits to the mutable rule row rule_id points at. Nil only when the
	// zero-tax fallback was used (tax-rules-svc unreachable, no real rule
	// content to snapshot).
	TaxLogicSnapshotID  *string             `json:"tax_logic_snapshot_id,omitempty"`
	TaxCategory         string              `json:"tax_category"`
	GrossAmount         float64             `json:"gross_amount"`
	TaxableAmount       float64             `json:"taxable_amount"`
	TaxRatePercentage   float64             `json:"tax_rate_percentage"`
	CalculatedTaxAmount float64             `json:"calculated_tax_amount"`
	ExemptAmount        float64             `json:"exempt_amount"`
	Currency            string              `json:"currency"`
	Status              DeterminationStatus `json:"status"`
	EffectiveFrom       string              `json:"effective_from"`
	EffectiveTo         *string             `json:"effective_to,omitempty"`
	EvaluatedAt         time.Time           `json:"evaluated_at"`
	EvaluatedBy         string              `json:"evaluated_by"`
	CreatedAt           time.Time           `json:"created_at"`
	UpdatedAt           time.Time           `json:"updated_at"`

	// ── TAX-03 inputs, preserved on the determination ─────────────────────
	//
	// Kept on the record rather than only on the request, because §9.J's
	// retrieval requirement and Appendix B's calculation evidence object both
	// need the determination to be reconstructable from what it was given. A
	// tax position defended two years later is defended with these facts.

	SellerPartyID string `json:"seller_party_id"`
	BuyerPartyID  string `json:"buyer_party_id"`

	SellerEstablishmentID *string `json:"seller_establishment_id,omitempty"`
	BuyerEstablishmentID  *string `json:"buyer_establishment_id,omitempty"`

	ShipFromJurisdictionID *string `json:"ship_from_jurisdiction_id,omitempty"`
	ShipToJurisdictionID   *string `json:"ship_to_jurisdiction_id,omitempty"`

	SupplyJurisdictionID string             `json:"supply_jurisdiction_id"`
	SupplyDate           *string            `json:"supply_date,omitempty"`
	PlaceOfSupplyBasis   PlaceOfSupplyBasis `json:"place_of_supply_basis"`

	ProductClassification string     `json:"product_classification"`
	SupplyKind            SupplyKind `json:"supply_kind"`
	SupplyType            SupplyType `json:"supply_type"`

	BuyerTaxRegistrationID *string `json:"buyer_tax_registration_id,omitempty"`

	ExemptionReason         *string `json:"exemption_reason,omitempty"`
	ExemptionCertificateRef *string `json:"exemption_certificate_ref,omitempty"`

	// SellerRegistrationID and SellerRegistrationStatus are the §9.J
	// server-resolved "Registrations" input, read from
	// tenant-entity-registry-svc's tax identity bundles for the seller's entity
	// in the place of supply.
	//
	// Both nil when the seller holds no bundle there. That is a real and common
	// state, not an error: an unregistered seller is precisely the fact that
	// decides whether tax is charged at all, so it is recorded rather than
	// treated as a missing lookup.
	SellerRegistrationID     *string `json:"seller_registration_id,omitempty"`
	SellerRegistrationStatus *string `json:"seller_registration_status,omitempty"`
}

type DetermineTaxRequest struct {
	TransactionID  string  `json:"transaction_id"`
	SourceModule   string  `json:"source_module"`
	LegalEntityID  string  `json:"legal_entity_id"`
	JurisdictionID string  `json:"jurisdiction_id"`
	TaxCategory    string  `json:"tax_category"`
	GrossAmount    float64 `json:"gross_amount"`
	ExemptAmount   float64 `json:"exempt_amount,omitempty"`
	Currency       string  `json:"currency"`
	EffectiveFrom  string  `json:"effective_from"`
	EvaluatedBy    string  `json:"evaluated_by"`

	// ── TAX-03 required business/source inputs (§9.J) ──────────────────────
	//
	// Which rate applies is not decided by a jurisdiction and a category alone.
	// It depends on who is selling and buying, where each is established, what
	// moves where, what is being supplied, and whether the buyer carries its own
	// registration. Without these the caller has already answered the question
	// this service exists to answer.

	// SellerPartyID and BuyerPartyID are required. Unvalidated: no single party
	// master exists that both a selling entity and a customer resolve against.
	SellerPartyID string `json:"seller_party_id"`
	BuyerPartyID  string `json:"buyer_party_id"`

	// Establishments. Optional — ORG-08 Address & Establishment does not exist,
	// so nothing can issue or check one.
	SellerEstablishmentID string `json:"seller_establishment_id,omitempty"`
	BuyerEstablishmentID  string `json:"buyer_establishment_id,omitempty"`

	// Ship-from/to as jurisdiction references. Optional — a supply of services
	// often has neither — but validated against jurisdiction-rules-svc when
	// present, because a movement between jurisdictions nobody recognises is a
	// determination nobody can defend.
	ShipFromJurisdictionID string `json:"ship_from_jurisdiction_id,omitempty"`
	ShipToJurisdictionID   string `json:"ship_to_jurisdiction_id,omitempty"`

	// SupplyJurisdictionID is the place of supply: the jurisdiction whose rules
	// govern this transaction. Required.
	//
	// Caller-asserted, and recorded as such — see Determination.PlaceOfSupplyBasis.
	SupplyJurisdictionID string `json:"supply_jurisdiction_id"`

	// SupplyDate is the tax point: the date the supply is treated as taking
	// place. Required, and distinct from EffectiveFrom, which is when the rule
	// version applies. They are usually the same day and legitimately differ
	// for a supply invoiced in one period and delivered in another.
	SupplyDate string `json:"supply_date"`

	ProductClassification string     `json:"product_classification"`
	SupplyKind            SupplyKind `json:"supply_kind"`
	SupplyType            SupplyType `json:"supply_type"`

	// BuyerTaxRegistrationID is the buyer's registration in the place of supply.
	// Optional, and consequential: its presence is what makes a cross-border
	// B2B supply reverse-charge in most VAT systems. Required when SupplyType
	// is B2B — a business buyer with no registration is a B2C supply in
	// substance, and treating it as B2B is how tax goes uncharged.
	BuyerTaxRegistrationID string `json:"buyer_tax_registration_id,omitempty"`

	// Exemption facts. ExemptAmount says how much; these say why and on what
	// authority. Required whenever ExemptAmount is greater than zero (INV-10).
	ExemptionReason         string `json:"exemption_reason,omitempty"`
	ExemptionCertificateRef string `json:"exemption_certificate_ref,omitempty"`
}

type OverrideTaxRequest struct {
	OverriddenTaxAmount float64 `json:"overridden_tax_amount"`
	Reason              string  `json:"reason"`
	UpdatedBy           string  `json:"updated_by"`
}
