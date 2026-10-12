// Package periodhistory is the HTTP client to accounting-period-svc (REF-05)
// that answers one question for REF-04: does this calendar have posted or
// closed periods, and how far do they reach? It implements
// service.PeriodHistoryClient.
//
// Contract (owned by REF-05, consumed here):
//
//	GET {base}/v1/calendar-usage?calendar_id=<uuid>
//	200 {"latest_period_end":"YYYY-MM-DD" | null, "has_posted_or_closed_periods": bool}
//
// Anything other than a well-formed 200 is an error; the caller treats an error
// as "unknown" and fails closed.
package periodhistory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zoiko.io/fiscal-calendar-svc/internal/domain"
	"zoiko.io/fiscal-calendar-svc/internal/service"
)

// WorkloadID identifies this service to REF-05.
const WorkloadID = "fiscal-calendar-svc"

// Client calls accounting-period-svc.
type Client struct {
	baseURL string
	http    *http.Client
}

var _ service.PeriodHistoryClient = (*Client)(nil)

// New builds a client. A nil httpClient gets a 5-second timeout.
func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}
}

type usageResponse struct {
	LatestPeriodEnd          *string `json:"latest_period_end"`
	HasPostedOrClosedPeriods bool    `json:"has_posted_or_closed_periods"`
}

// CalendarUsage implements service.PeriodHistoryClient.
func (c *Client) CalendarUsage(ctx context.Context, tenantID, legalEntityID, calendarID string) (*service.PeriodHistory, error) {
	u := c.baseURL + "/v1/calendar-usage?calendar_id=" + url.QueryEscape(calendarID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	req.Header.Set("X-Workload-Id", WorkloadID)
	req.Header.Set("Accept", "application/json")
	if corr := ctx.Value(correlationKey{}); corr != nil {
		req.Header.Set("X-Correlation-ID", fmt.Sprint(corr))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("accounting-period-svc unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read accounting-period-svc response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("accounting-period-svc returned %d", resp.StatusCode)
	}
	var ur usageResponse
	if err := json.Unmarshal(body, &ur); err != nil {
		return nil, fmt.Errorf("decode accounting-period-svc response: %w", err)
	}
	out := &service.PeriodHistory{HasPostedOrClosedPeriods: ur.HasPostedOrClosedPeriods}
	if ur.LatestPeriodEnd != nil && *ur.LatestPeriodEnd != "" {
		d, err := domain.ParseDate(*ur.LatestPeriodEnd)
		if err != nil {
			return nil, fmt.Errorf("accounting-period-svc latest_period_end: %w", err)
		}
		out.LatestPeriodEnd = &d
	}
	return out, nil
}

type correlationKey struct{}

// WithCorrelation attaches a correlation id to forward to REF-05.
func WithCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}
