package handler_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
)

// ── purposes ─────────────────────────────────────────────────────────────────

func TestPurpose_CreateThenPublish_RecordsPublisherEvidence(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	if p.VersionStatus != domain.PurposeStatusPublished || p.PublishedByPrincipalID == nil || *p.PublishedByPrincipalID != checker || p.PublishedAt == nil {
		t.Fatalf("publisher evidence missing: %+v", p)
	}
	if p.NecessityRationale == "" {
		t.Fatalf("necessity rationale was not persisted")
	}
}

func TestPurpose_MakerCannotPublishOwnVersion(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	w := call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{Statement: "s", CompatibilityClass: "PRIMARY"}, tenantA, maker)
	v := decode[domain.PurposeVersion](t, w)
	wantErr(t, call(r, http.MethodPost, purposesPath+"/"+v.PurposeID+"/versions/"+v.PurposeVersionID+"/publish", nil, tenantA, maker),
		http.StatusForbidden, domain.ErrCodeSoDDenied)
}

// ── lifecycle: segregation of duties ─────────────────────────────────────────

func TestSoD_MakerCannotApproveOrRejectOwnActivity(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v := newDraft(t, r, tenantA, goodActivity(p.PurposeID))
	v = step(t, r, tenantA, v, "validate", maker, nil, http.StatusOK)
	step(t, r, tenantA, v, "submit", maker, nil, http.StatusOK)

	wantErr(t, call(r, http.MethodPost, versionPath(v, "approve"), nil, tenantA, maker), http.StatusForbidden, domain.ErrCodeSoDDenied)
	wantErr(t, call(r, http.MethodPost, versionPath(v, "reject"), domain.RejectActivityRequest{Reason: "self"}, tenantA, maker), http.StatusForbidden, domain.ErrCodeSoDDenied)

	// An independent checker can.
	approved := step(t, r, tenantA, v, "approve", checker, nil, http.StatusOK)
	if approved.ApprovedByPrincipalID == nil || *approved.ApprovedByPrincipalID != checker || approved.ApprovedAt == nil {
		t.Fatalf("approver evidence missing: %+v", approved)
	}
}

// ── lifecycle: state machine ─────────────────────────────────────────────────

func TestLifecycle_StatesCannotBeSkipped(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v := newDraft(t, r, tenantA, goodActivity(p.PurposeID))

	for _, verb := range []string{"submit", "approve", "activate", "suspend", "resume", "retire"} {
		body := interface{}(nil)
		if verb == "reject" {
			body = domain.RejectActivityRequest{Reason: "x"}
		}
		wantErr(t, call(r, http.MethodPost, versionPath(v, verb), body, tenantA, checker), http.StatusConflict, domain.ErrCodeInvalidTransition)
	}
	wantErr(t, call(r, http.MethodPost, versionPath(v, "reject"), domain.RejectActivityRequest{Reason: "x"}, tenantA, checker), http.StatusConflict, domain.ErrCodeInvalidTransition)

	// Validated is still not approvable.
	v = step(t, r, tenantA, v, "validate", maker, nil, http.StatusOK)
	wantErr(t, call(r, http.MethodPost, versionPath(v, "approve"), nil, tenantA, checker), http.StatusConflict, domain.ErrCodeInvalidTransition)
	wantErr(t, call(r, http.MethodPost, versionPath(v, "activate"), nil, tenantA, ops), http.StatusConflict, domain.ErrCodeInvalidTransition)
}

func TestLifecycle_FullPath_RecordsEveryActor(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))

	if v.VersionStatus != domain.ActivityStatusActive {
		t.Fatalf("not ACTIVE: %s", v.VersionStatus)
	}
	if v.ActivatedByPrincipalID == nil || *v.ActivatedByPrincipalID != ops || v.ActivatedAt == nil {
		t.Fatalf("activator evidence missing: %+v", v)
	}
	if v.ApprovedByPrincipalID == nil || *v.ApprovedByPrincipalID != checker {
		t.Fatalf("approver evidence missing")
	}
	if v.EffectiveFrom == nil || time.Since(*v.EffectiveFrom) > time.Minute || v.EffectiveTo != nil {
		t.Fatalf("effective window wrong: from=%v to=%v", v.EffectiveFrom, v.EffectiveTo)
	}

	suspended := step(t, r, tenantA, v, "suspend", ops, nil, http.StatusOK)
	if suspended.VersionStatus != domain.ActivityStatusSuspended {
		t.Fatalf("not SUSPENDED")
	}
	resumed := step(t, r, tenantA, v, "resume", ops, nil, http.StatusOK)
	if resumed.VersionStatus != domain.ActivityStatusActive {
		t.Fatalf("not resumed")
	}
	retired := step(t, r, tenantA, v, "retire", ops, nil, http.StatusOK)
	if retired.VersionStatus != domain.ActivityStatusRetired || retired.EffectiveTo == nil {
		t.Fatalf("retire must close the valid-time window: %+v", retired)
	}
}

func TestReject_RequiresReason_ThenFixLoopCreatesSuccessor(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v := newDraft(t, r, tenantA, goodActivity(p.PurposeID))
	v = step(t, r, tenantA, v, "validate", maker, nil, http.StatusOK)
	step(t, r, tenantA, v, "submit", maker, nil, http.StatusOK)

	wantErr(t, call(r, http.MethodPost, versionPath(v, "reject"), domain.RejectActivityRequest{}, tenantA, checker), http.StatusBadRequest, domain.ErrCodeValidationFailed)

	rejected := step(t, r, tenantA, v, "reject", checker, domain.RejectActivityRequest{Reason: "recipients incomplete"}, http.StatusOK)
	if rejected.VersionStatus != domain.ActivityStatusRejected || rejected.RejectedByPrincipalID == nil ||
		*rejected.RejectedByPrincipalID != checker || rejected.RejectionReason == nil || rejected.RejectedAt == nil {
		t.Fatalf("rejecter evidence missing: %+v", rejected)
	}
	// REJECTED is terminal (PRV-I20) …
	wantErr(t, call(r, http.MethodPost, versionPath(v, "submit"), nil, tenantA, maker), http.StatusConflict, domain.ErrCodeInvalidTransition)
	// … remediation is a successor draft naming the rejected one as parent.
	w := call(r, http.MethodPost, activitiesPath+"/"+v.ActivityID+"/versions", domain.CreateActivityVersionRequest{
		ParentVersionID: v.ActivityVersionID, PrivacyRole: "CONTROLLER", Owner: "head-of-billing", PrivacyOwner: "dpo",
		PurposeIDs: []string{p.PurposeID}, SubjectClasses: []string{"CUSTOMER"}, DataCategories: []string{"CONTACT_INFO"},
		Sources: []string{"signup-form"}, Recipients: []string{"INTERNAL_ONLY", "PAYMENT_PROVIDER"}, Jurisdictions: []string{"GB"},
		RetentionRuleRefs: []string{"r"}, NoticeConsentDependency: "REQUIRED", DPIATIAStatus: "RESOLVED",
	}, tenantA, maker)
	wantOK(t, w, http.StatusCreated)
	succ := decode[domain.ProcessingActivityVersion](t, w)
	if succ.VersionStatus != domain.ActivityStatusDraft || succ.SupersedesVersionID == nil || *succ.SupersedesVersionID != v.ActivityVersionID {
		t.Fatalf("bad successor: %+v", succ)
	}
}

func TestCanonicalLatestRoutes_RunTheWholeLifecycle(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v := newDraft(t, r, tenantA, goodActivity(p.PurposeID))
	base := activitiesPath + "/" + v.ActivityID

	for _, s := range []struct{ verb, who string }{{"validate", maker}, {"submit", maker}, {"approve", checker}, {"activate", ops}, {"suspend", ops}, {"resume", ops}, {"retire", ops}} {
		wantOK(t, call(r, http.MethodPost, base+"/"+s.verb, nil, tenantA, s.who), http.StatusOK)
	}
	// An unknown activity is refused AFTER authentication, with a stable code.
	wantErr(t, call(r, http.MethodPost, activitiesPath+"/00000000-0000-0000-0000-0000000000aa/validate", nil, tenantA, maker), http.StatusNotFound, domain.ErrCodeNotFound)
}

// ── §8.2 activation gates ────────────────────────────────────────────────────

// gate asserts that mutating a known-good request in one way makes /validate
// leave the version DRAFT with a finding carrying the expected §32 code & field.
func gate(t *testing.T, name, wantCode, wantField string, mutate func(req *domain.CreateActivityRequest, p *domain.PurposeVersion), purposeOpts ...func(*domain.CreatePurposeRequest)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		r := newRouter(newStubStore(), &stubAuthz{})
		p := newPurpose(t, r, tenantA, purposeOpts...)
		req := goodActivity(p.PurposeID)
		mutate(&req, p)
		v := newDraft(t, r, tenantA, req)
		v = step(t, r, tenantA, v, "validate", maker, nil, http.StatusOK)
		if v.VersionStatus != domain.ActivityStatusDraft {
			t.Fatalf("gate %q did not hold: status %s", name, v.VersionStatus)
		}
		for _, f := range v.ValidationFindings {
			if f.Code == wantCode && f.Field == wantField {
				return
			}
		}
		t.Fatalf("want finding %s on %s, got %+v", wantCode, wantField, v.ValidationFindings)
	})
}

func TestActivationGates_EveryGateIsEnforced(t *testing.T) {
	// Gate 1 — named accountable business owner AND privacy owner.
	gate(t, "gate1 privacy owner missing", "PRV-003", "privacy_owner", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.PrivacyOwner = "" })

	// Gate 3 — purpose statement + necessity/minimization rationale.
	gate(t, "gate3 no purpose linked", "PRV-001", "purpose_ids", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.PurposeIDs = nil })
	gate(t, "gate3 purpose not registered", "PRV-001", "purpose_ids", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) {
		q.PurposeIDs = []string{"00000000-0000-0000-0000-00000000dead"}
	})
	gate(t, "gate3 necessity rationale missing", "PRV-001", "purpose_ids", func(*domain.CreateActivityRequest, *domain.PurposeVersion) {},
		func(p *domain.CreatePurposeRequest) { p.NecessityRationale = "  " })

	// Gate 4 — approved lawful basis; special-category condition where applicable.
	gate(t, "gate4 lawful basis missing", "PRV-005", "purpose_ids", func(*domain.CreateActivityRequest, *domain.PurposeVersion) {},
		func(p *domain.CreatePurposeRequest) { p.LawfulBasisRefs = nil })
	gate(t, "gate4 special category without condition", "PRV-010", "special_condition_refs",
		func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) {
			q.DataCategories = []string{"CONTACT_INFO", "HEALTH"}
		})

	// Gate 5 — subjects, categories, sources, recipients, jurisdictions.
	gate(t, "gate5 jurisdictions missing", "PRV-004", "jurisdictions", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.Jurisdictions = nil })
	gate(t, "gate5 jurisdiction not in registry", "PRV-004", "jurisdictions", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.Jurisdictions = []string{"ZZ"} })
	gate(t, "gate5 jurisdiction free text", "PRV-004", "jurisdictions", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.Jurisdictions = []string{"Narnia"} })
	gate(t, "gate5 subject classes missing", "PRV-010", "subject_classes", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.SubjectClasses = nil })
	gate(t, "gate5 data categories missing", "PRV-010", "data_categories", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.DataCategories = nil })
	gate(t, "gate5 data category not in taxonomy", "PRV-010", "data_categories", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.DataCategories = []string{"VIBES"} })
	gate(t, "gate5 sources missing", "PRV-019", "sources", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.Sources = nil })
	gate(t, "gate5 recipients missing", "PRV-019", "recipients", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.Recipients = nil })
	gate(t, "gate5 blank entry in a list", "PRV-019", "sources", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) {
		q.Sources = []string{"signup-form", "  "}
	})

	// Gate 6 — retention, transfer and processor dependencies.
	gate(t, "gate6 retention missing", "PRV-014", "retention_rule_refs", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.RetentionRuleRefs = nil })
	gate(t, "gate6 cross-border without transfer refs", "PRV-015", "transfer_refs", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) {
		q.Jurisdictions = []string{"GB", "US"}
	})
	gate(t, "gate6 processor without instruction ref", "PRV-017", "processor_refs", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) {
		q.PrivacyRole = string(domain.RoleProcessor)
	})

	// Gate 7 — notice/consent dependency explicit; Gate 8 — DPIA/TIA resolved.
	gate(t, "gate7 notice/consent implicit", "PRV-006", "notice_consent_dependency", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.NoticeConsentDependency = "" })
	gate(t, "gate8 dpia/tia unresolved", "PRV-016", "dpia_tia_status", func(q *domain.CreateActivityRequest, _ *domain.PurposeVersion) { q.DPIATIAStatus = "" })
}

func TestActivationGates_SatisfiedVariantsPass(t *testing.T) {
	cases := map[string]func(*domain.CreateActivityRequest){
		"sensitive data with a special condition (see purpose option)": nil,
		"cross-border with transfer refs": func(q *domain.CreateActivityRequest) {
			q.Jurisdictions, q.TransferRefs = []string{"GB", "US"}, []string{"transfer-scc-2026"}
		},
		"processor with instruction ref": func(q *domain.CreateActivityRequest) {
			q.PrivacyRole, q.ProcessorRefs = string(domain.RoleProcessor), []string{"processor-relationship-1"}
		},
		"DPIA marked review-required is acceptable": func(q *domain.CreateActivityRequest) { q.DPIATIAStatus = string(domain.DPIATIAReviewRequired) },
		"regime jurisdiction":                       func(q *domain.CreateActivityRequest) { q.Jurisdictions = []string{"EU"} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRouter(newStubStore(), &stubAuthz{})
			p := newPurpose(t, r, tenantA)
			req := goodActivity(p.PurposeID)
			if mut != nil {
				mut(&req)
			}
			v := newDraft(t, r, tenantA, req)
			v = step(t, r, tenantA, v, "validate", maker, nil, http.StatusOK)
			if v.VersionStatus != domain.ActivityStatusValidated {
				t.Fatalf("expected VALIDATED, findings: %+v", v.ValidationFindings)
			}
		})
	}

	t.Run("sensitive data with a special-condition reference", func(t *testing.T) {
		r := newRouter(newStubStore(), &stubAuthz{})
		p := newPurpose(t, r, tenantA, func(c *domain.CreatePurposeRequest) { c.SpecialConditionRefs = []string{"art9-2a-explicit-consent"} })
		req := goodActivity(p.PurposeID)
		req.DataCategories = []string{"CONTACT_INFO", "HEALTH"}
		v := step(t, r, tenantA, newDraft(t, r, tenantA, req), "validate", maker, nil, http.StatusOK)
		if v.VersionStatus != domain.ActivityStatusValidated {
			t.Fatalf("expected VALIDATED, findings: %+v", v.ValidationFindings)
		}
	})
}

func TestRegistryValues_AreCanonicalisedOnWrite(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	req := goodActivity(p.PurposeID)
	req.Jurisdictions = []string{" gb "}
	req.DataCategories = []string{"contact_info"}
	v := newDraft(t, r, tenantA, req)
	if v.Jurisdictions[0] != "GB" || v.DataCategories[0] != "CONTACT_INFO" {
		t.Fatalf("registry codes were stored un-normalised: %+v / %+v", v.Jurisdictions, v.DataCategories)
	}
	// The RoPA jurisdiction filter is case-insensitive from the caller's side.
	toActive(t, r, tenantA, v)
	ropa := decode[struct{ Count int }](t, call(r, http.MethodGet, "/privacy/ropa?jurisdiction=gb", nil, tenantA, "reader-01"))
	if ropa.Count != 1 {
		t.Fatalf("ropa jurisdiction filter missed the row: %d", ropa.Count)
	}
}

func TestActivation_RechecksGatesAgainstTheLiveRegistries(t *testing.T) {
	st := newStubStore()
	r := newRouter(st, &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	prior := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))

	// A successor that passes validation, is approved, and is then found to
	// carry a value the registry no longer recognises (the shape of a legacy
	// row validated under the old "any two letters" jurisdiction rule).
	req := goodActivity(p.PurposeID)
	next := toApproved(t, r, tenantA, newDraftVersion(t, r, tenantA, prior, req))
	st.activityVersions[next.ActivityVersionID].Jurisdictions = []string{"ZZ"}

	w := call(r, http.MethodPost, versionPath(next, "activate"), nil, tenantA, ops)
	e := wantErr(t, w, http.StatusUnprocessableEntity, "PRV-004")
	if len(e.Findings) == 0 || e.Findings[0].Field != "jurisdictions" {
		t.Fatalf("refusal must carry the findings: %+v", e)
	}
	if got := st.activityVersions[next.ActivityVersionID].VersionStatus; got != domain.ActivityStatusApproved {
		t.Fatalf("refused version changed state: %s", got)
	}
	if got := st.activityVersions[prior.ActivityVersionID].VersionStatus; got != domain.ActivityStatusActive {
		t.Fatalf("a refused activation retired the governing version: %s", got)
	}
}

// newDraftVersion creates a successor draft of prior from req's content.
func newDraftVersion(t *testing.T, r http.Handler, scope string, prior *domain.ProcessingActivityVersion, req domain.CreateActivityRequest) *domain.ProcessingActivityVersion {
	t.Helper()
	w := call(r, http.MethodPost, activitiesPath+"/"+prior.ActivityID+"/versions", domain.CreateActivityVersionRequest{
		ParentVersionID: prior.ActivityVersionID, PrivacyRole: req.PrivacyRole, Owner: req.Owner, PrivacyOwner: req.PrivacyOwner,
		PurposeIDs: req.PurposeIDs, SubjectClasses: req.SubjectClasses, DataCategories: req.DataCategories,
		Sources: req.Sources, Recipients: req.Recipients, Jurisdictions: req.Jurisdictions,
		RetentionRuleRefs: req.RetentionRuleRefs, TransferRefs: req.TransferRefs, ProcessorRefs: req.ProcessorRefs,
		NoticeConsentDependency: req.NoticeConsentDependency, DPIATIAStatus: req.DPIATIAStatus,
	}, scope, maker)
	wantOK(t, w, http.StatusCreated)
	return ptr(decode[domain.ProcessingActivityVersion](t, w))
}

// ── item 6: effective time ───────────────────────────────────────────────────

func TestActivation_EffectiveFromCannotBeBackdatedOrPostdated(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v := toApproved(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))

	for name, at := range map[string]time.Time{
		"backdated 1h":  time.Now().Add(-time.Hour),
		"backdated 5y":  time.Now().AddDate(-5, 0, 0),
		"post-dated 1h": time.Now().Add(time.Hour),
	} {
		w := call(r, http.MethodPost, versionPath(v, "activate"), domain.ActivateActivityRequest{EffectiveFrom: &at}, tenantA, ops)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: HTTP %d %s", name, w.Code, w.Body.String())
		}
		wantErr(t, w, http.StatusUnprocessableEntity, domain.ErrCodeEffectiveTime)
	}
	// Still APPROVED after every refusal.
	if got := decode[domain.ProcessingActivityVersion](t, call(r, http.MethodGet, versionURL(v), nil, tenantA, "r")).VersionStatus; got != domain.ActivityStatusApproved {
		t.Fatalf("refusals changed state to %s", got)
	}

	// "Now" (within clock skew) is accepted; the RECORDED time is the server's.
	asserted := time.Now().Add(-5 * time.Second)
	got := step(t, r, tenantA, v, "activate", ops, domain.ActivateActivityRequest{EffectiveFrom: &asserted}, http.StatusOK)
	if got.EffectiveFrom == nil || got.EffectiveFrom.Equal(asserted) || time.Since(*got.EffectiveFrom) > time.Minute {
		t.Fatalf("effective_from must be the server's activation instant, got %v (asserted %v)", got.EffectiveFrom, asserted)
	}
}

// ── item 7: one ACTIVE version; supersession ─────────────────────────────────

func TestActivation_RetiresThePreviousActiveVersion(t *testing.T) {
	st := newStubStore()
	r := newRouter(st, &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v1 := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))

	v2 := toActive(t, r, tenantA, newDraftVersion(t, r, tenantA, v1, goodActivity(p.PurposeID)))

	old := st.activityVersions[v1.ActivityVersionID]
	if old.VersionStatus != domain.ActivityStatusRetired || old.EffectiveTo == nil {
		t.Fatalf("previous ACTIVE not retired with a closed window: %+v", old)
	}
	if !old.EffectiveTo.Equal(*v2.EffectiveFrom) && old.EffectiveTo.After(*v2.EffectiveFrom) {
		t.Fatalf("windows overlap: old ends %v, new starts %v", old.EffectiveTo, v2.EffectiveFrom)
	}
	active := 0
	for _, v := range st.activityVersions {
		if v.ActivityID == v1.ActivityID && v.VersionStatus == domain.ActivityStatusActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("exactly one ACTIVE version required, found %d", active)
	}
	ropa := decode[struct{ Count int }](t, call(r, http.MethodGet, "/privacy/ropa", nil, tenantA, "reader-01"))
	if ropa.Count != 1 {
		t.Fatalf("RoPA must list one active version per activity, got %d", ropa.Count)
	}
}

func TestActivation_AlsoRetiresASuspendedSibling(t *testing.T) {
	st := newStubStore()
	r := newRouter(st, &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v1 := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))
	step(t, r, tenantA, v1, "suspend", ops, nil, http.StatusOK)

	toActive(t, r, tenantA, newDraftVersion(t, r, tenantA, v1, goodActivity(p.PurposeID)))

	if got := st.activityVersions[v1.ActivityVersionID].VersionStatus; got != domain.ActivityStatusRetired {
		t.Fatalf("a SUSPENDED sibling must be retired by the new activation, is %s", got)
	}
	// So it can never be resumed alongside the new ACTIVE version.
	wantErr(t, call(r, http.MethodPost, versionPath(v1, "resume"), nil, tenantA, ops), http.StatusConflict, domain.ErrCodeInvalidTransition)
}

// ── item 12: as_of / bitemporal behaviour ────────────────────────────────────

func TestAsOf_InvalidValuesAreRefused_NotSilentlyIgnored(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))

	for _, q := range []string{"as_of=yesterday", "as_of=2026-13-45", "as_of=1700000000", "as_of=", "known_as_of=soon", "as_of=2026-01-01"} {
		for _, path := range []string{purposesPath + "/" + p.PurposeID, activitiesPath + "/" + v.ActivityID} {
			w := call(r, http.MethodGet, path+"?"+q, nil, tenantA, "reader-01")
			if q == "as_of=" {
				continue // an empty value is "absent", same as not sending it
			}
			wantErr(t, w, http.StatusBadRequest, domain.ErrCodeInvalidAsOf)
		}
	}
	// A well-formed RFC 3339 value is accepted.
	ts := url.QueryEscape(time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
	wantOK(t, call(r, http.MethodGet, activitiesPath+"/"+v.ActivityID+"?as_of="+ts, nil, tenantA, "reader-01"), http.StatusOK)
}

func TestAsOf_ResolvesTheVersionThatGovernedThatInstant(t *testing.T) {
	st := newStubStore()
	r := newRouter(st, &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v1 := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))
	v2 := toActive(t, r, tenantA, newDraftVersion(t, r, tenantA, v1, goodActivity(p.PurposeID)))

	// Build a deterministic timeline directly on the rows:
	//   v1 governed [now-3h, now-1h)   v2 governs [now-1h, ∞)
	now := time.Now().UTC()
	h := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	a, b := st.activityVersions[v1.ActivityVersionID], st.activityVersions[v2.ActivityVersionID]
	a.EffectiveFrom, a.EffectiveTo, a.ApprovedAt = h(-3*time.Hour), h(-time.Hour), h(-4*time.Hour)
	b.EffectiveFrom, b.EffectiveTo, b.ApprovedAt = h(-time.Hour), nil, h(-90*time.Minute)

	get := func(q string) *domain.ProcessingActivityVersion {
		w := call(r, http.MethodGet, activitiesPath+"/"+v1.ActivityID+"?"+q, nil, tenantA, "reader-01")
		if w.Code != http.StatusOK {
			return nil
		}
		return ptr(decode[domain.ProcessingActivityVersion](t, w))
	}
	at := func(d time.Duration) string { return "as_of=" + url.QueryEscape(now.Add(d).Format(time.RFC3339Nano)) }

	if got := get(at(-2 * time.Hour)); got == nil || got.ActivityVersionID != v1.ActivityVersionID {
		t.Fatalf("2h ago v1 governed, got %+v", got)
	}
	if got := get(at(-30 * time.Minute)); got == nil || got.ActivityVersionID != v2.ActivityVersionID {
		t.Fatalf("30m ago v2 governed, got %+v", got)
	}
	if got := get(""); got == nil || got.ActivityVersionID != v2.ActivityVersionID {
		t.Fatalf("now v2 governs, got %+v", got)
	}
	// Before anything governed: PRV-002, not "the oldest we have".
	wantErr(t, call(r, http.MethodGet, activitiesPath+"/"+v1.ActivityID+"?"+at(-5*time.Hour), nil, tenantA, "reader-01"), http.StatusNotFound, domain.ErrCodeActivityInactive)

	// Knowledge time: asked 'as we knew it 3h ago' — v1 was approved 4h ago, so
	// it is known; v2 (approved 90m ago) is not yet known.
	if got := get(at(-30*time.Minute) + "&known_as_of=" + url.QueryEscape(now.Add(-3*time.Hour).Format(time.RFC3339Nano))); got != nil {
		t.Fatalf("v2 was not yet approved 3h ago; knowledge-time query must not return it, got %+v", got)
	}
}

func TestRetiredActivity_NoLongerGovernsNow_ButIsHistoricallyResolvable(t *testing.T) {
	st := newStubStore()
	r := newRouter(st, &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))
	before := time.Now().UTC()
	time.Sleep(15 * time.Millisecond)
	step(t, r, tenantA, v, "retire", ops, nil, http.StatusOK)
	time.Sleep(15 * time.Millisecond)

	wantErr(t, call(r, http.MethodGet, activitiesPath+"/"+v.ActivityID, nil, tenantA, "r"), http.StatusNotFound, domain.ErrCodeActivityInactive)
	w := call(r, http.MethodGet, activitiesPath+"/"+v.ActivityID+"?as_of="+url.QueryEscape(before.Format(time.RFC3339Nano)), nil, tenantA, "r")
	wantOK(t, w, http.StatusOK)
}

// ── RoPA ─────────────────────────────────────────────────────────────────────

func TestROPA_OnlyActiveVersions_AndFilterValidation(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	newDraft(t, r, tenantA, goodActivity(p.PurposeID))
	toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))

	got := decode[struct{ Count int }](t, call(r, http.MethodGet, "/privacy/ropa", nil, tenantA, "r"))
	if got.Count != 1 {
		t.Fatalf("only ACTIVE versions belong in the RoPA, got %d", got.Count)
	}
	wantErr(t, call(r, http.MethodGet, "/privacy/ropa?role=WIZARD", nil, tenantA, "r"), http.StatusBadRequest, domain.ErrCodeValidationFailed)
	none := decode[struct{ Count int }](t, call(r, http.MethodGet, "/privacy/ropa?role=PROCESSOR", nil, tenantA, "r"))
	if none.Count != 0 {
		t.Fatalf("role filter ignored")
	}
}

// ── input hygiene ────────────────────────────────────────────────────────────

func TestActivityCreate_RejectsBadEnumerations(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	for name, mut := range map[string]func(*domain.CreateActivityRequest){
		"role":   func(q *domain.CreateActivityRequest) { q.PrivacyRole = "WIZARD" },
		"owner":  func(q *domain.CreateActivityRequest) { q.Owner = "" },
		"notice": func(q *domain.CreateActivityRequest) { q.NoticeConsentDependency = "MAYBE" },
		"dpia":   func(q *domain.CreateActivityRequest) { q.DPIATIAStatus = "EXEMPT" },
	} {
		req := goodActivity()
		mut(&req)
		w := call(r, http.MethodPost, activitiesPath, req, tenantA, maker)
		e := wantErr(t, w, http.StatusBadRequest, domain.ErrCodeValidationFailed)
		if !strings.Contains(strings.ToLower(e.Error), strings.ToLower(strings.Split(name, " ")[0])) && name != "dpia" {
			t.Logf("%s: message %q", name, e.Error)
		}
	}
}

// A blank business owner is refused at creation, so gate 1's owner finding is
// only reachable through a row that predates that rule. Prove both halves.
func TestGate1_BusinessOwner_RefusedAtCreate_AndCaughtOnLegacyRows(t *testing.T) {
	st := newStubStore()
	r := newRouter(st, &stubAuthz{})
	p := newPurpose(t, r, tenantA)

	req := goodActivity(p.PurposeID)
	req.Owner = "   "
	wantErr(t, call(r, http.MethodPost, activitiesPath, req, tenantA, maker), http.StatusBadRequest, domain.ErrCodeValidationFailed)

	v := newDraft(t, r, tenantA, goodActivity(p.PurposeID))
	st.activityVersions[v.ActivityVersionID].Owner = "" // legacy row
	v = step(t, r, tenantA, v, "validate", maker, nil, http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusDraft {
		t.Fatalf("a version with no business owner must not validate")
	}
	found := false
	for _, f := range v.ValidationFindings {
		found = found || (f.Code == "PRV-003" && f.Field == "owner")
	}
	if !found {
		t.Fatalf("expected PRV-003 on owner, got %+v", v.ValidationFindings)
	}
}
