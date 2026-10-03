package catalogue

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/source"
)

func TestWave6_ThreeSingleSidedNoPeriodControls(t *testing.T) {
	defs, err := Wave6("2026-09-01")
	require.NoError(t, err)
	require.Len(t, defs, 3)
	want := map[string][2]string{
		"FIN-CTRL-037": {SysTaxAuthority, domain.KindExceptionScan},
		"FIN-CTRL-038": {SysFinancialClose, domain.KindArithmetic},
		"FIN-CTRL-045": {SysPayeeBanking, domain.KindExceptionScan},
	}
	for _, d := range defs {
		l, err := domain.ParseRuleLogic(d.InitialLogic)
		require.NoError(t, err, d.ControlCode)
		w := want[d.ControlCode]
		assert.Equal(t, w[1], l.Kind, d.ControlCode)
		assert.False(t, l.TwoSided(), d.ControlCode)
		s, err := source.ParseSpec(d.SourceSpec)
		require.NoError(t, err, d.ControlCode)
		assert.Equal(t, w[0], s.System, d.ControlCode)
		assert.True(t, s.NoPeriod, d.ControlCode)
	}
}

func TestWave6_ScopeBinding(t *testing.T) {
	defs, _ := Wave6("2026-09-01")
	for _, d := range defs {
		s, _ := source.ParseSpec(d.SourceSpec)
		switch d.ControlCode {
		case "FIN-CTRL-037":
			assert.Equal(t, "${scope.submitted_before}", s.Params["submitted_before"])
		case "FIN-CTRL-038":
			assert.Equal(t, "${scope.batch_id}", s.Params["batch_id"])
		case "FIN-CTRL-045":
			assert.Equal(t, "${scope.changed_from}", s.Params["changed_from"])
			assert.Equal(t, "${scope.changed_to}", s.Params["changed_to"])
		}
	}
}

func TestWave6_MigrationTieOutCoversTotalsRowsAndBalance(t *testing.T) {
	defs, _ := Wave6("2026-09-01")
	for _, d := range defs {
		if d.ControlCode != "FIN-CTRL-038" {
			continue
		}
		l, err := domain.ParseRuleLogic(d.InitialLogic)
		require.NoError(t, err)
		names := map[string]bool{}
		for _, e := range l.Equations {
			names[e.Name] = true
		}
		for _, n := range []string{"debits_tie_to_declared", "credits_tie_to_declared", "rows_tie_to_declared", "opening_balance_balanced"} {
			assert.True(t, names[n], n)
		}
	}
}
