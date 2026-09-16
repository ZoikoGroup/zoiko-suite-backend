package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	"zoiko.io/treasury-svc/internal/handler"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

type mockStore struct {
	bankAccounts         map[string]*domain.BankAccount
	evidence             map[string][]domain.OwnershipEvidence
	cashBalances         map[string]*domain.CashBalance
	thresholds           map[string]*domain.LiquidityThreshold
	transfers            map[string]*domain.TreasuryTransfer
	transfersByCorr      map[string]string
	createErr            error
	getErr               error
	listErr              error
	updateErr            error
	balErr               error
	threshErr            error
	transferErr          error
	bnk01Err             error
	allOwnershipVerified bool

	fxRates map[string]*domain.FXRate
}

func newMockStore() *mockStore {
	return &mockStore{
		bankAccounts:    make(map[string]*domain.BankAccount),
		evidence:        make(map[string][]domain.OwnershipEvidence),
		cashBalances:    make(map[string]*domain.CashBalance),
		thresholds:      make(map[string]*domain.LiquidityThreshold),
		transfers:       make(map[string]*domain.TreasuryTransfer),
		transfersByCorr: make(map[string]string),
		// Tests that never call VerifyBankAccountOwnership shouldn't have
		// to also seed ownership evidence just to get past
		// CreateTreasuryTransfer's BNK-01 verification gate.
		allOwnershipVerified: true,
	}
}

func (m *mockStore) CreateBankAccount(ctx context.Context, acct *domain.BankAccount) (bool, error) {
	if m.createErr != nil {
		return false, m.createErr
	}
	if acct.CorrelationID != "" {
		for _, existing := range m.bankAccounts {
			if existing.CorrelationID == acct.CorrelationID {
				*acct = *existing
				return false, nil
			}
		}
	}
	m.bankAccounts[acct.BankAccountID] = acct
	return true, nil
}

// ── BNK-01 ───────────────────────────────────────────────────────────────────

func (m *mockStore) VerifyBankAccountOwnership(ctx context.Context, p domain.VerifyOwnershipParams) (*domain.OwnershipEvidence, error) {
	if m.bnk01Err != nil {
		return nil, m.bnk01Err
	}
	if _, ok := m.bankAccounts[p.BankAccountID]; !ok {
		return nil, domain.ErrBankAccountNotFound
	}
	for i := range m.evidence[p.BankAccountID] {
		superseded := "superseded"
		m.evidence[p.BankAccountID][i].SupersededBy = &superseded
	}
	e := domain.OwnershipEvidence{
		EvidenceID: "ev-" + p.BankAccountID, BankAccountID: p.BankAccountID, TenantID: p.TenantID,
		VerificationMethod: p.VerificationMethod, EvidenceRef: p.EvidenceRef, VerifiedByPrincipalID: p.VerifiedByPrincipalID,
		VerifiedAt: time.Now().UTC(),
	}
	m.evidence[p.BankAccountID] = append(m.evidence[p.BankAccountID], e)
	return &e, nil
}

func (m *mockStore) ListOwnershipEvidence(ctx context.Context, tenantID, bankAccountID string) ([]domain.OwnershipEvidence, error) {
	if m.bnk01Err != nil {
		return nil, m.bnk01Err
	}
	return m.evidence[bankAccountID], nil
}

func (m *mockStore) IsOwnershipVerified(ctx context.Context, tenantID, bankAccountID string) (bool, error) {
	if m.bnk01Err != nil {
		return false, m.bnk01Err
	}
	if m.allOwnershipVerified {
		return true, nil
	}
	for _, e := range m.evidence[bankAccountID] {
		if e.SupersededBy == nil {
			return true, nil
		}
	}
	return false, nil
}

func (m *mockStore) transition(bankAccountID string, allowed func(string) bool, apply func(*domain.BankAccount)) (*domain.BankAccount, error) {
	if m.bnk01Err != nil {
		return nil, m.bnk01Err
	}
	acct, ok := m.bankAccounts[bankAccountID]
	if !ok {
		return nil, domain.ErrBankAccountNotFound
	}
	if !allowed(acct.AccountStatus) {
		return nil, domain.ErrInvalidTransition
	}
	apply(acct)
	return acct, nil
}

func (m *mockStore) AmendBankAccountMetadata(ctx context.Context, p domain.AmendBankAccountMetadataParams) (*domain.BankAccount, error) {
	return m.transition(p.BankAccountID, domain.CanAmendMetadata, func(a *domain.BankAccount) {
		a.AccountName, a.BranchRef, a.BankIdentifier, a.Country, a.AccountType = p.AccountName, p.BranchRef, p.BankIdentifier, p.Country, p.AccountType
	})
}

func (m *mockStore) ChangeOperationalUse(ctx context.Context, p domain.ChangeOperationalUseParams) (*domain.BankAccount, error) {
	return m.transition(p.BankAccountID, domain.CanChangeOperationalUse, func(a *domain.BankAccount) {
		a.RequestedOperationalUse = p.RequestedOperationalUse
	})
}

func (m *mockStore) SuspendBankAccount(ctx context.Context, p domain.SuspendAccountParams) (*domain.BankAccount, error) {
	return m.transition(p.BankAccountID, domain.CanSuspendAccount, func(a *domain.BankAccount) { a.AccountStatus = domain.BankAccountSuspended })
}

func (m *mockStore) ReactivateBankAccount(ctx context.Context, p domain.ReactivateAccountParams) (*domain.BankAccount, error) {
	return m.transition(p.BankAccountID, domain.CanReactivateAccount, func(a *domain.BankAccount) { a.AccountStatus = domain.BankAccountActive })
}

func (m *mockStore) CloseBankAccount(ctx context.Context, p domain.CloseAccountParams) (*domain.BankAccount, error) {
	return m.transition(p.BankAccountID, domain.CanCloseAccount, func(a *domain.BankAccount) { a.AccountStatus = domain.BankAccountClosed })
}

func (m *mockStore) RotateAccountIdentifierToken(ctx context.Context, p domain.RotateAccountTokenParams) (*domain.BankAccount, error) {
	return m.transition(p.BankAccountID, domain.CanRotateAccountToken, func(a *domain.BankAccount) {
		a.MaskedAccountNumber = p.NewMaskedAccountNumber
		if p.NewBankIdentifier != "" {
			a.BankIdentifier = p.NewBankIdentifier
		}
		a.TokenVersion++
	})
}

func (m *mockStore) GetBankAccount(ctx context.Context, bankAccountID string) (*domain.BankAccount, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.bankAccounts[bankAccountID], nil
}

func (m *mockStore) ListBankAccounts(ctx context.Context, legalEntityID string) ([]domain.BankAccount, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var out []domain.BankAccount
	for _, acct := range m.bankAccounts {
		if legalEntityID == "" || acct.LegalEntityID == legalEntityID {
			out = append(out, *acct)
		}
	}
	return out, nil
}

func (m *mockStore) UpdateBankAccountStatus(ctx context.Context, bankAccountID, status string) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	if acct, ok := m.bankAccounts[bankAccountID]; ok {
		acct.AccountStatus = status
		return nil
	}
	return domain.ErrBankAccountNotFound
}

func (m *mockStore) CreateCashBalance(ctx context.Context, bal *domain.CashBalance) error {
	if m.balErr != nil {
		return m.balErr
	}
	m.cashBalances[bal.BankAccountID] = bal
	return nil
}

func (m *mockStore) GetLatestCashBalance(ctx context.Context, bankAccountID string) (*domain.CashBalance, error) {
	if m.balErr != nil {
		return nil, m.balErr
	}
	return m.cashBalances[bankAccountID], nil
}

func (m *mockStore) SetLiquidityThreshold(ctx context.Context, threshold *domain.LiquidityThreshold) error {
	if m.threshErr != nil {
		return m.threshErr
	}
	key := threshold.LegalEntityID + ":" + threshold.CurrencyCode
	m.thresholds[key] = threshold
	return nil
}

func (m *mockStore) GetLiquidityThreshold(ctx context.Context, legalEntityID, currencyCode string) (*domain.LiquidityThreshold, error) {
	if m.threshErr != nil {
		return nil, m.threshErr
	}
	key := legalEntityID + ":" + currencyCode
	return m.thresholds[key], nil
}

// ── BNK-09 ───────────────────────────────────────────────────────────────────

func (m *mockStore) CreateTreasuryTransfer(ctx context.Context, p domain.CreateTreasuryTransferParams) (*domain.TreasuryTransfer, bool, error) {
	if m.transferErr != nil {
		return nil, false, m.transferErr
	}
	if p.CorrelationID != "" {
		if id, ok := m.transfersByCorr[p.CorrelationID]; ok {
			return m.transfers[id], false, nil
		}
	}
	id := "transfer-" + p.CorrelationID
	if id == "transfer-" {
		id = p.SourceBankAccountID + "->" + p.TargetBankAccountID
	}
	t := &domain.TreasuryTransfer{
		TransferID: id, TenantID: p.TenantID, SourceBankAccountID: p.SourceBankAccountID, TargetBankAccountID: p.TargetBankAccountID,
		Amount: p.Amount, CurrencyCode: p.CurrencyCode, CorrelationID: p.CorrelationID, IsCrossEntity: p.IsCrossEntity,
		Status: domain.TransferPendingApproval, MakerPrincipalID: p.MakerPrincipalID,
	}
	m.transfers[id] = t
	if p.CorrelationID != "" {
		m.transfersByCorr[p.CorrelationID] = id
	}
	return t, true, nil
}

func (m *mockStore) GetTreasuryTransfer(ctx context.Context, tenantID, transferID string) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[transferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	return t, nil
}

func (m *mockStore) ApproveTreasuryTransfer(ctx context.Context, p domain.ApproveTreasuryTransferParams) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[p.TransferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	if t.MakerPrincipalID == p.CheckerPrincipalID {
		return nil, domain.ErrTransferSelfApproval
	}
	if !domain.CanApproveTransfer(t.Status) {
		return nil, domain.ErrInvalidTransferTransition
	}
	t.Status = domain.TransferApproved
	t.CheckerPrincipalID = p.CheckerPrincipalID
	return t, nil
}

func (m *mockStore) RejectTreasuryTransfer(ctx context.Context, p domain.RejectTreasuryTransferParams) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[p.TransferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	if !domain.CanRejectTransfer(t.Status) {
		return nil, domain.ErrInvalidTransferTransition
	}
	t.Status = domain.TransferRejected
	t.CheckerPrincipalID = p.CheckerPrincipalID
	t.RejectReason = p.Reason
	return t, nil
}

func (m *mockStore) MarkTransferSubmitted(ctx context.Context, tenantID, transferID, paymentAttemptID string) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[transferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	t.Status = domain.TransferSubmitted
	t.PaymentAttemptID = paymentAttemptID
	return t, nil
}

func (m *mockStore) MarkTransferLedgerPosted(ctx context.Context, tenantID, transferID, sourceJournalID string) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[transferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	t.Status = domain.TransferLedgerPosted
	t.SourceJournalID = sourceJournalID
	return t, nil
}

func (m *mockStore) MarkTransferIntercompanyPaired(ctx context.Context, tenantID, transferID, intercompanyEntryID string) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[transferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	t.Status = domain.TransferIntercompanyPaired
	t.IntercompanyEntryID = intercompanyEntryID
	return t, nil
}

func (m *mockStore) MarkTransferCompleted(ctx context.Context, tenantID, transferID string) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[transferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	t.Status = domain.TransferCompleted
	return t, nil
}

// ── BNK-10 ───────────────────────────────────────────────────────────────────

func (m *mockStore) RecordFXRate(ctx context.Context, p domain.RecordFXRateParams) (*domain.FXRate, error) {
	if m.fxRates == nil {
		m.fxRates = map[string]*domain.FXRate{}
	}
	r := &domain.FXRate{
		RateID: "rate-" + p.CurrencyPair, TenantID: p.TenantID, CurrencyPair: p.CurrencyPair,
		Rate: p.Rate, EffectiveAt: p.EffectiveAt, RecordedByPrincipalID: p.RecordedByPrincipalID,
	}
	m.fxRates[p.CurrencyPair] = r
	return r, nil
}

func (m *mockStore) GetLatestFXRate(ctx context.Context, tenantID, currencyPair string) (*domain.FXRate, error) {
	r, ok := m.fxRates[currencyPair]
	if !ok {
		return nil, domain.ErrFXRateNotFound
	}
	return r, nil
}

type mockPublisher struct {
	cashPositions []domain.CashBalance
	effectiveCash []domain.EffectiveCashResponse
	breaches      []domain.EffectiveCashResponse
}

func (m *mockPublisher) PublishCashPositionUpdated(ctx context.Context, correlationID, legalEntityID, actorID string, balance domain.CashBalance) {
	m.cashPositions = append(m.cashPositions, balance)
}

func (m *mockPublisher) PublishEffectiveCashUpdated(ctx context.Context, correlationID, actorID string, resp domain.EffectiveCashResponse) {
	m.effectiveCash = append(m.effectiveCash, resp)
}

func (m *mockPublisher) PublishLiquidityThresholdBreached(ctx context.Context, correlationID, actorID string, resp domain.EffectiveCashResponse) {
	m.breaches = append(m.breaches, resp)
}

type mockAuthz struct {
	allowed bool
	err     error
}

func (m *mockAuthz) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	if m.err != nil {
		return m.err
	}
	if !m.allowed {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

type mockClients struct {
	apCommitments float64
	payroll       float64
	tax           float64
	inflows       float64
	inflowsData   []domain.ExpectedCashFlow
	outflowsData  []domain.ExpectedCashFlow
	apErr         error
	obErr         error
	arErr         error
	fcErr         error
}

func (m *mockClients) GetPendingAPCommitments(ctx context.Context, tenantID, legalEntityID, currencyCode string) (float64, error) {
	return m.apCommitments, m.apErr
}

func (m *mockClients) GetOutstandingObligations(ctx context.Context, tenantID, legalEntityID, currencyCode string) (float64, float64, error) {
	return m.payroll, m.tax, m.obErr
}

func (m *mockClients) GetForecastedInflows(ctx context.Context, tenantID, legalEntityID, currencyCode string) (float64, error) {
	return m.inflows, m.arErr
}

func (m *mockClients) GetLiquidityForecastData(ctx context.Context, tenantID, legalEntityID, currencyCode string) ([]domain.ExpectedCashFlow, []domain.ExpectedCashFlow, error) {
	if m.fcErr != nil {
		return nil, nil, m.fcErr
	}
	return m.inflowsData, m.outflowsData, nil
}

// mockTransferClients is a stub — none of the tests in this file exercise
// ExecuteTreasuryTransfer's outbound calls, only CreateTreasuryTransfer's
// HTTP-layer behavior (threshold checks, idempotency, validation).
type mockTransferClients struct{}

func (m *mockTransferClients) SubmitTreasuryPayment(ctx context.Context, tenantID, principalID, correlationID, legalEntityID, transferID, payerAccountRef, payeeRef string, amount float64, currency string) (string, error) {
	return "attempt-stub", nil
}

func (m *mockTransferClients) PostTreasuryTransferJournal(ctx context.Context, tenantID, principalID, correlationID, legalEntityID, fiscalPeriod, transferID string, amount float64) (string, error) {
	return "journal-stub", nil
}

func (m *mockTransferClients) PairTreasuryTransferIntercompany(ctx context.Context, tenantID, principalID, correlationID, sourceLegalEntityID, targetLegalEntityID, sourceJournalID string, amount float64, currencyCode string) (string, error) {
	return "intercompany-stub", nil
}

func TestHandler_RegisterBankAccount(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}
	c := &mockClients{}
	log := zap.NewNop()

	h := handler.New(s, p, az, c, &mockTransferClients{}, log)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	body := []byte(`{
		"legal_entity_id": "ent-123",
		"account_name": "Operating Checking",
		"masked_account_number": "****9876",
		"bank_identifier": "TESTBIC",
		"currency_code": "USD"
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/treasury/accounts", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var acct domain.BankAccount
	if err := json.NewDecoder(rr.Body).Decode(&acct); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if acct.AccountName != "Operating Checking" {
		t.Errorf("expected account name Operating Checking, got %s", acct.AccountName)
	}
}

func TestHandler_GetEffectiveCash(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}
	c := &mockClients{
		apCommitments: 200.0,
		payroll:       150.0,
		tax:           50.0,
	}
	log := zap.NewNop()

	// Register a mock bank account with balance 1000.00
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{
		BankAccountID: acctID,
		LegalEntityID: "ent-123",
		CurrencyCode:  "USD",
		AccountStatus: "ACTIVE",
	}
	s.cashBalances[acctID] = &domain.CashBalance{
		BankAccountID:    acctID,
		AvailableBalance: 1000.0,
	}

	h := handler.New(s, p, az, c, &mockTransferClients{}, log)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/effective-cash?legal_entity_id=ent-123&currency_code=USD", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp domain.EffectiveCashResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// 1000 (bankSum) - 200 (apSum) - 150 (payroll) - 50 (tax) = 600
	if resp.EffectiveAvailableCash != 600.0 {
		t.Errorf("expected effective available cash to be 600, got %f", resp.EffectiveAvailableCash)
	}

	// The fixture never sets CashBalance.AsOfTimestamp, so it's the zero
	// value — arbitrarily old, and correctly flagged stale rather than
	// silently presented as a current figure.
	if !resp.HasStaleComponent {
		t.Error("expected HasStaleComponent=true for a bank balance with no recorded as_of_timestamp")
	}
}

// TestHandler_GetEffectiveCash_FreshBankBalance_NotFlaggedStale is the
// negative control for TestHandler_GetEffectiveCash's staleness
// assertion above: a bank balance recorded just now must NOT be flagged.
func TestHandler_GetEffectiveCash_FreshBankBalance_NotFlaggedStale(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}
	c := &mockClients{apCommitments: 200.0, payroll: 150.0, tax: 50.0}
	log := zap.NewNop()

	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{BankAccountID: acctID, LegalEntityID: "ent-123", CurrencyCode: "USD", AccountStatus: "ACTIVE"}
	s.cashBalances[acctID] = &domain.CashBalance{BankAccountID: acctID, AvailableBalance: 1000.0, AsOfTimestamp: time.Now().UTC()}

	h := handler.New(s, p, az, c, &mockTransferClients{}, log)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/effective-cash?legal_entity_id=ent-123&currency_code=USD", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}
	var resp domain.EffectiveCashResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.HasStaleComponent {
		t.Error("expected HasStaleComponent=false for a freshly recorded bank balance")
	}
}

func TestHandler_CreateTreasuryTransfer_SuccessAndThreshold(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}
	c := &mockClients{}
	log := zap.NewNop()

	// Register mock bank accounts
	s.bankAccounts["src-1"] = &domain.BankAccount{
		BankAccountID: "src-1",
		LegalEntityID: "ent-123",
		CurrencyCode:  "USD",
		AccountStatus: "ACTIVE",
	}
	s.cashBalances["src-1"] = &domain.CashBalance{
		BankAccountID:    "src-1",
		AvailableBalance: 500.0,
	}

	s.bankAccounts["tgt-2"] = &domain.BankAccount{
		BankAccountID: "tgt-2",
		LegalEntityID: "ent-123",
		CurrencyCode:  "USD",
		AccountStatus: "ACTIVE",
	}
	s.cashBalances["tgt-2"] = &domain.CashBalance{
		BankAccountID:    "tgt-2",
		AvailableBalance: 100.0,
	}

	// Set liquidity threshold of 200 USD
	s.thresholds["ent-123:USD"] = &domain.LiquidityThreshold{
		LegalEntityID:          "ent-123",
		CurrencyCode:           "USD",
		MinimumRequiredBalance: 200.0,
	}

	h := handler.New(s, p, az, c, &mockTransferClients{}, log)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	// 1. Attempt transfer of 400 USD — leaves 100 which is below threshold of 200
	body1 := []byte(`{
		"source_bank_account_id": "src-1",
		"target_bank_account_id": "tgt-2",
		"amount": 400.0,
		"currency_code": "USD",
		"correlation_id": "corr-transfer-1"
	}`)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/treasury/transfers", bytes.NewReader(body1))
	req1.Header.Set("X-Tenant-Id", "tenant-abc")
	req1.Header.Set("X-Principal-Id", "usr-999")

	rr1 := httptest.NewRecorder()
	r.ServeHTTP(rr1, req1.WithContext(svcmiddleware.WithTenant(req1.Context(), "tenant-abc")))

	if rr1.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected status 412 Precondition Failed, got %d. Body: %s", rr1.Code, rr1.Body.String())
	}

	// 2. Attempt transfer of 100 USD — leaves 400 which is safe
	body2 := []byte(`{
		"source_bank_account_id": "src-1",
		"target_bank_account_id": "tgt-2",
		"amount": 100.0,
		"currency_code": "USD",
		"correlation_id": "corr-transfer-2"
	}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/treasury/transfers", bytes.NewReader(body2))
	req2.Header.Set("X-Tenant-Id", "tenant-abc")
	req2.Header.Set("X-Principal-Id", "usr-999")

	rr2 := httptest.NewRecorder()
	r.ServeHTTP(rr2, req2.WithContext(svcmiddleware.WithTenant(req2.Context(), "tenant-abc")))

	if rr2.Code != http.StatusCreated {
		t.Fatalf("expected status 201 Created (a new PENDING_APPROVAL transfer), got %d. Body: %s", rr2.Code, rr2.Body.String())
	}
	var created domain.TreasuryTransfer
	if err := json.Unmarshal(rr2.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Status != domain.TransferPendingApproval {
		t.Fatalf("expected a new transfer to be PENDING_APPROVAL, got %q", created.Status)
	}
}

func TestHandler_CreateTreasuryTransfer_MissingCorrelationID_Rejected(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}
	c := &mockClients{}
	log := zap.NewNop()

	s.bankAccounts["src-1"] = &domain.BankAccount{BankAccountID: "src-1", LegalEntityID: "ent-123", CurrencyCode: "USD", AccountStatus: "ACTIVE"}
	s.bankAccounts["tgt-2"] = &domain.BankAccount{BankAccountID: "tgt-2", LegalEntityID: "ent-123", CurrencyCode: "USD", AccountStatus: "ACTIVE"}
	s.cashBalances["src-1"] = &domain.CashBalance{BankAccountID: "src-1", AvailableBalance: 500.0}

	h := handler.New(s, p, az, c, &mockTransferClients{}, log)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	body := []byte(`{
		"source_bank_account_id": "src-1",
		"target_bank_account_id": "tgt-2",
		"amount": 100.0,
		"currency_code": "USD"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/treasury/transfers", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no correlation_id, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandler_CreateTreasuryTransfer_RetriedCorrelationID_DoesNotCreateASecondTransfer(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}
	c := &mockClients{}
	log := zap.NewNop()

	s.bankAccounts["src-1"] = &domain.BankAccount{BankAccountID: "src-1", LegalEntityID: "ent-123", CurrencyCode: "USD", AccountStatus: "ACTIVE"}
	s.bankAccounts["tgt-2"] = &domain.BankAccount{BankAccountID: "tgt-2", LegalEntityID: "ent-123", CurrencyCode: "USD", AccountStatus: "ACTIVE"}
	s.cashBalances["src-1"] = &domain.CashBalance{BankAccountID: "src-1", AvailableBalance: 500.0}
	s.cashBalances["tgt-2"] = &domain.CashBalance{BankAccountID: "tgt-2", AvailableBalance: 100.0}

	h := handler.New(s, p, az, c, &mockTransferClients{}, log)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	body := []byte(`{
		"source_bank_account_id": "src-1",
		"target_bank_account_id": "tgt-2",
		"amount": 100.0,
		"currency_code": "USD",
		"correlation_id": "corr-retry-1"
	}`)

	doTransfer := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/treasury/transfers", bytes.NewReader(body))
		req.Header.Set("X-Tenant-Id", "tenant-abc")
		req.Header.Set("X-Principal-Id", "usr-999")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
		return rr
	}

	first := doTransfer()
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}
	var firstTransfer domain.TreasuryTransfer
	if err := json.Unmarshal(first.Body.Bytes(), &firstTransfer); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	retry := doTransfer()
	if retry.Code != http.StatusOK {
		t.Fatalf("expected 200 (idempotent replay, not a new creation) on retried call, got %d: %s", retry.Code, retry.Body.String())
	}
	var retryTransfer domain.TreasuryTransfer
	if err := json.Unmarshal(retry.Body.Bytes(), &retryTransfer); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if retryTransfer.TransferID != firstTransfer.TransferID {
		t.Fatalf("expected the retried call to return the ORIGINAL transfer id %s, got %s — this is a duplicate-transfer bug if true", firstTransfer.TransferID, retryTransfer.TransferID)
	}
	if len(s.transfers) != 1 {
		t.Fatalf("expected exactly 1 transfer to exist after a retried create, got %d", len(s.transfers))
	}
}

func TestHandler_GetForecasts_Endpoint(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}

	now := time.Now().UTC()
	c := &mockClients{
		inflowsData: []domain.ExpectedCashFlow{
			{Amount: 500.0, DueDate: now.AddDate(0, 0, 5), Category: "RECEIVABLE"},   // in 7-day
			{Amount: 1000.0, DueDate: now.AddDate(0, 0, 20), Category: "RECEIVABLE"}, // in 30-day
			{Amount: 2000.0, DueDate: now.AddDate(0, 0, 60), Category: "RECEIVABLE"}, // in 90-day
		},
		outflowsData: []domain.ExpectedCashFlow{
			{Amount: 100.0, DueDate: now.AddDate(0, 0, 4), Category: "PAYABLE"},     // in 7-day
			{Amount: 300.0, DueDate: now.AddDate(0, 0, 15), Category: "PAYABLE"},    // in 30-day
			{Amount: 600.0, DueDate: now.AddDate(0, 0, 45), Category: "OBLIGATION"}, // in 90-day
		},
	}
	log := zap.NewNop()

	// Register a mock bank account with balance 100.00
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{
		BankAccountID: acctID,
		LegalEntityID: "ent-123",
		CurrencyCode:  "USD",
		AccountStatus: "ACTIVE",
	}
	s.cashBalances[acctID] = &domain.CashBalance{
		BankAccountID:    acctID,
		AvailableBalance: 100.0,
	}

	h := handler.New(s, p, az, c, &mockTransferClients{}, log)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/forecasts?legal_entity_id=ent-123&currency_code=USD", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp domain.LiquidityForecastResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// 7-day:
	// currentCash = 100
	// in = 500
	// out = 100
	// forecasted = 100 + 500 - 100 = 500
	if resp.Forecast7Day.ExpectedInflows != 500.0 {
		t.Errorf("expected 7-day inflow to be 500, got %f", resp.Forecast7Day.ExpectedInflows)
	}
	if resp.Forecast7Day.ForecastedBalance != 500.0 {
		t.Errorf("expected 7-day balance to be 500, got %f", resp.Forecast7Day.ForecastedBalance)
	}

	// 30-day:
	// currentCash = 100
	// in = 500 + 1000 = 1500
	// out = 100 + 300 = 400
	// forecasted = 100 + 1500 - 400 = 1200
	if resp.Forecast30Day.ExpectedInflows != 1500.0 {
		t.Errorf("expected 30-day inflow to be 1500, got %f", resp.Forecast30Day.ExpectedInflows)
	}
	if resp.Forecast30Day.ForecastedBalance != 1200.0 {
		t.Errorf("expected 30-day balance to be 1200, got %f", resp.Forecast30Day.ForecastedBalance)
	}

	// 90-day:
	// currentCash = 100
	// in = 500 + 1000 + 2000 = 3500
	// out = 100 + 300 + 600 = 1000
	// forecasted = 100 + 3500 - 1000 = 2600
	if resp.Forecast90Day.ExpectedInflows != 3500.0 {
		t.Errorf("expected 90-day inflow to be 3500, got %f", resp.Forecast90Day.ExpectedInflows)
	}
	if resp.Forecast90Day.ForecastedBalance != 2600.0 {
		t.Errorf("expected 90-day balance to be 2600, got %f", resp.Forecast90Day.ForecastedBalance)
	}
}

// ── BNK-10 FX Exposure ────────────────────────────────────────────────────────

func TestHandler_GetFXExposure_RequiresRecordedRate(t *testing.T) {
	s := newMockStore()
	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/fx/exposure?legal_entity_id=ent-123&exposure_currency=EUR&functional_currency=USD", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 with no FX rate recorded for EUR/USD, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandler_GetFXExposure_ConvertsAtRecordedRate(t *testing.T) {
	s := newMockStore()
	now := time.Now().UTC()
	c := &mockClients{
		inflowsData:  []domain.ExpectedCashFlow{{Amount: 1000.0, DueDate: now.AddDate(0, 0, 10), Category: "RECEIVABLE"}},
		outflowsData: []domain.ExpectedCashFlow{{Amount: 400.0, DueDate: now.AddDate(0, 0, 10), Category: "PAYABLE"}},
	}
	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, c, &mockTransferClients{}, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	// Record a rate: 1 EUR = 1.10 USD.
	body := []byte(`{"currency_pair":"EUR/USD","rate":1.10}`)
	recordReq := httptest.NewRequest(http.MethodPost, "/v1/treasury/fx/rates", bytes.NewReader(body))
	recordReq.Header.Set("X-Tenant-Id", "tenant-abc")
	recordReq.Header.Set("X-Principal-Id", "usr-999")
	recordRR := httptest.NewRecorder()
	r.ServeHTTP(recordRR, recordReq.WithContext(svcmiddleware.WithTenant(recordReq.Context(), "tenant-abc")))
	if recordRR.Code != http.StatusCreated {
		t.Fatalf("RecordFXRate: expected 201, got %d: %s", recordRR.Code, recordRR.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/fx/exposure?legal_entity_id=ent-123&exposure_currency=EUR&functional_currency=USD", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.FXExposureResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RateUsed != 1.10 {
		t.Errorf("expected rate_used=1.10, got %f", resp.RateUsed)
	}
	if resp.RateIsStale {
		t.Error("expected a just-recorded rate to not be flagged stale")
	}
	// net exposure = 1000 - 400 = 600 EUR -> 660 USD at 1.10.
	wantExposure, wantFunctional := 600.0, 660.0
	if resp.TotalExposureAmount != wantExposure {
		t.Errorf("expected total_exposure_currency_amount=%f, got %f", wantExposure, resp.TotalExposureAmount)
	}
	if resp.TotalFunctionalAmount != wantFunctional {
		t.Errorf("expected total_functional_currency_amount=%f, got %f", wantFunctional, resp.TotalFunctionalAmount)
	}
}

// TestHandler_RunFXScenario_UsesHypotheticalRate proves the scenario path
// computes under the CALLER-SUPPLIED rate, not the recorded one, while
// Current still reflects the real recorded rate for comparison.
func TestHandler_RunFXScenario_UsesHypotheticalRate(t *testing.T) {
	s := newMockStore()
	now := time.Now().UTC()
	c := &mockClients{
		inflowsData: []domain.ExpectedCashFlow{{Amount: 1000.0, DueDate: now.AddDate(0, 0, 10), Category: "RECEIVABLE"}},
	}
	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, c, &mockTransferClients{}, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	recordBody := []byte(`{"currency_pair":"EUR/USD","rate":1.10}`)
	recordReq := httptest.NewRequest(http.MethodPost, "/v1/treasury/fx/rates", bytes.NewReader(recordBody))
	recordReq.Header.Set("X-Tenant-Id", "tenant-abc")
	recordReq.Header.Set("X-Principal-Id", "usr-999")
	recordRR := httptest.NewRecorder()
	r.ServeHTTP(recordRR, recordReq.WithContext(svcmiddleware.WithTenant(recordReq.Context(), "tenant-abc")))
	if recordRR.Code != http.StatusCreated {
		t.Fatalf("RecordFXRate: expected 201, got %d: %s", recordRR.Code, recordRR.Body.String())
	}

	scenarioBody := []byte(`{"legal_entity_id":"ent-123","exposure_currency":"EUR","functional_currency":"USD","hypothetical_rate":1.20}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/treasury/fx/scenario", bytes.NewReader(scenarioBody))
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.FXScenarioResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Current.RateUsed != 1.10 {
		t.Errorf("expected Current.RateUsed=1.10 (the recorded rate), got %f", resp.Current.RateUsed)
	}
	if resp.Scenario.RateUsed != 1.20 {
		t.Errorf("expected Scenario.RateUsed=1.20 (the hypothetical rate), got %f", resp.Scenario.RateUsed)
	}
	// current = 1000*1.10 = 1100, scenario = 1000*1.20 = 1200, delta = 100.
	if resp.FunctionalAmountDelta != 100.0 {
		t.Errorf("expected functional_amount_delta=100, got %f", resp.FunctionalAmountDelta)
	}
}
