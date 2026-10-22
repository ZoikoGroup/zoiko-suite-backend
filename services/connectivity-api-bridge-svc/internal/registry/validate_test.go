package registry_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
)

func issues(c registry.Content) []registry.Issue {
	return registry.ValidateContent(&c, registry.DefaultLimits())
}

func hasIssue(is []registry.Issue, pathPart string) bool {
	for _, i := range is {
		if strings.Contains(i.Path, pathPart) {
			return true
		}
	}
	return false
}

func setProfile(c *registry.Content, kind, name, spec string) {
	for i := range c.Profiles {
		if c.Profiles[i].Kind == kind && c.Profiles[i].Name == name {
			c.Profiles[i].Spec = json.RawMessage(spec)
		}
	}
}

func op(c *registry.Content, id string) *registry.Operation {
	for i := range c.Operations {
		if c.Operations[i].ID == id {
			return &c.Operations[i]
		}
	}
	panic("no op " + id)
}

func TestReferenceContractIsValid(t *testing.T) {
	if is := issues(registrytest.Content()); len(is) != 0 {
		t.Fatalf("%v", is)
	}
}

// Every rule of §6 and §15 has a mutation that breaks it and the issue that names it.
func TestValidationRules(t *testing.T) {
	type tc struct {
		name   string
		mutate func(c *registry.Content)
		want   string
	}
	cases := []tc{
		{"no operations", func(c *registry.Content) { c.Operations = nil }, "operations"},
		{"bad schema ref", func(c *registry.Content) { c.Schemas[0].Ref = "statement" }, "schemas[0].schema"},
		{"schema ref without exact version", func(c *registry.Content) { c.Schemas[0].Ref = "statement@latest" }, "schemas[0].schema"},
		{"duplicate schema", func(c *registry.Content) { c.Schemas[1].Ref = c.Schemas[0].Ref }, "schemas[1].schema"},
		{"unsupported media type", func(c *registry.Content) { c.Schemas[0].MediaType = "text/html" }, "media_type"},
		{"schema over ceiling", func(c *registry.Content) { c.Schemas[0].MaxSizeBytes = 1 << 40 }, "max_size_bytes"},
		{"schema keyword outside subset", func(c *registry.Content) {
			c.Schemas[0].ValidationProfile = json.RawMessage(`{"type":"object","oneOf":[]}`)
		}, "validation_profile"},
		{"csv without columns", func(c *registry.Content) { c.Schemas[0].MediaType = "text/csv" }, "validation_profile"},
		{"unknown profile kind", func(c *registry.Content) { c.Profiles[0].Kind = "MAGIC" }, "profiles[0].kind"},
		{"duplicate profile", func(c *registry.Content) { c.Profiles[1].Name = c.Profiles[0].Name }, "profiles[1].name"},

		// auth
		{"oauth without audience", func(c *registry.Content) {
			setProfile(c, "AUTH", "oauth", `{"mechanism":"OAUTH2_CLIENT_CREDENTIALS","scopes_required":["a"],"credential_class":"PROVIDER_CREDENTIAL"}`)
		}, "audience"},
		{"oauth without scopes", func(c *registry.Content) {
			setProfile(c, "AUTH", "oauth", `{"mechanism":"OAUTH2_CLIENT_CREDENTIALS","audience":"x","credential_class":"PROVIDER_CREDENTIAL"}`)
		}, "scopes_required"},
		{"hmac weak algorithm", func(c *registry.Content) {
			setProfile(c, "AUTH", "hmac-in", `{"mechanism":"HMAC","signature_algorithm":"HMAC-MD5","credential_class":"CALLBACK_SECRET"}`)
		}, "signature_algorithm"},
		{"mtls not connection-bound", func(c *registry.Content) {
			setProfile(c, "AUTH", "hmac-in", `{"mechanism":"MTLS","credential_class":"CALLBACK_SECRET"}`)
		}, "subject_binding"},
		{"basic auth is not a mechanism", func(c *registry.Content) {
			setProfile(c, "AUTH", "hmac-in", `{"mechanism":"BASIC","credential_class":"CALLBACK_SECRET"}`)
		}, "mechanism"},
		{"auth unknown field", func(c *registry.Content) {
			setProfile(c, "AUTH", "hmac-in", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","credential_class":"CALLBACK_SECRET","password":"x"}`)
		}, "profiles"},
		{"auth credential class missing", func(c *registry.Content) {
			setProfile(c, "AUTH", "hmac-in", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256"}`)
		}, "credential_class"},

		// endpoint (SSRF and residency at the contract)
		{"endpoint http scheme", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["http"],"hosts":["api.bank-x.com"],"regions":["uk"],"redirects":"NONE"}`)
		}, "schemes"},
		{"endpoint ip literal", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["169.254.169.254"],"regions":["uk"],"redirects":"NONE"}`)
		}, "hosts"},
		{"endpoint localhost", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["localhost"],"regions":["uk"],"redirects":"NONE"}`)
		}, "hosts"},
		{"endpoint internal name", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["db.corp.internal"],"regions":["uk"],"redirects":"NONE"}`)
		}, "hosts"},
		{"endpoint user-info host", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["evil.com@api.bank-x.com"],"regions":["uk"],"redirects":"NONE"}`)
		}, "hosts"},
		{"endpoint no regions", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["api.bank-x.com"],"regions":[],"redirects":"NONE"}`)
		}, "regions"},
		{"endpoint implicit redirects", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["api.bank-x.com"],"regions":["uk"],"redirects":"FOLLOW"}`)
		}, "redirects"},
		{"endpoint approved redirects without hosts", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["api.bank-x.com"],"regions":["uk"],"redirects":"APPROVED_HOSTS"}`)
		}, "approved_redirect_hosts"},
		{"failover outside regions (NP-30)", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["api.bank-x.com"],"regions":["uk"],"redirects":"NONE","failover":[{"host":"dr.bank-x.com","region":"us"}]}`)
		}, "failover"},
		{"bad port", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["api.bank-x.com"],"ports":[70000],"regions":["uk"],"redirects":"NONE"}`)
		}, "ports"},

		// idempotency
		{"idempotency no key source", func(c *registry.Content) {
			setProfile(c, "IDEMPOTENCY", "idem-in", `{"key_sources":[],"scope":"CONNECTION","retention_days":30,"on_body_change":"CONFLICT"}`)
		}, "key_sources"},
		{"idempotency global scope", func(c *registry.Content) {
			setProfile(c, "IDEMPOTENCY", "idem-in", `{"key_sources":["provider_event_id"],"scope":"GLOBAL","retention_days":30,"on_body_change":"CONFLICT"}`)
		}, "scope"},
		{"idempotency retention out of range", func(c *registry.Content) {
			setProfile(c, "IDEMPOTENCY", "idem-in", `{"key_sources":["provider_event_id"],"scope":"TENANT","retention_days":0,"on_body_change":"CONFLICT"}`)
		}, "retention_days"},
		{"idempotency body change ignored (NP-07)", func(c *registry.Content) {
			setProfile(c, "IDEMPOTENCY", "idem-in", `{"key_sources":["provider_event_id"],"scope":"TENANT","retention_days":30,"on_body_change":"OVERWRITE"}`)
		}, "on_body_change"},
		{"idempotency bad pointer", func(c *registry.Content) {
			setProfile(c, "IDEMPOTENCY", "idem-in", `{"key_sources":["body:event_id"],"scope":"TENANT","retention_days":30,"on_body_change":"CONFLICT"}`)
		}, "key_sources"},

		// callback
		{"callback can invent a transition", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"provider_event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"SETTLED":["PENDING"]}}`)
		}, "transitions"},
		{"callback drives impossible edge", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"provider_event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"RESERVED":["SETTLED"]}}`)
		}, "transitions"},
		{"callback replay window too long", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":86400,"timestamp_header":"X-Timestamp","dedupe_key_source":"provider_event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"PENDING":["SETTLED"]}}`)
		}, "replay_window_seconds"},
		{"callback unsigned timestamp", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"dedupe_key_source":"provider_event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"PENDING":["SETTLED"]}}`)
		}, "timestamp_header"},
		{"callback tenant from payload", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"provider_event_id","schema":"payment-cb@1","tenant_binding":"PAYLOAD","transitions":{"PENDING":["SETTLED"]}}`)
		}, "tenant_binding"},
		{"callback unknown schema", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"provider_event_id","schema":"nope@1","tenant_binding":"CONNECTION","transitions":{"PENDING":["SETTLED"]}}`)
		}, ".schema"},
		{"callback names no credential", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"body:/event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"PENDING":["SETTLED"]},"reference_pointer":"/payment_id","status_pointer":"/status"}`)
		}, ".auth_profile"},
		{"callback names an AUTH profile that does not exist", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"body:/event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"PENDING":["SETTLED"]},"auth_profile":"nope","reference_pointer":"/payment_id","status_pointer":"/status"}`)
		}, ".auth_profile"},
		{"callback verified by a credential of another mechanism", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"body:/event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"PENDING":["SETTLED"]},"auth_profile":"oauth","reference_pointer":"/payment_id","status_pointer":"/status"}`)
		}, ".auth_profile"},
		{"callback does not say where the provider reference is", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"body:/event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"PENDING":["SETTLED"]},"auth_profile":"hmac-in","status_pointer":"/status"}`)
		}, ".reference_pointer"},
		{"callback status pointer is not a pointer", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"body:/event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"PENDING":["SETTLED"]},"auth_profile":"hmac-in","reference_pointer":"/payment_id","status_pointer":"status"}`)
		}, ".status_pointer"},
		{"callback tenant claim pointer is not a pointer", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"body:/event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"PENDING":["SETTLED"]},"auth_profile":"hmac-in","reference_pointer":"/payment_id","status_pointer":"/status","tenant_pointer":"tenant"}`)
		}, ".tenant_pointer"},

		// certification
		{"no certification profile", func(c *registry.Content) {
			var keep []registry.Profile
			for _, p := range c.Profiles {
				if p.Kind != "CERTIFICATION" {
					keep = append(keep, p)
				}
			}
			c.Profiles = keep
		}, "profiles"},
		{"certification omits an obligatory negative test", func(c *registry.Content) {
			setProfile(c, "CERTIFICATION", "cert", `{"test_pack_version":"tp","required_negative_tests":["NP-01"],"expiry_days":30,"retest_triggers":["MAJOR_VERSION"]}`)
		}, "required_negative_tests"},
		{"certification bad test id", func(c *registry.Content) {
			setProfile(c, "CERTIFICATION", "cert", `{"test_pack_version":"tp","required_negative_tests":["NP-99"],"expiry_days":30,"retest_triggers":["MAJOR_VERSION"]}`)
		}, "required_negative_tests"},
		{"certification expiry too long", func(c *registry.Content) {
			setProfile(c, "CERTIFICATION", "cert", `{"test_pack_version":"tp","required_negative_tests":["NP-01"],"expiry_days":9999,"retest_triggers":["MAJOR_VERSION"]}`)
		}, "expiry_days"},
		{"certification no retest triggers", func(c *registry.Content) {
			setProfile(c, "CERTIFICATION", "cert", `{"test_pack_version":"tp","required_negative_tests":["NP-01"],"expiry_days":30,"retest_triggers":[]}`)
		}, "retest_triggers"},

		// operations
		{"duplicate operation", func(c *registry.Content) { c.Operations[1].ID = c.Operations[0].ID }, "operation_id"},
		{"inbound with outbound channel", func(c *registry.Content) { op(c, "statement-push").Channel = "HTTPS" }, "channel"},
		{"http op without method", func(c *registry.Content) { op(c, "statement-push").Method = "" }, "method"},
		{"endpoint template is a url (I-18)", func(c *registry.Content) { op(c, "payment-submit").EndpointTemplate = "https://evil.example/v1" }, "endpoint_template"},
		{"endpoint template path traversal", func(c *registry.Content) { op(c, "payment-submit").EndpointTemplate = "/v1/../admin" }, "endpoint_template"},
		{"endpoint template carries query", func(c *registry.Content) { op(c, "payment-submit").EndpointTemplate = "/v1?x=1" }, "endpoint_template"},
		{"endpoint template userinfo", func(c *registry.Content) { op(c, "payment-submit").EndpointTemplate = "/@evil.com/x" }, "endpoint_template"},
		{"outbound without endpoint profile", func(c *registry.Content) { op(c, "payment-submit").EndpointProfile = "" }, "endpoint_profile"},
		{"inbound with a destination", func(c *registry.Content) { op(c, "statement-push").EndpointProfile = "bank-api" }, "endpoint_profile"},
		{"unknown auth profile", func(c *registry.Content) { op(c, "payment-submit").AuthProfile = "nope" }, "auth_profile"},
		{"inbound without idempotency (I-08)", func(c *registry.Content) { op(c, "statement-push").IdempotencyProfile = "" }, "idempotency_profile"},
		{"callback on an inbound operation", func(c *registry.Content) { op(c, "statement-push").CallbackProfile = "cb" }, "callback_profile"},
		{"unknown request schema (no fallback)", func(c *registry.Content) { op(c, "payment-submit").RequestSchema = "payment-req@2" }, "request_schema"},
		{"request schema wrong direction", func(c *registry.Content) { op(c, "payment-submit").RequestSchema = "payment-resp@1" }, "request_schema"},
		{"no payload ceiling", func(c *registry.Content) { op(c, "payment-submit").MaxPayloadBytes = 0 }, "max_payload_bytes"},
		{"no rate limit", func(c *registry.Content) { op(c, "payment-submit").RateLimitPerMinute = 0 }, "rate_limit_per_minute"},
		{"no data classes", func(c *registry.Content) { op(c, "payment-submit").DataClasses = nil }, "data_classes"},
		{"unknown data class", func(c *registry.Content) { op(c, "payment-submit").DataClasses = []string{"SECRET"} }, "data_classes"},
		{"no regions", func(c *registry.Content) { op(c, "payment-submit").AllowedRegions = nil }, "allowed_regions"},
		{"region the endpoint does not serve", func(c *registry.Content) { op(c, "payment-submit").AllowedRegions = []string{"us"} }, "allowed_regions"},
		{"deprecated without a date", func(c *registry.Content) { op(c, "payment-status").Deprecated = true }, "supported_until"},
		{"date without deprecation", func(c *registry.Content) {
			d := registrytest.T0.Add(time.Hour)
			op(c, "payment-status").SupportedUntil = &d
		}, "supported_until"},

		// OD-07 and unknown-outcome safety (I-13..15)
		{"irreversible without provider idempotency (OD-07)", func(c *registry.Content) {
			op(c, "payment-submit").ProviderIdempotency = registry.ProviderIdempotency{}
			op(c, "payment-submit").ResubmitAfterUnknown = false
		}, "provider_idempotency"},
		{"provider key header missing", func(c *registry.Content) { op(c, "payment-submit").ProviderIdempotency.Header = "" }, "provider_idempotency.header"},
		{"side effect that cannot resolve UNKNOWN", func(c *registry.Content) { op(c, "payment-submit").UnknownResolution = registry.UnknownResolution{} }, "unknown_resolution"},
		{"status query names nothing", func(c *registry.Content) { op(c, "payment-submit").UnknownResolution.StatusQueryOperation = "ghost" }, "status_query_operation"},
		{"status query is itself a side effect", func(c *registry.Content) {
			op(c, "payment-submit").UnknownResolution.StatusQueryOperation = "payment-submit"
		}, "status_query_operation"},
		{"resolve via callback without a callback profile", func(c *registry.Content) {
			op(c, "payment-submit").CallbackProfile = ""
		}, "via_callback"},
		{"blind resubmit without provider key (NP-21)", func(c *registry.Content) {
			o := op(c, "payment-submit")
			o.Irreversible = false
			o.ProviderIdempotency = registry.ProviderIdempotency{}
		}, "resubmit_after_unknown"},
		{"side-effect settings on a read", func(c *registry.Content) { op(c, "payment-status").Irreversible = true }, "irreversible"},

		// mappings
		{"inbound body without a mapping (I-09)", func(c *registry.Content) { c.Mappings = nil }, "request_schema"},
		{"mapping from an outbound schema", func(c *registry.Content) { c.Mappings[0].SourceSchema = "payment-req@1" }, "source_schema"},
		{"mapping from an unknown schema", func(c *registry.Content) { c.Mappings[0].SourceSchema = "ghost@1" }, "source_schema"},
		{"mapping spec invalid", func(c *registry.Content) {
			c.Mappings[0].Spec = json.RawMessage(`{"canonical_contract":"bank.statement.v1","fields":[]}`)
		}, "spec"},
		{"mapping contract ref disagrees with its spec", func(c *registry.Content) { c.Mappings[0].CanonicalContract = "other.v1" }, "canonical_contract"},
		{"duplicate mapping version", func(c *registry.Content) { c.Mappings = append(c.Mappings, c.Mappings[0]) }, "mapping_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := registrytest.Clone(registrytest.Content())
			tc.mutate(&c)
			is := issues(c)
			if !hasIssue(is, tc.want) {
				t.Fatalf("no issue naming %q; got %v", tc.want, is)
			}
		})
	}
}

func TestIssuesCarryNoContractValues(t *testing.T) {
	c := registrytest.Clone(registrytest.Content())
	c.Profiles[0].Spec = json.RawMessage(`{"mechanism":"OAUTH2_CLIENT_CREDENTIALS","audience":"","scopes_required":[],"credential_class":"sk_live_SECRETVALUE"}`) // gitleaks:allow (fake value: proves a credential-shaped credential_class is refused)
	raw, _ := json.Marshal(issues(c))
	if strings.Contains(string(raw), "sk_live_SECRETVALUE") { // gitleaks:allow
		t.Fatalf("a contract value leaked into an issue: %s", raw)
	}
}

func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	c := registrytest.Clone(registrytest.Content())
	op(&c, "payment-submit").MaxPayloadBytes = 0
	op(&c, "payment-submit").DataClasses = nil
	op(&c, "payment-status").AuthProfile = "nope"
	if is := issues(c); len(is) < 3 {
		t.Fatalf("only %d issues for three defects: %v", len(is), is)
	}
}

func TestHeaderValidation(t *testing.T) {
	good := registry.ContractVersion{Semver: "1.2.3", EffectiveFrom: registrytest.T0}
	if is := registry.ValidateHeader(&good); len(is) != 0 {
		t.Fatalf("%v", is)
	}
	for name, v := range map[string]registry.ContractVersion{
		"semver prerelease": {Semver: "1.2.3-beta", EffectiveFrom: registrytest.T0},
		"semver leading 0":  {Semver: "01.2.3", EffectiveFrom: registrytest.T0},
		"semver short":      {Semver: "1.2", EffectiveFrom: registrytest.T0},
		"no effective":      {Semver: "1.0.0"},
		"until before from": {Semver: "1.0.0", EffectiveFrom: registrytest.T0, SupportedUntil: ptr(registrytest.T0.Add(-time.Hour))},
		"bad ref":           {Semver: "1.0.0", EffectiveFrom: registrytest.T0, RollbackRef: "x"},
	} {
		v := v
		if len(registry.ValidateHeader(&v)) == 0 {
			t.Errorf("%s accepted", name)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestRequiredNegativeTestsFollowTheOperations(t *testing.T) {
	c := registrytest.Content()
	got := strings.Join(registry.RequiredNegativeTests(&c), ",")
	for _, want := range []string{"NP-01", "NP-02", "NP-05", "NP-06", "NP-08", "NP-17", "NP-19", "NP-22", "NP-23", "NP-26", "NP-28", "NP-29"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s missing from %s", want, got)
		}
	}
	// a contract with only a file channel is obliged to the batch tests, not the webhook ones
	f := registry.Content{Operations: []registry.Operation{{ID: "file-in", Direction: registry.Inbound, Channel: "SFTP"}}}
	got = strings.Join(registry.RequiredNegativeTests(&f), ",")
	if !strings.Contains(got, "NP-33") || strings.Contains(got, "NP-02") {
		t.Fatalf("file obligations = %s", got)
	}
}

func TestContentHashIsStableAndSensitive(t *testing.T) {
	mk := func() *registry.ContractVersion {
		in := registrytest.Input("1.0.0")
		return &registry.ContractVersion{Semver: in.Semver, EffectiveFrom: in.EffectiveFrom, RollbackRef: in.RollbackRef, Content: in.Content}
	}
	a, b := mk(), mk()
	ha, _ := registry.ContentHash(a)
	hb, _ := registry.ContentHash(b)
	if ha != hb || len(ha) != 64 {
		t.Fatalf("unstable hash %q %q", ha, hb)
	}
	b.Content.Operations[0].RateLimitPerMinute++
	if hc, _ := registry.ContentHash(b); hc == ha {
		t.Fatal("content change did not change the hash")
	}
	b = mk()
	b.RollbackRef = "RUNBOOK-999 something else"
	if hc, _ := registry.ContentHash(b); hc == ha {
		t.Fatal("header change did not change the hash")
	}
	b = mk()
	b.EffectiveFrom = b.EffectiveFrom.Add(time.Hour)
	if hc, _ := registry.ContentHash(b); hc == ha {
		t.Fatal("effective date change did not change the hash")
	}
	// the same instant in another zone is the same contract
	b = mk()
	b.EffectiveFrom = b.EffectiveFrom.In(time.FixedZone("x", 3600))
	if hc, _ := registry.ContentHash(b); hc != ha {
		t.Fatal("timezone representation changed the hash")
	}
}

func TestInboundVerificationFields(t *testing.T) {
	hm := func(extra string) func(c *registry.Content) {
		return func(c *registry.Content) {
			setProfile(c, "AUTH", "hmac-in", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","credential_class":"CALLBACK_SECRET"`+extra+`}`)
		}
	}
	for name, tc := range map[string]struct {
		mutate func(*registry.Content)
		want   string
	}{
		"bad signature header":     {hm(`,"signature_header":"bad header"`), "signature_header"},
		"bad encoding":             {hm(`,"signature_encoding":"rot13"`), "signature_encoding"},
		"bad signed content":       {hm(`,"signed_content":"HEADERS"`), "signed_content"},
		"bad timestamp format":     {hm(`,"timestamp_format":"LOCAL"`), "timestamp_format"},
		"replay window too long":   {hm(`,"replay_window_seconds":86400`), "replay_window_seconds"},
		"replay window too short":  {hm(`,"replay_window_seconds":5`), "replay_window_seconds"},
		"body-only without a time": {hm(`,"signed_content":"BODY"`), "timestamp_header"},
		"bad api key header": {func(c *registry.Content) {
			setProfile(c, "AUTH", "hmac-in", `{"mechanism":"API_KEY_HEADER","credential_class":"CALLBACK_SECRET","api_key_header":"x y"}`)
		}, "api_key_header"},
		"tenant pointer not a pointer": {func(c *registry.Content) { op(c, "statement-push").PayloadTenantPointer = "tenant" }, "payload_tenant_pointer"},
		"tenant pointer on outbound":   {func(c *registry.Content) { op(c, "payment-submit").PayloadTenantPointer = "/tenant" }, "payload_tenant_pointer"},
		"account pointer on outbound":  {func(c *registry.Content) { op(c, "payment-submit").PayloadAccountPointer = "/a" }, "payload_account_pointer"},
		"schema header on outbound":    {func(c *registry.Content) { op(c, "payment-submit").SchemaVersionHeader = "X-V" }, "schema_version_header"},
		"schema header malformed":      {func(c *registry.Content) { op(c, "statement-push").SchemaVersionHeader = "bad header" }, "schema_version_header"},
	} {
		c := registrytest.Clone(registrytest.Content())
		tc.mutate(&c)
		if is := issues(c); !hasIssue(is, tc.want) {
			t.Errorf("%s: no issue naming %q; got %v", name, tc.want, is)
		}
	}
	// the good forms validate and surface with defaults applied
	c := registrytest.Clone(registrytest.Content())
	hm(`,"signature_header":"X-Hub-Signature-256","signature_encoding":"hex","signed_content":"BODY","timestamp_header":"X-Hub-Timestamp","timestamp_format":"RFC3339","replay_window_seconds":120`)(&c)
	op(&c, "statement-push").PayloadTenantPointer = "/tenant/id"
	op(&c, "statement-push").PayloadAccountPointer = "/account"
	op(&c, "statement-push").SchemaVersionHeader = "X-Schema"
	if is := issues(c); len(is) != 0 {
		t.Fatalf("%v", is)
	}
	v := &registry.ContractVersion{Content: c}
	info := v.UsedAuthProfiles()["hmac-in"]
	if info.SignatureHeader != "X-Hub-Signature-256" || info.ReplayWindowSeconds != 120 || info.TimestampFormat != "RFC3339" {
		t.Fatalf("%+v", info)
	}
	def := (&registry.ContractVersion{Content: registrytest.Content()}).UsedAuthProfiles()["hmac-in"]
	if def.SignatureHeader != registry.DefaultSignatureHeader || def.ReplayWindowSeconds != 300 || def.SignedContent != "TIMESTAMP_DOT_BODY" || def.SignatureEncoding != "hex" {
		t.Fatalf("defaults %+v", def)
	}
}
