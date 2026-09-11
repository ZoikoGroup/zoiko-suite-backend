// Package retention keeps the access_decision_log partition set healthy.
//
// The table records one row per authorization evaluation, platform-wide, and
// 000009 converted it to monthly RANGE partitions with two helpers: one that
// creates a month's partition, one that detaches every partition older than a
// cutoff. This package is what calls them on a timer. Until it existed,
// neither ran: the window was configured, the SQL was written and tested, and
// nothing invoked either half.
//
// ── THE TWO HALVES ARE NOT EQUALLY URGENT ───────────────────────────────────
//
// Creating ahead is the one that prevents an incident. A partitioned table
// rejects an insert for which no partition exists, and on this table that is a
// 503 on the platform's hottest endpoint. 000009's default partition is the
// safety net that makes that a soft failure instead of an outage — but a row
// landing there means the runway ran out, and nothing was extending it.
//
// Detaching is housekeeping. It bounds growth and insert latency, and it is
// deliberately non-destructive: DETACH leaves the rows in an ordinary table
// for an operator to archive and then drop on purpose, so the log stays
// append-only. Nothing here ever issues a DELETE or a DROP.
//
// So a sweep creates first and detaches second, and a failure in the second
// half never prevents the first. If only one of them ever runs, it should be
// the one that keeps the service answering.
//
// ── WHY THIS NEEDED A MIGRATION TO WORK AT ALL ──────────────────────────────
//
// Both helpers were LANGUAGE plpgsql with no SECURITY clause, so they executed
// as the caller — and the service connects as a role with USAGE and DML and no
// DDL, correctly. Measured against the running stack as app_authorization,
// before 000011: the create half answered "permission denied for schema
// public" and the detach half "must be owner of table access_decision_log".
// 000009's own tests passed throughout, because they run on the migration
// connection, which owns the table. 000011 makes both SECURITY DEFINER.
package retention

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// Detached is one partition the sweep removed from the parent. It still exists
// as an ordinary table — that is the point of DETACH over DROP.
type Detached struct {
	PartitionName string
	RowCount      int64
}

// Store is the narrow surface this package needs.
type Store interface {
	// EnsureAccessDecisionPartition creates the partition covering month and
	// returns its name. Idempotent: a month that already exists is a no-op.
	EnsureAccessDecisionPartition(ctx context.Context, month time.Time) (string, error)

	// DetachAccessDecisionPartitionsBefore detaches every monthly partition
	// whose range ends on or before cutoff. Never touches the default
	// partition.
	DetachAccessDecisionPartitionsBefore(ctx context.Context, cutoff time.Time) ([]Detached, error)

	// TryRetentionLock takes a session-scoped advisory lock, returning false if
	// another replica holds it. Releasing is the caller's job.
	TryRetentionLock(ctx context.Context) (bool, error)
	ReleaseRetentionLock(ctx context.Context) error
}

// Runner sweeps on an interval.
type Runner struct {
	store Store
	log   *zap.Logger

	interval        time.Duration
	monthsAhead     int
	retentionMonths int
}

// Config values that are not worth an env var each.
const (
	// minMonthsAhead is a floor, not a default. A runway of one month means
	// the partition for next month is created at some point during this one,
	// which is enough only if the sweep never fails. Three is the shipped
	// default; anything below one is a misconfiguration that would let the
	// runway reach zero, so it is raised rather than honoured.
	minMonthsAhead = 1

	// sweepTimeout bounds one sweep. Generous: it does DDL, and a sweep that
	// is slow is far less alarming than one that is cancelled halfway through
	// creating a runway.
	sweepTimeout = 2 * time.Minute
)

func New(store Store, log *zap.Logger, interval time.Duration, monthsAhead, retentionMonths int) *Runner {
	if monthsAhead < minMonthsAhead {
		log.Warn("retention: months-ahead below the floor — raising it",
			zap.Int("configured", monthsAhead), zap.Int("using", minMonthsAhead))
		monthsAhead = minMonthsAhead
	}
	return &Runner{
		store:           store,
		log:             log,
		interval:        interval,
		monthsAhead:     monthsAhead,
		retentionMonths: retentionMonths,
	}
}

// Run sweeps immediately and then on the interval, until ctx is cancelled.
//
// The immediate sweep matters more than the schedule: a service starting after
// a long outage, or a fresh deployment whose migrations created only the months
// current at migration time, needs the runway extended now rather than at the
// next tick.
func (r *Runner) Run(ctx context.Context) {
	r.log.Info("retention sweeper started",
		zap.Duration("interval", r.interval),
		zap.Int("months_ahead", r.monthsAhead),
		zap.Int("retention_months", r.retentionMonths))

	r.Sweep(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.log.Info("retention sweeper stopped")
			return
		case <-ticker.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep extends the partition runway, then detaches what has aged out.
//
// Exported so a test can drive one sweep without a ticker, and so an operator
// endpoint could trigger one if that is ever wanted.
func (r *Runner) Sweep(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()

	// One replica at a time. Creating is idempotent so concurrent creates are
	// harmless, but DETACH is not: two replicas detaching the same partition
	// means one of them errors, and an error on a maintenance path is noise
	// that trains an operator to ignore it. Skipping when another replica holds
	// the lock is correct rather than merely convenient — the work is
	// idempotent and time-based, so the next tick covers anything missed.
	locked, err := r.store.TryRetentionLock(ctx)
	if err != nil {
		r.log.Error("retention: could not take the sweep lock — skipping this sweep", zap.Error(err))
		return
	}
	if !locked {
		r.log.Debug("retention: another replica is sweeping — skipping")
		return
	}
	defer func() {
		if err := r.store.ReleaseRetentionLock(ctx); err != nil {
			r.log.Warn("retention: could not release the sweep lock", zap.Error(err))
		}
	}()

	r.extendRunway(ctx)
	r.detachAged(ctx)
}

// extendRunway creates this month's partition and the next monthsAhead.
//
// This month is included deliberately. It exists in the ordinary case, and
// creating it is a no-op — but the case that matters is a service starting into
// a month nobody pre-created, where every insert is currently landing in the
// default partition. Extending from "next month" would leave that unrepaired.
func (r *Runner) extendRunway(ctx context.Context) {
	now := time.Now().UTC()
	created, existing := 0, 0

	for i := 0; i <= r.monthsAhead; i++ {
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, i, 0)

		name, err := r.store.EnsureAccessDecisionPartition(ctx, month)
		if err != nil {
			// Logged per month and the loop continues: a failure on month i+2
			// says nothing about month i+1, and the near months are the ones
			// that keep the service answering.
			r.log.Error("retention: could not ensure a partition",
				zap.String("month", month.Format("2006-01")), zap.Error(err))
			continue
		}
		// The function is idempotent and returns the name either way, so this
		// counts months covered rather than tables created. The distinction is
		// not worth a second round trip.
		if i == 0 {
			existing++
		} else {
			created++
		}
		r.log.Debug("retention: partition ensured", zap.String("partition", name))
	}

	r.log.Info("retention: partition runway extended",
		zap.Int("months_covered", created+existing),
		zap.String("through", time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).
			AddDate(0, r.monthsAhead+1, 0).Add(-time.Second).Format("2006-01-02")))
}

// detachAged detaches partitions whose whole month is older than the window.
func (r *Runner) detachAged(ctx context.Context) {
	// 0 is the documented off switch — see the config comment on
	// AUTHZ_ACCESS_DECISION_RETENTION_MONTHS. Anything negative is a
	// misconfiguration, and the safe reading of one on this path is "do not
	// detach", never "detach more".
	if r.retentionMonths <= 0 {
		r.log.Debug("retention: detaching disabled", zap.Int("retention_months", r.retentionMonths))
		return
	}

	now := time.Now().UTC()
	cutoff := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).
		AddDate(0, -r.retentionMonths, 0)

	// A belt-and-braces floor. The arithmetic above cannot produce a cutoff in
	// the current month for retentionMonths >= 1, and this asserts it anyway:
	// the failure mode being guarded is detaching a partition that is still
	// being written to, which would take the live month's decisions out of the
	// parent table.
	firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if !cutoff.Before(firstOfThisMonth) {
		r.log.Error("retention: refusing to detach — the cutoff reaches the current month",
			zap.String("cutoff", cutoff.Format("2006-01-02")),
			zap.Int("retention_months", r.retentionMonths))
		return
	}

	detached, err := r.store.DetachAccessDecisionPartitionsBefore(ctx, cutoff)
	if err != nil {
		r.log.Error("retention: detach sweep failed",
			zap.String("cutoff", cutoff.Format("2006-01-02")), zap.Error(err))
		return
	}
	if len(detached) == 0 {
		r.log.Debug("retention: nothing aged out", zap.String("cutoff", cutoff.Format("2006-01-02")))
		return
	}

	// Info, with the row counts, and worded so the follow-up is obvious. These
	// tables still hold decision artifacts: they have left the parent, not the
	// database, and somebody has to archive and drop them deliberately.
	var rows int64
	names := make([]string, 0, len(detached))
	for _, d := range detached {
		rows += d.RowCount
		names = append(names, d.PartitionName)
	}
	r.log.Info("retention: partitions detached — they still exist and now need archiving",
		zap.Strings("partitions", names),
		zap.Int64("rows", rows),
		zap.String("cutoff", cutoff.Format("2006-01-02")))
}
