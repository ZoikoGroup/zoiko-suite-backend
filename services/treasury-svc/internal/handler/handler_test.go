package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func (m *mockStore) GetBankAccountAsOf(ctx context.Context, tenantID, bankAccountID string, asOf time.Time) (*domain.AccountHistoryEntry, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	a := m.bankAccounts[bankAccountID]
	if a == nil {
		return nil, nil
	}
	return &domain.AccountHistoryEntry{
		HistoryID: "hist-" + a.BankAccountID, BankAccountID: a.BankAccountID, TenantID: a.TenantID,
		AccountName: a.AccountName, MaskedAccountNumber: a.MaskedAccountNumber, BankIdentifier: a.BankIdentifier,
		AccountStatus: a.AccountStatus, BranchRef: a.BranchRef, Country: a.Country, AccountType: a.AccountType,
		RequestedOperationalUse: a.RequestedOperationalUse, TokenVersion: a.TokenVersion,
		ChangedByPrincipalID: a.CreatedByPrincipalID, EffectiveAt: asOf,
	}, nil
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
	initialStatus := domain.TransferPendingApproval
	if p.SaveAsDraft {
		initialStatus = domain.TransferDraft
	}
	t := &domain.TreasuryTransfer{
		TransferID: id, TenantID: p.TenantID, SourceBankAccountID: p.SourceBankAccountID, TargetBankAccountID: p.TargetBankAccountID,
		Amount: p.Amount, CurrencyCode: p.CurrencyCode, CorrelationID: p.CorrelationID, IsCrossEntity: p.IsCrossEntity,
		Status: initialStatus, MakerPrincipalID: p.MakerPrincipalID,
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

func (m *mockStore) AmendTreasuryTransfer(ctx context.Context, p domain.AmendTreasuryTransferParams) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[p.TransferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	if t.MakerPrincipalID != p.ActorPrincipalID {
		return nil, domain.ErrOnlyMakerMayModifyTransfer
	}
	if !domain.CanAmendTransfer(t.Status) {
		return nil, domain.ErrInvalidTransferTransition
	}
	t.SourceBankAccountID = p.SourceBankAccountID
	t.TargetBankAccountID = p.TargetBankAccountID
	t.Amount = p.Amount
	t.CurrencyCode = p.CurrencyCode
	return t, nil
}

func (m *mockStore) SubmitTransferForApproval(ctx context.Context, p domain.SubmitTransferForApprovalParams) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[p.TransferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	if t.MakerPrincipalID != p.ActorPrincipalID {
		return nil, domain.ErrOnlyMakerMayModifyTransfer
	}
	if !domain.CanSubmitTransferForApproval(t.Status) {
		return nil, domain.ErrInvalidTransferTransition
	}
	t.Status = domain.TransferPendingApproval
	return t, nil
}

func (m *mockStore) CancelBeforeSubmission(ctx context.Context, p domain.CancelBeforeSubmissionParams) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[p.TransferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	if t.MakerPrincipalID != p.ActorPrincipalID {
		return nil, domain.ErrOnlyMakerMayModifyTransfer
	}
	if !domain.CanCancelBeforeSubmission(t.Status) {
		return nil, domain.ErrInvalidTransferTransition
	}
	t.Status = domain.TransferCancelled
	t.CancelReason = p.Reason
	return t, nil
}

func (m *mockStore) MarkTransferReturned(ctx context.Context, p domain.MarkTransferReturnedParams) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[p.TransferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	if !domain.CanMarkTransferReturned(t.Status) {
		return nil, domain.ErrInvalidTransferTransition
	}
	t.Status = domain.TransferReturned
	t.ReturnReason = p.Reason
	return t, nil
}

func (m *mockStore) ResolveTreasuryTransfer(ctx context.Context, p domain.ResolveTreasuryTransferParams) (*domain.TreasuryTransfer, error) {
	t, ok := m.transfers[p.TransferID]
	if !ok {
		return nil, domain.ErrTransferNotFound
	}
	if !domain.CanResolveTransfer(t.Status) {
		return nil, domain.ErrInvalidTransferTransition
	}
	switch p.Resolution {
	case domain.ResolutionResubmit:
		t.Status = domain.TransferPendingApproval
		t.CheckerPrincipalID = ""
		t.PaymentAttemptID = ""
		t.SourceJournalID = ""
		t.IntercompanyEntryID = ""
	case domain.ResolutionCancel:
		t.Status = domain.TransferCancelled
	default:
		return nil, domain.ErrInvalidResolution
	}
	t.ResolutionNote = p.Note
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

	bankAccountCreated               []domain.BankAccount
	bankAccountOwnershipVerified     []domain.BankAccount
	bankAccountMetadataAmended       []domain.BankAccount
	bankAccountOperationalUseChanged []domain.BankAccount
	bankAccountSuspended             []domain.BankAccount
	bankAccountReactivated           []domain.BankAccount
	bankAccountClosed                []domain.BankAccount
	bankAccountTokenRotated          []domain.BankAccount

	treasuryTransferReturned  []domain.TreasuryTransfer
	treasuryTransferCancelled []domain.TreasuryTransfer
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

func (m *mockPublisher) PublishBankAccountCreated(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	m.bankAccountCreated = append(m.bankAccountCreated, acct)
}

func (m *mockPublisher) PublishBankAccountOwnershipVerified(ctx context.Context, correlationID, actorID string, acct domain.BankAccount, evidence domain.OwnershipEvidence) {
	m.bankAccountOwnershipVerified = append(m.bankAccountOwnershipVerified, acct)
}

func (m *mockPublisher) PublishBankAccountMetadataAmended(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	m.bankAccountMetadataAmended = append(m.bankAccountMetadataAmended, acct)
}

func (m *mockPublisher) PublishBankAccountOperationalUseChanged(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	m.bankAccountOperationalUseChanged = append(m.bankAccountOperationalUseChanged, acct)
}

func (m *mockPublisher) PublishBankAccountSuspended(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	m.bankAccountSuspended = append(m.bankAccountSuspended, acct)
}

func (m *mockPublisher) PublishBankAccountReactivated(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	m.bankAccountReactivated = append(m.bankAccountReactivated, acct)
}

func (m *mockPublisher) PublishBankAccountClosed(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	m.bankAccountClosed = append(m.bankAccountClosed, acct)
}

func (m *mockPublisher) PublishBankAccountTokenRotated(ctx context.Context, correlationID, actorID string, acct domain.BankAccount) {
	m.bankAccountTokenRotated = append(m.bankAccountTokenRotated, acct)
}

func (m *mockPublisher) PublishTreasuryTransferReturned(ctx context.Context, correlationID, actorID string, t domain.TreasuryTransfer) {
	m.treasuryTransferReturned = append(m.treasuryTransferReturned, t)
}

func (m *mockPublisher) PublishTreasuryTransferCancelled(ctx context.Context, correlationID, actorID string, t domain.TreasuryTransfer) {
	m.treasuryTransferCancelled = append(m.treasuryTransferCancelled, t)
}

type mockAuthz struct {
	allowed bool
	err     error
	// lastAction records the actionType of the most recent CheckAllowed
	// call, so a test can assert a route requested the specific
	// permission it's supposed to (e.g. the masked read using a distinct,
	// lower-privilege action from the full detail read).
	lastAction string
}

func (m *mockAuthz) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	m.lastAction = actionType
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

type mockBankingConnector struct {
	options []domain.ConnectionOption
	err     error
}

func (m *mockBankingConnector) ListConnectionOptions(ctx context.Context, tenantID, legalEntityID, bankAccountID, correlationID string) ([]domain.ConnectionOption, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.options, nil
}

func TestHandler_RegisterBankAccount(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}
	c := &mockClients{}
	log := zap.NewNop()

	h := handler.New(s, p, az, c, &mockTransferClients{}, nil, log)
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
	if len(p.bankAccountCreated) != 1 {
		t.Errorf("expected PublishBankAccountCreated to be called once, got %d", len(p.bankAccountCreated))
	}
}

// TestHandler_BankAccountLifecycle_PublishesEveryEvent walks every BNK-01
// command and asserts the matching domain event was published exactly
// once — previously none of these commands published anything at all.
func TestHandler_BankAccountLifecycle_PublishesEveryEvent(t *testing.T) {
	s := newMockStore()
	p := &mockPublisher{}
	az := &mockAuthz{allowed: true}
	c := &mockClients{}
	h := handler.New(s, p, az, c, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	acctID := "acct-lifecycle-1"
	s.bankAccounts[acctID] = &domain.BankAccount{
		BankAccountID: acctID, LegalEntityID: "ent-123", CurrencyCode: "USD",
		AccountStatus: domain.BankAccountActive, CreatedByPrincipalID: "usr-creator",
	}

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		req.Header.Set("X-Tenant-Id", "tenant-abc")
		req.Header.Set("X-Principal-Id", "usr-verifier")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
		return rr
	}

	if rr := do(http.MethodPost, "/v1/treasury/accounts/"+acctID+"/verify-ownership", `{"verification_method":"MICRO_DEPOSIT","evidence_ref":"ref-1"}`); rr.Code != http.StatusOK {
		t.Fatalf("verify-ownership: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := do(http.MethodPost, "/v1/treasury/accounts/"+acctID+"/amend-metadata", `{"account_name":"New Name"}`); rr.Code != http.StatusOK {
		t.Fatalf("amend-metadata: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := do(http.MethodPost, "/v1/treasury/accounts/"+acctID+"/change-operational-use", `{"requested_operational_use":"PAYROLL"}`); rr.Code != http.StatusOK {
		t.Fatalf("change-operational-use: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := do(http.MethodPost, "/v1/treasury/accounts/"+acctID+"/suspend", `{"reason":"under review"}`); rr.Code != http.StatusOK {
		t.Fatalf("suspend: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := do(http.MethodPost, "/v1/treasury/accounts/"+acctID+"/reactivate", `{}`); rr.Code != http.StatusOK {
		t.Fatalf("reactivate: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := do(http.MethodPost, "/v1/treasury/accounts/"+acctID+"/rotate-token", `{"new_masked_account_number":"****1111"}`); rr.Code != http.StatusOK {
		t.Fatalf("rotate-token: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := do(http.MethodPost, "/v1/treasury/accounts/"+acctID+"/close", `{"reason":"account closed"}`); rr.Code != http.StatusOK {
		t.Fatalf("close: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if len(p.bankAccountOwnershipVerified) != 1 {
		t.Errorf("expected PublishBankAccountOwnershipVerified once, got %d", len(p.bankAccountOwnershipVerified))
	}
	if len(p.bankAccountMetadataAmended) != 1 {
		t.Errorf("expected PublishBankAccountMetadataAmended once, got %d", len(p.bankAccountMetadataAmended))
	}
	if len(p.bankAccountOperationalUseChanged) != 1 {
		t.Errorf("expected PublishBankAccountOperationalUseChanged once, got %d", len(p.bankAccountOperationalUseChanged))
	}
	if len(p.bankAccountSuspended) != 1 {
		t.Errorf("expected PublishBankAccountSuspended once, got %d", len(p.bankAccountSuspended))
	}
	if len(p.bankAccountReactivated) != 1 {
		t.Errorf("expected PublishBankAccountReactivated once, got %d", len(p.bankAccountReactivated))
	}
	if len(p.bankAccountTokenRotated) != 1 {
		t.Errorf("expected PublishBankAccountTokenRotated once, got %d", len(p.bankAccountTokenRotated))
	}
	if len(p.bankAccountClosed) != 1 {
		t.Errorf("expected PublishBankAccountClosed once, got %d", len(p.bankAccountClosed))
	}
}

// TestHandler_VerifyBankAccountOwnership_CreatorCannotSelfVerify is the
// real proof of BNK-01's maker-checker rule: the principal who created
// the account cannot also verify its ownership.
func TestHandler_VerifyBankAccountOwnership_CreatorCannotSelfVerify(t *testing.T) {
	s := newMockStore()
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{
		BankAccountID: acctID, LegalEntityID: "ent-123", CurrencyCode: "USD",
		AccountStatus: domain.BankAccountPendingVerification, CreatedByPrincipalID: "usr-creator",
	}
	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	body := []byte(`{"verification_method":"MICRO_DEPOSIT","evidence_ref":"ref-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/treasury/accounts/"+acctID+"/verify-ownership", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-creator")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the creator tries to verify their own account, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHandler_VerifyBankAccountOwnership_DifferentPrincipal_Succeeds is
// the positive control.
func TestHandler_VerifyBankAccountOwnership_DifferentPrincipal_Succeeds(t *testing.T) {
	s := newMockStore()
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{
		BankAccountID: acctID, LegalEntityID: "ent-123", CurrencyCode: "USD",
		AccountStatus: domain.BankAccountPendingVerification, CreatedByPrincipalID: "usr-creator",
	}
	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	body := []byte(`{"verification_method":"MICRO_DEPOSIT","evidence_ref":"ref-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/treasury/accounts/"+acctID+"/verify-ownership", bytes.NewReader(body))
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-verifier")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
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

	h := handler.New(s, p, az, c, &mockTransferClients{}, nil, log)
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

	h := handler.New(s, p, az, c, &mockTransferClients{}, nil, log)
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

	h := handler.New(s, p, az, c, &mockTransferClients{}, nil, log)
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

	h := handler.New(s, p, az, c, &mockTransferClients{}, nil, log)
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

	h := handler.New(s, p, az, c, &mockTransferClients{}, nil, log)
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

// ── Wave 12: Amend/SubmitForApproval/Cancel/MarkReturned/Resolve ────────────

func doTransferJSONRequest(t *testing.T, r http.Handler, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", principalID)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	return rr
}

func newWave12Router(s *mockStore, p *mockPublisher) chi.Router {
	h := handler.New(s, p, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)
	return r
}

func TestHandler_AmendTreasuryTransfer_DraftOnly_OnlyMaker(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = &domain.TreasuryTransfer{TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferDraft, MakerPrincipalID: "maker-1", Amount: 100, CurrencyCode: "USD"}
	r := newWave12Router(s, &mockPublisher{})

	// A non-maker cannot amend.
	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/amend",
		domain.InitiateTransferRequest{SourceBankAccountID: "src", TargetBankAccountID: "tgt", Amount: 50, CurrencyCode: "USD"}, "not-the-maker")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 amending as a non-maker, got %d: %s", rr.Code, rr.Body.String())
	}

	// The maker can amend while DRAFT.
	rr = doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/amend",
		domain.InitiateTransferRequest{SourceBankAccountID: "src", TargetBankAccountID: "tgt", Amount: 50, CurrencyCode: "USD"}, "maker-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 amending as the maker, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.TreasuryTransfer
	_ = json.Unmarshal(rr.Body.Bytes(), &updated)
	if updated.Amount != 50 {
		t.Fatalf("expected the amended amount to persist, got %v", updated.Amount)
	}
}

func TestHandler_SubmitTransferForApproval_MovesDraftToPendingApproval(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = &domain.TreasuryTransfer{TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferDraft, MakerPrincipalID: "maker-1"}
	r := newWave12Router(s, &mockPublisher{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/submit-for-approval", nil, "maker-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.TreasuryTransfer
	_ = json.Unmarshal(rr.Body.Bytes(), &updated)
	if updated.Status != domain.TransferPendingApproval {
		t.Fatalf("expected PENDING_APPROVAL, got %s", updated.Status)
	}
}

func TestHandler_CancelBeforeSubmission_PublishesCancelledEvent(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = &domain.TreasuryTransfer{TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferPendingApproval, MakerPrincipalID: "maker-1"}
	p := &mockPublisher{}
	r := newWave12Router(s, p)

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/cancel", map[string]string{"reason": "no longer needed"}, "maker-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.TreasuryTransfer
	_ = json.Unmarshal(rr.Body.Bytes(), &updated)
	if updated.Status != domain.TransferCancelled {
		t.Fatalf("expected CANCELLED, got %s", updated.Status)
	}
	if len(p.treasuryTransferCancelled) != 1 {
		t.Fatalf("expected PublishTreasuryTransferCancelled to be called once, got %d", len(p.treasuryTransferCancelled))
	}
}

// TestHandler_CancelBeforeSubmission_AfterSubmission_Rejected is the
// negative control: once the bank has seen the transfer, cancellation is
// refused — CancelBeforeSubmission means before.
func TestHandler_CancelBeforeSubmission_AfterSubmission_Rejected(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = &domain.TreasuryTransfer{TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferSubmitted, MakerPrincipalID: "maker-1"}
	r := newWave12Router(s, &mockPublisher{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/cancel", map[string]string{"reason": "too late"}, "maker-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 cancelling a SUBMITTED transfer, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandler_MarkTransferReturned_PublishesReturnedEvent(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = &domain.TreasuryTransfer{TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferSubmitted, MakerPrincipalID: "maker-1"}
	p := &mockPublisher{}
	r := newWave12Router(s, p)

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/mark-returned", map[string]string{"reason": "destination account closed"}, "ops-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.TreasuryTransfer
	_ = json.Unmarshal(rr.Body.Bytes(), &updated)
	if updated.Status != domain.TransferReturned {
		t.Fatalf("expected RETURNED, got %s", updated.Status)
	}
	if len(p.treasuryTransferReturned) != 1 {
		t.Fatalf("expected PublishTreasuryTransferReturned to be called once, got %d", len(p.treasuryTransferReturned))
	}
}

func TestHandler_MarkTransferReturned_MissingReason_Returns400(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = &domain.TreasuryTransfer{TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferSubmitted, MakerPrincipalID: "maker-1"}
	r := newWave12Router(s, &mockPublisher{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/mark-returned", map[string]string{}, "ops-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no reason, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandler_ResolveTreasuryTransfer_Resubmit(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = &domain.TreasuryTransfer{
		TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferReturned, MakerPrincipalID: "maker-1",
		CheckerPrincipalID: "checker-1", PaymentAttemptID: "attempt-1",
	}
	r := newWave12Router(s, &mockPublisher{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/resolve",
		map[string]string{"resolution": "RESUBMIT", "note": "corrected, resubmitting"}, "ops-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.TreasuryTransfer
	_ = json.Unmarshal(rr.Body.Bytes(), &updated)
	if updated.Status != domain.TransferPendingApproval {
		t.Fatalf("expected PENDING_APPROVAL, got %s", updated.Status)
	}
	if updated.CheckerPrincipalID != "" || updated.PaymentAttemptID != "" {
		t.Fatalf("expected the prior checker/attempt to be cleared on resubmit, got %+v", updated)
	}
}

func TestHandler_ResolveTreasuryTransfer_Cancel_PublishesCancelledEvent(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = &domain.TreasuryTransfer{TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferReturned, MakerPrincipalID: "maker-1"}
	p := &mockPublisher{}
	r := newWave12Router(s, p)

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/resolve",
		map[string]string{"resolution": "CANCEL", "note": "abandoned"}, "ops-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.TreasuryTransfer
	_ = json.Unmarshal(rr.Body.Bytes(), &updated)
	if updated.Status != domain.TransferCancelled {
		t.Fatalf("expected CANCELLED, got %s", updated.Status)
	}
	if len(p.treasuryTransferCancelled) != 1 {
		t.Fatalf("expected PublishTreasuryTransferCancelled to be called once, got %d", len(p.treasuryTransferCancelled))
	}
}

func TestHandler_ResolveTreasuryTransfer_InvalidResolution_Returns400(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = &domain.TreasuryTransfer{TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferReturned, MakerPrincipalID: "maker-1"}
	r := newWave12Router(s, &mockPublisher{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/resolve",
		map[string]string{"resolution": "BOGUS", "note": "x"}, "ops-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid resolution, got %d: %s", rr.Code, rr.Body.String())
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

	h := handler.New(s, p, az, c, &mockTransferClients{}, nil, log)
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
	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
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
	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, c, &mockTransferClients{}, nil, zap.NewNop())
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
	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, c, &mockTransferClients{}, nil, zap.NewNop())
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

// ── BNK-01: GetBankAccountByID exposes is_ownership_verified ────────────────
//
// payment-initiation-adapter-svc's BNK-06 fix (Wave 7b) depends on this
// field to stop trusting a caller-supplied PayerAccountVerified flag —
// this proves it's actually on the wire, not just computed and dropped.

func TestHandler_GetBankAccountByID_IncludesOwnershipVerified(t *testing.T) {
	s := newMockStore()
	s.allOwnershipVerified = true
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{BankAccountID: acctID, LegalEntityID: "ent-123", CurrencyCode: "USD", AccountStatus: "ACTIVE"}

	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/accounts/"+acctID, nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		IsOwnershipVerified bool `json:"is_ownership_verified"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.IsOwnershipVerified {
		t.Error("expected is_ownership_verified=true")
	}
}

func TestHandler_GetBankAccountByID_UnverifiedAccount_ReturnsFalse(t *testing.T) {
	s := newMockStore()
	s.allOwnershipVerified = false
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{BankAccountID: acctID, LegalEntityID: "ent-123", CurrencyCode: "USD", AccountStatus: "ACTIVE"}

	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/accounts/"+acctID, nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		IsOwnershipVerified bool `json:"is_ownership_verified"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.IsOwnershipVerified {
		t.Error("expected is_ownership_verified=false for an account with no non-superseded evidence")
	}
}

// ── Wave 10: history, masked reads, available actions ───────────────────────

func TestHandler_GetBankAccountAsOf_ReturnsHistoricalEntry(t *testing.T) {
	s := newMockStore()
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{BankAccountID: acctID, TenantID: "tenant-abc", LegalEntityID: "ent-123", AccountName: "Corporate Checking", CurrencyCode: "USD", AccountStatus: "ACTIVE"}

	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/accounts/"+acctID+"/as-of?at=2026-01-01T00:00:00Z", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var entry domain.AccountHistoryEntry
	if err := json.NewDecoder(rr.Body).Decode(&entry); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if entry.AccountName != "Corporate Checking" {
		t.Fatalf("expected the account's name in the history entry, got %q", entry.AccountName)
	}
}

func TestHandler_GetBankAccountAsOf_InvalidTimestamp_Returns400(t *testing.T) {
	s := newMockStore()
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{BankAccountID: acctID, TenantID: "tenant-abc", LegalEntityID: "ent-123", AccountStatus: "ACTIVE"}

	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/accounts/"+acctID+"/as-of?at=not-a-timestamp", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed at param, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHandler_GetBankAccountMasked_UsesDistinctPermission proves the
// masked read is gated on its own lower-privilege action — not silently
// reusing the same permission as the full detail read (GetBankAccountByID).
func TestHandler_GetBankAccountMasked_UsesDistinctPermission(t *testing.T) {
	s := newMockStore()
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{
		BankAccountID: acctID, LegalEntityID: "ent-123", AccountName: "Corporate Checking",
		MaskedAccountNumber: "****1234", BankIdentifier: "SWIFT-TEST", CurrencyCode: "USD", AccountStatus: "ACTIVE",
	}
	az := &mockAuthz{allowed: true}

	h := handler.New(s, &mockPublisher{}, az, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/accounts/"+acctID+"/masked", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if az.lastAction != "BANK_ACCOUNT_VIEW_MASKED" {
		t.Fatalf("expected the masked route to check BANK_ACCOUNT_VIEW_MASKED, got %q", az.lastAction)
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, hasBankIdentifier := resp["bank_identifier"]; hasBankIdentifier {
		t.Error("expected the masked response to omit bank_identifier")
	}
	if resp["masked_account_number"] != "****1234" {
		t.Fatalf("expected masked_account_number to be present, got %+v", resp)
	}
}

func TestHandler_GetAvailableActions_ActiveAccount_ExcludesReactivate(t *testing.T) {
	s := newMockStore()
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{BankAccountID: acctID, LegalEntityID: "ent-123", AccountStatus: "ACTIVE"}

	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/accounts/"+acctID+"/available-actions", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Actions []string `json:"available_actions"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]bool{"amend-metadata": true, "change-operational-use": true, "suspend": true, "close": true, "rotate-token": true}
	for _, a := range resp.Actions {
		if a == "reactivate" {
			t.Fatal("expected an ACTIVE account to NOT report reactivate as available")
		}
		delete(want, a)
	}
	if len(want) != 0 {
		t.Fatalf("expected all of %v to be reported available for an ACTIVE account, missing some: got %v", want, resp.Actions)
	}
}

func TestHandler_GetAvailableActions_ClosedAccount_ReportsNone(t *testing.T) {
	s := newMockStore()
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{BankAccountID: acctID, LegalEntityID: "ent-123", AccountStatus: "CLOSED"}

	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/accounts/"+acctID+"/available-actions", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Actions []string `json:"available_actions"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Actions) != 0 {
		t.Fatalf("expected a CLOSED account to report zero available actions, got %v", resp.Actions)
	}
}

func TestHandler_ListConnectionOptions_Success(t *testing.T) {
	s := newMockStore()
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{BankAccountID: acctID, TenantID: "tenant-abc", LegalEntityID: "ent-123", AccountStatus: "ACTIVE"}
	banking := &mockBankingConnector{options: []domain.ConnectionOption{{ConnectionID: "conn-1", Status: "ACTIVE", ProviderRef: "plaid"}}}

	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, banking, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/accounts/"+acctID+"/connection-options", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var options []domain.ConnectionOption
	if err := json.NewDecoder(rr.Body).Decode(&options); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(options) != 1 || options[0].ConnectionID != "conn-1" {
		t.Fatalf("expected the one connection option to be returned, got %+v", options)
	}
}

// TestHandler_ListConnectionOptions_ConnectorUnavailable_FailsClosed proves
// a banking-connector-svc failure is reported as 503, never as an empty
// (and misleadingly reassuring) connection list.
func TestHandler_ListConnectionOptions_ConnectorUnavailable_FailsClosed(t *testing.T) {
	s := newMockStore()
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{BankAccountID: acctID, TenantID: "tenant-abc", LegalEntityID: "ent-123", AccountStatus: "ACTIVE"}
	banking := &mockBankingConnector{err: errors.New("connector down")}

	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, banking, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/accounts/"+acctID+"/connection-options", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when banking-connector-svc is unreachable, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHandler_ListConnectionOptions_NotConfigured_Returns503 proves a
// deployment that hasn't wired banking-connector-svc's URL fails closed
// rather than panicking on the nil client.
func TestHandler_ListConnectionOptions_NotConfigured_Returns503(t *testing.T) {
	s := newMockStore()
	acctID := "acct-1"
	s.bankAccounts[acctID] = &domain.BankAccount{BankAccountID: acctID, TenantID: "tenant-abc", LegalEntityID: "ent-123", AccountStatus: "ACTIVE"}

	h := handler.New(s, &mockPublisher{}, &mockAuthz{allowed: true}, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/treasury/accounts/"+acctID+"/connection-options", nil)
	req.Header.Set("X-Tenant-Id", "tenant-abc")
	req.Header.Set("X-Principal-Id", "usr-999")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-abc")))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when banking-connector integration is not configured, got %d: %s", rr.Code, rr.Body.String())
	}
}
