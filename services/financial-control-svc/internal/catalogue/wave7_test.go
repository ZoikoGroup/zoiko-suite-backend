package catalogue

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/source"
)

func wave7(t *testing.T) map[string]domain.CreateControlDefinitionRequest {
	t.Helper()
	defs, err := Wave7("2026-10-01")
	require.NoError(t, err)
	m := map[string]domain.CreateControlDefinitionRequest{}
	for _, d := range defs {
		m[d.ControlCode] = d
	}
	return m
}

func TestWave7_DefinitionsAreValidAndExecutable(t *testing.T) {
	m := wave7(t)
	require.Len(t, m, 3)
	for _, code := range []string{"FIN-CTRL-005", "PAY-CTRL-004", "FIN-CTRL-006"} {
		d, ok := m[code]
		require.True(t, ok, code)
		require.NoError(t, d.Validate(), code)
		l, err := domain.ParseRuleLogic(d.InitialLogic)
		require.NoError(t, err, code)
		assert.True(t, l.TwoSided(), "%s compares two populations", code)
		_, err = source.ParseSpec(d.SourceSpec)
		require.NoError(t, err, code)
		_, err = source.ParseSpec(d.TargetSpec)
		require.NoError(t, err, code)
		assert.True(t, d.CloseGating, "%s gates the close", code)
		assert.NotEmpty(t, d.CertifierRole, code)
		assert.NotEqual(t, d.OwnerRole, d.CertifierRole, "%s is certified by someone other than its owner", code)
	}
	_, err := Wave7("")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestWave7_ComparesOneRecordPerAcceptedResultExactly(t *testing.T) {
	m := wave7(t)
	spec := func(raw []byte) source.Spec {
		s, err := source.ParseSpec(raw)
		require.NoError(t, err)
		return s
	}
	for code, want := range map[string][4]string{
		"FIN-CTRL-005": {"wfp-results", "gross", "wfp-journals", "gross"},
		"PAY-CTRL-004": {"wfp-results", "net", "wfp-journals", "net"},
		"FIN-CTRL-006": {"wfp-results", "net", "wfp-net-instructions", "net"},
	} {
		d := m[code]
		a, b := spec(d.SourceSpec), spec(d.TargetSpec)
		assert.Equal(t, SysPayroll, a.System, code)
		assert.Equal(t, SysPayroll, b.System, code)
		assert.Equal(t, want, [4]string{a.Population, a.Params["measure"], b.Population, b.Params["measure"]}, code)
		l, err := domain.ParseRuleLogic(d.InitialLogic)
		require.NoError(t, err)
		assert.False(t, l.AllowGroups, "%s: one record per result on each side, nothing is netted across results", code)
		assert.Empty(t, l.Exclusions, "%s: the populations are already restricted to accepted, posted subjects", code)
	}
}

func TestWave7_UnblocksThePayrollControlsAndSaysWhatItDoesNotCover(t *testing.T) {
	by := map[string]ControlStatus{}
	for _, c := range Status() {
		by[c.Code] = c
	}
	for _, code := range []string{"FIN-CTRL-005", "FIN-CTRL-006"} {
		assert.Equal(t, StatePartial, by[code].State, code)
		assert.Empty(t, by[code].BlockedBy, code)
		assert.Contains(t, by[code].Note, "seed wave 7", code)
	}
	assert.Contains(t, by["FIN-CTRL-006"].Note, "not that Banking settled", "instructed is not paid")
	assert.Equal(t, StateImplemented, by["PAY-CTRL-004"].State)
}
