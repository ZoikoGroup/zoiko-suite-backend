package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const vatWithObligations = `{"family":"REGISTRATION_TYPE","registration":{"kind":"VAT","authority":"Example Revenue","obligations":["VAT_RETURN","EC_SALES_LIST"],
 "identifier":{"pattern":"^GB[0-9]{9}$","normalize":"NONE","examples_valid":["GB123456789"],"examples_invalid":["GB12345"]}}}`

func TestRegistrationType_ObligationsAreValidated(t *testing.T) {
	p, err := ParseRuleParameters([]byte(vatWithObligations))
	require.NoError(t, err)
	assert.Equal(t, []string{"VAT_RETURN", "EC_SALES_LIST"}, p.Registration.Obligations)

	// A registration type that names no obligations stays valid: the field is optional and earlier rules are unaffected.
	_, err = ParseRuleParameters([]byte(vatType))
	assert.NoError(t, err)

	bad := map[string]string{
		"lower case":   strings.Replace(vatWithObligations, `"VAT_RETURN"`, `"vat_return"`, 1),
		"too short":    strings.Replace(vatWithObligations, `"VAT_RETURN"`, `"V"`, 1),
		"duplicate":    strings.Replace(vatWithObligations, `"EC_SALES_LIST"`, `"VAT_RETURN"`, 1),
		"has a space":  strings.Replace(vatWithObligations, `"VAT_RETURN"`, `"VAT RETURN"`, 1),
		"not a string": strings.Replace(vatWithObligations, `["VAT_RETURN","EC_SALES_LIST"]`, `[1,2]`, 1),
	}
	for name, raw := range bad {
		_, err := ParseRuleParameters([]byte(raw))
		assert.Error(t, err, name)
	}

	many := make([]string, 21)
	for i := range many {
		many[i] = `"OBLIGATION_` + string(rune('A'+i)) + `"`
	}
	_, err = ParseRuleParameters([]byte(strings.Replace(vatWithObligations, `["VAT_RETURN","EC_SALES_LIST"]`, "["+strings.Join(many, ",")+"]", 1)))
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "at most 20")
	}
}

func TestOtherFamiliesStillRejectTheObligationsField(t *testing.T) {
	// obligations belongs to the registration payload only: it is not accepted next to another family.
	_, err := ParseRuleParameters([]byte(`{"family":"TAX_RATE","rate":"0.2","rounding":{"mode":"HALF_UP","scale":2},"obligations":["X1"]}`))
	assert.Error(t, err)
}
