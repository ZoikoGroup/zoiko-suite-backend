// Package transferregistry is a real HTTP client to privacy-transfer-svc (PRV-05),
// used to evaluate transfer conditions and authorization (§12.1, §13 step 4, §16-17).
package transferregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("privacy-transfer-svc unavailable")

type EvaluateTransferRequest struct {
	TenantID                string `json:"tenant_id,omitempty"`
	RelationshipID          string `json:"relationship_id"`
	TransferMechanismID     string `json:"transfer_mechanism_id"`
	DestinationJurisdiction string `json:"destination_jurisdiction,omitempty"`
	AssessmentRequired      bool   `json:"assessment_required"`
}

type TransferDecision struct {
	DecisionID              string   `json:"decision_id"`
	Result                  string   `json:"result"` // AUTHORIZED, CONDITIONAL, BLOCKED, REVIEW_REQUIRED
	ReasonCodes             []string `json:"reason_codes"`
	TransferMechanismID     string   `json:"transfer_mechanism_id"`
	RelationshipID          string   `json:"relationship_id"`
	DestinationJurisdiction string   `json:"destination_jurisdiction,omitempty"`
}

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

// EvaluateTransfer calls POST /privacy/transfer-decisions in PRV-05.
func (c *Client) EvaluateTransfer(ctx context.Context, tenantID, principalID string, req *EvaluateTransferRequest) (*TransferDecision, error) {
	if req == nil {
		return nil, errors.New("transfer request is nil")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/privacy/transfer-decisions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		httpReq.Header.Set("X-Principal-Id", principalID)
	}
	if tenantID != "" {
		httpReq.Header.Set("X-Tenant-Id", tenantID)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}

	var decision TransferDecision
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		return nil, ErrUnavailable
	}
	return &decision, nil
}
