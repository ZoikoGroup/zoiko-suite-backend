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
	bankAccounts  map[string]*domain.BankAccount
	cashBalances  map[string]*domain.CashBalance
	thresholds    map[string]*domain.LiquidityThreshold
	transfers     map[string]bool
	createErr     error
	getErr        error
	listErr       error
	updateErr     error
	balErr        error
	threshErr     error
	transferErr   error
}

func newMockStore() *mockStore {
	return &mockStore{
		bankAccounts: make(map[string]*domain.BankAccount),
		cashBalances: make(map[string]*domain.CashBalance),
		thresholds:   make(map[string]*domain.LiquidityThreshold),
		transfers:    make(map[string]bool),
	}
}

func (m *mockStore) CreateBankAccount(ctx context.Context, acct *domain.BankAccount) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.bankAccounts[acct.BankAccountID] = acct
	return nil
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

func (m *mockStore) ExecuteTransfer(ctx context.Context, srcAcctID, tgtAcctID string, amount float64, currencyCode string, correlationID string) (bool, error) {
	if m.transferErr != nil {
		return false, m.transferErr
	}
	if correlationID != "" && m.transfers[correlationID] {
		return false, nil
	}
	if correlationID != "" {
		m.transfers[correlationID] = true
	}
	return true, nil
}

type mockPublisher struct {
	cashPositions []domain.CashBalance
	effectiveCash []domain.EffectiveCashResponse
	breaches      []domain.EffectiveCashResponse
}

func (m *mockPublisher) PublishCashPositionUpdated(ctx context.Context, correlationID string, balance domain.CashBalance) {
	m.cashPositions = append(m.cashPositions, balance)
}

func (m *mockPublisher) PublishEffectiveCashUpdated(ctx context.Context, correlationID string, resp domain.EffectiveCashResponse) {
	m.effectiveCash = append(m.effectiveCash, resp)
}

func (m *mockPublisher) PublishLiquidityThresholdBreached(ctx context.Context, correlationID string, resp domain.EffectiveCashResponse) {
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

func TestHandler_RegisterBankAccount(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}
	c := &mockClients{}
	log := zap.NewNop()

	h := handler.New(s, p, az, c, log)
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

	h := handler.New(s, p, az, c, log)
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
}

func TestHandler_InitiateTransfer_SuccessAndThreshold(t *testing.T) {
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

	h := handler.New(s, p, az, c, log)
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

	if rr2.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d. Body: %s", rr2.Code, rr2.Body.String())
	}
}

func TestHandler_InitiateTransfer_MissingCorrelationID_Rejected(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}
	c := &mockClients{}
	log := zap.NewNop()

	s.bankAccounts["src-1"] = &domain.BankAccount{BankAccountID: "src-1", LegalEntityID: "ent-123", CurrencyCode: "USD", AccountStatus: "ACTIVE"}
	s.bankAccounts["tgt-2"] = &domain.BankAccount{BankAccountID: "tgt-2", LegalEntityID: "ent-123", CurrencyCode: "USD", AccountStatus: "ACTIVE"}
	s.cashBalances["src-1"] = &domain.CashBalance{BankAccountID: "src-1", AvailableBalance: 500.0}

	h := handler.New(s, p, az, c, log)
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

func TestHandler_InitiateTransfer_RetriedCorrelationID_DoesNotDoubleMoveMoney(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}
	c := &mockClients{}
	log := zap.NewNop()

	s.bankAccounts["src-1"] = &domain.BankAccount{BankAccountID: "src-1", LegalEntityID: "ent-123", CurrencyCode: "USD", AccountStatus: "ACTIVE"}
	s.bankAccounts["tgt-2"] = &domain.BankAccount{BankAccountID: "tgt-2", LegalEntityID: "ent-123", CurrencyCode: "USD", AccountStatus: "ACTIVE"}
	s.cashBalances["src-1"] = &domain.CashBalance{BankAccountID: "src-1", AvailableBalance: 500.0}
	s.cashBalances["tgt-2"] = &domain.CashBalance{BankAccountID: "tgt-2", AvailableBalance: 100.0}

	h := handler.New(s, p, az, c, log)
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
	if first.Code != http.StatusOK {
		t.Fatalf("expected 200 on first call, got %d: %s", first.Code, first.Body.String())
	}

	retry := doTransfer()
	if retry.Code != http.StatusOK {
		t.Fatalf("expected 200 on retried call, got %d: %s", retry.Code, retry.Body.String())
	}
	if len(p.cashPositions) != 2 {
		t.Fatalf("expected exactly 2 PublishCashPositionUpdated calls (one transfer, two legs), got %d — a retry must not move money again", len(p.cashPositions))
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
			{Amount: 100.0, DueDate: now.AddDate(0, 0, 4), Category: "PAYABLE"},   // in 7-day
			{Amount: 300.0, DueDate: now.AddDate(0, 0, 15), Category: "PAYABLE"},   // in 30-day
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

	h := handler.New(s, p, az, c, log)
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
