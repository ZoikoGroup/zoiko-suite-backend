package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ConsolPlan is what the repository writes in one transaction: the group-book journals and the run that records them.
type ConsolPlan struct {
	Tenant      string
	Group       ConsolGroup
	GroupBook   Book
	Run         ConsolRun
	AsOf        time.Time
	Currency    string
	Actor       string
	Correlation string
	Gate        bool
	MemberBooks []ConsolInput // each member's book and the watermark the run read it to
}

// ConsolRepo is the persistence boundary of consolidation.
type ConsolRepo interface {
	InsertGroup(ctx context.Context, tenant string, g *ConsolGroup, out OutEvent) error
	GetGroup(ctx context.Context, tenant, id string) (*ConsolGroup, error)
	ListGroups(ctx context.Context, tenant string) ([]ConsolGroup, error)
	// InsertMember refuses (ErrOverlap) a window that overlaps another membership of the same entity in the group.
	InsertMember(ctx context.Context, tenant string, m *ConsolMember, actor string, out OutEvent) error
	EndMember(ctx context.Context, tenant, groupID, memberID string, effectiveTo time.Time, out OutEvent) error
	// InsertMapping writes the next version and supersedes the one in force, atomically.
	InsertMapping(ctx context.Context, tenant string, m *ConsolMapping, out OutEvent) (*ConsolMapping, error)
	ActiveMapping(ctx context.Context, tenant, groupID string) (*ConsolMapping, error)
	ListMappings(ctx context.Context, tenant, groupID string) ([]ConsolMapping, error)

	InsertAdjustment(ctx context.Context, tenant string, a *ConsolAdjustment, out OutEvent) error
	GetAdjustment(ctx context.Context, tenant, id string) (*ConsolAdjustment, error)
	GetAdjustmentBySourceRef(ctx context.Context, tenant, ref string) (*ConsolAdjustment, error)
	ListAdjustments(ctx context.Context, tenant, groupID, period string) ([]ConsolAdjustment, error)
	// ApprovedAdjustments lists the approved adjustments of the group up to and including the period.
	ApprovedAdjustments(ctx context.Context, tenant, groupID, upToPeriod string) ([]ConsolAdjustment, error)
	ApproveAdjustment(ctx context.Context, tenant, id, actor string, at time.Time, out OutEvent) error
	WithdrawAdjustment(ctx context.Context, tenant, id, actor, reason string, at time.Time, out OutEvent) error

	AccountTypes(ctx context.Context, tenant string) (map[string]string, error)
	// ICPositions reads the intercompany positions of a book, to a ledger watermark when given.
	ICPositions(ctx context.Context, tenant, entity, bookCode, functional string, asOf time.Time, upToSeq int64, accounts []string) ([]ICPosition, int64, error)

	GetConsolRun(ctx context.Context, tenant, id string) (*ConsolRun, error)
	// LiveRuns lists the runs of a group that have not been superseded, latest period first.
	LiveRuns(ctx context.Context, tenant, groupID string) ([]ConsolRun, error)
	ListConsolRuns(ctx context.Context, tenant, groupID string) ([]ConsolRun, error)
	// WriteConsolidation posts the group-book journals and records the run in one transaction. It returns the existing run for the
	// scope, and refuses (AK-CONC) when a member posted into the population after it was frozen or a newer run appeared.
	WriteConsolidation(ctx context.Context, p *ConsolPlan) (*ConsolRun, error)
	CertifyRun(ctx context.Context, tenant, id, actor string, at time.Time, snapshotID string, out OutEvent) (*ConsolRun, error)
	// SupersedeRun reverses the run's group-book journals and marks it superseded.
	SupersedeRun(ctx context.Context, tenant, id, actor, reason string, at time.Time, asOf time.Time, group ConsolGroup, book Book) (*ConsolRun, error)
}

// ConsolService is the command side of intercompany pairing and consolidation.
type ConsolService struct {
	Repo  ConsolRepo
	Books Repo
	Snaps *SnapshotService
	Seals SealRepo
	Rates FXSource
	Dir   Directory
	Gate  PeriodGate
	Clock func() time.Time
}

func (s *ConsolService) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func consolBad(f, why string) error {
	return exc(FamData, "CONSOLIDATION_INVALID", "%s: %s", f, why).with("field", f)
}

// GroupRequest creates a consolidation group.
type GroupRequest struct {
	GroupCode            string `json:"group_code"`
	GroupName            string `json:"group_name"`
	ParentEntityID       string `json:"parent_entity_id"`
	GroupBookID          string `json:"group_book_id"`
	PresentationCurrency string `json:"presentation_currency"`
	ICTolerance          string `json:"ic_tolerance,omitempty"`
}

// CreateGroup creates a group. Its book must be a group-consolidation book of the group's reporting entity, in the presentation
// currency, so group results are never written to a statutory or management book.
func (s *ConsolService) CreateGroup(ctx context.Context, tenant string, a Actor, req GroupRequest) (*ConsolGroup, error) {
	if !codeRE.MatchString(req.GroupCode) {
		return nil, consolBad("group_code", "1-63 letters, digits, dot, dash or underscore")
	}
	if strings.TrimSpace(req.GroupName) == "" {
		return nil, consolBad("group_name", "required")
	}
	if _, err := uuid.Parse(req.ParentEntityID); err != nil {
		return nil, consolBad("parent_entity_id", "a legal entity id")
	}
	if _, err := uuid.Parse(req.GroupBookID); err != nil {
		return nil, consolBad("group_book_id", "an accounting book id")
	}
	if !currencyRE.MatchString(req.PresentationCurrency) {
		return nil, consolBad("presentation_currency", "an ISO 4217 code")
	}
	tol := "0"
	if req.ICTolerance != "" {
		tol = req.ICTolerance
	}
	t, ok := new(big.Rat).SetString(tol)
	if !ok || t.Sign() < 0 {
		return nil, consolBad("ic_tolerance", "zero or a positive decimal")
	}
	bk, err := s.Books.GetBook(ctx, tenant, req.GroupBookID)
	if err != nil {
		return nil, err
	}
	switch {
	case bk.BookType != BookGroup:
		return nil, exc(FamRule, "GROUP_BOOK_INVALID", "book %s is a %s book: group results are written only to a %s book", bk.BookCode, bk.BookType, BookGroup)
	case bk.LegalEntityID != req.ParentEntityID:
		return nil, exc(FamRule, "GROUP_BOOK_INVALID", "book %s belongs to another legal entity than the group's reporting entity", bk.BookCode)
	case bk.FunctionalCurrency != req.PresentationCurrency:
		return nil, exc(FamRule, "GROUP_BOOK_INVALID", "book %s is kept in %s, not the presentation currency %s", bk.BookCode, bk.FunctionalCurrency, req.PresentationCurrency)
	case bk.Status != BookActive:
		return nil, exc(FamRule, "GROUP_BOOK_INVALID", "book %s is %s", bk.BookCode, strings.ToLower(bk.Status))
	}
	g := &ConsolGroup{GroupID: uuid.NewString(), GroupCode: req.GroupCode, GroupName: strings.TrimSpace(req.GroupName), ParentEntityID: req.ParentEntityID, GroupBookID: bk.BookID,
		PresentationCurrency: req.PresentationCurrency, ICTolerance: t.FloatString(2), Status: "ACTIVE", CreatedBy: a.PrincipalID, CreatedAt: s.now()}
	if err := s.Repo.InsertGroup(ctx, tenant, g, OutEvent{Fact: "consolidation-group.created", AggregateType: "consolidation-group", AggregateID: g.GroupID, Actor: a.PrincipalID, CorrelationID: a.CorrelationID,
		LegalEntityID: g.ParentEntityID, Data: map[string]any{"group_id": g.GroupID, "group_code": g.GroupCode, "group_book_id": g.GroupBookID, "presentation_currency": g.PresentationCurrency}}); err != nil {
		return nil, err
	}
	return g, nil
}

// MemberRequest adds an entity's book to a group from a date.
type MemberRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	SourceBookID  string `json:"source_book_id"`
	OwnershipPct  string `json:"ownership_pct"`
	Method        string `json:"method,omitempty"`
	EffectiveFrom string `json:"effective_from"`
	// capital consolidation, optional and all together: the investor entity, the account holding the investment, the equity accounts of this member
	InvestmentAccount string   `json:"investment_account,omitempty"`
	InvestorEntityID  string   `json:"investor_entity_id,omitempty"`
	EquityAccounts    []string `json:"equity_accounts,omitempty"`
}

// AddMember adds a member. The group's reporting entity cannot be a member: its own periods must stay open for the group book.
func (s *ConsolService) AddMember(ctx context.Context, tenant string, a Actor, groupID string, req MemberRequest) (*ConsolMember, error) {
	g, err := s.Repo.GetGroup(ctx, tenant, groupID)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.LegalEntityID); err != nil {
		return nil, consolBad("legal_entity_id", "a legal entity id")
	}
	if req.LegalEntityID == g.ParentEntityID {
		return nil, exc(FamRule, "MEMBER_INVALID", "the group's reporting entity cannot be a member of its own consolidation")
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "FULL"
	}
	if method != "FULL" {
		return nil, consolBad("method", "FULL: other consolidation methods are not supported")
	}
	pct, ok := new(big.Rat).SetString(req.OwnershipPct)
	if !ok || pct.Sign() <= 0 || pct.Cmp(big.NewRat(100, 1)) > 0 {
		return nil, consolBad("ownership_pct", "a percentage above 0 and up to 100")
	}
	from, err := time.Parse("2006-01-02", req.EffectiveFrom)
	if err != nil {
		return nil, consolBad("effective_from", "a date, YYYY-MM-DD")
	}
	bk, err := s.Books.GetBook(ctx, tenant, req.SourceBookID)
	if err != nil {
		return nil, err
	}
	switch {
	case bk.LegalEntityID != req.LegalEntityID:
		return nil, exc(FamRule, "MEMBER_INVALID", "book %s does not belong to entity %s", bk.BookCode, req.LegalEntityID)
	case bk.BookType == BookGroup:
		return nil, exc(FamRule, "MEMBER_INVALID", "a group book cannot be consolidated into a group")
	case bk.Status != BookActive:
		return nil, exc(FamRule, "MEMBER_INVALID", "book %s is %s", bk.BookCode, strings.ToLower(bk.Status))
	}
	capital := req.InvestmentAccount != "" || req.InvestorEntityID != "" || len(req.EquityAccounts) > 0
	if capital {
		if _, perr := uuid.Parse(req.InvestorEntityID); perr != nil || req.InvestmentAccount == "" || len(req.EquityAccounts) == 0 || req.InvestorEntityID == req.LegalEntityID {
			return nil, consolBad("investor_entity_id", "the capital consolidation needs another entity as investor, the investment account and the equity accounts together")
		}
		for _, ea := range req.EquityAccounts {
			if strings.TrimSpace(ea) == "" {
				return nil, consolBad("equity_accounts", "non-empty account codes")
			}
		}
	}
	m := &ConsolMember{MemberID: uuid.NewString(), GroupID: g.GroupID, LegalEntityID: req.LegalEntityID, SourceBookID: bk.BookID, Method: method, OwnershipPct: pct.FloatString(4), EffectiveFrom: from.Format("2006-01-02"),
		InvestmentAccount: req.InvestmentAccount, InvestorEntityID: req.InvestorEntityID, EquityAccounts: req.EquityAccounts}
	if err := s.Repo.InsertMember(ctx, tenant, m, a.PrincipalID, OutEvent{Fact: "consolidation-member.added", AggregateType: "consolidation-group", AggregateID: g.GroupID, Actor: a.PrincipalID,
		CorrelationID: a.CorrelationID, LegalEntityID: g.ParentEntityID, Data: map[string]any{"group_id": g.GroupID, "member_id": m.MemberID, "legal_entity_id": m.LegalEntityID, "ownership_pct": m.OwnershipPct, "effective_from": m.EffectiveFrom}}); err != nil {
		return nil, err
	}
	return m, nil
}

// EndMember ends a membership on a date (a disposal or a change of ownership: the new ownership is a new membership from that date).
func (s *ConsolService) EndMember(ctx context.Context, tenant string, a Actor, groupID, memberID, effectiveTo string) error {
	to, err := time.Parse("2006-01-02", effectiveTo)
	if err != nil {
		return consolBad("effective_to", "a date, YYYY-MM-DD")
	}
	g, err := s.Repo.GetGroup(ctx, tenant, groupID)
	if err != nil {
		return err
	}
	return s.Repo.EndMember(ctx, tenant, groupID, memberID, to, OutEvent{Fact: "consolidation-member.ended", AggregateType: "consolidation-group", AggregateID: g.GroupID, Actor: a.PrincipalID,
		CorrelationID: a.CorrelationID, LegalEntityID: g.ParentEntityID, Data: map[string]any{"group_id": g.GroupID, "member_id": memberID, "effective_to": to.Format("2006-01-02")}})
}

// MappingRequest is a new version of the group's account mapping.
type MappingRequest struct {
	AccountMap map[string]string `json:"account_map"`
	ICRoles    map[string]string `json:"ic_roles,omitempty"`
}

// PutMapping creates the next mapping version and makes it the one in force. Past runs keep the version they used.
func (s *ConsolService) PutMapping(ctx context.Context, tenant string, a Actor, groupID string, req MappingRequest) (*ConsolMapping, error) {
	g, err := s.Repo.GetGroup(ctx, tenant, groupID)
	if err != nil {
		return nil, err
	}
	if len(req.AccountMap) == 0 {
		return nil, consolBad("account_map", "at least one local account")
	}
	book, err := s.Books.GetBook(ctx, tenant, g.GroupBookID)
	if err != nil {
		return nil, err
	}
	on := today(s.now())
	checked := map[string]bool{}
	for local, group := range req.AccountMap {
		if strings.TrimSpace(local) == "" || strings.TrimSpace(group) == "" {
			return nil, consolBad("account_map", "an account on both sides of every entry")
		}
		if checked[group] {
			continue
		}
		info, aerr := s.Dir.ResolveAccount(ctx, AccountRef{AccountCode: group}, book.BookID, on)
		if aerr != nil || info == nil || !info.Active || !info.Postable {
			return nil, exc(FamCOA, "GROUP_ACCOUNT_UNAVAILABLE", "group account %s is not an active, postable account of the chart", group).with("account_code", group)
		}
		checked[group] = true
	}
	for acct, role := range req.ICRoles {
		ok := false
		for _, r := range ICRoles {
			ok = ok || r == role
		}
		if !ok {
			return nil, consolBad("ic_roles", fmt.Sprintf("account %s has role %q: one of %s", acct, role, strings.Join(ICRoles, ", ")))
		}
		if _, mapped := req.AccountMap[acct]; !mapped {
			return nil, consolBad("ic_roles", fmt.Sprintf("account %s has an intercompany role but no group account in account_map", acct))
		}
	}
	if req.ICRoles == nil {
		req.ICRoles = map[string]string{}
	}
	m := &ConsolMapping{GroupID: g.GroupID, Status: "ACTIVE", AccountMap: req.AccountMap, ICRoles: req.ICRoles, CreatedBy: a.PrincipalID, CreatedAt: s.now()}
	return s.Repo.InsertMapping(ctx, tenant, m, OutEvent{Fact: "consolidation-mapping.versioned", AggregateType: "consolidation-group", AggregateID: g.GroupID, Actor: a.PrincipalID,
		CorrelationID: a.CorrelationID, LegalEntityID: g.ParentEntityID, Data: map[string]any{"group_id": g.GroupID, "accounts": len(req.AccountMap), "ic_roles": len(req.ICRoles)}})
}

// activeMembers returns the memberships in force at the date and the exceptions for memberships that changed inside the period: an
// entity that joined, left or changed ownership mid-period has no single ownership the whole period can be consolidated under.
func activeMembers(all []ConsolMember, pstart, asOf time.Time) (in []ConsolMember, excs []ConsolException) {
	byEntity := map[string][]ConsolMember{}
	for _, m := range all {
		byEntity[m.LegalEntityID] = append(byEntity[m.LegalEntityID], m)
	}
	ents := make([]string, 0, len(byEntity))
	for e := range byEntity {
		ents = append(ents, e)
	}
	sort.Strings(ents)
	for _, e := range ents {
		var overlapping []ConsolMember
		var covering *ConsolMember
		for i, m := range byEntity[e] {
			from, _ := time.Parse("2006-01-02", m.EffectiveFrom)
			var to time.Time
			if m.EffectiveTo != "" {
				to, _ = time.Parse("2006-01-02", m.EffectiveTo)
			}
			if from.After(asOf) || (!to.IsZero() && !to.After(pstart)) {
				continue
			}
			overlapping = append(overlapping, m)
			if !from.After(asOf) && (to.IsZero() || to.After(asOf)) {
				covering = &byEntity[e][i]
			}
			if from.After(pstart) {
				excs = append(excs, ConsolException{Code: "MEMBERSHIP_CHANGED_IN_PERIOD", Blocking: true, Entity: e,
					Detail: fmt.Sprintf("the entity joined the group on %s, inside the period: its ownership is not effective for the whole period", m.EffectiveFrom), Facts: map[string]any{"effective_from": m.EffectiveFrom}})
			}
			if !to.IsZero() && to.Before(asOf.AddDate(0, 0, 1)) {
				excs = append(excs, ConsolException{Code: "MEMBERSHIP_CHANGED_IN_PERIOD", Blocking: true, Entity: e,
					Detail: fmt.Sprintf("the membership ends on %s, inside the period", m.EffectiveTo), Facts: map[string]any{"effective_to": m.EffectiveTo}})
			}
		}
		if len(overlapping) > 1 {
			excs = append(excs, ConsolException{Code: "OWNERSHIP_CHANGED_IN_PERIOD", Blocking: true, Entity: e,
				Detail: fmt.Sprintf("%d ownership records apply to the entity within the period", len(overlapping)), Facts: map[string]any{"records": len(overlapping)}})
		}
		if covering != nil {
			in = append(in, *covering)
		}
	}
	return in, excs
}

// ConsolRunRequest asks for the consolidation of one group and period.
type ConsolRunRequest struct {
	GroupID            string `json:"group_id"`
	FiscalPeriod       string `json:"fiscal_period"`
	AsOf               string `json:"as_of,omitempty"`
	AllowUnsealed      bool   `json:"allow_unsealed,omitempty"`
	BalanceSheetPolicy string `json:"balance_sheet_policy,omitempty"`
	ProfitLossPolicy   string `json:"profit_loss_policy,omitempty"`
}

func (s *ConsolService) periodScope(period, asOfText string) (pstart, asOf time.Time, err error) {
	pstart, pend, ok := periodBounds(period)
	if !ok {
		return pstart, asOf, consolBad("fiscal_period", "a period, YYYY-MM")
	}
	asOf = pend
	if asOfText != "" {
		if asOf, err = time.Parse("2006-01-02", asOfText); err != nil {
			return pstart, asOf, consolBad("as_of", "a date, YYYY-MM-DD")
		}
		if asOf.Before(pstart) || asOf.After(pend) {
			return pstart, asOf, consolBad("as_of", "a date inside the fiscal period")
		}
	}
	return pstart, asOf, nil
}

func (s *ConsolService) loadGroup(ctx context.Context, tenant, id string) (*ConsolGroup, *Book, error) {
	g, err := s.Repo.GetGroup(ctx, tenant, id)
	if err != nil {
		return nil, nil, err
	}
	book, err := s.Books.GetBook(ctx, tenant, g.GroupBookID)
	if err != nil {
		return nil, nil, err
	}
	return g, book, nil
}

func roleAccounts(roles map[string]string) []string {
	out := make([]string, 0, len(roles))
	for a := range roles {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Run consolidates a group for a period. Each member's population is frozen as a signed snapshot, translated, paired and eliminated,
// and the change from the prior run is posted into the group book. A run that exists for the group and period is returned unchanged.
func (s *ConsolService) Run(ctx context.Context, tenant string, a Actor, req ConsolRunRequest) (*ConsolRun, error) {
	if s.Snaps == nil || s.Snaps.Signer == nil {
		return nil, exc(FamExt, "SIGNING_NOT_CONFIGURED", "no snapshot signing key is configured: a consolidation freezes its inputs as signed snapshots")
	}
	if _, err := uuid.Parse(req.GroupID); err != nil {
		return nil, consolBad("group_id", "a consolidation group id")
	}
	pstart, asOf, err := s.periodScope(req.FiscalPeriod, req.AsOf)
	if err != nil {
		return nil, err
	}
	if asOf.After(today(s.now())) {
		return nil, consolBad("as_of", "not in the future")
	}
	g, gbook, err := s.loadGroup(ctx, tenant, req.GroupID)
	if err != nil {
		return nil, err
	}
	if g.Status != "ACTIVE" {
		return nil, exc(FamRule, "GROUP_RETIRED", "group %s is retired", g.GroupCode)
	}
	if !gbook.InForce(asOf) {
		return nil, exc(FamRule, "BOOK_NOT_IN_FORCE", "group book %s is %s on %s", gbook.BookCode, strings.ToLower(gbook.Status), asOf.Format("2006-01-02"))
	}
	live, err := s.Repo.LiveRuns(ctx, tenant, g.GroupID)
	if err != nil {
		return nil, err
	}
	var prior *ConsolRun
	for i := range live {
		if live[i].FiscalPeriod == req.FiscalPeriod {
			r := live[i]
			r.Existing = true
			return &r, nil
		}
	}
	for i := range live {
		if live[i].AsOf > asOf.Format("2006-01-02") {
			return nil, exc(FamRule, "CONSOLIDATION_OUT_OF_ORDER", "period %s has already been consolidated: supersede it before consolidating the earlier period %s", live[i].FiscalPeriod, req.FiscalPeriod)
		}
		if prior == nil {
			p := live[i]
			prior = &p
		}
	}
	mapping, err := s.Repo.ActiveMapping(ctx, tenant, g.GroupID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, exc(FamCOA, "CONSOLIDATION_MAPPING_MISSING", "the group has no account mapping: the local accounts have nowhere to go")
		}
		return nil, err
	}
	adjustments, err := s.Repo.ApprovedAdjustments(ctx, tenant, g.GroupID, req.FiscalPeriod)
	if err != nil {
		return nil, err
	}
	members, memberExc := activeMembers(g.Members, pstart, asOf)
	if len(members) == 0 {
		return nil, exc(FamRule, "NO_MEMBERS", "no member of the group is in force on %s", asOf.Format("2006-01-02"))
	}

	bsPolicy, plPolicy := req.BalanceSheetPolicy, req.ProfitLossPolicy
	if bsPolicy == "" {
		bsPolicy = DefaultBalanceSheetPolicy
	}
	if plPolicy == "" {
		plPolicy = DefaultProfitLossPolicy
	}
	pols := map[string]FXPolicy{}
	for class, id := range map[string]string{"BS": bsPolicy, "PL": plPolicy} {
		p, ok := FXPolicyByID(id)
		if !ok {
			return nil, consolBad("balance_sheet_policy", fmt.Sprintf("%s is not an exchange-rate policy", id))
		}
		pols[class] = p
	}
	scale, err := s.Dir.MinorUnits(ctx, g.PresentationCurrency, asOf)
	if err != nil {
		return nil, exc(FamExt, "CURRENCY_UNAVAILABLE", "the precision of %s could not be read: %v", g.PresentationCurrency, err)
	}
	if scale > 2 {
		return nil, exc(FamData, "LEDGER_PRECISION", "the ledger stores two decimals and %s has %d", g.PresentationCurrency, scale)
	}

	// every member's book, and whether its period is sealed: a run over an unsealed population cannot be certified
	type loaded struct {
		m    ConsolMember
		book *Book
	}
	var ms []loaded
	var unsealed []string
	for _, m := range members {
		b, berr := s.Books.GetBook(ctx, tenant, m.SourceBookID)
		if berr != nil {
			return nil, berr
		}
		if !b.InForce(asOf) {
			return nil, exc(FamRule, "BOOK_NOT_IN_FORCE", "member book %s is %s on %s", b.BookCode, strings.ToLower(b.Status), asOf.Format("2006-01-02"))
		}
		seal, _, serr := s.Seals.GetSeal(ctx, tenant, m.LegalEntityID, req.FiscalPeriod)
		sealed := serr == nil && seal.Status == "SEALED"
		if serr != nil && !errors.Is(serr, ErrNotFound) {
			return nil, serr
		}
		if !sealed {
			unsealed = append(unsealed, m.LegalEntityID)
		}
		ms = append(ms, loaded{m, b})
	}
	if len(unsealed) > 0 && !req.AllowUnsealed {
		return nil, exc(FamPeriod, "INPUT_NOT_SEALED", "%d member(s) have not sealed %s: a consolidation consumes closed books", len(unsealed), req.FiscalPeriod).with("legal_entity_ids", unsealed)
	}

	// rates: one per currency and class, from the rate store under the policies; every one that is missing is reported together
	rates := map[string]ConsolRate{}
	var missing []map[string]any
	types, err := s.Repo.AccountTypes(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for _, l := range ms {
		cur := l.book.FunctionalCurrency
		if cur == g.PresentationCurrency {
			continue
		}
		for _, class := range []string{"BS", "PL"} {
			if _, done := rates[rateKey(cur, class)]; done {
				continue
			}
			b, rerr := s.Rates.Basis(ctx, pols[class].ID, cur, g.PresentationCurrency, asOf)
			if rerr != nil {
				if e, isExc := AsException(rerr); isExc {
					if e.Family == FamExt {
						return nil, e
					}
					missing = append(missing, map[string]any{"currency": cur, "class": class, "code": e.Code, "detail": e.Detail})
					continue
				}
				return nil, rerr
			}
			rates[rateKey(cur, class)] = ConsolRate{Currency: cur, Class: class, Policy: pols[class].ID, Rate: b.Rate, RateID: b.RateID, RateType: b.RateType, AsOf: b.AsOf}
		}
	}
	if len(missing) > 0 {
		return nil, exc(FamFX, "CONSOLIDATION_RATE_UNAVAILABLE", "%d rate(s) are not available under the group's translation policies: a consolidation never uses a stale or alternate rate", len(missing)).with("rates", missing)
	}
	var ctaAcct, diffAcct string
	for _, key := range []string{MapCTA, MapICDifference} {
		info, aerr := s.Dir.ResolveAccount(ctx, AccountRef{MappingKey: key}, gbook.BookID, asOf)
		if aerr != nil || info == nil || !info.Active || !info.Postable {
			return nil, exc(FamCOA, "CONSOLIDATION_ACCOUNT_UNAVAILABLE", "the mapping %s does not resolve to a postable account on %s", key, asOf.Format("2006-01-02")).with("mapping_key", key)
		}
		if key == MapCTA {
			ctaAcct = info.Code
		} else {
			diffAcct = info.Code
		}
	}

	// population freeze: a signed snapshot of every member's book, and its intercompany positions at the snapshot's watermark
	var data []ConsolMemberData
	roleAccts := roleAccounts(mapping.ICRoles)
	for _, l := range ms {
		snap, serr := s.Snaps.Create(ctx, tenant, a, SnapshotRequest{BookID: l.book.BookID, FiscalPeriod: req.FiscalPeriod, AsOf: asOf.Format("2006-01-02"), Purpose: SnapshotConsolidation})
		if serr != nil {
			return nil, serr
		}
		var pos []ICPosition
		if len(roleAccts) > 0 {
			pos, _, serr = s.Repo.ICPositions(ctx, tenant, l.m.LegalEntityID, l.book.BookCode, l.book.FunctionalCurrency, asOf, snap.LedgerWatermark, roleAccts)
			if serr != nil {
				return nil, serr
			}
		}
		sealed := true
		for _, u := range unsealed {
			sealed = sealed && u != l.m.LegalEntityID
		}
		data = append(data, ConsolMemberData{
			Input: ConsolInput{LegalEntityID: l.m.LegalEntityID, BookID: l.book.BookID, BookCode: l.book.BookCode, FunctionalCurrency: l.book.FunctionalCurrency, OwnershipPct: l.m.OwnershipPct,
				SnapshotID: snap.SnapshotID, ContentHash: snap.ContentHash, LedgerWatermark: snap.LedgerWatermark, Sealed: sealed, InvestmentAccount: l.m.InvestmentAccount, InvestorEntityID: l.m.InvestorEntityID, EquityAccounts: l.m.EquityAccounts},
			Lines: snap.Lines, Positions: pos})
	}

	tol, _ := new(big.Rat).SetString(g.ICTolerance)
	if tol == nil {
		tol = new(big.Rat)
	}
	cc := &ConsolCompute{Group: *g, Mapping: *mapping, AccountTypes: types, Scale: scale, Members: data, Rates: rates, CTAAccount: ctaAcct, DiffAccount: diffAcct, Tolerance: tol, Adjustments: adjustments}
	for _, d := range data {
		if d.Input.InvestmentAccount == "" {
			continue
		}
		for _, key := range []string{MapGoodwill, MapNCIEquity} {
			info, aerr := s.Dir.ResolveAccount(ctx, AccountRef{MappingKey: key}, gbook.BookID, asOf)
			if aerr != nil || info == nil || !info.Active || !info.Postable {
				return nil, exc(FamCOA, "CONSOLIDATION_ACCOUNT_UNAVAILABLE", "the mapping %s does not resolve to a postable account on %s: the capital consolidation needs it", key, asOf.Format("2006-01-02")).with("mapping_key", key)
			}
			if key == MapGoodwill {
				cc.GoodwillAccount = info.Code
			} else {
				cc.NCIAccount = info.Code
			}
		}
		break
	}
	pop, err := cc.Compute()
	if err != nil {
		return nil, err
	}
	pop.BalanceSheetPolicy, pop.ProfitLossPolicy = pols["BS"].ID, pols["PL"].ID
	pop.Exceptions = append(pop.Exceptions, memberExc...)
	for _, d := range data {
		if !d.Input.Sealed {
			pop.Exceptions = append(pop.Exceptions, ConsolException{Code: "INPUT_NOT_SEALED", Blocking: true, Entity: d.Input.LegalEntityID,
				Detail: fmt.Sprintf("the member's period %s was not sealed when its population was frozen", req.FiscalPeriod)})
		}
	}
	var priorTrans, priorElim, priorAdj map[string]string
	if prior != nil {
		pop.PriorRunID = prior.RunID
		priorTrans, priorElim, priorAdj = prior.Population.Translation, prior.Population.Elimination, prior.Population.Adjustment
	}
	pop.TranslationPosted = PostedDelta(pop.Translation, priorTrans, scale)
	pop.EliminationPosted = PostedDelta(pop.Elimination, priorElim, scale)
	pop.AdjustmentPosted = PostedDelta(pop.Adjustment, priorAdj, scale)

	if s.Gate != nil && (len(pop.TranslationPosted) > 0 || len(pop.EliminationPosted) > 0 || len(pop.AdjustmentPosted) > 0) {
		if err := s.Gate.CheckPostable(ctx, tenant, g.ParentEntityID, req.FiscalPeriod, asOf, false, "", a.PrincipalID); err != nil {
			return nil, err
		}
	}
	run := ConsolRun{RunID: uuid.NewString(), GroupID: g.GroupID, FiscalPeriod: req.FiscalPeriod, AsOf: asOf.Format("2006-01-02"), Status: ConsolProcessed, MappingVersion: mapping.Version,
		PriorRunID: pop.PriorRunID, Population: *pop, BlockingCount: pop.BlockingCount(), RunHash: pop.Hash(g.GroupID, req.FiscalPeriod, asOf.Format("2006-01-02")), CreatedBy: a.PrincipalID, CreatedAt: s.now()}
	mb := make([]ConsolInput, 0, len(data))
	for _, d := range data {
		mb = append(mb, d.Input)
	}
	return s.Repo.WriteConsolidation(ctx, &ConsolPlan{Tenant: tenant, Group: *g, GroupBook: *gbook, Run: run, AsOf: asOf, Currency: g.PresentationCurrency, Actor: a.PrincipalID,
		Correlation: a.CorrelationID, MemberBooks: mb})
}

// Pairing reports how the intercompany positions of the members pair up at a date, from the live ledgers. It freezes nothing.
func (s *ConsolService) Pairing(ctx context.Context, tenant, groupID, period, asOfText string) (map[string]any, error) {
	g, _, err := s.loadGroup(ctx, tenant, groupID)
	if err != nil {
		return nil, err
	}
	pstart, asOf, err := s.periodScope(period, asOfText)
	if err != nil {
		return nil, err
	}
	mapping, err := s.Repo.ActiveMapping(ctx, tenant, g.GroupID)
	if err != nil {
		return nil, err
	}
	members, memberExc := activeMembers(g.Members, pstart, asOf)
	var data []ConsolMemberData
	for _, m := range members {
		b, berr := s.Books.GetBook(ctx, tenant, m.SourceBookID)
		if berr != nil {
			return nil, berr
		}
		pos, _, perr := s.Repo.ICPositions(ctx, tenant, m.LegalEntityID, b.BookCode, b.FunctionalCurrency, asOf, 0, roleAccounts(mapping.ICRoles))
		if perr != nil {
			return nil, perr
		}
		data = append(data, ConsolMemberData{Input: ConsolInput{LegalEntityID: m.LegalEntityID, BookID: b.BookID, BookCode: b.BookCode, FunctionalCurrency: b.FunctionalCurrency}, Positions: pos})
	}
	pairs, excs := PairPositions(data, mapping.ICRoles)
	if pairs == nil {
		pairs = []ICPair{}
	}
	summary := map[string]int{}
	for _, p := range pairs {
		summary[p.Status]++
	}
	return map[string]any{"group_id": g.GroupID, "fiscal_period": period, "as_of": asOf.Format("2006-01-02"), "mapping_version": mapping.Version, "pairs": pairs, "summary": summary, "exceptions": append(excs, memberExc...)}, nil
}

// Certify closes a consolidation: the run must have nothing blocking, its inputs must still reproduce, the group book must carry
// exactly the figures the run produced, and someone other than the preparer signs the group book's balances.
func (s *ConsolService) Certify(ctx context.Context, tenant string, a Actor, runID string) (*ConsolRun, error) {
	run, err := s.Repo.GetConsolRun(ctx, tenant, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != ConsolProcessed {
		return nil, ErrState
	}
	if run.CreatedBy == a.PrincipalID {
		return nil, ErrSelfApproval
	}
	if run.BlockingCount > 0 {
		var codes []string
		for _, e := range run.Population.Exceptions {
			if e.Blocking {
				codes = append(codes, e.Code)
			}
		}
		return nil, exc(FamRule, "CONSOLIDATION_BLOCKED", "%d exception(s) stop the certification: resolve them and consolidate again", run.BlockingCount).with("codes", codes)
	}
	live, err := s.Repo.LiveRuns(ctx, tenant, run.GroupID)
	if err != nil {
		return nil, err
	}
	if len(live) == 0 || live[0].RunID != run.RunID {
		return nil, exc(FamRule, "CONSOLIDATION_NOT_LATEST", "a later period has been consolidated: only the latest run of a group can be certified")
	}
	g, gbook, err := s.loadGroup(ctx, tenant, run.GroupID)
	if err != nil {
		return nil, err
	}
	if s.Snaps == nil || s.Snaps.Signer == nil {
		return nil, exc(FamExt, "SIGNING_NOT_CONFIGURED", "no snapshot signing key is configured")
	}
	// the inputs still reproduce from the ledger at the watermarks they were frozen at
	var broken []string
	for _, in := range run.Population.Members {
		v, verr := s.Snaps.Verify(ctx, tenant, in.SnapshotID)
		if verr != nil {
			return nil, verr
		}
		if v["verified"] != true {
			broken = append(broken, in.LegalEntityID)
		}
	}
	if len(broken) > 0 {
		return nil, exc(FamConc, "INPUT_SNAPSHOT_BROKEN", "the frozen input of %d member(s) no longer verifies", len(broken)).with("legal_entity_ids", broken)
	}
	// group reconciliation: the group book carries what the run says it does
	asOf, _ := time.Parse("2006-01-02", run.AsOf)
	pstart, _, _ := periodBounds(run.FiscalPeriod)
	wm, lines, err := s.Snaps.Repo.Balances(ctx, tenant, g.ParentEntityID, gbook.BookCode, asOf, pstart, 0)
	if err != nil {
		return nil, err
	}
	if diffs := groupBookDifferences(lines, run.Population.Consolidated); len(diffs) > 0 {
		return nil, exc(FamBal, "GROUP_RECONCILIATION_FAILED", "the group book does not carry the consolidated figures of the run in %d account(s)", len(diffs)).with("differences", diffs)
	}
	snap, err := s.Snaps.Create(ctx, tenant, a, SnapshotRequest{BookID: gbook.BookID, FiscalPeriod: run.FiscalPeriod, AsOf: run.AsOf, Purpose: SnapshotConsolidationClose})
	if err != nil {
		return nil, err
	}
	if snap.LedgerWatermark != wm {
		return nil, exc(FamConc, "POPULATION_CHANGED", "the group book changed during the certification; certify again")
	}
	return s.Repo.CertifyRun(ctx, tenant, run.RunID, a.PrincipalID, s.now(), snap.SnapshotID, OutEvent{Fact: "consolidation-run.certified", AggregateType: "consolidation-run", AggregateID: run.RunID,
		Actor: a.PrincipalID, CorrelationID: a.CorrelationID, LegalEntityID: g.ParentEntityID, Data: map[string]any{"consolidation_run_id": run.RunID, "group_id": g.GroupID, "fiscal_period": run.FiscalPeriod,
			"group_snapshot_id": snap.SnapshotID, "run_hash": run.RunHash, "mapping_version": run.MappingVersion}})
}

func groupBookDifferences(lines []SnapshotLine, want map[string]string) []map[string]string {
	have := map[string]*big.Rat{}
	for _, l := range lines {
		if n := dec(l.Net); n.Sign() != 0 {
			have[l.AccountCode] = n
		}
	}
	keys := map[string]bool{}
	for k := range have {
		keys[k] = true
	}
	for k := range want {
		keys[k] = true
	}
	var out []map[string]string
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		h, w := new(big.Rat), new(big.Rat)
		if v, ok := have[k]; ok {
			h = v
		}
		if v, ok := want[k]; ok {
			w = dec(v)
		}
		if h.Cmp(w) != 0 {
			out = append(out, map[string]string{"account_code": k, "group_book": h.FloatString(2), "run": w.FloatString(2)})
		}
	}
	return out
}

// Supersede withdraws the latest run of a group: its group-book journals are reversed in the period, and the period can be
// consolidated again. A certified run can be superseded; it keeps its record.
func (s *ConsolService) Supersede(ctx context.Context, tenant string, a Actor, runID, reason string) (*ConsolRun, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, consolBad("reason", "required")
	}
	run, err := s.Repo.GetConsolRun(ctx, tenant, runID)
	if err != nil {
		return nil, err
	}
	if run.Status == ConsolSuperseded {
		return nil, ErrState
	}
	live, err := s.Repo.LiveRuns(ctx, tenant, run.GroupID)
	if err != nil {
		return nil, err
	}
	if len(live) == 0 || live[0].RunID != run.RunID {
		return nil, exc(FamRule, "SUPERSEDE_NOT_LATEST", "a later period has been consolidated on top of this run: supersede that one first")
	}
	g, gbook, err := s.loadGroup(ctx, tenant, run.GroupID)
	if err != nil {
		return nil, err
	}
	asOf, _ := time.Parse("2006-01-02", run.AsOf)
	if s.Gate != nil && (run.TranslationJournalID != "" || run.EliminationJournalID != "" || run.AdjustmentJournalID != "") {
		if err := s.Gate.CheckPostable(ctx, tenant, g.ParentEntityID, run.FiscalPeriod, asOf, false, "", a.PrincipalID); err != nil {
			return nil, err
		}
	}
	return s.Repo.SupersedeRun(ctx, tenant, run.RunID, a.PrincipalID, strings.TrimSpace(reason), s.now(), asOf, *g, *gbook)
}

// AdjustmentRequest proposes a group policy adjustment.
type AdjustmentRequest struct {
	GroupID      string `json:"group_id"`
	FiscalPeriod string `json:"fiscal_period"`
	Description  string `json:"description"`
	PolicyRef    string `json:"policy_ref"`
	EvidenceRef  string `json:"evidence_ref"`
	// SourceRef names the record in the submitting service this adjustment comes from: the same reference again is the same adjustment
	SourceRef string           `json:"source_ref,omitempty"`
	Lines     []AdjustmentLine `json:"lines"`
}

// CreateAdjustment proposes an adjustment. It must name the policy it applies and its evidence, move at least two accounts of the group
// chart and balance exactly; it changes nothing until someone else approves it and a run applies it.
func (s *ConsolService) CreateAdjustment(ctx context.Context, tenant string, a Actor, req AdjustmentRequest) (*ConsolAdjustment, error) {
	g, gbook, err := s.loadGroup(ctx, tenant, req.GroupID)
	if err != nil {
		return nil, err
	}
	if _, _, ok := periodBounds(req.FiscalPeriod); !ok {
		return nil, consolBad("fiscal_period", "a period, YYYY-MM")
	}
	for f, v := range map[string]string{"description": req.Description, "policy_ref": req.PolicyRef, "evidence_ref": req.EvidenceRef} {
		if strings.TrimSpace(v) == "" {
			return nil, consolBad(f, "required")
		}
	}
	if len(req.Lines) < 2 {
		return nil, consolBad("lines", "at least two lines")
	}
	sum := new(big.Rat)
	norm := make([]AdjustmentLine, 0, len(req.Lines))
	_, pend, _ := periodBounds(req.FiscalPeriod)
	for _, l := range req.Lines {
		amt, ok := new(big.Rat).SetString(l.Amount)
		if !ok || amt.Sign() == 0 {
			return nil, consolBad("lines", fmt.Sprintf("account %s needs a non-zero decimal amount", l.AccountCode))
		}
		info, aerr := s.Dir.ResolveAccount(ctx, AccountRef{AccountCode: l.AccountCode}, gbook.BookID, pend)
		if aerr != nil || info == nil || !info.Active || !info.Postable {
			return nil, exc(FamCOA, "GROUP_ACCOUNT_UNAVAILABLE", "group account %s is not an active, postable account of the chart", l.AccountCode).with("account_code", l.AccountCode)
		}
		sum.Add(sum, amt)
		norm = append(norm, AdjustmentLine{AccountCode: l.AccountCode, Amount: amt.FloatString(2), Description: l.Description})
	}
	if sum.Sign() != 0 {
		return nil, exc(FamBal, "ADJUSTMENT_UNBALANCED", "the lines of the adjustment add up to %s, not zero", sum.FloatString(2))
	}
	adj := &ConsolAdjustment{AdjustmentID: uuid.NewString(), GroupID: g.GroupID, FiscalPeriod: req.FiscalPeriod, Description: strings.TrimSpace(req.Description), PolicyRef: strings.TrimSpace(req.PolicyRef), SourceRef: strings.TrimSpace(req.SourceRef),
		EvidenceRef: strings.TrimSpace(req.EvidenceRef), Lines: norm, Status: "PROPOSED", CreatedBy: a.PrincipalID, CreatedAt: s.now()}
	if adj.SourceRef != "" {
		if prior, perr := s.Repo.GetAdjustmentBySourceRef(ctx, tenant, adj.SourceRef); perr == nil {
			return sameAdjustment(prior, adj)
		} else if !errors.Is(perr, ErrNotFound) {
			return nil, perr
		}
	}
	if err := s.Repo.InsertAdjustment(ctx, tenant, adj, OutEvent{Fact: "consolidation-adjustment.proposed", AggregateType: "consolidation-adjustment", AggregateID: adj.AdjustmentID, Actor: a.PrincipalID,
		CorrelationID: a.CorrelationID, LegalEntityID: g.ParentEntityID, Data: map[string]any{"adjustment_id": adj.AdjustmentID, "group_id": g.GroupID, "fiscal_period": adj.FiscalPeriod, "policy_ref": adj.PolicyRef, "source_ref": adj.SourceRef}}); err != nil {
		if errors.Is(err, ErrExists) && adj.SourceRef != "" {
			if prior, perr := s.Repo.GetAdjustmentBySourceRef(ctx, tenant, adj.SourceRef); perr == nil {
				return sameAdjustment(prior, adj)
			}
		}
		return nil, err
	}
	return adj, nil
}

// ApproveAdjustment approves a proposed adjustment; someone other than its author must.
func (s *ConsolService) ApproveAdjustment(ctx context.Context, tenant string, a Actor, id string) (*ConsolAdjustment, error) {
	adj, err := s.Repo.GetAdjustment(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if adj.Status != "PROPOSED" {
		return nil, ErrState
	}
	if adj.CreatedBy == a.PrincipalID {
		return nil, ErrSelfApproval
	}
	g, err := s.Repo.GetGroup(ctx, tenant, adj.GroupID)
	if err != nil {
		return nil, err
	}
	if err := s.Repo.ApproveAdjustment(ctx, tenant, id, a.PrincipalID, s.now(), OutEvent{Fact: "consolidation-adjustment.approved", AggregateType: "consolidation-adjustment", AggregateID: id, Actor: a.PrincipalID,
		CorrelationID: a.CorrelationID, LegalEntityID: g.ParentEntityID, Data: map[string]any{"adjustment_id": id, "group_id": adj.GroupID, "fiscal_period": adj.FiscalPeriod}}); err != nil {
		return nil, err
	}
	return s.Repo.GetAdjustment(ctx, tenant, id)
}

// WithdrawAdjustment withdraws a proposed or approved adjustment. One a live run has applied cannot be withdrawn: that run is superseded first.
func (s *ConsolService) WithdrawAdjustment(ctx context.Context, tenant string, a Actor, id, reason string) (*ConsolAdjustment, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, consolBad("reason", "required")
	}
	adj, err := s.Repo.GetAdjustment(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	if adj.Status == "WITHDRAWN" {
		return nil, ErrState
	}
	live, err := s.Repo.LiveRuns(ctx, tenant, adj.GroupID)
	if err != nil {
		return nil, err
	}
	for _, r := range live {
		for _, ap := range r.Population.Adjustments {
			if ap.AdjustmentID == id {
				return nil, exc(FamRule, "ADJUSTMENT_APPLIED", "the adjustment is in the group figures of the run for %s: supersede that run first", r.FiscalPeriod)
			}
		}
	}
	g, err := s.Repo.GetGroup(ctx, tenant, adj.GroupID)
	if err != nil {
		return nil, err
	}
	if err := s.Repo.WithdrawAdjustment(ctx, tenant, id, a.PrincipalID, strings.TrimSpace(reason), s.now(), OutEvent{Fact: "consolidation-adjustment.withdrawn", AggregateType: "consolidation-adjustment", AggregateID: id,
		Actor: a.PrincipalID, CorrelationID: a.CorrelationID, LegalEntityID: g.ParentEntityID, Data: map[string]any{"adjustment_id": id, "group_id": adj.GroupID, "reason": strings.TrimSpace(reason)}}); err != nil {
		return nil, err
	}
	return s.Repo.GetAdjustment(ctx, tenant, id)
}

// sameAdjustment answers a repeated submission: the adjustment that already exists for the source reference when it says the same thing,
// and a conflict when the same reference now comes with other content.
func sameAdjustment(prior, now *ConsolAdjustment) (*ConsolAdjustment, error) {
	a, _ := json.Marshal([]any{prior.GroupID, prior.FiscalPeriod, prior.Lines})
	b, _ := json.Marshal([]any{now.GroupID, now.FiscalPeriod, now.Lines})
	if string(a) != string(b) {
		return nil, ErrIdempotency
	}
	prior.Existing = true
	return prior, nil
}
