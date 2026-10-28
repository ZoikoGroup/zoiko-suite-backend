package kernel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Signed balance snapshots (standard §10.2, §26): the balances of a book at a ledger watermark, retained and signed. A close
// snapshot is taken at the watermark the period was sealed at, so it describes exactly the period that was closed; it can be
// recomputed from the ledger at any time, and a snapshot that no longer matches (or whose signature does not verify) says so.

// Snapshot purposes.
const (
	SnapshotClose         = "CLOSE"
	SnapshotReclose       = "RECLOSE"
	SnapshotConsolidation = "CONSOLIDATION_FREEZE"
	// SnapshotConsolidationClose is the signed balance of a group book when its consolidation is certified.
	SnapshotConsolidationClose = "CONSOLIDATION_CLOSE"
	SnapshotAdHoc              = "AD_HOC"
)

// SnapshotLine is the balance of one account: cumulative to the as-of date, and the movement inside the period.
type SnapshotLine struct {
	AccountCode  string `json:"account_code"`
	Debit        string `json:"debit_total"`
	Credit       string `json:"credit_total"`
	Net          string `json:"net_balance"` // debit minus credit
	PeriodDebit  string `json:"period_debit"`
	PeriodCredit string `json:"period_credit"`
}

// BalanceSnapshot is a retained, signed set of balances.
type BalanceSnapshot struct {
	SnapshotID      string         `json:"snapshot_id"`
	TenantID        string         `json:"-"`
	LegalEntityID   string         `json:"legal_entity_id"`
	BookID          string         `json:"accounting_book_id"`
	FiscalPeriod    string         `json:"fiscal_period"`
	AsOf            string         `json:"as_of"`
	Purpose         string         `json:"purpose"`
	LedgerWatermark int64          `json:"ledger_watermark"`
	Lines           []SnapshotLine `json:"lines"`
	AccountCount    int            `json:"account_count"`
	TotalDebit      string         `json:"total_debit"`
	TotalCredit     string         `json:"total_credit"`
	ContentHash     string         `json:"content_hash"`
	Signature       string         `json:"signature"`
	KeyID           string         `json:"key_id"`
	SignedBy        string         `json:"signed_by"`
	CreatedAt       time.Time      `json:"created_at"`
}

// Signer signs and verifies snapshot hashes. A kernel without one cannot produce a signed snapshot, and says so.
type Signer interface {
	Sign(hash string) (signature, keyID string)
	Verify(hash, signature, keyID string) (valid, keyKnown bool)
}

// HMACSigner signs with HMAC-SHA256 under a key held in the service's configuration.
type HMACSigner struct {
	Key   []byte
	KeyID string
}

// Sign implements Signer.
func (s HMACSigner) Sign(hash string) (string, string) {
	m := hmac.New(sha256.New, s.Key)
	m.Write([]byte(hash))
	return hex.EncodeToString(m.Sum(nil)), s.KeyID
}

// Verify implements Signer.
func (s HMACSigner) Verify(hash, signature, keyID string) (bool, bool) {
	if keyID != s.KeyID {
		return false, false
	}
	want, _ := s.Sign(hash)
	return hmac.Equal([]byte(want), []byte(signature)), true
}

// SnapshotRepo is the persistence boundary of snapshots.
type SnapshotRepo interface {
	// Balances reads the balances of a book from one consistent snapshot of the ledger: up to upToSeq when given, else up to the
	// highest entry at that moment. It returns the watermark it read to.
	Balances(ctx context.Context, tenant, entity, bookCode string, asOf, periodStart time.Time, upToSeq int64) (int64, []SnapshotLine, error)
	InsertSnapshot(ctx context.Context, s *BalanceSnapshot, out OutEvent) error
	GetSnapshot(ctx context.Context, tenant, id string) (*BalanceSnapshot, error)
	ListSnapshots(ctx context.Context, tenant, entity, bookID, period string) ([]BalanceSnapshot, error)
}

// SnapshotService is the command side of snapshots.
type SnapshotService struct {
	Repo   SnapshotRepo
	Books  Repo
	Seals  SealRepo
	Signer Signer
	Clock  func() time.Time
}

func (s *SnapshotService) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

// SnapshotRequest asks for a snapshot of one book and period.
type SnapshotRequest struct {
	BookID       string `json:"book_id"`
	FiscalPeriod string `json:"fiscal_period"`
	AsOf         string `json:"as_of,omitempty"`
	Purpose      string `json:"purpose,omitempty"`
}

func periodBounds(period string) (start, end time.Time, ok bool) {
	t, err := time.Parse("2006-01", period)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	return t, t.AddDate(0, 1, -1), true
}

func contentHash(s *BalanceSnapshot) string {
	b, _ := json.Marshal(struct {
		T, E, B, P, A string
		W             int64
		L             []SnapshotLine
	}{s.TenantID, s.LegalEntityID, s.BookID, s.FiscalPeriod, s.AsOf, s.LedgerWatermark, s.Lines})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func totals(lines []SnapshotLine) (dr, cr *big.Rat) {
	dr, cr = new(big.Rat), new(big.Rat)
	for _, l := range lines {
		dr.Add(dr, dec(l.Debit))
		cr.Add(cr, dec(l.Credit))
	}
	return
}

// Create takes, signs and retains a snapshot. A close or re-close snapshot is taken at the watermark the period was sealed at and
// needs the period to be sealed.
func (s *SnapshotService) Create(ctx context.Context, tenant string, a Actor, req SnapshotRequest) (*BalanceSnapshot, error) {
	bad := func(f, why string) error { return exc(FamData, "SNAPSHOT_INVALID", "%s: %s", f, why).with("field", f) }
	if s.Signer == nil {
		return nil, exc(FamExt, "SIGNING_NOT_CONFIGURED", "no snapshot signing key is configured: a snapshot cannot be signed")
	}
	if _, err := uuid.Parse(req.BookID); err != nil {
		return nil, bad("book_id", "an accounting book id")
	}
	purpose := strings.ToUpper(strings.TrimSpace(req.Purpose))
	if purpose == "" {
		purpose = SnapshotAdHoc
	}
	if purpose != SnapshotClose && purpose != SnapshotReclose && purpose != SnapshotConsolidation && purpose != SnapshotConsolidationClose && purpose != SnapshotAdHoc {
		return nil, bad("purpose", "CLOSE, RECLOSE, CONSOLIDATION_FREEZE, CONSOLIDATION_CLOSE or AD_HOC")
	}
	pstart, pend, ok := periodBounds(req.FiscalPeriod)
	if !ok {
		return nil, bad("fiscal_period", "a period, YYYY-MM")
	}
	asOf := pend
	if req.AsOf != "" {
		var err error
		if asOf, err = time.Parse("2006-01-02", req.AsOf); err != nil {
			return nil, bad("as_of", "a date, YYYY-MM-DD")
		}
	}
	bk, err := s.Books.GetBook(ctx, tenant, req.BookID)
	if err != nil {
		return nil, err
	}
	var upTo int64
	if purpose == SnapshotClose || purpose == SnapshotReclose {
		seal, _, serr := s.Seals.GetSeal(ctx, tenant, bk.LegalEntityID, req.FiscalPeriod)
		if errors.Is(serr, ErrNotFound) || (serr == nil && seal.Status != "SEALED") {
			return nil, exc(FamPeriod, "PERIOD_NOT_SEALED", "a %s snapshot is taken of a sealed period, at the watermark it was sealed at", strings.ToLower(purpose)).with("fiscal_period", req.FiscalPeriod)
		}
		if serr != nil {
			return nil, serr
		}
		upTo = seal.Watermark
	}
	wm, lines, err := s.Repo.Balances(ctx, tenant, bk.LegalEntityID, bk.BookCode, asOf, pstart, upTo)
	if err != nil {
		return nil, err
	}
	dr, cr := totals(lines)
	if dr.Cmp(cr) != 0 {
		return nil, exc(FamBal, "LEDGER_UNBALANCED", "the ledger of book %s does not balance at watermark %d: debits %s, credits %s", bk.BookCode, wm, dr.FloatString(2), cr.FloatString(2))
	}
	snap := &BalanceSnapshot{SnapshotID: uuid.NewString(), TenantID: tenant, LegalEntityID: bk.LegalEntityID, BookID: bk.BookID, FiscalPeriod: req.FiscalPeriod, AsOf: asOf.Format("2006-01-02"),
		Purpose: purpose, LedgerWatermark: wm, Lines: lines, AccountCount: len(lines), TotalDebit: dr.FloatString(2), TotalCredit: cr.FloatString(2), SignedBy: a.PrincipalID, CreatedAt: s.now()}
	snap.ContentHash = contentHash(snap)
	snap.Signature, snap.KeyID = s.Signer.Sign(snap.ContentHash)
	if err := s.Repo.InsertSnapshot(ctx, snap, OutEvent{Fact: "balance-snapshot.signed", AggregateType: "balance-snapshot", AggregateID: snap.SnapshotID, Actor: a.PrincipalID, CorrelationID: a.CorrelationID,
		LegalEntityID: snap.LegalEntityID, Data: map[string]any{"snapshot_id": snap.SnapshotID, "book_id": snap.BookID, "fiscal_period": snap.FiscalPeriod, "purpose": purpose, "ledger_watermark": wm,
			"content_hash": snap.ContentHash, "key_id": snap.KeyID, "total_debit": snap.TotalDebit}}); err != nil {
		return nil, err
	}
	return snap, nil
}

// Verify recomputes a snapshot from the ledger at its watermark and checks its signature.
func (s *SnapshotService) Verify(ctx context.Context, tenant, id string) (map[string]any, error) {
	snap, err := s.Repo.GetSnapshot(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	bk, err := s.Books.GetBook(ctx, tenant, snap.BookID)
	if err != nil {
		return nil, err
	}
	asOf, _ := time.Parse("2006-01-02", snap.AsOf)
	pstart, _, _ := periodBounds(snap.FiscalPeriod)
	_, lines, err := s.Repo.Balances(ctx, tenant, snap.LegalEntityID, bk.BookCode, asOf, pstart, snap.LedgerWatermark)
	if err != nil {
		return nil, err
	}
	again := *snap
	again.Lines = lines
	reproducible := contentHash(&again) == snap.ContentHash
	storedOK := contentHash(snap) == snap.ContentHash
	res := map[string]any{"snapshot_id": snap.SnapshotID, "content_hash": snap.ContentHash, "ledger_watermark": snap.LedgerWatermark, "reproducible_from_ledger": reproducible, "content_matches_hash": storedOK}
	if s.Signer == nil {
		res["signature_valid"], res["key_known"] = false, false
	} else {
		valid, known := s.Signer.Verify(snap.ContentHash, snap.Signature, snap.KeyID)
		res["signature_valid"], res["key_known"], res["key_id"] = valid, known, snap.KeyID
	}
	res["verified"] = reproducible && storedOK && res["signature_valid"] == true
	return res, nil
}
