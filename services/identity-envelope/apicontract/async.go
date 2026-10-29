package apicontract

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type OperationState string

const (
	OperationQueued          OperationState = "queued"
	OperationRunning         OperationState = "running"
	OperationWaitingExternal OperationState = "waiting_external"
	OperationSucceeded       OperationState = "succeeded"
	OperationFailed          OperationState = "failed"
	OperationCancelRequested OperationState = "cancel_requested"
	OperationCancelled       OperationState = "cancelled"
)

var (
	ErrOperationNotFound      = errors.New("apicontract: operation not found")
	ErrOperationConflict      = errors.New("apicontract: operation conflicts with an existing idempotent request")
	ErrOperationNotCancelable = errors.New("apicontract: operation is no longer cancelable")
)

func (s OperationState) Valid() bool {
	switch s {
	case OperationQueued, OperationRunning, OperationWaitingExternal, OperationSucceeded,
		OperationFailed, OperationCancelRequested, OperationCancelled:
		return true
	}
	return false
}

func (s OperationState) Terminal() bool {
	return s == OperationSucceeded || s == OperationFailed || s == OperationCancelled
}

func CanTransition(from, to OperationState) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to {
		return true
	}
	switch from {
	case OperationQueued:
		return to == OperationRunning || to == OperationCancelRequested || to == OperationFailed || to == OperationCancelled
	case OperationRunning:
		return to == OperationWaitingExternal || to == OperationSucceeded || to == OperationFailed || to == OperationCancelRequested
	case OperationWaitingExternal:
		return to == OperationRunning || to == OperationSucceeded || to == OperationFailed || to == OperationCancelRequested
	case OperationCancelRequested:
		return to == OperationCancelled || to == OperationSucceeded || to == OperationFailed
	}
	return false
}

type OperationProgress struct {
	CompletedUnits int64 `json:"completed_units"`
	TotalUnits     int64 `json:"total_units,omitempty"`
}

type Operation struct {
	ID             string             `json:"id"`
	TenantID       string             `json:"-"`
	PrincipalID    string             `json:"-"`
	Type           string             `json:"type"`
	Route          string             `json:"-"`
	State          OperationState     `json:"state"`
	Progress       *OperationProgress `json:"progress,omitempty"`
	ResourceRef    string             `json:"resource_ref,omitempty"`
	Problem        *Problem           `json:"problem,omitempty"`
	Cancelable     bool               `json:"cancelable"`
	IdempotencyKey string             `json:"-"`
	RequestHash    string             `json:"-"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
	StartedAt      *time.Time         `json:"started_at,omitempty"`
	FinishedAt     *time.Time         `json:"finished_at,omitempty"`
}

type OperationStore interface {
	Create(ctx context.Context, op Operation) (Operation, bool, error)
	Get(ctx context.Context, tenantID, operationID string) (Operation, error)
	FindByKey(ctx context.Context, tenantID, principalID, route, idempotencyKey string) (Operation, bool, error)
	Advance(ctx context.Context, tenantID, operationID string, to OperationState, progress *OperationProgress, problem *Problem, resourceRef string) (Operation, error)
	RequestCancel(ctx context.Context, tenantID, operationID string) (Operation, error)
}

func NewOperationID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("op_%x", b)
}

func (o Operation) Validate() error {
	if o.ID == "" {
		return errors.New("apicontract: operation id is required")
	}
	if o.TenantID == "" {
		return errors.New("apicontract: operation tenant is required")
	}
	if o.Type == "" {
		return errors.New("apicontract: operation type is required")
	}
	if !o.State.Valid() {
		return fmt.Errorf("apicontract: invalid operation state %q", o.State)
	}
	return nil
}

func (o Operation) ProgressValues() (completed, total int64) {
	if o.Progress == nil {
		return 0, 0
	}
	return o.Progress.CompletedUnits, o.Progress.TotalUnits
}

func WriteOperation(w http.ResponseWriter, status int, op Operation) {
	if status == http.StatusAccepted {
		SetLocation(w, OperationLocation(op.ID))
	}
	WriteJSON(w, status, op)
}
