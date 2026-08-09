// Package authority lowers live kernel authority into an exact OCI execution
// plan. It is pure: it performs no I/O and starts no workloads.
package authority

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/sandbox"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
)

const (
	WorkspaceReadWrite = "rw"
	NetworkNone        = "none"
	NetworkModelBroker = "internal_model_broker"
	ModelResourceKind  = "model"
	ModelInvoke        = "model.invoke"
)

// ModelBrokerCeiling is the daemon's already-enforced static broker policy.
// Finite contract budgets must be at least as restrictive as this policy.
type ModelBrokerCeiling struct {
	Provider                  string
	AllowedModels             []string
	ImageDigest               string
	PolicyDigest              string
	MaxModelCalls             int64
	MaxInputTokens            int64
	MaxOutputTokens           int64
	MaxOutputTokensPerRequest int64
}

// DaemonCeilings may narrow persisted authority, never widen it.
type DaemonCeilings struct {
	MaxWallTimeSeconds  int64
	AllowedImageDigests []string
	ModelBroker         *ModelBrokerCeiling
}

// CompileInput combines authoritative stored objects with untrusted executable
// material. ImageReference and Command carry bytes to execute; their admitted
// digests remain the authority.
type CompileInput struct {
	Task        model.AgentTask
	Contract    model.ExecutionContract
	Run         model.AgentRun
	Grants      []model.CapabilityGrant
	Transaction model.AgentTransaction
	Executions  []model.AgentExecution

	ImageReference       string
	Command              []string
	Ceilings             DaemonCeilings
	CallerTimeoutSeconds *int64
	Now                  time.Time
}

type ModelBrokerPlan struct {
	Provider                  string
	AllowedModels             []string
	ImageDigest               string
	PolicyDigest              string
	MaxModelCalls             int64
	MaxInputTokens            int64
	MaxOutputTokens           int64
	MaxOutputTokensPerRequest int64
}

// ExecutionPlan is directly consumable by the current OCI handler. If
// Entrypoint is set, Command excludes that first executable argument.
type ExecutionPlan struct {
	Namespace      string
	TransactionID  string
	StageBindingID string
	TaskID         string
	TaskDigest     string
	ContractID     string
	ContractDigest string
	RunID          string
	CapabilityIDs  []string

	ImageReference string
	ImageDigest    string
	CommandDigest  string
	Entrypoint     string
	Command        []string

	WorkspaceAccess string
	NetworkAccess   string
	ModelBroker     *ModelBrokerPlan
	CompiledAt      time.Time
	NotAfter        time.Time
	TimeoutSeconds  int64
}

// Compile verifies live admission lineage, active grants, executable digests,
// daemon ceilings, and deadlines before producing a launch plan.
func Compile(input CompileInput) (ExecutionPlan, error) {
	now := input.Now.UTC()
	if now.IsZero() {
		return ExecutionPlan{}, deny(model.ErrorSchemaInvalid, input.Run.ID, "daemon clock is required", nil)
	}
	if err := validateObjects(input); err != nil {
		return ExecutionPlan{}, err
	}
	if err := validateBindings(input); err != nil {
		return ExecutionPlan{}, err
	}
	if err := validateCeilings(input.Ceilings); err != nil {
		return ExecutionPlan{}, err
	}

	imageDigest, err := sandbox.ImageDigest(input.ImageReference)
	if err != nil {
		return ExecutionPlan{}, deny(model.ErrorSchemaInvalid, input.Task.ID, "image must be digest pinned", err)
	}
	if imageDigest != input.Task.AgentProfile.ImageDigest ||
		imageDigest != input.Run.ImageDigest {
		return ExecutionPlan{}, deny(model.ErrorCapabilityDenied, input.Run.ID, "image digest does not match admission", nil)
	}
	if !contains(input.Ceilings.AllowedImageDigests, imageDigest) {
		return ExecutionPlan{}, deny(model.ErrorCapabilityDenied, imageDigest, "image is outside the daemon ceiling", nil)
	}

	commandDigest, err := commandDigest(input.Command)
	if err != nil {
		return ExecutionPlan{}, deny(model.ErrorSchemaInvalid, input.Task.ID, "command is invalid", err)
	}
	if commandDigest != input.Task.AgentProfile.CommandDigest {
		return ExecutionPlan{}, deny(model.ErrorCapabilityDenied, input.Run.ID, "command digest does not match admission", nil)
	}
	entrypoint := input.Task.AgentProfile.Entrypoint
	command := append([]string(nil), input.Command...)
	if entrypoint != "" {
		if input.Command[0] != entrypoint {
			return ExecutionPlan{}, deny(model.ErrorCapabilityDenied, input.Run.ID, "command does not match admitted entrypoint", nil)
		}
		command = append([]string(nil), input.Command[1:]...)
	}

	modelPlan, network, err := validateGrants(input, now)
	if err != nil {
		return ExecutionPlan{}, err
	}
	notAfter, timeout, err := timeoutBoundary(input, now)
	if err != nil {
		return ExecutionPlan{}, err
	}
	return ExecutionPlan{
		Namespace: input.Task.Namespace, TransactionID: input.Transaction.ID,
		StageBindingID: input.Transaction.StageBindings[0].ID,
		TaskID:         input.Task.ID, TaskDigest: input.Task.Digest,
		ContractID: input.Contract.ID, ContractDigest: input.Contract.Digest,
		RunID:          input.Run.ID,
		CapabilityIDs:  append([]string(nil), input.Run.CapabilityIDs...),
		ImageReference: input.ImageReference, ImageDigest: imageDigest,
		CommandDigest: commandDigest, Entrypoint: entrypoint, Command: command,
		WorkspaceAccess: WorkspaceReadWrite, NetworkAccess: network,
		ModelBroker: cloneModelPlan(modelPlan),
		CompiledAt:  now, NotAfter: notAfter, TimeoutSeconds: timeout,
	}, nil
}

func validateObjects(input CompileInput) error {
	if err := input.Task.Validate(); err != nil {
		return err
	}
	if err := input.Contract.Validate(); err != nil {
		return err
	}
	if err := input.Run.Validate(); err != nil {
		return err
	}
	if err := input.Transaction.Validate(); err != nil {
		return err
	}
	digest, err := model.ComputeExecutionContractDigest(input.Contract)
	if err != nil {
		return deny(model.ErrorInternal, input.Contract.ID, "digest contract", err)
	}
	if digest != input.Contract.Digest {
		return deny(model.ErrorEventChain, input.Contract.ID, "contract content digest is invalid", nil)
	}
	for index := range input.Grants {
		if err := input.Grants[index].Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateBindings(input CompileInput) error {
	task, contract := input.Task, input.Contract
	run, transaction := input.Run, input.Transaction
	binding := transaction.Admission
	if task.AgentProfile.RuntimeClass != "oci" {
		return deny(model.ErrorCapabilityDenied, task.ID, "only OCI execution is authoritative", nil)
	}
	if !executionLaunchableRunState(run.State) ||
		run.ActiveExecutionID != "" || run.Runtime != nil ||
		run.ParentRunID != "" ||
		run.CheckpointID != "" || len(run.OutstandingApprovalIDs) != 0 {
		return deny(model.ErrorTransitionInvalid, run.ID, "run is not launchable for a supervised execution", nil)
	}
	if transaction.State != model.TransactionRunning ||
		len(transaction.StageBindings) != 1 ||
		hasActiveExecution(input.Executions) {
		return deny(model.ErrorTransitionInvalid, transaction.ID, "transaction must be running with one stage and no active execution", nil)
	}
	if binding == nil || transaction.Task == nil ||
		task.Namespace != run.Namespace || task.Namespace != transaction.Namespace ||
		task.TransactionID != transaction.ID || task.RunID != run.ID ||
		run.ContractDigest != contract.Digest ||
		binding.TaskDigest != task.Digest || binding.RunID != run.ID ||
		binding.ContractDigest != contract.Digest ||
		!reflect.DeepEqual(*transaction.Task, task) ||
		transaction.IntentDigest != task.IntentDigest ||
		len(transaction.AgentRunIDs) != 1 ||
		transaction.AgentRunIDs[0] != run.ID {
		return deny(model.ErrorEventChain, transaction.ID, "task, contract, run, and transaction do not cross-bind", nil)
	}
	if !reflect.DeepEqual(contract.Owner, transaction.Sponsor) ||
		len(run.DelegationChain) != 1 ||
		!reflect.DeepEqual(run.DelegationChain[0], transaction.Sponsor) {
		return deny(model.ErrorIdentityInvalid, run.ID, "delegation lineage does not match sponsor", nil)
	}
	if run.ImageDigest != task.AgentProfile.ImageDigest ||
		!sameTime(run.Deadline, contract.Deadline) ||
		!reflect.DeepEqual(run.BudgetLimits, contract.Budgets) {
		return deny(model.ErrorEventChain, run.ID, "run executable, deadline, or budget binding changed", nil)
	}
	if contract.MaxChildDepth != nil || contract.MaxChildren != nil ||
		len(contract.Checkpoints) != 0 || len(contract.Obligations) != 0 ||
		contract.Escalation != nil || contract.ReleaseContractRef != "" ||
		contract.Budgets.MaxToolCalls != nil ||
		contract.Budgets.MaxCostMicros != nil {
		return deny(model.ErrorCapabilityDenied, contract.ID, "contract contains unsupported execution controls", nil)
	}
	return nil
}

func executionLaunchableRunState(state model.RunState) bool {
	switch state {
	case model.RunAdmitted, model.RunWaitingForAgent, model.RunWaitingForEvent:
		return true
	default:
		return false
	}
}

func validateCeilings(ceilings DaemonCeilings) error {
	if ceilings.MaxWallTimeSeconds < 1 || len(ceilings.AllowedImageDigests) == 0 {
		return deny(model.ErrorSchemaInvalid, "daemon", "daemon ceilings are incomplete", nil)
	}
	seen := map[string]struct{}{}
	for _, digest := range ceilings.AllowedImageDigests {
		if !model.IsSHA256Digest(digest) {
			return deny(model.ErrorSchemaInvalid, "daemon", "daemon image digest is invalid", nil)
		}
		if _, duplicate := seen[digest]; duplicate {
			return deny(model.ErrorSchemaInvalid, "daemon", "daemon image digests are duplicated", nil)
		}
		seen[digest] = struct{}{}
	}
	if broker := ceilings.ModelBroker; broker != nil {
		if !model.IsIdentifier(broker.Provider) ||
			!model.IsSHA256Digest(broker.ImageDigest) ||
			!model.IsSHA256Digest(broker.PolicyDigest) ||
			len(broker.AllowedModels) == 0 ||
			broker.MaxModelCalls < 1 || broker.MaxInputTokens < 1 ||
			broker.MaxOutputTokens < 1 || broker.MaxOutputTokensPerRequest < 1 ||
			broker.MaxOutputTokensPerRequest > broker.MaxOutputTokens {
			return deny(model.ErrorSchemaInvalid, "daemon-model-broker", "model broker ceiling is invalid", nil)
		}
		for _, allowed := range broker.AllowedModels {
			if strings.TrimSpace(allowed) != allowed || allowed == "" || strings.ContainsRune(allowed, 0) {
				return deny(model.ErrorSchemaInvalid, "daemon-model-broker", "allowed model is invalid", nil)
			}
		}
	}
	return nil
}

func validateGrants(input CompileInput, now time.Time) (*ModelBrokerPlan, string, error) {
	if len(input.Grants) != len(input.Contract.Resources) ||
		len(input.Grants) != len(input.Run.CapabilityIDs) {
		return nil, "", deny(model.ErrorEventChain, input.Run.ID, "grants do not exactly cover contract resources", nil)
	}
	workspaceSeen, modelSeen := false, false
	decisionID := ""
	for index, grant := range input.Grants {
		resource := input.Contract.Resources[index]
		if grant.ID != input.Run.CapabilityIDs[index] ||
			grant.SubjectRunID != input.Run.ID ||
			!reflect.DeepEqual(grant.Resource, resource.Selector) ||
			!reflect.DeepEqual(grant.Operations, resource.Operations) ||
			!reflect.DeepEqual(grant.Conditions, resource.Conditions) ||
			grant.Issuer != daemonPrincipal() || grant.Delegable ||
			grant.DelegationParentID != "" ||
			!grant.IssuedAt.Equal(input.Run.CreatedAt) ||
			grant.MaxUses != nil {
			return nil, "", deny(model.ErrorEventChain, grant.ID, "grant does not match admission", nil)
		}
		if decisionID == "" {
			decisionID = grant.PolicyDecisionID
		} else if decisionID != grant.PolicyDecisionID {
			return nil, "", deny(model.ErrorEventChain, grant.ID, "grants do not share one decision", nil)
		}
		if grant.RevokedAt != nil {
			return nil, "", deny(model.ErrorCapabilityRevoked, grant.ID, "grant is revoked", nil)
		}
		if grant.IssuedAt.After(now) {
			return nil, "", deny(model.ErrorCapabilityDenied, grant.ID, "grant is not active yet", nil)
		}
		if !grant.ExpiresAt.After(now) {
			return nil, "", deny(model.ErrorCapabilityExpired, grant.ID, "grant is expired", nil)
		}
		if input.Run.Deadline != nil && grant.ExpiresAt.After(*input.Run.Deadline) {
			return nil, "", deny(model.ErrorEventChain, grant.ID, "grant exceeds run deadline", nil)
		}

		switch resource.Selector.Kind {
		case "filesystem":
			if workspaceSeen || resource.Selector.Pattern != "workspace/**" ||
				!exactOperations(resource.Operations, "filesystem.read", "filesystem.write") ||
				resource.Conditions.WorkspaceRoot != "workspace" ||
				unsupportedConditions(resource.Conditions, true) {
				return nil, "", deny(model.ErrorCapabilityDenied, resource.ID, "only full workspace read/write is enforceable", nil)
			}
			workspaceSeen = true
		case ModelResourceKind:
			if modelSeen || input.Ceilings.ModelBroker == nil {
				return nil, "", deny(model.ErrorCapabilityDenied, resource.ID, "model grant has no exact daemon broker", nil)
			}
			broker := input.Ceilings.ModelBroker
			if resource.Selector.Pattern != broker.Provider+"/*" ||
				!exactOperations(resource.Operations, ModelInvoke) ||
				unsupportedConditions(resource.Conditions, false) {
				return nil, "", deny(model.ErrorCapabilityDenied, resource.ID, "model grant is not the supported provider boundary", nil)
			}
			modelSeen = true
		default:
			return nil, "", deny(model.ErrorCapabilityDenied, resource.ID, "resource kind is not enforceable", nil)
		}
	}
	if !workspaceSeen {
		return nil, "", deny(model.ErrorCapabilityDenied, input.Contract.ID, "full workspace authority is required", nil)
	}
	hasModelBudgets := input.Contract.Budgets.MaxInputTokens != nil ||
		input.Contract.Budgets.MaxOutputTokens != nil ||
		input.Contract.Budgets.MaxModelCalls != nil
	if !modelSeen {
		if hasModelBudgets {
			return nil, "", deny(model.ErrorCapabilityDenied, input.Contract.ID, "model budgets require a model grant", nil)
		}
		return nil, NetworkNone, nil
	}
	broker := input.Ceilings.ModelBroker
	for _, limit := range []struct {
		contract *int64
		usage    int64
		daemon   int64
		name     string
	}{
		{input.Contract.Budgets.MaxModelCalls, input.Run.BudgetUsage.ModelCalls, broker.MaxModelCalls, "model calls"},
		{input.Contract.Budgets.MaxInputTokens, input.Run.BudgetUsage.InputTokens, broker.MaxInputTokens, "input tokens"},
		{input.Contract.Budgets.MaxOutputTokens, input.Run.BudgetUsage.OutputTokens, broker.MaxOutputTokens, "output tokens"},
	} {
		if limit.contract != nil {
			remaining := *limit.contract - limit.usage
			if remaining < 1 {
				return nil, "", deny(model.ErrorBudgetExceeded, input.Run.ID, limit.name+" budget is exhausted", nil)
			}
			if limit.daemon > remaining {
				return nil, "", deny(model.ErrorCapabilityDenied, input.Run.ID, "static broker exceeds "+limit.name+" budget", nil)
			}
		}
	}
	return &ModelBrokerPlan{
		Provider: broker.Provider, AllowedModels: append([]string(nil), broker.AllowedModels...),
		ImageDigest: broker.ImageDigest, PolicyDigest: broker.PolicyDigest,
		MaxModelCalls: broker.MaxModelCalls, MaxInputTokens: broker.MaxInputTokens,
		MaxOutputTokens:           broker.MaxOutputTokens,
		MaxOutputTokensPerRequest: broker.MaxOutputTokensPerRequest,
	}, NetworkModelBroker, nil
}

func timeoutBoundary(input CompileInput, now time.Time) (time.Time, int64, error) {
	notAfter := now.Add(time.Duration(input.Ceilings.MaxWallTimeSeconds) * time.Second)
	for _, deadline := range []*time.Time{input.Contract.Deadline, input.Run.Deadline} {
		if deadline != nil && deadline.Before(notAfter) {
			notAfter = deadline.UTC()
		}
	}
	for index := range input.Grants {
		if input.Grants[index].ExpiresAt.Before(notAfter) {
			notAfter = input.Grants[index].ExpiresAt.UTC()
		}
	}
	if wall := input.Contract.Budgets.MaxWallTimeSeconds; wall != nil {
		remaining := *wall - input.Run.BudgetUsage.WallTimeSeconds
		if remaining < 1 {
			return time.Time{}, 0, deny(model.ErrorBudgetExceeded, input.Run.ID, "wall-time budget is exhausted", nil)
		}
		if boundary := now.Add(time.Duration(remaining) * time.Second); boundary.Before(notAfter) {
			notAfter = boundary
		}
	}
	maximum := int64(notAfter.Sub(now) / time.Second)
	if maximum < 1 {
		return time.Time{}, 0, deny(model.ErrorCapabilityExpired, input.Run.ID, "authority expires before launch", nil)
	}
	if input.CallerTimeoutSeconds == nil {
		return notAfter, maximum, nil
	}
	if *input.CallerTimeoutSeconds < 1 {
		return time.Time{}, 0, deny(model.ErrorSchemaInvalid, input.Run.ID, "caller timeout must be positive", nil)
	}
	if *input.CallerTimeoutSeconds > maximum {
		return time.Time{}, 0, deny(model.ErrorCapabilityDenied, input.Run.ID, "caller timeout widens authority", nil)
	}
	return now.Add(time.Duration(*input.CallerTimeoutSeconds) * time.Second),
		*input.CallerTimeoutSeconds, nil
}

func commandDigest(command []string) (string, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return "", fmt.Errorf("command is required")
	}
	for _, argument := range command {
		if strings.ContainsRune(argument, 0) {
			return "", fmt.Errorf("command contains NUL")
		}
	}
	return transactionreducer.ComputeCommandDigest(command)
}

func unsupportedConditions(conditions model.CapabilityConditions, workspace bool) bool {
	return conditions.ApprovalRequired ||
		(!workspace && conditions.WorkspaceRoot != "") ||
		len(conditions.EnvironmentAllow) != 0 ||
		len(conditions.DataClassifications) != 0 ||
		conditions.MaxOutputBytes != nil
}

func exactOperations(actual []string, expected ...string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for _, operation := range expected {
		if !contains(actual, operation) {
			return false
		}
	}
	return true
}

func hasActiveExecution(executions []model.AgentExecution) bool {
	for _, execution := range executions {
		switch execution.Status {
		case model.AgentExecutionSucceeded,
			model.AgentExecutionFailed,
			model.AgentExecutionInterrupted,
			model.AgentExecutionStartFailed:
			continue
		default:
			return true
		}
	}
	return false
}

func sameTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func daemonPrincipal() model.Principal {
	return model.Principal{ID: "service:gatemoled", Kind: model.PrincipalService, Issuer: "gatemoled"}
}

func cloneModelPlan(plan *ModelBrokerPlan) *ModelBrokerPlan {
	if plan == nil {
		return nil
	}
	cloned := *plan
	cloned.AllowedModels = append([]string(nil), plan.AllowedModels...)
	return &cloned
}

func contains[T comparable](values []T, target T) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func deny(code model.ErrorCode, resource, message string, cause error) *model.KernelError {
	return &model.KernelError{
		Code: code, Operation: "compile_execution_authority",
		Resource: resource, Message: message, Cause: cause,
	}
}
