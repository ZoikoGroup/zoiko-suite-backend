package telemetry

import (
	"go.uber.org/zap"
)

func NewLogger(level string) (*zap.Logger, error) {
	config := zap.NewProductionConfig()
	if level == "debug" {
		config.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	}
	return config.Build()
}
