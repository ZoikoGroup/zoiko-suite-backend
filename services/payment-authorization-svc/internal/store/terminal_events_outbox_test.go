package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payment-authorization-svc/internal/domain"
	"zoiko.io/payment-authorization-svc/internal/middleware"
	"zoiko.io/payment-authorization-svc/internal/store"
)

// TestPgStore_TerminalOutcomes_PublishToOutbox: the spec's
// PaymentAuthorizationInvalidated (and the rejected/revoked/expired
// outcomes) used to be written only to the local authorization_events table
// and never published. Each must now leave exactly one outbox row, written in
// the same transaction as the state change.
func TestPgStore_TerminalOutcomes_PublishToOutbox(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool, zap.NewNop())

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	newAuth := func() string {
		a, err := s.RequestAuthorization(ctx, tenantID, domain.PaymentAuthorization{
			LegalEntityID: legalEntityID, ProposalID: uuid.New().String(), ProposalFingerprint: "sha256:fp",
			NetAmount: 100, Currency: "USD", RequestedByPrincipalID: "maker",
		}, nil)
		require.NoError(t, err)
		return a.AuthorizationID
	}
	outboxTypes := func(authID string) []string {
		rows, err := pool.Query(context.Background(), `SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at`, authID)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var et string
			require.NoError(t, rows.Scan(&et))
			out = append(out, et)
		}
		return out
	}

	cases := []struct {
		name string
		want string
		do   func(id string) error
	}{
		{"rejected", domain.EventAuthorizationRejected, func(id string) error {
			_, err := s.RejectAuthorization(ctx, id, domain.RejectPaymentRequest{Reason: "no"}, "checker")
			return err
		}},
		{"invalidated", domain.EventAuthorizationInvalidated, func(id string) error {
			_, err := s.InvalidateAuthorization(ctx, id, "fingerprint changed")
			return err
		}},
		{"revoked", domain.EventAuthorizationRevoked, func(id string) error {
			_, err := s.RevokeAuthorization(ctx, id, domain.RevokeAuthorizationRequest{Reason: "changed mind"}, "checker")
			return err
		}},
		{"expired", domain.EventAuthorizationExpired, func(id string) error {
			_, err := s.ExpireAuthorization(ctx, id, "scheduler")
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := newAuth()
			before := outboxTypes(id)
			require.NoError(t, c.do(id))
			after := outboxTypes(id)
			require.Equal(t, len(before)+1, len(after), "expected exactly one new outbox row")
			require.Equal(t, c.want, after[len(after)-1])
		})
	}

	t.Run("a refused transition publishes nothing", func(t *testing.T) {
		id := newAuth()
		_, err := s.ExpireAuthorization(ctx, id, "scheduler")
		require.NoError(t, err)
		n := len(outboxTypes(id))
		_, err = s.RevokeAuthorization(ctx, id, domain.RevokeAuthorizationRequest{Reason: "late"}, "checker")
		require.ErrorIs(t, err, domain.ErrInvalidTransition)
		require.Equal(t, n, len(outboxTypes(id)))
	})
}
