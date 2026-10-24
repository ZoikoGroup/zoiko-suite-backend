package store_test

import (
	"os"

	"go.uber.org/zap"
)

// kenvLogger is silent unless KENV_LOG is set, when handler errors are printed: a 503 in a test is otherwise a mystery.
func kenvLogger() *zap.Logger {
	if os.Getenv("KENV_LOG") != "" {
		l, _ := zap.NewDevelopment()
		return l
	}
	return zap.NewNop()
}
