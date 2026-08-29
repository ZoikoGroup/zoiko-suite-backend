// Package purposeregistry is a real HTTP client to
// privacy-purpose-registry-svc (PRV-01), used to enforce PRV-C01
// (purpose limitation) with actual registry state, not a caller-supplied
// claim.
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
	ActivityVersionID string   `json:"activity_version_id"`
	ActivityID        string   `json:"activity_id"`
	PurposeIDs        []string `json:"purpose_ids"`
	VersionStatus     string   `json:"version_status"`
}

type PurposeVersion struct {
	PurposeVersionID string `json:"purpose_version_id"`
	PurposeID        string `json:"purpose_id"`
	VersionStatus    string `json:"version_status"`
}

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: &http.Client{Timeout: 5 * time.Second}}
}

// ResolveActivity returns the currently-resolvable version of
// activityID, or nil if it doesn't resolve to one at all (not found, or
// exists but never activated) — GET /privacy/processing-activities/{id}
// in PRV-01 already only ever resolves ACTIVE/SUSPENDED/RETIRED.
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

// ResolvePurpose mirrors ResolveActivity for GET /privacy/purposes/{id},
// which only ever resolves a PUBLISHED version.
func (c *Client) ResolvePurpose(ctx context.Context, purposeID string) (*PurposeVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/privacy/purposes/"+purposeID, nil)
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
		var v PurposeVersion
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
