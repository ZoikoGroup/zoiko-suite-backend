package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/handler"
	"zoiko.io/privacy-decision-svc/internal/middleware"
	"zoiko.io/privacy-decision-svc/internal/purposeregistry"
	"zoiko.io/privacy-decision-svc/internal/retentionregistry"
	"zoiko.io/privacy-decision-svc/internal/transferregistry"
)

// ── harness ──────────────────────────────────────────────────────────────────

type env struct {
	st        *stubStore
	purposes  *stubPurposeRegistry
	consents  *stubConsentRegistry
	holds     *stubHoldRegistry
	transfers *stubTransferRegistry
	activity  *purposeregistry.ActivityVersion
	h         http.Handler
}

func newEnv() *env {
	e := &env{
		st: newStubStore(), purposes: newStubPurposeRegistry(),
		consents: &stubConsentRegistry{status: "GRANTED", consentReceiptID: "rcpt-1", noticeVersionID: "nv-1"},
		holds:    &stubHoldRegistry{}, transfers: &stubTransferRegistry{},
		activity: activeActivity(),
	}
	e.purposes.activities[testActivity] = e.activity
	e.purposes.purposes[testPurpose] = publishedPurpose()
	e.rebuild()
	return e
}

func (e *env) rebuild() { e.h = newTestRouter(e.st, e.purposes, e.consents, e.holds, e.transfers) }

func baseReq() domain.EvaluateDecisionRequest {
	return domain.EvaluateDecisionRequest{
		SubjectRef: "subject-1", ProcessingActivityID: testActivity, PurposeID: testPurpose,
		ProposedOperation: domain.OperationUse, SubjectContext: validSubjectContext(),
	}
}

func (e *env) eval(t *testing.T, req domain.EvaluateDecisionRequest) domain.PrivacyDecision {
	t.Helper()
	w := doRequest(e.h, http.MethodPost, "/privacy/decisions", req, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var d domain.PrivacyDecision
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func expect(t *testing.T, d domain.PrivacyDecision, result domain.DecisionResult, reason string) {
	t.Helper()
	if d.Result != result {
		t.Fatalf("expected %s, got %s %v", result, d.Result, d.ReasonCodes)
	}
	if result == domain.ResultPermit {
		return
	}
	for _, r := range d.ReasonCodes {
		if r == reason {
			return
		}
	}
	t.Fatalf("expected reason %q in %v", reason, d.ReasonCodes)
}

// ── Gap 4 / 10: registered scope and unknown values ─────────────────────────

func TestScope_DataCategories(t *testing.T) {
	cases := []struct {
		name       string
		registered []string
		requested  *domain.DataContext
		want       domain.DecisionResult
		reason     string
	}{
		{"matching category", []string{"CONTACT_INFO", "IDENTIFIERS"}, &domain.DataContext{DataCategories: []string{"contact_info"}}, domain.ResultPermit, ""},
		{"single data_category matches", []string{"CONTACT_INFO"}, &domain.DataContext{DataCategory: "CONTACT_INFO"}, domain.ResultPermit, ""},
		{"no categories declared uses registered scope", []string{"CONTACT_INFO"}, nil, domain.ResultPermit, ""},
		{"unauthorized (unregistered) category", []string{"CONTACT_INFO"}, &domain.DataContext{DataCategories: []string{"FINANCIAL"}}, domain.ResultBlock, domain.PRV010DataCategoryRestricted},
		{"one of several unauthorized", []string{"CONTACT_INFO"}, &domain.DataContext{DataCategories: []string{"CONTACT_INFO", "HEALTH"}}, domain.ResultBlock, domain.PRV010DataCategoryRestricted},
		{"registry lists no categories (scope unknown)", nil, &domain.DataContext{DataCategories: []string{"CONTACT_INFO"}}, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"registered category outside taxonomy", []string{"MYSTERY_DATA"}, &domain.DataContext{DataCategories: []string{"MYSTERY_DATA"}}, domain.ResultReviewRequired, domain.PRV010DataCategoryRestricted},
		{"sensitive registered category omitted by caller", []string{"CONTACT_INFO", "HEALTH"}, &domain.DataContext{DataCategories: []string{"CONTACT_INFO"}}, domain.ResultReviewRequired, domain.PRV010DataCategoryRestricted},
		{"sensitive registered, caller declares nothing and no flags", []string{"BIOMETRIC"}, nil, domain.ResultReviewRequired, domain.PRV010DataCategoryRestricted},
		{"caller flag cannot downgrade (flag absent) but can add", []string{"CONTACT_INFO"}, &domain.DataContext{DataCategories: []string{"CONTACT_INFO"}, SensitivityFlags: []string{"HEALTH"}}, domain.ResultReviewRequired, domain.PRV010DataCategoryRestricted},
		{"conflicting classification RESTRICTED", []string{"CONTACT_INFO"}, &domain.DataContext{DataCategories: []string{"CONTACT_INFO"}, Classification: "RESTRICTED"}, domain.ResultReviewRequired, domain.PRV010DataCategoryRestricted},
		{"classification PUBLIC does not downgrade sensitive registered", []string{"HEALTH"}, &domain.DataContext{Classification: "PUBLIC"}, domain.ResultReviewRequired, domain.PRV010DataCategoryRestricted},
		{"classification UNKNOWN", []string{"CONTACT_INFO"}, &domain.DataContext{Classification: "UNKNOWN"}, domain.ResultReviewRequired, domain.PRV010DataCategoryRestricted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv()
			e.activity.DataCategories = c.registered
			req := baseReq()
			req.DataContext = c.requested
			expect(t, e.eval(t, req), c.want, c.reason)
		})
	}
}

func TestScope_SubjectClasses(t *testing.T) {
	cases := []struct {
		name       string
		registered []string
		class      string
		want       domain.DecisionResult
		reason     string
	}{
		{"matching class", []string{"CUSTOMER"}, "customer", domain.ResultPermit, ""},
		{"missing class in request", []string{"CUSTOMER"}, "", domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"class not registered for activity", []string{"CUSTOMER"}, "VENDOR", domain.ResultBlock, domain.PRV009PurposeIncompatible},
		{"UNKNOWN class", []string{"CUSTOMER"}, "UNKNOWN", domain.ResultReviewRequired, domain.PRV019PrivacyContextIndeterminate},
		{"registry lists no classes", nil, "CUSTOMER", domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv()
			e.activity.SubjectClasses = c.registered
			req := baseReq()
			req.SubjectContext.SubjectClass = c.class
			expect(t, e.eval(t, req), c.want, c.reason)
		})
	}
}

func TestUnknownValues_FailClosed(t *testing.T) {
	type mut func(e *env, r *domain.EvaluateDecisionRequest)
	cases := []struct {
		name   string
		mutate mut
		want   domain.DecisionResult
		reason string
	}{
		{"age band unsupported", func(e *env, r *domain.EvaluateDecisionRequest) { r.SubjectContext.AgeBand = "TEEN" }, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"age band UNKNOWN", func(e *env, r *domain.EvaluateDecisionRequest) { r.SubjectContext.AgeBand = "UNKNOWN" }, domain.ResultReviewRequired, domain.PRV019PrivacyContextIndeterminate},
		{"age band MINOR", func(e *env, r *domain.EvaluateDecisionRequest) { r.SubjectContext.AgeBand = "MINOR" }, domain.ResultReviewRequired, domain.PRV010DataCategoryRestricted},
		{"classification unsupported", func(e *env, r *domain.EvaluateDecisionRequest) {
			r.DataContext = &domain.DataContext{Classification: "TOPSECRET"}
		}, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"sensitivity flag unsupported", func(e *env, r *domain.EvaluateDecisionRequest) {
			r.DataContext = &domain.DataContext{SensitivityFlags: []string{"MADE_UP"}}
		}, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"data source unsupported", func(e *env, r *domain.EvaluateDecisionRequest) { r.DataContext = &domain.DataContext{Source: "RUMOUR"} }, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"recipient type unsupported", func(e *env, r *domain.EvaluateDecisionRequest) {
			r.RecipientContext = &domain.RecipientContext{RecipientType: "FRIEND"}
		}, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"jurisdiction UNKNOWN", func(e *env, r *domain.EvaluateDecisionRequest) {
			r.SubjectContext.Jurisdiction, r.SubjectContext.Residency = "UNKNOWN", ""
		}, domain.ResultIndeterminate, domain.PRV004JurisdictionUnresolved},
		{"jurisdiction missing", func(e *env, r *domain.EvaluateDecisionRequest) {
			r.SubjectContext.Jurisdiction, r.SubjectContext.Residency = "", ""
		}, domain.ResultIndeterminate, domain.PRV004JurisdictionUnresolved},
		{"notice_consent_dependency empty", func(e *env, r *domain.EvaluateDecisionRequest) { e.activity.NoticeConsentDependency = "" }, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"notice_consent_dependency unrecognised", func(e *env, r *domain.EvaluateDecisionRequest) { e.activity.NoticeConsentDependency = "MAYBE" }, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"dpia status empty", func(e *env, r *domain.EvaluateDecisionRequest) { e.activity.DpiaTiaStatus = "" }, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"dpia status unrecognised", func(e *env, r *domain.EvaluateDecisionRequest) { e.activity.DpiaTiaStatus = "PENDING_MAYBE" }, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"dpia review required", func(e *env, r *domain.EvaluateDecisionRequest) { e.activity.DpiaTiaStatus = "REVIEW_REQUIRED" }, domain.ResultReviewRequired, domain.PRV016AssessmentRequired},
		{"policy refs missing", func(e *env, r *domain.EvaluateDecisionRequest) {
			e.purposes.purposes[testPurpose].LawfulBasisRefs = nil
		}, domain.ResultIndeterminate, domain.PRV005PolicyUnavailable},
		{"consent status unrecognised", func(e *env, r *domain.EvaluateDecisionRequest) {
			e.activity.NoticeConsentDependency = "REQUIRED"
			e.consents.status = "SOMETHING_NEW"
		}, domain.ResultBlock, domain.PRV006ConsentRequiredMissing},
		{"consent status empty", func(e *env, r *domain.EvaluateDecisionRequest) {
			e.activity.NoticeConsentDependency = "REQUIRED"
			e.consents.status = ""
		}, domain.ResultBlock, domain.PRV006ConsentRequiredMissing},
		{"consent CONDITIONAL dependency fails closed to consent-required", func(e *env, r *domain.EvaluateDecisionRequest) {
			e.activity.NoticeConsentDependency = "CONDITIONAL"
			e.consents.status = "NOT_REQUESTED"
		}, domain.ResultBlock, domain.PRV006ConsentRequiredMissing},
		{"consent GRANTED without receipt evidence", func(e *env, r *domain.EvaluateDecisionRequest) {
			e.activity.NoticeConsentDependency = "REQUIRED"
			e.consents.consentReceiptID, e.consents.noticeVersionID = "", ""
		}, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"asserted consent receipt is not the latest", func(e *env, r *domain.EvaluateDecisionRequest) {
			e.activity.NoticeConsentDependency = "REQUIRED"
			r.ConsentCheck = &domain.ConsentCheckRequest{Required: true, ReceiptID: "rcpt-OLD"}
		}, domain.ResultBlock, domain.PRV006ConsentRequiredMissing},
		{"consent registry error", func(e *env, r *domain.EvaluateDecisionRequest) {
			e.activity.NoticeConsentDependency = "REQUIRED"
			e.consents.err = errors.New("down")
		}, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv()
			req := baseReq()
			c.mutate(e, &req)
			e.rebuild()
			expect(t, e.eval(t, req), c.want, c.reason)
		})
	}
}

// ── Gap 5: model training ────────────────────────────────────────────────────

func TestTrainModel_NeverAutoPermitted(t *testing.T) {
	e := newEnv()
	req := baseReq()
	req.ProposedOperation = domain.OperationTrainModel
	d := e.eval(t, req)
	expect(t, d, domain.ResultReviewRequired, domain.PRV009PurposeIncompatible)

	// Same request with every other safeguard satisfied but USE is permitted:
	// the difference is solely the TRAIN_MODEL operation.
	req.ProposedOperation = domain.OperationUse
	expect(t, e.eval(t, req), domain.ResultPermit, "")
}

func TestTrainModel_HigherPriorityBlocksStillApply(t *testing.T) {
	e := newEnv()
	e.activity.NoticeConsentDependency = "REQUIRED"
	e.consents.status = "WITHDRAWN"
	req := baseReq()
	req.ProposedOperation = domain.OperationTrainModel
	expect(t, e.eval(t, req), domain.ResultBlock, domain.PRV007ConsentWithdrawn)
}

func TestTrainModel_UnboundPurposeBlocked(t *testing.T) {
	e := newEnv()
	e.purposes.purposes["telemetry-purpose"] = publishedPurpose()
	req := baseReq()
	req.PurposeID = "telemetry-purpose" // not registered for this activity: not inherited (§13.1)
	req.ProposedOperation = domain.OperationTrainModel
	expect(t, e.eval(t, req), domain.ResultBlock, domain.PRV009PurposeIncompatible)
}

// ── Gap 6: legal hold / retention ────────────────────────────────────────────

type fnHold func() (*retentionregistry.RetentionResolution, error)

func (f fnHold) Resolve(context.Context, string, string, string, string) (*retentionregistry.RetentionResolution, error) {
	return f()
}

func TestLegalHold_FailClosed(t *testing.T) {
	check := &domain.LegalHoldCheckRequest{RecordClass: "HR_RECORD", EntityRef: "subject-1"}
	hold := &retentionregistry.LegalHold{LegalHoldID: "hold-7"}
	cases := []struct {
		name   string
		op     domain.ProposedOperation
		check  *domain.LegalHoldCheckRequest
		res    func() (*retentionregistry.RetentionResolution, error)
		want   domain.DecisionResult
		reason string
		holdID string
	}{
		{"DELETE without hold information", domain.OperationDelete, nil, nil, domain.ResultIndeterminate, domain.PRV014RetentionOrHoldBlock, ""},
		{"DELETE with blank record_class", domain.OperationDelete, &domain.LegalHoldCheckRequest{RecordClass: "  "}, nil, domain.ResultIndeterminate, domain.PRV014RetentionOrHoldBlock, ""},
		{"ANONYMIZE without hold information", domain.OperationAnonymize, nil, nil, domain.ResultIndeterminate, domain.PRV014RetentionOrHoldBlock, ""},
		{"DELETE held", domain.OperationDelete, check, func() (*retentionregistry.RetentionResolution, error) {
			return &retentionregistry.RetentionResolution{Blocked: true, MatchedHold: hold}, nil
		}, domain.ResultBlock, domain.PRV014RetentionOrHoldBlock, "hold-7"},
		{"DELETE not held", domain.OperationDelete, check, func() (*retentionregistry.RetentionResolution, error) {
			return &retentionregistry.RetentionResolution{}, nil
		}, domain.ResultPermit, "", ""},
		{"DELETE registry error", domain.OperationDelete, check, func() (*retentionregistry.RetentionResolution, error) {
			return nil, errors.New("down")
		}, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate, ""},
		{"DELETE nil resolution", domain.OperationDelete, check, func() (*retentionregistry.RetentionResolution, error) { return nil, nil }, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate, ""},
		{"DELETE conflicting state (hold matched, not blocked)", domain.OperationDelete, check, func() (*retentionregistry.RetentionResolution, error) {
			return &retentionregistry.RetentionResolution{Blocked: false, MatchedHold: hold}, nil
		}, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate, "hold-7"},
		{"DELETE blocked without matched hold still blocks", domain.OperationDelete, check, func() (*retentionregistry.RetentionResolution, error) {
			return &retentionregistry.RetentionResolution{Blocked: true}, nil
		}, domain.ResultBlock, domain.PRV014RetentionOrHoldBlock, ""},
		{"USE held is blocked", domain.OperationUse, check, func() (*retentionregistry.RetentionResolution, error) {
			return &retentionregistry.RetentionResolution{Blocked: true, MatchedHold: hold}, nil
		}, domain.ResultBlock, domain.PRV014RetentionOrHoldBlock, "hold-7"},
		{"RETAIN under hold continues and records the hold", domain.OperationRetain, check, func() (*retentionregistry.RetentionResolution, error) {
			return &retentionregistry.RetentionResolution{Blocked: true, MatchedHold: hold}, nil
		}, domain.ResultPermit, "", "hold-7"},
		{"RETAIN registry error", domain.OperationRetain, check, func() (*retentionregistry.RetentionResolution, error) {
			return nil, errors.New("down")
		}, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv()
			// retention_rule_refs must NOT be used as a record_class (undocumented mapping removed).
			e.activity.RetentionRuleRefs = []string{"RETENTION-RULE-1"}
			var calls []string
			h := handler.New(e.st, e.purposes, e.consents, fnHold(func() (*retentionregistry.RetentionResolution, error) {
				calls = append(calls, "called")
				return c.res()
			}), e.transfers, zap.NewNop())
			r := routerFor(h)
			req := baseReq()
			req.ProposedOperation = c.op
			req.LegalHoldCheck = c.check
			req.DeidentificationControlRef = "approved-control-1" // isolate the hold gate from the anonymization safeguard
			w := doRequest(r, http.MethodPost, "/privacy/decisions", req, testTenant)
			var d domain.PrivacyDecision
			_ = json.Unmarshal(w.Body.Bytes(), &d)
			expect(t, d, c.want, c.reason)
			if c.holdID != "" && (d.LegalHoldID == nil || *d.LegalHoldID != c.holdID) {
				t.Fatalf("expected legal_hold_id %s recorded, got %v", c.holdID, d.LegalHoldID)
			}
			if c.check == nil && len(calls) != 0 {
				t.Fatal("no record_class was supplied; the registry must not be queried with an invented one")
			}
		})
	}
}

// ── Gap 7: transfer gate ─────────────────────────────────────────────────────

func TestTransfer_ResultsFailClosed(t *testing.T) {
	cases := []struct {
		name     string
		decision *transferregistry.TransferDecision
		err      error
		want     domain.DecisionResult
		reason   string
	}{
		{"authorized", &transferregistry.TransferDecision{DecisionID: "td-1", Result: "AUTHORIZED"}, nil, domain.ResultPermit, ""},
		{"authorized lower-case", &transferregistry.TransferDecision{DecisionID: "td-1", Result: "authorized"}, nil, domain.ResultPermit, ""},
		{"conditional", &transferregistry.TransferDecision{DecisionID: "td-1", Result: "CONDITIONAL"}, nil, domain.ResultRestrict, domain.PRV011MinimizationRequired},
		{"blocked", &transferregistry.TransferDecision{DecisionID: "td-1", Result: "BLOCKED"}, nil, domain.ResultBlock, domain.PRV015TransferNotAuthorized},
		{"review required", &transferregistry.TransferDecision{DecisionID: "td-1", Result: "REVIEW_REQUIRED"}, nil, domain.ResultReviewRequired, domain.PRV016AssessmentRequired},
		{"empty result", &transferregistry.TransferDecision{DecisionID: "td-1", Result: ""}, nil, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"unknown result", &transferregistry.TransferDecision{DecisionID: "td-1", Result: "APPROVED_MAYBE"}, nil, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
		{"downstream error", nil, errors.New("down"), domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv()
			e.transfers = &stubTransferRegistry{decision: c.decision, err: c.err}
			e.rebuild()
			req := baseReq()
			req.ProposedOperation = domain.OperationExport
			req.TransferCheck = &domain.TransferCheckRequest{RelationshipID: "rel", TransferMechanismID: "mech", DestinationJurisdiction: "US"}
			expect(t, e.eval(t, req), c.want, c.reason)
		})
	}
}

func TestTransfer_NilDecisionAndNilClientFailClosed(t *testing.T) {
	e := newEnv()
	req := baseReq()
	req.ProposedOperation = domain.OperationExport
	req.TransferCheck = &domain.TransferCheckRequest{RelationshipID: "rel", TransferMechanismID: "mech"}

	h := handler.New(e.st, e.purposes, e.consents, e.holds, nil, zap.NewNop())
	w := doRequest(routerFor(h), http.MethodPost, "/privacy/decisions", req, testTenant)
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	expect(t, d, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate)

	h = handler.New(e.st, e.purposes, e.consents, e.holds, nilDecisionTransfer{}, zap.NewNop())
	w = doRequest(routerFor(h), http.MethodPost, "/privacy/decisions", req, testTenant)
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	expect(t, d, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate)
}

type nilDecisionTransfer struct{}

func (nilDecisionTransfer) EvaluateTransfer(context.Context, string, string, *transferregistry.EvaluateTransferRequest) (*transferregistry.TransferDecision, error) {
	return nil, nil
}

func TestTransfer_ExportWithoutTransferCheckBlocked(t *testing.T) {
	e := newEnv()
	req := baseReq()
	req.ProposedOperation = domain.OperationExport
	expect(t, e.eval(t, req), domain.ResultBlock, domain.PRV015TransferNotAuthorized)
}

// ── Gap 8: secondary purpose (I14, N02) ──────────────────────────────────────

func TestSecondaryPurpose(t *testing.T) {
	t.Run("free text is blocked and never stored as a purpose id", func(t *testing.T) {
		e := newEnv()
		req := baseReq()
		req.ProposedSecondaryPurpose = "marketing analytics, whatever"
		d := e.eval(t, req)
		expect(t, d, domain.ResultBlock, domain.PRV001PurposeNotRegistered)
		if d.SecondaryPurposeID != nil {
			t.Fatalf("free text must not be stored as secondary_purpose_id: %q", *d.SecondaryPurposeID)
		}
		raw, _ := json.Marshal(d)
		if strings.Contains(string(raw), "marketing analytics") {
			t.Fatal("free-text purpose leaked into the decision evidence")
		}
	})
	t.Run("registered and bound secondary purpose is never auto-permitted", func(t *testing.T) {
		e := newEnv()
		e.activity.PurposeIDs = []string{testPurpose, "purpose-secondary"}
		e.purposes.purposes["purpose-secondary"] = &purposeregistry.PurposeVersion{PurposeVersionID: "pv-s", PurposeID: "purpose-secondary", VersionStatus: "PUBLISHED"}
		req := baseReq()
		req.SecondaryPurposeID = "purpose-secondary"
		d := e.eval(t, req)
		expect(t, d, domain.ResultReviewRequired, domain.PRV009PurposeIncompatible)
		if d.SecondaryPurposeID == nil || *d.SecondaryPurposeID != "purpose-secondary" {
			t.Fatalf("registered secondary id must be recorded, got %v", d.SecondaryPurposeID)
		}
	})
	t.Run("unregistered secondary id blocks", func(t *testing.T) {
		e := newEnv()
		req := baseReq()
		req.SecondaryPurposeID = "does-not-exist"
		expect(t, e.eval(t, req), domain.ResultBlock, domain.PRV001PurposeNotRegistered)
	})
	t.Run("unpublished secondary purpose blocks", func(t *testing.T) {
		e := newEnv()
		e.purposes.purposes["draft"] = &purposeregistry.PurposeVersion{PurposeID: "draft", VersionStatus: "DRAFT"}
		req := baseReq()
		req.SecondaryPurposeID = "draft"
		expect(t, e.eval(t, req), domain.ResultBlock, domain.PRV001PurposeNotRegistered)
	})
	t.Run("published secondary purpose not bound to the activity blocks", func(t *testing.T) {
		e := newEnv()
		e.purposes.purposes["other"] = &purposeregistry.PurposeVersion{PurposeID: "other", VersionStatus: "PUBLISHED"}
		req := baseReq()
		req.SecondaryPurposeID = "other"
		expect(t, e.eval(t, req), domain.ResultBlock, domain.PRV009PurposeIncompatible)
	})
}

// ── Gap 9: exact version resolution (§13, I29) ───────────────────────────────

func TestExactVersionResolution(t *testing.T) {
	t.Run("requested activity version is resolved exactly and recorded", func(t *testing.T) {
		e := newEnv()
		v := *e.activity
		v.ActivityVersionID = "av-exact"
		e.purposes.versions[testActivity+"@av-exact"] = &v
		req := baseReq()
		req.ActivityVersionID = "av-exact"
		d := e.eval(t, req)
		expect(t, d, domain.ResultPermit, "")
		if d.ActivityVersionID == nil || *d.ActivityVersionID != "av-exact" {
			t.Fatalf("exact version must be recorded, got %v", d.ActivityVersionID)
		}
	})
	t.Run("requested historical activity version never becomes current", func(t *testing.T) {
		e := newEnv() // only the current av-1 exists
		req := baseReq()
		req.ActivityVersionID = "av-old"
		expect(t, e.eval(t, req), domain.ResultBlock, domain.PRV002ProcessingActivityInactive)
	})
	t.Run("requested version that is not ACTIVE blocks", func(t *testing.T) {
		e := newEnv()
		v := *e.activity
		v.ActivityVersionID, v.VersionStatus = "av-retired", "RETIRED"
		e.purposes.versions[testActivity+"@av-retired"] = &v
		req := baseReq()
		req.ActivityVersionID = "av-retired"
		expect(t, e.eval(t, req), domain.ResultBlock, domain.PRV002ProcessingActivityInactive)
	})
	t.Run("registry answering a different version than requested fails closed", func(t *testing.T) {
		e := newEnv()
		v := *e.activity // ActivityVersionID av-1
		e.purposes.versions[testActivity+"@av-x"] = &v
		req := baseReq()
		req.ActivityVersionID = "av-x"
		expect(t, e.eval(t, req), domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate)
	})
	t.Run("version belonging to another activity blocks", func(t *testing.T) {
		e := newEnv()
		v := *e.activity
		v.ActivityID = "someone-elses-activity"
		v.ActivityVersionID = "av-y"
		e.purposes.versions[testActivity+"@av-y"] = &v
		req := baseReq()
		req.ActivityVersionID = "av-y"
		expect(t, e.eval(t, req), domain.ResultBlock, domain.PRV002ProcessingActivityInactive)
	})
	t.Run("matching purpose version is accepted and recorded", func(t *testing.T) {
		e := newEnv()
		req := baseReq()
		req.PurposeVersionID = "pv-1"
		d := e.eval(t, req)
		expect(t, d, domain.ResultPermit, "")
		if d.PurposeVersionID == nil || *d.PurposeVersionID != "pv-1" {
			t.Fatalf("got %v", d.PurposeVersionID)
		}
	})
	t.Run("requested purpose version that is not the resolvable one fails closed", func(t *testing.T) {
		e := newEnv()
		req := baseReq()
		req.PurposeVersionID = "pv-historical"
		d := e.eval(t, req)
		expect(t, d, domain.ResultIndeterminate, domain.PRV019PrivacyContextIndeterminate)
		if d.PurposeVersionID != nil {
			t.Fatalf("the current purpose version must not be recorded in place of the requested one: %v", *d.PurposeVersionID)
		}
	})
}

// ── Gap 11: expires_at ───────────────────────────────────────────────────────

func TestExpiresAt(t *testing.T) {
	soon := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	later := soon.Add(24 * time.Hour)
	exportReq := func() domain.EvaluateDecisionRequest {
		r := baseReq()
		r.ProposedOperation = domain.OperationExport
		r.TransferCheck = &domain.TransferCheckRequest{RelationshipID: "rel", TransferMechanismID: "m"}
		return r
	}
	t.Run("none when no upstream expiry exists", func(t *testing.T) {
		e := newEnv()
		if d := e.eval(t, baseReq()); d.ExpiresAt != nil {
			t.Fatalf("no authoritative rule or upstream expiry exists; expires_at must be omitted, got %v", d.ExpiresAt)
		}
	})
	t.Run("earliest of consent and transfer expiry", func(t *testing.T) {
		e := newEnv()
		e.activity.NoticeConsentDependency = "REQUIRED"
		e.consents.expiresAt = &later
		e.transfers = &stubTransferRegistry{decision: &transferregistry.TransferDecision{DecisionID: "td", Result: "AUTHORIZED", ExpiresAt: &soon}}
		e.rebuild()
		d := e.eval(t, exportReq())
		if d.ExpiresAt == nil || !d.ExpiresAt.Equal(soon) {
			t.Fatalf("expected %v, got %v", soon, d.ExpiresAt)
		}
		e.transfers = &stubTransferRegistry{decision: &transferregistry.TransferDecision{DecisionID: "td", Result: "AUTHORIZED", ExpiresAt: &later}}
		e.consents.expiresAt = &soon
		e.rebuild()
		d = e.eval(t, exportReq())
		if d.ExpiresAt == nil || !d.ExpiresAt.Equal(soon) {
			t.Fatalf("consent expiry should win: expected %v, got %v", soon, d.ExpiresAt)
		}
	})
	t.Run("persisted, returned on GET and preserved on replay", func(t *testing.T) {
		e := newEnv()
		e.activity.NoticeConsentDependency = "REQUIRED"
		e.consents.expiresAt = &soon
		e.rebuild()
		hdr := map[string]string{"Idempotency-Key": "k-exp"}
		w1 := doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, hdr)
		w2 := doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, hdr)
		var d1, d2, d3 domain.PrivacyDecision
		_ = json.Unmarshal(w1.Body.Bytes(), &d1)
		_ = json.Unmarshal(w2.Body.Bytes(), &d2)
		w3 := doRequest(e.h, http.MethodGet, "/privacy/decisions/"+d1.DecisionID, nil, testTenant)
		_ = json.Unmarshal(w3.Body.Bytes(), &d3)
		for i, d := range []domain.PrivacyDecision{d1, d2, d3} {
			if d.ExpiresAt == nil || !d.ExpiresAt.Equal(soon) {
				t.Fatalf("response %d lost expires_at: %v", i, d.ExpiresAt)
			}
		}
	})
	t.Run("already-expired consent receipt does not support a decision", func(t *testing.T) {
		e := newEnv()
		e.activity.NoticeConsentDependency = "REQUIRED"
		past := time.Now().Add(-time.Minute)
		e.consents.expiresAt = &past
		e.rebuild()
		expect(t, e.eval(t, baseReq()), domain.ResultBlock, domain.PRV006ConsentRequiredMissing)
	})
}

// ── Gap 12: correlation echo ─────────────────────────────────────────────────

func TestCorrelationIDEchoed(t *testing.T) {
	e := newEnv()
	hdr := map[string]string{"X-Correlation-ID": "corr-echo-42", "Idempotency-Key": "k-corr"}
	w1 := doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, hdr)
	if got := w1.Header().Get("X-Correlation-ID"); got != "corr-echo-42" {
		t.Fatalf("first response must echo correlation id, got %q", got)
	}
	w2 := doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, hdr)
	if w2.Header().Get("Idempotency-Replay") != "true" || w2.Header().Get("X-Correlation-ID") != "corr-echo-42" {
		t.Fatalf("replay must echo the caller's correlation id (replay=%q corr=%q)", w2.Header().Get("Idempotency-Replay"), w2.Header().Get("X-Correlation-ID"))
	}
	// The replay echoes the *current* request's header, even if it differs from the stored one.
	hdr2 := map[string]string{"X-Correlation-ID": "corr-other", "Idempotency-Key": "k-corr"}
	w3 := doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, hdr2)
	if w3.Header().Get("X-Correlation-ID") != "corr-other" {
		t.Fatalf("replay must echo the current correlation id, got %q", w3.Header().Get("X-Correlation-ID"))
	}
	var d domain.PrivacyDecision
	_ = json.Unmarshal(w1.Body.Bytes(), &d)
	wg := doRequestWithHeaders(e.h, http.MethodGet, "/privacy/decisions/"+d.DecisionID, nil, testTenant, map[string]string{"X-Correlation-ID": "corr-get"})
	if wg.Header().Get("X-Correlation-ID") != "corr-get" {
		t.Fatalf("GET must echo a supplied correlation id, got %q", wg.Header().Get("X-Correlation-ID"))
	}
	wm := doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, map[string]string{"X-Correlation-ID": ""})
	if wm.Code != http.StatusBadRequest || wm.Header().Get("X-Correlation-ID") != "" {
		t.Fatalf("missing correlation must be 400 with nothing echoed, got %d %q", wm.Code, wm.Header().Get("X-Correlation-ID"))
	}
}

// ── Gap 2/13/15: atomic persistence, error contract, principal scope ────────

func TestCommitFailure_Returns503_NothingPersisted_RetryCreatesOneDecision(t *testing.T) {
	e := newEnv()
	hdr := map[string]string{"Idempotency-Key": "k-atomic"}
	e.st.failCommit = true
	w := doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, hdr)
	assertError(t, w, http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable)
	if len(e.st.decisions) != 0 || len(e.st.idempotency) != 0 {
		t.Fatalf("failed commit must persist nothing: %d decisions %d keys", len(e.st.decisions), len(e.st.idempotency))
	}

	e.st.failCommit = false
	w = doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, hdr)
	if w.Code != http.StatusOK || w.Header().Get("Idempotency-Replay") == "true" {
		t.Fatalf("retry after failure must be a fresh success, got %d replay=%q", w.Code, w.Header().Get("Idempotency-Replay"))
	}
	w = doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, hdr)
	if w.Header().Get("Idempotency-Replay") != "true" {
		t.Fatal("subsequent identical request must replay")
	}
	if len(e.st.decisions) != 1 {
		t.Fatalf("expected exactly one decision (no duplicate evidence), got %d", len(e.st.decisions))
	}
}

func TestIdempotency_PrincipalScope_Handler(t *testing.T) {
	e := newEnv()
	hdrA := map[string]string{"Idempotency-Key": "shared", "X-Principal-Id": "principal-A"}
	hdrB := map[string]string{"Idempotency-Key": "shared", "X-Principal-Id": "principal-B"}
	var a, b domain.PrivacyDecision
	wa := doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, hdrA)
	wb := doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, hdrB)
	_ = json.Unmarshal(wa.Body.Bytes(), &a)
	_ = json.Unmarshal(wb.Body.Bytes(), &b)
	if wb.Header().Get("Idempotency-Replay") == "true" || a.DecisionID == b.DecisionID {
		t.Fatal("principal B must not replay principal A's decision")
	}
	if a.ActorPrincipalID != "principal-A" || b.ActorPrincipalID != "principal-B" {
		t.Fatalf("actor attribution wrong: %q %q", a.ActorPrincipalID, b.ActorPrincipalID)
	}
	wa2 := doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", baseReq(), testTenant, hdrA)
	var a2 domain.PrivacyDecision
	_ = json.Unmarshal(wa2.Body.Bytes(), &a2)
	if wa2.Header().Get("Idempotency-Replay") != "true" || a2.DecisionID != a.DecisionID {
		t.Fatal("principal A must replay its own decision")
	}
}

func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status: want %d got %d (%s)", status, w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error_code"] != code {
		t.Fatalf("error_code: want %s got %v", code, body)
	}
}

func routerFor(h *handler.Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func TestErrorContract_AllPaths(t *testing.T) {
	type tc struct {
		name    string
		method  string
		path    string
		body    interface{}
		tenant  string
		headers map[string]string
		setup   func(e *env)
		status  int
		code    string
	}
	good := baseReq()
	cases := []tc{
		{"missing principal", "POST", "/privacy/decisions", good, testTenant, map[string]string{"X-Principal-Id": ""}, nil, 401, domain.ErrCodeAuthenticationFailed},
		{"missing tenant", "POST", "/privacy/decisions", good, "", nil, nil, 400, domain.ErrCodeTenantScopeRequired},
		{"missing correlation", "POST", "/privacy/decisions", good, testTenant, map[string]string{"X-Correlation-ID": ""}, nil, 400, domain.ErrCodeCorrelationRequired},
		{"missing idempotency key", "POST", "/privacy/decisions", good, testTenant, map[string]string{"Idempotency-Key": ""}, nil, 400, domain.ErrCodeIdempotencyRequired},
		{"invalid body", "POST", "/privacy/decisions", "not-an-object", testTenant, nil, nil, 400, domain.ErrCodeValidationFailed},
		{"missing required fields", "POST", "/privacy/decisions", domain.EvaluateDecisionRequest{ProposedOperation: domain.OperationUse}, testTenant, nil, nil, 400, domain.ErrCodeValidationFailed},
		{"invalid operation", "POST", "/privacy/decisions", func() interface{} { r := baseReq(); r.ProposedOperation = "TELEPORT"; return r }(), testTenant, nil, nil, 400, domain.ErrCodeValidationFailed},
		{"tenant mismatch", "POST", "/privacy/decisions", func() interface{} { r := baseReq(); r.TenantID = "other-tenant"; return r }(), testTenant, nil, nil, 403, domain.ErrCodeTenantScopeMismatch},
		{"commit failure", "POST", "/privacy/decisions", good, testTenant, nil, func(e *env) { e.st.failCommit = true }, 503, domain.ErrCodeStoreUnavailable},
		{"GET missing principal", "GET", "/privacy/decisions/x", nil, testTenant, map[string]string{"X-Principal-Id": ""}, nil, 401, domain.ErrCodeAuthenticationFailed},
		{"GET missing tenant", "GET", "/privacy/decisions/x", nil, "", nil, nil, 400, domain.ErrCodeTenantScopeRequired},
		{"GET not found", "GET", "/privacy/decisions/does-not-exist", nil, testTenant, nil, nil, 404, domain.ErrCodeNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv()
			if c.setup != nil {
				c.setup(e)
			}
			w := doRequestWithHeaders(e.h, c.method, c.path, c.body, c.tenant, c.headers)
			if w.Code != c.status {
				t.Fatalf("status: want %d got %d (%s)", c.status, w.Code, w.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body must be JSON: %v (%s)", err, w.Body.String())
			}
			if body["error_code"] != c.code || body["error"] == "" {
				t.Fatalf("body: want code %s with message, got %v", c.code, body)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("content-type %q", ct)
			}
		})
	}
	// 409 PRV-020 for a reused key with a different payload.
	e := newEnv()
	hdr := map[string]string{"Idempotency-Key": "k-409"}
	doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", good, testTenant, hdr)
	other := baseReq()
	other.SubjectRef = "someone-else"
	w := doRequestWithHeaders(e.h, http.MethodPost, "/privacy/decisions", other, testTenant, hdr)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error_code"] != domain.ErrCodeImmutableEvidenceConflict {
		t.Fatalf("want PRV-020, got %v", body)
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "idempotency_save_failed") || strings.Contains(w.Body.String(), "IDEMPOTENCY_RECORD_NOT_SAVED") {
		t.Fatal("the removed post-commit save-failure code must not appear")
	}
}

// ── Gap 14: no undocumented event ────────────────────────────────────────────

func TestNoDecisionEventPublished(t *testing.T) {
	// §19 lists no privacy.decision.* event and nothing consumes one, so the
	// service must not emit it. Guard against it being reintroduced.
	var offenders []string
	for _, root := range []string{"../../internal", "../../cmd"} {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "privacy.decision.") || strings.Contains(string(b), "kafka") {
				offenders = append(offenders, p)
			}
			return nil
		})
	}
	// config.go still carries the (unused) Kafka settings; nothing may publish.
	for _, o := range offenders {
		if !strings.HasSuffix(filepath.ToSlash(o), "internal/config/config.go") {
			t.Fatalf("unexpected event/kafka reference in %s", o)
		}
	}
}

// ── Gap 16: formats ──────────────────────────────────────────────────────────

func TestIdentifierFormats(t *testing.T) {
	e := newEnv()
	d := e.eval(t, baseReq())
	if d.EvidenceRef != "prv-dec:"+d.DecisionID {
		t.Fatalf("evidence_ref format: %q", d.EvidenceRef)
	}
	if !strings.HasPrefix(d.InputFingerprint, "v2:") || len(d.InputFingerprint) != len("v2:")+64 {
		t.Fatalf("input_fingerprint format: %q", d.InputFingerprint)
	}
	if d.ContextHash != d.InputFingerprint {
		t.Fatal("context_hash must equal input_fingerprint (§18 alias)")
	}
}
