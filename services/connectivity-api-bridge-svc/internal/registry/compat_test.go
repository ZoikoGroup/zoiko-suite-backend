package registry_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
)

func version(semver string, c registry.Content) *registry.ContractVersion {
	in := registrytest.Input(semver)
	in.Content = c
	return &registry.ContractVersion{ContractVersionID: "v-" + semver, ConnectorID: "bank-x", Semver: semver,
		EffectiveFrom: in.EffectiveFrom, RollbackRef: in.RollbackRef, Content: c}
}

func classify(t *testing.T, mutate func(*registry.Content), candidateSemver string) *registry.CompatReport {
	t.Helper()
	base := version("1.0.0", registrytest.Content())
	c := registrytest.Clone(registrytest.Content())
	mutate(&c)
	return registry.Classify(base, version(candidateSemver, c))
}

func has(r *registry.CompatReport, code string) bool {
	for _, c := range r.Changes {
		if c.Code == code {
			return true
		}
	}
	return false
}

func schemaOf(c *registry.Content, ref string) *registry.Schema {
	for i := range c.Schemas {
		if c.Schemas[i].Ref == ref {
			return &c.Schemas[i]
		}
	}
	panic(ref)
}

// bump gives an operation's schema a new version so a candidate can change it legitimately.
func bumpStatement(c *registry.Content, profile string) {
	schemaOf(c, "statement@1").Ref = "statement@2"
	schemaOf(c, "statement@2").ValidationProfile = json.RawMessage(profile)
	op(c, "statement-push").RequestSchema = "statement@2"
	c.Mappings[0].SourceSchema = "statement@2"
}

func TestFirstVersionHasNothingToCompare(t *testing.T) {
	r := registry.Classify(nil, version("1.0.0", registrytest.Content()))
	if r.Class != registry.ClassNone || len(r.Changes) != 0 {
		t.Fatalf("%+v", r)
	}
	if is := r.Enforce(nil, version("1.0.0", registrytest.Content())); len(is) != 0 {
		t.Fatalf("%v", is)
	}
}

func TestIdenticalContractIsNoChange(t *testing.T) {
	r := classify(t, func(*registry.Content) {}, "1.0.1")
	if r.Class != registry.ClassNone {
		t.Fatalf("%+v", r.Changes)
	}
}

// §6.4, row by row.
func TestCompatibilityClassification(t *testing.T) {
	statementWith := func(extraProps, required string) string {
		return `{"type":"object","required":[` + required + `],"properties":{"event_id":{"type":"string"},"account":{"type":"string"},"balance":{"type":"string"},
		 "currency":{"type":"string","enum":["GBP","EUR","USD"]},"booked_at":{"type":"string"}` + extraProps + `}}`
	}
	req := `"event_id","account","balance","booked_at"`
	cases := []struct {
		name   string
		mutate func(*registry.Content)
		class  string
		code   string
	}{
		{"add optional field", func(c *registry.Content) {
			bumpStatement(c, statementWith(`,"memo":{"type":"string"}`, req))
		}, registry.ClassPotentiallyCompatible, "optional_property_added"},
		{"add required field", func(c *registry.Content) {
			bumpStatement(c, statementWith(`,"memo":{"type":"string"}`, req+`,"memo"`))
		}, registry.ClassBreaking, "required_property_added"},
		{"remove a field", func(c *registry.Content) {
			bumpStatement(c, `{"type":"object","required":["event_id"],"properties":{"event_id":{"type":"string"}}}`)
		}, registry.ClassBreaking, "property_removed"},
		{"rename a required field", func(c *registry.Content) {
			bumpStatement(c, strings.Replace(statementWith("", req), `"account"`, `"acct"`, -1))
		}, registry.ClassBreaking, "property_removed"},
		{"optional becomes required", func(c *registry.Content) {
			bumpStatement(c, statementWith("", req+`,"currency"`))
		}, registry.ClassBreaking, "property_became_required"},
		{"required becomes optional", func(c *registry.Content) {
			bumpStatement(c, statementWith("", `"event_id","account","balance"`))
		}, registry.ClassPotentiallyCompatible, "property_became_optional"},
		{"field type changes", func(c *registry.Content) {
			bumpStatement(c, strings.Replace(statementWith("", req), `"balance":{"type":"string"}`, `"balance":{"type":"number"}`, 1))
		}, registry.ClassBreaking, "schema_type_changed"},
		{"enum value removed", func(c *registry.Content) {
			bumpStatement(c, strings.Replace(statementWith("", req), `["GBP","EUR","USD"]`, `["GBP","EUR"]`, 1))
		}, registry.ClassBreaking, "enum_value_removed"},
		{"enum value added", func(c *registry.Content) {
			bumpStatement(c, strings.Replace(statementWith("", req), `["GBP","EUR","USD"]`, `["GBP","EUR","USD","CHF"]`, 1))
		}, registry.ClassPotentiallyCompatible, "enum_value_added"},
		{"closed to extra properties", func(c *registry.Content) {
			bumpStatement(c, strings.Replace(statementWith("", req), `{"type":"object","required"`, `{"type":"object","additionalProperties":false,"required"`, 1))
		}, registry.ClassBreaking, "additional_properties_closed"},
		{"length constraint tightened", func(c *registry.Content) {
			bumpStatement(c, strings.Replace(statementWith("", req), `"account":{"type":"string"}`, `"account":{"type":"string","maxLength":10}`, 1))
		}, registry.ClassBreaking, "length_constraint_tightened"},
		{"change auth mechanism", func(c *registry.Content) {
			setProfile(c, "AUTH", "oauth", `{"mechanism":"MTLS","subject_binding":"CONNECTION","credential_class":"PROVIDER_CREDENTIAL"}`)
		}, registry.ClassMaterialSecurity, "auth_mechanism_changed"},
		{"change oauth audience", func(c *registry.Content) {
			setProfile(c, "AUTH", "oauth", `{"mechanism":"OAUTH2_CLIENT_CREDENTIALS","audience":"https://other.example","scopes_required":["payments.write","payments.read"],"credential_class":"PROVIDER_CREDENTIAL"}`)
		}, registry.ClassMaterialSecurity, "auth_mechanism_changed"},
		{"change required oauth scopes (NP-48)", func(c *registry.Content) {
			setProfile(c, "AUTH", "oauth", `{"mechanism":"OAUTH2_CLIENT_CREDENTIALS","audience":"https://api.bank-x.com","scopes_required":["payments.write","payments.read","accounts.read"],"credential_class":"PROVIDER_CREDENTIAL"}`)
		}, registry.ClassMaterialSecurity, "required_scopes_changed"},
		{"change callback signing algorithm (NP-49)", func(c *registry.Content) {
			setProfile(c, "CALLBACK", "cb", `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA512","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"body:/event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"PENDING":["SETTLED"]}}`)
		}, registry.ClassMaterialSecurity, "callback_signing_changed"},
		{"change mapping semantics", func(c *registry.Content) {
			c.Mappings[0].Spec = json.RawMessage(strings.Replace(registrytest.MappingSpec, `"scale":2`, `"scale":4`, 1))
		}, registry.ClassMaterialBusiness, "mapping_semantics_changed"},
		{"change provider base domain", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["api.bank-y.com"],"ports":[443],"regions":["uk"],"redirects":"NONE"}`)
		}, registry.ClassMaterialEgress, "endpoint_changed"},
		{"change provider region", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["api.bank-x.com"],"ports":[443],"regions":["uk","us"],"redirects":"NONE"}`)
		}, registry.ClassMaterialEgress, "endpoint_changed"},
		{"loosen redirect policy", func(c *registry.Content) {
			setProfile(c, "ENDPOINT", "bank-api", `{"schemes":["https"],"hosts":["api.bank-x.com"],"ports":[443],"regions":["uk"],"redirects":"SAME_HOST"}`)
		}, registry.ClassMaterialEgress, "endpoint_changed"},
		{"deprecate an operation", func(c *registry.Content) {
			o := op(c, "payment-status")
			o.Deprecated = true
			d := registrytest.T0.Add(400 * 24 * time.Hour)
			o.SupportedUntil = &d
		}, registry.ClassLifecycle, "operation_deprecated"},
		{"remove an operation", func(c *registry.Content) {
			var keep []registry.Operation
			for _, o := range c.Operations {
				if o.ID != "payment-status" {
					keep = append(keep, o)
				}
			}
			c.Operations = keep
		}, registry.ClassBreaking, "operation_removed"},
		{"change an operation path", func(c *registry.Content) { op(c, "payment-submit").EndpointTemplate = "/v2/payments" }, registry.ClassBreaking, "operation_path_changed"},
		{"change an operation method", func(c *registry.Content) { op(c, "payment-submit").Method = "PUT" }, registry.ClassBreaking, "operation_shape_changed"},
		{"drop provider idempotency", func(c *registry.Content) {
			o := op(c, "payment-submit")
			o.ProviderIdempotency = registry.ProviderIdempotency{}
		}, registry.ClassBreaking, "provider_idempotency_dropped"},
		{"add an operation", func(c *registry.Content) {
			c.Operations = append(c.Operations, registry.Operation{ID: "payment-list", Direction: registry.Outbound, Channel: "HTTPS", Method: "GET",
				EndpointTemplate: "/v1/payments", AuthProfile: "oauth", EndpointProfile: "bank-api", ResponseSchema: "status-resp@1",
				MaxPayloadBytes: 1 << 16, RateLimitPerMinute: 10, DataClasses: []string{"FINANCIAL"}, AllowedRegions: []string{"uk"}})
		}, registry.ClassPotentiallyCompatible, "operation_added"},
		{"change irreversibility", func(c *registry.Content) { op(c, "payment-submit").Irreversible = false }, registry.ClassMaterialBusiness, "irreversibility_changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := classify(t, tc.mutate, "2.0.0")
			if r.Class != tc.class || !has(r, tc.code) {
				t.Fatalf("class=%s want %s, code %s present=%v; changes=%s", r.Class, tc.class, tc.code, has(r, tc.code), r.DescribeChanges())
			}
		})
	}
}

func TestStrictestChangeDecidesTheClass(t *testing.T) {
	r := classify(t, func(c *registry.Content) {
		setProfile(c, "AUTH", "oauth", `{"mechanism":"MTLS","subject_binding":"CONNECTION","credential_class":"PROVIDER_CREDENTIAL"}`)
		op(c, "payment-submit").EndpointTemplate = "/v2/payments"
	}, "2.0.0")
	if r.Class != registry.ClassBreaking || !has(r, "auth_mechanism_changed") || !has(r, "operation_path_changed") {
		t.Fatalf("%s", r.DescribeChanges())
	}
	// strictest first
	if r.Changes[0].Class != registry.ClassBreaking {
		t.Fatalf("changes not ordered by severity: %s", r.DescribeChanges())
	}
}

// §6.4 "Required handling": the classification places obligations on the candidate.
func TestEnforcement(t *testing.T) {
	base := version("1.4.0", registrytest.Content())
	candidate := func(semver string, mutate func(*registry.Content), mod func(*registry.ContractVersion)) *registry.ContractVersion {
		c := registrytest.Clone(registrytest.Content())
		mutate(&c)
		v := version(semver, c)
		if mod != nil {
			mod(v)
		}
		return v
	}
	breaking := func(c *registry.Content) { op(c, "payment-submit").EndpointTemplate = "/v2/payments" }
	material := func(c *registry.Content) {
		setProfile(c, "AUTH", "oauth", `{"mechanism":"MTLS","subject_binding":"CONNECTION","credential_class":"PROVIDER_CREDENTIAL"}`)
	}
	business := func(c *registry.Content) {
		c.Mappings[0].Spec = json.RawMessage(strings.Replace(registrytest.MappingSpec, `"scale":2`, `"scale":4`, 1))
	}
	withPlan := func(v *registry.ContractVersion) { v.MigrationPlanRef = "MIG-77 move pinned connections" }
	withReplay := func(v *registry.ContractVersion) { v.ReplayAnalysisRef = "REPLAY-9 affected domains analysed" }

	enforce := func(v *registry.ContractVersion) []registry.Issue {
		return registry.Classify(base, v).Enforce(base, v)
	}

	// a breaking change: new major AND a migration plan
	if is := enforce(candidate("1.5.0", breaking, withPlan)); !hasIssue(is, "semver") {
		t.Errorf("breaking change accepted without a major bump: %v", is)
	}
	if is := enforce(candidate("2.0.0", breaking, nil)); !hasIssue(is, "migration_plan_ref") {
		t.Errorf("breaking change accepted without a migration plan: %v", is)
	}
	if is := enforce(candidate("2.0.0", breaking, withPlan)); len(is) != 0 {
		t.Errorf("a well-formed breaking change was refused: %v", is)
	}
	// a material security change: at least a new minor
	if is := enforce(candidate("1.4.1", material, nil)); !hasIssue(is, "semver") {
		t.Errorf("material change accepted as a patch: %v", is)
	}
	if is := enforce(candidate("1.5.0", material, nil)); len(is) != 0 {
		t.Errorf("material change with a minor bump refused: %v", is)
	}
	if r := registry.Classify(base, candidate("1.5.0", material, nil)); !r.Requires.Reauthorization || !r.Requires.Recertification || !r.Requires.Reapproval {
		t.Errorf("material security change must require reauthorization and recertification: %+v", r.Requires)
	}
	// a mapping meaning change: affected-domain replay analysis
	if is := enforce(candidate("1.5.0", business, nil)); !hasIssue(is, "replay_analysis_ref") {
		t.Errorf("mapping semantic change accepted without replay analysis: %v", is)
	}
	if is := enforce(candidate("1.5.0", business, withReplay)); len(is) != 0 {
		t.Errorf("mapping change with analysis refused: %v", is)
	}
	// semver must move forward
	if is := enforce(candidate("1.4.0", func(*registry.Content) {}, nil)); !hasIssue(is, "semver") {
		t.Errorf("equal version accepted: %v", is)
	}
	if is := enforce(candidate("1.3.9", func(*registry.Content) {}, nil)); !hasIssue(is, "semver") {
		t.Errorf("lower version accepted: %v", is)
	}
	// numeric, not lexical, ordering: 1.10.0 follows 1.9.0
	b9 := version("1.9.0", registrytest.Content())
	v10 := version("1.10.0", registrytest.Content())
	if is := registry.Classify(b9, v10).Enforce(b9, v10); len(is) != 0 {
		t.Errorf("1.10.0 refused after 1.9.0: %v", is)
	}
	// a deprecated operation needs a future supported-until date and a migration plan
	dep := func(c *registry.Content) {
		o := op(c, "payment-status")
		o.Deprecated = true
		d := base.EffectiveFrom.Add(-time.Hour) // before the version even takes effect
		o.SupportedUntil = &d
	}
	if is := enforce(candidate("1.5.0", dep, withPlan)); !hasIssue(is, "supported_until") {
		t.Errorf("deprecation with a past sunset accepted: %v", is)
	}
	dep2 := func(c *registry.Content) {
		o := op(c, "payment-status")
		o.Deprecated = true
		d := registrytest.T0.Add(300 * 24 * time.Hour)
		o.SupportedUntil = &d
	}
	if is := enforce(candidate("1.5.0", dep2, nil)); !hasIssue(is, "migration_plan_ref") {
		t.Errorf("deprecation without an inventory/migration plan accepted: %v", is)
	}
	if is := enforce(candidate("1.5.0", dep2, withPlan)); len(is) != 0 {
		t.Errorf("good deprecation refused: %v", is)
	}
}

func TestSchemaReferenceMeansOneThing(t *testing.T) {
	// the schema reference statement@1 cannot be silently redefined by a later contract: that is "implicit
	// fallback" by another name (§6.2). Caught at validation; see service tests for the end-to-end refusal.
	a := registrytest.Content()
	b := registrytest.Clone(a)
	schemaOf(&b, "statement@1").MaxSizeBytes = 2048
	if registry.SchemaHash(schemaOf(&a, "statement@1")) == registry.SchemaHash(schemaOf(&b, "statement@1")) {
		t.Fatal("schema hash ignores the size limit")
	}
	c := registrytest.Clone(a)
	schemaOf(&c, "statement@1").ValidationProfile = json.RawMessage(strings.ReplaceAll(string(schemaOf(&c, "statement@1").ValidationProfile), "\n", " "))
	// whitespace in the schema document is not a change of meaning
	ha, hc := registry.SchemaHash(schemaOf(&a, "statement@1")), registry.SchemaHash(schemaOf(&c, "statement@1"))
	if ha != hc {
		t.Fatal("whitespace changed the schema hash")
	}
}
