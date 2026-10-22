// Package bankrec reads bank-reconciliation-svc's runs and UNMATCHED
// statement lines — the population this service produces suggestions for
// (Doc03 §15.4: "ReconciliationSuggestion (advisory), consuming
// bank-reconciliation").
//
// It is read-only by construction: there is no method here that calls a
// bank-reconciliation-svc write route. That service owns statement lines,
// their match state, and its own propose/confirm/reject match flow; a real
// match is established there, by a human, never from here (ZS-CONTROL-001
// §2, §10).
//
// The client fails CLOSED. A population that cannot be shown to be exactly
// the run's UNMATCHED lines — truncated, out of scope, malformed — is never
// returned, so no suggestion is ever produced about a partial or wrong set.
package bankrec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	svcenvelope "zoiko.io/reconciliation-intelligence-svc/internal/envelope"
)

var (
	// ErrServiceUnavailable: transport failure or a 5xx. An outage.
	ErrServiceUnavailable = errors.New("bank-reconciliation-svc unavailable")
	// ErrRequestRefused: bank-reconciliation-svc answered with a 4xx other
	// than not-found. That is a defect in this request (an envelope header
	// missing, a parameter it no longer accepts), not an outage, and is
	// reported as such rather than folded into ErrServiceUnavailable.
	ErrRequestRefused = errors.New("bank-reconciliation-svc refused the request")
	ErrRunNotFound    = errors.New("bank reconciliation run not found")
	// ErrInconsistent: the answer does not describe the population asked for
	// (a line outside the run's scope or not UNMATCHED, a duplicate line, an
	// amount that cannot be held exactly, an unreadable body).
	ErrInconsistent = errors.New("bank-reconciliation-svc returned an inconsistent population")
	// ErrPopulationTooLarge: GET /v1/statement-lines has no cursor and caps a
	// page at 1000 rows, so a full page cannot be told apart from a
	// truncated population.
	ErrPopulationTooLarge = errors.New("unmatched population is too large to read completely")
	// ErrScopeMissing: no verified tenant or principal to forward. Refuse
	// rather than call under a made-up identity.
	ErrScopeMissing = errors.New("tenant and principal are required to read bank-reconciliation-svc")
)

const (
	// maxLines is bank-reconciliation-svc's domain.MaxListLimit. A page this
	// full may have been cut short, so it is refused, not used.
	maxLines         = 1000
	maxResponseBytes = 16 << 20

	runStatusSuperseded = "SUPERSEDED"
	lineStatusUnmatched = "UNMATCHED"
)

// Scope is who a read is made on behalf of: the VERIFIED tenant and
// principal of the inbound request, forwarded so bank-reconciliation-svc
// applies its own tenant isolation to the caller, not to this service.
type Scope struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string
}

// Run is the part of bank-reconciliation-svc's ReconciliationRun this
// service needs: the population scope and whether it is still current.
type Run struct {
	RunID             string  `json:"run_id"`
	LegalEntityID     string  `json:"legal_entity_id"`
	BankAccountID     string  `json:"bank_account_id"`
	StatementDate     string  `json:"statement_date"`
	Status            string  `json:"status"`
	SupersededByRunID *string `json:"superseded_by_run_id,omitempty"`
}

// Superseded reports whether a later run replaced this one.
func (r *Run) Superseded() bool { return r.Status == runStatusSuperseded }

// statementLine mirrors bank-reconciliation-svc's StatementLine wire shape.
// Amount is decoded as json.Number so the decimal text is kept exactly as
// sent — never through a float64 on this side (ZS-DATA-001 D06). It also
// accepts a JSON string, so it keeps working when that service moves its
// own amount to a decimal string.
type statementLine struct {
	StatementLineID string      `json:"statement_line_id"`
	LegalEntityID   string      `json:"legal_entity_id"`
	BankAccountID   string      `json:"bank_account_id"`
	StatementDate   string      `json:"statement_date"`
	Amount          json.Number `json:"amount"`
	CurrencyCode    string      `json:"currency_code"`
	BankReference   string      `json:"bank_reference"`
	Status          string      `json:"status"`
}

type Client struct {
	baseURL    string
	httpClient *http.Client
	logger     *zap.Logger
}

func NewClient(baseURL string, logger *zap.Logger) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		logger:     logger,
	}
}

// GetRun reads GET /v1/reconciliation-runs/{run_id}.
func (c *Client) GetRun(ctx context.Context, scope Scope, runID string) (*Run, error) {
	var run Run
	if err := c.get(ctx, scope, "/v1/reconciliation-runs/"+url.PathEscape(runID), nil, ErrRunNotFound, &run); err != nil {
		return nil, err
	}
	if run.RunID != runID || run.LegalEntityID == "" || run.BankAccountID == "" || run.StatementDate == "" {
		return nil, fmt.Errorf("%w: run %s came back incomplete", ErrInconsistent, runID)
	}
	return &run, nil
}

// ListUnmatchedLines reads the run's UNMATCHED statement lines.
//
// A run's population is scoped by (tenant, bank_account_id,
// statement_date) — the exact WHERE clause bank-reconciliation-svc's
// FreezePopulation uses — so the lines are read with that filter plus
// status=UNMATCHED. GET /v1/reconciliation-runs/{id}/population is not
// used: it returns the population's header (count, hash, total), not its
// lines.
//
// Every returned line is checked against the scope that was asked for. A
// filter silently ignored upstream (a renamed parameter, say) would
// otherwise turn into suggestions about the whole bank register.
func (c *Client) ListUnmatchedLines(ctx context.Context, scope Scope, run *Run) ([]domain.BankStatementLine, error) {
	runDate, err := dateOnly(run.StatementDate)
	if err != nil {
		return nil, fmt.Errorf("%w: run %s statement_date %q: %v", ErrInconsistent, run.RunID, run.StatementDate, err)
	}
	q := url.Values{
		"bank_account_id": {run.BankAccountID},
		"statement_date":  {runDate},
		"status":          {lineStatusUnmatched},
		"limit":           {strconv.Itoa(maxLines)},
	}
	var raw []statementLine
	if err := c.get(ctx, scope, "/v1/statement-lines", q, nil, &raw); err != nil {
		return nil, err
	}
	if len(raw) >= maxLines {
		return nil, fmt.Errorf("%w: run %s has %d or more UNMATCHED lines and bank-reconciliation-svc offers no way to read past %d",
			ErrPopulationTooLarge, run.RunID, maxLines, maxLines)
	}

	seen := make(map[string]bool, len(raw))
	out := make([]domain.BankStatementLine, 0, len(raw))
	for _, l := range raw {
		if l.StatementLineID == "" || seen[l.StatementLineID] {
			return nil, fmt.Errorf("%w: missing or duplicate statement_line_id %q", ErrInconsistent, l.StatementLineID)
		}
		seen[l.StatementLineID] = true

		lineDate, err := dateOnly(l.StatementDate)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%w: line %s statement_date %q: %v", ErrInconsistent, l.StatementLineID, l.StatementDate, err)
		case l.Status != lineStatusUnmatched:
			return nil, fmt.Errorf("%w: line %s has status %s, asked for UNMATCHED", ErrInconsistent, l.StatementLineID, l.Status)
		case l.BankAccountID != run.BankAccountID || lineDate != runDate:
			return nil, fmt.Errorf("%w: line %s is outside run %s's bank account and statement date", ErrInconsistent, l.StatementLineID, run.RunID)
		case l.LegalEntityID != run.LegalEntityID:
			return nil, fmt.Errorf("%w: line %s belongs to a different legal entity than run %s", ErrInconsistent, l.StatementLineID, run.RunID)
		}

		amount, err := domain.ParseStorableAmount(l.Amount.String())
		if err != nil {
			return nil, fmt.Errorf("%w: line %s amount: %v", ErrInconsistent, l.StatementLineID, err)
		}
		out = append(out, domain.BankStatementLine{
			StatementLineID: l.StatementLineID,
			LegalEntityID:   l.LegalEntityID,
			BankAccountID:   l.BankAccountID,
			StatementDate:   lineDate,
			Amount:          domain.CanonicalAmount(amount),
			CurrencyCode:    l.CurrencyCode,
			BankReference:   l.BankReference,
		})
	}
	return out, nil
}

// get performs one GET and decodes a 200 body into out. notFound, when
// non-nil, is returned for a 404.
func (c *Client) get(ctx context.Context, scope Scope, path string, q url.Values, notFound error, out any) error {
	if scope.TenantID == "" || scope.PrincipalID == "" {
		return ErrScopeMissing
	}
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrServiceUnavailable, err)
	}
	setEnvelopeHeaders(ctx, req, scope)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("bank-reconciliation-svc unreachable — failing closed", zap.String("path", path), zap.Error(err))
		return fmt.Errorf("%w: %v", ErrServiceUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound && notFound != nil:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return notFound
	case resp.StatusCode >= 500:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%w: %s answered HTTP %d", ErrServiceUnavailable, path, resp.StatusCode)
	default:
		// Logged with the body: an envelope_incomplete 400 must read as the
		// missing header it is, not as an outage.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.logger.Error("bank-reconciliation-svc refused the request",
			zap.String("path", path), zap.Int("status", resp.StatusCode), zap.ByteString("body", body))
		return fmt.Errorf("%w: %s answered HTTP %d", ErrRequestRefused, path, resp.StatusCode)
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("%w: %s returned an unreadable body: %v", ErrInconsistent, path, err)
	}
	return nil
}

// setEnvelopeHeaders forwards the canonical service input envelope
// (ZS-ARCH-SVC-001 §4). bank-reconciliation-svc refuses any request — reads
// included — without tenant, actor, request id, correlation id and source
// channel.
//
// Same pattern as financial-close-svc's checkAllowedLive/compileTrialBalance
// (7abd0f8a, d6c2cc38): the values are the CALLER's, taken from the envelope
// the middleware parsed into this request's context, so the read is
// traceable to the request that caused it. An id is minted only when
// neither the request-id middleware nor the envelope supplied one. Tenant
// and principal come from the verified Scope and are never invented
// (b75655a2) — get refuses without them.
func setEnvelopeHeaders(ctx context.Context, req *http.Request, scope Scope) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set(svcenvelope.HeaderTenantID, scope.TenantID)
	req.Header.Set(svcenvelope.HeaderActorSubjectID, scope.PrincipalID)
	if scope.LegalEntityID != "" {
		req.Header.Set(svcenvelope.HeaderLegalEntityID, scope.LegalEntityID)
	}

	requestID := middleware.GetReqID(ctx)
	// Service-to-service. "system" is in the contract's accepted set; the
	// caller's own channel replaces it when the envelope carries one.
	sourceChannel := string(svcenvelope.ChannelSystem)
	correlationID := ""
	if env, ok := svcenvelope.FromContext(ctx); ok {
		if env.RequestID != "" {
			requestID = env.RequestID
		}
		if env.SourceChannel != "" {
			sourceChannel = string(env.SourceChannel)
		}
		// An external channel (import, integration) obliges source_system;
		// forwarding the channel without it would be refused.
		if env.SourceSystem != "" {
			req.Header.Set(svcenvelope.HeaderSourceSystem, env.SourceSystem)
		}
		if env.CorrelationID != "" {
			correlationID = env.CorrelationID
		}
		if env.CausationID != "" {
			req.Header.Set(svcenvelope.HeaderCausationID, env.CausationID)
		}
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set(svcenvelope.HeaderRequestID, requestID)
	req.Header.Set(svcenvelope.HeaderCorrelationID, correlationID)
	req.Header.Set(svcenvelope.HeaderSourceChannel, sourceChannel)
}

// dateOnly normalizes "2026-07-01" or an RFC 3339 timestamp at midnight
// ("2026-07-01T00:00:00Z", how a Go time.Time DATE column serializes) to
// YYYY-MM-DD.
func dateOnly(s string) (string, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Format("2006-01-02"), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", errors.New("not a date")
	}
	// The calendar date as written, not converted to UTC: a DATE serialized
	// with an offset would otherwise move to the neighbouring day.
	return t.Format("2006-01-02"), nil
}
