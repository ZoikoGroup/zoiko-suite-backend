package ar10

import (
	"context"
	"errors"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Tier is one write-off approval authority level: the largest write-off its holder may approve and the authz action that grants it.
type Tier struct {
	Level  int
	Limit  kit.Amount // 0 = unlimited
	Action string
}

// DefaultTiers are the package-constant authority tiers (ascending). delegated-authority-svc holds grants but exposes no limit-check
// route, so limits live here, applied to the write-off amount in its own currency (no FX conversion). A delegated-authority-backed
// AuthorityPolicy can replace ConstantTierPolicy later without touching the commands.
var DefaultTiers = []Tier{
	{Level: 1, Limit: kit.MustAmount("500.0000"), Action: ActionWriteOffApproveT1},
	{Level: 2, Limit: kit.MustAmount("5000.0000"), Action: ActionWriteOffApproveT2},
	{Level: 3, Limit: kit.MustAmount("50000.0000"), Action: ActionWriteOffApproveT3},
	{Level: 4, Limit: 0, Action: ActionWriteOffApproveT4},
}

// AuthorityPolicy decides who may approve which write-off. It is the seam for a delegated-authority-svc implementation.
type AuthorityPolicy interface {
	// Required is the tier a write-off of that amount needs.
	Required(amount kit.Amount, currency string) Tier
	// Held is the highest tier level the caller holds (0 = none). A failure to answer is an error (fail closed).
	Held(ctx context.Context, c kit.Caller) (int, error)
}

// ConstantTierPolicy resolves tiers from the package constants and the authorization-svc action grants.
type ConstantTierPolicy struct {
	Tiers []Tier
	Authz kit.Authz
}

// NewConstantTierPolicy builds the default policy.
func NewConstantTierPolicy(a kit.Authz) *ConstantTierPolicy {
	return &ConstantTierPolicy{Tiers: DefaultTiers, Authz: a}
}

func (p *ConstantTierPolicy) Required(amount kit.Amount, _ string) Tier {
	for _, t := range p.Tiers {
		if t.Limit == 0 || amount <= t.Limit {
			return t
		}
	}
	return p.Tiers[len(p.Tiers)-1]
}

func (p *ConstantTierPolicy) Held(ctx context.Context, c kit.Caller) (int, error) {
	for i := len(p.Tiers) - 1; i >= 0; i-- {
		err := p.Authz.CheckAllowed(ctx, c.PrincipalID, c.LegalEntityID, p.Tiers[i].Action)
		switch {
		case err == nil:
			return p.Tiers[i].Level, nil
		case errors.Is(err, domain.ErrAuthorizationDenied):
		default:
			return 0, kit.Unavailable("authorization service unavailable")
		}
	}
	return 0, nil
}
