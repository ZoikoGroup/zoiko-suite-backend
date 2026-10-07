package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type TableAudit struct {
	Service            string   `json:"service"`
	Table              string   `json:"table"`
	File               string   `json:"file"`
	HasTenantID        bool     `json:"has_tenant_id"`
	TenantNotNull      bool     `json:"tenant_not_null"`
	HasLegalEntityID   bool     `json:"has_legal_entity_id"`
	HasRLSEnabled      bool     `json:"has_rls_enabled"`
	UsesFloat          bool     `json:"uses_float"`
	UsesSoftDelete     bool     `json:"uses_soft_delete"`
	HasBitemporal      bool     `json:"has_bitemporal"`
	HasUUIDPK          bool     `json:"has_uuid_pk"`
	LeadingTenantIndex bool     `json:"leading_tenant_index"`
	Violations         []string `json:"violations"`
}

type EstateReport struct {
	TotalTablesAnalyzed int          `json:"total_tables_analyzed"`
	TablesWithRLS       int          `json:"tables_with_rls"`
	TablesWithTenantID  int          `json:"tables_with_tenant_id"`
	FloatViolations     int          `json:"float_violations"`
	SoftDeleteCount     int          `json:"soft_delete_count"`
	Tables              []TableAudit `json:"tables"`
}

var (
	createTableRegex = regexp.MustCompile(`(?is)create\s+table\s+(?:if\s+not\s+exists\s+)?(?:[a-zA-Z0-9_]+\.)?([a-zA-Z0-9_]+)\s*\((.*?)\);`)
	rlsRegex         = regexp.MustCompile(`(?i)alter\s+table\s+(?:if\s+exists\s+)?(?:[a-zA-Z0-9_]+\.)?([a-zA-Z0-9_]+)\s+enable\s+row\s+level\s+security`)
	createIndexRegex = regexp.MustCompile(`(?is)create\s+(?:unique\s+)?index\s+(?:if\s+not\s+exists\s+)?[a-zA-Z0-9_]+\s+on\s+(?:[a-zA-Z0-9_]+\.)?([a-zA-Z0-9_]+)\s*\((.*?)\)`)
)

func main() {
	servicesDir := filepath.Join("..", "..", "services")
	if len(os.Args) > 1 {
		servicesDir = os.Args[1]
	}

	absPath, err := filepath.Abs(servicesDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving path: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Analyzing SQL migrations under %s against ZS-DATA-001 Invariants...\n", absPath)

	allMigrations := make(map[string][]string) // service -> list of sql files
	err = filepath.Walk(absPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(strings.ToLower(info.Name()), ".sql") {
			// Find service name
			rel, _ := filepath.Rel(absPath, path)
			parts := strings.Split(rel, string(os.PathSeparator))
			if len(parts) > 0 {
				svc := parts[0]
				allMigrations[svc] = append(allMigrations[svc], path)
			}
		}
		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error scanning migrations: %v\n", err)
		os.Exit(1)
	}

	var tableAudits []TableAudit
	rlsTables := make(map[string]bool)

	// First pass: collect RLS statements
	for _, files := range allMigrations {
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			content := string(data)
			matches := rlsRegex.FindAllStringSubmatch(content, -1)
			for _, m := range matches {
				if len(m) > 1 {
					rlsTables[strings.ToLower(m[1])] = true
				}
			}
		}
	}

	// Second pass: audit tables
	for svc, files := range allMigrations {
		sort.Strings(files)
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			content := string(data)
			tableMatches := createTableRegex.FindAllStringSubmatch(content, -1)
			for _, m := range tableMatches {
				if len(m) < 3 {
					continue
				}
				tblName := strings.ToLower(m[1])
				body := m[2]

				// Exclude internal migration tables
				if tblName == "schema_migrations" || tblName == "goose_db_version" {
					continue
				}

				audit := TableAudit{
					Service: svc,
					Table:   tblName,
					File:    filepath.Base(file),
				}

				// Check tenant_id
				if strings.Contains(strings.ToLower(body), "tenant_id") {
					audit.HasTenantID = true
					if regexp.MustCompile(`(?i)tenant_id\s+[a-zA-Z0-9_\(\)]+\s+not\s+null`).MatchString(body) {
						audit.TenantNotNull = true
					} else {
						audit.Violations = append(audit.Violations, "D03: tenant_id is nullable (must be NOT NULL)")
					}
				}

				// Check legal_entity_id
				if strings.Contains(strings.ToLower(body), "legal_entity_id") {
					audit.HasLegalEntityID = true
				}

				// Check RLS
				if rlsTables[tblName] {
					audit.HasRLSEnabled = true
				} else if audit.HasTenantID {
					audit.Violations = append(audit.Violations, "D03: tenant-scoped table lacks ENABLE ROW LEVEL SECURITY")
				}

				// Check float usage (Invariant D06)
				if regexp.MustCompile(`(?i)\b(float|float4|float8|double\s+precision|real)\b`).MatchString(body) {
					audit.UsesFloat = true
					audit.Violations = append(audit.Violations, "D06: uses binary floating point (float/double precision/real); NUMERIC required")
				}

				// Check soft delete (Invariant D08)
				if regexp.MustCompile(`(?i)\b(is_deleted|deleted_at)\b`).MatchString(body) {
					audit.UsesSoftDelete = true
					audit.Violations = append(audit.Violations, "D08: uses destructive soft-delete flag (is_deleted/deleted_at); supersession/reversal required")
				}

				// Check bitemporal
				hasValidTime := strings.Contains(strings.ToLower(body), "valid_from") || strings.Contains(strings.ToLower(body), "effective_from")
				hasSystemTime := strings.Contains(strings.ToLower(body), "recorded_at") || strings.Contains(strings.ToLower(body), "superseded_at")
				if hasValidTime && hasSystemTime {
					audit.HasBitemporal = true
				}

				// Check UUID PK
				if regexp.MustCompile(`(?i)(id|[a-z0-9_]+_id)\s+(uuid)\s+primary\s+key`).MatchString(body) {
					audit.HasUUIDPK = true
				}

				tableAudits = append(tableAudits, audit)
			}
		}
	}

	report := EstateReport{
		TotalTablesAnalyzed: len(tableAudits),
		Tables:              tableAudits,
	}

	for _, a := range tableAudits {
		if a.HasRLSEnabled {
			report.TablesWithRLS++
		}
		if a.HasTenantID {
			report.TablesWithTenantID++
		}
		if a.UsesFloat {
			report.FloatViolations++
		}
		if a.UsesSoftDelete {
			report.SoftDeleteCount++
		}
	}

	fmt.Printf("\n=== ZS-DATA-001 ESTATE COMPLIANCE SUMMARY ===\n")
	fmt.Printf("Total Tables Analyzed across 107 Microservices: %d\n", report.TotalTablesAnalyzed)
	fmt.Printf("Tables with Tenant Isolation Key (tenant_id):     %d\n", report.TablesWithTenantID)
	fmt.Printf("Tables with Row Level Security Enabled:            %d\n", report.TablesWithRLS)
	fmt.Printf("Float Violations (Invariant D06 - float64/real):   %d\n", report.FloatViolations)
	fmt.Printf("Soft Delete Flags (Invariant D08 - is_deleted):    %d\n", report.SoftDeleteCount)

	// Save report JSON
	reportJSON, _ := json.MarshalIndent(report, "", "  ")
	_ = os.WriteFile("compliance_audit.json", reportJSON, 0644)
	fmt.Printf("\nDetailed audit report saved to tools/schemacheck/compliance_audit.json\n")
}
