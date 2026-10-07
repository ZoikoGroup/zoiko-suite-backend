package banking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

var (
	ErrTransactionNotFound = errors.New("canonical transaction not found")
	ErrUnavailable         = errors.New("banking-connector-svc unavailable")
)

type CanonicalTransaction struct {
	TransactionID   string  `json:"transaction_id"`
	TenantID        string  `json:"tenant_id"`
	StatementLineID string  `json:"statement_line_id"`
	MappingVersion  int     `json:"mapping_version"`
	Amount          float64 `json:"amount"`
	Currency        string  `json:"currency"`
	Status          string  `json:"status"`
}

type Client interface {
	GetCanonicalTransaction(ctx context.Context, tenantID, transactionID string) (*CanonicalTransaction, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 3 * time.Second},
	}
}

func (c *HTTPClient) GetCanonicalTransaction(ctx context.Context, tenantID, transactionID string) (*CanonicalTransaction, error) {
	reqURL := fmt.Sprintf("%s/v1/banking/canonical-transactions/%s", c.baseURL, url.PathEscape(transactionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrTransactionNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("%w: status %d: %s", ErrUnavailable, resp.StatusCode, body)
	}
	var t CanonicalTransaction
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	return &t, nil
}
