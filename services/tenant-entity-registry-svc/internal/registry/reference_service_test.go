package registry_test

// MDM-04 service: validation before authorization before the store, the right permission per command at the tenant, reads
// ungated and defaulting to now, and a store without reference releases answering "unavailable".

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type refFake struct {
	*memStore
	registry.ReferenceStore
	calls int
	actor string
	last  string
	asOf  time.Time
}

func (r *refFake) CreateReferenceSet(_ context.Context, _ domain.CreateReferenceSetRequest, actor string) (*domain.ReferenceSet, error) {
	r.calls++
	r.actor, r.last = actor, "create-set"
	return &domain.ReferenceSet{}, nil
}
func (r *refFake) StageReferenceRelease(_ context.Context, _ string, _ domain.StageReferenceReleaseRequest, actor string) (*domain.ReferenceRelease, error) {
	r.calls++
	r.actor, r.last = actor, "stage"
	return &domain.ReferenceRelease{}, nil
}
func (r *refFake) PublishReferenceRelease(_ context.Context, _, _ string, _ domain.PublishReferenceReleaseRequest, actor string) (*domain.ReferenceRelease, error) {
	r.calls++
	r.actor, r.last = actor, "publish"
	return &domain.ReferenceRelease{}, nil
}
func (r *refFake) ResolveReference(_ context.Context, q domain.ReferenceQuery) (*domain.ReferenceResolution, error) {
	r.calls++
	r.asOf = q.AsOf
	return &domain.ReferenceResolution{}, nil
}

func refSvc(t *testing.T, az registry.AuthorizationClient) (*registry.Service, *refFake) {
	t.Helper()
	f := &refFake{memStore: newMemStore()}
	return newSvc(t, f, az, acceptAllJurisd{}), f
}

func TestReference_EachCommandIsItsOwnPermissionAtTheTenant(t *testing.T) {
	stage := domain.StageReferenceReleaseRequest{ReleaseVersion: "1", SourcePublishedAt: "2026-01-01T00:00:00Z", EffectiveFrom: "2026-02-01", Compatibility: domain.CompatAutoAdopt,
		EvidenceRef: "e", Entries: []domain.ReferenceEntryInput{{Code: "GB", ChangeType: domain.ChangeAdd, Label: "UK"}}}
	for name, tc := range map[string]struct {
		action, last string
		run          func(*registry.Service) error
	}{
		"create-set": {"create-set", "create-set", func(s *registry.Service) error {
			_, err := s.CreateReferenceSet(tenantCtx("tenant-001"), domain.CreateReferenceSetRequest{SetCode: "ISO_3166_COUNTRY", Name: "n", SourceAuthority: "ISO"})
			return err
		}},
		"stage": {"stage-release", "stage", func(s *registry.Service) error {
			_, err := s.StageReferenceRelease(tenantCtx("tenant-001"), "ISO_3166_COUNTRY", stage)
			return err
		}},
		"publish": {"publish-release", "publish", func(s *registry.Service) error {
			_, err := s.PublishReferenceRelease(tenantCtx("tenant-001"), "ISO_3166_COUNTRY", "1", domain.PublishReferenceReleaseRequest{EvidenceRef: "e"})
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			az := &recordingAuthZ{}
			svc, f := refSvc(t, az)
			require.NoError(t, tc.run(svc))
			assert.Equal(t, "reference", az.resource)
			assert.Equal(t, tc.action, az.action)
			assert.Equal(t, tc.last, f.last)
			assert.Equal(t, testPrincipal, f.actor)

			deny, f2 := refSvc(t, &recordingAuthZ{err: registry.ErrUnauthorized})
			require.ErrorIs(t, tc.run(deny), registry.ErrUnauthorized)
			assert.Empty(t, f2.last, "a denied caller changes nothing")

			susp, f3 := refSvc(t, permitAllAuthZ{})
			f3.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
			require.Error(t, tc.run(susp))
			assert.Empty(t, f3.last)
		})
	}
}

func TestReference_ValidationFirstAndReadsAreUngated(t *testing.T) {
	svc, f := refSvc(t, permitAllAuthZ{})
	ctx := tenantCtx("tenant-001")
	_, err := svc.CreateReferenceSet(ctx, domain.CreateReferenceSetRequest{SetCode: "x"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.StageReferenceRelease(ctx, "ISO_3166_COUNTRY", domain.StageReferenceReleaseRequest{})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.PublishReferenceRelease(ctx, "ISO_3166_COUNTRY", "1", domain.PublishReferenceReleaseRequest{})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = svc.ResolveReference(ctx, domain.ReferenceQuery{SetCode: "bad", Code: "GB"})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, f.calls)

	// Resolving a code needs no grant of its own and defaults to now.
	deny, f2 := refSvc(t, denyAllAuthZ{})
	_, err = deny.ResolveReference(tenantCtx("tenant-001"), domain.ReferenceQuery{SetCode: "ISO_3166_COUNTRY", Code: "GB"})
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), f2.asOf, time.Minute)
}

func TestReference_AStoreWithoutReferenceReleasesIsUnavailable(t *testing.T) {
	svc := newSvc(t, newMemStore(), permitAllAuthZ{}, acceptAllJurisd{})
	ctx := tenantCtx("tenant-001")
	_, err := svc.ListReferenceSets(ctx)
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = svc.ListReferenceReleases(ctx, "ISO_3166_COUNTRY")
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = svc.ResolveReference(ctx, domain.ReferenceQuery{SetCode: "ISO_3166_COUNTRY", Code: "GB"})
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = svc.CreateReferenceSet(ctx, domain.CreateReferenceSetRequest{SetCode: "ISO_3166_COUNTRY", Name: "n", SourceAuthority: "ISO"})
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
