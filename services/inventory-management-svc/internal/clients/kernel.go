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

	"zoiko.io/inventory-management-svc/internal/domain"
)

// WithKernelBook makes inventory valuation post through general-ledger-svc's accounting kernel into the given accounting book, in the
// book's functional currency, instead of the legacy posting command. The inventory service carries no currency of its own: valuation
// entries are in the currency of the book they feed, so that currency is configured with the book. Both empty keeps the legacy path.
func (c *Clients) WithKernelBook(bookID, currency string) *Clients {
	c.kernelBookID, c.kernelCurrency = strings.TrimSpace(bookID), strings.TrimSpace(currency)
	return c
}

// KernelPostingEnabled reports whether inventory valuation posts through the kernel.
func (c *Clients) KernelPostingEnabled() bool { return c.kernelBookID != "" && c.kernelCurrency != "" }

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

// KernelEvent is one inventory valuation posting: the kernel event type, the key that makes a retry the same event, the facts and the
// dimensions (the location code and the item type the ledger reconciles stock by).
type KernelEvent struct {
	EventType    string
	SourceKey    string
	FiscalPeriod string
	Facts        map[string]string
	Dimensions   map[string]string
}

// PostInventoryKernelEvent registers the event and posts it into the kernel book, at the end of its fiscal period. Both steps are
// idempotent on SourceKey, so a retry after a lost response or a partial emit never posts a second time. The accounts are the tenant's
// mappings in the ledger, not accounts this service names. It returns the journal id.
func (c *Clients) PostInventoryKernelEvent(ctx context.Context, tenantID, principalID, legalEntityID, correlationID string, e KernelEvent) (string, error) {
	periodStart, perr := time.Parse("2006-01", e.FiscalPeriod)
	if perr != nil {
		return "", fmt.Errorf("fiscal period %q is not YYYY-MM: %w", e.FiscalPeriod, perr)
	}
	day := periodStart.AddDate(0, 1, -1).Format("2006-01-02") + "T00:00:00Z"
	body := map[string]any{
		"event_type": e.EventType, "event_version": 1, "legal_entity_id": legalEntityID, "source_system": "inventory-management-svc",
		"source_object_type": "inventory_valuation", "source_object_id": e.SourceKey, "source_event_id": e.SourceKey, "occurred_at": time.Now().UTC().Format(time.RFC3339),
		"document_date": day, "transaction_currency": c.kernelCurrency, "idempotency_key": e.SourceKey, "correlation_id": correlationID, "causation_id": e.SourceKey,
		"dimensions": e.Dimensions, "facts": e.Facts,
	}
	status, resp, err := c.kernelDo(ctx, "/v1/accounting-events", tenantID, principalID, legalEntityID, correlationID, e.SourceKey, body)
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
	status, resp, err = c.kernelDo(ctx, "/v1/accounting-postings", tenantID, principalID, legalEntityID, correlationID, e.SourceKey,
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
