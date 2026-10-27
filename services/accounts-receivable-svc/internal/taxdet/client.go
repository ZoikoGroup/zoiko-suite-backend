// Package taxdet is the AR client for tax-determination-svc (TAX-03).
//
// Spec invariant 3: "Tax determination is consumed from the TAX domain with exact
// rule-pack/version provenance; AR does not improvise tax." AR therefore calls
// Determine to obtain a determination for a document line group and Get to
// re-verify one before issue (a tax result changed after approval must block
// issuance). It never computes a rate itself.
package taxdet

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

var (
	ErrNotFound    = errors.New("tax determination not found")
	ErrUnavailable = errors.New("tax-determination-svc unavailable")
	// ErrRejected is a 4xx on Determine: the request lacked an input the tax
	// domain requires (supply kind, place of supply, ...). The text is the TAX
	// domain's own explanation, safe to show the caller.
	ErrRejected = errors.New("tax-determination-svc rejected the determination request")
)

// Request is the subset of tax-determination-svc DetermineTaxRequest (TAX-03)
// that an AR document supplies. Amounts are decimal strings on our side and
// converted to the wire's JSON numbers here, at the boundary only.
type Request struct {
	TransactionID           string
	SourceModule            string // "AR"
	LegalEntityID           string
	JurisdictionID          string
	SupplyJurisdictionID    string
	TaxCategory             string
	GrossAmount             kit.Amount
	ExemptAmount            kit.Amount
	Currency                string
	EffectiveFrom           string // YYYY-MM-DD
	SupplyDate              string // tax point, YYYY-MM-DD
	SellerPartyID           string
	BuyerPartyID            string
	ProductClassification   string
	SupplyKind              string // GOODS | SERVICES | DIGITAL_SERVICES
	SupplyType              string // B2B | B2C | B2G
	BuyerTaxRegistrationID  string
	ExemptionReason         string
	ExemptionCertificateRef string
	EvaluatedBy             string
}

type wireRequest struct {
	TransactionID           string  `json:"transaction_id"`
	SourceModule            string  `json:"source_module"`
	LegalEntityID           string  `json:"legal_entity_id"`
	JurisdictionID          string  `json:"jurisdiction_id"`
	TaxCategory             string  `json:"tax_category"`
	GrossAmount             float64 `json:"gross_amount"`
	ExemptAmount            float64 `json:"exempt_amount,omitempty"`
	Currency                string  `json:"currency"`
	EffectiveFrom           string  `json:"effective_from"`
	EvaluatedBy             string  `json:"evaluated_by"`
	SellerPartyID           string  `json:"seller_party_id"`
	BuyerPartyID            string  `json:"buyer_party_id"`
	SupplyJurisdictionID    string  `json:"supply_jurisdiction_id"`
	SupplyDate              string  `json:"supply_date"`
	ProductClassification   string  `json:"product_classification"`
	SupplyKind              string  `json:"supply_kind"`
	SupplyType              string  `json:"supply_type"`
	BuyerTaxRegistrationID  string  `json:"buyer_tax_registration_id,omitempty"`
	ExemptionReason         string  `json:"exemption_reason,omitempty"`
	ExemptionCertificateRef string  `json:"exemption_certificate_ref,omitempty"`
}

// Determination is the subset of tax-determination-svc's TaxDetermination AR
// needs. Tax amounts are decimal strings parsed to exact Amounts.
type Determination struct {
	DeterminationID    string
	LegalEntityID      string
	RuleID             string
	TaxLogicSnapshotID string
	Status             string
	TaxableAmount      kit.Amount
	TaxRatePercentage  string
	Tax                kit.Amount
	GrossAmount        kit.Amount
	Currency           string
	JurisdictionID     string
	SupplyDate         string
}

type wireDetermination struct {
	DeterminationID     string      `json:"determination_id"`
	LegalEntityID       string      `json:"legal_entity_id"`
	JurisdictionID      string      `json:"jurisdiction_id"`
	RuleID              string      `json:"rule_id"`
	TaxLogicSnapshotID  *string     `json:"tax_logic_snapshot_id"`
	Status              string      `json:"status"`
	GrossAmount         json.Number `json:"gross_amount"`
	TaxableAmount       json.Number `json:"taxable_amount"`
	TaxRatePercentage   json.Number `json:"tax_rate_percentage"`
	CalculatedTaxAmount json.Number `json:"calculated_tax_amount"`
	Currency            string      `json:"currency"`
	SupplyDate          string      `json:"supply_date"`
}

// Usable reports whether the determination can back a document: a REVERSED one
// no longer represents a tax position.
func (d Determination) Usable() bool {
	switch d.Status {
	case "CALCULATED", "APPLIED", "OVERRIDDEN":
		return true
	default:
		return false
	}
}

// Provenance is the exact rule-pack reference stored on the document.
func (d Determination) Provenance() string {
	if d.TaxLogicSnapshotID != "" {
		return d.TaxLogicSnapshotID
	}
	return d.RuleID
}

// Client is what AR modules depend on.
type Client interface {
	Determine(ctx context.Context, caller svcclient.Caller, req Request) (*Determination, error)
	Get(ctx context.Context, caller svcclient.Caller, determinationID string) (*Determination, error)
}

type HTTPClient struct{ c *svcclient.Client }

func NewHTTPClient(baseURL string) *HTTPClient { return &HTTPClient{c: svcclient.New(baseURL)} }

func decode(body []byte) (*Determination, error) {
	var w wireDetermination
	if err := json.Unmarshal(body, &w); err != nil || w.DeterminationID == "" {
		return nil, ErrUnavailable
	}
	parse := func(n json.Number) kit.Amount {
		if n == "" {
			return 0
		}
		a, err := kit.ParseAmount(n.String())
		if err != nil {
			return 0
		}
		return a
	}
	d := &Determination{
		DeterminationID:   w.DeterminationID,
		LegalEntityID:     w.LegalEntityID,
		RuleID:            w.RuleID,
		Status:            w.Status,
		TaxableAmount:     parse(w.TaxableAmount),
		TaxRatePercentage: w.TaxRatePercentage.String(),
		Tax:               parse(w.CalculatedTaxAmount),
		GrossAmount:       parse(w.GrossAmount),
		Currency:          w.Currency,
		JurisdictionID:    w.JurisdictionID,
		SupplyDate:        w.SupplyDate,
	}
	if w.TaxLogicSnapshotID != nil {
		d.TaxLogicSnapshotID = *w.TaxLogicSnapshotID
	}
	return d, nil
}

func (h *HTTPClient) Determine(ctx context.Context, caller svcclient.Caller, req Request) (*Determination, error) {
	w := wireRequest{
		TransactionID: req.TransactionID, SourceModule: orDefault(req.SourceModule, "AR"),
		LegalEntityID: req.LegalEntityID, JurisdictionID: req.JurisdictionID, TaxCategory: req.TaxCategory,
		GrossAmount: req.GrossAmount.Float(), ExemptAmount: req.ExemptAmount.Float(), Currency: req.Currency,
		EffectiveFrom: req.EffectiveFrom, EvaluatedBy: orDefault(req.EvaluatedBy, caller.PrincipalID),
		SellerPartyID: req.SellerPartyID, BuyerPartyID: req.BuyerPartyID,
		SupplyJurisdictionID: orDefault(req.SupplyJurisdictionID, req.JurisdictionID), SupplyDate: req.SupplyDate,
		ProductClassification: req.ProductClassification, SupplyKind: req.SupplyKind, SupplyType: req.SupplyType,
		BuyerTaxRegistrationID: req.BuyerTaxRegistrationID, ExemptionReason: req.ExemptionReason,
		ExemptionCertificateRef: req.ExemptionCertificateRef,
	}
	status, body, err := h.c.Do(ctx, caller, "POST", "/v1/tax-determinations", w)
	if err != nil {
		return nil, ErrUnavailable
	}
	switch {
	case status == 200 || status == 201:
		return decode(body)
	case status >= 400 && status < 500:
		return nil, errors.Join(ErrRejected, errors.New(string(body)))
	default:
		return nil, ErrUnavailable
	}
}

func (h *HTTPClient) Get(ctx context.Context, caller svcclient.Caller, id string) (*Determination, error) {
	status, body, err := h.c.Do(ctx, caller, "GET", "/v1/tax-determinations/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	switch status {
	case 200:
		return decode(body)
	case 404:
		return nil, ErrNotFound
	default:
		return nil, ErrUnavailable
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
