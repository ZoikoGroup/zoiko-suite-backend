package whout

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// TopicLister reports which topics currently exist on the broker.
type TopicLister func(ctx context.Context) (map[string]bool, error)

// SuperviseTopics runs a reader over the configured topics that exist, and restarts it
// when another one appears.
//
// A consumer group that joins before one of its topics exists gets an empty assignment
// for it and is never given the partitions later (found in a live run: the group sat
// joined and idle while events accumulated). The fix is not to join until there is
// something to read, and to rejoin when the set of existing topics grows. With the
// reader started at the beginning of a new group (FanOut delivers only events that
// postdate a subscription's activation), nothing produced in the gap is lost.
//
// run blocks until its context is cancelled. SuperviseTopics returns when ctx is.
func SuperviseTopics(ctx context.Context, log *zap.Logger, configured []string, list TopicLister,
	run func(ctx context.Context, topics []string), every time.Duration) {
	if log == nil {
		log = zap.NewNop()
	}
	if every <= 0 {
		every = 15 * time.Second
	}
	existing := func() []string {
		have, err := list(ctx)
		if err != nil {
			log.Warn("webhook fan-out: cannot list topics; will retry", zap.Error(err))
			return nil
		}
		var out []string
		for _, t := range configured {
			if have[t] {
				out = append(out, t)
			}
		}
		return out
	}
	sleep := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	warned := false
	for ctx.Err() == nil {
		present := existing()
		if len(present) == 0 {
			if !warned {
				log.Warn("webhook fan-out: none of the source topics exist yet; waiting for them", zap.Strings("topics", configured))
				warned = true
			}
			if !sleep(every) {
				return
			}
			continue
		}
		warned = false
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			run(runCtx, present)
		}()
	watch:
		for {
			select {
			case <-ctx.Done():
				cancel()
				<-done
				return
			case <-done:
				cancel()
				break watch // the reader ended by itself: start it again
			case <-time.After(every):
				if now := existing(); len(now) > len(present) {
					log.Info("webhook fan-out: a source topic appeared; rejoining to read it",
						zap.Strings("was", present), zap.Strings("now", now))
					cancel()
					<-done
					break watch
				}
			}
		}
	}
}
