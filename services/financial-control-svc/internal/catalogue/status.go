package catalogue

import "sort"

// Implementation states of a §29 catalogue control.
const (
	StateImplemented = "IMPLEMENTED" // definition exists in this build and can run on data the platform holds
	StatePartial     = "PARTIAL"     // runs on real data but covers less than the catalogue wording; Note says exactly what
	StateBlocked     = "BLOCKED"     // cannot yet produce a valid result: a named capability elsewhere is missing
	StateNotStarted  = "NOT_STARTED" // belongs to a later implementation wave
)

// ControlStatus is one row of the catalogue status report.
type ControlStatus struct {
	Code      string   `json:"control_code"`
	Name      string   `json:"name"`
	Wave      int      `json:"wave"`
	State     string   `json:"state"`
	BlockedBy []string `json:"blocked_by,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// Status reports, for every control in the ZS-CONTROL-001 §29 catalogue, whether
// it is implemented, blocked or not started. It exists so that "the framework
// has 45 controls" is never mistaken for "45 controls work": a BLOCKED control
// names the exact capability another service still has to provide, and would
// fail closed (FAILED / INDETERMINATE) rather than pass if it were run today.
func Status() []ControlStatus {
	impl := func(code, name string, wave int, note string) ControlStatus {
		return ControlStatus{Code: code, Name: name, Wave: wave, State: StateImplemented, Note: note}
	}
	blk := func(code, name string, wave int, why ...string) ControlStatus {
		return ControlStatus{Code: code, Name: name, Wave: wave, State: StateBlocked, BlockedBy: why}
	}
	part := func(code, name string, wave int, note string) ControlStatus {
		return ControlStatus{Code: code, Name: name, Wave: wave, State: StatePartial, Note: note}
	}

	out := []ControlStatus{
		impl("FIN-CTRL-001", "AR subledger to GL", 2, ""),
		impl("FIN-CTRL-002", "AP subledger to GL", 2, ""),
		impl("FIN-CTRL-003", "Bank statement to cash GL", 2, "banking-connector-svc runs in the phase-7 compose stack"),
		blk("FIN-CTRL-004", "Payment provider settlement", 2,
			"no provider settlement population: payment-status-svc carries no amount/currency, and bank settlement evidence is not linked to payment instructions"),
		part("FIN-CTRL-005", "Payroll run to GL", 3, "seed wave 7. Ties each ACCEPTED payroll result's gross to the gross expense of its POSTED payroll journal (one record per result, exact); an accepted result with no posted journal surfaces as missing. Gross and net only: per-line, liability and per-worker comparison is the WFP reconciliation run inside payroll-run-svc, and a Zoiko result is flagged EMPLOYER_LIABILITIES_NOT_COMPUTED so employer cost is absent from both sides"),
		part("FIN-CTRL-006", "Payroll net-pay to payment", 3, "seed wave 7. Ties each ACCEPTED payroll result's net pay to the sum of its live net-pay payment instructions (cancelled, superseded and returned ones do not count); it proves payment is instructed, not that Banking settled it: settlement, returns and unknown outcomes are observed and reconciled inside payroll-run-svc"),
		blk("FIN-CTRL-007", "Tax ledger to GL", 3,
			"no tax ledger exists (TAX-07) and no tax service posts to the GL"),
		blk("FIN-CTRL-008", "Tax return to tax ledger", 3,
			"no tax ledger exists (TAX-07)",
			"vat_returns totals are caller-supplied, not derived from tax determinations"),
		blk("FIN-CTRL-009", "Tax payment/refund to liability", 3,
			"no tax payment/refund records or liability balance exist",
			"no tax-payment-to-bank path"),
		blk("FIN-CTRL-010", "Fixed asset register to GL", 3,
			"asset-management-svc keeps cost on depreciation schedules without a currency; capitalisation posting to the GL is optional and keyed by event_id, with no schedule-to-event link",
			"no dated per-asset population endpoint"),
		blk("FIN-CTRL-011", "Depreciation run to GL", 3,
			"general-ledger-svc PostAccountingEvent now requires transaction_currency and document_date, but asset-management-svc's ledger client does not send them, so its depreciation postings are refused with 400 until that client is updated",
			"depreciation_runs carry no currency",
			"asset-management-svc has no control-populations endpoint"),
		blk("FIN-CTRL-012", "Inventory subledger to GL", 3,
			"inbound inventory value and adjustments are deliberately not posted to the GL, so total inventory value cannot be reconciled",
			"no inventory table carries a currency"),
		part("FIN-CTRL-013", "Physical count to inventory", 3,
			"quantity existence only; the valuation assertion is not covered (see FIN-CTRL-012)"),
		part("FIN-CTRL-014", "Intercompany reciprocal balances", 4,
			"ties each entity's SOURCE-leg intercompany entries to its own ledger receivable postings; a run covers one legal entity, so entity-A-versus-entity-B reciprocity is established transitively (entry = both legs), not compared directly"),
		part("FIN-CTRL-015", "Intercompany reciprocal activity", 4,
			"ties each entity's TARGET-leg entries to its own ledger payable postings; same single-entity limitation as FIN-CTRL-014; intercompany entries carry no period, so the legs are lifetime populations"),
		blk("FIN-CTRL-016", "Elimination completeness", 4,
			"consolidation-svc keeps no link from an elimination to intercompany entries (adjustment lines carry only account and amounts) and does not persist per-entry eliminations",
			"automatic elimination in StartRun subtracts whole-journal nets for every tenant-wide MATCHED entry, regardless of period or entity"),
		part("FIN-CTRL-017", "Entity TB to consolidation input", 4,
			"compares the ledger trial balance (uncertified: no certified per-account entity TB exists) to consolidation's recorded contributions; expect it to fail today because consolidation excludes REVERSED originals, truncates at 200 journals and translates nothing"),
		blk("FIN-CTRL-018", "Consolidated TB to statements", 4,
			"no trial-balance-account-to-statement-line mapping and no financial statements exist anywhere in the platform"),
		part("FIN-CTRL-019", "Accounting event to journal", 5, "finds committed events without a posted journal, event-sourced journals without a committed event, and duplicate journals per event; it covers events that reached the posting engine, so an event that never arrived is invisible to it"),
		impl("FIN-CTRL-020", "Journal debit-credit balance", 5, "detective re-performance from ledger_entries per journal; the posting path enforces balance at write time"),
		part("FIN-CTRL-021", "Control-account direct posting", 5, "detective test of a preventive control: finds ledger entries on restricted control accounts whose journal has no source event; it cannot block the posting"),
		part("FIN-CTRL-022", "Unposted accounting events", 5, "covers events that reached the posting engine and did not COMMIT; an event that never arrived is invisible to it"),
		part("FIN-CTRL-023", "Late/manual journal review", 5, "independent-approver check on manual journals; lateness against the close date is not covered because general-ledger-svc does not know when a period closed"),
		part("FIN-CTRL-024", "Closed-period posting block", 5, "detective test of the GL's own period seal: ledger entries of a currently SEALED period whose entry_seq is above the seal's watermark (the posting path refuses a sealed period, so such an entry came in out of band); a period that was never sealed in the GL is not tested, and neither is financial-close-svc's or accounting-period-svc's notion of closed; a REOPENED period is authorized by the reopen authority and is not a finding; it cannot block the posting"),
		blk("FIN-CTRL-025", "Foreign-currency revaluation to GL", 3,
			"financial-close-svc posts revaluation via POST /v1/journals with correlation_id only (source_event_id is null), so the GL reference cannot be paired to the run",
			"by code inspection (not run) the GL rejects that call: financial-close-svc omits journal_type, transaction_date, posting_date and currency_code, which general-ledger-svc requires",
			"financial-close-svc has no control-populations endpoint"),
		part("FIN-CTRL-026", "FX rate source completeness", 3, "the general ledger's own sourced FX rates (the store revaluation and translation read) of the run's period_id month, one rate type (fx_rate_type, default SPOT), tested per currency pair for a rate on every business day up to today; pairs listed in fx_pairs_expected must be covered even if the feed delivered nothing, and with none listed a dead feed passes; each record carries its source so a gap is attributable, but only presence is tested, not the rate's accuracy against an external benchmark, and rates are tenant-level so the entity's own reliance is not tested"),
		impl("FIN-CTRL-027", "Customer invoice sequence / issue completeness", 2, ""),
		impl("FIN-CTRL-028", "Supplier invoice duplicate control", 2, ""),
		impl("FIN-CTRL-029", "Unapplied cash aging", 2, "AR-08 receipts with unapplied_amount > 0 and a value date before the run's unapplied_before cut-off (the aging limit is a §34 decision, supplied per run); AR-side only, the unapplied balance is not tied to the GL unapplied-cash account"),
		impl("FIN-CTRL-030", "Supplier unapplied payment aging", 2, "payment-run-svc payables of SETTLED instructions that AP-08 has not yet accepted (payable_applied_at is null) and that settled before the run's unapplied_before cut-off (the aging limit is a §34 decision, supplied per run); covers payments made through AP-11 runs only, and returned or unmatched bank payments that never had a run are not seen"),
		part("FIN-CTRL-031", "Suspense account aging", 5, "ages the open balance of every chart account typed CLEARING by FIFO (later opposite-direction postings clear the oldest amounts first), per account, currency and book, against the run's aged_before cut-off (the aging limit is a §34 decision, supplied per run); the ledger has no item-level clearing, so origin, owner and clearing authority per item (§20) are not covered, and neither is materiality or carry-forward"),
		impl("FIN-CTRL-032", "Bank feed completeness", 2, ""),
		blk("FIN-CTRL-033", "Import batch completeness", 6,
			"no generic file-import registry: only bank statements carry a header count and batch id, and the loaded-side count is not persisted as a population"),
		blk("FIN-CTRL-034", "Event producer-consumer completeness", 6,
			"consumer-side receipt is not recorded generically: only bank-reconciliation-svc keeps an inbox (evidence-conflict events), so published-versus-consumed cannot be compared"),
		blk("FIN-CTRL-035", "E-invoice submission to internal invoice", 6,
			"no e-invoice service or submission records exist; invoices carry no e-invoice columns"),
		blk("FIN-CTRL-036", "E-invoice authority acknowledgement", 6,
			"no e-invoice submission or acknowledgement store exists"),
		part("FIN-CTRL-037", "Tax filing acknowledgement", 6, "finds submissions past a cut-off with no authority acknowledgement reference; see the write-back note in the population contract: if nothing ever records a genuine acknowledgement, every submission will surface"),
		part("FIN-CTRL-038", "Opening balance migration tie-out", 6, "ties the batch declared totals and row count to the loaded crosswalk and checks it balances; it does not tie to the posted GL journal or to a legacy trial balance"),
		blk("FIN-CTRL-039", "Open AR/AP migration tie-out", 6,
			"migrated open items are barred from living in the AR/AP subledger, so there is no shared identity to match crosswalk lines against; a subledger-total to control-account tie-out has no defined source"),
		blk("FIN-CTRL-040", "Report population to certified TB", 4,
			"reporting-orchestration-svc produces no report output (runs end NOT_IMPLEMENTED)",
			"financial_snapshots carries no per-account lines and no period, and certification is not verified against the ledger"),
		impl("FIN-CTRL-041", "Close mandatory-control gate", 5, "computed by this service (GET /controls/v1/close-gate), not a catalogue run: open only when every close_gating control's latest run for the entity and period is CERTIFIED; fail-closed when none are configured; financial-close-svc does not yet call it"),
		impl("FIN-CTRL-042", "Material exception aggregation", 5, "computed by this service (GET /controls/v1/exception-summary): unresolved exceptions of each control's latest run, per currency, against the entity's aggregate threshold; exposure across controls can overlap, so it is an upper bound; other currencies are reported unassessed, not converted"),
		blk("FIN-CTRL-043", "Related-party/intercompany classification", 4,
			"no intercompany or related-party flag on chart-of-accounts, journal lines or counterparties; journal lines carry no counterparty-entity dimension"),
		part("FIN-CTRL-044", "Cash account ownership/entity validation", 6, "treasury-svc bank accounts of the run's entity in use (ACTIVE or SUSPENDED) with no current ownership evidence, or whose evidence was recorded by the account's own creator; the account-to-ledger-cash-account link is NOT covered, because nothing maps a bank account to its GL cash account (statement_lines.gl_cash_account_code is nullable and unowned), so the entity match of the GL side is untested"),
		part("FIN-CTRL-045", "Supplier bank detail change vs payment release SoD", 6, "checks proposer/verifier/approver independence on the bank-detail change itself; it does not link the change to the payment released afterwards, which spans payment-authorization-svc"),

		// Extensions implemented from §15's payroll tests, on payroll's own data.
		impl("PAY-CTRL-001", "Payroll gross-to-net integrity (per pay slip)", 3, "extension implementing §15 Gross-to-net; does not replace FIN-CTRL-005"),
		impl("PAY-CTRL-002", "Payroll run net total to pay slips", 3, "extension implementing §15 run-control-total tie-out; does not replace FIN-CTRL-005"),
		impl("PAY-CTRL-003", "Payroll run gross total to pay slips", 3, "extension implementing §15 run-control-total tie-out; does not replace FIN-CTRL-005"),
		impl("PAY-CTRL-004", "Payroll result net pay to posted net-pay payable", 3, "seed wave 7: accepted result net pay to the net-pay payable of its POSTED payroll journal"),
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// Summary counts controls per state.
func Summary(s []ControlStatus) map[string]int {
	m := map[string]int{StateImplemented: 0, StatePartial: 0, StateBlocked: 0, StateNotStarted: 0}
	for _, c := range s {
		m[c.State]++
	}
	return m
}
