package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/approval"
	"github.com/duriantaco/vouch/internal/kernel/identity"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/sandbox"
	"github.com/duriantaco/vouch/internal/kernel/store"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

func TestRunRefusesAndPreservesExistingSocketPath(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "vouchd.sock")
	if err := os.WriteFile(socket, []byte("owned by another process"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Config{
		DatabasePath:    filepath.Join(dir, "kernel.db"),
		SocketPath:      socket,
		TransactionRoot: filepath.Join(dir, "transactions"),
	})
	if err == nil || !strings.Contains(err.Error(), "exists and is not a socket") {
		t.Fatalf("expected existing socket error, got %v", err)
	}
	data, readErr := os.ReadFile(socket)
	if readErr != nil {
		t.Fatalf("existing socket path was removed: %v", readErr)
	}
	if string(data) != "owned by another process" {
		t.Fatalf("existing socket path was modified: %q", data)
	}
}

func TestPrepareSocketPathRemovesOnlyStaleUnixSocket(t *testing.T) {
	socket := shortSocketPath(t, "stale.sock")
	listener, err := net.ListenUnix(
		"unix",
		&net.UnixAddr{Name: socket, Net: "unix"},
	)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("sandbox does not permit Unix sockets: %v", err)
		}
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepareSocketPath(socket); err != nil {
		t.Fatalf("stale Unix socket was not recovered: %v", err)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale socket remains after recovery: %v", err)
	}
}

func TestPrepareSocketPathPreservesActiveUnixSocket(t *testing.T) {
	socket := shortSocketPath(t, "active.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("sandbox does not permit Unix sockets: %v", err)
		}
		t.Fatal(err)
	}
	defer listener.Close()
	if err := prepareSocketPath(socket); err == nil ||
		!strings.Contains(err.Error(), "active listener") {
		t.Fatalf("active Unix socket was not preserved: %v", err)
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("active socket was removed: %v", err)
	}
}

func TestPrepareSocketPathPreservesSocketOnIndeterminateDial(t *testing.T) {
	socket := shortSocketPath(t, "indeterminate.sock")
	listener, err := net.ListenUnix(
		"unix",
		&net.UnixAddr{Name: socket, Net: "unix"},
	)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("sandbox does not permit Unix sockets: %v", err)
		}
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	err = prepareSocketPathWithDial(
		socket,
		func(string, string, time.Duration) (net.Conn, error) {
			return nil, syscall.EACCES
		},
	)
	if err == nil || !strings.Contains(err.Error(), "preserving") {
		t.Fatalf("indeterminate socket dial was accepted: %v", err)
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("socket was removed after indeterminate dial: %v", err)
	}
}

func TestOwnedSocketCleanupPreservesReplacementPath(t *testing.T) {
	socket := shortSocketPath(t, "replacement.sock")
	listener, err := net.ListenUnix(
		"unix",
		&net.UnixAddr{Name: socket, Net: "unix"},
	)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("sandbox does not permit Unix sockets: %v", err)
		}
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	owned, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedSocket(socket, owned); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(socket)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("replacement path was removed or changed: %q, %v", data, err)
	}
}

func shortSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "vouch-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

func TestRunValidatesRequiredPaths(t *testing.T) {
	for name, config := range map[string]Config{
		"database":         {SocketPath: "socket"},
		"socket":           {DatabasePath: "kernel.db"},
		"transaction root": {DatabasePath: "kernel.db", SocketPath: "socket"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Run(context.Background(), config); err == nil {
				t.Fatal("expected required path error")
			}
		})
	}
}

func TestProductionRuntimePolicyFailsClosed(t *testing.T) {
	if _, err := executionRuntimePolicy("production", nil); err == nil {
		t.Fatal("production runtime accepted an empty image allowlist")
	}
	if _, err := executionRuntimePolicy("production", []string{"agent:latest"}); err == nil {
		t.Fatal("production runtime accepted a mutable image tag")
	}
	reference := "registry.example.invalid/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	policy, err := executionRuntimePolicy("production", []string{reference})
	if err != nil {
		t.Fatal(err)
	}
	if policy.AllowHost {
		t.Fatal("production runtime allowed host execution")
	}
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, allowed := policy.AllowedImageDigests[digest]; !allowed {
		t.Fatalf("production image digest was not normalized: %#v", policy.AllowedImageDigests)
	}
}

func TestProductionWorkloadIdentityMustMatchNonRootDaemon(t *testing.T) {
	if err := validateWorkloadIdentity("production", 1001, 1001, 1000, 1000); err == nil {
		t.Fatal("production accepted a bind-mounted workload owned by another UID")
	}
	if err := validateWorkloadIdentity("production", 1000, 1000, 1000, 1000); err != nil {
		t.Fatalf("production rejected matching daemon/workload identity: %v", err)
	}
	if err := validateWorkloadIdentity("production", 1000, 1000, 0, 0); err != nil {
		t.Fatalf("root daemon could not configure a non-root workload: %v", err)
	}
	if err := validateWorkloadIdentity("production", 0, 1000, 0, 0); err == nil {
		t.Fatal("production accepted a root workload")
	}
}

func TestProductionConfigFilesRejectSymlinkAndWritableTrust(t *testing.T) {
	dir := t.TempDir()
	writable := filepath.Join(dir, "writable.json")
	if err := os.WriteFile(writable, []byte("{}"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := validateProductionConfigFile(writable, "trust"); err == nil {
		t.Fatal("production accepted a group/world-writable trust file")
	}
	secure := filepath.Join(dir, "secure.json")
	if err := os.WriteFile(secure, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "trust-link.json")
	if err := os.Symlink(secure, link); err != nil {
		t.Fatal(err)
	}
	if err := validateProductionConfigFile(link, "trust"); err == nil {
		t.Fatal("production accepted a symlinked trust file")
	}
	if err := validateProductionConfigFile(secure, "trust"); err != nil {
		t.Fatalf("production rejected a secure regular trust file: %v", err)
	}
}

func TestDevelopmentRuntimePolicyIsExplicitlyPermissive(t *testing.T) {
	policy, err := executionRuntimePolicy("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.AllowHost {
		t.Fatal("development profile unexpectedly rejected host execution")
	}
}

func TestProductionProfileRejectsUnsafeHostExecution(t *testing.T) {
	dir := t.TempDir()
	err := Run(context.Background(), Config{
		DatabasePath:             filepath.Join(dir, "kernel.db"),
		SocketPath:               filepath.Join(dir, "vouchd.sock"),
		TransactionRoot:          filepath.Join(dir, "transactions"),
		RuntimeProfile:           "production",
		AllowUnsafeHostExecution: true,
		AllowedImages: []string{
			"registry.example.invalid/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "forbids client-supervised host execution") {
		t.Fatalf("production accepted unsafe host execution: %v", err)
	}
}

func TestProductionProfileRequiresApprovalTrust(t *testing.T) {
	dir := t.TempDir()
	workloadUID, workloadGID := testProductionWorkloadIdentity()
	err := Run(context.Background(), Config{
		DatabasePath:    filepath.Join(dir, "kernel.db"),
		SocketPath:      filepath.Join(dir, "vouchd.sock"),
		TransactionRoot: filepath.Join(dir, "transactions"),
		RuntimeProfile:  "production",
		VerifierUID:     workloadUID,
		VerifierGID:     workloadGID,
		AllowedImages: []string{
			"registry.example.invalid/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "trusted approval key") {
		t.Fatalf("production daemon accepted no approval trust roots: %v", err)
	}
}

func TestProductionProfileRequiresOIDCIdentityTrust(t *testing.T) {
	dir := t.TempDir()
	workloadUID, workloadGID := testProductionWorkloadIdentity()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trustData, err := json.Marshal(approval.TrustDocument{
		Version: approval.TrustDocumentVersion,
		Keys: []approval.TrustDocumentKey{{
			KeyID: "key:reviewer", PublicKeyBase64: base64.StdEncoding.EncodeToString(publicKey),
			Principal: model.Principal{
				ID: "human:reviewer", Kind: model.PrincipalHuman,
			},
			ApprovalClasses: []string{"security-reviewer"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	approvalTrust := filepath.Join(dir, "approval-trust.json")
	if err := os.WriteFile(approvalTrust, trustData, 0o600); err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), Config{
		DatabasePath:      filepath.Join(dir, "kernel.db"),
		SocketPath:        filepath.Join(dir, "vouchd.sock"),
		TransactionRoot:   filepath.Join(dir, "transactions"),
		RuntimeProfile:    "production",
		VerifierUID:       workloadUID,
		VerifierGID:       workloadGID,
		ApprovalTrustFile: approvalTrust,
		AllowedImages: []string{
			"registry.example.invalid/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "OIDC identity trust") {
		t.Fatalf("production daemon accepted no OIDC identity trust: %v", err)
	}
}

func TestProductionProfileRequiresDaemonOwnedVerifierProfiles(t *testing.T) {
	dir := t.TempDir()
	workloadUID, workloadGID := testProductionWorkloadIdentity()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approvalData, err := json.Marshal(approval.TrustDocument{
		Version: approval.TrustDocumentVersion,
		Keys: []approval.TrustDocumentKey{{
			KeyID:           "key:reviewer",
			PublicKeyBase64: base64.StdEncoding.EncodeToString(publicKey),
			Principal: model.Principal{
				ID: "human:reviewer", Kind: model.PrincipalHuman,
			},
			ApprovalClasses: []string{"security-reviewer"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	approvalTrust := filepath.Join(dir, "approval-trust.json")
	if err := os.WriteFile(approvalTrust, approvalData, 0o600); err != nil {
		t.Fatal(err)
	}
	jwk, err := identity.Ed25519JWK("key:identity", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	identityData, err := json.Marshal(identity.TrustDocument{
		Version:                 identity.TrustDocumentVersion,
		Issuer:                  "https://identity.example.invalid/",
		Audiences:               []string{"vouch-production"},
		MaxTokenLifetimeSeconds: 3600,
		JWKS:                    identity.JWKS{Keys: []identity.JWK{jwk}},
	})
	if err != nil {
		t.Fatal(err)
	}
	identityTrust := filepath.Join(dir, "identity-trust.json")
	if err := os.WriteFile(identityTrust, identityData, 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeEngine, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), Config{
		DatabasePath:      filepath.Join(dir, "kernel.db"),
		SocketPath:        filepath.Join(dir, "vouchd.sock"),
		TransactionRoot:   filepath.Join(dir, "transactions"),
		RuntimeProfile:    "production",
		RuntimeEngine:     runtimeEngine,
		VerifierUID:       workloadUID,
		VerifierGID:       workloadGID,
		ApprovalTrustFile: approvalTrust,
		IdentityTrustFile: identityTrust,
		AllowedGitRefs:    []string{"refs/heads/release/*"},
		AllowedImages: []string{
			"registry.example.invalid/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "daemon-owned verifier profiles") {
		t.Fatalf("production daemon accepted no verifier profiles: %v", err)
	}
}

func TestProductionReleasePolicyFailsClosed(t *testing.T) {
	if _, err := releaseRuntimePolicy("production", nil); err == nil {
		t.Fatal("production release policy accepted an empty Git ref allowlist")
	}
	if _, err := releaseRuntimePolicy("production", []string{"main"}); err == nil {
		t.Fatal("production release policy accepted a short branch name")
	}
	policy, err := releaseRuntimePolicy(
		"production", []string{"refs/heads/release/*"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.AllowsGitRef("refs/heads/release/2026-07") ||
		policy.AllowsGitRef("refs/heads/main") {
		t.Fatalf("unexpected release policy matching: %#v", policy)
	}
}

func TestRecoverAgentExecutionsCleansContainerAndPersistsInterruptedReceipt(t *testing.T) {
	ctx := context.Background()
	kernelStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	projection := createActiveExecution(t, kernelStore)
	var cleaned []string
	recoveredAt := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	recovered, err := recoverAgentExecutions(
		ctx,
		kernelStore,
		func(_ context.Context, transactionID, runID string) error {
			cleaned = append(cleaned, sandbox.ContainerName(transactionID, runID))
			return nil
		},
		func() time.Time { return recoveredAt },
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered=%d, want 1", recovered)
	}
	wantContainer := sandbox.ContainerName(
		projection.Transaction.ID,
		projection.Executions[0].RunID,
	)
	if len(cleaned) != 1 || cleaned[0] != wantContainer {
		t.Fatalf("cleaned=%v, want [%s]", cleaned, wantContainer)
	}
	restored, err := kernelStore.GetTransaction(
		ctx,
		projection.Transaction.Namespace,
		projection.Transaction.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	execution := restored.Executions[len(restored.Executions)-1]
	if execution.Status != model.AgentExecutionInterrupted ||
		execution.CompletedAt == nil ||
		!execution.CompletedAt.Equal(recoveredAt) ||
		execution.StdoutDigest != emptySHA256Digest ||
		execution.StderrDigest != emptySHA256Digest {
		t.Fatalf("unexpected recovered execution: %#v", execution)
	}
	if err := kernelStore.VerifyTransaction(
		ctx,
		projection.Transaction.Namespace,
		projection.Transaction.ID,
	); err != nil {
		t.Fatalf("verify recovered transaction: %v", err)
	}
	recovered, err = recoverAgentExecutions(
		ctx,
		kernelStore,
		func(_ context.Context, _, _ string) error {
			t.Fatal("terminal execution was cleaned twice")
			return nil
		},
		time.Now,
	)
	if err != nil || recovered != 0 {
		t.Fatalf("second recovery=(%d, %v), want (0, nil)", recovered, err)
	}
}

func TestRecoverAgentExecutionsFailsClosedBeforeChangingLedger(t *testing.T) {
	ctx := context.Background()
	kernelStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	projection := createActiveExecution(t, kernelStore)
	_, err = recoverAgentExecutions(
		ctx,
		kernelStore,
		func(context.Context, string, string) error { return os.ErrPermission },
		time.Now,
	)
	if err == nil {
		t.Fatal("recovery accepted a failed container cleanup")
	}
	restored, err := kernelStore.GetTransaction(
		ctx,
		projection.Transaction.Namespace,
		projection.Transaction.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Executions[len(restored.Executions)-1].Status != model.AgentExecutionRunning {
		t.Fatalf("failed cleanup changed the ledger: %#v", restored.Executions)
	}
}

func TestProcessLockRejectsASecondDaemonForTheSameDatabase(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "kernel.db")
	first, err := acquireProcessLock(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := acquireProcessLock(databasePath); err == nil ||
		!strings.Contains(err.Error(), "already owned") {
		t.Fatalf("second process lock was not rejected: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireProcessLock(databasePath)
	if err != nil {
		t.Fatalf("released process lock could not be reacquired: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func createActiveExecution(t *testing.T, kernelStore *store.SQLiteStore) transactionreducer.Projection {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	actor := model.Principal{ID: "human:alice", Kind: model.PrincipalHuman}
	transaction := model.AgentTransaction{
		Version:                model.AgentTransactionVersion,
		ID:                     "tx:daemon-recovery",
		Namespace:              "team-runtime",
		IntentDigest:           testDigest("1"),
		Sponsor:                actor,
		AgentRunIDs:            []string{"run:agent"},
		StageBindings:          []model.StageBinding{},
		State:                  model.TransactionCreated,
		EffectIDs:              []string{},
		VerificationResultIDs:  []string{},
		OutstandingApprovalIDs: []string{},
		EventSequence:          1,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	created, err := transactionreducer.CreationEvent(transaction, actor)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := kernelStore.CreateTransaction(ctx, created)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	started, err := transactionreducer.NextEvent(
		projection,
		transactionreducer.EventTransactionStateChanged,
		actor,
		now,
		transactionreducer.TransactionStateChangedPayload{
			From: model.TransactionCreated,
			To:   model.TransactionRunning,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	projection, err = kernelStore.AppendTransactionEvents(
		ctx,
		transaction.Namespace,
		projection.Transaction.EventSequence,
		[]model.TransactionEvent{started},
	)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	binding := model.StageBinding{
		ID:           "stage:daemon-recovery",
		Kind:         "git_worktree",
		Resource:     model.ResourceSelector{Kind: "git_repository", Pattern: "/tmp/repository"},
		Location:     "/tmp/worktree",
		BaseRevision: strings.Repeat("a", 40),
		CreatedAt:    now,
	}
	bound, err := transactionreducer.NextEvent(
		projection,
		transactionreducer.EventStageBindingCreated,
		actor,
		now,
		transactionreducer.StageBindingCreatedPayload{Binding: binding},
	)
	if err != nil {
		t.Fatal(err)
	}
	projection, err = kernelStore.AppendTransactionEvents(
		ctx,
		transaction.Namespace,
		projection.Transaction.EventSequence,
		[]model.TransactionEvent{bound},
	)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	execution := model.AgentExecution{
		Version:             model.AgentExecutionVersion,
		ID:                  transactionreducer.ExecutionID(transaction.ID, projection.Transaction.EventSequence+1),
		TransactionID:       transaction.ID,
		RunID:               "run:agent",
		StageBindingID:      binding.ID,
		Program:             "agent",
		CommandDigest:       testDigest("2"),
		RuntimeClass:        "oci",
		RuntimeConfigDigest: testDigest("3"),
		ImageDigest:         testDigest("4"),
		Status:              model.AgentExecutionRunning,
		StartedAt:           now,
	}
	executionStarted, err := transactionreducer.NextEvent(
		projection,
		transactionreducer.EventAgentExecutionStarted,
		actor,
		now,
		transactionreducer.AgentExecutionStartedPayload{Execution: execution},
	)
	if err != nil {
		t.Fatal(err)
	}
	projection, err = kernelStore.AppendTransactionEvents(
		ctx,
		transaction.Namespace,
		projection.Transaction.EventSequence,
		[]model.TransactionEvent{executionStarted},
	)
	if err != nil {
		t.Fatal(err)
	}
	return projection
}

func testDigest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}

func testProductionWorkloadIdentity() (int, int) {
	if os.Geteuid() == 0 {
		return 1000, 1000
	}
	return os.Geteuid(), os.Getegid()
}
