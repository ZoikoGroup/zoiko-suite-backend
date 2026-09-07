package envelope

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Requirement mirrors the Requirement column of §4.
type Requirement int

const (
	// NotRequired means the service never needs the field. It is still parsed
	// and propagated — §12 retrieval depends on fields being carried even by
	// services that do not act on them.
	NotRequired Requirement = iota
	// Required means §4 marks the field Mandatory for this service.
	Required
	// RequiredOnWrite means Mandatory only for material state changes, which is
	// how §4 qualifies idempotency_key ("Mandatory for material state changes").
	RequiredOnWrite
)

// Policy declares which conditional §4 fields a given service actually needs.
//
// §4 splits its fields into three groups: unconditionally Mandatory,
// Conditional, and "Mandatory for <some class of action>". The unconditional
// ones are enforced for every service and are not expressible here — there is
// no legitimate reason for a service to opt out of tenant_id. This struct
// covers only the conditional ones, where the answer genuinely differs per
// service: general-ledger-svc must have book_id, notification-svc must not be
// forced to invent one.
type Policy struct {
	// ServiceName appears in refusals so a caller reading a 400 in an
	// aggregated log can tell which service refused.
	ServiceName string

	// LegalEntityID — §4: "Conditional but mandatory for entity-specific
	// records". Set Required for services whose records are entity-scoped.
	LegalEntityID Requirement

	// BookID — §4: "Mandatory for accounting/reporting actions". Set Required
	// only for services that post to, or report from, a book (INV-03).
	BookID Requirement

	// PurposeContext — §4: "Required for governed sensitive access". Set
	// Required on services whose reads touch personal, bank, tax or privileged
	// content, so the reason for access is captured before the data is served.
	PurposeContext Requirement

	// IdempotencyKey — §4 and INV-08. Defaults to RequiredOnWrite when left at
	// NotRequired, because a service that changes material state without replay
	// protection violates the invariant; a service may raise it to Required but
	// disabling it is deliberately not expressible.
	IdempotencyKey Requirement

	// Timezone — §4: "Required for time-sensitive actions".
	Timezone Requirement

	// JurisdictionContext — §4 Conditional, server-resolved/validated.
	JurisdictionContext Requirement

	// MaterialWrite decides which requests count as material state changes for
	// RequiredOnWrite. Defaults to defaultMaterialWrite (any non-GET/HEAD/
	// OPTIONS). Override where a service exposes a POST that changes nothing —
	// a search or evaluate endpoint — so it is not forced to carry an
	// idempotency key it has no state to protect.
	MaterialWrite func(*http.Request) bool

	// ExemptPaths lists exact paths that bypass validation, in addition to the
	// health probes.
	//
	// This exists for endpoints that *produce* the envelope's mandatory fields
	// rather than consume them, where requiring those fields as input is
	// circular. gateway-auth-svc's /verify is the canonical case: Traefik calls
	// it with the client's original method and headers precisely so it can
	// derive X-Tenant-Id and X-Principal-Id from the signed token, so demanding
	// those two headers on the way in would refuse every request the gateway
	// exists to authenticate — and would only ever be satisfiable by a caller
	// spoofing the values the gateway is supposed to establish.
	//
	// Declared as data rather than a closure so rollout.sh can generate it; a
	// service needing richer logic sets Exempt instead, which takes precedence.
	ExemptPaths []string

	// Exempt suppresses validation entirely for a request. Health and readiness
	// probes reach the router before any gateway sets identity headers, so
	// without this every service would fail its own liveness check the moment
	// enforcement turned on.
	//
	// Set this to replace the default behaviour entirely — it overrides both
	// ExemptPaths and the built-in probe list, so an implementation must
	// re-exempt the probes itself.
	Exempt func(*http.Request) bool
}

// defaultMaterialWrite treats every non-idempotent HTTP method as a material
// state change. GET/HEAD/OPTIONS are safe by definition; PUT and DELETE are
// idempotent at the HTTP level but not at the accounting level — replaying
// DELETE /v1/entity-jurisdictions/{id} end-dates a second assignment if the
// first call succeeded and the response was lost — so they are included.
func defaultMaterialWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// defaultExempt exempts the platform's two probe routes, which every service in
// this repo registers directly on the root router.
func defaultExempt(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz", "/readyz", "/health":
		return true
	default:
		return false
	}
}

func (p Policy) materialWrite(r *http.Request) bool {
	if p.MaterialWrite != nil {
		return p.MaterialWrite(r)
	}
	return defaultMaterialWrite(r)
}

func (p Policy) exempt(r *http.Request) bool {
	if p.Exempt != nil {
		return p.Exempt(r)
	}
	for _, path := range p.ExemptPaths {
		if r.URL.Path == path {
			return true
		}
	}
	return defaultExempt(r)
}

// Violation is one unmet §4 obligation.
type Violation struct {
	Field  string `json:"field"`
	Header string `json:"header"`
	Reason string `json:"reason"`
}

// ValidationError is the set of §4 obligations a request failed.
//
// It carries every violation rather than the first, because a caller adopting
// the envelope typically misses several fields at once and a one-at-a-time
// refusal turns that into as many failed round trips.
type ValidationError struct {
	Service    string      `json:"service"`
	Violations []Violation `json:"violations"`
}

func (v *ValidationError) Error() string {
	fields := make([]string, 0, len(v.Violations))
	for _, x := range v.Violations {
		fields = append(fields, x.Field)
	}
	sort.Strings(fields)
	return fmt.Sprintf("canonical input contract violated: %s", strings.Join(fields, ", "))
}

// Validate applies §4 to e for the given request, returning nil when the
// envelope satisfies the contract.
//
// Ordering note: the unconditional fields are checked first so that a caller
// sending nothing at all gets the same leading violations from every service.
func (p Policy) Validate(e Envelope, r *http.Request) *ValidationError {
	if p.exempt(r) {
		return nil
	}

	var vs []Violation
	add := func(field, header, reason string) {
		vs = append(vs, Violation{Field: field, Header: header, Reason: reason})
	}

	// --- §4 unconditionally Mandatory ---

	if e.TenantID == "" {
		// INV-01: "Every authenticated operation resolves exactly one tenant
		// context before data access." Missing tenant scope is an authentication
		// failure, not a validation failure — see StatusFor.
		add("tenant_id", HeaderTenantID, "mandatory: tenant authority and isolation boundary")
	}
	if e.Actor() == "" {
		add("actor_subject_id", HeaderActorSubjectID,
			"mandatory: supply "+HeaderActorSubjectID+" for a human subject or "+HeaderWorkloadID+" for a workload")
	}
	if e.RequestID == "" {
		add("request_id", HeaderRequestID, "mandatory: request tracing")
	}
	if e.CorrelationID == "" {
		add("correlation_id", HeaderCorrelationID, "mandatory: end-to-end business trace")
	}
	if e.SourceChannel == "" {
		add("source_channel", HeaderSourceChannel, "mandatory: one of "+channelList())
	} else if !e.SourceChannel.Valid() {
		add("source_channel", HeaderSourceChannel,
			fmt.Sprintf("unrecognised channel %q: expected one of %s", string(e.SourceChannel), channelList()))
	}

	// --- §4 "Mandatory for <class>" ---

	// source_system: §4 marks it "Mandatory for imported/integrated records".
	// The channel is what tells us the record is imported or integrated, so the
	// obligation is derived from it rather than declared per service.
	if e.SourceChannel.External() && e.SourceSystem == "" {
		add("source_system", HeaderSourceSystem,
			fmt.Sprintf("mandatory for %s records: external provenance", string(e.SourceChannel)))
	}

	// idempotency_key: NotRequired is read as RequiredOnWrite, so a service
	// cannot opt out of INV-08 by leaving the field unset. See Policy.
	idem := p.IdempotencyKey
	if idem == NotRequired {
		idem = RequiredOnWrite
	}
	if e.IdempotencyKey == "" && required(idem, p.materialWrite(r)) {
		add("idempotency_key", HeaderIdempotencyKey,
			"mandatory for material state changes: duplicate/replay protection (INV-08)")
	}

	// --- §4 Conditional, per-service ---

	if e.LegalEntityID == "" && required(p.LegalEntityID, p.materialWrite(r)) {
		add("legal_entity_id", HeaderLegalEntityID, "mandatory for entity-specific records (INV-02)")
	}
	if e.BookID == "" && e.ReportingBasis == "" && required(p.BookID, p.materialWrite(r)) {
		add("book_id", HeaderBookID,
			"mandatory for accounting/reporting actions: supply "+HeaderBookID+" or "+HeaderReportingBasis+" (INV-03)")
	}
	if e.PurposeContext == "" && required(p.PurposeContext, p.materialWrite(r)) {
		add("purpose_context", HeaderPurposeContext, "required for governed sensitive access")
	}
	if e.Timezone == "" && required(p.Timezone, p.materialWrite(r)) {
		add("timezone", HeaderTimezone, "required for time-sensitive actions")
	}
	if e.JurisdictionContext == "" && required(p.JurisdictionContext, p.materialWrite(r)) {
		add("jurisdiction_context", HeaderJurisdictionContext, "required for regulatory/tax policy routing")
	}

	// --- malformed rather than missing ---

	for _, h := range e.badTimes {
		add(fieldForHeader(h), h, "must be an RFC3339 timestamp, e.g. 2026-08-24T09:30:00Z")
	}

	if len(vs) == 0 {
		return nil
	}
	return &ValidationError{Service: p.ServiceName, Violations: vs}
}

func required(req Requirement, isWrite bool) bool {
	switch req {
	case Required:
		return true
	case RequiredOnWrite:
		return isWrite
	default:
		return false
	}
}

func fieldForHeader(h string) string {
	switch h {
	case HeaderOccurredAt:
		return "occurred_at"
	case HeaderEffectiveAt:
		return "effective_at"
	default:
		return h
	}
}

func channelList() string {
	names := make([]string, 0, len(validChannels))
	for c := range validChannels {
		names = append(names, string(c))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// StatusFor picks the HTTP status for a refusal.
//
// A missing tenant_id or actor is 401, not 400: those two headers are set by
// gateway-auth-svc after it verifies the signed identity envelope, so their
// absence means the request never passed that verification. Reporting it as 400
// would tell the caller to fix its payload when what it actually has to fix is
// authentication. Every other violation is a genuine contract error, so 400.
func StatusFor(err *ValidationError) int {
	for _, v := range err.Violations {
		if v.Field == "tenant_id" || v.Field == "actor_subject_id" {
			return http.StatusUnauthorized
		}
	}
	return http.StatusBadRequest
}
