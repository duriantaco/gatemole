package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	kernelmodelbroker "github.com/duriantaco/gatemole/internal/kernel/modelbroker"
	"github.com/duriantaco/gatemole/internal/kernel/sandbox"
	"github.com/duriantaco/gatemole/internal/kernel/store"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
)

const emptySHA256Digest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type executionCleanup func(context.Context, string, string) error

type modelExecutionRecovery func(
	context.Context,
	string,
	string,
	string,
	model.ModelBrokerExecution,
) (*model.ModelBrokerExecution, error)

type executionRecoveryKey struct {
	namespace     string
	transactionID string
}

func recoverAgentExecutions(
	ctx context.Context,
	kernelStore store.Store,
	cleanup executionCleanup,
	recoverModelExecution modelExecutionRecovery,
	now func() time.Time,
) (int, error) {
	projections, err := kernelStore.ListAllTransactions(ctx)
	if err != nil {
		return 0, err
	}
	runHeads := make(map[executionRecoveryKey]store.ExecutionHeads)
	for _, projection := range projections {
		if err := kernelStore.VerifyTransaction(
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
		if projection.Transaction.Admission == nil {
			continue
		}
		runID := projection.Transaction.Admission.RunID
		if err := kernelStore.VerifyRun(
			ctx,
			projection.Transaction.Namespace,
			runID,
		); err != nil {
			return 0, fmt.Errorf(
				"verify admitted run %s before recovery: %w",
				runID,
				err,
			)
		}
		run, err := kernelStore.GetRun(
			ctx,
			projection.Transaction.Namespace,
			runID,
		)
		if err != nil {
			return 0, fmt.Errorf("load admitted run %s before recovery: %w", runID, err)
		}
		if len(projection.Executions) > 0 {
			execution := projection.Executions[len(projection.Executions)-1]
			if execution.Status == model.AgentExecutionRunning {
				paired := run.Run.State == model.RunRunning &&
					run.Run.ActiveExecutionID == execution.ID
				legacyUnpaired := run.Run.State == model.RunAdmitted &&
					run.Run.ActiveExecutionID == ""
				if !paired && !legacyUnpaired {
					return 0, fmt.Errorf(
						"active execution %s disagrees with admitted run %s",
						execution.ID,
						runID,
					)
				}
			} else if run.Run.ActiveExecutionID != "" {
				return 0, fmt.Errorf(
					"settled execution %s disagrees with active admitted run %s",
					execution.ID,
					runID,
				)
			}
		}
		runHeads[executionRecoveryKey{
			namespace:     projection.Transaction.Namespace,
			transactionID: projection.Transaction.ID,
		}] = store.ExecutionHeads{
			TransactionSequence: projection.Transaction.EventSequence,
			TransactionDigest:   projection.LastEventDigest,
			RunSequence:         run.Run.EventSequence,
			RunDigest:           run.LastEventDigest,
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
		modelExecution := execution.ModelBroker
		if modelExecution != nil && recoverModelExecution != nil {
			modelExecution, err = recoverModelExecution(
				ctx,
				projection.Transaction.ID,
				execution.RunID,
				execution.ID,
				*modelExecution,
			)
			if err != nil {
				return recovered, fmt.Errorf(
					"recover model receipts for execution %s: %w",
					execution.ID,
					err,
				)
			}
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
				ModelBroker:  modelExecution,
			},
		)
		if err != nil {
			return recovered, fmt.Errorf("build interrupted execution receipt for %s: %w", execution.ID, err)
		}
		if projection.Transaction.Admission != nil {
			if _, err := kernelStore.AppendPairedExecutionEvent(
				ctx,
				projection.Transaction.Namespace,
				runHeads[executionRecoveryKey{
					namespace:     projection.Transaction.Namespace,
					transactionID: projection.Transaction.ID,
				}],
				event,
			); err != nil {
				return recovered, fmt.Errorf("persist paired interrupted execution receipt for %s: %w", execution.ID, err)
			}
		} else if _, err := kernelStore.AppendTransactionEvents(
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

func recoverModelExecutionFrom(
	transactionRoot string,
) modelExecutionRecovery {
	if strings.TrimSpace(transactionRoot) == "" {
		return nil
	}
	return func(
		_ context.Context,
		transactionID string,
		runID string,
		executionID string,
		original model.ModelBrokerExecution,
	) (*model.ModelBrokerExecution, error) {
		directory := sandbox.ModelBrokerExecutionEvidenceDirectory(
			transactionRoot,
			transactionID,
			runID,
			executionID,
		)
		path := filepath.Join(directory, "model-calls.jsonl")
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if _, directoryErr := os.Stat(directory); directoryErr == nil {
				copy := original
				return &copy, nil
			} else if !errors.Is(directoryErr, os.ErrNotExist) {
				return nil, fmt.Errorf("inspect model receipt directory: %w", directoryErr)
			}
			// An execution started by the previous layout used one flat ledger
			// for the transaction/run pair. New starts pre-create their scoped
			// directory, so fallback cannot attach an older retry's flat ledger.
			path = filepath.Join(
				transactionRoot,
				".gatemole-model-evidence",
				sandbox.ModelBrokerContainerName(transactionID, runID),
				"model-calls.jsonl",
			)
			if _, legacyErr := os.Stat(path); errors.Is(legacyErr, os.ErrNotExist) {
				copy := original
				return &copy, nil
			} else if legacyErr != nil {
				return nil, fmt.Errorf("inspect legacy model receipt ledger: %w", legacyErr)
			}
		} else if err != nil {
			return nil, fmt.Errorf("inspect model receipt ledger: %w", err)
		}
		summary, err := kernelmodelbroker.FinalizeLedger(
			path,
			transactionID,
			runID,
			original.Provider,
		)
		if err != nil {
			return nil, err
		}
		return &model.ModelBrokerExecution{
			Provider:            original.Provider,
			ImageDigest:         original.ImageDigest,
			PolicyDigest:        original.PolicyDigest,
			ReceiptLedgerDigest: summary.Digest,
			Calls:               summary.Calls,
			CompletedCalls:      summary.Completed,
			FailedCalls:         summary.Failed,
			UnknownCalls:        summary.Unknown,
			InputTokens:         summary.InputTokens,
			OutputTokens:        summary.OutputTokens,
		}, nil
	}
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
