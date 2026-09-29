package registry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/authz"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

func ptr(s string) *string { return &s }

// ORG-03: "Legal-form mapping may use ISO 20275/ELF code where available,
// while preserving local legal-form text/source."
func TestLegalForm_ELFCodeNeedsItsSourceAndLocalText(t *testing.T) {
	for name, tc := range map[string]struct {
		code, source, local *string
		want                error
	}{
		"not an ELF code": {ptr("LTD"), ptr("GLEIF"), ptr("Limited"), registry.ErrInvalidInput},
		"no source":       {ptr("H0PO"), nil, ptr("Private limited company"), registry.ErrSourceUnverified},
		"no local text":   {ptr("H0PO"), ptr("GLEIF-ELF-1.6"), nil, registry.ErrInvalidInput},
		"complete, lower": {ptr("h0po"), ptr("GLEIF-ELF-1.6"), ptr("Private limited company"), nil},
	} {
		t.Run(name, func(t *testing.T) {
			svc, ms := baseSvc(t)
			seedEntityFor(ms, "ent-lf", orgTenant, "", "JUR-US")
			_, err := svc.AmendLegalProfile(tenantCtx(orgTenant), "ent-lf", domain.AmendLegalProfileRequest{
				LegalFormCode: tc.code, LegalFormSource: tc.source, LegalFormLocalText: tc.local,
			})
			if tc.want == nil {
				require.True(t, err == nil || isPending(err), "got %v", err)
				return
			}
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func isPending(err error) bool {
	var p *registry.PendingApprovalError
	return err != nil && errorsAs(err, &p)
}

// denyActions refuses the listed actions and permits the rest.
type denyActions map[string]bool

func (d denyActions) Authorize(_ context.Context, _, _, resource, action string) error {
	if d[resource+"."+action] {
		return authz.ErrUnauthorized
	}
	return nil
}

// Purpose limitation: quarantine rows hold a rejected claimant's payload and
// proposals hold command payloads; tenant membership is no longer enough.
func TestReads_QuarantineAndProposalsNeedTheirReadPermission(t *testing.T) {
	ms := newMemStore()
	svc := newSvc(t, ms, denyActions{"entity.registry-conflict.read": true, "approval-request.read": true}, acceptAllJurisd{})

	_, err := svc.ListRegistryConflicts(tenantCtx(orgTenant), true)
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	_, err = svc.ListApprovalRequests(tenantCtx(orgTenant), true)
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	_, err = svc.GetApprovalRequest(tenantCtx(orgTenant), "any")
	require.ErrorIs(t, err, registry.ErrUnauthorized)
}

// Deciding implies reading: an approver with only the .approve permission can
// still release a request.
func TestReads_DecidingDoesNotNeedTheSeparateReadPermission(t *testing.T) {
	ms := newMemStore()
	svc := newSvc(t, ms, denyActions{"approval-request.read": true}, acceptAllJurisd{})
	a := proposeTermination(t, svc, ms)
	_, err := svc.ApproveRequest(approverCtx(orgTenant), a.ApprovalRequestID,
		domain.ApproveRequestBody{PayloadFingerprint: a.PayloadFingerprint})
	require.NoError(t, err)
	assert.Equal(t, domain.ApprovalApproved, ms.approvals()[a.ApprovalRequestID].Status)
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }
