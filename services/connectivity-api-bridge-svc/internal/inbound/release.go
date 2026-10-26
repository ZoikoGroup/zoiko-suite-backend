package inbound

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// Quarantine release (§8.3). A held delivery is never silently released, and never by the person who asked:
//
//	SCHEMA, MAPPING     correct the contract or mapping version, then release: the stored raw body is run through the
//	                    pipeline again under the connection's current pin, as a new attempt of the same exchange
//	DOMAIN_PRECONDITION the domain or its workflow decides: release re-queues the canonical payload for delivery
//	DATA_POLICY         resolved by policy or legal; the body was never kept, so it cannot be replayed from here
//	AUTH_CONTEXT        a security matter: never released, and never with a rewritten tenant
//	IDEMPOTENCY         a reused key with a different body is investigated; the sender uses a new key
//
// Dismissal (REJECTED, with a reason) is available for every class: it closes the exchange without delivering.

// RequestAction asks to release or dismiss a quarantined exchange.
func (s *Service) RequestAction(ctx context.Context, a Actor, exchangeID, action, reason string) (*QRequest, error) {
	action = strings.ToUpper(strings.TrimSpace(action))
	if action != ActionRelease && action != ActionDismiss {
		return nil, fmt.Errorf("%w: action is RELEASE or DISMISS", ErrBadRequest)
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 255 {
		return nil, fmt.Errorf("%w: a reason of at most 255 characters is required", ErrBadRequest)
	}
	var req *QRequest
	_, err := s.Repo.Mutate(ctx, exchangeID, func(tx *Tx) error {
		if tx.E.State != StateQuarantined {
			return ErrTransition
		}
		if action == ActionRelease {
			if err := releasable(tx.E); err != nil {
				return err
			}
		}
		for _, r := range tx.Requests {
			if r.Status == ReqPending || r.Status == ReqApproved {
				return fmt.Errorf("%w: another request is open", ErrRequestState)
			}
		}
		req = &QRequest{RequestID: uuid.NewString(), ExchangeID: exchangeID, Action: action, Reason: reason, Status: ReqPending, RequestedBy: a.PrincipalID, CreatedAt: s.now()}
		tx.NewRequest = req
		tx.NewEvent = &Event{Kind: "QUARANTINE_" + action + "_REQUESTED", From: StateQuarantined, To: StateQuarantined, Actor: a.PrincipalID, At: s.now(),
			Detail: mustJSON(map[string]any{"reason": reason, "class": tx.E.QuarantineClass})}
		return nil
	})
	return req, err
}

// releasable says whether an exchange can be released at all, and from what it would be re-run.
func releasable(e *Exchange) error {
	switch e.QuarantineClass {
	case ClassAuthContext, ClassDataPolicy, ClassIdempotency:
		return fmt.Errorf("%w: %s quarantines are dismissed, never released", ErrNotReleasable, e.QuarantineClass)
	case ClassDomainPrecondition:
		if len(e.Canonical) == 0 && len(e.Raw) == 0 {
			return ErrNoRaw
		}
	default:
		if len(e.Raw) == 0 {
			return ErrNoRaw
		}
	}
	return nil
}

// DecideAction approves or rejects a release or dismissal. Approval applies it at once.
func (s *Service) DecideAction(ctx context.Context, a Actor, exchangeID, requestID string, approve bool) (*Exchange, *QRequest, error) {
	// The pipeline re-run needs the connection, the contract and the stored body, so they are read before the
	// transaction opens; the transaction re-checks that the exchange is still the version that was read.
	snap, err := s.Repo.Get(ctx, exchangeID)
	if err != nil {
		return nil, nil, err
	}
	reqs, err := s.Repo.Requests(ctx, exchangeID)
	if err != nil {
		return nil, nil, err
	}
	var pend *QRequest
	for i := range reqs {
		if reqs[i].RequestID == requestID {
			pend = &reqs[i]
		}
	}
	if pend == nil {
		return nil, nil, ErrRequestNotFound
	}
	var rerun *decision
	needRerun := approve && pend.Action == ActionRelease && pend.Status == ReqPending && snap.State == StateQuarantined &&
		(snap.QuarantineClass == ClassSchema || snap.QuarantineClass == ClassMapping || (snap.QuarantineClass == ClassDomainPrecondition && len(snap.Canonical) == 0))
	if needRerun {
		d, err := s.rerun(ctx, snap)
		if err != nil {
			return nil, nil, err
		}
		rerun = &d
	}
	now := s.now()
	var dec *QRequest
	ex, err := s.Repo.Mutate(ctx, exchangeID, func(tx *Tx) error {
		var req *QRequest
		for i := range tx.Requests {
			if tx.Requests[i].RequestID == requestID {
				req = &tx.Requests[i]
			}
		}
		if req == nil {
			return ErrRequestNotFound
		}
		if req.Status != ReqPending {
			return ErrRequestState
		}
		if a.PrincipalID == req.RequestedBy {
			return ErrSelfAction
		}
		if tx.E.State != StateQuarantined {
			return ErrTransition
		}
		if tx.E.Version != snap.Version && rerun != nil {
			return ErrVersionConflict // it changed while the re-run was computed
		}
		req.DecidedBy, req.DecidedAt = a.PrincipalID, &now
		switch {
		case !approve:
			req.Status = ReqRejected
			tx.NewEvent = &Event{Kind: "QUARANTINE_" + req.Action + "_REJECTED", From: StateQuarantined, To: StateQuarantined, Actor: a.PrincipalID, At: now}
		case req.Action == ActionDismiss:
			req.Status = ReqUsed
			tx.E.State, tx.E.ReasonCode = StateRejected, "DISMISSED_"+tx.E.QuarantineClass
			tx.NewEvent = &Event{Kind: "DISMISSED", From: StateQuarantined, To: StateRejected, Actor: a.PrincipalID, At: now, Detail: mustJSON(map[string]any{"reason": req.Reason})}
			tx.Out = append(tx.Out, OutEvent{Type: "integration.inbound.quarantine-dismissed", AggregateType: "inbound-exchange", AggregateID: tx.E.ExchangeID, ActorID: a.PrincipalID,
				CorrelationID: a.CorrelationID, Data: map[string]any{"exchange_id": tx.E.ExchangeID, "tenant_id": tx.E.TenantID, "quarantine_class": tx.E.QuarantineClass}})
		default:
			if err := releasable(tx.E); err != nil {
				return err
			}
			req.Status = ReqUsed
			prevClass := tx.E.QuarantineClass
			tx.E.AttemptNo++
			tx.E.HandoffAttempts, tx.E.LastErrorCode, tx.E.DomainStatus, tx.E.DomainRef = 0, "", 0, ""
			if rerun != nil {
				applyDecision(tx.E, *rerun, now)
			} else { // DOMAIN_PRECONDITION with the canonical payload retained: re-queue it
				tx.E.State, tx.E.QuarantineClass, tx.E.ReasonCode, tx.E.XICCode = StateValidated, "", "", ""
				tx.E.DomainOutcome = DomainPending
				t := now
				tx.E.NextAttemptAt = &t
			}
			tx.NewEvent = &Event{Kind: "QUARANTINE_RELEASED", From: StateQuarantined, To: tx.E.State, Actor: a.PrincipalID, At: now,
				Detail: mustJSON(map[string]any{"reason": req.Reason, "class": prevClass, "attempt": tx.E.AttemptNo, "result": tx.E.State})}
			tx.Out = append(tx.Out, OutEvent{Type: "integration.inbound.quarantine-released", AggregateType: "inbound-exchange", AggregateID: tx.E.ExchangeID, ActorID: a.PrincipalID,
				CorrelationID: a.CorrelationID, Data: map[string]any{"exchange_id": tx.E.ExchangeID, "tenant_id": tx.E.TenantID, "quarantine_class": prevClass, "attempt": tx.E.AttemptNo, "result": tx.E.State}})
		}
		tx.UpdateRequest, dec = req, req
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return ex, dec, nil
}

// applyDecision writes a re-run's result onto the exchange.
func applyDecision(e *Exchange, d decision, now time.Time) {
	e.State, e.QuarantineClass, e.ReasonCode, e.XICCode = d.State, d.Class, d.Reason, string(d.XIC)
	e.SourceSchema = dflt(d.SourceSchema, e.SourceSchema)
	e.MappingVersionID, e.CanonicalContract, e.CanonicalHash, e.Lineage = d.MappingVersionID, d.CanonicalContract, d.CanonicalHash, d.Lineage
	e.Canonical = nil
	e.DomainOutcome, e.NextAttemptAt = DomainNA, nil
	if d.State == StateValidated || (d.State == StateQuarantined && d.Class == ClassDomainPrecondition) {
		e.Canonical = d.Canonical
	}
	if d.State == StateValidated {
		e.DomainOutcome = DomainPending
		t := now
		e.NextAttemptAt = &t
	}
}

// rerun runs a quarantined exchange's stored raw body through the pipeline again, under the connection's current
// pin. It never touches the exchange.
func (s *Service) rerun(ctx context.Context, e *Exchange) (decision, error) {
	if len(e.Raw) == 0 {
		return decision{}, ErrNoRaw
	}
	conn, err := s.Conns.Connection(ctx, e.ConnectionID)
	if err != nil {
		if errors.Is(err, xicconn.ErrNotFound) {
			return decision{}, ErrNotFound
		}
		return decision{}, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	if conn.TenantID != e.TenantID {
		return decision{}, ErrNotFound
	}
	v, err := s.Conns.PinnedContract(middleware.WithTenant(ctx, conn.TenantID), conn)
	if err != nil {
		return decision{}, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	op := v.Operation(e.OperationID)
	if op == nil || op.Direction != registry.Inbound {
		return decision{}, fmt.Errorf("%w: the connection's current contract no longer has this operation", ErrNotReleasable)
	}
	return s.evaluate(ctx, conn, v, op, e.SourceSchema, e.ContentType, e.Raw), nil
}
