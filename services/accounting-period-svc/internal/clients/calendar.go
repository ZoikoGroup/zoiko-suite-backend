package clients

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/service"
)

// HTTPCalendar reads REF-04 (fiscal-calendar-svc):
//
//	GET {FiscalCalendarURL}/v1/fiscal-calendar-versions/{vid}/periods-preview?fiscal_year=YYYY
//	GET {FiscalCalendarURL}/v1/fiscal-calendars:resolve?legal_entity_id=&scope=&date=
//
// Unreachable / 5xx -> DEPENDENCY_UNAVAILABLE; 404 -> NOT_FOUND; any other
// non-200 or an undecodable body -> SOURCE_UNVERIFIED.
type HTTPCalendar struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPCalendar builds the client with a short timeout.
func NewHTTPCalendar(baseURL string) *HTTPCalendar {
	return &HTTPCalendar{BaseURL: baseURL, Client: &http.Client{Timeout: 5 * time.Second}}
}

var _ service.CalendarClient = (*HTTPCalendar)(nil)

func (c *HTTPCalendar) get(ctx context.Context, tenantID, path string, q url.Values, out any) error {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return domain.Errf(domain.CodeDependencyUnavailable, "cannot build fiscal-calendar request")
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Workload-Id", workloadID)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return domain.Errf(domain.CodeDependencyUnavailable, "fiscal-calendar-svc unreachable")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return domain.Errf(domain.CodeDependencyUnavailable, "fiscal-calendar-svc answered %d", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return domain.Errf(domain.CodeNotFound, "fiscal-calendar-svc does not know that calendar or version")
	case resp.StatusCode != http.StatusOK:
		return domain.Errf(domain.CodeSourceUnverified, "fiscal-calendar-svc answered %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return domain.Errf(domain.CodeSourceUnverified, "fiscal-calendar-svc answer is not valid JSON")
	}
	return nil
}

// Resolve implements service.CalendarClient.
func (c *HTTPCalendar) Resolve(ctx context.Context, tenantID, legalEntityID, scope, date string) (*service.CalendarRef, error) {
	q := url.Values{"legal_entity_id": {legalEntityID}, "date": {date}}
	if scope != "" {
		q.Set("scope", scope)
	}
	var ref service.CalendarRef
	if err := c.get(ctx, tenantID, "/v1/fiscal-calendars:resolve", q, &ref); err != nil {
		return nil, err
	}
	if ref.CalendarID == "" || ref.VersionID == "" {
		return nil, domain.Errf(domain.CodeSourceUnverified, "fiscal-calendar-svc resolve answer lacks calendar_id/version_id")
	}
	return &ref, nil
}

// PeriodsPreview implements service.CalendarClient.
func (c *HTTPCalendar) PeriodsPreview(ctx context.Context, tenantID, versionID string, fiscalYear int) (*service.CalendarPreview, error) {
	var pv service.CalendarPreview
	err := c.get(ctx, tenantID, "/v1/fiscal-calendar-versions/"+url.PathEscape(versionID)+"/periods-preview",
		url.Values{"fiscal_year": {strconv.Itoa(fiscalYear)}}, &pv)
	if err != nil {
		return nil, err
	}
	return &pv, nil
}
