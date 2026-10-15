//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
	"zoiko.io/access-control-svc/internal/store"
)

// Migrations 000008-000012 and the governance store, through the NOBYPASSRLS
// app pool so the policies actually apply.
//
// A tenant of their own: the isolation suite counts tenant A's outbox rows, and
// these tests enqueue events.
const tenantGov = "tenant-gov"

// Fingerprints are SHA-256 hex in production; the column is CHAR(64).
var fp1, fp2 = strings.Repeat("a", 64), strings.Repeat("b", 64)

func TestTaxonomyLookupIsExactAndRated(t *testing.T) {
	cat := store.NewPermissionCatalogue(store.New(appPool))
	got, err := cat.Lookup(svcmiddleware.WithTenant(context.Background(), tenantGov),
		[]string{"payment.release", "Payment.Release", "ROLE_MANAGE", "payment.relase"})
	require.NoError(t, err)
	require.Contains(t, got, "payment.release")
	require.Equal(t, domain.RiskCritical, got["payment.release"].RiskTier)
	require.NotContains(t, got, "Payment.Release", "matching is exact: a near miss grants nothing")
	require.NotContains(t, got, "payment.relase")
	require.True(t, got["ROLE_MANAGE"].Protected, "legacy protected names are flagged")
}

func TestPlatformCataloguesAreNotWritableByTheApp(t *testing.T) {
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO permission_definitions (action_name, naming) VALUES ('x.y', 'TAXONOMY')`,
		`INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type) VALUES ('X', 'x', 'x', 'TENANT')`,
	} {
		err := pgx.BeginFunc(ctx, appPool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		require.Error(t, err, stmt)
	}
}

func TestTemplateVersionsAreImmutableEvenForTheOwner(t *testing.T) {
	_, err := ownerPool.Exec(context.Background(), `UPDATE role_template_versions SET permitted_actions = '{payment.release}' WHERE template_code = 'TREASURY_PREPARER'`)
	require.ErrorContains(t, err, "append-only")
	_, err = ownerPool.Exec(context.Background(), `DELETE FROM role_template_versions WHERE template_code = 'TREASURY_PREPARER'`)
	require.ErrorContains(t, err, "append-only")
}

func TestTemplatesSeedTheArchetypes(t *testing.T) {
	s := store.New(appPool)
	ctx := svcmiddleware.WithTenant(context.Background(), tenantGov)
	list, err := s.ListTemplates(ctx)
	require.NoError(t, err)
	archetypes := 0
	for _, tpl := range list {
		if tpl.IsArchetype {
			archetypes++
			require.Equal(t, 1, tpl.LatestVersion)
			require.NotEmpty(t, tpl.LatestActions)
		}
	}
	require.Equal(t, 21, archetypes)
	v, err := s.GetTemplateVersion(ctx, "TREASURY_RELEASER", 0)
	require.NoError(t, err)
	require.Equal(t, []string{"payment.read", "payment.release"}, v.PermittedActions)
	_, err = s.GetTemplateVersion(ctx, "TREASURY_RELEASER", 9)
	require.ErrorIs(t, err, domain.ErrTemplateVersionNotFound)
}

func TestTemplateRoleCreateAndUpgradeEnqueueTheirEvents(t *testing.T) {
	s := store.New(appPool)
	ctx := svcmiddleware.WithTenant(context.Background(), tenantGov)
	now := time.Now().UTC()
	roleID, cid := uuid.NewString(), uuid.NewString()
	role := &domain.RoleDefinition{RoleDefinitionID: roleID, TenantID: tenantGov, RoleCode: "TPL_" + roleID[:8], RoleName: "t",
		RoleScopeType: "LEGAL_ENTITY", Status: domain.RoleStatusActive, CreatedByPrincipalID: "p", CorrelationID: cid,
		CreatedAt: now, UpdatedAt: now, TemplateCode: "TREASURY_PREPARER", TemplateVersion: 1}
	bundle := &domain.PermissionBundleDef{BundleID: uuid.NewString(), TenantID: tenantGov, RoleDefinitionID: roleID, BundleCode: "TEMPLATE",
		PermittedActions: []string{"payment.create"}, ActiveFlag: true, CorrelationID: cid, CreatedAt: now, UpdatedAt: now,
		TemplateCode: "TREASURY_PREPARER", TemplateVersion: 1}
	created, err := s.CreateTemplateRole(ctx, role, bundle, "p")
	require.NoError(t, err)
	require.True(t, created)

	again := *role
	againBundle := domain.PermissionBundleDef{}
	again.RoleDefinitionID = uuid.NewString()
	created, err = s.CreateTemplateRole(ctx, &again, &againBundle, "p")
	require.NoError(t, err)
	require.False(t, created, "a replay")
	require.Equal(t, roleID, again.RoleDefinitionID)
	require.Equal(t, bundle.BundleID, againBundle.BundleID)

	got, err := s.GetTemplateBundle(ctx, roleID)
	require.NoError(t, err)
	require.Equal(t, "TREASURY_PREPARER", got.TemplateCode)

	_, err = s.UpgradeTemplateRole(ctx, roleID, 1, []string{"payment.create"}, "p")
	require.ErrorIs(t, err, domain.ErrTemplateVersionNotNewer)
	out, err := s.UpgradeTemplateRole(ctx, roleID, 2, []string{"payment.create", "payment.submit"}, "p")
	require.NoError(t, err)
	require.Equal(t, 2, out.Role.TemplateVersion)
	require.Equal(t, []string{"payment.create", "payment.submit"}, out.Bundle.PermittedActions)

	var published, roleUpdated int
	require.NoError(t, ownerPool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE event_type='iam.role.published'), count(*) FILTER (WHERE event_type='role.updated')
		   FROM event_outbox WHERE aggregate_key = $1`, roleID).Scan(&published, &roleUpdated))
	require.Equal(t, 2, published, "instantiation and upgrade")
	require.GreaterOrEqual(t, roleUpdated, 2)
}

func TestAssignmentRequestsAreTenantIsolatedAndIndependent(t *testing.T) {
	s := store.New(appPool)
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantGov)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)
	roleID := seedRole(t, tenantGov, "ASG_"+uuid.NewString()[:8])
	now := time.Now().UTC()
	a := &domain.AssignmentRequest{RequestID: uuid.NewString(), TenantID: tenantGov, TargetPrincipalID: "subject",
		RoleDefinitionID: roleID, LegalEntityID: uuid.NewString(), EffectiveFrom: now, Justification: "j",
		RiskTier: domain.RiskCritical, ApprovalRequired: true, Status: domain.AssignmentPendingApproval,
		RequestedByPrincipalID: "requester", CorrelationID: uuid.NewString(), CreatedAt: now, UpdatedAt: now}
	created, err := s.CreateAssignmentRequest(ctxA, a, "requester")
	require.NoError(t, err)
	require.True(t, created)

	_, err = s.GetAssignmentRequest(ctxB, a.RequestID)
	require.ErrorIs(t, err, domain.ErrAssignmentNotFound, "another tenant cannot see it")

	_, err = s.DecideAssignmentRequest(ctxA, a.RequestID, domain.AssignmentProvisioned, "subject", "", a.RequestID)
	require.Error(t, err, "the CHECK refuses a self-approval even if the handler did not")
	_, err = s.DecideAssignmentRequest(ctxA, a.RequestID, domain.AssignmentProvisioned, "requester", "", a.RequestID)
	require.Error(t, err, "nor the requester's own approval")

	out, err := s.DecideAssignmentRequest(ctxA, a.RequestID, domain.AssignmentProvisioned, "approver", "ok", a.RequestID)
	require.NoError(t, err)
	require.Equal(t, domain.AssignmentProvisioned, out.Status)
	_, err = s.DecideAssignmentRequest(ctxA, a.RequestID, domain.AssignmentProvisioned, "approver-2", "ok", a.RequestID)
	require.ErrorIs(t, err, domain.ErrAssignmentState, "a second approval finds it no longer pending")

	rev, err := s.RevokeAssignmentRequest(ctxA, a.RequestID, "admin", "left")
	require.NoError(t, err)
	require.Equal(t, domain.AssignmentRevoked, rev.Status)
	var subject string
	require.NoError(t, ownerPool.QueryRow(context.Background(),
		`SELECT payload->'payload'->>'principal_id' FROM event_outbox WHERE event_type='iam.assignment.revoked' AND aggregate_key=$1`,
		a.RequestID).Scan(&subject))
	require.Equal(t, "subject", subject)
}

func TestCampaignCannotCloseOverUnresolvedHighRisk(t *testing.T) {
	s := store.New(appPool)
	ctx := svcmiddleware.WithTenant(context.Background(), tenantGov)
	now := time.Now().UTC()
	c := &domain.ReviewCampaign{CampaignID: uuid.NewString(), TenantID: tenantGov, CampaignName: "c", ReviewType: "PERIODIC",
		LegalEntityID: uuid.NewString(), DefaultReviewerPrincipalID: "rev", Status: domain.CampaignOpen,
		DueAt: now.Add(time.Hour), CreatedByPrincipalID: "admin", CorrelationID: uuid.NewString(), CreatedAt: now, UpdatedAt: now}
	high := domain.ReviewItem{ItemID: uuid.NewString(), AuthzAssignmentID: uuid.NewString(), TargetPrincipalID: "u1",
		RoleDefinitionID: uuid.NewString(), RoleCode: "R", GrantedActions: []string{"payment.release"}, RiskTier: domain.RiskCritical,
		Flags: []string{}, ReviewerPrincipalID: "rev", Status: domain.ReviewItemOpen, CreatedAt: now, UpdatedAt: now}
	std := high
	std.ItemID, std.AuthzAssignmentID, std.RiskTier, std.TargetPrincipalID = uuid.NewString(), uuid.NewString(), domain.RiskStandard, "u2"
	created, err := s.CreateCampaign(ctx, c, []domain.ReviewItem{high, std}, "admin")
	require.NoError(t, err)
	require.True(t, created)

	_, err = s.CompleteCampaign(ctx, c.CampaignID, "admin")
	require.ErrorIs(t, err, domain.ErrUnresolvedHighRisk)

	_, err = s.DecideReviewItem(ctx, c.CampaignID, high.ItemID, domain.DecisionEscalate, "unsure", "rev", false)
	require.NoError(t, err)
	_, err = s.CompleteCampaign(ctx, c.CampaignID, "admin")
	require.ErrorIs(t, err, domain.ErrUnresolvedHighRisk, "an escalated high-risk item still blocks")

	_, err = ownerPool.Exec(context.Background(), `UPDATE access_review_items SET decided_by_principal_id = target_principal_id WHERE item_id = $1`, high.ItemID)
	require.Error(t, err, "the CHECK refuses self-attestation")

	_, err = s.ReassignReviewItem(ctx, c.CampaignID, high.ItemID, "rev2")
	require.NoError(t, err)
	_, err = s.DecideReviewItem(ctx, c.CampaignID, high.ItemID, domain.DecisionKeep, "still needed", "rev2", false)
	require.NoError(t, err)
	done, err := s.CompleteCampaign(ctx, c.CampaignID, "admin")
	require.NoError(t, err)
	require.Equal(t, domain.CampaignCompleted, done.Status)
	require.Equal(t, 1, done.Summary["status_EXPIRED"], "the open STANDARD item is recorded, not dropped")
	_, err = s.DecideReviewItem(ctx, c.CampaignID, std.ItemID, domain.DecisionKeep, "late", "rev", false)
	require.ErrorIs(t, err, domain.ErrCampaignClosed)
}

func TestIdempotencyKeysBindTheFingerprint(t *testing.T) {
	s := store.New(appPool)
	ctx := context.Background()
	key := uuid.NewString()
	rec, err := s.ClaimIdempotencyKey(ctx, tenantGov, "POST /x", key, fp1)
	require.NoError(t, err)
	require.Nil(t, rec)
	require.NoError(t, s.CompleteIdempotencyKey(ctx, tenantGov, "POST /x", key, 201, []byte(`{"a":1}`)))
	rec, err = s.ClaimIdempotencyKey(ctx, tenantGov, "POST /x", key, fp1)
	require.NoError(t, err)
	require.Equal(t, 201, rec.ResponseStatus)
	_, err = s.ClaimIdempotencyKey(ctx, tenantGov, "POST /x", key, fp2)
	require.ErrorIs(t, err, store.ErrIdempotencyFingerprintMismatch)
	rec, err = s.ClaimIdempotencyKey(ctx, tenantB, "POST /x", key, fp2)
	require.NoError(t, err)
	require.Nil(t, rec, "keys are per tenant")
}
