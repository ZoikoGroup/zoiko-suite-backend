package store

// R-001 WFC-05 deadline clocks: persistence (migration 000016).
//
// Every write runs in the clock's tenant transaction (FORCE RLS), writes an
// append-only transition row and its event through the outbox in the same
// transaction (R-INV-23). due_at and its provenance are never updated (a
// trigger refuses it); a recalculation supersedes the clock and inserts its
// successor in one transaction (R-INV-17). The sweep and the calendar
// consumer find work across tenants under the read-only app.clock_sweep
// policy and then act inside each clock's own tenant.

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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/obligations-svc/internal/domain"
)

// System actors.
const (
	ActorClockSweep       = "system:clock-sweep"
	ActorCalendarConsumer = "system:calendar-consumer"
	ActorEscalation       = "system:escalation"
)

// Outbox destinations (000016).
const (
	DestinationKafka        = "KAFKA"
	DestinationWorkflowCase = "WORKFLOW_CASE"
)

const clockColumns = `clock_id, clock_key, obligation_id, tenant_id, legal_entity_id, clock_class, clock_state,
	COALESCE(jurisdiction_code, ''), COALESCE(rule_obligation_code, ''), obligation_rule_id, rule_version, COALESCE(rule_content_digest, ''),
	calendar_code, calendar_version, calendar_version_id, COALESCE(timezone, 'UTC'), business_day_adjustment,
	anchor_date::text, unadjusted_date::text, COALESCE(local_due_date::text, ''), due_at, EXTRACT(EPOCH FROM due_soon_threshold)::bigint,
	extension_days_granted, pack_ref, pack_version, pack_version_id, COALESCE(calculation_ref, ''), calculation_explanation,
	COALESCE(trigger_event_id, ''), COALESCE(trigger_event_type, ''), COALESCE(trigger_event_time, created_at), business_event_time,
	COALESCE(source_object_type, ''), COALESCE(source_object_id, ''), source_object_version, ingested_at, ingestion_latency_ms,
	supersedes_clock_id::text, superseded_by_clock_id::text, superseded_at, supersession_reason,
	satisfied_at, satisfied_by_principal_id, evidence_refs, evidence_evaluation_id, evidence_outcome, evidence_exempt_reason,
	domain_confirmation_ref, subject_state,
	authority_action, requested_by, approved_by, approval_reference, reason_code, reason_text, authority_evidence_refs,
	applicability_decision_id::text, waived_at, not_applicable_at, overdue_since,
	escalation_policy_id::text, escalation_policy_version, priority, additional_owners,
	plane, data_class, residency_region, correlation_id, created_by, updated_by, created_at, updated_at, row_version`

func scanClockRow(row pgx.Row) (*domain.Clock, error) {
	var c domain.Clock
	var class, state string
	var dueSoonSeconds int64
	var explain []byte
	err := row.Scan(&c.ClockID, &c.ClockKey, &c.ObligationID, &c.TenantID, &c.LegalEntityID, &class, &state,
		&c.JurisdictionCode, &c.RuleObligationCode, &c.ObligationRuleID, &c.RuleVersion, &c.RuleContentDigest,
		&c.CalendarCode, &c.CalendarVersion, &c.CalendarVersionID, &c.Timezone, &c.BusinessDayAdjustment,
		&c.AnchorDate, &c.UnadjustedDate, &c.LocalDueDate, &c.DueAt, &dueSoonSeconds,
		&c.ExtensionDaysGranted, &c.PackRef, &c.PackVersion, &c.PackVersionID, &c.CalculationRef, &explain,
		&c.Trigger.EventID, &c.Trigger.EventType, &c.Trigger.EventTime, &c.Trigger.BusinessEventTime,
		&c.Trigger.SourceObjectType, &c.Trigger.SourceObjectID, &c.Trigger.SourceObjectVersion, &c.Trigger.IngestedAt, &c.Trigger.IngestionLatencyMS,
		&c.SupersedesClockID, &c.SupersededByClockID, &c.SupersededAt, &c.SupersessionReason,
		&c.SatisfiedAt, &c.SatisfiedBy, &c.EvidenceRefs, &c.EvidenceEvaluationID, &c.EvidenceOutcome, &c.EvidenceExemptReason,
		&c.DomainConfirmationRef, &c.SubjectState,
		&c.AuthorityAction, &c.RequestedBy, &c.ApprovedBy, &c.ApprovalReference, &c.ReasonCode, &c.ReasonText, &c.AuthorityEvidenceRefs,
		&c.ApplicabilityDecisionID, &c.WaivedAt, &c.NotApplicableAt, &c.OverdueSince,
		&c.EscalationPolicyID, &c.EscalationPolicyVersion, &c.Priority, &c.AdditionalOwners,
		&c.Plane, &c.DataClass, &c.ResidencyRegion, &c.CorrelationID, &c.CreatedBy, &c.UpdatedBy, &c.CreatedAt, &c.UpdatedAt, &c.RowVersion)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrClockNotFound
		}
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, mapPgError(err))
	}
	c.ClockClass, c.StoredState = domain.ClockClass(class), domain.ClockState(state)
	if dueSoonSeconds > 0 {
		t := c.DueAt.Add(-time.Duration(dueSoonSeconds) * time.Second)
		c.DueSoonAt = &t
	}
	if len(explain) == 0 {
		explain = []byte("[]")
	}
	c.CalculationExplain = explain
	if c.EvidenceRefs == nil {
		c.EvidenceRefs = []string{}
	}
	if c.AuthorityEvidenceRefs == nil {
		c.AuthorityEvidenceRefs = []string{}
	}
	if c.AdditionalOwners == nil {
		c.AdditionalOwners = []string{}
	}
	c.Resolve(time.Now().UTC())
	return &c, nil
}

// withTenantIDTx is withTenantTx for system workers, which carry the tenant
// of the row they act on rather than a request's.
func (s *PgStore) withTenantIDTx(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	if tenantID == "" {
		return domain.ErrTenantMissing
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, mapPgError(err))
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// withSweepTx runs a cross-tenant READ under the app.clock_sweep policy.
func (s *PgStore) withSweepTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.clock_sweep', 'true', true)"); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func storeErr(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" { // check_violation from a guard trigger
		return fmt.Errorf("%w: %s", domain.ErrClockCommandNotAllowed, pgErr.Message)
	}
	if errors.Is(err, domain.ErrStoreUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, mapPgError(err))
}

// insertClockEventTx writes a clock event with the R-001 s10.3 envelope
// fields to the outbox, in the caller's transaction.
func insertClockEventTx(ctx context.Context, tx pgx.Tx, c *domain.Clock, eventType, actor, correlationID, causationID string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["clock_id"] = c.ClockID
	payload["obligation_id"] = c.ObligationID
	payload["clock_class"] = string(c.ClockClass)
	payload["due_at"] = c.DueAt.UTC().Format(time.RFC3339Nano)
	payload["local_due_date"] = c.LocalDueDate
	payload["timezone"] = c.Timezone
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	sum := sha256.Sum256(rawPayload)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ruleRefs := map[string]any{"obligation_rule_id": c.ObligationRuleID, "rule_version": c.RuleVersion,
		"rule_content_digest": c.RuleContentDigest, "calculation_ref": c.CalculationRef}
	if c.CalendarVersionID != nil {
		ruleRefs["calendar_version_id"] = *c.CalendarVersionID
	}
	if c.PackVersionID != nil {
		ruleRefs["pack_version_id"] = *c.PackVersionID
	}
	evidence := append([]string{}, c.EvidenceRefs...)
	evidence = append(evidence, c.AuthorityEvidenceRefs...)
	env := map[string]any{
		"event_id":               "evt-" + uuid.New().String(),
		"event_type":             eventType,
		"event_name":             eventType,
		"event_version":          "1.0",
		"payload_schema_version": "1.0",
		"schema_version":         "1.0",
		"event_time":             now,
		"emitted_at":             now,
		"source_service":         "obligations-svc",
		"emitting_service":       "obligations-svc",
		"tenant_id":              c.TenantID,
		"plane":                  c.Plane,
		"legal_entity_id":        c.LegalEntityID,
		"jurisdiction":           c.JurisdictionCode,
		"actor_id":               actor,
		"subject":                map[string]any{"type": "OBLIGATION_CLOCK", "id": c.ClockID, "version": c.RowVersion},
		"correlation_id":         correlationID,
		"causation_id":           causationID,
		"rule_refs":              ruleRefs,
		"data_class":             c.DataClass,
		"residency_region":       c.ResidencyRegion,
		"evidence_refs":          evidence,
		"idempotency_key":        c.ClockKey,
		"payload_hash":           "sha256:" + hex.EncodeToString(sum[:]),
		"payload":                json.RawMessage(rawPayload),
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}
	if correlationID == "" {
		correlationID = "clock-" + c.ClockID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO event_outbox (tenant_id, event_type, event_payload, correlation_id, destination)
		VALUES ($1, $2, $3, $4, 'KAFKA')`, c.TenantID, eventType, raw, correlationID); err != nil {
		return storeErr(err)
	}
	return nil
}

func insertClockTransitionTx(ctx context.Context, tx pgx.Tx, c *domain.Clock, from *domain.ClockState, to domain.ClockState,
	command, actor string, reasonCode, reasonText *string, evidence []string, effectiveAt time.Time, correlationID, causationID string) error {
	var fromS *string
	if from != nil {
		v := string(*from)
		fromS = &v
	}
	if evidence == nil {
		evidence = []string{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO obligation_clock_transitions
		(tenant_id, clock_id, from_state, to_state, command, actor, reason_code, reason_text, evidence_refs, row_version,
		 effective_at, correlation_id, causation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12, ''), NULLIF($13, ''))`,
		c.TenantID, c.ClockID, fromS, string(to), command, actor, reasonCode, reasonText, evidence, c.RowVersion,
		effectiveAt, correlationID, causationID)
	return storeErr(err)
}

// stateEvent names the event a state move emits.
func stateEvent(to domain.ClockState) string {
	switch to {
	case domain.ClockStateActive:
		return "obligation.clock.activated"
	case domain.ClockStateDueSoon:
		return "obligation.clock.due_soon"
	case domain.ClockStateDue:
		return "obligation.clock.due"
	case domain.ClockStateOverdue:
		return "obligation.clock.overdue"
	case domain.ClockStateFulfilled:
		return "obligation.clock.fulfilled"
	case domain.ClockStateWaived:
		return "obligation.clock.waived"
	case domain.ClockStateNotApplicable:
		return "obligation.clock.not_applicable"
	case domain.ClockStateSuperseded:
		return "obligation.clock.superseded"
	}
	return "obligation.clock." + strings.ToLower(string(to)) // unreachable: every state is named above
}

// computedEffectiveAt is the logical instant of a computed move: due_at for
// OVERDUE, the start of the due date for DUE, the warning instant for
// DUE_SOON. A late sweep records when the state began, not when it noticed.
func computedEffectiveAt(c *domain.Clock, to domain.ClockState, now time.Time) time.Time {
	switch to {
	case domain.ClockStateOverdue:
		return c.DueAt
	case domain.ClockStateDue:
		return c.DueDayStart()
	case domain.ClockStateDueSoon:
		if c.DueSoonAt != nil {
			return *c.DueSoonAt
		}
	}
	return now
}

// persistComputedTx writes the computed move of a running clock (if any) as
// a sweep transition, guarded on the state and row version it read (NP-37).
// It returns the clock as stored after the move.
func persistComputedTx(ctx context.Context, tx pgx.Tx, c *domain.Clock, now time.Time, correlationID string) (*domain.Clock, bool, error) {
	eff := domain.EffectiveState(c.StoredState, now, c.DueAt, c.DueDayStart(), c.DueSoonAt)
	if eff == c.StoredState {
		return c, false, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE obligation_clocks
		SET clock_state = $3, row_version = row_version + 1, updated_at = NOW(), updated_by = $4,
		    overdue_since = CASE WHEN $3 = 'OVERDUE' AND overdue_since IS NULL THEN due_at ELSE overdue_since END
		WHERE clock_id = $1 AND clock_state = $2 AND row_version = $5`,
		c.ClockID, string(c.StoredState), string(eff), ActorClockSweep, c.RowVersion)
	if err != nil {
		return nil, false, storeErr(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, false, domain.ErrClockVersionMismatch
	}
	from := c.StoredState
	next, err := scanClockRow(tx.QueryRow(ctx, `SELECT `+clockColumns+` FROM obligation_clocks WHERE clock_id = $1`, c.ClockID))
	if err != nil {
		return nil, false, err
	}
	at := computedEffectiveAt(next, eff, now)
	if err := insertClockTransitionTx(ctx, tx, next, &from, eff, domain.ClockCommandSweep, ActorClockSweep, nil, nil, nil, at, correlationID, ""); err != nil {
		return nil, false, err
	}
	if err := insertClockEventTx(ctx, tx, next, stateEvent(eff), ActorClockSweep, correlationID, "",
		map[string]any{"from_state": string(from), "to_state": string(eff), "effective_at": at.UTC().Format(time.RFC3339Nano)}); err != nil {
		return nil, false, err
	}
	return next, true, nil
}

func lockClockTx(ctx context.Context, tx pgx.Tx, clockID string) (*domain.Clock, error) {
	return scanClockRow(tx.QueryRow(ctx, `SELECT `+clockColumns+` FROM obligation_clocks WHERE clock_id = $1 FOR UPDATE`, clockID))
}

// ComputeClockRequestHash is the idempotency hash of a clock start request.
func ComputeClockRequestHash(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// StartClock inserts a clock from a jurisdiction calculation, with its
// milestones, its first transition and its event. It is idempotent on
// (tenant, clock_key): the same key and request returns the stored clock
// (created=false); the same key with a different request is
// ErrClockKeyConflict.
func (s *PgStore) StartClock(ctx context.Context, p domain.StartClockParams) (*domain.Clock, bool, error) {
	var out *domain.Clock
	created := false
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		c, err := s.startClockTx(ctx, tx, tenantID, p)
		if err != nil {
			return err
		}
		out, created = c.clock, c.created
		return nil
	})
	return out, created, err
}

type startResult struct {
	clock   *domain.Clock
	created bool
}

func (s *PgStore) startClockTx(ctx context.Context, tx pgx.Tx, tenantID string, p domain.StartClockParams) (*startResult, error) {
	if p.Calc.Outcome != domain.CalculationOutcomeCalculated || p.Calc.DueAt == nil {
		return nil, domain.ErrCalculationRefused
	}
	// Replay on the key.
	var existingHash *string
	var existingID string
	err := tx.QueryRow(ctx, `SELECT clock_id::text, request_hash FROM obligation_clocks WHERE clock_key = $1`, p.ClockKey).Scan(&existingID, &existingHash)
	switch {
	case err == nil:
		if existingHash == nil || *existingHash != p.RequestHash {
			return nil, domain.ErrClockKeyConflict
		}
		c, err := scanClockRow(tx.QueryRow(ctx, `SELECT `+clockColumns+` FROM obligation_clocks WHERE clock_id = $1`, existingID))
		if err != nil {
			return nil, err
		}
		return &startResult{clock: c}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, storeErr(err)
	}

	var legalEntityID string
	if err := tx.QueryRow(ctx, `SELECT legal_entity_id::text FROM obligations WHERE obligation_id = $1`, p.ObligationID).Scan(&legalEntityID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrObligationNotFound
		}
		return nil, storeErr(err)
	}

	state := domain.ClockStateActive
	if p.Planned {
		state = domain.ClockStatePlanned
	}
	var dueSoon any = "0 seconds"
	if p.DueSoonAt != nil && p.DueSoonAt.Before(*p.Calc.DueAt) {
		dueSoon = fmt.Sprintf("%d seconds", int64(p.Calc.DueAt.Sub(*p.DueSoonAt)/time.Second))
	}
	var latency *int64
	if !p.Trigger.EventTime.IsZero() {
		ms := time.Now().UTC().Sub(p.Trigger.EventTime).Milliseconds()
		latency = &ms
	}
	calcReq := []byte(p.CalculationRequest)
	if len(calcReq) == 0 {
		calcReq, _ = json.Marshal(map[string]any{"jurisdiction": p.JurisdictionCode, "obligation_code": p.RuleObligationCode})
	}
	explain := p.Calc.Explanation
	if len(explain) == 0 {
		explain = json.RawMessage("[]")
	}
	var policyVersion *int
	if p.EscalationPolicyID != nil {
		var v int
		var status string
		if err := tx.QueryRow(ctx, `SELECT version, status FROM escalation_policies WHERE escalation_policy_id = $1`, *p.EscalationPolicyID).Scan(&v, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, domain.ErrEscalationPolicyNotFound
			}
			return nil, storeErr(err)
		}
		if status != "ACTIVE" {
			return nil, domain.ErrEscalationPolicyNotFound
		}
		policyVersion = &v
	}

	clockID := uuid.NewString()
	_, err = tx.Exec(ctx, `INSERT INTO obligation_clocks (
		clock_id, clock_key, request_hash, calculation_request, obligation_id, tenant_id, legal_entity_id, clock_class, clock_state,
		jurisdiction_code, rule_obligation_code, obligation_rule_id, rule_version, rule_content_digest,
		calendar_code, calendar_version, calendar_version_id, timezone, business_day_adjustment,
		anchor_date, unadjusted_date, local_due_date, due_at, due_soon_threshold, extension_days_granted,
		pack_ref, pack_version, pack_version_id, calculation_ref, calculation_explanation,
		trigger_event_id, trigger_event_type, trigger_event_time, business_event_time,
		source_object_type, source_object_id, source_object_version, ingestion_latency_ms,
		supersedes_clock_id, supersession_reason, escalation_policy_id, escalation_policy_version,
		correlation_id, causation_id, created_by, updated_by, row_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9,
		        $10, $11, $12, $13, $14,
		        NULLIF($15, ''), NULLIF($16, 0), NULLIF($17, ''), $18, NULLIF($19, ''),
		        NULLIF($20, '')::date, NULLIF($21, '')::date, $22::date, $23, $24::interval, $25,
		        NULLIF($26, ''), NULLIF($27, ''), NULLIF($28, ''), $29, $30,
		        $31, $32, $33, $34,
		        $35, $36, $37, $38,
		        $39, $40, $41, $42,
		        NULLIF($43, ''), NULLIF($44, ''), $45, $45, 1)`,
		clockID, p.ClockKey, p.RequestHash, calcReq, p.ObligationID, tenantID, legalEntityID, string(p.ClockClass), string(state),
		p.JurisdictionCode, p.RuleObligationCode, p.Calc.ObligationRuleID, p.Calc.RuleVersion, p.Calc.ContentDigest,
		p.Calc.CalendarCode, p.Calc.CalendarVersion, p.Calc.CalendarVersionID, p.Calc.Timezone, p.Calc.BusinessDayAdjustment,
		p.Calc.AnchorDate, p.Calc.UnadjustedDate, p.Calc.DueDate, p.Calc.DueAt.UTC(), dueSoon, p.Calc.ExtensionGranted,
		p.Calc.PackRef, p.Calc.PackVersion, p.Calc.PackVersionID, p.Calc.DecisionID, []byte(explain),
		p.Trigger.EventID, p.Trigger.EventType, p.Trigger.EventTime.UTC(), p.Trigger.BusinessEventTime,
		p.Trigger.SourceObjectType, p.Trigger.SourceObjectID, p.Trigger.SourceObjectVersion, latency,
		p.SupersedesClockID, p.SupersessionReason, p.EscalationPolicyID, policyVersion,
		p.CorrelationID, p.CausationID, p.Actor)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, domain.ErrClockKeyConflict
		}
		return nil, storeErr(err)
	}

	// Milestones: the escalation policy's, then the request's own.
	specs := append([]domain.MilestoneSpec{}, p.ExtraMilestones...)
	var policyMilestones []domain.MilestoneSpec
	if p.EscalationPolicyID != nil {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT milestones FROM escalation_policies WHERE escalation_policy_id = $1`, *p.EscalationPolicyID).Scan(&raw); err != nil {
			return nil, storeErr(err)
		}
		if err := json.Unmarshal(raw, &policyMilestones); err != nil {
			return nil, fmt.Errorf("%w: escalation policy milestones: %v", domain.ErrStoreUnavailable, err)
		}
	}
	insertMilestone := func(m domain.MilestoneSpec, fromPolicy bool) error {
		params, _ := json.Marshal(m.Params)
		if m.Params == nil {
			params = []byte("{}")
		}
		var pid any
		var pver any
		if fromPolicy {
			pid, pver = *p.EscalationPolicyID, *policyVersion
		}
		_, err := tx.Exec(ctx, `INSERT INTO obligation_clock_milestones
			(tenant_id, clock_id, kind, offset_seconds, fire_at, action, action_params, escalation_policy_id, escalation_policy_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (clock_id, kind, offset_seconds, action) DO NOTHING`,
			tenantID, clockID, m.Kind, m.OffsetSeconds, p.Calc.DueAt.UTC().Add(time.Duration(m.OffsetSeconds)*time.Second),
			m.Action, params, pid, pver)
		return storeErr(err)
	}
	for _, m := range policyMilestones {
		if err := insertMilestone(m, true); err != nil {
			return nil, err
		}
	}
	for _, m := range specs {
		if err := insertMilestone(m, false); err != nil {
			return nil, err
		}
	}

	c, err := scanClockRow(tx.QueryRow(ctx, `SELECT `+clockColumns+` FROM obligation_clocks WHERE clock_id = $1`, clockID))
	if err != nil {
		return nil, err
	}
	effective := p.Trigger.EventTime
	if effective.IsZero() {
		effective = time.Now().UTC()
	}
	if err := insertClockTransitionTx(ctx, tx, c, nil, state, domain.ClockCommandStart, p.Actor, nil, nil, nil, effective, p.CorrelationID, p.CausationID); err != nil {
		return nil, err
	}
	payload := map[string]any{"state": string(state), "rule_obligation_code": c.RuleObligationCode, "trigger_event_id": c.Trigger.EventID}
	if c.SupersedesClockID != nil {
		payload["supersedes_clock_id"] = *c.SupersedesClockID
	}
	if err := insertClockEventTx(ctx, tx, c, "obligation.clock.started", p.Actor, p.CorrelationID, p.CausationID, payload); err != nil {
		return nil, err
	}
	return &startResult{clock: c, created: true}, nil
}

// GetClock reads one clock (effective state resolved at now).
func (s *PgStore) GetClock(ctx context.Context, clockID string) (*domain.Clock, error) {
	var c *domain.Clock
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		var err error
		c, err = scanClockRow(tx.QueryRow(ctx, `SELECT `+clockColumns+` FROM obligation_clocks WHERE clock_id = $1`, clockID))
		return err
	})
	return c, err
}

// ListClocks lists clocks, oldest first, keyset-paged on (created_at, clock_id).
func (s *PgStore) ListClocks(ctx context.Context, f domain.ListClocksFilter) ([]*domain.Clock, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	var out []*domain.Clock
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		q := `SELECT ` + clockColumns + ` FROM obligation_clocks WHERE TRUE`
		args := []any{}
		add := func(cond string, v any) {
			args = append(args, v)
			q += fmt.Sprintf(" AND "+cond, len(args))
		}
		if f.ObligationID != "" {
			add("obligation_id = $%d", f.ObligationID)
		}
		if f.ClockClass != "" {
			add("clock_class = $%d", string(f.ClockClass))
		}
		if f.StoredState != "" {
			add("clock_state = $%d", string(f.StoredState))
		}
		if f.AfterCreatedAt != nil {
			args = append(args, *f.AfterCreatedAt, f.AfterID)
			q += fmt.Sprintf(" AND (created_at, clock_id) > ($%d, $%d::uuid)", len(args)-1, len(args))
		}
		args = append(args, f.Limit)
		q += fmt.Sprintf(" ORDER BY created_at, clock_id LIMIT $%d", len(args))
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return storeErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanClockRow(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return storeErr(rows.Err())
	})
	return out, err
}

// LiveClocksForObligation returns the obligation's clocks that are not
// terminal (PLANNED or running).
func (s *PgStore) LiveClocksForObligation(ctx context.Context, obligationID string) ([]*domain.Clock, error) {
	var out []*domain.Clock
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, err := tx.Query(ctx, `SELECT `+clockColumns+` FROM obligation_clocks
			WHERE obligation_id = $1 AND clock_state IN ('PLANNED','ACTIVE','DUE_SOON','DUE','OVERDUE')
			ORDER BY created_at, clock_id`, obligationID)
		if err != nil {
			return storeErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanClockRow(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return storeErr(rows.Err())
	})
	return out, err
}

// ApplyClockCommand runs activate, satisfy, waive or not-applicable on one
// clock. A computed move the sweep has not persisted yet is written first,
// so the history shows the lateness before the fulfilment (NP-38).
func (s *PgStore) ApplyClockCommand(ctx context.Context, p domain.ClockCommandParams) (*domain.Clock, error) {
	var out *domain.Clock
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		c, err := lockClockTx(ctx, tx, p.ClockID)
		if err != nil {
			return err
		}
		if p.ExpectedVersion > 0 && c.RowVersion != p.ExpectedVersion {
			return domain.ErrClockVersionMismatch
		}
		now := p.Now
		if now.IsZero() {
			now = time.Now().UTC()
		}
		if !domain.ClockCommandAllowed(p.Command, domain.EffectiveState(c.StoredState, now, c.DueAt, c.DueDayStart(), c.DueSoonAt)) {
			return domain.ErrClockCommandNotAllowed
		}
		if c, _, err = persistComputedTx(ctx, tx, c, now, p.CorrelationID); err != nil {
			return err
		}
		from := c.StoredState
		var to domain.ClockState
		var reasonCode, reasonText *string
		var evidence []string
		switch p.Command {
		case domain.ClockCommandActivate:
			to = domain.ClockStateActive
			_, err = tx.Exec(ctx, `UPDATE obligation_clocks SET clock_state = 'ACTIVE', row_version = row_version + 1,
				updated_at = NOW(), updated_by = $2 WHERE clock_id = $1`, c.ClockID, p.Actor)
		case domain.ClockCommandSatisfy:
			to, evidence = domain.ClockStateFulfilled, p.EvidenceRefs
			_, err = tx.Exec(ctx, `UPDATE obligation_clocks SET clock_state = 'FULFILLED', row_version = row_version + 1,
				satisfied_at = $2, satisfied_by_principal_id = $3, evidence_refs = $4, evidence_exempt_reason = $5,
				evidence_evaluation_id = $6, evidence_outcome = $7, domain_confirmation_ref = $8, subject_state = $9,
				approval_reference = COALESCE($10, approval_reference),
				overdue_since = CASE WHEN overdue_since IS NULL AND $2 > due_at THEN due_at ELSE overdue_since END,
				updated_at = NOW(), updated_by = $3 WHERE clock_id = $1`,
				c.ClockID, now, p.Actor, nonNil(p.EvidenceRefs), p.EvidenceExemptReason, p.EvidenceEvaluationID, p.EvidenceOutcome,
				p.DomainConfirmationRef, p.SubjectState, p.ApprovalReference)
		case domain.ClockCommandWaive, domain.ClockCommandNotApplicable:
			to, reasonCode, reasonText, evidence = domain.ClockStateWaived, p.ReasonCode, p.ReasonText, p.EvidenceRefs
			action, stamp := "WAIVE", "waived_at"
			if p.Command == domain.ClockCommandNotApplicable {
				to, action, stamp = domain.ClockStateNotApplicable, "NOT_APPLICABLE", "not_applicable_at"
			}
			_, err = tx.Exec(ctx, `UPDATE obligation_clocks SET clock_state = $2, row_version = row_version + 1,
				authority_action = $3, requested_by = $4, approved_by = $5, approval_reference = $6, reason_code = $7,
				reason_text = $8, authority_evidence_refs = $9, applicability_decision_id = $10::uuid, `+stamp+` = $11,
				overdue_since = CASE WHEN overdue_since IS NULL AND $11 > due_at THEN due_at ELSE overdue_since END,
				updated_at = NOW(), updated_by = $4 WHERE clock_id = $1`,
				c.ClockID, string(to), action, p.Actor, p.ApprovedBy, p.ApprovalReference, p.ReasonCode, p.ReasonText,
				nonNil(p.EvidenceRefs), p.ApplicabilityDecisionID, now)
		default:
			return domain.ErrClockCommandNotAllowed
		}
		if err != nil {
			return storeErr(err)
		}
		next, err := scanClockRow(tx.QueryRow(ctx, `SELECT `+clockColumns+` FROM obligation_clocks WHERE clock_id = $1`, c.ClockID))
		if err != nil {
			return err
		}
		if err := insertClockTransitionTx(ctx, tx, next, &from, to, p.Command, p.Actor, reasonCode, reasonText, evidence, now, p.CorrelationID, ""); err != nil {
			return err
		}
		payload := map[string]any{"from_state": string(from), "to_state": string(to)}
		if next.OverdueSince != nil {
			payload["overdue_since"] = next.OverdueSince.UTC().Format(time.RFC3339Nano)
		}
		if next.ApprovalReference != nil {
			payload["approval_reference"] = *next.ApprovalReference
		}
		if next.ReasonCode != nil {
			payload["reason_code"] = *next.ReasonCode
		}
		if err := insertClockEventTx(ctx, tx, next, stateEvent(to), p.Actor, p.CorrelationID, "", payload); err != nil {
			return err
		}
		out = next
		return nil
	})
	return out, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// SupersedeClock marks a live clock SUPERSEDED and inserts its successor in
// the same transaction, linked both ways (R-INV-17, NP-14, NP-32). The old
// clock keeps its due_at. If the successor's due_at equals the old one there
// is nothing to supersede: the old clock is returned unchanged and
// superseded=false.
func (s *PgStore) SupersedeClock(ctx context.Context, oldClockID string, expectedVersion int, successor domain.StartClockParams) (old, next *domain.Clock, superseded bool, err error) {
	run := func(tx pgx.Tx, tenantID string) error {
		c, err := lockClockTx(ctx, tx, oldClockID)
		if err != nil {
			return err
		}
		if expectedVersion > 0 && c.RowVersion != expectedVersion {
			return domain.ErrClockVersionMismatch
		}
		now := time.Now().UTC()
		if !domain.ClockCommandAllowed(domain.ClockCommandRecalculate, domain.EffectiveState(c.StoredState, now, c.DueAt, c.DueDayStart(), c.DueSoonAt)) {
			return domain.ErrClockCommandNotAllowed
		}
		if successor.Calc.DueAt == nil || successor.Calc.Outcome != domain.CalculationOutcomeCalculated {
			return domain.ErrCalculationRefused
		}
		if successor.Calc.DueAt.Equal(c.DueAt) && sameCalendar(c, successor.Calc) {
			old = c
			return nil
		}
		if c, _, err = persistComputedTx(ctx, tx, c, now, successor.CorrelationID); err != nil {
			return err
		}
		successor.ObligationID = c.ObligationID
		successor.ClockClass = c.ClockClass
		successor.JurisdictionCode = c.JurisdictionCode
		successor.RuleObligationCode = c.RuleObligationCode
		successor.SupersedesClockID = &c.ClockID
		successor.Planned = c.StoredState == domain.ClockStatePlanned
		if successor.Trigger.EventID == "" {
			successor.Trigger = c.Trigger // the trigger is the original one; never erased (s7.1)
		}
		if successor.EscalationPolicyID == nil {
			successor.EscalationPolicyID = c.EscalationPolicyID
		}
		if successor.DueSoonAt == nil && c.DueSoonAt != nil {
			t := successor.Calc.DueAt.Add(-c.DueAt.Sub(*c.DueSoonAt))
			successor.DueSoonAt = &t
		}
		res, err := s.startClockTx(ctx, tx, tenantID, successor)
		if err != nil {
			return err
		}
		from := c.StoredState
		if _, err := tx.Exec(ctx, `UPDATE obligation_clocks SET clock_state = 'SUPERSEDED', row_version = row_version + 1,
			superseded_at = NOW(), superseded_by_clock_id = $2, supersession_reason = $3, updated_at = NOW(), updated_by = $4
			WHERE clock_id = $1`, c.ClockID, res.clock.ClockID, successor.SupersessionReason, successor.Actor); err != nil {
			return storeErr(err)
		}
		oc, err := scanClockRow(tx.QueryRow(ctx, `SELECT `+clockColumns+` FROM obligation_clocks WHERE clock_id = $1`, c.ClockID))
		if err != nil {
			return err
		}
		reason := "CALENDAR_CORRECTED"
		if successor.SupersessionReason != nil {
			reason = *successor.SupersessionReason
		}
		if err := insertClockTransitionTx(ctx, tx, oc, &from, domain.ClockStateSuperseded, domain.ClockCommandSupersede, successor.Actor,
			&reason, nil, nil, now, successor.CorrelationID, successor.CausationID); err != nil {
			return err
		}
		if err := insertClockEventTx(ctx, tx, oc, "obligation.clock.superseded", successor.Actor, successor.CorrelationID, successor.CausationID,
			map[string]any{"superseded_by_clock_id": res.clock.ClockID, "reason": reason, "previous_due_at": oc.DueAt.UTC().Format(time.RFC3339Nano),
				"new_due_at": res.clock.DueAt.UTC().Format(time.RFC3339Nano)}); err != nil {
			return err
		}
		old, next, superseded = oc, res.clock, true
		return nil
	}
	if successor.SystemTenantID != "" {
		err = s.withTenantIDTx(ctx, successor.SystemTenantID, func(tx pgx.Tx) error { return run(tx, successor.SystemTenantID) })
	} else {
		err = s.withTenantTx(ctx, run)
	}
	return old, next, superseded, err
}

func sameCalendar(c *domain.Clock, calc domain.Calculation) bool {
	got := ""
	if c.CalendarVersionID != nil {
		got = *c.CalendarVersionID
	}
	return got == calc.CalendarVersionID
}

// ListClockTransitions returns a clock's append-only history, oldest first.
func (s *PgStore) ListClockTransitions(ctx context.Context, clockID string) ([]*domain.ClockTransition, error) {
	var out []*domain.ClockTransition
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, err := tx.Query(ctx, `SELECT transition_id::text, clock_id::text, from_state, to_state, command, actor, reason_code, reason_text,
			evidence_refs, row_version, effective_at, recorded_at, correlation_id
			FROM obligation_clock_transitions WHERE clock_id = $1 ORDER BY row_version, recorded_at`, clockID)
		if err != nil {
			return storeErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.ClockTransition
			if err := rows.Scan(&t.TransitionID, &t.ClockID, &t.FromState, &t.ToState, &t.Command, &t.Actor, &t.ReasonCode, &t.ReasonText,
				&t.EvidenceRefs, &t.RowVersion, &t.EffectiveAt, &t.RecordedAt, &t.CorrelationID); err != nil {
				return storeErr(err)
			}
			if t.EvidenceRefs == nil {
				t.EvidenceRefs = []string{}
			}
			out = append(out, &t)
		}
		return storeErr(rows.Err())
	})
	return out, err
}

// ListClockMilestones returns a clock's warning and escalation schedule.
func (s *PgStore) ListClockMilestones(ctx context.Context, clockID string) ([]*domain.Milestone, error) {
	var out []*domain.Milestone
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, err := tx.Query(ctx, `SELECT milestone_id::text, clock_id::text, kind, offset_seconds, fire_at, action, action_params,
			escalation_policy_id::text, escalation_policy_version, fired_at
			FROM obligation_clock_milestones WHERE clock_id = $1 ORDER BY fire_at, milestone_id`, clockID)
		if err != nil {
			return storeErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.Milestone
			var params []byte
			if err := rows.Scan(&m.MilestoneID, &m.ClockID, &m.Kind, &m.OffsetSeconds, &m.FireAt, &m.Action, &params,
				&m.EscalationPolicyID, &m.EscalationPolicyVersion, &m.FiredAt); err != nil {
				return storeErr(err)
			}
			_ = json.Unmarshal(params, &m.Params)
			if m.Params == nil {
				m.Params = map[string]any{}
			}
			out = append(out, &m)
		}
		return storeErr(rows.Err())
	})
	return out, err
}

// ListEscalationEvents returns a clock's escalation record, oldest first.
func (s *PgStore) ListEscalationEvents(ctx context.Context, clockID string) ([]*domain.EscalationEvent, error) {
	var out []*domain.EscalationEvent
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, err := tx.Query(ctx, `SELECT escalation_event_id::text, clock_id::text, milestone_id::text, action, outcome, result_ref, detail, occurred_at
			FROM escalation_events WHERE clock_id = $1 ORDER BY occurred_at, escalation_event_id`, clockID)
		if err != nil {
			return storeErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.EscalationEvent
			var detail []byte
			if err := rows.Scan(&e.EscalationEventID, &e.ClockID, &e.MilestoneID, &e.Action, &e.Outcome, &e.ResultRef, &detail, &e.OccurredAt); err != nil {
				return storeErr(err)
			}
			_ = json.Unmarshal(detail, &e.Detail)
			if e.Detail == nil {
				e.Detail = map[string]any{}
			}
			out = append(out, &e)
		}
		return storeErr(rows.Err())
	})
	return out, err
}

// ── sweep ────────────────────────────────────────────────────────────────────

// WorkItem names one row a system worker acts on inside its tenant.
type WorkItem struct {
	TenantID string
	ID       string
}

// ClocksNeedingSweep lists running clocks whose computed state is ahead of
// their stored state at now, across tenants.
func (s *PgStore) ClocksNeedingSweep(ctx context.Context, now time.Time, limit int) ([]WorkItem, error) {
	if limit <= 0 {
		limit = 200
	}
	var out []WorkItem
	err := s.withSweepTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id::text, clock_id::text FROM obligation_clocks
			WHERE (clock_state IN ('ACTIVE','DUE_SOON','DUE') AND due_at < $1)
			   OR (clock_state IN ('ACTIVE','DUE_SOON') AND COALESCE(local_due_date, (due_at - INTERVAL '1 day')::date) <= ($1 AT TIME ZONE COALESCE(timezone, 'UTC'))::date)
			   OR (clock_state = 'ACTIVE' AND due_soon_threshold > INTERVAL '0' AND due_at - due_soon_threshold <= $1)
			ORDER BY due_at LIMIT $2`, now, limit)
		if err != nil {
			return storeErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var w WorkItem
			if err := rows.Scan(&w.TenantID, &w.ID); err != nil {
				return storeErr(err)
			}
			out = append(out, w)
		}
		return storeErr(rows.Err())
	})
	return out, err
}

// SweepClock persists one clock's computed move inside its tenant. It is a
// no-op when another worker already moved it (moved=false, no error).
func (s *PgStore) SweepClock(ctx context.Context, item WorkItem, now time.Time) (bool, error) {
	moved := false
	err := s.withTenantIDTx(ctx, item.TenantID, func(tx pgx.Tx) error {
		c, err := lockClockTx(ctx, tx, item.ID)
		if err != nil {
			return err
		}
		_, moved, err = persistComputedTx(ctx, tx, c, now, "sweep-"+c.ClockID)
		if errors.Is(err, domain.ErrClockVersionMismatch) {
			moved, err = false, nil
		}
		return err
	})
	return moved, err
}

// DueMilestones lists unfired milestones whose time has come, across tenants.
func (s *PgStore) DueMilestones(ctx context.Context, now time.Time, limit int) ([]WorkItem, error) {
	if limit <= 0 {
		limit = 200
	}
	var out []WorkItem
	err := s.withSweepTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id::text, milestone_id::text FROM obligation_clock_milestones
			WHERE fired_at IS NULL AND fire_at <= $1 ORDER BY fire_at LIMIT $2`, now, limit)
		if err != nil {
			return storeErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var w WorkItem
			if err := rows.Scan(&w.TenantID, &w.ID); err != nil {
				return storeErr(err)
			}
			out = append(out, w)
		}
		return storeErr(rows.Err())
	})
	return out, err
}

// FireMilestone fires one milestone exactly once (the UPDATE is guarded on
// fired_at IS NULL and the row is locked). Escalation raises priority, adds an
// owner, queues a workflow case, or emits obligation.escalated; it never
// changes the clock's state or due_at (R-001 s7.4, R-INV-19). A milestone of
// a terminal clock is marked fired and recorded as SKIPPED.
func (s *PgStore) FireMilestone(ctx context.Context, item WorkItem, now time.Time) (fired bool, err error) {
	err = s.withTenantIDTx(ctx, item.TenantID, func(tx pgx.Tx) error {
		var m domain.Milestone
		var params []byte
		err := tx.QueryRow(ctx, `SELECT milestone_id::text, clock_id::text, kind, offset_seconds, fire_at, action, action_params, fired_at
			FROM obligation_clock_milestones WHERE milestone_id = $1 FOR UPDATE`, item.ID).
			Scan(&m.MilestoneID, &m.ClockID, &m.Kind, &m.OffsetSeconds, &m.FireAt, &m.Action, &params, &m.FiredAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return storeErr(err)
		}
		if m.FiredAt != nil {
			return nil
		}
		_ = json.Unmarshal(params, &m.Params)
		if m.Params == nil {
			m.Params = map[string]any{}
		}
		tag, err := tx.Exec(ctx, `UPDATE obligation_clock_milestones SET fired_at = $2 WHERE milestone_id = $1 AND fired_at IS NULL`, m.MilestoneID, now)
		if err != nil {
			return storeErr(err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		fired = true
		c, err := lockClockTx(ctx, tx, m.ClockID)
		if err != nil {
			return err
		}
		corr := "milestone-" + m.MilestoneID
		record := func(outcome string, resultRef *string, detail map[string]any) error {
			raw, _ := json.Marshal(detail)
			_, err := tx.Exec(ctx, `INSERT INTO escalation_events (tenant_id, clock_id, milestone_id, action, outcome, result_ref, detail, correlation_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, c.TenantID, c.ClockID, m.MilestoneID, m.Action, outcome, resultRef, raw, corr)
			return storeErr(err)
		}
		if c.StoredState.Terminal() {
			return record("SKIPPED", nil, map[string]any{"reason": "clock is " + string(c.StoredState)})
		}
		if c, _, err = persistComputedTx(ctx, tx, c, now, corr); err != nil {
			return err
		}
		escalated := func(detail map[string]any) error {
			detail["action"] = m.Action
			detail["milestone_id"] = m.MilestoneID
			detail["kind"] = m.Kind
			detail["clock_state"] = string(c.StoredState)
			return insertClockEventTx(ctx, tx, c, "obligation.escalated", ActorEscalation, corr, "", detail)
		}
		switch m.Action {
		case domain.ActionMarkDueSoon:
			return record("APPLIED", nil, map[string]any{"clock_state": string(c.StoredState)})
		case domain.ActionRaisePriority:
			want, _ := m.Params["priority"].(string)
			if _, err := tx.Exec(ctx, `UPDATE obligation_clocks SET priority = CASE
					WHEN priority = 'CRITICAL' THEN priority WHEN $2 = 'CRITICAL' THEN 'CRITICAL' ELSE $2 END,
					row_version = row_version + 1, updated_at = NOW(), updated_by = $3 WHERE clock_id = $1`,
				c.ClockID, want, ActorEscalation); err != nil {
				return storeErr(err)
			}
			if err := escalated(map[string]any{"priority": want}); err != nil {
				return err
			}
			return record("APPLIED", nil, map[string]any{"priority": want})
		case domain.ActionAddOwner:
			owner, _ := m.Params["owner"].(string)
			if _, err := tx.Exec(ctx, `UPDATE obligation_clocks SET additional_owners =
					CASE WHEN $2 = ANY(additional_owners) THEN additional_owners ELSE array_append(additional_owners, $2) END,
					row_version = row_version + 1, updated_at = NOW(), updated_by = $3 WHERE clock_id = $1`,
				c.ClockID, owner, ActorEscalation); err != nil {
				return storeErr(err)
			}
			if err := escalated(map[string]any{"owner": owner}); err != nil {
				return err
			}
			return record("APPLIED", nil, map[string]any{"owner": owner})
		case domain.ActionOpenCase:
			req := CaseRequest{
				TenantID: c.TenantID, LegalEntityID: c.LegalEntityID, SourceType: "OBLIGATION_CLOCK", SourceID: c.ClockID,
				Milestone: m.MilestoneID, CaseFamily: "LEGAL_COMPLIANCE", Confidentiality: "RESTRICTED",
				Severity: severityFor(c), Title: fmt.Sprintf("Obligation deadline %s is %s", c.RuleObligationCode, c.StoredState),
				Description: fmt.Sprintf("Clock %s (%s) due %s; escalation milestone %s fired.", c.ClockID, c.ClockClass,
					c.DueAt.UTC().Format(time.RFC3339), m.MilestoneID),
				IdempotencyKey: "obligations:" + c.ClockID + ":" + m.MilestoneID,
			}
			raw, _ := json.Marshal(req)
			if _, err := tx.Exec(ctx, `INSERT INTO event_outbox (tenant_id, event_type, event_payload, correlation_id, destination)
				VALUES ($1, 'workflow.case.open_requested', $2, $3, 'WORKFLOW_CASE')`, c.TenantID, raw, corr); err != nil {
				return storeErr(err)
			}
			if err := escalated(map[string]any{"case_request_key": req.IdempotencyKey}); err != nil {
				return err
			}
			return record("REQUESTED", nil, map[string]any{"idempotency_key": req.IdempotencyKey})
		case domain.ActionNotify:
			detail := map[string]any{"notify": true}
			for k, v := range m.Params {
				detail[k] = v
			}
			if err := escalated(detail); err != nil {
				return err
			}
			return record("APPLIED", nil, map[string]any{"event": "obligation.escalated"})
		case domain.ActionRequireApproval:
			if err := escalated(map[string]any{"approval_required": true}); err != nil {
				return err
			}
			return record("REQUESTED", nil, map[string]any{"approval_required": true})
		}
		return record("FAILED", nil, map[string]any{"reason": "unknown action " + m.Action})
	})
	return fired, err
}

func severityFor(c *domain.Clock) string {
	switch {
	case c.ClockClass == domain.ClockClassHardLegal || c.ClockClass == domain.ClockClassRegulatory:
		return "HIGH"
	case c.Priority == "CRITICAL":
		return "HIGH"
	}
	return "MEDIUM"
}

// CaseRequest is the WORKFLOW_CASE outbox payload: the case workflow-svc
// opens for an escalation (WF-S7: idempotent on source and milestone).
type CaseRequest struct {
	TenantID        string `json:"tenant_id"`
	LegalEntityID   string `json:"legal_entity_id"`
	SourceType      string `json:"source_type"`
	SourceID        string `json:"source_id"`
	Milestone       string `json:"milestone"`
	CaseFamily      string `json:"case_family"`
	Confidentiality string `json:"confidentiality"`
	Severity        string `json:"severity"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	IdempotencyKey  string `json:"idempotency_key"`
}

// RecordCaseOpened records the workflow case an escalation opened.
func (s *PgStore) RecordCaseOpened(ctx context.Context, req CaseRequest, caseID string) error {
	return s.withTenantIDTx(ctx, req.TenantID, func(tx pgx.Tx) error {
		raw, _ := json.Marshal(map[string]any{"case_id": caseID, "idempotency_key": req.IdempotencyKey})
		_, err := tx.Exec(ctx, `INSERT INTO escalation_events (tenant_id, clock_id, milestone_id, action, outcome, result_ref, detail, correlation_id)
			VALUES ($1, $2, $3, 'OPEN_CASE', 'COMPLETED', $4, $5, $6)`, req.TenantID, req.SourceID, req.Milestone, caseID, raw, "milestone-"+req.Milestone)
		return storeErr(err)
	})
}

// RelayOutboxDestination is RelayOutbox for one destination.
func (s *PgStore) RelayOutboxDestination(ctx context.Context, destination string, limit int, deliver func(ctx context.Context, e OutboxEvent) error) (delivered, failed int, err error) {
	return s.relay(ctx, destination, limit, deliver)
}

// ── calendar corrections (OB-S8) ─────────────────────────────────────────────

// ClocksOnOlderCalendar lists live clocks computed on an older version of a
// calendar, across tenants.
func (s *PgStore) ClocksOnOlderCalendar(ctx context.Context, calendarCode string, version int, limit int) ([]WorkItem, error) {
	if limit <= 0 {
		limit = 500
	}
	var out []WorkItem
	err := s.withSweepTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id::text, clock_id::text FROM obligation_clocks
			WHERE calendar_code = $1 AND calendar_version < $2
			  AND clock_state IN ('PLANNED','ACTIVE','DUE_SOON','DUE','OVERDUE')
			ORDER BY tenant_id, created_at LIMIT $3`, calendarCode, version, limit)
		if err != nil {
			return storeErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			var w WorkItem
			if err := rows.Scan(&w.TenantID, &w.ID); err != nil {
				return storeErr(err)
			}
			out = append(out, w)
		}
		return storeErr(rows.Err())
	})
	return out, err
}

// GetClockForTenant reads a clock inside an explicit tenant (system workers).
func (s *PgStore) GetClockForTenant(ctx context.Context, tenantID, clockID string) (*domain.Clock, error) {
	var c *domain.Clock
	err := s.withTenantIDTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		c, err = scanClockRow(tx.QueryRow(ctx, `SELECT `+clockColumns+` FROM obligation_clocks WHERE clock_id = $1`, clockID))
		return err
	})
	return c, err
}

// ClockCalculationRequest returns the stored calculation facts of a clock.
func (s *PgStore) ClockCalculationRequest(ctx context.Context, tenantID, clockID string) (json.RawMessage, error) {
	var raw []byte
	run := func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT calculation_request FROM obligation_clocks WHERE clock_id = $1`, clockID).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrClockNotFound
		}
		return storeErr(err)
	}
	var err error
	if tenantID != "" {
		err = s.withTenantIDTx(ctx, tenantID, run)
	} else {
		err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error { return run(tx) })
	}
	return raw, err
}

// MarkEventConsumed records a consumed event id. first=false means it was
// already applied (replay).
func (s *PgStore) MarkEventConsumed(ctx context.Context, eventID, eventType string, outcome map[string]any) (bool, error) {
	first := false
	err := s.withSweepTx(ctx, func(tx pgx.Tx) error {
		raw, _ := json.Marshal(outcome)
		tag, err := tx.Exec(ctx, `INSERT INTO consumed_events (event_id, event_type, outcome) VALUES ($1, $2, $3)
			ON CONFLICT (event_id) DO NOTHING`, eventID, eventType, raw)
		if err != nil {
			return storeErr(err)
		}
		first = tag.RowsAffected() == 1
		return nil
	})
	return first, err
}

// EventConsumed reports whether an event id was already applied.
func (s *PgStore) EventConsumed(ctx context.Context, eventID string) (bool, error) {
	seen := false
	err := s.withSweepTx(ctx, func(tx pgx.Tx) error {
		return storeErr(tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM consumed_events WHERE event_id = $1)`, eventID).Scan(&seen))
	})
	return seen, err
}

// ── escalation policies ──────────────────────────────────────────────────────

// CreateEscalationPolicy creates the next version of a policy code (version 1
// for a new code). Earlier versions stay as they are.
func (s *PgStore) CreateEscalationPolicy(ctx context.Context, code string, milestones []domain.MilestoneSpec, actor, correlationID string) (*domain.EscalationPolicy, error) {
	var out *domain.EscalationPolicy
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		raw, _ := json.Marshal(milestones)
		var id string
		err := tx.QueryRow(ctx, `INSERT INTO escalation_policies (tenant_id, policy_code, version, milestones, created_by, correlation_id)
			SELECT $1, $2, COALESCE(MAX(version), 0) + 1, $3, $4, NULLIF($5, '') FROM escalation_policies WHERE policy_code = $2
			RETURNING escalation_policy_id::text`, tenantID, code, raw, actor, correlationID).Scan(&id)
		if err != nil {
			return storeErr(err)
		}
		out, err = scanPolicy(tx.QueryRow(ctx, policySelect+` WHERE escalation_policy_id = $1`, id))
		return err
	})
	return out, err
}

const policySelect = `SELECT escalation_policy_id::text, policy_code, version, status, milestones, effective_from, effective_to,
	created_by, created_at, row_version FROM escalation_policies`

func scanPolicy(row pgx.Row) (*domain.EscalationPolicy, error) {
	var p domain.EscalationPolicy
	var raw []byte
	if err := row.Scan(&p.EscalationPolicyID, &p.PolicyCode, &p.Version, &p.Status, &raw, &p.EffectiveFrom, &p.EffectiveTo,
		&p.CreatedBy, &p.CreatedAt, &p.RowVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrEscalationPolicyNotFound
		}
		return nil, storeErr(err)
	}
	_ = json.Unmarshal(raw, &p.Milestones)
	if p.Milestones == nil {
		p.Milestones = []domain.MilestoneSpec{}
	}
	return &p, nil
}

// GetEscalationPolicy reads one policy version.
func (s *PgStore) GetEscalationPolicy(ctx context.Context, id string) (*domain.EscalationPolicy, error) {
	var out *domain.EscalationPolicy
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		var err error
		out, err = scanPolicy(tx.QueryRow(ctx, policySelect+` WHERE escalation_policy_id = $1`, id))
		return err
	})
	return out, err
}

// ListEscalationPolicies lists policy versions, by code then version.
func (s *PgStore) ListEscalationPolicies(ctx context.Context, code string, limit int, afterCode string, afterVersion int) ([]*domain.EscalationPolicy, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []*domain.EscalationPolicy
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, err := tx.Query(ctx, policySelect+` WHERE ($1 = '' OR policy_code = $1) AND (policy_code, version) > ($2, $3)
			ORDER BY policy_code, version LIMIT $4`, code, afterCode, afterVersion, limit)
		if err != nil {
			return storeErr(err)
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPolicy(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return storeErr(rows.Err())
	})
	return out, err
}

// RetireEscalationPolicy retires a policy version (If-Match on row_version).
// Clocks already on it keep their milestones.
func (s *PgStore) RetireEscalationPolicy(ctx context.Context, id string, expectedVersion int, actor string) (*domain.EscalationPolicy, error) {
	var out *domain.EscalationPolicy
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		p, err := scanPolicy(tx.QueryRow(ctx, policySelect+` WHERE escalation_policy_id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if expectedVersion > 0 && p.RowVersion != expectedVersion {
			return domain.ErrClockVersionMismatch
		}
		if p.Status != "RETIRED" {
			if _, err := tx.Exec(ctx, `UPDATE escalation_policies SET status = 'RETIRED', effective_to = NOW(), row_version = row_version + 1
				WHERE escalation_policy_id = $1`, id); err != nil {
				return storeErr(err)
			}
		}
		out, err = scanPolicy(tx.QueryRow(ctx, policySelect+` WHERE escalation_policy_id = $1`, id))
		return err
	})
	return out, err
}

// ── summary (console KPIs) ───────────────────────────────────────────────────

// Summary computes the obligations console KPIs at now, in the caller's tenant.
func (s *PgStore) Summary(ctx context.Context, now time.Time) (*domain.ObligationSummary, error) {
	out := &domain.ObligationSummary{AsOf: now, ObligationsByStatus: map[string]int{}, ObligationsBySource: map[string]int{},
		ClocksByState: map[string]int{}, ClocksByClass: map[string]int{}}
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, err := tx.Query(ctx, `SELECT obligation_status, obligation_source_type, COUNT(*) FROM obligations GROUP BY 1, 2`)
		if err != nil {
			return storeErr(err)
		}
		for rows.Next() {
			var st, src string
			var n int
			if err := rows.Scan(&st, &src, &n); err != nil {
				rows.Close()
				return storeErr(err)
			}
			out.ObligationsByStatus[st] += n
			out.ObligationsBySource[src] += n
			if st != "CLOSED" {
				out.ApplicableObligations += n
			}
		}
		rows.Close()
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM obligations WHERE obligation_status <> 'CLOSED' AND due_date >= $1::date AND due_date < ($1 + INTERVAL '30 days')::date`, now).
			Scan(&out.DueWithin30Days); err != nil {
			return storeErr(err)
		}
		crow, err := tx.Query(ctx, `SELECT `+clockColumns+` FROM obligation_clocks`)
		if err != nil {
			return storeErr(err)
		}
		defer crow.Close()
		for crow.Next() {
			c, err := scanClockRow(crow)
			if err != nil {
				return err
			}
			c.Resolve(now)
			out.ClocksByState[string(c.ClockState)]++
			out.ClocksByClass[string(c.ClockClass)]++
			if c.ClockState == domain.ClockStateOverdue {
				out.OverdueClocks++
			}
			if c.ClockState == domain.ClockStateFulfilled {
				out.FulfilledClocks++
				if len(c.EvidenceRefs) > 0 {
					out.FulfilledWithEvidence++
				}
				if c.OverdueSince != nil {
					out.FulfilledLate++
				}
			}
		}
		return storeErr(crow.Err())
	})
	// Waiver / N/A requests awaiting approval live in workflow-svc; this
	// service cannot count them, so the figure is reported as unavailable.
	out.AwaitingReviewNote = "waiver and not-applicable approvals are pending in workflow-svc (workflow_type OBLIGATION_WAIVER); not counted here"
	return out, err
}

// clockKeyFromHeader normalises an Idempotency-Key into a clock key.
func ClockKeyFromHeader(key string) string { return strings.TrimSpace(key) }

// GetApplicabilityDecision reads one applicability decision by id.
func (s *PgStore) GetApplicabilityDecision(ctx context.Context, id string) (*domain.ApplicabilityDecision, error) {
	var d domain.ApplicabilityDecision
	err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		err := tx.QueryRow(ctx, `SELECT applicability_decision_id::text, obligation_id::text, decision
			FROM applicability_decisions WHERE applicability_decision_id = $1`, id).Scan(&d.ApplicabilityDecisionID, &d.ObligationID, &d.Decision)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrApplicabilityMismatch
		}
		return storeErr(err)
	})
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// OverdueCount is one cell of the overdue-clock gauge.
type OverdueCount struct {
	ClockClass   string
	Jurisdiction string
	Count        int
}

// ClockStats reads, across tenants, the running clocks past due_at by class
// and jurisdiction, and the age of the oldest one whose OVERDUE state is not
// yet persisted (the sweep lag).
func (s *PgStore) ClockStats(ctx context.Context, now time.Time) ([]OverdueCount, float64, error) {
	var out []OverdueCount
	var lag float64
	err := s.withSweepTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT clock_class, COALESCE(jurisdiction_code, ''), COUNT(*) FROM obligation_clocks
			WHERE clock_state IN ('ACTIVE','DUE_SOON','DUE','OVERDUE') AND due_at < $1 GROUP BY 1, 2`, now)
		if err != nil {
			return storeErr(err)
		}
		for rows.Next() {
			var c OverdueCount
			if err := rows.Scan(&c.ClockClass, &c.Jurisdiction, &c.Count); err != nil {
				rows.Close()
				return storeErr(err)
			}
			out = append(out, c)
		}
		rows.Close()
		return storeErr(tx.QueryRow(ctx, `SELECT COALESCE(EXTRACT(EPOCH FROM $1 - MIN(due_at)), 0) FROM obligation_clocks
			WHERE clock_state IN ('ACTIVE','DUE_SOON','DUE') AND due_at < $1`, now).Scan(&lag))
	})
	return out, lag, err
}
