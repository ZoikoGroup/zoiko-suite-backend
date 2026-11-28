package signatory

import (
	"errors"
	"strings"
	"testing"

	"zoiko.io/identity-envelope/govdecision"
)

// The Doc 01 §5.2 example matrix (illustrative test fixture, not a live
// delegation of authority).
const gbpMatrix = `{
  "currency": "GBP",
  "valuation_basis": "GROSS",
  "instrument_types": ["SUPPLIER_CONTRACT", "PURCHASE_ORDER", "PAYMENT_INSTRUCTION", "NDA"],
  "legal_review_risk_flags": ["NONSTANDARD_DATA_PROCESSING"],
  "bands": [
    {"band_id": "B3", "above": "100000", "up_to": null, "min_distinct_signers": 2,
     "required_offices": [{"office": "CFO", "min_count": 1}, {"office": "CEO", "min_count": 1}],
     "sequencing": "SEQUENTIAL", "order": ["CFO", "CEO"], "separate_release_authorization": true},
    {"band_id": "B1", "above": null, "up_to": "10000", "min_distinct_signers": 1,
     "required_offices": [{"office": "FINANCE_SIGNER", "min_count": 1}]},
    {"band_id": "B2", "above": "10000", "up_to": "100000", "min_distinct_signers": 2,
     "required_offices": [{"office": "DIRECTOR", "min_count": 1}],
     "eligible_offices": ["FINANCE_SIGNER", "DIRECTOR", "CFO", "CEO"], "distinct_departments": true}
  ],
  "exemptions": [{"exemption_id": "E-NDA", "instrument_type": "NDA", "reason": "mutual NDA on the published template"}]
}`

const hash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func mustMatrix(t *testing.T, payload string) *Matrix {
	t.Helper()
	m, err := ParseMatrix([]byte(payload))
	if err != nil {
		t.Fatalf("matrix refused: %v", err)
	}
	return m
}

func resolve(t *testing.T, m *Matrix, input string) Result {
	t.Helper()
	in, err := ParseInput([]byte(input))
	if err != nil {
		t.Fatalf("input refused: %v", err)
	}
	return Evaluate("pv-1", m, in)
}

func quorum(t *testing.T, r Result) govdecision.RequiredAction {
	t.Helper()
	for _, a := range r.RequiredActions {
		if a.Kind == govdecision.ActionKindSignatureQuorum {
			return a
		}
	}
	t.Fatalf("no signature quorum in %+v", r)
	return govdecision.RequiredAction{}
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// P0-05: 11,000 needs two distinct persons; 100,000 and 100,000.01 resolve to
// different bands; exactly 10,000 stays in the first band.
func TestEvaluate_BandBoundariesAreExact(t *testing.T) {
	m := mustMatrix(t, gbpMatrix)
	cases := []struct {
		amount  string
		band    string
		signers int
	}{
		{"0", "B1", 1},
		{"10000", "B1", 1},
		{"10000.00", "B1", 1},
		{"10000.01", "B2", 2},
		{"11000", "B2", 2},
		{"100000", "B2", 2},
		{"100000.00", "B2", 2},
		{"100000.01", "B3", 2},
		{"5000000", "B3", 2},
	}
	for _, c := range cases {
		r := resolve(t, m, `{"instrument_type":"SUPPLIER_CONTRACT","amount":"`+c.amount+`","currency":"GBP","basis":"GROSS","document_hash":"`+hash+`"}`)
		if r.Outcome != govdecision.SignatureRequired || r.Detail.BandID != c.band {
			t.Fatalf("%s: outcome %s band %s, want SIGNATURE_REQUIRED %s", c.amount, r.Outcome, r.Detail.BandID, c.band)
		}
		q := quorum(t, r)
		if q.MinDistinctSigners != c.signers || q.DocumentHash != hash {
			t.Fatalf("%s: quorum %+v", c.amount, q)
		}
		for _, inv := range []string{govdecision.ConstraintDistinctHumans, govdecision.ConstraintPreparerExcluded, govdecision.ConstraintAuthorityAtSigning, govdecision.ConstraintDocumentHashBound} {
			if !has(q.Constraints, inv) {
				t.Fatalf("%s: quorum lacks invariant %s", c.amount, inv)
			}
		}
		if r.ReasonCodes[0] != govdecision.ReasonSignatureQuorumIncomplete {
			t.Fatalf("%s: reasons %v", c.amount, r.ReasonCodes)
		}
	}
}

// Example G-02: a GBP 120,000 payment needs CFO and CEO in order, and a
// separate release authorization: a signature is not a bank mandate.
func TestEvaluate_PaymentNeedsCFOAndCEOAndSeparateRelease(t *testing.T) {
	r := resolve(t, mustMatrix(t, gbpMatrix), `{"instrument_type":"PAYMENT_INSTRUCTION","amount":"120000.00","currency":"GBP","basis":"GROSS"}`)
	q := quorum(t, r)
	if q.Sequencing != SequencingSequential || strings.Join(q.Order, ",") != "CFO,CEO" || len(q.RequiredOffices) != 2 {
		t.Fatalf("quorum %+v", q)
	}
	var release bool
	for _, a := range r.RequiredActions {
		if a.Kind == govdecision.ActionKindReleaseAuthorization && has(a.Constraints, govdecision.ConstraintNotProxyForBankMandate) {
			release = true
		}
	}
	if !release || !has(r.ReasonCodes, govdecision.ReasonSignatureReleaseMandate) {
		t.Fatalf("payment release authorization missing: %+v", r)
	}
}

// Example G-01: USD bands, a Director in band 2, legal review on nonstandard
// data-processing terms.
func TestEvaluate_USDContractWithLegalReview(t *testing.T) {
	usd := strings.ReplaceAll(gbpMatrix, `"currency": "GBP"`, `"currency": "USD"`)
	r := resolve(t, mustMatrix(t, usd), `{"instrument_type":"SUPPLIER_CONTRACT","amount":"75000","currency":"USD","basis":"GROSS","risk_flags":["NONSTANDARD_DATA_PROCESSING"]}`)
	q := quorum(t, r)
	if q.MinDistinctSigners != 2 || q.RequiredOffices[0].Office != "DIRECTOR" || !has(q.Constraints, govdecision.ConstraintDistinctDepartments) {
		t.Fatalf("quorum %+v", q)
	}
	if !has(r.ReasonCodes, govdecision.ReasonSignatureLegalReview) {
		t.Fatalf("legal review missing: %v", r.ReasonCodes)
	}
}

func TestEvaluate_NotConfiguredNeverPermits(t *testing.T) {
	m := mustMatrix(t, gbpMatrix)
	cases := map[string]struct{ input, reason string }{
		"other currency, no valuation": {`{"instrument_type":"SUPPLIER_CONTRACT","amount":"5000","currency":"USD","basis":"GROSS"}`, govdecision.ReasonConfigCurrencyUnsupported},
		"basis mismatch":               {`{"instrument_type":"SUPPLIER_CONTRACT","amount":"5000","currency":"GBP","basis":"NET"}`, govdecision.ReasonConfigValuationBasisMismatch},
		"instrument not governed":      {`{"instrument_type":"EMPLOYMENT_AGREEMENT","amount":"5000","currency":"GBP","basis":"GROSS"}`, govdecision.ReasonConfigPolicyMissing},
		"high-risk below every band":   {`{"instrument_type":"GUARANTEE","amount":"1","currency":"GBP","basis":"GROSS"}`, govdecision.ReasonConfigInstrumentMatrixMissing},
		"related party flag":           {`{"instrument_type":"SUPPLIER_CONTRACT","amount":"1","currency":"GBP","basis":"GROSS","risk_flags":["RELATED_PARTY"]}`, govdecision.ReasonConfigInstrumentMatrixMissing},
		"series in another currency":   {`{"instrument_type":"SUPPLIER_CONTRACT","amount":"1","currency":"GBP","basis":"GROSS","related_commitments":[{"resource_id":"C-0","amount":"5","currency":"EUR"}]}`, govdecision.ReasonConfigCurrencyUnsupported},
	}
	for name, c := range cases {
		r := resolve(t, m, c.input)
		if r.Outcome != govdecision.NotConfigured || !has(r.ReasonCodes, c.reason) || len(r.RequiredActions) != 0 {
			t.Fatalf("%s: %+v, want NOT_CONFIGURED %s", name, r, c.reason)
		}
	}
}

// Threshold evasion: amendments of one series are aggregated upward.
func TestEvaluate_SeriesAggregatesUpward(t *testing.T) {
	m := mustMatrix(t, gbpMatrix)
	r := resolve(t, m, `{"instrument_type":"SUPPLIER_CONTRACT","amount":"9000","currency":"GBP","basis":"GROSS",
	  "related_commitments":[{"resource_id":"C-1","amount":"6000","currency":"GBP"},{"resource_id":"C-2","amount":"90000","currency":"GBP"}]}`)
	if r.Detail.BandID != "B3" || r.Detail.ComparedAmount != "105000.00" || !has(r.ReasonCodes, govdecision.ReasonSignatureSeriesAggregated) {
		t.Fatalf("%+v", r)
	}
	if !has(r.UpstreamRefs, "series:C-2") {
		t.Fatalf("series refs %v", r.UpstreamRefs)
	}
	// Options count toward the committed value.
	r = resolve(t, m, `{"instrument_type":"SUPPLIER_CONTRACT","amount":"9000","options_amount":"1000.01","currency":"GBP","basis":"GROSS"}`)
	if r.Detail.BandID != "B2" {
		t.Fatalf("options not counted: %+v", r.Detail)
	}
}

// A conversion by the FX owner is used; its snapshot is an upstream reference.
func TestEvaluate_ConvertedByOwnerIsUsed(t *testing.T) {
	r := resolve(t, mustMatrix(t, gbpMatrix), `{"instrument_type":"SUPPLIER_CONTRACT","amount":"75000","currency":"USD","basis":"GROSS",
	  "converted":{"amount":"59000.00","currency":"GBP","fx_snapshot_ref":"spend-fx-881"}}`)
	if r.Outcome != govdecision.SignatureRequired || r.Detail.BandID != "B2" || !has(r.UpstreamRefs, "fx:spend-fx-881") {
		t.Fatalf("%+v", r)
	}
}

func TestEvaluate_OnlyAnExplicitExemptionPermits(t *testing.T) {
	r := resolve(t, mustMatrix(t, gbpMatrix), `{"instrument_type":"NDA","amount":"0","currency":"GBP","basis":"GROSS"}`)
	if r.Outcome != govdecision.Permit || r.ReasonCodes[0] != govdecision.ReasonSignatureExempt || r.Detail.ExemptionID != "E-NDA" {
		t.Fatalf("%+v", r)
	}
	// Zero amount on a governed instrument is never PERMIT.
	r = resolve(t, mustMatrix(t, gbpMatrix), `{"instrument_type":"SUPPLIER_CONTRACT","amount":"0","currency":"GBP","basis":"GROSS"}`)
	if r.Outcome != govdecision.SignatureRequired {
		t.Fatalf("zero-value contract: %+v", r)
	}
}

func TestEvaluate_HighRiskMatrixGovernsHighRiskInstrument(t *testing.T) {
	m := mustMatrix(t, `{"currency":"GBP","valuation_basis":"GROSS","high_risk":true,"instrument_types":["GUARANTEE","SUPPLIER_CONTRACT"],
	  "bands":[{"band_id":"H1","above":null,"up_to":null,"min_distinct_signers":2,"required_offices":[{"office":"DIRECTOR","min_count":2}],"legal_review":true}]}`)
	r := resolve(t, m, `{"instrument_type":"GUARANTEE","amount":"1","currency":"GBP","basis":"GROSS"}`)
	if r.Outcome != govdecision.SignatureRequired || !has(r.ReasonCodes, govdecision.ReasonSignatureHighRiskInstrument) || !has(r.ReasonCodes, govdecision.ReasonSignatureLegalReview) {
		t.Fatalf("%+v", r)
	}
	if !m.Covers("SUPPLIER_CONTRACT", true) || mustMatrix(t, gbpMatrix).Covers("SUPPLIER_CONTRACT", true) {
		t.Fatal("only a high-risk matrix covers a high-risk request")
	}
}

func TestParseMatrix_RefusesUnsafeMatrices(t *testing.T) {
	band := func(id, above, upTo string, signers int) string {
		q := func(s string) string {
			if s == "" {
				return "null"
			}
			return `"` + s + `"`
		}
		return `{"band_id":"` + id + `","above":` + q(above) + `,"up_to":` + q(upTo) + `,"min_distinct_signers":` + string(rune('0'+signers)) + `,"required_offices":[],"eligible_offices":["DIRECTOR"]}`
	}
	wrap := func(extra, bands string) string {
		return `{"currency":"GBP","valuation_basis":"GROSS","instrument_types":["SUPPLIER_CONTRACT"]` + extra + `,"bands":[` + bands + `]}`
	}
	cases := map[string]string{
		"overlap":              wrap("", band("A", "", "100", 1)+","+band("B", "50", "", 2)),
		"gap":                  wrap("", band("A", "", "100", 1)+","+band("B", "150", "", 2)),
		"two from zero":        wrap("", band("A", "", "100", 1)+","+band("B", "", "", 2)),
		"unbounded middle":     wrap("", band("A", "", "", 1)+","+band("B", "100", "", 2)),
		"not from zero":        wrap("", band("A", "10", "", 1)),
		"fewer signers above":  wrap("", band("A", "", "100", 2)+","+band("B", "100", "", 1)),
		"empty bands":          wrap("", ""),
		"high-risk instrument": `{"currency":"GBP","valuation_basis":"GROSS","instrument_types":["GUARANTEE"],"bands":[` + band("A", "", "", 1) + `]}`,
		"no basis":             `{"currency":"GBP","instrument_types":["X1"],"bands":[` + band("A", "", "", 1) + `]}`,
		"bad currency":         `{"currency":"gbp","valuation_basis":"GROSS","instrument_types":["X1"],"bands":[` + band("A", "", "", 1) + `]}`,
		"unknown field":        wrap(`,"max_amount":"5"`, band("A", "", "", 1)),
		"float money":          wrap("", `{"band_id":"A","above":null,"up_to":1e5,"min_distinct_signers":1,"eligible_offices":["X1"]}`),
		"offices exceed seats": wrap("", `{"band_id":"A","above":null,"up_to":null,"min_distinct_signers":1,"required_offices":[{"office":"CFO","min_count":1},{"office":"CEO","min_count":1}]}`),
		"unfilled seat":        wrap("", `{"band_id":"A","above":null,"up_to":null,"min_distinct_signers":2,"required_offices":[{"office":"CFO","min_count":1}]}`),
		"sequential no order":  wrap("", `{"band_id":"A","above":null,"up_to":null,"min_distinct_signers":1,"required_offices":[{"office":"CFO","min_count":1}],"sequencing":"SEQUENTIAL"}`),
		"exempt high-risk":     `{"currency":"GBP","valuation_basis":"GROSS","high_risk":true,"instrument_types":["GUARANTEE"],"bands":[` + band("A", "", "", 1) + `],"exemptions":[{"exemption_id":"E","instrument_type":"GUARANTEE","reason":"x"}]}`,
		"ttl too long":         wrap(`,"decision_ttl_seconds":999999`, band("A", "", "", 1)),
	}
	for name, payload := range cases {
		if _, err := ParseMatrix([]byte(payload)); !errors.Is(err, ErrInvalidMatrix) {
			t.Errorf("%s: accepted (err %v)", name, err)
		}
	}
	if _, err := ParseMatrix([]byte(wrap("", band("A", "", "100", 1)+","+band("B", "100", "", 2)))); err != nil {
		t.Fatalf("contiguous bands refused: %v", err)
	}
}

func TestParseInput_RefusesBadRequests(t *testing.T) {
	cases := map[string]string{
		"empty":            ``,
		"no instrument":    `{"amount":"1","currency":"GBP","basis":"GROSS"}`,
		"negative":         `{"instrument_type":"NDA","amount":"-1","currency":"GBP","basis":"GROSS"}`,
		"exponent":         `{"instrument_type":"NDA","amount":"1e5","currency":"GBP","basis":"GROSS"}`,
		"no basis":         `{"instrument_type":"NDA","amount":"1","currency":"GBP"}`,
		"bad hash":         `{"instrument_type":"NDA","amount":"1","currency":"GBP","basis":"GROSS","document_hash":"md5:1"}`,
		"fx without ref":   `{"instrument_type":"NDA","amount":"1","currency":"USD","basis":"GROSS","converted":{"amount":"1","currency":"GBP"}}`,
		"fx plus options":  `{"instrument_type":"NDA","amount":"1","currency":"USD","basis":"GROSS","options_amount":"1","converted":{"amount":"1","currency":"GBP","fx_snapshot_ref":"x"}}`,
		"series no id":     `{"instrument_type":"NDA","amount":"1","currency":"GBP","basis":"GROSS","related_commitments":[{"amount":"1","currency":"GBP"}]}`,
		"lower-case flags": `{"instrument_type":"NDA","amount":"1","currency":"GBP","basis":"GROSS","risk_flags":["x"]}`,
	}
	for name, input := range cases {
		var in *InputError
		if _, err := ParseInput([]byte(input)); !errors.As(err, &in) {
			t.Errorf("%s: accepted (err %v)", name, err)
		}
	}
}

func TestCovers_RiskRegimesNeverCompete(t *testing.T) {
	hr := mustMatrix(t, `{"currency":"GBP","valuation_basis":"GROSS","high_risk":true,"instrument_types":["SUPPLIER_CONTRACT"],"bands":[{"band_id":"H","above":null,"up_to":null,"min_distinct_signers":2,"required_offices":[{"office":"DIRECTOR","min_count":2}]}]}`)
	ord := mustMatrix(t, gbpMatrix)
	if hr.Covers("SUPPLIER_CONTRACT", false) || !hr.Covers("SUPPLIER_CONTRACT", true) || ord.Covers("SUPPLIER_CONTRACT", true) || !ord.Covers("SUPPLIER_CONTRACT", false) {
		t.Fatal("each matrix governs only its own risk regime")
	}
	if _, overlap := hr.Overlaps(ord); overlap {
		t.Fatal("a high-risk and an ordinary matrix never overlap")
	}
	if i, overlap := ord.Overlaps(mustMatrix(t, gbpMatrix)); !overlap || i == "" {
		t.Fatal("two ordinary matrices for the same instrument overlap")
	}
	r := resolve(t, hr, `{"instrument_type":"SUPPLIER_CONTRACT","amount":"1","currency":"GBP","basis":"GROSS"}`)
	if r.Outcome != govdecision.NotConfigured || r.ReasonCodes[0] != govdecision.ReasonConfigPolicyMissing {
		t.Fatalf("a high-risk matrix must not govern an ordinary request: %+v", r)
	}
}
