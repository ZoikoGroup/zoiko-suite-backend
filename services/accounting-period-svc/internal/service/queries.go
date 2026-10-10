package service

import (
	"context"
	"sort"

	"zoiko.io/accounting-period-svc/internal/domain"
)

// Purposes understood by the posting gate.
const (
	PurposePost = "post"
	PurposeRead = "read"
)

// ResolveInput is ResolvePeriodByDate, the posting gate.
type ResolveInput struct {
	TenantID      string
	LegalEntityID string
	Date          string // YYYY-MM-DD
	BookScope     string
	ModuleScope   string
	Purpose       string // post (default) | read
	// Kind optionally narrows to NORMAL or SPECIAL periods when both cover the date.
	Kind string
	// SoftCloseException is the caller's assertion that it holds the soft-close
	// exception; it only matters for SOFT_CLOSED periods.
	SoftCloseException bool
}

// Resolution is the gate's answer.
type Resolution struct {
	PeriodID       string       `json:"period_id"`
	PeriodKey      string       `json:"period_key"`
	State          domain.State `json:"state"`
	PostingAllowed bool         `json:"posting_allowed"`
	PostingMode    string       `json:"posting_mode"`
	Reason         string       `json:"reason"`
	Version        int64        `json:"version"`
	StateVersion   int64        `json:"state_version"`
}

func scopeMatches(periodScope, requested string) bool {
	return periodScope == "" || periodScope == requested
}

func specificity(p *domain.Period) int {
	n := 0
	if p.BookScope != "" {
		n++
	}
	if p.ModuleScope != "" {
		n++
	}
	return n
}

// ResolvePeriodByDate is the posting gate. It reads the PRIMARY (the store
// has no replica path) inside one transaction, and never defaults:
//
//   - no period covers the entity/date/scope  -> PERIOD_NOT_FOUND (not OPEN)
//   - several cover it with different outcomes -> RULE_AMBIGUOUS
//   - several cover it with the SAME outcome   -> the most specific scope wins
//     (then NORMAL before SPECIAL, then the lowest period_id), deterministically
//   - REOPEN_AUTHORIZED is judged against the service clock at read time, so an
//     expired window blocks posting without any sweeper having run.
func (s *Service) ResolvePeriodByDate(ctx context.Context, in ResolveInput) (*Resolution, error) {
	if in.TenantID == "" || in.LegalEntityID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "tenant context and legal_entity_id are required")
	}
	if _, ok := validDate(in.Date); !ok {
		return nil, domain.Errf(domain.CodeContextInvalid, "date is required (YYYY-MM-DD)")
	}
	if in.Purpose == "" {
		in.Purpose = PurposePost
	}
	if in.Purpose != PurposePost && in.Purpose != PurposeRead {
		return nil, domain.Errf(domain.CodeContextInvalid, "purpose must be %q or %q", PurposePost, PurposeRead)
	}
	if in.Kind != "" && !domain.Kind(in.Kind).Valid() {
		return nil, domain.Errf(domain.CodeContextInvalid, "kind must be NORMAL or SPECIAL")
	}

	var cands []domain.Period
	err := s.store.InTx(ctx, in.TenantID, func(tx Tx) error {
		all, err := tx.FindPeriodsCovering(ctx, in.LegalEntityID, in.Date)
		if err != nil {
			return err
		}
		for _, p := range all {
			if !scopeMatches(p.BookScope, in.BookScope) || !scopeMatches(p.ModuleScope, in.ModuleScope) {
				continue
			}
			if in.Kind != "" && string(p.Kind) != in.Kind {
				continue
			}
			cands = append(cands, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, domain.Errf(domain.CodePeriodNotFound,
			"no accounting period covers %s for legal entity %s (book_scope %q, module_scope %q); posting is not allowed",
			in.Date, in.LegalEntityID, in.BookScope, in.ModuleScope)
	}

	now := s.now()
	type scored struct {
		p *domain.Period
		d domain.Decision
	}
	rs := make([]scored, len(cands))
	for i := range cands {
		rs[i] = scored{&cands[i], cands[i].PostingDecision(now, in.BookScope, in.ModuleScope, in.SoftCloseException)}
	}
	for _, r := range rs[1:] {
		if r.p.State != rs[0].p.State || r.d != rs[0].d {
			return nil, domain.Errf(domain.CodeRuleAmbiguous,
				"%d periods cover %s for this scope with different states/outcomes (e.g. %s %s vs %s %s); no precedence rule resolves this",
				len(rs), in.Date, rs[0].p.PeriodKey, rs[0].p.State, r.p.PeriodKey, r.p.State)
		}
	}
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i].p, rs[j].p
		if specificity(a) != specificity(b) {
			return specificity(a) > specificity(b)
		}
		if a.Kind != b.Kind {
			return a.Kind == domain.KindNormal
		}
		return a.PeriodID < b.PeriodID
	})
	w := rs[0]
	return &Resolution{
		PeriodID: w.p.PeriodID, PeriodKey: w.p.PeriodKey, State: w.p.State,
		PostingAllowed: w.d.Allowed, PostingMode: w.d.Mode, Reason: w.d.Reason,
		Version: w.p.Version, StateVersion: w.p.Version,
	}, nil
}

// GetPeriod returns one period.
func (s *Service) GetPeriod(ctx context.Context, tenantID, id string) (*domain.Period, error) {
	var p *domain.Period
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		var err error
		p, err = tx.GetPeriod(ctx, id, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, domain.Errf(domain.CodeNotFound, "accounting period %s not found", id)
	}
	return p, nil
}

// ListPeriods is ListOpenPeriods (state=OPEN) and the general listing.
func (s *Service) ListPeriods(ctx context.Context, tenantID, legalEntityID, state string, limit, offset int) ([]domain.Period, error) {
	if tenantID == "" || legalEntityID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "tenant context and legal_entity_id are required")
	}
	if state != "" && !domain.State(state).Valid() {
		return nil, domain.Errf(domain.CodeContextInvalid, "unknown state %q", state)
	}
	var out []domain.Period
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = tx.ListPeriods(ctx, legalEntityID, state, limit, offset)
		return err
	})
	return out, err
}

// GetPeriodStateHistory returns the append-only history, oldest first.
func (s *Service) GetPeriodStateHistory(ctx context.Context, tenantID, id string) ([]domain.HistoryEntry, error) {
	var h []domain.HistoryEntry
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		p, err := tx.GetPeriod(ctx, id, false)
		if err != nil {
			return err
		}
		if p == nil {
			return domain.Errf(domain.CodeNotFound, "accounting period %s not found", id)
		}
		h, err = tx.ListHistory(ctx, id)
		return err
	})
	return h, err
}

// StatusByKey is the compatibility read for general-ledger-svc's existing
// close client: OPEN/REOPEN_AUTHORIZED(in window) -> OPEN, SOFT_CLOSED/RECLOSED/
// expired reopen -> CLOSED, HARD_CLOSED -> LOCKED. An unknown period_key is
// PERIOD_NOT_FOUND, never OPEN. The legacy shape carries no book scope, so when
// several periods share the key the MOST RESTRICTIVE status is returned.
func (s *Service) StatusByKey(ctx context.Context, tenantID, legalEntityID, periodKey string) (string, error) {
	if tenantID == "" || legalEntityID == "" || periodKey == "" {
		return "", domain.Errf(domain.CodeContextInvalid, "tenant context, legal_entity_id and period_key are required")
	}
	var ps []domain.Period
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		var err error
		ps, err = tx.ListPeriodsByKey(ctx, legalEntityID, periodKey)
		return err
	})
	if err != nil {
		return "", err
	}
	if len(ps) == 0 {
		return "", domain.Errf(domain.CodePeriodNotFound, "no accounting period %q for legal entity %s", periodKey, legalEntityID)
	}
	now := s.now()
	worst := "OPEN"
	for i := range ps {
		if st := ps[i].CloseStatus(now); domain.CloseStatusRank(st) > domain.CloseStatusRank(worst) {
			worst = st
		}
	}
	return worst, nil
}

// CalendarUsageResult is what fiscal-calendar-svc asks before approving a calendar change.
type CalendarUsageResult struct {
	LatestPeriodEnd         *string `json:"latest_period_end"`
	HasPostedOrClosedPeriod bool    `json:"has_posted_or_closed_periods"`
}

// CalendarUsage reports how far a calendar (version) has been materialised and
// whether any period is no longer OPEN. This service cannot see postings, so
// "posted or closed" is approximated by "not OPEN" (see SPEC_DEVIATIONS.md).
func (s *Service) CalendarUsage(ctx context.Context, tenantID, calendarID, calendarVersionID string) (*CalendarUsageResult, error) {
	if tenantID == "" || calendarID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "tenant context and calendar_id are required")
	}
	res := &CalendarUsageResult{}
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		latest, anyNotOpen, err := tx.CalendarUsage(ctx, calendarID, calendarVersionID)
		res.LatestPeriodEnd, res.HasPostedOrClosedPeriod = latest, anyNotOpen
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
