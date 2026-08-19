package clients

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"zoiko.io/financial-close-svc/internal/domain"
)

// decisionCacheTTL bounds how long a GRANTED/DENIED decision from
// authorization-svc may be reused locally before it is asked again.
//
// Doc 05 (Security Architecture Specification) §6.5 anticipates exactly
// this cost: "For Tier 0 and latency-sensitive services, policy and
// authorization evaluation may use high-speed distributed enforcement
// patterns, including local policy caches... provided policy source
// remains centralized, policy provenance is auditable, stale decision
// risk is bounded, fail-safe behavior is defined." This constant is that
// bound — short enough that a permission revocation or role change
// propagates within one cache generation, long enough to absorb the
// repeat checks a single user action or request burst produces.
//
// Only real GRANTED/DENIED decisions are ever cached. An unreachable or
// misbehaving authorization-svc is never cached — that would turn one
// transient outage into a standing permit-or-deny for every subsequent
// caller on this instance, which defeats fail-closed.
const decisionCacheTTL = 5 * time.Second

type cachedDecision struct {
	deniedErr error
	expiresAt time.Time
}

type Clients struct {
	authzURL  string
	ledgerURL string
	apURL     string
	arURL     string
	vaultURL  string
	http      *http.Client
	log       *zap.Logger

	// authzHTTP, when set, is used instead of http for calls to
	// authorization-svc only — the mTLS pilot's Transport carries this
	// service's leaf certificate and trusts authorization-svc's CA (see
	// internal/mtls.NewClientHTTPClient). The GL/AP/AR/vault clients in
	// this same struct keep using the plain http field.
	authzHTTP *http.Client

	cacheMu     sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

func New(authzURL, ledgerURL, apURL, arURL, vaultURL string, log *zap.Logger) *Clients {
	return &Clients{
		authzURL:  authzURL,
		ledgerURL: ledgerURL,
		apURL:     apURL,
		arURL:     arURL,
		vaultURL:  vaultURL,
		http:      &http.Client{Timeout: 5 * time.Second, Transport: newRetryTransport()},
		log:       log,
		cache:     make(map[string]cachedDecision),
	}
}

// NewWithAuthzHTTPClient is New but with a caller-supplied *http.Client used
// solely for calls to authorization-svc — used for the mTLS pilot. Every
// other outbound client (GL/AP/AR/vault) built here is unaffected and keeps
// using the plain, non-mTLS http.Client.
func NewWithAuthzHTTPClient(authzURL, ledgerURL, apURL, arURL, vaultURL string, log *zap.Logger, authzHTTPClient *http.Client) *Clients {
	return &Clients{
		authzURL:  authzURL,
		ledgerURL: ledgerURL,
		apURL:     apURL,
		arURL:     arURL,
		vaultURL:  vaultURL,
		http:      &http.Client{Timeout: 5 * time.Second, Transport: newRetryTransport()},
		authzHTTP: authzHTTPClient,
		log:       log,
		cache:     make(map[string]cachedDecision),
	}
}

// ---------------------------------------------------------------------------
// Authorization Client
// ---------------------------------------------------------------------------

type authzReq struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

// authzResp is authorization-svc's actual reply shape.
//
// This used to decode `{"allowed": bool, "reason": string}` — two fields that
// service has never returned. It answers `{"decision_outcome": "GRANTED",
// "decision_basis": "..."}`, so `allowed` was always absent, always decoded to
// Go's zero value of false, and EVERY authorization check therefore denied.
// Register a period, list the register, check readiness, close a period: all of
// them 403, for every principal, no matter what was granted. The entire write
// surface of this service was dead.
//
// Nothing caught it because the failure is invisible from both sides: the unit
// tests stub this client out, so they never exercise the parsing, and a
// fail-closed authorization check that always fails closed looks exactly like a
// permission that was never granted. It took running the service against the
// real authorization-svc with a grant that provably existed.
//
// Matches general-ledger-svc's authz client, which had it right.
type authzResp struct {
	DecisionOutcome string `json:"decision_outcome"`
	DecisionBasis   string `json:"decision_basis"`
}

func (c *Clients) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	key := principalID + "|" + legalEntityID + "|" + actionType

	if decision, hit := c.lookupCache(key); hit {
		return decision
	}

	err := c.checkAllowedLive(ctx, principalID, legalEntityID, actionType)

	// Cache the decision itself (GRANTED or DENIED), never an unavailable
	// outcome — see the doc comment on decisionCacheTTL.
	if err == nil || errors.Is(err, domain.ErrAuthorizationDenied) {
		c.storeCache(key, err)
	}

	return err
}

// lookupCache returns the cached decision for key and whether it is still
// within decisionCacheTTL. An expired entry is evicted on read.
func (c *Clients) lookupCache(key string) (error, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	d, ok := c.cache[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(d.expiresAt) {
		delete(c.cache, key)
		return nil, false
	}
	return d.deniedErr, true
}

// storeCache records a real GRANTED/DENIED decision. Every 1000th write
// sweeps expired entries so a long-lived instance with many distinct
// (principal, entity, action) combinations doesn't grow the map
// unboundedly between reads of the same key.
func (c *Clients) storeCache(key string, decision error) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	c.cache[key] = cachedDecision{deniedErr: decision, expiresAt: time.Now().Add(decisionCacheTTL)}

	c.cacheWrites++
	if c.cacheWrites%1000 == 0 {
		now := time.Now()
		for k, v := range c.cache {
			if now.After(v.expiresAt) {
				delete(c.cache, k)
			}
		}
	}
}

// checkAllowedLive is the real, uncached call to authorization-svc.
func (c *Clients) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, _ := json.Marshal(authzReq{
		PrincipalID:   principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.authzURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := c.http
	if c.authzHTTP != nil {
		httpClient = c.authzHTTP
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		c.log.Error("failed to call authorization-svc", zap.Error(err))
		return domain.ErrAuthzServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return domain.ErrAuthzServiceUnavailable
	}

	var authResp authzResp
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return err
	}

	// Anything other than an explicit GRANTED is a denial, including an empty
	// outcome — a reply this service cannot understand must never read as
	// permission.
	if authResp.DecisionOutcome != "GRANTED" {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

// ---------------------------------------------------------------------------
// General Ledger Client
// ---------------------------------------------------------------------------

type glJournal struct {
	JournalID    string `json:"journal_id"`
	Status       string `json:"status"`
	FiscalPeriod string `json:"fiscal_period"`
}

type glJournalLine struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount"`
	CreditAmount float64 `json:"credit_amount"`
}

type glJournalWithLines struct {
	JournalID string          `json:"journal_id"`
	Lines     []glJournalLine `json:"lines"`
}

// unpostedStatuses are the journal states that have NOT reached the books. A
// period cannot be closed while any of them exist for it.
var unpostedStatuses = []string{"PENDING", "VALIDATED"}

// GetUnpostedJournalsCount counts journals in the period that are still drafts.
//
// Asks the ledger for each unposted state by name rather than fetching the
// whole period and filtering here: the register is bounded, so "fetch
// everything and count the ones I care about" silently under-counts as soon as
// a period has more journals than one page — and under-counting here reads as
// "ready to close".
func (c *Clients) GetUnpostedJournalsCount(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (int, error) {
	unposted := 0
	for _, status := range unpostedStatuses {
		journals, err := c.listJournals(ctx, tenantID, legalEntityID, fiscalPeriod, status)
		if err != nil {
			return 0, err
		}
		unposted += len(journals)
	}
	return unposted, nil
}

// ledgerPageLimit is the largest page general-ledger-svc will serve
// (store.MaxListLimit there). Asked for explicitly rather than relying on its
// default, which is 200 — see the truncation check below for why that number
// silently mattered.
const ledgerPageLimit = 1000

// postedStatuses are the journal states whose lines are ON the books.
//
// FINALIZED alone is NOT the answer, and using it was an accounting defect that
// mis-stated the trial balance by exactly double the value of every reversal.
// A reversal in general-ledger-svc does not erase anything: the original moves
// to REVERSED and keeps its lines, and a NEW journal is posted FINALIZED
// carrying the exact inverse. Both entries are real postings and they net to
// zero. Filtering to FINALIZED dropped the original while keeping its inverse,
// so a reversed 100.00 debit read as a 100.00 CREDIT on the trial balance
// instead of as nothing at all — and that balance is what gets hashed, signed
// and locked as the period's evidence.
var postedStatuses = []string{"FINALIZED", "REVERSED"}

// CompileTrialBalance sums every posted line for the period into per-account
// net balances (debit positive, credit negative).
func (c *Clients) CompileTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (map[string]float64, error) {
	balances := make(map[string]float64)

	for _, status := range postedStatuses {
		journals, err := c.listJournals(ctx, tenantID, legalEntityID, fiscalPeriod, status)
		if err != nil {
			return nil, err
		}
		for _, j := range journals {
			lines, err := c.getJournalLines(ctx, tenantID, j.JournalID)
			if err != nil {
				return nil, err
			}
			for _, line := range lines {
				balances[line.AccountCode] += line.DebitAmount - line.CreditAmount
			}
		}
	}

	return balances, nil
}

// listJournals fetches one page of journals and REFUSES a full page.
//
// general-ledger-svc bounds its register, so a period with more journals than
// the cap comes back truncated with nothing in the response to say so. A
// trial balance compiled from a truncated ledger is not a smaller trial
// balance, it is a wrong one — and this one is about to be hashed, signed, and
// locked as the period's permanent evidence. Refusing to produce it is the only
// honest option; a close that fails loudly can be retried, a close that quietly
// omits journals cannot be detected afterwards.
func (c *Clients) listJournals(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, status string) ([]glJournal, error) {
	u, err := url.Parse(c.ledgerURL + "/v1/journals")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("tenant_id", tenantID)
	q.Set("legal_entity_id", legalEntityID)
	q.Set("fiscal_period", fiscalPeriod)
	q.Set("status", status)
	q.Set("limit", strconv.Itoa(ledgerPageLimit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, domain.ErrGLServiceUnavailable
	}

	var list []glJournal
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}

	if len(list) >= ledgerPageLimit {
		c.log.Error("ledger page came back full — the trial balance may be incomplete",
			zap.String("fiscal_period", fiscalPeriod),
			zap.String("status", status),
			zap.Int("returned", len(list)))
		return nil, domain.ErrLedgerPageTruncated
	}

	return list, nil
}

func (c *Clients) getJournalLines(ctx context.Context, tenantID, journalID string) ([]glJournalLine, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v1/journals/%s", c.ledgerURL, journalID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, domain.ErrGLServiceUnavailable
	}

	var detail glJournalWithLines
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}

	return detail.Lines, nil
}

// ---------------------------------------------------------------------------
// Accounts Payable Client
// ---------------------------------------------------------------------------

type apInvoice struct {
	InvoiceID string `json:"invoice_id"`
	Status    string `json:"status"`
	// DueDate is the only business date accounts-payable-svc carries — there is
	// no invoice_date column — so it is what places an invoice in a period.
	DueDate time.Time `json:"due_date"`
}

// GetUnsettledAPInvoicesCount counts payables belonging to THIS period that have
// not reached PAYMENT_REQUESTED.
//
// The period bounds are the fix, not a refinement. This counted every unsettled
// invoice for the legal entity regardless of date, so a single invoice raised in
// December blocked the close of every month of the year — and since a going
// concern always has something outstanding, no period could ever be closed at
// all. The service's central operation was unreachable in normal operation.
//
// accounts-payable-svc's list endpoint has no date filter, so the period bound
// is applied here. That means the whole register is fetched to count a subset;
// acceptable because it is bounded by the entity and this runs once per close,
// and preferable to a count that is wrong.
func (c *Clients) GetUnsettledAPInvoicesCount(ctx context.Context, tenantID, legalEntityID string, periodStart, periodEnd time.Time) (int, error) {
	u, err := url.Parse(c.apURL + "/v1/invoices")
	if err != nil {
		return 0, err
	}
	q := u.Query()
	q.Set("tenant_id", tenantID)
	q.Set("legal_entity_id", legalEntityID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, domain.ErrAPServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, domain.ErrAPServiceUnavailable
	}

	var list []apInvoice
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return 0, err
	}

	unsettled := 0
	for _, inv := range list {
		if !withinPeriod(inv.DueDate, periodStart, periodEnd) {
			continue
		}
		// PAYMENT_REQUESTED is terminal in accounts-payable-svc — executing the
		// payment belongs to Treasury — so it is as settled as that service can
		// report.
		if inv.Status != "PAYMENT_REQUESTED" {
			unsettled++
		}
	}
	return unsettled, nil
}

// withinPeriod reports whether a business date falls inside the fiscal period,
// inclusive of both ends.
//
// Compared as calendar days in UTC. The period bounds are timestamps, and an
// invoice due on the last day of the period carries midnight UTC, so a
// strict instant comparison against a period_end of that same midnight would
// include it while one against 00:00:00.000001 would not — a boundary an
// operator cannot see and would never predict.
func withinPeriod(date, start, end time.Time) bool {
	if date.IsZero() {
		// No business date at all. Counted as in-period rather than skipped: an
		// invoice that cannot be placed in time is exactly the kind of thing a
		// close should stop for, and silently excluding it would let it slip
		// past every period forever.
		return true
	}
	d := day(date)
	return !d.Before(day(start)) && !d.After(day(end))
}

func day(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// Accounts Receivable Client
// ---------------------------------------------------------------------------

type arInvoice struct {
	InvoiceID string    `json:"invoice_id"`
	Status    string    `json:"status"`
	DueDate   time.Time `json:"due_date"`
}

// GetUnsettledARInvoicesCount counts receivables belonging to THIS period that
// are not PAID. Period-bounded for the same reason as the payables count above.
func (c *Clients) GetUnsettledARInvoicesCount(ctx context.Context, tenantID, legalEntityID string, periodStart, periodEnd time.Time) (int, error) {
	u, err := url.Parse(c.arURL + "/v1/invoices")
	if err != nil {
		return 0, err
	}
	q := u.Query()
	q.Set("tenant_id", tenantID)
	q.Set("legal_entity_id", legalEntityID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, domain.ErrARServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, domain.ErrARServiceUnavailable
	}

	var list []arInvoice
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return 0, err
	}

	unsettled := 0
	for _, inv := range list {
		if !withinPeriod(inv.DueDate, periodStart, periodEnd) {
			continue
		}
		if inv.Status != "PAID" {
			unsettled++
		}
	}
	return unsettled, nil
}

// ---------------------------------------------------------------------------
// Document Vault Client
// ---------------------------------------------------------------------------

type docReq struct {
	TenantID        string `json:"tenant_id"`
	LegalEntityID   string `json:"legal_entity_id"`
	Title           string `json:"title"`
	Classification  string `json:"classification"`
	RetentionPolicy string `json:"retention_policy"`
	ContentType     string `json:"content_type"`
	ContentBase64   string `json:"content_base64"`
}

// docResp is document-vault-svc's actual reply to a create.
//
// It returns the document object itself, NOT wrapped in a "document" envelope —
// this used to decode `{"document": {"document_id": …}}`, so the id was always
// empty and every close ended in "document_id was missing in vault response".
// The third response-shape mismatch in this service's outbound clients, all of
// them invisible to a test suite that stubs the dependency out.
type docResp struct {
	DocumentID string `json:"document_id"`
}

func (c *Clients) UploadCloseEvidence(ctx context.Context, tenantID, legalEntityID, periodName string, trialBalance map[string]float64, principalID string) (string, error) {
	contentBytes, err := json.Marshal(trialBalance)
	if err != nil {
		return "", err
	}

	contentBase64 := base64.StdEncoding.EncodeToString(contentBytes)

	reqBody, _ := json.Marshal(docReq{
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		Title:           fmt.Sprintf("Close Evidence Trail Balance — %s", periodName),
		Classification:  "CONFIDENTIAL",
		RetentionPolicy: "DEFAULT",
		ContentType:     "application/json",
		ContentBase64:   contentBase64,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.vaultURL+"/v1/documents", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principalID) // the creator recorded on the document
	req.Header.Set("X-Tenant-Id", tenantID)       // tenant scope, as every other call here sends

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to call document-vault-svc", zap.Error(err))
		return "", domain.ErrVaultServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		c.log.Error("document-vault-svc returned non-200/201 status", zap.Int("status", resp.StatusCode))
		return "", domain.ErrVaultServiceUnavailable
	}

	var dResp docResp
	if err := json.NewDecoder(resp.Body).Decode(&dResp); err != nil {
		return "", err
	}

	if dResp.DocumentID == "" {
		return "", errors.New("document_id was missing in vault response")
	}

	return dResp.DocumentID, nil
}
