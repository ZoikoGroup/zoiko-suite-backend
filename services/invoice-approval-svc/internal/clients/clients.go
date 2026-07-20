package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"zoiko.io/invoice-approval-svc/internal/domain"
)

type Clients struct {
	apURL       string
	workflowURL string
	http        *http.Client
	log         *zap.Logger
}

func New(apURL, workflowURL string, log *zap.Logger) *Clients {
	return &Clients{
		apURL:       apURL,
		workflowURL: workflowURL,
		http:        &http.Client{Timeout: 5 * time.Second},
		log:         log,
	}
}

type APInvoice struct {
	InvoiceID     string  `json:"invoice_id"`
	Amount        float64 `json:"amount"`
	CurrencyCode  string  `json:"currency_code"`
	Status        string  `json:"status"`
	LegalEntityID string  `json:"legal_entity_id"`
}

func (c *Clients) FetchInvoice(ctx context.Context, tenantID, invoiceID string) (*APInvoice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v1/invoices/%s", c.apURL, invoiceID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to query accounts-payable-svc", zap.Error(err))
		return nil, domain.ErrAPServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrRequestNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, domain.ErrAPServiceUnavailable
	}

	var inv APInvoice
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		return nil, err
	}
	return &inv, nil
}

func (c *Clients) StartWorkflowInstance(ctx context.Context, tenantID, invoiceID, principalID string) (string, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"workflow_definition": "invoice-approval-v1",
		"reference_id":        invoiceID,
		"initiated_by":        principalID,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.workflowURL+"/v1/workflows/instances", bytes.NewReader(reqBody))
	if err != nil {
		return uuid.NewString(), nil // fallback to generated UUID if workflow endpoint unavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("workflow-svc unavailable — generating internal workflow_instance_id", zap.Error(err))
		return uuid.NewString(), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return uuid.NewString(), nil
	}

	var res struct {
		WorkflowInstanceID string `json:"workflow_instance_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err == nil && res.WorkflowInstanceID != "" {
		return res.WorkflowInstanceID, nil
	}
	return uuid.NewString(), nil
}