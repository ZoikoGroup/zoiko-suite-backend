package handler_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"zoiko.io/asset-management-svc/internal/domain"
)

// PreflightApplyAssetEvent mirrors the stub's ApplyAssetEvent checks without
// mutating anything — the real store does the same via a rolled-back tx.
func (s *stubStore) PreflightApplyAssetEvent(_ context.Context, eventID string, _ time.Time) error {
	e, ok := s.assetEvents[eventID]
	if !ok || e.Status != domain.AssetEventStatusApproved {
		return domain.ErrInvalidAssetEventTransition
	}
	if e.EventType == domain.AssetEventTypeDisposal {
		asset, ok := s.assets[e.AssetID]
		if !ok || (asset.Status != domain.AssetStatusActive && asset.Status != domain.AssetStatusSuspended) {
			return domain.ErrAssetNotEligibleForDisposal
		}
	}
	return nil
}

// A store-side refusal must surface BEFORE the GL journal is posted: otherwise
// a posted journal is left behind while the event stays APPROVED.
func TestApplyAssetEvent_StoreSideRefusal_NeverPostsJournal(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{postJournalID: "jrnl-x"}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	id := createActiveAsset(t, s, r, "le-1")

	first := createDraftAssetEvent(t, r, id, domain.AssetEventTypeDisposal, domain.CreateAssetEventRequest{})
	walkToApproved(t, r, first.EventID)
	if rr := doReq(r, http.MethodPost, "/v1/asset-events/"+first.EventID+"/apply", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("first disposal failed: %d %s", rr.Code, rr.Body.String())
	}
	callsAfterFirst := ledger.postCalls

	amount := 400.0
	effDate := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	second := createDraftAssetEvent(t, r, id, domain.AssetEventTypeDisposal, domain.CreateAssetEventRequest{
		Amount: &amount, Currency: "EUR", EffectiveDate: &effDate,
		DebitAccountCode: "LOSS-ON-DISPOSAL", CreditAccountCode: "FIXED-ASSETS",
	})
	walkToApproved(t, r, second.EventID)

	rr := doReq(r, http.MethodPost, "/v1/asset-events/"+second.EventID+"/apply", nil, "approver-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 disposing an already-DISPOSED asset, got %d: %s", rr.Code, rr.Body.String())
	}
	if ledger.postCalls != callsAfterFirst {
		t.Fatalf("a refused apply must not post a journal; posts went %d -> %d", callsAfterFirst, ledger.postCalls)
	}
	if s.assetEvents[second.EventID].Status != domain.AssetEventStatusApproved {
		t.Fatalf("expected the refused event to stay APPROVED, got %q", s.assetEvents[second.EventID].Status)
	}
}
