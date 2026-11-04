package commercial

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	idenv "zoiko.io/identity-envelope"
)

var (
	ErrNotFound    = errors.New("commercial account not found")
	ErrUnavailable = errors.New("commercial account service unavailable")
)

type Client struct {
	baseURL  string
	http     *http.Client
	exchanger *idenv.Exchanger
}

type CommercialAccount struct {
	CommercialAccountID string `json:"commercial_account_id"`
	OrganizationID      string `json:"organization_id"`
	Status              string `json:"status"`
}

type Subscription struct {
	SubscriptionID      string `json:"subscription_id"`
	OrganizationID      string `json:"organization_id"`
	CommercialAccountID string `json:"commercial_account_id"`
	ProductID           string `json:"product_id"`
	LifecycleStatus     string `json:"lifecycle_status"`
}

func New(baseURL string) *Client {
	if baseURL == "" {
		return nil
	}
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 2 * time.Second},
	}
}

func NewWithExchanger(baseURL string, ex *idenv.Exchanger) *Client {
	c := New(baseURL)
	if c != nil {
		c.exchanger = ex
	}
	return c
}

func (c *Client) Enabled() bool { return c != nil }

func (c *Client) GetCommercialPlan(ctx context.Context, tenantID string) (plan, orgUnit string, err error) {
	if c == nil {
		return "", "", ErrUnavailable
	}

	// Try to get commercial account by organization ID (tenant ID)
	acct, err := c.getCommercialAccountByOrganization(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", "", ErrNotFound
		}
		return "", "", err
	}

	// Get active subscription for this commercial account
	sub, err := c.getActiveSubscription(ctx, acct.CommercialAccountID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", acct.OrganizationID, nil // Have org unit but no plan
		}
		return "", "", err
	}

	return sub.ProductID, acct.OrganizationID, nil
}

func (c *Client) getCommercialAccountByOrganization(ctx context.Context, organizationID string) (*CommercialAccount, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/commercial-accounts/by-organization/"+organizationID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.exchanger != nil {
		_ = idenv.ForwardTo(ctx, req, c.exchanger, "commercial-account-svc")
	} else {
		idenv.Forward(ctx, req)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var acct CommercialAccount
		if err := json.NewDecoder(resp.Body).Decode(&acct); err != nil {
			return nil, err
		}
		return &acct, nil
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, ErrUnavailable
	}
}

func (c *Client) getActiveSubscription(ctx context.Context, commercialAccountID string) (*Subscription, error) {
	// Try to get subscription by commercial account ID
	// Note: This endpoint may not exist yet; if it fails, we return ErrNotFound
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/commercial-accounts/"+commercialAccountID+"/subscription", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.exchanger != nil {
		_ = idenv.ForwardTo(ctx, req, c.exchanger, "commercial-account-svc")
	} else {
		idenv.Forward(ctx, req)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var sub Subscription
		if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
			return nil, err
		}
		return &sub, nil
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, ErrUnavailable
	}
}