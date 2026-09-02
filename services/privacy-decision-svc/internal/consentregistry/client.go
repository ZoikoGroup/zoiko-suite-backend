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
	Status        string `json:"status"`
	LatestReceipt *struct {
		ConsentReceiptID string `json:"consent_receipt_id"`
	} `json:"latest_receipt,omitempty"`
}

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: &http.Client{Timeout: 5 * time.Second}}
}

// ResolveStatus calls GET /privacy/consents?subject_ref=&purpose_id= —
// always 200, distinguishing NOT_REQUESTED/GRANTED/DENIED/WITHDRAWN by
// the response body, same contract PRV-02 documents for that route.
func (c *Client) ResolveStatus(ctx context.Context, subjectRef, purposeID string) (*ConsentResolution, error) {
	u := c.baseURL + "/privacy/consents?subject_ref=" + url.QueryEscape(subjectRef) + "&purpose_id=" + url.QueryEscape(purposeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
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
