package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Port         string
	DatabaseURL  string
	KafkaBrokers string
	KafkaTopic   string
	AuthzURL     string
	LogLevel     string

	// EventResidencyRegion is the residencyregion every event this service
	// emits carries (ZS-EVENT-001 §4: required, "authoritative regional
	// data-bearing context"). Deliberately NO default: a wrong region
	// silently stamped on every financial event is worse than refusing to
	// start. EVENT_RESIDENCY_REGION, a lower-case code such as "uk" or
	// "eu-west".
	EventResidencyRegion string

	// FinancialControlURL is financial-control-svc's base URL — the
	// governed, versioned TolerancePolicy registry AnalyzeUnmatchedStatementLines
	// fetches its write-off tolerances from (ZS-SVC-Z-001 INV-09).
	FinancialControlURL string

	// BankReconciliationURL is bank-reconciliation-svc's base URL — the
	// owner of the statement lines and runs this service analyzes. Read
	// only: nothing here writes to it (see internal/bankrec).
	BankReconciliationURL string

	// GeneralLedgerURL is general-ledger-svc's base URL — read-only, for the
	// FINALIZED ledger entries a candidate search proposes a statement
	// line's book side from (A2a; see internal/ledger).
	GeneralLedgerURL string

	// CandidateSearchHorizonDays bounds how far from a statement date the
	// candidate search reads (CANDIDATE_SEARCH_HORIZON_DAYS, 1-366, default
	// 31). It is a read bound, not a match criterion: whether a journal is an
	// AUTO_MATCH or only a DATE_SKEW is decided by the governed
	// RECONCILIATION_CANDIDATE_DATE_MATCH tolerance from financial-control-svc.
	// The window actually read is never narrower than that tolerance.
	CandidateSearchHorizonDays int

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthzURL (plain HTTP) keeps being used unless
	// explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string
}

func Load() (*Config, error) {
	region := os.Getenv("EVENT_RESIDENCY_REGION")
	if region == "" {
		return nil, errors.New("EVENT_RESIDENCY_REGION is required (e.g. uk, eu-west): every emitted event must carry its residency region")
	}

	port := getEnv("PORT", "8137")
	dbURL := getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/reconciliation_intelligence?sslmode=disable")
	kafkaBrokers := getEnv("KAFKA_BROKERS", "localhost:9092")
	kafkaTopic := getEnv("KAFKA_TOPIC", "zoiko.reconciliation-intelligence.events")
	authzURL := getEnv("AUTHZ_SERVICE_URL", "http://localhost:8089")
	logLevel := getEnv("LOG_LEVEL", "info")
	financialControlURL := getEnv("FINANCIAL_CONTROL_SERVICE_URL", "http://financial-control-svc:8171")
	bankReconciliationURL := getEnv("BANK_RECONCILIATION_SERVICE_URL", "http://bank-reconciliation-svc:8102")
	generalLedgerURL := getEnv("GENERAL_LEDGER_SERVICE_URL", "http://general-ledger-svc:8098")
	horizon, err := strconv.Atoi(getEnv("CANDIDATE_SEARCH_HORIZON_DAYS", "31"))
	if err != nil || horizon < 1 || horizon > 366 {
		return nil, fmt.Errorf("CANDIDATE_SEARCH_HORIZON_DAYS must be a whole number of days from 1 to 366")
	}

	return &Config{
		EventResidencyRegion: region,
		Port:                 port,
		DatabaseURL:          dbURL,
		KafkaBrokers:         kafkaBrokers,
		KafkaTopic:           kafkaTopic,
		AuthzURL:             authzURL,
		LogLevel:             logLevel,
		FinancialControlURL:  financialControlURL,

		BankReconciliationURL: bankReconciliationURL,

		GeneralLedgerURL:           generalLedgerURL,
		CandidateSearchHorizonDays: horizon,

		AuthzMTLSEnabled:         getEnv("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnv("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
	}, nil
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
