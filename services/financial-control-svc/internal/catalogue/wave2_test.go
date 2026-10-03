package catalogue

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/source"
)

var opts = Wave2Options{ARControlAccounts: "1200", APControlAccounts: "2000,2010", CashAccounts: "1000",
	BankAccountsExpected: []string{"ACC-1"}, EffectiveFrom: "2026-09-01"}

func byCode(t *testing.T) map[string]domain.CreateControlDefinitionRequest {
	t.Helper()
	defs, err := Wave2(opts)
	require.NoError(t, err)
	m := map[string]domain.CreateControlDefinitionRequest{}
	for _, d := range defs {
		m[d.ControlCode] = d
	}
	return m
}

func TestWave2_SixControlsAllValidAndExecutable(t *testing.T) {
	m := byCode(t)
	require.Len(t, m, 6)
	for _, code := range []string{"FIN-CTRL-001", "FIN-CTRL-002", "FIN-CTRL-003", "FIN-CTRL-027", "FIN-CTRL-028", "FIN-CTRL-032"} {
		d, ok := m[code]
		require.True(t, ok, code)
		require.NoError(t, d.Validate(), code)

		logic, err := domain.ParseRuleLogic(d.InitialLogic)
		require.NoError(t, err, code)
		_, err = source.ParseSpec(d.SourceSpec)
		require.NoError(t, err, "%s source_spec must be executable by the engine", code)
		if logic.TwoSided() {
			_, err = source.ParseSpec(d.TargetSpec)
			require.NoError(t, err, "%s target_spec must be executable by the engine", code)
		}
		assert.NotEmpty(t, d.OwnerRole, code)
		assert.NotEmpty(t, d.Assertions, code)
	}
}

func TestWave2_KeyControlsAreCloseGatingWithSeparateCertifier(t *testing.T) {
	m := byCode(t)
	for _, code := range []string{"FIN-CTRL-001", "FIN-CTRL-002", "FIN-CTRL-003"} {
		d := m[code]
		assert.Equal(t, "KEY", d.RiskTier, code)
		assert.True(t, d.CloseGating, code)
		assert.NotEmpty(t, d.ReviewerRole, code)
		assert.NotEmpty(t, d.CertifierRole, code)
		assert.NotEqual(t, d.OwnerRole, d.CertifierRole, "%s: the preparer role is not the certifier role", code)
	}
	for _, code := range []string{"FIN-CTRL-027", "FIN-CTRL-028", "FIN-CTRL-032"} {
		assert.False(t, m[code].CloseGating, code)
	}
}

func TestWave2_GLTargetsUseTheRightNormalBalanceAndAccounts(t *testing.T) {
	m := byCode(t)
	target := func(code string) source.Spec {
		s, err := source.ParseSpec(m[code].TargetSpec)
		require.NoError(t, err)
		return s
	}
	ar, ap, cash := target("FIN-CTRL-001"), target("FIN-CTRL-002"), target("FIN-CTRL-003")
	assert.Equal(t, "DEBIT", ar.Params["normal_balance"], "receivables are debit-normal")
	assert.Equal(t, "1200", ar.Params["account_codes"])
	assert.Equal(t, "CREDIT", ap.Params["normal_balance"], "payables are credit-normal, so both report a positive open balance")
	assert.Equal(t, "2000,2010", ap.Params["account_codes"])
	assert.Equal(t, "DEBIT", cash.Params["normal_balance"])
	assert.Equal(t, "general-ledger", ar.System)
	assert.Equal(t, "account-postings", ar.Population)
}

func TestWave2_GroupControlsAllowGroupsAndSkipContentDuplicates(t *testing.T) {
	m := byCode(t)
	for _, code := range []string{"FIN-CTRL-001", "FIN-CTRL-002", "FIN-CTRL-003"} {
		l, err := domain.ParseRuleLogic(m[code].InitialLogic)
		require.NoError(t, err)
		assert.True(t, l.AllowGroups, code)
		assert.True(t, l.SkipContentDuplicates, code)
	}
}

func TestWave2_BankControlHasOutstandingWindowAndOwnedExclusion(t *testing.T) {
	l, err := domain.ParseRuleLogic(byCode(t)["FIN-CTRL-003"].InitialLogic)
	require.NoError(t, err)
	assert.Equal(t, 5, l.OutstandingDays)
	require.Len(t, l.Exclusions, 1)
	x := l.Exclusions[0]
	assert.Equal(t, "A", x.Side)
	assert.Equal(t, "status", x.Attr)
	assert.Equal(t, []string{"SUPERSEDED"}, x.Values)
	assert.NotEmpty(t, x.Reason, "every exclusion is explained")
	assert.NotEmpty(t, x.Authority, "and owned")
}

func TestWave2_SingleSidedControls(t *testing.T) {
	m := byCode(t)
	seq, _ := domain.ParseRuleLogic(m["FIN-CTRL-027"].InitialLogic)
	dup, _ := domain.ParseRuleLogic(m["FIN-CTRL-028"].InitialLogic)
	cov, _ := domain.ParseRuleLogic(m["FIN-CTRL-032"].InitialLogic)
	assert.Equal(t, domain.KindSequenceGap, seq.Kind)
	assert.Equal(t, domain.KindDuplicateScan, dup.Kind)
	assert.Equal(t, []string{"attr:vendor_id", "amount", "currency", "date"}, dup.KeyFields)
	assert.Equal(t, domain.KindDateCoverage, cov.Kind)
	assert.Equal(t, []string{"ACC-1"}, cov.ExpectedGroups, "a dead account cannot pass by sending nothing")
	assert.True(t, cov.SkipWeekends)
	for _, l := range []domain.RuleLogic{seq, dup, cov} {
		assert.False(t, l.TwoSided())
	}
}

func TestWave2_RequiresEntitySpecificInputs(t *testing.T) {
	for name, mutate := range map[string]func(*Wave2Options){
		"ar accounts":   func(o *Wave2Options) { o.ARControlAccounts = "" },
		"ap accounts":   func(o *Wave2Options) { o.APControlAccounts = " " },
		"cash accounts": func(o *Wave2Options) { o.CashAccounts = "" },
		"effective":     func(o *Wave2Options) { o.EffectiveFrom = "" },
	} {
		o := opts
		mutate(&o)
		_, err := Wave2(o)
		assert.True(t, errors.Is(err, domain.ErrInvalidArgument), name)
	}
	o := opts
	o.BankAccountsExpected = nil
	defs, err := Wave2(o)
	require.NoError(t, err, "expected bank accounts are optional")
	var raw map[string]any
	for _, d := range defs {
		if d.ControlCode == "FIN-CTRL-032" {
			require.NoError(t, json.Unmarshal(d.InitialLogic, &raw))
			assert.Equal(t, []any{}, raw["expected_groups"], "a nil list is serialised as [] not null")
		}
	}
}

// A misspelt logic key is an error, never a silently different control.
func TestStrictLogicRejectsTypos(t *testing.T) {
	_, err := domain.ParseRuleLogic(json.RawMessage(`{"kind":"MATCH","allow_group":true}`))
	assert.Error(t, err)
	_, err = domain.ParseRuleLogic(json.RawMessage(`{"kind":"MATCH","exclusions":[{"attr":"status","values":["X"],"reason":"r"}]}`))
	assert.Error(t, err, "an exclusion without an authority is refused")
}
