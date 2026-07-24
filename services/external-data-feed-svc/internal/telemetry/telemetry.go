package telemetry

import "go.uber.org/zap"

func InitLogger() (*zap.Logger, error) {
	return zap.NewProduction()
}
