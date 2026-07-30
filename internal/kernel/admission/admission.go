// Package admission constructs and validates the daemon-authored authority
// envelope for one supervised agent task.
package admission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/capability"
	"github.com/duriantaco/vouch/internal/kernel/eventlog"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

const (
	LegacyRequestVersion = "vouch.task_admission_request.v0"
	RequestVersion       = "vouch.task_admission_request.v1"
	LegacyResultVersion  = "vouch.task_admission.v0"
	ResultVersion        = "vouch.task_admission.v1"
)

// ContractSpec contains only caller-owned policy inputs. The daemon derives
// the contract identity, owner, goal and content digest from the enclosing
// admission request.
type ContractSpec struct {
	Risk               string                        `json:"risk"`
	Resources          []model.ContractResource      `json:"resources"`
	Budgets            model.BudgetLimits            `json:"budgets"`
	Deadline           *time.Time                    `json:"deadline,omitempty"`
	MaxChildDepth      *int64                        `json:"max_child_depth,omitempty"`
	MaxChildren        *int64                        `json:"max_children,omitempty"`
	Checkpoints        []model.CheckpointRequirement `json:"checkpoints,omitempty"`
	Obligations        []model.ContractObligation    `json:"obligations,omitempty"`
	Escalation         *model.EscalationPolicy       `json:"escalation,omitempty"`
	ReleaseContractRef string                        `json:"release_contract_ref,omitempty"`
}

// Request is the non-authoritative task admission input accepted by vouchd.
// Digests, event envelopes, timestamps and lifecycle state are intentionally
// absent.
type Request struct {
	Version                    string                        `json:"version"`
	ExpectedRuntimeID          string                        `json:"expected_runtime_id,omitempty"`
	ExpectedEnforcementProfile string                        `json:"expected_enforcement_profile,omitempty"`
	IdempotencyKey             string                        `json:"idempotency_key"`
	TransactionID              string                        `json:"transaction_id"`
	RunID                      string                        `json:"run_id,omitempty"`
	Intent                     string                        `json:"intent"`
	AgentProfile               model.AgentTaskProfileBinding `json:"agent_profile"`
	Sponsor                    model.Principal               `json:"sponsor"`
	Actor                      model.Principal               `json:"actor"`
	Contract                   ContractSpec                  `json:"contract"`
}

// Result is the immutable representation returned for both the initial
// admission and an identical idempotent retry.
type Result struct {
	Version            string                        `json:"version"`
	RuntimeID          string                        `json:"runtime_id,omitempty"`
	EnforcementProfile string                        `json:"enforcement_profile,omitempty"`
	IdempotencyKey     string                        `json:"idempotency_key"`
	RequestDigest      string                        `json:"request_digest"`
	Task               model.AgentTask               `json:"task"`
	Contract           model.ExecutionContract       `json:"contract"`
	Run                reducer.Projection            `json:"run"`
	Grants             []model.CapabilityGrant       `json:"grants"`
	Transaction        transactionreducer.Projection `json:"transaction"`
}

// Prepared carries the immutable result plus the exact daemon-authored events
// that a Store must persist in one database transaction.
type Prepared struct {
	Namespace        string
	IdempotencyKey   string
	RequestDigest    string
	Result           Result
	RunEvents        []model.RunEvent
	TransactionEvent model.TransactionEvent
}

// ComputeRequestDigest binds the authenticated, caller-owned admission input
// without binding one short-lived OIDC token's claims digest. The principal
// identity and issuer remain material.
func ComputeRequestDigest(namespace string, request Request) (string, error) {
	if !model.IsIdentifier(namespace) {
		return "", admissionError(
			model.ErrorSchemaInvalid,
			"compute_admission_request_digest",
			namespace,
			"namespace is invalid",
			nil,
		)
	}
	normalized := request
	normalized.IdempotencyKey = ""
	normalized.Actor.ClaimsDigest = ""
	if normalized.Contract.Deadline != nil {
		deadline := normalized.Contract.Deadline.UTC()
		normalized.Contract.Deadline = &deadline
	}
	input := struct {
		Namespace string  `json:"namespace"`
		Request   Request `json:"request"`
	}{
		Namespace: namespace,
		Request:   normalized,
	}
	data, err := json.Marshal(input)
	if err != nil {
		return "", admissionError(
			model.ErrorInternal,
			"compute_admission_request_digest",
			request.TransactionID,
			"encode admission request",
			err,
		)
	}
	return digestBytes(data), nil
}

// Prepare creates the exact resources and event histories for a new task
// admission. Callers should check the idempotency index before invoking it so
// an old admission can still be replayed after its authority has expired.
func Prepare(namespace string, request Request, now time.Time) (Prepared, error) {
	switch request.Version {
	case LegacyRequestVersion:
		if request.ExpectedRuntimeID != "" ||
			request.ExpectedEnforcementProfile != "" {
			return Prepared{}, admissionError(
				model.ErrorSchemaInvalid,
				"prepare_task_admission",
				request.TransactionID,
				"v0 admission cannot carry Runtime metadata",
				nil,
			)
		}
	case RequestVersion:
		if !model.IsRuntimeID(request.ExpectedRuntimeID) ||
			!model.IsEnforcementProfile(
				request.ExpectedEnforcementProfile,
			) {
			return Prepared{}, admissionError(
				model.ErrorSchemaInvalid,
				"prepare_task_admission",
				request.ExpectedRuntimeID,
				"v1 Runtime metadata is invalid",
				nil,
			)
		}
	default:
		return Prepared{}, admissionError(
			model.ErrorSchemaInvalid,
			"prepare_task_admission",
			request.TransactionID,
			fmt.Sprintf(
				"version must be %q or %q",
				LegacyRequestVersion,
				RequestVersion,
			),
			nil,
		)
	}
	for field, value := range map[string]string{
		"idempotency_key": request.IdempotencyKey,
		"transaction_id":  request.TransactionID,
	} {
		if !model.IsIdentifier(value) {
			return Prepared{}, admissionError(
				model.ErrorSchemaInvalid,
				"prepare_task_admission",
				request.TransactionID,
				field+" is invalid",
				nil,
			)
		}
	}
	if request.RunID != "" && !model.IsIdentifier(request.RunID) {
		return Prepared{}, admissionError(
			model.ErrorSchemaInvalid,
			"prepare_task_admission",
			request.RunID,
			"run_id is invalid",
			nil,
		)
	}
	now = now.UTC()
	if now.IsZero() {
		return Prepared{}, admissionError(
			model.ErrorSchemaInvalid,
			"prepare_task_admission",
			request.TransactionID,
			"daemon clock is required",
			nil,
		)
	}
	requestDigest, err := ComputeRequestDigest(namespace, request)
	if err != nil {
		return Prepared{}, err
	}
	runID := request.RunID
	if runID == "" {
		runID = stableID("run", namespace, request.TransactionID)
	}
	taskID := stableID("task", namespace, request.TransactionID)
	contractID := stableID("contract", namespace, request.TransactionID)
	deadline := cloneTime(request.Contract.Deadline)
	if request.Contract.Budgets.MaxWallTimeSeconds != nil {
		const maxDurationSeconds = int64((1<<63 - 1) / int64(time.Second))
		seconds := *request.Contract.Budgets.MaxWallTimeSeconds
		if seconds < 1 || seconds > maxDurationSeconds {
			return Prepared{}, admissionError(
				model.ErrorSchemaInvalid,
				"prepare_task_admission",
				request.TransactionID,
				"max_wall_time_seconds must fit the supported positive duration range",
				nil,
			)
		}
		wallTimeDeadline := now.Add(
			time.Duration(seconds) * time.Second,
		)
		if deadline == nil || wallTimeDeadline.Before(*deadline) {
			deadline = &wallTimeDeadline
		}
	}

	contract := model.ExecutionContract{
		Version:            model.ExecutionContractVersion,
		ID:                 contractID,
		Owner:              request.Sponsor,
		Goal:               request.Intent,
		Risk:               request.Contract.Risk,
		Resources:          append([]model.ContractResource(nil), request.Contract.Resources...),
		Budgets:            request.Contract.Budgets,
		Deadline:           deadline,
		MaxChildDepth:      cloneInt64(request.Contract.MaxChildDepth),
		MaxChildren:        cloneInt64(request.Contract.MaxChildren),
		Checkpoints:        append([]model.CheckpointRequirement(nil), request.Contract.Checkpoints...),
		Obligations:        append([]model.ContractObligation(nil), request.Contract.Obligations...),
		Escalation:         cloneEscalation(request.Contract.Escalation),
		ReleaseContractRef: request.Contract.ReleaseContractRef,
	}
	contract.Digest, err = model.ComputeExecutionContractDigest(contract)
	if err != nil {
		return Prepared{}, err
	}
	if err := contract.Validate(); err != nil {
		return Prepared{}, err
	}

	task, err := model.NewAgentTask(
		taskID,
		request.TransactionID,
		namespace,
		runID,
		request.Intent,
		request.AgentProfile,
		now,
	)
	if err != nil {
		return Prepared{}, err
	}
	imageDigest := request.AgentProfile.ImageDigest
	if imageDigest == "" {
		// Host execution has no OCI image. Its command digest is the immutable
		// executable boundary understood by the current v0 AgentRun model.
		imageDigest = request.AgentProfile.CommandDigest
	}
	run := model.AgentRun{
		Version:                model.AgentRunVersion,
		ID:                     runID,
		Namespace:              namespace,
		ImageDigest:            imageDigest,
		ContractDigest:         contract.Digest,
		Principal:              model.Principal{ID: runID, Kind: model.PrincipalRun, Issuer: "vouchd"},
		DelegationChain:        []model.Principal{request.Sponsor},
		State:                  model.RunCreated,
		Deadline:               cloneTime(contract.Deadline),
		BudgetLimits:           contract.Budgets,
		BudgetUsage:            model.BudgetUsage{},
		Workspace:              admittedWorkspaceRoot(contract.Resources),
		CapabilityIDs:          []string{},
		OutstandingApprovalIDs: []string{},
		EventSequence:          1,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	if err := run.Validate(); err != nil {
		return Prepared{}, err
	}
	createdEvent, err := createRunEvent(run, request.Actor, now)
	if err != nil {
		return Prepared{}, err
	}
	runProjection, err := reducer.Apply(nil, createdEvent)
	if err != nil {
		return Prepared{}, err
	}
	grants, err := capability.Compile(contract, runProjection.Run, now)
	if err != nil {
		return Prepared{}, err
	}
	daemon := daemonPrincipal()
	grantsEvent, err := eventlog.Next(
		runProjection,
		reducer.EventCapabilitiesGranted,
		daemon,
		now,
		model.CapabilitiesGrantedPayload{
			ContractDigest: contract.Digest,
			Grants:         grants,
		},
	)
	if err != nil {
		return Prepared{}, err
	}
	grantsEvent.PolicyDecisionID = grants[0].PolicyDecisionID
	grantsEvent.Digest, err = model.ComputeEventDigest(grantsEvent)
	if err != nil {
		return Prepared{}, err
	}
	runProjection, err = reducer.Apply(&runProjection, grantsEvent)
	if err != nil {
		return Prepared{}, err
	}
	admittedEvent, err := eventlog.Next(
		runProjection,
		reducer.EventRunStateChanged,
		daemon,
		now,
		model.RunStateChangedPayload{
			From: model.RunCreated,
			To:   model.RunAdmitted,
		},
	)
	if err != nil {
		return Prepared{}, err
	}
	runProjection, err = reducer.Apply(&runProjection, admittedEvent)
	if err != nil {
		return Prepared{}, err
	}

	transaction := model.AgentTransaction{
		Version:      model.AgentTransactionVersion,
		ID:           request.TransactionID,
		Namespace:    namespace,
		IntentDigest: task.IntentDigest,
		Task:         &task,
		Admission: &model.TransactionAdmissionBinding{
			RuntimeID:          request.ExpectedRuntimeID,
			EnforcementProfile: request.ExpectedEnforcementProfile,
			TaskDigest:         task.Digest,
			RunID:              runID,
			ContractDigest:     contract.Digest,
		},
		Sponsor:                request.Sponsor,
		AgentRunIDs:            []string{runID},
		StageBindings:          []model.StageBinding{},
		State:                  model.TransactionCreated,
		EffectIDs:              []string{},
		VerificationResultIDs:  []string{},
		OutstandingApprovalIDs: []string{},
		EventSequence:          1,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	transactionEvent, err := transactionreducer.CreationEvent(transaction, request.Actor)
	if err != nil {
		return Prepared{}, err
	}
	transactionProjection, err := transactionreducer.Apply(nil, transactionEvent)
	if err != nil {
		return Prepared{}, err
	}
	resultVersion := ResultVersion
	if request.Version == LegacyRequestVersion {
		resultVersion = LegacyResultVersion
	}
	result := Result{
		Version:            resultVersion,
		RuntimeID:          request.ExpectedRuntimeID,
		EnforcementProfile: request.ExpectedEnforcementProfile,
		IdempotencyKey:     request.IdempotencyKey,
		RequestDigest:      requestDigest,
		Task:               task,
		Contract:           contract,
		Run:                runProjection,
		Grants:             grants,
		Transaction:        transactionProjection,
	}
	prepared := Prepared{
		Namespace:        namespace,
		IdempotencyKey:   request.IdempotencyKey,
		RequestDigest:    requestDigest,
		Result:           result,
		RunEvents:        []model.RunEvent{createdEvent, grantsEvent, admittedEvent},
		TransactionEvent: transactionEvent,
	}
	if err := prepared.Validate(); err != nil {
		return Prepared{}, err
	}
	return prepared, nil
}

// admittedWorkspaceRoot binds the run's logical workspace to the same unique
// root named by its compiled contract. Transaction execution materializes that
// logical root separately; low-level connector drivers use it to prevent a
// v1-admitted run from acting outside its declared workspace.
func admittedWorkspaceRoot(
	resources []model.ContractResource,
) string {
	root := ""
	for _, resource := range resources {
		candidate := resource.Conditions.WorkspaceRoot
		if candidate == "" {
			continue
		}
		if root != "" && root != candidate {
			return ""
		}
		root = candidate
	}
	return root
}

// Validate checks the immutable admission representation without requiring
// access to its event rows.
func (result Result) Validate(namespace string) error {
	if !model.IsIdentifier(namespace) ||
		!model.IsIdentifier(result.IdempotencyKey) ||
		!model.IsSHA256Digest(result.RequestDigest) {
		return admissionError(
			model.ErrorEventChain,
			"validate_task_admission",
			result.IdempotencyKey,
			"admission result envelope is invalid",
			nil,
		)
	}
	switch result.Version {
	case LegacyResultVersion:
		if result.RuntimeID != "" || result.EnforcementProfile != "" {
			return admissionError(
				model.ErrorEventChain,
				"validate_task_admission",
				result.RuntimeID,
				"v0 admission cannot carry Runtime metadata",
				nil,
			)
		}
	case ResultVersion:
		if !model.IsRuntimeID(result.RuntimeID) ||
			!model.IsEnforcementProfile(result.EnforcementProfile) {
			return admissionError(
				model.ErrorEventChain,
				"validate_task_admission",
				result.RuntimeID,
				"v1 admission Runtime metadata is invalid",
				nil,
			)
		}
	default:
		return admissionError(
			model.ErrorEventChain,
			"validate_task_admission",
			result.Version,
			"admission result version is invalid",
			nil,
		)
	}
	if err := result.Task.Validate(); err != nil {
		return err
	}
	if err := result.Contract.Validate(); err != nil {
		return err
	}
	contractDigest, err := model.ComputeExecutionContractDigest(result.Contract)
	if err != nil {
		return err
	}
	if contractDigest != result.Contract.Digest {
		return admissionError(
			model.ErrorEventChain,
			"validate_task_admission",
			result.Contract.ID,
			"contract digest does not match its content",
			nil,
		)
	}
	if err := result.Run.Run.Validate(); err != nil {
		return err
	}
	if err := result.Transaction.Validate(); err != nil {
		return err
	}
	run := result.Run.Run
	transaction := result.Transaction.Transaction
	binding := transaction.Admission
	if run.State != model.RunAdmitted ||
		run.EventSequence != 3 ||
		transaction.State != model.TransactionCreated ||
		transaction.EventSequence != 1 ||
		binding == nil ||
		binding.RuntimeID != result.RuntimeID ||
		binding.EnforcementProfile != result.EnforcementProfile ||
		result.Task.Namespace != namespace ||
		run.Namespace != namespace ||
		transaction.Namespace != namespace ||
		result.Task.TransactionID != transaction.ID ||
		result.Task.RunID != run.ID ||
		result.Task.IntentDigest != transaction.IntentDigest ||
		transaction.Task == nil ||
		!reflect.DeepEqual(*transaction.Task, result.Task) ||
		len(transaction.AgentRunIDs) != 1 ||
		transaction.AgentRunIDs[0] != run.ID ||
		binding.TaskDigest != result.Task.Digest ||
		binding.RunID != run.ID ||
		binding.ContractDigest != result.Contract.Digest ||
		run.ContractDigest != result.Contract.Digest {
		return admissionError(
			model.ErrorEventChain,
			"validate_task_admission",
			transaction.ID,
			"task, contract, run, and transaction bindings do not match",
			nil,
		)
	}
	if len(run.CapabilityIDs) != len(result.Grants) {
		return admissionError(
			model.ErrorEventChain,
			"validate_task_admission",
			run.ID,
			"run capability IDs do not match the admission grants",
			nil,
		)
	}
	for index, grant := range result.Grants {
		if err := grant.Validate(); err != nil {
			return err
		}
		if grant.SubjectRunID != run.ID ||
			run.CapabilityIDs[index] != grant.ID {
			return admissionError(
				model.ErrorEventChain,
				"validate_task_admission",
				grant.ID,
				"capability grant is not bound to the admitted run",
				nil,
			)
		}
	}
	return nil
}

// ValidateAgainstRequest verifies that an internally valid daemon result
// represents the caller-owned authority in request. RequestDigest alone is not
// sufficient for client correlation because authenticated middleware may add
// verified actor issuer/claims before hashing, while that actor metadata is not
// part of the returned task/contract projections.
func (result Result) ValidateAgainstRequest(
	namespace string,
	request Request,
) error {
	if err := result.Validate(namespace); err != nil {
		return err
	}
	expected, err := Prepare(
		namespace,
		request,
		result.Task.CreatedAt,
	)
	if err != nil {
		return admissionError(
			model.ErrorEventChain,
			"validate_task_admission_response",
			request.TransactionID,
			"reconstruct caller-owned admission authority",
			err,
		)
	}
	expectedResult := expected.Result
	if result.Version != expectedResult.Version ||
		result.RuntimeID != request.ExpectedRuntimeID ||
		result.EnforcementProfile !=
			request.ExpectedEnforcementProfile ||
		result.IdempotencyKey != request.IdempotencyKey ||
		!reflect.DeepEqual(result.Task, expectedResult.Task) ||
		!reflect.DeepEqual(result.Contract, expectedResult.Contract) ||
		!reflect.DeepEqual(result.Run.Run, expectedResult.Run.Run) ||
		!reflect.DeepEqual(result.Grants, expectedResult.Grants) ||
		!reflect.DeepEqual(
			result.Transaction.Transaction,
			expectedResult.Transaction.Transaction,
		) {
		return admissionError(
			model.ErrorEventChain,
			"validate_task_admission_response",
			request.TransactionID,
			"admission response does not match caller-owned request authority",
			nil,
		)
	}
	return nil
}

// Validate verifies every resource, digest, event chain and cross-resource
// binding before a Store is allowed to persist the aggregate.
func (prepared Prepared) Validate() error {
	if !model.IsIdentifier(prepared.Namespace) ||
		prepared.IdempotencyKey != prepared.Result.IdempotencyKey ||
		prepared.RequestDigest != prepared.Result.RequestDigest {
		return admissionError(
			model.ErrorSchemaInvalid,
			"validate_task_admission",
			prepared.IdempotencyKey,
			"prepared identity does not match its admission result",
			nil,
		)
	}
	result := prepared.Result
	if err := result.Validate(prepared.Namespace); err != nil {
		return err
	}
	if len(prepared.RunEvents) != 3 ||
		prepared.RunEvents[0].Type != reducer.EventRunCreated ||
		prepared.RunEvents[1].Type != reducer.EventCapabilitiesGranted ||
		prepared.RunEvents[2].Type != reducer.EventRunStateChanged {
		return admissionError(
			model.ErrorEventSequence,
			"validate_task_admission",
			result.Run.Run.ID,
			"admission requires exactly the created, grants, and admitted run events",
			nil,
		)
	}
	for _, event := range prepared.RunEvents {
		valid, err := model.VerifyEventDigest(event)
		if err != nil {
			return admissionError(
				model.ErrorInternal,
				"validate_task_admission",
				event.ID,
				"compute run event digest",
				err,
			)
		}
		if !valid {
			return admissionError(
				model.ErrorEventChain,
				"validate_task_admission",
				event.ID,
				"run event digest does not match its content",
				nil,
			)
		}
	}
	replayedRun, err := reducer.Replay(prepared.RunEvents)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(replayedRun, result.Run) {
		return admissionError(
			model.ErrorEventChain,
			"validate_task_admission",
			result.Run.Run.ID,
			"run projection does not match event replay",
			nil,
		)
	}
	grantMap, _, err := capability.GrantsFromEvents(prepared.RunEvents)
	if err != nil {
		return err
	}
	if len(grantMap) != len(result.Grants) ||
		len(result.Run.Run.CapabilityIDs) != len(result.Grants) {
		return admissionError(
			model.ErrorEventChain,
			"validate_task_admission",
			result.Run.Run.ID,
			"capability projections do not match the grant event",
			nil,
		)
	}
	for index, grant := range result.Grants {
		stored, exists := grantMap[grant.ID]
		if !exists || !reflect.DeepEqual(stored, grant) ||
			result.Run.Run.CapabilityIDs[index] != grant.ID {
			return admissionError(
				model.ErrorEventChain,
				"validate_task_admission",
				grant.ID,
				"capability grant is not bound to the admitted run",
				nil,
			)
		}
	}
	replayedTransaction, err := transactionreducer.Replay(
		[]model.TransactionEvent{prepared.TransactionEvent},
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(replayedTransaction, result.Transaction) {
		return admissionError(
			model.ErrorEventChain,
			"validate_task_admission",
			result.Transaction.Transaction.ID,
			"transaction projection does not match event replay",
			nil,
		)
	}
	return nil
}

func createRunEvent(
	run model.AgentRun,
	actor model.Principal,
	occurredAt time.Time,
) (model.RunEvent, error) {
	payload, err := json.Marshal(model.RunCreatedPayload{Run: run})
	if err != nil {
		return model.RunEvent{}, admissionError(
			model.ErrorInternal,
			"prepare_task_admission",
			run.ID,
			"encode run creation",
			err,
		)
	}
	event := model.RunEvent{
		Version:    model.RunEventVersion,
		ID:         eventlog.ID(run.ID, 1),
		RunID:      run.ID,
		Sequence:   1,
		Type:       reducer.EventRunCreated,
		Actor:      actor,
		OccurredAt: occurredAt.UTC(),
		Payload:    payload,
	}
	event.Digest, err = model.ComputeEventDigest(event)
	if err != nil {
		return model.RunEvent{}, admissionError(
			model.ErrorInternal,
			"prepare_task_admission",
			run.ID,
			"digest run creation",
			err,
		)
	}
	return event, nil
}

func daemonPrincipal() model.Principal {
	return model.Principal{
		ID:     "service:vouchd",
		Kind:   model.PrincipalService,
		Issuer: "vouchd",
	}
}

func stableID(prefix string, values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return prefix + ":" + hex.EncodeToString(hash.Sum(nil)[:16])
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneEscalation(value *model.EscalationPolicy) *model.EscalationPolicy {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.TimeoutSeconds = cloneInt64(value.TimeoutSeconds)
	return &cloned
}

func admissionError(
	code model.ErrorCode,
	operation, resource, message string,
	cause error,
) *model.KernelError {
	return &model.KernelError{
		Code:      code,
		Operation: operation,
		Resource:  resource,
		Message:   strings.TrimSpace(message),
		Cause:     cause,
	}
}
