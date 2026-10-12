package config

import (
	"reflect"
	"testing"
	"time"
)

func TestMirrorFlags_Defaults(t *testing.T) {
	t.Setenv("CLOSE_SIGNING_KEY", "k")
	for _, k := range []string{"PERIOD_SERVICE_MIRROR", "ACCOUNTING_PERIOD_URL", "PERIOD_MIRROR_REOPEN_WINDOW", "WORKFLOW_REF_CALLERS"} {
		t.Setenv(k, "")
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PeriodMirrorEnabled || c.PeriodMirrorModeInvalid {
		t.Error("mirror must default to off")
	}
	if c.AccountingPeriodURL != "http://accounting-period-svc:8174" {
		t.Errorf("url %q", c.AccountingPeriodURL)
	}
	if c.PeriodMirrorReopenWindow != 24*time.Hour || c.PeriodMirrorReopenWindowAdjusted {
		t.Errorf("window %v", c.PeriodMirrorReopenWindow)
	}
	if !reflect.DeepEqual(c.WorkflowRefCallers, []string{"accounting-period-svc"}) {
		t.Errorf("callers %v", c.WorkflowRefCallers)
	}
}

func TestMirrorFlags_Parsing(t *testing.T) {
	for raw, want := range map[string]struct{ on, invalid bool }{
		"": {false, false}, "off": {false, false}, "OFF": {false, false}, "on": {true, false}, " On ": {true, false},
		"true": {false, true}, "1": {false, true}, "yes": {false, true}, "enforce": {false, true},
	} {
		on, invalid := normalizeMirrorMode(raw)
		if on != want.on || invalid != want.invalid {
			t.Errorf("%q => on=%v invalid=%v, want %v", raw, on, invalid, want)
		}
	}
	for raw, want := range map[string]struct {
		d   time.Duration
		adj bool
	}{
		"": {24 * time.Hour, false}, "1h": {time.Hour, false}, "72h": {72 * time.Hour, false},
		"73h": {72 * time.Hour, true}, "720h": {72 * time.Hour, true},
		"garbage": {24 * time.Hour, true}, "0": {24 * time.Hour, true}, "-5h": {24 * time.Hour, true},
	} {
		d, adj := normalizeReopenWindow(raw)
		if d != want.d || adj != want.adj {
			t.Errorf("%q => %v adj=%v, want %v", raw, d, adj, want)
		}
	}
	if got := splitList(" a, b ,,c "); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("%v", got)
	}
	if got := splitList(""); len(got) != 0 {
		t.Errorf("%v", got)
	}
}
