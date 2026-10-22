package recon

import "time"

// Policy is the reconciliation default of one connector family (OD-12): how often a connection is reconciled on its
// own and how large a difference has to be before it is a variance, and how severe. An operator may override a family
// by configuration; the defaults below are deliberately strict for money and filings.
type Policy struct {
	Domain            string        `json:"domain"`
	FrequencyHours    int           `json:"frequency_hours"`     // a scheduled run covers this much time
	CountTolerance    int           `json:"count_tolerance"`     // an absolute count difference up to this is a match
	MediumCountDelta  int           `json:"medium_count_delta"`  // a count difference above this is at least MEDIUM
	HighCountDelta    int           `json:"high_count_delta"`    // a count difference at or above this is HIGH
	AmountToleranceBP int           `json:"amount_tolerance_bp"` // basis points of the provider's total that still match
	HighAmountBP      int           `json:"high_amount_bp"`      // a difference at or above this many basis points is HIGH
	UnknownMaxAge     time.Duration `json:"unknown_max_age"`     // OD-13: an UNKNOWN attempt older than this is escalated
	PendingMaxAge     time.Duration `json:"pending_max_age"`
	BatchLag          time.Duration `json:"batch_lag"` // a batch not reconciled after this long is a variance
	Grace             time.Duration `json:"grace"`     // a scheduled run ends this long before now, so late callbacks can land
}

func strict(domain string, freq int, unknown time.Duration) Policy {
	return Policy{Domain: domain, FrequencyHours: freq, CountTolerance: 0, MediumCountDelta: 2, HighCountDelta: 10, AmountToleranceBP: 0, HighAmountBP: 100,
		UnknownMaxAge: unknown, PendingMaxAge: 2 * unknown, BatchLag: 24 * time.Hour, Grace: 30 * time.Minute}
}

// DefaultPolicies returns the platform defaults by connector family. Money rails reconcile most often and give an
// ambiguous attempt the least time; filings and master-data exchanges reconcile daily.
func DefaultPolicies() map[string]Policy {
	p := map[string]Policy{
		"PAYMENT_RAIL":     strict("PAYMENT_RAIL", 6, 4*time.Hour),
		"BANK_FEED":        strict("BANK_FEED", 24, 24*time.Hour),
		"TAX_AUTHORITY":    strict("TAX_AUTHORITY", 24, 24*time.Hour),
		"EINVOICE_NETWORK": strict("EINVOICE_NETWORK", 24, 12*time.Hour),
		"PAYROLL":          strict("PAYROLL", 24, 12*time.Hour),
		"ESIGNATURE":       strict("ESIGNATURE", 24, 48*time.Hour),
		"REGULATOR":        strict("REGULATOR", 24, 24*time.Hour),
		"FILE_TRANSFER":    strict("FILE_TRANSFER", 24, 24*time.Hour),
	}
	for _, d := range []string{"HRIS", "BENEFITS", "ERP", "CRM", "API"} {
		q := strict(d, 24, 48*time.Hour)
		q.CountTolerance = 1
		q.AmountToleranceBP = 5
		p[d] = q
	}
	return p
}

// PolicyFor returns the policy of a family, falling back to the strictest generic one for a family it does not know.
func (s *Service) PolicyFor(family string) Policy {
	if p, ok := s.Policies[family]; ok {
		return p
	}
	if p, ok := DefaultPolicies()[family]; ok {
		return p
	}
	return strict(family, 24, 24*time.Hour)
}
