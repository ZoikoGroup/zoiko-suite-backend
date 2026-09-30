package ncd

import (
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/events"
)

// Actor is who is doing something, as the gateway verified it.
type Actor struct {
	TenantID      string
	PrincipalID   string
	CorrelationID string
}

// Limits are the §6.5 quotas. Decisions where the spec is silent (OD-13,
// OD-15), recorded here rather than guessed at call sites.
type Limits struct {
	TenantPerHour    int
	IntentPerHour    int
	RecipientPerHour int
	// CriticalRecipientPerHour bounds protected-capacity security traffic
	// against abusive loops (§6.5) — it is exempt from the tenant and intent
	// quotas, never unbounded.
	CriticalRecipientPerHour int
	BulkApprovalThreshold    int
	MaxAttemptsPerRoute      int
	EvidenceWindow           time.Duration
	RecordHandoffWindow      time.Duration
	DeadlineAtRiskWindow     time.Duration
	CircuitFailureThreshold  int
	DeferStep                time.Duration
}

// DefaultLimits are the values the service runs with unless configured.
func DefaultLimits() Limits {
	return Limits{
		TenantPerHour:            5000,
		IntentPerHour:            2000,
		RecipientPerHour:         20,
		CriticalRecipientPerHour: 30,
		BulkApprovalThreshold:    50,
		MaxAttemptsPerRoute:      3,
		EvidenceWindow:           15 * time.Minute,
		RecordHandoffWindow:      time.Hour,
		DeadlineAtRiskWindow:     24 * time.Hour,
		CircuitFailureThreshold:  5,
		DeferStep:                5 * time.Minute,
	}
}

// Service is the NCD application layer: handlers and the worker both call it.
type Service struct {
	store     Store
	recipient RecipientResolver
	transport Transport
	limits    Limits
	log       *zap.Logger
	now       func() time.Time
	kick      chan struct{}
}

// NewService wires the plane.
func NewService(store Store, recipient RecipientResolver, transport Transport, limits Limits, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{
		store: store, recipient: recipient, transport: transport, limits: limits, log: log,
		now:  func() time.Time { return time.Now().UTC() },
		kick: make(chan struct{}, 1),
	}
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Kick asks the worker to run now instead of at its next tick. Non-blocking.
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// emit builds and enqueues one canonical event on tx.
func emit(tx Tx, a Actor, eventType, legalEntityID, key string, payload map[string]any) error {
	out, err := events.Build(eventType, a.CorrelationID, tx.TenantID(), legalEntityID, a.PrincipalID, key, payload)
	if err != nil {
		return err
	}
	return tx.Enqueue(out)
}

// §10.2 canonical event names.
const (
	EvtPrepared         = "communication.prepared"
	EvtBlocked          = "communication.blocked"
	EvtAttemptCreated   = "delivery.attempt.created"
	EvtAttemptUnknown   = "delivery.attempt.unknown"
	EvtEvidenceRecorded = "delivery.evidence.recorded"
	EvtEndpointSupp     = "endpoint.suppressed"
	EvtNoticeAck        = "notice.acknowledged"
	EvtDeadlineAtRisk   = "notice.deadline.at_risk"
	EvtCorrection       = "communication.correction.issued"
	EvtRecordDeclared   = "communication.record.declared"
)

func ptr[T any](v T) *T { return &v }
