package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrTaxRuleNotFound     = errors.New("no active tax rule found matching jurisdiction and category")
	ErrTaxRulesUnavailable = errors.New("tax-rules-svc is unavailable")
)

type TaxRuleDTO struct {
	RuleID             string  `json:"rule_id"`
	JurisdictionID     string  `json:"jurisdiction_id"`
	RuleCode           string  `json:"rule_code"`
	Name               string  `json:"name"`
	Category           string  `json:"category"`
	TaxRatePercentage  float64 `json:"tax_rate_percentage"`
	StandardDeductions float64 `json:"standard_deductions"`
	Status             string  `json:"status"`
}

// Caller is the identity of the request this lookup is made on behalf of.
//
// tax-rules-svc refuses a read without X-Tenant-Id (401 missing_tenant_scope)
// and without X-Principal-Id (401 "X-Principal-Id header is required"), so the
// determination must forward the identity it was itself called with. Sending
// only the tenant made every determination fail closed with "tax-rules-svc
// could not be reached" even though tax-rules was up.
type Caller struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string
	CorrelationID string
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

// FetchActiveRule calls GET /v1/tax-rules?jurisdiction_id=..&category=..&status=ACTIVE
// on tax-rules-svc with the caller's identity envelope, and returns the first
// ACTIVE rule. A 404 or an empty list is ErrTaxRuleNotFound; any other non-200
// (including 401/403) is ErrTaxRulesUnavailable so the caller fails closed.
func (c *Client) FetchActiveRule(ctx context.Context, caller Caller, jurisdictionID, category string) (*TaxRuleDTO, error) {
	u := fmt.Sprintf("%s/v1/tax-rules?jurisdiction_id=%s&category=%s&status=ACTIVE",
		c.baseURL, url.QueryEscape(jurisdictionID), url.QueryEscape(category))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	setCallerEnvelope(req, caller)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTaxRulesUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrTaxRuleNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: tax-rules-svc returned %d", ErrTaxRulesUnavailable, resp.StatusCode)
	}

	var payload struct {
		Rules []TaxRuleDTO `json:"rules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode tax rules response: %w", err)
	}

	if len(payload.Rules) > 0 {
		return &payload.Rules[0], nil
	}

	return nil, ErrTaxRuleNotFound
}

// setCallerEnvelope writes the platform identity headers for a
// service-to-service read. X-Source-Channel is "system" because this call is
// made by tax-determination-svc, not typed by a user; a fresh X-Request-Id
// identifies this hop while X-Correlation-ID ties it to the original request.
func setCallerEnvelope(req *http.Request, caller Caller) {
	req.Header.Set("X-Tenant-Id", caller.TenantID)
	req.Header.Set("X-Principal-Id", caller.PrincipalID)
	if caller.LegalEntityID != "" {
		req.Header.Set("X-Legal-Entity-Id", caller.LegalEntityID)
	}
	if caller.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", caller.CorrelationID)
	}
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
}
