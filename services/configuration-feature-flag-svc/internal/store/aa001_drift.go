package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/events"
)

// ── drift detection (INV-23/24, NP-23/24/26/27, TC-07/08) ─────────────────────
//
// Attestations used to be recorded and never read: nothing compared what a
// runtime reported serving with what it should be serving, drift_events was
// never written and config.drift.detected never emitted. A fleet running stale
// or unissued configuration was undetectable.
//
// Detection compares exact identities — epoch and digest — never labels
// (INV-23). It only records and announces: remediation is a governed change
// like any other (INV-24), so nothing here writes a configuration value.

// classifyDrift compares one observation with the environment's desired
// snapshot. nil means the runtime serves exactly what it should.
func classifyDrift(ctx context.Context, tx pgx.Tx, runtimeID, environment string, tenantID, observedSnapshotID *string, observedEpoch int64, observedDigest string) (*domain.DriftEvent, error) {
	d := &domain.DriftEvent{
		RuntimeID:          runtimeID,
		Environment:        environment,
		TenantID:           tenantID,
		ObservedSnapshotID: observedSnapshotID,
		ObservedEpoch:      &observedEpoch,
		ObservedDigest:     &observedDigest,
		RemediationStatus:  domain.RemediationOpen,
	}

	desired, err := latestSnapshotRowQ(ctx, tx, environment)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing has been issued here, so whatever the runtime serves did not
		// come from CFG.
		d.DriftClass, d.Severity = domain.DriftUnknown, domain.SeverityHigh
		return d, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	d.DesiredSnapshotID, d.DesiredEpoch, d.DesiredDigest = &desired.SnapshotID, &desired.Epoch, &desired.Digest

	if observedEpoch == desired.Epoch && observedDigest == desired.Digest &&
		(observedSnapshotID == nil || *observedSnapshotID == desired.SnapshotID) {
		return nil, nil
	}

	// What CFG actually issued at the epoch the runtime claims.
	var issuedID, issuedDigest string
	var issuedDeadline time.Time
	err = tx.QueryRow(ctx, `
		SELECT snapshot_id, digest, freshness_deadline FROM config_snapshots
		WHERE environment = $1 AND epoch = $2`, environment, observedEpoch).
		Scan(&issuedID, &issuedDigest, &issuedDeadline)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// An epoch CFG never issued — a future epoch, or a fabricated one.
		d.DriftClass, d.Severity = domain.DriftUnauthorized, domain.SeverityCritical
		return d, nil
	case err != nil:
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if issuedDigest != observedDigest || (observedSnapshotID != nil && *observedSnapshotID != issuedID) {
		// NP-24 / NP-27: content that does not hash to what was issued — a
		// corrupted payload, or a local override of canonical keys.
		d.DriftClass, d.Severity = domain.DriftUnauthorized, domain.SeverityCritical
		return d, nil
	}

	// A genuine, older snapshot (NP-23): stale, and more serious once the
	// snapshot it holds is past its own freshness deadline (INV-27).
	d.DriftClass, d.Severity = domain.DriftStale, domain.SeverityMedium
	if time.Now().After(issuedDeadline) {
		d.Severity = domain.SeverityHigh
	}
	return d, nil
}

// recordDrift writes the finding and enqueues config.drift.detected in the
// caller's transaction, so a finding is never recorded without being announced.
func recordDrift(ctx context.Context, tx pgx.Tx, d *domain.DriftEvent, callerTenantID, correlationID string) error {
	if err := tx.QueryRow(ctx, `
		INSERT INTO drift_events
			(runtime_id, environment, tenant_id, desired_snapshot_id, desired_epoch, desired_digest,
			 observed_snapshot_id, observed_epoch, observed_digest, drift_class, severity)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING drift_id, detected_at`,
		d.RuntimeID, d.Environment, d.TenantID, d.DesiredSnapshotID, d.DesiredEpoch, d.DesiredDigest,
		observedSnapshotRef(ctx, tx, d.ObservedSnapshotID), d.ObservedEpoch, d.ObservedDigest, d.DriftClass, d.Severity,
	).Scan(&d.DriftID, &d.DetectedAt); err != nil {
		return fmt.Errorf("%w: record drift: %v", domain.ErrStoreUnavailable, err)
	}
	outEv, evErr := events.DriftDetected(*d, correlationID)
	return enqueueEvent(ctx, tx, callerTenantID, outEv, evErr)
}

// observedSnapshotRef keeps the drift row's foreign key honest: a runtime that
// reports a snapshot id CFG never issued is exactly the UNAUTHORIZED case, and
// that id cannot be stored in a column referencing config_snapshots. The
// claimed epoch and digest are still recorded beside it.
func observedSnapshotRef(ctx context.Context, tx pgx.Tx, id *string) *string {
	if id == nil {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM config_snapshots WHERE snapshot_id::text = $1)`, *id).Scan(&exists); err != nil || !exists {
		return nil
	}
	return id
}

// driftConvergenceWindow is domain.DriftConvergenceWindow, held in a variable
// only so the store tests can shorten it: snapshots are immutable, so a test
// cannot age one past the window instead.
var driftConvergenceWindow = domain.DriftConvergenceWindow

// detectFleetDrift is the sweep's half of detection: a runtime that has not
// converged on the environment's current snapshot within the convergence
// window after it was issued is STALE; one whose latest attestation is past
// its freshness deadline has gone silent and is UNKNOWN (NP-26: the system
// must not assume a runtime that cannot attest is current). One open finding
// per runtime, desired snapshot and class — a runtime that stays stale across
// many sweeps is one finding, not one per tick.
func detectFleetDrift(ctx context.Context, tx pgx.Tx, environment string, result *SweepResult) error {
	desired, err := latestSnapshotRowQ(ctx, tx, environment)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (runtime_id, COALESCE(tenant_id, '`+nilScopeUUID+`'::UUID))
		       runtime_id, tenant_id, observed_snapshot_id, observed_epoch, observed_digest, freshness_deadline
		FROM runtime_attestations
		WHERE environment = $1
		ORDER BY runtime_id, COALESCE(tenant_id, '`+nilScopeUUID+`'::UUID), reported_at DESC`, environment)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	type latest struct {
		runtimeID string
		tenantID  *string
		snapID    *string
		epoch     int64
		digest    string
		deadline  time.Time
	}
	var fleet []latest
	for rows.Next() {
		var l latest
		if err := rows.Scan(&l.runtimeID, &l.tenantID, &l.snapID, &l.epoch, &l.digest, &l.deadline); err != nil {
			rows.Close()
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		fleet = append(fleet, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	now := time.Now()
	converged := now.Sub(desired.IssuedAt) >= driftConvergenceWindow
	for _, l := range fleet {
		class, severity := "", ""
		switch {
		case now.After(l.deadline):
			class, severity = domain.DriftUnknown, domain.SeverityHigh
		case l.epoch < desired.Epoch && converged:
			class, severity = domain.DriftStale, domain.SeverityMedium
		default:
			continue
		}
		var open bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM drift_events
				WHERE runtime_id = $1 AND environment = $2
				  AND COALESCE(tenant_id, '`+nilScopeUUID+`'::UUID) = COALESCE($3::uuid, '`+nilScopeUUID+`'::UUID)
				  AND desired_snapshot_id = $4 AND drift_class = $5 AND remediation_status = 'OPEN')`,
			l.runtimeID, environment, l.tenantID, desired.SnapshotID, class).Scan(&open); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if open {
			continue
		}
		epoch, digest := l.epoch, l.digest
		d := &domain.DriftEvent{
			RuntimeID: l.runtimeID, Environment: environment, TenantID: l.tenantID,
			DesiredSnapshotID: &desired.SnapshotID, DesiredEpoch: &desired.Epoch, DesiredDigest: &desired.Digest,
			ObservedSnapshotID: l.snapID, ObservedEpoch: &epoch, ObservedDigest: &digest,
			DriftClass: class, Severity: severity, RemediationStatus: domain.RemediationOpen,
		}
		if err := recordDrift(ctx, tx, d, "", ""); err != nil {
			return err
		}
		result.DriftFindings++
	}
	return nil
}
