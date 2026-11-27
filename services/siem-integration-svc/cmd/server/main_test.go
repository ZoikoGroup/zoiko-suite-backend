package main

import "testing"

// A missing DATABASE_URL is a startup error, never a hard-coded fallback DSN.
func TestDatabaseURL_RequiredNoFallback(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if dsn, err := databaseURL(); err == nil || dsn != "" {
		t.Fatalf("unset DATABASE_URL must fail startup, got %q, %v", dsn, err)
	}
	t.Setenv("DATABASE_URL", "postgres://app@db:5432/siem_integration")
	if dsn, err := databaseURL(); err != nil || dsn != "postgres://app@db:5432/siem_integration" {
		t.Fatalf("set DATABASE_URL: got %q, %v", dsn, err)
	}
}
