package store

import (
	"context"

	"github.com/duriantaco/vouch/internal/kernel/admission"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

// RunStore reads durable materialized run projections.
type RunStore interface {
	GetRun(context.Context, string, string) (reducer.Projection, error)
	ListRuns(context.Context, string) ([]model.AgentRun, error)
	ListAllRuns(context.Context) ([]reducer.Projection, error)
}

// EventStore appends and verifies the authoritative event history.
type EventStore interface {
	CreateRun(context.Context, model.RunEvent) (reducer.Projection, error)
	AppendEvent(context.Context, string, int64, model.RunEvent) (reducer.Projection, error)
	Events(context.Context, string, string, int64) ([]model.RunEvent, error)
	VerifyRun(context.Context, string, string) error
}

type TransactionStore interface {
	CreateTransaction(context.Context, model.TransactionEvent) (transactionreducer.Projection, error)
	AppendTransactionEvents(context.Context, string, int64, []model.TransactionEvent) (transactionreducer.Projection, error)
	GetTransaction(context.Context, string, string) (transactionreducer.Projection, error)
	ListTransactions(context.Context, string) ([]model.AgentTransaction, error)
	ListAllTransactions(context.Context) ([]transactionreducer.Projection, error)
	TransactionEvents(context.Context, string, string, int64) ([]model.TransactionEvent, error)
	VerifyTransaction(context.Context, string, string) error
}

// AdmissionStore atomically creates and retrieves the immutable authority
// envelope that binds a task, contract, run, capabilities, and transaction.
type AdmissionStore interface {
	AdmitTask(context.Context, admission.Prepared) (admission.Result, bool, error)
	GetTaskAdmission(context.Context, string, string) (admission.Result, string, error)
}

// Store combines the local kernel persistence boundaries with lifecycle
// management. Implementations must commit an event and its projection in one
// transaction.
type Store interface {
	RunStore
	EventStore
	TransactionStore
	AdmissionStore
	Health(context.Context) error
	Close() error
}
