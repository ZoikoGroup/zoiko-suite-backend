package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// ImportConfig configures INT-06 controlled import (ZS-SVC-M-001). It is off by default: it needs
// configured targets (the owning domains imports are executed against) and a malware scanner.
type ImportConfig struct {
	Enabled bool

	// Targets are the owning-domain services an import profile may name, key -> base URL. A
	// profile can only point at a key listed here, never at an address of its own.
	Targets map[string]string

	// ClamAVAddr is the clamd daemon (host:port). With it empty, imports refuse to scan unless
	// AllowBuiltinScanOnly is set: there is no silent "clean" default. Built-in checks alone are
	// type, size and signature checks, not a malware engine, and the flag is for development.
	ClamAVAddr           string
	AllowBuiltinScanOnly bool

	// MaxBatchBytes defaults to 24 MiB because clamd refuses a stream longer than its StreamMaxLength
	// (25 MiB by default): a batch over that could never be scanned. Raise both together.
	MaxBatchBytes int64
	MaxChunkBytes int64
	ExecWorkers   int
	WorkerIdle    time.Duration
	TargetTimeout time.Duration
}

func loadImport() (ImportConfig, error) {
	c := ImportConfig{
		Enabled:              getEnv("IMPORT_ENABLED", "false") == "true",
		ClamAVAddr:           os.Getenv("IMPORT_CLAMAV_ADDR"),
		AllowBuiltinScanOnly: getEnv("IMPORT_ALLOW_BUILTIN_SCAN_ONLY", "false") == "true",
		MaxBatchBytes:        envInt64("IMPORT_MAX_BATCH_BYTES", 24<<20),
		MaxChunkBytes:        envInt64("IMPORT_MAX_CHUNK_BYTES", 4<<20),
		ExecWorkers:          int(envInt64("IMPORT_EXEC_WORKERS", 8)),
		WorkerIdle:           envDur("IMPORT_WORKER_IDLE", 2*time.Second),
		TargetTimeout:        envDur("IMPORT_TARGET_TIMEOUT", 15*time.Second),
		Targets:              map[string]string{},
	}
	for _, pair := range strings.Split(os.Getenv("IMPORT_TARGETS"), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return c, errors.New("IMPORT_TARGETS must be a comma-separated list of key=baseURL")
		}
		c.Targets[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if c.Enabled && len(c.Targets) == 0 {
		return c, errors.New("IMPORT_TARGETS is required when imports are enabled")
	}
	if c.MaxBatchBytes < 1 || c.MaxChunkBytes < 1 || c.MaxChunkBytes > 8<<20 {
		return c, errors.New("IMPORT_MAX_BATCH_BYTES and IMPORT_MAX_CHUNK_BYTES (at most 8 MiB) must be positive")
	}
	return c, nil
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}
