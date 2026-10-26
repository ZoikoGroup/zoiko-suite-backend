package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry/mapping"
	"zoiko.io/eventing/adapter"
)

// Limits are the platform ceilings contracts are validated against. They are configuration, not constants:
// ZS-SVC-T-001 §19.1 leaves their values open (OD-04 payload sizes, OD-06 idempotency retention, OD-11
// certification cadence), so each is an explicit knob with a safe default.
type Limits struct {
	MaxPayloadBytes      int64 // OD-04: ceiling for a schema's max size and an operation's max payload
	IdempotencyMinDays   int   // OD-06
	IdempotencyMaxDays   int
	CertificationMaxDays int // OD-11: longest a certification may be valid
}

// DefaultLimits are the defaults used when nothing is configured.
func DefaultLimits() Limits {
	return Limits{MaxPayloadBytes: 25 << 20, IdempotencyMinDays: 1, IdempotencyMaxDays: 3650, CertificationMaxDays: 365}
}

var (
	semverRE    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	opIDRE      = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,62}$`)
	nameRE      = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,62}$`)
	schemaRefRE = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,62}@[1-9][0-9]{0,5}$`)
	pointerRE   = regexp.MustCompile(`^(/([^/~]|~[01])*)+$`)
	headerRE    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,62}$`)
	hostRE      = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	regionRE    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	npRE        = regexp.MustCompile(`^NP-(0[1-9]|[1-4][0-9]|5[0-2])$`)
	refRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _./:#@-]{2,254}$`)
)

var mediaTypes = map[string]bool{
	"application/json": true, "application/x-ndjson": true, "application/xml": true, "text/xml": true,
	"text/csv": true, "application/pdf": true, "application/octet-stream": true,
}

var dataClasses = map[string]bool{
	"PUBLIC": true, "INTERNAL": true, "CONFIDENTIAL": true, "RESTRICTED": true,
	"PERSONAL": true, "FINANCIAL": true, "TAX": true,
}

var httpMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// ValidateHeader checks the fields a contract version carries outside its content.
func ValidateHeader(v *ContractVersion) []Issue {
	var is []Issue
	add := func(p, r string) { is = append(is, Issue{p, r}) }
	if !semverRE.MatchString(v.Semver) {
		add("semver", "must be MAJOR.MINOR.PATCH")
	}
	if v.EffectiveFrom.IsZero() {
		add("effective_from", "required")
	}
	if v.SupportedUntil != nil && !v.SupportedUntil.After(v.EffectiveFrom) {
		add("supported_until", "must be after effective_from")
	}
	for p, ref := range map[string]string{"migration_plan_ref": v.MigrationPlanRef, "replay_analysis_ref": v.ReplayAnalysisRef, "rollback_ref": v.RollbackRef} {
		if ref != "" && !refRE.MatchString(ref) {
			add(p, "must be a document or ticket reference")
		}
	}
	return is
}

// ValidateContent checks everything a contract version contains against the rules of §6 and §15. It returns
// every issue found (never just the first), so an author fixes a contract in one pass.
func ValidateContent(c *Content, l Limits) []Issue {
	var is []Issue
	add := func(p, r string) { is = append(is, Issue{p, r}) }

	if len(c.Operations) == 0 {
		add("operations", "at least one operation is required")
	}
	if len(c.Operations) > 256 || len(c.Schemas) > 256 || len(c.Profiles) > 256 || len(c.Mappings) > 256 {
		add("content", "too many entries (limit 256 per collection)")
		return is
	}

	// ── schemas ──
	schemas := map[string]*Schema{}
	for i := range c.Schemas {
		s := &c.Schemas[i]
		at := fmt.Sprintf("schemas[%d]", i)
		if !schemaRefRE.MatchString(s.Ref) {
			add(at+".schema", "must be id@version with an exact positive version")
			continue
		}
		if _, dup := schemas[s.Ref]; dup {
			add(at+".schema", "duplicate schema reference")
			continue
		}
		schemas[s.Ref] = s
		if s.Direction != Inbound && s.Direction != Outbound {
			add(at+".direction", "must be INBOUND or OUTBOUND")
		}
		if !mediaTypes[s.MediaType] {
			add(at+".media_type", "not a supported media type")
		}
		if s.MaxSizeBytes < 1 || s.MaxSizeBytes > l.MaxPayloadBytes {
			add(at+".max_size_bytes", "must be between 1 and the platform ceiling")
		}
		validateProfileDoc(at, s, &is)
	}

	// ── profiles ──
	byKind := map[string]map[string]*Profile{}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		at := fmt.Sprintf("profiles[%d]", i)
		if !nameRE.MatchString(p.Name) {
			add(at+".name", "lowercase letters, digits, hyphen and underscore")
			continue
		}
		if byKind[p.Kind] == nil {
			byKind[p.Kind] = map[string]*Profile{}
		}
		if _, dup := byKind[p.Kind][p.Name]; dup {
			add(at+".name", "duplicate profile name for this kind")
			continue
		}
		byKind[p.Kind][p.Name] = p
		switch p.Kind {
		case ProfileAuth:
			validateAuth(at, p.Spec, &is)
		case ProfileEndpoint:
			validateEndpoint(at, p.Spec, &is)
		case ProfileIdempotency:
			validateIdempotency(at, p.Spec, l, &is)
		case ProfileCallback:
			validateCallback(at, p.Spec, schemas, &is)
		case ProfileCertification:
			validateCertification(at, p.Spec, l, &is)
		case ProfileBatch:
			validateBatch(at, p.Spec, &is)
		default:
			add(at+".kind", "not a supported profile kind")
		}
	}

	// a callback is authenticated with a credential of its own: the AUTH profile it names must exist and use the
	// same mechanism as the callback (I-16)
	for i := range c.Profiles {
		p := &c.Profiles[i]
		if p.Kind != ProfileCallback {
			continue
		}
		var cb callbackSpec
		if json.Unmarshal(p.Spec, &cb) != nil || cb.AuthProfile == "" {
			continue
		}
		ap, ok := byKind[ProfileAuth][cb.AuthProfile]
		if !ok {
			add(fmt.Sprintf("profiles[%d].spec.auth_profile", i), "names no AUTH profile of this contract")
			continue
		}
		var a authSpec
		if json.Unmarshal(ap.Spec, &a) == nil && a.Mechanism != cb.Mechanism && !(cb.Mechanism == "OAUTH2" && strings.HasPrefix(a.Mechanism, "OAUTH2")) {
			add(fmt.Sprintf("profiles[%d].spec.auth_profile", i), "the AUTH profile's mechanism must be the callback's mechanism")
		}
	}

	// ── operations ──
	ops := map[string]*Operation{}
	for i := range c.Operations {
		o := &c.Operations[i]
		if opIDRE.MatchString(o.ID) {
			if _, dup := ops[o.ID]; dup {
				add(fmt.Sprintf("operations[%d].operation_id", i), "duplicate operation")
			}
			ops[o.ID] = o
		}
	}
	for i := range c.Operations {
		validateOperation(fmt.Sprintf("operations[%d]", i), &c.Operations[i], ops, schemas, byKind, l, &is)
	}

	// ── mappings ──
	seenMap := map[string]bool{}
	mapped := map[string]bool{} // source schema refs that have a mapping
	for i := range c.Mappings {
		m := &c.Mappings[i]
		at := fmt.Sprintf("mappings[%d]", i)
		if !nameRE.MatchString(m.Key) {
			add(at+".mapping_key", "lowercase letters, digits, hyphen and underscore")
		}
		if m.Version < 1 {
			add(at+".version", "must be positive")
		}
		id := fmt.Sprintf("%s@%d", m.Key, m.Version)
		if seenMap[id] {
			add(at+".mapping_key", "duplicate mapping version")
		}
		seenMap[id] = true
		src, ok := schemas[m.SourceSchema]
		switch {
		case !ok:
			add(at+".source_schema", "does not name a schema in this contract")
		case src.Direction != Inbound:
			add(at+".source_schema", "a mapping's source schema is an inbound schema")
		default:
			mapped[m.SourceSchema] = true
		}
		if strings.TrimSpace(m.CanonicalContract) == "" || len(m.CanonicalContract) > 128 {
			add(at+".canonical_contract_ref", "required")
		}
		comp, err := mapping.CompileJSON(m.Spec)
		if err != nil {
			add(at+".spec", "mapping specification is invalid")
		} else if comp.Spec().CanonicalContract != m.CanonicalContract {
			add(at+".spec.canonical_contract", "must equal canonical_contract_ref")
		}
	}
	// every inbound operation that carries a body has a mapping into the canonical contract: the platform
	// never guesses one (I-09)
	for i := range c.Operations {
		o := &c.Operations[i]
		if o.Direction == Inbound && o.RequestSchema != "" && !mapped[o.RequestSchema] {
			add(fmt.Sprintf("operations[%d].request_schema", i), "no mapping version consumes this schema")
		}
	}

	// ── the certification profile and the negative tests the operations oblige it to list ──
	cert := byKind[ProfileCertification]
	if len(cert) != 1 {
		add("profiles", "exactly one CERTIFICATION profile is required")
	} else {
		for _, p := range cert {
			var cs certSpec
			if json.Unmarshal(p.Spec, &cs) == nil {
				have := map[string]bool{}
				for _, id := range cs.RequiredNegativeTests {
					have[id] = true
				}
				for _, id := range RequiredNegativeTests(c) {
					if !have[id] {
						add("profiles."+p.Name+".required_negative_tests", id+" is required by this contract's operations")
					}
				}
			}
		}
	}
	sort.Slice(is, func(a, b int) bool {
		if is[a].Path != is[b].Path {
			return is[a].Path < is[b].Path
		}
		return is[a].Rule < is[b].Rule
	})
	return is
}

func validateProfileDoc(at string, s *Schema, is *[]Issue) {
	add := func(p, r string) { *is = append(*is, Issue{at + p, r}) }
	switch s.MediaType {
	case "application/json", "application/x-ndjson":
		if _, err := adapter.CompileSchema(s.ValidationProfile); err != nil {
			add(".validation_profile", "not a valid schema in the supported subset")
		}
	case "text/csv":
		var p struct {
			Format    string          `json:"format"`
			Columns   []string        `json:"columns"`
			Delimiter string          `json:"delimiter,omitempty"`
			Header    *bool           `json:"header,omitempty"`
			RowSchema json.RawMessage `json:"row_schema,omitempty"`
		}
		if json.Unmarshal(s.ValidationProfile, &p) != nil || p.Format != "csv" || len(p.Columns) == 0 {
			add(".validation_profile", "a csv schema declares format \"csv\" and its columns")
			return
		}
		if len(p.Columns) > 512 {
			add(".validation_profile.columns", "at most 512 columns")
		}
		seen := map[string]bool{}
		for _, c := range p.Columns {
			if !columnRE.MatchString(c) || seen[c] {
				add(".validation_profile.columns", "column names are unique identifiers (letters, digits, underscore)")
			}
			seen[c] = true
		}
		if p.Delimiter != "" && (len(p.Delimiter) != 1 || strings.ContainsAny(p.Delimiter, "\"\r\n") || p.Delimiter[0] > 126 || p.Delimiter[0] < 9) {
			add(".validation_profile.delimiter", "one printable character")
		}
		if len(p.RowSchema) > 0 {
			if _, err := adapter.CompileSchema(p.RowSchema); err != nil {
				add(".validation_profile.row_schema", "not a valid schema in the supported subset")
			}
		}
	case "application/xml", "text/xml":
		var p struct {
			Format      string `json:"format"`
			RootElement string `json:"root_element"`
		}
		if json.Unmarshal(s.ValidationProfile, &p) != nil || p.Format != "xml" || p.RootElement == "" {
			add(".validation_profile", "an xml schema declares format \"xml\" and its root element")
		}
	default:
		var p struct {
			Format string `json:"format"`
		}
		if json.Unmarshal(s.ValidationProfile, &p) != nil || p.Format == "" {
			add(".validation_profile", "a binary schema declares its format")
		}
	}
}

// ── per-kind profile specs ───────────────────────────────────────────────────

type authSpec struct {
	Mechanism          string   `json:"mechanism"`
	Audience           string   `json:"audience,omitempty"`
	ScopesRequired     []string `json:"scopes_required,omitempty"`
	SignatureAlgorithm string   `json:"signature_algorithm,omitempty"`
	CredentialClass    string   `json:"credential_class"`
	SubjectBinding     string   `json:"subject_binding,omitempty"`

	// How an INBOUND request carrying this mechanism is verified. Each is optional; the gateway applies the documented
	// default when absent, and a value that is present is validated here.
	SignatureHeader     string `json:"signature_header,omitempty"`
	SignatureEncoding   string `json:"signature_encoding,omitempty"` // hex | base64
	SignedContent       string `json:"signed_content,omitempty"`     // BODY | TIMESTAMP_DOT_BODY
	TimestampHeader     string `json:"timestamp_header,omitempty"`
	TimestampFormat     string `json:"timestamp_format,omitempty"` // UNIX_SECONDS | RFC3339
	ReplayWindowSeconds int    `json:"replay_window_seconds,omitempty"`
	APIKeyHeader        string `json:"api_key_header,omitempty"`
}

var authMechanisms = map[string]bool{
	"OAUTH2_CLIENT_CREDENTIALS": true, "OAUTH2_AUTH_CODE": true, "MTLS": true, "HMAC": true, "API_KEY_HEADER": true,
	"SFTP_PASSWORD": true, "SFTP_KEY": true,
}
var hmacAlgorithms = map[string]bool{"HMAC-SHA256": true, "HMAC-SHA384": true, "HMAC-SHA512": true}

func strictDecode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing content")
	}
	return nil
}

func validateAuth(at string, raw json.RawMessage, is *[]Issue) {
	add := func(p, r string) { *is = append(*is, Issue{at + ".spec" + p, r}) }
	var a authSpec
	if err := strictDecode(raw, &a); err != nil {
		add("", "not a valid AUTH specification")
		return
	}
	if !authMechanisms[a.Mechanism] {
		add(".mechanism", "not a supported mechanism")
		return
	}
	if !regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,47}$`).MatchString(a.CredentialClass) {
		add(".credential_class", "names the vault credential class")
	}
	switch a.Mechanism {
	case "OAUTH2_CLIENT_CREDENTIALS", "OAUTH2_AUTH_CODE":
		// the token's audience and scopes are verified (NP-03) and the scopes are the least needed
		if strings.TrimSpace(a.Audience) == "" {
			add(".audience", "an OAuth profile verifies the token audience")
		}
		if len(a.ScopesRequired) == 0 {
			add(".scopes_required", "an OAuth profile lists the scopes it needs, and no more")
		}
	case "HMAC":
		if !hmacAlgorithms[a.SignatureAlgorithm] {
			add(".signature_algorithm", "HMAC-SHA256, HMAC-SHA384 or HMAC-SHA512")
		}
		validateInboundAuthFields(a, add)
	case "API_KEY_HEADER":
		if a.APIKeyHeader != "" && !headerRE.MatchString(a.APIKeyHeader) {
			add(".api_key_header", "a header name")
		}
	case "MTLS":
		// a certificate is bound to one connection, never accepted for any tenant (NP-04)
		if a.SubjectBinding != "CONNECTION" {
			add(".subject_binding", "an mTLS profile binds the certificate to one connection")
		}
	}
}

type endpointSpec struct {
	Schemes               []string `json:"schemes"`
	Hosts                 []string `json:"hosts"`
	Ports                 []int    `json:"ports,omitempty"`
	Regions               []string `json:"regions"`
	Redirects             string   `json:"redirects"`
	ApprovedRedirectHosts []string `json:"approved_redirect_hosts,omitempty"`
	PrivateEndpoint       bool     `json:"private_endpoint,omitempty"`
	// HostKeys are the SHA-256 fingerprints (SHA256:base64) of the provider's SSH host keys. An SFTP endpoint is
	// never trusted on first use: a connection to a host whose key is not listed is refused.
	HostKeys []string `json:"host_keys,omitempty"`
	Failover []struct {
		Host   string `json:"host"`
		Region string `json:"region"`
	} `json:"failover,omitempty"`
}

func validHost(h string) bool {
	if net.ParseIP(h) != nil { // an address is never a registered destination: names resolve under egress policy
		return false
	}
	h = strings.ToLower(h)
	return hostRE.MatchString(h) && h != "localhost" && !strings.HasSuffix(h, ".localhost") &&
		!strings.HasSuffix(h, ".internal") && !strings.HasSuffix(h, ".local")
}

func validateEndpoint(at string, raw json.RawMessage, is *[]Issue) {
	add := func(p, r string) { *is = append(*is, Issue{at + ".spec" + p, r}) }
	var e endpointSpec
	if err := strictDecode(raw, &e); err != nil {
		add("", "not a valid ENDPOINT specification")
		return
	}
	if len(e.Schemes) == 0 {
		add(".schemes", "required")
	}
	for _, s := range e.Schemes {
		if s != "https" && s != "sftp" {
			add(".schemes", "only https and sftp are permitted")
		}
	}
	if len(e.Hosts) == 0 || len(e.Hosts) > 32 {
		add(".hosts", "between 1 and 32 hosts")
	}
	if contains(e.Schemes, "sftp") {
		if len(e.HostKeys) == 0 || len(e.HostKeys) > 16 {
			add(".host_keys", "an sftp endpoint pins the provider's host keys (SHA256:<base64>); it is never trusted on first use")
		}
		for _, k := range e.HostKeys {
			if !hostKeyRE.MatchString(k) {
				add(".host_keys", "a host key fingerprint is SHA256: followed by the base64 digest")
			}
		}
	} else if len(e.HostKeys) > 0 {
		add(".host_keys", "only valid on an sftp endpoint")
	}
	for _, h := range e.Hosts {
		if !validHost(h) {
			add(".hosts", "must be a public DNS name, never an address, localhost or an internal name")
		}
	}
	for _, p := range e.Ports {
		if p < 1 || p > 65535 {
			add(".ports", "not a port")
		}
	}
	if len(e.Regions) == 0 {
		add(".regions", "the provider's data regions are required")
	}
	regions := map[string]bool{}
	for _, r := range e.Regions {
		if !regionRE.MatchString(r) {
			add(".regions", "not a region code")
		}
		regions[r] = true
	}
	switch e.Redirects {
	case "NONE", "SAME_HOST":
		if len(e.ApprovedRedirectHosts) > 0 {
			add(".approved_redirect_hosts", "only valid with redirects APPROVED_HOSTS")
		}
	case "APPROVED_HOSTS":
		if len(e.ApprovedRedirectHosts) == 0 {
			add(".approved_redirect_hosts", "required with redirects APPROVED_HOSTS")
		}
		for _, h := range e.ApprovedRedirectHosts {
			if !validHost(h) {
				add(".approved_redirect_hosts", "must be a public DNS name")
			}
		}
	default:
		add(".redirects", "NONE, SAME_HOST or APPROVED_HOSTS: redirects are never implicit")
	}
	// a failover endpoint is pre-approved for the same data regions as the primary: a region failover that
	// leaves the residency boundary is never silent (NP-30, OD-15)
	for _, f := range e.Failover {
		if !validHost(f.Host) {
			add(".failover", "failover host must be a public DNS name")
		}
		if !regions[f.Region] {
			add(".failover", "failover region is outside the endpoint's approved regions")
		}
	}
}

type idemSpec struct {
	KeySources    []string `json:"key_sources"`
	Scope         string   `json:"scope"`
	RetentionDays int      `json:"retention_days"`
	OnBodyChange  string   `json:"on_body_change"`
}

func validateIdempotency(at string, raw json.RawMessage, l Limits, is *[]Issue) {
	add := func(p, r string) { *is = append(*is, Issue{at + ".spec" + p, r}) }
	var s idemSpec
	if err := strictDecode(raw, &s); err != nil {
		add("", "not a valid IDEMPOTENCY specification")
		return
	}
	if len(s.KeySources) == 0 {
		add(".key_sources", "at least one key source")
	}
	for _, k := range s.KeySources {
		switch {
		case k == "provider_event_id":
		case strings.HasPrefix(k, "header:") && headerRE.MatchString(strings.TrimPrefix(k, "header:")):
		case strings.HasPrefix(k, "body:") && pointerRE.MatchString(strings.TrimPrefix(k, "body:")):
		default:
			add(".key_sources", "header:<Name>, body:</pointer> or provider_event_id")
		}
	}
	if s.Scope != "CONNECTION" && s.Scope != "TENANT" {
		add(".scope", "CONNECTION or TENANT: a key is never global")
	}
	if s.RetentionDays < l.IdempotencyMinDays || s.RetentionDays > l.IdempotencyMaxDays {
		add(".retention_days", fmt.Sprintf("between %d and %d days", l.IdempotencyMinDays, l.IdempotencyMaxDays))
	}
	// the same key with a different body is never "the same event" (NP-07)
	if s.OnBodyChange != "CONFLICT" && s.OnBodyChange != "QUARANTINE" {
		add(".on_body_change", "CONFLICT or QUARANTINE")
	}
}

type callbackSpec struct {
	Mechanism           string              `json:"mechanism"`
	SignatureAlgorithm  string              `json:"signature_algorithm,omitempty"`
	ReplayWindowSeconds int                 `json:"replay_window_seconds"`
	TimestampHeader     string              `json:"timestamp_header,omitempty"`
	DedupeKeySource     string              `json:"dedupe_key_source"`
	Schema              string              `json:"schema"`
	TenantBinding       string              `json:"tenant_binding"`
	Transitions         map[string][]string `json:"transitions"`

	// AuthProfile names the AUTH profile of this contract whose bound credential verifies the callback: the key
	// that authenticates a provider's callbacks is its own, never the one that authenticates our outbound calls.
	AuthProfile string `json:"auth_profile,omitempty"`
	// How a callback is tied to the ORIGINAL outbound attempt, and what it says. ReferencePointer and StatusPointer
	// are JSON pointers into the (schema-validated) callback body; ClientRefPointer, when present, carries the
	// domain request reference and lets a callback that beats the submit response still find its attempt (NP-24).
	ReferencePointer string `json:"reference_pointer,omitempty"`
	StatusPointer    string `json:"status_pointer,omitempty"`
	ClientRefPointer string `json:"client_ref_pointer,omitempty"`
	// Claims the provider makes about whose callback this is. They are corroboration, never authority: a
	// disagreement with the authenticated connection is a security event (NP-26).
	TenantPointer  string `json:"tenant_pointer,omitempty"`
	AccountPointer string `json:"account_pointer,omitempty"`
}

// callbackGraph is the ONLY set of outbound-attempt transitions a callback may ever drive. A contract's map is
// a subset of it: a callback cannot invent a transition (§6.2, I-17).
var callbackGraph = map[string][]string{
	"RESERVED":   {"SUBMITTING", "CANCELLED"},
	"SUBMITTING": {"PENDING", "UNKNOWN", "SETTLED", "FAILED", "CANCELLED"},
	"PENDING":    {"SETTLED", "FAILED", "CANCELLED", "UNKNOWN"},
	"UNKNOWN":    {"SETTLED", "FAILED", "PENDING"},
}

// AttemptStates are the canonical outbound attempt states (§4).
var AttemptStates = []string{"RESERVED", "SUBMITTING", "PENDING", "UNKNOWN", "SETTLED", "FAILED", "CANCELLED"}

func validateCallback(at string, raw json.RawMessage, schemas map[string]*Schema, is *[]Issue) {
	add := func(p, r string) { *is = append(*is, Issue{at + ".spec" + p, r}) }
	var c callbackSpec
	if err := strictDecode(raw, &c); err != nil {
		add("", "not a valid CALLBACK specification")
		return
	}
	switch c.Mechanism {
	case "HMAC":
		if !hmacAlgorithms[c.SignatureAlgorithm] {
			add(".signature_algorithm", "HMAC-SHA256, HMAC-SHA384 or HMAC-SHA512")
		}
		if c.TimestampHeader == "" || !headerRE.MatchString(c.TimestampHeader) {
			add(".timestamp_header", "a signed callback carries a timestamp")
		}
	case "MTLS", "OAUTH2":
		if c.SignatureAlgorithm != "" {
			add(".signature_algorithm", "only valid with mechanism HMAC")
		}
	default:
		add(".mechanism", "HMAC, MTLS or OAUTH2")
	}
	if c.ReplayWindowSeconds < 30 || c.ReplayWindowSeconds > 900 {
		add(".replay_window_seconds", "between 30 and 900")
	}
	if c.DedupeKeySource != "provider_event_id" &&
		!(strings.HasPrefix(c.DedupeKeySource, "body:") && pointerRE.MatchString(strings.TrimPrefix(c.DedupeKeySource, "body:"))) &&
		!(strings.HasPrefix(c.DedupeKeySource, "header:") && headerRE.MatchString(strings.TrimPrefix(c.DedupeKeySource, "header:"))) {
		add(".dedupe_key_source", "header:<Name>, body:</pointer> or provider_event_id")
	}
	if s, ok := schemas[c.Schema]; !ok || s.Direction != Inbound {
		add(".schema", "names an inbound schema in this contract (an unknown callback shape is rejected, never guessed)")
	}
	if c.TenantBinding != "CONNECTION" {
		add(".tenant_binding", "CONNECTION: the callback's tenant is resolved from the connection, never the payload")
	}
	if c.Mechanism == "HMAC" || c.Mechanism == "MTLS" {
		if strings.TrimSpace(c.AuthProfile) == "" || !nameRE.MatchString(c.AuthProfile) {
			add(".auth_profile", "names the AUTH profile whose credential verifies this callback")
		}
	}
	if !pointerRE.MatchString(c.ReferencePointer) {
		add(".reference_pointer", "a JSON pointer to the provider's reference in the callback body")
	}
	if !pointerRE.MatchString(c.StatusPointer) {
		add(".status_pointer", "a JSON pointer to the status in the callback body")
	}
	for name, ptr := range map[string]string{".client_ref_pointer": c.ClientRefPointer, ".tenant_pointer": c.TenantPointer, ".account_pointer": c.AccountPointer} {
		if ptr != "" && !pointerRE.MatchString(ptr) {
			add(name, "not a JSON pointer")
		}
	}
	if len(c.Transitions) == 0 {
		add(".transitions", "required")
	}
	for from, tos := range c.Transitions {
		allowed := map[string]bool{}
		for _, t := range callbackGraph[from] {
			allowed[t] = true
		}
		if _, ok := callbackGraph[from]; !ok {
			add(".transitions", "a callback cannot transition out of "+from)
			continue
		}
		for _, to := range tos {
			if !allowed[to] {
				add(".transitions", "a callback cannot drive "+from+" to "+to)
			}
		}
	}
}

type certSpec struct {
	TestPackVersion       string   `json:"test_pack_version"`
	RequiredNegativeTests []string `json:"required_negative_tests"`
	ExpiryDays            int      `json:"expiry_days"`
	RetestTriggers        []string `json:"retest_triggers"`
}

var retestTriggers = map[string]bool{
	"MAJOR_VERSION": true, "AUTH_CHANGE": true, "MAPPING_CHANGE": true, "ENDPOINT_CHANGE": true,
	"SCOPE_CHANGE": true, "CALLBACK_CHANGE": true,
}

func validateCertification(at string, raw json.RawMessage, l Limits, is *[]Issue) {
	add := func(p, r string) { *is = append(*is, Issue{at + ".spec" + p, r}) }
	var c certSpec
	if err := strictDecode(raw, &c); err != nil {
		add("", "not a valid CERTIFICATION specification")
		return
	}
	if strings.TrimSpace(c.TestPackVersion) == "" || len(c.TestPackVersion) > 64 {
		add(".test_pack_version", "required")
	}
	if len(c.RequiredNegativeTests) == 0 {
		add(".required_negative_tests", "required")
	}
	for _, id := range c.RequiredNegativeTests {
		if !npRE.MatchString(id) {
			add(".required_negative_tests", "not a negative-path scenario id (NP-01 to NP-52)")
		}
	}
	if c.ExpiryDays < 1 || c.ExpiryDays > l.CertificationMaxDays {
		add(".expiry_days", fmt.Sprintf("between 1 and %d days", l.CertificationMaxDays))
	}
	if len(c.RetestTriggers) == 0 {
		add(".retest_triggers", "required")
	}
	for _, t := range c.RetestTriggers {
		if !retestTriggers[t] {
			add(".retest_triggers", "not a supported retest trigger")
		}
	}
}

// RequiredNegativeTests is the baseline set of §16 scenarios a contract's operations oblige its certification to
// cover. A contract may list more; it may not list fewer.
func RequiredNegativeTests(c *Content) []string {
	need := map[string]bool{}
	add := func(ids ...string) {
		for _, id := range ids {
			need[id] = true
		}
	}
	for _, o := range c.Operations {
		switch {
		case o.Direction == Inbound && (o.Channel == "WEBHOOK" || o.Channel == "API" || o.Channel == "EVENT"):
			add("NP-01", "NP-05", "NP-06", "NP-08")
			if o.Channel == "WEBHOOK" {
				add("NP-02", "NP-22", "NP-23")
			}
		case o.Direction == Inbound && (o.Channel == "SFTP" || o.Channel == "OBJECT"):
			add("NP-33", "NP-34", "NP-35", "NP-36", "NP-37", "NP-38")
		case o.Direction == Outbound && o.Channel == "HTTPS":
			add("NP-28", "NP-29")
			if sideEffecting(&o) {
				add("NP-17", "NP-19")
			}
		case o.Direction == Outbound:
			add("NP-33", "NP-37")
		}
		if o.CallbackProfile != "" {
			add("NP-22", "NP-23", "NP-26")
		}
	}
	out := make([]string, 0, len(need))
	for id := range need {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sideEffecting(o *Operation) bool {
	if o.Direction != Outbound {
		return false
	}
	return o.Channel != "HTTPS" || o.Method != "GET"
}

func validateOperation(at string, o *Operation, ops map[string]*Operation, schemas map[string]*Schema,
	byKind map[string]map[string]*Profile, l Limits, is *[]Issue) {
	add := func(p, r string) { *is = append(*is, Issue{at + p, r}) }
	if !opIDRE.MatchString(o.ID) {
		add(".operation_id", "lowercase letters, digits, dot, underscore and hyphen")
	}
	switch o.Direction {
	case Inbound:
		if !inboundChannels[o.Channel] {
			add(".channel", "not an inbound channel")
		}
	case Outbound:
		if !outboundChannels[o.Channel] {
			add(".channel", "not an outbound channel")
		}
	default:
		add(".direction", "must be INBOUND or OUTBOUND")
		return
	}
	httpish := o.Channel == "HTTPS" || o.Channel == "WEBHOOK" || o.Channel == "API"
	if httpish && !httpMethods[o.Method] {
		add(".method", "an HTTP operation declares its method")
	}
	if !httpish && o.Method != "" && o.Method != "PUT" && o.Method != "GET" {
		add(".method", "a file operation is PUT or GET")
	}
	// the destination is never part of the contract text a caller can influence: only a path template (I-18)
	if o.EndpointTemplate != "" {
		t := o.EndpointTemplate
		if !strings.HasPrefix(t, "/") || strings.Contains(t, "://") || strings.ContainsAny(t, "@\\ ?#") || strings.Contains(t, "..") || strings.Contains(t, "//") {
			add(".endpoint_template", "a path template beginning with / — never a URL, host, query or fragment")
		}
	}
	if o.Direction == Outbound && o.Channel == "HTTPS" && o.EndpointTemplate == "" {
		add(".endpoint_template", "an outbound HTTPS operation declares its path")
	}
	// auth
	if _, ok := byKind[ProfileAuth][o.AuthProfile]; !ok {
		add(".auth_profile", "does not name an AUTH profile in this contract")
	}
	// endpoint: every outbound operation resolves its destination from an approved endpoint profile
	if o.Direction == Outbound {
		ep, ok := byKind[ProfileEndpoint][o.EndpointProfile]
		if !ok {
			add(".endpoint_profile", "an outbound operation names an ENDPOINT profile")
		} else if o.Channel == "HTTPS" {
			var e endpointSpec
			if json.Unmarshal(ep.Spec, &e) == nil && !contains(e.Schemes, "https") {
				add(".endpoint_profile", "an HTTPS operation needs an endpoint profile that permits https")
			}
		} else if o.Channel == "SFTP" {
			var e endpointSpec
			if json.Unmarshal(ep.Spec, &e) == nil && !contains(e.Schemes, "sftp") {
				add(".endpoint_profile", "an SFTP operation needs an endpoint profile that permits sftp")
			}
		}
	} else if o.EndpointProfile != "" {
		// the one inbound operation with a destination is an SFTP PULL: the platform connects to the provider's pinned
		// server and collects what it has published
		if ep, ok := byKind[ProfileEndpoint][o.EndpointProfile]; o.Channel != "SFTP" || !ok {
			add(".endpoint_profile", "an inbound operation has no destination (only an SFTP pull names an ENDPOINT profile)")
		} else {
			var e endpointSpec
			if json.Unmarshal(ep.Spec, &e) == nil && !contains(e.Schemes, "sftp") {
				add(".endpoint_profile", "an SFTP pull needs an endpoint profile that permits sftp")
			}
		}
	}
	validateBatchOperation(at, o, schemas, byKind, is)
	// idempotency and callbacks
	if o.IdempotencyProfile != "" {
		if _, ok := byKind[ProfileIdempotency][o.IdempotencyProfile]; !ok {
			add(".idempotency_profile", "does not name an IDEMPOTENCY profile in this contract")
		}
	} else if o.Direction == Inbound {
		add(".idempotency_profile", "an inbound operation declares how duplicates are recognized (I-08)")
	}
	if o.CallbackProfile != "" {
		if _, ok := byKind[ProfileCallback][o.CallbackProfile]; !ok {
			add(".callback_profile", "does not name a CALLBACK profile in this contract")
		}
		if o.Direction != Outbound {
			add(".callback_profile", "a callback belongs to an outbound operation")
		}
	}
	// schemas
	for _, ref := range []struct{ field, v, dir string }{
		{".request_schema", o.RequestSchema, ""}, {".response_schema", o.ResponseSchema, ""},
	} {
		if ref.v == "" {
			continue
		}
		s, ok := schemas[ref.v]
		if !ok {
			add(ref.field, "does not name a schema in this contract; there is no implicit fallback")
			continue
		}
		want := Inbound
		if o.Direction == Outbound && ref.field == ".request_schema" {
			want = Outbound
		}
		if o.Direction == Outbound && ref.field == ".response_schema" {
			want = Inbound
		}
		if o.Direction == Inbound && ref.field == ".response_schema" {
			want = Outbound
		}
		if s.Direction != want {
			add(ref.field, "schema direction does not fit this operation")
		}
	}
	// limits and classification
	if o.MaxPayloadBytes < 1 || o.MaxPayloadBytes > l.MaxPayloadBytes {
		add(".max_payload_bytes", "between 1 and the platform ceiling")
	}
	if o.RateLimitPerMinute < 1 || o.RateLimitPerMinute > 1_000_000 {
		add(".rate_limit_per_minute", "required")
	}
	if len(o.DataClasses) == 0 {
		add(".data_classes", "every operation declares what classes of data it carries")
	}
	for _, d := range o.DataClasses {
		if !dataClasses[d] {
			add(".data_classes", "not a data class")
		}
	}
	if len(o.AllowedRegions) == 0 {
		add(".allowed_regions", "every operation declares the regions its data may flow to")
	}
	for _, r := range o.AllowedRegions {
		if !regionRE.MatchString(r) {
			add(".allowed_regions", "not a region code")
		}
	}
	if o.Direction == Outbound {
		if ep, ok := byKind[ProfileEndpoint][o.EndpointProfile]; ok {
			var e endpointSpec
			if json.Unmarshal(ep.Spec, &e) == nil {
				regions := map[string]bool{}
				for _, r := range e.Regions {
					regions[r] = true
				}
				for _, r := range o.AllowedRegions {
					if !regions[r] {
						add(".allowed_regions", "region is not served by the endpoint profile")
					}
				}
			}
		}
	}
	for _, p := range []struct{ f, v string }{{".payload_tenant_pointer", o.PayloadTenantPointer}, {".payload_account_pointer", o.PayloadAccountPointer}} {
		if p.v != "" && (!pointerRE.MatchString(p.v) || o.Direction != Inbound) {
			add(p.f, "a JSON pointer, on an inbound operation only")
		}
	}
	if o.SchemaVersionHeader != "" && (!headerRE.MatchString(o.SchemaVersionHeader) || o.Direction != Inbound) {
		add(".schema_version_header", "a header name, on an inbound operation only")
	}
	// lifecycle
	if o.Deprecated && o.SupportedUntil == nil {
		add(".supported_until", "a deprecated operation declares when support ends")
	}
	if !o.Deprecated && o.SupportedUntil != nil {
		add(".supported_until", "only a deprecated operation has a supported-until date")
	}

	// ── outbound side effects: reserve, ambiguity and resolution (I-13..15, OD-07) ──
	if sideEffecting(o) {
		pi := o.ProviderIdempotency
		if pi.Supported && !headerRE.MatchString(pi.Header) {
			add(".provider_idempotency.header", "the header that carries the provider key")
		}
		if !pi.Supported && pi.Header != "" {
			add(".provider_idempotency.header", "only with supported: true")
		}
		// OD-07: an operation that cannot be undone is not published without a provider key
		if o.Irreversible && !pi.Supported {
			add(".provider_idempotency", "an irreversible operation requires provider idempotency support")
		}
		ur := o.UnknownResolution
		if ur.StatusQueryOperation == "" && !ur.ViaCallback && !ur.ViaReconciliation {
			add(".unknown_resolution", "an unknown outcome must be resolvable: status query, callback or reconciliation")
		}
		if ur.StatusQueryOperation != "" {
			q, ok := ops[ur.StatusQueryOperation]
			switch {
			case !ok:
				add(".unknown_resolution.status_query_operation", "does not name an operation in this contract")
			case q.Direction != Outbound || sideEffecting(q):
				add(".unknown_resolution.status_query_operation", "a status query is a read-only outbound operation")
			}
		}
		if ur.ViaCallback && o.CallbackProfile == "" {
			add(".unknown_resolution.via_callback", "resolution by callback needs a callback profile")
		}
		// a blind second attempt is never authorized: resubmission is only same-attempt, same-key (NP-20/21)
		if o.ResubmitAfterUnknown && !pi.Supported {
			add(".resubmit_after_unknown", "never without provider idempotency: resolve the original attempt instead")
		}
	} else {
		if o.Irreversible || o.ProviderIdempotency.Supported || o.ResubmitAfterUnknown ||
			o.UnknownResolution != (UnknownResolution{}) {
			add(".irreversible", "outbound side-effect settings on an operation that has no side effect")
		}
	}
}

// ContentHash is the fingerprint of everything that defines a contract version: its content and its
// definitional header fields, in canonical form. Two versions with the same hash mean the same thing.
func ContentHash(v *ContractVersion) (string, error) {
	def := struct {
		Semver            string     `json:"semver"`
		EffectiveFrom     time.Time  `json:"effective_from"`
		SupportedUntil    *time.Time `json:"supported_until"`
		MigrationPlanRef  string     `json:"migration_plan_ref"`
		ReplayAnalysisRef string     `json:"replay_analysis_ref"`
		RollbackRef       string     `json:"rollback_ref"`
		Content           Content    `json:"content"`
	}{v.Semver, v.EffectiveFrom.UTC(), utcPtr(v.SupportedUntil), v.MigrationPlanRef, v.ReplayAnalysisRef, v.RollbackRef, v.Content}
	canon, err := mapping.CanonicalJSON(def)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// SchemaHash is the fingerprint of a schema definition, used to prove a schema reference means the same thing in
// every contract version that names it.
func SchemaHash(s *Schema) string {
	canon, err := mapping.CanonicalJSON(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// Inbound verification defaults, applied by the gateway when a profile leaves a field out.
const (
	DefaultSignatureHeader     = "X-Signature"
	DefaultTimestampHeader     = "X-Timestamp"
	DefaultAPIKeyHeader        = "X-Api-Key"
	DefaultSignatureEncoding   = "hex"
	DefaultSignedContent       = "TIMESTAMP_DOT_BODY"
	DefaultTimestampFormat     = "UNIX_SECONDS"
	DefaultReplayWindowSeconds = 300
	DefaultSchemaVersionHeader = "X-Source-Schema-Version"
)

func validateInboundAuthFields(a authSpec, add func(p, r string)) {
	if a.SignatureHeader != "" && !headerRE.MatchString(a.SignatureHeader) {
		add(".signature_header", "a header name")
	}
	if a.SignatureEncoding != "" && a.SignatureEncoding != "hex" && a.SignatureEncoding != "base64" {
		add(".signature_encoding", "hex or base64")
	}
	if a.SignedContent != "" && a.SignedContent != "BODY" && a.SignedContent != "TIMESTAMP_DOT_BODY" {
		add(".signed_content", "BODY or TIMESTAMP_DOT_BODY")
	}
	if a.TimestampHeader != "" && !headerRE.MatchString(a.TimestampHeader) {
		add(".timestamp_header", "a header name")
	}
	if a.TimestampFormat != "" && a.TimestampFormat != "UNIX_SECONDS" && a.TimestampFormat != "RFC3339" {
		add(".timestamp_format", "UNIX_SECONDS or RFC3339")
	}
	if a.ReplayWindowSeconds != 0 && (a.ReplayWindowSeconds < 30 || a.ReplayWindowSeconds > 900) {
		add(".replay_window_seconds", "between 30 and 900")
	}
	// a signature over the body alone cannot be replay-protected by time: the timestamp must be signed
	if a.SignedContent == "BODY" && a.TimestampHeader == "" {
		add(".timestamp_header", "a body-only signature still needs a timestamp header for the replay window")
	}
}
