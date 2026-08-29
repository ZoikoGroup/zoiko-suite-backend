// Package purposeregistry is a real HTTP client to
// privacy-purpose-registry-svc (PRV-01), used to validate a
// ProcessorRelationship's purpose_activity_refs against actual registry
// state rather than trusting them as opaque strings — same discipline as
// PRV-02/PRV-03's own dependency on PRV-01.
package purposeregistry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("privacy-purpose-registry-svc unavailable")

type ActivityVersion struct {
	ActivityVersionID string `json:"activity_version_id"`
	ActivityID        string `json:"activity_id"`
	VersionStatus     string `json:"version_status"`
}

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: &http.Client{Timeout: 5 * time.Second}}
}

// ResolveActivity mirrors privacy-decision-svc's client — GET
// /privacy/processing-activities/{id} only ever resolves ACTIVE/
// SUSPENDED/RETIRED.
func (c *Client) ResolveActivity(ctx context.Context, activityID string) (*ActivityVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/privacy/processing-activities/"+activityID, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var v ActivityVersion
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			return nil, ErrUnavailable
		}
		return &v, nil
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, ErrUnavailable
	}
}
