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
	Store    *store.Memory
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

// New builds the stack. now is the release creation time.
func New(ctx context.Context, modelTimeout time.Duration) (*Env, error) {
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		return nil, err
	}
	st := store.NewMemory()
	dir := FixturesDir()
	now := time.Now().UTC()
	kb, kbSig, _, err := release.BuildKB(filepath.Join(dir, "kb"), "kb-fixture-1", now, priv)
	if err != nil {
		return nil, fmt.Errorf("build kb: %w", err)
	}
	cl, clSig, err := release.BuildClaims(filepath.Join(dir, "claims.json"), "claims-fixture-1", now, priv)
	if err != nil {
		return nil, fmt.Errorf("build claims: %w", err)
	}
	if err := release.Publish(ctx, st, pub, "kb-fixture-1", "kb", kb, kbSig); err != nil {
		return nil, err
	}
	if err := release.Publish(ctx, st, pub, "claims-fixture-1", "claims", cl, clSig); err != nil {
		return nil, err
	}
	_ = release.Promote(ctx, st, "kb", "kb-fixture-1", nil)
	_ = release.Promote(ctx, st, "claims", "claims-fixture-1", nil)

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
		Pipeline: &orchestrator.Pipeline{Releases: rt, Flags: fl, Model: model.Gateway{Provider: fake, Timeout: modelTimeout}, Store: st, MaxOutputTokens: 400},
	}, nil
}

// Refresh reloads releases and flags after a change.
func (e *Env) Refresh(ctx context.Context) error {
	if err := e.Releases.Refresh(ctx); err != nil {
		return err
	}
	return e.Flags.Refresh(ctx)
}
