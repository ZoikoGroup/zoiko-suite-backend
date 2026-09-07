package domain

import "slices"

// The controlled vocabularies for an employment contract.
//
// These were a comment on EmploymentContract and nothing else — no validation
// in the handler, no constraint in the compose schema — so contract_type and
// pay_frequency were whatever the caller sent. supabase/migrations/0030 makes
// them CHECK constraints, which is the backstop that binds every writer
// including one that is not this service.
//
// This copy exists so a bad request is answered as a 400 by the handler rather
// than as a 500 by the constraint. The two must agree: adding a value here
// without adding it to 0030 turns an accepted request into a failed INSERT.
var (
	ContractTypes    = []string{"FULL_TIME", "PART_TIME", "FIXED_TERM", "EXECUTIVE"}
	ContractStatuses = []string{"DRAFT", "ACTIVE", "SUPERSEDED", "TERMINATED", "EXPIRED"}
	PayFrequencies   = []string{"MONTHLY", "BIWEEKLY", "WEEKLY"}
)

func IsValidContractType(v string) bool   { return slices.Contains(ContractTypes, v) }
func IsValidContractStatus(v string) bool { return slices.Contains(ContractStatuses, v) }
func IsValidPayFrequency(v string) bool   { return slices.Contains(PayFrequencies, v) }

// IsValidCurrency reports whether v is a three-letter uppercase ISO 4217 code,
// which is what every consumer assumes when it renders an amount. It does not
// check the code is assigned — that is jurisdiction-rules-svc's job, not a
// format check's.
func IsValidCurrency(v string) bool {
	if len(v) != 3 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 'A' || v[i] > 'Z' {
			return false
		}
	}
	return true
}
