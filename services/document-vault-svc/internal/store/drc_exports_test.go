package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/store"
)

// POST /v1/exports (ZS-SVC-S-001 §9.2): recipient and purpose are stored with the
// package and announced in export_package_created (§9.3 recipient_scope).

func TestExport_RecipientAndPurpose_ArePersistedAndAnnounced(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "export-recipient")

	pkg, err := s.CreateExportPackage(tenantCtx(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: "exporter-1", RecipientScope: "regulator:FCA", Purpose: "regulatory request R-77",
		Items: []domain.ExportPackageItemInput{{ItemType: string(domain.ExportItemDocumentVersion), RefID: v.DocumentVersionID}},
	})
	require.NoError(t, err)
	require.Equal(t, "regulator:FCA", pkg.RecipientScope)
	require.Equal(t, "regulatory request R-77", pkg.Purpose)

	fetched, err := s.GetExportPackage(tenantCtx(), pkg.PackageID)
	require.NoError(t, err)
	require.Equal(t, "regulator:FCA", fetched.RecipientScope)
	require.Equal(t, "regulatory request R-77", fetched.Purpose)

	var raw []byte
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox_events WHERE aggregate_id::text = $1 AND event_type = 'export_package_created'`, pkg.PackageID).Scan(&raw))
	var env struct {
		Payload map[string]any `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	payload := env.Payload
	if payload == nil {
		require.NoError(t, json.Unmarshal(raw, &payload))
	}
	require.Equal(t, "regulator:FCA", payload["recipient_scope"], "the §9.3 anchor")
	require.Equal(t, "regulatory request R-77", payload["purpose"])
	require.Equal(t, pkg.PackageID, payload["package_id"])
}

func TestExport_WithoutRecipient_StaysAsBefore(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	_, v := createTestDocument(t, s, "export-no-recipient")
	item := []domain.ExportPackageItemInput{{ItemType: string(domain.ExportItemDocumentVersion), RefID: v.DocumentVersionID}}

	plain, err := s.CreateExportPackage(tenantCtx(), domain.CreateExportPackageParams{RequestedByPrincipalID: "exporter-1", Items: item})
	require.NoError(t, err)
	require.Empty(t, plain.RecipientScope)
	require.Empty(t, plain.Purpose)

	// The package hash identifies the exact items; recipient and purpose do not
	// change it.
	labelled, err := s.CreateExportPackage(tenantCtx(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: "exporter-1", RecipientScope: "r", Purpose: "p", Items: item})
	require.NoError(t, err)
	require.Equal(t, plain.PackageHash, labelled.PackageHash)
}

func TestExport_RecipientIsTenantScoped(t *testing.T) {
	s := store.New(requireTestDB(t), zap.NewNop())
	_, v := createTestDocument(t, s, "export-tenant")
	pkg, err := s.CreateExportPackage(tenantCtx(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: "exporter-1", RecipientScope: "r", Purpose: "p",
		Items: []domain.ExportPackageItemInput{{ItemType: string(domain.ExportItemDocumentVersion), RefID: v.DocumentVersionID}}})
	require.NoError(t, err)

	_, err = s.GetExportPackage(otherTenantCtx(), pkg.PackageID)
	require.ErrorIs(t, err, domain.ErrExportPackageNotFound)

	// Another tenant cannot export this tenant's version either: it does not exist for them.
	_, err = s.CreateExportPackage(otherTenantCtx(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: "exporter-9", RecipientScope: "r", Purpose: "p",
		Items: []domain.ExportPackageItemInput{{ItemType: string(domain.ExportItemDocumentVersion), RefID: v.DocumentVersionID}}})
	require.ErrorIs(t, err, domain.ErrExportItemRefNotFound)
}
