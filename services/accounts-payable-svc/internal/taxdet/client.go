// Package taxdet verifies tax/withholding determinations against
// tax-determination-svc. AP never calculates tax: it only checks that the
// determination an invoice line cites exists, belongs to this entity and
// currency, is in a usable status, and records which rule pack produced it.
package taxdet

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"zoiko.io/accounts-payable-svc/internal/svcclient"
)

var (
	ErrNotFound    = errors.New("tax determination not found")
	ErrUnavailable = errors.New("tax-determination-svc unavailable")
)

// Determination is the subset of tax-determination-svc's TaxDetermination AP
// needs (GET /v1/tax-determinations/{id}).
type Determination struct {
	DeterminationID     string  `json:"determination_id"`
	LegalEntityID       string  `json:"legal_entity_id"`
	RuleID              string  `json:"rule_id"`
	TaxLogicSnapshotID  *string `json:"tax_logic_snapshot_id"`
	Status              string  `json:"status"`
	CalculatedTaxAmount float64 `json:"calculated_tax_amount"`
	Currency            string  `json:"currency"`
}

func (d Determination) Snapshot() string {
	if d.TaxLogicSnapshotID == nil {
		return ""
	}
	return *d.TaxLogicSnapshotID
}

// Usable reports whether the determination can back an invoice: a REVERSED one
// no longer represents a tax position.
func (d Determination) Usable() bool {
	switch d.Status {
	case "CALCULATED", "APPLIED", "OVERRIDDEN":
		return true
	default:
		return false
	}
}

type Verifier interface {
	Get(ctx context.Context, caller svcclient.Caller, determinationID string) (*Determination, error)
}

type HTTPClient struct{ c *svcclient.Client }

func NewHTTPClient(baseURL string) *HTTPClient { return &HTTPClient{c: svcclient.New(baseURL)} }

func (h *HTTPClient) Get(ctx context.Context, caller svcclient.Caller, id string) (*Determination, error) {
	status, body, err := h.c.Do(ctx, caller, "GET", "/v1/tax-determinations/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	switch status {
	case 200:
	case 404:
		return nil, ErrNotFound
	default:
		return nil, ErrUnavailable
	}
	var d Determination
	if err := json.Unmarshal(body, &d); err != nil || d.DeterminationID == "" {
		return nil, ErrUnavailable
	}
	return &d, nil
}
