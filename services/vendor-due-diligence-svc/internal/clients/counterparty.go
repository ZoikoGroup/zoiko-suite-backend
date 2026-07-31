package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// CounterpartyClient calls counterparty-management-svc directly, pushing a
// completed vendor.dd check's outcome onto the counterparty's record.
//
// counterparty-management-svc's own handler never actually calls its authz
// client on any write (checked directly in its handler.go — CreateCounterparty,
// UpdateCounterparty, and UpdateComplianceStatus all skip the check entirely).
// That is a real gap in that service, tracked separately — not something
// this client works around, since being correct on our own side doesn't
// depend on the other side's bug. Headers are still sent as if it enforced
// them, for when that gap is fixed.
type CounterpartyClient struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *CounterpartyClient {
	return &CounterpartyClient{baseURL: baseURL, http: &http.Client{Timeout: 5 * time.Second}}
}

// UpdateComplianceStatus calls POST /{id}/compliance — the only field this
// endpoint accepts is compliance_status (VERIFIED, PENDING, or REJECTED);
// risk_category is a separate call, see UpdateRiskCategory.
func (c *CounterpartyClient) UpdateComplianceStatus(ctx context.Context, tenantID, counterpartyID, complianceStatus string) error {
	body, _ := json.Marshal(map[string]string{
		"compliance_status": complianceStatus,
		"updated_by":        "vendor-due-diligence-svc",
	})
	return c.post(ctx, tenantID, fmt.Sprintf("/v1/counterparties/%s/compliance", counterpartyID), body)
}

// UpdateRiskCategory calls PUT /{id} — counterparty-management-svc has no
// dedicated risk-only endpoint, so a risk change goes through the general
// update route alongside every other editable field, sending only the one
// field being changed (its handler treats empty fields as "leave unchanged").
func (c *CounterpartyClient) UpdateRiskCategory(ctx context.Context, tenantID, counterpartyID, riskCategory string) error {
	body, _ := json.Marshal(map[string]string{
		"risk_category": riskCategory,
		"updated_by":    "vendor-due-diligence-svc",
	})
	return c.put(ctx, tenantID, fmt.Sprintf("/v1/counterparties/%s", counterpartyID), body)
}

func (c *CounterpartyClient) post(ctx context.Context, tenantID, path string, body []byte) error {
	return c.do(ctx, http.MethodPost, tenantID, path, body)
}

func (c *CounterpartyClient) put(ctx context.Context, tenantID, path string, body []byte) error {
	return c.do(ctx, http.MethodPut, tenantID, path, body)
}

func (c *CounterpartyClient) do(ctx context.Context, method, tenantID, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("counterparty-management-svc unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("counterparty-management-svc returned %d for %s %s", resp.StatusCode, method, path)
	}
	return nil
}
