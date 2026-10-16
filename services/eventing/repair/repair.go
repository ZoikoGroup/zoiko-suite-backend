// Package repair records privileged outbox/inbox repair and replay actions.
//
// ZS-SVC-M-001 §4.3: "privileged repair/replay requires JIT approval and
// evidence" and "repair tooling cannot mutate event payload". A repair is
// therefore never just a state change: it needs a named operator, a DIFFERENT
// named approver, an approval reference and a reason, and every one is written
// to an append-only log in the same transaction as the state change.
package repair

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TableName is the append-only repair log.
const TableName = "eventing_repair_log"

// SchemaSQL is copied verbatim into an adopting service's migration, beside the
// outbox/inbox DDL. Rows can be inserted and read, never changed or removed:
// the log is the evidence.
const SchemaSQL = `
CREATE TABLE eventing_repair_log (
    repair_id     UUID         PRIMARY KEY,
    target_kind   TEXT         NOT NULL,
    consumer      TEXT         NOT NULL DEFAULT '',
    tenant_id     TEXT         NOT NULL,
    event_id      TEXT         NOT NULL,
    action        TEXT         NOT NULL,
    operator      TEXT         NOT NULL,
    approver      TEXT         NOT NULL,
    approval_ref  TEXT         NOT NULL,
    reason        TEXT         NOT NULL,
    prior_state   TEXT         NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT eventing_repair_log_kind_valid CHECK (target_kind IN ('outbox', 'inbox')),
    CONSTRAINT eventing_repair_log_distinct_people CHECK (operator <> approver)
);

CREATE INDEX eventing_repair_log_event_idx ON eventing_repair_log (event_id, created_at);

CREATE FUNCTION eventing_repair_log_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'eventing_repair_log is append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER eventing_repair_log_no_change
    BEFORE UPDATE OR DELETE ON eventing_repair_log
    FOR EACH ROW EXECUTE FUNCTION eventing_repair_log_immutable();
`

// Authorization is who did a repair, who approved it, and why.
type Authorization struct {
	Operator    string // the person performing the repair
	Approver    string // a different person who approved it
	ApprovalRef string // the JIT approval record (ticket / approval id)
	Reason      string
}

// ErrNotAuthorized is returned for any repair lacking proper approval.
var ErrNotAuthorized = errors.New("repair: privileged repair needs an operator, a different approver, an approval reference and a reason")

// Validate enforces separation of duties and evidence.
func (a Authorization) Validate() error {
	op, ap := strings.TrimSpace(a.Operator), strings.TrimSpace(a.Approver)
	if op == "" || ap == "" || strings.TrimSpace(a.ApprovalRef) == "" || strings.TrimSpace(a.Reason) == "" {
		return ErrNotAuthorized
	}
	if strings.EqualFold(op, ap) {
		return errors.New("repair: operator and approver must be different people")
	}
	return nil
}

// Entry is one logged repair.
type Entry struct {
	Kind       string // "outbox" or "inbox"
	Consumer   string // inbox only
	TenantID   string
	EventID    string
	Action     string // e.g. "requeue", "replay"
	PriorState string
	Auth       Authorization
}

// Record appends e to the log inside tx. It validates the authorization first,
// so no caller can write a repair row (or change state beside it) without it.
func Record(ctx context.Context, tx pgx.Tx, e Entry) error {
	if err := e.Auth.Validate(); err != nil {
		return err
	}
	if e.Kind != "outbox" && e.Kind != "inbox" {
		return errors.New("repair: kind must be outbox or inbox")
	}
	if e.EventID == "" || e.TenantID == "" || e.Action == "" {
		return errors.New("repair: entry needs tenant, event and action")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO eventing_repair_log
		    (repair_id, target_kind, consumer, tenant_id, event_id, action,
		     operator, approver, approval_ref, reason, prior_state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		id, e.Kind, e.Consumer, e.TenantID, e.EventID, e.Action,
		strings.TrimSpace(e.Auth.Operator), strings.TrimSpace(e.Auth.Approver),
		strings.TrimSpace(e.Auth.ApprovalRef), strings.TrimSpace(e.Auth.Reason), e.PriorState)
	return err
}

// RLSSQL scopes the repair log to a tenant. The outbox relay repairs events
// across tenants under its own capability (app.outbox_relay), so that is
// admitted as well, exactly as for eventing_outbox.
const RLSSQL = `
ALTER TABLE eventing_repair_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_repair_log FORCE ROW LEVEL SECURITY;

CREATE POLICY eventing_repair_log_tenant_isolation ON eventing_repair_log FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
`
