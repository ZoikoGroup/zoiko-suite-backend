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
	"net/url"
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
	Jurisdictions           []string `json:"jurisdictions,omitempty"`
	RetentionRuleRefs       []string `json:"retention_rule_refs,omitempty"`
	TransferRefs            []string `json:"transfer_refs,omitempty"`
}

type PurposeVersion struct {
	PurposeVersionID     string   `json:"purpose_version_id"`
	PurposeID            string   `json:"purpose_id"`
	VersionStatus        string   `json:"version_status"`
	LawfulBasisRefs      []string `json:"lawful_basis_refs,omitempty"`
	SpecialConditionRefs []string `json:"special_condition_refs,omitempty"`
	CompatibilityClass   string   `json:"compatibility_class,omitempty"`
}

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: &http.Client{Timeout: 5 * time.Second}}
}

// ResolveActivity returns the currently-resolvable version of activityID.
// Forwards verified tenant and principal identity to authenticate against PRV-01.
func (c *Client) ResolveActivity(ctx context.Context, tenantID, principalID, activityID string) (*ActivityVersion, error) {
	return c.getActivity(ctx, tenantID, principalID, "/privacy/processing-activities/"+url.PathEscape(activityID))
}

// ResolveActivityVersion returns exactly the requested historical or current
// version (GET /privacy/processing-activities/{id}/versions/{versionID}); it
// never substitutes the current version for a different requested one (§13, I29).
func (c *Client) ResolveActivityVersion(ctx context.Context, tenantID, principalID, activityID, versionID string) (*ActivityVersion, error) {
	return c.getActivity(ctx, tenantID, principalID,
		"/privacy/processing-activities/"+url.PathEscape(activityID)+"/versions/"+url.PathEscape(versionID))
}

func (c *Client) getActivity(ctx context.Context, tenantID, principalID, path string) (*ActivityVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
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
func (c *Client) ResolvePurpose(ctx context.Context, tenantID, principalID, purposeID string) (*PurposeVersion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/privacy/purposes/"+url.PathEscape(purposeID), nil)
	if err != nil {
		return nil, err
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
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
