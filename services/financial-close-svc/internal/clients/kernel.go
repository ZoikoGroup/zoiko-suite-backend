package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
)

// The Global Accounting Kernel lives in general-ledger-svc. Two things the period close needs from it: what the kernel knows about
// whether a period is ready (open reconciliation cases, failed postings, foreign items awaiting revaluation, journals in the review
// queue), and the period seal — the close watermark that no later posting can slip under.

// GetKernelCloseEvidence asks the kernel whether a period of an entity is ready to close. A failure to ask is an error, never an
// empty answer: "could not check" must not read as "nothing to report".
func (c *Clients) GetKernelCloseEvidence(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod string, require []string) (*domain.KernelCloseEvidence, error) {
	u, err := url.Parse(c.ledgerURL + "/v1/close-evidence")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("legal_entity_id", legalEntityID)
	q.Set("fiscal_period", fiscalPeriod)
	if len(require) > 0 {
		q.Set("require", strings.Join(require, ","))
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	c.kernelHeaders(req, tenantID, principalID, legalEntityID)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return nil, fmt.Errorf("%w: kernel close evidence answered %d", domain.ErrGLServiceUnavailable, resp.StatusCode)
	}
	var ev domain.KernelCloseEvidence
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		return nil, fmt.Errorf("decode kernel close evidence: %w", err)
	}
	return &ev, nil
}

// ApplyKernelPeriodSeal seals a period in the kernel (hard close), reopens its seal under named authority, or seals it again after a
// reopen. changed is false when the kernel had nothing to do: a period sealed already, or one that was never sealed (closed before
// the kernel kept seals) being reopened. Anything else that is not a success is an error.
func (c *Clients) ApplyKernelPeriodSeal(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod, action, authorityRef, reason string) (changed bool, err error) {
	changed, status, err := c.sealOnce(ctx, tenantID, principalID, legalEntityID, fiscalPeriod, action, authorityRef, reason)
	if err != nil {
		return false, err
	}
	if status == http.StatusConflict && action == "RESEAL" {
		// the period was closed before the kernel kept seals, so there is no reopened seal to close again: seal it
		changed, status, err = c.sealOnce(ctx, tenantID, principalID, legalEntityID, fiscalPeriod, "SEAL", "", "")
		if err != nil {
			return false, err
		}
	}
	switch status {
	case http.StatusOK, http.StatusCreated:
		return changed, nil
	case http.StatusConflict:
		return false, nil
	default:
		return false, fmt.Errorf("%w: the kernel answered %d to %s", domain.ErrGLServiceUnavailable, status, action)
	}
}

func (c *Clients) sealOnce(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod, action, authorityRef, reason string) (changed bool, status int, err error) {
	body, _ := json.Marshal(map[string]string{"legal_entity_id": legalEntityID, "fiscal_period": fiscalPeriod, "action": action, "authority_ref": authorityRef, "reason": reason})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ledgerURL+"/v1/period-seals", bytes.NewReader(body))
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.kernelHeaders(req, tenantID, principalID, legalEntityID)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, 0, domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()
	var out struct {
		Existing bool `json:"existing"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return !out.Existing, resp.StatusCode, nil
}

func (c *Clients) kernelHeaders(req *http.Request, tenantID, principalID, legalEntityID string) {
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "service")
	if req.Method != http.MethodGet {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	if corr := req.Header.Get("X-Correlation-ID"); corr == "" {
		req.Header.Set("X-Correlation-ID", uuid.NewString())
	}
}
