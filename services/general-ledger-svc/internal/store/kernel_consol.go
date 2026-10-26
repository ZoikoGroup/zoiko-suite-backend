package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/kernel"
)

var _ kernel.ConsolRepo = (*KernelRepo)(nil)

func consolLock(ctx context.Context, t pgx.Tx, tenant, group string) error {
	_, err := t.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "consol:"+tenant+":"+group)
	return err
}

// ── groups and members ───────────────────────────────────────────────────────────────────────────────────────────────

// InsertGroup implements kernel.ConsolRepo.
func (r *KernelRepo) InsertGroup(ctx context.Context, tenant string, g *kernel.ConsolGroup, out kernel.OutEvent) error {
	return kernelErr(r.insertGroup(ctx, tenant, g, out))
}

func (r *KernelRepo) insertGroup(ctx context.Context, tenant string, g *kernel.ConsolGroup, out kernel.OutEvent) error {
	return r.tx(ctx, tenant, func(t pgx.Tx) error {
		if _, e := t.Exec(ctx, `INSERT INTO consolidation_groups (group_id, tenant_id, group_code, group_name, parent_entity_id, group_book_id, presentation_currency, ic_tolerance, status, created_by, created_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid, $6::uuid, $7, $8::numeric, $9, $10, $11)`,
			g.GroupID, tenant, g.GroupCode, g.GroupName, g.ParentEntityID, g.GroupBookID, g.PresentationCurrency, g.ICTolerance, g.Status, g.CreatedBy, g.CreatedAt); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, out)
	})
}

const groupCols = `group_id::text, group_code, group_name, parent_entity_id::text, group_book_id::text, presentation_currency, ic_tolerance::text, status, created_by, created_at`

func scanGroup(row pgx.Row, g *kernel.ConsolGroup) error {
	return row.Scan(&g.GroupID, &g.GroupCode, &g.GroupName, &g.ParentEntityID, &g.GroupBookID, &g.PresentationCurrency, &g.ICTolerance, &g.Status, &g.CreatedBy, &g.CreatedAt)
}

func loadMembers(ctx context.Context, t pgx.Tx, tenant, group string) ([]kernel.ConsolMember, error) {
	rows, err := t.Query(ctx, `SELECT member_id::text, group_id::text, legal_entity_id::text, source_book_id::text, method, ownership_pct::text, effective_from::text, COALESCE(effective_to::text, '')
		FROM consolidation_members WHERE tenant_id = $1::uuid AND group_id = $2::uuid ORDER BY legal_entity_id, effective_from`, tenant, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []kernel.ConsolMember{}
	for rows.Next() {
		var m kernel.ConsolMember
		if err := rows.Scan(&m.MemberID, &m.GroupID, &m.LegalEntityID, &m.SourceBookID, &m.Method, &m.OwnershipPct, &m.EffectiveFrom, &m.EffectiveTo); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetGroup implements kernel.ConsolRepo.
func (r *KernelRepo) GetGroup(ctx context.Context, tenant, id string) (*kernel.ConsolGroup, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var g kernel.ConsolGroup
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		if e := scanGroup(t.QueryRow(ctx, `SELECT `+groupCols+` FROM consolidation_groups WHERE tenant_id = $1::uuid AND group_id = $2::uuid`, tenant, id), &g); e != nil {
			return e
		}
		var e error
		g.Members, e = loadMembers(ctx, t, tenant, id)
		return e
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &g, nil
}

// ListGroups implements kernel.ConsolRepo.
func (r *KernelRepo) ListGroups(ctx context.Context, tenant string) ([]kernel.ConsolGroup, error) {
	out := []kernel.ConsolGroup{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT `+groupCols+` FROM consolidation_groups WHERE tenant_id = $1::uuid ORDER BY group_code LIMIT 200`, tenant)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var g kernel.ConsolGroup
			if e := scanGroup(rows, &g); e != nil {
				return e
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// InsertMember implements kernel.ConsolRepo.
func (r *KernelRepo) InsertMember(ctx context.Context, tenant string, m *kernel.ConsolMember, actor string, out kernel.OutEvent) error {
	return kernelErr(r.insertMember(ctx, tenant, m, actor, out))
}

func (r *KernelRepo) insertMember(ctx context.Context, tenant string, m *kernel.ConsolMember, actor string, out kernel.OutEvent) error {
	return r.tx(ctx, tenant, func(t pgx.Tx) error {
		if e := consolLock(ctx, t, tenant, m.GroupID); e != nil {
			return e
		}
		var overlapping int
		if e := t.QueryRow(ctx, `SELECT count(*) FROM consolidation_members WHERE tenant_id = $1::uuid AND group_id = $2::uuid AND legal_entity_id = $3::uuid
			AND (effective_to IS NULL OR effective_to > $4::date)`, tenant, m.GroupID, m.LegalEntityID, m.EffectiveFrom).Scan(&overlapping); e != nil {
			return e
		}
		if overlapping > 0 {
			return kernel.ErrOverlap
		}
		if _, e := t.Exec(ctx, `INSERT INTO consolidation_members (member_id, tenant_id, group_id, legal_entity_id, source_book_id, method, ownership_pct, effective_from, created_by)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::uuid, $6, $7::numeric, $8::date, $9)`,
			m.MemberID, tenant, m.GroupID, m.LegalEntityID, m.SourceBookID, m.Method, m.OwnershipPct, m.EffectiveFrom, actor); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, out)
	})
}

// EndMember implements kernel.ConsolRepo.
func (r *KernelRepo) EndMember(ctx context.Context, tenant, groupID, memberID string, effectiveTo time.Time, out kernel.OutEvent) error {
	return kernelErr(r.endMember(ctx, tenant, groupID, memberID, effectiveTo, out))
}

func (r *KernelRepo) endMember(ctx context.Context, tenant, groupID, memberID string, effectiveTo time.Time, out kernel.OutEvent) error {
	for _, id := range []string{groupID, memberID} {
		if _, err := uuid.Parse(id); err != nil {
			return kernel.ErrNotFound
		}
	}
	return r.tx(ctx, tenant, func(t pgx.Tx) error {
		if e := consolLock(ctx, t, tenant, groupID); e != nil {
			return e
		}
		var from time.Time
		var ended bool
		if e := t.QueryRow(ctx, `SELECT effective_from, effective_to IS NOT NULL FROM consolidation_members WHERE tenant_id = $1::uuid AND group_id = $2::uuid AND member_id = $3::uuid`,
			tenant, groupID, memberID).Scan(&from, &ended); e != nil {
			return e
		}
		if ended || !effectiveTo.After(from) {
			return kernel.ErrState
		}
		if _, e := t.Exec(ctx, `UPDATE consolidation_members SET effective_to = $4::date WHERE tenant_id = $1::uuid AND group_id = $2::uuid AND member_id = $3::uuid`, tenant, groupID, memberID, effectiveTo); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, out)
	})
}

// ── mapping versions ─────────────────────────────────────────────────────────────────────────────────────────────────

func scanMapping(row pgx.Row, m *kernel.ConsolMapping) error {
	var am, roles []byte
	if err := row.Scan(&m.GroupID, &m.Version, &m.Status, &am, &roles, &m.CreatedBy, &m.CreatedAt); err != nil {
		return err
	}
	if err := json.Unmarshal(am, &m.AccountMap); err != nil {
		return err
	}
	return json.Unmarshal(roles, &m.ICRoles)
}

const mappingCols = `group_id::text, version, status, account_map, ic_roles, created_by, created_at`

// InsertMapping implements kernel.ConsolRepo.
func (r *KernelRepo) InsertMapping(ctx context.Context, tenant string, m *kernel.ConsolMapping, out kernel.OutEvent) (*kernel.ConsolMapping, error) {
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		if e := consolLock(ctx, t, tenant, m.GroupID); e != nil {
			return e
		}
		if e := t.QueryRow(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM consolidation_mappings WHERE tenant_id = $1::uuid AND group_id = $2::uuid`, tenant, m.GroupID).Scan(&m.Version); e != nil {
			return e
		}
		if _, e := t.Exec(ctx, `UPDATE consolidation_mappings SET status = 'SUPERSEDED' WHERE tenant_id = $1::uuid AND group_id = $2::uuid AND status = 'ACTIVE'`, tenant, m.GroupID); e != nil {
			return e
		}
		am, _ := json.Marshal(m.AccountMap)
		roles, _ := json.Marshal(m.ICRoles)
		if _, e := t.Exec(ctx, `INSERT INTO consolidation_mappings (tenant_id, group_id, version, status, account_map, ic_roles, created_by, created_at)
			VALUES ($1::uuid, $2::uuid, $3, 'ACTIVE', $4::jsonb, $5::jsonb, $6, $7)`, tenant, m.GroupID, m.Version, string(am), string(roles), m.CreatedBy, m.CreatedAt); e != nil {
			return e
		}
		out.Data["version"] = m.Version
		return r.enqueue(ctx, t, tenant, out)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return m, nil
}

// ActiveMapping implements kernel.ConsolRepo.
func (r *KernelRepo) ActiveMapping(ctx context.Context, tenant, groupID string) (*kernel.ConsolMapping, error) {
	var m kernel.ConsolMapping
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanMapping(t.QueryRow(ctx, `SELECT `+mappingCols+` FROM consolidation_mappings WHERE tenant_id = $1::uuid AND group_id = $2::uuid AND status = 'ACTIVE'`, tenant, groupID), &m)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &m, nil
}

// ListMappings implements kernel.ConsolRepo.
func (r *KernelRepo) ListMappings(ctx context.Context, tenant, groupID string) ([]kernel.ConsolMapping, error) {
	out := []kernel.ConsolMapping{}
	if _, err := uuid.Parse(groupID); err != nil {
		return out, nil
	}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT `+mappingCols+` FROM consolidation_mappings WHERE tenant_id = $1::uuid AND group_id = $2::uuid ORDER BY version DESC`, tenant, groupID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var m kernel.ConsolMapping
			if e := scanMapping(rows, &m); e != nil {
				return e
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// ── reads that feed a run ────────────────────────────────────────────────────────────────────────────────────────────

// AccountTypes implements kernel.ConsolRepo.
func (r *KernelRepo) AccountTypes(ctx context.Context, tenant string) (map[string]string, error) {
	out := map[string]string{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT account_code, account_type FROM chart_of_accounts WHERE tenant_id = $1::uuid`, tenant)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var code, typ string
			if e := rows.Scan(&code, &typ); e != nil {
				return e
			}
			out[code] = typ
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// ICPositions implements kernel.ConsolRepo: the signed positions of the given accounts, by counterparty and transaction currency,
// up to a date and, when upToSeq is given, up to a watermark so the same positions can be read again later.
func (r *KernelRepo) ICPositions(ctx context.Context, tenant, entity, bookCode, functional string, asOf time.Time, upToSeq int64, accounts []string) ([]kernel.ICPosition, int64, error) {
	if len(accounts) == 0 {
		return []kernel.ICPosition{}, upToSeq, nil
	}
	out := []kernel.ICPosition{}
	var wm int64
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		wm = upToSeq
		if wm == 0 {
			if e := t.QueryRow(ctx, `SELECT COALESCE(max(entry_seq), 0) FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid`, tenant, entity).Scan(&wm); e != nil {
				return e
			}
		}
		rows, e := t.Query(ctx, `
			SELECT account_code, COALESCE(counterparty_id, ''), COALESCE(transaction_currency, $4),
			       sum(CASE WHEN transaction_amount IS NULL THEN debit_amount - credit_amount WHEN debit_amount > 0 THEN transaction_amount ELSE -transaction_amount END)::text,
			       (sum(debit_amount) - sum(credit_amount))::text
			FROM ledger_entries
			WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND book_id = $3 AND account_code = ANY($5) AND posting_date <= $6::date AND entry_seq <= $7
			GROUP BY account_code, COALESCE(counterparty_id, ''), COALESCE(transaction_currency, $4)
			HAVING sum(debit_amount) <> sum(credit_amount)
			    OR sum(CASE WHEN transaction_amount IS NULL THEN debit_amount - credit_amount WHEN debit_amount > 0 THEN transaction_amount ELSE -transaction_amount END) <> 0
			ORDER BY 1, 2, 3`, tenant, entity, bookCode, functional, accounts, asOf, wm)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			p := kernel.ICPosition{Entity: entity}
			if e := rows.Scan(&p.Account, &p.Counterparty, &p.Currency, &p.TxBalance, &p.Functional); e != nil {
				return e
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, wm, kernelErr(err)
}

// ── runs ─────────────────────────────────────────────────────────────────────────────────────────────────────────────

const consolRunCols = `r.run_id::text, r.group_id::text, r.fiscal_period, r.as_of::text, r.status, r.mapping_version, COALESCE(r.prior_run_id::text, ''), r.population, r.blocking_count,
	COALESCE(r.translation_journal_id::text, ''), COALESCE(tj.journal_number, ''), COALESCE(r.elimination_journal_id::text, ''), COALESCE(ej.journal_number, ''),
	COALESCE(r.group_snapshot_id::text, ''), r.run_hash, r.created_by, r.created_at, COALESCE(r.certified_by, ''), r.certified_at, COALESCE(r.superseded_by, ''), r.superseded_at,
	COALESCE(r.supersede_reason, '')`

const consolRunFrom = ` FROM consolidation_runs r LEFT JOIN journal_headers tj ON tj.journal_id = r.translation_journal_id LEFT JOIN journal_headers ej ON ej.journal_id = r.elimination_journal_id`

func scanConsolRun(row pgx.Row, v *kernel.ConsolRun) error {
	var pop []byte
	if err := row.Scan(&v.RunID, &v.GroupID, &v.FiscalPeriod, &v.AsOf, &v.Status, &v.MappingVersion, &v.PriorRunID, &pop, &v.BlockingCount, &v.TranslationJournalID, &v.TranslationNumber,
		&v.EliminationJournalID, &v.EliminationNumber, &v.GroupSnapshotID, &v.RunHash, &v.CreatedBy, &v.CreatedAt, &v.CertifiedBy, &v.CertifiedAt, &v.SupersededBy, &v.SupersededAt,
		&v.SupersedeReason); err != nil {
		return err
	}
	return json.Unmarshal(pop, &v.Population)
}

// GetRun implements kernel.ConsolRepo.
func (r *KernelRepo) GetConsolRun(ctx context.Context, tenant, id string) (*kernel.ConsolRun, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var v kernel.ConsolRun
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanConsolRun(t.QueryRow(ctx, `SELECT `+consolRunCols+consolRunFrom+` WHERE r.tenant_id = $1::uuid AND r.run_id = $2::uuid`, tenant, id), &v)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &v, nil
}

func (r *KernelRepo) runs(ctx context.Context, tenant, groupID string, liveOnly bool) ([]kernel.ConsolRun, error) {
	out := []kernel.ConsolRun{}
	if groupID != "" {
		if _, err := uuid.Parse(groupID); err != nil {
			return out, nil
		}
	}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT `+consolRunCols+consolRunFrom+` WHERE r.tenant_id = $1::uuid AND ($2 = '' OR r.group_id::text = $2) AND (NOT $3::boolean OR r.status <> 'SUPERSEDED')
			ORDER BY r.as_of DESC, r.created_at DESC LIMIT 200`, tenant, groupID, liveOnly)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var v kernel.ConsolRun
			if e := scanConsolRun(rows, &v); e != nil {
				return e
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// LiveRuns implements kernel.ConsolRepo.
func (r *KernelRepo) LiveRuns(ctx context.Context, tenant, groupID string) ([]kernel.ConsolRun, error) {
	return r.runs(ctx, tenant, groupID, true)
}

// ListRuns implements kernel.ConsolRepo.
func (r *KernelRepo) ListConsolRuns(ctx context.Context, tenant, groupID string) ([]kernel.ConsolRun, error) {
	return r.runs(ctx, tenant, groupID, false)
}

func deltaLines(delta map[string]string, what string) []SysLine {
	accounts := make([]string, 0, len(delta))
	for a := range delta {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	var lines []SysLine
	for _, a := range accounts {
		n, _ := new(big.Rat).SetString(delta[a])
		l := SysLine{Account: a, Debit: "0", Credit: "0", Description: what, RuleLineRef: "CONSOL"}
		if n.Sign() > 0 {
			l.Debit = n.FloatString(2)
		} else {
			l.Credit = new(big.Rat).Neg(n).FloatString(2)
		}
		lines = append(lines, l)
	}
	return lines
}

// WriteConsolidation implements kernel.ConsolRepo: the group-book journals and the run in one transaction.
func (r *KernelRepo) WriteConsolidation(ctx context.Context, p *kernel.ConsolPlan) (*kernel.ConsolRun, error) {
	run := p.Run
	var out *kernel.ConsolRun
	err := r.tx(ctx, p.Tenant, func(t pgx.Tx) error {
		if e := consolLock(ctx, t, p.Tenant, p.Group.GroupID); e != nil {
			return e
		}
		var existing kernel.ConsolRun
		switch e := scanConsolRun(t.QueryRow(ctx, `SELECT `+consolRunCols+consolRunFrom+` WHERE r.tenant_id = $1::uuid AND r.group_id = $2::uuid AND r.fiscal_period = $3 AND r.status <> 'SUPERSEDED'`,
			p.Tenant, p.Group.GroupID, run.FiscalPeriod), &existing); {
		case e == nil:
			existing.Existing = true
			out = &existing
			return nil
		case !errors.Is(e, pgx.ErrNoRows):
			return e
		}
		// the run was computed against the latest run that existed then: another one since makes its deltas wrong
		var latest string
		var latestAsOf time.Time
		switch e := t.QueryRow(ctx, `SELECT run_id::text, as_of FROM consolidation_runs WHERE tenant_id = $1::uuid AND group_id = $2::uuid AND status <> 'SUPERSEDED' ORDER BY as_of DESC LIMIT 1`,
			p.Tenant, p.Group.GroupID).Scan(&latest, &latestAsOf); {
		case errors.Is(e, pgx.ErrNoRows):
			latest = ""
		case e != nil:
			return e
		}
		if latest != run.PriorRunID {
			return &kernel.Exception{Family: kernel.FamConc, Code: "POPULATION_CHANGED", Detail: "another consolidation run of the group was recorded while this one was prepared; run it again"}
		}
		for _, m := range p.MemberBooks {
			var late int
			if e := t.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND book_id = $3 AND posting_date <= $4::date AND entry_seq > $5`,
				p.Tenant, m.LegalEntityID, m.BookCode, p.AsOf, m.LedgerWatermark).Scan(&late); e != nil {
				return e
			}
			if late > 0 {
				return &kernel.Exception{Family: kernel.FamConc, Code: "POPULATION_CHANGED", Detail: fmt.Sprintf("%d entries were posted into the book of member %s after its population was frozen; consolidate again", late, m.LegalEntityID),
					Facts: map[string]any{"legal_entity_id": m.LegalEntityID}}
			}
		}
		post := func(delta map[string]string, typ domain.JournalType, purpose, kind, what, corr string) (*SysPosted, error) {
			lines := deltaLines(delta, what)
			if len(lines) == 0 {
				return nil, nil
			}
			trace, _ := json.Marshal(map[string]any{"consolidation_run_id": run.RunID, "group_id": p.Group.GroupID, "mapping_version": run.MappingVersion, "run_hash": run.RunHash, "prior_run_id": run.PriorRunID})
			return r.postSystemJournal(ctx, t, &SysJournal{Tenant: p.Tenant, Entity: p.Group.ParentEntityID, Period: run.FiscalPeriod, BookID: p.GroupBook.BookID, BookCode: p.GroupBook.BookCode,
				Type: typ, Purpose: purpose, Description: fmt.Sprintf("%s %s of group %s", what, run.FiscalPeriod, p.Group.GroupCode), Correlation: corr, PostingDate: p.AsOf, Currency: p.Currency,
				Actor: p.Actor, At: run.CreatedAt, ExecutionKind: kind, Trace: string(trace), Lines: lines})
		}
		tj, e := post(run.Population.TranslationPosted, domain.JournalTypeConsolidation, "CONSOLIDATION", "KERNEL_CONSOL", "Translated member balances", "consol:"+run.RunID)
		if e != nil {
			return e
		}
		ej, e := post(run.Population.EliminationPosted, domain.JournalTypeElimination, "ELIMINATION", "KERNEL_ELIM", "Intercompany eliminations", "consol-elim:"+run.RunID)
		if e != nil {
			return e
		}
		if tj != nil {
			run.TranslationJournalID, run.TranslationNumber = tj.JournalID, tj.Number
		}
		if ej != nil {
			run.EliminationJournalID, run.EliminationNumber = ej.JournalID, ej.Number
		}
		pop, _ := json.Marshal(run.Population)
		if _, e := t.Exec(ctx, `INSERT INTO consolidation_runs (run_id, tenant_id, group_id, fiscal_period, as_of, status, mapping_version, prior_run_id, population, blocking_count,
				translation_journal_id, elimination_journal_id, run_hash, created_by, created_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::date, 'PROCESSED', $6, NULLIF($7, '')::uuid, $8::jsonb, $9, NULLIF($10, '')::uuid, NULLIF($11, '')::uuid, $12, $13, $14)`,
			run.RunID, p.Tenant, p.Group.GroupID, run.FiscalPeriod, run.AsOf, run.MappingVersion, run.PriorRunID, string(pop), run.BlockingCount, run.TranslationJournalID, run.EliminationJournalID,
			run.RunHash, run.CreatedBy, run.CreatedAt); e != nil {
			return e
		}
		if e := r.enqueue(ctx, t, p.Tenant, kernel.OutEvent{Fact: "consolidation-run.completed", AggregateType: "consolidation-run", AggregateID: run.RunID, Actor: run.CreatedBy, CorrelationID: p.Correlation,
			LegalEntityID: p.Group.ParentEntityID, Data: map[string]any{"consolidation_run_id": run.RunID, "group_id": p.Group.GroupID, "fiscal_period": run.FiscalPeriod, "blocking_exceptions": run.BlockingCount,
				"translation_journal_id": run.TranslationJournalID, "elimination_journal_id": run.EliminationJournalID, "run_hash": run.RunHash}}); e != nil {
			return e
		}
		out = &run
		return nil
	})
	if err != nil {
		if _, ok := kernel.AsException(err); ok {
			return nil, err
		}
		return nil, kernelErr(err)
	}
	return out, nil
}

// CertifyRun implements kernel.ConsolRepo.
func (r *KernelRepo) CertifyRun(ctx context.Context, tenant, id, actor string, at time.Time, snapshotID string, out kernel.OutEvent) (*kernel.ConsolRun, error) {
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		tag, e := t.Exec(ctx, `UPDATE consolidation_runs SET status = 'CERTIFIED', certified_by = $3, certified_at = $4, group_snapshot_id = $5::uuid
			WHERE tenant_id = $1::uuid AND run_id = $2::uuid AND status = 'PROCESSED'`, tenant, id, actor, at, snapshotID)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return kernel.ErrState
		}
		return r.enqueue(ctx, t, tenant, out)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return r.GetConsolRun(ctx, tenant, id)
}

// SupersedeRun implements kernel.ConsolRepo.
func (r *KernelRepo) SupersedeRun(ctx context.Context, tenant, id, actor, reason string, at time.Time, asOf time.Time, group kernel.ConsolGroup, book kernel.Book) (*kernel.ConsolRun, error) {
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		if e := consolLock(ctx, t, tenant, group.GroupID); e != nil {
			return e
		}
		var status, period, trans, elim string
		if e := t.QueryRow(ctx, `SELECT status, fiscal_period, COALESCE(translation_journal_id::text, ''), COALESCE(elimination_journal_id::text, '') FROM consolidation_runs
			WHERE tenant_id = $1::uuid AND run_id = $2::uuid`, tenant, id).Scan(&status, &period, &trans, &elim); e != nil {
			return e
		}
		if status == "SUPERSEDED" {
			return kernel.ErrState
		}
		var later int
		if e := t.QueryRow(ctx, `SELECT count(*) FROM consolidation_runs WHERE tenant_id = $1::uuid AND group_id = $2::uuid AND status <> 'SUPERSEDED' AND as_of > $3::date`, tenant, group.GroupID, asOf).Scan(&later); e != nil {
			return e
		}
		if later > 0 {
			return &kernel.Exception{Family: kernel.FamRule, Code: "SUPERSEDE_NOT_LATEST", Detail: "a later period has been consolidated on top of this run: supersede that one first"}
		}
		for n, jid := range []string{trans, elim} {
			if jid == "" {
				continue
			}
			rows, e := t.Query(ctx, `SELECT account_code, debit_amount::text, credit_amount::text FROM journal_lines WHERE tenant_id = $1::uuid AND journal_id = $2::uuid ORDER BY line_number`, tenant, jid)
			if e != nil {
				return e
			}
			var back []SysLine
			for rows.Next() {
				var l SysLine
				var dr, cr string
				if e := rows.Scan(&l.Account, &dr, &cr); e != nil {
					rows.Close()
					return e
				}
				l.Debit, l.Credit, l.Description, l.RuleLineRef = cr, dr, "reversal: run superseded", "CONSOL_SUPERSEDE"
				back = append(back, l)
			}
			rows.Close()
			if e := rows.Err(); e != nil {
				return e
			}
			trace, _ := json.Marshal(map[string]any{"consolidation_run_id": id, "reverses": jid, "reason": reason})
			if _, e := r.postSystemJournal(ctx, t, &SysJournal{Tenant: tenant, Entity: group.ParentEntityID, Period: period, BookID: book.BookID, BookCode: book.BookCode, Type: domain.JournalTypeReversal,
				Purpose: "CONSOLIDATION", Description: fmt.Sprintf("Reversal of superseded consolidation run %s of group %s", period, group.GroupCode), Correlation: fmt.Sprintf("consol-sup:%s:%d", id, n),
				PostingDate: asOf, Currency: group.PresentationCurrency, Actor: actor, At: at, ReversalOf: jid, ReversalReason: reason, ExecutionKind: "KERNEL_CONSOL_SUP", Trace: string(trace), Lines: back}); e != nil {
				return e
			}
		}
		if _, e := t.Exec(ctx, `UPDATE consolidation_runs SET status = 'SUPERSEDED', superseded_by = $3, superseded_at = $4, supersede_reason = $5 WHERE tenant_id = $1::uuid AND run_id = $2::uuid`,
			tenant, id, actor, at, reason); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, kernel.OutEvent{Fact: "consolidation-run.superseded", AggregateType: "consolidation-run", AggregateID: id, Actor: actor, LegalEntityID: group.ParentEntityID,
			Data: map[string]any{"consolidation_run_id": id, "group_id": group.GroupID, "fiscal_period": period, "reason": reason}})
	})
	if err != nil {
		if _, ok := kernel.AsException(err); ok {
			return nil, err
		}
		return nil, kernelErr(err)
	}
	return r.GetConsolRun(ctx, tenant, id)
}
