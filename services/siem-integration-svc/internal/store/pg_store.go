package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/siem-integration-svc/internal/credential"
	"zoiko.io/siem-integration-svc/internal/domain"
)

// PgStore is the production Store (deployments/migrations/000001).
//
// Every statement runs in a transaction that first sets app.tenant_id, so the
// FORCEd row-level security policies apply; each query also carries its own
// tenant_id predicate.
//
// The exporter auth_token is sealed with the credential keyring before it
// reaches the database and is never read back by any Store method — nothing in
// this service uses it yet (no SIEM egress), and no read route may return it.
type PgStore struct {
	pool *pgxpool.Pool
	keys *credential.Keyring
}

// NewPgStore returns a store backed by pool. keys seals exporter credentials;
// nil means a create carrying an auth_token is refused
// (ErrCredentialSealingUnavailable) rather than stored in plaintext.
func NewPgStore(pool *pgxpool.Pool, keys *credential.Keyring) *PgStore {
	return &PgStore{pool: pool, keys: keys}
}

var _ Store = (*PgStore)(nil)

const exporterColumns = `id, tenant_id, legal_entity_id, name, platform, endpoint_url, status,
	events_sent, last_streamed, created_at, updated_at`

func scanExporter(row pgx.Row) (*domain.SIEMExporter, error) {
	var e domain.SIEMExporter
	var platform, status string
	if err := row.Scan(&e.ID, &e.TenantID, &e.LegalEntityID, &e.Name, &platform, &e.EndpointURL, &status,
		&e.EventsSent, &e.LastStreamed, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	e.Platform = domain.SIEMPlatform(platform)
	e.Status = domain.ExporterStatus(status)
	return &e, nil
}

// Ping checks the pool can reach the database.
func (s *PgStore) Ping(ctx context.Context) error {
	if s.pool == nil {
		return errors.New("database pool not initialised")
	}
	return s.pool.Ping(ctx)
}

func (s *PgStore) CreateExporter(ctx context.Context, tenantID string, exp *domain.SIEMExporter) error {
	id := uuid.New().String()
	var ciphertext, keyID *string
	if exp.AuthToken != "" {
		if s.keys == nil {
			return ErrCredentialSealingUnavailable
		}
		ct, kid, err := s.keys.Seal([]byte(exp.AuthToken), credential.ExporterAAD(tenantID, id))
		if err != nil {
			return fmt.Errorf("seal exporter credential: %w", err)
		}
		ciphertext, keyID = &ct, &kid
	}
	now := time.Now().UTC()
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO siem_exporters (id, tenant_id, legal_entity_id, name, platform, endpoint_url,
				auth_token_ciphertext, auth_token_key_id, status, events_sent, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,$10)`,
			id, tenantID, exp.LegalEntityID, exp.Name, string(exp.Platform), exp.EndpointURL,
			ciphertext, keyID, string(domain.ExporterActive), now)
		if err != nil {
			return fmt.Errorf("insert exporter: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	exp.ID = id
	exp.TenantID = tenantID
	exp.Status = domain.ExporterActive
	exp.CreatedAt = now
	exp.UpdatedAt = now
	// The plaintext has served its purpose; drop it from the caller's struct
	// too, so nothing downstream can log or return it by accident.
	exp.AuthToken = ""
	return nil
}

func (s *PgStore) GetExporterByID(ctx context.Context, tenantID, id string) (*domain.SIEMExporter, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	var out *domain.SIEMExporter
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		e, err := scanExporter(tx.QueryRow(ctx,
			`SELECT `+exporterColumns+` FROM siem_exporters WHERE tenant_id = $1 AND id = $2`, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get exporter: %w", err)
		}
		out = e
		return nil
	})
	return out, err
}

func (s *PgStore) ListExporters(ctx context.Context, tenantID, legalEntityID string) ([]domain.SIEMExporter, error) {
	var out []domain.SIEMExporter
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+exporterColumns+` FROM siem_exporters
			 WHERE tenant_id = $1 AND ($2 = '' OR legal_entity_id = $2)
			 ORDER BY created_at, id`, tenantID, legalEntityID)
		if err != nil {
			return fmt.Errorf("list exporters: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanExporter(rows)
			if err != nil {
				return fmt.Errorf("scan exporter: %w", err)
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}

// StreamEvent appends the event and advances the exporter's counters in one
// transaction. The exporter row is locked and incremented in SQL
// (events_sent + 1), so concurrent streams never lose a count, and an event is
// never recorded without its counter update or vice versa.
//
// Status DELIVERED means "recorded by this service" — the same value the
// in-memory store set. There is no egress to the external SIEM platform yet
// (see domain.SIEMExporter.AuthToken), so it does not mean the platform
// received it.
func (s *PgStore) StreamEvent(ctx context.Context, tenantID string, evt *domain.SIEMEvent) error {
	if _, err := uuid.Parse(evt.ExporterID); err != nil {
		return ErrNotFound
	}
	id := uuid.New().String()
	now := time.Now().UTC()
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE siem_exporters
			   SET events_sent = events_sent + 1, last_streamed = $3, updated_at = $3
			 WHERE tenant_id = $1 AND id = $2`, tenantID, evt.ExporterID, now)
		if err != nil {
			return fmt.Errorf("advance exporter counters: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO siem_events (id, tenant_id, exporter_id, source_service, event_type, severity,
				message, payload, status, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'DELIVERED',$9)`,
			id, tenantID, evt.ExporterID, evt.SourceSvc, evt.EventType, string(evt.Severity),
			evt.Message, evt.Payload, now)
		if err != nil {
			return fmt.Errorf("append event: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	evt.ID = id
	evt.TenantID = tenantID
	evt.Status = "DELIVERED"
	evt.Timestamp = now
	return nil
}

func (s *PgStore) ListEvents(ctx context.Context, tenantID, exporterID string) ([]domain.SIEMEvent, error) {
	if exporterID != "" {
		if _, err := uuid.Parse(exporterID); err != nil {
			return nil, nil
		}
	}
	var out []domain.SIEMEvent
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, exporter_id, source_service, event_type, severity, message, payload, status, occurred_at
			  FROM siem_events
			 WHERE tenant_id = $1 AND ($2 = '' OR exporter_id::text = $2)
			 ORDER BY occurred_at, id`, tenantID, exporterID)
		if err != nil {
			return fmt.Errorf("list events: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.SIEMEvent
			var severity string
			if err := rows.Scan(&e.ID, &e.TenantID, &e.ExporterID, &e.SourceSvc, &e.EventType, &severity,
				&e.Message, &e.Payload, &e.Status, &e.Timestamp); err != nil {
				return fmt.Errorf("scan event: %w", err)
			}
			e.Severity = domain.EventSeverity(severity)
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// inTenantTx runs fn in a transaction scoped to tenantID for row-level
// security. An empty tenant is refused rather than run unscoped.
func (s *PgStore) inTenantTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if s.pool == nil {
		return errors.New("database pool not initialised")
	}
	if tenantID == "" {
		return errors.New("tenant scope is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("set tenant scope: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
