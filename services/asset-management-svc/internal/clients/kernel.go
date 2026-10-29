package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/asset-management-svc/internal/domain"
)

// WithKernelBook makes depreciation post through general-ledger-svc's accounting kernel into the given accounting book, in the book's
// functional currency, instead of the legacy posting command. The register carries no currency of its own: a depreciation schedule is
// kept in the currency of its book, so the book's currency is configured with it. Both empty keeps the legacy path.
func (c *Clients) WithKernelBook(bookID, currency string) *Clients {
	c.kernelBookID, c.kernelCurrency = strings.TrimSpace(bookID), strings.TrimSpace(currency)
	return c
}

// KernelPostingEnabled reports whether depreciation posts through the kernel.
func (c *Clients) KernelPostingEnabled() bool { return c.kernelBookID != "" && c.kernelCurrency != "" }

// KernelCurrency is the currency depreciation is posted in on the kernel path (the book's functional currency), or ""
// when the kernel path is off.
func (c *Clients) KernelCurrency() string { return c.kernelCurrency }

func (c *Clients) kernelDo(ctx context.Context, path, tenantID, principalID, legalEntityID, correlationID, idempotencyKey string, body any) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ledgerURL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("general-ledger-svc kernel call failed", zap.String("path", path), zap.Error(err))
		return 0, nil, domain.ErrLedgerUnavailable
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

// PostDepreciationKernelEvent registers the depreciation of one asset category in a run as an ASSET.DEPRECIATION_POSTED accounting event
// and posts it into the kernel book. The accounts are the tenant's depreciation mappings in the ledger, not accounts this service names.
// Both steps are idempotent on the run and category, so a retried emit never posts a category twice. amount is an exact two-decimal
// string. It returns the journal id.
func (c *Clients) PostDepreciationKernelEvent(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod, runID, assetCategory, amount, correlationID string) (journalID string, err error) {
	periodStart, perr := time.Parse("2006-01", fiscalPeriod)
	if perr != nil {
		return "", fmt.Errorf("fiscal period %q is not YYYY-MM: %w", fiscalPeriod, perr)
	}
	day := periodStart.AddDate(0, 1, -1).Format("2006-01-02") + "T00:00:00Z"
	source := "ast-dep:" + runID + ":" + assetCategory
	event := map[string]any{
		"event_type": "ASSET.DEPRECIATION_POSTED", "event_version": 1, "legal_entity_id": legalEntityID, "source_system": "asset-management-svc",
		"source_object_type": "depreciation_run", "source_object_id": runID, "source_event_id": source, "occurred_at": time.Now().UTC().Format(time.RFC3339),
		"document_date": day, "transaction_currency": c.kernelCurrency, "idempotency_key": source, "correlation_id": correlationID, "causation_id": runID,
		"dimensions": map[string]string{"asset_class": assetCategory},
		"facts":      map[string]string{"amount": amount, "schedule_ref": runID, "policy_version": "ast02-run-" + runID, "period_ref": fiscalPeriod},
	}
	status, resp, err := c.kernelDo(ctx, "/v1/accounting-events", tenantID, principalID, legalEntityID, correlationID, source, event)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", kernelRefusal("register the event", status, resp)
	}
	var ev struct {
		ID string `json:"accounting_event_id"`
	}
	if err := json.Unmarshal(resp, &ev); err != nil || ev.ID == "" {
		return "", fmt.Errorf("%w: the kernel answered without an accounting event id", domain.ErrLedgerUnavailable)
	}
	status, resp, err = c.kernelDo(ctx, "/v1/accounting-postings", tenantID, principalID, legalEntityID, correlationID, source,
		map[string]any{"accounting_event_id": ev.ID, "book_id": c.kernelBookID, "posting_date": day})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", kernelRefusal("post the event", status, resp)
	}
	var p struct {
		JournalID string `json:"journal_id"`
	}
	if err := json.Unmarshal(resp, &p); err != nil || p.JournalID == "" {
		return "", fmt.Errorf("%w: the kernel answered without a journal id", domain.ErrLedgerUnavailable)
	}
	return p.JournalID, nil
}

func kernelRefusal(step string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 400 {
		msg = msg[:400]
	}
	return fmt.Errorf("%w: general-ledger-svc could not %s (%d): %s", domain.ErrLedgerUnavailable, step, status, msg)
}
