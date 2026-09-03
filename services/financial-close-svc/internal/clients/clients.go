package clients

import (
	svcenvelope "zoiko.io/financial-close-svc/internal/envelope"
	"github.com/go-chi/chi/v5/middleware"
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

	// authorization-svc validates the same canonical envelope contract this
	// service does and answers 400 envelope_incomplete without it. A non-200 is
	// treated as unavailable below, so an unforwarded envelope turned EVERY
	// gated write into a 503 that reads like an outage rather than a missing
	// header. Same defect, same fix as 3c618c2 (HR) and dbf6e45 (notification).
	//
	// The values are the CALLER's, taken from the envelope the middleware
	// already parsed into this request's context. Minting fresh ones would
	// satisfy the contract and lose the only thing it is for: a decision in
	// access_decision_log traceable to the request that caused it.
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)

	authzRequestID := middleware.GetReqID(ctx)
	// Service-to-service. "system" is in the contract's accepted set; the
	// caller's own channel replaces it when the envelope carries one.
	authzSourceChannel := "system"
	if env, ok := svcenvelope.FromContext(ctx); ok {
		if env.TenantID != "" {
			req.Header.Set("X-Tenant-Id", env.TenantID)
		}
		if env.RequestID != "" {
			authzRequestID = env.RequestID
		}
		if env.SourceChannel != "" {
			authzSourceChannel = string(env.SourceChannel)
		}
		if env.CorrelationID != "" {
			req.Header.Set("X-Correlation-ID", env.CorrelationID)
		}
		if env.CausationID != "" {
			req.Header.Set("X-Causation-Id", env.CausationID)
		}
	}
	req.Header.Set("X-Request-Id", authzRequestID)
	req.Header.Set("X-Source-Channel", authzSourceChannel)
	// One decision per (request, action): an inbound request may authorize
	// several actions, and each is its own decision to record.
	req.Header.Set("Idempotency-Key", authzRequestID+":"+actionType)
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

// tbCompileRequest/tbCompileResponse mirror general-ledger-svc's own
// domain.CompileTrialBalanceRequest/TrialBalanceSnapshot wire shapes —
// duplicated rather than imported, same posture this file already takes
// with glJournal/glJournalLine, since these two services share no Go
// module.
type tbCompileRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	FiscalPeriod  string `json:"fiscal_period"`
}

type tbLine struct {
	AccountCode string  `json:"account_code"`
	NetBalance  float64 `json:"net_balance"`
}

type tbSnapshot struct {
	TrialBalanceSnapshotID string   `json:"trial_balance_snapshot_id"`
	LedgerWatermark        int64    `json:"ledger_watermark"`
	Lines                  []tbLine `json:"lines"`
}

// CompileTrialBalance asks general-ledger-svc's own authoritative,
// watermarked trial-balance endpoint to compile the period — replacing
// what used to be this service re-deriving one client-side by paging raw
// journals itself (see master-register-findings-2026-08-27.md §3.32).
// general-ledger-svc owns the "FINALIZED alone double-counts a reversal"
// correctness rule now — this client's only job is to ask and parse the
// answer, not re-implement ledger semantics a second time.
//
// principalID is forwarded as X-Principal-Id: the new endpoint records who
// compiled each durable snapshot, and there is no separate "financial-
// close-svc" system identity anywhere on this platform to invent — the
// principal already driving this close is the real caller.
func (c *Clients) CompileTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, principalID string) (map[string]float64, error) {
	body, err := json.Marshal(tbCompileRequest{LegalEntityID: legalEntityID, FiscalPeriod: fiscalPeriod})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ledgerURL+"/v1/trial-balance/compile", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to compile trial balance via general-ledger-svc", zap.Error(err))
		return nil, domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		c.log.Error("general-ledger-svc refused to compile trial balance",
			zap.Int("status", resp.StatusCode), zap.String("fiscal_period", fiscalPeriod))
		return nil, domain.ErrGLServiceUnavailable
	}

	var snap tbSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, err
	}

	balances := make(map[string]float64, len(snap.Lines))
	for _, l := range snap.Lines {
		balances[l.AccountCode] = l.NetBalance
	}
	return balances, nil
}

// glAccountMapping mirrors general-ledger-svc's ACC-02
// domain.AccountMapping wire shape — only the field this client needs.
type glAccountMapping struct {
	AccountCode string `json:"account_code"`
}

// GetControlAccountCode resolves an ACC-06 caller-declared
// control_account_mapping_key to the real, chart-registered account code
// it currently names, via general-ledger-svc's ACC-02 mapping endpoint.
// ACC-06 must never guess or hardcode which GL account a subledger
// reconciles against — the mapping is the single source of truth, same
// doctrine CompileTrialBalance already applies to journal semantics.
func (c *Clients) GetControlAccountCode(ctx context.Context, tenantID, mappingKey string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ledgerURL+"/v1/account-mappings/"+url.PathEscape(mappingKey), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", domain.ErrControlAccountMappingNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", domain.ErrGLServiceUnavailable
	}

	var m glAccountMapping
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return "", err
	}
	return m.AccountCode, nil
}

// glCreateJournalRequest/glJournalLineInput/glJournalCreateResponse mirror
// general-ledger-svc's own CreateJournalRequest/CreateJournalLineInput/
// journal-create response wire shapes.
type glCreateJournalRequest struct {
	TenantID      string               `json:"tenant_id"`
	LegalEntityID string               `json:"legal_entity_id"`
	FiscalPeriod  string               `json:"fiscal_period"`
	Description   string               `json:"description"`
	CorrelationID string               `json:"correlation_id"`
	Lines         []glJournalLineInput `json:"lines"`
}

type glJournalLineInput struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
	Description  string  `json:"description,omitempty"`
}

type glJournalCreateResponse struct {
	JournalID string `json:"journal_id"`
	Status    string `json:"status"`
}

// PostAccrualRecognitionJournal creates, validates and posts a balanced
// journal entry for one period's accrual recognition — ACC-07 "must never
// own: Direct ledger writes," so this is the ONLY way a recognition
// becomes an accounting fact: through general-ledger-svc's own
// Create/Validate/Post lifecycle, same as goods-service-receipt-svc's
// PostGRNI client and every other capability that posts to the ledger.
//
// correlationID is the caller's idempotency key (this schedule's own
// schedule_id + fiscal_period). general-ledger-svc's CreateJournal is
// itself idempotent on correlation_id — a replayed recognition run for an
// already-posted period gets the SAME journal back (created=false)
// instead of a duplicate, satisfying the spec's own negative-path
// requirement for "Recognition run replay" without this client needing to
// implement replay detection itself.
func (c *Clients) PostAccrualRecognitionJournal(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, correlationID, principalID, description, debitAccountCode, creditAccountCode string, amount float64) (journalID string, err error) {
	body := glCreateJournalRequest{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		FiscalPeriod:  fiscalPeriod,
		Description:   description,
		CorrelationID: correlationID,
		Lines: []glJournalLineInput{
			{AccountCode: debitAccountCode, DebitAmount: amount, Description: description},
			{AccountCode: creditAccountCode, CreditAmount: amount, Description: description},
		},
	}
	journal, err := c.createGLJournal(ctx, tenantID, principalID, body)
	if err != nil {
		return "", err
	}
	if journal.Status == "PENDING" {
		if err := c.transitionGLJournal(ctx, tenantID, principalID, journal.JournalID, "validate"); err != nil {
			return "", err
		}
		journal.Status = "VALIDATED"
	}
	if journal.Status == "VALIDATED" {
		if err := c.transitionGLJournal(ctx, tenantID, principalID, journal.JournalID, "post"); err != nil {
			return "", err
		}
	}
	return journal.JournalID, nil
}

func (c *Clients) createGLJournal(ctx context.Context, tenantID, principalID string, body glCreateJournalRequest) (*glJournalCreateResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ledgerURL+"/v1/journals", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, domain.ErrJournalPostingFailed
	}
	var out glJournalCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.JournalID == "" {
		return nil, domain.ErrGLServiceUnavailable
	}
	return &out, nil
}

func (c *Clients) transitionGLJournal(ctx context.Context, tenantID, principalID, journalID, action string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ledgerURL+"/v1/journals/"+journalID+"/"+action, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		return domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return domain.ErrJournalPostingFailed
	}
	return nil
}

// glAccountLookup mirrors general-ledger-svc's own ACC-01 domain.Account
// wire shape — only the fields this client needs.
type glAccountLookup struct {
	AccountCode string `json:"account_code"`
	Status      string `json:"status"`
}

// GetAccountStatus resolves an account code against GL's own ACC-01 Chart
// of Accounts — ACC-09 uses this to validate a driver's
// recipient_account_code at rule approval time (the spec's own negative
// path, "Recipient dimension invalid," made a real, blocking check rather
// than trusting whatever code the caller typed).
func (c *Clients) GetAccountStatus(ctx context.Context, tenantID, principalID, accountCode string) (status string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ledgerURL+"/v1/chart-of-accounts/"+url.PathEscape(accountCode), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", domain.ErrRecipientAccountInvalid
	}
	if resp.StatusCode != http.StatusOK {
		return "", domain.ErrGLServiceUnavailable
	}
	var a glAccountLookup
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return "", err
	}
	return a.Status, nil
}

// PostAllocationJournal posts ONE balanced journal crediting the
// allocation's source account for the full amount and debiting every
// recipient for its computed share — a genuine multi-line posting, unlike
// ACC-07/08's fixed two-line PostAccrualRecognitionJournal, reusing the
// same underlying createGLJournal/transitionGLJournal Create-Validate-Post
// primitives rather than duplicating them.
func (c *Clients) PostAllocationJournal(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, correlationID, principalID, description, sourceAccountCode string, sourceAmount float64, debitLines []domain.AllocationJournalLine) (journalID string, err error) {
	lines := make([]glJournalLineInput, 0, len(debitLines)+1)
	for _, l := range debitLines {
		lines = append(lines, glJournalLineInput{AccountCode: l.AccountCode, DebitAmount: l.Amount, Description: description})
	}
	lines = append(lines, glJournalLineInput{AccountCode: sourceAccountCode, CreditAmount: sourceAmount, Description: description})

	body := glCreateJournalRequest{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		FiscalPeriod:  fiscalPeriod,
		Description:   description,
		CorrelationID: correlationID,
		Lines:         lines,
	}
	journal, err := c.createGLJournal(ctx, tenantID, principalID, body)
	if err != nil {
		return "", err
	}
	if journal.Status == "PENDING" {
		if err := c.transitionGLJournal(ctx, tenantID, principalID, journal.JournalID, "validate"); err != nil {
			return "", err
		}
		journal.Status = "VALIDATED"
	}
	if journal.Status == "VALIDATED" {
		if err := c.transitionGLJournal(ctx, tenantID, principalID, journal.JournalID, "post"); err != nil {
			return "", err
		}
	}
	return journal.JournalID, nil
}

// GetAccountType resolves an account's AccountType (ASSET, LIABILITY,
// EQUITY, REVENUE, EXPENSE) via GL's own ACC-01 Chart of Accounts —
// ACC-10 uses this to enforce its own negative path, "Non-monetary item
// included": only ASSET/LIABILITY accounts carry a monetary balance
// subject to revaluation.
func (c *Clients) GetAccountType(ctx context.Context, tenantID, principalID, accountCode string) (accountType string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ledgerURL+"/v1/chart-of-accounts/"+url.PathEscape(accountCode), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", domain.ErrNonMonetaryItemIncluded
	}
	if resp.StatusCode != http.StatusOK {
		return "", domain.ErrGLServiceUnavailable
	}
	var a struct {
		AccountType string `json:"account_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return "", err
	}
	return a.AccountType, nil
}

// PostMultiLineJournal posts one journal with arbitrary mixed debit/credit
// lines — ACC-10's FX revaluation needs both signs in a single posting
// (some monetary items move by a debit, others by a credit), unlike
// ACC-09's fixed all-debits-plus-one-credit shape. Reuses the same
// createGLJournal/transitionGLJournal Create-Validate-Post primitives
// every other ACC posting client in this file already relies on.
func (c *Clients) PostMultiLineJournal(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, correlationID, principalID, description string, lines []domain.JournalLineInput) (journalID string, err error) {
	glLines := make([]glJournalLineInput, len(lines))
	for i, l := range lines {
		glLines[i] = glJournalLineInput{AccountCode: l.AccountCode, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount, Description: description}
	}
	body := glCreateJournalRequest{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		FiscalPeriod:  fiscalPeriod,
		Description:   description,
		CorrelationID: correlationID,
		Lines:         glLines,
	}
	journal, err := c.createGLJournal(ctx, tenantID, principalID, body)
	if err != nil {
		return "", err
	}
	if journal.Status == "PENDING" {
		if err := c.transitionGLJournal(ctx, tenantID, principalID, journal.JournalID, "validate"); err != nil {
			return "", err
		}
		journal.Status = "VALIDATED"
	}
	if journal.Status == "VALIDATED" {
		if err := c.transitionGLJournal(ctx, tenantID, principalID, journal.JournalID, "post"); err != nil {
			return "", err
		}
	}
	return journal.JournalID, nil
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
	Amount  float64   `json:"amount"`
}

// subledgerPageLimit is the largest page AP/AR will serve one request
// (each service's own maxLimit). Asked for explicitly, same reasoning as
// ledgerPageLimit above: a caller that never asks silently gets the
// service's default page instead.
const subledgerPageLimit = 500

// GetAPSubledgerTotal sums the OUTSTANDING (not yet PAYMENT_REQUESTED)
// balance of every payable for the legal entity — the AP subledger's own
// total, the other half of ACC-06's reconciliation. Unlike
// GetUnsettledAPInvoicesCount, this is NOT period-bounded by due date: a
// control account balance is a point-in-time total of everything still
// open, not just what happens to fall due within the period being closed.
//
// A full page is refused rather than summed, same doctrine as
// listJournals: a control run is recorded as permanent evidence, and a
// total that might be understated must never be persisted as if it were
// complete.
func (c *Clients) GetAPSubledgerTotal(ctx context.Context, tenantID, legalEntityID string) (float64, error) {
	u, err := url.Parse(c.apURL + "/v1/invoices")
	if err != nil {
		return 0, err
	}
	q := u.Query()
	q.Set("tenant_id", tenantID)
	q.Set("legal_entity_id", legalEntityID)
	q.Set("limit", strconv.Itoa(subledgerPageLimit))
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

	if len(list) >= subledgerPageLimit {
		c.log.Error("AP invoice page came back full — the subledger total may be incomplete",
			zap.String("legal_entity_id", legalEntityID), zap.Int("returned", len(list)))
		return 0, domain.ErrSubledgerPageTruncated
	}

	var total float64
	for _, inv := range list {
		if inv.Status != "PAYMENT_REQUESTED" {
			total += inv.Amount
		}
	}
	return total, nil
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
	Amount    float64   `json:"amount"`
}

// GetARSubledgerTotal sums the OUTSTANDING (not yet PAID) balance of every
// receivable for the legal entity — the AR half of ACC-06, same posture as
// GetAPSubledgerTotal: not period-bounded by due date, and a full page is
// refused rather than summed.
func (c *Clients) GetARSubledgerTotal(ctx context.Context, tenantID, legalEntityID string) (float64, error) {
	u, err := url.Parse(c.arURL + "/v1/invoices")
	if err != nil {
		return 0, err
	}
	q := u.Query()
	q.Set("tenant_id", tenantID)
	q.Set("legal_entity_id", legalEntityID)
	q.Set("limit", strconv.Itoa(subledgerPageLimit))
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

	if len(list) >= subledgerPageLimit {
		c.log.Error("AR invoice page came back full — the subledger total may be incomplete",
			zap.String("legal_entity_id", legalEntityID), zap.Int("returned", len(list)))
		return 0, domain.ErrSubledgerPageTruncated
	}

	var total float64
	for _, inv := range list {
		if inv.Status != "PAID" {
			total += inv.Amount
		}
	}
	return total, nil
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
