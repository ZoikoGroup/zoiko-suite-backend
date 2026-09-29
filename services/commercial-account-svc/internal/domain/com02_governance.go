// COM-02 part 2c: discount applications under delegated authority and price
// migration offers (ZS-SVC-Q-001 §4.1, §4.2; COM-CTRL-003, -005; negative
// paths #06, #38).
package domain

import "time"

const (
	PrefixDiscountApplication = "cdsc_"
	PrefixMigrationOffer      = "cmig_"
)

// ChangeKindMigration moves a subscription from its bound price version to
// a later version of the same product under a published migration offer.
const (
	ChangeKindMigration ChangeKind = "PRICE_MIGRATION"
	ChangePriceMigrated ChangeType = "PRICE_MIGRATED"
)

type DiscountStatus string

const (
	DiscountProposed  DiscountStatus = "PROPOSED"
	DiscountApproved  DiscountStatus = "APPROVED"
	DiscountRejected  DiscountStatus = "REJECTED"
	DiscountWithdrawn DiscountStatus = "WITHDRAWN"
)

// Approval bases: a second person, or the catalogue's own policy for a
// component it does not require approval for.
const (
	ApprovalByApprover    = "APPROVER"
	ApprovalCatalogPolicy = "CATALOG_POLICY"
)

// DiscountApplication applies one of the catalogue's DISCOUNT components to
// one subscription. The component's type, value, cap, duration and
// eligibility live on the immutable price version; this records who asked,
// on what basis, and who approved.
type DiscountApplication struct {
	DiscountApplicationID  string         `json:"discount_application_id"`
	SubscriptionID         string         `json:"subscription_id"`
	PriceVersionID         string         `json:"price_version_id"`
	ComponentKey           string         `json:"component_key"`
	Status                 DiscountStatus `json:"status"`
	Reason                 string         `json:"reason"`
	CustomerBasisRef       string         `json:"customer_basis_ref"`
	RequestedByPrincipalID string         `json:"requested_by_principal_id"`
	RequestedAt            time.Time      `json:"requested_at"`
	ApprovalBasis          *string        `json:"approval_basis,omitempty"`
	ApprovedByPrincipalID  *string        `json:"approved_by_principal_id,omitempty"`
	ApprovedAt             *time.Time     `json:"approved_at,omitempty"`
	DecidedReason          *string        `json:"decided_reason,omitempty"`
	DecidedByPrincipalID   *string        `json:"decided_by_principal_id,omitempty"`
	DecidedAt              *time.Time     `json:"decided_at,omitempty"`
	RowVersion             int            `json:"row_version"`
}

// ProposeDiscountParams is a validated ProposeDiscount command.
type ProposeDiscountParams struct {
	DiscountApplicationID string
	SubscriptionID        string
	ProductCode           string
	ComponentKey          string
	Reason                string
	CustomerBasisRef      string
	Actor                 string
	Now                   time.Time
}

type MigrationOfferStatus string

const (
	MigrationOfferDraft     MigrationOfferStatus = "DRAFT"
	MigrationOfferPublished MigrationOfferStatus = "PUBLISHED"
	MigrationOfferWithdrawn MigrationOfferStatus = "WITHDRAWN"
)

const (
	EligibilityAllOnFromVersion    = "ALL_ON_FROM_VERSION"
	EligibilityListedSubscriptions = "LISTED_SUBSCRIPTIONS"
)

// MigrationOffer offers subscribers on one price version a move to a later
// version of the same product, taken up at their next renewal.
type MigrationOffer struct {
	MigrationOfferID       string               `json:"migration_offer_id"`
	ProductID              string               `json:"product_id"`
	FromPriceVersionID     string               `json:"from_price_version_id"`
	ToPriceVersionID       string               `json:"to_price_version_id"`
	EligibilityMode        *string              `json:"eligibility_mode"`
	TargetSubscriptionIDs  []string             `json:"target_subscription_ids,omitempty"`
	AcceptBy               *time.Time           `json:"accept_by,omitempty"`
	Reason                 string               `json:"reason"`
	Status                 MigrationOfferStatus `json:"status"`
	RowVersion             int                  `json:"row_version"`
	CreatedAt              time.Time            `json:"created_at"`
	CreatedByPrincipalID   string               `json:"created_by_principal_id"`
	PublishedAt            *time.Time           `json:"published_at,omitempty"`
	PublishedByPrincipalID *string              `json:"published_by_principal_id,omitempty"`
	WithdrawnAt            *time.Time           `json:"withdrawn_at,omitempty"`
	WithdrawReason         *string              `json:"withdraw_reason,omitempty"`
}

var (
	ErrDiscountNotApplicable       = errorString("the subscription's bound price versions have no such discount component")
	ErrDiscountExists              = errorString("this discount is already proposed or approved for the subscription")
	ErrDiscountNotFound            = errorString("discount application not found")
	ErrDiscountInvalidState        = errorString("discount application is not in a state that allows this action")
	ErrDiscountNotRequester        = errorString("only the principal who proposed a discount can withdraw it")
	ErrMigrationOfferNotFound      = errorString("migration offer not found")
	ErrMigrationOfferInvalidState  = errorString("migration offer is not in a state that allows this action")
	ErrMigrationEligibilityMissing = errorString("a migration offer cannot be published without an eligibility rule naming who it is for")
	ErrNotEligibleForMigration     = errorString("the subscription is not eligible for this migration offer")
	ErrMigrationTargetInvalid      = errorString("a migration moves from a price version to a later published version of the same product")
)

// ValidateMigrationOffer checks an offer's own fields at creation. Whether
// it names an eligibility rule is checked at publication (#38): a draft may
// still be missing one.
func ValidateMigrationOffer(o *MigrationOffer) error {
	if o.EligibilityMode != nil && *o.EligibilityMode != EligibilityAllOnFromVersion && *o.EligibilityMode != EligibilityListedSubscriptions {
		return invalid("eligibility_mode", "must be ALL_ON_FROM_VERSION or LISTED_SUBSCRIPTIONS")
	}
	if (o.EligibilityMode == nil || *o.EligibilityMode != EligibilityListedSubscriptions) && len(o.TargetSubscriptionIDs) > 0 {
		return invalid("target_subscription_ids", "targets are only given with eligibility_mode LISTED_SUBSCRIPTIONS")
	}
	if len(o.Reason) == 0 {
		return invalid("reason", "is required")
	}
	return nil
}
