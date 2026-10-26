package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// Intercompany and consolidation (standard §25, §26). Consolidation consumes the controlled books of the member entities, translates
// them to the group's presentation currency under approved rate categories, eliminates the intercompany positions that pair up and
// writes the result into the group book. It never writes a member's ledger.
//
// This file holds the pure computation, so the same inputs always give the same group figures; consol_service.go holds the commands.

// Intercompany account roles. A mapping version assigns a role to the local accounts that carry intercompany balances.
const (
	RoleDueFrom   = "DUE_FROM"
	RoleDueTo     = "DUE_TO"
	RoleICRevenue = "IC_REVENUE"
	RoleICExpense = "IC_EXPENSE"
)

// ICRoles lists the valid roles.
var ICRoles = []string{RoleDueFrom, RoleDueTo, RoleICRevenue, RoleICExpense}

// Mapping keys of the tenant's chart that a consolidation resolves in the group book.
const (
	MapCTA          = "CTA"
	MapICDifference = "IC_ELIM_DIFFERENCE"
	MapGoodwill     = "GOODWILL"
	MapNCIEquity    = "NCI_EQUITY"
)

// Default rate policies: balance-sheet accounts are translated at the closing rate, profit-and-loss accounts at the average rate.
const (
	DefaultBalanceSheetPolicy = "CLOSING_ON_DATE"
	DefaultProfitLossPolicy   = "AVERAGE_FOR_PERIOD"
)

// Intercompany pair classes and statuses (§25: matched, timing, amount, currency difference, unmatched).
const (
	ICClassBalance    = "BALANCE"
	ICClassProfitLoss = "PROFIT_LOSS"

	ICMatched            = "MATCHED"
	ICAmountDifference   = "AMOUNT_DIFFERENCE"
	ICCurrencyDifference = "CURRENCY_DIFFERENCE"
	ICUnmatched          = "UNMATCHED"
	ICBlocked            = "BLOCKED"
)

// Consolidation run statuses.
const (
	ConsolProcessed  = "PROCESSED"
	ConsolCertified  = "CERTIFIED"
	ConsolSuperseded = "SUPERSEDED"
)

// ConsolGroup is a consolidation group: the group book it writes to and the members whose books it consumes.
type ConsolGroup struct {
	GroupID              string         `json:"group_id"`
	GroupCode            string         `json:"group_code"`
	GroupName            string         `json:"group_name"`
	ParentEntityID       string         `json:"parent_entity_id"`
	GroupBookID          string         `json:"group_book_id"`
	PresentationCurrency string         `json:"presentation_currency"`
	ICTolerance          string         `json:"ic_tolerance"`
	Status               string         `json:"status"`
	CreatedBy            string         `json:"created_by"`
	CreatedAt            time.Time      `json:"created_at"`
	Members              []ConsolMember `json:"members,omitempty"`
}

// ConsolMember is the effective-dated membership of one entity's book in a group, with the ownership the group holds.
type ConsolMember struct {
	MemberID      string `json:"member_id"`
	GroupID       string `json:"group_id"`
	LegalEntityID string `json:"legal_entity_id"`
	SourceBookID  string `json:"source_book_id"`
	Method        string `json:"method"`
	OwnershipPct  string `json:"ownership_pct"`
	EffectiveFrom string `json:"effective_from"`
	EffectiveTo   string `json:"effective_to,omitempty"`
	// the capital consolidation of the member: the investor entity, the account that holds the investment, and the equity accounts of the member it is eliminated against
	InvestmentAccount string   `json:"investment_account,omitempty"`
	InvestorEntityID  string   `json:"investor_entity_id,omitempty"`
	EquityAccounts    []string `json:"equity_accounts,omitempty"`
}

// ConsolMapping is one immutable version of the local-account to group-account map and the intercompany roles.
type ConsolMapping struct {
	GroupID    string            `json:"group_id"`
	Version    int               `json:"version"`
	Status     string            `json:"status"`
	AccountMap map[string]string `json:"account_map"`
	ICRoles    map[string]string `json:"ic_roles"`
	CreatedBy  string            `json:"created_by"`
	CreatedAt  time.Time         `json:"created_at"`
}

// ICPosition is what one entity's book holds on one intercompany account with one counterparty in one transaction currency, as of a
// ledger watermark. Both figures are signed, debit positive.
type ICPosition struct {
	Entity       string `json:"entity"`
	Account      string `json:"account_code"`
	Counterparty string `json:"counterparty_id"`
	Currency     string `json:"currency"`
	TxBalance    string `json:"transaction_balance"`
	Functional   string `json:"functional_balance"`
}

// ConsolInput records one member's frozen population: the signed snapshot the run translated and the seal it rests on.
type ConsolInput struct {
	LegalEntityID      string `json:"legal_entity_id"`
	BookID             string `json:"accounting_book_id"`
	BookCode           string `json:"book_code"`
	FunctionalCurrency string `json:"functional_currency"`
	OwnershipPct       string `json:"ownership_pct"`
	SnapshotID         string `json:"snapshot_id"`
	ContentHash        string `json:"content_hash"`
	LedgerWatermark    int64  `json:"ledger_watermark"`
	Sealed             bool   `json:"sealed"`

	InvestmentAccount string   `json:"investment_account,omitempty"`
	InvestorEntityID  string   `json:"investor_entity_id,omitempty"`
	EquityAccounts    []string `json:"equity_accounts,omitempty"`
}

// ConsolRate is a rate a run translated with.
type ConsolRate struct {
	Currency string `json:"currency"`
	Class    string `json:"class"` // BS or PL
	Policy   string `json:"policy_id"`
	Rate     string `json:"rate"`
	RateID   string `json:"rate_id,omitempty"`
	RateType string `json:"rate_type,omitempty"`
	AsOf     string `json:"as_of,omitempty"`
}

// ConsolLine is one account of one member as translated.
type ConsolLine struct {
	LegalEntityID string `json:"legal_entity_id"`
	LocalAccount  string `json:"local_account"`
	GroupAccount  string `json:"group_account"`
	AccountType   string `json:"account_type"`
	Class         string `json:"rate_class"`
	Rate          string `json:"rate"`
	Functional    string `json:"functional_net"`
	Translated    string `json:"translated_net"`
}

// ICSide is the transaction-currency position one entity holds against the other, by currency.
type ICCurrencyRow struct {
	Currency   string `json:"currency"`
	SideA      string `json:"entity_a_balance"`
	SideB      string `json:"entity_b_balance"`
	Difference string `json:"difference"` // a + b: a receivable and its payable cancel, so anything left is a difference
}

// ICPair is the pairing of the two sides of the intercompany positions between two entities of one class.
type ICPair struct {
	Class      string          `json:"class"`
	EntityA    string          `json:"entity_a"`
	EntityB    string          `json:"entity_b"`
	Status     string          `json:"status"`
	Currencies []ICCurrencyRow `json:"currencies"`
	Eliminated bool            `json:"eliminated"`
	Residual   string          `json:"translation_residual,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Positions  []ICPosition    `json:"positions,omitempty"`
}

// ConsolException is something a run found that a person must look at. A blocking one stops the certification.
type ConsolException struct {
	Code     string         `json:"code"`
	Blocking bool           `json:"blocking"`
	Entity   string         `json:"legal_entity_id,omitempty"`
	Detail   string         `json:"detail"`
	Facts    map[string]any `json:"facts,omitempty"`
}

// NCIItem is the non-controlling share of a member's translated net assets, calculated by policy and recorded; it is not journalled.
type NCIItem struct {
	LegalEntityID string `json:"legal_entity_id"`
	NCIPct        string `json:"nci_pct"`
	NetAssets     string `json:"translated_net_assets"`
	NCIAmount     string `json:"nci_amount"`
}

// ConsolPopulation is the immutable record of what a run consumed and produced.
type ConsolPopulation struct {
	PresentationCurrency string               `json:"presentation_currency"`
	BalanceSheetPolicy   string               `json:"balance_sheet_policy"`
	ProfitLossPolicy     string               `json:"profit_loss_policy"`
	MappingVersion       int                  `json:"mapping_version"`
	Members              []ConsolInput        `json:"members"`
	Rates                []ConsolRate         `json:"rates"`
	Lines                []ConsolLine         `json:"lines"`
	CTA                  map[string]string    `json:"cta_by_entity"`
	Translation          map[string]string    `json:"translation"`  // group account -> cumulative translated net, CTA included
	Elimination          map[string]string    `json:"elimination"`  // group account -> cumulative elimination net
	Consolidated         map[string]string    `json:"consolidated"` // translation + elimination, zero accounts left out
	Pairs                []ICPair             `json:"intercompany_pairs"`
	NCI                  []NCIItem            `json:"non_controlling_interest"`
	Exceptions           []ConsolException    `json:"exceptions"`
	TranslationPosted    map[string]string    `json:"translation_posted"` // what this run posted: the change from the prior run
	EliminationPosted    map[string]string    `json:"elimination_posted"`
	Adjustment           map[string]string    `json:"group_adjustment"`        // cumulative effect of the approved group adjustments
	AdjustmentPosted     map[string]string    `json:"group_adjustment_posted"` // the change from the prior run
	Adjustments          []AppliedAdjustment  `json:"group_adjustments"`
	Capital              []CapitalElimination `json:"capital_eliminations"`
	PriorRunID           string               `json:"prior_run_id,omitempty"`
}

// ConsolRun is a completed consolidation run.
type ConsolRun struct {
	RunID                string           `json:"consolidation_run_id"`
	GroupID              string           `json:"group_id"`
	FiscalPeriod         string           `json:"fiscal_period"`
	AsOf                 string           `json:"as_of"`
	Status               string           `json:"status"`
	MappingVersion       int              `json:"mapping_version"`
	PriorRunID           string           `json:"prior_run_id,omitempty"`
	Population           ConsolPopulation `json:"population"`
	BlockingCount        int              `json:"blocking_count"`
	TranslationJournalID string           `json:"translation_journal_id,omitempty"`
	TranslationNumber    string           `json:"translation_journal_number,omitempty"`
	EliminationJournalID string           `json:"elimination_journal_id,omitempty"`
	EliminationNumber    string           `json:"elimination_journal_number,omitempty"`
	AdjustmentJournalID  string           `json:"adjustment_journal_id,omitempty"`
	AdjustmentNumber     string           `json:"adjustment_journal_number,omitempty"`
	GroupSnapshotID      string           `json:"group_snapshot_id,omitempty"`
	RunHash              string           `json:"run_hash"`
	CreatedBy            string           `json:"created_by"`
	CreatedAt            time.Time        `json:"created_at"`
	CertifiedBy          string           `json:"certified_by,omitempty"`
	CertifiedAt          *time.Time       `json:"certified_at,omitempty"`
	SupersededBy         string           `json:"superseded_by,omitempty"`
	SupersededAt         *time.Time       `json:"superseded_at,omitempty"`
	SupersedeReason      string           `json:"supersede_reason,omitempty"`
	Existing             bool             `json:"existing,omitempty"`
}

// RateClassOf says which translation rate class an account type takes.
func RateClassOf(accountType string) string {
	switch strings.ToUpper(accountType) {
	case "REVENUE", "EXPENSE":
		return "PL"
	}
	return "BS"
}

// ConsolMemberData is one member as the computation sees it.
type ConsolMemberData struct {
	Input     ConsolInput
	Lines     []SnapshotLine
	Positions []ICPosition
}

// ConsolCompute is everything a consolidation computation depends on.
type ConsolCompute struct {
	Group        ConsolGroup
	Mapping      ConsolMapping
	AccountTypes map[string]string
	Scale        int
	Members      []ConsolMemberData
	Rates        map[string]ConsolRate // currency|class
	CTAAccount   string
	DiffAccount  string
	Tolerance    *big.Rat

	// GoodwillAccount and NCIAccount carry the capital consolidation; Adjustments are the approved group adjustments up to the period.
	GoodwillAccount string
	NCIAccount      string
	Adjustments     []ConsolAdjustment
}

func rateKey(currency, class string) string { return currency + "|" + class }

func (c *ConsolCompute) translate(net *big.Rat, rate string) *big.Rat {
	x := new(big.Rat).Mul(net, dec(rate))
	return new(big.Rat).SetFrac(RoundMinor(x, c.Scale, "HALF_EVEN"), pow10(c.Scale))
}

func (c *ConsolCompute) rateFor(currency, class string) (ConsolRate, bool) {
	if currency == c.Group.PresentationCurrency {
		return ConsolRate{Currency: currency, Class: class, Rate: "1"}, true
	}
	r, ok := c.Rates[rateKey(currency, class)]
	return r, ok
}

func addTo(m map[string]*big.Rat, k string, v *big.Rat) {
	if cur, ok := m[k]; ok {
		cur.Add(cur, v)
		return
	}
	m[k] = new(big.Rat).Set(v)
}

func nonZeroStrings(m map[string]*big.Rat, scale int) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if v.Sign() != 0 {
			out[k] = v.FloatString(scale)
		}
	}
	return out
}

// Compute translates the members, pairs and eliminates their intercompany positions, and returns the population. A missing mapping
// or rate is an error here: a run that cannot translate everything does not produce a partial group figure.
func (c *ConsolCompute) Compute() (*ConsolPopulation, error) {
	pop := &ConsolPopulation{PresentationCurrency: c.Group.PresentationCurrency, MappingVersion: c.Mapping.Version, CTA: map[string]string{}, Exceptions: []ConsolException{}}
	members := append([]ConsolMemberData(nil), c.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Input.LegalEntityID < members[j].Input.LegalEntityID })
	memberSet := map[string]ConsolInput{}
	for _, m := range members {
		memberSet[m.Input.LegalEntityID] = m.Input
		pop.Members = append(pop.Members, m.Input)
	}

	// a local account that maps nowhere or a currency that has no rate stops the run, listing every one at once
	var unmapped []map[string]any
	translation := map[string]*big.Rat{}
	memberSum := map[string]*big.Rat{}
	used := map[string]ConsolRate{}
	for _, m := range members {
		ent := m.Input.LegalEntityID
		memberSum[ent] = new(big.Rat)
		for _, l := range m.Lines {
			net := dec(l.Net)
			if net.Sign() == 0 {
				continue
			}
			ga, ok := c.Mapping.AccountMap[l.AccountCode]
			if !ok {
				unmapped = append(unmapped, map[string]any{"legal_entity_id": ent, "account_code": l.AccountCode, "net_balance": l.Net})
				continue
			}
			typ := c.AccountTypes[l.AccountCode]
			class := RateClassOf(typ)
			rate, ok := c.rateFor(m.Input.FunctionalCurrency, class)
			if !ok {
				return nil, exc(FamFX, "CONSOLIDATION_RATE_UNAVAILABLE", "no %s rate for %s into %s", class, m.Input.FunctionalCurrency, c.Group.PresentationCurrency).with("currency", m.Input.FunctionalCurrency)
			}
			if m.Input.FunctionalCurrency != c.Group.PresentationCurrency {
				used[rateKey(m.Input.FunctionalCurrency, class)] = rate
			}
			tr := c.translate(net, rate.Rate)
			pop.Lines = append(pop.Lines, ConsolLine{LegalEntityID: ent, LocalAccount: l.AccountCode, GroupAccount: ga, AccountType: typ, Class: class, Rate: rate.Rate,
				Functional: net.FloatString(c.Scale), Translated: tr.FloatString(c.Scale)})
			addTo(translation, ga, tr)
			memberSum[ent].Add(memberSum[ent], tr)
		}
	}
	if len(unmapped) > 0 {
		return nil, exc(FamCOA, "CONSOLIDATION_MAPPING_MISSING", "%d balance(s) have no group account in mapping version %d", len(unmapped), c.Mapping.Version).with("accounts", unmapped)
	}
	keys := make([]string, 0, len(used))
	for k := range used {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pop.Rates = append(pop.Rates, used[k])
	}

	// a member's translated book no longer balances when its rates differ across classes: the difference is its translation reserve
	ctaTotal := new(big.Rat)
	for _, m := range members {
		cta := new(big.Rat).Neg(memberSum[m.Input.LegalEntityID])
		if cta.Sign() != 0 {
			pop.CTA[m.Input.LegalEntityID] = cta.FloatString(c.Scale)
			ctaTotal.Add(ctaTotal, cta)
		}
	}
	if ctaTotal.Sign() != 0 {
		addTo(translation, c.CTAAccount, ctaTotal)
	}

	// non-controlling interest: the minority's share of each partly owned member's translated net assets
	for _, m := range members {
		pct := dec(m.Input.OwnershipPct)
		if pct.Cmp(big.NewRat(100, 1)) >= 0 {
			continue
		}
		na := new(big.Rat)
		for _, l := range pop.Lines {
			if l.LegalEntityID == m.Input.LegalEntityID && (strings.EqualFold(l.AccountType, "ASSET") || strings.EqualFold(l.AccountType, "LIABILITY")) {
				na.Add(na, dec(l.Translated))
			}
		}
		share := new(big.Rat).Sub(big.NewRat(100, 1), pct)
		share.Quo(share, big.NewRat(100, 1))
		pop.NCI = append(pop.NCI, NCIItem{LegalEntityID: m.Input.LegalEntityID, NCIPct: new(big.Rat).Sub(big.NewRat(100, 1), pct).FloatString(4),
			NetAssets: na.FloatString(c.Scale), NCIAmount: c.translate(new(big.Rat).Mul(na, share), "1").FloatString(c.Scale)})
	}

	// intercompany: pair the two sides, eliminate the pairs that match
	elimination := map[string]*big.Rat{}
	if err := c.eliminate(pop, members, memberSet, elimination); err != nil {
		return nil, err
	}
	if err := c.capital(pop, members, memberSet, elimination); err != nil {
		return nil, err
	}
	adjustment := map[string]*big.Rat{}
	pop.Adjustments = []AppliedAdjustment{}
	for _, adj := range c.Adjustments {
		pop.Adjustments = append(pop.Adjustments, AppliedAdjustment{AdjustmentID: adj.AdjustmentID, FiscalPeriod: adj.FiscalPeriod, PolicyRef: adj.PolicyRef, EvidenceRef: adj.EvidenceRef})
		for _, l := range adj.Lines {
			addTo(adjustment, l.AccountCode, dec(l.Amount))
		}
	}
	pop.Adjustment = nonZeroStrings(adjustment, c.Scale)

	pop.Translation = nonZeroStrings(translation, c.Scale)
	pop.Elimination = nonZeroStrings(elimination, c.Scale)
	cons := map[string]*big.Rat{}
	for k, v := range translation {
		addTo(cons, k, v)
	}
	for k, v := range elimination {
		addTo(cons, k, v)
	}
	for k, v := range adjustment {
		addTo(cons, k, v)
	}
	pop.Consolidated = nonZeroStrings(cons, c.Scale)
	return pop, nil
}

func roleClass(role string) string {
	if role == RoleDueFrom || role == RoleDueTo {
		return ICClassBalance
	}
	return ICClassProfitLoss
}

func pairKey(class, a, b string) (string, string, string) {
	if a > b {
		a, b = b, a
	}
	return class + "|" + a + "|" + b, a, b
}

// PairPositions pairs the intercompany positions of the members and classifies every pair. It is used by the run and by the pairing
// report; it translates nothing.
func PairPositions(members []ConsolMemberData, roles map[string]string) (pairs []ICPair, exceptions []ConsolException) {
	memberSet := map[string]bool{}
	for _, m := range members {
		memberSet[m.Input.LegalEntityID] = true
	}
	type side struct{ ccy map[string]*big.Rat }
	type acc struct {
		a, b string
		cls  string
		sa   side
		sb   side
		pos  []ICPosition
	}
	byKey := map[string]*acc{}
	for _, m := range members {
		ent := m.Input.LegalEntityID
		for _, p := range m.Positions {
			role, ok := roles[p.Account]
			if !ok {
				continue
			}
			if p.Counterparty == "" {
				exceptions = append(exceptions, ConsolException{Code: "IC_COUNTERPARTY_MISSING", Blocking: true, Entity: ent,
					Detail: fmt.Sprintf("account %s carries an intercompany balance with no counterparty entity, so it cannot be paired", p.Account), Facts: map[string]any{"account_code": p.Account, "currency": p.Currency}})
				continue
			}
			if !memberSet[p.Counterparty] {
				exceptions = append(exceptions, ConsolException{Code: "IC_COUNTERPARTY_OUTSIDE_GROUP", Blocking: false, Entity: ent,
					Detail: fmt.Sprintf("account %s is held with %s, which is not a member of the group: it is not eliminated", p.Account, p.Counterparty), Facts: map[string]any{"account_code": p.Account, "counterparty_id": p.Counterparty}})
				continue
			}
			k, a, b := pairKey(roleClass(role), ent, p.Counterparty)
			g := byKey[k]
			if g == nil {
				g = &acc{a: a, b: b, cls: roleClass(role), sa: side{map[string]*big.Rat{}}, sb: side{map[string]*big.Rat{}}}
				byKey[k] = g
			}
			s := g.sa
			if ent == b {
				s = g.sb
			}
			addTo(s.ccy, p.Currency, dec(p.TxBalance))
			g.pos = append(g.pos, p)
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g := byKey[k]
		nz := func(s side) map[string]bool {
			o := map[string]bool{}
			for c, v := range s.ccy {
				if v.Sign() != 0 {
					o[c] = true
				}
			}
			return o
		}
		na, nb := nz(g.sa), nz(g.sb)
		all := map[string]bool{}
		for c := range g.sa.ccy {
			all[c] = true
		}
		for c := range g.sb.ccy {
			all[c] = true
		}
		ccys := make([]string, 0, len(all))
		for c := range all {
			ccys = append(ccys, c)
		}
		sort.Strings(ccys)
		p := ICPair{Class: g.cls, EntityA: g.a, EntityB: g.b, Status: ICMatched, Positions: g.pos}
		diffAny := false
		for _, c := range ccys {
			va, vb := new(big.Rat), new(big.Rat)
			if v, ok := g.sa.ccy[c]; ok {
				va = v
			}
			if v, ok := g.sb.ccy[c]; ok {
				vb = v
			}
			d := new(big.Rat).Add(va, vb)
			p.Currencies = append(p.Currencies, ICCurrencyRow{Currency: c, SideA: va.FloatString(2), SideB: vb.FloatString(2), Difference: d.FloatString(2)})
			diffAny = diffAny || d.Sign() != 0
		}
		sameSet := len(na) == len(nb)
		for c := range na {
			sameSet = sameSet && nb[c]
		}
		switch {
		case len(na) == 0 && len(nb) == 0:
			// each side nets to nothing on its own: there is nothing to pair, and it is eliminated as it stands
		case (len(na) == 0) != (len(nb) == 0) && diffAny:
			p.Status = ICUnmatched
			p.Reason = "only one of the two entities holds the position"
		case !sameSet && diffAny:
			p.Status = ICCurrencyDifference
			p.Reason = "the two entities hold the position in different currencies"
		case diffAny:
			p.Status = ICAmountDifference
			p.Reason = "the two sides are in the same currency but do not cancel"
		}
		pairs = append(pairs, p)
	}
	return
}

func (c *ConsolCompute) eliminate(pop *ConsolPopulation, members []ConsolMemberData, memberSet map[string]ConsolInput, elimination map[string]*big.Rat) error {
	pairs, excs := PairPositions(members, c.Mapping.ICRoles)
	pop.Exceptions = append(pop.Exceptions, excs...)
	var unmapped []map[string]any
	for i := range pairs {
		p := &pairs[i]
		if p.Status != ICMatched {
			p.Reason = strings.TrimSpace(p.Reason)
			pop.Exceptions = append(pop.Exceptions, ConsolException{Code: "IC_PAIR_" + p.Status, Blocking: true, Entity: p.EntityA,
				Detail: fmt.Sprintf("%s pair of %s and %s is %s: %s", strings.ToLower(p.Class), p.EntityA, p.EntityB, p.Status, p.Reason),
				Facts:  map[string]any{"entity_a": p.EntityA, "entity_b": p.EntityB, "class": p.Class, "currencies": p.Currencies}})
			continue
		}
		part := map[string]*big.Rat{}
		residual := new(big.Rat)
		ok := true
		for _, pos := range p.Positions {
			ga, mapped := c.Mapping.AccountMap[pos.Account]
			if !mapped {
				unmapped = append(unmapped, map[string]any{"legal_entity_id": pos.Entity, "account_code": pos.Account})
				ok = false
				continue
			}
			in := memberSet[pos.Entity]
			rate, found := c.rateFor(in.FunctionalCurrency, RateClassOf(c.AccountTypes[pos.Account]))
			if !found {
				return exc(FamFX, "CONSOLIDATION_RATE_UNAVAILABLE", "no rate for %s into %s", in.FunctionalCurrency, c.Group.PresentationCurrency).with("currency", in.FunctionalCurrency)
			}
			tr := c.translate(dec(pos.Functional), rate.Rate)
			residual.Add(residual, tr)
			addTo(part, ga, new(big.Rat).Neg(tr))
		}
		if !ok {
			continue
		}
		if new(big.Rat).Abs(residual).Cmp(c.Tolerance) > 0 {
			p.Status = ICBlocked
			p.Reason = "the translated positions differ by more than the group's intercompany tolerance"
			p.Residual = residual.FloatString(c.Scale)
			pop.Exceptions = append(pop.Exceptions, ConsolException{Code: "IC_PAIR_TRANSLATION_DIFFERENCE", Blocking: true, Entity: p.EntityA,
				Detail: fmt.Sprintf("%s pair of %s and %s matches in its transaction currencies but its translated positions differ by %s (tolerance %s)", strings.ToLower(p.Class), p.EntityA, p.EntityB, p.Residual, c.Tolerance.FloatString(c.Scale)),
				Facts:  map[string]any{"entity_a": p.EntityA, "entity_b": p.EntityB, "class": p.Class, "residual": p.Residual}})
			continue
		}
		for k, v := range part {
			addTo(elimination, k, v)
		}
		if residual.Sign() != 0 {
			addTo(elimination, c.DiffAccount, residual)
			p.Residual = residual.FloatString(c.Scale)
		}
		p.Eliminated = true
	}
	if len(unmapped) > 0 {
		return exc(FamCOA, "CONSOLIDATION_MAPPING_MISSING", "%d intercompany account(s) have no group account in mapping version %d", len(unmapped), c.Mapping.Version).with("accounts", unmapped)
	}
	pop.Pairs = pairs
	return nil
}

// PostedDelta is what a run posts for one journal: the change from the figures the prior run left in the group book.
func PostedDelta(now, prior map[string]string, scale int) map[string]string {
	d := map[string]*big.Rat{}
	for k, v := range now {
		addTo(d, k, dec(v))
	}
	for k, v := range prior {
		addTo(d, k, new(big.Rat).Neg(dec(v)))
	}
	return nonZeroStrings(d, scale)
}

// BlockingCount is the number of exceptions that stop a certification.
func (p *ConsolPopulation) BlockingCount() int {
	n := 0
	for _, e := range p.Exceptions {
		if e.Blocking {
			n++
		}
	}
	return n
}

// Hash is the digest of what the run consumed and produced.
func (p *ConsolPopulation) Hash(group, period, asOf string) string {
	b, _ := json.Marshal(struct {
		G, P, A string
		Pop     *ConsolPopulation
	}{group, period, asOf, p})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// AdjustmentLine is one line of a group adjustment: an account of the group chart and a signed amount, debit positive.
type AdjustmentLine struct {
	AccountCode string `json:"account_code"`
	Amount      string `json:"amount"`
	Description string `json:"description,omitempty"`
}

// ConsolAdjustment is a group policy adjustment: a balanced, evidenced journal that is part of the group figures, never of a member
// ledger. Someone other than its author approves it; a run applies every approved one up to its period.
type ConsolAdjustment struct {
	AdjustmentID     string           `json:"adjustment_id"`
	GroupID          string           `json:"group_id"`
	FiscalPeriod     string           `json:"fiscal_period"`
	Description      string           `json:"description"`
	PolicyRef        string           `json:"policy_ref"`
	EvidenceRef      string           `json:"evidence_ref"`
	Lines            []AdjustmentLine `json:"lines"`
	Status           string           `json:"status"`
	CreatedBy        string           `json:"created_by"`
	CreatedAt        time.Time        `json:"created_at"`
	ApprovedBy       string           `json:"approved_by,omitempty"`
	ApprovedAt       *time.Time       `json:"approved_at,omitempty"`
	WithdrawnBy      string           `json:"withdrawn_by,omitempty"`
	WithdrawnAt      *time.Time       `json:"withdrawn_at,omitempty"`
	WithdrawalReason string           `json:"withdrawal_reason,omitempty"`
}

// AppliedAdjustment records which adjustment a run applied.
type AppliedAdjustment struct {
	AdjustmentID string `json:"adjustment_id"`
	FiscalPeriod string `json:"fiscal_period"`
	PolicyRef    string `json:"policy_ref"`
	EvidenceRef  string `json:"evidence_ref"`
}

// CapitalElimination is the elimination of an investment against the equity it bought: the part held by the group, the part held by
// others (non-controlling interest) and the difference, goodwill (or a gain on a bargain) when the cost differs from the share of equity.
type CapitalElimination struct {
	Subsidiary   string `json:"subsidiary"`
	Investor     string `json:"investor"`
	OwnershipPct string `json:"ownership_pct"`
	Investment   string `json:"investment"`
	Equity       string `json:"eliminated_equity"`
	NCI          string `json:"non_controlling_interest"`
	Goodwill     string `json:"goodwill"`
}

// capital eliminates each investment against the equity of the member it holds, journalling the non-controlling interest.
func (c *ConsolCompute) capital(pop *ConsolPopulation, members []ConsolMemberData, memberSet map[string]ConsolInput, elimination map[string]*big.Rat) error {
	idx := map[string]*big.Rat{}
	for _, l := range pop.Lines {
		addTo(idx, l.LegalEntityID+"|"+l.LocalAccount, dec(l.Translated))
	}
	var unmapped []map[string]any
	for _, m := range members {
		in := m.Input
		if in.InvestmentAccount == "" {
			continue
		}
		block := func(code, detail string) {
			pop.Exceptions = append(pop.Exceptions, ConsolException{Code: code, Blocking: true, Entity: in.LegalEntityID, Detail: detail})
		}
		if _, ok := memberSet[in.InvestorEntityID]; !ok {
			block("CAPITAL_INVESTOR_NOT_A_MEMBER", fmt.Sprintf("the investor %s is not a member of the group", in.InvestorEntityID))
			continue
		}
		inv, held := idx[in.InvestorEntityID+"|"+in.InvestmentAccount]
		if !held || inv.Sign() == 0 {
			block("CAPITAL_INVESTMENT_MISSING", fmt.Sprintf("the investor holds nothing on account %s to eliminate", in.InvestmentAccount))
			continue
		}
		invGA, ok := c.Mapping.AccountMap[in.InvestmentAccount]
		if !ok {
			unmapped = append(unmapped, map[string]any{"legal_entity_id": in.InvestorEntityID, "account_code": in.InvestmentAccount})
			continue
		}
		part := map[string]*big.Rat{}
		equity := new(big.Rat)
		complete := true
		for _, ea := range in.EquityAccounts {
			net, has := idx[in.LegalEntityID+"|"+ea]
			if !has || net.Sign() == 0 {
				continue
			}
			ga, mapped := c.Mapping.AccountMap[ea]
			if !mapped {
				unmapped = append(unmapped, map[string]any{"legal_entity_id": in.LegalEntityID, "account_code": ea})
				complete = false
				continue
			}
			// the equity is a credit balance: eliminating it is a debit of the same size
			amt := new(big.Rat).Neg(net)
			addTo(part, ga, amt)
			equity.Add(equity, amt)
		}
		if !complete {
			continue
		}
		if equity.Sign() == 0 {
			block("CAPITAL_EQUITY_MISSING", "the equity accounts named for the member hold nothing to eliminate against")
			continue
		}
		pct := new(big.Rat).Quo(dec(in.OwnershipPct), big.NewRat(100, 1))
		nci := c.translate(new(big.Rat).Mul(equity, new(big.Rat).Sub(big.NewRat(1, 1), pct)), "1")
		goodwill := new(big.Rat).Sub(inv, new(big.Rat).Sub(equity, nci))
		addTo(part, invGA, new(big.Rat).Neg(inv))
		if nci.Sign() != 0 {
			addTo(part, c.NCIAccount, new(big.Rat).Neg(nci))
		}
		if goodwill.Sign() != 0 {
			addTo(part, c.GoodwillAccount, goodwill)
		}
		for k, v := range part {
			addTo(elimination, k, v)
		}
		pop.Capital = append(pop.Capital, CapitalElimination{Subsidiary: in.LegalEntityID, Investor: in.InvestorEntityID, OwnershipPct: in.OwnershipPct, Investment: inv.FloatString(c.Scale),
			Equity: equity.FloatString(c.Scale), NCI: nci.FloatString(c.Scale), Goodwill: goodwill.FloatString(c.Scale)})
	}
	if len(unmapped) > 0 {
		return exc(FamCOA, "CONSOLIDATION_MAPPING_MISSING", "%d capital consolidation account(s) have no group account in mapping version %d", len(unmapped), c.Mapping.Version).with("accounts", unmapped)
	}
	return nil
}
