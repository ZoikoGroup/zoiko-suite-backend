package config

import (
	"reflect"
	"testing"
)

func TestKernelCloseFlags(t *testing.T) {
	t.Setenv("CLOSE_SIGNING_KEY", "k")
	set := func(gate, seal, require string) *Config {
		t.Setenv("KERNEL_CLOSE_GATE_MODE", gate)
		t.Setenv("KERNEL_PERIOD_SEAL_MODE", seal)
		t.Setenv("KERNEL_REQUIRED_RECONCILIATIONS", require)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// both off unless asked for: closing behaves as it did before the kernel was wired in
	if c := set("", "", ""); c.KernelCloseGateMode != "off" || c.KernelPeriodSealMode != "off" || c.KernelModeInvalid || len(c.KernelRequiredReconciliations) != 0 {
		t.Fatalf("%+v", c)
	}
	c := set("enforce", "ENFORCE", " ar_gl, AP_GL ,,bank_gl")
	if c.KernelCloseGateMode != "enforce" || c.KernelPeriodSealMode != "enforce" || c.KernelModeInvalid {
		t.Fatalf("%+v", c)
	}
	if !reflect.DeepEqual(c.KernelRequiredReconciliations, []string{"AR_GL", "AP_GL", "BANK_GL"}) {
		t.Fatalf("%v", c.KernelRequiredReconciliations)
	}
	// a typo does not switch a control off: it enforces, and says so
	if c = set("enfroce", "", ""); c.KernelCloseGateMode != "enforce" || !c.KernelModeInvalid {
		t.Fatalf("%+v", c)
	}
}
