package config_test

import (
	"reflect"
	"testing"

	"zoiko.io/general-ledger-svc/internal/config"
)

func TestParseOnOff(t *testing.T) {
	for raw, want := range map[string]bool{"on": true, "ON": true, " true ": true, "off": false, "Off": false, "false": false} {
		if got, err := config.ParseOnOff("X", raw, !want); err != nil || got != want {
			t.Fatalf("%q: %v, %v; want %v", raw, got, err, want)
		}
	}
	// Unset takes the default either way; anything else is an error, so a typo cannot leave a feature in a state nobody chose.
	for _, def := range []bool{true, false} {
		if got, err := config.ParseOnOff("X", "", def); err != nil || got != def {
			t.Fatalf("unset with default %v: %v %v", def, got, err)
		}
	}
	for _, raw := range []string{"of", "yes", "1", "enabled", "onn"} {
		if _, err := config.ParseOnOff("X", raw, true); err == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
}

func TestParseTopics(t *testing.T) {
	def, err := config.ParseTopics("")
	if err != nil || !reflect.DeepEqual(def, []string{"zoiko.data.quality.events", "zoiko.data.reconciliation_engine.events"}) {
		t.Fatalf("default: %v %v", def, err)
	}
	// The default is a copy: changing what one call returned cannot change the next.
	def[0] = "tampered"
	if again, _ := config.ParseTopics(" "); again[0] != "zoiko.data.quality.events" {
		t.Fatalf("the default list is shared: %v", again)
	}
	got, err := config.ParseTopics(" a.b , c-d,a.b ,e_f")
	if err != nil || !reflect.DeepEqual(got, []string{"a.b", "c-d", "e_f"}) {
		t.Fatalf("%v %v", got, err)
	}
	for _, raw := range []string{"a,,b", ",", "bad topic", "a/b", "-leading", "x*"} {
		if got, err := config.ParseTopics(raw); err == nil {
			t.Fatalf("%q was accepted as %v", raw, got)
		}
	}
}

func TestLoad_CertWithdrawalSettings(t *testing.T) {
	t.Setenv("EVENT_RESIDENCY_REGION", "uk")
	for _, k := range []string{"CERT_WITHDRAWAL_CONSUMER", "CERT_WITHDRAWAL_TOPICS", "CERT_WITHDRAWAL_GROUP_ID"} {
		t.Setenv(k, "")
	}
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.CertWithdrawalConsumer || len(c.CertWithdrawalTopics) != 2 || c.CertWithdrawalGroupID != "general-ledger-svc.certification-withdrawals" {
		t.Fatalf("defaults: %+v", c)
	}
	t.Setenv("CERT_WITHDRAWAL_CONSUMER", "off")
	t.Setenv("CERT_WITHDRAWAL_TOPICS", "only.one")
	t.Setenv("CERT_WITHDRAWAL_GROUP_ID", "custom-group")
	if c, err = config.Load(); err != nil || c.CertWithdrawalConsumer || !reflect.DeepEqual(c.CertWithdrawalTopics, []string{"only.one"}) || c.CertWithdrawalGroupID != "custom-group" {
		t.Fatalf("%+v %v", c, err)
	}
	// A bad value stops the service at startup.
	t.Setenv("CERT_WITHDRAWAL_CONSUMER", "of")
	if _, err := config.Load(); err == nil {
		t.Fatal("a bad switch was accepted at startup")
	}
	t.Setenv("CERT_WITHDRAWAL_CONSUMER", "on")
	t.Setenv("CERT_WITHDRAWAL_TOPICS", "bad topic")
	if _, err := config.Load(); err == nil {
		t.Fatal("a bad topic was accepted at startup")
	}
}
