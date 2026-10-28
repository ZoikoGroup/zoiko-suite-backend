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

	"zoiko.io/bank-reconciliation-svc/internal/envelope"
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

// Client reads BNK-04 canonical transactions from banking-connector-svc.
//
// principalID is the already-resolved caller this read is made on behalf
// of. banking-connector-svc's GET /v1/banking/canonical-transactions/{id}
// refuses a request with no X-Principal-Id (401), so it is a required
// argument rather than something the client could silently omit — an
// omitted principal used to surface as "banking connector unavailable" on
// every canonical match.
type Client interface {
	GetCanonicalTransaction(ctx context.Context, tenantID, principalID, transactionID string) (*CanonicalTransaction, error)
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

func (c *HTTPClient) GetCanonicalTransaction(ctx context.Context, tenantID, principalID, transactionID string) (*CanonicalTransaction, error) {
	if tenantID == "" || principalID == "" {
		// Fail closed before the call rather than send a request the
		// connector can only refuse — and never as an anonymous read.
		return nil, fmt.Errorf("%w: tenant and principal are required", ErrUnavailable)
	}
	reqURL := fmt.Sprintf("%s/v1/banking/canonical-transactions/%s", c.baseURL, url.PathEscape(transactionID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	// The §4 envelope, propagated the same way envelope.Resolver does for its
	// own outbound call: the inbound correlation and request ids carry
	// through so the connector's read is traceable to the match that caused
	// it, and the channel is "system" because this service, not the end
	// user, is the direct caller.
	req.Header.Set(envelope.HeaderTenantID, tenantID)
	req.Header.Set(envelope.HeaderActorSubjectID, principalID)
	if e, ok := envelope.FromContext(ctx); ok {
		if e.CorrelationID != "" {
			req.Header.Set(envelope.HeaderCorrelationID, e.CorrelationID)
		}
		if e.RequestID != "" {
			req.Header.Set(envelope.HeaderRequestID, e.RequestID)
		}
	}
	req.Header.Set(envelope.HeaderSourceChannel, string(envelope.ChannelSystem))
	req.Header.Set("Accept", "application/json")
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
