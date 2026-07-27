package store

import (
	"context"

	"zoiko.io/tax-authority-interface-svc/internal/domain"
)

type Store interface {
	CreateInterface(ctx context.Context, tf *domain.TaxInterface) error
	GetInterfaceByID(ctx context.Context, id string) (*domain.TaxInterface, error)
	ListInterfaces(ctx context.Context, legalEntityID string) ([]domain.TaxInterface, error)
	CreateSubmission(ctx context.Context, sub *domain.TaxFilingSubmission) error
	GetSubmissionByID(ctx context.Context, id string) (*domain.TaxFilingSubmission, error)
	ListSubmissions(ctx context.Context, interfaceID string) ([]domain.TaxFilingSubmission, error)
}
