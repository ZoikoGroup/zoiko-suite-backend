package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

var (
	ErrMissingTenantContext     = errors.New("missing or unauthenticated tenant context")
	ErrInvalidIngestRequest     = errors.New("invalid event ingest request parameters")
	ErrRecipientEmailUnresolved = errors.New("recipient email could not be resolved")
)

// LedgerStore specifies the persistence methods required by the Orchestrator.
type LedgerStore interface {
	CreateMessageIntent(ctx context.Context, intent *MessageIntent) (bool, *MessageIntent, error)
	RecordMessageRender(ctx context.Context, render *MessageRender) error
	RecordDeliveryAttempt(ctx context.Context, attempt *DeliveryAttempt) error
	RecordDeliveryEvent(ctx context.Context, event *DeliveryEvent) error
	UpdateIntentStatus(ctx context.Context, tenantID, intentID string, status IntentStatus, failureReason *string) error
	GetMessageIntent(ctx context.Context, tenantID, intentID string) (*MessageIntent, error)
	GetRenderByIntent(ctx context.Context, tenantID, intentID string) (*MessageRender, error)
}

// Deliverer specifies the delivery transport execution boundary.
type Deliverer interface {
	Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome
}

// RecipientResolver resolves principal contact facts from identity-context-svc.
type RecipientResolver interface {
	ResolveEmail(ctx context.Context, tenantID, callerPrincipalID, recipientPrincipalID string) (string, error)
}

// PolicyResolver evaluates communications policy and suppression rules before rendering.
type PolicyResolver interface {
	Evaluate(ctx context.Context, intent *MessageIntent, stream SenderStream) (PolicyDecision, error)
}

// Orchestrator coordinates event ingestion, deduplication, template integrity,
// kill-switch enforcement, delivery dispatch, and audit ledger recording.
type Orchestrator struct {
	store      LedgerStore
	compiler   *Compiler
	killSwitch *KillSwitchManager
	policy     PolicyResolver
	deliverer  Deliverer
	recipient  RecipientResolver
	log        *zap.Logger
}

// NewOrchestrator constructs the canonical communications orchestrator.
func NewOrchestrator(
	store LedgerStore,
	compiler *Compiler,
	killSwitch *KillSwitchManager,
	deliverer Deliverer,
	recipient RecipientResolver,
	log *zap.Logger,
) *Orchestrator {
	if log == nil {
		log = zap.NewNop()
	}
	return &Orchestrator{
		store:      store,
		compiler:   compiler,
		killSwitch: killSwitch,
		deliverer:  deliverer,
		recipient:  recipient,
		log:        log,
	}
}

// WithPolicyResolver attaches a policy precedence and suppression resolver.
func (o *Orchestrator) WithPolicyResolver(pr PolicyResolver) *Orchestrator {
	o.policy = pr
	return o
}

// OrchestrationResult represents the outcome of an event ingestion and dispatch.
type OrchestrationResult struct {
	MessageIntentID  string       `json:"message_intent_id"`
	Status           IntentStatus `json:"status"`
	IsReplay         bool         `json:"is_replay"`
	DeduplicationKey string       `json:"deduplication_key"`
	RenderID         *string      `json:"render_id,omitempty"`
	AttemptID        *string      `json:"attempt_id,omitempty"`
	ProviderResponse *string      `json:"provider_response,omitempty"`
	FailureReason    *string      `json:"failure_reason,omitempty"`
}

// ComputeDeduplicationKey computes a deterministic SHA-256 idempotency key:
// SHA-256(tenant_id + ":" + event_type + ":" + event_id)
func ComputeDeduplicationKey(tenantID, eventType, eventID string) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s:%s:%s", tenantID, eventType, eventID)))
	return hex.EncodeToString(h.Sum(nil))
}

// IngestEvent executes the canonical communications pipeline per ZS-COMMS-EMAIL-001 §3.
func (o *Orchestrator) IngestEvent(ctx context.Context, req EventIngestRequest, callerPrincipalID string) (*OrchestrationResult, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, ErrMissingTenantContext
	}

	// 1. Validate mandatory fields
	if strings.TrimSpace(req.EventID) == "" {
		return nil, fmt.Errorf("%w: event_id is required", ErrInvalidIngestRequest)
	}
	if strings.TrimSpace(req.EventType) == "" {
		return nil, fmt.Errorf("%w: event_type is required", ErrInvalidIngestRequest)
	}
	if strings.TrimSpace(req.RecipientPrincipalID) == "" {
		return nil, fmt.Errorf("%w: recipient_principal_id is required", ErrInvalidIngestRequest)
	}
	if strings.TrimSpace(req.TemplateKey) == "" {
		return nil, fmt.Errorf("%w: template_key is required", ErrInvalidIngestRequest)
	}
	if strings.TrimSpace(req.CorrelationID) == "" {
		return nil, fmt.Errorf("%w: correlation_id is required", ErrInvalidIngestRequest)
	}
	if strings.TrimSpace(req.LegalEntityID) == "" {
		return nil, fmt.Errorf("%w: legal_entity_id is required", ErrInvalidIngestRequest)
	}

	// 2. Resolve template definition to get communication class and sender stream
	tmplDef, ok := o.compiler.GetDefinition(req.TemplateKey)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrTemplateNotFound, req.TemplateKey)
	}

	// 3. Resolve recipient email
	recipientEmail := strings.TrimSpace(req.RecipientEmail)
	if recipientEmail == "" && o.recipient != nil {
		resolved, err := o.recipient.ResolveEmail(ctx, tenantID, callerPrincipalID, req.RecipientPrincipalID)
		if err != nil {
			o.log.Warn("failed to resolve recipient email from identity service",
				zap.String("recipient_principal_id", req.RecipientPrincipalID),
				zap.Error(err),
			)
			return nil, fmt.Errorf("%w: %v", ErrRecipientEmailUnresolved, err)
		}
		recipientEmail = resolved
	}
	if recipientEmail == "" {
		return nil, fmt.Errorf("%w: recipient %q has no valid email address", ErrRecipientEmailUnresolved, req.RecipientPrincipalID)
	}

	// 4. Compute deterministic deduplication key
	dedupKey := ComputeDeduplicationKey(tenantID, req.EventType, req.EventID)

	// 5. Check Emergency Kill Switch
	blocked, killReason := o.killSwitch.Check(ctx, tenantID, req.TemplateKey)
	if blocked {
		o.log.Warn("communications orchestrator: kill-switch blocked event",
			zap.String("tenant_id", tenantID),
			zap.String("template_key", req.TemplateKey),
			zap.String("reason", killReason),
		)
		intentID := uuid.NewString()
		intent := &MessageIntent{
			MessageIntentID:      intentID,
			TenantID:             tenantID,
			LegalEntityID:        req.LegalEntityID,
			RecipientPrincipalID: req.RecipientPrincipalID,
			RecipientEmail:       recipientEmail,
			Channel:              "EMAIL",
			CommunicationClass:   tmplDef.CommunicationClass,
			TemplateKey:          req.TemplateKey,
			EventID:              req.EventID,
			SourceEventType:      req.EventType,
			DeduplicationKey:     dedupKey,
			CorrelationID:        req.CorrelationID,
			CausationID:          req.CausationID,
			Status:               IntentStatusKilled,
			FailureReason:        &killReason,
			CreatedAt:            time.Now().UTC(),
			UpdatedAt:            time.Now().UTC(),
		}
		created, stored, err := o.store.CreateMessageIntent(ctx, intent)
		if err != nil {
			o.log.Error("failed to record killed intent in ledger", zap.String("intent_id", intentID), zap.Error(err))
		}
		if stored != nil {
			intentID = stored.MessageIntentID
		}
		return &OrchestrationResult{
			MessageIntentID:  intentID,
			Status:           IntentStatusKilled,
			IsReplay:         !created,
			DeduplicationKey: dedupKey,
			FailureReason:    &killReason,
		}, nil
	}

	// 6. Insert Message Intent (Idempotent ON CONFLICT)
	intentID := uuid.NewString()
	intent := &MessageIntent{
		MessageIntentID:      intentID,
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		RecipientPrincipalID: req.RecipientPrincipalID,
		RecipientEmail:       recipientEmail,
		Channel:              "EMAIL",
		CommunicationClass:   tmplDef.CommunicationClass,
		TemplateKey:          req.TemplateKey,
		EventID:              req.EventID,
		SourceEventType:      req.EventType,
		DeduplicationKey:     dedupKey,
		CorrelationID:        req.CorrelationID,
		CausationID:          req.CausationID,
		Status:               IntentStatusPending,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}

	created, storedIntent, err := o.store.CreateMessageIntent(ctx, intent)
	if err != nil {
		return nil, fmt.Errorf("create message intent: %w", err)
	}

	// 7. Idempotent replay handling
	if !created {
		o.log.Info("communications orchestrator: duplicate event ingested (idempotent replay)",
			zap.String("tenant_id", tenantID),
			zap.String("event_id", req.EventID),
			zap.String("deduplication_key", dedupKey),
			zap.String("existing_intent_id", storedIntent.MessageIntentID),
		)
		var existingRenderID *string
		if r, rErr := o.store.GetRenderByIntent(ctx, tenantID, storedIntent.MessageIntentID); rErr == nil && r != nil {
			existingRenderID = &r.RenderID
		}
		return &OrchestrationResult{
			MessageIntentID:  storedIntent.MessageIntentID,
			Status:           storedIntent.Status,
			IsReplay:         true,
			DeduplicationKey: dedupKey,
			RenderID:         existingRenderID,
			FailureReason:    storedIntent.FailureReason,
		}, nil
	}

	// 7.5. Policy & Suppression Precedence Evaluation (ZS-COMMS-EMAIL-001 §4, §5)
	if o.policy != nil {
		decision, pErr := o.policy.Evaluate(ctx, intent, tmplDef.SenderStream)
		if pErr != nil {
			failMsg := fmt.Sprintf("policy evaluation error: %v", pErr)
			if upErr := o.store.UpdateIntentStatus(ctx, tenantID, intent.MessageIntentID, IntentStatusFailed, &failMsg); upErr != nil {
				o.log.Error("failed to update intent status to FAILED", zap.String("intent_id", intent.MessageIntentID), zap.Error(upErr))
			}
			return nil, fmt.Errorf("policy evaluation: %w", pErr)
		}
		if !decision.Allowed {
			o.log.Info("communications orchestrator: event suppressed by policy",
				zap.String("tenant_id", tenantID),
				zap.String("intent_id", intent.MessageIntentID),
				zap.String("rule", decision.RuleName),
				zap.String("reason", decision.Reason),
			)
			suppressReason := fmt.Sprintf("policy_suppressed: %s", decision.Reason)
			if upErr := o.store.UpdateIntentStatus(ctx, tenantID, intent.MessageIntentID, IntentStatusKilled, &suppressReason); upErr != nil {
				o.log.Error("failed to update suppressed intent to KILLED", zap.String("intent_id", intent.MessageIntentID), zap.Error(upErr))
			}
			return &OrchestrationResult{
				MessageIntentID:  intent.MessageIntentID,
				Status:           IntentStatusKilled,
				IsReplay:         false,
				DeduplicationKey: dedupKey,
				FailureReason:    &suppressReason,
			}, nil
		}
	}

	// 8. Template compilation and rendering with SHA-256 verification
	renderRes, err := o.compiler.Render(req.TemplateKey, req.Variables)
	if err != nil {
		failMsg := fmt.Sprintf("render template failed: %v", err)
		if upErr := o.store.UpdateIntentStatus(ctx, tenantID, intent.MessageIntentID, IntentStatusFailed, &failMsg); upErr != nil {
			o.log.Error("failed to update intent status to FAILED", zap.String("intent_id", intent.MessageIntentID), zap.Error(upErr))
		}
		return nil, fmt.Errorf("render template %q: %w", req.TemplateKey, err)
	}

	// 9. Record Message Render in Delivery Ledger
	renderID := uuid.NewString()
	render := &MessageRender{
		RenderID:        renderID,
		MessageIntentID: intent.MessageIntentID,
		TenantID:        tenantID,
		TemplateKey:     renderRes.TemplateKey,
		TemplateVersion: renderRes.TemplateVersion,
		Locale:          renderRes.Locale,
		ContentHash:     renderRes.ContentHash,
		Subject:         renderRes.Subject,
		BodyHTML:        renderRes.BodyHTML,
		BodyText:        renderRes.BodyText,
		RenderedAt:      time.Now().UTC(),
	}
	if err := o.store.RecordMessageRender(ctx, render); err != nil {
		failMsg := fmt.Sprintf("record message render failed: %v", err)
		if upErr := o.store.UpdateIntentStatus(ctx, tenantID, intent.MessageIntentID, IntentStatusFailed, &failMsg); upErr != nil {
			o.log.Error("failed to update intent status to FAILED", zap.String("intent_id", intent.MessageIntentID), zap.Error(upErr))
		}
		return nil, fmt.Errorf("record message render: %w", err)
	}
	if err := o.store.UpdateIntentStatus(ctx, tenantID, intent.MessageIntentID, IntentStatusRendered, nil); err != nil {
		o.log.Warn("failed to update intent status to RENDERED", zap.String("intent_id", intent.MessageIntentID), zap.Error(err))
	}

	// 10. Dispatch delivery using existing Deliverer abstraction
	senderIdentity := ResolveSenderIdentity(tmplDef.SenderStream, req.TemplateKey)

	headers := make(map[string]string)
	if tmplDef.SenderStream == StreamMarketing {
		// RFC 8058 One-Click List-Unsubscribe
		headers["List-Unsubscribe"] = fmt.Sprintf("<https://notify.zoiko.com/v1/notifications/unsubscribe?tenant_id=%s>, <mailto:unsubscribe@news.zoikosuite.com?subject=unsubscribe>", tenantID)
		headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
	}

	notificationForDeliverer := domain.Notification{
		NotificationID:       intent.MessageIntentID,
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		RecipientPrincipalID: req.RecipientPrincipalID,
		RecipientAddress:     recipientEmail,
		Channel:              "EMAIL",
		From:                 senderIdentity.Formatted(),
		Headers:              headers,
		Subject:              renderRes.Subject,
		Body:                 renderRes.BodyHTML,
		Status:               "PENDING",
		SourceEventType:      req.EventType,
		SourceReference:      req.EventID,
		CorrelationID:        req.CorrelationID,
		CreatedByPrincipalID: callerPrincipalID,
		CreatedAt:            time.Now().UTC(),
	}

	outcome := o.deliverer.Deliver(ctx, notificationForDeliverer)

	// 11. Record Delivery Attempt
	attemptID := uuid.NewString()
	attemptStatus := AttemptStatusAccepted
	var attemptFailureReason *string
	if !outcome.Delivered {
		attemptStatus = AttemptStatusFailed
		attemptFailureReason = &outcome.Reason
	}
	var providerMessageID *string
	if outcome.ProviderResponse != "" {
		resp := outcome.ProviderResponse
		providerMessageID = &resp
	}

	providerName := outcome.ProviderName
	if providerName == "" {
		providerName = "DEFAULT"
	}

	attempt := &DeliveryAttempt{
		ProviderAttemptID: attemptID,
		MessageIntentID:   intent.MessageIntentID,
		RenderID:          renderID,
		TenantID:          tenantID,
		SenderStream:      tmplDef.SenderStream,
		FromAddress:       senderIdentity.Email,
		ToAddress:         recipientEmail,
		ProviderName:      providerName,
		ProviderMessageID: providerMessageID,
		Status:            attemptStatus,
		FailureReason:     attemptFailureReason,
		AttemptNumber:     1,
		AttemptedAt:       time.Now().UTC(),
	}
	if err := o.store.RecordDeliveryAttempt(ctx, attempt); err != nil {
		o.log.Error("failed to record delivery attempt in ledger",
			zap.String("intent_id", intent.MessageIntentID),
			zap.String("attempt_id", attemptID),
			zap.Error(err),
		)
	}

	// 12. Record initial Delivery Event
	deliveryEventID := uuid.NewString()
	eventType := DeliveryEventAccepted
	if !outcome.Delivered {
		eventType = DeliveryEventDropped
	}
	payloadBytes, _ := json.Marshal(map[string]any{
		"outcome_delivered": outcome.Delivered,
		"provider_response": outcome.ProviderResponse,
		"reason":            outcome.Reason,
	})
	deliveryEvent := &DeliveryEvent{
		DeliveryEventID:   deliveryEventID,
		ProviderAttemptID: attemptID,
		MessageIntentID:   intent.MessageIntentID,
		TenantID:          tenantID,
		EventType:         eventType,
		RawPayload:        payloadBytes,
		OccurredAt:        time.Now().UTC(),
	}
	if err := o.store.RecordDeliveryEvent(ctx, deliveryEvent); err != nil {
		o.log.Error("failed to record delivery event in ledger",
			zap.String("intent_id", intent.MessageIntentID),
			zap.String("event_id", deliveryEventID),
			zap.Error(err),
		)
	}

	// 13. Update Intent status to final outcome
	finalStatus := IntentStatusDispatched
	if !outcome.Delivered {
		finalStatus = IntentStatusFailed
	}
	if err := o.store.UpdateIntentStatus(ctx, tenantID, intent.MessageIntentID, finalStatus, attemptFailureReason); err != nil {
		o.log.Error("failed to update intent status to final outcome",
			zap.String("intent_id", intent.MessageIntentID),
			zap.String("status", string(finalStatus)),
			zap.Error(err),
		)
	}

	return &OrchestrationResult{
		MessageIntentID:  intent.MessageIntentID,
		Status:           finalStatus,
		IsReplay:         false,
		DeduplicationKey: dedupKey,
		RenderID:         &renderID,
		AttemptID:        &attemptID,
		ProviderResponse: providerMessageID,
		FailureReason:    attemptFailureReason,
	}, nil
}
