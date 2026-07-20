package clients

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"go.uber.org/zap"
	"zoiko.io/treasury-svc/internal/domain"
)

type Clients struct {
	apURL        string
	arURL        string
	obligationsURL string
	http         *http.Client
	log          *zap.Logger
}

func New(apURL, arURL, obligationsURL string, log *zap.Logger) *Clients {
	return &Clients{
		apURL:        apURL,
		arURL:        arURL,
		obligationsURL: obligationsURL,
		http:         &http.Client{Timeout: 3 * time.Second},
		log:          log,
	}
}

type apInvoice struct {
	InvoiceID     string    `json:"invoice_id"`
	Amount        float64   `json:"amount"`
	CurrencyCode  string    `json:"currency_code"`
	Status        string    `json:"status"`
	LegalEntityID string    `json:"legal_entity_id"`
	DueDate       time.Time `json:"due_date"`
}

type arInvoice struct {
	InvoiceID     string    `json:"invoice_id"`
	Amount        float64   `json:"amount"`
	CurrencyCode  string    `json:"currency_code"`
	Status        string    `json:"status"`
	LegalEntityID string    `json:"legal_entity_id"`
	DueDate       time.Time `json:"due_date"`
}

type obligation struct {
	ObligationID     string    `json:"obligation_id"`
	LegalEntityID    string    `json:"legal_entity_id"`
	ObligationType   string    `json:"obligation_type"`
	ObligationStatus string    `json:"obligation_status"`
	SourceReference  string    `json:"source_reference"`
	DueDate          time.Time `json:"due_date"`
}

// GetPendingAPCommitments gets the sum of AP commitments (validated, approved, payment_requested)
func (c *Clients) GetPendingAPCommitments(ctx context.Context, tenantID, legalEntityID, currencyCode string) (float64, error) {
	u, err := url.Parse(c.apURL + "/v1/invoices")
	if err != nil {
		return 0, err
	}
	q := u.Query()
	q.Set("tenant_id", tenantID)
	q.Set("legal_entity_id", legalEntityID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to query AP service", zap.Error(err))
		return 0, domain.ErrAPServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.Error("AP service returned non-200 status", zap.Int("status", resp.StatusCode))
		return 0, domain.ErrAPServiceUnavailable
	}

	var list []apInvoice
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return 0, err
	}

	var sum float64
	for _, inv := range list {
		if inv.CurrencyCode == currencyCode && (inv.Status == "VALIDATED" || inv.Status == "APPROVED" || inv.Status == "PAYMENT_REQUESTED") {
			sum += inv.Amount
		}
	}
	return sum, nil
}

// GetOutstandingObligations queries obligations service and parses numeric values from SourceReference
func (c *Clients) GetOutstandingObligations(ctx context.Context, tenantID, legalEntityID, currencyCode string) (float64, float64, error) {
	u, err := url.Parse(c.obligationsURL + "/v1/obligations")
	if err != nil {
		return 0, 0, err
	}
	q := u.Query()
	q.Set("legal_entity_id", legalEntityID)
	q.Set("status", "OPEN") // or IN_PROGRESS/OVERDUE
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to query Obligations service", zap.Error(err))
		return 0, 0, domain.ErrObligationsUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return 0, 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("Obligations service returned non-200 status", zap.Int("status", resp.StatusCode))
		return 0, 0, domain.ErrObligationsUnavailable
	}

	var list []obligation
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return 0, 0, err
	}

	var payrollSum, taxSum float64
	re := regexp.MustCompile(`\d+(?:\.\d+)?`)

	for _, o := range list {
		if o.ObligationStatus == "CLOSED" {
			continue
		}
		// Extract value from SourceReference or ObligationCode
		matches := re.FindStringSubmatch(o.SourceReference)
		if len(matches) == 0 {
			matches = re.FindStringSubmatch(o.ObligationID) // Fallback to parsing UUID numeric prefix if any
		}
		var val float64
		if len(matches) > 0 {
			val, _ = strconv.ParseFloat(matches[0], 64)
		}

		if o.ObligationType == "TAX_PAYMENT" {
			taxSum += val
		} else if o.ObligationType == "FILING" || o.ObligationType == "REGULATORY_REPORT" {
			// Other regulatory/payroll obligations
			payrollSum += val
		}
	}

	return payrollSum, taxSum, nil
}

// GetForecastedInflows aggregates outstanding customer receivables (issued, sent, overdue status)
func (c *Clients) GetForecastedInflows(ctx context.Context, tenantID, legalEntityID, currencyCode string) (float64, error) {
	u, err := url.Parse(c.arURL + "/v1/invoices")
	if err != nil {
		return 0, err
	}
	q := u.Query()
	q.Set("tenant_id", tenantID)
	q.Set("legal_entity_id", legalEntityID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to query AR service", zap.Error(err))
		return 0, domain.ErrARServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.Error("AR service returned non-200 status", zap.Int("status", resp.StatusCode))
		return 0, domain.ErrARServiceUnavailable
	}

	var list []arInvoice
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return 0, err
	}

	var sum float64
	for _, inv := range list {
		if inv.CurrencyCode == currencyCode && (inv.Status == "ISSUED" || inv.Status == "SENT" || inv.Status == "OVERDUE") {
			sum += inv.Amount
		}
	}
	return sum, nil
}

// GetLiquidityForecastData queries AP, AR, and Obligations services and builds list of expected inflows and outflows
func (c *Clients) GetLiquidityForecastData(ctx context.Context, tenantID, legalEntityID, currencyCode string) ([]domain.ExpectedCashFlow, []domain.ExpectedCashFlow, error) {
	var inflows []domain.ExpectedCashFlow
	var outflows []domain.ExpectedCashFlow

	// 1. Get AR invoices
	uAR, err := url.Parse(c.arURL + "/v1/invoices")
	if err == nil {
		q := uAR.Query()
		q.Set("tenant_id", tenantID)
		q.Set("legal_entity_id", legalEntityID)
		uAR.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, uAR.String(), nil)
		if err == nil {
			req.Header.Set("X-Tenant-Id", tenantID)
			resp, err := c.http.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				var list []arInvoice
				if json.NewDecoder(resp.Body).Decode(&list) == nil {
					for _, inv := range list {
						if inv.CurrencyCode == currencyCode && (inv.Status == "ISSUED" || inv.Status == "SENT" || inv.Status == "OVERDUE") {
							inflows = append(inflows, domain.ExpectedCashFlow{
								Amount:   inv.Amount,
								DueDate:  inv.DueDate,
								Category: "RECEIVABLE",
							})
						}
					}
				}
				resp.Body.Close()
			}
		}
	}

	// 2. Get AP invoices
	uAP, err := url.Parse(c.apURL + "/v1/invoices")
	if err == nil {
		q := uAP.Query()
		q.Set("tenant_id", tenantID)
		q.Set("legal_entity_id", legalEntityID)
		uAP.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, uAP.String(), nil)
		if err == nil {
			req.Header.Set("X-Tenant-Id", tenantID)
			resp, err := c.http.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				var list []apInvoice
				if json.NewDecoder(resp.Body).Decode(&list) == nil {
					for _, inv := range list {
						if inv.CurrencyCode == currencyCode && (inv.Status == "VALIDATED" || inv.Status == "APPROVED" || inv.Status == "PAYMENT_REQUESTED") {
							outflows = append(outflows, domain.ExpectedCashFlow{
								Amount:   inv.Amount,
								DueDate:  inv.DueDate,
								Category: "PAYABLE",
							})
						}
					}
				}
				resp.Body.Close()
			}
		}
	}

	// 3. Get Obligations
	uOB, err := url.Parse(c.obligationsURL + "/v1/obligations")
	if err == nil {
		q := uOB.Query()
		q.Set("legal_entity_id", legalEntityID)
		uOB.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, uOB.String(), nil)
		if err == nil {
			req.Header.Set("X-Tenant-Id", tenantID)
			resp, err := c.http.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				var list []obligation
				if json.NewDecoder(resp.Body).Decode(&list) == nil {
					re := regexp.MustCompile(`\d+(?:\.\d+)?`)
					for _, o := range list {
						if o.ObligationStatus == "CLOSED" {
							continue
						}
						matches := re.FindStringSubmatch(o.SourceReference)
						if len(matches) == 0 {
							matches = re.FindStringSubmatch(o.ObligationID)
						}
						var val float64
						if len(matches) > 0 {
							val, _ = strconv.ParseFloat(matches[0], 64)
						}
						outflows = append(outflows, domain.ExpectedCashFlow{
							Amount:   val,
							DueDate:  o.DueDate,
							Category: "OBLIGATION",
						})
					}
				}
				resp.Body.Close()
			}
		}
	}

	return inflows, outflows, nil
}
