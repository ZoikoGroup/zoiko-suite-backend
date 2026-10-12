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
	ActivityVersionID       string   `json:"activity_version_id"`
	ActivityID              string   `json:"activity_id"`
	PurposeIDs              []string `json:"purpose_ids"`
	VersionStatus           string   `json:"version_status"`
	NoticeConsentDependency string   `json:"notice_consent_dependency,omitempty"`
	DpiaTiaStatus           string   `json:"dpia_tia_status,omitempty"`
	SubjectClasses          []string `json:"subject_classes,omitempty"`
	DataCategories          []string `json:"data_categories,omitempty"`
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
// tenantID is forwarded via X-Tenant-Id — PRV-01's own RLS scopes this
// read by tenant, and an activity genuinely owned by the caller's tenant
// resolves to nothing without it (see retentionregistry.Client.Resolve's
// doc comment for the same convention).
func (c *Client) ResolveActivity(ctx context.Context, tenantID, activityID string) (*ActivityVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/privacy/processing-activities/"+activityID, nil)
	if err != nil {
		return nil, err
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
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
// which only ever resolves a PUBLISHED version. tenantID is forwarded the
// same way — see ResolveActivity's doc comment.
func (c *Client) ResolvePurpose(ctx context.Context, tenantID, purposeID string) (*PurposeVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/privacy/purposes/"+purposeID, nil)
	if err != nil {
		return nil, err
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
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
