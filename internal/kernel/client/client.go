package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/admission"
	"github.com/duriantaco/vouch/internal/kernel/broker"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
	"github.com/duriantaco/vouch/internal/kernel/runtimeidentity"
	"github.com/duriantaco/vouch/internal/kernel/runtimepreflight"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
	"github.com/duriantaco/vouch/internal/kernel/transaction/gitstage"
	"github.com/duriantaco/vouch/internal/kernel/verification"
)

const maxResponseBytes = 4 << 20

// Client talks to gatemoled over a local Unix socket. New verifies that each
// connection's peer owns the stable, private socket path before HTTP can send
// a bearer token. gatemoled still validates every request and never trusts this
// client.
type Client struct {
	http              *http.Client
	bearerToken       string
	expectedRuntimeID string
}

func New(socketPath string) *Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialVerifiedUnix(
				ctx,
				dialer.DialContext,
				socketPath,
			)
		},
	}
	client := NewWithTransport(transport)
	client.bearerToken = os.Getenv("GATEMOLE_IDENTITY_TOKEN")
	return client
}

// NewWithTransport supports embedded callers and transport-level tests. Most
// callers should use New so requests stay on the local gatemoled Unix socket.
func NewWithTransport(transport http.RoundTripper) *Client {
	return &Client{http: &http.Client{Transport: transport, Timeout: 30 * time.Second}}
}

func (c *Client) WithBearerToken(token string) *Client {
	cloned := *c
	cloned.bearerToken = token
	return &cloned
}

// WithExpectedRuntimeID returns a client that binds every request to one
// Runtime. A configured daemon rejects the request before handler side effects
// when the header names another Runtime.
func (c *Client) WithExpectedRuntimeID(runtimeID string) *Client {
	cloned := *c
	cloned.expectedRuntimeID = runtimeID
	return &cloned
}

func (c *Client) CreateRun(ctx context.Context, event model.RunEvent) (reducer.Projection, error) {
	var projection reducer.Projection
	err := c.do(ctx, http.MethodPost, "/v0/runs", event, &projection)
	return projection, err
}

func (c *Client) GetRun(ctx context.Context, namespace, runID string) (reducer.Projection, error) {
	var projection reducer.Projection
	err := c.do(ctx, http.MethodGet, runPath(namespace, runID), nil, &projection)
	return projection, err
}

func (c *Client) ListRuns(ctx context.Context, namespace string) ([]model.AgentRun, error) {
	var runs []model.AgentRun
	path := "/v0/namespaces/" + url.PathEscape(namespace) + "/runs"
	err := c.do(ctx, http.MethodGet, path, nil, &runs)
	return runs, err
}

func (c *Client) Events(ctx context.Context, namespace, runID string, after int64) ([]model.RunEvent, error) {
	var events []model.RunEvent
	path := runPath(namespace, runID) + "/events?after=" + strconv.FormatInt(after, 10)
	err := c.do(ctx, http.MethodGet, path, nil, &events)
	return events, err
}

func (c *Client) AppendEvent(
	ctx context.Context,
	namespace, runID string,
	expectedSequence int64,
	event model.RunEvent,
) (reducer.Projection, error) {
	request := struct {
		ExpectedSequence int64          `json:"expected_sequence"`
		Event            model.RunEvent `json:"event"`
	}{ExpectedSequence: expectedSequence, Event: event}
	var projection reducer.Projection
	err := c.do(ctx, http.MethodPost, runPath(namespace, runID)+"/events", request, &projection)
	return projection, err
}

type CapabilityInstallResult struct {
	Projection reducer.Projection      `json:"projection"`
	Grants     []model.CapabilityGrant `json:"grants"`
}

func (c *Client) InstallCapabilities(
	ctx context.Context,
	namespace, runID string,
	expectedSequence int64,
	contract model.ExecutionContract,
	actor model.Principal,
) (CapabilityInstallResult, error) {
	request := struct {
		ExpectedSequence int64                   `json:"expected_sequence"`
		Contract         model.ExecutionContract `json:"contract"`
		Actor            model.Principal         `json:"actor"`
	}{ExpectedSequence: expectedSequence, Contract: contract, Actor: actor}
	var result CapabilityInstallResult
	err := c.do(ctx, http.MethodPost, runPath(namespace, runID)+"/capabilities", request, &result)
	return result, err
}

func (c *Client) ExecuteAction(
	ctx context.Context,
	namespace, runID string,
	request broker.ExecuteRequest,
) (broker.Outcome, error) {
	var outcome broker.Outcome
	err := c.do(ctx, http.MethodPost, runPath(namespace, runID)+"/actions", request, &outcome)
	return outcome, err
}

func (c *Client) CreateTransaction(ctx context.Context, event model.TransactionEvent) (transactionreducer.Projection, error) {
	var projection transactionreducer.Projection
	err := c.do(ctx, http.MethodPost, "/v0/transactions", event, &projection)
	return projection, err
}

func (c *Client) AdmitTask(
	ctx context.Context,
	namespace string,
	request admission.Request,
) (admission.Result, error) {
	var result admission.Result
	path := "/v0/namespaces/" + url.PathEscape(namespace) + "/task-admissions"
	err := c.do(ctx, http.MethodPost, path, request, &result)
	if err != nil {
		return admission.Result{}, err
	}
	if err := result.ValidateAgainstRequest(namespace, request); err != nil {
		return admission.Result{}, fmt.Errorf(
			"validate task admission response: %w",
			err,
		)
	}
	if result.RuntimeID != request.ExpectedRuntimeID ||
		result.EnforcementProfile != request.ExpectedEnforcementProfile ||
		result.IdempotencyKey != request.IdempotencyKey {
		return admission.Result{}, errors.New(
			"validate task admission response: response does not match request",
		)
	}
	return result, nil
}

func (c *Client) PreflightRuntime(
	ctx context.Context,
	namespace string,
	request runtimepreflight.Request,
) (runtimepreflight.Result, error) {
	if err := request.Validate(); err != nil {
		return runtimepreflight.Result{}, fmt.Errorf(
			"validate Runtime preflight request: %w",
			err,
		)
	}
	var result runtimepreflight.Result
	path := "/v0/namespaces/" + url.PathEscape(namespace) +
		"/runtime/preflight"
	err := c.do(ctx, http.MethodPost, path, request, &result)
	if err != nil {
		return runtimepreflight.Result{}, err
	}
	if err := result.Validate(); err != nil {
		return runtimepreflight.Result{}, fmt.Errorf(
			"validate Runtime preflight response: %w",
			err,
		)
	}
	if result.RuntimeID != request.ExpectedRuntimeID {
		return runtimepreflight.Result{}, errors.New(
			"validate Runtime preflight response: Runtime identity does not match request",
		)
	}
	if request.RequiredEnforcementProfile != "" &&
		result.EnforcementProfile != request.RequiredEnforcementProfile {
		return runtimepreflight.Result{}, errors.New(
			"validate Runtime preflight response: enforcement profile does not match request",
		)
	}
	switch {
	case request.Agent == nil &&
		(result.AgentProfileID != "" || result.ImageDigest != ""):
		return runtimepreflight.Result{}, errors.New(
			"validate Runtime preflight response: unexpected agent binding",
		)
	case request.Agent != nil &&
		(result.AgentProfileID != request.Agent.Profile.ID ||
			result.ImageDigest != request.Agent.Profile.ImageDigest):
		return runtimepreflight.Result{}, errors.New(
			"validate Runtime preflight response: agent binding does not match request",
		)
	}
	return result, nil
}

// CreateTaskTransaction is the product-level creation path for a single
// supervised agent task. The lower-level CreateTransaction method remains for
// replay tools and v0 callers that construct their own creation event.
func (c *Client) CreateTaskTransaction(
	ctx context.Context,
	task model.AgentTask,
	sponsor, actor model.Principal,
) (transactionreducer.Projection, error) {
	if err := task.Validate(); err != nil {
		return transactionreducer.Projection{}, err
	}
	transaction := model.AgentTransaction{
		Version:                model.AgentTransactionVersion,
		ID:                     task.TransactionID,
		Namespace:              task.Namespace,
		IntentDigest:           task.IntentDigest,
		Task:                   &task,
		Sponsor:                sponsor,
		AgentRunIDs:            []string{task.RunID},
		StageBindings:          []model.StageBinding{},
		State:                  model.TransactionCreated,
		EffectIDs:              []string{},
		VerificationResultIDs:  []string{},
		OutstandingApprovalIDs: []string{},
		EventSequence:          1,
		CreatedAt:              task.CreatedAt,
		UpdatedAt:              task.CreatedAt,
	}
	event, err := transactionreducer.CreationEvent(transaction, actor)
	if err != nil {
		return transactionreducer.Projection{}, err
	}
	return c.CreateTransaction(ctx, event)
}

func (c *Client) GetTransaction(ctx context.Context, namespace, transactionID string) (transactionreducer.Projection, error) {
	var projection transactionreducer.Projection
	err := c.do(ctx, http.MethodGet, transactionPath(namespace, transactionID), nil, &projection)
	return projection, err
}

func (c *Client) ListTransactions(ctx context.Context, namespace string) ([]model.AgentTransaction, error) {
	var transactions []model.AgentTransaction
	path := "/v0/namespaces/" + url.PathEscape(namespace) + "/transactions"
	err := c.do(ctx, http.MethodGet, path, nil, &transactions)
	return transactions, err
}

func (c *Client) TransactionEvents(ctx context.Context, namespace, transactionID string, after int64) ([]model.TransactionEvent, error) {
	var events []model.TransactionEvent
	path := transactionPath(namespace, transactionID) + "/events?after=" + strconv.FormatInt(after, 10)
	err := c.do(ctx, http.MethodGet, path, nil, &events)
	return events, err
}

type TransactionWorktreeResult struct {
	Projection transactionreducer.Projection `json:"projection"`
	Workspace  gitstage.Workspace            `json:"workspace"`
}

type TransactionStageResult struct {
	Projection transactionreducer.Projection `json:"projection"`
	Snapshot   gitstage.Snapshot             `json:"snapshot"`
}

type TransactionValidationResult struct {
	Projection transactionreducer.Projection       `json:"projection"`
	Decision   transactionreducer.SequenceDecision `json:"decision"`
}

type TransactionAbortResult struct {
	Projection       transactionreducer.Projection `json:"projection"`
	WorkspaceRemoved bool                          `json:"workspace_removed"`
	CleanupWarning   string                        `json:"cleanup_warning,omitempty"`
}

type TransactionVerificationRecordResult struct {
	Projection transactionreducer.Projection `json:"projection"`
	Result     model.VerificationResult      `json:"result"`
}

type TransactionVerificationRunResult struct {
	Projection        transactionreducer.Projection `json:"projection"`
	Result            model.VerificationResult      `json:"result"`
	Process           verification.ProcessReceipt   `json:"process"`
	EvidenceDirectory string                        `json:"evidence_directory"`
}

type TransactionAgentRunResult struct {
	Projection        transactionreducer.Projection `json:"projection"`
	Execution         model.AgentExecution          `json:"execution"`
	Process           verification.ProcessReceipt   `json:"process"`
	EvidenceDirectory string                        `json:"evidence_directory"`
}

type TransactionPrepareResult struct {
	Projection transactionreducer.Projection       `json:"projection"`
	Decision   transactionreducer.SequenceDecision `json:"decision"`
}

type TransactionReleaseResult struct {
	Projection transactionreducer.Projection `json:"projection"`
	Prepared   gitstage.PreparedCommit       `json:"prepared"`
	Publish    gitstage.PublishResult        `json:"publish"`
	Message    string                        `json:"message,omitempty"`
}

func (c *Client) StartTransaction(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	actor model.Principal,
) (transactionreducer.Projection, error) {
	var projection transactionreducer.Projection
	err := c.transactionMutation(ctx, namespace, transactionID, "start", expectedSequence, actor, &projection)
	return projection, err
}

func (c *Client) CreateTransactionWorktree(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	revision string,
	actor model.Principal,
) (TransactionWorktreeResult, error) {
	request := struct {
		ExpectedSequence int64           `json:"expected_sequence"`
		Revision         string          `json:"revision,omitempty"`
		Actor            model.Principal `json:"actor"`
	}{ExpectedSequence: expectedSequence, Revision: revision, Actor: actor}
	var result TransactionWorktreeResult
	err := c.do(ctx, http.MethodPost, transactionPath(namespace, transactionID)+"/git-worktree", request, &result)
	return result, err
}

func (c *Client) StartAgentExecution(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	runID, program, commandDigest, runtimeClass, runtimeConfigDigest, imageDigest string,
	actor model.Principal,
) (transactionreducer.Projection, error) {
	request := struct {
		ExpectedSequence    int64           `json:"expected_sequence"`
		Actor               model.Principal `json:"actor"`
		RunID               string          `json:"run_id"`
		Program             string          `json:"program"`
		CommandDigest       string          `json:"command_digest"`
		RuntimeClass        string          `json:"runtime_class"`
		RuntimeConfigDigest string          `json:"runtime_config_digest"`
		ImageDigest         string          `json:"image_digest,omitempty"`
	}{
		ExpectedSequence:    expectedSequence,
		Actor:               actor,
		RunID:               runID,
		Program:             program,
		CommandDigest:       commandDigest,
		RuntimeClass:        runtimeClass,
		RuntimeConfigDigest: runtimeConfigDigest,
		ImageDigest:         imageDigest,
	}
	var projection transactionreducer.Projection
	err := c.do(
		ctx, http.MethodPost,
		transactionPath(namespace, transactionID)+"/executions/start",
		request, &projection,
	)
	return projection, err
}

func (c *Client) FinishAgentExecution(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	executionID string,
	status model.AgentExecutionStatus,
	exitCode *int,
	stdoutDigest, stderrDigest string,
	actor model.Principal,
) (transactionreducer.Projection, error) {
	request := struct {
		ExpectedSequence int64                      `json:"expected_sequence"`
		Actor            model.Principal            `json:"actor"`
		ExecutionID      string                     `json:"execution_id"`
		Status           model.AgentExecutionStatus `json:"status"`
		ExitCode         *int                       `json:"exit_code,omitempty"`
		StdoutDigest     string                     `json:"stdout_digest"`
		StderrDigest     string                     `json:"stderr_digest"`
	}{
		ExpectedSequence: expectedSequence,
		Actor:            actor,
		ExecutionID:      executionID,
		Status:           status,
		ExitCode:         exitCode,
		StdoutDigest:     stdoutDigest,
		StderrDigest:     stderrDigest,
	}
	var projection transactionreducer.Projection
	err := c.do(
		ctx, http.MethodPost,
		transactionPath(namespace, transactionID)+"/executions/finish",
		request, &projection,
	)
	return projection, err
}

func (c *Client) RunTransactionAgent(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	image string,
	command []string,
	timeoutSeconds int64,
	actor model.Principal,
) (TransactionAgentRunResult, error) {
	request := struct {
		ExpectedSequence int64           `json:"expected_sequence"`
		Actor            model.Principal `json:"actor"`
		Image            string          `json:"image"`
		Command          []string        `json:"command"`
		TimeoutSeconds   int64           `json:"timeout_seconds,omitempty"`
	}{
		ExpectedSequence: expectedSequence,
		Actor:            actor,
		Image:            image,
		Command:          append([]string(nil), command...),
		TimeoutSeconds:   timeoutSeconds,
	}
	var result TransactionAgentRunResult
	longHTTP := *c.http
	if timeoutSeconds > 0 {
		longHTTP.Timeout =
			time.Duration(timeoutSeconds)*time.Second + 30*time.Second
	} else {
		longHTTP.Timeout = 0
	}
	longClient := *c
	longClient.http = &longHTTP
	err := longClient.do(
		ctx, http.MethodPost,
		transactionPath(namespace, transactionID)+"/executions/run",
		request, &result,
	)
	return result, err
}

func (c *Client) StageTransaction(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	actor model.Principal,
) (TransactionStageResult, error) {
	var result TransactionStageResult
	err := c.transactionMutation(ctx, namespace, transactionID, "stage", expectedSequence, actor, &result)
	return result, err
}

func (c *Client) ValidateTransaction(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	actor model.Principal,
) (TransactionValidationResult, error) {
	var result TransactionValidationResult
	err := c.transactionMutation(ctx, namespace, transactionID, "validate", expectedSequence, actor, &result)
	return result, err
}

func (c *Client) RecordTransactionVerification(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	name string,
	status model.VerificationStatus,
	commandDigest, runtimeConfigDigest, imageDigest string,
	evidence []model.ArtifactRef,
	summary string,
	actor model.Principal,
) (TransactionVerificationRecordResult, error) {
	request := struct {
		ExpectedSequence    int64                    `json:"expected_sequence"`
		Actor               model.Principal          `json:"actor"`
		Name                string                   `json:"name"`
		Status              model.VerificationStatus `json:"status"`
		CommandDigest       string                   `json:"command_digest"`
		RuntimeConfigDigest string                   `json:"runtime_config_digest"`
		ImageDigest         string                   `json:"image_digest"`
		Evidence            []model.ArtifactRef      `json:"evidence"`
		Summary             string                   `json:"summary"`
	}{
		ExpectedSequence:    expectedSequence,
		Actor:               actor,
		Name:                name,
		Status:              status,
		CommandDigest:       commandDigest,
		RuntimeConfigDigest: runtimeConfigDigest,
		ImageDigest:         imageDigest,
		Evidence:            evidence,
		Summary:             summary,
	}
	var result TransactionVerificationRecordResult
	err := c.do(
		ctx, http.MethodPost,
		transactionPath(namespace, transactionID)+"/verifications",
		request, &result,
	)
	return result, err
}

func (c *Client) RunTransactionVerification(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	name, image string,
	command []string,
	timeoutSeconds int64,
	actor model.Principal,
) (TransactionVerificationRunResult, error) {
	request := struct {
		ExpectedSequence int64           `json:"expected_sequence"`
		Actor            model.Principal `json:"actor"`
		Name             string          `json:"name"`
		Image            string          `json:"image"`
		Command          []string        `json:"command"`
		TimeoutSeconds   int64           `json:"timeout_seconds"`
	}{
		ExpectedSequence: expectedSequence,
		Actor:            actor,
		Name:             name,
		Image:            image,
		Command:          append([]string(nil), command...),
		TimeoutSeconds:   timeoutSeconds,
	}
	var result TransactionVerificationRunResult
	longHTTP := *c.http
	longHTTP.Timeout = time.Duration(timeoutSeconds)*time.Second + 30*time.Second
	longClient := *c
	longClient.http = &longHTTP
	err := longClient.do(
		ctx, http.MethodPost,
		transactionPath(namespace, transactionID)+"/verifications/run",
		request, &result,
	)
	return result, err
}

func (c *Client) PrepareTransaction(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	gitRef string,
	actor model.Principal,
) (TransactionPrepareResult, error) {
	var result TransactionPrepareResult
	request := struct {
		ExpectedSequence int64           `json:"expected_sequence"`
		Actor            model.Principal `json:"actor"`
		GitRef           string          `json:"git_ref"`
	}{
		ExpectedSequence: expectedSequence,
		Actor:            actor,
		GitRef:           gitRef,
	}
	err := c.do(
		ctx, http.MethodPost,
		transactionPath(namespace, transactionID)+"/prepare",
		request, &result,
	)
	return result, err
}

func (c *Client) ResolveTransactionApproval(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	decision model.ApprovalDecision,
) (transactionreducer.Projection, error) {
	request := struct {
		ExpectedSequence int64                  `json:"expected_sequence"`
		Decision         model.ApprovalDecision `json:"decision"`
	}{
		ExpectedSequence: expectedSequence,
		Decision:         decision,
	}
	var projection transactionreducer.Projection
	err := c.do(
		ctx, http.MethodPost,
		transactionPath(namespace, transactionID)+"/approvals",
		request, &projection,
	)
	return projection, err
}

func (c *Client) ReleaseTransaction(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	actor model.Principal,
) (TransactionReleaseResult, error) {
	var result TransactionReleaseResult
	err := c.transactionMutation(
		ctx, namespace, transactionID, "release",
		expectedSequence, actor, &result,
	)
	return result, err
}

func (c *Client) AbortTransaction(
	ctx context.Context,
	namespace, transactionID string,
	expectedSequence int64,
	actor model.Principal,
) (TransactionAbortResult, error) {
	var result TransactionAbortResult
	err := c.transactionMutation(ctx, namespace, transactionID, "abort", expectedSequence, actor, &result)
	return result, err
}

func (c *Client) transactionMutation(
	ctx context.Context,
	namespace, transactionID, operation string,
	expectedSequence int64,
	actor model.Principal,
	output any,
) error {
	request := struct {
		ExpectedSequence int64           `json:"expected_sequence"`
		Actor            model.Principal `json:"actor"`
	}{ExpectedSequence: expectedSequence, Actor: actor}
	return c.do(ctx, http.MethodPost, transactionPath(namespace, transactionID)+"/"+operation, request, output)
}

func (c *Client) do(ctx context.Context, method, path string, input, output any) error {
	if c.expectedRuntimeID != "" &&
		!runtimeidentity.IsRuntimeID(c.expectedRuntimeID) {
		return errors.New(
			"build kernel request: expected Runtime identity is invalid",
		)
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode kernel request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://gatemoled"+path, body)
	if err != nil {
		return fmt.Errorf("build kernel request: %w", err)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
	if c.expectedRuntimeID != "" {
		request.Header.Set(
			runtimeidentity.HTTPHeader,
			c.expectedRuntimeID,
		)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("connect to gatemoled: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read kernel response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return errors.New("kernel response exceeds 4 MiB")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var kernelErr model.KernelError
		if err := json.Unmarshal(data, &kernelErr); err == nil && kernelErr.Code != "" {
			return &kernelErr
		}
		return fmt.Errorf("gatemoled returned %s", response.Status)
	}
	if output == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode kernel response: %w", err)
	}
	return nil
}

func runPath(namespace, runID string) string {
	return "/v0/namespaces/" + url.PathEscape(namespace) + "/runs/" + url.PathEscape(runID)
}

func transactionPath(namespace, transactionID string) string {
	return "/v0/namespaces/" + url.PathEscape(namespace) + "/transactions/" + url.PathEscape(transactionID)
}
