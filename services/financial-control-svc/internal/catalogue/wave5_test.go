package catalogue

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/source"
)

func TestWave5_SingleSidedLedgerControls(t *testing.T) {
	defs, err := Wave5("2026-09-01")
	require.NoError(t, err)
	require.Len(t, defs, 5)
	want := map[string]string{"FIN-CTRL-019": domain.KindExceptionScan, "FIN-CTRL-020": domain.KindArithmetic, "FIN-CTRL-021": domain.KindExceptionScan,
		"FIN-CTRL-022": domain.KindExceptionScan, "FIN-CTRL-023": domain.KindExceptionScan}
	for _, d := range defs {
		l, err := domain.ParseRuleLogic(d.InitialLogic)
		require.NoError(t, err, d.ControlCode)
		assert.Equal(t, want[d.ControlCode], l.Kind, d.ControlCode)
		assert.False(t, l.TwoSided(), d.ControlCode)
		assert.Empty(t, d.TargetSpec, d.ControlCode)
		s, err := source.ParseSpec(d.SourceSpec)
		require.NoError(t, err, d.ControlCode)
		assert.Equal(t, SysGL, s.System)
		assert.True(t, s.NoPeriod, "%s: the GL kernel populations reject period_id", d.ControlCode)
	}
}

func TestWave5_ScopeBoundParams(t *testing.T) {
	defs, _ := Wave5("2026-09-01")
	for _, d := range defs {
		s, _ := source.ParseSpec(d.SourceSpec)
		switch d.ControlCode {
		case "FIN-CTRL-019", "FIN-CTRL-022":
			assert.Equal(t, "${scope.created_before}", s.Params["created_before"])
		default:
			assert.Equal(t, "${scope.fiscal_period}", s.Params["fiscal_period"], d.ControlCode)
		}
	}
}

func TestWave5_RequiresEffectiveFrom(t *testing.T) {
	_, err := Wave5("")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
