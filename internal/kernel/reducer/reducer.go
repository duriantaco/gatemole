package reducer

import (
	"fmt"
	"math"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

const (
	EventRunCreated             = "run.created"
	EventRunStateChanged        = "run.state_changed"
	EventRunExecutionStarted    = "run.execution_started"
	EventRunExecutionFinished   = "run.execution_finished"
	EventCapabilitiesGranted    = "capabilities.granted"
	EventActionRequested        = "action.requested"
	EventActionDenied           = "action.denied"
	EventActionApprovalRequired = "action.approval_required"
	EventActionAuthorized       = "action.authorized"
	EventActionExecuting        = "action.executing"
	EventActionCommitted        = "action.committed"
	EventActionFailed           = "action.failed"
	EventActionUnknown          = "action.unknown"
)

type Projection struct {
	Run             model.AgentRun `json:"run"`
	LastEventDigest string         `json:"last_event_digest"`
}

var allowedTransitions = map[model.RunState]map[model.RunState]struct{}{
	model.RunCreated: states(
		model.RunAdmitted,
		model.RunFailed,
		model.RunCancelled,
	),
	model.RunAdmitted: states(
		model.RunRunning,
		model.RunBlocked,
		model.RunFailed,
		model.RunCancelled,
	),
	model.RunRunning: states(
		model.RunWaitingForEvent,
		model.RunWaitingForAgent,
		model.RunWaitingForApproval,
		model.RunBlocked,
		model.RunFailed,
		model.RunCancelled,
		model.RunCompleted,
	),
	model.RunWaitingForEvent: states(
		model.RunRunning,
		model.RunBlocked,
		model.RunFailed,
		model.RunCancelled,
	),
	model.RunWaitingForAgent: states(
		model.RunRunning,
		model.RunBlocked,
		model.RunFailed,
		model.RunCancelled,
	),
	model.RunWaitingForApproval: states(
		model.RunRunning,
		model.RunBlocked,
		model.RunFailed,
		model.RunCancelled,
	),
	model.RunBlocked: states(
		model.RunAdmitted,
		model.RunRunning,
		model.RunFailed,
		model.RunCancelled,
	),
	model.RunFailed:    states(),
	model.RunCancelled: states(),
	model.RunCompleted: states(),
}

func CanTransition(from, to model.RunState) bool {
	targets, exists := allowedTransitions[from]
	if !exists {
		return false
	}
	_, exists = targets[to]
	return exists
}

func ValidateTransition(from, to model.RunState, reason string) error {
	if !CanTransition(from, to) {
		return transitionError(fmt.Sprintf("transition from %q to %q is not allowed", from, to))
	}
	if requiresReason(to) && reason == "" {
		return transitionError(fmt.Sprintf("transition to %q requires a reason", to))
	}
	return nil
}

func Replay(events []model.RunEvent) (Projection, error) {
	var current *Projection
	for i := range events {
		next, err := Apply(current, events[i])
		if err != nil {
			return Projection{}, fmt.Errorf("replay event %d (%s): %w", i, events[i].ID, err)
		}
		current = &next
	}
	if current == nil {
		return Projection{}, &model.KernelError{
			Code:      model.ErrorEventSequence,
			Operation: "replay",
			Message:   "at least one event is required",
		}
	}
	return *current, nil
}

func Apply(current *Projection, event model.RunEvent) (Projection, error) {
	if err := event.Validate(); err != nil {
		return Projection{}, err
	}
	if current == nil {
		return applyCreation(event)
	}
	if event.RunID != current.Run.ID {
		return Projection{}, &model.KernelError{
			Code:      model.ErrorEventSequence,
			Operation: "reduce",
			Resource:  event.ID,
			Field:     "run_id",
			Message:   fmt.Sprintf("event run %q does not match projection run %q", event.RunID, current.Run.ID),
		}
	}
	wantSequence := current.Run.EventSequence + 1
	if event.Sequence != wantSequence {
		return Projection{}, &model.KernelError{
			Code:      model.ErrorEventSequence,
			Operation: "reduce",
			Resource:  event.ID,
			Field:     "sequence",
			Message:   fmt.Sprintf("expected sequence %d, got %d", wantSequence, event.Sequence),
		}
	}
	if event.PreviousDigest != current.LastEventDigest {
		return Projection{}, &model.KernelError{
			Code:      model.ErrorEventChain,
			Operation: "reduce",
			Resource:  event.ID,
			Field:     "previous_digest",
			Message:   "previous digest does not match the accepted event chain",
		}
	}
	if event.OccurredAt.Before(current.Run.UpdatedAt) {
		return Projection{}, &model.KernelError{
			Code:      model.ErrorSchemaInvalid,
			Operation: "reduce",
			Resource:  event.ID,
			Field:     "occurred_at",
			Message:   "event timestamp cannot precede the current run timestamp",
		}
	}
	if event.Type == EventRunCreated {
		return Projection{}, transitionError("run.created can only be the first event")
	}

	run := current.Run
	switch event.Type {
	case EventRunStateChanged:
		payload, err := model.DecodePayloadStrict[model.RunStateChangedPayload](event)
		if err != nil {
			return Projection{}, err
		}
		if payload.From != run.State {
			return Projection{}, transitionError(fmt.Sprintf(
				"transition payload starts at %q but current state is %q",
				payload.From,
				run.State,
			))
		}
		if err := ValidateTransition(payload.From, payload.To, payload.Reason); err != nil {
			return Projection{}, err
		}
		if run.ActiveExecutionID != "" && payload.To != model.RunRunning {
			return Projection{}, transitionError("an active execution must settle before the run can leave running state")
		}
		run.State = payload.To
		run.StateReason = payload.Reason
		if payload.To.Terminal() {
			completedAt := event.OccurredAt
			run.CompletedAt = &completedAt
		} else {
			run.CompletedAt = nil
		}
	case EventRunExecutionStarted:
		payload, err := model.DecodePayloadStrict[model.RunExecutionStartedPayload](event)
		if err != nil {
			return Projection{}, err
		}
		if err := applyExecutionStarted(&run, payload); err != nil {
			return Projection{}, err
		}
	case EventRunExecutionFinished:
		payload, err := model.DecodePayloadStrict[model.RunExecutionFinishedPayload](event)
		if err != nil {
			return Projection{}, err
		}
		if err := applyExecutionFinished(&run, payload); err != nil {
			return Projection{}, err
		}
	case EventCapabilitiesGranted:
		payload, err := model.DecodePayloadStrict[model.CapabilitiesGrantedPayload](event)
		if err != nil {
			return Projection{}, err
		}
		if payload.ContractDigest != run.ContractDigest {
			return Projection{}, transitionError("capability contract digest does not match the run")
		}
		if len(payload.Grants) == 0 {
			return Projection{}, transitionError("capabilities.granted requires at least one grant")
		}
		seen := make(map[string]struct{}, len(run.CapabilityIDs)+len(payload.Grants))
		for _, id := range run.CapabilityIDs {
			seen[id] = struct{}{}
		}
		for _, grant := range payload.Grants {
			if err := grant.Validate(); err != nil {
				return Projection{}, err
			}
			if grant.SubjectRunID != run.ID {
				return Projection{}, transitionError("capability subject does not match the run")
			}
			if _, exists := seen[grant.ID]; exists {
				return Projection{}, transitionError("capability ID is already installed on the run")
			}
			seen[grant.ID] = struct{}{}
			run.CapabilityIDs = append(run.CapabilityIDs, grant.ID)
		}
	case EventActionRequested:
		payload, err := model.DecodePayloadStrict[model.ActionRequestedPayload](event)
		if err != nil {
			return Projection{}, err
		}
		if err := payload.Request.Validate(); err != nil {
			return Projection{}, err
		}
		if payload.Request.RunID != run.ID {
			return Projection{}, transitionError("action request does not belong to the run")
		}
		if run.State != model.RunRunning {
			return Projection{}, transitionError("actions may only be requested while a run is running")
		}
	case EventActionDenied, EventActionApprovalRequired, EventActionAuthorized:
		payload, err := model.DecodePayloadStrict[model.ActionDecisionPayload](event)
		if err != nil {
			return Projection{}, err
		}
		if payload.ActionID == "" || payload.Reason == "" {
			return Projection{}, transitionError("action decisions require action_id and reason")
		}
		if event.Type == EventActionDenied && payload.Decision != model.DecisionDeny {
			return Projection{}, transitionError("action.denied requires a deny decision")
		}
		if event.Type == EventActionApprovalRequired && payload.Decision != model.DecisionRequireApproval {
			return Projection{}, transitionError("action.approval_required requires a require_approval decision")
		}
		if event.Type == EventActionAuthorized && payload.Decision != model.DecisionAllow && payload.Decision != model.DecisionConstrain {
			return Projection{}, transitionError("action.authorized requires an allow or constrain decision")
		}
	case EventActionExecuting:
		payload, err := model.DecodePayloadStrict[model.ActionExecutingPayload](event)
		if err != nil {
			return Projection{}, err
		}
		if payload.ActionID == "" || payload.CapabilityID == "" {
			return Projection{}, transitionError("action.executing requires action_id and capability_id")
		}
	case EventActionCommitted, EventActionFailed, EventActionUnknown:
		payload, err := model.DecodePayloadStrict[model.ActionResultPayload](event)
		if err != nil {
			return Projection{}, err
		}
		wantStatus := map[string]string{
			EventActionCommitted: "committed",
			EventActionFailed:    "failed",
			EventActionUnknown:   "unknown",
		}[event.Type]
		if payload.ActionID == "" || payload.CapabilityID == "" || payload.Status != wantStatus {
			return Projection{}, transitionError(fmt.Sprintf("%s payload is invalid", event.Type))
		}
	default:
		return Projection{}, transitionError(fmt.Sprintf("unsupported event type %q", event.Type))
	}
	run.EventSequence = event.Sequence
	run.UpdatedAt = event.OccurredAt
	if err := run.Validate(); err != nil {
		return Projection{}, err
	}
	return Projection{Run: run, LastEventDigest: event.Digest}, nil
}

func applyExecutionStarted(run *model.AgentRun, payload model.RunExecutionStartedPayload) error {
	if !model.IsIdentifier(payload.TransactionID) ||
		!model.IsIdentifier(payload.ExecutionID) || payload.Attempt < 1 {
		return transitionError("run execution start binding is invalid")
	}
	if run.ActiveExecutionID != "" {
		return transitionError("run already has an active execution")
	}
	switch run.State {
	case model.RunAdmitted, model.RunWaitingForAgent, model.RunWaitingForEvent:
	default:
		return transitionError(fmt.Sprintf("execution cannot start while run is %q", run.State))
	}
	if err := ValidateTransition(run.State, model.RunRunning, ""); err != nil {
		return err
	}
	run.State = model.RunRunning
	run.StateReason = ""
	run.ActiveExecutionID = payload.ExecutionID
	run.CompletedAt = nil
	return nil
}

func applyExecutionFinished(run *model.AgentRun, payload model.RunExecutionFinishedPayload) error {
	if !model.IsIdentifier(payload.TransactionID) ||
		!model.IsIdentifier(payload.ExecutionID) || payload.Attempt < 1 ||
		!payload.Status.Terminal() {
		return transitionError("run execution settlement binding is invalid")
	}
	if run.State != model.RunRunning || run.ActiveExecutionID != payload.ExecutionID {
		return transitionError("run execution settlement does not match the active execution")
	}
	usage, err := addBudgetUsage(run.BudgetUsage, payload.Usage)
	if err != nil {
		return err
	}
	run.BudgetUsage = usage
	run.ActiveExecutionID = ""
	run.StateReason = ""
	if payload.Status == model.AgentExecutionSucceeded {
		run.State = model.RunWaitingForEvent
	} else {
		run.State = model.RunWaitingForAgent
	}
	return nil
}

func addBudgetUsage(current, delta model.BudgetUsage) (model.BudgetUsage, error) {
	values := []struct {
		name    string
		current *int64
		delta   int64
	}{
		{"input_tokens", &current.InputTokens, delta.InputTokens},
		{"output_tokens", &current.OutputTokens, delta.OutputTokens},
		{"model_calls", &current.ModelCalls, delta.ModelCalls},
		{"tool_calls", &current.ToolCalls, delta.ToolCalls},
		{"cost_micros", &current.CostMicros, delta.CostMicros},
		{"wall_time_seconds", &current.WallTimeSeconds, delta.WallTimeSeconds},
	}
	for _, value := range values {
		if value.delta < 0 || value.delta > math.MaxInt64-*value.current {
			return model.BudgetUsage{}, &model.KernelError{
				Code:      model.ErrorBudgetExceeded,
				Operation: "reduce",
				Field:     "budget_usage." + value.name,
				Message:   "execution budget charge is negative or overflows",
			}
		}
		*value.current += value.delta
	}
	return current, nil
}

func applyCreation(event model.RunEvent) (Projection, error) {
	if event.Sequence != 1 {
		return Projection{}, &model.KernelError{
			Code:      model.ErrorEventSequence,
			Operation: "reduce",
			Resource:  event.ID,
			Field:     "sequence",
			Message:   "the first event must have sequence 1",
		}
	}
	if event.Type != EventRunCreated {
		return Projection{}, transitionError("the first event must be run.created")
	}
	payload, err := model.DecodePayloadStrict[model.RunCreatedPayload](event)
	if err != nil {
		return Projection{}, err
	}
	run := payload.Run
	if err := run.Validate(); err != nil {
		return Projection{}, err
	}
	if run.ID != event.RunID {
		return Projection{}, &model.KernelError{
			Code:      model.ErrorEventSequence,
			Operation: "reduce",
			Resource:  event.ID,
			Field:     "run_id",
			Message:   "created run ID does not match event run ID",
		}
	}
	if run.State != model.RunCreated {
		return Projection{}, transitionError("run.created payload must begin in created state")
	}
	if run.EventSequence != 1 {
		return Projection{}, &model.KernelError{
			Code:      model.ErrorEventSequence,
			Operation: "reduce",
			Resource:  event.ID,
			Field:     "payload.run.event_sequence",
			Message:   "created run event sequence must be 1",
		}
	}
	if !run.CreatedAt.Equal(event.OccurredAt) || !run.UpdatedAt.Equal(event.OccurredAt) {
		return Projection{}, &model.KernelError{
			Code:      model.ErrorSchemaInvalid,
			Operation: "reduce",
			Resource:  event.ID,
			Field:     "occurred_at",
			Message:   "run.created timestamp must match created_at and updated_at",
		}
	}
	return Projection{Run: run, LastEventDigest: event.Digest}, nil
}

func states(values ...model.RunState) map[model.RunState]struct{} {
	result := make(map[model.RunState]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func requiresReason(state model.RunState) bool {
	return state == model.RunBlocked || state == model.RunFailed || state == model.RunCancelled
}

func transitionError(message string) *model.KernelError {
	return &model.KernelError{
		Code:      model.ErrorTransitionInvalid,
		Operation: "transition",
		Message:   message,
	}
}
