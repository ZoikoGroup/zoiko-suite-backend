// Package periodgate is the REF-05 SHADOW period gate for general-ledger-svc.
//
// PHASE 2 CONTRACT: the gate is consulted and COMPARED against the legacy
// financial-close-svc check, and its result is only ever counted and logged.
// It must never change the outcome (or the timing beyond the legacy call) of
// any GL operation. Enforcement is phase 4 and is deliberately not implemented
// here; config rejects PERIOD_GATE_MODE=enforce at startup.
package periodgate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// LegacyOutcome is the classification of a close.Client.CheckPeriodOpen result.
type LegacyOutcome string

const (
	LegacyAllowed     LegacyOutcome = "allowed"
	LegacyLocked      LegacyOutcome = "locked"
	LegacyUnavailable LegacyOutcome = "unavailable"
)

// GateOutcome is the classification of an accounting-period-svc resolve call.
type GateOutcome string

const (
	GateAllowed   GateOutcome = "allowed"   // 200, posting_allowed=true
	GateBlocked   GateOutcome = "blocked"   // 200, posting_allowed=false
	GateNotFound  GateOutcome = "not_found" // 404 PERIOD_NOT_FOUND
	GateAmbiguous GateOutcome = "ambiguous" // 409 RULE_AMBIGUOUS
	GateError     GateOutcome = "error"     // timeout, 5xx, 401/422, bad body, breaker
)

// Agreement label values.
const (
	AgreeYes         = "true"
	AgreeNo          = "false"
	AgreeShadowError = "shadow_error"
)

// ClassifyLegacy maps a legacy CheckPeriodOpen result to an outcome.
func ClassifyLegacy(err error) LegacyOutcome {
	switch {
	case err == nil:
		return LegacyAllowed
	case errors.Is(err, domain.ErrPeriodLocked):
		return LegacyLocked
	default:
		return LegacyUnavailable
	}
}

// Compare returns the agreement label. A gate error is never a (dis)agreement.
func Compare(l LegacyOutcome, g GateOutcome) string {
	if g == GateError {
		return AgreeShadowError
	}
	if (l == LegacyAllowed && g == GateAllowed) || (l == LegacyLocked && g == GateBlocked) {
		return AgreeYes
	}
	return AgreeNo
}

// Resolution is the classified result of one resolve call.
type Resolution struct {
	Outcome GateOutcome
	State   string // gate state when 200 (log only)
	Code    string // gate error code, or a local code (TIMEOUT, TRANSPORT, BAD_JSON, ...)
	Skipped bool   // true when the local breaker suppressed the call (no request sent)
}

// Resolver is what the shadow wrapper needs; *Client implements it.
type Resolver interface {
	Resolve(ctx context.Context, tenantID, legalEntityID, date string) Resolution
}

// Client calls accounting-period-svc GET /v1/accounting-periods:resolve.
// It never retries. A consecutive-failure breaker makes a dead gate cost
// nothing; a tripped breaker only skips shadow calls.
type Client struct {
	baseURL string
	http    *http.Client

	mu        sync.Mutex
	fails     int
	openUntil time.Time
	now       func() time.Time
}

const (
	breakerThreshold = 5
	breakerCooldown  = 10 * time.Second
	maxBody          = 64 << 10
)

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: timeout},
		now:     time.Now,
	}
}

type resolveBody struct {
	State          string `json:"state"`
	PostingAllowed *bool  `json:"posting_allowed"`
	Code           string `json:"code"`
}

// Resolve never panics and never returns an error: every failure is an
// Outcome of GateError. ctx carries the caller's deadline (the shadow timeout).
func (c *Client) Resolve(ctx context.Context, tenantID, legalEntityID, date string) (res Resolution) {
	defer func() {
		if r := recover(); r != nil {
			res = Resolution{Outcome: GateError, Code: "PANIC"}
		}
	}()
	if c.breakerOpen() {
		return Resolution{Outcome: GateError, Code: "CIRCUIT_OPEN", Skipped: true}
	}
	res = c.do(ctx, tenantID, legalEntityID, date)
	// 404/409 are healthy answers; only GateError counts against the breaker.
	c.record(res.Outcome != GateError)
	return res
}

func (c *Client) do(ctx context.Context, tenantID, legalEntityID, date string) Resolution {
	u, err := url.Parse(c.baseURL + "/v1/accounting-periods:resolve")
	if err != nil {
		return Resolution{Outcome: GateError, Code: "BAD_URL"}
	}
	q := u.Query()
	q.Set("legal_entity_id", legalEntityID)
	q.Set("date", date)
	q.Set("purpose", "post")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Resolution{Outcome: GateError, Code: "BAD_REQUEST"}
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return Resolution{Outcome: GateError, Code: "TIMEOUT"}
		}
		var ne interface{ Timeout() bool }
		if errors.As(err, &ne) && ne.Timeout() {
			return Resolution{Outcome: GateError, Code: "TIMEOUT"}
		}
		return Resolution{Outcome: GateError, Code: "TRANSPORT"}
	}
	defer resp.Body.Close()
	var body resolveBody
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body)

	switch resp.StatusCode {
	case http.StatusOK:
		if decodeErr != nil || body.PostingAllowed == nil {
			return Resolution{Outcome: GateError, Code: "BAD_JSON"}
		}
		if *body.PostingAllowed {
			return Resolution{Outcome: GateAllowed, State: boundedStr(body.State)}
		}
		return Resolution{Outcome: GateBlocked, State: boundedStr(body.State)}
	case http.StatusNotFound:
		return Resolution{Outcome: GateNotFound, Code: codeOr(body.Code, decodeErr, "PERIOD_NOT_FOUND")}
	case http.StatusConflict:
		return Resolution{Outcome: GateAmbiguous, Code: codeOr(body.Code, decodeErr, "RULE_AMBIGUOUS")}
	default:
		return Resolution{Outcome: GateError, Code: codeOr(body.Code, decodeErr, "HTTP_ERROR")}
	}
}

func boundedStr(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

// codeOr keeps the gate's own code when the body decoded, else a local default.
func codeOr(code string, decodeErr error, def string) string {
	if decodeErr != nil || code == "" {
		return def
	}
	return boundedStr(code)
}

func (c *Client) breakerOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fails >= breakerThreshold && c.now().Before(c.openUntil)
}

func (c *Client) record(ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.fails = 0
		return
	}
	c.fails++
	if c.fails >= breakerThreshold {
		c.openUntil = c.now().Add(breakerCooldown)
	}
}
