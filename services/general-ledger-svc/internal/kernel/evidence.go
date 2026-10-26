package kernel

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Close evidence (standard §10.2): what the kernel can say about whether a period of an entity is ready to close. The period close
// service asks; the kernel answers from its own records — open reconciliation cases, failed postings, foreign items awaiting
// revaluation, journals still in the review queue — and reports what a close would rest on (the seal and the signed snapshot).
// It never closes anything: closing belongs to the period close service.

// PendingRepo counts the journals of an entity and period that have not been posted or withdrawn.
type PendingRepo interface {
	PendingJournals(ctx context.Context, tenant, entity, period string) (int, error)
}

// BookEvidence is the evidence for one book.
type BookEvidence struct {
	BookID         string       `json:"accounting_book_id"`
	BookCode       string       `json:"book_code"`
	Ready          bool         `json:"ready"`
	Issues         []string     `json:"blocking_issues"`
	Reconciliation *CaseStatus  `json:"reconciliation"`
	Revaluation    RevalStatus  `json:"revaluation"`
	PendingJournal int          `json:"journals_in_review_queue"`
	Snapshot       *SnapshotRef `json:"latest_snapshot,omitempty"`
}

// RevalStatus says whether the period's foreign items have been revalued.
type RevalStatus struct {
	OpenForeignItems int    `json:"open_foreign_items"`
	Required         bool   `json:"required"`
	RunID            string `json:"revaluation_run_id,omitempty"`
	Done             bool   `json:"done"`
}

// SnapshotRef points at a retained snapshot.
type SnapshotRef struct {
	SnapshotID      string    `json:"snapshot_id"`
	Purpose         string    `json:"purpose"`
	LedgerWatermark int64     `json:"ledger_watermark"`
	ContentHash     string    `json:"content_hash"`
	CreatedAt       time.Time `json:"created_at"`
}

// CloseEvidence is the kernel's answer for an entity and period.
type CloseEvidence struct {
	LegalEntityID string         `json:"legal_entity_id"`
	FiscalPeriod  string         `json:"fiscal_period"`
	Ready         bool           `json:"ready"`
	Issues        []string       `json:"blocking_issues"`
	Books         []BookEvidence `json:"books"`
	Seal          *PeriodSeal    `json:"seal,omitempty"`
}

// EvidenceService assembles close evidence.
type EvidenceService struct {
	Books   Repo
	Recon   ReconRepo
	Reval   RevalRepo
	Seals   SealRepo
	Snaps   SnapshotRepo
	Pending PendingRepo
}

// For returns the evidence for an entity and period. require lists the reconciliation types that must have been run for each
// book (the kernel cannot know which subledgers an entity uses); an open case or a failed posting blocks regardless.
func (s *EvidenceService) For(ctx context.Context, tenant, entity, period string, require []string) (*CloseEvidence, error) {
	_, pend, ok := periodBounds(period)
	if !ok {
		return nil, exc(FamData, "EVIDENCE_INVALID", "fiscal_period: a period, YYYY-MM").with("field", "fiscal_period")
	}
	books, err := s.Books.ListBooks(ctx, tenant, BookFilter{LegalEntityID: entity, Status: BookActive, Limit: 200})
	if err != nil {
		return nil, err
	}
	ev := &CloseEvidence{LegalEntityID: entity, FiscalPeriod: period, Issues: []string{}, Books: []BookEvidence{}}
	if seal, _, serr := s.Seals.GetSeal(ctx, tenant, entity, period); serr == nil {
		ev.Seal = seal
	}
	for _, b := range books {
		be := BookEvidence{BookID: b.BookID, BookCode: b.BookCode, Issues: []string{}}
		cs, err := s.Recon.CaseStatus(ctx, tenant, entity, b.BookID, period)
		if err != nil {
			return nil, err
		}
		be.Reconciliation = cs
		if cs.OpenBreaks+cs.OpenWarnings > 0 {
			be.Issues = append(be.Issues, fmt.Sprintf("reconciliation_cases_open: %d break(s) and %d warning(s) are unresolved", cs.OpenBreaks, cs.OpenWarnings))
		}
		if cs.FailedPostings > 0 {
			be.Issues = append(be.Issues, fmt.Sprintf("failed_postings: %d posting execution(s) failed and are unresolved", cs.FailedPostings))
		}
		for _, typ := range require {
			if _, ran := cs.LatestByType[strings.ToUpper(typ)]; !ran {
				be.Issues = append(be.Issues, "reconciliation_not_run: "+strings.ToUpper(typ))
			}
		}
		positions, _, err := s.Reval.ForeignPositions(ctx, tenant, entity, b.BookCode, b.FunctionalCurrency, pend)
		if err != nil {
			return nil, err
		}
		for _, p := range positions {
			if dec(p.TransactionBalance).Sign() != 0 {
				be.Revaluation.OpenForeignItems++
			}
		}
		be.Revaluation.Required = be.Revaluation.OpenForeignItems > 0
		if run, rerr := s.Reval.GetRevalRun(ctx, tenant, entity, b.BookID, period); rerr == nil {
			be.Revaluation.RunID, be.Revaluation.Done = run.RunID, true
		}
		if be.Revaluation.Required && !be.Revaluation.Done {
			be.Issues = append(be.Issues, fmt.Sprintf("revaluation_not_run: %d open foreign monetary item(s) have not been revalued for %s", be.Revaluation.OpenForeignItems, period))
		}
		if be.PendingJournal, err = s.Pending.PendingJournals(ctx, tenant, entity, period); err != nil {
			return nil, err
		}
		if be.PendingJournal > 0 {
			be.Issues = append(be.Issues, fmt.Sprintf("journals_in_review_queue: %d journal(s) of the period are not posted or withdrawn", be.PendingJournal))
		}
		if snaps, serr := s.Snaps.ListSnapshots(ctx, tenant, entity, b.BookID, period); serr == nil && len(snaps) > 0 {
			sn := snaps[0]
			be.Snapshot = &SnapshotRef{SnapshotID: sn.SnapshotID, Purpose: sn.Purpose, LedgerWatermark: sn.LedgerWatermark, ContentHash: sn.ContentHash, CreatedAt: sn.CreatedAt}
		}
		be.Ready = len(be.Issues) == 0
		for _, is := range be.Issues {
			ev.Issues = append(ev.Issues, b.BookCode+": "+is)
		}
		ev.Books = append(ev.Books, be)
	}
	ev.Ready = len(ev.Issues) == 0
	return ev, nil
}
