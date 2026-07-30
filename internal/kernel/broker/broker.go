package broker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/capability"
	"github.com/duriantaco/gatemole/internal/kernel/driver"
	"github.com/duriantaco/gatemole/internal/kernel/eventlog"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/reducer"
	"github.com/duriantaco/gatemole/internal/kernel/store"
)

type ExecuteRequest struct {
	ExpectedSequence int64               `json:"expected_sequence"`
	Action           model.ActionRequest `json:"action"`
	InputBase64      string              `json:"input_base64,omitempty"`
}

type Outcome struct {
	ActionID     string             `json:"action_id"`
	Status       string             `json:"status"`
	CapabilityID string             `json:"capability_id,omitempty"`
	ResultDigest string             `json:"result_digest,omitempty"`
	OutputBytes  int64              `json:"output_bytes,omitempty"`
	OutputBase64 string             `json:"output_base64,omitempty"`
	Projection   reducer.Projection `json:"projection"`
}

type Broker struct {
	store      store.Store
	filesystem *driver.Filesystem
	now        func() time.Time
}

type Option func(*Broker)

func WithClock(clock func() time.Time) Option {
	return func(broker *Broker) { broker.now = clock }
}

func New(kernelStore store.Store, repositoryRoot string, options ...Option) (*Broker, error) {
	filesystem, err := driver.NewFilesystem(repositoryRoot)
	if err != nil {
		return nil, err
	}
	actionBroker := &Broker{store: kernelStore, filesystem: filesystem, now: time.Now}
	for _, option := range options {
		option(actionBroker)
	}
	return actionBroker, nil
}

// Recover closes action histories interrupted by a daemon crash. Requests that
// never reached authorization are denied; anything authorized or executing is
// marked unknown because an effect may have occurred. Recovery never retries.
func (broker *Broker) Recover(ctx context.Context) (int, error) {
	projections, err := broker.store.ListAllRuns(ctx)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, projection := range projections {
		if projection.Run.State.Terminal() {
			continue
		}
		events, err := broker.store.Events(ctx, projection.Run.Namespace, projection.Run.ID, 0)
		if err != nil {
			return recovered, err
		}
		pending, order, err := interruptedActions(events)
		if err != nil {
			return recovered, err
		}
		for _, actionID := range order {
			action, exists := pending[actionID]
			if !exists {
				continue
			}
			if action.CapabilityID == "" {
				payload := model.ActionDecisionPayload{
					ActionID:  actionID,
					Decision:  model.DecisionDeny,
					Reason:    "daemon restarted before authorization completed",
					ErrorCode: model.ErrorDriverUnavailable,
				}
				projection, err = broker.append(ctx, projection.Run.Namespace, projection, reducer.EventActionDenied, servicePrincipal(), payload, "")
			} else {
				payload := model.ActionResultPayload{
					ActionID:     actionID,
					CapabilityID: action.CapabilityID,
					Status:       "unknown",
					ErrorCode:    model.ErrorActionUnknown,
					Reason:       "daemon restarted after authorization; effect requires reconciliation",
				}
				projection, err = broker.append(ctx, projection.Run.Namespace, projection, reducer.EventActionUnknown, servicePrincipal(), payload, "")
			}
			if err != nil {
				return recovered, err
			}
			recovered++
		}
	}
	return recovered, nil
}

func (broker *Broker) Execute(ctx context.Context, namespace string, input ExecuteRequest) (Outcome, error) {
	request := input.Action
	if err := request.Validate(); err != nil {
		return Outcome{ActionID: request.ID}, err
	}
	projection, err := broker.store.GetRun(ctx, namespace, request.RunID)
	if err != nil {
		return Outcome{ActionID: request.ID}, err
	}
	if projection.Run.EventSequence != input.ExpectedSequence {
		return Outcome{ActionID: request.ID, Projection: projection}, &model.KernelError{
			Code: model.ErrorConflict, Operation: "request_action", Resource: request.ID,
			Message: fmt.Sprintf("expected run sequence %d, current sequence is %d", input.ExpectedSequence, projection.Run.EventSequence),
		}
	}
	if projection.Run.State != model.RunRunning {
		return Outcome{ActionID: request.ID, Projection: projection}, &model.KernelError{
			Code: model.ErrorTransitionInvalid, Operation: "request_action", Resource: request.ID,
			Message: "actions may only execute while the run is running",
		}
	}
	events, err := broker.store.Events(ctx, namespace, request.RunID, 0)
	if err != nil {
		return Outcome{ActionID: request.ID, Projection: projection}, err
	}
	if err := ensureUniqueAttempt(events, request); err != nil {
		return Outcome{ActionID: request.ID, Projection: projection}, err
	}
	grants, uses, err := capability.GrantsFromEvents(events)
	if err != nil {
		return Outcome{ActionID: request.ID, Projection: projection}, err
	}

	projection, err = broker.append(ctx, namespace, projection, reducer.EventActionRequested, projection.Run.Principal,
		model.ActionRequestedPayload{Request: request}, "")
	if err != nil {
		return Outcome{ActionID: request.ID, Projection: projection}, err
	}
	grant, authErr := authorize(request, grants, uses, broker.now().UTC())
	if authErr != nil {
		decision := model.DecisionDeny
		eventType := reducer.EventActionDenied
		if hasCode(authErr, model.ErrorApprovalRequired) {
			decision = model.DecisionRequireApproval
			eventType = reducer.EventActionApprovalRequired
		}
		var kernelErr *model.KernelError
		_ = errors.As(authErr, &kernelErr)
		payload := model.ActionDecisionPayload{
			ActionID: request.ID,
			Decision: decision,
			Reason:   authErr.Error(),
		}
		if kernelErr != nil {
			payload.ErrorCode = kernelErr.Code
			payload.CapabilityID = kernelErr.Resource
		}
		projection, appendErr := broker.append(ctx, namespace, projection, eventType, servicePrincipal(), payload, "")
		if appendErr != nil {
			return Outcome{ActionID: request.ID, Status: "unknown", Projection: projection}, appendErr
		}
		status := "denied"
		if decision == model.DecisionRequireApproval {
			status = "approval_required"
		}
		return Outcome{ActionID: request.ID, Status: status, CapabilityID: payload.CapabilityID, Projection: projection}, authErr
	}

	authorized := model.ActionDecisionPayload{
		ActionID:     request.ID,
		Decision:     model.DecisionAllow,
		CapabilityID: grant.ID,
		Reason:       "request matched an active capability grant",
	}
	projection, err = broker.append(ctx, namespace, projection, reducer.EventActionAuthorized, servicePrincipal(), authorized, grant.PolicyDecisionID)
	if err != nil {
		return Outcome{ActionID: request.ID, Status: "unknown", CapabilityID: grant.ID, Projection: projection}, err
	}
	projection, err = broker.append(ctx, namespace, projection, reducer.EventActionExecuting, servicePrincipal(),
		model.ActionExecutingPayload{ActionID: request.ID, CapabilityID: grant.ID}, grant.PolicyDecisionID)
	if err != nil {
		return Outcome{ActionID: request.ID, Status: "unknown", CapabilityID: grant.ID, Projection: projection}, err
	}

	inputBytes, err := decodeInput(input.InputBase64)
	if err != nil {
		return broker.recordFailure(ctx, namespace, projection, grant, request, err, false)
	}
	result, executionErr := broker.filesystem.Execute(projection.Run, grant, request, inputBytes)
	if executionErr != nil {
		var effectErr *driver.ExecutionError
		unknown := errors.As(executionErr, &effectErr) && effectErr.Unknown
		return broker.recordFailure(ctx, namespace, projection, grant, request, executionErr, unknown)
	}
	payload := model.ActionResultPayload{
		ActionID:     request.ID,
		CapabilityID: grant.ID,
		Status:       "committed",
		ResultDigest: result.Digest,
		OutputBytes:  result.Bytes,
	}
	projection, err = broker.append(ctx, namespace, projection, reducer.EventActionCommitted, servicePrincipal(), payload, grant.PolicyDecisionID)
	if err != nil {
		// The effect completed but its receipt did not. Never claim failure and
		// never retry automatically; the durable executing event is ambiguous.
		return Outcome{ActionID: request.ID, Status: "unknown", CapabilityID: grant.ID, ResultDigest: result.Digest, OutputBytes: result.Bytes, Projection: projection}, &model.KernelError{
			Code: model.ErrorActionUnknown, Operation: "commit_action_receipt", Resource: request.ID,
			Message: "effect completed but its receipt could not be committed",
			Cause:   err,
		}
	}
	return Outcome{
		ActionID:     request.ID,
		Status:       "committed",
		CapabilityID: grant.ID,
		ResultDigest: result.Digest,
		OutputBytes:  result.Bytes,
		OutputBase64: base64.StdEncoding.EncodeToString(result.Output),
		Projection:   projection,
	}, nil
}

func (broker *Broker) recordFailure(
	ctx context.Context,
	namespace string,
	projection reducer.Projection,
	grant model.CapabilityGrant,
	request model.ActionRequest,
	cause error,
	unknown bool,
) (Outcome, error) {
	status := "failed"
	eventType := reducer.EventActionFailed
	errorCode := model.ErrorDriverUnavailable
	if unknown {
		status = "unknown"
		eventType = reducer.EventActionUnknown
		errorCode = model.ErrorActionUnknown
	}
	payload := model.ActionResultPayload{
		ActionID:     request.ID,
		CapabilityID: grant.ID,
		Status:       status,
		ErrorCode:    errorCode,
		Reason:       cause.Error(),
	}
	next, err := broker.append(ctx, namespace, projection, eventType, servicePrincipal(), payload, grant.PolicyDecisionID)
	if err != nil {
		return Outcome{ActionID: request.ID, Status: "unknown", CapabilityID: grant.ID, Projection: projection}, &model.KernelError{
			Code: model.ErrorActionUnknown, Operation: "record_action_failure", Resource: request.ID,
			Message: "action outcome could not be durably recorded",
			Cause:   err,
		}
	}
	return Outcome{ActionID: request.ID, Status: status, CapabilityID: grant.ID, Projection: next}, nil
}

func (broker *Broker) append(
	ctx context.Context,
	namespace string,
	projection reducer.Projection,
	eventType string,
	actor model.Principal,
	payload any,
	policyDecisionID string,
) (reducer.Projection, error) {
	event, err := eventlog.Next(projection, eventType, actor, broker.now().UTC(), payload)
	if err != nil {
		return projection, err
	}
	if policyDecisionID != "" {
		event.PolicyDecisionID = policyDecisionID
		event.Digest, err = model.ComputeEventDigest(event)
		if err != nil {
			return projection, err
		}
	}
	return broker.store.AppendEvent(ctx, namespace, projection.Run.EventSequence, event)
}

func authorize(
	request model.ActionRequest,
	grants map[string]model.CapabilityGrant,
	uses map[string]int64,
	now time.Time,
) (model.CapabilityGrant, error) {
	candidates := make([]model.CapabilityGrant, 0, len(grants))
	if request.CapabilityHint != "" {
		grant, exists := grants[request.CapabilityHint]
		if !exists {
			return model.CapabilityGrant{}, capabilityError(model.ErrorCapabilityDenied, request.CapabilityHint, "hinted capability is not installed")
		}
		candidates = append(candidates, grant)
	} else {
		for _, grant := range grants {
			candidates = append(candidates, grant)
		}
		slices.SortFunc(candidates, func(a, b model.CapabilityGrant) int { return stringsCompare(a.ID, b.ID) })
	}
	for _, grant := range candidates {
		if grant.SubjectRunID != request.RunID || grant.Resource.Kind != request.Resource.Kind ||
			!slices.Contains(grant.Operations, request.Operation) ||
			!capability.MatchResource(grant.Resource.Pattern, request.Resource.Pattern) {
			continue
		}
		if grant.RevokedAt != nil && !grant.RevokedAt.After(now) {
			return model.CapabilityGrant{}, capabilityError(model.ErrorCapabilityRevoked, grant.ID, "capability is revoked")
		}
		if !grant.ExpiresAt.After(now) {
			return model.CapabilityGrant{}, capabilityError(model.ErrorCapabilityExpired, grant.ID, "capability is expired")
		}
		if grant.MaxUses != nil && uses[grant.ID] >= *grant.MaxUses {
			return model.CapabilityGrant{}, capabilityError(model.ErrorCapabilityDenied, grant.ID, "capability use limit is exhausted")
		}
		if grant.Conditions.ApprovalRequired {
			return model.CapabilityGrant{}, capabilityError(model.ErrorApprovalRequired, grant.ID, "capability requires human approval")
		}
		return grant, nil
	}
	return model.CapabilityGrant{}, capabilityError(model.ErrorCapabilityDenied, "", "no capability authorizes the requested resource and operation")
}

func ensureUniqueAttempt(events []model.RunEvent, request model.ActionRequest) error {
	for _, event := range events {
		if event.Type != reducer.EventActionRequested {
			continue
		}
		payload, err := model.DecodePayloadStrict[model.ActionRequestedPayload](event)
		if err != nil {
			return err
		}
		if payload.Request.ID == request.ID || payload.Request.IdempotencyKey == request.IdempotencyKey {
			return &model.KernelError{
				Code: model.ErrorConflict, Operation: "request_action", Resource: request.ID,
				Message: "action ID or idempotency key already exists for this run",
			}
		}
	}
	return nil
}

type pendingAction struct {
	CapabilityID string
}

func interruptedActions(events []model.RunEvent) (map[string]pendingAction, []string, error) {
	pending := map[string]pendingAction{}
	order := []string{}
	for _, event := range events {
		switch event.Type {
		case reducer.EventActionRequested:
			payload, err := model.DecodePayloadStrict[model.ActionRequestedPayload](event)
			if err != nil {
				return nil, nil, err
			}
			pending[payload.Request.ID] = pendingAction{}
			order = append(order, payload.Request.ID)
		case reducer.EventActionAuthorized:
			payload, err := model.DecodePayloadStrict[model.ActionDecisionPayload](event)
			if err != nil {
				return nil, nil, err
			}
			if _, exists := pending[payload.ActionID]; exists {
				pending[payload.ActionID] = pendingAction{CapabilityID: payload.CapabilityID}
			}
		case reducer.EventActionExecuting:
			payload, err := model.DecodePayloadStrict[model.ActionExecutingPayload](event)
			if err != nil {
				return nil, nil, err
			}
			if _, exists := pending[payload.ActionID]; exists {
				pending[payload.ActionID] = pendingAction{CapabilityID: payload.CapabilityID}
			}
		case reducer.EventActionDenied, reducer.EventActionApprovalRequired:
			payload, err := model.DecodePayloadStrict[model.ActionDecisionPayload](event)
			if err != nil {
				return nil, nil, err
			}
			delete(pending, payload.ActionID)
		case reducer.EventActionCommitted, reducer.EventActionFailed, reducer.EventActionUnknown:
			payload, err := model.DecodePayloadStrict[model.ActionResultPayload](event)
			if err != nil {
				return nil, nil, err
			}
			delete(pending, payload.ActionID)
		}
	}
	return pending, order, nil
}

func decodeInput(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, &model.KernelError{
			Code: model.ErrorSchemaInvalid, Operation: "request_action", Message: "input_base64 is invalid", Cause: err,
		}
	}
	return data, nil
}

func servicePrincipal() model.Principal {
	return model.Principal{ID: "service:gatemoled", Kind: model.PrincipalService, Issuer: "gatemoled"}
}

func capabilityError(code model.ErrorCode, resource, message string) *model.KernelError {
	return &model.KernelError{Code: code, Operation: "authorize_action", Resource: resource, Message: message}
}

func hasCode(err error, code model.ErrorCode) bool {
	var kernelErr *model.KernelError
	return errors.As(err, &kernelErr) && kernelErr.Code == code
}

func stringsCompare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
