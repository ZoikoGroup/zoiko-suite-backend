// Package domain defines the authoritative domain types for
// commercial-account-svc.
//
// Per docs/original_doc/zoiko_suite_doc7.txt §3 (Five-Plane Trust Doctrine),
// this service owns Plane 1 — Zoiko Commercial Account: the verified
// customer record that buys ZoikoSuite, and the actor-to-organization
// membership relationship. It is deliberately separate from
// tenant-entity-registry-svc (Plane 2 — Tenant Business Operations), which
// owns the tenant's own operating facts. A workspace's non-payment
// restriction must never mutate tenant business data, and this service
// never touches tenant_id-scoped operational tables — see §3's own
// constraint: "ZoikoSuite subscription payment state may affect access to
// ZoikoSuite, but it may never mutate the tenant's underlying accounting,
// payroll, billing, HR or legal facts."
package domain

import "time"

// CommercialAccountStatus is the account's own operational state, distinct
// from any workspace's billing_classification (owned by
// tenant-entity-registry-svc) and distinct from entitlement state (a future
// entitlement-service concern, not modeled here yet).
type CommercialAccountStatus string

const (
	CommercialAccountStatusActive     CommercialAccountStatus = "ACTIVE"
	CommercialAccountStatusPastDue    CommercialAccountStatus = "PAST_DUE"
	CommercialAccountStatusRestricted CommercialAccountStatus = "RESTRICTED"
	CommercialAccountStatusSuspended  CommercialAccountStatus = "SUSPENDED"
	CommercialAccountStatusCanceled   CommercialAccountStatus = "CANCELED"
)

// CommercialAccount is the verified customer record that buys ZoikoSuite
// (doc7 §A4). OrganizationID is this platform's existing tenant_id — see
// package doc comment: doc7's "organization" concept is satisfied by the
// tenant object tenant-entity-registry-svc already owns, so this is a plain
// string reference, not a cross-service foreign key.
type CommercialAccount struct {
	CommercialAccountID  string                  `json:"commercial_account_id"`
	OrganizationID       string                  `json:"organization_id"`
	LegalCustomerName    string                  `json:"legal_customer_name"`
	BillingCurrencyCode  string                  `json:"billing_currency_code"`
	ContactEmail         string                  `json:"contact_email,omitempty"`
	ContractReference    *string                 `json:"contract_reference,omitempty"`
	ProcessorCustomerRef *string                 `json:"processor_customer_ref,omitempty"`
	Status               CommercialAccountStatus `json:"status"`
	CreatedAt            time.Time               `json:"created_at"`
	UpdatedAt            time.Time               `json:"updated_at"`
	CreatedByPrincipalID string                  `json:"created_by_principal_id"`
}

type CreateCommercialAccountRequest struct {
	OrganizationID       string `json:"organization_id"`
	LegalCustomerName    string `json:"legal_customer_name"`
	BillingCurrencyCode  string `json:"billing_currency_code"`
	ContactEmail         string `json:"contact_email,omitempty"`
	ContractReference    string `json:"contract_reference,omitempty"`
	ProcessorCustomerRef string `json:"processor_customer_ref,omitempty"`
	CorrelationID        string `json:"correlation_id"`
}

// MembershipStatus is the operational state of a membership. Per doc7 §A6:
// removing a member ends access but never deletes the historical
// attribution row — a membership is deactivated, never hard-deleted.
type MembershipStatus string

const (
	MembershipStatusActive      MembershipStatus = "ACTIVE"
	MembershipStatusDeactivated MembershipStatus = "DEACTIVATED"
)

// Membership answers "does this principal belong to this organization at
// all" (doc7 §A3) — a commercial question, distinct from authorization-svc's
// RBAC (what may they DO once they're in). WorkspaceID/LegalEntityID are
// optional narrower scopes; when both are nil the membership is
// organization-wide.
type Membership struct {
	MembershipID         string           `json:"membership_id"`
	PrincipalID          string           `json:"principal_id"`
	OrganizationID       string           `json:"organization_id"`
	WorkspaceID          *string          `json:"workspace_id,omitempty"`
	LegalEntityID        *string          `json:"legal_entity_id,omitempty"`
	Status               MembershipStatus `json:"status"`
	EffectiveFrom        time.Time        `json:"effective_from"`
	EffectiveTo          *time.Time       `json:"effective_to,omitempty"`
	CreatedAt            time.Time        `json:"created_at"`
	CreatedByPrincipalID string           `json:"created_by_principal_id"`
}

type CreateMembershipRequest struct {
	PrincipalID    string `json:"principal_id"`
	OrganizationID string `json:"organization_id"`
	WorkspaceID    string `json:"workspace_id,omitempty"`
	LegalEntityID  string `json:"legal_entity_id,omitempty"`
	CorrelationID  string `json:"correlation_id"`
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrCommercialAccountNotFound = errorString("commercial account not found")
	ErrMembershipNotFound        = errorString("membership not found")
	ErrStoreUnavailable          = errorString("commercial account store unavailable")
	// ErrConflict is returned when an organization already has a commercial
	// account — doc7 §A4: the commercial account is the verified customer
	// record, and an organization does not have two competing billing
	// identities.
	ErrConflict = errorString("conflict: organization already has a commercial account")
)
