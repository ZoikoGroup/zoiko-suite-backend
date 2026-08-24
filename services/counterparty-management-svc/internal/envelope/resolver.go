package envelope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// §4 splits every field into caller-supplied and server-resolved, and §5 class S
// is emphatic about which wins: "Client may request context but cannot override
// result." This file implements the server-resolved half for the fields that an
// existing ZoikoSuite service can actually answer today.
//
// WHAT IS RESOLVABLE, AND WHAT IS NOT
//
// Resolvable now, because a service owns the fact and exposes a read:
//
//	jurisdiction_context  <- tenant-entity-registry-svc GET /v1/entities/{id}
//	                         .primary_jurisdiction_id
//	timezone              <- tenant-entity-registry-svc GET /v1/tenants/{id}
//	                         .primary_timezone
//	tenant status         <- the same tenant read (INV-01)
//	entity status         <- the same entity read (INV-02)
//
// NOT resolvable, because the owning service does not exist in services/:
//
//	book_id / reporting_basis  needs REF-06 Accounting Book / Ledger Basis
//	functional_currency        needs REF-02 Currency Registry (§6.1)
//	exchange_rate              needs REF-03 Foreign Exchange Rates (§6.1)
//	fiscal period validity     needs REF-04 Fiscal Calendar — LegalEntity carries
//	                           a fiscal_calendar_id, but nothing resolves it
//
// Those four are reported in the gap table rather than faked here. Filling
// book_id with a default would be the exact failure INV-03 exists to prevent:
// a posting that claims a basis nobody decided.

// ResolvedContext is the server-resolved half of the envelope.
//
// Every field is authoritative — it came from the service that owns the fact,
// not from the request — so a handler may act on these where it must not act on
// the caller's equivalent claims.
type ResolvedContext struct {
	TenantStatus        string
	TenantLifecycle     string
	Timezone            string
	DefaultCurrency     string
	ResidencyPolicyID   string
	LegalEntityStatus   string
	JurisdictionContext string
	FiscalCalendarID    string

	// Conflicts names fields where the caller asserted a value that disagrees
	// with the resolved one. The resolved value always wins (§5 class S); this
	// exists so the disagreement is visible in evidence rather than silently
	// discarded, which is what makes a caller sending the wrong jurisdiction
	// detectable instead of merely ineffective.
	Conflicts []Conflict
}

// Conflict is one caller assertion overridden by an authoritative value.
type Conflict struct {
	Field     string `json:"field"`
	Claimed   string `json:"claimed"`
	Resolved  string `json:"resolved"`
	Authority string `json:"authority"`
}

// ErrResolverUnavailable means the owning service could not be reached, so the
// context could not be resolved.
//
// Callers must treat this as "cannot answer", never as "no". jurisdiction-rules-svc
// already established this contract for the platform — a 503 from a validation
// probe means unknown, and a governed write must fail closed rather than proceed
// with an unvalidated jurisdiction.
var ErrResolverUnavailable = errors.New("context resolver unavailable")

// ErrTenantNotResolvable means the tenant does not exist, is suspended, or is
// otherwise not in a state that permits business operations.
var ErrTenantNotResolvable = errors.New("tenant context not resolvable")

// ErrEntityNotResolvable means the legal entity does not exist under this
// tenant, or is not in an active state.
var ErrEntityNotResolvable = errors.New("legal entity context not resolvable")

// Resolver reads authoritative context from tenant-entity-registry-svc.
type Resolver struct {
	registryBaseURL string
	http            *http.Client
}

// NewResolver builds a Resolver against tenant-entity-registry-svc.
//
// A short timeout is deliberate: this call sits in front of every governed
// write, so a slow registry must surface as a fast fail-closed refusal rather
// than as latency absorbed by every service on the platform.
func NewResolver(registryBaseURL string) *Resolver {
	return &Resolver{
		registryBaseURL: registryBaseURL,
		http:            &http.Client{Timeout: 5 * time.Second},
	}
}

// tenantView is the subset of tenant-entity-registry-svc's Tenant this package
// reads. Declared narrowly so an unrelated change to that service's response
// cannot break decoding here.
type tenantView struct {
	TenantID                     string `json:"tenant_id"`
	Status                       string `json:"status"`
	LifecycleState               string `json:"lifecycle_state"`
	PrimaryTimezone              string `json:"primary_timezone"`
	DefaultCurrencyCode          string `json:"default_currency_code"`
	DefaultDataResidencyPolicyID string `json:"default_data_residency_policy_id"`
}

type entityView struct {
	LegalEntityID         string `json:"legal_entity_id"`
	TenantID              string `json:"tenant_id"`
	EntityStatus          string `json:"entity_status"`
	PrimaryJurisdictionID string `json:"primary_jurisdiction_id"`
	FiscalCalendarID      string `json:"fiscal_calendar_id"`
	DataResidencyPolicyID string `json:"data_residency_policy_id"`
}

// Resolve fills the server-resolved half of the envelope for e.
//
// The legal-entity read is skipped when the envelope carries no entity, because
// §4 makes legal_entity_id conditional — a tenant-scoped operation such as a
// configuration read legitimately has none, and demanding one here would
// contradict the contract this package implements.
func (rs *Resolver) Resolve(ctx context.Context, e Envelope) (ResolvedContext, error) {
	var out ResolvedContext

	if e.TenantID == "" {
		return out, ErrTenantNotResolvable
	}

	var t tenantView
	if err := rs.get(ctx, e, "/v1/tenants/"+e.TenantID, &t); err != nil {
		if errors.Is(err, errNotFound) {
			return out, ErrTenantNotResolvable
		}
		return out, err
	}

	// INV-01. A suspended or terminated tenant resolves successfully at the HTTP
	// level and must still be refused: the read answering 200 says the tenant
	// exists, not that it may transact.
	if !tenantOperable(t.Status, t.LifecycleState) {
		return out, fmt.Errorf("%w: status=%s lifecycle=%s", ErrTenantNotResolvable, t.Status, t.LifecycleState)
	}

	out.TenantStatus = t.Status
	out.TenantLifecycle = t.LifecycleState
	out.Timezone = t.PrimaryTimezone
	out.DefaultCurrency = t.DefaultCurrencyCode
	out.ResidencyPolicyID = t.DefaultDataResidencyPolicyID

	if e.LegalEntityID != "" {
		var ent entityView
		if err := rs.get(ctx, e, "/v1/entities/"+e.LegalEntityID, &ent); err != nil {
			if errors.Is(err, errNotFound) {
				return out, ErrEntityNotResolvable
			}
			return out, err
		}

		// INV-02 plus INV-01. An entity belonging to another tenant must not be
		// usable just because the caller named it — the registry answers the
		// entity read by ID, so this is the check that keeps a cross-tenant
		// entity reference out of a posting.
		if ent.TenantID != "" && ent.TenantID != e.TenantID {
			return out, fmt.Errorf("%w: entity belongs to a different tenant", ErrEntityNotResolvable)
		}

		out.LegalEntityStatus = ent.EntityStatus
		out.JurisdictionContext = ent.PrimaryJurisdictionID
		out.FiscalCalendarID = ent.FiscalCalendarID
		if ent.DataResidencyPolicyID != "" {
			out.ResidencyPolicyID = ent.DataResidencyPolicyID
		}
	}

	out.Conflicts = conflicts(e, out)
	return out, nil
}

// tenantOperable reports whether a tenant may transact.
//
// The check is allow-listed rather than deny-listed: an unrecognised status —
// one added to the registry after this file was written — reads as not
// operable. A deny-list would admit exactly the states nobody has considered yet.
func tenantOperable(status, lifecycle string) bool {
	if status != "ACTIVE" {
		return false
	}
	switch lifecycle {
	case "ACTIVE", "PROVISIONED", "LIVE":
		return true
	default:
		return false
	}
}

func conflicts(e Envelope, r ResolvedContext) []Conflict {
	var out []Conflict
	if e.JurisdictionContext != "" && r.JurisdictionContext != "" && e.JurisdictionContext != r.JurisdictionContext {
		out = append(out, Conflict{
			Field:     "jurisdiction_context",
			Claimed:   e.JurisdictionContext,
			Resolved:  r.JurisdictionContext,
			Authority: "tenant-entity-registry-svc",
		})
	}
	if e.Timezone != "" && r.Timezone != "" && e.Timezone != r.Timezone {
		// Not a conflict in the same sense: §4 sources timezone from "tenant/user/
		// entity context", so a user in a different timezone from the tenant's
		// primary one is ordinary. Recorded because interpreting a local
		// timestamp under the wrong zone is a period-assignment bug, and this is
		// the only place the two values are ever seen together.
		out = append(out, Conflict{
			Field:     "timezone",
			Claimed:   e.Timezone,
			Resolved:  r.Timezone,
			Authority: "tenant-entity-registry-svc",
		})
	}
	return out
}

var errNotFound = errors.New("not found")

func (rs *Resolver) get(ctx context.Context, e Envelope, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rs.registryBaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrResolverUnavailable, err)
	}

	// The envelope propagates onto the resolver call itself. Without this the
	// registry's own row-level security has no tenant to scope by, and the
	// resulting read would be attributed to nothing in the audit trail —
	// a governed write whose validation step is invisible.
	req.Header.Set(HeaderTenantID, e.TenantID)
	if actor := e.Actor(); actor != "" {
		req.Header.Set(HeaderActorSubjectID, actor)
	}
	if e.CorrelationID != "" {
		req.Header.Set(HeaderCorrelationID, e.CorrelationID)
	}
	if e.RequestID != "" {
		req.Header.Set(HeaderRequestID, e.RequestID)
	}
	req.Header.Set(HeaderSourceChannel, string(ChannelSystem))
	req.Header.Set("Accept", "application/json")

	resp, err := rs.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrResolverUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode >= 500:
		// Explicitly not a denial. See ErrResolverUnavailable.
		return fmt.Errorf("%w: registry returned %d", ErrResolverUnavailable, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: registry returned %d", ErrResolverUnavailable, resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("%w: malformed registry response: %v", ErrResolverUnavailable, err)
	}
	return nil
}

// Apply overlays the resolved context onto the envelope, so downstream handlers
// read authoritative values through the same struct they already use.
//
// Resolved values overwrite caller-supplied ones rather than filling only the
// blanks — that is what §5 class S requires, and filling-only-blanks would let a
// caller pin a jurisdiction simply by asserting one first.
func (e Envelope) Apply(r ResolvedContext) Envelope {
	if r.JurisdictionContext != "" {
		e.JurisdictionContext = r.JurisdictionContext
	}
	if e.Timezone == "" && r.Timezone != "" {
		// Timezone is the one exception, and deliberately so: §4 sources it from
		// "tenant/user/entity context", which makes a caller-supplied zone a
		// legitimate narrowing of the tenant default rather than an override of
		// an authoritative value.
		e.Timezone = r.Timezone
	}
	return e
}
