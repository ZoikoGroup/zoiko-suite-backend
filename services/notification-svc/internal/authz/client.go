// Package authz calls authorization-svc and fails closed.
//
// It lives here rather than inline in cmd/server because a client that only
// exists in package main cannot be tested: every handler test replaces it with
// a double, so nothing ever exercised the parsing. That is precisely how
// financial-close-svc shipped a client decoding an "allowed" boolean field
// authorization-svc has never sent — the field was always absent, always
// false, and every authorization check denied. A fail-closed check that always
// fails closed looks exactly like a permission nobody granted, from both
// sides. See client_test.go, which drives a real server with the real shape.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
)

type Client struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

func NewClient(baseURL string, log *zap.Logger) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 5 * time.Second},
		log:     log,
	}
}

type authorizeRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

// authorizeResponse is the shape authorization-svc actually sends: it always
// answers 200 and signals the decision through decision_outcome
// ("GRANTED" | "DENIED"). There is no boolean field.
type authorizeResponse struct {
	DecisionOutcome string `json:"decision_outcome"`
}

// CheckAllowed returns nil only on an explicit GRANTED. Every other outcome —
// a denial, a transport failure, a non-200, an undecodable body, or a decision
// string this client does not recognise — is an error, and callers must treat
// all of them as a refusal.
func (c *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	body, err := json.Marshal(authorizeRequest{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/authorize", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		c.log.Error("failed to call authorization-svc", zap.Error(err))
		return domain.ErrAuthzServiceUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return domain.ErrAuthzServiceUnavailable
	}

	var res authorizeResponse
	// A decode error used to be returned raw, which the handler's error
	// mapping did not recognise as either a denial or an outage — so a
	// malformed response produced a 503 with the JSON parser's message in it.
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return domain.ErrAuthzServiceUnavailable
	}

	switch res.DecisionOutcome {
	case "GRANTED":
		return nil
	case "DENIED":
		return domain.ErrAuthorizationDenied
	default:
		// Includes the empty string — the zero value, which is what a renamed
		// field or a changed envelope looks like from here. Refused as an
		// unusable answer rather than reported as a denial, because it is not
		// a decision this service was given.
		return fmt.Errorf("%w: unrecognised decision_outcome %q",
			domain.ErrAuthzServiceUnavailable, res.DecisionOutcome)
	}
}
