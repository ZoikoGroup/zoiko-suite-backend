package integration

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/eventing/envelope"
)

// Header names. The first four follow the estate's existing convention.
const (
	HeaderTenantID       = "X-Tenant-Id"
	HeaderLegalEntityID  = "X-Legal-Entity-Id"
	HeaderCorrelationID  = "X-Correlation-Id"
	HeaderIdempotencyKey = "Idempotency-Key"
)

// Context is the Trusted Integration Context Envelope (ZS-SVC-M-001 §2.1).
//
// Every field is server-resolved or validated; none is trusted merely because
// a caller sent it (§1.1 invariant 2). Build it with FromRequest so the tenant
// and entity come from the authenticated scope, then fill in the integration
// specific fields and call Validate.
type Context struct {
	TenantID      string
	LegalEntityID string // absent only when genuinely not applicable
	BookID        string

	ActorSubject     string // authenticated user principal
	WorkloadIdentity string // authenticated service principal
	AssuranceLevel   string
	DelegationRef    string // delegation / support-access context, if any

	Purpose string // approved business/integration purpose

	HomeRegion       string
	ProcessingRegion string

	CorrelationID string
	CausationID   string
	TraceID       string

	IdempotencyKey     string
	SubjectFingerprint string

	ContractVersion string
	SchemaProfile   string

	DataClassification envelope.Classification

	SourceSystem      string // canonical connector/provider identity
	DestinationSystem string

	RequestTime  time.Time // UTC instant
	BusinessDate string    // effective date kept separately (YYYY-MM-DD), optional

	AuthzDecisionRef string
}

// Trusted is the scope established by authentication and tenant resolution,
// never by request content.
type Trusted struct {
	TenantID         string
	LegalEntityID    string
	ActorSubject     string
	WorkloadIdentity string
	AssuranceLevel   string
	HomeRegion       string
	ProcessingRegion string
}

// system identifies a connector/provider canonically, not as free text.
var systemToken = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// regionToken matches the controlled lower-case region codes ("uk", "eu-west").
var regionToken = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

var dateOnly = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// FromRequest builds a Context from a trusted scope and a request's headers.
//
// A tenant or entity named in the headers is only ever compared with the
// trusted value; if they disagree the request is refused with INT-CTX-001 and
// nothing is routed (INT-NP-001). A missing correlation id is generated, so
// every request is traceable end to end.
func FromRequest(h http.Header, t Trusted, now time.Time) (Context, error) {
	if strings.TrimSpace(t.TenantID) == "" {
		return Context{}, New(CodeContextInvalid, "tenant could not be resolved from the authenticated scope")
	}
	if err := ResolveClaim("tenant", t.TenantID, h.Get(HeaderTenantID)); err != nil {
		return Context{}, err
	}
	if t.LegalEntityID != "" {
		if err := ResolveClaim("legal entity", t.LegalEntityID, h.Get(HeaderLegalEntityID)); err != nil {
			return Context{}, err
		}
	}
	corr := strings.TrimSpace(h.Get(HeaderCorrelationID))
	if corr == "" {
		corr = uuid.NewString()
	}
	return Context{
		TenantID:         t.TenantID,
		LegalEntityID:    t.LegalEntityID,
		ActorSubject:     t.ActorSubject,
		WorkloadIdentity: t.WorkloadIdentity,
		AssuranceLevel:   t.AssuranceLevel,
		HomeRegion:       t.HomeRegion,
		ProcessingRegion: t.ProcessingRegion,
		CorrelationID:    corr,
		IdempotencyKey:   strings.TrimSpace(h.Get(HeaderIdempotencyKey)),
		RequestTime:      now.UTC(),
	}, nil
}

// ResolveClaim checks a caller-supplied value against the trusted one. An empty
// claim is fine (the caller said nothing); a different claim is a forged or
// confused scope and fails closed.
func ResolveClaim(what, trusted, claimed string) error {
	claimed = strings.TrimSpace(claimed)
	if claimed == "" || claimed == trusted {
		return nil
	}
	return Newf(CodeContextInvalid, "%s in request does not match the authenticated %s", what, what)
}

// Rules selects which conditionally-required fields Validate insists on.
type Rules struct {
	RequireEntity      bool // target domain requires entity scope
	RequireIdempotency bool // command/side effect
	RequireDestination bool // outbound interaction
}

// Validate checks the envelope. All violations are reported together as one
// INT-CTX-001 so a caller fixes them in one pass. Detail names fields, never
// values.
func (c Context) Validate(r Rules) error {
	var bad []string
	need := func(ok bool, msg string) {
		if !ok {
			bad = append(bad, msg)
		}
	}
	need(strings.TrimSpace(c.TenantID) != "", "tenant_id is required")
	need(!r.RequireEntity || strings.TrimSpace(c.LegalEntityID) != "", "legal_entity_id is required")
	need(c.ActorSubject != "" || c.WorkloadIdentity != "", "actor_subject or workload_identity is required")
	need(strings.TrimSpace(c.Purpose) != "", "purpose is required")
	need(regionToken.MatchString(c.HomeRegion), "home_region must be a region code")
	need(regionToken.MatchString(c.ProcessingRegion), "processing_region must be a region code")
	need(strings.TrimSpace(c.CorrelationID) != "", "correlation_id is required")
	need(!r.RequireIdempotency || strings.TrimSpace(c.IdempotencyKey) != "", "idempotency_key is required")
	need(strings.TrimSpace(c.ContractVersion) != "", "contract_version is required")
	need(isClassification(c.DataClassification), "data_classification is invalid")
	need(systemToken.MatchString(c.SourceSystem), "source_system must be a canonical system identity")
	need((!r.RequireDestination && c.DestinationSystem == "") || systemToken.MatchString(c.DestinationSystem), "destination_system must be a canonical system identity")
	need(!c.RequestTime.IsZero() && c.RequestTime.Location() == time.UTC, "request_time must be a UTC instant")
	need(c.BusinessDate == "" || dateOnly.MatchString(c.BusinessDate), "business_date must be YYYY-MM-DD")
	if len(bad) == 0 {
		return nil
	}
	return New(CodeContextInvalid, strings.Join(bad, "; ")).WithCorrelation(c.CorrelationID)
}

func isClassification(c envelope.Classification) bool {
	switch c {
	case envelope.Public, envelope.Internal, envelope.Confidential, envelope.Restricted:
		return true
	}
	return false
}

// CheckResidency refuses processing outside the regions the residency policy
// allows for this payload (INT-REG-002). allowed must come from the policy
// decision, never from the request. An empty allowed list denies everything:
// no decision is not a permission.
func (c Context) CheckResidency(allowed []string) error {
	for _, r := range allowed {
		if r == c.ProcessingRegion {
			return nil
		}
	}
	return Newf(CodeResidencyDenied, "processing in region %q is not permitted for this tenant and data class", c.ProcessingRegion).
		WithCorrelation(c.CorrelationID)
}

// LogFields returns the identifiers that are safe in routine logs and metrics:
// ids, regions, versions and classification, never actor names, keys or bodies
// (INT-CTRL-035).
func (c Context) LogFields() map[string]string {
	m := map[string]string{
		"tenant_id":         c.TenantID,
		"correlation_id":    c.CorrelationID,
		"purpose":           c.Purpose,
		"processing_region": c.ProcessingRegion,
		"contract_version":  c.ContractVersion,
		"classification":    string(c.DataClassification),
		"source_system":     c.SourceSystem,
	}
	if c.DestinationSystem != "" {
		m["destination_system"] = c.DestinationSystem
	}
	if c.TraceID != "" {
		m["trace_id"] = c.TraceID
	}
	return m
}

type ctxKey struct{}

// WithContext stores c on ctx for downstream calls and evidence.
func WithContext(ctx context.Context, c Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext returns the Context stored by WithContext.
func FromContext(ctx context.Context) (Context, bool) {
	c, ok := ctx.Value(ctxKey{}).(Context)
	return c, ok
}

// Inject writes the propagation headers for an outbound internal call. Only
// identifiers travel; the receiving service must still resolve its own trusted
// scope and compare (FromRequest), so this is correlation, not authority.
func (c Context) Inject(h http.Header) {
	set := func(k, v string) {
		if v != "" {
			h.Set(k, v)
		}
	}
	set(HeaderTenantID, c.TenantID)
	set(HeaderLegalEntityID, c.LegalEntityID)
	set(HeaderCorrelationID, c.CorrelationID)
	set(HeaderIdempotencyKey, c.IdempotencyKey)
}
