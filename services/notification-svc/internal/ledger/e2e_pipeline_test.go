package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

type memoryLedgerStore struct {
	intents      map[string]*ledger.MessageIntent
	renders      map[string]*ledger.MessageRender
	attempts     []*ledger.DeliveryAttempt
	events       []*ledger.DeliveryEvent
	suppressions map[string]*ledger.EmailSuppression
}

func newMemoryLedgerStore() *memoryLedgerStore {
	return &memoryLedgerStore{
		intents:      make(map[string]*ledger.MessageIntent),
		renders:      make(map[string]*ledger.MessageRender),
		suppressions: make(map[string]*ledger.EmailSuppression),
	}
}

func (m *memoryLedgerStore) CreateMessageIntent(_ context.Context, intent *ledger.MessageIntent) (bool, *ledger.MessageIntent, error) {
	for _, existing := range m.intents {
		if existing.TenantID == intent.TenantID && existing.DeduplicationKey == intent.DeduplicationKey {
			return false, existing, nil
		}
	}
	m.intents[intent.MessageIntentID] = intent
	return true, intent, nil
}

func (m *memoryLedgerStore) GetMessageIntent(_ context.Context, _, intentID string) (*ledger.MessageIntent, error) {
	it, ok := m.intents[intentID]
	if !ok {
		return nil, errors.New("intent not found")
	}
	return it, nil
}

func (m *memoryLedgerStore) UpdateIntentStatus(_ context.Context, _, intentID string, status ledger.IntentStatus, reason *string) error {
	it, ok := m.intents[intentID]
	if !ok {
		return errors.New("intent not found")
	}
	it.Status = status
	it.FailureReason = reason
	return nil
}

func (m *memoryLedgerStore) RecordMessageRender(_ context.Context, render *ledger.MessageRender) error {
	m.renders[render.RenderID] = render
	return nil
}

func (m *memoryLedgerStore) GetRenderByIntent(_ context.Context, _, intentID string) (*ledger.MessageRender, error) {
	for _, r := range m.renders {
		if r.MessageIntentID == intentID {
			return r, nil
		}
	}
	return nil, nil
}

func (m *memoryLedgerStore) RecordDeliveryAttempt(_ context.Context, attempt *ledger.DeliveryAttempt) error {
	m.attempts = append(m.attempts, attempt)
	return nil
}

func (m *memoryLedgerStore) RecordDeliveryEvent(_ context.Context, event *ledger.DeliveryEvent) error {
	m.events = append(m.events, event)
	return nil
}

func (m *memoryLedgerStore) AddSuppression(_ context.Context, supp *ledger.EmailSuppression) error {
	key := supp.TenantID + ":" + supp.RecipientEmail
	m.suppressions[key] = supp
	return nil
}

func (m *memoryLedgerStore) IsEmailSuppressed(_ context.Context, tenantID, recipientEmail string, stream ledger.SenderStream, commClass ledger.CommunicationClass) (bool, string, error) {
	key := tenantID + ":" + recipientEmail
	supp, ok := m.suppressions[key]
	if !ok {
		return false, "", nil
	}
	if commClass == ledger.ClassS0 || commClass == ledger.ClassT0 {
		if supp.Reason == ledger.SuppressionReasonUnsubscribe {
			return false, "", nil
		}
		return true, string(supp.Reason), nil
	}
	return true, string(supp.Reason), nil
}

type memoryDeliverer struct {
	delivered bool
	lastSent  *domain.Notification
	calls     int
}

func (d *memoryDeliverer) Deliver(_ context.Context, n domain.Notification) domain.DeliveryOutcome {
	d.calls++
	d.lastSent = &n
	return domain.DeliveryOutcome{
		Delivered:        d.delivered,
		ProviderResponse: "msg-mem-123",
		ProviderName:     "memory-smtp",
	}
}

type memoryResolver struct {
	email string
}

func (r *memoryResolver) ResolveEmail(_ context.Context, _, _, _ string) (string, error) {
	return r.email, nil
}

func TestE2EPipeline_Unit_FullLifecycle(t *testing.T) {
	store := newMemoryLedgerStore()
	compiler := ledger.NewCompiler()
	for _, seed := range ledger.DefaultSeedDefinitions() {
		require.NoError(t, compiler.Register(seed))
	}
	killSwitch := ledger.NewKillSwitchManager(zap.NewNop())
	deliverer := &memoryDeliverer{delivered: true}
	resolver := &memoryResolver{email: "user@example.com"}

	orchestrator := ledger.NewOrchestrator(store, compiler, killSwitch, deliverer, resolver, zap.NewNop())

	ctx := svcmiddleware.WithTenant(context.Background(), "tenant-1")

	// 1. Ingest valid event
	req := ledger.EventIngestRequest{
		EventID:              "evt-" + uuid.NewString(),
		EventType:            "identity.password_reset_requested",
		TemplateKey:          "ZS-IA-001",
		LegalEntityID:        "entity-1",
		RecipientPrincipalID: "usr-1",
		RecipientEmail:       "user@example.com",
		CorrelationID:        "corr-1",
		Variables: map[string]string{
			"recipient.first_name":           "Dave",
			"recipient.email_masked":         "d***@example.com",
			"links.action_url":               "https://auth.zoiko.com/reset?token=xyz",
			"security.link_expires_at_local": "15 minutes",
			"message.reference":              "REF-12345",
		},
	}

	res, err := orchestrator.IngestEvent(ctx, req, "principal-caller")
	require.NoError(t, err)
	assert.Equal(t, ledger.IntentStatusDispatched, res.Status)
	assert.False(t, res.IsReplay)
	assert.Equal(t, 1, deliverer.calls)

	// 2. Verify duplicate ingest is idempotent replay
	resDup, err := orchestrator.IngestEvent(ctx, req, "principal-caller")
	require.NoError(t, err)
	assert.Equal(t, ledger.IntentStatusDispatched, resDup.Status)
	assert.True(t, resDup.IsReplay)
	assert.Equal(t, 1, deliverer.calls, "Duplicate send must not dispatch transport")
}
