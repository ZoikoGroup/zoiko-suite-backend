// Package fiscalcal checks a fiscal-calendar reference against fiscal-calendar-svc (REF-04), read-only.
//
// ORG-03 s4.3 permits a legal entity only a fiscal calendar that exists. The calendar is owned and versioned by
// fiscal-calendar-svc and is tenant data, so it is read under the entity's tenant: another tenant's calendar is a
// 404, the same as an unknown one. The answer is fail-closed: a service that cannot be reached means the entity is
// not created, never that the reference is assumed good.
package fiscalcal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"

	svcenvelope "zoiko.io/tenant-entity-registry-svc/internal/envelope"
)

// Validator reports whether a tenant has a fiscal calendar with this id. false is an answer (it does not exist for
// the tenant); an error is ErrUnavailable and the caller fails closed.
type Validator interface {
	Exists(ctx context.Context, tenantID, calendarID string) (bool, error)
}

// ErrUnavailable is returned when fiscal-calendar-svc cannot answer.
var ErrUnavailable = errors.New("fiscal calendar service unavailable")

// Stub accepts every reference. Development and test only: main.go refuses it in staging and production.
type Stub struct{ log *zap.Logger }

// NewStub builds the development stub.
func NewStub(log *zap.Logger) *Stub { return &Stub{log: log} }

// Exists implements Validator.
func (s *Stub) Exists(_ context.Context, _, calendarID string) (bool, error) {
	s.log.Debug("fiscal calendar validator stub: accepted without checking", zap.String("fiscal_calendar_id", calendarID))
	return true, nil
}

// Unavailable fails every check closed. It is used when no service is configured outside development.
type Unavailable struct{}

// Exists implements Validator.
func (Unavailable) Exists(context.Context, string, string) (bool, error) {
	return false, fmt.Errorf("%w: FISCAL_CALENDAR_URL is not configured", ErrUnavailable)
}

// HTTP is the production validator.
type HTTP struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

// NewHTTP builds a validator that calls fiscal-calendar-svc.
func NewHTTP(baseURL string, log *zap.Logger) *HTTP {
	return &HTTP{baseURL: baseURL, log: log, client: &http.Client{Timeout: 2 * time.Second}}
}

// Exists reads GET /v1/fiscal-calendars/{id} under the tenant. 200 exists, 404 does not, anything else is unavailable.
func (h *HTTP) Exists(ctx context.Context, tenantID, calendarID string) (bool, error) {
	u := fmt.Sprintf("%s/v1/fiscal-calendars/%s", h.baseURL, url.PathEscape(calendarID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, ErrUnavailable
	}
	svcenvelope.ForwardTo(ctx, req)
	idenv.Forward(ctx, req)
	// The calendar is tenant data: ask under the entity's own tenant, whatever the caller's envelope says.
	req.Header.Set("X-Tenant-Id", tenantID)
	resp, err := h.client.Do(req)
	if err != nil {
		h.log.Error("fiscal calendar service unreachable: failing closed", zap.String("fiscal_calendar_id", calendarID), zap.Error(err))
		return false, ErrUnavailable
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		h.log.Error("unexpected response from fiscal calendar service: failing closed", zap.Int("status", resp.StatusCode), zap.String("fiscal_calendar_id", calendarID))
		return false, ErrUnavailable
	}
}
