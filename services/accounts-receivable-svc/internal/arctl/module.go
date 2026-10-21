package arctl

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/ar07"
	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Sources are the injected collaborators of the control module.
type Sources struct {
	Documents    DocumentSource
	Completeness OpenItemCompleteness // *ar07.Module
	Postings     PostingSource
	Subledger    SubledgerSource
	GL           GLBalanceSource
	Exceptions   []ExceptionSource
}

// Module is the AR-to-GL control module.
type Module struct {
	deps        kit.Deps
	src         Sources
	mappingKeys []string
	pendingTTL  time.Duration
	glPrincipal string
}

// New builds the module. mappingKeys default to [AR_RECEIVABLE_STANDARD]; the pending TTL to DefaultPostingPendingTTL.
func New(deps kit.Deps, src Sources) *Module {
	return &Module{deps: deps, src: src, mappingKeys: []string{MappingARReceivable}, pendingTTL: DefaultPostingPendingTTL}
}

// WithMappingKeys overrides the GL mapping keys that make up the AR control account(s).
func (m *Module) WithMappingKeys(keys ...string) *Module {
	if len(keys) > 0 {
		m.mappingKeys = keys
	}
	return m
}

// WithPendingTTL sets how long a PENDING/IN_PROGRESS posting may wait before it is a finding.
func (m *Module) WithPendingTTL(d time.Duration) *Module {
	if d > 0 {
		m.pendingTTL = d
	}
	return m
}

// WithGLPrincipal makes the GL read use a service principal (e.g. cfg.PostingPrincipalID) rather than the requesting user,
// so the run does not depend on the user also holding GL read grants. Empty = the requesting user.
func (m *Module) WithGLPrincipal(p string) *Module { m.glPrincipal = p; return m }

// Register mounts the spec-style paths at the router root.
func (m *Module) Register(r chi.Router) {
	for _, q := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"getReconciliationRun", m.getReconciliationRun},
		{"listReconciliationRuns", m.listReconciliationRuns},
		{"getFindings", m.getFindings},
		{"getControlExceptions", m.getControlExceptions},
		{"getARControlLink", m.getARControlLink},
	} {
		h := m.deps.Query(q.h)
		r.Get("/internal/v1/arctl/"+q.name, h)
		r.Post("/internal/v1/arctl/"+q.name, h)
	}
	r.Post("/internal/v1/arctl/commands/runARGLReconciliation", m.deps.Cmd("ARCTL.RunARGLReconciliation", m.runARGLReconciliation))
	r.Post("/internal/v1/arctl/commands/acknowledgeFinding", m.deps.Cmd("ARCTL.AcknowledgeFinding", m.acknowledgeFinding))
}

func (m *Module) fail(w http.ResponseWriter, err error) {
	err = kit.MapPgErr(err)
	var p *kit.Problem
	if !errors.As(err, &p) {
		m.deps.Log.Error("arctl request failed", zap.Error(err))
	}
	kit.WriteErr(w, err)
}

// collect gathers every input of a run. GL / subledger failures are recorded as unavailable data (UNVERIFIED); a failure of
// any other source fails the run (503) - a partial run is never recorded as complete.
func (m *Module) collect(ctx context.Context, c kit.Caller, asOf time.Time, tol ToleranceRef) (*Inputs, error) {
	cutoff := CutoffOf(asOf)
	in := &Inputs{LegalEntityID: c.LegalEntityID, AsOfDate: asOf.Format("2006-01-02"), CutoffDate: cutoff.Format("2006-01-02"),
		ComputedAt: m.deps.Now(), PendingTTLSeconds: int64(m.pendingTTL / time.Second), Tolerance: tol}

	docs, err := m.src.Documents.ExpectedOpenItemDocuments(ctx, c.TenantID, c.LegalEntityID)
	if err != nil {
		return nil, err
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].DocumentID < docs[j].DocumentID })
	in.Expected = docs
	exp := make([]ar07.ExpectedDocument, len(docs))
	for i, d := range docs {
		exp[i] = ar07.ExpectedDocument{DocumentID: d.DocumentID, Currency: d.Currency, Amount: d.Amount}
	}
	if in.Completeness, err = m.src.Completeness.Completeness(ctx, c.TenantID, c.LegalEntityID, exp); err != nil {
		return nil, err
	}

	if in.Facts, in.Queue, err = m.src.Postings.PostingFacts(ctx, c.TenantID, c.LegalEntityID); err != nil {
		return nil, err
	}
	sort.SliceStable(in.Facts, func(i, j int) bool {
		if in.Facts[i].SourceType != in.Facts[j].SourceType {
			return in.Facts[i].SourceType < in.Facts[j].SourceType
		}
		return in.Facts[i].SourceEventID < in.Facts[j].SourceEventID
	})
	sort.SliceStable(in.Queue, func(i, j int) bool { return in.Queue[i].SourceEventID < in.Queue[j].SourceEventID })

	if sl, err := m.src.Subledger.OpenReceivable(ctx, c.TenantID, c.LegalEntityID, cutoff); err != nil || sl == nil {
		in.Subledger = Subledger{Available: false, Error: errText(err), Totals: map[string]kit.Amount{}}
	} else {
		in.Subledger = *sl
	}

	svc := c.SvcCaller()
	if m.glPrincipal != "" {
		svc.PrincipalID = m.glPrincipal
	}
	gl, err := m.src.GL.ControlAccountBalance(ctx, svc, c.LegalEntityID, PeriodOf(asOf), m.mappingKeys)
	if err != nil || gl == nil {
		gl = &GLRead{Available: false, Error: errText(err), MappingKeys: m.mappingKeys, PeriodID: PeriodOf(asOf), Totals: map[string]kit.Amount{}}
	}
	if gl.Totals == nil {
		gl.Totals = map[string]kit.Amount{}
	}
	in.GL = *gl

	in.Exceptions, err = m.rollup(ctx, c.TenantID, c.LegalEntityID)
	if err != nil {
		return nil, err
	}
	return in, nil
}

func errText(err error) string {
	if err == nil {
		return "no data returned"
	}
	return err.Error()
}

// rollup aggregates the visible exceptions of the owning modules.
func (m *Module) rollup(ctx context.Context, tenantID, legalEntityID string) ([]ExceptionItem, error) {
	out := []ExceptionItem{}
	for _, s := range m.src.Exceptions {
		items, err := s.Exceptions(ctx, tenantID, legalEntityID)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.Source == "" {
				it.Source = s.Name()
			}
			out = append(out, it)
		}
	}
	return out, nil
}

// Ensure the ar07 module satisfies the completeness port and the domain import stays used by population wiring.
var (
	_ OpenItemCompleteness = (*ar07.Module)(nil)
	_ PopulationFunc       = func(context.Context, domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
		return nil, nil
	}
)
