package close

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
	"zoiko.io/general-ledger-svc/internal/domain"
)

// PeriodRef identifies what a period check is about. The legacy status call
// needs only the entity and period name; PostingDate and JournalID exist for
// the REF-05 shadow gate (internal/periodgate), which resolves by DATE.
// PostingDate is the zero Date when the call site has no posting date; the
// shadow gate then skips rather than guessing one.
type PeriodRef struct {
	LegalEntityID string
	PeriodName    string
	PostingDate   domain.Date
	JournalID     string // "" when the journal has no id yet (create paths)
}

type Client interface {
	CheckPeriodOpen(ctx context.Context, tenantID, legalEntityID, periodName string) error
	// CheckPeriodOpenAt returns EXACTLY what CheckPeriodOpen would for
	// (ref.LegalEntityID, ref.PeriodName). Implementations may additionally
	// observe ref.PostingDate/JournalID but must never let that change the
	// returned error.
	CheckPeriodOpenAt(ctx context.Context, tenantID string, ref PeriodRef) error
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		log:     log,
		http:    &http.Client{Timeout: 2 * time.Second, Transport: newRetryTransport()},
	}
}

type periodStatusResp struct {
	CloseStatus     string     `json:"close_status"`
	PeriodState     string     `json:"period_state"`
	PostingPolicy   string     `json:"posting_policy"`
	ReopenExpiresAt *time.Time `json:"reopen_expires_at,omitempty"`
}

// CheckPeriodOpenAt is the legacy check; the extra PeriodRef fields are unused.
func (c *HTTPClient) CheckPeriodOpenAt(ctx context.Context, tenantID string, ref PeriodRef) error {
	return c.CheckPeriodOpen(ctx, tenantID, ref.LegalEntityID, ref.PeriodName)
}

func (c *HTTPClient) CheckPeriodOpen(ctx context.Context, tenantID, legalEntityID, periodName string) error {
	u, err := url.Parse(c.baseURL + "/v1/close/periods/status")
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("legal_entity_id", legalEntityID)
	q.Set("period_name", periodName)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return domain.ErrCloseServiceUnavailable
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("financial-close-svc unreachable — failing closed on journal posting", zap.Error(err))
		return domain.ErrCloseServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil // If period not registered, default to open
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("unexpected status from financial-close-svc — failing closed", zap.Int("status", resp.StatusCode))
		return domain.ErrCloseServiceUnavailable
	}

	var statusResp periodStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return domain.ErrCloseServiceUnavailable
	}

	switch statusResp.PostingPolicy {
	case "OPEN", "REOPENED":
		return nil
	case "RESTRICTED":
		return domain.ErrSoftCloseOverrideRequired
	case "CLOSE_JOURNALS_ONLY":
		return domain.ErrPeriodHardClosed
	case "CLOSED":
		return domain.ErrPeriodHardClosed
	default:
		c.log.Error("unrecognized posting_policy from financial-close-svc — failing closed", zap.String("posting_policy", statusResp.PostingPolicy), zap.String("period_state", statusResp.PeriodState))
		return domain.ErrPeriodHardClosed
	}
}