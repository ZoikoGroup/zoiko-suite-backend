//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
	"zoiko.io/access-control-svc/internal/store"
)

// P0-2 / Doc 03 §8.2 (J-06): jurisdiction-rules-svc authorizes
// POST /v1/validate-action as JURISDICTION_RULE_VALIDATE. Before 000023 the
// action had no permission definition, so no bundle could name it and the
// endpoint answered 403 to every caller forever — the failure the plan §2 R5
// describes.
func TestJurisdictionRuleValidate_IsRegisteredAndGrantedByItsTemplate(t *testing.T) {
	s := store.New(appPool)
	ctx := svcmiddleware.WithTenant(context.Background(), tenantGov)

	cat := store.NewPermissionCatalogue(s)
	got, err := cat.Lookup(ctx, []string{"JURISDICTION_RULE_VALIDATE"})
	require.NoError(t, err)
	def, ok := got["JURISDICTION_RULE_VALIDATE"]
	require.True(t, ok, "JURISDICTION_RULE_VALIDATE must be registered or no bundle can ever grant it")
	require.Equal(t, domain.RiskStandard, def.RiskTier)
	require.False(t, def.Protected, "the validation read is not a platform-admin action")

	// The template a tenant instantiates carries the action, so a principal
	// assigned a role made from it is granted it.
	v, err := s.GetTemplateVersion(ctx, "JURISDICTION_COMPLIANCE_OFFICER", 0)
	require.NoError(t, err)
	require.Contains(t, v.PermittedActions, "JURISDICTION_RULE_VALIDATE")

	roleID, cid := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	role := &domain.RoleDefinition{
		RoleDefinitionID: roleID, TenantID: tenantGov, RoleCode: "JCO_" + roleID[:8], RoleName: "Jurisdiction Compliance Officer",
		RoleScopeType: "TENANT", Status: domain.RoleStatusActive, CreatedByPrincipalID: "p", CorrelationID: cid,
		CreatedAt: now, UpdatedAt: now, TemplateCode: "JURISDICTION_COMPLIANCE_OFFICER", TemplateVersion: v.TemplateVersion,
	}
	bundle := &domain.PermissionBundleDef{
		BundleID: uuid.NewString(), TenantID: tenantGov, RoleDefinitionID: roleID, BundleCode: "TEMPLATE",
		PermittedActions: v.PermittedActions, ActiveFlag: true, CorrelationID: cid, CreatedAt: now, UpdatedAt: now,
		TemplateCode: "JURISDICTION_COMPLIANCE_OFFICER", TemplateVersion: v.TemplateVersion,
	}
	created, err := s.CreateTemplateRole(ctx, role, bundle, "p")
	require.NoError(t, err)
	require.True(t, created)

	gotBundle, err := s.GetTemplateBundle(ctx, roleID)
	require.NoError(t, err)
	require.Contains(t, gotBundle.PermittedActions, "JURISDICTION_RULE_VALIDATE",
		"an assigned role made from the template must carry the action the route checks")
}
