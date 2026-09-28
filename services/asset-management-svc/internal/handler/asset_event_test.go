package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/asset-management-svc/internal/domain"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func createDraftAssetEvent(t *testing.T, r chi.Router, assetID, eventType string, extra domain.CreateAssetEventRequest) domain.AssetEvent {
	t.Helper()
	req := extra
	req.AssetID = assetID
	req.EventType = eventType
	if req.SourceDocumentRef == "" {
		req.SourceDocumentRef = "SRC-DOC-1"
	}
	if req.FiscalPeriod == "" {
		req.FiscalPeriod = "2026-09"
	}
	rr := doReq(r, http.MethodPost, "/v1/asset-events/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create asset event failed: %d %s", rr.Code, rr.Body.String())
	}
	var e domain.AssetEvent
	_ = json.NewDecoder(rr.Body).Decode(&e)
	return e
}

func walkToApproved(t *testing.T, r chi.Router, eventID string) {
	t.Helper()
	v := doReq(r, http.MethodPost, "/v1/asset-events/"+eventID+"/validate", nil, "preparer-1")
	if v.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", v.Code, v.Body.String())
	}
	a := doReq(r, http.MethodPost, "/v1/asset-events/"+eventID+"/approve", nil, "approver-1")
	if a.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", a.Code, a.Body.String())
	}
}

// ── CreateAssetEvent ─────────────────────────────────────────────────────────

func TestCreateAssetEvent_MissingEventType_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")

	req := domain.CreateAssetEventRequest{AssetID: id, SourceDocumentRef: "SRC-1", FiscalPeriod: "2026-09"}
	rr := doReq(r, http.MethodPost, "/v1/asset-events/", req, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateAssetEvent_ComponentReplacementWithoutComponent_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")

	req := domain.CreateAssetEventRequest{AssetID: id, SourceDocumentRef: "SRC-1", FiscalPeriod: "2026-09"}
	req.EventType = domain.AssetEventTypeComponentReplacement
	rr := doReq(r, http.MethodPost, "/v1/asset-events/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Negative path #3: cross-entity transfer blocked ─────────────────────────

func TestCreateAssetEvent_CrossEntityTransfer_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")

	req := domain.CreateAssetEventRequest{
		AssetID: id, SourceDocumentRef: "SRC-1", FiscalPeriod: "2026-09",
		DestinationLegalEntityID: "le-2",
	}
	req.EventType = domain.AssetEventTypeTransfer
	rr := doReq(r, http.MethodPost, "/v1/asset-events/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 cross-entity transfer blocked, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateAssetEvent_SameEntityTransfer_Allowed(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")

	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeTransfer, domain.CreateAssetEventRequest{
		DestinationLegalEntityID: "le-1", DestinationCustodianID: "custodian-2",
	})
	if e.Status != domain.AssetEventStatusDraft {
		t.Fatalf("expected DRAFT, got %q", e.Status)
	}
}

// ── Negative path #1: impairment/revaluation requires evidence ──────────────

func TestValidateAssetEvent_ImpairmentWithoutEvidence_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	amount := 1000.0
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeImpairment, domain.CreateAssetEventRequest{Amount: &amount})

	rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/validate", nil, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestValidateAssetEvent_ImpairmentWithEvidence_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	amount := 1000.0
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeImpairment, domain.CreateAssetEventRequest{
		Amount: &amount, ValuationEvidenceRef: "APPRAISAL-1",
	})

	rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/validate", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── SoD: self-approval blocked only for material types ──────────────────────

func TestApproveAssetEvent_MaterialType_SelfApproval_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	amount := 1000.0
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeImpairment, domain.CreateAssetEventRequest{
		Amount: &amount, ValuationEvidenceRef: "APPRAISAL-1",
	})
	v := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/validate", nil, "preparer-1")
	if v.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", v.Code, v.Body.String())
	}
	approve := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/approve", nil, "preparer-1")
	if approve.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval, got %d: %s", approve.Code, approve.Body.String())
	}
}

func TestApproveAssetEvent_NonMaterialType_SelfApproval_Allowed(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeAddition, domain.CreateAssetEventRequest{})
	v := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/validate", nil, "preparer-1")
	if v.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", v.Code, v.Body.String())
	}
	approve := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/approve", nil, "preparer-1")
	if approve.Code != http.StatusOK {
		t.Fatalf("expected 200 (non-material self-approval allowed), got %d: %s", approve.Code, approve.Body.String())
	}
}

// ── ApplyAssetEvent: DISPOSAL drives a real book-state delta ────────────────

func TestApplyAssetEvent_Disposal_TransitionsAssetToDisposed(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	proceeds := 500.0
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeDisposal, domain.CreateAssetEventRequest{ProceedsAmount: &proceeds})
	walkToApproved(t, r, e.EventID)

	rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/apply", nil, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("apply failed: %d %s", rr.Code, rr.Body.String())
	}
	if s.assets[id].Status != domain.AssetStatusDisposed {
		t.Fatalf("expected asset DISPOSED, got %q", s.assets[id].Status)
	}
}

func TestApplyAssetEvent_DisposalTwice_SecondRefused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	proceeds := 500.0

	first := createDraftAssetEvent(t, r, id, domain.AssetEventTypeDisposal, domain.CreateAssetEventRequest{ProceedsAmount: &proceeds})
	walkToApproved(t, r, first.EventID)
	applyFirst := doReq(r, http.MethodPost, "/v1/asset-events/"+first.EventID+"/apply", nil, "approver-1")
	if applyFirst.Code != http.StatusOK {
		t.Fatalf("first apply failed: %d %s", applyFirst.Code, applyFirst.Body.String())
	}

	second := createDraftAssetEvent(t, r, id, domain.AssetEventTypeDisposal, domain.CreateAssetEventRequest{ProceedsAmount: &proceeds})
	walkToApproved(t, r, second.EventID)
	applySecond := doReq(r, http.MethodPost, "/v1/asset-events/"+second.EventID+"/apply", nil, "approver-1")
	if applySecond.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 disposing an already-DISPOSED asset, got %d: %s", applySecond.Code, applySecond.Body.String())
	}
}

// ── Negative path #4: hard-closed-period ─────────────────────────────────────

func TestApplyAssetEvent_PeriodLocked_Returns422(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{checkPeriodErr: domain.ErrPeriodLocked}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	id := createActiveAsset(t, s, r, "le-1")
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeAddition, domain.CreateAssetEventRequest{})
	walkToApproved(t, r, e.EventID)

	rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/apply", nil, "approver-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 period locked, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.assetEvents[e.EventID].Status != domain.AssetEventStatusApproved {
		t.Fatalf("expected event to remain APPROVED when period is locked, got %q", s.assetEvents[e.EventID].Status)
	}
}

func TestApplyAssetEvent_PeriodCheckUnavailable_Returns503(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{checkPeriodErr: domain.ErrPeriodCheckUnavailable}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	id := createActiveAsset(t, s, r, "le-1")
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeAddition, domain.CreateAssetEventRequest{})
	walkToApproved(t, r, e.EventID)

	rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/apply", nil, "approver-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── ApplyAssetEvent: $ amount posts a journal in the same command ───────────

func TestApplyAssetEvent_WithAmountAndAccountCodes_PostsJournalAndEmits(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{postJournalID: "jrnl-1"}
	pub := &stubPublisher{}
	r := newRouterWithLedger(s, pub, &stubAuthZ{}, ledger)
	id := createActiveAsset(t, s, r, "le-1")
	amount := 250.0
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeImpairment, domain.CreateAssetEventRequest{
		Amount: &amount, ValuationEvidenceRef: "APPRAISAL-1",
		DebitAccountCode: "IMPAIRMENT-EXPENSE", CreditAccountCode: "ACCUM-IMPAIRMENT",
	})
	walkToApproved(t, r, e.EventID)
	callsBeforeApply := pub.calls

	rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/apply", nil, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("apply failed: %d %s", rr.Code, rr.Body.String())
	}
	if s.assetEvents[e.EventID].Status != domain.AssetEventStatusAccountingEventEmitted {
		t.Fatalf("expected ACCOUNTING_EVENT_EMITTED, got %q", s.assetEvents[e.EventID].Status)
	}
	if ledger.postCalls != 1 {
		t.Fatalf("expected exactly 1 journal post, got %d", ledger.postCalls)
	}
	if ledger.lastPostedSourceEventID != e.EventID {
		t.Fatalf("expected source_event_id keyed by the event's own ID, got %q", ledger.lastPostedSourceEventID)
	}
	// Three publishes on a successful IMPAIRMENT apply-with-journal:
	// PublishAssetEventAccountingEventEmitted (financial-close-svc's own
	// ACC-18 lineage consumer's real source), PublishAssetEventApplied
	// (fires on every successful apply), and PublishAssetImpaired (the
	// type-specific event for this event_type).
	if pub.calls != callsBeforeApply+3 {
		t.Fatalf("expected exactly 3 new publish calls (accounting-event-emitted + applied + impaired), got %d", pub.calls-callsBeforeApply)
	}
}

func TestApplyAssetEvent_NoAmount_LandsInAppliedNeverEmitted(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{}
	pub := &stubPublisher{}
	r := newRouterWithLedger(s, pub, &stubAuthZ{}, ledger)
	id := createActiveAsset(t, s, r, "le-1")
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeAddition, domain.CreateAssetEventRequest{})
	walkToApproved(t, r, e.EventID)
	callsBeforeApply := pub.calls

	rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/apply", nil, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("apply failed: %d %s", rr.Code, rr.Body.String())
	}
	if s.assetEvents[e.EventID].Status != domain.AssetEventStatusApplied {
		t.Fatalf("expected APPLIED (never emitted, no $ amount), got %q", s.assetEvents[e.EventID].Status)
	}
	if ledger.postCalls != 0 {
		t.Fatalf("expected no journal post for a $0 event, got %d calls", ledger.postCalls)
	}
	// No journal posted means no accounting-event-emitted signal — but
	// PublishAssetEventApplied still fires on every successful apply
	// regardless of whether a journal was posted, and ADDITION has no
	// type-specific event (only IMPAIRMENT/REVALUATION/DISPOSAL do).
	if pub.calls != callsBeforeApply+1 {
		t.Fatalf("expected exactly 1 new publish call (applied only, no journal/type-specific event), got %d", pub.calls-callsBeforeApply)
	}
}

// TestAssetEvent_PublishesRemainingLifecycleEvents proves
// AssetEventCreated/AssetEventApproved/AssetEventReversed/AssetRevalued
// — the spec's own remaining named AST-03 events, previously never
// published — each fire exactly once at their own real lifecycle step.
// AssetImpaired/AssetDisposed are already covered by the IMPAIRMENT and
// DISPOSAL apply tests above/below.
func TestAssetEvent_PublishesRemainingLifecycleEvents(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{}
	pub := &stubPublisher{}
	r := newRouterWithLedger(s, pub, &stubAuthZ{}, ledger)
	id := createActiveAsset(t, s, r, "le-1")
	amount := 300.0

	callsBeforeCreate := pub.calls
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeRevaluation, domain.CreateAssetEventRequest{
		Amount: &amount, ValuationEvidenceRef: "APPRAISAL-2",
		DebitAccountCode: "FIXED-ASSETS", CreditAccountCode: "REVALUATION-SURPLUS",
	})
	if pub.calls != callsBeforeCreate+1 {
		t.Fatalf("expected 1 new publish call (AssetEventCreated), got %d", pub.calls-callsBeforeCreate)
	}

	validateRR := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/validate", nil, "preparer-1")
	if validateRR.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", validateRR.Code, validateRR.Body.String())
	}

	callsBeforeApprove := pub.calls
	approveRR := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/approve", nil, "approver-1")
	if approveRR.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", approveRR.Code, approveRR.Body.String())
	}
	if pub.calls != callsBeforeApprove+1 {
		t.Fatalf("expected 1 new publish call (AssetEventApproved), got %d", pub.calls-callsBeforeApprove)
	}

	callsBeforeApply := pub.calls
	applyRR := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/apply", nil, "approver-1")
	if applyRR.Code != http.StatusOK {
		t.Fatalf("apply failed: %d %s", applyRR.Code, applyRR.Body.String())
	}
	// Account codes given, so a journal posts: AccountingEventEmitted +
	// AssetEventApplied + AssetRevalued.
	if pub.calls != callsBeforeApply+3 {
		t.Fatalf("expected 3 new publish calls (accounting-event-emitted + applied + revalued), got %d", pub.calls-callsBeforeApply)
	}

	callsBeforeReverse := pub.calls
	reverseRR := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/reverse", domain.ReverseAssetEventRequest{Reason: "wrong appraisal"}, "preparer-1")
	if reverseRR.Code != http.StatusOK {
		t.Fatalf("reverse failed: %d %s", reverseRR.Code, reverseRR.Body.String())
	}
	if pub.calls != callsBeforeReverse+1 {
		t.Fatalf("expected 1 new publish call (AssetEventReversed), got %d", pub.calls-callsBeforeReverse)
	}
}

// ── ReverseAssetEvent / SupersedeAssetEvent ──────────────────────────────────

func TestReverseAssetEvent_Disposal_RevertsAssetToActive(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{postJournalID: "jrnl-1"}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	id := createActiveAsset(t, s, r, "le-1")
	proceeds, amount := 500.0, 100.0
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeDisposal, domain.CreateAssetEventRequest{
		ProceedsAmount: &proceeds, Amount: &amount, DebitAccountCode: "DISPOSAL-CLEARING", CreditAccountCode: "FIXED-ASSETS",
	})
	walkToApproved(t, r, e.EventID)
	apply := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/apply", nil, "approver-1")
	if apply.Code != http.StatusOK {
		t.Fatalf("apply failed: %d %s", apply.Code, apply.Body.String())
	}
	if s.assets[id].Status != domain.AssetStatusDisposed {
		t.Fatalf("expected DISPOSED after apply, got %q", s.assets[id].Status)
	}

	rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/reverse", domain.ReverseAssetEventRequest{Reason: "posted in error"}, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("reverse failed: %d %s", rr.Code, rr.Body.String())
	}
	if s.assets[id].Status != domain.AssetStatusActive {
		t.Fatalf("expected asset reverted to ACTIVE after reversal, got %q", s.assets[id].Status)
	}
	if ledger.reverseCalls != 1 {
		t.Fatalf("expected exactly 1 journal reversal, got %d", ledger.reverseCalls)
	}
}

func TestReverseAssetEvent_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeAddition, domain.CreateAssetEventRequest{})

	rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/reverse", map[string]string{}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Negative path #2: applied events are immutable (structural, not just ────
// ── documented — see the handler's own writeAssetEventErr / store's guarded ─
// ── transitions: there is simply no HTTP path that edits a non-DRAFT event) ─

func TestValidateAssetEvent_AlreadyValidated_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeAddition, domain.CreateAssetEventRequest{})
	first := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/validate", nil, "preparer-1")
	if first.Code != http.StatusOK {
		t.Fatalf("first validate failed: %d %s", first.Code, first.Body.String())
	}
	second := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/validate", nil, "preparer-1")
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 re-validating an already-VALIDATED event, got %d: %s", second.Code, second.Body.String())
	}
}

// ── GetAssetBookStateAsOf / ExplainAssetState ────────────────────────────────

func TestGetAssetBookStateAsOf_MissingBookID_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")

	rr := doReq(r, http.MethodGet, "/v1/assets/"+id+"/book-state-as-of", nil, "reader-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetAssetBookStateAsOf_ReturnsSchedule(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	buildRR := doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(id), "preparer-1")
	if buildRR.Code != http.StatusCreated {
		t.Fatalf("build schedule failed: %d %s", buildRR.Code, buildRR.Body.String())
	}

	rr := doReq(r, http.MethodGet, "/v1/assets/"+id+"/book-state-as-of?book_id=book-1", nil, "reader-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var sch domain.DepreciationSchedule
	_ = json.NewDecoder(rr.Body).Decode(&sch)
	if sch.AssetID != id || sch.BookID != "book-1" {
		t.Fatalf("expected the schedule for this asset/book, got %+v", sch)
	}
}

func TestExplainAssetState_ReturnsAssetAndEvents(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	createDraftAssetEvent(t, r, id, domain.AssetEventTypeAddition, domain.CreateAssetEventRequest{})

	rr := doReq(r, http.MethodGet, "/v1/assets/"+id+"/explain-state", nil, "reader-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Asset  domain.FixedAsset   `json:"asset"`
		Events []domain.AssetEvent `json:"events"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if out.Asset.AssetID != id || len(out.Events) != 1 {
		t.Fatalf("expected 1 asset + 1 event, got %+v", out)
	}
}
