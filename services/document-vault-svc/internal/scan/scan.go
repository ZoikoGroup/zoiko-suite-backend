// Package scan is the upload-time malware/type gate BIZ-01's own
// failure-semantics line requires: "Malware/type/hash failure quarantines
// upload." Every upload (CreateDocument, AddVersion) runs through a
// Scanner before its bytes ever reach storage or a database row.
package scan

import (
	"context"
	"errors"
)

// Result is a scan verdict for one upload attempt. Reason is populated
// only when Clean is false, and is returned to the caller and recorded
// on the quarantine event — never silently swallowed.
type Result struct {
	Clean  bool
	Reason string
}

// Scanner is the interface CreateDocument/AddVersion depend on. A real
// implementation (ClamAV, a cloud AV API, a file-type sniffer comparing
// declared content-type against actual magic bytes) can be substituted
// here without either handler changing.
type Scanner interface {
	Scan(ctx context.Context, content []byte, contentType string) (Result, error)
}

// ErrNotConfigured is returned by UnconfiguredScanner. The handlers treat any
// scanner error as "scan unavailable" and refuse the upload (503
// scan_unavailable) before anything is stored.
var ErrNotConfigured = errors.New("no malware scanner configured (CLAMAV_URL is unset)")

// UnconfiguredScanner is what the service runs with when no scanning engine is
// configured. It FAILS CLOSED: every Scan returns ErrNotConfigured, so every
// upload and every new version is refused until a scanner is configured.
//
// This replaces NoOpScanner, which reported every upload as clean. An absent
// scanner is not a clean result — treating it as one made DRC-I18 ("Malware /
// quarantined content cannot become ACTIVE") true only on deployments that
// happened to set CLAMAV_URL, and the service gave no sign of which kind it
// was. Reads, record, retention and preservation routes are unaffected.
type UnconfiguredScanner struct{}

func (UnconfiguredScanner) Scan(_ context.Context, _ []byte, _ string) (Result, error) {
	return Result{}, ErrNotConfigured
}
