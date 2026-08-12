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

// Sentinel errors returned by CheckAllowed. Callers must fail closed: any
// error from CheckAllowed (denial or unavailability) means the caller must
// refuse the write.
var (
	// ErrAuthorizationDenied is returned when authorization-svc explicitly
	// denies the requested action.
	ErrAuthorizationDenied = errors.New("authorization denied")
	// ErrAuthzServiceUnavailable is returned when authorization-svc cannot be
	// reached, times out, or returns a non-200 response. Callers must treat
	// this the same as a denial (fail closed).
	ErrAuthzServiceUnavailable = errors.New("authorization service unavailable")
)

// Client calls the Authorization Service to check permissions.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a new authorization client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

type checkAllowedRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

type checkAllowedResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

// CheckAllowed calls authorization-svc's POST /v1/authorize endpoint and
// fails closed: any transport error, non-200 response, decode error, or a
// decision other than "GRANTED" results in a non-nil error.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, err := json.Marshal(checkAllowedRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport failure — fail closed.
		return ErrAuthzServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Ambiguous/unexpected response — fail closed.
		return ErrAuthzServiceUnavailable
	}

	var res checkAllowedResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return ErrAuthzServiceUnavailable
	}

	if res.DecisionOutcome != "GRANTED" {
		return ErrAuthorizationDenied
	}
	return nil
}
