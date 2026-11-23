package store_test

import (
	"context"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/store"
)

// Doc 08 §04/§07, OPR-033: stored releases are immutable (migration 007).
func TestPostgresReleasesImmutable(t *testing.T) {
	pool := openTestDB(t)
	s := store.NewPostgres(pool)
	ctx := context.Background()
	if err := s.PutRelease(ctx, store.Release{ReleaseID: "kb-imm-1", Kind: "kb", Manifest: []byte("{}"), Signature: []byte("sig"), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE public_ai.releases SET manifest = 'x' WHERE release_id = 'kb-imm-1'`); err == nil {
		t.Fatal("updating a release must be rejected")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM public_ai.releases WHERE release_id = 'kb-imm-1'`); err == nil {
		t.Fatal("deleting a release must be rejected")
	}
}
