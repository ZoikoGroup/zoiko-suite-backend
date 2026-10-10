// Package payee reads the ORG-10 active payee destination
// (payee-banking-identity-svc GET /org10/parties/{ref}/active). It exists only
// so AP-05 can COMPARE invoice-supplied bank data with the governed
// destination; the invoice's own bank details are never used for payment.
package payee

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"zoiko.io/accounts-payable-svc/internal/svcclient"
)

var (
	ErrNoActiveDestination = errors.New("no active ORG-10 destination")
	ErrUnavailable         = errors.New("payee-banking-identity-svc unavailable")
)

type Destination struct {
	DestinationID     string
	AccountIdentifier string
	AccountLast4      string
	Currency          string
}

type Reader interface {
	Active(ctx context.Context, caller svcclient.Caller, partyRef string) (*Destination, error)
}

type HTTPClient struct{ c *svcclient.Client }

func NewHTTPClient(baseURL string) *HTTPClient { return &HTTPClient{c: svcclient.New(baseURL)} }

func (h *HTTPClient) Active(ctx context.Context, caller svcclient.Caller, partyRef string) (*Destination, error) {
	status, body, err := h.c.Do(ctx, caller, "GET", "/org10/parties/"+url.PathEscape(partyRef)+"/active", nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	switch status {
	case 200:
	case 404:
		return nil, ErrNoActiveDestination
	default:
		return nil, ErrUnavailable
	}
	// ORG-10 serialises its domain struct without json tags (PascalCase); accept
	// that and snake_case so a later tagging change does not break the check.
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, ErrUnavailable
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	d := &Destination{
		DestinationID:     str("DestinationID", "destination_id"),
		AccountIdentifier: str("AccountIdentifier", "account_identifier"),
		AccountLast4:      str("AccountLast4", "account_last4"),
		Currency:          str("Currency", "currency"),
	}
	if d.DestinationID == "" {
		return nil, ErrUnavailable
	}
	return d, nil
}
