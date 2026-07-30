package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	kernelmodelbroker "github.com/duriantaco/gatemole/internal/kernel/modelbroker"
	"github.com/duriantaco/gatemole/internal/kernel/runtimeidentity"
	"github.com/duriantaco/gatemole/internal/kernel/sandbox"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
	"github.com/duriantaco/gatemole/internal/kernel/transaction/gitstage"
	"github.com/duriantaco/gatemole/internal/kernel/verification"
)

const emptySHA256Digest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type transactionMutationRequest struct {
	ExpectedSequence int64           `json:"expected_sequence"`
	Actor            model.Principal `json:"actor"`
}

type createWorktreeRequest struct {
	ExpectedSequence int64           `json:"expected_sequence"`
	Revision         string          `json:"revision,omitempty"`
	Actor            model.Principal `json:"actor"`
}

type worktreeResponse struct {
	Projection transactionreducer.Projection `json:"projection"`
	Workspace  gitstage.Workspace            `json:"workspace"`
}

type stageResponse struct {
	Projection transactionreducer.Projection `json:"projection"`
	Snapshot   gitstage.Snapshot             `json:"snapshot"`
}

type validationResponse struct {
	Projection transactionreducer.Projection       `json:"projection"`
	Decision   transactionreducer.SequenceDecision `json:"decision"`
}

type abortResponse struct {
	Projection       transactionreducer.Projection `json:"projection"`
	WorkspaceRemoved bool                          `json:"workspace_removed"`
	CleanupWarning   string                        `json:"cleanup_warning,omitempty"`
}

type startAgentExecutionRequest struct {
	ExpectedSequence    int64           `json:"expected_sequence"`
	Actor               model.Principal `json:"actor"`
	RunID               string          `json:"run_id"`
	Program             string          `json:"program"`
	CommandDigest       string          `json:"command_digest"`
	RuntimeClass        string          `json:"runtime_class"`
	RuntimeConfigDigest string          `json:"runtime_config_digest"`
	ImageDigest         string          `json:"image_digest,omitempty"`
}

type finishAgentExecutionRequest struct {
	ExpectedSequence int64                      `json:"expected_sequence"`
	Actor            model.Principal            `json:"actor"`
	ExecutionID      string                     `json:"execution_id"`
	Status           model.AgentExecutionStatus `json:"status"`
	ExitCode         *int                       `json:"exit_code,omitempty"`
	StdoutDigest     string                     `json:"stdout_digest"`
	StderrDigest     string                     `json:"stderr_digest"`
}

type runAgentExecutionRequest struct {
	ExpectedSequence int64           `json:"expected_sequence"`
	Actor            model.Principal `json:"actor"`
	Image            string          `json:"image"`
	Command          []string        `json:"command"`
	TimeoutSeconds   int64           `json:"timeout_seconds,omitempty"`
}

type runAgentExecutionResponse struct {
	Projection        transactionreducer.Projection `json:"projection"`
	Execution         model.AgentExecution          `json:"execution"`
	Process           verification.ProcessReceipt   `json:"process"`
	EvidenceDirectory string                        `json:"evidence_directory"`
}

type recordVerificationRequest struct {
	ExpectedSequence    int64                    `json:"expected_sequence"`
	Actor               model.Principal          `json:"actor"`
	Name                string                   `json:"name"`
	Status              model.VerificationStatus `json:"status"`
	CommandDigest       string                   `json:"command_digest"`
	RuntimeConfigDigest string                   `json:"runtime_config_digest"`
	ImageDigest         string                   `json:"image_digest"`
	Evidence            []model.ArtifactRef      `json:"evidence"`
	Summary             string                   `json:"summary"`
}

type recordVerificationResponse struct {
	Projection transactionreducer.Projection `json:"projection"`
	Result     model.VerificationResult      `json:"result"`
}

type runVerificationRequest struct {
	ExpectedSequence int64           `json:"expected_sequence"`
	Actor            model.Principal `json:"actor"`
	Name             string          `json:"name"`
	Image            string          `json:"image"`
	Command          []string        `json:"command"`
	TimeoutSeconds   int64           `json:"timeout_seconds"`
}

type runVerificationResponse struct {
	Projection        transactionreducer.Projection `json:"projection"`
	Result            model.VerificationResult      `json:"result"`
	Process           verification.ProcessReceipt   `json:"process"`
	EvidenceDirectory string                        `json:"evidence_directory"`
}

type prepareTransactionResponse struct {
	Projection transactionreducer.Projection       `json:"projection"`
	Decision   transactionreducer.SequenceDecision `json:"decision"`
}

type prepareTransactionRequest struct {
	ExpectedSequence int64           `json:"expected_sequence"`
	Actor            model.Principal `json:"actor"`
	GitRef           string          `json:"git_ref"`
}

type resolveApprovalRequest struct {
	ExpectedSequence int64                  `json:"expected_sequence"`
	Decision         model.ApprovalDecision `json:"decision"`
}

type releaseTransactionResponse struct {
	Projection transactionreducer.Projection `json:"projection"`
	Prepared   gitstage.PreparedCommit       `json:"prepared"`
	Publish    gitstage.PublishResult        `json:"publish"`
	Message    string                        `json:"message,omitempty"`
}

func (s *Server) createTransaction(w http.ResponseWriter, r *http.Request) {
	var event model.TransactionEvent
	if err := decodeBody(w, r, &event); err != nil {
		writeError(w, err)
		return
	}
	lock := s.runLock("transaction", event.TransactionID)
	lock.Lock()
	defer lock.Unlock()
	projection, err := s.store.CreateTransaction(r.Context(), event)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, projection)
}

func (s *Server) listTransactions(w http.ResponseWriter, r *http.Request) {
	transactions, err := s.store.ListTransactions(r.Context(), r.PathValue("namespace"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, transactions)
}

func (s *Server) getTransaction(w http.ResponseWriter, r *http.Request) {
	projection, err := s.store.GetTransaction(r.Context(), r.PathValue("namespace"), r.PathValue("transactionID"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projection)
}

func (s *Server) listTransactionEvents(w http.ResponseWriter, r *http.Request) {
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			writeError(w, schemaError("after must be a non-negative integer", err))
			return
		}
		after = value
	}
	events, err := s.store.TransactionEvents(
		r.Context(), r.PathValue("namespace"), r.PathValue("transactionID"), after,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) startTransaction(w http.ResponseWriter, r *http.Request) {
	s.mutateTransactionState(w, r, model.TransactionRunning, "")
}

func (s *Server) createTransactionWorktree(w http.ResponseWriter, r *http.Request) {
	if s.gitStage == nil {
		writeError(w, transactionRuntimeUnavailable())
		return
	}
	var request createWorktreeRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()
	projection, err := s.checkedTransaction(
		r, namespace, transactionID, request.ExpectedSequence,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	if projection.Transaction.State != model.TransactionRunning {
		writeError(w, transactionTransitionError("Git worktree creation requires a running transaction"))
		return
	}
	if len(projection.Transaction.StageBindings) != 0 {
		writeError(w, &model.KernelError{Code: model.ErrorConflict, Operation: "create_transaction_worktree", Resource: transactionID, Message: "transaction already has a stage boundary"})
		return
	}
	workspace, err := s.gitStage.Create(
		r.Context(), s.transactionRepository, s.transactionStaging,
		transactionID, request.Revision, s.now().UTC(),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	event, err := transactionreducer.NextEvent(
		projection, transactionreducer.EventStageBindingCreated,
		request.Actor, s.now().UTC(),
		transactionreducer.StageBindingCreatedPayload{Binding: workspace.Binding()},
	)
	if err != nil {
		_ = s.gitStage.Discard(r.Context(), workspace)
		writeError(w, err)
		return
	}
	next, err := s.store.AppendTransactionEvents(r.Context(), namespace, request.ExpectedSequence, []model.TransactionEvent{event})
	if err != nil {
		_ = s.gitStage.Discard(r.Context(), workspace)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, worktreeResponse{Projection: next, Workspace: workspace})
}

func (s *Server) startAgentExecution(w http.ResponseWriter, r *http.Request) {
	if !s.executionPolicy.AllowExternalExecution {
		writeError(w, &model.KernelError{
			Code: model.ErrorCapabilityDenied, Operation: "start_agent_execution",
			Message: "production policy requires the daemon to execute agents",
		})
		return
	}
	var request startAgentExecutionRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()
	projection, err := s.checkedTransaction(
		r, namespace, transactionID, request.ExpectedSequence,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	if projection.Transaction.State != model.TransactionRunning ||
		len(projection.Transaction.StageBindings) != 1 {
		writeError(w, transactionTransitionError("agent execution requires a running transaction with one stage boundary"))
		return
	}
	if request.RuntimeClass == "host" && !s.executionPolicy.AllowHost {
		writeError(w, &model.KernelError{
			Code: model.ErrorCapabilityDenied, Operation: "start_agent_execution",
			Resource: transactionID, Message: "host execution is disabled by daemon policy",
		})
		return
	}
	if request.RuntimeClass == "oci" &&
		!s.executionPolicy.agentImageAllowed(request.ImageDigest) {
		writeError(w, &model.KernelError{
			Code: model.ErrorCapabilityDenied, Operation: "start_agent_execution",
			Resource: transactionID, Message: "OCI image digest is not allowed by daemon policy",
		})
		return
	}
	if err := taskAuthorizesExecution(
		projection.Transaction.Task,
		request.RunID,
		request.RuntimeClass,
		request.ImageDigest,
		request.CommandDigest,
	); err != nil {
		writeError(w, err)
		return
	}
	now := s.now().UTC()
	taskDigest := ""
	if projection.Transaction.Task != nil {
		taskDigest = projection.Transaction.Task.Digest
	}
	execution := model.AgentExecution{
		Version:             model.AgentExecutionVersion,
		ID:                  transactionreducer.ExecutionID(transactionID, request.ExpectedSequence+1),
		TransactionID:       transactionID,
		RunID:               request.RunID,
		StageBindingID:      projection.Transaction.StageBindings[0].ID,
		Program:             request.Program,
		CommandDigest:       request.CommandDigest,
		RuntimeClass:        request.RuntimeClass,
		RuntimeConfigDigest: request.RuntimeConfigDigest,
		ImageDigest:         request.ImageDigest,
		TaskDigest:          taskDigest,
		Status:              model.AgentExecutionRunning,
		StartedAt:           now,
	}
	event, err := transactionreducer.NextEvent(
		projection, transactionreducer.EventAgentExecutionStarted,
		request.Actor, now,
		transactionreducer.AgentExecutionStartedPayload{Execution: execution},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	next, err := s.store.AppendTransactionEvents(
		r.Context(), namespace, request.ExpectedSequence, []model.TransactionEvent{event},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, next)
}

func (s *Server) finishAgentExecution(w http.ResponseWriter, r *http.Request) {
	if !s.executionPolicy.AllowExternalExecution {
		writeError(w, &model.KernelError{
			Code: model.ErrorCapabilityDenied, Operation: "finish_agent_execution",
			Message: "production policy requires the daemon to execute agents",
		})
		return
	}
	var request finishAgentExecutionRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()
	projection, err := s.checkedTransaction(
		r, namespace, transactionID, request.ExpectedSequence,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	event, err := transactionreducer.NextEvent(
		projection, transactionreducer.EventAgentExecutionFinished,
		request.Actor, s.now().UTC(),
		transactionreducer.AgentExecutionFinishedPayload{
			ExecutionID:  request.ExecutionID,
			Status:       request.Status,
			ExitCode:     request.ExitCode,
			StdoutDigest: request.StdoutDigest,
			StderrDigest: request.StderrDigest,
		},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	next, err := s.store.AppendTransactionEvents(
		r.Context(), namespace, request.ExpectedSequence, []model.TransactionEvent{event},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, next)
}

func (s *Server) runAgentExecution(w http.ResponseWriter, r *http.Request) {
	if s.gitStage == nil {
		writeError(w, transactionRuntimeUnavailable())
		return
	}
	var request runAgentExecutionRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if len(request.Command) == 0 ||
		request.Image == "" ||
		request.TimeoutSeconds < 0 ||
		s.executionPolicy.MaxAgentTimeoutSecs < 1 ||
		s.executionPolicy.EnginePath == "" {
		writeError(w, schemaError("daemon-run agent request or runtime policy is invalid", nil))
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()
	// compileLiveExecutionAuthority performs the Runtime authority lookup and
	// binds both transaction and run heads before any workload effect. Avoid a
	// second earlier authority read so the launch-claim race check retains one
	// coherent snapshot.
	projection, err := s.transactionAtSequence(
		r,
		namespace,
		transactionID,
		request.ExpectedSequence,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	if projection.Transaction.State != model.TransactionRunning ||
		len(projection.Transaction.StageBindings) != 1 {
		if err := s.requireCurrentRuntimeAuthority(
			r.Context(),
			namespace,
			transactionID,
			projection,
		); err != nil {
			writeError(w, err)
			return
		}
		writeError(w, transactionTransitionError("agent execution requires a running transaction with one stage boundary"))
		return
	}
	liveAuthority, err := s.compileLiveExecutionAuthority(
		r.Context(),
		namespace,
		transactionID,
		projection,
		request.Image,
		request.Command,
		request.TimeoutSeconds,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	executionPlan := liveAuthority.Plan
	authorityContext, cancelAuthority := context.WithDeadline(
		r.Context(),
		executionPlan.NotAfter,
	)
	defer cancelAuthority()
	workspace, err := s.transactionWorkspace(
		r.WithContext(authorityContext),
		projection,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	releaseWorkload, err := s.acquireWorkloadSlot(
		transactionID,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	defer releaseWorkload()
	if err := sandbox.PrepareWorkspaceOwnership(
		workspace.Path,
		s.executionPolicy.VerifierUID,
		s.executionPolicy.VerifierGID,
	); err != nil {
		writeError(w, &model.KernelError{
			Code: model.ErrorDriverUnavailable, Operation: "prepare_agent_workspace",
			Resource: transactionID,
			Message:  "assign staged workspace ownership for OCI execution",
			Cause:    err,
		})
		return
	}
	config := sandbox.OCIConfig{
		EnginePath:    s.executionPolicy.EnginePath,
		Image:         executionPlan.ImageReference,
		Workspace:     workspace.Path,
		TransactionID: transactionID,
		RunID:         executionPlan.RunID,
		Entrypoint:    executionPlan.Entrypoint,
		Command:       append([]string(nil), executionPlan.Command...),
		UID:           s.executionPolicy.VerifierUID,
		GID:           s.executionPolicy.VerifierGID,
		MemoryBytes:   s.executionPolicy.VerifierMemoryBytes,
		CPUMillis:     s.executionPolicy.VerifierCPUMillis,
		PIDsLimit:     s.executionPolicy.VerifierPIDsLimit,
		TmpfsBytes:    s.executionPolicy.VerifierTmpfsBytes,
		ContainerName: sandbox.ContainerName(
			transactionID,
			executionPlan.RunID,
		),
		Role:          "agent",
		WorkspaceMode: "transaction_rw",
	}
	taskDigest := executionPlan.TaskDigest
	if projection.Transaction.Task != nil {
		taskDirectory, cleanupTask, taskErr := materializeAgentTask(
			s.transactionStaging,
			*projection.Transaction.Task,
		)
		if taskErr != nil {
			writeError(w, taskErr)
			return
		}
		defer func() { _ = cleanupTask() }()
		config.TaskDirectory = taskDirectory
		config.TaskDigest = executionPlan.TaskDigest
	}
	var brokerSession *sandbox.ModelBrokerSession
	var brokerExecution *model.ModelBrokerExecution
	var brokerConfig *sandbox.ModelBrokerConfig
	if executionPlan.ModelBroker != nil {
		brokerPolicy := s.executionPolicy.ModelBroker
		if brokerPolicy == nil {
			writeError(w, &model.KernelError{
				Code:      model.ErrorInternal,
				Operation: "run_agent_execution",
				Resource:  transactionID,
				Message:   "compiled model authority has no daemon broker",
			})
			return
		}
		agentToken, tokenErr := randomBrokerToken()
		if tokenErr != nil {
			writeError(w, &model.KernelError{
				Code: model.ErrorInternal, Operation: "run_agent_execution",
				Resource: transactionID, Message: "generate transaction-scoped model broker token",
				Cause: tokenErr,
			})
			return
		}
		receiptDirectory := filepath.Join(
			s.transactionStaging,
			".gatemole-model-evidence",
			sandbox.ModelBrokerContainerName(
				transactionID,
				executionPlan.RunID,
			),
		)
		brokerConfig = &sandbox.ModelBrokerConfig{
			EnginePath:          s.executionPolicy.EnginePath,
			Image:               brokerPolicy.Image,
			PolicyData:          append([]byte(nil), brokerPolicy.PolicyData...),
			PolicyDigest:        brokerPolicy.PolicyDigest,
			ReceiptDirectory:    receiptDirectory,
			TransactionID:       transactionID,
			RunID:               executionPlan.RunID,
			AgentToken:          agentToken,
			ProviderBearerToken: brokerPolicy.ProviderBearerToken,
			UID:                 s.executionPolicy.VerifierUID, GID: s.executionPolicy.VerifierGID,
			MemoryBytes: 512 << 20, CPUMillis: 1000,
			PIDsLimit: 64, TmpfsBytes: 64 << 20,
		}
		brokerExecution = &model.ModelBrokerExecution{
			Provider:     executionPlan.ModelBroker.Provider,
			ImageDigest:  executionPlan.ModelBroker.ImageDigest,
			PolicyDigest: executionPlan.ModelBroker.PolicyDigest,
		}
		tokenDigest := sha256.Sum256([]byte(agentToken))
		config.NetworkName = sandbox.ModelBrokerNetworkName(
			transactionID,
			executionPlan.RunID,
		)
		config.ModelBroker = &sandbox.ModelBrokerBinding{
			URL:   "http://gatemole-model-broker:8080/v1",
			Token: agentToken, ImageDigest: executionPlan.ModelBroker.ImageDigest,
			PolicyDigest:      executionPlan.ModelBroker.PolicyDigest,
			TokenDigest:       "sha256:" + hex.EncodeToString(tokenDigest[:]),
			ReceiptLedgerHint: filepath.Join(receiptDirectory, "model-calls.jsonl"),
		}
	}
	brokerStopped := false
	defer func() {
		if brokerStopped || brokerSession == nil {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sandbox.RemoveModelBroker(cleanupContext, s.executionPolicy.EnginePath, *brokerSession)
	}()
	runtimeDigest, err := config.RuntimeConfigDigest()
	if err != nil {
		writeError(w, schemaError("validate daemon-owned OCI runtime", err))
		return
	}
	program := filepath.Base(request.Command[0])
	if !model.IsIdentifier(program) {
		program = "agent-command"
	}
	if err := s.requireLiveExecutionAuthority(
		authorityContext,
		transactionID,
		executionPlan.NotAfter,
	); err != nil {
		writeError(w, err)
		return
	}
	now := s.now().UTC()
	execution := model.AgentExecution{
		Version:             model.AgentExecutionVersion,
		ID:                  transactionreducer.ExecutionID(transactionID, request.ExpectedSequence+1),
		TransactionID:       transactionID,
		RunID:               executionPlan.RunID,
		StageBindingID:      executionPlan.StageBindingID,
		Program:             program,
		CommandDigest:       executionPlan.CommandDigest,
		RuntimeClass:        "oci",
		RuntimeConfigDigest: runtimeDigest,
		ImageDigest:         executionPlan.ImageDigest,
		TaskDigest:          taskDigest,
		ModelBroker:         brokerExecution,
		Status:              model.AgentExecutionRunning,
		StartedAt:           now,
	}
	started, err := transactionreducer.NextEvent(
		projection, transactionreducer.EventAgentExecutionStarted,
		request.Actor, now,
		transactionreducer.AgentExecutionStartedPayload{Execution: execution},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	projection, err = s.store.AppendTransactionEventsIfRunCurrent(
		authorityContext,
		namespace,
		request.ExpectedSequence,
		liveAuthority.RunSequence,
		liveAuthority.RunLastEventDigest,
		[]model.TransactionEvent{started},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	var outcome verification.Outcome
	var runErr error
	runOperation := "run_agent_execution"
	runMessage := "execute daemon-owned OCI agent"
	if brokerConfig != nil {
		startedBroker, brokerErr := sandbox.StartModelBroker(
			authorityContext,
			*brokerConfig,
		)
		if brokerErr != nil {
			runErr = brokerErr
			if authorityErr := s.requireLiveExecutionAuthority(
				authorityContext,
				transactionID,
				executionPlan.NotAfter,
			); authorityErr != nil {
				runErr = authorityErr
			}
			runOperation = "start_model_broker"
			runMessage = "start policy-controlled model egress"
		} else {
			brokerSession = &startedBroker
			if startedBroker.ImageDigest != executionPlan.ModelBroker.ImageDigest ||
				startedBroker.PolicyDigest != executionPlan.ModelBroker.PolicyDigest {
				runErr = errors.New("started model broker does not match execution authority")
				runOperation = "start_model_broker"
				runMessage = "start policy-controlled model egress"
			}
		}
	}
	if runErr == nil {
		runErr = s.requireLiveExecutionAuthority(
			authorityContext,
			transactionID,
			executionPlan.NotAfter,
		)
	}
	if runErr == nil {
		outcome, runErr = (verification.Runner{
			EvidenceRoot: filepath.Join(s.transactionStaging, ".gatemole-agent-evidence"),
		}).Run(authorityContext, config, execution.ID)
	}
	if brokerSession != nil {
		cleanupContext, stopBroker := context.WithTimeout(context.Background(), 10*time.Second)
		brokerErr := sandbox.RemoveModelBroker(
			cleanupContext, s.executionPolicy.EnginePath, *brokerSession,
		)
		stopBroker()
		if brokerErr != nil {
			writeError(w, &model.KernelError{
				Code: model.ErrorDriverUnavailable, Operation: "stop_model_broker",
				Resource: transactionID,
				Message:  "model broker cleanup failed; execution remains active until daemon recovery",
				Cause:    brokerErr,
			})
			return
		}
		brokerStopped = true
		summary, summaryErr := kernelmodelbroker.FinalizeLedger(
			brokerSession.ReceiptPath,
			transactionID,
			executionPlan.RunID,
			executionPlan.ModelBroker.Provider,
		)
		if summaryErr != nil {
			writeError(w, &model.KernelError{
				Code: model.ErrorEventChain, Operation: "finalize_model_receipts",
				Resource: transactionID,
				Message:  "model call receipt ledger failed verification; execution remains active",
				Cause:    summaryErr,
			})
			return
		}
		brokerExecution = &model.ModelBrokerExecution{
			Provider:            s.executionPolicy.ModelBroker.Policy.Provider,
			ImageDigest:         brokerSession.ImageDigest,
			PolicyDigest:        brokerSession.PolicyDigest,
			ReceiptLedgerDigest: summary.Digest,
			Calls:               summary.Calls,
			CompletedCalls:      summary.Completed,
			FailedCalls:         summary.Failed,
			UnknownCalls:        summary.Unknown,
			InputTokens:         summary.InputTokens,
			OutputTokens:        summary.OutputTokens,
		}
	}
	if verification.IsCleanupError(runErr) {
		writeError(w, &model.KernelError{
			Code: model.ErrorDriverUnavailable, Operation: "run_agent_execution",
			Resource: transactionID,
			Message:  "agent container cleanup failed; execution remains active until daemon recovery",
			Cause:    runErr,
		})
		return
	}
	if runErr != nil && outcome.Receipt.Status == "" {
		outcome.Receipt = verification.ProcessReceipt{
			Version:             "gatemole.verification_process_receipt.v0",
			Name:                execution.ID,
			Status:              model.AgentExecutionStartFailed,
			CommandDigest:       executionPlan.CommandDigest,
			RuntimeConfigDigest: runtimeDigest,
			ImageDigest:         executionPlan.ImageDigest,
			StdoutDigest:        emptySHA256Digest,
			StderrDigest:        emptySHA256Digest,
			CompletedAt:         s.now().UTC(),
		}
	}
	finished, finishErr := transactionreducer.NextEvent(
		projection, transactionreducer.EventAgentExecutionFinished,
		model.Principal{ID: "service:gatemoled-runtime", Kind: model.PrincipalService},
		s.now().UTC(),
		transactionreducer.AgentExecutionFinishedPayload{
			ExecutionID:  execution.ID,
			Status:       outcome.Receipt.Status,
			ExitCode:     outcome.Receipt.ExitCode,
			StdoutDigest: outcome.Receipt.StdoutDigest,
			StderrDigest: outcome.Receipt.StderrDigest,
			ModelBroker:  brokerExecution,
		},
	)
	if finishErr != nil {
		writeError(w, finishErr)
		return
	}
	finishContext, stopFinish := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopFinish()
	projection, finishErr = s.store.AppendTransactionEvents(
		finishContext, namespace, projection.Transaction.EventSequence,
		[]model.TransactionEvent{finished},
	)
	if finishErr != nil {
		writeError(w, finishErr)
		return
	}
	execution = projection.Executions[len(projection.Executions)-1]
	if runErr != nil {
		var kernelErr *model.KernelError
		if errors.As(runErr, &kernelErr) {
			writeError(w, runErr)
			return
		}
		writeError(w, &model.KernelError{
			Code: model.ErrorDriverUnavailable, Operation: runOperation,
			Resource: transactionID, Message: runMessage, Cause: runErr,
		})
		return
	}
	writeJSON(w, http.StatusOK, runAgentExecutionResponse{
		Projection: projection, Execution: execution,
		Process: outcome.Receipt, EvidenceDirectory: outcome.EvidenceDirectory,
	})
}

func taskAuthorizesExecution(
	task *model.AgentTask,
	runID, runtimeClass, imageDigest, commandDigest string,
) error {
	if task == nil {
		return nil
	}
	profile := task.AgentProfile
	if task.RunID == runID &&
		profile.RuntimeClass == runtimeClass &&
		profile.ImageDigest == imageDigest &&
		profile.CommandDigest == commandDigest {
		return nil
	}
	return &model.KernelError{
		Code:      model.ErrorCapabilityDenied,
		Operation: "run_agent_execution",
		Resource:  task.TransactionID,
		Message:   "persisted task does not authorize the requested run or agent profile",
	}
}

func materializeAgentTask(
	stagingRoot string,
	task model.AgentTask,
) (string, func() error, error) {
	if err := task.Validate(); err != nil {
		return "", nil, err
	}
	if !filepath.IsAbs(stagingRoot) || strings.ContainsRune(stagingRoot, 0) {
		return "", nil, &model.KernelError{
			Code: model.ErrorSchemaInvalid, Operation: "materialize_agent_task",
			Resource: task.ID, Message: "transaction staging root must be absolute",
		}
	}
	parent := filepath.Join(stagingRoot, ".gatemole-agent-tasks")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", nil, &model.KernelError{
			Code: model.ErrorDriverUnavailable, Operation: "materialize_agent_task",
			Resource: task.ID, Message: "create private task materialization root", Cause: err,
		}
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() ||
		parentInfo.Mode()&os.ModeSymlink != 0 ||
		parentInfo.Mode().Perm()&0o077 != 0 {
		return "", nil, &model.KernelError{
			Code: model.ErrorDriverUnavailable, Operation: "materialize_agent_task",
			Resource: task.ID, Message: "private task materialization root is unsafe", Cause: err,
		}
	}
	directory, err := os.MkdirTemp(parent, "task-")
	if err != nil {
		return "", nil, &model.KernelError{
			Code: model.ErrorDriverUnavailable, Operation: "materialize_agent_task",
			Resource: task.ID, Message: "create private task materialization", Cause: err,
		}
	}
	cleanup := func() error {
		if err := os.Chmod(directory, 0o700); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.RemoveAll(directory)
	}
	data, err := json.Marshal(task)
	if err != nil {
		_ = cleanup()
		return "", nil, &model.KernelError{
			Code: model.ErrorInternal, Operation: "materialize_agent_task",
			Resource: task.ID, Message: "encode persisted task", Cause: err,
		}
	}
	if int64(len(data)) > sandbox.MaximumTaskEnvelopeBytes {
		_ = cleanup()
		return "", nil, &model.KernelError{
			Code: model.ErrorBudgetExceeded, Operation: "materialize_agent_task",
			Resource: task.ID, Message: "task envelope exceeds the OCI mount limit",
		}
	}
	path := filepath.Join(directory, "task.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
	if err == nil {
		var written int
		written, err = file.Write(data)
		if err == nil && written != len(data) {
			err = fmt.Errorf("short task envelope write: %d of %d bytes", written, len(data))
		}
	}
	if closeErr := fileClose(file); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(path, 0o444)
	}
	if err == nil {
		err = os.Chmod(directory, 0o555)
	}
	if err != nil {
		_ = cleanup()
		return "", nil, &model.KernelError{
			Code: model.ErrorDriverUnavailable, Operation: "materialize_agent_task",
			Resource: task.ID, Message: "write private read-only task envelope", Cause: err,
		}
	}
	return directory, cleanup, nil
}

func fileClose(file *os.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func randomBrokerToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (s *Server) stageTransaction(w http.ResponseWriter, r *http.Request) {
	if s.gitStage == nil {
		writeError(w, transactionRuntimeUnavailable())
		return
	}
	var request transactionMutationRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()
	projection, err := s.checkedTransaction(r, namespace, transactionID, request.ExpectedSequence)
	if err != nil {
		writeError(w, err)
		return
	}
	if projection.Transaction.State != model.TransactionRunning || len(projection.Effects) != 0 {
		writeError(w, transactionTransitionError("only an un-staged running transaction can freeze effects"))
		return
	}
	workspace, err := s.transactionWorkspace(r, projection)
	if err != nil {
		writeError(w, err)
		return
	}
	snapshot, err := s.gitStage.Inspect(r.Context(), workspace, s.now().UTC())
	if err != nil {
		writeError(w, err)
		return
	}
	if len(snapshot.Effects) == 0 {
		writeError(w, transactionTransitionError("transaction worktree has no effects to stage"))
		return
	}
	temporary := projection
	events := make([]model.TransactionEvent, 0, len(snapshot.Effects)+1)
	for _, effect := range snapshot.Effects {
		if len(temporary.Transaction.AgentRunIDs) == 1 {
			effect.RunID = temporary.Transaction.AgentRunIDs[0]
		}
		event, buildErr := transactionreducer.NextEvent(
			temporary, transactionreducer.EventEffectAdded, request.Actor, s.now().UTC(),
			transactionreducer.EffectAddedPayload{Effect: effect},
		)
		if buildErr != nil {
			writeError(w, buildErr)
			return
		}
		temporary, buildErr = transactionreducer.Apply(&temporary, event)
		if buildErr != nil {
			writeError(w, buildErr)
			return
		}
		events = append(events, event)
	}
	// Recompute after attaching the participating run identity.
	effectSetDigest, err := transactionreducer.ComputeEffectSetDigest(temporary.Effects)
	if err != nil {
		writeError(w, err)
		return
	}
	stageEvent, err := transactionreducer.NextEvent(
		temporary, transactionreducer.EventTransactionStaged, request.Actor, s.now().UTC(),
		transactionreducer.TransactionStagedPayload{
			StagedStateDigest: snapshot.StagedStateDigest,
			EffectSetDigest:   effectSetDigest,
		},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	events = append(events, stageEvent)
	next, err := s.store.AppendTransactionEvents(r.Context(), namespace, request.ExpectedSequence, events)
	if err != nil {
		writeError(w, err)
		return
	}
	snapshot.Effects = next.Effects
	snapshot.EffectSetDigest = next.Transaction.EffectSetDigest
	writeJSON(w, http.StatusOK, stageResponse{Projection: next, Snapshot: snapshot})
}

func (s *Server) validateTransaction(w http.ResponseWriter, r *http.Request) {
	if s.gitStage == nil || s.sequencePolicy == nil {
		writeError(w, transactionRuntimeUnavailable())
		return
	}
	var request transactionMutationRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()
	projection, err := s.checkedTransaction(r, namespace, transactionID, request.ExpectedSequence)
	if err != nil {
		writeError(w, err)
		return
	}
	if projection.Transaction.State != model.TransactionStaged {
		writeError(w, transactionTransitionError("sequence validation requires a staged transaction"))
		return
	}
	workspace, err := s.transactionWorkspace(r, projection)
	if err != nil {
		writeError(w, err)
		return
	}
	current, err := s.gitStage.Inspect(r.Context(), workspace, s.now().UTC())
	if err != nil {
		writeError(w, err)
		return
	}
	if !matchesFrozenStage(current, projection) {
		writeError(w, &model.KernelError{Code: model.ErrorTransactionConflict, Operation: "validate_transaction", Resource: transactionID, Message: "worktree changed after effects were frozen"})
		return
	}
	decision := s.sequencePolicy.Evaluate(projection.Effects)
	temporary := projection
	events := make([]model.TransactionEvent, 0, 2)
	start, err := transactionreducer.NextEvent(
		temporary, transactionreducer.EventTransactionStateChanged, request.Actor, s.now().UTC(),
		transactionreducer.TransactionStateChangedPayload{From: model.TransactionStaged, To: model.TransactionValidating},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	temporary, err = transactionreducer.Apply(&temporary, start)
	if err != nil {
		writeError(w, err)
		return
	}
	events = append(events, start)
	var terminal model.TransactionState
	switch decision.Outcome {
	case transactionreducer.SequenceBlock:
		terminal = model.TransactionBlocked
	case transactionreducer.SequenceRevise:
		terminal = model.TransactionReviseRequired
	}
	if terminal != "" {
		reason := sequenceReason(decision)
		finish, buildErr := transactionreducer.NextEvent(
			temporary, transactionreducer.EventTransactionStateChanged, request.Actor, s.now().UTC(),
			transactionreducer.TransactionStateChangedPayload{From: model.TransactionValidating, To: terminal, Reason: reason},
		)
		if buildErr != nil {
			writeError(w, buildErr)
			return
		}
		events = append(events, finish)
	}
	next, err := s.store.AppendTransactionEvents(r.Context(), namespace, request.ExpectedSequence, events)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, validationResponse{Projection: next, Decision: decision})
}

func (s *Server) recordTransactionVerification(w http.ResponseWriter, r *http.Request) {
	if s.gitStage == nil {
		writeError(w, transactionRuntimeUnavailable())
		return
	}
	if !s.executionPolicy.AllowExternalVerification {
		writeError(w, &model.KernelError{
			Code: model.ErrorCapabilityDenied, Operation: "record_transaction_verification",
			Message: "production policy requires the daemon to execute verification",
		})
		return
	}
	var request recordVerificationRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()
	projection, err := s.checkedTransaction(r, namespace, transactionID, request.ExpectedSequence)
	if err != nil {
		writeError(w, err)
		return
	}
	if projection.Transaction.State != model.TransactionValidating {
		writeError(w, transactionTransitionError("verification requires a validating transaction"))
		return
	}
	if !model.IsSHA256Digest(request.CommandDigest) ||
		!model.IsSHA256Digest(request.RuntimeConfigDigest) ||
		!model.IsSHA256Digest(request.ImageDigest) {
		writeError(w, schemaError("verification command, runtime, and image digests must be sha256 digests", nil))
		return
	}
	if len(s.executionPolicy.AllowedImageDigests) > 0 {
		if _, allowed := s.executionPolicy.AllowedImageDigests[request.ImageDigest]; !allowed {
			writeError(w, &model.KernelError{
				Code: model.ErrorCapabilityDenied, Operation: "record_transaction_verification",
				Resource: transactionID, Message: "verifier image digest is not allowed by daemon policy",
			})
			return
		}
	}
	for _, existing := range projection.Verifications {
		if existing.Name == request.Name {
			writeError(w, &model.KernelError{
				Code: model.ErrorConflict, Operation: "record_transaction_verification",
				Resource: transactionID, Message: "verification name already exists in the transaction",
			})
			return
		}
	}
	workspace, err := s.transactionWorkspace(r, projection)
	if err != nil {
		writeError(w, err)
		return
	}
	current, err := s.gitStage.Inspect(r.Context(), workspace, s.now().UTC())
	if err != nil {
		writeError(w, err)
		return
	}
	if !matchesFrozenStage(current, projection) {
		writeError(w, &model.KernelError{
			Code: model.ErrorTransactionConflict, Operation: "record_transaction_verification",
			Resource: transactionID, Message: "worktree changed after effects were frozen",
		})
		return
	}
	verifierDigest, err := transactionreducer.ComputeVerifierDigest(
		request.CommandDigest, request.RuntimeConfigDigest, request.ImageDigest,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	inputsDigest, err := transactionreducer.ComputeVerificationInputsDigest(
		projection.Transaction.EffectSetDigest,
		projection.Transaction.StagedStateDigest,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	now := s.now().UTC()
	expiresAt := now.Add(time.Hour)
	result := model.VerificationResult{
		Version:           model.VerificationResultVersion,
		ID:                transactionreducer.VerificationID(transactionID, request.Name, request.ExpectedSequence+1),
		TransactionID:     transactionID,
		Name:              request.Name,
		Kind:              model.VerificationInvariant,
		Status:            request.Status,
		EffectSetDigest:   projection.Transaction.EffectSetDigest,
		StagedStateDigest: projection.Transaction.StagedStateDigest,
		Verifier: model.Principal{
			ID: "service:external-verifier", Kind: model.PrincipalService,
			Issuer: "gatemole-client:external", ClaimsDigest: verifierDigest,
		},
		Independence:   model.VerificationAgentSupplied,
		VerifierDigest: verifierDigest,
		InputsDigest:   inputsDigest,
		Evidence:       request.Evidence,
		Summary:        request.Summary,
		EvaluatedAt:    now,
		ExpiresAt:      &expiresAt,
	}
	if err := result.Validate(); err != nil {
		writeError(w, err)
		return
	}
	temporary := projection
	events := make([]model.TransactionEvent, 0, len(projection.Effects)+2)
	if result.Status == model.VerificationPassed {
		for _, effect := range temporary.Effects {
			if effect.Status != model.EffectStaged {
				continue
			}
			event, buildErr := transactionreducer.NextEvent(
				temporary, transactionreducer.EventEffectStateChanged,
				result.Verifier, now,
				transactionreducer.EffectStateChangedPayload{
					EffectID: effect.ID, From: model.EffectStaged, To: model.EffectValidated,
				},
			)
			if buildErr != nil {
				writeError(w, buildErr)
				return
			}
			temporary, buildErr = transactionreducer.Apply(&temporary, event)
			if buildErr != nil {
				writeError(w, buildErr)
				return
			}
			events = append(events, event)
		}
	}
	recorded, err := transactionreducer.NextEvent(
		temporary, transactionreducer.EventVerificationRecorded,
		result.Verifier, now,
		transactionreducer.VerificationRecordedPayload{Result: result},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	temporary, err = transactionreducer.Apply(&temporary, recorded)
	if err != nil {
		writeError(w, err)
		return
	}
	events = append(events, recorded)
	if result.Status != model.VerificationPassed {
		failed, buildErr := transactionreducer.NextEvent(
			temporary, transactionreducer.EventTransactionStateChanged,
			result.Verifier, now,
			transactionreducer.TransactionStateChangedPayload{
				From: model.TransactionValidating, To: model.TransactionValidationFailed,
				Reason: "verification " + request.Name + " did not pass",
			},
		)
		if buildErr != nil {
			writeError(w, buildErr)
			return
		}
		events = append(events, failed)
	}
	next, err := s.store.AppendTransactionEvents(
		r.Context(), namespace, request.ExpectedSequence, events,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, recordVerificationResponse{
		Projection: next,
		Result:     result,
	})
}

func (s *Server) runTransactionVerification(w http.ResponseWriter, r *http.Request) {
	if s.gitStage == nil {
		writeError(w, transactionRuntimeUnavailable())
		return
	}
	var request runVerificationRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if !model.IsIdentifier(request.Name) ||
		len(request.Command) == 0 ||
		request.Image == "" ||
		request.TimeoutSeconds < 1 ||
		s.executionPolicy.MaxVerificationTimeoutSecs < 1 ||
		request.TimeoutSeconds > s.executionPolicy.MaxVerificationTimeoutSecs ||
		s.executionPolicy.EnginePath == "" {
		writeError(w, schemaError("daemon-run verification request or runtime policy is invalid", nil))
		return
	}
	imageDigest, err := sandbox.ImageDigest(request.Image)
	if err != nil {
		writeError(w, schemaError("verification image must be digest-pinned", err))
		return
	}
	verifierProfileDigest := ""
	if profile, found := s.executionPolicy.VerifierProfiles.Lookup(
		request.Name,
	); found {
		if request.Image != profile.Image ||
			imageDigest != profile.ImageDigest ||
			!slices.Equal(request.Command, profile.Command) ||
			request.TimeoutSeconds != profile.TimeoutSeconds {
			writeError(w, &model.KernelError{
				Code: model.ErrorCapabilityDenied, Operation: "run_transaction_verification",
				Resource: request.Name,
				Message:  "verification request does not match the daemon-owned profile",
			})
			return
		}
		verifierProfileDigest = profile.Digest
	} else if s.executionPolicy.RequireVerifierProfiles {
		writeError(w, &model.KernelError{
			Code: model.ErrorCapabilityDenied, Operation: "run_transaction_verification",
			Resource: request.Name,
			Message:  "verification name is not present in the daemon-owned profile set",
		})
		return
	} else if len(s.executionPolicy.AllowedImageDigests) > 0 {
		if _, allowed := s.executionPolicy.AllowedImageDigests[imageDigest]; !allowed {
			writeError(w, &model.KernelError{
				Code: model.ErrorCapabilityDenied, Operation: "run_transaction_verification",
				Resource: imageDigest, Message: "verifier image digest is not allowed by daemon policy",
			})
			return
		}
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()
	projection, err := s.checkedTransaction(r, namespace, transactionID, request.ExpectedSequence)
	if err != nil {
		writeError(w, err)
		return
	}
	if projection.Transaction.State != model.TransactionValidating {
		writeError(w, transactionTransitionError("verification requires a validating transaction"))
		return
	}
	for _, existing := range projection.Verifications {
		if existing.Name == request.Name {
			writeError(w, &model.KernelError{
				Code: model.ErrorConflict, Operation: "run_transaction_verification",
				Resource: transactionID, Message: "verification name already exists in the transaction",
			})
			return
		}
	}
	workspace, err := s.transactionWorkspace(r, projection)
	if err != nil {
		writeError(w, err)
		return
	}
	frozenSnapshot, err := s.gitStage.Inspect(r.Context(), workspace, s.now().UTC())
	if err != nil {
		writeError(w, err)
		return
	}
	if !matchesFrozenStage(frozenSnapshot, projection) {
		writeError(w, &model.KernelError{
			Code:      model.ErrorTransactionConflict,
			Operation: "run_transaction_verification",
			Resource:  transactionID,
			Message:   "transaction worktree changed before verification",
		})
		return
	}
	releaseWorkload, err := s.acquireWorkloadSlot(
		transactionID,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	defer releaseWorkload()
	verifierWorkspace, cleanupVerifierWorkspace, err := s.gitStage.MaterializeSnapshot(
		r.Context(),
		frozenSnapshot,
		filepath.Join(s.transactionStaging, ".gatemole-verifier-trees"),
	)
	if err != nil {
		writeError(w, &model.KernelError{
			Code:      model.ErrorDriverUnavailable,
			Operation: "run_transaction_verification",
			Resource:  transactionID,
			Message:   "materialize immutable verifier input",
			Cause:     err,
		})
		return
	}
	cleanupPending := true
	defer func() {
		if cleanupPending {
			_ = cleanupVerifierWorkspace()
		}
	}()
	runID := "verifier:" + request.Name
	config := sandbox.OCIConfig{
		EnginePath:    s.executionPolicy.EnginePath,
		Image:         request.Image,
		Workspace:     verifierWorkspace,
		TransactionID: transactionID,
		RunID:         runID,
		Command:       append([]string(nil), request.Command...),
		UID:           s.executionPolicy.VerifierUID,
		GID:           s.executionPolicy.VerifierGID,
		MemoryBytes:   s.executionPolicy.VerifierMemoryBytes,
		CPUMillis:     s.executionPolicy.VerifierCPUMillis,
		PIDsLimit:     s.executionPolicy.VerifierPIDsLimit,
		TmpfsBytes:    s.executionPolicy.VerifierTmpfsBytes,
		ContainerName: sandbox.ContainerName(transactionID, runID),
		Role:          "verifier",
		WorkspaceMode: "staged_ro",
	}
	processContext, cancel := context.WithTimeout(
		r.Context(), time.Duration(request.TimeoutSeconds)*time.Second,
	)
	defer cancel()
	outcome, runErr := (verification.Runner{
		EvidenceRoot: filepath.Join(s.transactionStaging, ".gatemole-evidence"),
	}).Run(processContext, config, request.Name)
	cleanupErr := cleanupVerifierWorkspace()
	cleanupPending = false
	if runErr != nil {
		writeError(w, &model.KernelError{
			Code: model.ErrorDriverUnavailable, Operation: "run_transaction_verification",
			Resource: transactionID, Message: "execute OCI verifier",
			Cause: errors.Join(runErr, cleanupErr),
		})
		return
	}
	if cleanupErr != nil {
		writeError(w, &model.KernelError{
			Code: model.ErrorDriverUnavailable, Operation: "run_transaction_verification",
			Resource: transactionID, Message: "remove immutable verifier input",
			Cause: cleanupErr,
		})
		return
	}
	current, err := s.gitStage.Inspect(r.Context(), workspace, s.now().UTC())
	if err != nil {
		writeError(w, err)
		return
	}
	if !matchesFrozenStage(current, projection) {
		writeError(w, &model.KernelError{
			Code: model.ErrorTransactionConflict, Operation: "run_transaction_verification",
			Resource: transactionID, Message: "transaction worktree changed during verification",
		})
		return
	}
	verifierDigest, err := transactionreducer.ComputeVerifierDigest(
		outcome.Receipt.CommandDigest,
		outcome.Receipt.RuntimeConfigDigest,
		outcome.Receipt.ImageDigest,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	inputsDigest, err := transactionreducer.ComputeVerificationInputsDigest(
		projection.Transaction.EffectSetDigest,
		projection.Transaction.StagedStateDigest,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	status := model.VerificationFailed
	if outcome.Receipt.Status == model.AgentExecutionSucceeded {
		status = model.VerificationPassed
	} else if outcome.Receipt.Status == model.AgentExecutionInterrupted ||
		outcome.Receipt.Status == model.AgentExecutionStartFailed {
		status = model.VerificationIndeterminate
	}
	now := s.now().UTC()
	expiresAt := now.Add(time.Hour)
	verifierClaimsDigest := verifierDigest
	if verifierProfileDigest != "" {
		verifierClaimsDigest = verifierProfileDigest
	}
	result := model.VerificationResult{
		Version:           model.VerificationResultVersion,
		ID:                transactionreducer.VerificationID(transactionID, request.Name, request.ExpectedSequence+1),
		TransactionID:     transactionID,
		Name:              request.Name,
		Kind:              model.VerificationInvariant,
		Status:            status,
		EffectSetDigest:   projection.Transaction.EffectSetDigest,
		StagedStateDigest: projection.Transaction.StagedStateDigest,
		Verifier: model.Principal{
			ID: "service:gatemole-verifier", Kind: model.PrincipalService,
			Issuer: "gatemoled:oci", ClaimsDigest: verifierClaimsDigest,
		},
		Independence:   model.VerificationPlatformRun,
		VerifierDigest: verifierDigest,
		InputsDigest:   inputsDigest,
		Evidence:       outcome.Evidence,
		Summary: fmt.Sprintf(
			"daemon-run OCI verifier %s completed with process status %s",
			request.Name, outcome.Receipt.Status,
		),
		EvaluatedAt: now,
		ExpiresAt:   &expiresAt,
	}
	if err := result.Validate(); err != nil {
		writeError(w, err)
		return
	}
	next, err := s.appendPlatformVerification(
		r, namespace, projection, result,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, runVerificationResponse{
		Projection: next, Result: result,
		Process: outcome.Receipt, EvidenceDirectory: outcome.EvidenceDirectory,
	})
}

func (s *Server) appendPlatformVerification(
	r *http.Request,
	namespace string,
	projection transactionreducer.Projection,
	result model.VerificationResult,
) (transactionreducer.Projection, error) {
	temporary := projection
	events := make([]model.TransactionEvent, 0, len(projection.Effects)+2)
	if result.Status == model.VerificationPassed {
		for _, effect := range temporary.Effects {
			if effect.Status != model.EffectStaged {
				continue
			}
			event, err := transactionreducer.NextEvent(
				temporary, transactionreducer.EventEffectStateChanged,
				result.Verifier, result.EvaluatedAt,
				transactionreducer.EffectStateChangedPayload{
					EffectID: effect.ID, From: model.EffectStaged, To: model.EffectValidated,
				},
			)
			if err != nil {
				return transactionreducer.Projection{}, err
			}
			temporary, err = transactionreducer.Apply(&temporary, event)
			if err != nil {
				return transactionreducer.Projection{}, err
			}
			events = append(events, event)
		}
	}
	recorded, err := transactionreducer.NextEvent(
		temporary, transactionreducer.EventVerificationRecorded,
		result.Verifier, result.EvaluatedAt,
		transactionreducer.VerificationRecordedPayload{Result: result},
	)
	if err != nil {
		return transactionreducer.Projection{}, err
	}
	temporary, err = transactionreducer.Apply(&temporary, recorded)
	if err != nil {
		return transactionreducer.Projection{}, err
	}
	events = append(events, recorded)
	if result.Status != model.VerificationPassed {
		failed, err := transactionreducer.NextEvent(
			temporary, transactionreducer.EventTransactionStateChanged,
			result.Verifier, result.EvaluatedAt,
			transactionreducer.TransactionStateChangedPayload{
				From: model.TransactionValidating, To: model.TransactionValidationFailed,
				Reason: "verification " + result.Name + " did not pass",
			},
		)
		if err != nil {
			return transactionreducer.Projection{}, err
		}
		events = append(events, failed)
	}
	return s.store.AppendTransactionEvents(
		r.Context(), namespace, projection.Transaction.EventSequence, events,
	)
}

func (s *Server) prepareTransaction(w http.ResponseWriter, r *http.Request) {
	if s.gitStage == nil {
		writeError(w, transactionRuntimeUnavailable())
		return
	}
	var request prepareTransactionRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()

	projection, err := s.verifiedAuthorityTransaction(
		r, namespace, transactionID, request.ExpectedSequence,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	if projection.Transaction.State != model.TransactionValidating {
		writeError(w, transactionTransitionError("authority preparation requires a validating transaction"))
		return
	}
	workspace, err := s.transactionWorkspace(r, projection)
	if err != nil {
		writeError(w, err)
		return
	}
	current, err := s.gitStage.Inspect(r.Context(), workspace, s.now().UTC())
	if err != nil {
		writeError(w, err)
		return
	}
	if !matchesFrozenStage(current, projection) {
		writeError(w, &model.KernelError{
			Code: model.ErrorTransactionConflict, Operation: "prepare_transaction",
			Resource: transactionID, Message: "worktree changed after verification",
		})
		return
	}
	if err := s.gitStage.ValidateTargetRef(r.Context(), workspace.RepositoryRoot, request.GitRef); err != nil {
		writeError(w, schemaError("invalid Git release target", err))
		return
	}
	if !s.releasePolicy.AllowsGitRef(request.GitRef) {
		writeError(w, &model.KernelError{
			Code: model.ErrorCapabilityDenied, Operation: "prepare_transaction",
			Resource: request.GitRef, Message: "Git release target is not allowed by daemon policy",
		})
		return
	}
	if err := s.validateVerifierProfiles(projection); err != nil {
		writeError(w, err)
		return
	}

	decision := s.sequencePolicy.Evaluate(projection.Effects)
	policyDigest, err := s.currentAuthorityPolicyDigest()
	if err != nil {
		writeError(w, &model.KernelError{
			Code: model.ErrorInternal, Operation: "prepare_transaction",
			Resource: transactionID, Message: "sequence policy cannot be bound to immutable authority", Cause: err,
		})
		return
	}
	prepared, err := transactionreducer.PrepareAuthority(
		projection,
		decision,
		policyDigest,
		transactionreducer.ReleaseBinding{
			Connector: "git",
			Target: model.ResourceSelector{
				Kind: "git_ref", Pattern: request.GitRef,
			},
			ExpectedResourceVersion: workspace.BaseRevision,
		},
		s.now().UTC(),
	)
	if err != nil {
		writeError(w, err)
		return
	}

	temporary := projection
	events := make([]model.TransactionEvent, 0, 3)
	planEvent, err := transactionreducer.NextEvent(
		temporary, transactionreducer.EventCommitPlanFrozen, request.Actor, s.now().UTC(),
		transactionreducer.CommitPlanFrozenPayload{Plan: prepared.Plan},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	temporary, err = transactionreducer.Apply(&temporary, planEvent)
	if err != nil {
		writeError(w, err)
		return
	}
	events = append(events, planEvent)

	approvalEvent, err := transactionreducer.NextEvent(
		temporary, transactionreducer.EventApprovalPackageFrozen, request.Actor, s.now().UTC(),
		transactionreducer.ApprovalPackageFrozenPayload{Package: prepared.Approval},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	temporary, err = transactionreducer.Apply(&temporary, approvalEvent)
	if err != nil {
		writeError(w, err)
		return
	}
	events = append(events, approvalEvent)

	stateEvent, err := transactionreducer.NextEvent(
		temporary, transactionreducer.EventTransactionStateChanged, request.Actor, s.now().UTC(),
		transactionreducer.TransactionStateChangedPayload{
			From:                   model.TransactionValidating,
			To:                     prepared.TargetState,
			OutstandingApprovalIDs: prepared.OutstandingApprovalIDs,
		},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := transactionreducer.Apply(&temporary, stateEvent); err != nil {
		writeError(w, err)
		return
	}
	events = append(events, stateEvent)

	next, err := s.store.AppendTransactionEvents(
		r.Context(), namespace, request.ExpectedSequence, events,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prepareTransactionResponse{
		Projection: next,
		Decision:   decision,
	})
}

func (s *Server) resolveTransactionApproval(w http.ResponseWriter, r *http.Request) {
	var request resolveApprovalRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()

	projection, err := s.verifiedAuthorityTransaction(
		r, namespace, transactionID, request.ExpectedSequence,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	if projection.Transaction.State != model.TransactionPendingApproval ||
		projection.ApprovalPackage == nil {
		writeError(w, transactionTransitionError("approval resolution requires a pending immutable package"))
		return
	}
	events, err := s.store.TransactionEvents(
		r.Context(), namespace, transactionID, 0,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := validateApprovalSeparation(
		projection, events, request.Decision.Approver,
		request.Decision.Digest,
	); err != nil {
		writeError(w, err)
		return
	}
	if err := s.approvalTrust.Verify(
		request.Decision, *projection.ApprovalPackage, s.now().UTC(),
	); err != nil {
		writeError(w, err)
		return
	}
	event, err := transactionreducer.NextEvent(
		projection, transactionreducer.EventApprovalResolved,
		request.Decision.Approver, s.now().UTC(),
		transactionreducer.ApprovalResolvedPayload{Decision: request.Decision},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	next, err := s.store.AppendTransactionEvents(
		r.Context(), namespace, request.ExpectedSequence, []model.TransactionEvent{event},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, next)
}

func (s *Server) releaseTransaction(w http.ResponseWriter, r *http.Request) {
	if s.gitStage == nil {
		writeError(w, transactionRuntimeUnavailable())
		return
	}
	var request transactionMutationRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()

	projection, err := s.verifiedAuthorityTransaction(
		r, namespace, transactionID, request.ExpectedSequence,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	if projection.Transaction.State != model.TransactionReadyToCommit &&
		projection.Transaction.State != model.TransactionCommitting {
		writeError(w, transactionTransitionError("release requires ready-to-commit or recoverable committing state"))
		return
	}
	if projection.CommitPlan == nil ||
		projection.CommitPlan.Connector != "git" ||
		projection.CommitPlan.Target.Kind != "git_ref" {
		writeError(w, transactionTransitionError("release requires a frozen Git commit plan"))
		return
	}
	if !s.releasePolicy.AllowsGitRef(projection.CommitPlan.Target.Pattern) {
		writeError(w, &model.KernelError{
			Code: model.ErrorCapabilityDenied, Operation: "release_transaction",
			Resource: projection.CommitPlan.Target.Pattern,
			Message:  "frozen Git release target is no longer allowed by daemon policy",
		})
		return
	}
	workspace, err := s.transactionWorkspace(r, projection)
	if err != nil {
		writeError(w, err)
		return
	}
	current, inspectErr := s.gitStage.Inspect(r.Context(), workspace, s.now().UTC())
	if inspectErr != nil {
		writeError(w, inspectErr)
		return
	}
	if !matchesFrozenStage(current, projection) {
		writeError(w, &model.KernelError{
			Code: model.ErrorTransactionConflict, Operation: "release_transaction",
			Resource: transactionID, Message: "worktree changed after commit authority was granted",
		})
		return
	}
	if projection.Transaction.State == model.TransactionReadyToCommit {
		now := s.now().UTC()
		if err := s.validateReleaseAuthority(
			r.Context(), namespace, projection, request.Actor, now,
		); err != nil {
			writeError(w, err)
			return
		}
		for _, verification := range projection.Verifications {
			if verification.Status != model.VerificationPassed ||
				(verification.ExpiresAt != nil && !verification.ExpiresAt.After(now)) {
				writeError(w, &model.KernelError{
					Code: model.ErrorVerificationFailed, Operation: "release_transaction",
					Resource: verification.ID, Message: "verification is not passing and current at release time",
				})
				return
			}
		}
	}
	prepared, err := s.gitStage.PrepareCommit(r.Context(), workspace, *projection.CommitPlan)
	if err != nil {
		writeError(w, err)
		return
	}

	if projection.Transaction.State == model.TransactionReadyToCommit {
		events := make([]model.TransactionEvent, 0, len(projection.Effects)*2+1)
		temporary := projection
		for _, effect := range temporary.Effects {
			if effect.RecoveryClass == model.RecoveryReadOnly {
				continue
			}
			event, buildErr := transactionreducer.NextEvent(
				temporary, transactionreducer.EventEffectStateChanged,
				request.Actor, s.now().UTC(),
				transactionreducer.EffectStateChangedPayload{
					EffectID: effect.ID, From: model.EffectValidated, To: model.EffectReleaseReady,
				},
			)
			if buildErr != nil {
				writeError(w, buildErr)
				return
			}
			temporary, buildErr = transactionreducer.Apply(&temporary, event)
			if buildErr != nil {
				writeError(w, buildErr)
				return
			}
			events = append(events, event)
		}
		stateEvent, buildErr := transactionreducer.NextEvent(
			temporary, transactionreducer.EventTransactionStateChanged,
			request.Actor, s.now().UTC(),
			transactionreducer.TransactionStateChangedPayload{
				From: model.TransactionReadyToCommit, To: model.TransactionCommitting,
			},
		)
		if buildErr != nil {
			writeError(w, buildErr)
			return
		}
		temporary, buildErr = transactionreducer.Apply(&temporary, stateEvent)
		if buildErr != nil {
			writeError(w, buildErr)
			return
		}
		events = append(events, stateEvent)
		for _, effect := range temporary.Effects {
			if effect.RecoveryClass == model.RecoveryReadOnly {
				continue
			}
			event, eventErr := transactionreducer.NextEvent(
				temporary, transactionreducer.EventEffectStateChanged,
				request.Actor, s.now().UTC(),
				transactionreducer.EffectStateChangedPayload{
					EffectID: effect.ID, From: model.EffectReleaseReady, To: model.EffectCommitting,
				},
			)
			if eventErr != nil {
				writeError(w, eventErr)
				return
			}
			temporary, eventErr = transactionreducer.Apply(&temporary, event)
			if eventErr != nil {
				writeError(w, eventErr)
				return
			}
			events = append(events, event)
		}
		projection, err = s.store.AppendTransactionEvents(
			r.Context(), namespace, request.ExpectedSequence, events,
		)
		if err != nil {
			writeError(w, err)
			return
		}
	}

	publish, publishErr := s.gitStage.PublishCommit(r.Context(), workspace, prepared)
	switch publish.Status {
	case gitstage.PublishApplied, gitstage.PublishReconciled:
		projection, err = s.finishGitRelease(
			r, namespace, projection, request.Actor, prepared,
		)
	case gitstage.PublishConflict:
		projection, err = s.failGitRelease(
			r, namespace, projection, request.Actor,
			model.EffectFailed, model.TransactionReleaseFailed,
			"Git release target changed before the atomic update",
		)
	case gitstage.PublishUnknown:
		projection, err = s.failGitRelease(
			r, namespace, projection, request.Actor,
			model.EffectUnknown, model.TransactionManualRecoveryRequired,
			"Git release outcome could not be reconciled; inspect the target ref",
		)
	default:
		err = fmt.Errorf("unknown Git publish status %q", publish.Status)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	message := ""
	if publishErr != nil {
		message = publishErr.Error()
	}
	writeJSON(w, http.StatusOK, releaseTransactionResponse{
		Projection: projection,
		Prepared:   prepared,
		Publish:    publish,
		Message:    message,
	})
}

func (s *Server) finishGitRelease(
	r *http.Request,
	namespace string,
	projection transactionreducer.Projection,
	actor model.Principal,
	prepared gitstage.PreparedCommit,
) (transactionreducer.Projection, error) {
	temporary := projection
	events := make([]model.TransactionEvent, 0, len(projection.Effects)+1)
	now := s.now().UTC()
	for _, effect := range temporary.Effects {
		if effect.RecoveryClass == model.RecoveryReadOnly {
			continue
		}
		receipt := &model.EffectReceipt{
			Driver:          "git",
			OperationID:     "git-ref:" + prepared.CommitRevision[:16],
			ResultDigest:    prepared.MessageDigest,
			ResourceVersion: prepared.CommitRevision,
			CommittedAt:     now,
		}
		event, err := transactionreducer.NextEvent(
			temporary, transactionreducer.EventEffectStateChanged,
			actor, now,
			transactionreducer.EffectStateChangedPayload{
				EffectID: effect.ID, From: model.EffectCommitting,
				To: model.EffectCommitted, Receipt: receipt,
			},
		)
		if err != nil {
			return transactionreducer.Projection{}, err
		}
		temporary, err = transactionreducer.Apply(&temporary, event)
		if err != nil {
			return transactionreducer.Projection{}, err
		}
		events = append(events, event)
	}
	stateEvent, err := transactionreducer.NextEvent(
		temporary, transactionreducer.EventTransactionStateChanged,
		actor, now,
		transactionreducer.TransactionStateChangedPayload{
			From: model.TransactionCommitting, To: model.TransactionCommitted,
		},
	)
	if err != nil {
		return transactionreducer.Projection{}, err
	}
	events = append(events, stateEvent)
	return s.store.AppendTransactionEvents(
		r.Context(), namespace, projection.Transaction.EventSequence, events,
	)
}

func (s *Server) failGitRelease(
	r *http.Request,
	namespace string,
	projection transactionreducer.Projection,
	actor model.Principal,
	effectStatus model.EffectStatus,
	transactionState model.TransactionState,
	reason string,
) (transactionreducer.Projection, error) {
	temporary := projection
	events := make([]model.TransactionEvent, 0, len(projection.Effects)+1)
	for _, effect := range temporary.Effects {
		if effect.RecoveryClass == model.RecoveryReadOnly {
			continue
		}
		event, err := transactionreducer.NextEvent(
			temporary, transactionreducer.EventEffectStateChanged,
			actor, s.now().UTC(),
			transactionreducer.EffectStateChangedPayload{
				EffectID: effect.ID, From: model.EffectCommitting, To: effectStatus,
			},
		)
		if err != nil {
			return transactionreducer.Projection{}, err
		}
		temporary, err = transactionreducer.Apply(&temporary, event)
		if err != nil {
			return transactionreducer.Projection{}, err
		}
		events = append(events, event)
	}
	stateEvent, err := transactionreducer.NextEvent(
		temporary, transactionreducer.EventTransactionStateChanged,
		actor, s.now().UTC(),
		transactionreducer.TransactionStateChangedPayload{
			From: model.TransactionCommitting, To: transactionState, Reason: reason,
		},
	)
	if err != nil {
		return transactionreducer.Projection{}, err
	}
	events = append(events, stateEvent)
	return s.store.AppendTransactionEvents(
		r.Context(), namespace, projection.Transaction.EventSequence, events,
	)
}

func (s *Server) abortTransaction(w http.ResponseWriter, r *http.Request) {
	var request transactionMutationRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()
	projection, err := s.checkedTransaction(r, namespace, transactionID, request.ExpectedSequence)
	if err != nil {
		writeError(w, err)
		return
	}
	event, err := transactionreducer.NextEvent(
		projection, transactionreducer.EventTransactionStateChanged, request.Actor, s.now().UTC(),
		transactionreducer.TransactionStateChangedPayload{
			From: projection.Transaction.State, To: model.TransactionAborted, Reason: "aborted by operator",
		},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	next, err := s.store.AppendTransactionEvents(r.Context(), namespace, request.ExpectedSequence, []model.TransactionEvent{event})
	if err != nil {
		writeError(w, err)
		return
	}
	response := abortResponse{Projection: next}
	if s.gitStage != nil && len(next.Transaction.StageBindings) == 1 {
		workspace, workspaceErr := s.gitStage.FromBinding(r.Context(), transactionID, next.Transaction.StageBindings[0])
		if workspaceErr == nil {
			workspaceErr = s.gitStage.Discard(r.Context(), workspace)
		}
		if workspaceErr != nil {
			response.CleanupWarning = workspaceErr.Error()
		} else {
			response.WorkspaceRemoved = true
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) mutateTransactionState(w http.ResponseWriter, r *http.Request, target model.TransactionState, reason string) {
	var request transactionMutationRequest
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	namespace := r.PathValue("namespace")
	transactionID := r.PathValue("transactionID")
	lock := s.runLock(namespace, "transaction:"+transactionID)
	lock.Lock()
	defer lock.Unlock()
	projection, err := s.checkedTransaction(r, namespace, transactionID, request.ExpectedSequence)
	if err != nil {
		writeError(w, err)
		return
	}
	event, err := transactionreducer.NextEvent(
		projection, transactionreducer.EventTransactionStateChanged, request.Actor, s.now().UTC(),
		transactionreducer.TransactionStateChangedPayload{From: projection.Transaction.State, To: target, Reason: reason},
	)
	if err != nil {
		writeError(w, err)
		return
	}
	next, err := s.store.AppendTransactionEvents(r.Context(), namespace, request.ExpectedSequence, []model.TransactionEvent{event})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, next)
}

// checkedTransaction is the common gate for transaction mutation routes. The
// daemon-run execution path compiles the same authority and launch claim in one
// snapshot instead. A configured Runtime may inspect legacy history, but only a
// current atomic v1 task admission can advance state or reach an external effect.
func (s *Server) checkedTransaction(r *http.Request, namespace, transactionID string, expected int64) (transactionreducer.Projection, error) {
	projection, err := s.transactionAtSequence(
		r,
		namespace,
		transactionID,
		expected,
	)
	if err != nil {
		return transactionreducer.Projection{}, err
	}
	if err := s.requireCurrentRuntimeAuthority(
		r.Context(),
		namespace,
		transactionID,
		projection,
	); err != nil {
		return transactionreducer.Projection{}, err
	}
	return projection, nil
}

func (s *Server) transactionAtSequence(
	r *http.Request,
	namespace string,
	transactionID string,
	expected int64,
) (transactionreducer.Projection, error) {
	projection, err := s.store.GetTransaction(r.Context(), namespace, transactionID)
	if err != nil {
		return transactionreducer.Projection{}, err
	}
	if projection.Transaction.EventSequence != expected {
		return transactionreducer.Projection{}, &model.KernelError{
			Code: model.ErrorConflict, Operation: "mutate_transaction", Resource: transactionID,
			Message: fmt.Sprintf("expected transaction sequence %d, current sequence is %d", expected, projection.Transaction.EventSequence),
		}
	}
	return projection, nil
}

func (s *Server) verifiedAuthorityTransaction(
	r *http.Request,
	namespace string,
	transactionID string,
	expected int64,
) (transactionreducer.Projection, error) {
	projection, err := s.checkedTransaction(
		r,
		namespace,
		transactionID,
		expected,
	)
	if err != nil {
		return transactionreducer.Projection{}, err
	}
	if !runtimeidentity.IsRuntimeID(s.runtimeID) {
		if err := s.store.VerifyTransaction(
			r.Context(), namespace, transactionID,
		); err != nil {
			return transactionreducer.Projection{}, err
		}
	}
	return projection, nil
}

func (s *Server) acquireWorkloadSlot(
	resource string,
) (func(), error) {
	if s.workloadSlots == nil {
		return func() {}, nil
	}
	select {
	case s.workloadSlots <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-s.workloadSlots })
		}, nil
	default:
		return nil, &model.KernelError{
			Code:      model.ErrorBudgetExceeded,
			Operation: "admit_oci_workload",
			Resource:  resource,
			Message:   "daemon OCI workload concurrency limit is exhausted",
		}
	}
}

func (s *Server) transactionWorkspace(r *http.Request, projection transactionreducer.Projection) (gitstage.Workspace, error) {
	if len(projection.Transaction.StageBindings) != 1 {
		return gitstage.Workspace{}, transactionTransitionError("transaction requires exactly one Git stage boundary")
	}
	return s.gitStage.FromBinding(r.Context(), projection.Transaction.ID, projection.Transaction.StageBindings[0])
}

func sequenceReason(decision transactionreducer.SequenceDecision) string {
	parts := make([]string, 0, len(decision.Findings))
	for _, finding := range decision.Findings {
		parts = append(parts, finding.RuleID+": "+finding.Summary)
	}
	if len(parts) == 0 {
		return "sequence policy outcome: " + string(decision.Outcome)
	}
	return strings.Join(parts, "; ")
}

func matchesFrozenStage(
	snapshot gitstage.Snapshot,
	projection transactionreducer.Projection,
) bool {
	if snapshot.StagedStateDigest !=
		projection.Transaction.StagedStateDigest {
		return false
	}
	rawDigest, err := transactionreducer.ComputeEffectSetDigest(
		snapshot.Effects,
	)
	if err != nil || rawDigest != snapshot.EffectSetDigest {
		return false
	}
	effects := append([]model.Effect(nil), snapshot.Effects...)
	if len(projection.Transaction.AgentRunIDs) == 1 {
		for index := range effects {
			effects[index].RunID =
				projection.Transaction.AgentRunIDs[0]
		}
	}
	boundDigest, err := transactionreducer.ComputeEffectSetDigest(effects)
	return err == nil &&
		boundDigest == projection.Transaction.EffectSetDigest
}

func transactionRuntimeUnavailable() *model.KernelError {
	return &model.KernelError{Code: model.ErrorDriverUnavailable, Operation: "transaction_runtime", Message: "transaction Git staging runtime is not configured"}
}

func transactionTransitionError(message string) *model.KernelError {
	return &model.KernelError{Code: model.ErrorTransitionInvalid, Operation: "transaction", Message: message}
}
