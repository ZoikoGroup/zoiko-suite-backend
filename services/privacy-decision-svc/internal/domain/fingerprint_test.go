package domain

import (
	"reflect"
	"strings"
	"testing"
)

func fullRequest() EvaluateDecisionRequest {
	return EvaluateDecisionRequest{
		SubjectRef: "s", ProcessingActivityID: "a", ActivityVersionID: "av", PurposeID: "p", PurposeVersionID: "pv",
		ProposedOperation:  OperationUse,
		SubjectContext:     &SubjectContext{SubjectRef: "s", SubjectClass: "CUSTOMER", AgeBand: "ADULT", Residency: "GB", Jurisdiction: "GB", Relationship: "r"},
		DataContext:        &DataContext{DataCategories: []string{"CONTACT_INFO"}, DataCategory: "IDENTIFIERS", SensitivityFlags: []string{"HEALTH"}, Source: "DIRECT", Classification: "INTERNAL"},
		SecondaryPurposeID: "sp", ProposedSecondaryPurpose: "free",
		RecipientContext:           &RecipientContext{RecipientRef: "rr", RecipientType: "PROCESSOR", DestinationJurisdiction: "US", TransferMechanismID: "tm"},
		ConsentCheck:               &ConsentCheckRequest{Required: false, ReceiptID: "rc"},
		LegalHoldCheck:             &LegalHoldCheckRequest{RecordClass: "rcl", EntityRef: "er"},
		TransferCheck:              &TransferCheckRequest{RelationshipID: "rel", TransferMechanismID: "tmech", DestinationJurisdiction: "US", AssessmentCheck: false},
		DeidentificationControlRef: "dc",
	}
}

func TestFingerprint_FormatAndDeterminism(t *testing.T) {
	r := fullRequest()
	a := ComputeInputFingerprint(&r, "t1")
	if !strings.HasPrefix(a, "v2:") || len(a) != 3+64 {
		t.Fatalf("unexpected format %q", a)
	}
	if b := ComputeInputFingerprint(&r, "t1"); a != b {
		t.Fatal("not deterministic")
	}
	if ComputeInputFingerprint(&r, "t2") == a {
		t.Fatal("tenant must be part of the fingerprint")
	}
}

// Every decision-affecting input — including the ones the original scheme
// omitted — must change the fingerprint.
func TestFingerprint_EveryInputChangesIt(t *testing.T) {
	base := fullRequest()
	want := ComputeInputFingerprint(&base, "t")
	mutations := map[string]func(r *EvaluateDecisionRequest){
		"subject_ref":                func(r *EvaluateDecisionRequest) { r.SubjectRef = "x" },
		"processing_activity_id":     func(r *EvaluateDecisionRequest) { r.ProcessingActivityID = "x" },
		"activity_version_id":        func(r *EvaluateDecisionRequest) { r.ActivityVersionID = "x" },
		"purpose_id":                 func(r *EvaluateDecisionRequest) { r.PurposeID = "x" },
		"purpose_version_id":         func(r *EvaluateDecisionRequest) { r.PurposeVersionID = "x" },
		"proposed_operation":         func(r *EvaluateDecisionRequest) { r.ProposedOperation = OperationExport },
		"secondary_purpose_id":       func(r *EvaluateDecisionRequest) { r.SecondaryPurposeID = "x" },
		"proposed_secondary_purpose": func(r *EvaluateDecisionRequest) { r.ProposedSecondaryPurpose = "x" },
		"subject.subject_ref":        func(r *EvaluateDecisionRequest) { r.SubjectContext.SubjectRef = "x" },
		"subject.subject_class":      func(r *EvaluateDecisionRequest) { r.SubjectContext.SubjectClass = "VENDOR" },
		"subject.age_band":           func(r *EvaluateDecisionRequest) { r.SubjectContext.AgeBand = "MINOR" },
		"subject.residency":          func(r *EvaluateDecisionRequest) { r.SubjectContext.Residency = "FR" },
		"subject.jurisdiction":       func(r *EvaluateDecisionRequest) { r.SubjectContext.Jurisdiction = "FR" },
		"subject.relationship":       func(r *EvaluateDecisionRequest) { r.SubjectContext.Relationship = "x" },
		"subject_context nil":        func(r *EvaluateDecisionRequest) { r.SubjectContext = nil },
		"data.categories":            func(r *EvaluateDecisionRequest) { r.DataContext.DataCategories = []string{"FINANCIAL"} },
		"data.category":              func(r *EvaluateDecisionRequest) { r.DataContext.DataCategory = "LOCATION" },
		"data.flags":                 func(r *EvaluateDecisionRequest) { r.DataContext.SensitivityFlags = nil },
		"data.source":                func(r *EvaluateDecisionRequest) { r.DataContext.Source = "DERIVED" },
		"data.classification":        func(r *EvaluateDecisionRequest) { r.DataContext.Classification = "RESTRICTED" },
		"data_context nil":           func(r *EvaluateDecisionRequest) { r.DataContext = nil },
		"recipient.ref":              func(r *EvaluateDecisionRequest) { r.RecipientContext.RecipientRef = "x" },
		"recipient.type":             func(r *EvaluateDecisionRequest) { r.RecipientContext.RecipientType = "INTERNAL" },
		"recipient.destination":      func(r *EvaluateDecisionRequest) { r.RecipientContext.DestinationJurisdiction = "IN" },
		"recipient.mechanism":        func(r *EvaluateDecisionRequest) { r.RecipientContext.TransferMechanismID = "x" },
		"consent_check.required":     func(r *EvaluateDecisionRequest) { r.ConsentCheck.Required = true },
		"consent_check.receipt_id":   func(r *EvaluateDecisionRequest) { r.ConsentCheck.ReceiptID = "x" },
		"consent_check nil":          func(r *EvaluateDecisionRequest) { r.ConsentCheck = nil },
		"legal_hold.record_class":    func(r *EvaluateDecisionRequest) { r.LegalHoldCheck.RecordClass = "x" },
		"legal_hold.entity_ref":      func(r *EvaluateDecisionRequest) { r.LegalHoldCheck.EntityRef = "x" },
		"legal_hold_check nil":       func(r *EvaluateDecisionRequest) { r.LegalHoldCheck = nil },
		"transfer.relationship":      func(r *EvaluateDecisionRequest) { r.TransferCheck.RelationshipID = "x" },
		"transfer.mechanism":         func(r *EvaluateDecisionRequest) { r.TransferCheck.TransferMechanismID = "x" },
		"transfer.destination":       func(r *EvaluateDecisionRequest) { r.TransferCheck.DestinationJurisdiction = "IN" },
		"transfer.assessment":        func(r *EvaluateDecisionRequest) { r.TransferCheck.AssessmentCheck = true },
		"transfer_check nil":         func(r *EvaluateDecisionRequest) { r.TransferCheck = nil },
		"deidentification_control":   func(r *EvaluateDecisionRequest) { r.DeidentificationControlRef = "x" },
	}
	for name, mutate := range mutations {
		r := fullRequest()
		mutate(&r)
		if got := ComputeInputFingerprint(&r, "t"); got == want {
			t.Errorf("changing %s did not change the fingerprint", name)
		}
	}
}

func TestFingerprint_NormalizationIsStable(t *testing.T) {
	a := fullRequest()
	b := fullRequest()
	b.DataContext.DataCategories = []string{" contact_info ", "CONTACT_INFO"}
	b.DataContext.SensitivityFlags = []string{"health", "HEALTH"}
	b.SubjectContext.SubjectClass = " customer "
	b.ProposedOperation = "use"
	if ComputeInputFingerprint(&a, "t") != ComputeInputFingerprint(&b, "t") {
		t.Fatal("case/whitespace/duplicate/order differences must not change the fingerprint")
	}
	c := fullRequest()
	d := fullRequest()
	c.DataContext.DataCategories = []string{"B", "A"}
	d.DataContext.DataCategories = []string{"A", "B"}
	if ComputeInputFingerprint(&c, "t") != ComputeInputFingerprint(&d, "t") {
		t.Fatal("category order must not matter")
	}
}

// Guard: a new EvaluateDecisionRequest field must be consciously covered by
// the fingerprint (or explicitly excluded here).
func TestFingerprint_CoversEveryRequestField(t *testing.T) {
	excluded := map[string]string{"TenantID": "passed separately and verified against X-Tenant-Id"}
	typ := reflect.TypeOf(EvaluateDecisionRequest{})
	r := fullRequest()
	want := ComputeInputFingerprint(&r, "t")
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if _, ok := excluded[f.Name]; ok {
			continue
		}
		r2 := fullRequest()
		fv := reflect.ValueOf(&r2).Elem().Field(i)
		switch fv.Kind() {
		case reflect.String:
			fv.SetString("mutated-" + f.Name)
		case reflect.Ptr:
			fv.Set(reflect.Zero(fv.Type()))
		default:
			t.Fatalf("field %s has unhandled kind %s: extend the fingerprint and this test", f.Name, fv.Kind())
		}
		if ComputeInputFingerprint(&r2, "t") == want {
			t.Errorf("request field %s is not covered by the input fingerprint", f.Name)
		}
	}
}

func TestTaxonomy(t *testing.T) {
	if !IsSensitiveCategory("health") || IsSensitiveCategory("CONTACT_INFO") {
		t.Fatal("sensitive taxonomy wrong")
	}
	if IsKnownCategory("MYSTERY") || !IsKnownCategory("CONTACT_INFO") || !IsKnownCategory("BIOMETRIC") {
		t.Fatal("known-category check wrong")
	}
	if !IsValidSensitivityFlag("SENSITIVE") || !IsValidSensitivityFlag("health") || IsValidSensitivityFlag("NOPE") {
		t.Fatal("flag validation wrong")
	}
	if !IsValidAgeBand("minor") || IsValidAgeBand("TEEN") || !IsValidClassification("restricted") || IsValidClassification("X") {
		t.Fatal("enum validation wrong")
	}
}
