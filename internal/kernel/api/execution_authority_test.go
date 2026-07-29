package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/admission"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/store"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
	"github.com/duriantaco/vouch/internal/kernel/transaction/gitstage"
)

func TestRunAgentExecutionRejectsExpiredAuthorityBeforeWorkload(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	fixture := newExecutionAuthorityAPIFixture(
		t,
		"expired",
		base,
		model.ContractResource{
			ID: "workspace",
			Selector: model.ResourceSelector{
				Kind:    "filesystem",
				Pattern: "workspace/**",
			},
			Operations: []string{"filesystem.read", "filesystem.write"},
			Conditions: model.CapabilityConditions{
				WorkspaceRoot: "workspace",
			},
		},
	)

	fixture.now = base.Add(11 * time.Second)
	assertExecutionAuthorityRejectedBeforeWorkload(
		t,
		fixture,
		model.ErrorCapabilityExpired,
	)
}

func TestRunAgentExecutionRejectsNarrowWorkspaceBeforeWorkload(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC)
	fixture := newExecutionAuthorityAPIFixture(
		t,
		"narrow-workspace",
		base,
		model.ContractResource{
			ID: "workspace-source-only",
			Selector: model.ResourceSelector{
				Kind:    "filesystem",
				Pattern: "workspace/src/**",
			},
			Operations: []string{"filesystem.read", "filesystem.write"},
			Conditions: model.CapabilityConditions{
				WorkspaceRoot: "workspace",
			},
		},
	)

	assertExecutionAuthorityRejectedBeforeWorkload(
		t,
		fixture,
		model.ErrorCapabilityDenied,
	)
}

func TestRunAgentExecutionRejectsLegacyTransactionBeforeWorkload(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC)
	fixture := newLegacyExecutionAuthorityAPIFixture(t, base)

	assertExecutionAuthorityRejectedBeforeWorkload(
		t,
		fixture,
		model.ErrorCapabilityDenied,
	)
}

type executionAuthorityAPIFixture struct {
	handler         http.Handler
	kernelStore     *store.SQLiteStore
	now             time.Time
	namespace       string
	transactionID   string
	runID           string
	image           string
	command         []string
	actor           model.Principal
	engineMarker    string
	transactionHead int64
	transactionSize int
	runHead         int64
	runSize         int
}

func newExecutionAuthorityAPIFixture(
	t *testing.T,
	suffix string,
	now time.Time,
	resource model.ContractResource,
) *executionAuthorityAPIFixture {
	t.Helper()
	fixture := newExecutionAuthorityServerFixture(t, suffix, now)
	admitted := admitExecutionAuthorityTask(t, fixture, suffix, resource)
	fixture.prepareRunningTransaction(t, admitted.Transaction)
	fixture.captureEventHeads(t)
	return fixture
}

func admitExecutionAuthorityTask(
	t *testing.T,
	fixture *executionAuthorityAPIFixture,
	suffix string,
	resource model.ContractResource,
) admission.Result {
	t.Helper()
	commandDigest, err := transactionreducer.ComputeCommandDigest(fixture.command)
	if err != nil {
		t.Fatal(err)
	}
	maxWallTime := int64(10)
	request := admission.Request{
		Version:        admission.RequestVersion,
		IdempotencyKey: "admission:" + suffix,
		TransactionID:  fixture.transactionID,
		RunID:          fixture.runID,
		Intent:         "Exercise only the authority admitted for this task.",
		AgentProfile: model.AgentTaskProfileBinding{
			ID:            "agent-profile:" + suffix,
			Digest:        testDigest("b"),
			RuntimeClass:  "oci",
			ImageDigest:   testDigest("a"),
			CommandDigest: commandDigest,
		},
		Sponsor: model.Principal{
			ID:   "human:sponsor-" + suffix,
			Kind: model.PrincipalHuman,
		},
		Actor: fixture.actor,
		Contract: admission.ContractSpec{
			Risk:      "high",
			Resources: []model.ContractResource{resource},
			Budgets: model.BudgetLimits{
				MaxWallTimeSeconds: &maxWallTime,
			},
		},
	}
	response := requestJSON(
		t,
		fixture.handler,
		http.MethodPost,
		fmt.Sprintf(
			"/v0/namespaces/%s/task-admissions",
			fixture.namespace,
		),
		request,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf(
			"task admission status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var admitted admission.Result
	decodeResponse(t, response, &admitted)
	return admitted
}

func newLegacyExecutionAuthorityAPIFixture(
	t *testing.T,
	now time.Time,
) *executionAuthorityAPIFixture {
	t.Helper()
	fixture := newExecutionAuthorityServerFixture(t, "legacy", now)
	transaction := model.AgentTransaction{
		Version:                model.AgentTransactionVersion,
		ID:                     fixture.transactionID,
		Namespace:              fixture.namespace,
		IntentDigest:           testDigest("c"),
		Sponsor:                fixture.actor,
		AgentRunIDs:            []string{fixture.runID},
		StageBindings:          []model.StageBinding{},
		State:                  model.TransactionCreated,
		EffectIDs:              []string{},
		VerificationResultIDs:  []string{},
		OutstandingApprovalIDs: []string{},
		EventSequence:          1,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	event, err := transactionreducer.CreationEvent(transaction, fixture.actor)
	if err != nil {
		t.Fatal(err)
	}
	created, err := fixture.kernelStore.CreateTransaction(
		context.Background(),
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.prepareRunningTransaction(t, created)
	fixture.captureEventHeads(t)
	return fixture
}

func newExecutionAuthorityServerFixture(
	t *testing.T,
	suffix string,
	now time.Time,
) *executionAuthorityAPIFixture {
	t.Helper()
	return newExecutionAuthorityServerFixtureWithOptions(
		t,
		suffix,
		now,
		nil,
		nil,
	)
}

func newExecutionAuthorityServerFixtureWithOptions(
	t *testing.T,
	suffix string,
	now time.Time,
	clock func() time.Time,
	decorateStore func(store.Store) store.Store,
) *executionAuthorityAPIFixture {
	t.Helper()
	repository := executionAuthorityRepository(t)
	kernelStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	manager, err := gitstage.New()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "oci-invoked")
	engine := filepath.Join(t.TempDir(), "fake-oci-engine")
	engineScript := fmt.Sprintf(
		"#!/bin/sh\nprintf 'invoked\\n' >> %q\nexit 97\n",
		marker,
	)
	if err := os.WriteFile(engine, []byte(engineScript), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := &executionAuthorityAPIFixture{
		kernelStore:   kernelStore,
		now:           now,
		namespace:     "authority-api",
		transactionID: "tx:" + suffix,
		runID:         "run:" + suffix,
		image: "registry.example.invalid/agent@" +
			testDigest("a"),
		command:      []string{"/bin/sh", "-c", "exit 0"},
		actor:        model.Principal{ID: "operator:authority", Kind: model.PrincipalOperator},
		engineMarker: marker,
	}
	if clock == nil {
		clock = func() time.Time { return fixture.now }
	}
	serverStore := store.Store(kernelStore)
	if decorateStore != nil {
		serverStore = decorateStore(serverStore)
	}
	fixture.handler = NewServer(
		serverStore,
		WithClock(clock),
		WithTransactionRuntime(
			manager,
			repository,
			t.TempDir(),
			transactionreducer.BaselinePolicy{},
		),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			AllowedAgentImageDigests: map[string]struct{}{
				testDigest("a"): {},
			},
			EnginePath:          engine,
			VerifierUID:         1000,
			VerifierGID:         1000,
			VerifierMemoryBytes: 512 << 20,
			VerifierCPUMillis:   1000,
			VerifierPIDsLimit:   64,
			VerifierTmpfsBytes:  64 << 20,
			MaxAgentTimeoutSecs: 60,
		}),
	).Handler()
	return fixture
}

func (fixture *executionAuthorityAPIFixture) prepareRunningTransaction(
	t *testing.T,
	projection transactionreducer.Projection,
) {
	t.Helper()
	path := fmt.Sprintf(
		"/v0/namespaces/%s/transactions/%s",
		fixture.namespace,
		fixture.transactionID,
	)
	response := requestJSON(
		t,
		fixture.handler,
		http.MethodPost,
		path+"/start",
		transactionMutationRequest{
			ExpectedSequence: projection.Transaction.EventSequence,
			Actor:            fixture.actor,
		},
	)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"start transaction status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	decodeResponse(t, response, &projection)

	worktreeResponse := requestJSON(
		t,
		fixture.handler,
		http.MethodPost,
		path+"/git-worktree",
		createWorktreeRequest{
			ExpectedSequence: projection.Transaction.EventSequence,
			Actor:            fixture.actor,
		},
	)
	if worktreeResponse.Code != http.StatusCreated {
		t.Fatalf(
			"create worktree status=%d body=%s",
			worktreeResponse.Code,
			worktreeResponse.Body.String(),
		)
	}
}

func (fixture *executionAuthorityAPIFixture) captureEventHeads(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	projection, err := fixture.kernelStore.GetTransaction(
		ctx,
		fixture.namespace,
		fixture.transactionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	events, err := fixture.kernelStore.TransactionEvents(
		ctx,
		fixture.namespace,
		fixture.transactionID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.transactionHead = projection.Transaction.EventSequence
	fixture.transactionSize = len(events)
	if _, err := fixture.kernelStore.GetRun(
		ctx,
		fixture.namespace,
		fixture.runID,
	); err != nil {
		var kernelErr *model.KernelError
		if errors.As(err, &kernelErr) &&
			kernelErr.Code == model.ErrorNotFound {
			return
		}
		t.Fatal(err)
	}
	run, err := fixture.kernelStore.GetRun(
		ctx,
		fixture.namespace,
		fixture.runID,
	)
	if err != nil {
		t.Fatal(err)
	}
	runEvents, err := fixture.kernelStore.Events(
		ctx,
		fixture.namespace,
		fixture.runID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runHead = run.Run.EventSequence
	fixture.runSize = len(runEvents)
}

func assertExecutionAuthorityRejectedBeforeWorkload(
	t *testing.T,
	fixture *executionAuthorityAPIFixture,
	wantCode model.ErrorCode,
) {
	t.Helper()
	response := requestJSON(
		t,
		fixture.handler,
		http.MethodPost,
		fmt.Sprintf(
			"/v0/namespaces/%s/transactions/%s/executions/run",
			fixture.namespace,
			fixture.transactionID,
		),
		runAgentExecutionRequest{
			ExpectedSequence: fixture.transactionHead,
			Actor:            fixture.actor,
			Image:            fixture.image,
			Command:          append([]string(nil), fixture.command...),
			TimeoutSeconds:   1,
		},
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"execution rejection status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var kernelErr model.KernelError
	decodeResponse(t, response, &kernelErr)
	if kernelErr.Code != wantCode {
		t.Fatalf(
			"execution rejection code=%q want=%q body=%s",
			kernelErr.Code,
			wantCode,
			response.Body.String(),
		)
	}
	if _, err := os.Stat(fixture.engineMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected authority invoked OCI engine: %v", err)
	}

	ctx := context.Background()
	transaction, err := fixture.kernelStore.GetTransaction(
		ctx,
		fixture.namespace,
		fixture.transactionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	transactionEvents, err := fixture.kernelStore.TransactionEvents(
		ctx,
		fixture.namespace,
		fixture.transactionID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if transaction.Transaction.EventSequence != fixture.transactionHead ||
		len(transactionEvents) != fixture.transactionSize ||
		len(transaction.Executions) != 0 {
		t.Fatalf(
			"rejected authority changed transaction: sequence=%d events=%d executions=%d",
			transaction.Transaction.EventSequence,
			len(transactionEvents),
			len(transaction.Executions),
		)
	}
	if fixture.runHead == 0 {
		return
	}
	run, err := fixture.kernelStore.GetRun(
		ctx,
		fixture.namespace,
		fixture.runID,
	)
	if err != nil {
		t.Fatal(err)
	}
	runEvents, err := fixture.kernelStore.Events(
		ctx,
		fixture.namespace,
		fixture.runID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.EventSequence != fixture.runHead ||
		len(runEvents) != fixture.runSize {
		t.Fatalf(
			"rejected authority changed run: sequence=%d events=%d",
			run.Run.EventSequence,
			len(runEvents),
		)
	}
}

func executionAuthorityRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repository, "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(repository, "src", "main.go"),
		[]byte("package main\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	executionAuthorityGit(t, repository, "init", "--initial-branch=main")
	executionAuthorityGit(t, repository, "add", "--", "src/main.go")
	executionAuthorityGit(
		t,
		repository,
		"-c",
		"user.name=Vouch Test",
		"-c",
		"user.email=vouch@example.invalid",
		"commit",
		"-m",
		"fixture",
	)
	return repository
}

func executionAuthorityGit(t *testing.T, repository string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}
