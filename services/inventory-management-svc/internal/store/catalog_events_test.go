package store_test

import (
	"context"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
	"zoiko.io/inventory-management-svc/internal/store"
)

const offPrefix = "com.zoikosuite.business."

// The BIZ-07 spec names exactly five events. Their payload schemas ship in contracts/events, and
// this fails if one is added, renamed or dropped without the other. (This replaces the old check
// of the hand-written asyncapi.biz07.yaml, whose event names were the pre-standard ones.)
func TestCatalogEvents_ContractsAreTheFiveSpecEvents(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	entries, err := filepath.Glob(filepath.Join(filepath.Dir(file), "..", "..", "contracts", "events", "business.offering*.schema.json"))
	require.NoError(t, err)
	var got []string
	for _, e := range entries {
		got = append(got, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(e), "business."), ".schema.json"))
	}
	sort.Strings(got)
	assert.Equal(t, []string{"offering-version.superseded", "offering.activated", "offering.created", "offering.retired", "offering.suspended"}, got)
}

// Each BIZ-07 event is written in the same transaction as the change, to the shared eventing
// outbox; payload schemas are checked for every row by verifyEventContracts.
func TestCatalogEvents_WrittenWithEachChange(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("local"))
	tenantID, entity := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithCorrelation(svcmiddleware.WithTenant(context.Background(), tenantID), "corr-cat")

	o, v1 := newTestOffering(tenantID, entity, "SKU-EV")
	require.NoError(t, s.CreateOffering(ctx, o, v1, nil))
	require.Equal(t, []string{offPrefix + "offering.created"}, eventingTypes(t, pool, o.OfferingID))

	now := time.Now().UTC()
	require.NoError(t, s.ApproveOfferingVersion(ctx, v1.VersionID, "reviewer-1", now))
	linkRequiredMappings(t, ctx, s, v1.VersionID)
	_, err := s.ActivateOfferingVersion(ctx, o.OfferingID, v1.VersionID, "reviewer-1", now)
	require.NoError(t, err)

	// Activating a second version supersedes the first: activated for v2, superseded for v1.
	v2 := &domain.OfferingVersion{VersionID: uuid.New().String(), TenantID: tenantID, OfferingID: o.OfferingID,
		Description: "Revised", Unit: "HOUR", Status: domain.CatalogVersionStatusDraft, CreatedAt: now, CreatedByPrincipalID: "owner-1"}
	require.NoError(t, s.CreateVersion(ctx, v2, nil))
	require.NoError(t, s.ApproveOfferingVersion(ctx, v2.VersionID, "reviewer-1", now.Add(time.Hour)))
	linkRequiredMappings(t, ctx, s, v2.VersionID)
	_, err = s.ActivateOfferingVersion(ctx, o.OfferingID, v2.VersionID, "reviewer-1", now.Add(time.Hour))
	require.NoError(t, err)

	require.NoError(t, s.SuspendOfferingVersion(ctx, v2.VersionID, "reviewer-1", "quality hold", now.Add(2*time.Hour)))
	require.NoError(t, s.RetireOfferingVersion(ctx, v2.VersionID, "reviewer-1", "discontinued", now.Add(3*time.Hour)))

	require.Equal(t, []string{
		offPrefix + "offering.created",
		offPrefix + "offering.activated",
		offPrefix + "offering.activated",
		offPrefix + "offering-version.superseded",
		offPrefix + "offering.suspended",
		offPrefix + "offering.retired",
	}, eventingTypes(t, pool, o.OfferingID))

	var all string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT string_agg(convert_from(payload, 'UTF8'), ' ') FROM eventing_outbox`).Scan(&all))
	for _, want := range []string{"corr-cat", "reviewer-1", "quality hold", "discontinued", "SKU-EV", v1.VersionID, v2.VersionID} {
		assert.Contains(t, all, want)
	}

	// A refused transition writes no event: retiring an already-retired version.
	require.Error(t, s.RetireOfferingVersion(ctx, v2.VersionID, "reviewer-1", "again", now.Add(4*time.Hour)))
	require.Len(t, eventingTypes(t, pool, o.OfferingID), 6)
}

// A store with no event region cannot build the envelope, so the write is refused and rolled
// back: no offering, no event; and an activation that cannot write its event leaves the version
// APPROVED.
func TestCatalogEvents_MissingRegionRollsTheWriteBack(t *testing.T) {
	pool := openTestPool(t)
	noRegion := store.New(pool) // no WithEventRegion
	good := store.New(pool, store.WithEventRegion("local"))
	tenantID, entity := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o, v := newTestOffering(tenantID, entity, "SKU-NOREG")
	require.Error(t, noRegion.CreateOffering(ctx, o, v, nil))
	_, err := good.GetOffering(ctx, o.OfferingID)
	require.Error(t, err, "the offering must not exist when its event could not be written")
	var events int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_outbox`).Scan(&events))
	assert.Zero(t, events)

	o2, v2 := newTestOffering(tenantID, entity, "SKU-NOREG2")
	require.NoError(t, good.CreateOffering(ctx, o2, v2, nil))
	now := time.Now().UTC()
	require.NoError(t, good.ApproveOfferingVersion(ctx, v2.VersionID, "reviewer-1", now))
	linkRequiredMappings(t, ctx, good, v2.VersionID)
	_, err = noRegion.ActivateOfferingVersion(ctx, o2.OfferingID, v2.VersionID, "reviewer-1", now)
	require.Error(t, err)
	got, err := good.GetOfferingVersion(ctx, v2.VersionID)
	require.NoError(t, err)
	assert.Equal(t, domain.CatalogVersionStatusApproved, got.Status, "a failed event must roll the activation back")
}
