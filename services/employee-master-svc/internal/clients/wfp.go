// Package clients holds the outbound calls WFP-01 makes before accepting a
// source delivery. Both fail closed: an unreachable dependency is "cannot
// answer", never "yes" and never a default.
package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zoiko.io/employee-master-svc/internal/domain"
)

func httpClient(c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{Timeout: 5 * time.Second}
	}
	return c
}

// SourceTrust asks payroll-run-svc (owner of source trust profiles) whether a
// source system is currently trusted for a capability.
type SourceTrust struct {
	baseURL string
	client  *http.Client
}

func NewSourceTrust(baseURL string, c *http.Client) *SourceTrust {
	return &SourceTrust{baseURL: strings.TrimRight(baseURL, "/"), client: httpClient(c)}
}

// CallerContext is forwarded so the callee authorizes and audits the real caller.
type CallerContext struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string
	CorrelationID string
}

func (c *SourceTrust) Check(ctx context.Context, caller CallerContext, sourceSystemID, capability string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/payroll/wfp/source-trust-profiles/by-source/"+url.PathEscape(sourceSystemID), nil)
	if err != nil {
		return domain.ErrSourceTrustUnavailable
	}
	req.Header.Set("X-Tenant-Id", caller.TenantID)
	req.Header.Set("X-Principal-Id", caller.PrincipalID)
	req.Header.Set("X-Legal-Entity-Id", caller.LegalEntityID)
	req.Header.Set("X-Correlation-ID", caller.CorrelationID)

	resp, err := c.client.Do(req)
	if err != nil {
		return domain.ErrSourceTrustUnavailable
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return domain.ErrSourceNotTrusted
	default:
		return domain.ErrSourceTrustUnavailable
	}
	var p struct {
		Status              string   `json:"status"`
		AllowedCapabilities []string `json:"allowed_capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return domain.ErrSourceTrustUnavailable
	}
	if p.Status != "ACTIVE" {
		return domain.ErrSourceNotTrusted
	}
	for _, cap := range p.AllowedCapabilities {
		if cap == capability {
			return nil
		}
	}
	return domain.ErrSourceNotTrusted
}

// Jurisdictions probes jurisdiction-rules-svc. Its GET /v1/jurisdictions/{id}
// is the platform's synchronous validation probe: 503 means "cannot answer".
type Jurisdictions struct {
	baseURL string
	client  *http.Client
}

func NewJurisdictions(baseURL string, c *http.Client) *Jurisdictions {
	return &Jurisdictions{baseURL: strings.TrimRight(baseURL, "/"), client: httpClient(c)}
}

// Exists reports whether the jurisdiction is registered and active. A non-nil
// error means the registry could not answer.
func (j *Jurisdictions) Exists(ctx context.Context, jurisdictionID, correlationID string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		j.baseURL+"/v1/jurisdictions/"+url.PathEscape(jurisdictionID), nil)
	if err != nil {
		return false, domain.ErrJurisdictionUnavailable
	}
	req.Header.Set("X-Correlation-ID", correlationID)
	resp, err := j.client.Do(req)
	if err != nil {
		return false, domain.ErrJurisdictionUnavailable
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			ActiveFlag *bool `json:"active_flag"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return false, fmt.Errorf("%w: undecodable response", domain.ErrJurisdictionUnavailable)
		}
		return body.ActiveFlag == nil || *body.ActiveFlag, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, domain.ErrJurisdictionUnavailable
	}
}
