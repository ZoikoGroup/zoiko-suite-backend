package catalogue

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/source"
)

var w4opts = Wave4Options{ICReceivableAccounts: "1300,1310", ICPayableAccounts: "2300"}

func wave4(t *testing.T) map[string]domain.CreateControlDefinitionRequest {
	t.Helper()
	defs, err := Wave4("2026-09-01", w4opts)
	require.NoError(t, err)
	m := map[string]domain.CreateControlDefinitionRequest{}
	for _, d := range defs {
		m[d.ControlCode] = d
	}
	return m
}

func specs(t *testing.T, d domain.CreateControlDefinitionRequest) (source.Spec, source.Spec) {
	t.Helper()
	a, err := source.ParseSpec(d.SourceSpec)
	require.NoError(t, err)
	b, err := source.ParseSpec(d.TargetSpec)
	require.NoError(t, err)
	return a, b
}

func TestWave4_ThreeExecutableTwoSidedControls(t *testing.T) {
	m := wave4(t)
	require.Len(t, m, 3)
	for _, code := range []string{"FIN-CTRL-014", "FIN-CTRL-015", "FIN-CTRL-017"} {
		d, ok := m[code]
		require.True(t, ok, code)
		require.NoError(t, d.Validate(), code)
		l, err := domain.ParseRuleLogic(d.InitialLogic)
		require.NoError(t, err, code)
		assert.True(t, l.TwoSided(), code)
		assert.False(t, l.AllowGroups, "%s: one intercompany entry pairs with exactly one journal per leg; a second journal is a defect, not an allocation", code)
		assert.NotEmpty(t, d.OwnerRole, code)
	}
}

func TestWave4_LegsUseReceivableDebitAndPayableCredit(t *testing.T) {
	m := wave4(t)
	srcA, glA := specs(t, m["FIN-CTRL-014"])
	assert.Equal(t, "intercompany-accounting", srcA.System)
	assert.Equal(t, "entry-legs", srcA.Population)
	assert.Equal(t, "source", srcA.Params["leg"])
	assert.Equal(t, "journal-account-totals", glA.Population)
	assert.Equal(t, "1300,1310", glA.Params["account_codes"])
	assert.Equal(t, "DEBIT", glA.Params["normal_balance"], "the source entity books the receivable (debit)")

	srcB, glB := specs(t, m["FIN-CTRL-015"])
	assert.Equal(t, "target", srcB.Params["leg"])
	assert.Equal(t, "2300", glB.Params["account_codes"])
	assert.Equal(t, "CREDIT", glB.Params["normal_balance"], "the target entity books the payable (credit)")
}

func TestWave4_TrialBalanceToConsolidationBindsBothIdentifiersFromTheRun(t *testing.T) {
	d := wave4(t)["FIN-CTRL-017"]
	tb, con := specs(t, d)
	assert.Equal(t, "general-ledger", tb.System)
	assert.Equal(t, "trial-balance", tb.Population)
	assert.Equal(t, "${scope.fiscal_period}", tb.Params["fiscal_period"])
	assert.Equal(t, "consolidation", con.System)
	assert.Equal(t, "balance-contributions", con.Population)
	assert.Equal(t, "${scope.consolidation_run_id}", con.Params["run_id"])

	bound, err := source.ResolveScopeParams(tb, []byte(`{"fiscal_period":"2026-09","consolidation_run_id":"r-1"}`))
	require.NoError(t, err)
	assert.Equal(t, "2026-09", bound.Params["fiscal_period"])
	_, err = source.ResolveScopeParams(con, []byte(`{"fiscal_period":"2026-09"}`))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "a run that does not name its consolidation run cannot execute")
	assert.Equal(t, "KEY", d.RiskTier)
	assert.True(t, d.CloseGating)
	assert.NotEmpty(t, d.CertifierRole)
}

func TestWave4_RequiresEntitySpecificInputs(t *testing.T) {
	for name, mutate := range map[string]func(*Wave4Options){
		"receivable": func(o *Wave4Options) { o.ICReceivableAccounts = "" },
		"payable":    func(o *Wave4Options) { o.ICPayableAccounts = " " },
	} {
		o := w4opts
		mutate(&o)
		_, err := Wave4("2026-09-01", o)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, name)
	}
	_, err := Wave4("", w4opts)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestStatus_Wave4BlockedControlsAndPartialNotes(t *testing.T) {
	by := map[string]ControlStatus{}
	for _, c := range Status() {
		by[c.Code] = c
	}
	for _, code := range []string{"FIN-CTRL-016", "FIN-CTRL-018", "FIN-CTRL-040", "FIN-CTRL-043"} {
		assert.Equal(t, StateBlocked, by[code].State, code)
		assert.NotEmpty(t, by[code].BlockedBy, code)
	}
	assert.Contains(t, by["FIN-CTRL-016"].BlockedBy[0], "link from an elimination")
	assert.Contains(t, by["FIN-CTRL-018"].BlockedBy[0], "statement")
	for _, code := range []string{"FIN-CTRL-013", "FIN-CTRL-014", "FIN-CTRL-015", "FIN-CTRL-017"} {
		assert.Equal(t, StatePartial, by[code].State, code)
		assert.NotEmpty(t, by[code].Note, "%s: a PARTIAL control must say exactly what it does not cover", code)
	}
	assert.Contains(t, by["FIN-CTRL-017"].Note, "uncertified")
	assert.Contains(t, by["FIN-CTRL-014"].Note, "one legal entity")
}

// Sources that reject period_id must never be sent one — otherwise a PERIOD_END run
// (which must carry a period) would be answered 400 and end FAILED / INDETERMINATE.
func TestNoPeriodIsSetOnEveryPopulationWhoseSourceRejectsPeriodID(t *testing.T) {
	check := func(code string, d domain.CreateControlDefinitionRequest) {
		a, b := specs(t, d)
		assert.True(t, a.NoPeriod, "%s side A %s/%s", code, a.System, a.Population)
		assert.True(t, b.NoPeriod, "%s side B %s/%s", code, b.System, b.Population)
	}
	for code, d := range wave4(t) {
		check(code, d)
	}
	check("FIN-CTRL-013", wave3(t)["FIN-CTRL-013"])

	// Sources that DO take a period stay period-scoped.
	m2 := byCode(t)
	a, b := specs(t, m2["FIN-CTRL-001"])
	assert.False(t, a.NoPeriod)
	assert.False(t, b.NoPeriod)
}
