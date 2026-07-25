package vouch

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	kernelapi "github.com/duriantaco/vouch/internal/kernel/api"
	"github.com/duriantaco/vouch/internal/kernel/approval"
	kernelclient "github.com/duriantaco/vouch/internal/kernel/client"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/store"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
	"github.com/duriantaco/vouch/internal/kernel/transaction/gitstage"
)

func TestTransactionRunSupervisesStagesAndValidatesAgent(t *testing.T) {
	repo := transactionRunRepository(t)
	runGitForTransactionTest(t, repo, "branch", "release/runtime-success", "HEAD")
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approver := model.Principal{ID: "human:alice", Kind: model.PrincipalHuman}
	approvalTrust, err := approval.NewTrustStore([]approval.TrustedKey{{
		KeyID: "key:alice", PublicKey: publicKey, Principal: approver,
		ApprovalClasses: []string{"security-reviewer"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	privateKeyPath := filepath.Join(t.TempDir(), "approver.key")
	if err := os.WriteFile(
		privateKeyPath,
		[]byte(base64.StdEncoding.EncodeToString(privateKey)+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	kernelStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	manager, err := gitstage.New()
	if err != nil {
		t.Fatal(err)
	}
	fakeRuntime := writeFakeOCIRuntime(t)
	failingStore := &failTransactionAppendStore{Store: kernelStore}
	handler := kernelapi.NewServer(
		failingStore,
		kernelapi.WithTransactionRuntime(
			manager, repo, t.TempDir(), transactionreducer.BaselinePolicy{},
		),
		kernelapi.WithApprovalTrustStore(approvalTrust),
		kernelapi.WithExecutionRuntimePolicy(verifierExecutionPolicy(fakeRuntime)),
	).Handler()
	newClient := func(string) transactionClient {
		return kernelclient.NewWithTransport(handlerTransport{handler: handler})
	}
	secretArgument := "operator-secret-must-not-enter-ledger"
	script := "printf 'package auth\\n\\nfunc Allowed() bool { return true }\\n' > internal/auth/middleware.go; " +
		"printf 'package auth\\n\\nfunc TestAllowed() { /* rewritten */ }\\n' > internal/auth/middleware_test.go; " +
		": " + secretArgument
	stdout, stderr, code := invokeTransactionRunCLI(
		repo, newClient, true,
		"run",
		"--id", "tx:runtime-success",
		"--namespace", "payments",
		"--intent", "Change authentication with independent verification",
		"--run", "run:runtime-success",
		"--runtime", "host",
		"--unsafe-host",
		"--",
		"/bin/sh", "-c", script,
	)
	if code != 0 {
		t.Fatalf("tx run code=%d stderr=%s", code, stderr)
	}
	var result transactionRunResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode tx run output: %v\n%s", err, stdout)
	}
	if result.Execution.Status != model.AgentExecutionSucceeded ||
		result.Execution.ExitCode == nil || *result.Execution.ExitCode != 0 {
		t.Fatalf("unexpected execution receipt: %#v", result.Execution)
	}
	if result.Projection.Transaction.Task == nil {
		t.Fatal("transaction run did not persist the task envelope")
	}
	task := result.Projection.Transaction.Task
	if task.Intent != "Change authentication with independent verification" ||
		task.AgentProfile.RuntimeClass != "host" ||
		task.AgentProfile.ImageDigest != "" {
		t.Fatalf("unexpected task envelope: %#v", task)
	}
	commandDigest, err := transactionreducer.ComputeCommandDigest(
		[]string{"/bin/sh", "-c", script},
	)
	if err != nil {
		t.Fatal(err)
	}
	if task.AgentProfile.CommandDigest != commandDigest {
		t.Fatalf(
			"task command digest=%s, want %s",
			task.AgentProfile.CommandDigest,
			commandDigest,
		)
	}
	if len(result.Projection.Effects) != 2 {
		t.Fatalf("effects=%d, want 2", len(result.Projection.Effects))
	}
	if result.Decision == nil ||
		result.Decision.Outcome != transactionreducer.SequenceRequireApproval {
		t.Fatalf("unexpected sequence decision: %#v", result.Decision)
	}
	if result.Projection.Transaction.State != model.TransactionValidating {
		t.Fatalf("state=%q, want validating", result.Projection.Transaction.State)
	}
	pinnedVerifier := "registry.example.invalid/verifier@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	verifyStdout, verifyStderr, verifyCode := invokeTransactionRunCLI(
		repo, newClient, true,
		"verify",
		"--namespace", "payments",
		"--id", "tx:runtime-success",
		"--name", "tests",
		"--image", pinnedVerifier,
		"--",
		"/bin/sh", "-c", "test -f internal/auth/middleware.go",
	)
	if verifyCode != 0 {
		t.Fatalf("tx verify code=%d stderr=%s", verifyCode, verifyStderr)
	}
	var verification transactionVerifyResult
	if err := json.Unmarshal([]byte(verifyStdout), &verification); err != nil {
		t.Fatalf("decode tx verify output: %v\n%s", err, verifyStdout)
	}
	if verification.Verification.Status != model.VerificationPassed ||
		verification.Verification.EffectSetDigest != result.Projection.Transaction.EffectSetDigest ||
		verification.Verification.StagedStateDigest != result.Projection.Transaction.StagedStateDigest {
		t.Fatalf("verification was not bound to frozen state: %#v", verification.Verification)
	}
	for _, effect := range verification.Projection.Effects {
		if effect.Status != model.EffectValidated {
			t.Fatalf("passing verifier left effect %s in %s", effect.ID, effect.Status)
		}
	}
	if _, err := os.Stat(filepath.Join(verification.EvidenceDirectory, "receipt.json")); err != nil {
		t.Fatalf("verification receipt missing: %v", err)
	}
	prepareStdout, prepareStderr, prepareCode := invokeTransactionRunCLI(
		repo, newClient, true,
		"prepare",
		"--namespace", "payments",
		"--id", "tx:runtime-success",
		"--git-ref", "refs/heads/release/runtime-success",
	)
	if prepareCode != 0 {
		t.Fatalf("tx prepare code=%d stderr=%s", prepareCode, prepareStderr)
	}
	var prepared kernelclient.TransactionPrepareResult
	if err := json.Unmarshal([]byte(prepareStdout), &prepared); err != nil {
		t.Fatalf("decode tx prepare output: %v\n%s", err, prepareStdout)
	}
	if prepared.Projection.Transaction.State != model.TransactionPendingApproval ||
		prepared.Projection.CommitPlan == nil ||
		prepared.Projection.ApprovalPackage == nil ||
		len(prepared.Projection.Transaction.OutstandingApprovalIDs) != 1 {
		t.Fatalf("transaction authority was not prepared: %#v", prepared)
	}
	planDigest, err := transactionreducer.ComputeCommitPlanDigest(*prepared.Projection.CommitPlan)
	if err != nil {
		t.Fatal(err)
	}
	approvalDigest, err := transactionreducer.ComputeApprovalPackageDigest(*prepared.Projection.ApprovalPackage)
	if err != nil {
		t.Fatal(err)
	}
	if planDigest != prepared.Projection.Transaction.CommitPlanDigest ||
		approvalDigest != prepared.Projection.Transaction.ApprovalPackageDigest {
		t.Fatal("prepared authority digests do not bind their immutable content")
	}
	_, roguePrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rogueKeyPath := filepath.Join(t.TempDir(), "rogue.key")
	if err := os.WriteFile(
		rogueKeyPath,
		[]byte(base64.StdEncoding.EncodeToString(roguePrivateKey)+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	_, rogueStderr, rogueCode := invokeTransactionRunCLI(
		repo, newClient, true,
		"approve",
		"--namespace", "payments",
		"--id", "tx:runtime-success",
		"--key", rogueKeyPath,
		"--key-id", "key:alice",
		"--approver", "human:alice",
		"--class", "security-reviewer",
	)
	if rogueCode != 1 || !strings.Contains(rogueStderr, string(model.ErrorApprovalInvalid)) {
		t.Fatalf("forged approval was not rejected: code=%d stderr=%s", rogueCode, rogueStderr)
	}
	_, approveStderr, approveCode := invokeTransactionRunCLI(
		repo, newClient, true,
		"approve",
		"--namespace", "payments",
		"--id", "tx:runtime-success",
		"--key", privateKeyPath,
		"--key-id", "key:alice",
		"--approver", "human:alice",
		"--class", "security-reviewer",
	)
	if approveCode != 0 {
		t.Fatalf("tx approve code=%d stderr=%s", approveCode, approveStderr)
	}
	authorized, err := newClient("").GetTransaction(
		context.Background(), "payments", "tx:runtime-success",
	)
	if err != nil {
		t.Fatal(err)
	}
	if authorized.Transaction.State != model.TransactionReadyToCommit ||
		len(authorized.Transaction.OutstandingApprovalIDs) != 0 ||
		len(authorized.ApprovalDigests) != 1 {
		t.Fatalf("signed approval did not grant commit authority: %#v", authorized)
	}
	failingStore.failAfter(2)
	releaseStdout, releaseStderr, releaseCode := invokeTransactionRunCLI(
		repo, newClient, true,
		"release",
		"--namespace", "payments",
		"--id", "tx:runtime-success",
	)
	if releaseCode != 1 || !strings.Contains(releaseStderr, string(model.ErrorInternal)) {
		t.Fatalf("release finalization failure was not surfaced: code=%d stderr=%s stdout=%s", releaseCode, releaseStderr, releaseStdout)
	}
	interrupted, err := newClient("").GetTransaction(
		context.Background(), "payments", "tx:runtime-success",
	)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Transaction.State != model.TransactionCommitting {
		t.Fatalf("crash window did not remain recoverable: %s", interrupted.Transaction.State)
	}
	releaseStdout, releaseStderr, releaseCode = invokeTransactionRunCLI(
		repo, newClient, true,
		"release",
		"--namespace", "payments",
		"--id", "tx:runtime-success",
	)
	if releaseCode != 0 {
		t.Fatalf("tx release recovery code=%d stderr=%s stdout=%s", releaseCode, releaseStderr, releaseStdout)
	}
	var released kernelclient.TransactionReleaseResult
	if err := json.Unmarshal([]byte(releaseStdout), &released); err != nil {
		t.Fatalf("decode tx release output: %v\n%s", err, releaseStdout)
	}
	if released.Projection.Transaction.State != model.TransactionCommitted ||
		released.Publish.Status != gitstage.PublishReconciled {
		t.Fatalf("Git release did not reconcile the crash window: %#v", released)
	}
	for _, effect := range released.Projection.Effects {
		if effect.Status != model.EffectCommitted ||
			effect.Receipt == nil ||
			effect.Receipt.ResourceVersion != released.Prepared.CommitRevision {
			t.Fatalf("effect lacks release receipt: %#v", effect)
		}
	}
	releasedSource := runGitForTransactionTestOutput(
		t, repo,
		"show", "refs/heads/release/runtime-success:internal/auth/middleware.go",
	)
	if !strings.Contains(releasedSource, "return true") {
		t.Fatalf("release branch does not contain staged change: %s", releasedSource)
	}
	source, err := os.ReadFile(filepath.Join(repo, "internal", "auth", "middleware.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "return true") {
		t.Fatal("supervised agent changed the source checkout")
	}
	events, err := newClient("").TransactionEvents(
		context.Background(), "payments", "tx:runtime-success", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	eventJSON, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(eventJSON, []byte(secretArgument)) {
		t.Fatal("transaction ledger persisted raw supervised command arguments")
	}
	if len(events) != 24 {
		t.Fatalf("events=%d, want 24", len(events))
	}
}

type failTransactionAppendStore struct {
	store.Store
	mu        sync.Mutex
	remaining int
}

func (store *failTransactionAppendStore) failAfter(appends int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.remaining = appends
}

func (store *failTransactionAppendStore) AppendTransactionEvents(
	ctx context.Context,
	namespace string,
	expectedSequence int64,
	events []model.TransactionEvent,
) (transactionreducer.Projection, error) {
	store.mu.Lock()
	if store.remaining > 0 {
		store.remaining--
		if store.remaining == 0 {
			store.mu.Unlock()
			return transactionreducer.Projection{}, &model.KernelError{
				Code: model.ErrorInternal, Operation: "append_transaction_events",
				Message: "injected durable-finalization failure",
			}
		}
	}
	store.mu.Unlock()
	return store.Store.AppendTransactionEvents(
		ctx, namespace, expectedSequence, events,
	)
}

func TestTransactionRunPersistsFailedAgentWithoutStaging(t *testing.T) {
	repo := transactionRunRepository(t)
	kernelStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	manager, err := gitstage.New()
	if err != nil {
		t.Fatal(err)
	}
	fakeRuntime := writeFakeOCIRuntime(t)
	handler := kernelapi.NewServer(
		kernelStore,
		kernelapi.WithTransactionRuntime(
			manager, repo, t.TempDir(), transactionreducer.BaselinePolicy{},
		),
		kernelapi.WithExecutionRuntimePolicy(verifierExecutionPolicy(fakeRuntime)),
	).Handler()
	newClient := func(string) transactionClient {
		return kernelclient.NewWithTransport(handlerTransport{handler: handler})
	}
	stdout, stderr, code := invokeTransactionRunCLI(
		repo, newClient, true,
		"run",
		"--id", "tx:runtime-failure",
		"--namespace", "payments",
		"--intent", "Attempt a change",
		"--run", "run:runtime-failure",
		"--runtime", "host",
		"--unsafe-host",
		"--",
		"/bin/sh", "-c", "exit 7",
	)
	if code != 7 {
		t.Fatalf("tx run code=%d, want 7; stderr=%s", code, stderr)
	}
	var result transactionRunResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode tx run output: %v\n%s", err, stdout)
	}
	if result.Execution.Status != model.AgentExecutionFailed ||
		result.Execution.ExitCode == nil || *result.Execution.ExitCode != 7 {
		t.Fatalf("unexpected failed receipt: %#v", result.Execution)
	}
	if result.Projection.Transaction.State != model.TransactionRunning ||
		len(result.Projection.Effects) != 0 || result.Decision != nil {
		t.Fatalf("failed execution staged effects: %#v", result)
	}
}

func TestTransactionRunDaemonOCIProvidesPersistedTaskEnvelope(t *testing.T) {
	repo := transactionRunRepository(t)
	kernelStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	manager, err := gitstage.New()
	if err != nil {
		t.Fatal(err)
	}
	fakeRuntime := writeFakeOCIRuntime(t)
	executionPolicy := verifierExecutionPolicy(fakeRuntime)
	executionPolicy.MaxAgentTimeoutSecs = 60
	handler := kernelapi.NewServer(
		kernelStore,
		kernelapi.WithTransactionRuntime(
			manager, repo, t.TempDir(), transactionreducer.BaselinePolicy{},
		),
		kernelapi.WithExecutionRuntimePolicy(executionPolicy),
	).Handler()
	newClient := func(string) transactionClient {
		return kernelclient.NewWithTransport(handlerTransport{handler: handler})
	}
	intent := "Change authentication through the mounted Vouch task"
	image := "registry.example.invalid/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	script := "test -r \"$VOUCH_TASK_PATH\" && " +
		"test -n \"$VOUCH_TASK_DIGEST\" && " +
		"grep -Fq '\"intent\":\"" + intent + "\"' \"$VOUCH_TASK_PATH\" && " +
		"printf 'package auth\\n\\nfunc Allowed() bool { return true }\\n' > internal/auth/middleware.go"
	stdout, stderr, code := invokeTransactionRunCLI(
		repo, newClient, true,
		"run",
		"--id", "tx:task-envelope-oci",
		"--namespace", "payments",
		"--intent", intent,
		"--run", "run:task-envelope-oci",
		"--runtime", "oci",
		"--image", image,
		"--timeout", "30s",
		"--",
		"/bin/sh", "-c", script,
	)
	if code != 0 {
		t.Fatalf("tx run code=%d stderr=%s", code, stderr)
	}
	var result transactionRunResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode tx run output: %v\n%s", err, stdout)
	}
	if result.Projection.Transaction.Task == nil {
		t.Fatal("OCI transaction did not retain its task envelope")
	}
	if result.Execution.Status != model.AgentExecutionSucceeded ||
		result.Execution.TaskDigest != result.Projection.Transaction.Task.Digest {
		t.Fatalf("execution was not bound to the persisted task: %#v", result.Execution)
	}
	if len(result.Projection.Effects) != 1 {
		t.Fatalf("effects=%d, want one task-driven source change", len(result.Projection.Effects))
	}
}

func TestTransactionRunNamedProfileEnforcesDeclaredEntrypoint(t *testing.T) {
	repo := transactionRunRepository(t)
	kernelStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	manager, err := gitstage.New()
	if err != nil {
		t.Fatal(err)
	}
	fakeRuntime := writeFakeOCIRuntime(t)
	executionPolicy := verifierExecutionPolicy(fakeRuntime)
	executionPolicy.MaxAgentTimeoutSecs = 60
	handler := kernelapi.NewServer(
		kernelStore,
		kernelapi.WithTransactionRuntime(
			manager, repo, t.TempDir(), transactionreducer.BaselinePolicy{},
		),
		kernelapi.WithExecutionRuntimePolicy(executionPolicy),
	).Handler()
	newClient := func(string) transactionClient {
		return kernelclient.NewWithTransport(handlerTransport{handler: handler})
	}
	profile := validAgentProfileForTest()
	profile.Descriptor.Runtime.Entrypoint = []string{"/bin/sh", "-c"}
	profilesPath := writeAgentProfilesForTest(t, repo, profile)
	script := "test \"$VOUCH_FAKE_ENTRYPOINT\" = /bin/sh && " +
		"printf 'package auth\\n\\nfunc Allowed() bool { return true }\\n' > internal/auth/middleware.go"
	stdout, stderr, code := invokeTransactionRunCLI(
		repo, newClient, true,
		"run",
		"--id", "tx:named-profile-entrypoint",
		"--namespace", "payments",
		"--intent", "Execute exactly the declared named agent profile",
		"--run", "run:named-profile-entrypoint",
		"--agent", "coding-agent",
		"--agent-profiles", profilesPath,
		"--timeout", "30s",
		"--",
		script,
	)
	if code != 0 {
		t.Fatalf("tx run code=%d stderr=%s", code, stderr)
	}
	var result transactionRunResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode tx run output: %v\n%s", err, stdout)
	}
	if result.Projection.Transaction.Task == nil ||
		result.Projection.Transaction.Task.AgentProfile.Entrypoint != "/bin/sh" {
		t.Fatalf("declared entrypoint was not task-bound: %#v", result.Projection.Transaction.Task)
	}
	if result.Execution.Status != model.AgentExecutionSucceeded ||
		len(result.Projection.Effects) != 1 {
		t.Fatalf("named profile did not execute through its entrypoint: %#v", result)
	}
}

func TestProductionRuntimePolicyRejectsHostExecution(t *testing.T) {
	repo := transactionRunRepository(t)
	kernelStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	manager, err := gitstage.New()
	if err != nil {
		t.Fatal(err)
	}
	allowedDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	handler := kernelapi.NewServer(
		kernelStore,
		kernelapi.WithTransactionRuntime(
			manager, repo, t.TempDir(), transactionreducer.BaselinePolicy{},
		),
		kernelapi.WithExecutionRuntimePolicy(kernelapi.ExecutionRuntimePolicy{
			AllowHost:           false,
			AllowedImageDigests: map[string]struct{}{allowedDigest: {}},
		}),
	).Handler()
	newClient := func(string) transactionClient {
		return kernelclient.NewWithTransport(handlerTransport{handler: handler})
	}
	_, stderr, code := invokeTransactionRunCLI(
		repo, newClient, true,
		"run",
		"--id", "tx:production-host-denied",
		"--namespace", "payments",
		"--intent", "Attempt unsafe host execution",
		"--run", "run:production-host-denied",
		"--runtime", "host",
		"--unsafe-host",
		"--",
		"/bin/sh", "-c", "exit 0",
	)
	if code != 1 || !strings.Contains(stderr, string(model.ErrorCapabilityDenied)) {
		t.Fatalf("host execution was not rejected: code=%d stderr=%s", code, stderr)
	}
	projection, err := newClient("").GetTransaction(
		context.Background(), "payments", "tx:production-host-denied",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Executions) != 0 ||
		projection.Transaction.State != model.TransactionRunning {
		t.Fatalf("rejected host execution changed authority: %#v", projection)
	}
}

func TestVerificationUsesReadOnlySnapshotAndRecordsMutationAttempt(t *testing.T) {
	repo := transactionRunRepository(t)
	kernelStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	manager, err := gitstage.New()
	if err != nil {
		t.Fatal(err)
	}
	fakeRuntime := writeFakeOCIRuntime(t)
	stagingRoot := t.TempDir()
	handler := kernelapi.NewServer(
		kernelStore,
		kernelapi.WithTransactionRuntime(
			manager, repo, stagingRoot, transactionreducer.BaselinePolicy{},
		),
		kernelapi.WithExecutionRuntimePolicy(verifierExecutionPolicy(fakeRuntime)),
	).Handler()
	newClient := func(string) transactionClient {
		return kernelclient.NewWithTransport(handlerTransport{handler: handler})
	}
	_, stderr, code := invokeTransactionRunCLI(
		repo, newClient, true,
		"run",
		"--id", "tx:verifier-tamper",
		"--namespace", "payments",
		"--intent", "Modify authentication implementation",
		"--run", "run:verifier-tamper",
		"--runtime", "host",
		"--unsafe-host",
		"--",
		"/bin/sh", "-c",
		"printf 'package auth\\n\\nfunc Allowed() bool { return true }\\n' > internal/auth/middleware.go",
	)
	if code != 0 {
		t.Fatalf("prepare transaction code=%d stderr=%s", code, stderr)
	}
	pinnedVerifier := "registry.example.invalid/verifier@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, stderr, code = invokeTransactionRunCLI(
		repo, newClient, true,
		"verify",
		"--namespace", "payments",
		"--id", "tx:verifier-tamper",
		"--name", "malicious-verifier",
		"--image", pinnedVerifier,
		"--",
		"/bin/sh", "-c",
		"printf 'tampered\\n' > internal/auth/middleware.go",
	)
	if code != 1 {
		t.Fatalf("mutating verifier did not fail: code=%d stderr=%s", code, stderr)
	}
	projection, err := newClient("").GetTransaction(
		context.Background(), "payments", "tx:verifier-tamper",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Verifications) != 1 ||
		projection.Verifications[0].Status != model.VerificationFailed ||
		projection.Transaction.State != model.TransactionValidationFailed {
		t.Fatalf("read-only verifier failure was not recorded: %#v", projection)
	}
	workspacePath := projection.Transaction.StageBindings[0].Location
	content, err := os.ReadFile(filepath.Join(workspacePath, "internal", "auth", "middleware.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte("return true")) ||
		bytes.Contains(content, []byte("tampered")) {
		t.Fatalf("verifier mutation escaped its read-only snapshot: %q", content)
	}
	sourceContent, err := os.ReadFile(filepath.Join(repo, "internal", "auth", "middleware.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(sourceContent, []byte("return false")) {
		t.Fatalf("transaction or verifier mutated the source worktree: %q", sourceContent)
	}
	materializationRoot := filepath.Join(stagingRoot, ".vouch-verifier-trees")
	entries, err := os.ReadDir(materializationRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("verifier snapshot was not cleaned up: %#v", entries)
	}
}

func transactionRunRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	auth := filepath.Join(repo, "internal", "auth")
	if err := os.MkdirAll(auth, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(auth, "middleware.go"),
		[]byte("package auth\n\nfunc Allowed() bool { return false }\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(auth, "middleware_test.go"),
		[]byte("package auth\n\nfunc TestAllowed() {}\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	runGitForTransactionTest(t, repo, "init", "--initial-branch=main")
	runGitForTransactionTest(t, repo, "add", "--", "internal/auth/middleware.go", "internal/auth/middleware_test.go")
	runGitForTransactionTest(
		t, repo,
		"-c", "user.name=Vouch Test",
		"-c", "user.email=vouch@example.invalid",
		"commit", "-m", "fixture",
	)
	return repo
}

func runGitForTransactionTest(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func runGitForTransactionTestOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func writeFakeOCIRuntime(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-oci-runtime")
	script := `#!/bin/sh
if [ "$1" = "rm" ]; then
  exit 0
fi
workspace=
task_directory=
task_digest=
entrypoint=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --mount=type=bind,src=*,dst=/workspace*)
      workspace=${1#--mount=type=bind,src=}
      workspace=${workspace%%,dst=/workspace*}
      shift
      ;;
    --mount=type=bind,src=*,dst=/vouch,readonly)
      task_directory=${1#--mount=type=bind,src=}
      task_directory=${task_directory%%,dst=/vouch,readonly}
      shift
      ;;
    --env=VOUCH_TASK_DIGEST=*)
      task_digest=${1#--env=VOUCH_TASK_DIGEST=}
      shift
      ;;
    --entrypoint=*)
      entrypoint=${1#--entrypoint=}
      shift
      ;;
    run|--*)
      shift
      ;;
    *)
      shift
      break
      ;;
  esac
done
if [ -z "$workspace" ]; then
  exit 125
fi
if [ -n "$task_directory" ]; then
  VOUCH_TASK_PATH=$task_directory/task.json
  VOUCH_TASK_DIGEST=$task_digest
  export VOUCH_TASK_PATH VOUCH_TASK_DIGEST
fi
cd "$workspace" || exit 125
if [ -n "$entrypoint" ]; then
  VOUCH_FAKE_ENTRYPOINT=$entrypoint
  export VOUCH_FAKE_ENTRYPOINT
  exec "$entrypoint" "$@"
fi
exec "$@"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func verifierExecutionPolicy(enginePath string) kernelapi.ExecutionRuntimePolicy {
	return kernelapi.ExecutionRuntimePolicy{
		AllowHost:                  true,
		AllowExternalExecution:     true,
		AllowExternalVerification:  false,
		EnginePath:                 enginePath,
		VerifierUID:                1000,
		VerifierGID:                1000,
		VerifierMemoryBytes:        4 << 30,
		VerifierCPUMillis:          2000,
		VerifierPIDsLimit:          256,
		VerifierTmpfsBytes:         1 << 30,
		MaxVerificationTimeoutSecs: 15 * 60,
	}
}

func invokeTransactionRunCLI(
	repo string,
	newClient transactionClientFactory,
	jsonOut bool,
	args ...string,
) (string, string, int) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := transactionCommandWithFactory(
		repo, args, jsonOut, &stdout, &stderr, newClient,
	)
	return stdout.String(), stderr.String(), code
}
