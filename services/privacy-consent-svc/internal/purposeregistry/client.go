// Package purposeregistry is a real HTTP client to
// privacy-purpose-registry-svc (PRV-01) — not a stub. Every ConsentReceipt
// this service writes names a purpose_id, and PRV-I01 ("possession of
// personal data is not permission to use it") means an opaque,
// unvalidated purpose string would be worthless as evidence: anyone could
// grant "consent" for a purpose that was never registered, never
// reviewed, and does not exist. This client is what keeps that from
// happening.
package purposeregistry

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

var ErrPurposeRegistryUnavailable = errors.New("privacy-purpose-registry-svc unavailable")

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// IsPublished reports whether purposeID resolves, right now, to a
// currently PUBLISHED purpose version. GET /privacy/purposes/{id} in
// PRV-01 already only ever resolves a PUBLISHED version (see
// ResolvePurposeAsOf) — a 404 there means either the purpose doesn't
// exist or it exists but has never been published, and either way this
// method reports it as not usable.
func (c *Client) IsPublished(ctx context.Context, purposeID string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/privacy/purposes/"+purposeID, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, ErrPurposeRegistryUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, ErrPurposeRegistryUnavailable
	}
}
