package ar01

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// Party is the slice of the party master AR-01 needs to decide whether a
// customer may carry a financial profile. Identity and address are deliberately
// not carried: AR-01 never duplicates the master.
type Party struct {
	PartyID     string
	TenantID    string
	Type        string // CUSTOMER, VENDOR, ...
	Status      string // ONBOARDING, ACTIVE, SUSPENDED, TERMINATED
	EffectiveTo string
}

// ErrPartyNotFound means the master was reached and has no such party for the tenant.
var ErrPartyNotFound = errors.New("party not found")

// PartyDirectory is the party/customer master port. An error other than
// ErrPartyNotFound means the master could not answer and callers fail closed (503).
type PartyDirectory interface {
	GetParty(ctx context.Context, c kit.Caller, partyID string) (*Party, error)
}

// CounterpartyDirectory is the real client for counterparty-management-svc
// (GET /v1/counterparties/{id}; status ONBOARDING|ACTIVE|SUSPENDED|TERMINATED,
// counterparty_type CUSTOMER|VENDOR|...).
type CounterpartyDirectory struct{ client *svcclient.Client }

// NewCounterpartyDirectory builds the client for the given base URL.
func NewCounterpartyDirectory(baseURL string) *CounterpartyDirectory {
	return &CounterpartyDirectory{client: svcclient.New(baseURL)}
}

// NewCounterpartyDirectoryWith uses a prepared svcclient (tests, custom transport).
func NewCounterpartyDirectoryWith(c *svcclient.Client) *CounterpartyDirectory {
	return &CounterpartyDirectory{client: c}
}

type counterpartyWire struct {
	CounterpartyID   string  `json:"counterparty_id"`
	TenantID         string  `json:"tenant_id"`
	CounterpartyType string  `json:"counterparty_type"`
	Status           string  `json:"status"`
	EffectiveTo      *string `json:"effective_to"`
}

func (d *CounterpartyDirectory) GetParty(ctx context.Context, c kit.Caller, partyID string) (*Party, error) {
	status, body, err := d.client.Do(ctx, c.SvcCaller(), http.MethodGet, "/v1/counterparties/"+url.PathEscape(partyID), nil)
	if err != nil {
		return nil, svcclient.ErrUnavailable
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrPartyNotFound
	default:
		return nil, svcclient.ErrUnavailable
	}
	var w counterpartyWire
	if json.Unmarshal(body, &w) != nil || w.CounterpartyID == "" || w.Status == "" || w.CounterpartyType == "" {
		return nil, svcclient.ErrUnavailable
	}
	p := &Party{PartyID: w.CounterpartyID, TenantID: w.TenantID, Type: w.CounterpartyType, Status: w.Status}
	if w.EffectiveTo != nil {
		p.EffectiveTo = *w.EffectiveTo
	}
	return p, nil
}
