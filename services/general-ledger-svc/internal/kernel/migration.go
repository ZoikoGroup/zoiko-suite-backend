package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Opening balances, migration and cutover (standard §28). Migration is not a special permission to bypass financial integrity: a
// batch of opening lines is staged from a source extract, validated against the same chart, dimension and counterparty rules a
// posting meets, previewed, approved by someone other than whoever staged it for a named authority inside a cutover window, posted
// as one OPENING journal, reconciled to the source totals and subledger control totals, and then frozen with its evidence.

// Migration batch statuses and kinds.
const (
	MigStaged     = "STAGED"
	MigValidated  = "VALIDATED"
	MigApproved   = "APPROVED"
	MigPosted     = "POSTED"
	MigReconciled = "RECONCILED"
	MigCancelled  = "CANCELLED"

	MigOpening    = "OPENING"
	MigCorrection = "CORRECTION"
)

// MaxMigrationLines bounds one batch: a larger population is staged in several batches (one per book and cutover purpose).
const MaxMigrationLines = 50000

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// MigrationTotals are the debit and credit totals of the source trial balance.
type MigrationTotals struct {
	Debit  string `json:"debit"`
	Credit string `json:"credit"`
}

// ControlTotal is what a subledger says one control area is worth at cutover: the open items it holds.
type ControlTotal struct {
	Total string `json:"total"`
	Count int    `json:"count"`
	Ref   string `json:"reference"`
}

// MigrationLineInput is one opening line as the source supplies it, already mapped to the target account.
type MigrationLineInput struct {
	SourceRef           string            `json:"source_ref"`
	SourceAccount       string            `json:"source_account"`
	AccountCode         string            `json:"account_code"`
	Debit               string            `json:"debit"`
	Credit              string            `json:"credit"`
	CounterpartyID      string            `json:"counterparty_id,omitempty"`
	TransactionAmount   string            `json:"transaction_amount,omitempty"`
	TransactionCurrency string            `json:"transaction_currency,omitempty"`
	ExchangeRate        string            `json:"exchange_rate,omitempty"`
	Dimensions          map[string]string `json:"dimensions,omitempty"`
	Description         string            `json:"description,omitempty"`
}

// MigrationLine is a staged line.
type MigrationLine struct {
	LineNo int `json:"line_no"`
	MigrationLineInput
}

// MigrationIssue is one thing wrong with a batch.
type MigrationIssue struct {
	Line      int    `json:"line,omitempty"`
	SourceRef string `json:"source_ref,omitempty"`
	Code      string `json:"code"`
	Detail    string `json:"detail"`
}

// MigrationReport is the outcome of a validation run, kept on the batch.
type MigrationReport struct {
	Valid     bool             `json:"valid"`
	CheckedAt time.Time        `json:"checked_at"`
	CheckedBy string           `json:"checked_by"`
	Issues    []MigrationIssue `json:"issues"`
}

// MigrationDifference is one figure of a reconciliation that does not agree.
type MigrationDifference struct {
	What     string `json:"what"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// MigrationReconciliation is the comparison of what was posted with what the source and the subledgers say.
type MigrationReconciliation struct {
	Status        string                `json:"status"` // MATCHED or BREAK
	LedgerDebit   string                `json:"ledger_debit"`
	LedgerCredit  string                `json:"ledger_credit"`
	JournalID     string                `json:"journal_id"`
	JournalNumber string                `json:"journal_number"`
	JournalHash   string                `json:"journal_hash"`
	Differences   []MigrationDifference `json:"differences"`
	CheckedAt     time.Time             `json:"checked_at"`
	CheckedBy     string                `json:"checked_by"`
}

// MigrationBatch is one migration batch.
type MigrationBatch struct {
	BatchID           string                   `json:"batch_id"`
	LegalEntityID     string                   `json:"legal_entity_id"`
	BookID            string                   `json:"accounting_book_id"`
	BatchCode         string                   `json:"batch_code"`
	Kind              string                   `json:"batch_kind"`
	CorrectsBatchID   string                   `json:"corrects_batch_id,omitempty"`
	CutoverDate       string                   `json:"cutover_date"`
	Currency          string                   `json:"currency"`
	SourceSystem      string                   `json:"source_system"`
	SourceExtractRef  string                   `json:"source_extract_ref"`
	SourceExtractHash string                   `json:"source_extract_hash"`
	MappingRef        string                   `json:"mapping_ref"`
	SourceTotals      MigrationTotals          `json:"source_totals"`
	ControlTotals     map[string]ControlTotal  `json:"control_totals"`
	LineCount         int                      `json:"line_count"`
	TotalDebit        string                   `json:"total_debit"`
	TotalCredit       string                   `json:"total_credit"`
	LinesHash         string                   `json:"lines_hash"`
	Status            string                   `json:"status"`
	Report            *MigrationReport         `json:"validation_report,omitempty"`
	CreatedBy         string                   `json:"created_by"`
	CreatedAt         time.Time                `json:"created_at"`
	ValidatedBy       string                   `json:"validated_by,omitempty"`
	ValidatedAt       *time.Time               `json:"validated_at,omitempty"`
	ApprovedBy        string                   `json:"approved_by,omitempty"`
	ApprovedAt        *time.Time               `json:"approved_at,omitempty"`
	AuthorityRef      string                   `json:"authority_ref,omitempty"`
	WindowStart       *time.Time               `json:"window_start,omitempty"`
	WindowEnd         *time.Time               `json:"window_end,omitempty"`
	JournalID         string                   `json:"journal_id,omitempty"`
	JournalNumber     string                   `json:"journal_number,omitempty"`
	PostedBy          string                   `json:"posted_by,omitempty"`
	PostedAt          *time.Time               `json:"posted_at,omitempty"`
	Reconciliation    *MigrationReconciliation `json:"reconciliation,omitempty"`
	ReconciledBy      string                   `json:"reconciled_by,omitempty"`
	ReconciledAt      *time.Time               `json:"reconciled_at,omitempty"`
	EvidenceHash      string                   `json:"evidence_hash,omitempty"`
	CancelledBy       string                   `json:"cancelled_by,omitempty"`
	CancelledAt       *time.Time               `json:"cancelled_at,omitempty"`
	CancelReason      string                   `json:"cancel_reason,omitempty"`
	Existing          bool                     `json:"existing,omitempty"`
}

// MigrationEvent is one entry of a batch's append-only history.
type MigrationEvent struct {
	Kind   string         `json:"kind"`
	Actor  string         `json:"actor"`
	At     time.Time      `json:"at"`
	Detail map[string]any `json:"detail,omitempty"`
}

// MigrationLedgerSummary is what the ledger holds for the journal a batch posted.
type MigrationLedgerSummary struct {
	Debit         string
	Credit        string
	ByControl     map[string]string // control type -> debit less credit
	JournalNumber string
	JournalHash   string
}

// MigrationPost is what the repository writes in one transaction: the OPENING journal and the batch's move to POSTED.
type MigrationPost struct {
	Tenant      string
	Batch       MigrationBatch
	Book        Book
	Lines       []MigrationLine
	Actor       string
	Correlation string
	At          time.Time
}

// MigrationRepo is the persistence boundary of migration batches.
type MigrationRepo interface {
	// InsertMigrationBatch stores a staged batch and its lines. A batch with the same code in the same book and the same content
	// is returned with existing=true; the same code with different content is refused (ErrExists).
	InsertMigrationBatch(ctx context.Context, tenant string, b *MigrationBatch, lines []MigrationLine, out OutEvent) (*MigrationBatch, bool, error)
	GetMigrationBatch(ctx context.Context, tenant, id string) (*MigrationBatch, error)
	ListMigrationBatches(ctx context.Context, tenant, entity, status string, limit int) ([]MigrationBatch, error)
	// MigrationLines returns the lines in order; a limit below 1 returns them all.
	MigrationLines(ctx context.Context, tenant, id string, offset, limit int) ([]MigrationLine, error)
	MigrationEvents(ctx context.Context, tenant, id string) ([]MigrationEvent, error)
	MarkMigrationValidated(ctx context.Context, tenant, id, actor string, report MigrationReport, at time.Time, out OutEvent) (*MigrationBatch, error)
	ApproveMigration(ctx context.Context, tenant, id, actor, authority string, windowStart, windowEnd, at time.Time, out OutEvent) (*MigrationBatch, error)
	CancelMigration(ctx context.Context, tenant, id, actor, reason string, at time.Time, out OutEvent) (*MigrationBatch, error)
	// PostMigration posts the journal and moves the batch to POSTED atomically, or does neither.
	PostMigration(ctx context.Context, p *MigrationPost) (*MigrationBatch, error)
	MigrationLedger(ctx context.Context, tenant, journalID string) (*MigrationLedgerSummary, error)
	RecordMigrationReconciliation(ctx context.Context, tenant, id, actor string, rec MigrationReconciliation, evidenceHash string, at time.Time, out OutEvent) (*MigrationBatch, error)
}

// MigrationService is the command side of migration batches.
type MigrationService struct {
	Repo  MigrationRepo
	Books Repo
	Dir   Directory
	Gate  PeriodGate
	Clock func() time.Time
}

func (s *MigrationService) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func migBad(f, why string) error {
	return exc(FamData, "MIGRATION_INVALID", "%s: %s", f, why).with("field", f)
}

// StageRequest stages a batch of opening lines from a source extract.
type StageRequest struct {
	LegalEntityID     string                  `json:"legal_entity_id"`
	BookID            string                  `json:"book_id"`
	BatchCode         string                  `json:"batch_code"`
	Kind              string                  `json:"batch_kind,omitempty"`
	CorrectsBatchID   string                  `json:"corrects_batch_id,omitempty"`
	CutoverDate       string                  `json:"cutover_date"`
	SourceSystem      string                  `json:"source_system"`
	SourceExtractRef  string                  `json:"source_extract_ref"`
	SourceExtractHash string                  `json:"source_extract_hash"`
	MappingRef        string                  `json:"mapping_ref"`
	SourceTotals      MigrationTotals         `json:"source_totals"`
	ControlTotals     map[string]ControlTotal `json:"control_totals,omitempty"`
	Lines             []MigrationLineInput    `json:"lines"`
}

// ControlNormal is the normal side of the control areas a subledger can supply a total for (true: debit).
var ControlNormal = map[string]bool{"AR": true, "BANK": true, "ASSET": true, "INVENTORY": true, "AP": false, "PAYROLL": false, "TAX": false}

// amount2 parses a non-negative amount with at most the two decimals the ledger stores.
func amount2(s string) (*big.Rat, bool) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || r.Sign() < 0 {
		return nil, false
	}
	if x := new(big.Rat).Mul(r, big.NewRat(100, 1)); !x.IsInt() {
		return nil, false
	}
	return r, true
}

// linesHash is the content hash of the staged lines, in order: the extract the batch holds itself to.
func linesHash(lines []MigrationLine) string {
	h := sha256.New()
	for _, l := range lines {
		dims := ""
		if len(l.Dimensions) > 0 {
			b, _ := json.Marshal(l.Dimensions) // map keys are sorted by the encoder
			dims = string(b)
		}
		fmt.Fprintf(h, "%d|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n", l.LineNo, l.SourceRef, l.SourceAccount, l.AccountCode, normAmount(l.Debit), normAmount(l.Credit), l.CounterpartyID,
			l.TransactionAmount, l.TransactionCurrency, l.ExchangeRate, dims, l.Description)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func normAmount(s string) string {
	if r, ok := amount2(s); ok {
		return r.FloatString(2)
	}
	return s
}

// shapeIssues are the schema-level problems of a request: nothing is staged until there are none.
func shapeIssues(req StageRequest) []MigrationIssue {
	var out []MigrationIssue
	add := func(i int, ref, code, why string) {
		if len(out) < 100 {
			out = append(out, MigrationIssue{Line: i, SourceRef: ref, Code: code, Detail: why})
		}
	}
	if len(req.Lines) == 0 {
		add(0, "", "NO_LINES", "a batch needs at least one line")
	}
	if len(req.Lines) > MaxMigrationLines {
		add(0, "", "TOO_MANY_LINES", fmt.Sprintf("a batch holds at most %d lines", MaxMigrationLines))
		return out
	}
	seen := map[string]int{}
	for i, l := range req.Lines {
		n := i + 1
		ref := strings.TrimSpace(l.SourceRef)
		switch {
		case ref == "":
			add(n, ref, "SOURCE_REF_REQUIRED", "every line names the source record it came from")
		case seen[ref] > 0:
			add(n, ref, "DUPLICATE_LINE", fmt.Sprintf("source_ref %q already appears on line %d", ref, seen[ref]))
		default:
			seen[ref] = n
		}
		if strings.TrimSpace(l.AccountCode) == "" || strings.TrimSpace(l.SourceAccount) == "" {
			add(n, ref, "ACCOUNT_REQUIRED", "a line carries the source account and the target account it maps to")
		}
		dr, dok := amount2(orZero(l.Debit))
		cr, cok := amount2(orZero(l.Credit))
		switch {
		case !dok || !cok:
			add(n, ref, "AMOUNT_INVALID", "debit and credit are non-negative amounts with at most two decimals")
		case (dr.Sign() > 0) == (cr.Sign() > 0):
			add(n, ref, "ONE_SIDE_REQUIRED", "exactly one of debit and credit is positive")
		}
		tx := l.TransactionAmount != "" || l.TransactionCurrency != "" || l.ExchangeRate != ""
		if tx {
			ta, tok := new(big.Rat).SetString(l.TransactionAmount)
			rate, rok := new(big.Rat).SetString(l.ExchangeRate)
			if !tok || !rok || ta.Sign() <= 0 || rate.Sign() <= 0 || len(l.TransactionCurrency) != 3 {
				add(n, ref, "TRANSACTION_BASIS_INVALID", "a foreign-currency line carries its amount, a three-letter currency and the exchange rate together")
			}
		}
	}
	return out
}

func orZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

// Stage validates the shape of a request and stages the batch. The extract hash is recorded as the evidence the source supplied; the
// lines hash is computed here and is what the batch is held to afterwards.
func (s *MigrationService) Stage(ctx context.Context, tenant string, a Actor, req StageRequest) (*MigrationBatch, error) {
	if !codeRE.MatchString(req.BatchCode) {
		return nil, migBad("batch_code", "1-63 letters, digits, dot, dash or underscore")
	}
	kind := strings.ToUpper(strings.TrimSpace(req.Kind))
	if kind == "" {
		kind = MigOpening
	}
	if kind != MigOpening && kind != MigCorrection {
		return nil, migBad("batch_kind", "OPENING or CORRECTION")
	}
	if (kind == MigCorrection) != (req.CorrectsBatchID != "") {
		return nil, migBad("corrects_batch_id", "a correction names the reconciled batch it corrects, and an opening names none")
	}
	if _, err := uuid.Parse(req.BookID); err != nil {
		return nil, migBad("book_id", "an accounting book id")
	}
	if _, err := uuid.Parse(req.LegalEntityID); err != nil {
		return nil, migBad("legal_entity_id", "a legal entity id")
	}
	cut, err := time.Parse("2006-01-02", req.CutoverDate)
	if err != nil {
		return nil, migBad("cutover_date", "a date, YYYY-MM-DD")
	}
	for f, v := range map[string]string{"source_system": req.SourceSystem, "source_extract_ref": req.SourceExtractRef, "mapping_ref": req.MappingRef} {
		if strings.TrimSpace(v) == "" {
			return nil, migBad(f, "required: the batch must say where its figures came from and which mapping version produced them")
		}
	}
	if !sha256RE.MatchString(req.SourceExtractHash) {
		return nil, migBad("source_extract_hash", "the SHA-256 of the source extract, as 64 lower-case hex digits")
	}
	sd, sdok := amount2(req.SourceTotals.Debit)
	sc, scok := amount2(req.SourceTotals.Credit)
	if !sdok || !scok {
		return nil, migBad("source_totals", "the debit and credit totals of the source trial balance, with at most two decimals")
	}
	for k, c := range req.ControlTotals {
		if _, ok := ControlNormal[k]; !ok {
			return nil, migBad("control_totals", fmt.Sprintf("%s is not a control area a subledger total can be supplied for", k))
		}
		if _, ok := amount2(c.Total); !ok || strings.TrimSpace(c.Ref) == "" {
			return nil, migBad("control_totals", fmt.Sprintf("%s needs a non-negative total with at most two decimals and the reference of the subledger report", k))
		}
	}
	if issues := shapeIssues(req); len(issues) > 0 {
		return nil, exc(FamData, "MIGRATION_INVALID", "the extract has %d problem(s) and nothing was staged", len(issues)).with("issues", issues)
	}
	bk, err := s.Books.GetBook(ctx, tenant, req.BookID)
	if err != nil {
		return nil, err
	}
	switch {
	case bk.LegalEntityID != req.LegalEntityID:
		return nil, exc(FamRule, "MIGRATION_BOOK_INVALID", "book %s does not belong to entity %s", bk.BookCode, req.LegalEntityID)
	case bk.BookType == BookGroup:
		return nil, exc(FamRule, "MIGRATION_BOOK_INVALID", "opening balances are not migrated into a group book: it is built by consolidation")
	case bk.Status != BookActive:
		return nil, exc(FamRule, "MIGRATION_BOOK_INVALID", "book %s is %s", bk.BookCode, strings.ToLower(bk.Status))
	}
	if kind == MigCorrection {
		prior, perr := s.Repo.GetMigrationBatch(ctx, tenant, req.CorrectsBatchID)
		if perr != nil {
			return nil, perr
		}
		if prior.Status != MigReconciled || prior.BookID != bk.BookID || prior.LegalEntityID != bk.LegalEntityID {
			return nil, exc(FamRule, "MIGRATION_CORRECTION_INVALID", "a correction names a reconciled batch of the same entity and book")
		}
	}

	lines := make([]MigrationLine, len(req.Lines))
	totalDr, totalCr := new(big.Rat), new(big.Rat)
	for i, l := range req.Lines {
		l.SourceRef, l.AccountCode, l.SourceAccount = strings.TrimSpace(l.SourceRef), strings.TrimSpace(l.AccountCode), strings.TrimSpace(l.SourceAccount)
		l.Debit, l.Credit = normAmount(orZero(l.Debit)), normAmount(orZero(l.Credit))
		lines[i] = MigrationLine{LineNo: i + 1, MigrationLineInput: l}
		d, _ := amount2(l.Debit)
		c, _ := amount2(l.Credit)
		totalDr.Add(totalDr, d)
		totalCr.Add(totalCr, c)
	}
	b := &MigrationBatch{
		BatchID: uuid.NewString(), LegalEntityID: bk.LegalEntityID, BookID: bk.BookID, BatchCode: req.BatchCode, Kind: kind, CorrectsBatchID: req.CorrectsBatchID,
		CutoverDate: cut.Format("2006-01-02"), Currency: bk.FunctionalCurrency, SourceSystem: strings.TrimSpace(req.SourceSystem), SourceExtractRef: strings.TrimSpace(req.SourceExtractRef),
		SourceExtractHash: req.SourceExtractHash, MappingRef: strings.TrimSpace(req.MappingRef),
		SourceTotals: MigrationTotals{Debit: sd.FloatString(2), Credit: sc.FloatString(2)}, ControlTotals: req.ControlTotals, LineCount: len(lines),
		TotalDebit: totalDr.FloatString(2), TotalCredit: totalCr.FloatString(2), LinesHash: linesHash(lines), Status: MigStaged, CreatedBy: a.PrincipalID, CreatedAt: s.now(),
	}
	if b.ControlTotals == nil {
		b.ControlTotals = map[string]ControlTotal{}
	}
	out, existing, err := s.Repo.InsertMigrationBatch(ctx, tenant, b, lines, s.event("migration-batch.staged", b, a, map[string]any{"line_count": b.LineCount, "lines_hash": b.LinesHash,
		"source_extract_hash": b.SourceExtractHash}))
	if err != nil {
		return nil, err
	}
	out.Existing = existing
	return out, nil
}

func (s *MigrationService) event(fact string, b *MigrationBatch, a Actor, extra map[string]any) OutEvent {
	d := map[string]any{"batch_id": b.BatchID, "batch_code": b.BatchCode, "legal_entity_id": b.LegalEntityID, "book_id": b.BookID, "batch_kind": b.Kind, "cutover_date": b.CutoverDate}
	for k, v := range extra {
		d[k] = v
	}
	return OutEvent{Fact: fact, AggregateType: "migration-batch", AggregateID: b.BatchID, Actor: a.PrincipalID, CorrelationID: a.CorrelationID, LegalEntityID: b.LegalEntityID, Data: d}
}

// MigrationAccountRow is what the batch does to one account.
type MigrationAccountRow struct {
	Account     string `json:"account_code"`
	ControlType string `json:"control_type,omitempty"`
	Lines       int    `json:"lines"`
	Debit       string `json:"debit"`
	Credit      string `json:"credit"`
	Net         string `json:"net_debit"`
}

// MigrationControlRow compares what the staged lines hold on a control area with what the subledger reported.
type MigrationControlRow struct {
	ControlType string `json:"control_type"`
	Staged      string `json:"staged_balance"`
	Supplied    string `json:"subledger_total,omitempty"`
	Reference   string `json:"reference,omitempty"`
	Match       bool   `json:"match"`
}

// MigrationAnalysis is the result of checking a batch against the chart, the dimensions and the totals it declares.
type MigrationAnalysis struct {
	Issues      []MigrationIssue      `json:"issues"`
	Accounts    []MigrationAccountRow `json:"accounts"`
	Controls    []MigrationControlRow `json:"controls"`
	TotalDebit  string                `json:"total_debit"`
	TotalCredit string                `json:"total_credit"`
}

// analyze checks every staged line the way a posting would be checked, and the batch against the totals it declares. It reads
// reference data only.
func (s *MigrationService) analyze(ctx context.Context, bk *Book, b *MigrationBatch, lines []MigrationLine) (*MigrationAnalysis, error) {
	cut, _ := time.Parse("2006-01-02", b.CutoverDate)
	an := &MigrationAnalysis{Issues: []MigrationIssue{}, Accounts: []MigrationAccountRow{}, Controls: []MigrationControlRow{}}
	add := func(l MigrationLine, code, why string) {
		if len(an.Issues) < 500 {
			an.Issues = append(an.Issues, MigrationIssue{Line: l.LineNo, SourceRef: l.SourceRef, Code: code, Detail: why})
		}
	}
	if !bk.InForce(cut) {
		an.Issues = append(an.Issues, MigrationIssue{Code: "BOOK_NOT_IN_FORCE", Detail: fmt.Sprintf("book %s is %s on the cutover date %s", bk.BookCode, strings.ToLower(bk.Status), b.CutoverDate)})
	}
	if bk.FunctionalCurrency != b.Currency {
		an.Issues = append(an.Issues, MigrationIssue{Code: "CURRENCY_CHANGED", Detail: fmt.Sprintf("the book keeps its accounts in %s and the batch was staged in %s", bk.FunctionalCurrency, b.Currency)})
	}
	type acct struct {
		info *AccountInfo
		err  error
	}
	cache := map[string]acct{}
	type roll struct {
		debit, credit *big.Rat
		lines         int
		control       string
	}
	byAccount := map[string]*roll{}
	byControl := map[string]*big.Rat{}
	totalDr, totalCr := new(big.Rat), new(big.Rat)
	dimOK := map[string]bool{}

	for _, l := range lines {
		a, seen := cache[l.AccountCode]
		if !seen {
			info, rerr := s.Dir.ResolveAccount(ctx, AccountRef{AccountCode: l.AccountCode}, bk.BookID, cut)
			a = acct{info, rerr}
			cache[l.AccountCode] = a
		}
		dr, _ := amount2(l.Debit)
		cr, _ := amount2(l.Credit)
		totalDr.Add(totalDr, dr)
		totalCr.Add(totalCr, cr)
		ro := byAccount[l.AccountCode]
		if ro == nil {
			ro = &roll{debit: new(big.Rat), credit: new(big.Rat)}
			byAccount[l.AccountCode] = ro
		}
		ro.debit.Add(ro.debit, dr)
		ro.credit.Add(ro.credit, cr)
		ro.lines++

		if a.err != nil || a.info == nil {
			add(l, "ACCOUNT_UNAVAILABLE", fmt.Sprintf("account %s is not in the chart of accounts", l.AccountCode))
			continue
		}
		info := a.info
		ro.control = info.ControlType
		if !info.Active || !info.Postable {
			add(l, "ACCOUNT_UNAVAILABLE", fmt.Sprintf("account %s is not an active, postable account on %s", l.AccountCode, b.CutoverDate))
			continue
		}
		if len(info.BookIDs) > 0 {
			in := false
			for _, id := range info.BookIDs {
				in = in || id == bk.BookID
			}
			if !in {
				add(l, "ACCOUNT_NOT_IN_BOOK", fmt.Sprintf("account %s is not valid in book %s", l.AccountCode, bk.BookCode))
			}
		}
		ccy := bk.FunctionalCurrency
		if l.TransactionCurrency != "" {
			ccy = l.TransactionCurrency
			if l.TransactionCurrency == bk.FunctionalCurrency {
				add(l, "TRANSACTION_BASIS_INVALID", "a line in the functional currency carries no transaction amount, currency or rate")
			}
		}
		if len(info.CurrencyRestricted) > 0 {
			ok := false
			for _, c := range info.CurrencyRestricted {
				ok = ok || c == ccy
			}
			if !ok {
				add(l, "ACCOUNT_CURRENCY_RESTRICTED", fmt.Sprintf("account %s is restricted to %s and the line is in %s", l.AccountCode, strings.Join(info.CurrencyRestricted, ", "), ccy))
			}
		}
		if (info.CounterpartyRequired || info.ControlType == "AR" || info.ControlType == "AP") && strings.TrimSpace(l.CounterpartyID) == "" {
			add(l, "COUNTERPARTY_REQUIRED", fmt.Sprintf("account %s is a control account: every open item names its counterparty", l.AccountCode))
		}
		for d, v := range l.Dimensions {
			key := d + "\x00" + v
			ok, seen := dimOK[key]
			if !seen {
				var derr error
				ok, derr = s.Dir.DimensionAllowed(ctx, d, v, bk.LegalEntityID, bk.BookID, cut)
				ok = ok && derr == nil
				dimOK[key] = ok
			}
			if !ok {
				add(l, "DIMENSION_INVALID", fmt.Sprintf("%s=%s is not an allowed value of dimension %s for this entity on %s", d, v, d, b.CutoverDate))
			}
		}
		if info.ControlType != "" {
			if _, supported := ControlNormal[info.ControlType]; supported {
				net := new(big.Rat).Sub(dr, cr)
				if !ControlNormal[info.ControlType] {
					net.Neg(net)
				}
				if byControl[info.ControlType] == nil {
					byControl[info.ControlType] = new(big.Rat)
				}
				byControl[info.ControlType].Add(byControl[info.ControlType], net)
			}
		}
	}

	an.TotalDebit, an.TotalCredit = totalDr.FloatString(2), totalCr.FloatString(2)
	if totalDr.Cmp(totalCr) != 0 {
		an.Issues = append(an.Issues, MigrationIssue{Code: "BATCH_UNBALANCED", Detail: fmt.Sprintf("the lines debit %s and credit %s: an opening trial balance balances exactly", an.TotalDebit, an.TotalCredit)})
	}
	if an.TotalDebit != b.SourceTotals.Debit || an.TotalCredit != b.SourceTotals.Credit {
		an.Issues = append(an.Issues, MigrationIssue{Code: "SOURCE_TOTALS_MISMATCH", Detail: fmt.Sprintf("the source trial balance says debit %s and credit %s, the lines hold debit %s and credit %s",
			b.SourceTotals.Debit, b.SourceTotals.Credit, an.TotalDebit, an.TotalCredit)})
	}
	if an.TotalDebit != b.TotalDebit || an.TotalCredit != b.TotalCredit || len(lines) != b.LineCount || linesHash(lines) != b.LinesHash {
		an.Issues = append(an.Issues, MigrationIssue{Code: "BATCH_ALTERED", Detail: "the staged lines no longer hash to the content the batch was staged with"})
	}

	codes := make([]string, 0, len(byAccount))
	for c := range byAccount {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		ro := byAccount[c]
		an.Accounts = append(an.Accounts, MigrationAccountRow{Account: c, ControlType: ro.control, Lines: ro.lines, Debit: ro.debit.FloatString(2), Credit: ro.credit.FloatString(2),
			Net: new(big.Rat).Sub(ro.debit, ro.credit).FloatString(2)})
	}
	areas := map[string]bool{}
	for k := range byControl {
		areas[k] = true
	}
	for k := range b.ControlTotals {
		areas[k] = true
	}
	names := make([]string, 0, len(areas))
	for k := range areas {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		staged := new(big.Rat)
		if v := byControl[k]; v != nil {
			staged = v
		}
		row := MigrationControlRow{ControlType: k, Staged: staged.FloatString(2)}
		if sup, ok := b.ControlTotals[k]; ok {
			want, _ := amount2(sup.Total)
			row.Supplied, row.Reference = want.FloatString(2), sup.Ref
			row.Match = want != nil && want.Cmp(staged) == 0
			if !row.Match {
				an.Issues = append(an.Issues, MigrationIssue{Code: "CONTROL_TOTAL_MISMATCH", Detail: fmt.Sprintf("the %s subledger reports %s and the staged lines hold %s on %s accounts (reference %s)",
					k, row.Supplied, row.Staged, k, sup.Ref)})
			}
		} else if staged.Sign() != 0 {
			row.Match = false
			an.Issues = append(an.Issues, MigrationIssue{Code: "CONTROL_TOTAL_MISSING", Detail: fmt.Sprintf("the staged lines hold %s on %s control accounts and the batch carries no subledger total to reconcile it to", row.Staged, k)})
		}
		an.Controls = append(an.Controls, row)
	}
	return an, nil
}

func (s *MigrationService) load(ctx context.Context, tenant, id string) (*MigrationBatch, []MigrationLine, *Book, error) {
	b, err := s.Repo.GetMigrationBatch(ctx, tenant, id)
	if err != nil {
		return nil, nil, nil, err
	}
	lines, err := s.Repo.MigrationLines(ctx, tenant, id, 0, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	bk, err := s.Books.GetBook(ctx, tenant, b.BookID)
	if err != nil {
		return nil, nil, nil, err
	}
	return b, lines, bk, nil
}

// MigrationPreview is the human-readable reconciliation shown before anything is posted.
type MigrationPreview struct {
	Batch    *MigrationBatch    `json:"batch"`
	Analysis *MigrationAnalysis `json:"analysis"`
	Ready    bool               `json:"ready_to_approve"`
}

// Preview shows what posting the batch would do, and every reason it would be refused. It writes nothing.
func (s *MigrationService) Preview(ctx context.Context, tenant, id string) (*MigrationPreview, error) {
	b, lines, bk, err := s.load(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	an, err := s.analyze(ctx, bk, b, lines)
	if err != nil {
		return nil, err
	}
	return &MigrationPreview{Batch: b, Analysis: an, Ready: len(an.Issues) == 0}, nil
}

// Validate checks the batch and records the report. A clean report moves it to VALIDATED; a dirty one keeps it STAGED with the issues
// on record, and the batch is cancelled and staged again from a corrected extract: staged lines are never edited.
func (s *MigrationService) Validate(ctx context.Context, tenant string, a Actor, id string) (*MigrationBatch, *MigrationAnalysis, error) {
	b, lines, bk, err := s.load(ctx, tenant, id)
	if err != nil {
		return nil, nil, err
	}
	if b.Status != MigStaged && b.Status != MigValidated {
		return nil, nil, ErrState
	}
	an, err := s.analyze(ctx, bk, b, lines)
	if err != nil {
		return nil, nil, err
	}
	rep := MigrationReport{Valid: len(an.Issues) == 0, CheckedAt: s.now(), CheckedBy: a.PrincipalID, Issues: an.Issues}
	if b.Status == MigValidated && !rep.Valid {
		// reference data moved since the batch was validated: it goes back to being unfit to approve
		return nil, an, exc(FamData, "MIGRATION_INVALID", "the batch no longer validates: %d issue(s)", len(an.Issues)).with("issues", an.Issues)
	}
	out, err := s.Repo.MarkMigrationValidated(ctx, tenant, id, a.PrincipalID, rep, s.now(), s.event("migration-batch.validated", b, a, map[string]any{"valid": rep.Valid, "issues": len(rep.Issues)}))
	return out, an, err
}

// Approve records the named authority and the cutover window. The approver is not the person who staged the batch.
func (s *MigrationService) Approve(ctx context.Context, tenant string, a Actor, id, authority string, windowStart, windowEnd time.Time) (*MigrationBatch, error) {
	b, err := s.Repo.GetMigrationBatch(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if b.Status != MigValidated {
		return nil, ErrState
	}
	if b.CreatedBy == a.PrincipalID {
		return nil, ErrSelfApproval
	}
	if strings.TrimSpace(authority) == "" {
		return nil, migBad("authority_ref", "the migration authority that permits this cutover")
	}
	if windowStart.IsZero() || !windowEnd.After(windowStart) {
		return nil, migBad("window_end", "a cutover window that ends after it starts")
	}
	if !windowEnd.After(s.now()) {
		return nil, migBad("window_end", "a cutover window that has not already ended")
	}
	return s.Repo.ApproveMigration(ctx, tenant, id, a.PrincipalID, strings.TrimSpace(authority), windowStart.UTC(), windowEnd.UTC(), s.now(),
		s.event("migration-batch.approved", b, a, map[string]any{"authority_ref": authority, "window_start": windowStart.UTC(), "window_end": windowEnd.UTC()}))
}

// Cancel withdraws a batch that has not been posted.
func (s *MigrationService) Cancel(ctx context.Context, tenant string, a Actor, id, reason string) (*MigrationBatch, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, migBad("reason", "required")
	}
	b, err := s.Repo.GetMigrationBatch(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if b.Status == MigPosted || b.Status == MigReconciled || b.Status == MigCancelled {
		return nil, ErrState
	}
	return s.Repo.CancelMigration(ctx, tenant, id, a.PrincipalID, strings.TrimSpace(reason), s.now(), s.event("migration-batch.cancelled", b, a, map[string]any{"reason": reason}))
}

// Post posts an approved batch as one OPENING journal inside its cutover window. The batch is checked again first, against the reference
// data as it is now, and the period gate is applied: a migration is not a way round either. A batch that has already posted returns
// unchanged.
func (s *MigrationService) Post(ctx context.Context, tenant string, a Actor, id string) (*MigrationBatch, error) {
	b, lines, bk, err := s.load(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if b.Status == MigPosted || b.Status == MigReconciled {
		b.Existing = true
		return b, nil
	}
	if b.Status != MigApproved {
		return nil, ErrState
	}
	now := s.now()
	if b.WindowStart == nil || b.WindowEnd == nil || now.Before(*b.WindowStart) || !now.Before(*b.WindowEnd) {
		return nil, exc(FamAuth, "MIGRATION_WINDOW_CLOSED", "the batch posts only inside its approved cutover window (%s to %s)", fmtTime(b.WindowStart), fmtTime(b.WindowEnd))
	}
	an, err := s.analyze(ctx, bk, b, lines)
	if err != nil {
		return nil, err
	}
	if len(an.Issues) > 0 {
		return nil, exc(FamData, "MIGRATION_INVALID", "the batch no longer validates: %d issue(s); nothing was posted", len(an.Issues)).with("issues", an.Issues)
	}
	cut, _ := time.Parse("2006-01-02", b.CutoverDate)
	if s.Gate != nil {
		if err := s.Gate.CheckPostable(ctx, tenant, b.LegalEntityID, FiscalPeriodOf(cut), cut, false, "", a.PrincipalID); err != nil {
			return nil, err
		}
	}
	return s.Repo.PostMigration(ctx, &MigrationPost{Tenant: tenant, Batch: *b, Book: *bk, Lines: lines, Actor: a.PrincipalID, Correlation: "migration:" + b.BatchID, At: now})
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "unset"
	}
	return t.UTC().Format(time.RFC3339)
}

// Reconcile compares what the ledger holds for the posted journal with the source trial balance and the subledger control totals. A
// match freezes the batch with its evidence; a break is recorded and the batch stays POSTED until it is explained and reconciled again.
func (s *MigrationService) Reconcile(ctx context.Context, tenant string, a Actor, id string) (*MigrationBatch, error) {
	b, err := s.Repo.GetMigrationBatch(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if b.Status == MigReconciled {
		b.Existing = true
		return b, nil
	}
	if b.Status != MigPosted || b.JournalID == "" {
		return nil, ErrState
	}
	led, err := s.Repo.MigrationLedger(ctx, tenant, b.JournalID)
	if err != nil {
		return nil, err
	}
	rec := MigrationReconciliation{LedgerDebit: led.Debit, LedgerCredit: led.Credit, JournalID: b.JournalID, JournalNumber: led.JournalNumber, JournalHash: led.JournalHash,
		Differences: []MigrationDifference{}, CheckedAt: s.now(), CheckedBy: a.PrincipalID}
	cmp := func(what, want, got string) {
		w, wok := new(big.Rat).SetString(want)
		g, gok := new(big.Rat).SetString(got)
		if !wok || !gok || w.Cmp(g) != 0 {
			rec.Differences = append(rec.Differences, MigrationDifference{What: what, Expected: want, Actual: got})
		}
	}
	cmp("debit total: source trial balance to ledger", b.SourceTotals.Debit, led.Debit)
	cmp("credit total: source trial balance to ledger", b.SourceTotals.Credit, led.Credit)
	areas := make([]string, 0, len(b.ControlTotals))
	for k := range b.ControlTotals {
		areas = append(areas, k)
	}
	sort.Strings(areas)
	for _, k := range areas {
		net, _ := new(big.Rat).SetString(orZero(led.ByControl[k]))
		if !ControlNormal[k] {
			net.Neg(net)
		}
		want, _ := amount2(b.ControlTotals[k].Total)
		cmp(k+" control total: subledger to ledger", want.FloatString(2), net.FloatString(2))
	}
	rec.Status = "MATCHED"
	if len(rec.Differences) > 0 {
		rec.Status = "BREAK"
	}
	hash := ""
	if rec.Status == "MATCHED" {
		hash = evidenceHash(b, rec)
	}
	return s.Repo.RecordMigrationReconciliation(ctx, tenant, id, a.PrincipalID, rec, hash, s.now(),
		s.event("migration-batch."+strings.ToLower(map[string]string{"MATCHED": "reconciled", "BREAK": "break-raised"}[rec.Status]), b, a, map[string]any{"status": rec.Status, "differences": len(rec.Differences), "evidence_hash": hash}))
}

// evidenceHash binds everything the batch rests on: the extract, the lines, the mapping, the approval and the reconciliation.
func evidenceHash(b *MigrationBatch, rec MigrationReconciliation) string {
	ws, we := "", ""
	if b.WindowStart != nil {
		ws = b.WindowStart.UTC().Format(time.RFC3339Nano)
	}
	if b.WindowEnd != nil {
		we = b.WindowEnd.UTC().Format(time.RFC3339Nano)
	}
	body, _ := json.Marshal(struct {
		Extract, Lines, Mapping, ApprovedBy, Authority, WindowStart, WindowEnd, JournalHash, Status, LedgerDebit, LedgerCredit string
		SourceTotals                                                                                                           MigrationTotals
		ControlTotals                                                                                                          map[string]ControlTotal
	}{b.SourceExtractHash, b.LinesHash, b.MappingRef, b.ApprovedBy, b.AuthorityRef, ws, we, rec.JournalHash, rec.Status, rec.LedgerDebit, rec.LedgerCredit, b.SourceTotals, b.ControlTotals})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// MigrationEvidence is the frozen evidence of a batch, with whether it still verifies.
type MigrationEvidence struct {
	Batch         *MigrationBatch  `json:"batch"`
	History       []MigrationEvent `json:"history"`
	LinesVerified bool             `json:"lines_verified"`
	Verified      bool             `json:"evidence_verified"`
}

// Evidence returns the batch with its history and recomputes the hashes from what is stored: the lines against the content hash they
// were staged with, and, once reconciled, the evidence hash from the approval, the mapping and the reconciliation.
func (s *MigrationService) Evidence(ctx context.Context, tenant, id string) (*MigrationEvidence, error) {
	b, err := s.Repo.GetMigrationBatch(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	lines, err := s.Repo.MigrationLines(ctx, tenant, id, 0, 0)
	if err != nil {
		return nil, err
	}
	hist, err := s.Repo.MigrationEvents(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	ev := &MigrationEvidence{Batch: b, History: hist, LinesVerified: linesHash(lines) == b.LinesHash}
	ev.Verified = ev.LinesVerified && (b.Status != MigReconciled || (b.Reconciliation != nil && evidenceHash(b, *b.Reconciliation) == b.EvidenceHash))
	return ev, nil
}
