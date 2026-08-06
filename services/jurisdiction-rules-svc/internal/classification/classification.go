// Package classification carries the data classification tiers from
// docs/architecture/04-data-model.md §20.
//
// docs/architecture/data_classification_audit.md §2.11 assigns this service
// two tiers: jurisdictions are PUBLIC (names, region codes, country codes),
// jurisdiction_rules are INTERNAL (rule domain settings and legislative
// metadata). Both rows carry the tier explicitly so downstream consumers —
// export, retention, residency — read it off the record rather than
// inferring it from the table name.
package classification

// Classification represents a data classification tier.
type Classification string

const (
	Public       Classification = "PUBLIC"
	Internal     Classification = "INTERNAL"
	Confidential Classification = "CONFIDENTIAL"
	Restricted   Classification = "RESTRICTED"
)

func (c Classification) String() string {
	return string(c)
}

func (c Classification) Valid() bool {
	switch c {
	case Public, Internal, Confidential, Restricted:
		return true
	default:
		return false
	}
}
