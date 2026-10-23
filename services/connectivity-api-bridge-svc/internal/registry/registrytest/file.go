package registrytest

import (
	"encoding/json"
	"sort"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
)

// FileColumns are the columns of the reference statement file.
var FileColumns = []string{"txn_id", "account", "balance", "currency", "booked_at"}

const fileRowSchema = `{"type":"object","required":["txn_id","account","balance","currency","booked_at"],
 "properties":{"txn_id":{"type":"string"},"account":{"type":"string"},"balance":{"type":"string"},
 "currency":{"type":"string","enum":["GBP","EUR","USD"]},"booked_at":{"type":"string"}}}`

// WithFileOperation adds to a contract a CSV statement file operation: delivered as a batch over the HTTPS object
// channel (OBJECT), authenticated by the hmac-in profile (the manifest is what is signed), recognised row by row by
// txn_id, limited and named by a BATCH profile, and mapped by the same canonical mapping the statement webhook uses.
// The certification profile is extended with the negative tests a file operation obliges it to list.
func WithFileOperation(c registry.Content) registry.Content {
	cols, _ := json.Marshal(FileColumns)
	c.Schemas = append(c.Schemas, registry.Schema{Ref: "stmt-csv@1", Direction: registry.Inbound, MediaType: "text/csv", MaxSizeBytes: 1 << 20,
		ValidationProfile: raw(`{"format":"csv","columns":` + string(cols) + `,"delimiter":",","header":true,"row_schema":` + fileRowSchema + `}`)})
	c.Profiles = append(c.Profiles,
		registry.Profile{Kind: registry.ProfileIdempotency, Name: "idem-file", Spec: raw(`{"key_sources":["body:/txn_id"],"scope":"CONNECTION","retention_days":30,"on_body_change":"CONFLICT"}`)},
		registry.Profile{Kind: registry.ProfileBatch, Name: "batch-stmt", Spec: raw(`{"file_name_pattern":"^stmt-[0-9]{4}\\.csv(\\.gz)?$","compression":["gzip"],"max_files":5,"max_rows":1000,
 "max_decompressed_bytes":1048576,"max_expansion_ratio":20,"checksum_algorithm":"SHA-256","require_record_count":true}`)})
	c.Operations = append(c.Operations, registry.Operation{ID: "statement-file", Direction: registry.Inbound, Channel: "OBJECT",
		AuthProfile: "hmac-in", IdempotencyProfile: "idem-file", BatchProfile: "batch-stmt", RequestSchema: "stmt-csv@1",
		MaxPayloadBytes: 1 << 20, RateLimitPerMinute: 60, DataClasses: []string{"FINANCIAL"}, AllowedRegions: []string{"uk"}})
	c.Mappings = append(c.Mappings, registry.MappingDef{Key: "stmt-csv", Version: 1, SourceSchema: "stmt-csv@1", CanonicalContract: "bank.statement.v1", Spec: raw(MappingSpec)})
	extendCertification(&c)
	return c
}

// extendCertification lists, in the certification profile, every negative test the contract's operations oblige it to.
func extendCertification(c *registry.Content) {
	need := registry.RequiredNegativeTests(c)
	for i := range c.Profiles {
		if c.Profiles[i].Kind != registry.ProfileCertification {
			continue
		}
		var spec map[string]any
		if json.Unmarshal(c.Profiles[i].Spec, &spec) != nil {
			continue
		}
		have := map[string]bool{}
		var list []string
		if cur, ok := spec["required_negative_tests"].([]any); ok {
			for _, v := range cur {
				if s, ok := v.(string); ok && !have[s] {
					have[s] = true
					list = append(list, s)
				}
			}
		}
		for _, id := range need {
			if !have[id] {
				list = append(list, id)
			}
		}
		sort.Strings(list)
		spec["required_negative_tests"] = list
		b, _ := json.Marshal(spec)
		c.Profiles[i].Spec = b
	}
}

// PassedFor is the negative-test attestation detail that satisfies the certification profile of c: every test the
// profile lists and every test the contract's operations oblige it to.
func PassedFor(c registry.Content) json.RawMessage {
	have := map[string]bool{}
	var list []string
	add := func(id string) {
		if !have[id] {
			have[id] = true
			list = append(list, id)
		}
	}
	for _, id := range registry.RequiredNegativeTests(&c) {
		add(id)
	}
	for _, p := range c.Profiles {
		if p.Kind != registry.ProfileCertification {
			continue
		}
		var spec struct {
			Required []string `json:"required_negative_tests"`
		}
		if json.Unmarshal(p.Spec, &spec) == nil {
			for _, id := range spec.Required {
				add(id)
			}
		}
	}
	sort.Strings(list)
	b, _ := json.Marshal(map[string]any{"test_pack_version": "tp-2026.10", "passed": list})
	return b
}

// SFTPHost is the provider's SFTP host in the reference contract.
const SFTPHost = "files.bank-x.com"

// WithSFTPPull adds, on top of WithFileOperation, an inbound SFTP PULL operation: the platform connects to the
// provider's server (pinned by hostKey, a SHA256:<base64> fingerprint), logs in with a vault credential (AUTH profile
// sftp-pw) and collects batches into the same pipeline an HTTPS delivery goes through.
func WithSFTPPull(c registry.Content, hostKeys ...string) registry.Content {
	keys, _ := json.Marshal(hostKeys)
	c.Profiles = append(c.Profiles,
		registry.Profile{Kind: registry.ProfileEndpoint, Name: "sftp-ep", Spec: raw(`{"schemes":["sftp"],"hosts":["` + SFTPHost + `"],"ports":[22],"regions":["uk"],"redirects":"NONE","host_keys":` + string(keys) + `}`)},
		registry.Profile{Kind: registry.ProfileAuth, Name: "sftp-pw", Spec: raw(`{"mechanism":"SFTP_PASSWORD","credential_class":"PROVIDER_CREDENTIAL"}`)})
	c.Operations = append(c.Operations, registry.Operation{ID: "statement-sftp", Direction: registry.Inbound, Channel: "SFTP",
		AuthProfile: "sftp-pw", IdempotencyProfile: "idem-file", BatchProfile: "batch-stmt", EndpointProfile: "sftp-ep", RequestSchema: "stmt-csv@1",
		MaxPayloadBytes: 1 << 20, RateLimitPerMinute: 60, DataClasses: []string{"FINANCIAL"}, AllowedRegions: []string{"uk"}})
	extendCertification(&c)
	return c
}
