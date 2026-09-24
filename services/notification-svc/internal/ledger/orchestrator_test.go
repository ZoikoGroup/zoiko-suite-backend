package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

type mockLedgerStore struct {
	intents  map[string]*ledger.MessageIntent // key: tenantID + ":" + dedupKey
	renders  map[string]*ledger.MessageRender
	attempts []*ledger.DeliveryAttempt
	events   []*ledger.DeliveryEvent
}

func newMockLedgerStore() *mockLedgerStore {
	return &mockLedgerStore{
		intents: make(map[string]*ledger.MessageIntent),
		renders: make(map[string]*ledger.MessageRender),
	}
}

func (m *mockLedgerStore) CreateMessageIntent(ctx context.Context, intent *ledger.MessageIntent) (bool, *ledger.MessageIntent, error) {
	key := intent.TenantID + ":" + intent.DeduplicationKey
	if existing, found := m.intents[key]; found {
		return false, existing, nil
	}
	m.intents[key] = intent
	m.intents[intent.TenantID+":id:"+intent.MessageIntentID] = intent
	return true, intent, nil
}

func (m *mockLedgerStore) RecordMessageRender(ctx context.Context, render *ledger.MessageRender) error {
	m.renders[render.TenantID+":"+render.MessageIntentID] = render
	return nil
}

func (m *mockLedgerStore) RecordDeliveryAttempt(ctx context.Context, attempt *ledger.DeliveryAttempt) error {
	m.attempts = append(m.attempts, attempt)
	return nil
}

func (m *mockLedgerStore) RecordDeliveryEvent(ctx context.Context, event *ledger.DeliveryEvent) error {
	m.events = append(m.events, event)
	return nil
}

func (m *mockLedgerStore) UpdateIntentStatus(ctx context.Context, tenantID, intentID string, status ledger.IntentStatus, failureReason *string) error {
	if intent, ok := m.intents[tenantID+":id:"+intentID]; ok {
		intent.Status = status
		intent.FailureReason = failureReason
		return nil
	}
	return errors.New("intent not found")
}

func (m *mockLedgerStore) GetMessageIntent(ctx context.Context, tenantID, intentID string) (*ledger.MessageIntent, error) {
	if intent, ok := m.intents[tenantID+":id:"+intentID]; ok {
		return intent, nil
	}
	return nil, errors.New("intent not found")
}

func (m *mockLedgerStore) GetRenderByIntent(ctx context.Context, tenantID, intentID string) (*ledger.MessageRender, error) {
	if render, ok := m.renders[tenantID+":"+intentID]; ok {
		return render, nil
	}
	return nil, errors.New("render not found")
}

type mockDeliverer struct {
	outcome      domain.DeliveryOutcome
	deliverCalls int
	lastReceived domain.Notification
}

func (d *mockDeliverer) Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome {
	d.deliverCalls++
	d.lastReceived = n
	return d.outcome
}

type mockRecipientResolver struct {
	email string
	err   error
}

func (r *mockRecipientResolver) ResolveEmail(ctx context.Context, tenantID, callerPrincipalID, recipientPrincipalID string) (string, error) {
	return r.email, r.err
}

func setupTestOrchestrator(t *testing.T) (*ledger.Orchestrator, *mockLedgerStore, *mockDeliverer, *ledger.KillSwitchManager) {
	t.Helper()
	store := newMockLedgerStore()
	compiler := ledger.NewCompiler()
	for _, seed := range ledger.DefaultSeedDefinitions() {
		if err := compiler.Register(seed); err != nil {
			t.Fatalf("failed to register seed %s: %v", seed.TemplateKey, err)
		}
	}
	killSwitch := ledger.NewKillSwitchManager(zap.NewNop())
	deliverer := &mockDeliverer{
		outcome: domain.DeliveryOutcome{
			Delivered:        true,
			ProviderResponse: "250 2.0.0 OK",
			ProviderName:     "primary-smtp",
		},
	}
	resolver := &mockRecipientResolver{email: "resolved@example.com"}
	orc := ledger.NewOrchestrator(store, compiler, killSwitch, deliverer, resolver, zap.NewNop())
	return orc, store, deliverer, killSwitch
}

func TestOrchestrator_SuccessfulPipeline(t *testing.T) {
	orc, store, deliverer, _ := setupTestOrchestrator(t)
	tenantID := "tenant-100"
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	req := ledger.EventIngestRequest{
		EventID:              "evt-001",
		EventType:            "identity.password_reset_requested",
		RecipientPrincipalID: "usr-001",
		LegalEntityID:        "entity-001",
		TemplateKey:          "ZS-IA-001",
		CorrelationID:        "corr-001",
		Variables: map[string]string{
			"recipient.first_name":           "Alice",
			"recipient.email_masked":         "a***@example.com",
			"links.action_url":               "https://auth.zoiko.com/verify?token=xyz",
			"security.link_expires_at_local": "15 minutes",
			"message.reference":              "REF-12345",
		},
	}

	res, err := orc.IngestEvent(ctx, req, "caller-001")
	if err != nil {
		t.Fatalf("expected ingest success, got %v", err)
	}

	if res.IsReplay {
		t.Errorf("expected is_replay=false")
	}
	if res.Status != ledger.IntentStatusDispatched {
		t.Errorf("expected status DISPATCHED, got %s", res.Status)
	}
	if deliverer.deliverCalls != 1 {
		t.Errorf("expected deliverer called once, got %d", deliverer.deliverCalls)
	}
	if len(store.attempts) != 1 {
		t.Errorf("expected 1 attempt recorded, got %d", len(store.attempts))
	}
	if len(store.events) != 1 {
		t.Errorf("expected 1 delivery event recorded, got %d", len(store.events))
	}

	// Verify idempotency replay
	res2, err := orc.IngestEvent(ctx, req, "caller-001")
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if !res2.IsReplay {
		t.Errorf("expected is_replay=true on replay")
	}
	if deliverer.deliverCalls != 1 {
		t.Errorf("deliverer must not be called again on replay, call count: %d", deliverer.deliverCalls)
	}
}

func TestOrchestrator_MissingVariables_FailsClosed(t *testing.T) {
	orc, _, deliverer, _ := setupTestOrchestrator(t)
	tenantID := "tenant-100"
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	req := ledger.EventIngestRequest{
		EventID:              "evt-002",
		EventType:            "identity.password_reset_requested",
		RecipientPrincipalID: "usr-001",
		LegalEntityID:        "entity-001",
		TemplateKey:          "ZS-IA-001",
		CorrelationID:        "corr-002",
		Variables: map[string]string{
			// missing required variables!
			"recipient_name": "Alice Smith",
		},
	}

	_, err := orc.IngestEvent(ctx, req, "caller-001")
	if !errors.Is(err, ledger.ErrMissingVariables) {
		t.Fatalf("expected ErrMissingVariables, got: %v", err)
	}
	if deliverer.deliverCalls != 0 {
		t.Errorf("deliverer must not be called when variables are missing")
	}
}

func TestOrchestrator_KillSwitch_PreventsDelivery(t *testing.T) {
	orc, store, deliverer, killSwitch := setupTestOrchestrator(t)
	tenantID := "tenant-killed"
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	killSwitch.EngageTenant(tenantID, ledger.KillSwitchReasonSecurityIncident, "security-ops")

	req := ledger.EventIngestRequest{
		EventID:              "evt-003",
		EventType:            "identity.password_reset_requested",
		RecipientPrincipalID: "usr-001",
		LegalEntityID:        "entity-001",
		TemplateKey:          "ZS-IA-001",
		CorrelationID:        "corr-003",
		Variables: map[string]string{
			"recipient.first_name":           "Alice",
			"recipient.email_masked":         "a***@example.com",
			"links.action_url":               "https://auth.zoiko.com/verify?token=xyz",
			"security.link_expires_at_local": "15 minutes",
			"message.reference":              "REF-12345",
		},
	}

	res, err := orc.IngestEvent(ctx, req, "caller-001")
	if err != nil {
		t.Fatalf("kill-switch ingest should record killed intent, got error: %v", err)
	}

	if res.Status != ledger.IntentStatusKilled {
		t.Errorf("expected status KILLED, got %s", res.Status)
	}
	if deliverer.deliverCalls != 0 {
		t.Errorf("deliverer must NOT be called when kill-switch is active")
	}

	// Verify intent is recorded and auditable
	key := tenantID + ":id:" + res.MessageIntentID
	intent, ok := store.intents[key]
	if !ok || intent.Status != ledger.IntentStatusKilled {
		t.Errorf("intent must be audited with status KILLED: %+v", intent)
	}
}

func TestOrchestrator_MissingTenantContext_Fails(t *testing.T) {
	orc, _, _, _ := setupTestOrchestrator(t)
	ctx := context.Background() // Missing tenant

	req := ledger.EventIngestRequest{
		EventID:              "evt-004",
		EventType:            "identity.password_reset_requested",
		RecipientPrincipalID: "usr-001",
		LegalEntityID:        "entity-001",
		TemplateKey:          "ZS-IA-001",
		CorrelationID:        "corr-004",
	}

	_, err := orc.IngestEvent(ctx, req, "caller-001")
	if !errors.Is(err, ledger.ErrMissingTenantContext) {
		t.Fatalf("expected ErrMissingTenantContext, got: %v", err)
	}
}

func TestOrchestrator_UnknownTemplate_Fails(t *testing.T) {
	orc, _, _, _ := setupTestOrchestrator(t)
	ctx := svcmiddleware.WithTenant(context.Background(), "tenant-1")

	req := ledger.EventIngestRequest{
		EventID:              "evt-005",
		EventType:            "unknown.event",
		RecipientPrincipalID: "usr-001",
		LegalEntityID:        "entity-001",
		TemplateKey:          "ZS-NON-EXISTENT",
		CorrelationID:        "corr-005",
	}

	_, err := orc.IngestEvent(ctx, req, "caller-001")
	if !errors.Is(err, ledger.ErrTemplateNotFound) {
		t.Fatalf("expected ErrTemplateNotFound, got: %v", err)
	}
}

type mockPolicyResolver struct {
	allowed bool
	reason  string
	err     error
}

func (m *mockPolicyResolver) Evaluate(ctx context.Context, intent *ledger.MessageIntent, stream ledger.SenderStream) (ledger.PolicyDecision, error) {
	if m.err != nil {
		return ledger.PolicyDecision{}, m.err
	}
	return ledger.PolicyDecision{
		Allowed:  m.allowed,
		Reason:   m.reason,
		RuleName: "MOCK_POLICY",
	}, nil
}

func TestOrchestrator_PolicySuppression_HaltedBeforeRender(t *testing.T) {
	orc, store, deliverer, _ := setupTestOrchestrator(t)
	tenantID := "tenant-suppressed"
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	policyMock := &mockPolicyResolver{
		allowed: false,
		reason:  "email address is suppressed (HARD_BOUNCE)",
	}
	orc.WithPolicyResolver(policyMock)

	req := ledger.EventIngestRequest{
		EventID:              "evt-supp-001",
		EventType:            "identity.password_reset_requested",
		RecipientPrincipalID: "usr-001",
		LegalEntityID:        "entity-001",
		TemplateKey:          "ZS-IA-001",
		CorrelationID:        "corr-supp-001",
		Variables: map[string]string{
			"recipient.first_name":           "Alice",
			"recipient.email_masked":         "a***@example.com",
			"links.action_url":               "https://auth.zoiko.com/verify?token=xyz",
			"security.link_expires_at_local": "15 minutes",
			"message.reference":              "REF-12345",
		},
	}

	res, err := orc.IngestEvent(ctx, req, "caller-001")
	if err != nil {
		t.Fatalf("expected ingest to succeed with KILLED intent, got error: %v", err)
	}

	if res.Status != ledger.IntentStatusKilled {
		t.Errorf("expected status KILLED, got %s", res.Status)
	}
	if deliverer.deliverCalls != 0 {
		t.Errorf("deliverer must NOT be called when email is suppressed, got %d calls", deliverer.deliverCalls)
	}
	if len(store.renders) != 0 {
		t.Errorf("no render should be recorded when suppressed, got %d", len(store.renders))
	}

	// Verify intent record in store has status KILLED and failure reason
	key := tenantID + ":id:" + res.MessageIntentID
	intent, ok := store.intents[key]
	if !ok {
		t.Fatalf("intent was not saved in ledger store")
	}
	if intent.Status != ledger.IntentStatusKilled {
		t.Errorf("expected intent in store to have status KILLED, got %s", intent.Status)
	}
	if intent.FailureReason == nil || *intent.FailureReason != "policy_suppressed: email address is suppressed (HARD_BOUNCE)" {
		t.Errorf("unexpected failure reason: %v", intent.FailureReason)
	}
}

func TestOrchestrator_PolicyEvaluationError_Fails(t *testing.T) {
	orc, store, deliverer, _ := setupTestOrchestrator(t)
	tenantID := "tenant-policy-err"
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	policyMock := &mockPolicyResolver{
		err: errors.New("db connection failure"),
	}
	orc.WithPolicyResolver(policyMock)

	req := ledger.EventIngestRequest{
		EventID:              "evt-policy-err-001",
		EventType:            "identity.password_reset_requested",
		RecipientPrincipalID: "usr-001",
		LegalEntityID:        "entity-001",
		TemplateKey:          "ZS-IA-001",
		CorrelationID:        "corr-001",
		Variables: map[string]string{
			"recipient.first_name":           "Alice",
			"recipient.email_masked":         "a***@example.com",
			"links.action_url":               "https://auth.zoiko.com/verify?token=xyz",
			"security.link_expires_at_local": "15 minutes",
			"message.reference":              "REF-12345",
		},
	}

	_, err := orc.IngestEvent(ctx, req, "caller-001")
	if err == nil {
		t.Fatalf("expected error from policy failure, got nil")
	}
	if deliverer.deliverCalls != 0 {
		t.Errorf("deliverer must not be called on policy error")
	}

	// Stored intent should be FAILED
	dedupKey := ledger.ComputeDeduplicationKey(tenantID, req.EventType, req.EventID)
	key := tenantID + ":" + dedupKey
	intent, ok := store.intents[key]
	if !ok {
		t.Fatalf("intent was not saved in store")
	}
	if intent.Status != ledger.IntentStatusFailed {
		t.Errorf("expected intent status FAILED, got %s", intent.Status)
	}
}

func TestOrchestrator_SenderStreamAlignment_AndRFC8058(t *testing.T) {
	orc, store, deliverer, _ := setupTestOrchestrator(t)
	tenantID := "tenant-streams-100"
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	// 1. Critical Stream (ZS-IA-004)
	reqCrit := ledger.EventIngestRequest{
		EventID:              "evt-crit-001",
		EventType:            "identity.sign_in_link_requested",
		RecipientPrincipalID: "usr-001",
		LegalEntityID:        "entity-001",
		TemplateKey:          "ZS-IA-004",
		CorrelationID:        "corr-crit",
		Variables: map[string]string{
			"recipient.first_name":   "Bob",
			"security.expiry_minutes": "10",
			"links.action_url":       "https://auth.zoiko.com/sign-in?token=abc",
			"message.reference":      "REF-CRIT-1",
		},
	}

	resCrit, err := orc.IngestEvent(ctx, reqCrit, "caller-001")
	if err != nil {
		t.Fatalf("ingest critical failed: %v", err)
	}
	if resCrit.Status != ledger.IntentStatusDispatched {
		t.Errorf("expected critical status DISPATCHED, got %s", resCrit.Status)
	}
	if deliverer.lastReceived.From != "ZoikoSuite Security <security@security.zoikosuite.com>" {
		t.Errorf("expected critical From header, got %s", deliverer.lastReceived.From)
	}
	if len(deliverer.lastReceived.Headers) != 0 {
		t.Errorf("critical emails should not have marketing unsubscribe headers")
	}

	critAttempt := store.attempts[len(store.attempts)-1]
	if critAttempt.FromAddress != "security@security.zoikosuite.com" {
		t.Errorf("expected attempt FromAddress security@security.zoikosuite.com, got %s", critAttempt.FromAddress)
	}
	if critAttempt.ProviderName != "primary-smtp" {
		t.Errorf("expected attempt ProviderName primary-smtp, got %s", critAttempt.ProviderName)
	}

	// 2. Marketing Stream with RFC 8058 headers
	// Register a marketing template
	compiler := ledger.NewCompiler()
	for _, seed := range ledger.DefaultSeedDefinitions() {
		_ = compiler.Register(seed)
	}
	marketingSeed := ledger.TemplateDefinition{
		TemplateKey:        "ZS-MKT-001",
		Version:            "1.0.0",
		Locale:             "en-US",
		CommunicationClass: ledger.ClassM1,
		SenderStream:       ledger.StreamMarketing,
		SubjectTemplate:    "Product Updates from ZoikoSuite",
		HTMLTemplate:       "<p>Hi {{index . \"recipient.first_name\"}}, check out our news!</p>",
		TextTemplate:       "Hi {{index . \"recipient.first_name\"}}, check out our news!",
		RequiredVariables:  []string{"recipient.first_name"},
	}
	if err := compiler.Register(marketingSeed); err != nil {
		t.Fatalf("failed to register marketing template: %v", err)
	}

	orcMarketing := ledger.NewOrchestrator(store, compiler, ledger.NewKillSwitchManager(zap.NewNop()), deliverer, &mockRecipientResolver{email: "mkt@example.com"}, zap.NewNop())

	reqMkt := ledger.EventIngestRequest{
		EventID:              "evt-mkt-001",
		EventType:            "marketing.newsletter_sent",
		RecipientPrincipalID: "usr-002",
		LegalEntityID:        "entity-001",
		TemplateKey:          "ZS-MKT-001",
		CorrelationID:        "corr-mkt",
		Variables: map[string]string{
			"recipient.first_name": "Charlie",
		},
	}

	resMkt, err := orcMarketing.IngestEvent(ctx, reqMkt, "caller-001")
	if err != nil {
		t.Fatalf("ingest marketing failed: %v", err)
	}
	if resMkt.Status != ledger.IntentStatusDispatched {
		t.Errorf("expected marketing status DISPATCHED, got %s", resMkt.Status)
	}
	if deliverer.lastReceived.From != "ZoikoSuite <hello@news.zoikosuite.com>" {
		t.Errorf("expected marketing From header, got %s", deliverer.lastReceived.From)
	}
	// Verify RFC 8058 headers
	unsubHeader, hasUnsub := deliverer.lastReceived.Headers["List-Unsubscribe"]
	if !hasUnsub || !strings.Contains(unsubHeader, "tenant_id="+tenantID) || !strings.Contains(unsubHeader, "mailto:unsubscribe@news.zoikosuite.com") {
		t.Errorf("missing or invalid List-Unsubscribe header: %s", unsubHeader)
	}
	unsubPost, hasPost := deliverer.lastReceived.Headers["List-Unsubscribe-Post"]
	if !hasPost || unsubPost != "List-Unsubscribe=One-Click" {
		t.Errorf("missing or invalid List-Unsubscribe-Post header: %s", unsubPost)
	}

	mktAttempt := store.attempts[len(store.attempts)-1]
	if mktAttempt.FromAddress != "hello@news.zoikosuite.com" {
		t.Errorf("expected attempt FromAddress hello@news.zoikosuite.com, got %s", mktAttempt.FromAddress)
	}
	if mktAttempt.ProviderName != "primary-smtp" {
		t.Errorf("expected attempt ProviderName primary-smtp, got %s", mktAttempt.ProviderName)
	}

	// 3. Billing transactional stream
	billingSeed := ledger.TemplateDefinition{
		TemplateKey:        "ZS-BE-001",
		Version:            "1.0.0",
		Locale:             "en-US",
		CommunicationClass: ledger.ClassT0,
		SenderStream:       ledger.StreamTransactional,
		SubjectTemplate:    "Invoice #12345",
		HTMLTemplate:       "<p>Hi {{index . \"recipient.first_name\"}}, invoice attached.</p>",
		TextTemplate:       "Hi {{index . \"recipient.first_name\"}}, invoice attached.",
		RequiredVariables:  []string{"recipient.first_name"},
	}
	if err := compiler.Register(billingSeed); err != nil {
		t.Fatalf("failed to register billing template: %v", err)
	}

	reqBilling := ledger.EventIngestRequest{
		EventID:              "evt-bill-001",
		EventType:            "billing.invoice_created",
		RecipientPrincipalID: "usr-003",
		LegalEntityID:        "entity-001",
		TemplateKey:          "ZS-BE-001",
		CorrelationID:        "corr-bill",
		Variables: map[string]string{
			"recipient.first_name": "Diana",
		},
	}

	resBill, err := orcMarketing.IngestEvent(ctx, reqBilling, "caller-001")
	if err != nil {
		t.Fatalf("ingest billing failed: %v", err)
	}
	if resBill.Status != ledger.IntentStatusDispatched {
		t.Errorf("expected billing status DISPATCHED, got %s", resBill.Status)
	}
	if deliverer.lastReceived.From != "ZoikoSuite Billing <billing@billing.zoikosuite.com>" {
		t.Errorf("expected billing From header, got %s", deliverer.lastReceived.From)
	}
	billAttempt := store.attempts[len(store.attempts)-1]
	if billAttempt.FromAddress != "billing@billing.zoikosuite.com" {
		t.Errorf("expected billing attempt FromAddress billing@billing.zoikosuite.com, got %s", billAttempt.FromAddress)
	}
}
