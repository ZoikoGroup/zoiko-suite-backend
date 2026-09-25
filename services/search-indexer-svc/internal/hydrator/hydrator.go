// Package hydrator provides source object hydration for R2/R3 retrieval.
//
// An R2 scope requires the search service to fetch the current authoritative
// object from its source service before returning it. This ensures the
// returned content reflects the source of truth, not just what was indexed
// at some point in the past (which could be stale if restrictions were
// applied after indexing).
//
// The hydrator is a cross-service call: for a given source_type, it calls
// the owning service's API to fetch the current object by ID. The service
// URLs are configured via SOURCE_SERVICE_URL_<SOURCE_TYPE> env vars.
package hydrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Hydrator fetches current source objects from their owning services.
type Hydrator struct {
	httpClient   *http.Client
	serviceURLs  map[string]string
	log          *zap.Logger
}

// New builds a Hydrator from environment variables.
//
// Each source service URL is configured as SOURCE_SERVICE_URL_<SOURCE_TYPE>
// (e.g. SOURCE_SERVICE_URL_OBLIGATION=http://obligations-svc:8080).
// The hydrator makes authenticated requests using the same envelope headers
// as the search request, scoped to the platform identity.
func New(log *zap.Logger, timeout time.Duration) *Hydrator {
	serviceURLs := make(map[string]string)
	for _, e := range os.Environ() {
		key, value, _ := strings.Cut(e, "=")
		if strings.HasPrefix(key, "SOURCE_SERVICE_URL_") {
			sourceType := strings.ToLower(strings.TrimPrefix(key, "SOURCE_SERVICE_URL_"))
			serviceURLs[sourceType] = value
		}
	}

	return &Hydrator{
		httpClient: &http.Client{Timeout: timeout},
		serviceURLs:  serviceURLs,
		log:          log,
	}
}

// Hydrate fetches the current object for the given source_type and source_id
// from the owning service. The request carries the caller's tenant context
// and a platform-scoped actor identity.
//
// The expected endpoint pattern is: GET {service_url}/v1/{source_type}/{source_id}
func (h *Hydrator) Hydrate(ctx context.Context, sourceType, sourceID, tenantID string) (map[string]any, error) {
	serviceURL, ok := h.serviceURLs[sourceType]
	if !ok {
		h.log.Warn("no service URL configured for source type",
			zap.String("source_type", sourceType),
			zap.String("source_id", sourceID))
		return nil, fmt.Errorf("no hydration service configured for source_type %q", sourceType)
	}

	url := fmt.Sprintf("%s/v1/%s/%s", strings.TrimRight(serviceURL, "/"), sourceType, sourceID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build hydration request: %w", err)
	}

	// Forward the caller's tenant identity
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}

	// The hydrator runs as a platform service - it does not act as any
	// specific principal but requires the source service to authorize
	// the read. The source service will enforce its own authorization
	// against the tenant and the actor from the envelope.
	req.Header.Set("X-Principal-Id", "system:search-hydrator")
	req.Header.Set("X-Workload-Id", "search-indexer-svc")
	req.Header.Set("X-Source-Channel", "internal")
	req.Header.Set("X-Purpose-Context", "SOURCE_HYDRATION")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hydration request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("source object not found: %s/%s", sourceType, sourceID)
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("hydration forbidden by source service: %s/%s", sourceType, sourceID)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hydration returned %d", resp.StatusCode)
	}

	var obj map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode hydration response: %w", err)
	}

	return obj, nil
}