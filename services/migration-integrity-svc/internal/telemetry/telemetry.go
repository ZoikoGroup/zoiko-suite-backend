package telemetry

import "go.uber.org/zap"

func NewLogger(level string) (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	if level == "debug" {
		cfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	}
	return cfg.Build()
}
