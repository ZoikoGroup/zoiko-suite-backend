//go:build integration

// The acceptance bar a job was judged against must survive on the stored
// record: when the configured default changes, an earlier decision must
// still say which bar it cleared (SPEC_DEVIATIONS A11).
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/migration-integrity-svc/internal/domain"
)

func TestPgStore_IntegrityScoreThresholdPersistsAndIsReadBack(t *testing.T) {
	s := newStore()
	tenantID := uuid.NewString()
	now := time.Now().UTC()

	job := &domain.MigrationJob{
		ID: uuid.NewString(), LegalEntityID: "le-uk-1", MigrationName: "threshold persistence",
		SourceSystem: "LEGACY_ERP", TargetService: "general-ledger-svc",
		TotalRecordsCount: 10, ValidRecordsCount: 9, InvalidRecordsCount: 1,
		IntegrityScore: 90, IntegrityScoreThreshold: 80.5,
		Status: domain.JobStatusCompleted, StartedAt: &now, CompletedAt: &now,
	}
	require.NoError(t, s.CreateJob(context.Background(), tenantID, job, nil, nil, "principal-1", "corr-1"))

	read, err := s.GetJobByID(context.Background(), tenantID, job.ID)
	require.NoError(t, err)
	assert.Equal(t, 80.5, read.IntegrityScoreThreshold, "get must return the stored bar")

	jobs, err := s.ListJobs(context.Background(), tenantID, "le-uk-1", "")
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, 80.5, jobs[0].IntegrityScoreThreshold, "list must return the stored bar")
}
