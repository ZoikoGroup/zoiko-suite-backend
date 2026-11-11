// Package sealer seals every tenant's unsealed governance decisions into a
// GOV-07 evidence package on a schedule (EVIDENCE_SEAL_INTERVAL).
//
// Sealing was a console command only, so a tenant's decisions stayed outside
// any Merkle-rooted package until someone pressed "Seal package" — and a
// tampered chain went unnoticed for as long. Each run lists the tenants whose
// chain head is past their last package, and seals each one through the same
// store path the handler uses: the chain is re-verified first (a broken chain
// is refused and reported, never certified), one sealer per tenant runs at a
// time (advisory lock), and the Idempotency-Key is derived from the head seen,
// so two replicas on the same tick produce one package, not two.
package sealer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/store"
)

// Store is what the sealer needs from the ledger.
type Store interface {
	UnsealedTenants(ctx context.Context) ([]store.UnsealedTenant, error)
	SealPackage(ctx context.Context, req store.SealRequest) (*domain.EvidencePackage, bool, error)
}

// Observer receives one outcome per tenant attempt.
type Observer interface {
	ObserveSeal(outcome string)
}

type nopObserver struct{}

func (nopObserver) ObserveSeal(string) {}

// Outcomes reported to the Observer.
const (
	OutcomeSealed      = "sealed"
	OutcomeChainBroken = "chain_broken"
	OutcomeError       = "error"
)

// Sealer runs scheduled seals.
type Sealer struct {
	store     Store
	principal string
	log       *zap.Logger
	obs       Observer
}

// New builds a sealer recording principal as sealed_by.
func New(s Store, principal string, log *zap.Logger, obs Observer) *Sealer {
	if obs == nil {
		obs = nopObserver{}
	}
	return &Sealer{store: s, principal: principal, log: log, obs: obs}
}

// Result counts one run.
type Result struct {
	Sealed, ChainBroken, Failed int
}

// RunOnce seals every tenant with unsealed decisions.
func (s *Sealer) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	tenants, err := s.store.UnsealedTenants(ctx)
	if err != nil {
		s.obs.ObserveSeal(OutcomeError)
		return res, fmt.Errorf("list unsealed tenants: %w", err)
	}
	label := "scheduled"
	reason := "scheduled seal (EVIDENCE_SEAL_INTERVAL)"
	for _, t := range tenants {
		key := fmt.Sprintf("scheduled-seal:%d", t.HeadSeq)
		sum := sha256.Sum256([]byte(s.principal + "\x00" + key))
		pkg, created, err := s.store.SealPackage(ctx, store.SealRequest{
			TenantID: t.TenantID, PrincipalID: s.principal,
			Label: &label, Reason: &reason,
			IdempotencyKey: key, RequestHash: sum[:],
		})
		switch {
		case err == nil:
			if created {
				res.Sealed++
				s.obs.ObserveSeal(OutcomeSealed)
				s.log.Info("evidence package sealed on schedule", zap.String("tenant_id", t.TenantID),
					zap.String("package_id", pkg.PackageID), zap.Int64("to_seq", pkg.ToSeq), zap.String("merkle_root", pkg.MerkleRoot))
			}
		case errors.Is(err, domain.ErrNothingToSeal):
			// Another replica sealed it between the listing and now.
		case errors.Is(err, domain.ErrChainBroken):
			res.ChainBroken++
			s.obs.ObserveSeal(OutcomeChainBroken)
			s.log.Error("evidence chain does not verify — scheduled seal refused; run GET /v1/evidence/chain/verify for this tenant",
				zap.String("tenant_id", t.TenantID), zap.Int64("head_seq", t.HeadSeq), zap.Int64("sealed_to", t.SealedTo))
		default:
			res.Failed++
			s.obs.ObserveSeal(OutcomeError)
			s.log.Warn("scheduled seal failed; retried next interval", zap.String("tenant_id", t.TenantID), zap.Error(err))
		}
	}
	return res, nil
}

// Run seals every interval until ctx ends.
func (s *Sealer) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("scheduled sealing run failed; retried next interval", zap.Error(err))
		}
	}
}
