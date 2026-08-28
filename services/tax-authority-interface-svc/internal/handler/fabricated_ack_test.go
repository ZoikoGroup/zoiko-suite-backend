package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/tax-authority-interface-svc/internal/domain"
)

// TestSubmitTaxFiling_DoesNotFabricateAcknowledgement is the regression test
// for a defect distinct from anything else fixed in this codebase: not a
// missing tenant check or a missing authorization check, but the service
// reporting a false real-world fact.
//
// SubmitTaxFiling used to hardcode AckReference: "TAX-ACK-991823" on every
// filing and mark it SUBMITTED, regardless of whether anything was actually
// transmitted to a tax authority — because nothing transmits anything: this
// service has no outbound client to any authority endpoint anywhere in its
// codebase. Every filing received the identical fake acknowledgement.
//
// ZS-SVC-F-001 (Tax/E-Invoicing/Regulatory Compliance) names this exact
// anti-pattern: "Transport success is not filing acceptance" / "Authority
// rejection hidden as submitted." This test asserts the fix: the filing is
// recorded honestly as PENDING with no acknowledgement, and the event
// published describes what actually happened (recorded) rather than
// asserting something that did not (submitted to the authority).
func TestSubmitTaxFiling_DoesNotFabricateAcknowledgement(t *testing.T) {
	r, pub := setupTestRouter()

	// Register an interface first (required before a filing can reference it).
	ifaceBody, _ := json.Marshal(domain.CreateInterfaceRequest{
		LegalEntityID: "le-101", Jurisdiction: "GB",
		AuthorityName: "HMRC MTD UK", Protocol: "REST/OAuth2",
	})
	ifaceReq := httptest.NewRequest("POST", "/v1/tax-authority/interfaces", bytes.NewReader(ifaceBody))
	ifaceReq.Header.Set("Content-Type", "application/json")
	ifaceReq.Header.Set("X-Tenant-ID", "tenant-test")
	ifaceReq.Header.Set("X-Principal-Id", "user-1")
	ifaceW := httptest.NewRecorder()
	r.ServeHTTP(ifaceW, ifaceReq)
	if ifaceW.Code != http.StatusCreated {
		t.Fatalf("create interface: expected 201, got %d: %s", ifaceW.Code, ifaceW.Body.String())
	}
	var iface domain.TaxInterface
	if err := json.Unmarshal(ifaceW.Body.Bytes(), &iface); err != nil {
		t.Fatalf("decode interface: %v", err)
	}

	filingBody, _ := json.Marshal(domain.SubmitTaxFilingRequest{
		InterfaceID: iface.InterfaceID, TaxPeriod: "2026-Q1", FilingType: "VAT_RETURN", TaxAmount: 15450.50,
	})
	req := httptest.NewRequest("POST", "/v1/tax-authority/filings", bytes.NewReader(filingBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-test")
	req.Header.Set("X-Principal-Id", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (recorded, not transmitted), got %d: %s", w.Code, w.Body.String())
	}

	var sub domain.TaxFilingSubmission
	if err := json.Unmarshal(w.Body.Bytes(), &sub); err != nil {
		t.Fatalf("decode submission: %v", err)
	}

	if sub.Status != domain.TaxFilingPending {
		t.Fatalf("FABRICATION: expected status PENDING (honest — nothing was transmitted), got %q", sub.Status)
	}
	if sub.AckReference != "" {
		t.Fatalf("FABRICATION: expected no acknowledgement reference, got %q — no tax authority was ever contacted", sub.AckReference)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("TAX-ACK")) {
		t.Fatalf("FABRICATION: response still contains a fabricated acknowledgement reference: %s", w.Body.String())
	}

	// The published event must describe what happened (recorded), not what
	// didn't (submitted to the authority) — downstream consumers trust this
	// service's events as fact. Interface registration above published its
	// own event first, so check the last one.
	if len(pub.Events) < 2 {
		t.Fatalf("expected at least 2 published events (interface + filing), got %d", len(pub.Events))
	}
	lastEvent := pub.Events[len(pub.Events)-1]
	if lastEvent.EventType != "tax.filing.recorded" {
		t.Fatalf("FABRICATION: event type is %q — must not be tax.filing.submitted, since nothing was submitted",
			lastEvent.EventType)
	}
}
