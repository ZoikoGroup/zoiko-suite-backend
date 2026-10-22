// Package registrytest holds a reference contract for tests of the registry, the handlers and the Postgres
// store. It is not imported by production code.
package registrytest

import (
	"encoding/json"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// T0 is the fixed "now" the reference contract is dated from.
var T0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

const statementSchema = `{"type":"object","required":["event_id","account","balance","booked_at"],
 "properties":{"event_id":{"type":"string"},"account":{"type":"string"},"balance":{"type":"string"},
 "currency":{"type":"string","enum":["GBP","EUR","USD"]},"booked_at":{"type":"string"}}}`

// MappingSpec is the reference mapping.
const MappingSpec = `{"canonical_contract":"bank.statement.v1","fields":[
 {"to":"/account","from":"/account","type":"string","required":true,"max_length":64},
 {"to":"/balance","from":"/balance","type":"decimal","scale":2,"required":true},
 {"to":"/currency","from":"/currency","type":"enum","required":true,"enum":{"GBP":"GBP","EUR":"EUR","USD":"USD"}},
 {"to":"/booked_at","from":"/booked_at","type":"datetime","required":true}],
 "prohibited_source_paths":["/holder/national_id"]}`

// Content returns a valid reference contract: an inbound statement webhook, an irreversible outbound payment with
// a callback, and a read-only status query.
func Content() registry.Content {
	return registry.Content{
		Schemas: []registry.Schema{
			{Ref: "statement@1", Direction: registry.Inbound, MediaType: "application/json", MaxSizeBytes: 1 << 20, ValidationProfile: raw(statementSchema)},
			{Ref: "payment-req@1", Direction: registry.Outbound, MediaType: "application/json", MaxSizeBytes: 1 << 16,
				ValidationProfile: raw(`{"type":"object","required":["amount","currency"],"properties":{"amount":{"type":"string"},"currency":{"type":"string"}}}`)},
			{Ref: "payment-resp@1", Direction: registry.Inbound, MediaType: "application/json", MaxSizeBytes: 1 << 16,
				ValidationProfile: raw(`{"type":"object","required":["payment_id"],"properties":{"payment_id":{"type":"string"},"status":{"type":"string"}}}`)},
			{Ref: "payment-cb@1", Direction: registry.Inbound, MediaType: "application/json", MaxSizeBytes: 1 << 16,
				ValidationProfile: raw(`{"type":"object","required":["event_id","payment_id","status"],"properties":{"event_id":{"type":"string"},"payment_id":{"type":"string"},"status":{"type":"string"}}}`)},
			{Ref: "status-resp@1", Direction: registry.Inbound, MediaType: "application/json", MaxSizeBytes: 1 << 16,
				ValidationProfile: raw(`{"type":"object","required":["status"],"properties":{"status":{"type":"string"}}}`)},
		},
		Profiles: []registry.Profile{
			{Kind: registry.ProfileAuth, Name: "oauth", Spec: raw(`{"mechanism":"OAUTH2_CLIENT_CREDENTIALS","audience":"https://api.bank-x.com","scopes_required":["payments.write","payments.read"],"credential_class":"PROVIDER_CREDENTIAL"}`)},
			{Kind: registry.ProfileAuth, Name: "hmac-in", Spec: raw(`{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","credential_class":"CALLBACK_SECRET"}`)},
			{Kind: registry.ProfileEndpoint, Name: "bank-api", Spec: raw(`{"schemes":["https"],"hosts":["api.bank-x.com"],"ports":[443],"regions":["uk"],"redirects":"NONE"}`)},
			{Kind: registry.ProfileIdempotency, Name: "idem-in", Spec: raw(`{"key_sources":["header:Idempotency-Key","body:/event_id"],"scope":"CONNECTION","retention_days":30,"on_body_change":"CONFLICT"}`)},
			{Kind: registry.ProfileCallback, Name: "cb", Spec: raw(`{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","replay_window_seconds":300,"timestamp_header":"X-Timestamp","dedupe_key_source":"body:/event_id","schema":"payment-cb@1","tenant_binding":"CONNECTION","transitions":{"SUBMITTING":["PENDING","SETTLED","FAILED"],"PENDING":["SETTLED","FAILED"],"UNKNOWN":["SETTLED","FAILED","PENDING"]}}`)},
			{Kind: registry.ProfileCertification, Name: "cert", Spec: raw(`{"test_pack_version":"tp-2026.10","required_negative_tests":["NP-01","NP-02","NP-05","NP-06","NP-08","NP-17","NP-19","NP-22","NP-23","NP-26","NP-28","NP-29"],"expiry_days":180,"retest_triggers":["MAJOR_VERSION","AUTH_CHANGE","MAPPING_CHANGE","ENDPOINT_CHANGE"]}`)},
		},
		Operations: []registry.Operation{
			{ID: "statement-push", Direction: registry.Inbound, Channel: "WEBHOOK", Method: "POST", EndpointTemplate: "/hooks/statement",
				AuthProfile: "hmac-in", IdempotencyProfile: "idem-in", RequestSchema: "statement@1",
				MaxPayloadBytes: 1 << 20, RateLimitPerMinute: 600, DataClasses: []string{"FINANCIAL"}, AllowedRegions: []string{"uk"}},
			{ID: "payment-submit", Direction: registry.Outbound, Channel: "HTTPS", Method: "POST", EndpointTemplate: "/v1/payments",
				AuthProfile: "oauth", EndpointProfile: "bank-api", CallbackProfile: "cb", RequestSchema: "payment-req@1", ResponseSchema: "payment-resp@1",
				Irreversible:         true,
				ProviderIdempotency:  registry.ProviderIdempotency{Supported: true, Header: "Idempotency-Key"},
				UnknownResolution:    registry.UnknownResolution{StatusQueryOperation: "payment-status", ViaCallback: true},
				ResubmitAfterUnknown: true,
				MaxPayloadBytes:      1 << 16, RateLimitPerMinute: 120, DataClasses: []string{"FINANCIAL", "PERSONAL"}, AllowedRegions: []string{"uk"}},
			{ID: "payment-status", Direction: registry.Outbound, Channel: "HTTPS", Method: "GET", EndpointTemplate: "/v1/payments/status",
				AuthProfile: "oauth", EndpointProfile: "bank-api", ResponseSchema: "status-resp@1",
				MaxPayloadBytes: 1 << 16, RateLimitPerMinute: 600, DataClasses: []string{"FINANCIAL"}, AllowedRegions: []string{"uk"}},
		},
		Mappings: []registry.MappingDef{
			{Key: "statement", Version: 1, SourceSchema: "statement@1", CanonicalContract: "bank.statement.v1", Spec: raw(MappingSpec)},
		},
	}
}

// Input returns a VersionInput around Content for the given version.
func Input(semver string) registry.VersionInput {
	return registry.VersionInput{
		Semver: semver, EffectiveFrom: T0.Add(24 * time.Hour), RollbackRef: "RUNBOOK-123 rollback to previous contract",
		Content: Content(),
	}
}

// Clone deep-copies a Content so a test can mutate it.
func Clone(c registry.Content) registry.Content {
	b, _ := json.Marshal(c)
	var out registry.Content
	_ = json.Unmarshal(b, &out)
	return out
}

// Passed is the negative-test attestation detail that satisfies the reference certification profile.
func Passed() json.RawMessage {
	return raw(`{"test_pack_version":"tp-2026.10","passed":["NP-01","NP-02","NP-05","NP-06","NP-08","NP-17","NP-19","NP-22","NP-23","NP-26","NP-28","NP-29"]}`)
}
