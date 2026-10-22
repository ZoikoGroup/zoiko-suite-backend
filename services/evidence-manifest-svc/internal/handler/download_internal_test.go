package handler

import (
	"archive/zip"
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/evidence-manifest-svc/internal/domain"
)

// failingWriter accepts n bytes and then fails every write, so a write error
// can be injected at a chosen point of the archive.
type failingWriter struct {
	remaining int
}

var errDiskFull = errors.New("injected write failure")

func (f *failingWriter) Write(p []byte) (int, error) {
	if len(p) > f.remaining {
		n := f.remaining
		f.remaining = 0
		return n, errDiskFull
	}
	f.remaining -= len(p)
	return len(p), nil
}

func sampleManifestAndRecords() (*domain.EvidenceManifest, []domain.ManifestRecord) {
	sum := "abc"
	m := &domain.EvidenceManifest{ManifestID: "m-1", ChecksumSHA256: &sum, Status: domain.StatusGenerated}
	recs := []domain.ManifestRecord{
		{SourceType: domain.SourceGovernanceDecision, SourceRecordID: "gd-1", RecordSnapshot: bytes.Repeat([]byte(`{"a":"b"}`), 200)},
		{SourceType: domain.SourceWorkflowInstance, SourceRecordID: "wf-1", RecordSnapshot: bytes.Repeat([]byte(`{"c":"d"}`), 200)},
	}
	return m, recs
}

func TestWriteManifestZip_CompleteArchive(t *testing.T) {
	m, recs := sampleManifestAndRecords()
	var buf bytes.Buffer
	require.NoError(t, writeManifestZip(&buf, m, recs))

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	assert.Equal(t, []string{"manifest.json", "checksum.sha256", "records/records.json",
		"records/001_GOVERNANCE_DECISION_gd-1.json", "records/002_WORKFLOW_INSTANCE_wf-1.json"}, names)
}

// Every write position is checked: a failure injected at ANY byte offset makes
// the function return an error. Before, the checksum, records index and record
// entries ignored their write errors and a truncated archive was served as 200.
func TestWriteManifestZip_AnyWriteFailureIsReturned(t *testing.T) {
	m, recs := sampleManifestAndRecords()
	var full bytes.Buffer
	require.NoError(t, writeManifestZip(&full, m, recs))

	for _, limit := range []int{0, 1, 10, full.Len() / 4, full.Len() / 2, full.Len() - 30, full.Len() - 1} {
		err := writeManifestZip(&failingWriter{remaining: limit}, m, recs)
		require.Error(t, err, "failure after %d of %d bytes must not be swallowed", limit, full.Len())
		assert.ErrorIs(t, err, errDiskFull)
	}
}
