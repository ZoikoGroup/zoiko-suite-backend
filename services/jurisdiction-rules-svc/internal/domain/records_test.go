package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rd(y, m, dd int) time.Time { return time.Date(y, time.Month(m), dd, 0, 0, 0, 0, time.UTC) }

func TestDuration_AddToClampsMonthsAndAddsDays(t *testing.T) {
	assert.Equal(t, rd(2027, 2, 28), Duration{Months: 6}.AddTo(rd(2026, 8, 31)))
	assert.Equal(t, rd(2028, 2, 29), Duration{Years: 1, Months: 6}.AddTo(rd(2026, 8, 31)), "leap-year February")
	assert.Equal(t, rd(2026, 4, 9), Duration{Days: 90}.AddTo(rd(2026, 1, 9)))
	assert.Equal(t, rd(2033, 3, 10), Duration{Years: 7}.AddTo(rd(2026, 3, 10)))
	assert.Equal(t, "P1Y6M", Duration{Years: 1, Months: 6}.ISO())
	assert.False(t, Duration{}.Valid())
	assert.False(t, Duration{Months: 12}.Valid())
}

func retentionParams(t *testing.T, raw string) RuleParameters {
	t.Helper()
	p, err := ParseRuleParameters([]byte(raw))
	require.NoError(t, err)
	return p
}

func TestRetentionParameters_RejectUnsafeDefinitions(t *testing.T) {
	ok := `{"family":"RETENTION","record_class":"PAYROLL_RECORDS","trigger":"EMPLOYMENT_END","minimum":{"years":6,"months":0,"days":0},"legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY"}`
	retentionParams(t, ok)
	for _, bad := range []string{
		`{"family":"RETENTION","record_class":"payroll","trigger":"CREATION","minimum":{"years":1},"legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY"}`,
		`{"family":"RETENTION","record_class":"X_Y","trigger":"NEVER","minimum":{"years":1},"legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY"}`,
		`{"family":"RETENTION","record_class":"X_Y","trigger":"CREATION","minimum":{"years":1},"destruction_rule":"ELIGIBILITY_ONLY"}`,
		`{"family":"RETENTION","record_class":"X_Y","trigger":"CREATION","minimum":{"years":1},"legal_hold_override":true,"destruction_rule":"AUTO_DELETE"}`,
		`{"family":"RETENTION","record_class":"X_Y","trigger":"CREATION","legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY"}`,
		`{"family":"RETENTION","record_class":"X_Y","trigger":"CREATION","minimum":{"years":1},"legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY","rate":"0.1"}`,
	} {
		_, err := ParseRuleParameters([]byte(bad))
		assert.ErrorIs(t, err, ErrParametersInvalid, bad)
	}
}

func TestDetermineRetention_TenantMayExtendNeverShorten(t *testing.T) {
	p := retentionParams(t, `{"family":"RETENTION","record_class":"PAYROLL_RECORDS","trigger":"EMPLOYMENT_END","minimum":{"years":6,"months":0,"days":0},"legal_hold_override":true,"destruction_rule":"ELIGIBILITY_ONLY"}`)
	at := rd(2026, 3, 10)
	r := DetermineRetention(p, at, "", nil)
	assert.Equal(t, OutcomeRetentionResolved, r.Outcome)
	assert.Equal(t, "2032-03-10", r.RetainUntil)
	assert.Equal(t, RetentionTenantTooShort, DetermineRetention(p, at, "", &Duration{Years: 5, Months: 11}).Outcome)
	assert.Equal(t, OutcomeRetentionResolved, DetermineRetention(p, at, "", &Duration{Years: 6}).Outcome, "equal to the statutory minimum is allowed")
	ext := DetermineRetention(p, at, "", &Duration{Years: 8})
	assert.Equal(t, "2034-03-10", ext.EffectiveRetention)
	assert.Equal(t, RetentionTriggerMismatch, DetermineRetention(p, at, "CREATION", nil).Outcome)
}

func TestMapping_UniqueEntriesAndNoGuessing(t *testing.T) {
	p, err := ParseRuleParameters([]byte(`{"family":"MAPPING","mapping_type":"TAX_CODE_TO_ACCOUNT_CLASS","entries":[{"from":"A","to":"X"},{"from":"B","to":"Y"}]}`))
	require.NoError(t, err)
	assert.Equal(t, "X", LookupMapping(p, "A").To)
	assert.Equal(t, MappingEntryNotFound, LookupMapping(p, "C").Outcome)
	assert.Len(t, LookupMapping(p, "").Entries, 2)
	assert.Equal(t, "2", PinnedParameterValue(p))
	_, err = ParseRuleParameters([]byte(`{"family":"MAPPING","mapping_type":"TAX_CODE_TO_ACCOUNT_CLASS","entries":[{"from":"A","to":"X"},{"from":"A","to":"Y"}]}`))
	assert.ErrorIs(t, err, ErrParametersInvalid)
}
