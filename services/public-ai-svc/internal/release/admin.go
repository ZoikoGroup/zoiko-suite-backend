package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/knowledge"
	"zoiko.io/public-ai-svc/internal/signing"
	"zoiko.io/public-ai-svc/internal/store"
)

// BuildKB ingests a fixture/import directory and signs the resulting manifest.
func BuildKB(dir, releaseID string, now time.Time, privKey string) ([]byte, []byte, []knowledge.Report, error) {
	m, reports, err := knowledge.IngestDir(dir, releaseID, now)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(m.Sources) == 0 {
		return nil, nil, reports, errors.New("no source passed ingestion; refusing to build an empty release")
	}
	if err := m.Validate(); err != nil {
		return nil, nil, reports, err
	}
	body, err := json.Marshal(m)
	if err != nil {
		return nil, nil, nil, err
	}
	sig, err := signing.Sign(privKey, body)
	return body, sig, reports, err
}

// BuildClaims validates a claim registry file and signs it.
func BuildClaims(path, releaseID string, now time.Time, privKey string) ([]byte, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var m claims.Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, nil, fmt.Errorf("claims file: %w", err)
	}
	m.ReleaseID, m.Kind, m.CreatedAt = releaseID, claims.KindClaims, now.UTC()
	if problems := m.Validate(); len(problems) > 0 {
		return nil, nil, fmt.Errorf("claims rejected: %s", strings.Join(problems, "; "))
	}
	body, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	sig, err := signing.Sign(privKey, body)
	return body, sig, err
}

// Publish stores a signed release after verifying it. Unverifiable
// artifacts are never stored.
func Publish(ctx context.Context, st store.Store, pubKey, releaseID, kind string, manifest, sig []byte) error {
	if err := signing.Verify(pubKey, manifest, sig); err != nil {
		return err
	}
	var head struct {
		ReleaseID string `json:"release_id"`
		Kind      string `json:"kind"`
	}
	if err := json.Unmarshal(manifest, &head); err != nil || head.ReleaseID != releaseID || head.Kind != kind {
		return fmt.Errorf("manifest header does not match release %s/%s", releaseID, kind)
	}
	if existing, err := st.GetRelease(ctx, releaseID); err == nil && !bytes.Equal(existing.Manifest, manifest) {
		return fmt.Errorf("release %s already exists with different content (releases are immutable)", releaseID)
	}
	return st.PutRelease(ctx, store.Release{ReleaseID: releaseID, Kind: kind, Manifest: manifest, Signature: sig, CreatedAt: time.Now().UTC()})
}

// Promote atomically moves the pointer for kind (also used for rollback:
// revocations still apply on top of whichever release is active).
func Promote(ctx context.Context, st store.Store, kind, releaseID string, events []store.OutboxEvent) error {
	return st.SetPointer(ctx, kind, releaseID, events)
}

// Revoke appends to the revocation ledger. targetKind is "source" or "claim".
func Revoke(ctx context.Context, st store.Store, targetKind, targetID, reason string, events []store.OutboxEvent) (int64, error) {
	if targetKind != "source" && targetKind != "claim" {
		return 0, fmt.Errorf("target kind must be source or claim")
	}
	if targetID == "" || reason == "" {
		return 0, fmt.Errorf("target id and reason code are required")
	}
	now := time.Now().UTC()
	return st.AddRevocation(ctx, store.Revocation{TargetKind: targetKind, TargetID: targetID, ReasonCode: reason, EffectiveAt: now, CreatedAt: now}, events)
}

// LoadFile publishes and promotes a signed release file (local dev loader).
func LoadFile(ctx context.Context, st store.Store, pubKey, path string) error {
	f, manifest, sig, err := signing.ReadFile(path)
	if err != nil {
		return err
	}
	if err := Publish(ctx, st, pubKey, f.ReleaseID, f.Kind, manifest, sig); err != nil {
		return err
	}
	return Promote(ctx, st, f.Kind, f.ReleaseID, nil)
}
