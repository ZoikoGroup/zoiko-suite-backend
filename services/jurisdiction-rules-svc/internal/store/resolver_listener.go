package store

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// ResolverChannel is the Postgres NOTIFY channel migration 000025 publishes resolver-relevant changes on.
const ResolverChannel = "jur_resolver_invalidate"

const (
	listenBackoffStart = time.Second
	listenBackoffMax   = 30 * time.Second
)

// ListenResolverChanges tells onChange whenever something a resolver caches has changed (a pack version's status, a deployment, a signing key,
// a rollout status), so that every replica drops its cache at once instead of waiting for the TTL. It blocks until ctx is done and reconnects
// with backoff when the connection is lost.
//
// NOTIFY is a wake-up, not a queue: anything that changed while the connection was down is missed. So onChange is also called with the payload
// "connected" on every (re)connect, and the caller should treat any call as "forget everything you cached".
//
// It holds one dedicated connection outside the pool's normal use. A pooler that multiplexes sessions per transaction (PgBouncer in transaction
// mode) cannot carry LISTEN: point the service at the database, or a session-mode pooler, for this to work.
func (s *PgStore) ListenResolverChanges(ctx context.Context, onChange func(payload string)) {
	backoff := listenBackoffStart
	for ctx.Err() == nil {
		connected, err := s.listenOnce(ctx, onChange)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = listenBackoffStart
		}
		s.log.Warn("resolver change listener lost its connection; retrying", zap.Error(err), zap.Duration("retry_in", backoff))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > listenBackoffMax {
			backoff = listenBackoffMax
		}
	}
}

// listenOnce runs one connection's life. connected reports whether LISTEN was established, so a connection that worked resets the backoff.
func (s *PgStore) listenOnce(ctx context.Context, onChange func(string)) (connected bool, err error) {
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	// Hijack so the connection leaves the pool for good: a connection that was LISTENing must never be handed back to a request.
	conn := pooled.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+ResolverChannel); err != nil {
		return false, err
	}
	onChange("connected")
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, err
		}
		onChange(n.Payload)
	}
}
