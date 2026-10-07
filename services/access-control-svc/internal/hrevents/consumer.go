// Package hrevents opens event-triggered access reviews from HR lifecycle
// events (Authorization Standard §24 "event-triggered review: manager change,
// entity transfer, ..."; Group 1 audit gap S9-C2).
//
// Producers, by the names they actually publish (grep the producers, not the
// spec):
//
//	employee-master-svc       zoiko.employee.events     employee.updated (manager_employee_id),
//	                                                    employee.status.changed (old/new_status, worker_type),
//	                                                    employee.terminated
//	offboarding-severance-svc zoiko.offboarding.events  employee.terminated
//
// Every payload names an employee_id. The subject is resolved through the
// administered subject link (migration 000015); an unlinked employee is
// counted and skipped, never guessed.
//
//	mover   employee.updated whose manager differs from the last one seen
//	        → review by the NEW manager's principal (when linked)
//	leaver  employee.terminated, or a status move into TERMINATED / RESIGNED /
//	        DEACTIVATED / INACTIVE / ARCHIVED ("contractor end" for a
//	        CONTRACTOR) → review by the last manager's principal (when linked)
//
// Everything else defaults to the configured reviewer. A review is opened,
// never a revocation: the items are decided by people.
package hrevents

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
)

// Links resolves and records employees (store.PgStore).
type Links interface {
	FindSubjectLink(ctx context.Context, employeeID string) (*domain.SubjectLink, error)
	ObserveSubject(ctx context.Context, employeeID, managerEmployeeID, status string) (*domain.SubjectLink, error)
}

// Trigger opens a review (handler.Gov).
type Trigger interface {
	TriggerReview(ctx context.Context, t domain.ReviewTrigger) (*domain.ReviewCampaign, bool, error)
}

// Outcome labels for Handler.Handle, exported for the metric.
const (
	OutcomeOpened    = "review_opened"
	OutcomeReplayed  = "review_replayed"
	OutcomeNothing   = "nothing_to_review"
	OutcomeNoTrigger = "no_trigger"
	OutcomeUnlinked  = "unlinked"
	OutcomeIgnored   = "ignored"
	OutcomeMalformed = "malformed"
	OutcomeRefused   = "refused"
)

// Handler decides what one HR event means.
type Handler struct {
	links   Links
	trigger Trigger
	// transient reports a failure worth retrying (an outage) as opposed to a
	// refusal that redelivery will not change (handler.IsTransient).
	transient func(error) bool
	log       *zap.Logger
}

func NewHandler(links Links, trigger Trigger, transient func(error) bool, log *zap.Logger) *Handler {
	return &Handler{links: links, trigger: trigger, transient: transient, log: log}
}

type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	TenantID      string          `json:"tenant_id"`
	LegalEntityID string          `json:"legal_entity_id"`
	Payload       json.RawMessage `json:"payload"`
}

type payload struct {
	EmployeeID        string `json:"employee_id"`
	LegalEntityID     string `json:"legal_entity_id"`
	ManagerEmployeeID string `json:"manager_employee_id"`
	Status            string `json:"status"`
	OldStatus         string `json:"old_status"`
	NewStatus         string `json:"new_status"`
	WorkerType        string `json:"worker_type"`
}

var leaverStatuses = map[string]bool{"TERMINATED": true, "RESIGNED": true, "DEACTIVATED": true, "INACTIVE": true, "ARCHIVED": true}

// Handle processes one message. A non-nil error is transient (store or
// authorization-svc unavailable) and the message should be retried; every
// other outcome is final and returned as the outcome label.
//
// Order matters for retries: the link is read, the review opened (idempotent
// on the event id), and only then is the observation recorded. Recording
// first would make a retried delivery see the new manager / status as
// already known and never open the review.
func (h *Handler) Handle(ctx context.Context, value []byte) (string, error) {
	var env envelope
	if err := json.Unmarshal(value, &env); err != nil {
		return OutcomeMalformed, nil
	}
	switch env.EventType {
	case "employee.updated", "employee.status.changed", "employee.terminated":
	default:
		return OutcomeIgnored, nil
	}
	var p payload
	if err := json.Unmarshal(env.Payload, &p); err != nil || env.TenantID == "" || p.EmployeeID == "" || env.EventID == "" {
		return OutcomeMalformed, nil
	}
	entity := env.LegalEntityID
	if entity == "" {
		entity = p.LegalEntityID
	}
	ctx = svcmiddleware.WithTenant(ctx, env.TenantID)

	link, err := h.links.FindSubjectLink(ctx, p.EmployeeID)
	if err != nil {
		return "", err
	}
	if link == nil {
		return OutcomeUnlinked, nil
	}
	if entity == "" {
		entity = link.LegalEntityID
	}

	var reason, reviewerEmployee, observedManager, observedStatus string
	switch env.EventType {
	case "employee.updated":
		observedManager, observedStatus = p.ManagerEmployeeID, p.Status
		if link.LastManagerEmployeeID != "" && p.ManagerEmployeeID != "" && p.ManagerEmployeeID != link.LastManagerEmployeeID {
			reason = "mover: manager changed from employee " + link.LastManagerEmployeeID + " to " + p.ManagerEmployeeID
			reviewerEmployee = p.ManagerEmployeeID
		}
	case "employee.status.changed":
		observedStatus = p.NewStatus
		if leaverStatuses[p.NewStatus] && !leaverStatuses[link.LastStatus] {
			reason = "leaver: status " + p.OldStatus + " -> " + p.NewStatus
			if strings.EqualFold(p.WorkerType, "CONTRACTOR") {
				reason = "contractor end: status " + p.OldStatus + " -> " + p.NewStatus
			}
			reviewerEmployee = link.LastManagerEmployeeID
		}
	case "employee.terminated":
		observedStatus = "TERMINATED"
		// Both producers publish it for the same exit; the second finds the
		// first's observation and opens nothing.
		if !leaverStatuses[link.LastStatus] {
			reason = "leaver: employee terminated"
			reviewerEmployee = link.LastManagerEmployeeID
		}
	}

	outcome := OutcomeNoTrigger
	if reason != "" {
		reviewer := ""
		if reviewerEmployee != "" {
			mgr, err := h.links.FindSubjectLink(ctx, reviewerEmployee)
			if err != nil {
				return "", err
			}
			if mgr != nil && mgr.PrincipalID != link.PrincipalID {
				reviewer = mgr.PrincipalID
			}
		}
		c, created, err := h.trigger.TriggerReview(ctx, domain.ReviewTrigger{
			TenantID: env.TenantID, LegalEntityID: entity, SubjectPrincipalID: link.PrincipalID,
			ReviewerPrincipalID: reviewer, Reason: reason, CorrelationID: "hr-" + env.EventID,
		})
		switch {
		case err != nil && h.transient(err):
			return "", err
		case err != nil:
			h.log.Warn("HR event review refused", zap.String("event_id", env.EventID), zap.String("employee_id", p.EmployeeID), zap.Error(err))
			outcome = OutcomeRefused
		case c == nil:
			outcome = OutcomeNothing
		case created:
			outcome = OutcomeOpened
			h.log.Info("event-triggered review opened", zap.String("campaign_id", c.CampaignID), zap.String("subject", link.PrincipalID), zap.String("reason", reason))
		default:
			outcome = OutcomeReplayed
		}
	}
	if _, err := h.links.ObserveSubject(ctx, p.EmployeeID, observedManager, observedStatus); err != nil {
		return "", err
	}
	return outcome, nil
}

// Reader is the slice of kafka.Reader the loop uses.
type Reader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Run consumes until ctx ends. A transient failure is retried with backoff
// (capped) for up to maxAttempts, then the message is committed and counted
// as dropped, so one bad partition head cannot stall every later event — the
// silent-stall failure mode kafka-go makes easy. observe records each outcome.
func Run(ctx context.Context, r Reader, h *Handler, log *zap.Logger, observe func(outcome string)) {
	const maxAttempts = 8
	for {
		m, err := r.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("HR event fetch failed", zap.Error(err))
			sleep(ctx, 2*time.Second)
			continue
		}
		outcome := "dropped"
		backoff := 500 * time.Millisecond
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			o, err := h.Handle(ctx, m.Value)
			if err == nil {
				outcome = o
				break
			}
			if ctx.Err() != nil {
				return
			}
			log.Warn("HR event handling failed; retrying", zap.Int("attempt", attempt), zap.String("topic", m.Topic), zap.Int64("offset", m.Offset), zap.Error(err))
			sleep(ctx, backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
		if outcome == "dropped" {
			log.Error("HR event dropped after retries; its review was not opened", zap.String("topic", m.Topic), zap.Int64("offset", m.Offset))
		}
		observe(outcome)
		if err := r.CommitMessages(ctx, m); err != nil && ctx.Err() == nil {
			log.Warn("HR event commit failed", zap.Error(err))
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
