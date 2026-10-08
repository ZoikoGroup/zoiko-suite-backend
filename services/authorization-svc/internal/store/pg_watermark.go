package store

import (
	"context"
	"fmt"
)

// VersionWatermark reads the assignment and policy versions (migration
// 000028). The decision cache puts both in every key, so a write through any
// replica — or straight to the database — retires the cached entries within
// one refresh (ZS-IAM-001 §19). authz_versions has no RLS: two counters,
// nothing tenant-owned.
func (s *PgStore) VersionWatermark(ctx context.Context) (policy, assignment int64, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT policy_version, assignment_version FROM authz_versions WHERE singleton = 1`).Scan(&policy, &assignment)
	if err != nil {
		return 0, 0, fmt.Errorf("read version watermark: %w", err)
	}
	return policy, assignment, nil
}
