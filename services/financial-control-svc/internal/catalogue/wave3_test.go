package catalogue

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/source"
)

func wave3(t *testing.T) map[string]domain.CreateControlDefinitionRequest {
	t.Helper()
	defs, err := Wave3("2026-09-01")
	require.NoError(t, err)
	m := map[string]domain.CreateControlDefinitionRequest{}
	for _, d := range defs {
		m[d.ControlCode] = d
	}
	return m
}

func TestWave3_DefinitionsValidAndExecutable(t *testing.T) {
	m := wave3(t)
	require.Len(t, m, 4)
	for _, code := range []string{"FIN-CTRL-013", "PAY-CTRL-001", "PAY-CTRL-002", "PAY-CTRL-003"} {
		d, ok := m[code]
		require.True(t, ok, code)
		require.NoError(t, d.Validate(), code)
		l, err := domain.ParseRuleLogic(d.InitialLogic)
		require.NoError(t, err, code)
		_, err = source.ParseSpec(d.SourceSpec)
		require.NoError(t, err, code)
		if l.TwoSided() {
			_, err = source.ParseSpec(d.TargetSpec)
			require.NoError(t, err, code)
		}
	}
	_, err := Wave3("")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestWave3_PhysicalCountBindsCountFromTheRunNotTheDefinition(t *testing.T) {
	d := wave3(t)["FIN-CTRL-013"]
	a, err := source.ParseSpec(d.SourceSpec)
	require.NoError(t, err)
	b, err := source.ParseSpec(d.TargetSpec)
	require.NoError(t, err)
	assert.Equal(t, "${scope.count_id}", a.Params["count_id"])
	assert.Equal(t, "book", a.Params["side"])
	assert.Equal(t, "physical", b.Params["side"])
	assert.Equal(t, "${scope.count_id}", b.Params["count_id"])

	// Bound per run: the same definition serves any count, and a run without one cannot execute.
	bound, err := source.ResolveScopeParams(a, []byte(`{"count_id":"7d6f0e3a-1111-2222-3333-444455556666"}`))
	require.NoError(t, err)
	assert.Equal(t, "7d6f0e3a-1111-2222-3333-444455556666", bound.Params["count_id"])
	_, err = source.ResolveScopeParams(a, []byte(`{}`))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	l, _ := domain.ParseRuleLogic(d.InitialLogic)
	assert.False(t, l.AllowGroups, "count lines pair one-to-one; a shared reference is a defect, not an allocation")
}

func TestWave3_PayrollGrossToNetEquation(t *testing.T) {
	d := wave3(t)["PAY-CTRL-001"]
	l, err := domain.ParseRuleLogic(d.InitialLogic)
	require.NoError(t, err)
	assert.Equal(t, domain.KindArithmetic, l.Kind)
	require.Len(t, l.Equations, 1)
	eq := l.Equations[0]
	assert.Equal(t, []string{"attr:gross_pay"}, eq.Plus)
	assert.Equal(t, []string{"attr:tax_withheld", "attr:benefits_deductions"}, eq.Minus)
	assert.Equal(t, "attr:net_pay", eq.Equals)
	assert.False(t, l.TwoSided())
}

func TestWave3_PayrollExclusionsAreOwnedAndScopedToTheRightAttribute(t *testing.T) {
	m := wave3(t)
	for _, code := range []string{"PAY-CTRL-001", "PAY-CTRL-002", "PAY-CTRL-003"} {
		l, err := domain.ParseRuleLogic(m[code].InitialLogic)
		require.NoError(t, err, code)
		require.NotEmpty(t, l.Exclusions, code)
		shadow, notFinal := 0, 0
		for _, x := range l.Exclusions {
			assert.NotEmpty(t, x.Authority, "%s: %s", code, x.Attr)
			assert.NotEmpty(t, x.Reason, code)
			switch x.Attr {
			case "is_shadow_run":
				shadow++
			case "status", "run_status":
				notFinal++
				assert.Equal(t, []string{"INITIATED", "CALCULATED", "BLOCKED"}, x.Values, "COMPLETED must never be excluded")
			}
		}
		assert.Equal(t, 1, shadow, code)
		assert.GreaterOrEqual(t, notFinal, 1, code)
	}
	// The run-vs-slips controls name the DIFFERENT status attribute each side actually carries.
	l, _ := domain.ParseRuleLogic(m["PAY-CTRL-002"].InitialLogic)
	sides := map[string]string{}
	for _, x := range l.Exclusions {
		if x.Attr != "is_shadow_run" {
			sides[x.Side] = x.Attr
		}
	}
	assert.Equal(t, map[string]string{"A": "status", "B": "run_status"}, sides)
	assert.True(t, l.AllowGroups, "slips group under their run")
}

func TestWave3_PayrollControlsUseMeasuresThatMatchOnBothSides(t *testing.T) {
	m := wave3(t)
	for code, measure := range map[string]string{"PAY-CTRL-002": "net", "PAY-CTRL-003": "gross"} {
		a, _ := source.ParseSpec(m[code].SourceSpec)
		b, _ := source.ParseSpec(m[code].TargetSpec)
		assert.Equal(t, measure, a.Params["measure"], code)
		assert.Equal(t, measure, b.Params["measure"], code)
		assert.Equal(t, "payroll-runs", a.Population)
		assert.Equal(t, "pay-slips", b.Population)
	}
}

// ── status registry ──────────────────────────────────────────────────────────

var codeRe = regexp.MustCompile(`^(FIN|PAY)-CTRL-\d{3}$`)

func TestStatus_CoversTheWholeCatalogueExactlyOnce(t *testing.T) {
	items := Status()
	seen := map[string]bool{}
	for _, c := range items {
		assert.Regexp(t, codeRe, c.Code)
		assert.False(t, seen[c.Code], "duplicate %s", c.Code)
		seen[c.Code] = true
		assert.NotEmpty(t, c.Name, c.Code)
		assert.Contains(t, []string{StateImplemented, StatePartial, StateBlocked, StateNotStarted}, c.State, c.Code)
	}
	for i := 1; i <= 45; i++ {
		code := "FIN-CTRL-" + pad3(i)
		assert.True(t, seen[code], "%s is missing from the status report", code)
	}
	assert.Len(t, items, 48, "45 catalogue controls + 3 payroll extensions")
}

func pad3(i int) string {
	s := "00" + itoa(i)
	return s[len(s)-3:]
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

func TestStatus_BlockedControlsAlwaysNameTheirBlocker(t *testing.T) {
	blocked := 0
	for _, c := range Status() {
		if c.State == StateBlocked {
			blocked++
			assert.NotEmpty(t, c.BlockedBy, "%s is blocked but says nothing about why", c.Code)
		} else {
			assert.Empty(t, c.BlockedBy, "%s is %s but lists blockers", c.Code, c.State)
		}
		if c.State == StateNotStarted {
			assert.GreaterOrEqual(t, c.Wave, 4, "%s: a not-started control belongs to a later wave", c.Code)
		}
	}
	assert.Equal(t, 25, blocked)
}

// The status report must agree with what the catalogue can actually create.
func TestStatus_ImplementedControlsAreExactlyTheOnesTheCatalogueBuilds(t *testing.T) {
	w2, err := Wave2(opts)
	require.NoError(t, err)
	w3, err := Wave3("2026-09-01")
	require.NoError(t, err)
	w4, err := Wave4("2026-09-01", Wave4Options{ICReceivableAccounts: "1300", ICPayableAccounts: "2300"})
	require.NoError(t, err)
	w5, err := Wave5("2026-09-01")
	require.NoError(t, err)
	w6, err := Wave6("2026-09-01")
	require.NoError(t, err)
	built := map[string]bool{}
	for _, d := range append(append(append(append(w2, w3...), w4...), w5...), w6...) {
		built[d.ControlCode] = true
	}
	impl := map[string]bool{}
	for _, c := range Status() {
		if c.Code == "FIN-CTRL-041" || c.Code == "FIN-CTRL-042" {
			continue // computed by the service itself, not created from a catalogue definition
		}
		if c.State == StateImplemented || c.State == StatePartial {
			impl[c.Code] = true
		}
	}
	assert.Equal(t, built, impl, "a control is IMPLEMENTED or PARTIAL iff a catalogue wave creates it")
}

func TestStatus_KeyBlockersAreSpecific(t *testing.T) {
	by := map[string]ControlStatus{}
	for _, c := range Status() {
		by[c.Code] = c
	}
	assert.Contains(t, by["FIN-CTRL-007"].BlockedBy[0], "tax ledger")
	assert.Contains(t, by["FIN-CTRL-005"].BlockedBy[0], "payroll")
	assert.Contains(t, by["FIN-CTRL-011"].BlockedBy[0], "PostAccountingEvent")
	assert.Contains(t, by["FIN-CTRL-013"].Note, "valuation")
	sum := Summary(Status())
	assert.Equal(t, 25, sum[StateBlocked])
	assert.Equal(t, 12, sum[StateImplemented], "7 catalogue controls (001,002,003,020,027,028,032) + 3 payroll extensions + 041, 042 computed in-service")
	assert.Equal(t, 11, sum[StatePartial], "013, 014, 015, 017, 019, 021, 022, 023, 037, 038, 045")
	assert.Equal(t, 0, sum[StateNotStarted], "every catalogue control now has a stated position")
}
