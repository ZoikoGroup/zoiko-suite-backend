package scan_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/document-vault-svc/internal/scan"
)

// With no scanning engine configured the service must refuse uploads, not
// approve them: an absent scanner is never a clean verdict.
func TestUnconfiguredScanner_FailsClosed_ForEveryInput(t *testing.T) {
	var s scan.Scanner = scan.UnconfiguredScanner{}
	for _, content := range [][]byte{[]byte("harmless"), {}, []byte("any bytes at all")} {
		res, err := s.Scan(context.Background(), content, "text/plain")
		require.ErrorIs(t, err, scan.ErrNotConfigured)
		assert.False(t, res.Clean, "an error result must never read as clean")
	}
}
