package inbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CaseTable is the quarantine case table (ZS-EVENT-001 section 11.1).
const CaseTable = "eventing_quarantine_case"

// CaseSQL is copied verbatim into an adopting service's migration, after
// SchemaSQL. A quarantined receipt says THAT an event is held; a case says WHO
// owns it, by WHEN, and what was decided, so a quarantine is never an
// unowned dead letter (E-18, section 11 "no silent drop").
const CaseSQL = `
CREATE TABLE eventing_quarantine_case (
    case_id          UUID         PRIMARY KEY,
    consumer         TEXT         NOT NULL,
    tenant_id        TEXT         NOT NULL,
    event_id         TEXT         NOT NULL,
    event_type       TEXT         NOT NULL,
    region           TEXT,
    failure_class    TEXT         NOT NULL,
    error_code       TEXT         NOT NULL,
    payload_hash     TEXT         NOT NULL,
    attempt_count    INTEGER      NOT NULL,
    first_failure_at TIMESTAMPTZ  NOT NULL,
    last_failure_at  TIMESTAMPTZ  NOT NULL,
    owner            TEXT         NOT NULL,
    priority         TEXT         NOT NULL,
    sla_due_at       TIMESTAMPTZ  NOT NULL,
    disposition      TEXT,
    evidence_refs    TEXT[]       NOT NULL DEFAULT '{}',
    resolved_by      TEXT,
    resolved_at      TIMESTAMPTZ,

    CONSTRAINT eventing_quarantine_case_unique UNIQUE (consumer, tenant_id, event_id),
    CONSTRAINT eventing_quarantine_case_owner CHECK (btrim(owner) <> ''),
    CONSTRAINT eventing_quarantine_case_class CHECK (failure_class IN
        ('transient-infrastructure', 'consumer-capacity', 'schema-contract',
         'business-precondition', 'permanent-data', 'external-provider', 'poison-event')),
    CONSTRAINT eventing_quarantine_case_priority CHECK (priority IN ('low', 'normal', 'high', 'critical')),
    CONSTRAINT eventing_quarantine_case_disposition CHECK (disposition IS NULL OR disposition IN
        ('fix-and-replay', 'compensating-action', 'producer-correction',
         'accepted-exception', 'permanent-rejection')),
    -- a case is resolved exactly when it has a disposition, a resolver and evidence
    CONSTRAINT eventing_quarantine_case_resolution CHECK (
        (disposition IS NULL AND resolved_at IS NULL AND resolved_by IS NULL)
        OR (disposition IS NOT NULL AND resolved_at IS NOT NULL AND btrim(COALESCE(resolved_by, '')) <> ''
            AND cardinality(evidence_refs) > 0))
);

CREATE INDEX eventing_quarantine_case_open_idx ON eventing_quarantine_case (sla_due_at)
    WHERE resolved_at IS NULL;

ALTER TABLE eventing_quarantine_case ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_quarantine_case FORCE ROW LEVEL SECURITY;
CREATE POLICY eventing_quarantine_case_tenant_isolation ON eventing_quarantine_case FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
`

// Failure classes (section 11 table).
const (
	ClassTransientInfra       = "transient-infrastructure"
	ClassConsumerCapacity     = "consumer-capacity"
	ClassSchemaContract       = "schema-contract"
	ClassBusinessPrecondition = "business-precondition"
	ClassPermanentData        = "permanent-data"
	ClassExternalProvider     = "external-provider"
	ClassPoison               = "poison-event"
)

// Dispositions (section 11.1).
const (
	DispFixAndReplay       = "fix-and-replay"
	DispCompensatingAction = "compensating-action"
	DispProducerCorrection = "producer-correction"
	DispAcceptedException  = "accepted-exception"
	DispPermanentRejection = "permanent-rejection"
)

var validClass = map[string]bool{ClassTransientInfra: true, ClassConsumerCapacity: true, ClassSchemaContract: true,
	ClassBusinessPrecondition: true, ClassPermanentData: true, ClassExternalProvider: true, ClassPoison: true}
var validDisposition = map[string]bool{DispFixAndReplay: true, DispCompensatingAction: true, DispProducerCorrection: true,
	DispAcceptedException: true, DispPermanentRejection: true}
var validPriority = map[string]bool{"low": true, "normal": true, "high": true, "critical": true}

// Case errors.
var (
	ErrCaseExists       = errors.New("inbox: a quarantine case already exists for that event")
	ErrNotQuarantined   = errors.New("inbox: only a quarantined receipt can have a case")
	ErrCaseResolved     = errors.New("inbox: the case is already resolved")
	ErrCaseNotFound     = errors.New("inbox: no quarantine case for that event")
	ErrInvalidCase      = errors.New("inbox: invalid quarantine case")
	ErrEvidenceRequired = errors.New("inbox: resolving a case requires at least one evidence reference")
)

// CaseOpen describes the ownership of a quarantined event.
type CaseOpen struct {
	Owner        string
	Priority     string // low | normal | high | critical
	FailureClass string
	SLA          time.Duration // time from now until the case is overdue
}

// Case is one quarantine case (section 11.1).
type Case struct {
	CaseID         string
	Consumer       string
	TenantID       string
	EventID        string
	EventType      string
	Region         string
	FailureClass   string
	ErrorCode      string
	PayloadHash    string
	AttemptCount   int
	FirstFailureAt time.Time
	LastFailureAt  time.Time
	Owner          string
	Priority       string
	SLADueAt       time.Time
	Disposition    string
	EvidenceRefs   []string
	ResolvedBy     string
	ResolvedAt     *time.Time
}

// Overdue reports whether an open case is past its SLA at now.
func (c Case) Overdue(now time.Time) bool { return c.ResolvedAt == nil && now.After(c.SLADueAt) }

const caseColumns = `case_id::text, consumer, tenant_id, event_id, event_type, COALESCE(region,''), failure_class,
	error_code, payload_hash, attempt_count, first_failure_at, last_failure_at, owner, priority, sla_due_at,
	COALESCE(disposition,''), evidence_refs, COALESCE(resolved_by,''), resolved_at`

func scanCase(row pgx.Row) (*Case, error) {
	var c Case
	err := row.Scan(&c.CaseID, &c.Consumer, &c.TenantID, &c.EventID, &c.EventType, &c.Region, &c.FailureClass,
		&c.ErrorCode, &c.PayloadHash, &c.AttemptCount, &c.FirstFailureAt, &c.LastFailureAt, &c.Owner, &c.Priority,
		&c.SLADueAt, &c.Disposition, &c.EvidenceRefs, &c.ResolvedBy, &c.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (o CaseOpen) validate() error {
	switch {
	case strings.TrimSpace(o.Owner) == "":
		return fmt.Errorf("%w: an owner is required", ErrInvalidCase)
	case !validPriority[o.Priority]:
		return fmt.Errorf("%w: priority %q", ErrInvalidCase, o.Priority)
	case !validClass[o.FailureClass]:
		return fmt.Errorf("%w: failure class %q", ErrInvalidCase, o.FailureClass)
	case o.SLA <= 0:
		return fmt.Errorf("%w: SLA must be positive", ErrInvalidCase)
	}
	return nil
}

// OpenCase gives a quarantined receipt a named owner, priority and SLA. The
// event facts (type, region, payload hash, attempts, first/last failure) are
// copied from the receipt in the same transaction, so the case cannot describe
// a different event than the one held.
func (i *Inbox) OpenCase(ctx context.Context, tenantID, eventID string, o CaseOpen) (*Case, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	var out *Case
	err = i.db.InTx(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO eventing_quarantine_case
			    (case_id, consumer, tenant_id, event_id, event_type, region, failure_class, error_code, payload_hash,
			     attempt_count, first_failure_at, last_failure_at, owner, priority, sla_due_at)
			SELECT $1, consumer, tenant_id, event_id, event_type, region, $4, COALESCE(last_error_code, 'QUARANTINED'),
			       payload_hash, attempt_count, received_at, updated_at, $5, $6, now() + make_interval(secs => $7)
			  FROM eventing_inbox
			 WHERE consumer = $2 AND tenant_id = $3 AND event_id = $8 AND state = 'quarantined'
			ON CONFLICT (consumer, tenant_id, event_id) DO NOTHING`,
			id, i.consumer, tenantID, o.FailureClass, o.Owner, o.Priority, o.SLA.Seconds(), eventID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM eventing_quarantine_case
				WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3)`, i.consumer, tenantID, eventID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return ErrCaseExists
			}
			return ErrNotQuarantined
		}
		out, err = scanCase(tx.QueryRow(ctx, `SELECT `+caseColumns+` FROM eventing_quarantine_case
			WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3`, i.consumer, tenantID, eventID))
		return err
	})
	return out, err
}

// GetCase returns the case for an event.
func (i *Inbox) GetCase(ctx context.Context, tenantID, eventID string) (*Case, error) {
	var c *Case
	err := i.db.InTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		c, err = scanCase(tx.QueryRow(ctx, `SELECT `+caseColumns+` FROM eventing_quarantine_case
			WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3`, i.consumer, tenantID, eventID))
		return err
	})
	return c, err
}

// ResolveCase records the owner's decision. It needs a disposition, the person
// who decided, and at least one evidence reference; it does not itself requeue
// or discard the event - a fix-and-replay disposition is carried out through
// Requeue, which has its own approval and repair log.
func (i *Inbox) ResolveCase(ctx context.Context, tenantID, eventID, disposition, resolvedBy string, evidence []string) error {
	if !validDisposition[disposition] {
		return fmt.Errorf("%w: disposition %q", ErrInvalidCase, disposition)
	}
	if strings.TrimSpace(resolvedBy) == "" {
		return fmt.Errorf("%w: resolver is required", ErrInvalidCase)
	}
	if len(evidence) == 0 {
		return ErrEvidenceRequired
	}
	for _, e := range evidence {
		if strings.TrimSpace(e) == "" {
			return ErrEvidenceRequired
		}
	}
	return i.db.InTx(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE eventing_quarantine_case
			   SET disposition = $4, resolved_by = $5, resolved_at = now(), evidence_refs = $6
			 WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3 AND resolved_at IS NULL`,
			i.consumer, tenantID, eventID, disposition, resolvedBy, evidence)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var resolved bool
		err = tx.QueryRow(ctx, `SELECT resolved_at IS NOT NULL FROM eventing_quarantine_case
			WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3`, i.consumer, tenantID, eventID).Scan(&resolved)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCaseNotFound
		}
		if err != nil {
			return err
		}
		return ErrCaseResolved
	})
}

// ListOpenCases returns unresolved cases, most overdue first.
func (i *Inbox) ListOpenCases(ctx context.Context, tenantID string, limit int) ([]Case, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Case
	err := i.db.InTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+caseColumns+` FROM eventing_quarantine_case
			WHERE consumer = $1 AND tenant_id = $2 AND resolved_at IS NULL ORDER BY sla_due_at LIMIT $3`,
			i.consumer, tenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCase(rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	return out, err
}

// UnownedQuarantined counts quarantined receipts that have no case yet - the
// "unowned DLQ" anti-pattern (section 27). Alert when it is non-zero for longer
// than the team's triage window.
func (i *Inbox) UnownedQuarantined(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := i.db.InTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM eventing_inbox i
			 WHERE i.consumer = $1 AND i.tenant_id = $2 AND i.state = 'quarantined'
			   AND NOT EXISTS (SELECT 1 FROM eventing_quarantine_case c
			                    WHERE c.consumer = i.consumer AND c.tenant_id = i.tenant_id AND c.event_id = i.event_id)`,
			i.consumer, tenantID).Scan(&n)
	})
	return n, err
}
