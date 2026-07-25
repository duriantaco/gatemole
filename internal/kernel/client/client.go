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

	"github.com/duriantaco/vouch/internal/kernel/broker"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
	"github.com/duriantaco/vouch/internal/kernel/transaction/gitstage"
	"github.com/duriantaco/vouch/internal/kernel/verification"
)

const maxResponseBytes = 4 << 20

// Client talks to vouchd over a local Unix socket. The socket is the trust
// boundary: vouchd still validates every request and never trusts this client.
type Client struct {
	http        *http.Client
	bearerToken string
}

func New(socketPath string) *Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	client := NewWithTransport(transport)
	client.bearerToken = os.Getenv("VOUCH_IDENTITY_TOKEN")
	return client
}

// NewWithTransport supports embedded callers and transport-level tests. Most
// callers should use New so requests stay on the local vouchd Unix socket.
func NewWithTransport(transport http.RoundTripper) *Client {
	return &Client{http: &http.Client{Transport: transport, Timeout: 30 * time.Second}}
}

func (c *Client) WithBearerToken(token string) *Client {
	cloned := *c
	cloned.bearerToken = token
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
	runID, image string,
	command []string,
	timeoutSeconds int64,
	actor model.Principal,
) (TransactionAgentRunResult, error) {
	request := struct {
		ExpectedSequence int64           `json:"expected_sequence"`
		Actor            model.Principal `json:"actor"`
		RunID            string          `json:"run_id"`
		Image            string          `json:"image"`
		Command          []string        `json:"command"`
		TimeoutSeconds   int64           `json:"timeout_seconds"`
	}{
		ExpectedSequence: expectedSequence,
		Actor:            actor,
		RunID:            runID,
		Image:            image,
		Command:          append([]string(nil), command...),
		TimeoutSeconds:   timeoutSeconds,
	}
	var result TransactionAgentRunResult
	longHTTP := *c.http
	longHTTP.Timeout = time.Duration(timeoutSeconds)*time.Second + 30*time.Second
	longClient := &Client{http: &longHTTP, bearerToken: c.bearerToken}
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
	longClient := &Client{http: &longHTTP, bearerToken: c.bearerToken}
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
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode kernel request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://vouchd"+path, body)
	if err != nil {
		return fmt.Errorf("build kernel request: %w", err)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("connect to vouchd: %w", err)
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
		return fmt.Errorf("vouchd returned %s", response.Status)
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
