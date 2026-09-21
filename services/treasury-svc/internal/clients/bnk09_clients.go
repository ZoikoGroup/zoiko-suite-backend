// BNK-09's outbound HTTP clients: payment-initiation-adapter-svc (BNK-06),
// general-ledger-svc, and intercompany-accounting-svc. Kept in their own
// file/struct rather than folded into the existing wide Clients (AP/AR/
// obligations) — this capability's dependencies are read-write execution
// calls with idempotency semantics, a different shape from the read-only
// aggregation calls Clients already makes.
package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
)

type TransferClients struct {
	paymentAdapterURL string
	ledgerURL         string
	intercompanyURL   string
	http              *http.Client
	log               *zap.Logger
}

func NewTransferClients(paymentAdapterURL, ledgerURL, intercompanyURL string, log *zap.Logger) *TransferClients {
	return &TransferClients{
		paymentAdapterURL: paymentAdapterURL,
		ledgerURL:         ledgerURL,
		intercompanyURL:   intercompanyURL,
		http:              &http.Client{Timeout: 5 * time.Second},
		log:               log,
	}
}

// payment-initiation-adapter-svc's domain.PrepareAttemptRequest/
// PaymentInitiationAttempt carry no json tags, so encoding/json falls
// back to the exported Go field name verbatim as the wire key —
// reproduced here field-for-field rather than guessing a snake_case shape
// that wouldn't decode on the other side.
type prepareAttemptRequest struct {
	LegalEntityID            string
	SourceReference          string
	AuthorizationFingerprint string
	// AuthorizationID/AuthorizationSource (Wave 11b): identify this as a
	// BNK-09-originated fingerprint so payment-initiation-adapter-svc's
	// PrepareAttempt re-fetches and compares it against treasury-svc's
	// own GetTreasuryTransferFingerprint, instead of the AuthorizationSourcePaymentAuthorization
	// path (which would look for it in payment-authorization-svc) or
	// leaving it unverified.
	AuthorizationID          string
	AuthorizationSource      string
	PayerAccountRef          string
	PayeeRef                 string
	Amount                   float64
	Currency                 string
	ExecutionDate            time.Time
	PaymentReference         string
	PayerAccountVerified     bool
	IdempotencyKey           string
}

// authorizationSourceTreasury must match payment-initiation-adapter-svc's
// domain.AuthorizationSourceTreasury exactly — the two services aren't
// sharing a Go package, so this is reproduced as a literal rather than an
// import, same posture as this file's own comment on prepareAttemptRequest's
// field names having no json tags to keep in sync by hand.
const authorizationSourceTreasury = "TREASURY_SVC"

type paymentInitiationAttempt struct {
	AttemptID       string
	Status          string
	RejectionReason string
}

// SubmitTreasuryPayment is BNK-09's real call to BNK-06: prepare an
// attempt (idempotent on transferID as the idempotency key — a resumed
// ExecuteTreasuryTransfer reuses the same key and gets the same attempt
// back rather than a second one), then submit it. Returns the resulting
// AttemptID once the attempt is durably SUBMITTED, or a sentinel error —
// PENDING_UNKNOWN and REJECTED_BEFORE_SUBMISSION are both surfaced as
// errors, since neither is a safe state for the caller to treat as done.
func (c *TransferClients) SubmitTreasuryPayment(ctx context.Context, tenantID, principalID, correlationID, legalEntityID, transferID, payerAccountRef, payeeRef, fingerprint string, amount float64, currency string) (string, error) {
	body, err := json.Marshal(prepareAttemptRequest{
		LegalEntityID: legalEntityID, SourceReference: "treasury-transfer:" + transferID,
		AuthorizationFingerprint: fingerprint, AuthorizationID: transferID, AuthorizationSource: authorizationSourceTreasury,
		PayerAccountRef: payerAccountRef, PayeeRef: payeeRef, Amount: amount, Currency: currency,
		ExecutionDate: time.Now().UTC(), PaymentReference: "Treasury transfer " + transferID,
		PayerAccountVerified: true, IdempotencyKey: transferID,
	})
	if err != nil {
		return "", err
	}
	var attempt paymentInitiationAttempt
	if err := c.doJSON(ctx, http.MethodPost, c.paymentAdapterURL+"/bnk06/attempts/", tenantID, principalID, correlationID, body, &attempt); err != nil {
		c.log.Error("payment-initiation-adapter-svc PrepareAttempt failed", zap.Error(err))
		return "", domain.ErrPaymentAdapterUnavailable
	}

	if attempt.Status == "PREPARED" {
		var submitted paymentInitiationAttempt
		if err := c.doJSON(ctx, http.MethodPost, c.paymentAdapterURL+"/bnk06/attempts/"+attempt.AttemptID+"/submit", tenantID, principalID, correlationID, nil, &submitted); err != nil {
			c.log.Error("payment-initiation-adapter-svc SubmitAttempt failed", zap.Error(err))
			return "", domain.ErrPaymentAdapterUnavailable
		}
		attempt = submitted
	}

	switch attempt.Status {
	case "SUBMITTED":
		return attempt.AttemptID, nil
	case "REJECTED_BEFORE_SUBMISSION":
		return "", domain.ErrPaymentRejected
	default:
		// PENDING_UNKNOWN (or anything else unexpected): the network call
		// to the provider is in an unresolved state — not safe to treat as
		// submitted, but also not a hard failure to retry blindly. Surface
		// as unavailable so ExecuteTreasuryTransfer's caller can decide
		// whether to poll/retry rather than silently completing.
		return "", domain.ErrPaymentAdapterUnavailable
	}
}

// postingEventLine uses MappingKey rather than AccountCode: treasury-svc
// doesn't own a chart-of-accounts, so it declares the semantic mapping
// key and lets general-ledger-svc's own account-mappings table resolve
// the real account code — the same separation of concerns the doc
// requires ("never writes the GL directly"). This requires
// TREASURY_TRANSFER_SOURCE/TREASURY_TRANSFER_TARGET mapping keys to be
// configured in general-ledger-svc; documented here as the honest
// deployment prerequisite rather than a fabricated account code.
type postingEventLine struct {
	MappingKey   string  `json:"mapping_key"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
}

type postAccountingEventRequest struct {
	LegalEntityID string             `json:"legal_entity_id"`
	FiscalPeriod  string             `json:"fiscal_period"`
	Description   string             `json:"description"`
	SourceEventID string             `json:"source_event_id"`
	CorrelationID string             `json:"correlation_id"`
	Lines         []postingEventLine `json:"lines"`
}

type postingExecutionResponse struct {
	JournalID *string `json:"journal_id"`
	Status    string  `json:"status"`
}

// PostTreasuryTransferJournal is BNK-09's cross-entity call into
// general-ledger-svc, using the transfer_id as SourceEventID —
// general-ledger-svc's own (tenant_id, source_event_id) uniqueness makes
// a resumed ExecuteTreasuryTransfer's re-post return the prior execution
// rather than posting twice.
func (c *TransferClients) PostTreasuryTransferJournal(ctx context.Context, tenantID, principalID, correlationID, legalEntityID, fiscalPeriod, transferID string, amount float64) (string, error) {
	body, err := json.Marshal(postAccountingEventRequest{
		LegalEntityID: legalEntityID, FiscalPeriod: fiscalPeriod,
		Description:   "Treasury transfer " + transferID,
		SourceEventID: transferID, CorrelationID: correlationID,
		Lines: []postingEventLine{
			{MappingKey: "TREASURY_TRANSFER_SOURCE", CreditAmount: amount},
			{MappingKey: "TREASURY_TRANSFER_TARGET", DebitAmount: amount},
		},
	})
	if err != nil {
		return "", err
	}
	var resp postingExecutionResponse
	if err := c.doJSON(ctx, http.MethodPost, c.ledgerURL+"/v1/postings/events", tenantID, principalID, correlationID, body, &resp); err != nil {
		c.log.Error("general-ledger-svc PostAccountingEvent failed", zap.Error(err))
		return "", domain.ErrGLServiceUnavailable
	}
	if resp.JournalID == nil {
		c.log.Error("general-ledger-svc PostAccountingEvent returned no journal_id", zap.String("status", resp.Status))
		return "", domain.ErrGLServiceUnavailable
	}
	return *resp.JournalID, nil
}

type createIntercompanyEntryRequest struct {
	SourceLegalEntityID string  `json:"source_legal_entity_id"`
	TargetLegalEntityID string  `json:"target_legal_entity_id"`
	SourceJournalID     string  `json:"source_journal_id"`
	Amount              float64 `json:"amount"`
	CurrencyCode        string  `json:"currency_code"`
}

type intercompanyEntry struct {
	IntercompanyEntryID string `json:"intercompany_entry_id"`
}

// PairTreasuryTransferIntercompany calls intercompany-accounting-svc with
// the journal_id PostTreasuryTransferJournal just produced —
// intercompany-accounting-svc dedupes on source_journal_id, so a resumed
// call is safe to repeat.
func (c *TransferClients) PairTreasuryTransferIntercompany(ctx context.Context, tenantID, principalID, correlationID, sourceLegalEntityID, targetLegalEntityID, sourceJournalID string, amount float64, currencyCode string) (string, error) {
	body, err := json.Marshal(createIntercompanyEntryRequest{
		SourceLegalEntityID: sourceLegalEntityID, TargetLegalEntityID: targetLegalEntityID,
		SourceJournalID: sourceJournalID, Amount: amount, CurrencyCode: currencyCode,
	})
	if err != nil {
		return "", err
	}
	var entry intercompanyEntry
	if err := c.doJSON(ctx, http.MethodPost, c.intercompanyURL+"/v1/intercompany/entries", tenantID, principalID, correlationID, body, &entry); err != nil {
		c.log.Error("intercompany-accounting-svc CreateEntry failed", zap.Error(err))
		return "", domain.ErrIntercompanyServiceUnavailable
	}
	if entry.IntercompanyEntryID == "" {
		return "", domain.ErrIntercompanyServiceUnavailable
	}
	return entry.IntercompanyEntryID, nil
}

// doJSON is the shared request/response plumbing for all three BNK-09
// outbound calls: POST with the standard identity headers, any non-2xx
// status treated as failure, response decoded into out.
func (c *TransferClients) doJSON(ctx context.Context, method, url, tenantID, principalID, correlationID string, body []byte, out interface{}) error {
	var bodyReader *bytes.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	} else {
		bodyReader = bytes.NewReader([]byte{})
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
