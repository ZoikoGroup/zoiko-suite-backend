// Package evidence delivers each stored policy evaluation to the governance
// ledger (governance-decision-log-svc) — the at-least-once half of decision
// C6 (14 Oct 2026). Evaluate commits the evaluation and an outbox row in one
// transaction before answering; this relay drains the outbox with retries,
// so a ledger outage delays the record but never loses it, and never makes
// an evaluation fail.
package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/decisionlog"
	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/store"
)

// Store is what the relay needs from the database.
type Store interface {
	ClaimEvidence(ctx context.Context, limit int, lease time.Duration) ([]store.PendingEvidence, error)
	FindEvaluationByID(ctx context.Context, tenantID, evaluationID string) (*domain.Evaluation, error)
	MarkEvidenceDelivered(ctx context.Context, evaluationID string) error
	MarkEvidenceRetry(ctx context.Context, evaluationID, lastError string, next time.Time) error
	MarkEvidenceFailed(ctx context.Context, evaluationID, lastError string) error
}

const (
	batchSize = 50
	// lease must outlast one batch of ledger calls (5 s timeout each).
	lease      = 5 * time.Minute
	maxBackoff = 10 * time.Minute
)

// Relay polls the evidence outbox and writes each record to the ledger.
type Relay struct {
	store     Store
	client    decisionlog.Client
	principal string
	interval  time.Duration
	log       *zap.Logger
	now       func() time.Time

	stop chan struct{}
	done chan struct{}
}

// NewRelay builds a relay writing to the ledger as principal (decision D-02:
// policy-svc's own service principal).
func NewRelay(st Store, client decisionlog.Client, principal string, interval time.Duration, log *zap.Logger) *Relay {
	return &Relay{
		store: st, client: client, principal: principal, interval: interval, log: log,
		now:  func() time.Time { return time.Now().UTC() },
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// Start runs the relay until Stop.
func (r *Relay) Start() {
	go func() {
		defer close(r.done)
		t := time.NewTicker(r.interval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), lease)
				r.RunOnce(ctx)
				cancel()
			}
		}
	}()
}

// Stop ends the loop and waits for an in-flight batch to finish.
func (r *Relay) Stop() {
	close(r.stop)
	<-r.done
}

// RunOnce delivers one batch and reports how many records landed.
func (r *Relay) RunOnce(ctx context.Context) int {
	if r.principal == "" {
		// Nothing can be authorized without a writer identity. The rows stay
		// pending (durable) until POLICY_LEDGER_PRINCIPAL_ID is configured.
		r.log.Error("evidence relay idle: POLICY_LEDGER_PRINCIPAL_ID is not set, evaluations are not reaching the governance ledger")
		return 0
	}
	pending, err := r.store.ClaimEvidence(ctx, batchSize, lease)
	if err != nil {
		r.log.Error("evidence relay: claim failed", zap.Error(err))
		return 0
	}
	delivered := 0
	for _, p := range pending {
		if r.deliver(ctx, p) {
			delivered++
		}
	}
	return delivered
}

func backoff(attempts int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < attempts && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// ledgerContext is evaluation_context as the ledger stores it: everything a
// reviewer needs to reproduce the decision (V-001 PDC-I-17). A struct, not a
// map, so redelivery sends byte-identical JSON and the ledger's idempotency
// hash matches.
type ledgerContext struct {
	EvidenceID             string          `json:"evidence_id"`
	RequestedByPrincipalID string          `json:"requested_by_principal_id,omitempty"`
	PolicyType             string          `json:"policy_type"`
	Result                 string          `json:"result"`
	ActionContext          json.RawMessage `json:"action_context"`
	InputFingerprint       string          `json:"input_fingerprint"`
	AsOf                   time.Time       `json:"as_of"`
	KnownAt                time.Time       `json:"known_at"`
	PolicyID               string          `json:"policy_id"`
	PolicyCode             string          `json:"policy_code"`
	ArtifactDigest         string          `json:"artifact_digest"`
	SelectedRuleIDs        []string        `json:"selected_rule_ids"`
	EvaluatorVersion       string          `json:"evaluator_version"`
}

// RecordFor builds the ledger record for an evaluation.
func RecordFor(ev *domain.Evaluation, principal string) (decisionlog.Record, error) {
	ctxJSON, err := json.Marshal(ledgerContext{
		EvidenceID:             ev.EvaluationID,
		RequestedByPrincipalID: ev.RequestedByPrincipalID,
		PolicyType:             ev.PolicyType,
		Result:                 ev.Result,
		ActionContext:          ev.ActionContext,
		InputFingerprint:       ev.InputFingerprint,
		AsOf:                   ev.AsOf.UTC(),
		KnownAt:                ev.KnownAt.UTC(),
		PolicyID:               ev.PolicyID,
		PolicyCode:             ev.PolicyCode,
		ArtifactDigest:         ev.ArtifactDigest,
		SelectedRuleIDs:        ev.SelectedRuleIDs,
		EvaluatorVersion:       ev.EvaluatorVersion,
	})
	if err != nil {
		return decisionlog.Record{}, err
	}
	entity := decisionlog.PlatformScope
	if ev.LegalEntityID != nil && *ev.LegalEntityID != "" {
		entity = *ev.LegalEntityID
	}
	causation := ev.CausationID
	if causation == "" {
		causation = ev.RequestID
	}
	return decisionlog.Record{
		TenantID:          ev.TenantID,
		LegalEntityID:     entity,
		PrincipalID:       principal,
		CorrelationID:     ev.CorrelationID,
		CausationID:       causation,
		RequestID:         "policy-evidence-" + ev.EvaluationID,
		IdempotencyKey:    "policy-svc:evaluation:" + ev.EvaluationID,
		DecisionID:        ev.DecisionID,
		ActionType:        ev.PolicyType,
		Outcome:           ev.Outcome,
		RuleBasis:         ev.RuleBasis,
		PolicyVersionID:   ev.PolicyVersionID,
		EvaluationContext: ctxJSON,
		DecidedAt:         ev.EvaluatedAt,
	}, nil
}

func (r *Relay) deliver(ctx context.Context, p store.PendingEvidence) bool {
	log := r.log.With(zap.String("evidence_id", p.EvaluationID), zap.Int("attempt", p.Attempts))
	ev, err := r.store.FindEvaluationByID(ctx, p.TenantID, p.EvaluationID)
	if err != nil {
		log.Error("evidence relay: evaluation unreadable", zap.Error(err))
		_ = r.store.MarkEvidenceRetry(ctx, p.EvaluationID, err.Error(), r.now().Add(backoff(p.Attempts)))
		return false
	}
	rec, err := RecordFor(ev, r.principal)
	if err == nil {
		err = r.client.RecordDecision(ctx, rec)
	}
	switch {
	case err == nil:
		if markErr := r.store.MarkEvidenceDelivered(ctx, p.EvaluationID); markErr != nil {
			// The ledger has it; a redelivery is answered 200 by its
			// idempotency key, so this only costs one more call.
			log.Error("evidence relay: delivered but not marked", zap.Error(markErr))
		}
		return true
	case errors.Is(err, decisionlog.ErrPermanent):
		log.Error("evidence relay: governance ledger refused the record; parked for review", zap.Error(err))
		_ = r.store.MarkEvidenceFailed(ctx, p.EvaluationID, err.Error())
	default:
		log.Warn("evidence relay: delivery failed; will retry", zap.Error(err))
		_ = r.store.MarkEvidenceRetry(ctx, p.EvaluationID, err.Error(), r.now().Add(backoff(p.Attempts)))
	}
	return false
}
