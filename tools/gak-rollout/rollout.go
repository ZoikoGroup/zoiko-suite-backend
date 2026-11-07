package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Config is what one tenant's rollout needs: the entity and its book, the account each mapping key resolves to, and the rule packs to
// instantiate. Everything in it is reference data a person has decided; the tool only puts it where the kernel can read it.
type Config struct {
	LegalEntityID string            `json:"legal_entity_id"`
	Book          BookSpec          `json:"book"`
	Mappings      map[string]string `json:"mappings"`
	Packs         []PackSpec        `json:"packs"`
}

type BookSpec struct {
	BookCode              string `json:"book_code"`
	BookType              string `json:"book_type"`
	AccountingFrameworkID string `json:"accounting_framework_id"`
	Jurisdiction          string `json:"jurisdiction,omitempty"`
	FunctionalCurrency    string `json:"functional_currency"`
	FiscalCalendarID      string `json:"fiscal_calendar_id"`
	ChartOfAccountsID     string `json:"chart_of_accounts_id"`
	DefaultLedgerID       string `json:"default_ledger_id"`
	EffectiveFrom         string `json:"effective_from"`
}

type PackSpec struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	EffectiveFrom string `json:"effective_from"`
}

var (
	dateRE     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)
	bookTypes  = map[string]bool{"PRIMARY_STATUTORY": true, "MANAGEMENT": true, "TAX": true, "GROUP_CONSOLIDATION": true, "REGULATORY": true}
)

// Validate refuses a configuration that cannot describe a rollout, before anything is read from or written to the service.
func (c *Config) Validate() error {
	var errs []string
	bad := func(f, why string) { errs = append(errs, f+": "+why) }
	if strings.TrimSpace(c.LegalEntityID) == "" {
		bad("legal_entity_id", "required")
	}
	b := c.Book
	if b.BookCode == "" {
		bad("book.book_code", "required")
	}
	if !bookTypes[b.BookType] {
		bad("book.book_type", "PRIMARY_STATUTORY, MANAGEMENT, TAX, GROUP_CONSOLIDATION or REGULATORY")
	}
	if b.AccountingFrameworkID == "" {
		bad("book.accounting_framework_id", "required")
	}
	if !currencyRE.MatchString(b.FunctionalCurrency) {
		bad("book.functional_currency", "a three-letter currency code")
	}
	for f, v := range map[string]string{"book.fiscal_calendar_id": b.FiscalCalendarID, "book.chart_of_accounts_id": b.ChartOfAccountsID, "book.default_ledger_id": b.DefaultLedgerID} {
		if v == "" {
			bad(f, "required")
		}
	}
	if !dateRE.MatchString(b.EffectiveFrom) {
		bad("book.effective_from", "a date, YYYY-MM-DD")
	}
	if len(c.Mappings) == 0 {
		bad("mappings", "at least one mapping key")
	}
	for k, v := range c.Mappings {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			bad("mappings", "every key and every account code is required")
			break
		}
	}
	if len(c.Packs) == 0 {
		bad("packs", "at least one rule pack")
	}
	seen := map[string]bool{}
	for i, p := range c.Packs {
		at := fmt.Sprintf("packs[%d]", i)
		if p.Name == "" || p.Version == "" {
			bad(at, "a name and a version")
		}
		if !dateRE.MatchString(p.EffectiveFrom) {
			bad(at+".effective_from", "a date, YYYY-MM-DD")
		}
		if seen[p.Name] {
			bad(at+".name", "listed twice")
		}
		seen[p.Name] = true
	}
	if len(errs) > 0 {
		return errors.New("the configuration is not usable:\n  " + strings.Join(errs, "\n  "))
	}
	return nil
}

// Step statuses. OK is already in place; PLANNED is what a dry run would do; DONE is what an applied run did. CONFLICT, BLOCKED and FAILED
// need a person: the tool never overwrites, reactivates or forces.
const (
	StOK       = "OK"
	StPlanned  = "PLANNED"
	StDone     = "DONE"
	StConflict = "CONFLICT"
	StBlocked  = "BLOCKED"
	StFailed   = "FAILED"
	StWarning  = "WARNING"
)

type Step struct {
	Area   string `json:"area"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	Applied  bool   `json:"applied"`
	BookID   string `json:"book_id,omitempty"`
	Currency string `json:"functional_currency,omitempty"`
	Steps    []Step `json:"steps"`
}

func (r *Report) add(area, name, status, detail string) {
	r.Steps = append(r.Steps, Step{Area: area, Name: name, Status: status, Detail: detail})
}

// mappingsAdded reports whether this run created any mapping.
func (r *Report) mappingsAdded() bool {
	for _, s := range r.Steps {
		if s.Area == "mapping" && s.Status == StDone {
			return true
		}
	}
	return false
}

// NeedsAttention is true when a person has something to resolve before the rollout can go on.
func (r *Report) NeedsAttention() bool {
	for _, s := range r.Steps {
		if s.Status == StConflict || s.Status == StBlocked || s.Status == StFailed {
			return true
		}
	}
	return false
}

type bookView struct {
	BookID                string `json:"book_id"`
	BookCode              string `json:"book_code"`
	BookType              string `json:"book_type"`
	AccountingFrameworkID string `json:"accounting_framework_id"`
	FunctionalCurrency    string `json:"functional_currency"`
	Status                string `json:"status"`
}

type mappingView struct {
	MappingKey    string     `json:"mapping_key"`
	AccountCode   string     `json:"account_code"`
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
	CreatedAt     time.Time  `json:"created_at"`
}

type packView struct {
	Name        string   `json:"name"`
	MappingKeys []string `json:"mapping_keys"`
}

type readinessView struct {
	Ready    bool `json:"ready"`
	Blocking int  `json:"blocking"`
	Items    []struct {
		Kind     string `json:"kind"`
		Key      string `json:"key"`
		State    string `json:"state"`
		Blocking bool   `json:"blocking"`
		Account  string `json:"account_code"`
		Remedy   string `json:"remedy"`
	} `json:"items"`
}

// Run brings one tenant towards the kernel: the book, the mappings and the rule packs, then a readiness check of each pack. Without apply
// it only reads and reports what it would do. It stops short of what needs people: testing, approving and activating a rule set are
// done by three different people and are not done here.
func Run(ctx context.Context, c *client, cfg *Config, apply bool, now time.Time) (*Report, error) {
	rep := &Report{Applied: apply, Currency: cfg.Book.FunctionalCurrency}

	var packs struct {
		Packs []packView `json:"packs"`
	}
	if err := c.do(ctx, "GET", "/v1/posting-rule-packs", nil, &packs); err != nil {
		return nil, fmt.Errorf("reading the rule pack catalogue: %w", err)
	}
	needed := map[string]bool{}
	byName := map[string]packView{}
	for _, p := range packs.Packs {
		byName[p.Name] = p
	}
	var selected []PackSpec
	for _, p := range cfg.Packs {
		pv, ok := byName[p.Name]
		if !ok {
			rep.add("pack", p.Name, StFailed, "there is no such rule pack; the catalogue has: "+strings.Join(sortedNames(byName), ", "))
			continue
		}
		selected = append(selected, p)
		for _, k := range pv.MappingKeys {
			needed[k] = true
		}
	}

	bookStep(ctx, c, cfg, apply, rep)
	planned := mappingSteps(ctx, c, cfg, apply, needed, rep)
	for _, p := range selected {
		packStep(ctx, c, cfg, p, apply, rep)
	}
	today := now.UTC().Format("2006-01-02")
	for _, p := range selected {
		readinessStep(ctx, p, c, planned, today, rep)
	}
	return rep, nil
}

func sortedNames(m map[string]packView) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func bookStep(ctx context.Context, c *client, cfg *Config, apply bool, rep *Report) {
	b := cfg.Book
	var list struct {
		Books []bookView `json:"books"`
	}
	if err := c.do(ctx, "GET", "/v1/accounting-books?legal_entity_id="+url.QueryEscape(cfg.LegalEntityID), nil, &list); err != nil {
		rep.add("book", b.BookCode, StFailed, "reading the entity's books: "+err.Error())
		return
	}
	var have *bookView
	for i := range list.Books {
		if list.Books[i].BookCode == b.BookCode {
			have = &list.Books[i]
		}
	}
	create := func() {
		var got bookView
		body := map[string]any{"legal_entity_id": cfg.LegalEntityID, "book_code": b.BookCode, "book_type": b.BookType, "accounting_framework_id": b.AccountingFrameworkID,
			"functional_currency": b.FunctionalCurrency, "fiscal_calendar_id": b.FiscalCalendarID, "chart_of_accounts_id": b.ChartOfAccountsID,
			"default_ledger_id": b.DefaultLedgerID, "effective_from": b.EffectiveFrom + "T00:00:00Z"}
		if b.Jurisdiction != "" {
			body["jurisdiction"] = b.Jurisdiction
		}
		if err := c.do(ctx, "POST", "/v1/accounting-books", body, &got); err != nil {
			rep.add("book", b.BookCode, StFailed, "creating the book: "+err.Error())
			return
		}
		rep.BookID = got.BookID
		activate(ctx, c, got.BookID, b.BookCode, StDone, "created and activated", rep)
	}
	switch {
	case have == nil:
		if !apply {
			rep.add("book", b.BookCode, StPlanned, "would create the book and activate it")
			return
		}
		create()
	case have.BookType != b.BookType || have.AccountingFrameworkID != b.AccountingFrameworkID || have.FunctionalCurrency != b.FunctionalCurrency:
		rep.BookID = have.BookID
		rep.add("book", b.BookCode, StConflict, fmt.Sprintf("the book exists as %s / %s / %s and a book's identity is fixed; the configuration says %s / %s / %s",
			have.BookType, have.AccountingFrameworkID, have.FunctionalCurrency, b.BookType, b.AccountingFrameworkID, b.FunctionalCurrency))
	case have.Status == "ACTIVE":
		rep.BookID = have.BookID
		rep.add("book", b.BookCode, StOK, "active ("+have.BookID+")")
	case have.Status == "DRAFT":
		rep.BookID = have.BookID
		if !apply {
			rep.add("book", b.BookCode, StPlanned, "exists as a draft; would activate it ("+have.BookID+")")
			return
		}
		activate(ctx, c, have.BookID, b.BookCode, StDone, "activated", rep)
	default:
		rep.BookID = have.BookID
		rep.add("book", b.BookCode, StBlocked, "the book is "+have.Status+"; reactivating a book that was taken out of use is a decision for a person ("+have.BookID+")")
	}
}

func activate(ctx context.Context, c *client, id, code, status, detail string, rep *Report) {
	if err := c.do(ctx, "POST", "/v1/accounting-books/"+id+"/activate", map[string]any{}, nil); err != nil {
		rep.add("book", code, StFailed, "activating the book: "+err.Error())
		return
	}
	rep.add("book", code, status, detail+" ("+id+")")
}

// mappingSteps puts each mapping key where the rules can resolve it. A key that already resolves to the same account is left alone, and
// one that resolves to a different account is a conflict: a mapping decides which account a posting lands on, so it is never changed
// here. It returns the keys a dry run would add.
func mappingSteps(ctx context.Context, c *client, cfg *Config, apply bool, needed map[string]bool, rep *Report) map[string]bool {
	planned := map[string]bool{}
	var current []mappingView
	if err := c.do(ctx, "GET", "/v1/account-mappings", nil, &current); err != nil {
		rep.add("mapping", "(all)", StFailed, "reading the mappings: "+err.Error())
		return planned
	}
	now := map[string]mappingView{}
	for _, m := range current {
		if m.EffectiveTo != nil {
			continue
		}
		if prev, ok := now[m.MappingKey]; !ok || m.CreatedAt.After(prev.CreatedAt) {
			now[m.MappingKey] = m
		}
	}
	keys := make([]string, 0, len(cfg.Mappings))
	for k := range cfg.Mappings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want := cfg.Mappings[k]
		switch have, ok := now[k]; {
		case ok && have.AccountCode == want:
			rep.add("mapping", k, StOK, "-> "+want)
		case ok:
			rep.add("mapping", k, StConflict, fmt.Sprintf("already maps to %s and the configuration says %s; a mapping decides where postings land, so it is changed deliberately through the chart administration, never here", have.AccountCode, want))
		case !apply:
			planned[k] = true
			rep.add("mapping", k, StPlanned, "would map to "+want)
		default:
			if err := c.do(ctx, "POST", "/v1/account-mappings", map[string]string{"mapping_key": k, "account_code": want}, nil); err != nil {
				rep.add("mapping", k, StFailed, err.Error())
				continue
			}
			rep.add("mapping", k, StDone, "-> "+want)
		}
		if !needed[k] {
			rep.add("mapping", k, StWarning, "no selected rule pack uses this key; check it for a typo")
		}
	}
	return planned
}

func packStep(ctx context.Context, c *client, cfg *Config, p PackSpec, apply bool, rep *Report) {
	name := p.Name + " " + p.Version
	if !apply {
		rep.add("pack", name, StPlanned, "would instantiate the pack's rules as drafts")
		return
	}
	body := map[string]string{"version": p.Version, "effective_from": p.EffectiveFrom, "book_type": cfg.Book.BookType, "accounting_framework_id": cfg.Book.AccountingFrameworkID}
	err := c.do(ctx, "POST", "/v1/posting-rule-packs/"+url.PathEscape(p.Name)+"/instantiate", body, nil)
	var ae *apiError
	switch {
	case err == nil:
		rep.add("pack", name, StDone, "instantiated as drafts; they still need testing, approval by another person and activation by a third")
	case errors.As(err, &ae) && ae.Status == 409:
		rep.add("pack", name, StOK, "already instantiated")
	default:
		rep.add("pack", name, StFailed, err.Error())
	}
}

// readinessStep asks the kernel whether everything a pack names can resolve for this tenant. In a dry run, a mapping the plan would add is
// not held against the pack.
func readinessStep(ctx context.Context, p PackSpec, c *client, planned map[string]bool, today string, rep *Report) {
	var r readinessView
	// A mapping made through the API is in force from the day it is made and cannot be backdated, so the check is made as of the later of
	// the pack's date and today: that is the first day the mappings can resolve.
	on := p.EffectiveFrom
	if today > on {
		on = today
		if len(planned) > 0 || rep.mappingsAdded() {
			rep.add("readiness", p.Name, StWarning, "the pack is effective from "+p.EffectiveFrom+" but mappings made now take effect on "+today+": events dated earlier will not resolve them (AK-COA MAPPING_MISSING). Make the pack effective from the go-live date, on or after "+today)
		}
	}
	path := "/v1/rollout-readiness?pack=" + url.QueryEscape(p.Name) + "&on=" + url.QueryEscape(on)
	if err := c.do(ctx, "GET", path, nil, &r); err != nil {
		rep.add("readiness", p.Name, StFailed, err.Error())
		return
	}
	var gaps []string
	remedy := ""
	for _, it := range r.Items {
		if !it.Blocking {
			continue
		}
		if it.Kind == "MAPPING" && it.State == "UNMAPPED" && planned[it.Key] {
			continue
		}
		gaps = append(gaps, it.Kind+" "+it.Key+" "+it.State)
		if remedy == "" {
			remedy = it.Remedy
		}
	}
	if len(gaps) == 0 {
		rep.add("readiness", p.Name, StOK, "everything the pack names resolves")
		return
	}
	detail := strings.Join(gaps, ", ")
	if len(gaps) > 5 {
		detail = strings.Join(gaps[:5], ", ") + fmt.Sprintf(" and %d more", len(gaps)-5)
	}
	if remedy != "" {
		detail += ". For the first: " + remedy
	}
	rep.add("readiness", p.Name, StBlocked, detail)
}
