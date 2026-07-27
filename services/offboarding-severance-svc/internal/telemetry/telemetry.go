package telemetry

import (
	"go.uber.org/zap"
)

func NewLogger(serviceName string) (*zap.Logger, error) {
	config := zap.NewProductionConfig()
	config.OutputPaths = []string{"stdout"}
	logger, err := config.Build(zap.Fields(zap.String("service", serviceName)))
	if err != nil {
		return nil, err
	}
	return logger, nil
}
