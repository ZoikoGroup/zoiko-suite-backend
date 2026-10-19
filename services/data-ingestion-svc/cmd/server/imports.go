package main

import (
	"context"
	"errors"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/data-ingestion-svc/internal/authz"
	"zoiko.io/data-ingestion-svc/internal/config"
	"zoiko.io/data-ingestion-svc/internal/handler"
	"zoiko.io/data-ingestion-svc/internal/imports"
	"zoiko.io/data-ingestion-svc/internal/store"
	"zoiko.io/eventing/scan"
)

// wireImports builds the INT-06 engine, starts its workers and returns the route registration. It
// returns nil when imports are off.
func wireImports(ctx context.Context, cfg *config.Config, log *zap.Logger, st *store.PgStore, a *authz.Client) func(chi.Router) {
	ic := cfg.Import
	if !ic.Enabled {
		log.Info("controlled imports are off (IMPORT_ENABLED is not true)")
		return nil
	}
	targets, err := imports.NewHTTPTargets(ic.Targets, ic.TargetTimeout)
	if err != nil {
		log.Fatal("imports: targets", zap.Error(err))
	}
	// There is no silent "clean" default: with no scanner configured, imports are refused.
	var scanner imports.ContentScanner
	chain, err := scan.Require(ic.ClamAVAddr, ic.AllowBuiltinScanOnly, ic.MaxBatchBytes, 60*time.Second)
	switch {
	case err == nil:
		scanner = chain
		if ic.ClamAVAddr == "" {
			log.Warn("imports are scanned by the built-in content checks ONLY (IMPORT_ALLOW_BUILTIN_SCAN_ONLY): this is not a malware engine; do not use it in production")
		}
	case errors.Is(err, scan.ErrNoScanner):
		log.Error("imports are REFUSED: no malware scanner is configured (set IMPORT_CLAMAV_ADDR; development may set IMPORT_ALLOW_BUILTIN_SCAN_ONLY=true)")
	default:
		log.Fatal("imports: scanner", zap.Error(err))
	}

	icfg := imports.DefaultConfig()
	icfg.MaxBatchBytes, icfg.MaxChunkBytes, icfg.ExecWorkers = ic.MaxBatchBytes, int(ic.MaxChunkBytes), ic.ExecWorkers
	svc, err := imports.NewService(imports.Deps{Repo: st, Scanner: scanner, Targets: targets, Config: icfg})
	if err != nil {
		log.Fatal("imports: service", zap.Error(err))
	}

	go imports.NewValidationRunner(svc, log).Run(ctx, ic.WorkerIdle)
	go imports.NewExecutor(svc, log, nil).Run(ctx, ic.WorkerIdle)

	h := handler.NewImportHandler(svc, a, log, ic.MaxChunkBytes)
	return func(r chi.Router) { handler.RegisterImportRoutes(r, h) }
}
