// Package consentregistry is a real HTTP client to privacy-consent-svc
// (PRV-02), used to enforce PRV-C04 (consent/withdrawal) with actual
// consent evidence, not a caller-supplied claim.
package consentregistry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("privacy-consent-svc unavailable")

type ConsentResolution struct {
	SubjectRef    string   `json:"subject_ref"`
	PurposeID     string   `json:"purpose_id"`
	Status        string   `json:"status"` // NOT_REQUESTED, GRANTED, DENIED, WITHDRAWN, EXPIRED, INVALIDATED
	LatestReceipt *Receipt `json:"latest_receipt,omitempty"`
}

// Receipt is the consent receipt evidence returned for the latest consent.
type Receipt struct {
	ConsentReceiptID string     `json:"consent_receipt_id"`
	NoticeVersionID  string     `json:"notice_version_id,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: &http.Client{Timeout: 5 * time.Second}}
}

// ResolveStatus calls GET /privacy/consents?subject_ref=&purpose_id=.
// Both tenantID and principalID are forwarded via X-Tenant-Id and X-Principal-Id
// so privacy-consent-svc can authenticate and scope the request under RLS.
func (c *Client) ResolveStatus(ctx context.Context, tenantID, principalID, subjectRef, purposeID string) (*ConsentResolution, error) {
	u := c.baseURL + "/privacy/consents?subject_ref=" + url.QueryEscape(subjectRef) + "&purpose_id=" + url.QueryEscape(purposeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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

	if resp.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}
	var res ConsentResolution
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, ErrUnavailable
	}
	return &res, nil
}
