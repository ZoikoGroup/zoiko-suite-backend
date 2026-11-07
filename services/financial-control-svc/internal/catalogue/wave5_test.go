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
	require.Len(t, defs, 7)
	want := map[string]string{"FIN-CTRL-024": domain.KindExceptionScan, "FIN-CTRL-019": domain.KindExceptionScan, "FIN-CTRL-020": domain.KindArithmetic, "FIN-CTRL-021": domain.KindExceptionScan,
		"FIN-CTRL-022": domain.KindExceptionScan, "FIN-CTRL-023": domain.KindExceptionScan, "FIN-CTRL-031": domain.KindExceptionScan}
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
		case "FIN-CTRL-031":
			assert.Equal(t, "${scope.aged_before}", s.Params["aged_before"])
		case "FIN-CTRL-024":
			assert.Empty(t, s.Params, "a lifetime population needs no scope key")
		default:
			assert.Equal(t, "${scope.fiscal_period}", s.Params["fiscal_period"], d.ControlCode)
		}
	}
}

// Which controls gate a close is a controlled decision (§34): 024 ships KEY but not close-gating, so seeding it does not
// widen the mandatory set of an entity's close gate (020, 021 and 023).
func TestWave5_ClosedPeriodPostingBlockDoesNotWidenTheCloseGate(t *testing.T) {
	defs, err := Wave5("2026-09-01")
	require.NoError(t, err)
	gating := map[string]bool{}
	for _, d := range defs {
		gating[d.ControlCode] = d.CloseGating
	}
	assert.False(t, gating["FIN-CTRL-024"])
	assert.True(t, gating["FIN-CTRL-020"] && gating["FIN-CTRL-021"] && gating["FIN-CTRL-023"])
}

func TestWave5_RequiresEffectiveFrom(t *testing.T) {
	_, err := Wave5("")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
