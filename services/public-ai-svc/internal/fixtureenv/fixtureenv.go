// Package fixtureenv assembles a complete in-process public-ai stack on the
// TEST-ONLY fixtures: in-memory store, freshly signed knowledge and claim
// releases, release runtime, flags and the pipeline with the Fake model. It
// is used by tests, the eval runner and local dev mode. Never production.
package fixtureenv

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/model"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/release"
	"zoiko.io/public-ai-svc/internal/signing"
	"zoiko.io/public-ai-svc/internal/store"
)

// Env is a ready stack.
type Env struct {
	Store    store.Store
	Releases *release.Runtime
	Flags    *flags.Loader
	Fake     *model.Fake
	Pipeline *orchestrator.Pipeline
	PubKey   string
	PrivKey  string
}

// FixturesDir returns the absolute testdata/fixtures directory.
func FixturesDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "fixtures")
}

// New builds the stack on an in-memory store.
func New(ctx context.Context, modelTimeout time.Duration) (*Env, error) {
	return NewWithStore(ctx, store.NewMemory(), "fixture-1", modelTimeout)
}

// NewWithStore builds the stack on st, publishing releases
// "kb-<suffix>" and "claims-<suffix>" signed with a fresh key and moving the
// release pointers to them. Use only on a dedicated test/drill database.
func NewWithStore(ctx context.Context, st store.Store, suffix string, modelTimeout time.Duration) (*Env, error) {
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		return nil, err
	}
	dir := FixturesDir()
	now := time.Now().UTC()
	kbID, clID := "kb-"+suffix, "claims-"+suffix
	kb, kbSig, _, err := release.BuildKB(filepath.Join(dir, "kb"), kbID, now, priv)
	if err != nil {
		return nil, fmt.Errorf("build kb: %w", err)
	}
	cl, clSig, err := release.BuildClaims(filepath.Join(dir, "claims.json"), clID, now, priv)
	if err != nil {
		return nil, fmt.Errorf("build claims: %w", err)
	}
	if err := release.Publish(ctx, st, pub, kbID, "kb", kb, kbSig); err != nil {
		return nil, err
	}
	if err := release.Publish(ctx, st, pub, clID, "claims", cl, clSig); err != nil {
		return nil, err
	}
	if err := release.Promote(ctx, st, "kb", kbID, nil); err != nil {
		return nil, err
	}
	if err := release.Promote(ctx, st, "claims", clID, nil); err != nil {
		return nil, err
	}

	rt := release.New(st, pub)
	if err := rt.Refresh(ctx); err != nil {
		return nil, err
	}
	fl := flags.NewLoader(st)
	if err := fl.Refresh(ctx); err != nil {
		return nil, err
	}
	fake := &model.Fake{}
	return &Env{
		Store: st, Releases: rt, Flags: fl, Fake: fake, PubKey: pub, PrivKey: priv,
		Pipeline: &orchestrator.Pipeline{Releases: rt, Flags: fl, Model: model.Gateway{Provider: fake, Timeout: modelTimeout, Breaker: &model.Breaker{Threshold: 5, Cooldown: 30 * time.Second}}, Store: st, MaxOutputTokens: 400},
	}, nil
}

// Refresh reloads releases and flags after a change.
func (e *Env) Refresh(ctx context.Context) error {
	if err := e.Releases.Refresh(ctx); err != nil {
		return err
	}
	return e.Flags.Refresh(ctx)
}
