package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrAuthzServiceUnavailable is returned when authorization-svc cannot be
	// reached, or responds with anything other than a well-formed decision.
	// Callers must treat this as a denial (fail closed).
	ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")

	// ErrAuthorizationDenied is returned when authorization-svc explicitly
	// denies the requested action.
	ErrAuthorizationDenied = errors.New("authorization denied")
)

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

// NewClientWithHTTPClient is NewClient but with a caller-supplied
// *http.Client — used for the mTLS pilot, where the client's Transport
// already carries this service's leaf certificate and trusts
// authorization-svc's CA (see internal/mtls.NewClientHTTPClient).
func NewClientWithHTTPClient(baseURL string, httpClient *http.Client) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
	}
}

// CheckAllowed asks authorization-svc whether principalID may perform
// actionType against legalEntityID. It fails closed: any transport error,
// non-200 response, malformed body, or decision other than "GRANTED" results
// in a non-nil error and the action must not proceed.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, _ := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ErrAuthzServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ErrAuthzServiceUnavailable
	}
	var res struct {
		DecisionOutcome string `json:"decision_outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}
	if res.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}
	return nil
}
