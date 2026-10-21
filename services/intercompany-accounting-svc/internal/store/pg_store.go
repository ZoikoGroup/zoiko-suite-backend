package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool

	// eventRegion is the residencyregion stamped on every intercompany event.
	// Empty means no region was configured, and every event-emitting write
	// then fails (envelope.New refuses it) rather than emitting an event
	// with a guessed region.
	eventRegion string
}

// Option configures a PgStore.
type Option func(*PgStore)

// WithEventRegion sets the residency region carried by emitted events
// (config EVENT_RESIDENCY_REGION).
func WithEventRegion(region string) Option {
	return func(s *PgStore) { s.eventRegion = region }
}

func New(pool *pgxpool.Pool, opts ...Option) *PgStore {
	s := &PgStore{pool: pool}
	for _, o := range opts {
		o(s)
	}
	return s
}

// enqueueEntryEvent writes one intercompany-entry lifecycle event to the
// transactional outbox inside tx, the transaction that made the change it
// reports (ZS-EVENT-001 §6: state change and outbox record are one local
// atomic transaction).
//
// legalEntityID is deliberately left empty: domain.IntercompanyEntry is
// inherently two-entity (SourceLegalEntityID and TargetLegalEntityID), so it
// has no single canonical legal_entity_id. Both remain explicit named
// fields inside the payload instead.
func (s *PgStore) enqueueEntryEvent(ctx context.Context, tx pgx.Tx, fact, legacyType, actorID, correlationID string, entry *domain.IntercompanyEntry) error {
	data := map[string]any{
		"intercompany_entry_id":      entry.IntercompanyEntryID,
		"tenant_id":                  entry.TenantID,
		"source_legal_entity_id":     entry.SourceLegalEntityID,
		"target_legal_entity_id":     entry.TargetLegalEntityID,
		"source_journal_id":          entry.SourceJournalID,
		"amount":                     entry.Amount,
		"currency_code":              entry.CurrencyCode,
		"counterparty_amount":        entry.CounterpartyAmount,
		"counterparty_currency_code": entry.CounterpartyCurrencyCode,
		"dimensions":                 entry.Dimensions,
		"match_status":               entry.MatchStatus,
		"timestamp":                  time.Now().UTC(),
	}
	if entry.FiscalPeriod != nil {
		data["fiscal_period"] = *entry.FiscalPeriod
	}
	if entry.TargetJournalID != nil {
		data["target_journal_id"] = *entry.TargetJournalID
	}
	if entry.MismatchReason != nil {
		data["mismatch_reason"] = *entry.MismatchReason
	}
	if entry.DisputeReason != nil {
		data["dispute_reason"] = *entry.DisputeReason
	}
	if entry.ResolutionNote != nil {
		data["resolution_note"] = *entry.ResolutionNote
	}
	if entry.ResolutionOutcome != nil {
		data["resolution_outcome"] = *entry.ResolutionOutcome
	}
	env, err := envelope.New(envelope.Spec{
		Type:            "com.zoikosuite.accounting.intercompany-entry." + fact,
		LegacyType:      legacyType,
		Service:         "intercompany-accounting-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        entry.TenantID,
		AggregateType:   "intercompany-entry",
		AggregateID:     entry.IntercompanyEntryID,
		CorrelationID:   correlationID,
		ActorID:         actorID,
		ResidencyRegion: s.eventRegion,
		// Intercompany entries are financial records of a tenant's books.
		Classification: envelope.Confidential,
		Data:           data,
	})
	if err != nil {
		return fmt.Errorf("build intercompany.%s event: %w", fact, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue intercompany.%s event: %w", fact, err)
	}
	return nil
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// CreateEntry inserts an UNMATCHED intercompany entry. It is the
// pre-A6-friendly form of CreateEntryWithAttempt with no create-time
// verification attempt; production create calls go through
// CreateEntryWithAttempt.
func (s *PgStore) CreateEntry(ctx context.Context, entry *domain.IntercompanyEntry, actorID, correlationID string) (created bool, err error) {
	return s.createEntry(ctx, entry, nil, actorID, correlationID)
}

// CreateEntryWithAttempt inserts an intercompany entry and, in the same
// transaction, the create-time source-verification attempt that put it in
// MISMATCH (SPEC_DEVIATIONS A6), its creation transition and its "created"
// lifecycle event. attempt is nil when the source journal passed and the pair
// is UNMATCHED. A MISMATCH pair without an attempt is refused: the reason
// would have no evidence.
//
// Idempotent on (tenant_id, source_journal_id), exactly like CreateEntry. On
// a replay the ORIGINAL row is loaded into *entry and created=false; no
// attempt is written for the replay, and no second event is enqueued.
//
// Amounts are bound as decimal text cast to NUMERIC; they never pass
// through float64 on the way in or (amount::text) on the way out.
func (s *PgStore) CreateEntryWithAttempt(ctx context.Context, entry *domain.IntercompanyEntry, attempt *domain.MatchAttempt, actorID, correlationID string) (created bool, err error) {
	switch {
	case entry.MatchStatus == domain.MatchStatusMismatch && attempt == nil:
		return false, fmt.Errorf("create MISMATCH entry %s: a match attempt is required", entry.IntercompanyEntryID)
	case attempt != nil && entry.MatchStatus != domain.MatchStatusMismatch:
		return false, fmt.Errorf("create entry %s: an attempt is only valid for MISMATCH, got %s", entry.IntercompanyEntryID, entry.MatchStatus)
	case attempt != nil && attempt.Outcome != domain.MatchStatusMismatch:
		return false, fmt.Errorf("create entry %s: attempt outcome must be MISMATCH, got %s", entry.IntercompanyEntryID, attempt.Outcome)
	case attempt != nil && attempt.IntercompanyEntryID != entry.IntercompanyEntryID:
		return false, fmt.Errorf("create entry %s: attempt describes %s", entry.IntercompanyEntryID, attempt.IntercompanyEntryID)
	}
	return s.createEntry(ctx, entry, attempt, actorID, correlationID)
}

func (s *PgStore) createEntry(ctx context.Context, entry *domain.IntercompanyEntry, attempt *domain.MatchAttempt, actorID, correlationID string) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	if entry.Dimensions == nil {
		entry.Dimensions = domain.Dimensions{}
	}
	if attempt != nil {
		// The pair reports the attempt's sentence, and the event and
		// transition below describe the row as committed.
		entry.MismatchReason = attempt.Message
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO intercompany_entries (
				intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
				source_journal_id, target_journal_id, amount, currency_code,
				counterparty_amount, counterparty_currency_code, fiscal_period, dimensions,
				match_status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9::numeric, $10, $11, $12, $13, $14, $15)
			ON CONFLICT (tenant_id, source_journal_id) DO NOTHING
		`, entry.IntercompanyEntryID, tenantID, entry.SourceLegalEntityID, entry.TargetLegalEntityID,
			entry.SourceJournalID, entry.TargetJournalID, string(entry.Amount), entry.CurrencyCode,
			string(entry.CounterpartyAmount), entry.CounterpartyCurrencyCode, entry.FiscalPeriod, entry.Dimensions,
			entry.MatchStatus, entry.CreatedAt, entry.UpdatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if err := scanIntercompanyEntry(tx.QueryRow(ctx, `
				SELECT `+intercompanyEntryColumns+`
				FROM intercompany_entries WHERE tenant_id = $1 AND source_journal_id = $2
			`, tenantID, entry.SourceJournalID), entry); err != nil {
				return err
			}
			// Replay of a prior request for the same source_journal_id —
			// the original row's own creation already recorded its attempt
			// and enqueued its events.
			created = false
			return nil
		}
		entry.TenantID = tenantID
		if attempt != nil {
			attempt.FiscalPeriod = entry.FiscalPeriod
			if err := insertMatchAttempt(ctx, tx, tenantID, attempt); err != nil {
				return err
			}
		}
		if err := recordTransition(ctx, tx, tenantID, entry.IntercompanyEntryID, transition{
			to: entry.MatchStatus, fiscalPeriod: entry.FiscalPeriod, reason: entry.MismatchReason,
			actor: actorID, correlationID: correlationID,
		}); err != nil {
			return err
		}
		if err := s.enqueueEntryEvent(ctx, tx, "created", "intercompany.entry.created", actorID, correlationID, entry); err != nil {
			return err
		}
		if attempt != nil {
			// A pair created already MISMATCH is both a created and a
			// detected-mismatch fact, and A7's event set has a name for each.
			if err := s.enqueueEntryEvent(ctx, tx, "mismatch-detected", "intercompany.mismatch.detected", actorID, correlationID, entry); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	return created, err
}

// intercompanyEntryColumns is the select list for a full pair, FROM
// intercompany_entries. mismatch_reason comes from the latest match attempt
// (match_attempts.go); the pair's own mismatch_reason column is no longer
// written or read.
var intercompanyEntryColumns = `
	intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
	source_journal_id, target_journal_id, amount::text, currency_code,
	counterparty_amount::text, counterparty_currency_code, fiscal_period, dimensions,
	match_status, ` + latestMismatchReason + ` AS mismatch_reason, acknowledged_at, acknowledged_by_principal_id,
	disputed_at, disputed_by_principal_id, dispute_reason,
	resolved_at, resolved_by_principal_id, resolution_note, resolution_outcome,
	closed_at, closed_by_principal_id, close_reason,
	created_at, updated_at`

func scanIntercompanyEntry(row pgx.Row, entry *domain.IntercompanyEntry) error {
	return row.Scan(
		&entry.IntercompanyEntryID, &entry.TenantID, &entry.SourceLegalEntityID, &entry.TargetLegalEntityID,
		&entry.SourceJournalID, &entry.TargetJournalID, &entry.Amount, &entry.CurrencyCode,
		&entry.CounterpartyAmount, &entry.CounterpartyCurrencyCode, &entry.FiscalPeriod, &entry.Dimensions,
		&entry.MatchStatus, &entry.MismatchReason, &entry.AcknowledgedAt, &entry.AcknowledgedByPrincipalID,
		&entry.DisputedAt, &entry.DisputedByPrincipalID, &entry.DisputeReason,
		&entry.ResolvedAt, &entry.ResolvedByPrincipalID, &entry.ResolutionNote, &entry.ResolutionOutcome,
		&entry.ClosedAt, &entry.ClosedByPrincipalID, &entry.CloseReason,
		&entry.CreatedAt, &entry.UpdatedAt,
	)
}

// loadEntry reads one pair through intercompanyEntryColumns inside tx, for a
// caller that already holds an RLS-scoped transaction. The lifecycle commands
// use it after their guarded UPDATE, so the event they enqueue describes the
// state they just committed rather than the row as read before the lock.
func loadEntry(ctx context.Context, tx pgx.Tx, tenantID, id string) (*domain.IntercompanyEntry, error) {
	var entry domain.IntercompanyEntry
	if err := scanIntercompanyEntry(tx.QueryRow(ctx, `
		SELECT `+intercompanyEntryColumns+`
		FROM intercompany_entries
		WHERE intercompany_entry_id = $1 AND tenant_id = $2
	`, id, tenantID), &entry); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrEntryNotFound
		}
		return nil, err
	}
	return &entry, nil
}

func (s *PgStore) GetEntry(ctx context.Context, id string) (*domain.IntercompanyEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var entry domain.IntercompanyEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanIntercompanyEntry(tx.QueryRow(ctx, `
			SELECT `+intercompanyEntryColumns+`
			FROM intercompany_entries
			WHERE intercompany_entry_id = $1 AND tenant_id = $2
		`, id, tenantID), &entry)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEntryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// GetEntryBySourceJournal resolves the create idempotency key without
// consulting general-ledger-svc, so a retried create returns the original
// pair even when the ledger is unavailable and never records a second
// create-time attempt (SPEC_DEVIATIONS A6).
func (s *PgStore) GetEntryBySourceJournal(ctx context.Context, sourceJournalID string) (*domain.IntercompanyEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var entry domain.IntercompanyEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanIntercompanyEntry(tx.QueryRow(ctx, `
			SELECT `+intercompanyEntryColumns+`
			FROM intercompany_entries
			WHERE tenant_id = $1 AND source_journal_id = $2
		`, tenantID, sourceJournalID), &entry)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEntryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

func (s *PgStore) ListEntries(ctx context.Context, sourceEntityID, targetEntityID string) ([]domain.IntercompanyEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.IntercompanyEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT ` + intercompanyEntryColumns + `
			FROM intercompany_entries
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if sourceEntityID != "" {
			args = append(args, sourceEntityID)
			query += fmt.Sprintf(" AND source_legal_entity_id = $%d", len(args))
		}
		if targetEntityID != "" {
			args = append(args, targetEntityID)
			query += fmt.Sprintf(" AND target_legal_entity_id = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var entry domain.IntercompanyEntry
			if err := scanIntercompanyEntry(rows, &entry); err != nil {
				return err
			}
			out = append(out, entry)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateMatch records a MatchIntercompany outcome and, in the same
// transaction, the attempt that produced it (intercompany_match_attempts)
// and the lifecycle event that outcome implies: "matched" for MATCHED (with
// the pre-standard "posted" name kept as its legacy event_type for one
// release), "mismatch-detected" for MISMATCH. entry must already carry the
// target state (TargetJournalID, MatchStatus) and attempt the evidence for it —
// see handler.go's MatchEntry. The pair's mismatch_reason is not stored on
// the pair: it is the latest attempt's message, and entry.MismatchReason is
// set from attempt on return.
//
// Guarded on the CURRENT status being one of domain.MatchableStatuses, read
// under a row lock, the same guard the other lifecycle methods use: without
// it, a pair disputed or matched by another request after the handler read
// it would be silently overwritten. Returns domain.ErrInvalidPairTransition
// in that case. A change of status is recorded in the pair's transition
// history in the same transaction.
func (s *PgStore) UpdateMatch(ctx context.Context, entry *domain.IntercompanyEntry, attempt *domain.MatchAttempt, actorID, correlationID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	// A match produces exactly these two outcomes; any other target status
	// belongs to a lifecycle command with its own guard (pair_lifecycle.go).
	if entry.MatchStatus != domain.MatchStatusMatched && entry.MatchStatus != domain.MatchStatusMismatch {
		return domain.ErrInvalidPairTransition
	}
	if attempt == nil || attempt.Outcome != entry.MatchStatus || attempt.IntercompanyEntryID != entry.IntercompanyEntryID {
		return fmt.Errorf("match attempt does not describe this outcome")
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		current, period, err := lockPair(ctx, tx, tenantID, entry.IntercompanyEntryID)
		if err != nil {
			return err
		}
		if !slices.Contains(domain.MatchableStatuses, current) {
			return domain.ErrInvalidPairTransition
		}
		now := time.Now().UTC()
		res, err := tx.Exec(ctx, `
			UPDATE intercompany_entries
			SET target_journal_id = $1, match_status = $2, updated_at = $3
			WHERE intercompany_entry_id = $4 AND tenant_id = $5 AND match_status = ANY($6)
		`, entry.TargetJournalID, entry.MatchStatus, now, entry.IntercompanyEntryID, tenantID, domain.MatchableStatuses)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrInvalidPairTransition
		}
		attempt.FiscalPeriod = period
		if err := insertMatchAttempt(ctx, tx, tenantID, attempt); err != nil {
			return err
		}
		entry.MismatchReason = attempt.Message

		// A repeated MISMATCH is not a state change; only a change is history
		// (the attempt itself is recorded above).
		if current != entry.MatchStatus {
			if err := recordTransition(ctx, tx, tenantID, entry.IntercompanyEntryID, transition{
				from: &current, to: entry.MatchStatus, fiscalPeriod: period, reason: entry.MismatchReason,
				actor: actorID, correlationID: correlationID,
			}); err != nil {
				return err
			}
		}
		entry.TenantID = tenantID
		entry.UpdatedAt = now

		switch entry.MatchStatus {
		case domain.MatchStatusMatched:
			// "matched" is the event's real name; "posted" is kept only as
			// its legacy event_type for one release so a consumer still
			// filtering on the old name keeps working (SPEC_DEVIATIONS B11).
			return s.enqueueEntryEvent(ctx, tx, "matched", "intercompany.entry.posted", actorID, correlationID, entry)
		case domain.MatchStatusMismatch:
			return s.enqueueEntryEvent(ctx, tx, "mismatch-detected", "intercompany.mismatch.detected", actorID, correlationID, entry)
		default:
			// The other lifecycle commands use their own store methods
			// (pair_lifecycle.go), not this one.
			return nil
		}
	})
}
