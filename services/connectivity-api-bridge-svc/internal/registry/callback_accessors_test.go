package registry_test

import (
	"encoding/json"
	"testing"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
)

func TestCallbackFor_DecodesTheProfileAndOnlyForAnOperationThatDeclaresOne(t *testing.T) {
	v := &registry.ContractVersion{Content: registrytest.Content()}
	submit := v.Operation("payment-submit")
	cb := v.CallbackFor(submit)
	if cb == nil || cb.Mechanism != "HMAC" || cb.AuthProfile != "hmac-in" || cb.ReferencePointer != "/payment_id" || cb.StatusPointer != "/status" ||
		cb.ClientRefPointer != "/client_ref" || cb.ReplayWindowSeconds != 300 || len(cb.Transitions["PENDING"]) != 2 {
		t.Fatalf("%+v", cb)
	}
	if v.CallbackFor(v.Operation("payment-status")) != nil || v.CallbackFor(nil) != nil {
		t.Fatal("an operation with no callback profile has no callback info")
	}
}

func TestUsedAuthProfiles_IncludesTheCredentialThatVerifiesCallbacks(t *testing.T) {
	c := registrytest.Content()
	// a callback key that no operation uses for itself: still a credential the connection must bind
	c.Profiles = append(c.Profiles, registry.Profile{Kind: registry.ProfileAuth, Name: "cb-key",
		Spec: json.RawMessage(`{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","credential_class":"CALLBACK_SECRET"}`)})
	for i := range c.Profiles {
		if c.Profiles[i].Name == "cb" {
			c.Profiles[i].Spec = json.RawMessage(`{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"body:/event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"PENDING":["SETTLED"]},"auth_profile":"cb-key","reference_pointer":"/payment_id","status_pointer":"/status"}`)
		}
	}
	v := &registry.ContractVersion{Content: c}
	used := v.UsedAuthProfiles()
	if _, ok := used["cb-key"]; !ok {
		t.Fatalf("the callback's credential is not among the profiles a connection must bind: %v", used)
	}
	if _, ok := used["oauth"]; !ok {
		t.Fatalf("%v", used)
	}
}

func TestEndpointInfo_CarriesTheRedirectPolicy(t *testing.T) {
	c := registrytest.Content()
	for i := range c.Profiles {
		if c.Profiles[i].Name == "bank-api" {
			c.Profiles[i].Spec = json.RawMessage(`{"schemes":["https"],"hosts":["api.bank-x.com"],"ports":[443],"regions":["uk"],"redirects":"APPROVED_HOSTS","approved_redirect_hosts":["files.bank-x.com"]}`)
		}
	}
	v := &registry.ContractVersion{Content: c}
	ep := v.UsedEndpointProfiles()["bank-api"]
	if ep.Redirects != "APPROVED_HOSTS" || len(ep.ApprovedRedirectHosts) != 1 || ep.ApprovedRedirectHosts[0] != "files.bank-x.com" || len(ep.Ports) != 1 || ep.Ports[0] != 443 {
		t.Fatalf("%+v", ep)
	}
}
