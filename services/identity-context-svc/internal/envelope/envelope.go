// Package envelope implements the ZoikoSuite Canonical Service Input Contract
// (ZS-ARCH-SVC-001 v2.0 §4) — the common request envelope every service-specific
// input contract inherits.
//
// # WHY THIS IS A VENDORED COPY, NOT AN IMPORT
//
// Each service in services/ is its own Go module (zoiko.io/<svc>) with no shared
// module and no replace directives, so there is nothing to import from. The repo
// already resolves this the same way for tenant scope — internal/middleware/tenant.go
// is duplicated across 73 services. This file follows that established pattern.
// The authoritative copy lives at services/_contract/envelope/ and is pushed into
// each service by services/_contract/rollout.sh; edit the authoritative copy,
// never a vendored one.
//
// # WHAT THIS DOES AND DOES NOT DECIDE
//
// The envelope carries context; it does not grant authority. §4 is explicit that a
// caller "may request context but cannot override result" for server-resolved
// values, so nothing here is trusted as an authorization, residency or tax claim.
// X-Tenant-Id and X-Principal-Id keep their existing meaning: they are set by
// gateway-auth-svc's ForwardAuth verification of the signed identity envelope, and
// are the only two fields in the request the caller did not write. Everything else
// this package parses is a caller assertion recorded for provenance and trace, and
// must be validated by the owning service before it is given any effect.
package envelope

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Canonical header names for the §4 fields.
//
// The first four already exist across the platform and keep their spelling
// exactly — renaming them would break gateway-auth-svc, the Next.js console and
// every inter-service client at once. Idempotency-Key is the IETF draft header
// rather than an X- variant because proxies and client libraries already
// understand it; the rest follow the existing X-<Field>-Id house style.
const (
	HeaderTenantID            = "X-Tenant-Id"
	HeaderActorSubjectID      = "X-Principal-Id"
	HeaderCorrelationID       = "X-Correlation-ID"
	HeaderLegalEntityID       = "X-Legal-Entity-Id"
	HeaderWorkloadID          = "X-Workload-Id"
	HeaderBookID              = "X-Book-Id"
	HeaderReportingBasis      = "X-Reporting-Basis"
	HeaderOperation           = "X-Operation"
	HeaderRequestID           = "X-Request-Id"
	HeaderCausationID         = "X-Causation-Id"
	HeaderIdempotencyKey      = "Idempotency-Key"
	HeaderSourceChannel       = "X-Source-Channel"
	HeaderSourceSystem        = "X-Source-System"
	HeaderExternalReference   = "X-External-Reference"
	HeaderOccurredAt          = "X-Occurred-At"
	HeaderEffectiveAt         = "X-Effective-At"
	HeaderTimezone            = "X-Timezone"
	HeaderJurisdictionContext = "X-Jurisdiction-Context"
	HeaderPurposeContext      = "X-Purpose-Context"
	HeaderExpectedVersion     = "X-Expected-Version"
	HeaderWorkflowInstanceID  = "X-Workflow-Instance-Id"
	HeaderApprovalReference   = "X-Approval-Reference"
	HeaderEvidenceRefs        = "X-Evidence-Refs"
)

// SourceChannel enumerates §4's permitted source_channel values: "Web, mobile,
// API, import, integration, scheduled job, system".
type SourceChannel string

const (
	ChannelWeb          SourceChannel = "web"
	ChannelMobile       SourceChannel = "mobile"
	ChannelAPI          SourceChannel = "api"
	ChannelImport       SourceChannel = "import"
	ChannelIntegration  SourceChannel = "integration"
	ChannelScheduledJob SourceChannel = "scheduled_job"
	ChannelSystem       SourceChannel = "system"
)

var validChannels = map[SourceChannel]bool{
	ChannelWeb: true, ChannelMobile: true, ChannelAPI: true, ChannelImport: true,
	ChannelIntegration: true, ChannelScheduledJob: true, ChannelSystem: true,
}

// Valid reports whether c is one of the seven channels §4 permits. An unknown
// channel is rejected rather than coerced: source_channel drives provenance
// class (§5) and import/integration additionally force source_system, so
// silently mapping an unrecognised value onto "api" would erase the very
// obligation the field exists to create.
func (c SourceChannel) Valid() bool { return validChannels[c] }

// External reports whether the channel means the fact originated outside
// ZoikoSuite, which is what makes source_system mandatory under §4 and puts the
// record in provenance class X (§5).
func (c SourceChannel) External() bool {
	return c == ChannelImport || c == ChannelIntegration
}

// Envelope is the parsed §4 common request envelope.
//
// Times are pointers because §4 distinguishes "not supplied" from a zero time:
// occurred_at is only "required for underlying business events" and effective_at
// only "when effect differs from processing time", so an absent value is a
// legitimate state that a zero time.Time cannot express.
type Envelope struct {
	TenantID            string
	ActorSubjectID      string
	WorkloadID          string
	LegalEntityID       string
	BookID              string
	ReportingBasis      string
	Operation           string
	RequestID           string
	CorrelationID       string
	CausationID         string
	IdempotencyKey      string
	SourceChannel       SourceChannel
	SourceSystem        string
	ExternalReference   string
	OccurredAt          *time.Time
	EffectiveAt         *time.Time
	Timezone            string
	JurisdictionContext string
	PurposeContext      string
	ExpectedVersion     string
	WorkflowInstanceID  string
	ApprovalReference   string
	EvidenceRefs        []string

	// badTimes records headers that were present but unparseable, so Validate
	// can refuse them. Without this a malformed X-Occurred-At would land as a
	// nil pointer and read as "the caller did not send one" — turning a corrupt
	// business-event timestamp into a silently absent one.
	badTimes []string
}

// Actor returns the acting identity, preferring the human subject over the
// workload. §4 makes "actor_subject_id / workload_id" one mandatory slot with
// two possible fillers, so callers that just need "who acted" ask here.
func (e Envelope) Actor() string {
	if e.ActorSubjectID != "" {
		return e.ActorSubjectID
	}
	return e.WorkloadID
}

// Parse reads the envelope from request headers. It performs no validation —
// Validate does that — so a handler can log or trace a request that is about to
// be refused.
func Parse(r *http.Request) Envelope {
	h := r.Header
	e := Envelope{
		TenantID:            strings.TrimSpace(h.Get(HeaderTenantID)),
		ActorSubjectID:      strings.TrimSpace(h.Get(HeaderActorSubjectID)),
		WorkloadID:          strings.TrimSpace(h.Get(HeaderWorkloadID)),
		LegalEntityID:       strings.TrimSpace(h.Get(HeaderLegalEntityID)),
		BookID:              strings.TrimSpace(h.Get(HeaderBookID)),
		ReportingBasis:      strings.TrimSpace(h.Get(HeaderReportingBasis)),
		Operation:           strings.TrimSpace(h.Get(HeaderOperation)),
		RequestID:           strings.TrimSpace(h.Get(HeaderRequestID)),
		CorrelationID:       strings.TrimSpace(h.Get(HeaderCorrelationID)),
		CausationID:         strings.TrimSpace(h.Get(HeaderCausationID)),
		IdempotencyKey:      strings.TrimSpace(h.Get(HeaderIdempotencyKey)),
		SourceChannel:       SourceChannel(strings.ToLower(strings.TrimSpace(h.Get(HeaderSourceChannel)))),
		SourceSystem:        strings.TrimSpace(h.Get(HeaderSourceSystem)),
		ExternalReference:   strings.TrimSpace(h.Get(HeaderExternalReference)),
		Timezone:            strings.TrimSpace(h.Get(HeaderTimezone)),
		JurisdictionContext: strings.TrimSpace(h.Get(HeaderJurisdictionContext)),
		PurposeContext:      strings.TrimSpace(h.Get(HeaderPurposeContext)),
		ExpectedVersion:     strings.TrimSpace(h.Get(HeaderExpectedVersion)),
		WorkflowInstanceID:  strings.TrimSpace(h.Get(HeaderWorkflowInstanceID)),
		ApprovalReference:   strings.TrimSpace(h.Get(HeaderApprovalReference)),
		EvidenceRefs:        splitRefs(h.Get(HeaderEvidenceRefs)),
	}

	// operation defaults to the route that was actually invoked. §4 calls
	// operation mandatory and sourced from the request; in a REST API the
	// method+path IS the requested command, so deriving it is faithful rather
	// than a fabricated value, and it keeps every existing caller compliant.
	if e.Operation == "" {
		e.Operation = r.Method + " " + r.URL.Path
	}

	e.OccurredAt = e.parseTime(h.Get(HeaderOccurredAt), HeaderOccurredAt)
	e.EffectiveAt = e.parseTime(h.Get(HeaderEffectiveAt), HeaderEffectiveAt)
	return e
}

// parseTime accepts RFC3339 only. §6.2 gives these fields legal meaning —
// occurred_at is when the real-world event happened, effective_at is when a
// record becomes effective — so a loose multi-format parse that guessed
// day/month order could silently move a transaction into a different period.
func (e *Envelope) parseTime(raw, header string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		e.badTimes = append(e.badTimes, header)
		return nil
	}
	return &t
}

func splitRefs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type ctxKey struct{}

// WithEnvelope returns a context carrying e.
func WithEnvelope(ctx context.Context, e Envelope) context.Context {
	return context.WithValue(ctx, ctxKey{}, e)
}

// FromContext returns the envelope placed by Middleware, and whether one was
// present. Handlers behind Middleware can ignore the bool; a false means the
// middleware is not wired, which is a deployment bug rather than a caller error.
func FromContext(ctx context.Context) (Envelope, bool) {
	e, ok := ctx.Value(ctxKey{}).(Envelope)
	return e, ok
}

// MustFromContext returns the envelope, or a zero envelope if the middleware is
// not wired. Use in logging and event-emission paths where an absent envelope
// must not panic a request that is otherwise valid.
func MustFromContext(ctx context.Context) Envelope {
	e, _ := FromContext(ctx)
	return e
}

// ApplyTo writes this envelope onto an OUTBOUND request, so a service calling a
// peer forwards the caller's envelope instead of dropping it.
//
// WHY THIS EXISTS. Every authz client in this estate built its outbound call to
// authorization-svc with one header:
//
//	req.Header.Set("Content-Type", "application/json")
//
// authorization-svc enforces the same canonical contract on POST /v1/authorize
// that every other service enforces on a write, so it answered 401
// envelope_incomplete naming six missing fields. The caller treats any non-2xx
// as "authorization service unavailable" and fails closed, so the symptom was
// 503 authz_unavailable on every authorized write in the platform -- with
// authorization-svc healthy, reachable, and answering correctly the whole time.
//
// It is the exact inverse of Parse, and deliberately in this file next to it:
// the two must not drift, because a header Parse reads and ApplyTo omits becomes
// a field that silently disappears on every internal hop.
//
// THE ENVELOPE IS FORWARDED VERBATIM, including the idempotency key. That is the
// doctrinal reading: the envelope identifies a business OPERATION, and an
// authorization check made while serving that operation belongs to it rather
// than being a new one. Minting a fresh key here would break the link between a
// decision and the write it authorised.
//
// Empty fields are omitted rather than set blank. Header.Get returns "" for both
// so a peer cannot tell them apart, but sending empty headers makes a request
// dump unreadable and invites a peer to treat "" as a real value.
func (e Envelope) ApplyTo(r *http.Request) {
	set := func(header, value string) {
		if value != "" {
			r.Header.Set(header, value)
		}
	}

	set(HeaderTenantID, e.TenantID)
	set(HeaderActorSubjectID, e.ActorSubjectID)
	set(HeaderWorkloadID, e.WorkloadID)
	set(HeaderLegalEntityID, e.LegalEntityID)
	set(HeaderBookID, e.BookID)
	set(HeaderReportingBasis, e.ReportingBasis)
	set(HeaderOperation, e.Operation)
	set(HeaderRequestID, e.RequestID)
	set(HeaderCorrelationID, e.CorrelationID)
	set(HeaderCausationID, e.CausationID)
	set(HeaderIdempotencyKey, e.IdempotencyKey)
	set(HeaderSourceSystem, e.SourceSystem)
	set(HeaderExternalReference, e.ExternalReference)
	set(HeaderTimezone, e.Timezone)
	set(HeaderJurisdictionContext, e.JurisdictionContext)
	set(HeaderPurposeContext, e.PurposeContext)
	set(HeaderExpectedVersion, e.ExpectedVersion)
	set(HeaderWorkflowInstanceID, e.WorkflowInstanceID)
	set(HeaderApprovalReference, e.ApprovalReference)

	// source_channel is mandatory downstream and has a closed vocabulary, so an
	// absent or unrecognised one cannot simply be passed along. A service-to-
	// service hop with no channel of its own genuinely IS a system call, which
	// is what ChannelSystem means -- this does not coerce a caller's real
	// channel, it only fills a gap that would otherwise be a 401.
	channel := e.SourceChannel
	if !channel.Valid() {
		channel = ChannelSystem
	}
	r.Header.Set(HeaderSourceChannel, string(channel))

	// RFC3339 because that is the only format Parse accepts. Anything else
	// arrives at the peer as a badTimes entry and is refused.
	if e.OccurredAt != nil {
		r.Header.Set(HeaderOccurredAt, e.OccurredAt.Format(time.RFC3339))
	}
	if e.EffectiveAt != nil {
		r.Header.Set(HeaderEffectiveAt, e.EffectiveAt.Format(time.RFC3339))
	}
	if len(e.EvidenceRefs) > 0 {
		r.Header.Set(HeaderEvidenceRefs, strings.Join(e.EvidenceRefs, ","))
	}
}

// ForwardTo copies the envelope from ctx onto an outbound request. The
// convenience form of ApplyTo for the common case, where a client has a ctx and
// a request and nothing else.
//
// A missing envelope is not an error here: it means the middleware did not run,
// which happens on an internal call made outside a request (a startup probe, a
// background reconciler). Those get a bare request, and the peer refuses them if
// its own contract says it must -- which is the correct outcome, decided at the
// peer rather than papered over here.
func ForwardTo(ctx context.Context, r *http.Request) {
	if e, ok := FromContext(ctx); ok {
		e.ApplyTo(r)
	}
}
