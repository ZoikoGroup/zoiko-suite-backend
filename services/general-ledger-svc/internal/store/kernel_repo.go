package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	"zoiko.io/general-ledger-svc/internal/kernel"
)

// KernelRepo is the Postgres implementation of kernel.Repo (migration 000018). Every method sets the tenant for row-level
// security AND filters by tenant in its own SQL: the pool may connect as a role that bypasses RLS.
type KernelRepo struct {
	pool        *pgxpool.Pool
	eventRegion string
}

// NewKernelRepo builds the repository.
func NewKernelRepo(pool *pgxpool.Pool, eventRegion string) *KernelRepo {
	return &KernelRepo{pool: pool, eventRegion: eventRegion}
}

var _ kernel.Repo = (*KernelRepo)(nil)

func (r *KernelRepo) tx(ctx context.Context, tenant string, fn func(pgx.Tx) error) error {
	if _, err := uuid.Parse(tenant); err != nil {
		return kernel.ErrNotFound
	}
	t, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer t.Rollback(ctx) //nolint:errcheck
	if _, err := t.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		return err
	}
	if err := fn(t); err != nil {
		return err
	}
	return t.Commit(ctx)
}

func (r *KernelRepo) enqueue(ctx context.Context, t pgx.Tx, tenant string, o kernel.OutEvent) error {
	if o.Fact == "" {
		return nil
	}
	env, err := envelope.New(envelope.Spec{
		Type: "com.zoikosuite.accounting.kernel." + o.Fact, Service: "general-ledger-svc", SchemaVersion: "1.0.0", OccurredAt: time.Now().UTC(),
		TenantID: tenant, LegalEntityID: o.LegalEntityID, AggregateType: o.AggregateType, AggregateID: o.AggregateID, CorrelationID: o.CorrelationID,
		ActorID: o.Actor, ResidencyRegion: r.eventRegion, Classification: envelope.Confidential, Data: o.Data,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", o.Fact, err)
	}
	return outbox.Enqueue(ctx, t, env)
}

// kernelErr maps database refusals to the kernel's sentinels.
func kernelErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return kernel.ErrNotFound
	}
	msg := err.Error()
	switch pgCode(err) {
	case "23505":
		return kernel.ErrExists
	case "23514":
		switch {
		case strings.Contains(msg, "author_approver"), strings.Contains(msg, "author_activator"):
			return kernel.ErrSelfApproval
		}
	case "23000":
		switch {
		case strings.Contains(msg, "already covers"):
			return kernel.ErrOverlap
		case strings.Contains(msg, "someone other than"):
			return kernel.ErrSelfApproval
		case strings.Contains(msg, "test pack has not passed"):
			return kernel.ErrNotTested
		case strings.Contains(msg, "version must advance"):
			return kernel.ErrVersion
		default:
			return kernel.ErrState
		}
	}
	return err
}

func pgCode(err error) string {
	var pe interface{ SQLState() string }
	if errors.As(err, &pe) {
		return pe.SQLState()
	}
	return ""
}

func jsonOrNil(v any) any {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

func dateOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// ── books ──────────────────────────────────────────────────────────────────────

const bookCols = `book_id::text, tenant_id::text, legal_entity_id::text, book_code, book_type, accounting_framework_id, functional_currency, fiscal_calendar_id,
	chart_of_accounts_id, default_ledger_id, effective_from, effective_to, status, created_by, version, created_at, updated_at`

func scanBook(row pgx.Row, b *kernel.Book) error {
	return row.Scan(&b.BookID, &b.TenantID, &b.LegalEntityID, &b.BookCode, &b.BookType, &b.AccountingFrameworkID, &b.FunctionalCurrency, &b.FiscalCalendarID,
		&b.ChartOfAccountsID, &b.DefaultLedgerID, &b.EffectiveFrom, &b.EffectiveTo, &b.Status, &b.CreatedBy, &b.Version, &b.CreatedAt, &b.UpdatedAt)
}

// InsertBook implements kernel.Repo.
func (r *KernelRepo) InsertBook(ctx context.Context, b *kernel.Book, out kernel.OutEvent) error {
	return kernelErr(r.tx(ctx, b.TenantID, func(t pgx.Tx) error {
		if _, err := t.Exec(ctx, `INSERT INTO accounting_books (book_id, tenant_id, legal_entity_id, book_code, book_type, accounting_framework_id, functional_currency, fiscal_calendar_id,
			chart_of_accounts_id, default_ledger_id, effective_from, effective_to, status, created_by, version, created_at, updated_at)
			VALUES ($1::uuid,$2::uuid,$3::uuid,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, b.BookID, b.TenantID, b.LegalEntityID, b.BookCode, b.BookType,
			b.AccountingFrameworkID, b.FunctionalCurrency, b.FiscalCalendarID, b.ChartOfAccountsID, b.DefaultLedgerID, b.EffectiveFrom, dateOrNil(b.EffectiveTo), b.Status,
			b.CreatedBy, b.Version, b.CreatedAt, b.UpdatedAt); err != nil {
			return err
		}
		return r.enqueue(ctx, t, b.TenantID, out)
	}))
}

// GetBook implements kernel.Repo.
func (r *KernelRepo) GetBook(ctx context.Context, tenant, id string) (*kernel.Book, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var b kernel.Book
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanBook(t.QueryRow(ctx, `SELECT `+bookCols+` FROM accounting_books WHERE tenant_id=$1::uuid AND book_id=$2::uuid`, tenant, id), &b)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &b, nil
}

// ListBooks implements kernel.Repo.
func (r *KernelRepo) ListBooks(ctx context.Context, tenant string, f kernel.BookFilter) ([]kernel.Book, error) {
	limit := clampLimit(f.Limit)
	var ent any
	if f.LegalEntityID != "" {
		if _, err := uuid.Parse(f.LegalEntityID); err != nil {
			return []kernel.Book{}, nil
		}
		ent = f.LegalEntityID
	}
	out := []kernel.Book{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT `+bookCols+` FROM accounting_books WHERE tenant_id=$1::uuid AND ($2::uuid IS NULL OR legal_entity_id=$2::uuid)
			AND ($3='' OR status=$3) ORDER BY legal_entity_id, book_code LIMIT $4`, tenant, ent, f.Status, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b kernel.Book
			if err := scanBook(rows, &b); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

func clampLimit(n int) int {
	if n < 1 || n > 500 {
		return 100
	}
	return n
}

// UpdateBook implements kernel.Repo.
func (r *KernelRepo) UpdateBook(ctx context.Context, tenant, id string, fn func(*kernel.Book) (kernel.OutEvent, error)) (*kernel.Book, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var out kernel.Book
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		var cur kernel.Book
		if err := scanBook(t.QueryRow(ctx, `SELECT `+bookCols+` FROM accounting_books WHERE tenant_id=$1::uuid AND book_id=$2::uuid FOR UPDATE`, tenant, id), &cur); err != nil {
			return err
		}
		w := cur
		ev, err := fn(&w)
		if err != nil {
			return err
		}
		if _, err := t.Exec(ctx, `UPDATE accounting_books SET status=$3, effective_to=$4, version=version+1, updated_at=now() WHERE tenant_id=$1::uuid AND book_id=$2::uuid`,
			tenant, id, w.Status, dateOrNil(w.EffectiveTo)); err != nil {
			return err
		}
		if err := r.enqueue(ctx, t, tenant, ev); err != nil {
			return err
		}
		return scanBook(t.QueryRow(ctx, `SELECT `+bookCols+` FROM accounting_books WHERE tenant_id=$1::uuid AND book_id=$2::uuid`, tenant, id), &out)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &out, nil
}

// ── events ─────────────────────────────────────────────────────────────────────

const eventCols = `accounting_event_id::text, tenant_id::text, legal_entity_id::text, event_type, event_version, source_system, source_object_type, source_object_id,
	source_event_id, occurred_at, effective_at, document_date, posting_date_requested, COALESCE(transaction_currency, ''), facts, COALESCE(counterparty_id, ''),
	dimensions, evidence_refs, idempotency_key, correlation_id, causation_id, payload_hash, registered_by, registered_at, COALESCE(supersedes_event_id::text, '')`

func scanEvent(row pgx.Row, e *kernel.AccountingEvent) error {
	var facts, dims, ev []byte
	if err := row.Scan(&e.AccountingEventID, &e.TenantID, &e.LegalEntityID, &e.EventType, &e.EventVersion, &e.SourceSystem, &e.SourceObjectType, &e.SourceObjectID,
		&e.SourceEventID, &e.OccurredAt, &e.EffectiveAt, &e.DocumentDate, &e.PostingDateRequested, &e.TransactionCurrency, &facts, &e.CounterpartyID, &dims, &ev,
		&e.IdempotencyKey, &e.CorrelationID, &e.CausationID, &e.PayloadHash, &e.RegisteredBy, &e.RegisteredAt, &e.SupersedesEventID); err != nil {
		return err
	}
	e.Facts = map[string]string{}
	_ = json.Unmarshal(facts, &e.Facts)
	if len(dims) > 0 {
		_ = json.Unmarshal(dims, &e.Dimensions)
	}
	if len(ev) > 0 {
		_ = json.Unmarshal(ev, &e.EvidenceRefs)
	}
	return nil
}

// InsertEvent implements kernel.Repo.
func (r *KernelRepo) InsertEvent(ctx context.Context, e *kernel.AccountingEvent, out kernel.OutEvent) (*kernel.AccountingEvent, bool, error) {
	var stored kernel.AccountingEvent
	existing := false
	err := r.tx(ctx, e.TenantID, func(t pgx.Tx) error {
		facts, _ := json.Marshal(e.Facts)
		tag, err := t.Exec(ctx, `INSERT INTO accounting_events (accounting_event_id, tenant_id, legal_entity_id, event_type, event_version, source_system, source_object_type,
			source_object_id, source_event_id, occurred_at, effective_at, document_date, posting_date_requested, transaction_currency, facts, counterparty_id, dimensions,
			evidence_refs, idempotency_key, correlation_id, causation_id, payload_hash, registered_by, registered_at, supersedes_event_id)
			VALUES ($1::uuid,$2::uuid,$3::uuid,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,''),$15,NULLIF($16,''),$17,$18,$19,$20,$21,$22,$23,$24,NULLIF($25,'')::uuid) ON CONFLICT DO NOTHING`,
			e.AccountingEventID, e.TenantID, e.LegalEntityID, e.EventType, e.EventVersion, e.SourceSystem, e.SourceObjectType, e.SourceObjectID, e.SourceEventID, e.OccurredAt,
			e.EffectiveAt, e.DocumentDate, e.PostingDateRequested, e.TransactionCurrency, facts, e.CounterpartyID, jsonOrNil(e.Dimensions), jsonOrNil(e.EvidenceRefs),
			e.IdempotencyKey, e.CorrelationID, e.CausationID, e.PayloadHash, e.RegisteredBy, e.RegisteredAt, e.SupersedesEventID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			stored = *e
			return r.enqueue(ctx, t, e.TenantID, out)
		}
		// the same producer event, or the same idempotency key, is already stored
		err = scanEvent(t.QueryRow(ctx, `SELECT `+eventCols+` FROM accounting_events WHERE tenant_id=$1::uuid AND
			((source_system=$2 AND source_event_id=$3 AND event_version=$4) OR idempotency_key=$5) ORDER BY registered_at LIMIT 1`,
			e.TenantID, e.SourceSystem, e.SourceEventID, e.EventVersion, e.IdempotencyKey), &stored)
		if err != nil {
			return err
		}
		existing = true
		if stored.PayloadHash != e.PayloadHash {
			return &kernel.Exception{Family: kernel.FamData, Code: "SOURCE_EVENT_CONFLICT",
				Detail: "this producer event or idempotency key was already registered with different content", Facts: map[string]any{"accounting_event_id": stored.AccountingEventID}}
		}
		return nil
	})
	if err != nil {
		if _, ok := kernel.AsException(err); ok {
			return nil, existing, err
		}
		return nil, false, kernelErr(err)
	}
	return &stored, existing, nil
}

// GetEvent implements kernel.Repo.
func (r *KernelRepo) GetEvent(ctx context.Context, tenant, id string) (*kernel.AccountingEvent, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var e kernel.AccountingEvent
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanEvent(t.QueryRow(ctx, `SELECT `+eventCols+` FROM accounting_events WHERE tenant_id=$1::uuid AND accounting_event_id=$2::uuid`, tenant, id), &e)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &e, nil
}

// ListEvents implements kernel.Repo.
func (r *KernelRepo) ListEvents(ctx context.Context, tenant string, f kernel.EventFilter) ([]kernel.AccountingEvent, error) {
	var ent any
	if f.LegalEntityID != "" {
		if _, err := uuid.Parse(f.LegalEntityID); err != nil {
			return []kernel.AccountingEvent{}, nil
		}
		ent = f.LegalEntityID
	}
	out := []kernel.AccountingEvent{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT `+eventCols+` FROM accounting_events WHERE tenant_id=$1::uuid AND ($2::uuid IS NULL OR legal_entity_id=$2::uuid)
			AND ($3='' OR event_type=$3) AND ($4='' OR source_object_id=$4) ORDER BY registered_at DESC, accounting_event_id LIMIT $5`,
			tenant, ent, f.EventType, f.SourceObject, clampLimit(f.Limit))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e kernel.AccountingEvent
			if err := scanEvent(rows, &e); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// ── rule sets ──────────────────────────────────────────────────────────────────

const ruleCols = `posting_rule_set_id::text, tenant_id::text, name, domain, event_type, book_type, accounting_framework_id, version, status, effective_from, effective_to,
	content, content_hash, test_pack, test_pack_id, COALESCE(tests_passed_hash, ''), source_authority_refs, created_by, COALESCE(approved_by, ''), approved_at,
	COALESCE(activated_by, ''), activated_at, row_version, created_at, updated_at`

func scanRule(row pgx.Row, x *kernel.RuleSet) error {
	var content, pack, auth []byte
	if err := row.Scan(&x.RuleSetID, &x.TenantID, &x.Name, &x.Domain, &x.EventType, &x.BookType, &x.FrameworkID, &x.Version, &x.Status, &x.EffectiveFrom, &x.EffectiveTo,
		&content, &x.ContentHash, &pack, &x.TestPackID, &x.TestsPassedHash, &auth, &x.CreatedBy, &x.ApprovedBy, &x.ApprovedAt, &x.ActivatedBy, &x.ActivatedAt,
		&x.RowVersion, &x.CreatedAt, &x.UpdatedAt); err != nil {
		return err
	}
	if err := json.Unmarshal(content, &x.Content); err != nil {
		return err
	}
	if err := json.Unmarshal(pack, &x.TestPack); err != nil {
		return err
	}
	if len(auth) > 0 {
		_ = json.Unmarshal(auth, &x.SourceAuthority)
	}
	return nil
}

func insertRuleEvent(ctx context.Context, t pgx.Tx, tenant, id string, ev kernel.RuleEvent) error {
	_, err := t.Exec(ctx, `INSERT INTO posting_rule_set_events (event_id, tenant_id, posting_rule_set_id, kind, actor_id, detail) VALUES ($1::uuid,$2::uuid,$3::uuid,$4,$5,$6)`,
		uuid.NewString(), tenant, id, ev.Kind, ev.Actor, jsonOrNil(ev.Detail))
	return err
}

// InsertRuleSet implements kernel.Repo.
func (r *KernelRepo) InsertRuleSet(ctx context.Context, x *kernel.RuleSet, ev kernel.RuleEvent, out kernel.OutEvent) error {
	content, _ := json.Marshal(x.Content)
	pack, _ := json.Marshal(x.TestPack)
	return kernelErr(r.tx(ctx, x.TenantID, func(t pgx.Tx) error {
		if _, err := t.Exec(ctx, `INSERT INTO posting_rule_sets (posting_rule_set_id, tenant_id, name, domain, event_type, book_type, accounting_framework_id, version, status,
			effective_from, effective_to, content, content_hash, test_pack, test_pack_id, source_authority_refs, created_by, row_version, created_at, updated_at)
			VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`, x.RuleSetID, x.TenantID, x.Name, x.Domain, x.EventType, x.BookType,
			x.FrameworkID, x.Version, x.Status, x.EffectiveFrom, dateOrNil(x.EffectiveTo), content, x.ContentHash, pack, x.TestPackID, jsonOrNil(x.SourceAuthority),
			x.CreatedBy, x.RowVersion, x.CreatedAt, x.UpdatedAt); err != nil {
			return err
		}
		if err := insertRuleEvent(ctx, t, x.TenantID, x.RuleSetID, ev); err != nil {
			return err
		}
		return r.enqueue(ctx, t, x.TenantID, out)
	}))
}

// GetRuleSet implements kernel.Repo.
func (r *KernelRepo) GetRuleSet(ctx context.Context, tenant, id string) (*kernel.RuleSet, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var x kernel.RuleSet
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanRule(t.QueryRow(ctx, `SELECT `+ruleCols+` FROM posting_rule_sets WHERE tenant_id=$1::uuid AND posting_rule_set_id=$2::uuid`, tenant, id), &x)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &x, nil
}

// ListRuleSets implements kernel.Repo.
func (r *KernelRepo) ListRuleSets(ctx context.Context, tenant string, f kernel.RuleFilter) ([]kernel.RuleSet, error) {
	out := []kernel.RuleSet{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT `+ruleCols+` FROM posting_rule_sets WHERE tenant_id=$1::uuid AND ($2='' OR event_type=$2) AND ($3='' OR status=$3) AND ($4='' OR name=$4)
			ORDER BY name, effective_from DESC, version DESC LIMIT $5`, tenant, f.EventType, f.Status, f.Name, clampLimit(f.Limit))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x kernel.RuleSet
			if err := scanRule(rows, &x); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

func updateRule(ctx context.Context, t pgx.Tx, tenant string, w *kernel.RuleSet) error {
	_, err := t.Exec(ctx, `UPDATE posting_rule_sets SET status=$3, effective_to=$4, tests_passed_hash=NULLIF($5,''), approved_by=NULLIF($6,''), approved_at=$7,
		activated_by=NULLIF($8,''), activated_at=$9, row_version=row_version+1, updated_at=now() WHERE tenant_id=$1::uuid AND posting_rule_set_id=$2::uuid`,
		tenant, w.RuleSetID, w.Status, dateOrNil(w.EffectiveTo), w.TestsPassedHash, w.ApprovedBy, w.ApprovedAt, w.ActivatedBy, w.ActivatedAt)
	return err
}

// UpdateRuleSet implements kernel.Repo.
func (r *KernelRepo) UpdateRuleSet(ctx context.Context, tenant, id string, fn func(*kernel.RuleSet) (kernel.RuleEvent, kernel.OutEvent, error)) (*kernel.RuleSet, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var out kernel.RuleSet
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		var cur kernel.RuleSet
		if err := scanRule(t.QueryRow(ctx, `SELECT `+ruleCols+` FROM posting_rule_sets WHERE tenant_id=$1::uuid AND posting_rule_set_id=$2::uuid FOR UPDATE`, tenant, id), &cur); err != nil {
			return err
		}
		w := cur
		ev, oe, err := fn(&w)
		if err != nil {
			return err
		}
		ev.At = time.Now().UTC()
		if err := updateRule(ctx, t, tenant, &w); err != nil {
			return err
		}
		if err := insertRuleEvent(ctx, t, tenant, id, ev); err != nil {
			return err
		}
		if err := r.enqueue(ctx, t, tenant, oe); err != nil {
			return err
		}
		return scanRule(t.QueryRow(ctx, `SELECT `+ruleCols+` FROM posting_rule_sets WHERE tenant_id=$1::uuid AND posting_rule_set_id=$2::uuid`, tenant, id), &out)
	})
	if err != nil {
		if _, ok := kernel.AsException(err); ok {
			return nil, err
		}
		return nil, kernelErr(err)
	}
	return &out, nil
}

// ActivateRuleSet implements kernel.Repo.
func (r *KernelRepo) ActivateRuleSet(ctx context.Context, tenant, id string, expectedRowVersion int64, actor string, out kernel.OutEvent) (*kernel.RuleSet, []string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, nil, kernel.ErrNotFound
	}
	var res kernel.RuleSet
	var superseded []string
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		var cur kernel.RuleSet
		if err := scanRule(t.QueryRow(ctx, `SELECT `+ruleCols+` FROM posting_rule_sets WHERE tenant_id=$1::uuid AND posting_rule_set_id=$2::uuid FOR UPDATE`, tenant, id), &cur); err != nil {
			return err
		}
		if expectedRowVersion > 0 && cur.RowVersion != expectedRowVersion {
			return kernel.ErrVersion
		}
		if cur.Status != kernel.RuleApproved {
			return kernel.ErrState
		}
		// the scope lock the database trigger also takes: concurrent activations of one scope are serialized here first
		if _, err := t.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, tenant+"|"+cur.EventType+"|"+cur.BookType+"|"+cur.FrameworkID); err != nil {
			return err
		}
		// an active version that started before this one is closed where this one starts
		rows, err := t.Query(ctx, `SELECT `+ruleCols+` FROM posting_rule_sets WHERE tenant_id=$1::uuid AND event_type=$2 AND book_type=$3 AND accounting_framework_id=$4
			AND status='ACTIVE' AND posting_rule_set_id<>$5::uuid AND effective_from < $6 AND (effective_to IS NULL OR effective_to > $6) FOR UPDATE`,
			tenant, cur.EventType, cur.BookType, cur.FrameworkID, id, cur.EffectiveFrom)
		if err != nil {
			return err
		}
		var prev []kernel.RuleSet
		for rows.Next() {
			var p kernel.RuleSet
			if err := scanRule(rows, &p); err != nil {
				rows.Close()
				return err
			}
			prev = append(prev, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, p := range prev {
			end := cur.EffectiveFrom
			p.Status, p.EffectiveTo = kernel.RuleSuperseded, &end
			if err := updateRule(ctx, t, tenant, &p); err != nil {
				return err
			}
			if err := insertRuleEvent(ctx, t, tenant, p.RuleSetID, kernel.RuleEvent{Kind: "SUPERSEDED", Actor: actor, At: now, Detail: map[string]any{"by": id}}); err != nil {
				return err
			}
			superseded = append(superseded, p.RuleSetID)
		}
		w := cur
		w.Status, w.ActivatedBy, w.ActivatedAt = kernel.RuleActive, actor, &now
		if err := updateRule(ctx, t, tenant, &w); err != nil {
			return err
		}
		if err := insertRuleEvent(ctx, t, tenant, id, kernel.RuleEvent{Kind: "ACTIVATED", Actor: actor, At: now, Detail: map[string]any{"superseded": superseded}}); err != nil {
			return err
		}
		if out.Data != nil {
			out.Data["superseded"] = superseded
		}
		if err := r.enqueue(ctx, t, tenant, out); err != nil {
			return err
		}
		return scanRule(t.QueryRow(ctx, `SELECT `+ruleCols+` FROM posting_rule_sets WHERE tenant_id=$1::uuid AND posting_rule_set_id=$2::uuid`, tenant, id), &res)
	})
	if err != nil {
		return nil, nil, kernelErr(err)
	}
	return &res, superseded, nil
}

// ActiveRuleSets implements kernel.Repo.
func (r *KernelRepo) ActiveRuleSets(ctx context.Context, tenant, eventType, bookType, framework string, on time.Time) ([]kernel.RuleSet, error) {
	out := []kernel.RuleSet{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT `+ruleCols+` FROM posting_rule_sets WHERE tenant_id=$1::uuid AND event_type=$2 AND book_type=$3 AND accounting_framework_id=$4
			AND status='ACTIVE' AND effective_from <= $5 AND (effective_to IS NULL OR effective_to > $5) ORDER BY effective_from DESC`, tenant, eventType, bookType, framework, on)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x kernel.RuleSet
			if err := scanRule(rows, &x); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// RuleSetEvents implements kernel.Repo.
func (r *KernelRepo) RuleSetEvents(ctx context.Context, tenant, id string) ([]kernel.RuleEvent, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	out := []kernel.RuleEvent{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		var one int
		if err := t.QueryRow(ctx, `SELECT 1 FROM posting_rule_sets WHERE tenant_id=$1::uuid AND posting_rule_set_id=$2::uuid`, tenant, id).Scan(&one); err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT kind, actor_id, detail, created_at FROM posting_rule_set_events WHERE tenant_id=$1::uuid AND posting_rule_set_id=$2::uuid ORDER BY created_at, event_id`, tenant, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e kernel.RuleEvent
			var d []byte
			if err := rows.Scan(&e.Kind, &e.Actor, &d, &e.At); err != nil {
				return err
			}
			if len(d) > 0 {
				_ = json.Unmarshal(d, &e.Detail)
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// ── dimensions ─────────────────────────────────────────────────────────────────

// InsertDimension implements kernel.Repo.
func (r *KernelRepo) InsertDimension(ctx context.Context, tenant string, d *kernel.Dimension) error {
	return kernelErr(r.tx(ctx, tenant, func(t pgx.Tx) error {
		_, err := t.Exec(ctx, `INSERT INTO dimension_definitions (tenant_id, dimension_code, name, status, created_by, created_at) VALUES ($1::uuid,$2,$3,$4,$5,$6)`,
			tenant, d.DimensionCode, d.Name, d.Status, d.CreatedBy, d.CreatedAt)
		return err
	}))
}

// ListDimensions implements kernel.Repo.
func (r *KernelRepo) ListDimensions(ctx context.Context, tenant string) ([]kernel.Dimension, error) {
	out := []kernel.Dimension{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT dimension_code, name, status, created_by, created_at FROM dimension_definitions WHERE tenant_id=$1::uuid ORDER BY dimension_code`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d kernel.Dimension
			if err := rows.Scan(&d.DimensionCode, &d.Name, &d.Status, &d.CreatedBy, &d.CreatedAt); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// InsertDimensionValue implements kernel.Repo.
func (r *KernelRepo) InsertDimensionValue(ctx context.Context, tenant string, v *kernel.DimensionValue) error {
	return kernelErr(r.tx(ctx, tenant, func(t pgx.Tx) error {
		var ent any
		if v.LegalEntityID != "" {
			ent = v.LegalEntityID
		}
		var parent any
		if v.ParentValueCode != "" {
			parent = v.ParentValueCode
		}
		_, err := t.Exec(ctx, `INSERT INTO dimension_values (value_id, tenant_id, dimension_code, value_code, name, legal_entity_id, parent_value_code, valid_from, valid_to, status, created_by, created_at)
			VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6::uuid,$7,$8,$9,$10,$11,$12)`, v.ValueID, tenant, v.DimensionCode, v.ValueCode, v.Name, ent, parent, v.ValidFrom, dateOrNil(v.ValidTo), v.Status, v.CreatedBy, v.CreatedAt)
		return err
	}))
}

// RetireDimensionValue implements kernel.Repo.
func (r *KernelRepo) RetireDimensionValue(ctx context.Context, tenant, valueID, actor string, on time.Time) (*kernel.DimensionValue, error) {
	if _, err := uuid.Parse(valueID); err != nil {
		return nil, kernel.ErrNotFound
	}
	var v kernel.DimensionValue
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		tag, err := t.Exec(ctx, `UPDATE dimension_values SET status='RETIRED', valid_to=GREATEST($3::date, valid_from + 1) WHERE tenant_id=$1::uuid AND value_id=$2::uuid AND status='ACTIVE'`, tenant, valueID, on)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return scanDimValue(t.QueryRow(ctx, dimValueSelect+` WHERE tenant_id=$1::uuid AND value_id=$2::uuid`, tenant, valueID), &v)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &v, nil
}

const dimValueSelect = `SELECT value_id::text, dimension_code, value_code, name, COALESCE(legal_entity_id::text, ''), COALESCE(parent_value_code, ''), valid_from, valid_to, status, created_by, created_at FROM dimension_values`

func scanDimValue(row pgx.Row, v *kernel.DimensionValue) error {
	return row.Scan(&v.ValueID, &v.DimensionCode, &v.ValueCode, &v.Name, &v.LegalEntityID, &v.ParentValueCode, &v.ValidFrom, &v.ValidTo, &v.Status, &v.CreatedBy, &v.CreatedAt)
}

// ListDimensionValues implements kernel.Repo.
func (r *KernelRepo) ListDimensionValues(ctx context.Context, tenant, dimension string) ([]kernel.DimensionValue, error) {
	out := []kernel.DimensionValue{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, dimValueSelect+` WHERE tenant_id=$1::uuid AND ($2='' OR dimension_code=$2) ORDER BY dimension_code, value_code LIMIT 1000`, tenant, dimension)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v kernel.DimensionValue
			if err := scanDimValue(rows, &v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// ── the evaluation directory (reference data read by the engine) ────────────────

// DimensionAllowed reports whether a value is active on the date for the entity (an entity-scoped value applies to that
// entity; an unscoped one to every entity of the tenant).
func (r *KernelRepo) DimensionAllowed(ctx context.Context, tenant, dimension, value, legalEntityID string, on time.Time) (bool, error) {
	ok := false
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return t.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dimension_values v JOIN dimension_definitions d USING (tenant_id, dimension_code)
			WHERE v.tenant_id=$1::uuid AND v.dimension_code=$2 AND v.value_code=$3 AND d.status='ACTIVE' AND v.valid_from <= $5 AND (v.valid_to IS NULL OR v.valid_to > $5)
			AND (v.legal_entity_id IS NULL OR v.legal_entity_id=$4::uuid) AND (v.status='ACTIVE' OR v.valid_to > $5))`, tenant, dimension, value, legalEntityID, on).Scan(&ok)
	})
	return ok, err
}

// AccountFacts reads what the chart says about an account (and whether it is a header with children).
func (r *KernelRepo) AccountFacts(ctx context.Context, tenant, code string) (active, postable bool, found bool, err error) {
	err = r.tx(ctx, tenant, func(t pgx.Tx) error {
		var status string
		var hasChildren bool
		e := t.QueryRow(ctx, `SELECT a.status, EXISTS (SELECT 1 FROM chart_of_accounts c WHERE c.tenant_id=a.tenant_id AND c.parent_account_id=a.account_id)
			FROM chart_of_accounts a WHERE a.tenant_id=$1::uuid AND a.account_code=$2`, tenant, code).Scan(&status, &hasChildren)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		found, active, postable = true, status == "ACTIVE", !hasChildren
		return nil
	})
	return
}

// MappedAccount resolves a mapping key to its account code on the date.
func (r *KernelRepo) MappedAccount(ctx context.Context, tenant, key string, on time.Time) (code string, found bool, err error) {
	err = r.tx(ctx, tenant, func(t pgx.Tx) error {
		e := t.QueryRow(ctx, `SELECT account_code FROM account_mappings WHERE tenant_id=$1::uuid AND mapping_key=$2 AND effective_from::date <= $3::date AND (effective_to IS NULL OR effective_to::date > $3::date)
			ORDER BY effective_from DESC LIMIT 1`, tenant, key, on).Scan(&code)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		found = true
		return nil
	})
	return
}

// ── account applicability (standard §12) ───────────────────────────────────────

var _ kernel.AccountApplicability = (*KernelRepo)(nil)

// AccountApplicability implements kernel.AccountApplicability: the books an account is valid in and its currency restriction.
// An account with neither is valid everywhere, which is every account that existed before applicability did.
func (r *KernelRepo) AccountApplicability(ctx context.Context, tenant, code string) (books []string, currency string, err error) {
	err = r.tx(ctx, tenant, func(t pgx.Tx) error {
		var ccy *string
		e := t.QueryRow(ctx, `SELECT COALESCE(applicable_book_ids, '{}'::uuid[])::text[], currency_restriction FROM chart_of_accounts WHERE tenant_id=$1::uuid AND account_code=$2`,
			tenant, code).Scan(&books, &ccy)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if ccy != nil {
			currency = strings.TrimSpace(*ccy)
		}
		return nil
	})
	return
}

// SetAccountApplicability restricts an account to the given books (empty = every book) and to one currency (empty = any).
// It changes the head row of the chart only; the reason it is safe is that applicability is checked at posting time against the
// current value and every posting records the rule and accounts it used.
func (r *KernelRepo) SetAccountApplicability(ctx context.Context, tenant, code string, books []string, currency string, actor kernel.Actor) error {
	return r.tx(ctx, tenant, func(t pgx.Tx) error {
		for _, b := range books {
			var ok bool
			if e := t.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounting_books WHERE tenant_id=$1::uuid AND book_id=$2::uuid)`, tenant, b).Scan(&ok); e != nil {
				return e
			}
			if !ok {
				return &kernel.Exception{Family: kernel.FamCOA, Code: "BOOK_UNKNOWN", Detail: "book " + b + " does not exist", Facts: map[string]any{"book_id": b}}
			}
		}
		var arg any
		if len(books) > 0 {
			arg = books
		}
		tag, e := t.Exec(ctx, `UPDATE chart_of_accounts SET applicable_book_ids=$3::uuid[], currency_restriction=NULLIF($4,'') WHERE tenant_id=$1::uuid AND account_code=$2`,
			tenant, code, arg, currency)
		if e != nil {
			return e
		}
		if tag.RowsAffected() == 0 {
			return kernel.ErrNotFound
		}
		return r.enqueue(ctx, t, tenant, kernel.OutEvent{Fact: "account.applicability-changed", AggregateType: "account", AggregateID: code, Actor: actor.PrincipalID,
			CorrelationID: actor.CorrelationID, Data: map[string]any{"account_code": code, "book_ids": books, "currency_restriction": currency}})
	})
}
