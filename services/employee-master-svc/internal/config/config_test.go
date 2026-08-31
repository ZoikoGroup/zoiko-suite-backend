package config

import (
	"strings"
	"testing"
)

// A compose deployment sets no schema and no options: the service owns a whole
// database and its tables sit in that database's public schema.
func TestDSN_OmitsSchemaAndOptionsWhenUnset(t *testing.T) {
	d := DBConfig{
		Host: "localhost", Port: 5432, Name: "employee_master",
		User: "postgres", Password: "secret", SSLMode: "disable",
	}

	dsn := d.DSN()
	if strings.Contains(dsn, "search_path") {
		t.Errorf("unset Schema must not emit search_path: %s", dsn)
	}
	if !strings.Contains(dsn, "dbname=employee_master") {
		t.Errorf("expected dbname in DSN: %s", dsn)
	}
}

// A Supabase deployment has one database, so isolation comes from the schema.
func TestDSN_EmitsSearchPathWhenSchemaSet(t *testing.T) {
	d := DBConfig{
		Host: "db.example.supabase.co", Port: 6543, Name: "postgres",
		User: "app_employee_master.abcdef", Password: "secret",
		SSLMode: "require", Schema: "employee_master",
	}

	dsn := d.DSN()
	if !strings.Contains(dsn, "search_path=employee_master") {
		t.Errorf("expected search_path=employee_master: %s", dsn)
	}
}

// DB_OPTIONS carries the pgx settings the transaction pooler needs. Appended
// verbatim, and last, so it can override anything built above it.
func TestDSN_AppendsOptionsVerbatim(t *testing.T) {
	opts := "default_query_exec_mode=exec statement_cache_capacity=0"
	d := DBConfig{
		Host: "h", Port: 6543, Name: "postgres", User: "u", Password: "p",
		SSLMode: "require", Schema: "employee_master", Options: opts,
	}

	dsn := d.DSN()
	if !strings.HasSuffix(dsn, opts) {
		t.Errorf("expected DSN to end with the options verbatim: %s", dsn)
	}
}

// The password used to be interpolated bare. A managed host generates passwords
// containing spaces and quotes, and either one silently produced a malformed
// DSN that pgx then reported as an authentication failure — pointing at the
// credential rather than at the encoding of it.
func TestDSN_QuotesPasswordSafely(t *testing.T) {
	for _, tc := range []struct {
		name     string
		password string
		want     string
	}{
		{"plain", "secret", `password='secret'`},
		{"with space", "two words", `password='two words'`},
		{"with quote", "it's", `password='it\'s'`},
		{"with backslash", `back\slash`, `password='back\\slash'`},
		{"empty", "", `password=''`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DBConfig{
				Host: "h", Port: 5432, Name: "n", User: "u",
				Password: tc.password, SSLMode: "require",
			}

			dsn := d.DSN()
			if !strings.Contains(dsn, tc.want) {
				t.Errorf("expected %s in DSN, got: %s", tc.want, dsn)
			}
			// sslmode follows password, so a password that broke out of its
			// quoting would swallow it.
			if !strings.Contains(dsn, "sslmode=require") {
				t.Errorf("password quoting swallowed a later field: %s", dsn)
			}
		})
	}
}

// TEST_DATABASE_URL short-circuits the builder entirely; the store tests rely
// on that, so a change to DSN() must not break the escape hatch.
func TestDSN_TestDatabaseURLWins(t *testing.T) {
	t.Setenv("TEST_DATABASE_URL", "postgres://someone@elsewhere/db")

	d := DBConfig{Host: "ignored", Schema: "ignored"}
	if got := d.DSN(); got != "postgres://someone@elsewhere/db" {
		t.Errorf("expected TEST_DATABASE_URL to win, got %s", got)
	}
}
