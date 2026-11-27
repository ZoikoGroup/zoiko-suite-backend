package certwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/certreliance"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// The event-driven path (ZS-SVC-Z-001 RB-05, DQC-017). When a certification is revoked, the issuing service publishes one
// "reliance-withdrawn" event per recorded reliance. This consumer lets the ledger hear about it in seconds instead of at the next
// scheduled sweep.
//
// THE EVENT IS A TRIGGER, NOT THE TRUTH. On an event that names this service as the relying one, the ledger does not flag anything
// on the event's say-so: it asks the issuing service what state the certification is in (the same read a sweep makes) and flags only
// what that answer supports. So a forged, mistaken, duplicated, reordered or replayed event can cause at most one extra check, never
// a false flag, and a LOST event costs only time, because the scheduled sweep still finds the revocation. For the same reason the
// handler is idempotent and the consumer needs no receipt store: an event that cannot be handled is logged and acknowledged, and the
// sweep is the backstop (the shared inbox would hold a database transaction open across the remote read, which is the pool-deadlock
// its own documentation warns about).

// The two producers publish different envelopes on different topics. data-quality-svc uses the legacy shape; reconciliation-engine-svc
// the canonical one (ZS-EVENT-001).
const (
	EventTypeDataQuality    = "dqc.certification.reliance-withdrawn"
	EventTypeReconciliation = "com.zoikosuite.accounting.reconciliation-certification.reliance-withdrawn"

	TopicDataQuality    = "zoiko.data.quality.events"
	TopicReconciliation = "zoiko.data.reconciliation_engine.events"
)

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Withdrawal is what the ledger needs from a reliance-withdrawn event: which tenant, and which certification.
type Withdrawal struct {
	TenantID string
	Ref      certreliance.Ref
	// Reliance and Revocation are carried for the log only; nothing acts on them.
	RelianceID, RevocationID string
}

type envelopeWire struct {
	Type       string          `json:"type"`
	LegacyType string          `json:"event_type"`
	TenantID   string          `json:"tenantid"`
	LegacyTen  string          `json:"tenant_id"`
	Data       json.RawMessage `json:"data"`
	LegacyBody json.RawMessage `json:"payload"`
}

type withdrawalBody struct {
	RunID          string `json:"run_id"`
	RelianceID     string `json:"reliance_id"`
	RevocationID   string `json:"revocation_id"`
	RelyingService string `json:"relying_service"`
}

// ParseWithdrawal reads one delivery. It reports relevant=false, with no error, for anything that is not a withdrawal this service
// is concerned with: another event type, or a withdrawal naming another relying service. A withdrawal that IS this service's but is
// malformed is an error, so it is logged rather than silently dropped.
func ParseWithdrawal(raw []byte) (w Withdrawal, relevant bool, err error) {
	var e envelopeWire
	if err := json.Unmarshal(raw, &e); err != nil {
		return Withdrawal{}, false, fmt.Errorf("undecodable event: %w", err)
	}
	eventType := e.Type
	if eventType == "" {
		eventType = e.LegacyType
	}
	var service string
	switch eventType {
	case EventTypeDataQuality:
		service = certreliance.ServiceDataQuality
	case EventTypeReconciliation:
		service = certreliance.ServiceReconciliation
	default:
		return Withdrawal{}, false, nil
	}
	body := e.Data
	if len(body) == 0 {
		body = e.LegacyBody
	}
	var b withdrawalBody
	if err := json.Unmarshal(body, &b); err != nil {
		return Withdrawal{}, false, fmt.Errorf("undecodable %s payload: %w", eventType, err)
	}
	if b.RelyingService != certreliance.RelyingService {
		return Withdrawal{}, false, nil // some other consumer's reliance
	}
	tenant := e.TenantID
	if tenant == "" {
		tenant = e.LegacyTen
	}
	if _, perr := uuid.Parse(tenant); perr != nil {
		return Withdrawal{}, true, fmt.Errorf("%s names no valid tenant", eventType)
	}
	if !runIDPattern.MatchString(b.RunID) {
		return Withdrawal{}, true, fmt.Errorf("%s names no valid certification run", eventType)
	}
	return Withdrawal{TenantID: tenant, Ref: certreliance.Ref{Service: service, RunID: b.RunID}, RelianceID: b.RelianceID, RevocationID: b.RevocationID}, true, nil
}

// Reaction is what a withdrawal event led to.
type Reaction struct {
	Watchers int // legal entities that have the certification on their watch list
	Swept    int
	Skipped  int // entities the principal is not authorized to sweep
	NewFlags int
	Failed   int
}

// Reactor turns a withdrawal event into checks. It acts as the same configured principal as the scheduled sweep and is authorized
// for the same permission on each entity, like any caller.
type Reactor struct {
	Store     Store
	Sweeper   *Sweeper
	Authz     Authorizer
	Principal string
	Observer  Observer
	Log       *zap.Logger
}

func (r *Reactor) log() *zap.Logger {
	if r.Log != nil {
		return r.Log
	}
	return zap.NewNop()
}

// OnWithdrawal checks the certification for every legal entity of the tenant that relies on it. A certification nobody here watches
// is nothing to do (it is logged: a relying party that never went through the recorder is worth knowing about).
func (r *Reactor) OnWithdrawal(ctx context.Context, w Withdrawal) (*Reaction, error) {
	if r.Principal == "" {
		return nil, fmt.Errorf("%w: no principal is configured to act on a withdrawal", ErrInvalid)
	}
	ctx = svcmiddleware.WithTenant(ctx, w.TenantID)
	watchers, err := r.Store.ListWatchers(ctx, w.TenantID, w.Ref)
	if err != nil {
		return nil, err
	}
	rx := &Reaction{Watchers: len(watchers)}
	obs := orNoop(r.Observer)
	if len(watchers) == 0 {
		obs.WithdrawalReaction(ReactionUnwatched)
		r.log().Warn("a withdrawal names this service as relying on a certification that is not on its watch list",
			zap.String("tenant_id", w.TenantID), zap.String("certification", w.Ref.String()), zap.String("reliance_id", w.RelianceID))
		return rx, nil
	}
	for _, watched := range watchers {
		if err := r.Authz.CheckAllowed(ctx, r.Principal, watched.LegalEntityID, SweepAction); err != nil {
			rx.Skipped++
			obs.WithdrawalReaction(ReactionUnauthorized)
			r.log().Warn("a withdrawal was not acted on for an entity: the principal is not authorized to sweep it",
				zap.String("tenant_id", w.TenantID), zap.String("legal_entity_id", watched.LegalEntityID), zap.String("principal", r.Principal), zap.Error(err))
			continue
		}
		o := r.Sweeper.SweepOne(ctx, w.TenantID, r.Principal, "certification-withdrawal-event", watched)
		rx.Swept++
		rx.NewFlags += o.NewFlags
		if o.Error == "" {
			obs.WithdrawalReaction(ReactionSwept)
		} else {
			obs.WithdrawalReaction(ReactionFailed)
			rx.Failed++
			r.log().Warn("a withdrawal could not be checked: left to the scheduled sweep", zap.String("tenant_id", w.TenantID),
				zap.String("legal_entity_id", watched.LegalEntityID), zap.String("certification", o.Certification), zap.String("error", o.Error))
		}
	}
	obs.FlagsRaised(TriggerEvent, rx.NewFlags)
	if rx.NewFlags > 0 {
		r.log().Warn("a withdrawal raised flags: journals or postings rest on a revoked certification", zap.String("tenant_id", w.TenantID),
			zap.String("certification", w.Ref.String()), zap.Int("new_flags", rx.NewFlags))
	}
	return rx, nil
}

// MessageReader is the part of *kafka.Reader the consumer uses.
type MessageReader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Consumer reads reliance-withdrawn events and hands each relevant one to the Reactor.
type Consumer struct {
	Reactor *Reactor
	Log     *zap.Logger
	// Attempts is how many times one event is tried when checking it fails (default 3), RetryDelay the wait between tries (default 2s).
	// After the last the event is acknowledged anyway: it was only a trigger, and the scheduled sweep finds the revocation.
	Attempts   int
	RetryDelay time.Duration
	// ReadRetryDelay is the wait after a failed read from the broker (default 5s).
	ReadRetryDelay time.Duration
	Observer       Observer
}

func (c *Consumer) log() *zap.Logger {
	if c.Log != nil {
		return c.Log
	}
	return zap.NewNop()
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Handle processes one delivery. It returns an error only when ctx is cancelled mid-retry; every other outcome (irrelevant,
// malformed, handled, failed after the retries) has a disposition and is safe to acknowledge.
func (c *Consumer) Handle(ctx context.Context, raw []byte) error {
	w, relevant, err := ParseWithdrawal(raw)
	if err != nil {
		orNoop(c.Observer).WithdrawalEvent(EventMalformed)
		c.log().Error("a certification withdrawal event could not be read and is dropped (the scheduled sweep covers it)", zap.Error(err))
		return nil
	}
	if !relevant {
		orNoop(c.Observer).WithdrawalEvent(EventIrrelevant)
		return nil
	}
	attempts := c.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	var last error
	for i := 1; i <= attempts; i++ {
		rx, err := c.Reactor.OnWithdrawal(ctx, w)
		if err == nil {
			orNoop(c.Observer).WithdrawalEvent(EventHandled)
			c.log().Info("a certification withdrawal was handled", zap.String("tenant_id", w.TenantID), zap.String("certification", w.Ref.String()),
				zap.String("reliance_id", w.RelianceID), zap.String("revocation_id", w.RevocationID), zap.Int("watchers", rx.Watchers),
				zap.Int("swept", rx.Swept), zap.Int("skipped", rx.Skipped), zap.Int("failed", rx.Failed), zap.Int("new_flags", rx.NewFlags))
			return nil
		}
		last = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(orDefault(c.RetryDelay, 2*time.Second)):
		}
	}
	orNoop(c.Observer).WithdrawalEvent(EventFailed)
	c.log().Error("a certification withdrawal could not be handled after retries: left to the scheduled sweep",
		zap.String("tenant_id", w.TenantID), zap.String("certification", w.Ref.String()), zap.Int("attempts", attempts), zap.Error(last))
	return nil
}

// Run consumes until ctx is cancelled. A broker that is absent or unreachable must not stop the service: the ledger's own work does
// not depend on this feed, and the scheduled sweep covers what it misses. Read errors are logged and retried. The offset is committed
// only AFTER a delivery has a disposition, so a crash re-delivers it (and the handler is idempotent).
func (c *Consumer) Run(ctx context.Context, reader MessageReader) {
	defer func() { _ = reader.Close() }()
	c.log().Info("certification withdrawal consumer started")
	failures := 0
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				c.log().Info("certification withdrawal consumer stopped")
				return
			}
			failures++
			orNoop(c.Observer).ConsumerError(ErrRead)
			if failures == 1 {
				c.log().Warn("certification withdrawal consumer: read failed, retrying until it recovers (the scheduled sweep covers the gap)", zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(orDefault(c.ReadRetryDelay, 5*time.Second)):
			}
			continue
		}
		if failures > 0 {
			c.log().Info("certification withdrawal consumer: read recovered", zap.Int("after_failures", failures))
			failures = 0
		}
		if err := c.Handle(ctx, msg.Value); err != nil {
			return // cancelled mid-retry: do not acknowledge
		}
		if err := reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
			orNoop(c.Observer).ConsumerError(ErrCommit)
			c.log().Warn("certification withdrawal consumer: offset commit failed, the event may be seen again (the handler is idempotent)", zap.Error(err))
		}
	}
}
