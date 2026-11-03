package kernel

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"
)

// Reconciliations the kernel runs from its own records (standard §27.1): the reciprocal balances of two entities of a group, and the
// ledger against a signed snapshot — the trial balance a report is made from. Neither needs a population from another system, so
// both sides are reproducible from the ledger watermarks the run records.

// ownSided reports whether a reconciliation type reads both of its sides from the kernel's own records.
func ownSided(t string) bool {
	return t == ReconEventPosting || t == ReconIntercompany || t == ReconLedgerSnapshot
}

func net(p LedgerPosition) *big.Rat { return new(big.Rat).Sub(dec(p.DebitTotal), dec(p.CreditTotal)) }

// reconcileIntercompany compares what one entity says it is owed by (or owes) another with what the other says: the two nets must
// cancel. A difference is a timing difference, an amount difference or an unmatched item, and is resolved as such; entities that keep
// books in different currencies cannot be compared here and are refused rather than compared across a rate nobody chose.
func (s *ReconService) reconcileIntercompany(ctx context.Context, tenant string, req RunRequest, bk *Book, asOf time.Time, run *ReconRun) (expected, actual *big.Rat, err error) {
	bad := func(f, why string) error { return exc(FamData, "RECON_INVALID", "%s: %s", f, why).with("field", f) }
	if req.CounterpartyBookID == "" {
		return nil, nil, bad("counterparty_book_id", "the other entity's book")
	}
	cb, err := s.Books.GetBook(ctx, tenant, req.CounterpartyBookID)
	if err != nil {
		return nil, nil, err
	}
	if cb.LegalEntityID == bk.LegalEntityID {
		return nil, nil, bad("counterparty_book_id", "a book of another legal entity")
	}
	if cb.FunctionalCurrency != bk.FunctionalCurrency {
		return nil, nil, exc(FamFX, "RECON_CURRENCY", "the books are in %s and %s: reconcile them in a common currency", bk.FunctionalCurrency, cb.FunctionalCurrency)
	}
	pa, err := s.Repo.IntercompanyPosition(ctx, tenant, bk.LegalEntityID, bk.BookCode, cb.LegalEntityID, asOf, 0)
	if err != nil {
		return nil, nil, err
	}
	pb, err := s.Repo.IntercompanyPosition(ctx, tenant, cb.LegalEntityID, cb.BookCode, bk.LegalEntityID, asOf, 0)
	if err != nil {
		return nil, nil, err
	}
	expected = net(pa)
	actual = new(big.Rat).Neg(net(pb))
	side := func(ent, book, other string, p LedgerPosition) Population {
		return Population{Source: "LEDGER_INTERCOMPANY", Reference: ent + " with " + other, Watermark: fmt.Sprint(p.Watermark), Count: p.Count, Currency: bk.FunctionalCurrency,
			Detail: map[string]any{"legal_entity_id": ent, "book_id": book, "counterparty_entity_id": other, "debit_total": p.DebitTotal, "credit_total": p.CreditTotal,
				"net_balance": ratFixed(net(p))}}
	}
	run.PopulationA = side(bk.LegalEntityID, bk.BookID, cb.LegalEntityID, pa)
	run.PopulationA.Total = ratFixed(expected)
	run.PopulationB = side(cb.LegalEntityID, cb.BookID, bk.LegalEntityID, pb)
	run.PopulationB.Total = ratFixed(actual)
	return expected, actual, nil
}

// reconcileLedgerSnapshot recomputes the ledger at a retained snapshot's watermark and compares it, account by account, with the
// balances the snapshot holds. The case is judged by its worst account, so offsetting differences cannot hide inside matching totals;
// a signature that does not verify is a break whatever the figures say. The snapshot is the report's source dataset: if this matches,
// the report can be rebuilt from the ledger.
func (s *ReconService) reconcileLedgerSnapshot(ctx context.Context, tenant string, bk *Book, snap *BalanceSnapshot, run *ReconRun) (expected, actual, worst *big.Rat, forceBreak bool, err error) {
	asOf, _ := time.Parse("2006-01-02", snap.AsOf)
	pstart, _, _ := periodBounds(snap.FiscalPeriod)
	_, lines, err := s.Snapshots.Balances(ctx, tenant, snap.LegalEntityID, bk.BookCode, asOf, pstart, snap.LedgerWatermark)
	if err != nil {
		return nil, nil, nil, false, err
	}
	held := map[string]SnapshotLine{}
	for _, l := range snap.Lines {
		held[l.AccountCode] = l
	}
	again := map[string]SnapshotLine{}
	actual = new(big.Rat)
	for _, l := range lines {
		again[l.AccountCode] = l
		actual.Add(actual, dec(l.Debit))
	}
	codes := map[string]bool{}
	for c := range held {
		codes[c] = true
	}
	for c := range again {
		codes[c] = true
	}
	sorted := make([]string, 0, len(codes))
	for c := range codes {
		sorted = append(sorted, c)
	}
	sort.Strings(sorted)
	worst = new(big.Rat)
	differing := []map[string]string{}
	for _, c := range sorted {
		h, l := held[c], again[c]
		dd := new(big.Rat).Sub(dec(h.Debit), dec(l.Debit))
		dc := new(big.Rat).Sub(dec(h.Credit), dec(l.Credit))
		w := new(big.Rat).Abs(dd)
		if a := new(big.Rat).Abs(dc); a.Cmp(w) > 0 {
			w = a
		}
		if w.Sign() == 0 {
			continue
		}
		if w.Cmp(worst) > 0 {
			worst = w
		}
		if len(differing) < 100 {
			differing = append(differing, map[string]string{"account_code": c, "snapshot_debit": ratFixed(dec(h.Debit)), "ledger_debit": ratFixed(dec(l.Debit)),
				"snapshot_credit": ratFixed(dec(h.Credit)), "ledger_credit": ratFixed(dec(l.Credit))})
		}
	}
	expected = dec(snap.TotalDebit)
	detail := map[string]any{"content_hash": snap.ContentHash, "key_id": snap.KeyID, "purpose": snap.Purpose}
	if s.Signer == nil {
		detail["signature_valid"], detail["key_known"] = false, false
		forceBreak = true
	} else {
		valid, known := s.Signer.Verify(snap.ContentHash, snap.Signature, snap.KeyID)
		detail["signature_valid"], detail["key_known"] = valid, known
		forceBreak = !valid || !known
	}
	if !contentHashOK(snap) {
		detail["content_matches_hash"] = false
		forceBreak = true
	}
	run.PopulationA = Population{Source: "SIGNED_SNAPSHOT", Reference: snap.SnapshotID, Watermark: fmt.Sprint(snap.LedgerWatermark), Count: snap.AccountCount,
		Total: ratFixed(expected), Currency: bk.FunctionalCurrency, Detail: detail}
	run.PopulationB = Population{Source: "LEDGER", Watermark: fmt.Sprint(snap.LedgerWatermark), Count: len(lines), Total: ratFixed(actual), Currency: bk.FunctionalCurrency,
		Detail: map[string]any{"accounts_differing": differing, "snapshot_id": snap.SnapshotID}}
	return expected, actual, worst, forceBreak, nil
}

func contentHashOK(s *BalanceSnapshot) bool { return contentHash(s) == s.ContentHash }

// verifyOwnSided recomputes the ledger side(s) of an intercompany or snapshot run from the watermarks it recorded.
func (s *ReconService) verifyOwnSided(ctx context.Context, tenant string, run *ReconRun) (map[string]any, error) {
	asOf, _ := time.Parse("2006-01-02", run.AsOf)
	storedActual, _ := new(big.Rat).SetString(run.ActualTotal)
	storedExpected, _ := new(big.Rat).SetString(run.ExpectedTotal)
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	if run.Type == ReconIntercompany {
		var wa, wb int64
		fmt.Sscan(run.PopulationA.Watermark, &wa)
		fmt.Sscan(run.PopulationB.Watermark, &wb)
		pa, err := s.Repo.IntercompanyPosition(ctx, tenant, str(run.PopulationA.Detail, "legal_entity_id"), s.bookCode(ctx, tenant, str(run.PopulationA.Detail, "book_id")),
			str(run.PopulationA.Detail, "counterparty_entity_id"), asOf, wa)
		if err != nil {
			return nil, err
		}
		pb, err := s.Repo.IntercompanyPosition(ctx, tenant, str(run.PopulationB.Detail, "legal_entity_id"), s.bookCode(ctx, tenant, str(run.PopulationB.Detail, "book_id")),
			str(run.PopulationB.Detail, "counterparty_entity_id"), asOf, wb)
		if err != nil {
			return nil, err
		}
		recExpected, recActual := net(pa), new(big.Rat).Neg(net(pb))
		return map[string]any{"reconciliation_run_id": run.RunID, "reproducible": storedExpected.Cmp(recExpected) == 0 && storedActual.Cmp(recActual) == 0,
			"stored_actual_total": run.ActualTotal, "recomputed_actual_total": ratFixed(recActual), "stored_expected_total": run.ExpectedTotal, "recomputed_expected_total": ratFixed(recExpected)}, nil
	}
	snap, err := s.Snapshots.GetSnapshot(ctx, tenant, run.PopulationA.Reference)
	if err != nil {
		return nil, err
	}
	bk, err := s.Books.GetBook(ctx, tenant, snap.BookID)
	if err != nil {
		return nil, err
	}
	pstart, _, _ := periodBounds(snap.FiscalPeriod)
	_, lines, err := s.Snapshots.Balances(ctx, tenant, snap.LegalEntityID, bk.BookCode, asOf, pstart, snap.LedgerWatermark)
	if err != nil {
		return nil, err
	}
	rec := new(big.Rat)
	for _, l := range lines {
		rec.Add(rec, dec(l.Debit))
	}
	return map[string]any{"reconciliation_run_id": run.RunID, "reproducible": storedActual.Cmp(rec) == 0, "stored_actual_total": run.ActualTotal, "recomputed_actual_total": ratFixed(rec)}, nil
}

func (s *ReconService) bookCode(ctx context.Context, tenant, bookID string) string {
	b, err := s.Books.GetBook(ctx, tenant, bookID)
	if err != nil {
		return ""
	}
	return b.BookCode
}
