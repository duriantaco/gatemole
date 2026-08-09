package transaction

import (
	"errors"
	"fmt"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

const (
	EventTransactionCreated      = "transaction.created"
	EventTransactionStateChanged = "transaction.state_changed"
	EventEffectAdded             = "effect.added"
	EventStageBindingCreated     = "stage_binding.created"
	EventEffectStateChanged      = "effect.state_changed"
	EventTransactionStaged       = "transaction.staged"
	EventVerificationRecorded    = "verification.recorded"
	EventVerificationSuperseded  = "verification.superseded"
	EventCommitPlanFrozen        = "commit_plan.frozen"
	EventApprovalPackageFrozen   = "approval_package.frozen"
	EventAuthorityRenewed        = "authority.renewed"
	EventApprovalResolved        = "approval.resolved"
	EventAgentExecutionStarted   = "agent_execution.started"
	EventAgentExecutionFinished  = "agent_execution.finished"
)

type Projection struct {
	Transaction             model.AgentTransaction     `json:"transaction"`
	Executions              []model.AgentExecution     `json:"executions"`
	Effects                 []model.Effect             `json:"effects"`
	Verifications           []model.VerificationResult `json:"verifications"`
	SupersededVerifications []model.VerificationResult `json:"superseded_verifications,omitempty"`
	CommitPlan              *model.CommitPlan          `json:"commit_plan,omitempty"`
	ApprovalPackage         *model.ApprovalPackage     `json:"approval_package,omitempty"`
	ApprovalDigests         []string                   `json:"approval_digests"`
	SupersededAuthorities   []AuthoritySnapshot        `json:"superseded_authorities,omitempty"`
	SupersededAttempts      []AttemptSnapshot          `json:"superseded_attempts,omitempty"`
	LastEventDigest         string                     `json:"last_event_digest"`
}

// AttemptSnapshot keeps superseded mutable work queryable without making it
// eligible for validation or release. The event stream remains authoritative.
type AttemptSnapshot struct {
	Attempt                 int64                      `json:"attempt"`
	State                   model.TransactionState     `json:"state"`
	Reason                  string                     `json:"reason"`
	SupersededAt            time.Time                  `json:"superseded_at"`
	ExecutionIDs            []string                   `json:"execution_ids"`
	EffectIDs               []string                   `json:"effect_ids"`
	Effects                 []model.Effect             `json:"effects"`
	StagedStateDigest       string                     `json:"staged_state_digest,omitempty"`
	EffectSetDigest         string                     `json:"effect_set_digest,omitempty"`
	VerificationResultIDs   []string                   `json:"verification_result_ids"`
	Verifications           []model.VerificationResult `json:"verifications"`
	SupersededVerifications []model.VerificationResult `json:"superseded_verifications,omitempty"`
	CommitPlan              *model.CommitPlan          `json:"commit_plan,omitempty"`
	CommitPlanDigest        string                     `json:"commit_plan_digest,omitempty"`
	ApprovalPackage         *model.ApprovalPackage     `json:"approval_package,omitempty"`
	ApprovalPackageDigest   string                     `json:"approval_package_digest,omitempty"`
	OutstandingApprovalIDs  []string                   `json:"outstanding_approval_ids"`
	ApprovalDigests         []string                   `json:"approval_digests"`
	SupersededAuthorities   []AuthoritySnapshot        `json:"superseded_authorities,omitempty"`
}

// AuthoritySnapshot records an authority package that was explicitly revoked
// and renewed while the staged effect set stayed unchanged.
type AuthoritySnapshot struct {
	Attempt                int64                  `json:"attempt"`
	State                  model.TransactionState `json:"state"`
	Reason                 string                 `json:"reason"`
	SupersededAt           time.Time              `json:"superseded_at"`
	OutstandingApprovalIDs []string               `json:"outstanding_approval_ids"`
	CommitPlan             *model.CommitPlan      `json:"commit_plan,omitempty"`
	ApprovalPackage        *model.ApprovalPackage `json:"approval_package,omitempty"`
	ApprovalDigests        []string               `json:"approval_digests"`
}

type TransactionCreatedPayload struct {
	Transaction model.AgentTransaction `json:"transaction"`
}

type TransactionStateChangedPayload struct {
	From                   model.TransactionState `json:"from"`
	To                     model.TransactionState `json:"to"`
	Reason                 string                 `json:"reason,omitempty"`
	OutstandingApprovalIDs []string               `json:"outstanding_approval_ids,omitempty"`
}

type EffectAddedPayload struct {
	Effect model.Effect `json:"effect"`
}

type StageBindingCreatedPayload struct {
	Binding model.StageBinding `json:"binding"`
}

type EffectStateChangedPayload struct {
	EffectID            string               `json:"effect_id"`
	From                model.EffectStatus   `json:"from"`
	To                  model.EffectStatus   `json:"to"`
	Receipt             *model.EffectReceipt `json:"receipt,omitempty"`
	CompensationReceipt *model.EffectReceipt `json:"compensation_receipt,omitempty"`
}

type TransactionStagedPayload struct {
	StagedStateDigest string `json:"staged_state_digest"`
	EffectSetDigest   string `json:"effect_set_digest"`
}

type VerificationRecordedPayload struct {
	Result model.VerificationResult `json:"result"`
}

type VerificationSupersededPayload struct {
	VerificationID string `json:"verification_id"`
	Reason         string `json:"reason"`
}

type CommitPlanFrozenPayload struct {
	Plan model.CommitPlan `json:"plan"`
}

type ApprovalPackageFrozenPayload struct {
	Package model.ApprovalPackage `json:"package"`
}

type AuthorityRenewedPayload struct {
	Reason string `json:"reason"`
}

type ApprovalResolvedPayload struct {
	Decision model.ApprovalDecision `json:"decision"`
}

type AgentExecutionStartedPayload struct {
	Execution model.AgentExecution `json:"execution"`
}

type AgentExecutionFinishedPayload struct {
	ExecutionID  string                      `json:"execution_id"`
	Status       model.AgentExecutionStatus  `json:"status"`
	ExitCode     *int                        `json:"exit_code,omitempty"`
	StdoutDigest string                      `json:"stdout_digest"`
	StderrDigest string                      `json:"stderr_digest"`
	ModelBroker  *model.ModelBrokerExecution `json:"model_broker,omitempty"`
}

func (projection Projection) Validate() error {
	if err := projection.Transaction.Validate(); err != nil {
		return err
	}
	if len(projection.Transaction.EffectIDs) != len(projection.Effects) {
		return transactionError(model.ErrorEventChain, "effect projection length does not match transaction effect IDs")
	}
	executionIDs := make(map[string]struct{}, len(projection.Executions))
	activeExecutions := 0
	for i, execution := range projection.Executions {
		if err := execution.Validate(); err != nil {
			return err
		}
		if execution.TransactionID != projection.Transaction.ID {
			return transactionError(model.ErrorEventChain, "execution projection transaction does not match")
		}
		if execution.Attempt > projection.Transaction.Attempt {
			return transactionError(model.ErrorEventChain, "execution belongs to a future transaction attempt")
		}
		if _, exists := executionIDs[execution.ID]; exists {
			return transactionError(model.ErrorEventChain, "execution projection contains duplicate IDs")
		}
		executionIDs[execution.ID] = struct{}{}
		if !stringContains(projection.Transaction.AgentRunIDs, execution.RunID) {
			return transactionError(model.ErrorEventChain, "execution run is not attached to the transaction")
		}
		if stageBindingIndex(projection.Transaction.StageBindings, execution.StageBindingID) < 0 {
			return transactionError(model.ErrorEventChain, "execution stage binding does not exist")
		}
		if execution.Status == model.AgentExecutionRunning {
			activeExecutions++
			if i != len(projection.Executions)-1 {
				return transactionError(model.ErrorEventChain, "only the latest execution may remain active")
			}
		}
	}
	if activeExecutions > 1 {
		return transactionError(model.ErrorEventChain, "transaction cannot have multiple active executions")
	}
	for i, effect := range projection.Effects {
		if err := effect.Validate(); err != nil {
			return err
		}
		if effect.TransactionID != projection.Transaction.ID ||
			effect.Attempt != projection.Transaction.Attempt ||
			effect.ID != projection.Transaction.EffectIDs[i] ||
			effect.Sequence != int64(i+1) {
			return transactionError(model.ErrorEventChain, "effect projection identity or order is invalid")
		}
		if effect.OriginExecutionID != "" {
			executionIndex := executionIndex(projection.Executions, effect.OriginExecutionID)
			if executionIndex < 0 {
				return transactionError(model.ErrorEventChain, "effect origin execution does not exist")
			}
			execution := projection.Executions[executionIndex]
			if execution.Attempt != effect.Attempt ||
				execution.Status != model.AgentExecutionSucceeded ||
				(effect.RunID != "" && execution.RunID != effect.RunID) {
				return transactionError(model.ErrorEventChain, "effect origin execution is not a successful execution in its attempt")
			}
		} else if projection.Transaction.Task != nil && projection.Transaction.Attempt > 0 {
			return transactionError(model.ErrorEventChain, "task-bound effect is missing its origin execution")
		}
	}
	if projection.Transaction.EffectSetDigest != "" {
		computed, err := ComputeEffectSetDigest(projection.Effects)
		if err != nil {
			return transactionError(
				model.ErrorEventChain,
				"effect projection digest cannot be recomputed",
			)
		}
		if computed != projection.Transaction.EffectSetDigest {
			return transactionError(
				model.ErrorEventChain,
				"effect projection does not match the frozen effect-set digest",
			)
		}
	}
	if len(projection.Transaction.VerificationResultIDs) != len(projection.Verifications) {
		return transactionError(model.ErrorEventChain, "verification projection length does not match transaction result IDs")
	}
	verificationIDs := make(map[string]struct{}, len(projection.Verifications)+len(projection.SupersededVerifications))
	verificationNames := make(map[string]struct{}, len(projection.Verifications))
	for i, result := range projection.Verifications {
		if err := result.Validate(); err != nil {
			return err
		}
		if result.TransactionID != projection.Transaction.ID ||
			result.Attempt != projection.Transaction.Attempt ||
			result.ID != projection.Transaction.VerificationResultIDs[i] ||
			result.EffectSetDigest != projection.Transaction.EffectSetDigest ||
			result.StagedStateDigest != projection.Transaction.StagedStateDigest {
			return transactionError(model.ErrorEventChain, "verification projection identity or order is invalid")
		}
		if _, exists := verificationIDs[result.ID]; exists {
			return transactionError(model.ErrorEventChain, "verification projection contains duplicate IDs")
		}
		verificationIDs[result.ID] = struct{}{}
		if _, exists := verificationNames[result.Name]; exists {
			return transactionError(model.ErrorEventChain, "verification projection contains duplicate names")
		}
		verificationNames[result.Name] = struct{}{}
	}
	for _, result := range projection.SupersededVerifications {
		if err := result.Validate(); err != nil {
			return err
		}
		if result.TransactionID != projection.Transaction.ID ||
			result.Attempt != projection.Transaction.Attempt ||
			result.EffectSetDigest != projection.Transaction.EffectSetDigest ||
			result.StagedStateDigest != projection.Transaction.StagedStateDigest {
			return transactionError(model.ErrorEventChain, "superseded verification belongs to another transaction attempt")
		}
		if _, exists := verificationIDs[result.ID]; exists {
			return transactionError(model.ErrorEventChain, "verification history contains duplicate IDs")
		}
		verificationIDs[result.ID] = struct{}{}
	}
	if projection.CommitPlan != nil {
		if err := projection.CommitPlan.Validate(); err != nil {
			return err
		}
		computed, err := ComputeCommitPlanDigest(*projection.CommitPlan)
		if err != nil {
			return transactionError(
				model.ErrorEventChain,
				"commit plan projection digest cannot be recomputed",
			)
		}
		if projection.CommitPlan.TransactionID != projection.Transaction.ID ||
			projection.CommitPlan.Attempt != projection.Transaction.Attempt ||
			projection.CommitPlan.IntentDigest != projection.Transaction.IntentDigest ||
			projection.CommitPlan.EffectSetDigest != projection.Transaction.EffectSetDigest ||
			projection.CommitPlan.StagedStateDigest != projection.Transaction.StagedStateDigest ||
			projection.CommitPlan.Digest != projection.Transaction.CommitPlanDigest ||
			projection.CommitPlan.Digest != computed {
			return transactionError(model.ErrorEventChain, "commit plan projection does not match transaction")
		}
		want := releaseEffectIDs(projection.Effects)
		got := make([]string, len(projection.CommitPlan.Steps))
		for index, step := range projection.CommitPlan.Steps {
			got[index] = step.EffectID
		}
		if !equalStrings(want, got) {
			return transactionError(
				model.ErrorEventChain,
				"commit plan projection does not cover the releasable effects",
			)
		}
	} else if projection.Transaction.CommitPlanDigest != "" {
		return transactionError(model.ErrorEventChain, "transaction references a missing commit plan")
	}
	if projection.ApprovalPackage != nil {
		if err := projection.ApprovalPackage.Validate(); err != nil {
			return err
		}
		computed, err := ComputeApprovalPackageDigest(*projection.ApprovalPackage)
		if err != nil {
			return transactionError(
				model.ErrorEventChain,
				"approval package projection digest cannot be recomputed",
			)
		}
		if projection.CommitPlan == nil ||
			projection.ApprovalPackage.TransactionID != projection.Transaction.ID ||
			projection.ApprovalPackage.Attempt != projection.Transaction.Attempt ||
			projection.ApprovalPackage.IntentDigest != projection.Transaction.IntentDigest ||
			projection.ApprovalPackage.EffectSetDigest != projection.Transaction.EffectSetDigest ||
			projection.ApprovalPackage.StagedStateDigest != projection.Transaction.StagedStateDigest ||
			projection.ApprovalPackage.CommitPlanDigest != projection.Transaction.CommitPlanDigest ||
			projection.ApprovalPackage.PolicyDigest != projection.CommitPlan.PolicyDigest ||
			!equalStrings(
				projection.ApprovalPackage.VerificationResultIDs,
				projection.Transaction.VerificationResultIDs,
			) ||
			projection.ApprovalPackage.Digest != projection.Transaction.ApprovalPackageDigest ||
			projection.ApprovalPackage.Digest != computed {
			return transactionError(model.ErrorEventChain, "approval package projection does not match transaction")
		}
	} else if projection.Transaction.ApprovalPackageDigest != "" {
		return transactionError(model.ErrorEventChain, "transaction references a missing approval package")
	}
	if err := validateProjectionHistory(projection); err != nil {
		return err
	}
	if !isDigest(projection.LastEventDigest) {
		return transactionError(model.ErrorEventChain, "transaction projection has an invalid last event digest")
	}
	return nil
}

var transactionTransitions = map[model.TransactionState]map[model.TransactionState]struct{}{
	model.TransactionCreated: states(model.TransactionRunning, model.TransactionBlocked, model.TransactionAborted),
	model.TransactionRunning: states(model.TransactionCompletedNoEffect, model.TransactionStaged, model.TransactionBlocked, model.TransactionAborted),
	model.TransactionStaged:  states(model.TransactionRunning, model.TransactionValidating, model.TransactionBlocked, model.TransactionAborted),
	model.TransactionValidating: states(
		model.TransactionValidationFailed,
		model.TransactionReviseRequired,
		model.TransactionBlocked,
		model.TransactionPendingApproval,
		model.TransactionReadyToCommit,
		model.TransactionAborted,
	),
	model.TransactionValidationFailed: states(model.TransactionRunning, model.TransactionValidating, model.TransactionAborted),
	model.TransactionReviseRequired:   states(model.TransactionRunning, model.TransactionAborted),
	model.TransactionPendingApproval:  states(model.TransactionReadyToCommit, model.TransactionReviseRequired, model.TransactionBlocked, model.TransactionAborted),
	model.TransactionReadyToCommit:    states(model.TransactionCommitting, model.TransactionReviseRequired, model.TransactionBlocked, model.TransactionAborted),
	model.TransactionCommitting: states(
		model.TransactionCommitted,
		model.TransactionReleaseFailed,
		model.TransactionCompensating,
		model.TransactionPartiallyCommitted,
		model.TransactionManualRecoveryRequired,
	),
	model.TransactionCompensating: states(
		model.TransactionRolledBack,
		model.TransactionPartiallyCommitted,
		model.TransactionManualRecoveryRequired,
	),
	model.TransactionBlocked:                states(),
	model.TransactionCompletedNoEffect:      states(),
	model.TransactionCommitted:              states(),
	model.TransactionRolledBack:             states(),
	model.TransactionReleaseFailed:          states(),
	model.TransactionPartiallyCommitted:     states(),
	model.TransactionManualRecoveryRequired: states(),
	model.TransactionAborted:                states(),
}

var effectTransitions = map[model.EffectStatus]map[model.EffectStatus]struct{}{
	model.EffectProposed:               statesEffect(model.EffectStaged, model.EffectBlocked, model.EffectRejected, model.EffectFailed),
	model.EffectStaged:                 statesEffect(model.EffectValidated, model.EffectBlocked, model.EffectRejected, model.EffectFailed),
	model.EffectValidated:              statesEffect(model.EffectReleaseReady, model.EffectBlocked, model.EffectRejected),
	model.EffectReleaseReady:           statesEffect(model.EffectCommitting, model.EffectRejected),
	model.EffectCommitting:             statesEffect(model.EffectCommitted, model.EffectFailed, model.EffectUnknown, model.EffectManualRecoveryRequired),
	model.EffectCommitted:              statesEffect(model.EffectCompensating, model.EffectManualRecoveryRequired),
	model.EffectUnknown:                statesEffect(model.EffectCommitted, model.EffectFailed, model.EffectManualRecoveryRequired),
	model.EffectCompensating:           statesEffect(model.EffectCompensated, model.EffectFailed, model.EffectUnknown, model.EffectManualRecoveryRequired),
	model.EffectBlocked:                statesEffect(),
	model.EffectRejected:               statesEffect(),
	model.EffectFailed:                 statesEffect(),
	model.EffectCompensated:            statesEffect(),
	model.EffectManualRecoveryRequired: statesEffect(),
}

func CanTransition(from, to model.TransactionState) bool {
	_, ok := transactionTransitions[from][to]
	return ok
}

func CanTransitionEffect(from, to model.EffectStatus) bool {
	_, ok := effectTransitions[from][to]
	return ok
}

func Replay(events []model.TransactionEvent) (Projection, error) {
	var projection *Projection
	for i, event := range events {
		next, err := Apply(projection, event)
		if err != nil {
			return Projection{}, fmt.Errorf("replay transaction event %d (%s): %w", i, event.ID, err)
		}
		projection = &next
	}
	if projection == nil {
		return Projection{}, transactionError(model.ErrorEventSequence, "at least one transaction event is required")
	}
	return *projection, nil
}

func Apply(current *Projection, event model.TransactionEvent) (Projection, error) {
	if err := event.Validate(); err != nil {
		return Projection{}, err
	}
	validDigest, err := model.VerifyTransactionEventDigest(event)
	if err != nil {
		return Projection{}, err
	}
	if !validDigest {
		return Projection{}, transactionError(model.ErrorEventChain, "transaction event digest does not match its content")
	}
	if current == nil {
		return applyCreation(event)
	}
	if event.TransactionID != current.Transaction.ID {
		return Projection{}, transactionError(model.ErrorEventSequence, "event transaction does not match the projection")
	}
	if event.Sequence != current.Transaction.EventSequence+1 {
		return Projection{}, transactionError(model.ErrorEventSequence, "transaction event sequence is not contiguous")
	}
	if event.PreviousDigest != current.LastEventDigest {
		return Projection{}, transactionError(model.ErrorEventChain, "transaction event previous digest does not match")
	}
	if event.OccurredAt.Before(current.Transaction.UpdatedAt) {
		return Projection{}, transactionError(model.ErrorSchemaInvalid, "transaction event timestamp moved backwards")
	}
	if event.Type == EventTransactionCreated {
		return Projection{}, transitionError("transaction.created can only be the first event")
	}

	next := cloneProjection(*current)
	switch event.Type {
	case EventTransactionStateChanged:
		if err := applyStateChange(&next, event); err != nil {
			return Projection{}, err
		}
	case EventEffectAdded:
		if err := applyEffectAdded(&next, event); err != nil {
			return Projection{}, err
		}
	case EventStageBindingCreated:
		if err := applyStageBindingCreated(&next, event); err != nil {
			return Projection{}, err
		}
	case EventEffectStateChanged:
		if err := applyEffectStateChange(&next, event); err != nil {
			return Projection{}, err
		}
	case EventTransactionStaged:
		if err := applyStaged(&next, event); err != nil {
			return Projection{}, err
		}
	case EventVerificationRecorded:
		if err := applyVerification(&next, event); err != nil {
			return Projection{}, err
		}
	case EventVerificationSuperseded:
		if err := applyVerificationSuperseded(&next, event); err != nil {
			return Projection{}, err
		}
	case EventCommitPlanFrozen:
		if err := applyCommitPlan(&next, event); err != nil {
			return Projection{}, err
		}
	case EventApprovalPackageFrozen:
		if err := applyApprovalPackage(&next, event); err != nil {
			return Projection{}, err
		}
	case EventAuthorityRenewed:
		if err := applyAuthorityRenewed(&next, event); err != nil {
			return Projection{}, err
		}
	case EventApprovalResolved:
		if err := applyApprovalResolution(&next, event); err != nil {
			return Projection{}, err
		}
	case EventAgentExecutionStarted:
		if err := applyAgentExecutionStarted(&next, event); err != nil {
			return Projection{}, err
		}
	case EventAgentExecutionFinished:
		if err := applyAgentExecutionFinished(&next, event); err != nil {
			return Projection{}, err
		}
	default:
		return Projection{}, transitionError(fmt.Sprintf("unsupported transaction event %q", event.Type))
	}

	next.Transaction.EventSequence = event.Sequence
	next.Transaction.UpdatedAt = event.OccurredAt
	next.LastEventDigest = event.Digest
	if err := next.Transaction.Validate(); err != nil {
		return Projection{}, err
	}
	return next, nil
}

func applyStageBindingCreated(projection *Projection, event model.TransactionEvent) error {
	if projection.Transaction.State != model.TransactionRunning {
		return transitionError("stage boundaries may only be created while a transaction is running")
	}
	payload, err := decodePayload[StageBindingCreatedPayload](event)
	if err != nil {
		return err
	}
	binding := payload.Binding
	for _, existing := range projection.Transaction.StageBindings {
		if existing.ID == binding.ID || (existing.Kind == binding.Kind && existing.Resource == binding.Resource) {
			return transactionError(model.ErrorConflict, "stage boundary already exists")
		}
	}
	projection.Transaction.StageBindings = append(projection.Transaction.StageBindings, binding)
	// AgentTransaction.Validate supplies the strict binding validation.
	return nil
}

func applyCreation(event model.TransactionEvent) (Projection, error) {
	if event.Sequence != 1 || event.Type != EventTransactionCreated {
		return Projection{}, transitionError("the first transaction event must be transaction.created at sequence 1")
	}
	payload, err := decodePayload[TransactionCreatedPayload](event)
	if err != nil {
		return Projection{}, err
	}
	transaction := payload.Transaction
	if err := transaction.Validate(); err != nil {
		return Projection{}, err
	}
	if transaction.ID != event.TransactionID ||
		transaction.State != model.TransactionCreated ||
		(transaction.Attempt != 0 && transaction.Attempt != 1) {
		return Projection{}, transitionError("transaction.created payload identity or state is invalid")
	}
	if transaction.EventSequence != 1 || !transaction.CreatedAt.Equal(event.OccurredAt) || !transaction.UpdatedAt.Equal(event.OccurredAt) {
		return Projection{}, transitionError("transaction.created event cursor and timestamps must match the transaction")
	}
	return Projection{
		Transaction:     transaction,
		Executions:      []model.AgentExecution{},
		Effects:         []model.Effect{},
		Verifications:   []model.VerificationResult{},
		ApprovalDigests: []string{},
		LastEventDigest: event.Digest,
	}, nil
}

func applyAgentExecutionStarted(projection *Projection, event model.TransactionEvent) error {
	if projection.Transaction.State != model.TransactionRunning {
		return transitionError("agent execution may only start while a transaction is running")
	}
	if activeExecutionIndex(projection.Executions) >= 0 {
		return transactionError(model.ErrorConflict, "transaction already has an active agent execution")
	}
	payload, err := decodePayload[AgentExecutionStartedPayload](event)
	if err != nil {
		return err
	}
	execution := payload.Execution
	if err := execution.Validate(); err != nil {
		return err
	}
	if execution.TransactionID != projection.Transaction.ID ||
		execution.Attempt != projection.Transaction.Attempt ||
		execution.Status != model.AgentExecutionRunning ||
		!execution.StartedAt.Equal(event.OccurredAt) {
		return transitionError("agent execution start payload does not match the transaction event")
	}
	if !stringContains(projection.Transaction.AgentRunIDs, execution.RunID) {
		return transactionError(model.ErrorIdentityInvalid, "agent execution run is not attached to the transaction")
	}
	if task := projection.Transaction.Task; task != nil {
		if execution.TaskDigest != task.Digest ||
			execution.RunID != task.RunID ||
			execution.RuntimeClass != task.AgentProfile.RuntimeClass ||
			execution.ImageDigest != task.AgentProfile.ImageDigest ||
			execution.CommandDigest != task.AgentProfile.CommandDigest {
			return transactionError(model.ErrorIdentityInvalid, "agent execution does not match its persisted task and agent profile")
		}
	} else if execution.TaskDigest != "" {
		return transactionError(model.ErrorEventChain, "legacy transaction execution cannot claim an unbound task")
	}
	if stageBindingIndex(projection.Transaction.StageBindings, execution.StageBindingID) < 0 {
		return transactionError(model.ErrorNotFound, "agent execution stage binding does not exist")
	}
	for _, existing := range projection.Executions {
		if existing.ID == execution.ID {
			return transactionError(model.ErrorConflict, "agent execution ID already exists")
		}
	}
	projection.Executions = append(projection.Executions, execution)
	return nil
}

func applyAgentExecutionFinished(projection *Projection, event model.TransactionEvent) error {
	if projection.Transaction.State != model.TransactionRunning {
		return transitionError("agent execution may only finish while a transaction is running")
	}
	payload, err := decodePayload[AgentExecutionFinishedPayload](event)
	if err != nil {
		return err
	}
	index := executionIndex(projection.Executions, payload.ExecutionID)
	if index < 0 {
		return transactionError(model.ErrorNotFound, "agent execution does not exist")
	}
	execution := projection.Executions[index]
	if execution.Status != model.AgentExecutionRunning || !payload.Status.Terminal() {
		return transitionError("agent execution finish requires an active execution and terminal status")
	}
	execution.Status = payload.Status
	execution.ExitCode = payload.ExitCode
	execution.StdoutDigest = payload.StdoutDigest
	execution.StderrDigest = payload.StderrDigest
	if execution.ModelBroker == nil && payload.ModelBroker != nil {
		return transactionError(model.ErrorEventChain, "execution cannot add a model broker at completion")
	}
	if execution.ModelBroker != nil {
		if payload.ModelBroker == nil ||
			execution.ModelBroker.Provider != payload.ModelBroker.Provider ||
			execution.ModelBroker.ImageDigest != payload.ModelBroker.ImageDigest ||
			execution.ModelBroker.PolicyDigest != payload.ModelBroker.PolicyDigest {
			return transactionError(model.ErrorEventChain, "model broker completion does not match execution start")
		}
		execution.ModelBroker = payload.ModelBroker
	}
	completedAt := event.OccurredAt
	execution.CompletedAt = &completedAt
	if err := execution.Validate(); err != nil {
		return err
	}
	projection.Executions[index] = execution
	return nil
}

func applyStateChange(projection *Projection, event model.TransactionEvent) error {
	payload, err := decodePayload[TransactionStateChangedPayload](event)
	if err != nil {
		return err
	}
	if payload.From != projection.Transaction.State {
		return transitionError("transaction transition does not start at the current state")
	}
	if !CanTransition(payload.From, payload.To) {
		return transitionError(fmt.Sprintf("transaction transition from %q to %q is not allowed", payload.From, payload.To))
	}
	if payload.To == model.TransactionStaged {
		return transitionError("staged state requires transaction.staged with frozen digests")
	}
	if payload.From == model.TransactionPendingApproval {
		return transitionError("pending approval can only advance through approval.resolved")
	}
	if stateRequiresReason(payload.To) && payload.Reason == "" {
		return transitionError("target transaction state requires a reason")
	}
	if err := validateStatePreconditions(*projection, payload); err != nil {
		return err
	}
	if payload.To == model.TransactionRunning && payload.From != model.TransactionCreated {
		if err := beginNextAttempt(projection, event.OccurredAt); err != nil {
			return err
		}
	}
	projection.Transaction.State = payload.To
	projection.Transaction.StateReason = payload.Reason
	if payload.To == model.TransactionPendingApproval {
		projection.Transaction.OutstandingApprovalIDs = append([]string(nil), payload.OutstandingApprovalIDs...)
	}
	if payload.To == model.TransactionRunning && payload.From == model.TransactionCreated {
		invalidateFrozenState(projection)
	}
	applyCompletionTime(&projection.Transaction, event.OccurredAt)
	return nil
}

func applyEffectAdded(projection *Projection, event model.TransactionEvent) error {
	if projection.Transaction.State != model.TransactionRunning {
		return transitionError("effects may only be added while a transaction is running")
	}
	payload, err := decodePayload[EffectAddedPayload](event)
	if err != nil {
		return err
	}
	effect := payload.Effect
	if err := effect.Validate(); err != nil {
		return err
	}
	if effect.TransactionID != projection.Transaction.ID ||
		effect.Attempt != projection.Transaction.Attempt ||
		effect.Sequence != int64(len(projection.Effects)+1) {
		return transactionError(model.ErrorEffectSequence, "effect transaction or sequence is invalid")
	}
	known := make(map[string]struct{}, len(projection.Effects))
	for _, existing := range projection.Effects {
		known[existing.ID] = struct{}{}
	}
	if _, exists := known[effect.ID]; exists {
		return transactionError(model.ErrorConflict, "effect ID already exists in the transaction")
	}
	for _, dependency := range effect.Dependencies {
		if _, exists := known[dependency]; !exists {
			return transactionError(model.ErrorEffectSequence, "effect dependency must refer to an earlier effect")
		}
	}
	projection.Effects = append(projection.Effects, effect)
	projection.Transaction.EffectIDs = append(projection.Transaction.EffectIDs, effect.ID)
	return nil
}

func applyEffectStateChange(projection *Projection, event model.TransactionEvent) error {
	payload, err := decodePayload[EffectStateChangedPayload](event)
	if err != nil {
		return err
	}
	index := effectIndex(projection.Effects, payload.EffectID)
	if index < 0 {
		return transactionError(model.ErrorNotFound, "effect does not exist")
	}
	effect := projection.Effects[index]
	if effect.Status != payload.From || !CanTransitionEffect(payload.From, payload.To) {
		return transitionError(fmt.Sprintf("effect transition from %q to %q is not allowed", payload.From, payload.To))
	}
	if err := validateEffectTransitionContext(projection.Transaction.State, payload.From, payload.To); err != nil {
		return err
	}
	effect.Status = payload.To
	if payload.Receipt != nil {
		effect.Receipt = payload.Receipt
	}
	if payload.CompensationReceipt != nil {
		effect.CompensationReceipt = payload.CompensationReceipt
	}
	effect.UpdatedAt = event.OccurredAt
	if err := effect.Validate(); err != nil {
		return err
	}
	projection.Effects[index] = effect
	return nil
}

func applyStaged(projection *Projection, event model.TransactionEvent) error {
	if projection.Transaction.State != model.TransactionRunning || len(projection.Effects) == 0 {
		return transitionError("only a running transaction with effects can be staged")
	}
	if activeExecutionIndex(projection.Executions) >= 0 {
		return transitionError("transaction cannot freeze effects while an agent execution is active")
	}
	payload, err := decodePayload[TransactionStagedPayload](event)
	if err != nil {
		return err
	}
	want, err := ComputeEffectSetDigest(projection.Effects)
	if err != nil {
		return transactionError(model.ErrorSchemaInvalid, err.Error())
	}
	if payload.EffectSetDigest != want {
		return transactionError(model.ErrorTransactionConflict, "frozen effect-set digest does not match the ordered effects")
	}
	if !isDigest(payload.StagedStateDigest) {
		return transactionError(model.ErrorSchemaInvalid, "staged-state digest is invalid")
	}
	projection.Transaction.State = model.TransactionStaged
	projection.Transaction.StagedStateDigest = payload.StagedStateDigest
	projection.Transaction.EffectSetDigest = payload.EffectSetDigest
	projection.Transaction.StateReason = ""
	return nil
}

func applyVerification(projection *Projection, event model.TransactionEvent) error {
	if projection.Transaction.State != model.TransactionValidating {
		return transitionError("verification can only be recorded while validating")
	}
	payload, err := decodePayload[VerificationRecordedPayload](event)
	if err != nil {
		return err
	}
	result := payload.Result
	if err := result.Validate(); err != nil {
		return err
	}
	if result.TransactionID != projection.Transaction.ID ||
		result.Attempt != projection.Transaction.Attempt ||
		result.EffectSetDigest != projection.Transaction.EffectSetDigest ||
		result.StagedStateDigest != projection.Transaction.StagedStateDigest {
		return transactionError(model.ErrorTransactionConflict, "verification does not bind to the frozen transaction state")
	}
	for _, existing := range projection.Verifications {
		if existing.ID == result.ID {
			return transactionError(model.ErrorConflict, "verification result ID already exists")
		}
		if existing.Name == result.Name {
			return transactionError(model.ErrorConflict, "verification result name already exists")
		}
	}
	for _, existing := range projection.SupersededVerifications {
		if existing.ID == result.ID {
			return transactionError(model.ErrorConflict, "verification result ID already exists in history")
		}
	}
	projection.Verifications = append(projection.Verifications, result)
	projection.Transaction.VerificationResultIDs = append(projection.Transaction.VerificationResultIDs, result.ID)
	return nil
}

func applyVerificationSuperseded(projection *Projection, event model.TransactionEvent) error {
	if projection.Transaction.State != model.TransactionValidating {
		return transitionError("verification can only be superseded while validating")
	}
	payload, err := decodePayload[VerificationSupersededPayload](event)
	if err != nil {
		return err
	}
	if payload.Reason == "" {
		return transactionError(model.ErrorSchemaInvalid, "verification supersession requires a reason")
	}
	index := verificationIndex(projection.Verifications, payload.VerificationID)
	if index < 0 {
		return transactionError(model.ErrorNotFound, "verification result does not exist")
	}
	result := projection.Verifications[index]
	if result.Status == model.VerificationPassed &&
		(result.ExpiresAt == nil || result.ExpiresAt.After(event.OccurredAt)) {
		return transitionError("a current passing verification cannot be superseded")
	}
	projection.SupersededVerifications = append(
		projection.SupersededVerifications,
		result,
	)
	projection.Verifications = removeVerification(projection.Verifications, index)
	projection.Transaction.VerificationResultIDs = removeString(
		projection.Transaction.VerificationResultIDs,
		index,
	)
	return nil
}

func applyCommitPlan(projection *Projection, event model.TransactionEvent) error {
	if projection.Transaction.State != model.TransactionValidating {
		return transitionError("a commit plan can only be frozen while validating")
	}
	payload, err := decodePayload[CommitPlanFrozenPayload](event)
	if err != nil {
		return err
	}
	plan := payload.Plan
	if err := plan.Validate(); err != nil {
		return err
	}
	planDigest, err := ComputeCommitPlanDigest(plan)
	if err != nil {
		return transactionError(model.ErrorSchemaInvalid, err.Error())
	}
	if plan.Digest != planDigest {
		return transactionError(model.ErrorTransactionConflict, "commit plan digest does not match its content")
	}
	if plan.TransactionID != projection.Transaction.ID ||
		plan.Attempt != projection.Transaction.Attempt ||
		plan.IntentDigest != projection.Transaction.IntentDigest ||
		plan.EffectSetDigest != projection.Transaction.EffectSetDigest ||
		plan.StagedStateDigest != projection.Transaction.StagedStateDigest {
		return transactionError(model.ErrorTransactionConflict, "commit plan does not bind to the frozen transaction")
	}
	want := releaseEffectIDs(projection.Effects)
	got := make([]string, len(plan.Steps))
	for i, step := range plan.Steps {
		got[i] = step.EffectID
	}
	if !equalStrings(want, got) {
		return transactionError(model.ErrorTransactionConflict, "commit plan does not cover the releasable effect set in order")
	}
	copy := plan
	projection.CommitPlan = &copy
	projection.Transaction.CommitPlanDigest = plan.Digest
	return nil
}

func applyApprovalPackage(projection *Projection, event model.TransactionEvent) error {
	if projection.Transaction.State != model.TransactionValidating || projection.CommitPlan == nil {
		return transitionError("an approval package requires validation and a frozen commit plan")
	}
	payload, err := decodePayload[ApprovalPackageFrozenPayload](event)
	if err != nil {
		return err
	}
	approval := payload.Package
	if err := approval.Validate(); err != nil {
		return err
	}
	approvalDigest, err := ComputeApprovalPackageDigest(approval)
	if err != nil {
		return transactionError(model.ErrorSchemaInvalid, err.Error())
	}
	if approval.Digest != approvalDigest {
		return transactionError(model.ErrorTransactionConflict, "approval package digest does not match its content")
	}
	if approval.TransactionID != projection.Transaction.ID ||
		approval.Attempt != projection.Transaction.Attempt ||
		approval.IntentDigest != projection.Transaction.IntentDigest ||
		approval.EffectSetDigest != projection.Transaction.EffectSetDigest ||
		approval.StagedStateDigest != projection.Transaction.StagedStateDigest ||
		approval.CommitPlanDigest != projection.Transaction.CommitPlanDigest ||
		!equalStrings(approval.VerificationResultIDs, projection.Transaction.VerificationResultIDs) {
		return transactionError(model.ErrorTransactionConflict, "approval package does not bind to the complete frozen transaction")
	}
	copy := approval
	projection.ApprovalPackage = &copy
	projection.Transaction.ApprovalPackageDigest = approval.Digest
	return nil
}

func applyAuthorityRenewed(projection *Projection, event model.TransactionEvent) error {
	if projection.Transaction.State != model.TransactionPendingApproval &&
		projection.Transaction.State != model.TransactionReadyToCommit {
		return transitionError("authority renewal requires pending or ready commit authority")
	}
	payload, err := decodePayload[AuthorityRenewedPayload](event)
	if err != nil {
		return err
	}
	if payload.Reason == "" {
		return transactionError(model.ErrorSchemaInvalid, "authority renewal requires a reason")
	}
	if projection.CommitPlan == nil || projection.ApprovalPackage == nil {
		return transitionError("authority renewal requires a frozen commit and approval package")
	}
	projection.SupersededAuthorities = append(
		projection.SupersededAuthorities,
		AuthoritySnapshot{
			Attempt:                projection.Transaction.Attempt,
			State:                  projection.Transaction.State,
			Reason:                 payload.Reason,
			SupersededAt:           event.OccurredAt,
			OutstandingApprovalIDs: append([]string(nil), projection.Transaction.OutstandingApprovalIDs...),
			CommitPlan:             cloneCommitPlan(projection.CommitPlan),
			ApprovalPackage:        cloneApprovalPackage(projection.ApprovalPackage),
			ApprovalDigests:        append([]string(nil), projection.ApprovalDigests...),
		},
	)
	current := projection.Verifications[:0]
	currentIDs := projection.Transaction.VerificationResultIDs[:0]
	for index, result := range projection.Verifications {
		if result.ExpiresAt != nil && !result.ExpiresAt.After(event.OccurredAt) {
			projection.SupersededVerifications = append(
				projection.SupersededVerifications,
				result,
			)
			continue
		}
		current = append(current, result)
		currentIDs = append(currentIDs, projection.Transaction.VerificationResultIDs[index])
	}
	projection.Verifications = current
	projection.Transaction.VerificationResultIDs = currentIDs
	projection.Transaction.State = model.TransactionValidating
	projection.Transaction.StateReason = ""
	projection.Transaction.OutstandingApprovalIDs = nil
	projection.Transaction.ApprovalPackageDigest = ""
	projection.Transaction.CommitPlanDigest = ""
	projection.CommitPlan = nil
	projection.ApprovalPackage = nil
	projection.ApprovalDigests = nil
	applyCompletionTime(&projection.Transaction, event.OccurredAt)
	return nil
}

func applyApprovalResolution(projection *Projection, event model.TransactionEvent) error {
	if projection.Transaction.State != model.TransactionPendingApproval || projection.ApprovalPackage == nil {
		return transitionError("approval resolution requires a pending immutable approval package")
	}
	payload, err := decodePayload[ApprovalResolvedPayload](event)
	if err != nil {
		return err
	}
	decision := payload.Decision
	if err := decision.Validate(); err != nil {
		return err
	}
	decisionDigest, err := ComputeApprovalDecisionDigest(decision)
	if err != nil {
		return transactionError(model.ErrorApprovalInvalid, "cannot compute approval decision digest")
	}
	if decision.Digest != decisionDigest ||
		decision.TransactionID != projection.Transaction.ID ||
		decision.PackageDigest != projection.Transaction.ApprovalPackageDigest ||
		decision.Approver != event.Actor ||
		event.OccurredAt.Before(decision.IssuedAt.Add(-2*time.Minute)) ||
		!decision.ExpiresAt.After(event.OccurredAt) ||
		!projection.ApprovalPackage.ExpiresAt.After(event.OccurredAt) ||
		!stringContains(projection.ApprovalPackage.RequiredApprovalClasses, decision.ApprovalClass) {
		return transactionError(model.ErrorApprovalInvalid, "approval does not bind to the current package")
	}
	index := stringIndex(projection.Transaction.OutstandingApprovalIDs, decision.ApprovalID)
	if index < 0 {
		return transactionError(model.ErrorApprovalInvalid, "approval is not outstanding or was already consumed")
	}
	switch decision.Decision {
	case model.ApprovalApprove:
		projection.Transaction.OutstandingApprovalIDs = removeString(projection.Transaction.OutstandingApprovalIDs, index)
		projection.ApprovalDigests = append(projection.ApprovalDigests, decision.Digest)
		if len(projection.Transaction.OutstandingApprovalIDs) == 0 {
			if err := requirePassingVerification(*projection); err != nil {
				return err
			}
			projection.Transaction.State = model.TransactionReadyToCommit
		}
	case model.ApprovalReject:
		projection.Transaction.State = model.TransactionBlocked
		projection.Transaction.StateReason = decision.Reason
		projection.Transaction.OutstandingApprovalIDs = nil
	case model.ApprovalRevise:
		projection.Transaction.State = model.TransactionReviseRequired
		projection.Transaction.StateReason = decision.Reason
		projection.Transaction.OutstandingApprovalIDs = nil
	default:
		return transactionError(model.ErrorApprovalInvalid, "approval decision must be approve, reject, or revise")
	}
	applyCompletionTime(&projection.Transaction, event.OccurredAt)
	return nil
}

func validateStatePreconditions(projection Projection, payload TransactionStateChangedPayload) error {
	switch payload.To {
	case model.TransactionCompletedNoEffect:
		if len(projection.Effects) != 0 || activeExecutionIndex(projection.Executions) >= 0 {
			return transitionError("no-effect completion requires no staged effects or active execution")
		}
		execution, ok := latestExecutionForAttempt(
			projection.Executions,
			projection.Transaction.Attempt,
		)
		if !ok || execution.Status != model.AgentExecutionSucceeded {
			return transitionError("no-effect completion requires a successful execution in the current attempt")
		}
	case model.TransactionValidating:
		if projection.Transaction.EffectSetDigest == "" || projection.Transaction.StagedStateDigest == "" {
			return transitionError("validation requires frozen staged state and effects")
		}
	case model.TransactionPendingApproval:
		if projection.CommitPlan == nil || projection.ApprovalPackage == nil || len(payload.OutstandingApprovalIDs) == 0 {
			return transitionError("pending approval requires a commit plan, approval package, and approval IDs")
		}
		if err := requireNoFailedVerification(projection); err != nil {
			return err
		}
	case model.TransactionReadyToCommit:
		if projection.CommitPlan == nil || projection.ApprovalPackage == nil {
			return transitionError("ready-to-commit requires frozen commit and approval packages")
		}
		if len(projection.ApprovalPackage.RequiredApprovalClasses) != 0 {
			return transactionError(model.ErrorApprovalRequired, "required approvals cannot be bypassed")
		}
		if len(payload.OutstandingApprovalIDs) != 0 {
			return transitionError("automatic commit cannot introduce approval IDs")
		}
		return requirePassingVerification(projection)
	case model.TransactionCommitting:
		return requireAllEffectsStatus(projection.Effects, model.EffectReleaseReady)
	case model.TransactionCommitted:
		return requireAllEffectsStatus(projection.Effects, model.EffectCommitted)
	case model.TransactionCompensating:
		if !hasEffectStatus(projection.Effects, model.EffectCommitted) {
			return transitionError("compensation requires at least one committed effect")
		}
	case model.TransactionRolledBack:
		for _, effect := range projection.Effects {
			if effect.RecoveryClass == model.RecoveryReadOnly {
				continue
			}
			if effect.Status == model.EffectCommitted || effect.Status == model.EffectCompensating || effect.Status == model.EffectUnknown || effect.Status == model.EffectManualRecoveryRequired {
				return transactionError(model.ErrorPartialCommit, "rolled_back cannot leave a committed or unresolved effect")
			}
		}
	case model.TransactionReleaseFailed:
		if !hasEffectStatus(projection.Effects, model.EffectFailed) {
			return transitionError("release_failed requires at least one failed effect")
		}
	case model.TransactionPartiallyCommitted:
		if !hasAnyEffectStatus(projection.Effects, model.EffectCommitted, model.EffectUnknown, model.EffectManualRecoveryRequired, model.EffectFailed) {
			return transactionError(model.ErrorPartialCommit, "partial commit requires a committed, failed, or unresolved effect")
		}
	case model.TransactionManualRecoveryRequired:
		if !hasAnyEffectStatus(projection.Effects, model.EffectUnknown, model.EffectManualRecoveryRequired, model.EffectCommitted) {
			return transactionError(model.ErrorPartialCommit, "manual recovery requires unresolved external state")
		}
	}
	return nil
}

func validateEffectTransitionContext(state model.TransactionState, from, target model.EffectStatus) error {
	switch target {
	case model.EffectValidated:
		if state != model.TransactionValidating {
			return transitionError("effects can only validate while the transaction is validating")
		}
	case model.EffectReleaseReady:
		if state != model.TransactionReadyToCommit {
			return transitionError("effects become release-ready only after transaction authority is complete")
		}
	case model.EffectCommitting, model.EffectCommitted, model.EffectUnknown:
		if state != model.TransactionCommitting && state != model.TransactionCompensating {
			return transitionError("release result requires a committing or compensating transaction")
		}
	case model.EffectFailed:
		if (from == model.EffectCommitting || from == model.EffectCompensating) && state != model.TransactionCommitting && state != model.TransactionCompensating {
			return transitionError("release failure requires a committing or compensating transaction")
		}
	case model.EffectCompensating, model.EffectCompensated:
		if state != model.TransactionCompensating {
			return transitionError("effect compensation requires a compensating transaction")
		}
	}
	return nil
}

func requireNoFailedVerification(projection Projection) error {
	if len(projection.Verifications) == 0 {
		return transactionError(model.ErrorVerificationFailed, "at least one verification result is required")
	}
	for _, result := range projection.Verifications {
		if result.Status == model.VerificationFailed {
			return transactionError(model.ErrorVerificationFailed, "a verification result failed")
		}
	}
	return nil
}

func requirePassingVerification(projection Projection) error {
	if err := requireNoFailedVerification(projection); err != nil {
		return err
	}
	for _, result := range projection.Verifications {
		if result.Status != model.VerificationPassed {
			return transactionError(model.ErrorVerificationFailed, "all verification results must pass before commit")
		}
		if result.Independence != model.VerificationPlatformRun {
			return transactionError(model.ErrorVerificationFailed, "commit authority requires daemon-run independent verification")
		}
	}
	return nil
}

func requireAllEffectsStatus(effects []model.Effect, status model.EffectStatus) error {
	for _, effect := range effects {
		if effect.RecoveryClass == model.RecoveryReadOnly {
			continue
		}
		if effect.Status != status {
			return transitionError(fmt.Sprintf("effect %q is %q, expected %q", effect.ID, effect.Status, status))
		}
	}
	return nil
}

func validateProjectionHistory(projection Projection) error {
	for _, authority := range projection.SupersededAuthorities {
		if err := validateAuthoritySnapshot(
			authority,
			projection.Transaction.ID,
			projection.Transaction.Attempt,
		); err != nil {
			return err
		}
	}
	if len(projection.SupersededAttempts) == 0 {
		if projection.Transaction.Attempt > 1 {
			return transactionError(model.ErrorEventChain, "active attempt is missing its superseded history")
		}
		return nil
	}
	firstAttempt := projection.SupersededAttempts[0].Attempt
	if firstAttempt != 0 && firstAttempt != 1 {
		return transactionError(model.ErrorEventChain, "superseded attempt history must begin at zero or one")
	}
	previousAttempt := firstAttempt - 1
	for _, attempt := range projection.SupersededAttempts {
		if attempt.Attempt != previousAttempt+1 ||
			attempt.Attempt >= projection.Transaction.Attempt {
			return transactionError(model.ErrorEventChain, "superseded attempts are not ordered before the active attempt")
		}
		previousAttempt = attempt.Attempt
		if attempt.SupersededAt.IsZero() || attempt.Reason == "" ||
			(attempt.State != model.TransactionStaged &&
				attempt.State != model.TransactionValidationFailed &&
				attempt.State != model.TransactionReviseRequired) {
			return transactionError(model.ErrorEventChain, "superseded attempt metadata is incomplete")
		}
		seenExecutions := make(map[string]struct{}, len(attempt.ExecutionIDs))
		for _, executionID := range attempt.ExecutionIDs {
			if _, exists := seenExecutions[executionID]; exists {
				return transactionError(model.ErrorEventChain, "superseded attempt contains duplicate execution IDs")
			}
			seenExecutions[executionID] = struct{}{}
			index := executionIndex(projection.Executions, executionID)
			if index < 0 || projection.Executions[index].Attempt != attempt.Attempt {
				return transactionError(model.ErrorEventChain, "superseded attempt execution binding is invalid")
			}
		}
		if len(attempt.EffectIDs) != len(attempt.Effects) ||
			!isDigest(attempt.StagedStateDigest) ||
			!isDigest(attempt.EffectSetDigest) {
			return transactionError(model.ErrorEventChain, "superseded attempt frozen effect metadata is invalid")
		}
		for index, effect := range attempt.Effects {
			if err := effect.Validate(); err != nil {
				return err
			}
			if effect.TransactionID != projection.Transaction.ID ||
				effect.Attempt != attempt.Attempt ||
				effect.ID != attempt.EffectIDs[index] ||
				effect.Sequence != int64(index+1) {
				return transactionError(model.ErrorEventChain, "superseded attempt effect binding is invalid")
			}
		}
		computedEffects, err := ComputeEffectSetDigest(attempt.Effects)
		if err != nil || computedEffects != attempt.EffectSetDigest {
			return transactionError(model.ErrorEventChain, "superseded attempt effect digest is invalid")
		}
		if len(attempt.VerificationResultIDs) != len(attempt.Verifications) {
			return transactionError(model.ErrorEventChain, "superseded attempt verification IDs are incomplete")
		}
		for index, result := range attempt.Verifications {
			if err := result.Validate(); err != nil {
				return err
			}
			if result.TransactionID != projection.Transaction.ID ||
				result.Attempt != attempt.Attempt ||
				result.ID != attempt.VerificationResultIDs[index] ||
				result.EffectSetDigest != attempt.EffectSetDigest ||
				result.StagedStateDigest != attempt.StagedStateDigest {
				return transactionError(model.ErrorEventChain, "superseded attempt verification binding is invalid")
			}
		}
		for _, result := range attempt.SupersededVerifications {
			if err := result.Validate(); err != nil {
				return err
			}
			if result.TransactionID != projection.Transaction.ID ||
				result.Attempt != attempt.Attempt ||
				result.EffectSetDigest != attempt.EffectSetDigest ||
				result.StagedStateDigest != attempt.StagedStateDigest {
				return transactionError(model.ErrorEventChain, "superseded attempt verification history binding is invalid")
			}
		}
		if attempt.CommitPlan != nil {
			if err := attempt.CommitPlan.Validate(); err != nil {
				return err
			}
			computed, err := ComputeCommitPlanDigest(*attempt.CommitPlan)
			if err != nil || computed != attempt.CommitPlan.Digest {
				return transactionError(model.ErrorEventChain, "superseded attempt commit plan digest is invalid")
			}
			if attempt.CommitPlan.TransactionID != projection.Transaction.ID ||
				attempt.CommitPlan.Attempt != attempt.Attempt ||
				attempt.CommitPlan.Digest != attempt.CommitPlanDigest ||
				attempt.CommitPlan.EffectSetDigest != attempt.EffectSetDigest ||
				attempt.CommitPlan.StagedStateDigest != attempt.StagedStateDigest {
				return transactionError(model.ErrorEventChain, "superseded attempt commit plan binding is invalid")
			}
		} else if attempt.CommitPlanDigest != "" {
			return transactionError(model.ErrorEventChain, "superseded attempt references a missing commit plan")
		}
		if attempt.ApprovalPackage != nil {
			if err := attempt.ApprovalPackage.Validate(); err != nil {
				return err
			}
			computed, err := ComputeApprovalPackageDigest(*attempt.ApprovalPackage)
			if err != nil || computed != attempt.ApprovalPackage.Digest {
				return transactionError(model.ErrorEventChain, "superseded attempt approval package digest is invalid")
			}
			if attempt.ApprovalPackage.TransactionID != projection.Transaction.ID ||
				attempt.ApprovalPackage.Attempt != attempt.Attempt ||
				attempt.ApprovalPackage.Digest != attempt.ApprovalPackageDigest ||
				attempt.ApprovalPackage.EffectSetDigest != attempt.EffectSetDigest ||
				attempt.ApprovalPackage.StagedStateDigest != attempt.StagedStateDigest ||
				attempt.ApprovalPackage.CommitPlanDigest != attempt.CommitPlanDigest {
				return transactionError(model.ErrorEventChain, "superseded attempt approval package binding is invalid")
			}
		} else if attempt.ApprovalPackageDigest != "" {
			return transactionError(model.ErrorEventChain, "superseded attempt references a missing approval package")
		}
		for _, authority := range attempt.SupersededAuthorities {
			if err := validateAuthoritySnapshot(
				authority,
				projection.Transaction.ID,
				attempt.Attempt,
			); err != nil {
				return err
			}
		}
	}
	if previousAttempt+1 != projection.Transaction.Attempt {
		return transactionError(model.ErrorEventChain, "superseded attempt history is not contiguous with the active attempt")
	}
	return nil
}

func validateAuthoritySnapshot(
	authority AuthoritySnapshot,
	transactionID string,
	attempt int64,
) error {
	if authority.Attempt != attempt ||
		(authority.State != model.TransactionPendingApproval &&
			authority.State != model.TransactionReadyToCommit) ||
		authority.Reason == "" || authority.SupersededAt.IsZero() ||
		authority.CommitPlan == nil || authority.ApprovalPackage == nil {
		return transactionError(model.ErrorEventChain, "superseded authority metadata is invalid")
	}
	if err := authority.CommitPlan.Validate(); err != nil {
		return err
	}
	if err := authority.ApprovalPackage.Validate(); err != nil {
		return err
	}
	planDigest, planErr := ComputeCommitPlanDigest(*authority.CommitPlan)
	approvalDigest, approvalErr := ComputeApprovalPackageDigest(*authority.ApprovalPackage)
	if planErr != nil || approvalErr != nil ||
		planDigest != authority.CommitPlan.Digest ||
		approvalDigest != authority.ApprovalPackage.Digest {
		return transactionError(model.ErrorEventChain, "superseded authority digest is invalid")
	}
	if authority.CommitPlan.TransactionID != transactionID ||
		authority.CommitPlan.Attempt != attempt ||
		authority.ApprovalPackage.TransactionID != transactionID ||
		authority.ApprovalPackage.Attempt != attempt ||
		authority.ApprovalPackage.CommitPlanDigest != authority.CommitPlan.Digest {
		return transactionError(model.ErrorEventChain, "superseded authority binding is invalid")
	}
	return nil
}

func invalidateFrozenState(projection *Projection) {
	projection.Transaction.EffectIDs = nil
	projection.Transaction.StagedStateDigest = ""
	projection.Transaction.EffectSetDigest = ""
	projection.Transaction.VerificationResultIDs = nil
	projection.Transaction.OutstandingApprovalIDs = nil
	projection.Transaction.ApprovalPackageDigest = ""
	projection.Transaction.CommitPlanDigest = ""
	projection.Effects = nil
	projection.Verifications = nil
	projection.SupersededVerifications = nil
	projection.CommitPlan = nil
	projection.ApprovalPackage = nil
	projection.ApprovalDigests = nil
	projection.SupersededAuthorities = nil
}

func beginNextAttempt(projection *Projection, supersededAt time.Time) error {
	if activeExecutionIndex(projection.Executions) >= 0 {
		return transitionError("a new attempt cannot begin while an agent execution is active")
	}
	reason := projection.Transaction.StateReason
	if reason == "" {
		reason = "transaction work revised"
	}
	executionIDs := make([]string, 0)
	for _, execution := range projection.Executions {
		if execution.Attempt == projection.Transaction.Attempt {
			executionIDs = append(executionIDs, execution.ID)
		}
	}
	projection.SupersededAttempts = append(
		projection.SupersededAttempts,
		AttemptSnapshot{
			Attempt:                 projection.Transaction.Attempt,
			State:                   projection.Transaction.State,
			Reason:                  reason,
			SupersededAt:            supersededAt,
			ExecutionIDs:            executionIDs,
			EffectIDs:               append([]string(nil), projection.Transaction.EffectIDs...),
			Effects:                 append([]model.Effect(nil), projection.Effects...),
			StagedStateDigest:       projection.Transaction.StagedStateDigest,
			EffectSetDigest:         projection.Transaction.EffectSetDigest,
			VerificationResultIDs:   append([]string(nil), projection.Transaction.VerificationResultIDs...),
			Verifications:           append([]model.VerificationResult(nil), projection.Verifications...),
			SupersededVerifications: append([]model.VerificationResult(nil), projection.SupersededVerifications...),
			CommitPlan:              cloneCommitPlan(projection.CommitPlan),
			CommitPlanDigest:        projection.Transaction.CommitPlanDigest,
			ApprovalPackage:         cloneApprovalPackage(projection.ApprovalPackage),
			ApprovalPackageDigest:   projection.Transaction.ApprovalPackageDigest,
			OutstandingApprovalIDs:  append([]string(nil), projection.Transaction.OutstandingApprovalIDs...),
			ApprovalDigests:         append([]string(nil), projection.ApprovalDigests...),
			SupersededAuthorities:   append([]AuthoritySnapshot(nil), projection.SupersededAuthorities...),
		},
	)
	projection.Transaction.Attempt++
	invalidateFrozenState(projection)
	return nil
}

func applyCompletionTime(transaction *model.AgentTransaction, occurredAt time.Time) {
	if transaction.State.Terminal() {
		completedAt := occurredAt
		transaction.CompletedAt = &completedAt
	} else {
		transaction.CompletedAt = nil
	}
}

func cloneProjection(source Projection) Projection {
	result := source
	result.Transaction.AgentRunIDs = append([]string(nil), source.Transaction.AgentRunIDs...)
	result.Transaction.StageBindings = append([]model.StageBinding(nil), source.Transaction.StageBindings...)
	result.Transaction.EffectIDs = append([]string(nil), source.Transaction.EffectIDs...)
	result.Transaction.VerificationResultIDs = append([]string(nil), source.Transaction.VerificationResultIDs...)
	result.Transaction.OutstandingApprovalIDs = append([]string(nil), source.Transaction.OutstandingApprovalIDs...)
	if source.Transaction.Task != nil {
		task := *source.Transaction.Task
		result.Transaction.Task = &task
	}
	result.Executions = append([]model.AgentExecution(nil), source.Executions...)
	result.Effects = append([]model.Effect(nil), source.Effects...)
	result.Verifications = append([]model.VerificationResult(nil), source.Verifications...)
	result.SupersededVerifications = append(
		[]model.VerificationResult(nil),
		source.SupersededVerifications...,
	)
	result.ApprovalDigests = append([]string(nil), source.ApprovalDigests...)
	result.CommitPlan = cloneCommitPlan(source.CommitPlan)
	result.ApprovalPackage = cloneApprovalPackage(source.ApprovalPackage)
	result.SupersededAuthorities = append(
		[]AuthoritySnapshot(nil),
		source.SupersededAuthorities...,
	)
	result.SupersededAttempts = append(
		[]AttemptSnapshot(nil),
		source.SupersededAttempts...,
	)
	return result
}

func cloneCommitPlan(source *model.CommitPlan) *model.CommitPlan {
	if source == nil {
		return nil
	}
	copy := *source
	return &copy
}

func cloneApprovalPackage(source *model.ApprovalPackage) *model.ApprovalPackage {
	if source == nil {
		return nil
	}
	copy := *source
	return &copy
}

func activeExecutionIndex(executions []model.AgentExecution) int {
	for i := len(executions) - 1; i >= 0; i-- {
		if executions[i].Status == model.AgentExecutionRunning {
			return i
		}
	}
	return -1
}

func executionIndex(executions []model.AgentExecution, executionID string) int {
	for i, execution := range executions {
		if execution.ID == executionID {
			return i
		}
	}
	return -1
}

func latestExecutionForAttempt(
	executions []model.AgentExecution,
	attempt int64,
) (model.AgentExecution, bool) {
	for index := len(executions) - 1; index >= 0; index-- {
		if executions[index].Attempt == attempt {
			return executions[index], true
		}
	}
	return model.AgentExecution{}, false
}

func verificationIndex(
	verifications []model.VerificationResult,
	verificationID string,
) int {
	for index, verification := range verifications {
		if verification.ID == verificationID {
			return index
		}
	}
	return -1
}

func removeVerification(
	verifications []model.VerificationResult,
	index int,
) []model.VerificationResult {
	result := append([]model.VerificationResult(nil), verifications[:index]...)
	return append(result, verifications[index+1:]...)
}

func stageBindingIndex(bindings []model.StageBinding, bindingID string) int {
	for i, binding := range bindings {
		if binding.ID == bindingID {
			return i
		}
	}
	return -1
}

func stringContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func decodePayload[T any](event model.TransactionEvent) (T, error) {
	value, err := model.DecodeStrict[T](event.Payload)
	if err == nil {
		return value, nil
	}
	var zero T
	var kernelErr *model.KernelError
	if errors.As(err, &kernelErr) {
		kernelErr.Operation = "decode_transaction_event_payload"
		kernelErr.Resource = event.ID
	}
	return zero, err
}

func releaseEffectIDs(effects []model.Effect) []string {
	result := make([]string, 0, len(effects))
	for _, effect := range effects {
		if effect.RecoveryClass != model.RecoveryReadOnly {
			result = append(result, effect.ID)
		}
	}
	return result
}

func effectIndex(effects []model.Effect, id string) int {
	for i, effect := range effects {
		if effect.ID == id {
			return i
		}
	}
	return -1
}

func hasEffectStatus(effects []model.Effect, status model.EffectStatus) bool {
	return hasAnyEffectStatus(effects, status)
}

func hasAnyEffectStatus(effects []model.Effect, statuses ...model.EffectStatus) bool {
	for _, effect := range effects {
		for _, status := range statuses {
			if effect.Status == status {
				return true
			}
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func stringIndex(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}
	return -1
}

func removeString(values []string, index int) []string {
	result := append([]string(nil), values[:index]...)
	return append(result, values[index+1:]...)
}

func states(values ...model.TransactionState) map[model.TransactionState]struct{} {
	result := make(map[model.TransactionState]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func statesEffect(values ...model.EffectStatus) map[model.EffectStatus]struct{} {
	result := make(map[model.EffectStatus]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func stateRequiresReason(state model.TransactionState) bool {
	switch state {
	case model.TransactionValidationFailed, model.TransactionReviseRequired,
		model.TransactionBlocked, model.TransactionReleaseFailed,
		model.TransactionPartiallyCommitted,
		model.TransactionManualRecoveryRequired, model.TransactionAborted:
		return true
	default:
		return false
	}
}

func transitionError(message string) *model.KernelError {
	return transactionError(model.ErrorTransitionInvalid, message)
}

func transactionError(code model.ErrorCode, message string) *model.KernelError {
	return &model.KernelError{Code: code, Operation: "transaction", Message: message}
}

func isDigest(value string) bool {
	if len(value) != len("sha256:")+64 || value[:len("sha256:")] != "sha256:" {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
