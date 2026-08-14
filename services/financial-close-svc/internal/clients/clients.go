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

// ---------------------------------------------------------------------------
// Authorization Client
// ---------------------------------------------------------------------------

type authzReq struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

type authzResp struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
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

	resp, err := c.http.Do(req)
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

	if !authResp.Allowed {
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

func (c *Clients) GetUnpostedJournalsCount(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (int, error) {
	u, err := url.Parse(c.ledgerURL + "/v1/journals")
	if err != nil {
		return 0, err
	}
	q := u.Query()
	q.Set("tenant_id", tenantID)
	q.Set("legal_entity_id", legalEntityID)
	q.Set("fiscal_period", fiscalPeriod)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, domain.ErrGLServiceUnavailable
	}

	var list []glJournal
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return 0, err
	}

	unposted := 0
	for _, j := range list {
		if j.Status == "PENDING" || j.Status == "VALIDATED" {
			unposted++
		}
	}
	return unposted, nil
}

func (c *Clients) CompileTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (map[string]float64, error) {
	u, err := url.Parse(c.ledgerURL + "/v1/journals")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("tenant_id", tenantID)
	q.Set("legal_entity_id", legalEntityID)
	q.Set("fiscal_period", fiscalPeriod)
	q.Set("status", "FINALIZED")
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

	balances := make(map[string]float64)

	for _, j := range list {
		// Fetch lines for each finalized journal
		lines, err := c.getJournalLines(ctx, tenantID, j.JournalID)
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			net := line.DebitAmount - line.CreditAmount
			balances[line.AccountCode] += net
		}
	}

	return balances, nil
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
}

func (c *Clients) GetUnsettledAPInvoicesCount(ctx context.Context, tenantID, legalEntityID string) (int, error) {
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
		// Invoices not completed/settled (i.e. not PAYMENT_REQUESTED status or equivalent paid status)
		if inv.Status != "PAYMENT_REQUESTED" {
			unsettled++
		}
	}
	return unsettled, nil
}

// ---------------------------------------------------------------------------
// Accounts Receivable Client
// ---------------------------------------------------------------------------

type arInvoice struct {
	InvoiceID string `json:"invoice_id"`
	Status    string `json:"status"`
}

func (c *Clients) GetUnsettledARInvoicesCount(ctx context.Context, tenantID, legalEntityID string) (int, error) {
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

type docInner struct {
	DocumentID string `json:"document_id"`
}

type docResp struct {
	Document docInner `json:"document"`
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
	req.Header.Set("X-Principal-Id", principalID) // Pass principal as creator

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

	if dResp.Document.DocumentID == "" {
		return "", errors.New("document_id was missing in vault response")
	}

	return dResp.Document.DocumentID, nil
}
