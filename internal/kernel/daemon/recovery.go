package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/sandbox"
	"github.com/duriantaco/vouch/internal/kernel/store"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

const emptySHA256Digest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type executionCleanup func(context.Context, string, string) error

func recoverAgentExecutions(
	ctx context.Context,
	transactionStore store.TransactionStore,
	cleanup executionCleanup,
	now func() time.Time,
) (int, error) {
	projections, err := transactionStore.ListAllTransactions(ctx)
	if err != nil {
		return 0, err
	}
	for _, projection := range projections {
		if err := transactionStore.VerifyTransaction(
			ctx,
			projection.Transaction.Namespace,
			projection.Transaction.ID,
		); err != nil {
			return 0, fmt.Errorf(
				"verify transaction %s before recovery: %w",
				projection.Transaction.ID,
				err,
			)
		}
	}
	recovered := 0
	for _, projection := range projections {
		if len(projection.Executions) == 0 {
			continue
		}
		execution := projection.Executions[len(projection.Executions)-1]
		if execution.Status != model.AgentExecutionRunning {
			continue
		}
		if cleanup == nil {
			return recovered, errors.New("cannot recover an active agent execution without an OCI engine")
		}
		if err := cleanup(ctx, projection.Transaction.ID, execution.RunID); err != nil {
			return recovered, fmt.Errorf("clean up interrupted execution %s: %w", execution.ID, err)
		}
		event, err := transactionreducer.NextEvent(
			projection,
			transactionreducer.EventAgentExecutionFinished,
			model.Principal{ID: "service:gatemoled-recovery", Kind: model.PrincipalService},
			now().UTC(),
			transactionreducer.AgentExecutionFinishedPayload{
				ExecutionID:  execution.ID,
				Status:       model.AgentExecutionInterrupted,
				StdoutDigest: emptySHA256Digest,
				StderrDigest: emptySHA256Digest,
				ModelBroker:  execution.ModelBroker,
			},
		)
		if err != nil {
			return recovered, fmt.Errorf("build interrupted execution receipt for %s: %w", execution.ID, err)
		}
		if _, err := transactionStore.AppendTransactionEvents(
			ctx,
			projection.Transaction.Namespace,
			projection.Transaction.EventSequence,
			[]model.TransactionEvent{event},
		); err != nil {
			return recovered, fmt.Errorf("persist interrupted execution receipt for %s: %w", execution.ID, err)
		}
		recovered++
	}
	return recovered, nil
}

func engineExecutionCleanup(enginePath string) executionCleanup {
	if strings.TrimSpace(enginePath) == "" {
		return nil
	}
	return func(ctx context.Context, transactionID, runID string) error {
		agentErr := sandbox.RemoveContainer(
			ctx, enginePath, sandbox.ContainerName(transactionID, runID),
		)
		brokerErr := sandbox.RemoveModelBroker(ctx, enginePath, sandbox.ModelBrokerSession{
			NetworkName:   sandbox.ModelBrokerNetworkName(transactionID, runID),
			ContainerName: sandbox.ModelBrokerContainerName(transactionID, runID),
		})
		return errors.Join(agentErr, brokerErr)
	}
}
