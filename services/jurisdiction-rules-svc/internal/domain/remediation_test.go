package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func rp(s string) *string { return &s }

func TestDispositionInput_Validate(t *testing.T) {
	good := []DispositionInput{
		{DecisionID: "d1", Disposition: DispositionReconfirmed, EvidenceRef: "REV-1"},
		{DecisionID: "d1", Disposition: DispositionNotAffected, EvidenceRef: "REV-1", Note: "used the unaffected band"},
		{DecisionID: "d1", Disposition: DispositionRecalc, EvidenceRef: "REV-1", NewDecisionID: rp("d2")},
		{DecisionID: "d1", Disposition: DispositionSuperseded, EvidenceRef: "REV-1", NewDecisionID: rp("d2")},
	}
	for _, d := range good {
		assert.NoError(t, d.Validate(), d.Disposition)
	}
	bad := map[string]struct {
		d    DispositionInput
		want string
	}{
		"no decision":              {DispositionInput{Disposition: DispositionReconfirmed, EvidenceRef: "E"}, "decision_id"},
		"unknown disposition":      {DispositionInput{DecisionID: "d1", Disposition: "FORGOTTEN", EvidenceRef: "E"}, "disposition must be"},
		"no evidence":              {DispositionInput{DecisionID: "d1", Disposition: DispositionReconfirmed}, "evidence_ref"},
		"blank evidence":           {DispositionInput{DecisionID: "d1", Disposition: DispositionReconfirmed, EvidenceRef: "   "}, "evidence_ref"},
		"recalculated, no new":     {DispositionInput{DecisionID: "d1", Disposition: DispositionRecalc, EvidenceRef: "E"}, "needs new_decision_id"},
		"superseded, blank new":    {DispositionInput{DecisionID: "d1", Disposition: DispositionSuperseded, EvidenceRef: "E", NewDecisionID: rp(" ")}, "needs new_decision_id"},
		"reconfirmed with new":     {DispositionInput{DecisionID: "d1", Disposition: DispositionReconfirmed, EvidenceRef: "E", NewDecisionID: rp("d2")}, "takes no new_decision_id"},
		"not affected with new":    {DispositionInput{DecisionID: "d1", Disposition: DispositionNotAffected, EvidenceRef: "E", NewDecisionID: rp("d2")}, "takes no new_decision_id"},
		"replaced by itself":       {DispositionInput{DecisionID: "d1", Disposition: DispositionRecalc, EvidenceRef: "E", NewDecisionID: rp("d1")}, "different decision"},
		"note too long":            {DispositionInput{DecisionID: "d1", Disposition: DispositionReconfirmed, EvidenceRef: "E", Note: strings.Repeat("x", 2001)}, "note"},
	}
	for name, c := range bad {
		err := c.d.Validate()
		if assert.Error(t, err, name) {
			assert.Contains(t, err.Error(), c.want, name)
		}
	}
}

func TestDispositionHelpers(t *testing.T) {
	for _, d := range []string{DispositionReconfirmed, DispositionNotAffected, DispositionRecalc, DispositionSuperseded} {
		assert.True(t, ValidDisposition(d), d)
	}
	assert.False(t, ValidDisposition("reconfirmed"), "case matters")
	assert.True(t, DispositionNeedsReplacement(DispositionRecalc))
	assert.True(t, DispositionNeedsReplacement(DispositionSuperseded))
	assert.False(t, DispositionNeedsReplacement(DispositionReconfirmed))
	assert.False(t, DispositionNeedsReplacement(DispositionNotAffected))
}
