//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
	"zoiko.io/access-control-svc/internal/store"
)

// siem-integration-svc authorizes its exporter and event routes as
// SIEM_EXPORTER_CREATE, SIEM_EXPORTER_READ and SIEM_EVENT_READ. Before 000024
// none was registered, so no bundle could name them and every person was
// refused 403 forever (plan §2 R5).
func TestSIEMActions_AreRegisteredAndCarriedByATemplate(t *testing.T) {
	s := store.New(appPool)
	ctx := svcmiddleware.WithTenant(context.Background(), tenantGov)

	got, err := store.NewPermissionCatalogue(s).Lookup(ctx, []string{"SIEM_EXPORTER_CREATE", "SIEM_EXPORTER_READ", "SIEM_EVENT_READ"})
	require.NoError(t, err)
	for action, tier := range map[string]string{
		"SIEM_EXPORTER_CREATE": domain.RiskHigh,
		"SIEM_EXPORTER_READ":   domain.RiskStandard,
		"SIEM_EVENT_READ":      domain.RiskHigh,
	} {
		def, ok := got[action]
		require.True(t, ok, "%s must be registered or no bundle can ever grant it", action)
		require.Equal(t, tier, def.RiskTier, action)
	}

	analyst, err := s.GetTemplateVersion(ctx, "SECURITY_OPERATIONS_ANALYST", 0)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"SIEM_EXPORTER_READ", "SIEM_EVENT_READ"}, analyst.PermittedActions)

	admin, err := s.GetTemplateVersion(ctx, "SECURITY_SIEM_ADMINISTRATOR", 0)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"SIEM_EXPORTER_CREATE", "SIEM_EXPORTER_READ"}, admin.PermittedActions,
		"the administrator configures exporters but does not read the event stream")
}
