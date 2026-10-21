package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
)

// memManual is an in-memory manual-input store that applies the same rules as the real one (four eyes, final
// decisions) so handler behaviour is tested without a database.
type memManual struct {
	manuals map[string]*domain.ManualInput
	events  []domain.OutboxEvent
}

func (m *memManual) ProposeManualInput(_ context.Context, in *domain.ManualInput, mk store.ManualEventBuilder) error {
	if m.manuals == nil {
		m.manuals = map[string]*domain.ManualInput{}
	}
	in.State, in.TenantID, in.CreatedAt = domain.ManualProposed, "tenant-abc", time.Now()
	c := *in
	m.manuals[in.ManualID] = &c
	return m.emit(mk, "PROPOSE", &c)
}

func (m *memManual) emit(mk store.ManualEventBuilder, kind string, in *domain.ManualInput) error {
	if mk == nil {
		return nil
	}
	ev, err := mk(store.ManualChange{Kind: kind, Input: in})
	if err != nil {
		return err
	}
	m.events = append(m.events, ev)
	return nil
}

func (m *memManual) GetManualInput(_ context.Context, id string) (*domain.ManualInput, error) {
	if in, ok := m.manuals[id]; ok {
		c := *in
		return &c, nil
	}
	return nil, domain.ErrManualInputNotFound
}

func (m *memManual) DecideManualInput(_ context.Context, id, decision, actor, note string, mk store.ManualEventBuilder) (*domain.ManualInput, error) {
	in, ok := m.manuals[id]
	if !ok {
		return nil, domain.ErrManualInputNotFound
	}
	if in.State != domain.ManualProposed {
		return nil, domain.ErrManualInputNotPending
	}
	switch decision {
	case domain.ManualApproved, domain.ManualRejected:
		if in.ProposedBy == actor {
			return nil, domain.ErrManualFourEyes
		}
	case domain.ManualWithdrawn:
		if in.ProposedBy != actor {
			return nil, domain.ErrManualOnlyMakerWithdraws
		}
	}
	now := time.Now()
	in.State, in.DecidedBy, in.DecidedAt = decision, &actor, &now
	if note != "" {
		in.DecisionNote = &note
	}
	kind := map[string]string{domain.ManualApproved: "APPROVE", domain.ManualRejected: "REJECT", domain.ManualWithdrawn: "WITHDRAW"}[decision]
	c := *in
	return &c, m.emit(mk, kind, &c)
}

func (m *memManual) ListManualInputs(_ context.Context, entity, period, state string) ([]domain.ManualInput, error) {
	out := []domain.ManualInput{}
	for _, in := range m.manuals {
		if in.LegalEntityID == entity && (period == "" || in.PayrollPeriodID == period) && (state == "" || in.State == state) {
			out = append(out, *in)
		}
	}
	return out, nil
}

func (m *memManual) ListApprovedManualInputs(ctx context.Context, entity, period string) ([]domain.ManualInput, error) {
	return m.ListManualInputs(ctx, entity, period, domain.ManualApproved)
}

const manBase = "/v1/payroll/wfp/manual-inputs"

func newManualRouter(st *stubInputStore, az handler.AuthZClient, p *stubPuller) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	h := handler.NewWFPInput(st, az, p, &stubRuns{}, zap.NewNop())
	handler.RegisterWFPInputRoutes(r, h)
	handler.RegisterWFPManualInputRoutes(r, h)
	return r
}

func manualBody(worker string) map[string]any {
	return map[string]any{
		"payroll_period_id": "2026-09", "worker_id": worker, "input_code": "SPOT-AWARD", "category": "EARNING",
		"amount": "250.0000", "currency": "GBP", "effective_date": "2026-09-20", "reason": "spot award for incident cover",
		"evidence_ref": "TICKET-4411",
	}
}

func propose(t *testing.T, r chi.Router, worker string, c caller2) map[string]any {
	t.Helper()
	rr := inDo(r, "POST", manBase, manualBody(worker), c)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	return out
}

var checker = caller2{principal: "p-2", entity: "le-1"}

func TestWFP03Manual_ProposeValidatesAndNeverPaysOnItsOwn(t *testing.T) {
	st, p, _ := newStubs()
	r := newManualRouter(st, &stubAuthZ{}, p)
	worker := uuid.NewString()
	out := propose(t, r, worker, inCaller)
	assert.Equal(t, "PROPOSED", out["state"])
	assert.Equal(t, "p-1", out["proposed_by"])
	assert.Equal(t, "le-1", out["legal_entity_id"], "the entity is the authorized scope of the caller")

	for name, mut := range map[string]func(map[string]any){
		"no evidence":     func(b map[string]any) { b["evidence_ref"] = " " },
		"no reason":       func(b map[string]any) { b["reason"] = "" },
		"zero amount":     func(b map[string]any) { b["amount"] = "0" },
		"negative amount": func(b map[string]any) { b["amount"] = "-5.00" },
		"not a number":    func(b map[string]any) { b["amount"] = "abc" },
		"bad category":    func(b map[string]any) { b["category"] = "BENEFIT" },
		"bad currency":    func(b map[string]any) { b["currency"] = "gbp" },
		"bad worker":      func(b map[string]any) { b["worker_id"] = "nobody" },
		"bad date":        func(b map[string]any) { b["effective_date"] = "20/09/2026" },
	} {
		b := manualBody(worker)
		mut(b)
		assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", manBase, b, inCaller).Code, name)
	}
	assert.Len(t, st.manuals, 1, "only the valid proposal was stored")

	assert.Equal(t, http.StatusUnauthorized, inDo(r, "POST", manBase, manualBody(worker), caller2{}).Code)
	denied := newManualRouter(st, &stubAuthZ{err: domain.ErrAuthorizationDenied}, p)
	assert.Equal(t, http.StatusForbidden, inDo(denied, "POST", manBase, manualBody(worker), inCaller).Code)
}

func TestWFP03Manual_TheMakerCanNeverBeTheChecker(t *testing.T) {
	st, p, _ := newStubs()
	r := newManualRouter(st, &stubAuthZ{}, p)
	id := propose(t, r, uuid.NewString(), inCaller)["manual_id"].(string)

	rr := inDo(r, "POST", manBase+"/"+id+"/approve", nil, inCaller)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.Equal(t, "FOUR_EYES_REQUIRED", inCode(t, rr))
	rr = inDo(r, "POST", manBase+"/"+id+"/reject", map[string]any{"note": "no"}, inCaller)
	assert.Equal(t, http.StatusForbidden, rr.Code, "the maker cannot reject either")
	assert.Equal(t, domain.ManualProposed, st.manuals[id].State)

	rr = inDo(r, "POST", manBase+"/"+id+"/approve", nil, checker)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, domain.ManualApproved, st.manuals[id].State)
	assert.Equal(t, "p-2", *st.manuals[id].DecidedBy)

	rr = inDo(r, "POST", manBase+"/"+id+"/reject", map[string]any{"note": "changed my mind"}, caller2{principal: "p-3", entity: "le-1"})
	assert.Equal(t, http.StatusConflict, rr.Code, "a decision is final")
	rr = inDo(r, "POST", manBase+"/"+id+"/withdraw", nil, inCaller)
	assert.Equal(t, http.StatusConflict, rr.Code, "an approved input cannot be withdrawn")
}

func TestWFP03Manual_RejectNeedsANoteAndOnlyTheMakerWithdraws(t *testing.T) {
	st, p, _ := newStubs()
	r := newManualRouter(st, &stubAuthZ{}, p)
	id := propose(t, r, uuid.NewString(), inCaller)["manual_id"].(string)

	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", manBase+"/"+id+"/reject", nil, checker).Code, "a rejection says why")
	assert.Equal(t, http.StatusForbidden, inDo(r, "POST", manBase+"/"+id+"/withdraw", nil, checker).Code, "only the maker withdraws")
	assert.Equal(t, http.StatusOK, inDo(r, "POST", manBase+"/"+id+"/withdraw", nil, inCaller).Code)
	assert.Equal(t, domain.ManualWithdrawn, st.manuals[id].State)
	assert.Equal(t, http.StatusNotFound, inDo(r, "POST", manBase+"/"+uuid.NewString()+"/approve", nil, checker).Code)
	assert.Equal(t, http.StatusBadRequest, inDo(r, "POST", manBase+"/not-a-uuid/approve", nil, checker).Code)
}

func TestWFP03Manual_DecisionsAreAuthorizedAgainstTheEntityOfTheInput(t *testing.T) {
	st, p, _ := newStubs()
	az := &entityAuthZ{}
	r := newManualRouter(st, az, p)
	id := propose(t, r, uuid.NewString(), inCaller)["manual_id"].(string)

	az.entities, az.actions = nil, nil
	rr := inDo(r, "POST", manBase+"/"+id+"/approve", nil, caller2{principal: "p-2", entity: "le-OTHER"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, []string{"le-1"}, az.entities, "the header entity is ignored; the entity of the input is checked")
	assert.Equal(t, []string{"WFP_MANUAL_INPUT_DECIDE"}, az.actions)
}

func TestWFP03Manual_EventsCarryNoAmountOrFreeText(t *testing.T) {
	st, p, _ := newStubs()
	r := newManualRouter(st, &stubAuthZ{}, p)
	id := propose(t, r, uuid.NewString(), inCaller)["manual_id"].(string)
	inDo(r, "POST", manBase+"/"+id+"/approve", nil, checker)
	require.Len(t, st.events, 2)
	assert.Equal(t, domain.EventManualInputProposed, st.events[0].EventType)
	assert.Equal(t, domain.EventManualInputApproved, st.events[1].EventType)
	for _, ev := range st.events {
		body := string(ev.Payload)
		assert.NotContains(t, body, "250.0000", "no amount travels")
		assert.NotContains(t, body, `"amount"`, "no amount field travels")
		assert.NotContains(t, body, "spot award", "no free text travels")
		assert.NotContains(t, body, "TICKET-4411")
	}
}

// ── how approved inputs reach a batch ───────────────────────────────────────

func manualMappings() map[string]domain.InputCodeMapping {
	m := testMappings()
	m[domain.MappingKey(domain.SourceManual, "SPOT-AWARD")] = domain.InputCodeMapping{MappingID: "m-spot", SourceKind: domain.SourceManual, SourceCode: "SPOT-AWARD",
		MappingVersion: 1, PayrollCode: "P-SPOT", Category: "EARNING", TaxTreatment: "TAXABLE", Active: true}
	return m
}

func TestWFP03Manual_OnlyApprovedInputsFlowIntoABatchAndAreClassifiedByTheMapping(t *testing.T) {
	st, p, _ := newStubs()
	st.mappings = manualMappings()
	worker := p.sources[0].WorkerID
	r := newManualRouter(st, &stubAuthZ{}, p)
	approved := propose(t, r, worker, inCaller)["manual_id"].(string)
	inDo(r, "POST", manBase+"/"+approved+"/approve", nil, checker)
	proposedOnly := propose(t, r, worker, inCaller)["manual_id"].(string)
	rejected := propose(t, r, worker, inCaller)["manual_id"].(string)
	inDo(r, "POST", manBase+"/"+rejected+"/reject", map[string]any{"note": "not evidenced"}, checker)

	rr := inDo(r, "POST", inBase, createBody(), inCaller)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	var manual []domain.InputLine
	for _, l := range st.created {
		if l.SourceKind == domain.SourceManual {
			manual = append(manual, l)
		}
	}
	require.Len(t, manual, 1, "only the approved input is a line")
	l := manual[0]
	assert.Equal(t, "MANUAL:"+approved, l.SourceLineID)
	assert.NotEqual(t, "MANUAL:"+proposedOnly, l.SourceLineID)
	assert.Equal(t, domain.InputLineAccepted, l.State)
	assert.Equal(t, "P-SPOT", *l.PayrollCode, "the governed mapping classifies it, not the proposer")
	assert.Equal(t, "250.0000", l.Amount)
	require.NotNil(t, l.ApprovalRef)
	assert.Equal(t, "p-2", *l.ApprovalRef, "the approver is the approval evidence of the line")
	assert.Contains(t, *l.Reason, "TICKET-4411", "the evidence reference stays on the line")
}

func TestWFP03Manual_UnmappedMismatchedAndUnknownWorkerInputsAreQuarantinedNotPaid(t *testing.T) {
	st, p, _ := newStubs()
	st.mappings = manualMappings()
	worker := p.sources[0].WorkerID
	r := newManualRouter(st, &stubAuthZ{}, p)
	approve := func(body map[string]any) string {
		rr := inDo(r, "POST", manBase, body, inCaller)
		require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
		id := out["manual_id"].(string)
		require.Equal(t, http.StatusOK, inDo(r, "POST", manBase+"/"+id+"/approve", nil, checker).Code)
		return id
	}
	unmapped := manualBody(worker)
	unmapped["input_code"] = "NEVER-MAPPED"
	wrongCat := manualBody(worker)
	wrongCat["category"] = "DEDUCTION" // the mapping says EARNING
	stranger := uuid.NewString()
	idUnmapped, idWrong, idStranger := approve(unmapped), approve(wrongCat), approve(manualBody(stranger))

	body := createBody()
	body["on_invalid"] = "QUARANTINE_LINES"
	rr := inDo(r, "POST", inBase, body, inCaller)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	reasons := map[string]string{}
	for _, l := range st.created {
		if l.SourceKind == domain.SourceManual {
			require.NotNil(t, l.Quarantine, l.SourceLineID)
			reasons[l.SourceLineID] = *l.Quarantine
		}
	}
	assert.Equal(t, domain.InReasonUnmapped, reasons["MANUAL:"+idUnmapped])
	assert.Equal(t, domain.InReasonCategoryConflict, reasons["MANUAL:"+idWrong])
	assert.Equal(t, domain.InReasonWorkerContext, reasons["MANUAL:"+idStranger], "an input for a worker outside the population is visible, never dropped")
}
