// BNK-01's ListConnectionOptions query — Wave 10d of the gap-remediation
// plan. The doc lists ListConnectionOptions under BNK-01's query row, but
// "supported connection/payment options; scheme/routing metadata" is
// BNK-02 connection data, which this service has never owned and doesn't
// start owning now. This is a thin, fail-closed read against
// banking-connector-svc's real connection list — same narrow-client idiom
// as payment-initiation-adapter-svc's internal/clients/treasury.go and
// banking-connector-svc's own internal/clients/bankaccount.go.
package clients

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zoiko.io/treasury-svc/internal/domain"
)

// ErrBankingConnectorUnavailable is returned on any network error, decode
// failure, or non-200 response — fail closed, never presented as "no
// connections configured."
var ErrBankingConnectorUnavailable = errors.New("banking-connector-svc connection lookup unavailable")

// bankConnection is the subset of banking-connector-svc's
// domain.BankConnection this read needs — reproduced field-for-field
// (that service's JSON tags), not guessed.
type bankConnection struct {
	ConnectionID  string   `json:"connection_id"`
	BankAccountID string   `json:"bank_account_id"`
	ProviderRef   string   `json:"provider_ref"`
	Status        string   `json:"status"`
	HealthStatus  string   `json:"health_status"`
	Region        string   `json:"region"`
	Currency      string   `json:"currency"`
	ConsentScope  []string `json:"consent_scope"`
}

type listConnectionsResponse struct {
	Connections []bankConnection `json:"connections"`
}

// BankingConnectorClient is the narrow interface ListConnectionOptions
// depends on.
type BankingConnectorClient interface {
	ListConnectionOptions(ctx context.Context, tenantID, legalEntityID, bankAccountID, correlationID string) ([]domain.ConnectionOption, error)
}

type BankingConnectorHTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewBankingConnectorHTTPClient(baseURL string) *BankingConnectorHTTPClient {
	return &BankingConnectorHTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 3 * time.Second}}
}

// ListConnectionOptions lists the connections banking-connector-svc has
// on file for bankAccountID. banking-connector-svc's own ListConnections
// only filters by legal_entity_id server-side (no bank_account_id
// filter exists there), so the account-scoping happens here, client-side
// — never returning a connection that doesn't genuinely belong to this
// account.
func (c *BankingConnectorHTTPClient) ListConnectionOptions(ctx context.Context, tenantID, legalEntityID, bankAccountID, correlationID string) ([]domain.ConnectionOption, error) {
	u, err := url.Parse(c.baseURL + "/v1/banking/connections")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("legal_entity_id", legalEntityID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ErrBankingConnectorUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrBankingConnectorUnavailable
	}
	var parsed listConnectionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, ErrBankingConnectorUnavailable
	}

	options := make([]domain.ConnectionOption, 0, len(parsed.Connections))
	for _, c := range parsed.Connections {
		if c.BankAccountID != bankAccountID {
			continue
		}
		options = append(options, domain.ConnectionOption{
			ConnectionID: c.ConnectionID, ProviderRef: c.ProviderRef, Status: c.Status,
			HealthStatus: c.HealthStatus, Region: c.Region, Currency: c.Currency, ConsentScope: c.ConsentScope,
		})
	}
	return options, nil
}
