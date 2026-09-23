package ledger_test

import (
	"context"
	"errors"
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
}

func (d *mockDeliverer) Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome {
	d.deliverCalls++
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
