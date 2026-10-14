package ar10

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// ErrGatewayUnavailable means the payment path could not answer. For SubmitAttempt it means the outcome is AMBIGUOUS: the provider
// may or may not have received the instruction, so the refund becomes UNKNOWN, never failed.
var ErrGatewayUnavailable = errors.New("payment gateway unavailable")

// GatewayRejected is a definitive refusal (4xx) of a PREPARE call: nothing was created, so the caller may retry after fixing it.
type GatewayRejected struct {
	Status int
	Msg    string
}

func (e *GatewayRejected) Error() string {
	return fmt.Sprintf("payment gateway rejected (%d): %s", e.Status, e.Msg)
}

// PrepareInput is the durable payment instruction. Amount crosses the legacy float64 wire boundary only inside the HTTP client.
type PrepareInput struct {
	LegalEntityID    string
	SourceReference  string // the refund id: the adapter allows one unresolved attempt per source reference
	IdempotencyKey   string // durable per refund; a repeat PrepareAttempt returns the same attempt
	PayerAccountRef  string
	PayeeRef         string // the verified destination reference
	Amount           kit.Amount
	Currency         string
	PaymentReference string
	ExecutionDate    time.Time
}

// Attempt is the BNK-06 external attempt. AttemptID is the durable external_attempt_id (never a request id).
type Attempt struct {
	AttemptID         string
	Status            string
	ProviderRequestID string
	RejectionReason   string
}

// PaymentState is the BNK-07 canonical execution state: the only source of settlement finality.
type PaymentState struct {
	PaymentID         string
	Status            string
	ProviderRequestID string
}

// PaymentGateway is the outbound payment path of a refund. Implemented over payment-initiation-adapter-svc and payment-status-svc;
// AR never calls a bank and never marks settlement itself. There is deliberately no retry/resubmit method: a refund whose
// submission is ambiguous is reconciled through GetAttempt / GetPayment and never re-sent.
type PaymentGateway interface {
	PrepareAttempt(ctx context.Context, c kit.Caller, in PrepareInput) (*Attempt, error)
	SubmitAttempt(ctx context.Context, c kit.Caller, attemptID string) (*Attempt, error)
	GetAttempt(ctx context.Context, c kit.Caller, attemptID string) (*Attempt, error)
	// RegisterPayment creates the BNK-07 tracking record for a submitted attempt (providerRequestID correlates it).
	RegisterPayment(ctx context.Context, c kit.Caller, legalEntityID, providerRequestID, sourceReference string) (paymentID string, err error)
	GetPayment(ctx context.Context, c kit.Caller, paymentID string) (*PaymentState, error)
}

// HTTPGateway talks to payment-initiation-adapter-svc (/bnk06/attempts) and payment-status-svc (/bnk07/payments). Both services
// decode and encode their domain structs WITHOUT json tags, so the wire keys are the Go field names (PascalCase).
type HTTPGateway struct{ init, status *svcclient.Client }

func NewHTTPGateway(paymentInitiationURL, paymentStatusURL string) *HTTPGateway {
	return &HTTPGateway{init: svcclient.New(paymentInitiationURL), status: svcclient.New(paymentStatusURL)}
}

type prepareWire struct {
	LegalEntityID        string
	SourceReference      string
	PayerAccountRef      string
	PayeeRef             string
	Amount               float64
	Currency             string
	ExecutionDate        time.Time
	PaymentReference     string
	PayerAccountVerified bool
	IdempotencyKey       string
}

type attemptWire struct {
	AttemptID           string
	Status              string
	ProviderRequestID   string
	ProviderResponseRef string
	RejectionReason     string
}

func (a attemptWire) attempt() *Attempt {
	return &Attempt{AttemptID: a.AttemptID, Status: a.Status, ProviderRequestID: a.ProviderRequestID, RejectionReason: a.RejectionReason}
}

func (g *HTTPGateway) decodeAttempt(status int, body []byte, ok ...int) (*Attempt, error) {
	for _, s := range ok {
		if status == s {
			var w attemptWire
			if json.Unmarshal(body, &w) != nil || w.AttemptID == "" {
				return nil, ErrGatewayUnavailable
			}
			return w.attempt(), nil
		}
	}
	return nil, ErrGatewayUnavailable
}

// PrepareAttempt persists the durable attempt BEFORE any submission. A 4xx is a definitive refusal (nothing created); anything else
// unanswered is unavailable (safe to retry: the adapter is idempotent on the key).
func (g *HTTPGateway) PrepareAttempt(ctx context.Context, c kit.Caller, in PrepareInput) (*Attempt, error) {
	wire := prepareWire{LegalEntityID: in.LegalEntityID, SourceReference: in.SourceReference, PayerAccountRef: in.PayerAccountRef, PayeeRef: in.PayeeRef,
		Amount: in.Amount.Float(), Currency: in.Currency, ExecutionDate: in.ExecutionDate, PaymentReference: in.PaymentReference,
		PayerAccountVerified: true, // the adapter independently verifies the payer account with treasury-svc; this is its required attestation flag
		IdempotencyKey:       in.IdempotencyKey}
	status, body, err := g.init.Do(ctx, c.SvcCaller(), "POST", "/bnk06/attempts/", wire)
	if err != nil {
		return nil, ErrGatewayUnavailable
	}
	if status >= 400 && status < 500 {
		return nil, &GatewayRejected{Status: status, Msg: clip(string(body), 300)}
	}
	return g.decodeAttempt(status, body, 200, 201)
}

// SubmitAttempt sends the prepared attempt to the provider (once). Any answer but a clean 200 is ambiguous (ErrGatewayUnavailable).
func (g *HTTPGateway) SubmitAttempt(ctx context.Context, c kit.Caller, attemptID string) (*Attempt, error) {
	status, body, err := g.init.Do(ctx, c.SvcCaller(), "POST", "/bnk06/attempts/"+url.PathEscape(attemptID)+"/submit", nil)
	if err != nil {
		return nil, ErrGatewayUnavailable
	}
	return g.decodeAttempt(status, body, 200)
}

func (g *HTTPGateway) GetAttempt(ctx context.Context, c kit.Caller, attemptID string) (*Attempt, error) {
	status, body, err := g.init.Do(ctx, c.SvcCaller(), "GET", "/bnk06/attempts/"+url.PathEscape(attemptID), nil)
	if err != nil {
		return nil, ErrGatewayUnavailable
	}
	return g.decodeAttempt(status, body, 200)
}

func (g *HTTPGateway) RegisterPayment(ctx context.Context, c kit.Caller, legalEntityID, providerRequestID, sourceReference string) (string, error) {
	status, body, err := g.status.Do(ctx, c.SvcCaller(), "POST", "/bnk07/payments/",
		map[string]string{"LegalEntityID": legalEntityID, "ProviderRequestID": providerRequestID, "SourceReference": sourceReference})
	if err != nil || (status != 200 && status != 201) {
		return "", ErrGatewayUnavailable
	}
	var w struct{ PaymentID string }
	if json.Unmarshal(body, &w) != nil || w.PaymentID == "" {
		return "", ErrGatewayUnavailable
	}
	return w.PaymentID, nil
}

func (g *HTTPGateway) GetPayment(ctx context.Context, c kit.Caller, paymentID string) (*PaymentState, error) {
	status, body, err := g.status.Do(ctx, c.SvcCaller(), "GET", "/bnk07/payments/"+url.PathEscape(paymentID), nil)
	if err != nil || status != 200 {
		return nil, ErrGatewayUnavailable
	}
	var w struct{ PaymentID, Status, ProviderRequestID string }
	if json.Unmarshal(body, &w) != nil || w.PaymentID == "" || w.Status == "" {
		return nil, ErrGatewayUnavailable
	}
	return &PaymentState{PaymentID: w.PaymentID, Status: w.Status, ProviderRequestID: w.ProviderRequestID}, nil
}
