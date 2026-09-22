// Package scan is the upload-time malware/type gate BIZ-01's own
// failure-semantics line requires: "Malware/type/hash failure quarantines
// upload." Every upload (CreateDocument, AddVersion) runs through a
// Scanner before its bytes ever reach storage or a database row.
package scan

import "context"

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

// NoOpScanner is the only Scanner this repo has today. It is NOT a real
// malware/type scanner — no scanning engine is integrated anywhere in
// this codebase, and this service has never had one. It exists so the
// GATE is real and exercised (the interface, the call site in
// CreateDocument/AddVersion, the 422 rejection path, and the tests
// proving a positive Result actually blocks an upload before it touches
// storage), while being explicit, not silent, about what is not yet
// backed by a real scan. Swapping in a real implementation is a
// cmd/server wiring change; CreateDocument/AddVersion would not need to
// change at all.
type NoOpScanner struct{}

func (NoOpScanner) Scan(_ context.Context, _ []byte, _ string) (Result, error) {
	return Result{Clean: true}, nil
}
