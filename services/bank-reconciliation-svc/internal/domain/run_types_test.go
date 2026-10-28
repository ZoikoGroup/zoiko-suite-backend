package domain_test

import (
	"errors"
	"testing"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
)

func TestCheckCertifierIndependence(t *testing.T) {
	preparers := []string{"creator-1", "freezer-1", "matcher-1", "proposer-1", ""}

	for _, actor := range preparers[:4] {
		if err := domain.CheckCertifierIndependence(actor, preparers); !errors.Is(err, domain.ErrRunSelfCertificationForbidden) {
			t.Fatalf("expected %q to be barred from certifying, got %v", actor, err)
		}
	}

	if err := domain.CheckCertifierIndependence("certifier-1", preparers); err != nil {
		t.Fatalf("an independent certifier must be allowed, got %v", err)
	}
}
