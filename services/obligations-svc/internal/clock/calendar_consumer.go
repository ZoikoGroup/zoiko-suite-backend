package clock

// Calendar corrections (R-001 s7.3, NP-14; tracker OB-S8). jurisdiction-rules
// publishes business-calendar.updated and jurisdiction.calendar.changed when a
// calendar version is published. Every live clock computed on an older
// version of that calendar is recalculated with the facts it was started
// from; if the due instant or the calendar version changes, the clock is
// superseded by a successor in the same transaction (reason
// CALENDAR_CORRECTED) and the old clock and its due_at stay readable.
//
// The consumer is replay-safe: an event id is applied once (consumed_events),
// and a clock already on the new version is not touched again.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/jurisdiction"
	"zoiko.io/obligations-svc/internal/store"
	"zoiko.io/obligations-svc/internal/telemetry"
)

// Calendar event names on zoiko.jurisdiction.events.
const (
	EventBusinessCalendarUpdated = "business-calendar.updated"
	EventCalendarChanged         = "jurisdiction.calendar.changed"
)

// CalendarStore is what the consumer needs from persistence.
type CalendarStore interface {
	EventConsumed(ctx context.Context, eventID string) (bool, error)
	MarkEventConsumed(ctx context.Context, eventID, eventType string, outcome map[string]any) (bool, error)
	ClocksOnOlderCalendar(ctx context.Context, calendarCode string, version int, limit int) ([]store.WorkItem, error)
	GetClockForTenant(ctx context.Context, tenantID, clockID string) (*domain.Clock, error)
	ClockCalculationRequest(ctx context.Context, tenantID, clockID string) (json.RawMessage, error)
	SupersedeClock(ctx context.Context, oldClockID string, expectedVersion int, successor domain.StartClockParams) (*domain.Clock, *domain.Clock, bool, error)
}

// CalendarConsumer applies calendar corrections to live clocks.
type CalendarConsumer struct {
	store     CalendarStore
	calc      jurisdiction.Calculator
	principal string
	metrics   *telemetry.Metrics
	log       *zap.Logger
}

// NewCalendarConsumer builds the consumer; principal is the service principal
// it calculates as (it holds OBLIGATION_CALCULATION_CALCULATE by migration).
func NewCalendarConsumer(st CalendarStore, calc jurisdiction.Calculator, principal string, metrics *telemetry.Metrics, log *zap.Logger) *CalendarConsumer {
	return &CalendarConsumer{store: st, calc: calc, principal: principal, metrics: metrics, log: log}
}

type calendarEnvelope struct {
	EventID   string          `json:"event_id"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

type calendarPayload struct {
	CalendarCode      string `json:"calendar_code"`
	CalendarVersion   int    `json:"calendar_version"`
	Version           int    `json:"version"`
	CalendarVersionID string `json:"calendar_version_id"`
}

// Result counts what one event did.
type Result struct {
	Ignored, Replayed     bool
	Superseded, Unchanged int
	Failed                int
}

// Handle applies one raw event. An error means the event must be retried
// (nothing was recorded as consumed).
func (c *CalendarConsumer) Handle(ctx context.Context, raw []byte) (Result, error) {
	var env calendarEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Result{Ignored: true}, nil // not ours to repair; a poison message is skipped, not retried forever
	}
	if env.EventType != EventBusinessCalendarUpdated && env.EventType != EventCalendarChanged {
		return Result{Ignored: true}, nil
	}
	if env.EventID == "" {
		return Result{Ignored: true}, nil
	}
	seen, err := c.store.EventConsumed(ctx, env.EventID)
	if err != nil {
		return Result{}, err
	}
	if seen {
		return Result{Replayed: true}, nil
	}
	var p calendarPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil || p.CalendarCode == "" {
		_, err := c.store.MarkEventConsumed(ctx, env.EventID, env.EventType, map[string]any{"ignored": "no calendar_code"})
		return Result{Ignored: true}, err
	}
	version := p.CalendarVersion
	if version == 0 {
		version = p.Version
	}
	res := Result{}
	items, err := c.store.ClocksOnOlderCalendar(ctx, p.CalendarCode, version, 500)
	if err != nil {
		return Result{}, err
	}
	for _, it := range items {
		switch out, err := c.recalculate(ctx, it, env.EventID); {
		case err != nil:
			res.Failed++
			c.log.Error("calendar correction: recalculation failed", zap.String("clock_id", it.ID), zap.String("tenant_id", it.TenantID), zap.Error(err))
			c.count("failed")
		case out:
			res.Superseded++
			c.count("superseded")
		default:
			res.Unchanged++
			c.count("unchanged")
		}
	}
	if res.Failed > 0 {
		// Not consumed: the event is retried, and the clocks already
		// superseded are no longer on the older version, so they are skipped.
		return res, fmt.Errorf("%d clock(s) could not be recalculated for %s v%d", res.Failed, p.CalendarCode, version)
	}
	_, err = c.store.MarkEventConsumed(ctx, env.EventID, env.EventType, map[string]any{"calendar_code": p.CalendarCode,
		"calendar_version": version, "superseded": res.Superseded, "unchanged": res.Unchanged})
	return res, err
}

func (c *CalendarConsumer) count(result string) {
	if c.metrics != nil {
		c.metrics.CalendarRecalculations.WithLabelValues(result).Inc()
	}
}

func (c *CalendarConsumer) recalculate(ctx context.Context, it store.WorkItem, eventID string) (bool, error) {
	clk, err := c.store.GetClockForTenant(ctx, it.TenantID, it.ID)
	if err != nil {
		return false, err
	}
	raw, err := c.store.ClockCalculationRequest(ctx, it.TenantID, it.ID)
	if err != nil {
		return false, err
	}
	var req jurisdiction.CalculationRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.Jurisdiction == "" {
		return false, errors.New("the clock holds no calculation request")
	}
	// A pinned pack would answer the old calendar again; a correction is
	// asked of the pack in force.
	req.PackRef, req.PackVersion = "", ""
	sctx := jurisdiction.WithSystemIdentity(ctx, jurisdiction.SystemIdentity{PrincipalID: c.principal, TenantID: it.TenantID})
	calc, err := c.calc.Calculate(sctx, req, "obligations-calendar:"+eventID+":"+clk.ClockID)
	if err != nil {
		return false, err
	}
	if calc.Outcome != domain.CalculationOutcomeCalculated || calc.DueAt == nil {
		return false, fmt.Errorf("jurisdiction-rules-svc answered %s", calc.Outcome)
	}
	reason := "CALENDAR_CORRECTED"
	creq, _ := json.Marshal(req)
	_, _, superseded, err := c.store.SupersedeClock(ctx, clk.ClockID, clk.RowVersion, domain.StartClockParams{
		ClockKey: "calendar:" + eventID + ":" + clk.ClockID, RequestHash: store.ComputeClockRequestHash(map[string]any{"event": eventID, "clock": clk.ClockID}),
		Calc: *calc, CalculationRequest: creq, SupersessionReason: &reason, SystemTenantID: it.TenantID,
		Actor: "system:calendar-consumer", CorrelationID: "calendar-" + eventID, CausationID: eventID})
	return superseded, err
}

// Run consumes the calendar topic until ctx ends. A failed event is retried
// in place (with a pause) before the next one is read, so a correction is
// never skipped; it is committed only once applied.
func (c *CalendarConsumer) Run(ctx context.Context, reader *kafka.Reader) {
	defer func() { _ = reader.Close() }()
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Error("calendar consumer: fetch failed", zap.Error(err))
			time.Sleep(time.Second)
			continue
		}
		for {
			_, err := c.Handle(ctx, msg.Value)
			if err == nil {
				break
			}
			c.log.Error("calendar consumer: event not applied; retrying", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
		if err := reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
			c.log.Error("calendar consumer: commit failed (the event will be redelivered and de-duplicated)", zap.Error(err))
		}
	}
}
