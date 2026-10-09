// Package hydrator fetches the current authoritative object for an R2/R3
// result (§7.1: "current source object/record excerpt is retrieved after
// authorization; index content is not trusted as current display value").
//
// Three rules, each a fix to the first version of this package:
//
//  1. The CALLER's envelope is forwarded, not a service identity. The first
//     version sent X-Principal-Id "system:search-hydrator" and X-Workload-Id
//     "search-indexer-svc" — asserting an identity on a direct service-to-
//     service call that the edge never sees, so no edge sanitation could ever
//     have checked it (Group 1 cross-service finding 1). It also substituted a
//     broader platform identity for the real actor, which §10.2 and INV-28
//     forbid for exactly this retrieval step. Forwarding the caller means the
//     source authorizes the person who is actually looking.
//  2. The request carries the full canonical envelope. Every source service
//     runs ZS_ENVELOPE_ENFORCEMENT=strict, and the first version omitted
//     X-Request-Id and X-Correlation-ID, so every hydration was refused 400
//     envelope_incomplete before the source even looked at the id.
//  3. The URL is configured, not guessed. The first version built
//     /v1/{source_type}/{id} — /v1/obligation/… against a service that serves
//     /v1/obligations/{obligation_id}. SOURCE_SERVICE_URL_<TYPE> now names the
//     record collection itself, and the id is appended.
//
// A 404 or 410 is domain.ErrSourceGone (NP-55: suppress, no stale body). Every
// other failure is a hydration failure the retriever reports as ESR-014 and
// suppresses — never serving the index's copy in its place (NP-20).
package hydrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/envelope"
)

// Hydrator fetches current source objects from their owning services.
type Hydrator struct {
	http *http.Client
	// collections maps a source_type to the URL of its record collection,
	// e.g. "obligation" → "http://obligations-svc:8088/v1/obligations".
	collections map[string]string
}

// New builds a Hydrator over the configured record collections.
func New(collections map[string]string, timeout time.Duration) *Hydrator {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	cleaned := make(map[string]string, len(collections))
	for sourceType, base := range collections {
		cleaned[strings.ToLower(strings.TrimSpace(sourceType))] = strings.TrimRight(strings.TrimSpace(base), "/")
	}
	return &Hydrator{http: &http.Client{Timeout: timeout}, collections: cleaned}
}

// Configured reports whether any source can be hydrated at all.
func (h *Hydrator) Configured() bool { return len(h.collections) > 0 }

// Hydrate fetches the current object for (sourceType, sourceID) as the caller.
func (h *Hydrator) Hydrate(ctx context.Context, sourceType, sourceID, tenantID string) (map[string]any, error) {
	base, ok := h.collections[strings.ToLower(sourceType)]
	if !ok {
		return nil, fmt.Errorf("no hydration source configured for source_type %q (SOURCE_SERVICE_URL_%s)",
			sourceType, strings.ToUpper(sourceType))
	}
	env, ok := envelope.FromContext(ctx)
	if !ok || env.Actor() == "" {
		// No caller to hydrate as. Refused rather than sent anonymously or
		// under a service identity — rule 1 above.
		return nil, fmt.Errorf("hydration requires the caller's verified envelope")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+url.PathEscape(sourceID), nil)
	if err != nil {
		return nil, fmt.Errorf("build hydration request: %w", err)
	}
	forwardEnvelope(req, env, tenantID, middleware.GetReqID(ctx))

	resp, err := h.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hydration request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		// NP-55. Also the answer a source gives a caller it does not let see
		// the record, which is the right outcome too: not shown.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: %s/%s", domain.ErrSourceGone, sourceType, sourceID)
	default:
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("hydration of %s/%s answered %d: %s", sourceType, sourceID, resp.StatusCode, raw)
	}

	var obj map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode hydration response: %w", err)
	}
	return obj, nil
}

// forwardEnvelope sets the canonical envelope on the outbound request from the
// caller's verified one — the pattern authz.HTTPClient already follows, and for
// the same reason: the source validates the same contract this service does,
// and a decision it makes must be traceable to the request that caused it.
func forwardEnvelope(req *http.Request, env envelope.Envelope, tenantID, fallbackRequestID string) {
	if tenantID == "" {
		tenantID = env.TenantID
	}
	req.Header.Set(envelope.HeaderTenantID, tenantID)
	if env.ActorSubjectID != "" {
		req.Header.Set(envelope.HeaderActorSubjectID, env.ActorSubjectID)
	}
	if env.WorkloadID != "" {
		// The caller's own workload, when it has one (an AI caller acting for
		// a principal). Never this service's.
		req.Header.Set(envelope.HeaderWorkloadID, env.WorkloadID)
	}
	requestID := env.RequestID
	if requestID == "" {
		requestID = fallbackRequestID
	}
	req.Header.Set(envelope.HeaderRequestID, requestID)
	correlation := env.CorrelationID
	if correlation == "" {
		correlation = requestID
	}
	req.Header.Set(envelope.HeaderCorrelationID, correlation)
	channel := string(env.SourceChannel)
	if channel == "" {
		channel = "system"
	}
	req.Header.Set(envelope.HeaderSourceChannel, channel)
	for header, value := range map[string]string{
		envelope.HeaderLegalEntityID:       env.LegalEntityID,
		envelope.HeaderPurposeContext:      env.PurposeContext,
		envelope.HeaderJurisdictionContext: env.JurisdictionContext,
		// source_system is mandatory at a strict source for an imported or
		// integrated caller (§4); without it such a caller's R2 results were
		// all refused 400 and suppressed as ESR-014.
		envelope.HeaderSourceSystem: env.SourceSystem,
		envelope.HeaderTimezone:     env.Timezone,
	} {
		if value != "" {
			req.Header.Set(header, value)
		}
	}
	req.Header.Set("Accept", "application/json")
}
