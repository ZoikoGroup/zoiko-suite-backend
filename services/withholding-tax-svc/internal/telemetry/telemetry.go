package telemetry

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func NewLogger(serviceName string) (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	cfg.EncoderConfig.TimeKey = "timestamp"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.InitialFields = map[string]interface{}{
		"service": serviceName,
	}
	return cfg.Build()
}
