package inbound

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// Rows and replays (ZS-SVC-T-001 §10). A row of a file delivery enters the SAME pipeline as a real-time delivery:
// the sender was authenticated once, for the whole file, so what is left is everything that decides what the row IS
// - its identity, its exact schema, the deterministic mapping, the duplicate check and the eligibility of a domain to
// receive it. A row is therefore an ordinary inbound exchange with its own evidence and its own handoff, and "file
// accepted" never says anything about it (§10.2).

// RowInput is one row of an accepted file, already converted to a JSON object by the batch engine.
type RowInput struct {
	Conn          *xicconn.Connection
	V             *registry.ContractVersion
	Op            *registry.Operation
	Body          []byte
	SourceRef     string
	CorrelationID string
	// RowKey names the row (file and row number). It is stored with the exchange so a row that meets its own exchange
	// again, because the exchange committed and the row's record was lost, can tell it from a duplicate in the data.
	RowKey string
}

// RowResult is what became of a row.
type RowResult struct {
	ExchangeID         string
	State              string
	Class              string
	Reason             string
	Duplicate          bool
	OriginalExchangeID string
	// Own is set on a duplicate whose original is this very row's earlier exchange.
	Own              bool
	CanonicalHash    string
	MappingVersionID string
}

// AdmitRow runs one row through the pipeline. Per-row outbox events are not emitted: the batch reports its own,
// and every row keeps its full evidence in its exchange.
func (s *Service) AdmitRow(ctx context.Context, in RowInput) (*RowResult, error) {
	now := s.now()
	conn, v, op := in.Conn, in.V, in.Op
	ictx := withTenant(ctx, conn.TenantID)
	base := &Exchange{ExchangeID: uuid.NewString(), TenantID: conn.TenantID, ConnectionID: conn.ConnectionID, ConnectorID: conn.ConnectorID,
		OperationID: op.ID, ContractVersionID: v.ContractVersionID, DomainOutcome: DomainNA, BodyHash: hashBytes(in.Body), BodySize: len(in.Body),
		ContentType: "application/json", SourceSchema: in.SourceRef, AuthMechanism: "BATCH_FILE", CorrelationID: rowCorrelation(in), AttemptNo: 1, Version: 1,
		ReceivedAt: now, UpdatedAt: now}
	idem := v.IdempotencyOf(op)
	if idem == nil {
		return s.rowOutcome(s.reject(ictx, base, "NO_IDEMPOTENCY_PROFILE", "", http.StatusUnprocessableEntity, "the operation declares no idempotency profile", "batch"))
	}
	values, ok := idempotencyValues(idem, http.Header{}, in.Body)
	if !ok {
		return s.rowOutcome(s.reject(ictx, base, "DEDUPE_KEY_MISSING", "", http.StatusUnprocessableEntity, "the row does not carry the identity its contract requires", "batch"))
	}
	scope := []string{"conn", conn.ConnectionID, op.ID}
	if idem.Scope == "TENANT" {
		scope = []string{"tenant", conn.TenantID, conn.ConnectorID}
	}
	base.DedupeKey = sha(append(scope, values...)...)
	base.SourceRefHash = sha(append([]string{"ref"}, values...)...)
	base.Raw = in.Body

	dec := s.evaluate(ctx, conn, v, op, in.SourceRef, "application/json", in.Body)
	if dec.Signal != nil {
		_ = s.Repo.RecordSignal(ictx, conn.TenantID, *dec.Signal(base))
	}
	s.finish(base, dec, op, v, now)
	ev, _ := s.evidence(base, dec, "batch")
	existing, err := s.Repo.Insert(ictx, base, ev, nil)
	switch {
	case err == nil:
		s.observe("row_"+base.State, base.ReasonCode)
		return resultOf(base), nil
	case errors.Is(err, ErrDedupe):
		res, rerr := s.rowOutcome(s.duplicate(ictx, base, existing, idem, "batch"))
		if rerr == nil && res != nil && in.RowKey != "" && existing != nil && existing.CorrelationID == base.CorrelationID {
			res.Own = true
		}
		return res, rerr
	}
	return nil, fmt.Errorf("%w: %v", ErrDependency, err)
}

// rowCorrelation is the correlation id stored with a row's exchange: the batch's, plus the row's key.
func rowCorrelation(in RowInput) string {
	if in.RowKey == "" {
		return in.CorrelationID
	}
	c := in.CorrelationID
	if len(c) > 70 {
		c = c[:70] // the column holds 128 characters
	}
	return c + "|" + in.RowKey
}

func resultOf(e *Exchange) *RowResult {
	return &RowResult{ExchangeID: e.ExchangeID, State: e.State, Class: e.QuarantineClass, Reason: e.ReasonCode, CanonicalHash: e.CanonicalHash, MappingVersionID: e.MappingVersionID}
}

func (s *Service) rowOutcome(o *Outcome) (*RowResult, error) {
	if o.Status == http.StatusServiceUnavailable || o.Exchange == nil {
		return nil, ErrDependency
	}
	if o.Duplicate {
		return &RowResult{ExchangeID: o.Exchange.ExchangeID, State: StateDuplicate, Duplicate: true, OriginalExchangeID: o.Exchange.ExchangeID, Reason: "DUPLICATE",
			CanonicalHash: o.Exchange.CanonicalHash, MappingVersionID: o.Exchange.MappingVersionID}, nil
	}
	return resultOf(o.Exchange), nil
}

// RowPreview is what a row WOULD become, without writing anything (a dry run).
type RowPreview struct {
	State         string
	Class         string
	Reason        string
	CanonicalHash string
	Duplicate     bool
}

// PreviewRow evaluates a row without admitting it. A row whose identity is already held is reported as a
// duplicate, never as something new.
func (s *Service) PreviewRow(ctx context.Context, in RowInput) (*RowPreview, error) {
	conn, v, op := in.Conn, in.V, in.Op
	idem := v.IdempotencyOf(op)
	if idem == nil {
		return &RowPreview{State: StateRejected, Reason: "NO_IDEMPOTENCY_PROFILE"}, nil
	}
	values, ok := idempotencyValues(idem, http.Header{}, in.Body)
	if !ok {
		return &RowPreview{State: StateRejected, Reason: "DEDUPE_KEY_MISSING"}, nil
	}
	scope := []string{"conn", conn.ConnectionID, op.ID}
	if idem.Scope == "TENANT" {
		scope = []string{"tenant", conn.TenantID, conn.ConnectorID}
	}
	key := sha(append(scope, values...)...)
	if _, err := s.Repo.GetByDedupe(withTenant(ctx, conn.TenantID), conn.TenantID, key); err == nil {
		return &RowPreview{State: StateDuplicate, Reason: "DUPLICATE", Duplicate: true}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, ErrDependency
	}
	d := s.evaluate(ctx, conn, v, op, in.SourceRef, "application/json", in.Body)
	return &RowPreview{State: d.State, Class: d.Class, Reason: d.Reason, CanonicalHash: d.CanonicalHash}, nil
}

// ── replay of a held exchange ────────────────────────────────────────────────

// ReplayInput names one exchange to re-run and the contract version to run it under.
type ReplayInput struct {
	ExchangeID string
	// Pin is the contract version the stored body is evaluated under. Nil means the version the exchange ARRIVED
	// under: a historical replay never silently uses today's mapping (I-23, NP-41). A governed migration replay passes
	// the connection's current version explicitly.
	Pin    *registry.ContractVersion
	Actor  Actor
	JobID  string
	DryRun bool
}

// ReplayResult is what a replay found, and whether it changed anything.
type ReplayResult struct {
	ExchangeID string
	Before     RowPreview
	After      RowPreview
	// Skipped says why the exchange was not re-run (it is already applied, final, not replayable, or its body is no
	// longer retained) and is empty when it was.
	Skipped string
	Changed bool
	Applied bool
}

// Replay re-runs a QUARANTINED exchange's stored body under a pinned contract version. It is idempotent against
// effects already applied: an exchange that was accepted, handed off or is final is never touched (the domain's
// own idempotency and compensation rules own anything beyond that, NP-43), and a body that is no longer retained is
// not reconstructed (NP-42): the provider redelivers.
func (s *Service) Replay(ctx context.Context, in ReplayInput) (*ReplayResult, error) {
	e, err := s.Repo.Get(ctx, in.ExchangeID)
	if err != nil {
		return nil, err
	}
	res := &ReplayResult{ExchangeID: e.ExchangeID, Before: RowPreview{State: e.State, Class: e.QuarantineClass, Reason: e.ReasonCode, CanonicalHash: e.CanonicalHash}}
	switch {
	case e.State == StateValidated || e.State == StateHandedOff:
		res.Skipped = "ALREADY_APPLIED"
		return res, nil
	case e.State != StateQuarantined:
		res.Skipped = "FINAL_" + e.State
		return res, nil
	}
	switch e.QuarantineClass {
	case ClassSchema, ClassMapping, ClassDomainPrecondition:
	default:
		res.Skipped = "NOT_REPLAYABLE_" + e.QuarantineClass
		return res, nil
	}
	if len(e.Raw) == 0 {
		res.Skipped = "RAW_NOT_RETAINED"
		return res, nil
	}
	conn, err := s.Conns.Connection(ctx, e.ConnectionID)
	if err != nil {
		if errors.Is(err, xicconn.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	if conn.TenantID != e.TenantID {
		return nil, ErrNotFound
	}
	v := in.Pin
	if v == nil {
		if s.Versions == nil {
			return nil, fmt.Errorf("%w: contract versions are not available", ErrDependency)
		}
		if v, err = s.Versions.GetVersion(middleware.WithTenant(ctx, conn.TenantID), e.ContractVersionID); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
	}
	op := v.Operation(e.OperationID)
	if op == nil || op.Direction != registry.Inbound {
		res.Skipped = "OPERATION_NOT_IN_PINNED_VERSION"
		return res, nil
	}
	d := s.evaluate(ctx, conn, v, op, e.SourceSchema, e.ContentType, e.Raw)
	res.After = RowPreview{State: d.State, Class: d.Class, Reason: d.Reason, CanonicalHash: d.CanonicalHash}
	res.Changed = d.State != e.State || d.Class != e.QuarantineClass || d.Reason != e.ReasonCode || d.CanonicalHash != e.CanonicalHash
	if in.DryRun || !res.Changed {
		return res, nil
	}
	now := s.now()
	from := e.Version
	_, err = s.Repo.Mutate(ctx, e.ExchangeID, func(tx *Tx) error {
		if tx.E.State != StateQuarantined || tx.E.Version != from {
			return ErrVersionConflict // it changed while the replay was computed
		}
		prev := tx.E.QuarantineClass
		tx.E.AttemptNo++
		tx.E.HandoffAttempts, tx.E.LastErrorCode, tx.E.DomainStatus, tx.E.DomainRef = 0, "", 0, ""
		applyDecision(tx.E, d, now)
		tx.NewEvent = &Event{Kind: "REPLAYED", From: StateQuarantined, To: tx.E.State, Actor: in.Actor.PrincipalID, At: now,
			Detail: mustJSON(map[string]any{"replay_job": in.JobID, "pinned_contract_version": v.ContractVersionID, "class": prev, "attempt": tx.E.AttemptNo, "result": tx.E.State})}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.inbound.replayed", AggregateType: "inbound-exchange", AggregateID: tx.E.ExchangeID, ActorID: in.Actor.PrincipalID,
			CorrelationID: in.Actor.CorrelationID, Data: map[string]any{"exchange_id": tx.E.ExchangeID, "tenant_id": tx.E.TenantID, "replay_job_id": in.JobID,
				"contract_version_id": v.ContractVersionID, "mapping_version_id": tx.E.MappingVersionID, "result": tx.E.State}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Applied = true
	return res, nil
}
