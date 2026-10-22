package registrytest_test

import (
	"testing"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
)

func TestFileContractIsValid(t *testing.T) {
	c := registrytest.WithFileOperation(registrytest.Content())
	if issues := registry.ValidateContent(&c, registry.DefaultLimits()); len(issues) > 0 {
		t.Fatalf("%v", issues)
	}
	v := &registry.ContractVersion{Content: c}
	bi := v.BatchFor(v.Operation("statement-file"))
	if bi == nil || bi.MaxRows != 1000 || !bi.Gzip() || !bi.RequireRecordCount || bi.MaxExpansionRatio != 20 || !bi.FileNamePattern.MatchString("stmt-0001.csv.gz") || bi.FileNamePattern.MatchString("evil.sh") {
		t.Fatalf("%+v", bi)
	}
	sch := v.SchemaByRef("stmt-csv@1")
	if lay, ok := sch.CSVLayout(); !ok || len(lay.Columns) != 5 || lay.Delimiter != ',' || !lay.Header || len(lay.RowSchema) == 0 {
		t.Fatalf("%+v", lay)
	}
}

const sampleHostKey = "SHA256:" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ" // 43 characters

func TestSFTPPullContractIsValidAndAnUnpinnedOneIsNot(t *testing.T) {
	c := registrytest.WithSFTPPull(registrytest.WithFileOperation(registrytest.Content()), sampleHostKey)
	if issues := registry.ValidateContent(&c, registry.DefaultLimits()); len(issues) > 0 {
		t.Fatalf("%v", issues)
	}
	v := &registry.ContractVersion{Content: c}
	op := v.Operation("statement-sftp")
	if op == nil || op.Channel != "SFTP" || op.EndpointProfile != "sftp-ep" || v.BatchFor(op) == nil {
		t.Fatalf("%+v", op)
	}
	// an SFTP endpoint with no pinned host key is refused: never trusted on first use
	bad := registrytest.WithSFTPPull(registrytest.WithFileOperation(registrytest.Content()), "SHA256:short")
	if issues := registry.ValidateContent(&bad, registry.DefaultLimits()); len(issues) == 0 {
		t.Fatal("a malformed host key was accepted")
	}
}
