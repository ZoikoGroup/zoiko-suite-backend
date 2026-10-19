package handler_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/access-control-svc/internal/domain"
)

// An identity-provider mapping (INT-10) obtains baseline access or nothing.

func federationRequest(g *govRig, caller, target, roleID, origin string) (int, []byte) {
	body := map[string]any{
		"legal_entity_id": "le-1", "target_principal_id": target, "role_definition_id": roleID,
		"justification": "federation mapping", "correlation_id": uuid.NewString(),
	}
	if origin != "" {
		body["origin"] = origin
	}
	rr := doReq(g.r, http.MethodPost, "/v1/iam/access-assignments/", body, caller)
	return rr.Code, rr.Body.Bytes()
}

func TestFederationOrigin_BaselineIsProvisionedLikeAnyOther(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	code, raw := federationRequest(g, "svc-identity", "user-7", role.RoleDefinitionID, domain.OriginFederation)
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d %s", code, raw)
	}
	if a := decodeBody[domain.AssignmentRequest](t, raw); a.Status != domain.AssignmentProvisioned || a.RiskTier != domain.RiskStandard {
		t.Fatalf("%+v", a)
	}
	if len(g.asg.creates) != 1 {
		t.Fatal("a baseline federation request is provisioned")
	}
}

func TestFederationOrigin_PrivilegedIsRefusedNotQueued(t *testing.T) {
	g := newGovRig()
	for name, action := range map[string]string{"critical": "payment.release", "protected": "iam.assignment.grant"} {
		role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, action)

		// control: the same request with no federation origin waits for an approver, as it always did
		if code, _ := federationRequest(g, "svc-identity", "user-ctl-"+name, role.RoleDefinitionID, ""); code != http.StatusAccepted {
			t.Fatalf("%s control: want 202, got %d", name, code)
		}
		queued := len(g.gov.assignments)

		code, raw := federationRequest(g, "svc-identity", "user-fed-"+name, role.RoleDefinitionID, domain.OriginFederation)
		if code != http.StatusConflict || govCode(raw) != "federation_privileged_refused" {
			t.Fatalf("%s: want 409 federation_privileged_refused, got %d %s", name, code, raw)
		}
		if len(g.gov.assignments) != queued {
			t.Fatalf("%s: a refused federation request must not open an approval path (queued %d -> %d)", name, queued, len(g.gov.assignments))
		}
	}
	if len(g.asg.creates) != 0 {
		t.Fatal("nothing privileged was provisioned")
	}
}

func TestFederationOrigin_HighRiskIsRefusedToo(t *testing.T) {
	g := newGovRig()
	g.tax.risk["ledger.adjust"] = domain.RiskHigh
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create", "ledger.adjust")
	code, raw := federationRequest(g, "svc-identity", "user-7", role.RoleDefinitionID, domain.OriginFederation)
	if code != http.StatusConflict || govCode(raw) != "federation_privileged_refused" {
		t.Fatalf("a single HIGH action makes the role non-baseline: %d %s", code, raw)
	}
}

func TestFederationOrigin_UnclassifiedActionIsNotBaseline(t *testing.T) {
	g := newGovRig()
	g.tax.unknown = map[string]bool{"mystery.action": true}
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "mystery.action")
	code, raw := federationRequest(g, "svc-identity", "user-7", role.RoleDefinitionID, domain.OriginFederation)
	if code != http.StatusConflict || govCode(raw) != "federation_privileged_refused" {
		t.Fatalf("an action the taxonomy cannot rate is treated as high risk: %d %s", code, raw)
	}
}

func TestFederationOrigin_SelfRequestIsRefused(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	code, raw := federationRequest(g, "user-7", "user-7", role.RoleDefinitionID, domain.OriginFederation)
	if code != http.StatusConflict || govCode(raw) != "federation_privileged_refused" {
		t.Fatalf("a mapping cannot elevate the account it runs as: %d %s", code, raw)
	}
	if len(g.gov.assignments) != 0 {
		t.Fatal("nothing queued")
	}
}

func TestFederationOrigin_UnknownOriginIsRefused(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	code, raw := federationRequest(g, "svc-identity", "user-7", role.RoleDefinitionID, "SOMETHING_ELSE")
	if code != http.StatusBadRequest || govCode(raw) != "invalid_origin" {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestFederationOrigin_StillSubjectToSoDAndTheRoleBeingActive(t *testing.T) {
	g := newGovRig()
	g.sod.err = &domain.SoDConflictError{Conflicts: []domain.SoDConflict{{CandidateAction: "supplier_invoice.create", ConflictsWith: "supplier_invoice.approve", Source: "held"}}}
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	if code, raw := federationRequest(g, "svc-identity", "user-7", role.RoleDefinitionID, domain.OriginFederation); code != http.StatusForbidden || govCode(raw) != "sod_conflict" {
		t.Fatalf("SoD still applies: %d %s", code, raw)
	}
	g.sod.err = nil
	retired := g.seedRole("LEGAL_ENTITY", domain.RoleStatusRetired, "supplier_invoice.create")
	if code, raw := federationRequest(g, "svc-identity", "user-7", retired.RoleDefinitionID, domain.OriginFederation); code != http.StatusConflict || govCode(raw) != "role_retired" {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestFederationOrigin_NeedsTheGrantToRequestAtAll(t *testing.T) {
	g := newGovRig()
	g.authz.deny["ROLE_MANAGE"] = true
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	if code, _ := federationRequest(g, "svc-identity", "user-7", role.RoleDefinitionID, domain.OriginFederation); code != http.StatusForbidden {
		t.Fatalf("the caller must hold ROLE_MANAGE: %d", code)
	}
}

func eligibility(g *govRig, roleID string) (int, []byte) {
	rr := doReq(g.r, http.MethodGet, "/v1/iam/federation/role-eligibility?role_definition_id="+roleID, nil, "svc-identity")
	return rr.Code, rr.Body.Bytes()
}

func TestRoleEligibility(t *testing.T) {
	g := newGovRig()
	baseline := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create", "supplier_invoice.read")
	critical := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create", "payment.release")
	retired := g.seedRole("LEGAL_ENTITY", domain.RoleStatusRetired, "supplier_invoice.create")

	code, raw := eligibility(g, baseline.RoleDefinitionID)
	if e := decodeBody[domain.FederationRoleEligibility](t, raw); code != http.StatusOK || !e.Eligible || e.RiskTier != domain.RiskStandard {
		t.Fatalf("baseline: %d %+v", code, e)
	}
	code, raw = eligibility(g, critical.RoleDefinitionID)
	if e := decodeBody[domain.FederationRoleEligibility](t, raw); code != http.StatusOK || e.Eligible || e.RiskTier != domain.RiskCritical || len(e.Drivers) == 0 {
		t.Fatalf("critical: %d %+v", code, e)
	}
	code, raw = eligibility(g, retired.RoleDefinitionID)
	if e := decodeBody[domain.FederationRoleEligibility](t, raw); code != http.StatusOK || e.Eligible {
		t.Fatalf("retired: %d %+v", code, e)
	}
	if code, _ := eligibility(g, uuid.NewString()); code != http.StatusNotFound {
		t.Fatalf("unknown role: %d", code)
	}
	if rr := doReq(g.r, http.MethodGet, "/v1/iam/federation/role-eligibility", nil, "svc-identity"); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing parameter: %d", rr.Code)
	}
	if rr := doReq(g.r, http.MethodGet, "/v1/iam/federation/role-eligibility?role_definition_id="+baseline.RoleDefinitionID, nil, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("an unidentified caller: %d", rr.Code)
	}
}

func TestRoleEligibility_TaxonomyOutageIsNeverEligible(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	g.tax.err = errors.New("taxonomy down")
	code, raw := eligibility(g, role.RoleDefinitionID)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("a risk that cannot be rated is 503, not eligible: %d %s", code, raw)
	}
	if e := decodeBody[domain.FederationRoleEligibility](t, raw); e.Eligible {
		t.Fatal("never eligible on an outage")
	}
}
