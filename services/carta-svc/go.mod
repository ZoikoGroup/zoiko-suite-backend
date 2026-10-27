module zoiko.io/carta-svc

go 1.25.0

require (
	github.com/go-chi/chi/v5 v5.0.12
	github.com/google/uuid v1.6.0
	go.uber.org/zap v1.27.0
	zoiko.io/identity-envelope v0.0.0
)

require (
	github.com/stretchr/testify v1.9.0 // indirect
	go.uber.org/multierr v1.10.0 // indirect
)

replace zoiko.io/identity-envelope => ../identity-envelope
