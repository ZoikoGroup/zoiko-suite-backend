// COM-04 Usage Metering persistence (migration 000011).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/outbox"
)

// UsageStore is the COM-04 persistence contract.
type UsageStore interface {
	RegisterMeterDefinition(ctx context.Context, m *domain.MeterDefinition, claim domain.IdempotencyClaim) (*domain.MeterDefinition, error)
	RetireMeterDefinition(ctx context.Context, meterKey string, version int, actor, reason string, now time.Time) (*domain.MeterDefinition, error)
	GetMeterDefinition(ctx context.Context, meterKey string, version int) (*domain.MeterDefinition, error)
	ListMeterDefinitions(ctx context.Context) ([]domain.MeterDefinition, error)

	RegisterUsageEvent(ctx context.Context, organizationID, subscriptionID, meterKey string, version int,
		usageEventID string, in domain.EventInput, sourceService string, observedAt time.Time, claim domain.IdempotencyClaim) (*domain.UsageEventRecord, error)
	CorrectUsageEvent(ctx context.Context, meterKey, originalEventID, newEventID string, in domain.EventInput,
		reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.UsageEventRecord, error)

	CloseUsageWindow(ctx context.Context, subscriptionID string, termNo int, meterKey string, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.UsageStatement, error)
	CertifyUsageStatement(ctx context.Context, statementID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.UsageStatement, error)
	ReopenWindow(ctx context.Context, statementID, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.UsageStatement, error)
	CreateUsageAdjustment(ctx context.Context, targetStatementID, meterKey, sourceUsageEventID, reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.UsageAdjustment, error)

	GetUsage(ctx context.Context, subscriptionID string, termNo int, meterKey string) (*domain.UsageStatement, error)
	GetUsageStatement(ctx context.Context, statementID string) (*domain.UsageStatement, error)
	ExplainAggregation(ctx context.Context, statementID string) ([]domain.UsageEventRecord, error)
	GetLateEvents(ctx context.Context, subscriptionID string) ([]domain.UsageEventRecord, error)
	GetDedupStatus(ctx context.Context, meterKey, usageEventID string) (*domain.UsageEventRecord, error)
}

var _ UsageStore = (*PgStore)(nil)

func mapUsageErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case pgErr.Code == "CP001":
		return fmt.Errorf("%w: %s", domain.ErrStatementInvalidState, pgErr.Message)
	case pgErr.Code == "23505" && pgErr.ConstraintName == "meter_definitions_pkey":
		return domain.ErrMeterDefinitionExists
	case pgErr.Code == "23505" && pgErr.ConstraintName == "usage_event_records_pkey":
		return domain.ErrUsageEventExists
	case pgErr.Code == "23503":
		return fmt.Errorf("%w: %s", domain.ErrMeterDefinitionNotFound, pgErr.Message)
	}
	return err
}

func (s *PgStore) usageTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return mapUsageErr(s.withSellerPlane(ctx, fn))
}

// ── Meter definitions ────────────────────────────────────────────────────────

const meterDefinitionColumns = `meter_key, meter_version, display_name, unit, aggregation_method, unique_dimension,
	allow_negative_correction, created_at, created_by_principal_id, retired_at, retired_by_principal_id, retire_reason`

func scanMeterDefinition(row pgx.Row) (*domain.MeterDefinition, error) {
	var m domain.MeterDefinition
	if err := row.Scan(&m.MeterKey, &m.MeterVersion, &m.DisplayName, &m.Unit, &m.AggregationMethod, &m.UniqueDimension,
		&m.AllowNegativeCorrection, &m.CreatedAt, &m.CreatedByPrincipalID, &m.RetiredAt, &m.RetiredByPrincipalID, &m.RetireReason); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *PgStore) RegisterMeterDefinition(ctx context.Context, m *domain.MeterDefinition, claim domain.IdempotencyClaim) (*domain.MeterDefinition, error) {
	var out *domain.MeterDefinition
	err := s.usageTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := scanMeterDefinition(tx.QueryRow(ctx, `
			INSERT INTO meter_definitions (meter_key, meter_version, display_name, unit, aggregation_method,
				unique_dimension, allow_negative_correction, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+meterDefinitionColumns,
			m.MeterKey, m.MeterVersion, m.DisplayName, m.Unit, m.AggregationMethod, m.UniqueDimension,
			m.AllowNegativeCorrection, m.CreatedAt, m.CreatedByPrincipalID))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "meter_definition",
			AggregateID: got.MeterKey + "/" + fmt.Sprint(got.MeterVersion), EventType: "meter_definition.registered", Payload: got})
	})
	return out, err
}

func (s *PgStore) RetireMeterDefinition(ctx context.Context, meterKey string, version int, actor, reason string, now time.Time) (*domain.MeterDefinition, error) {
	var out *domain.MeterDefinition
	err := s.usageTx(ctx, func(tx pgx.Tx) error {
		cur, err := scanMeterDefinition(tx.QueryRow(ctx, `SELECT `+meterDefinitionColumns+` FROM meter_definitions
			WHERE meter_key = $1 AND meter_version = $2 FOR UPDATE`, meterKey, version))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrMeterDefinitionNotFound
		}
		if err != nil {
			return err
		}
		if cur.RetiredAt != nil {
			return domain.ErrMeterDefinitionRetired
		}
		if _, err := tx.Exec(ctx, `UPDATE meter_definitions SET retired_at = $3, retired_by_principal_id = $4, retire_reason = $5
			WHERE meter_key = $1 AND meter_version = $2`, meterKey, version, now, actor, reason); err != nil {
			return err
		}
		got, err := scanMeterDefinition(tx.QueryRow(ctx, `SELECT `+meterDefinitionColumns+` FROM meter_definitions
			WHERE meter_key = $1 AND meter_version = $2`, meterKey, version))
		out = got
		return err
	})
	return out, err
}

func (s *PgStore) GetMeterDefinition(ctx context.Context, meterKey string, version int) (*domain.MeterDefinition, error) {
	var out *domain.MeterDefinition
	err := s.catalogTx(ctx, false, func(tx pgx.Tx) error {
		m, err := scanMeterDefinition(tx.QueryRow(ctx, `SELECT `+meterDefinitionColumns+` FROM meter_definitions
			WHERE meter_key = $1 AND meter_version = $2`, meterKey, version))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrMeterDefinitionNotFound
		}
		out = m
		return err
	})
	return out, err
}

func (s *PgStore) ListMeterDefinitions(ctx context.Context) ([]domain.MeterDefinition, error) {
	var out []domain.MeterDefinition
	err := s.catalogTx(ctx, false, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+meterDefinitionColumns+` FROM meter_definitions ORDER BY meter_key, meter_version`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMeterDefinition(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	return out, err
}

// loadRegisteredMeters answers CheckSubmittable's question: of the meters a
// price version's METERED components reference, which are registered and
// not retired right now? Read on the seller plane (meter_definitions has no
// tenant dimension), so it works inside the price book's own seller-plane
// transaction without needing a second one.
func loadRegisteredMeters(ctx context.Context, tx pgx.Tx, components []domain.PriceComponent) (map[string]bool, error) {
	out := map[string]bool{}
	for _, c := range components {
		if c.ComponentType != domain.ComponentMetered || c.MeterKey == nil || c.MeterVersion == nil {
			continue
		}
		var retiredAt *time.Time
		err := tx.QueryRow(ctx, `SELECT retired_at FROM meter_definitions WHERE meter_key = $1 AND meter_version = $2`,
			*c.MeterKey, *c.MeterVersion).Scan(&retiredAt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if retiredAt == nil {
			out[domain.RegisteredMeterKey(*c.MeterKey, *c.MeterVersion)] = true
		}
	}
	return out, nil
}

// loadRegisteredMetersForCapabilities is loadRegisteredMeters' counterpart
// for a price version's plan capabilities: of the meters a metered
// capability's limit references, which are registered and not retired?
func loadRegisteredMetersForCapabilities(ctx context.Context, tx pgx.Tx, caps []domain.PlanCapability) (map[string]bool, error) {
	out := map[string]bool{}
	for _, c := range caps {
		if c.MeterKey == nil || c.MeterVersion == nil {
			continue
		}
		var retiredAt *time.Time
		err := tx.QueryRow(ctx, `SELECT retired_at FROM meter_definitions WHERE meter_key = $1 AND meter_version = $2`,
			*c.MeterKey, *c.MeterVersion).Scan(&retiredAt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if retiredAt == nil {
			out[domain.RegisteredMeterKey(*c.MeterKey, *c.MeterVersion)] = true
		}
	}
	return out, nil
}

func loadMeterDefinition(ctx context.Context, tx pgx.Tx, meterKey string, version int) (*domain.MeterDefinition, error) {
	m, err := scanMeterDefinition(tx.QueryRow(ctx, `SELECT `+meterDefinitionColumns+` FROM meter_definitions
		WHERE meter_key = $1 AND meter_version = $2`, meterKey, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMeterDefinitionNotFound
	}
	return m, err
}

// ── Event scanning helpers ───────────────────────────────────────────────────

const usageEventColumns = `meter_key, usage_event_id, meter_version, organization_id::text, subscription_id, term_no,
	statement_id, quantity::text, dimensions, occurred_at, observed_at, source_service, status, quarantine_reason,
	late, superseded_by_event_id, created_at`

func scanUsageEvent(row pgx.Row) (*domain.UsageEventRecord, error) {
	var e domain.UsageEventRecord
	var dimJSON []byte
	if err := row.Scan(&e.MeterKey, &e.UsageEventID, &e.MeterVersion, &e.OrganizationID, &e.SubscriptionID, &e.TermNo,
		&e.StatementID, &e.Quantity, &dimJSON, &e.OccurredAt, &e.ObservedAt, &e.SourceService, &e.Status,
		&e.QuarantineReason, &e.Late, &e.SupersededByEventID, &e.CreatedAt); err != nil {
		return nil, err
	}
	if len(dimJSON) > 0 {
		if err := json.Unmarshal(dimJSON, &e.Dimensions); err != nil {
			return nil, fmt.Errorf("decode dimensions: %w", err)
		}
	}
	return &e, nil
}

// declareOrgForSellerPlane additionally sets app.tenant_id inside a
// transaction that already declared the seller plane (commercial_plane =
// 'seller'). subscription_terms/subscriptions carry no seller-plane RLS
// bypass of their own — they were built purely tenant-scoped (migration
// 000007) — so a seller-plane transaction that needs to read a named
// organization's terms (resolveTerm) must also declare that organization as
// its tenant scope for the SELECT to see anything. Usage/meter tables do
// have a seller-plane bypass and do not need this.
func declareOrgForSellerPlane(ctx context.Context, tx pgx.Tx, organizationID string) error {
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", organizationID)
	return err
}

// resolveTerm finds the subscription term whose [starts_at, ends_at) window
// contains at. The caller must have already declared the subscription's
// organization via declareOrgForSellerPlane in this transaction.
func resolveTerm(ctx context.Context, tx pgx.Tx, subscriptionID string, at time.Time) (int, error) {
	var termNo int
	err := tx.QueryRow(ctx, `SELECT term_no FROM subscription_terms
		WHERE subscription_id = $1 AND voided_at IS NULL AND starts_at <= $2 AND ends_at > $2`, subscriptionID, at).Scan(&termNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, domain.ErrStatementNotOpenForWindow
	}
	return termNo, err
}

// findOrCreateOpenStatement returns the OPEN statement for this window,
// creating it on first use — a statement is never pre-created for a window
// nothing has reported into yet.
func findOrCreateOpenStatement(ctx context.Context, tx pgx.Tx, subscriptionID string, termNo int, m domain.MeterDefinition, now time.Time) (*domain.UsageStatement, error) {
	st, err := scanStatement(tx.QueryRow(ctx, `SELECT `+usageStatementColumns+` FROM usage_statements
		WHERE subscription_id = $1 AND term_no = $2 AND meter_key = $3 AND status <> 'SUPERSEDED'`, subscriptionID, termNo, m.MeterKey))
	if err == nil {
		if st.Status != domain.StatementOpen {
			return nil, fmt.Errorf("%w: statement for this window is %s", domain.ErrStatementInvalidState, st.Status)
		}
		return st, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	id := domain.NewCommercialID(domain.PrefixUsageStatement)
	if _, err := tx.Exec(ctx, `INSERT INTO usage_statements (statement_id, subscription_id, term_no, meter_key,
		meter_version, status, created_at) VALUES ($1, $2, $3, $4, $5, 'OPEN', $6)`,
		id, subscriptionID, termNo, m.MeterKey, m.MeterVersion, now); err != nil {
		return nil, err
	}
	return scanStatement(tx.QueryRow(ctx, `SELECT `+usageStatementColumns+` FROM usage_statements WHERE statement_id = $1`, id))
}

// ── Ingestion ────────────────────────────────────────────────────────────────

// RegisterUsageEvent is the one intake path. It always persists an outcome
// (ACCEPTED or QUARANTINED — never silently drops an attempt), and never
// mutates a statement that is not OPEN: an event whose window has already
// been frozen or certified is accepted as evidence but recorded as late,
// with its quantity carried forward as an adjustment instead of counted
// into the closed population (COM-CTRL-017; negative path #18).
func (s *PgStore) RegisterUsageEvent(ctx context.Context, organizationID, subscriptionID, meterKey string, version int,
	usageEventID string, in domain.EventInput, sourceService string, observedAt time.Time, claim domain.IdempotencyClaim) (*domain.UsageEventRecord, error) {
	var out *domain.UsageEventRecord
	err := s.usageTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if err := declareOrgForSellerPlane(ctx, tx, organizationID); err != nil {
			return err
		}
		m, err := loadMeterDefinition(ctx, tx, meterKey, version)
		if err != nil {
			return err
		}
		if m.RetiredAt != nil {
			return fmt.Errorf("%w: cannot register usage against a retired meter version", domain.ErrMeterDefinitionRetired)
		}
		termNo, err := resolveTerm(ctx, tx, subscriptionID, in.OccurredAt)
		if err != nil {
			return err
		}

		reason := domain.ValidateUsageQuantity(*m, in)
		dimJSON, err := json.Marshal(in.Dimensions)
		if err != nil {
			return fmt.Errorf("marshal dimensions: %w", err)
		}

		if reason != "" {
			// Linked to the window it would have belonged to, so the
			// window's quarantined_count is real evidence (Completeness:
			// accepted+quarantined = everything ingested) — but only while
			// that window is still OPEN; a quarantined late arrival was
			// never going to count either way, so it is recorded unlinked.
			var statementID *string
			if st, serr := findOrCreateOpenStatement(ctx, tx, subscriptionID, termNo, *m, observedAt); serr == nil {
				statementID = &st.StatementID
			} else if !errors.Is(serr, domain.ErrStatementInvalidState) {
				return serr
			}
			e, err := scanUsageEvent(tx.QueryRow(ctx, `
				INSERT INTO usage_event_records (meter_key, usage_event_id, meter_version, organization_id,
					subscription_id, term_no, statement_id, quantity, dimensions, occurred_at, observed_at,
					source_service, status, quarantine_reason, late, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11, $12, 'QUARANTINED', $13, FALSE, $11)
				RETURNING `+usageEventColumns,
				meterKey, usageEventID, version, organizationID, subscriptionID, termNo, statementID, in.Quantity,
				dimJSON, in.OccurredAt, observedAt, sourceService, reason))
			if err != nil {
				return err
			}
			if statementID != nil {
				if err := bumpStatement(ctx, tx, statementID, in.Quantity, in.OccurredAt, false); err != nil {
					return err
				}
			}
			out = e
			return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "usage_event", AggregateID: meterKey + "/" + usageEventID,
				EventType: "usage.quarantined", TenantID: &organizationID, Payload: e})
		}

		st, err := findOrCreateOpenStatement(ctx, tx, subscriptionID, termNo, *m, observedAt)
		if err != nil && !errors.Is(err, domain.ErrStatementInvalidState) {
			return err
		}
		if err != nil {
			// The window this event belongs to is already frozen/certified:
			// route it as a late adjustment instead of failing the request.
			e, aerr := s.registerLateEvent(ctx, tx, organizationID, subscriptionID, termNo, *m, usageEventID, in,
				dimJSON, observedAt, sourceService, "usage arrived after its window was frozen")
			out = e
			return aerr
		}

		e, err := scanUsageEvent(tx.QueryRow(ctx, `
			INSERT INTO usage_event_records (meter_key, usage_event_id, meter_version, organization_id,
				subscription_id, term_no, statement_id, quantity, dimensions, occurred_at, observed_at,
				source_service, status, late, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11, $12, 'ACCEPTED', FALSE, $11)
			RETURNING `+usageEventColumns,
			meterKey, usageEventID, version, organizationID, subscriptionID, termNo, st.StatementID, in.Quantity,
			dimJSON, in.OccurredAt, observedAt, sourceService))
		if err != nil {
			return err
		}
		if err := bumpStatement(ctx, tx, &st.StatementID, in.Quantity, in.OccurredAt, true); err != nil {
			return err
		}
		out = e
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "usage_event", AggregateID: meterKey + "/" + usageEventID,
			EventType: "usage.accepted", TenantID: &organizationID, Payload: e})
	})
	return out, err
}

// bumpStatement advances an OPEN statement's running accumulators. It never
// touches total_quantity's meaning across aggregation methods here — the
// running counter is a live SUM-style count of accepted events and their
// most recent watermark; the authoritative total for any method is always
// recomputed from usage_event_records at CloseUsageWindow via
// domain.Aggregate, which is what CertifyUsageStatement and
// ExplainAggregation both read back. This counter exists only so
// GetUsageStatement can show a live running count before the window closes.
func bumpStatement(ctx context.Context, tx pgx.Tx, statementID *string, quantity string, occurredAt time.Time, accepted bool) error {
	if statementID == nil {
		return nil
	}
	if accepted {
		_, err := tx.Exec(ctx, `UPDATE usage_statements SET event_count = event_count + 1,
			watermark_at = GREATEST(COALESCE(watermark_at, $2), $2) WHERE statement_id = $1`, *statementID, occurredAt)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE usage_statements SET quarantined_count = quarantined_count + 1 WHERE statement_id = $1`, *statementID)
	return err
}

// registerLateEvent persists the late attempt (statement_id NULL, late=TRUE)
// and creates the adjustment that carries its quantity into the current
// open window for the same subscription+meter, or into a freshly opened one
// resolved from the subscription's term at the caller's now (observedAt) if
// none is open yet.
func (s *PgStore) registerLateEvent(ctx context.Context, tx pgx.Tx, organizationID, subscriptionID string, originTermNo int,
	m domain.MeterDefinition, usageEventID string, in domain.EventInput, dimJSON []byte, observedAt time.Time, sourceService, reasonPrefix string) (*domain.UsageEventRecord, error) {
	origin, err := scanStatement(tx.QueryRow(ctx, `SELECT `+usageStatementColumns+` FROM usage_statements
		WHERE subscription_id = $1 AND term_no = $2 AND meter_key = $3`, subscriptionID, originTermNo, m.MeterKey))
	if err != nil {
		return nil, err
	}
	e, err := scanUsageEvent(tx.QueryRow(ctx, `
		INSERT INTO usage_event_records (meter_key, usage_event_id, meter_version, organization_id, subscription_id,
			term_no, statement_id, quantity, dimensions, occurred_at, observed_at, source_service, status, late, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULL, $7::numeric, $8, $9, $10, $11, 'ACCEPTED', TRUE, $10)
		RETURNING `+usageEventColumns,
		m.MeterKey, usageEventID, m.MeterVersion, organizationID, subscriptionID, originTermNo, in.Quantity, dimJSON,
		in.OccurredAt, observedAt, sourceService))
	if err != nil {
		return nil, err
	}

	nowTermNo, terr := resolveTerm(ctx, tx, subscriptionID, observedAt)
	if terr != nil {
		return e, fmt.Errorf("%w: %v", domain.ErrNoOpenTermForCorrection, terr)
	}
	target, err := findOrCreateOpenStatement(ctx, tx, subscriptionID, nowTermNo, m, observedAt)
	if err != nil {
		return e, err
	}
	adjID := domain.NewCommercialID(domain.PrefixUsageAdjustment)
	if _, err := tx.Exec(ctx, `INSERT INTO usage_adjustments (adjustment_id, origin_statement_id, target_statement_id,
		meter_key, source_usage_event_id, quantity, occurred_at, dimensions, reason, created_at, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9, $10, $11)`,
		adjID, origin.StatementID, target.StatementID, m.MeterKey, usageEventID, in.Quantity,
		in.OccurredAt, dimJSON, reasonPrefix, observedAt, "system:usage-ingest"); err != nil {
		return e, err
	}
	if err := bumpStatement(ctx, tx, &target.StatementID, in.Quantity, observedAt, true); err != nil {
		return e, err
	}
	return e, outbox.Insert(ctx, tx, outbox.Event{AggregateType: "usage_event", AggregateID: m.MeterKey + "/" + usageEventID,
		EventType: "usage.late_adjustment_created", TenantID: &organizationID,
		Payload: map[string]any{"adjustment_id": adjID, "origin_statement_id": origin.StatementID, "target_statement_id": target.StatementID}})
}

const usageAdjustmentColumns = `adjustment_id, origin_statement_id, target_statement_id, meter_key, source_usage_event_id,
	quantity::text, occurred_at, dimensions, reason, created_at, created_by_principal_id`

func scanUsageAdjustment(row pgx.Row) (*domain.UsageAdjustment, error) {
	var a domain.UsageAdjustment
	var dimJSON []byte
	if err := row.Scan(&a.AdjustmentID, &a.OriginStatementID, &a.TargetStatementID, &a.MeterKey, &a.SourceUsageEventID,
		&a.Quantity, &a.OccurredAt, &dimJSON, &a.Reason, &a.CreatedAt, &a.CreatedByPrincipalID); err != nil {
		return nil, err
	}
	if len(dimJSON) > 0 {
		if err := json.Unmarshal(dimJSON, &a.Dimensions); err != nil {
			return nil, fmt.Errorf("decode dimensions: %w", err)
		}
	}
	return &a, nil
}

// CreateUsageAdjustment is the explicit, evidenced operator command C2:
// unlike the automatic late-routing path (registerLateEvent), this is for
// when an operator needs to redirect an ALREADY-INGESTED event's quantity
// into a specific open window — e.g. a quarantined event that investigation
// determined should count after all. The quantity is always the source
// event's own recorded quantity, never a caller-supplied number: an
// operator can redirect evidence, never fabricate an amount (the same
// doctrine the source_usage_event_id foreign key to usage_event_records
// already enforces at the schema level).
func (s *PgStore) CreateUsageAdjustment(ctx context.Context, targetStatementID, meterKey, sourceUsageEventID, reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.UsageAdjustment, error) {
	var out *domain.UsageAdjustment
	err := s.usageTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		source, err := scanUsageEvent(tx.QueryRow(ctx, `SELECT `+usageEventColumns+` FROM usage_event_records
			WHERE meter_key = $1 AND usage_event_id = $2`, meterKey, sourceUsageEventID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrUsageEventNotFound
		}
		if err != nil {
			return err
		}
		if source.StatementID == nil {
			return domain.ErrAdjustmentSourceUnlinked
		}
		target, err := loadStatement(ctx, tx, targetStatementID, true)
		if err != nil {
			return err
		}
		if target.Status != domain.StatementOpen {
			return fmt.Errorf("%w: target statement is %s, must be OPEN", domain.ErrStatementInvalidState, target.Status)
		}
		dimJSON, err := json.Marshal(source.Dimensions)
		if err != nil {
			return fmt.Errorf("marshal dimensions: %w", err)
		}
		adjID := domain.NewCommercialID(domain.PrefixUsageAdjustment)
		got, err := scanUsageAdjustment(tx.QueryRow(ctx, `
			INSERT INTO usage_adjustments (adjustment_id, origin_statement_id, target_statement_id, meter_key,
				source_usage_event_id, quantity, occurred_at, dimensions, reason, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9, $10, $11) RETURNING `+usageAdjustmentColumns,
			adjID, *source.StatementID, targetStatementID, meterKey, sourceUsageEventID, source.Quantity,
			source.OccurredAt, dimJSON, reason, now, actor))
		if err != nil {
			return err
		}
		if err := bumpStatement(ctx, tx, &targetStatementID, source.Quantity, source.OccurredAt, true); err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "usage_adjustment", AggregateID: adjID,
			EventType: "usage_adjustment.created", Payload: got})
	})
	return out, err
}

// CorrectUsageEvent replaces an accepted event's quantity/occurrence before
// its window closes. Once the window is not OPEN, a correction is no
// different from any other late fact: it is routed the same way a late
// first-time event would be, via registerLateEvent, rather than mutating
// closed history.
func (s *PgStore) CorrectUsageEvent(ctx context.Context, meterKey, originalEventID, newEventID string, in domain.EventInput,
	reason, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.UsageEventRecord, error) {
	var out *domain.UsageEventRecord
	err := s.usageTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		orig, err := scanUsageEvent(tx.QueryRow(ctx, `SELECT `+usageEventColumns+` FROM usage_event_records
			WHERE meter_key = $1 AND usage_event_id = $2 FOR UPDATE`, meterKey, originalEventID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrUsageEventNotFound
		}
		if err != nil {
			return err
		}
		if orig.Status != domain.UsageAccepted {
			return domain.ErrUsageEventNotCorrectable
		}
		if err := declareOrgForSellerPlane(ctx, tx, orig.OrganizationID); err != nil {
			return err
		}
		m, err := loadMeterDefinition(ctx, tx, meterKey, orig.MeterVersion)
		if err != nil {
			return err
		}
		dimJSON, err := json.Marshal(in.Dimensions)
		if err != nil {
			return fmt.Errorf("marshal dimensions: %w", err)
		}

		if !orig.Late {
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM usage_statements WHERE statement_id = $1`, *orig.StatementID).Scan(&status); err != nil {
				return err
			}
			if status != string(domain.StatementOpen) {
				return domain.ErrUsageEventNotCorrectable
			}
		} else {
			return domain.ErrUsageEventNotCorrectable
		}

		// The replacement row must exist before the original can point its
		// superseded_by_event_id at it: that column carries a foreign key
		// into this same table (meter_key, usage_event_id), so insert first,
		// link second.
		var e *domain.UsageEventRecord
		if r := domain.ValidateUsageQuantity(*m, in); r != "" {
			e, err = scanUsageEvent(tx.QueryRow(ctx, `
				INSERT INTO usage_event_records (meter_key, usage_event_id, meter_version, organization_id,
					subscription_id, term_no, statement_id, quantity, dimensions, occurred_at, observed_at,
					source_service, status, quarantine_reason, late, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, NULL, $7::numeric, $8, $9, $10, 'correction:'||$11, 'QUARANTINED', $12, FALSE, $10)
				RETURNING `+usageEventColumns,
				meterKey, newEventID, orig.MeterVersion, orig.OrganizationID, orig.SubscriptionID, orig.TermNo,
				in.Quantity, dimJSON, in.OccurredAt, now, actor, r))
		} else {
			st, serr := findOrCreateOpenStatement(ctx, tx, orig.SubscriptionID, orig.TermNo, *m, now)
			if serr != nil {
				return serr
			}
			e, err = scanUsageEvent(tx.QueryRow(ctx, `
				INSERT INTO usage_event_records (meter_key, usage_event_id, meter_version, organization_id, subscription_id,
					term_no, statement_id, quantity, dimensions, occurred_at, observed_at, source_service, status, late, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11, 'correction:'||$12, 'ACCEPTED', FALSE, $11)
				RETURNING `+usageEventColumns,
				meterKey, newEventID, orig.MeterVersion, orig.OrganizationID, orig.SubscriptionID, orig.TermNo,
				st.StatementID, in.Quantity, dimJSON, in.OccurredAt, now, actor))
			if err == nil {
				err = bumpStatement(ctx, tx, &st.StatementID, in.Quantity, in.OccurredAt, true)
			}
		}
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `UPDATE usage_event_records SET status = 'CORRECTED', superseded_by_event_id = $3
			WHERE meter_key = $1 AND usage_event_id = $2`, meterKey, originalEventID, newEventID); err != nil {
			return err
		}
		if err := reverseStatement(ctx, tx, *orig.StatementID); err != nil {
			return err
		}
		out = e
		return nil
	})
	return out, err
}

// reverseStatement removes a superseded event from its OPEN statement's
// running event_count before the correction's replacement is added, so the
// live counter never double-counts across a correction. The authoritative
// total is always recomputed from raw events at certify time, so there is
// no running total here to reverse.
func reverseStatement(ctx context.Context, tx pgx.Tx, statementID string) error {
	_, err := tx.Exec(ctx, `UPDATE usage_statements SET event_count = GREATEST(event_count - 1, 0) WHERE statement_id = $1`, statementID)
	return err
}

// ── Freeze / certify / reopen ────────────────────────────────────────────────

const usageStatementColumns = `statement_id, subscription_id, term_no, meter_key, meter_version, status,
	total_quantity::text, event_count, quarantined_count, watermark_at, created_at, frozen_at, frozen_by_principal_id,
	certified_at, certified_by_principal_id, superseded_at, superseded_by_principal_id, supersede_reason, superseded_by_statement_id`

func scanStatement(row pgx.Row) (*domain.UsageStatement, error) {
	var st domain.UsageStatement
	if err := row.Scan(&st.StatementID, &st.SubscriptionID, &st.TermNo, &st.MeterKey, &st.MeterVersion, &st.Status,
		&st.TotalQuantity, &st.EventCount, &st.QuarantinedCount, &st.WatermarkAt, &st.CreatedAt, &st.FrozenAt,
		&st.FrozenByPrincipalID, &st.CertifiedAt, &st.CertifiedByPrincipalID, &st.SupersededAt, &st.SupersededByPrincipalID,
		&st.SupersedeReason, &st.SupersededByStatementID); err != nil {
		return nil, err
	}
	return &st, nil
}

func loadStatement(ctx context.Context, tx pgx.Tx, statementID string, forUpdate bool) (*domain.UsageStatement, error) {
	q := `SELECT ` + usageStatementColumns + ` FROM usage_statements WHERE statement_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	st, err := scanStatement(tx.QueryRow(ctx, q, statementID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrStatementNotFound
	}
	return st, err
}

func acceptedEvents(ctx context.Context, tx pgx.Tx, statementID string) ([]domain.AcceptedEvent, error) {
	rows, err := tx.Query(ctx, `SELECT usage_event_id, quantity::text, dimensions, occurred_at FROM usage_event_records
		WHERE statement_id = $1 AND status = 'ACCEPTED' ORDER BY usage_event_id`, statementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AcceptedEvent
	for rows.Next() {
		var e domain.AcceptedEvent
		var dimJSON []byte
		if err := rows.Scan(&e.EventID, &e.Quantity, &dimJSON, &e.OccurredAt); err != nil {
			return nil, err
		}
		if len(dimJSON) > 0 {
			if err := json.Unmarshal(dimJSON, &e.Dimensions); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// adjustmentEvents is acceptedEvents' counterpart for late usage: every
// usage_adjustments row targeting this statement, converted to the exact
// same domain.AcceptedEvent shape so domain.Aggregate treats a late-arrived
// fact exactly as if it had arrived on time — correctly for every
// aggregation method (MAX/LAST need OccurredAt for ordering/tie-breaking;
// UNIQUE_COUNT needs Dimensions), not just SUM. Fixes COM-CTRL-017: before
// this, an adjustment's quantity was computed and stored but never actually
// counted toward any billed total.
func adjustmentEvents(ctx context.Context, tx pgx.Tx, statementID string) ([]domain.AcceptedEvent, error) {
	rows, err := tx.Query(ctx, `SELECT source_usage_event_id, quantity::text, dimensions, occurred_at
		FROM usage_adjustments WHERE target_statement_id = $1 ORDER BY source_usage_event_id`, statementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AcceptedEvent
	for rows.Next() {
		var e domain.AcceptedEvent
		var dimJSON []byte
		if err := rows.Scan(&e.EventID, &e.Quantity, &dimJSON, &e.OccurredAt); err != nil {
			return nil, err
		}
		if len(dimJSON) > 0 {
			if err := json.Unmarshal(dimJSON, &e.Dimensions); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// closeWindow is CloseUsageWindow's body, shared between the handler-facing
// command and the boundary worker at term end.
func closeWindow(ctx context.Context, tx pgx.Tx, subscriptionID string, termNo int, meterKey, actor string, now time.Time) (*domain.UsageStatement, error) {
	var statementID string
	err := tx.QueryRow(ctx, `SELECT statement_id FROM usage_statements
		WHERE subscription_id = $1 AND term_no = $2 AND meter_key = $3 AND status <> 'SUPERSEDED'`,
		subscriptionID, termNo, meterKey).Scan(&statementID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrStatementNotFound
	}
	if err != nil {
		return nil, err
	}
	st, err := loadStatement(ctx, tx, statementID, true)
	if err != nil {
		return nil, err
	}
	if st.Status != domain.StatementOpen {
		return st, nil // idempotent: already closed
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_statements SET status = 'FROZEN', frozen_at = $2, frozen_by_principal_id = $3
		WHERE statement_id = $1`, statementID, now, actor); err != nil {
		return nil, err
	}
	got, err := loadStatement(ctx, tx, statementID, false)
	if err != nil {
		return nil, err
	}
	return got, outbox.Insert(ctx, tx, outbox.Event{AggregateType: "usage_statement", AggregateID: statementID,
		EventType: "usage.window_frozen", Payload: got})
}

// freezeAndCertifyTermUsage closes and certifies every usage statement open
// for a subscription's term, whatever meter each is under — called by the
// boundary worker exactly once when that term ends (migration 000011 wires
// no separate usage-window calendar; the subscription term IS the window).
func freezeAndCertifyTermUsage(ctx context.Context, tx pgx.Tx, subscriptionID string, termNo int, actor string, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT meter_key FROM usage_statements
		WHERE subscription_id = $1 AND term_no = $2 AND status = 'OPEN'`, subscriptionID, termNo)
	if err != nil {
		return err
	}
	meterKeys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, meterKey := range meterKeys {
		st, err := closeWindow(ctx, tx, subscriptionID, termNo, meterKey, actor, now)
		if err != nil {
			return err
		}
		if _, err := certifyStatement(ctx, tx, st.StatementID, actor, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *PgStore) CloseUsageWindow(ctx context.Context, subscriptionID string, termNo int, meterKey string, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.UsageStatement, error) {
	var out *domain.UsageStatement
	err := s.usageTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := closeWindow(ctx, tx, subscriptionID, termNo, meterKey, actor, now)
		out = got
		return err
	})
	return out, err
}

// certifyStatement computes the statement's authoritative total from its
// accepted events under domain.Aggregate — the single source of truth for
// both certification and ExplainAggregation — and seals it. A statement
// that already has adjustment lines targeting it certifies as ADJUSTED
// instead of CERTIFIED, so a reader can see at a glance that its total
// isn't purely this window's own organic usage.
func certifyStatement(ctx context.Context, tx pgx.Tx, statementID, actor string, now time.Time) (*domain.UsageStatement, error) {
	st, err := loadStatement(ctx, tx, statementID, true)
	if err != nil {
		return nil, err
	}
	if st.Status != domain.StatementFrozen {
		if st.Status == domain.StatementCertified || st.Status == domain.StatementAdjusted {
			return st, nil // idempotent
		}
		return nil, fmt.Errorf("%w: statement is %s, must be FROZEN to certify", domain.ErrStatementInvalidState, st.Status)
	}
	m, err := loadMeterDefinition(ctx, tx, st.MeterKey, st.MeterVersion)
	if err != nil {
		return nil, err
	}
	events, err := acceptedEvents(ctx, tx, statementID)
	if err != nil {
		return nil, err
	}
	adjustments, err := adjustmentEvents(ctx, tx, statementID)
	if err != nil {
		return nil, err
	}
	// COM-CTRL-017 fix: a late usage fact's quantity must actually count
	// toward the certified total, not just flip the status to ADJUSTED.
	total := domain.Aggregate(*m, append(events, adjustments...))
	adjustmentCount := len(adjustments)
	status := domain.StatementCertified
	if adjustmentCount > 0 {
		status = domain.StatementAdjusted
	}

	if _, err := tx.Exec(ctx, `UPDATE usage_statements SET status = $2, total_quantity = $3::numeric,
		certified_at = $4, certified_by_principal_id = $5 WHERE statement_id = $1`,
		statementID, status, total, now, actor); err != nil {
		return nil, err
	}
	got, err := loadStatement(ctx, tx, statementID, false)
	if err != nil {
		return nil, err
	}
	return got, outbox.Insert(ctx, tx, outbox.Event{AggregateType: "usage_statement", AggregateID: statementID,
		EventType: "usage.statement_certified", Payload: got})
}

func (s *PgStore) CertifyUsageStatement(ctx context.Context, statementID, actor string, now time.Time, claim domain.IdempotencyClaim) (*domain.UsageStatement, error) {
	var out *domain.UsageStatement
	err := s.usageTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := certifyStatement(ctx, tx, statementID, actor, now)
		out = got
		return err
	})
	return out, err
}

// ReopenWindow is the "controlled exception" path (§4.4): a certified
// statement is never edited — it is superseded by a fresh OPEN statement for
// the same window, which must then be re-frozen and re-certified through the
// normal path. The reopener must not be who certified it (maker-checker,
// same doctrine as the price book).
func (s *PgStore) ReopenWindow(ctx context.Context, statementID, actor, reason string, now time.Time, claim domain.IdempotencyClaim) (*domain.UsageStatement, error) {
	var out *domain.UsageStatement
	err := s.usageTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		st, err := loadStatement(ctx, tx, statementID, true)
		if err != nil {
			return err
		}
		if st.Status != domain.StatementCertified && st.Status != domain.StatementAdjusted {
			return fmt.Errorf("%w: only a certified statement can be reopened", domain.ErrStatementInvalidState)
		}
		if st.CertifiedByPrincipalID != nil && *st.CertifiedByPrincipalID == actor {
			return domain.ErrReopenNeedsIndependentActor
		}
		// Original marked SUPERSEDED first, replacement inserted second: the
		// partial "one live statement per window" index cannot be attached
		// as a deferrable constraint (Postgres does not allow that for a
		// partial index), so this ordering is what keeps it satisfied at
		// every intermediate statement — the original drops out of the
		// index the moment it is marked SUPERSEDED, before the replacement
		// row appears. The FK this ordering conflicts with
		// (superseded_by_statement_id, needing its target to already
		// exist) is the one declared DEFERRABLE INITIALLY DEFERRED, so it
		// is only checked once both statements have run.
		replacementID := domain.NewCommercialID(domain.PrefixUsageStatement)
		if _, err := tx.Exec(ctx, `UPDATE usage_statements SET status = 'SUPERSEDED', superseded_at = $2,
			superseded_by_principal_id = $3, supersede_reason = $4, superseded_by_statement_id = $5
			WHERE statement_id = $1`, statementID, now, actor, reason, replacementID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO usage_statements (statement_id, subscription_id, term_no, meter_key,
			meter_version, status, created_at) VALUES ($1, $2, $3, $4, $5, 'OPEN', $6)`,
			replacementID, st.SubscriptionID, st.TermNo, st.MeterKey, st.MeterVersion, now); err != nil {
			return err
		}
		// Every event that had counted toward the superseded statement is
		// re-homed onto the replacement so it aggregates again on recertify.
		if _, err := tx.Exec(ctx, `UPDATE usage_event_records SET statement_id = $2
			WHERE statement_id = $1 AND status = 'ACCEPTED'`, statementID, replacementID); err != nil {
			return err
		}
		got, err := loadStatement(ctx, tx, replacementID, false)
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "usage_statement", AggregateID: statementID,
			EventType: "usage.window_reopened", Payload: map[string]any{"superseded_statement_id": statementID,
				"replacement_statement_id": replacementID, "reason": reason, "actor_id": actor}})
	})
	return out, err
}

// ── Queries ──────────────────────────────────────────────────────────────────

func (s *PgStore) GetUsage(ctx context.Context, subscriptionID string, termNo int, meterKey string) (*domain.UsageStatement, error) {
	var out *domain.UsageStatement
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		st, err := scanStatement(tx.QueryRow(ctx, `SELECT `+usageStatementColumns+` FROM usage_statements
			WHERE subscription_id = $1 AND term_no = $2 AND meter_key = $3 AND status <> 'SUPERSEDED'`, subscriptionID, termNo, meterKey))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrStatementNotFound
		}
		out = st
		return err
	})
	return out, err
}

func (s *PgStore) GetUsageStatement(ctx context.Context, statementID string) (*domain.UsageStatement, error) {
	var out *domain.UsageStatement
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		st, err := loadStatement(ctx, tx, statementID, false)
		out = st
		return err
	})
	return out, err
}

// ExplainAggregation returns exactly the accepted events certification
// summed — proof, not just an assertion, of how the total was reached.
func (s *PgStore) ExplainAggregation(ctx context.Context, statementID string) ([]domain.UsageEventRecord, error) {
	var out []domain.UsageEventRecord
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		if _, err := loadStatement(ctx, tx, statementID, false); err != nil {
			return err
		}
		// Includes every event directly linked to this statement, plus —
		// since COM-CTRL-017's fix — every late event whose quantity was
		// carried in via a usage_adjustments row targeting it: certified and
		// explained must never disagree about what was counted.
		rows, err := tx.Query(ctx, `
			SELECT `+usageEventColumns+` FROM usage_event_records WHERE statement_id = $1
			UNION
			SELECT `+usageEventColumns+` FROM usage_event_records
			WHERE (meter_key, usage_event_id) IN (
				SELECT meter_key, source_usage_event_id FROM usage_adjustments WHERE target_statement_id = $1)
			ORDER BY occurred_at, usage_event_id`, statementID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanUsageEvent(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) GetLateEvents(ctx context.Context, subscriptionID string) ([]domain.UsageEventRecord, error) {
	var out []domain.UsageEventRecord
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+usageEventColumns+` FROM usage_event_records
			WHERE subscription_id = $1 AND late ORDER BY occurred_at`, subscriptionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanUsageEvent(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) GetDedupStatus(ctx context.Context, meterKey, usageEventID string) (*domain.UsageEventRecord, error) {
	var out *domain.UsageEventRecord
	err := s.usageTx(ctx, func(tx pgx.Tx) error {
		e, err := scanUsageEvent(tx.QueryRow(ctx, `SELECT `+usageEventColumns+` FROM usage_event_records
			WHERE meter_key = $1 AND usage_event_id = $2`, meterKey, usageEventID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrUsageEventNotFound
		}
		out = e
		return err
	})
	return out, err
}
