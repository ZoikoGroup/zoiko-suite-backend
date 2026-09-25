package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/policy"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/webhook"
)

type e2eDeliverer struct {
	delivered    bool
	response     string
	lastSent     *domain.Notification
	sendCalls    int
}

func (d *e2eDeliverer) Deliver(_ context.Context, n domain.Notification) domain.DeliveryOutcome {
	d.sendCalls++
	d.lastSent = &n
	if !d.delivered {
		return domain.DeliveryOutcome{
			Delivered:        false,
			Reason:           "simulated deliverer failure",
			ProviderName:     "mock-smtp",
			Retryable:        true,
		}
	}
	return domain.DeliveryOutcome{
		Delivered:        true,
		ProviderResponse: d.response,
		ProviderName:     "mock-smtp",
	}
}

type e2eResolver struct {
	email string
}

func (r *e2eResolver) ResolveEmail(_ context.Context, _, _, _ string) (string, error) {
	return r.email, nil
}

// TestE2E_DeliveryPipeline_CompleteFlow tests the full 10-step lifecycle against PostgreSQL:
// Ingestion -> Policy Evaluation -> Render -> Dispatch -> Webhook Intake -> Ledger Outcome & Suppression.
func TestE2E_DeliveryPipeline_CompleteFlow(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()
	log := zap.NewNop()

	tenantID := "tenant-" + uuid.NewString()

	// 1. Setup Compiler with seed template definitions
	compiler := ledger.NewCompiler()
	seeds := ledger.DefaultSeedDefinitions()
	for _, seed := range seeds {
		require.NoError(t, compiler.Register(seed))
	}
	// Register marketing template
	marketingDef := ledger.TemplateDefinition{
		TemplateKey:        "ZS-MKT-001",
		Version:            "1.0.0",
		Locale:             "en-US",
		CommunicationClass: ledger.ClassM1,
		SenderStream:       ledger.StreamMarketing,
		SubjectTemplate:    "Zoiko Updates",
		HTMLTemplate:       "<p>Hi {{index . \"recipient.first_name\"}}</p>",
		TextTemplate:       "Hi {{index . \"recipient.first_name\"}}",
		RequiredVariables:  []string{"recipient.first_name"},
	}
	require.NoError(t, compiler.Register(marketingDef))

	// 2. Setup Precedence Policy & KillSwitch
	policyEngine := policy.NewPrecedenceEngine(s, log)
	killSwitch := ledger.NewKillSwitchManager(log)

	// 3. Setup Deliverer & Resolver
	deliverer := &e2eDeliverer{delivered: true, response: "provider-msg-e2e-001"}
	resolver := &e2eResolver{email: "alice@example.com"}

	// 4. Setup Orchestrator
	orchestrator := ledger.NewOrchestrator(s, compiler, killSwitch, deliverer, resolver, log).
		WithPolicyResolver(policyEngine)

	// ── SCENARIO 1: Successful End-to-End Delivery Flow ─────────────────────────
	ctx = svcmiddleware.WithTenant(context.Background(), tenantID)

	reqSuccess := ledger.EventIngestRequest{
		EventID:              "evt-" + uuid.NewString(),
		EventType:            "identity.verification_requested",
		TemplateKey:          "ZS-IA-001",
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "usr-001",
		RecipientEmail:       "alice@example.com",
		CorrelationID:        "corr-e2e-1",
		Variables: map[string]string{
			"recipient.first_name":           "Alice",
			"recipient.email_masked":         "a***@example.com",
			"links.action_url":               "https://auth.zoiko.com/verify?token=xyz",
			"security.link_expires_at_local": "15 minutes",
			"message.reference":              "REF-12345",
		},
	}

	res, err := orchestrator.IngestEvent(ctx, reqSuccess, "principal-caller-1")
	require.NoError(t, err)
	require.Equal(t, ledger.IntentStatusDispatched, res.Status)
	assert.False(t, res.IsReplay)
	assert.NotNil(t, res.RenderID)
	assert.NotNil(t, res.AttemptID)
	assert.Equal(t, 1, deliverer.sendCalls)

	// Verify ledger state in PostgreSQL
	intentRow, err := s.GetMessageIntent(ctx, tenantID, res.MessageIntentID)
	require.NoError(t, err)
	assert.Equal(t, ledger.IntentStatusDispatched, intentRow.Status)
	assert.Equal(t, ledger.ClassT0, intentRow.CommunicationClass)

	// Verify Render record
	renderRow, err := s.GetRenderByIntent(ctx, tenantID, res.MessageIntentID)
	require.NoError(t, err)
	assert.Equal(t, "ZS-IA-001", renderRow.TemplateKey)
	assert.Contains(t, renderRow.BodyHTML, "Alice")
	assert.NotEmpty(t, renderRow.ContentHash)

	// Verify Delivery Attempt record via LookupAttemptByProviderMessageID
	lookup, err := s.LookupAttemptByProviderMessageID(ctx, "provider-msg-e2e-001")
	require.NoError(t, err)
	assert.Equal(t, res.MessageIntentID, lookup.MessageIntentID)
	assert.Equal(t, tenantID, lookup.TenantID)
	assert.Equal(t, ledger.StreamTransactional, lookup.SenderStream)
	assert.Equal(t, "alice@example.com", lookup.RecipientAddress)

	// ── SCENARIO 2: Webhook Delivery Outcome Ingestion ─────────────────────────
	webhookProcessor := webhook.NewProcessor(s, log)

	webhookDeliveredPayload := []byte(`[
		{
			"event": "delivered",
			"email": "alice@example.com",
			"sg_message_id": "provider-msg-e2e-001",
			"timestamp": 1700000000
		}
	]`)

	err = webhookProcessor.ProcessRawPayload(ctx, "sendgrid", webhookDeliveredPayload)
	require.NoError(t, err)

	// Verify delivery event recorded in ledger table
	var deliveredEventsCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM delivery_events WHERE tenant_id = $1 AND event_type = 'DELIVERED'", tenantID).Scan(&deliveredEventsCount)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deliveredEventsCount, 1, "Ledger must contain DELIVERED delivery event from webhook")

	// ── SCENARIO 3: Suppression & Precedence Enforcement ───────────────────────
	// Add an unsubscribe suppression for Bob
	err = s.AddSuppression(ctx, &ledger.EmailSuppression{
		SuppressionID:  uuid.NewString(),
		TenantID:       tenantID,
		RecipientEmail: "bob@example.com",
		Reason:         ledger.SuppressionReasonUnsubscribe,
		SourceStream:   "ALL",
		CreatedAt:      time.Now().UTC(),
	})
	require.NoError(t, err)

	// Bob attempts to receive marketing message
	reqMarketing := ledger.EventIngestRequest{
		EventID:              "evt-" + uuid.NewString(),
		EventType:            "marketing.newsletter",
		TemplateKey:          "ZS-MKT-001",
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "usr-002",
		RecipientEmail:       "bob@example.com",
		CorrelationID:        "corr-e2e-2",
		Variables: map[string]string{
			"recipient.first_name": "Bob",
		},
	}

	sendCallsBefore := deliverer.sendCalls
	resMkt, err := orchestrator.IngestEvent(ctx, reqMarketing, "principal-caller-1")
	require.NoError(t, err)
	// Must be suppressed & marked KILLED
	assert.Equal(t, ledger.IntentStatusKilled, resMkt.Status)
	assert.NotNil(t, resMkt.FailureReason)
	assert.Contains(t, *resMkt.FailureReason, "policy_suppressed")
	// Deliverer MUST NOT be invoked
	assert.Equal(t, sendCallsBefore, deliverer.sendCalls, "Deliverer must not be called when suppressed by policy")

	// Verify intent record is stored as KILLED
	mktIntent, err := s.GetMessageIntent(ctx, tenantID, resMkt.MessageIntentID)
	require.NoError(t, err)
	assert.Equal(t, ledger.IntentStatusKilled, mktIntent.Status)

	// ── SCENARIO 4: Webhook Hard Bounce Causes Permanent Suppression ───────────
	// Dispatch a password reset message to Carol
	deliverer.response = "provider-msg-carol-999"
	reqCarol := ledger.EventIngestRequest{
		EventID:              "evt-" + uuid.NewString(),
		EventType:            "identity.password_reset_requested",
		TemplateKey:          "ZS-IA-005",
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "usr-003",
		RecipientEmail:       "carol@example.com",
		CorrelationID:        "corr-e2e-3",
		Variables: map[string]string{
			"recipient.first_name":    "Carol",
			"security.expiry_minutes": "15",
			"links.action_url":        "https://auth.zoiko.com/reset?token=xyz",
			"message.reference":       "REF-9999",
		},
	}
	resCarol, err := orchestrator.IngestEvent(ctx, reqCarol, "principal-caller-1")
	require.NoError(t, err)
	assert.Equal(t, ledger.IntentStatusDispatched, resCarol.Status)

	// SendGrid delivers a HARD BOUNCE webhook for Carol
	webhookBouncePayload := []byte(`[
		{
			"event": "bounce",
			"type": "blocked",
			"email": "carol@example.com",
			"sg_message_id": "provider-msg-carol-999",
			"reason": "550 5.1.1 User unknown",
			"status": "5.1.1",
			"timestamp": 1700000050
		}
	]`)
	err = webhookProcessor.ProcessRawPayload(ctx, "sendgrid", webhookBouncePayload)
	require.NoError(t, err)

	// Verify Carol is now permanently suppressed
	suppressed, reason, err := s.IsEmailSuppressed(ctx, tenantID, "carol@example.com", ledger.StreamMarketing, ledger.ClassM1)
	require.NoError(t, err)
	assert.True(t, suppressed, "Carol must now be suppressed after hard bounce")
	assert.Equal(t, string(ledger.SuppressionReasonHardBounce), reason)

	// Even Security S0 is suppressed by HARD_BOUNCE (dead mailbox)
	suppressedS0, _, err := s.IsEmailSuppressed(ctx, tenantID, "carol@example.com", ledger.StreamCritical, ledger.ClassS0)
	require.NoError(t, err)
	assert.True(t, suppressedS0, "Hard bounce must suppress subsequent sends including S0")
}
