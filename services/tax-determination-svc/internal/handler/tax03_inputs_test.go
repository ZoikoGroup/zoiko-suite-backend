package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/tax-determination-svc/internal/domain"
	"zoiko.io/tax-determination-svc/internal/registry"
)

// Coverage for the TAX-03 required business/source inputs and the two
// server-resolved inputs added in migration 000003.

func baseTAX03Request() domain.DetermineTaxRequest {
	return domain.DetermineTaxRequest{
		TransactionID:  "tx-inv-2001",
		SourceModule:   "INVOICE",
		LegalEntityID:  "le-001",
		JurisdictionID: "uk-england",
		TaxCategory:    "VAT",
		GrossAmount:    1000,
		Currency:       "GBP",
		EffectiveFrom:  "2026-01-01",
		EvaluatedBy:    "billing-engine",

		SellerPartyID:         "party-seller-1",
		BuyerPartyID:          "party-buyer-1",
		SupplyJurisdictionID:  "uk-england",
		SupplyDate:            "2026-01-15",
		ProductClassification: "SOFTWARE_LICENCE",
		SupplyKind:            domain.SupplyKindDigitalServices,
		SupplyType:            domain.SupplyTypeB2C,
	}
}

func determine(h *Handler, req domain.DetermineTaxRequest) (int, domain.TaxDetermination, string) {
	w := httptest.NewRecorder()
	h.DetermineTax(w, buildRequest(http.MethodPost, "/v1/tax-determinations", req))

	var det domain.TaxDetermination
	raw := w.Body.String()
	_ = json.Unmarshal([]byte(raw), &det)
	return w.Code, det, raw
}

func TestTAX03_InputsArePreservedOnTheDetermination(t *testing.T) {
	req := baseTAX03Request()
	req.SellerEstablishmentID = "est-seller-gb"
	req.BuyerEstablishmentID = "est-buyer-ie"
	req.ShipFromJurisdictionID = "uk-england"
	req.ShipToJurisdictionID = "ie"
	req.SupplyType = domain.SupplyTypeB2B
	req.BuyerTaxRegistrationID = "IE1234567X"

	code, det, raw := determine(newTestHandler(), req)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", code, raw)
	}

	if det.SellerPartyID != "party-seller-1" || det.BuyerPartyID != "party-buyer-1" {
		t.Errorf("parties not preserved: %+v", det)
	}
	if det.SupplyJurisdictionID != "uk-england" {
		t.Errorf("supply_jurisdiction_id = %q", det.SupplyJurisdictionID)
	}
	if det.SupplyDate == nil || *det.SupplyDate != "2026-01-15" {
		t.Errorf("supply_date = %v, want 2026-01-15", det.SupplyDate)
	}
	if det.SupplyKind != domain.SupplyKindDigitalServices || det.SupplyType != domain.SupplyTypeB2B {
		t.Errorf("classification not preserved: kind=%q type=%q", det.SupplyKind, det.SupplyType)
	}
	if det.BuyerTaxRegistrationID == nil || *det.BuyerTaxRegistrationID != "IE1234567X" {
		t.Errorf("buyer_tax_registration_id = %v", det.BuyerTaxRegistrationID)
	}
	if det.ShipToJurisdictionID == nil || *det.ShipToJurisdictionID != "ie" {
		t.Errorf("ship_to_jurisdiction_id = %v", det.ShipToJurisdictionID)
	}
}

// The engine did not derive the place of supply — no pack carries
// place-of-supply rules — and the record has to say so rather than let a
// caller-supplied jurisdiction read as a determination.
func TestTAX03_PlaceOfSupplyIsRecordedAsCallerAsserted(t *testing.T) {
	code, det, raw := determine(newTestHandler(), baseTAX03Request())
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", code, raw)
	}
	if det.PlaceOfSupplyBasis != domain.PlaceOfSupplyCallerAsserted {
		t.Fatalf("place_of_supply_basis = %q, want CALLER_ASSERTED", det.PlaceOfSupplyBasis)
	}
}

func TestTAX03_EachRequiredInputIsRefusedByName(t *testing.T) {
	cases := []struct {
		field  string
		mutate func(*domain.DetermineTaxRequest)
	}{
		{"seller_party_id", func(r *domain.DetermineTaxRequest) { r.SellerPartyID = "" }},
		{"buyer_party_id", func(r *domain.DetermineTaxRequest) { r.BuyerPartyID = "" }},
		{"supply_jurisdiction_id", func(r *domain.DetermineTaxRequest) { r.SupplyJurisdictionID = "" }},
		{"supply_date", func(r *domain.DetermineTaxRequest) { r.SupplyDate = "" }},
		{"product_classification", func(r *domain.DetermineTaxRequest) { r.ProductClassification = "" }},
		{"supply_kind", func(r *domain.DetermineTaxRequest) { r.SupplyKind = "" }},
		{"supply_type", func(r *domain.DetermineTaxRequest) { r.SupplyType = "" }},
		{"currency", func(r *domain.DetermineTaxRequest) { r.Currency = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			req := baseTAX03Request()
			tc.mutate(&req)

			code, _, raw := determine(newTestHandler(), req)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", code)
			}
			// The refusal has to name the field, or a caller cannot fix it.
			if !strings.Contains(raw, tc.field) {
				t.Errorf("refusal does not name %s: %s", tc.field, raw)
			}
		})
	}
}

func TestTAX03_UnknownSupplyKindAndTypeAreRefused(t *testing.T) {
	req := baseTAX03Request()
	req.SupplyKind = "GOOD" // plausible typo for GOODS
	if code, _, _ := determine(newTestHandler(), req); code != http.StatusBadRequest {
		t.Errorf("bad supply_kind -> %d, want 400", code)
	}

	req = baseTAX03Request()
	req.SupplyType = "B2X"
	if code, _, _ := determine(newTestHandler(), req); code != http.StatusBadRequest {
		t.Errorf("bad supply_type -> %d, want 400", code)
	}
}

// UNSPECIFIED is the marker migration 000003 backfilled onto pre-contract rows.
func TestTAX03_UnspecifiedIsNotAcceptableOnANewDetermination(t *testing.T) {
	req := baseTAX03Request()
	req.SupplyKind = domain.SupplyKindUnspecified
	if code, _, _ := determine(newTestHandler(), req); code != http.StatusBadRequest {
		t.Errorf("UNSPECIFIED supply_kind -> %d, want 400", code)
	}

	req = baseTAX03Request()
	req.SupplyType = domain.SupplyTypeUnspecified
	if code, _, _ := determine(newTestHandler(), req); code != http.StatusBadRequest {
		t.Errorf("UNSPECIFIED supply_type -> %d, want 400", code)
	}
}

// The fact that decides reverse charge. A business buyer carrying no
// registration in the place of supply is a B2C supply in substance, and
// treating it as B2B is how a cross-border supply goes untaxed on both sides.
func TestTAX03_B2BRequiresBuyerRegistration(t *testing.T) {
	req := baseTAX03Request()
	req.SupplyType = domain.SupplyTypeB2B

	code, _, raw := determine(newTestHandler(), req)
	if code != http.StatusBadRequest {
		t.Fatalf("B2B with no buyer registration -> %d, want 400", code)
	}
	if !strings.Contains(raw, "buyer_tax_registration_id") {
		t.Errorf("refusal does not name the field: %s", raw)
	}

	req.BuyerTaxRegistrationID = "IE1234567X"
	if code, _, raw := determine(newTestHandler(), req); code != http.StatusCreated {
		t.Fatalf("B2B with buyer registration -> %d, want 201: %s", code, raw)
	}
}

// B2C and B2G do not need one — only B2B turns on the buyer's registration.
func TestTAX03_B2CAndB2GDoNotRequireBuyerRegistration(t *testing.T) {
	for _, st := range []domain.SupplyType{domain.SupplyTypeB2C, domain.SupplyTypeB2G} {
		req := baseTAX03Request()
		req.SupplyType = st
		if code, _, raw := determine(newTestHandler(), req); code != http.StatusCreated {
			t.Errorf("%s -> %d, want 201: %s", st, code, raw)
		}
	}
}

// INV-10: an exemption is the one number on a determination that reduces tax
// without a rule having said so, so it cannot stand on an amount alone.
func TestTAX03_ExemptAmountRequiresAReason(t *testing.T) {
	req := baseTAX03Request()
	req.ExemptAmount = 250

	code, _, raw := determine(newTestHandler(), req)
	if code != http.StatusBadRequest {
		t.Fatalf("exemption with no reason -> %d, want 400", code)
	}
	if !strings.Contains(raw, "exemption_reason") {
		t.Errorf("refusal does not name the field: %s", raw)
	}

	req.ExemptionReason = "Zero-rated export of services"
	req.ExemptionCertificateRef = "cert-2026-0042"
	code, det, raw := determine(newTestHandler(), req)
	if code != http.StatusCreated {
		t.Fatalf("exemption with a reason -> %d, want 201: %s", code, raw)
	}
	if det.ExemptionReason == nil || *det.ExemptionReason == "" {
		t.Error("exemption_reason was not preserved on the determination")
	}
	if det.ExemptionCertificateRef == nil || *det.ExemptionCertificateRef != "cert-2026-0042" {
		t.Errorf("exemption_certificate_ref = %v", det.ExemptionCertificateRef)
	}
}

func TestTAX03_ExemptAmountCannotExceedGross(t *testing.T) {
	req := baseTAX03Request()
	req.ExemptAmount = 2000 // gross is 1000
	req.ExemptionReason = "Zero-rated"

	if code, _, _ := determine(newTestHandler(), req); code != http.StatusBadRequest {
		t.Fatalf("exempt > gross -> %d, want 400", code)
	}
}

func TestTAX03_SupplyDateMustBeAnISODate(t *testing.T) {
	for _, bad := range []string{"15/01/2026", "2026-01-15T00:00:00Z", "Jan 15 2026", "2026-13-01"} {
		req := baseTAX03Request()
		req.SupplyDate = bad
		if code, _, _ := determine(newTestHandler(), req); code != http.StatusBadRequest {
			t.Errorf("supply_date %q -> %d, want 400", bad, code)
		}
	}
}

func TestTAX03_CurrencyMustBeThreeUppercaseLetters(t *testing.T) {
	for _, bad := range []string{"gbp", "GB", "GBPX", "G8P"} {
		req := baseTAX03Request()
		req.Currency = bad
		if code, _, _ := determine(newTestHandler(), req); code != http.StatusBadRequest {
			t.Errorf("currency %q -> %d, want 400", bad, code)
		}
	}
}

// ── server-resolved inputs ──────────────────────────────────────────────────

// Every jurisdiction the determination names is probed, not just the place of
// supply — a ship-to nobody recognises is a determination nobody can defend.
func TestTAX03_EveryNamedJurisdictionIsValidated(t *testing.T) {
	jv := &stubJurisdiction{}
	h := newTestHandlerWith(jv, &stubRegistry{})

	req := baseTAX03Request()
	req.ShipFromJurisdictionID = "uk-england"
	req.ShipToJurisdictionID = "ie"

	if code, _, raw := determine(h, req); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", code, raw)
	}
	for _, want := range []string{"uk-england", "ie"} {
		found := false
		for _, seen := range jv.seen {
			if seen == want {
				found = true
			}
		}
		if !found {
			t.Errorf("jurisdiction %q was never validated; probed %v", want, jv.seen)
		}
	}
}

func TestTAX03_UnknownJurisdictionIsARefusalNotAnOutage(t *testing.T) {
	h := newTestHandlerWith(
		&stubJurisdiction{err: fmt.Errorf("%w: xx-nowhere", domain.ErrJurisdictionUnknown)},
		&stubRegistry{},
	)
	if code, _, raw := determine(h, baseTAX03Request()); code != http.StatusBadRequest {
		t.Fatalf("unknown jurisdiction -> %d, want 400: %s", code, raw)
	}
}

// "Cannot answer" is never "no". An unreachable jurisdiction service must fail
// the determination closed rather than pin a tax position to an unvalidated
// jurisdiction — the contract jurisdiction-rules-svc set for the platform.
func TestTAX03_UnverifiableJurisdictionFailsClosed(t *testing.T) {
	h := newTestHandlerWith(
		&stubJurisdiction{err: fmt.Errorf("%w: dial tcp: refused", domain.ErrJurisdictionUnverifiable)},
		&stubRegistry{},
	)
	code, _, raw := determine(h, baseTAX03Request())
	if code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable jurisdiction service -> %d, want 503: %s", code, raw)
	}
}

// TAX-03's server-resolved "Registrations": whether the seller is registered in
// the place of supply is read from the registry, not accepted from the caller.
func TestTAX03_SellerRegistrationIsResolvedFromTheRegistry(t *testing.T) {
	h := newTestHandlerWith(&stubJurisdiction{}, &stubRegistry{
		reg: &registry.Registration{BundleID: "tib-gb-001", JurisdictionID: "uk-england", Status: "ACTIVE"},
	})

	code, det, raw := determine(h, baseTAX03Request())
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", code, raw)
	}
	if det.SellerRegistrationID == nil || *det.SellerRegistrationID != "tib-gb-001" {
		t.Errorf("seller_registration_id = %v, want tib-gb-001", det.SellerRegistrationID)
	}
	if det.SellerRegistrationStatus == nil || *det.SellerRegistrationStatus != "ACTIVE" {
		t.Errorf("seller_registration_status = %v, want ACTIVE", det.SellerRegistrationStatus)
	}
}

// An unregistered seller is a real state and the fact that decides whether tax
// is charged at all — recorded as absent, not treated as a failed lookup.
func TestTAX03_UnregisteredSellerIsAnAnswerNotAnError(t *testing.T) {
	h := newTestHandlerWith(&stubJurisdiction{}, &stubRegistry{reg: nil})

	code, det, raw := determine(h, baseTAX03Request())
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", code, raw)
	}
	if det.SellerRegistrationID != nil {
		t.Errorf("seller_registration_id = %v, want nil", det.SellerRegistrationID)
	}
}

// Being unable to ask is different from the answer being "not registered".
func TestTAX03_UnreachableRegistryFailsClosed(t *testing.T) {
	h := newTestHandlerWith(&stubJurisdiction{}, &stubRegistry{
		err: fmt.Errorf("%w: dial tcp: refused", domain.ErrRegistryUnavailable),
	})
	code, _, raw := determine(h, baseTAX03Request())
	if code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable registry -> %d, want 503: %s", code, raw)
	}
}
