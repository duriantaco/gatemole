package store

import (
	"context"

	"github.com/duriantaco/gatemole/internal/kernel/admission"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/reducer"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
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
	GetExecutionAuthority(context.Context, string, string) (ExecutionAuthoritySnapshot, error)
	GetExecutionAuthorityForRun(context.Context, string, string) (ExecutionAuthoritySnapshot, error)
}

// ExecutionHeads pins both ledgers at one authority snapshot. A paired append
// must reject the whole mutation when either head has changed.
type ExecutionHeads struct {
	TransactionSequence int64
	TransactionDigest   string
	RunSequence         int64
	RunDigest           string
}

// ExecutionProjection is the result of one atomically paired execution event.
type ExecutionProjection struct {
	Run         reducer.Projection
	Transaction transactionreducer.Projection
}

// ExecutionStore owns the cross-ledger execution lifecycle. Implementations
// must append the run and transaction events and update both projections in
// one database transaction.
type ExecutionStore interface {
	AppendPairedExecutionEvent(context.Context, string, ExecutionHeads, model.TransactionEvent) (ExecutionProjection, error)
}

// ExecutionAuthoritySnapshot is the consistently read authority envelope used
// immediately before execution. The task, contract, and grants are immutable
// admission outputs; the run and transaction are their current projections.
type ExecutionAuthoritySnapshot struct {
	Task        model.AgentTask
	Contract    model.ExecutionContract
	Grants      []model.CapabilityGrant
	Run         reducer.Projection
	Transaction transactionreducer.Projection
}

// Store combines the local kernel persistence boundaries with lifecycle
// management. Implementations must commit an event and its projection in one
// transaction.
type Store interface {
	RunStore
	EventStore
	TransactionStore
	AdmissionStore
	ExecutionStore
	Health(context.Context) error
	Close() error
}
