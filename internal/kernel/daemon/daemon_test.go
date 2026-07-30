package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/approval"
	"github.com/duriantaco/vouch/internal/kernel/identity"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/runtimeidentity"
	"github.com/duriantaco/vouch/internal/kernel/sandbox"
	"github.com/duriantaco/vouch/internal/kernel/store"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

func TestRunRejectsRuntimeMismatchBeforeSocketOrLedgerMutation(t *testing.T) {
	ctx := context.Background()
	repository := t.TempDir()
	initializeDaemonRuntime(t, repository)
	identity, err := runtimeidentity.Load(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	ledgerRuntimeID := "runtime:" + strings.Repeat("f", 64)
	if ledgerRuntimeID == identity.RuntimeID {
		ledgerRuntimeID = "runtime:" + strings.Repeat("e", 64)
	}
	databasePath := filepath.Join(repository, ".gatemole", "kernel.db")
	kernelStore, err := store.OpenSQLiteForRuntime(
		ctx,
		databasePath,
		ledgerRuntimeID,
		store.EnforcementProfileDevelopment,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := kernelStore.Close(); err != nil {
		t.Fatal(err)
	}
	before := daemonSQLiteArtifactsSnapshot(t, databasePath)

	// A dedicated Runtime lock directory keeps this regression independent of
	// host-global daemon state. The database process lock is intentionally not
	// part of the ledger artifact snapshot.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	socketDirectory := filepath.Join(repository, "socket-must-not-exist")
	socketPath := filepath.Join(socketDirectory, "gatemoled.sock")
	if _, err := os.Lstat(socketDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket directory exists before daemon startup: %v", err)
	}

	err = Run(ctx, Config{
		DatabasePath:    databasePath,
		SocketPath:      socketPath,
		RepositoryRoot:  repository,
		TransactionRoot: filepath.Join(t.TempDir(), "transactions"),
		RuntimeEngine:   filepath.Join(repository, "missing-oci-engine"),
	})
	if err == nil || !strings.Contains(err.Error(), "different Runtime instance") {
		t.Fatalf("daemon accepted a ledger bound to another Runtime: %v", err)
	}
	if _, statErr := os.Lstat(socketDirectory); !errors.Is(
		statErr,
		os.ErrNotExist,
	) {
		t.Fatalf(
			"Runtime mismatch created a socket directory or listener: %v",
			statErr,
		)
	}
	after := daemonSQLiteArtifactsSnapshot(t, databasePath)
	if !equalDaemonSQLiteArtifactSnapshots(before, after) {
		t.Fatal("Runtime mismatch changed ledger bytes or sidecars")
	}
}

func TestRunRejectsLegacyVouchStateBeforeSocketOrLedgerMutation(t *testing.T) {
	ctx := context.Background()
	repository := t.TempDir()
	initializeDaemonRuntime(t, repository)
	identityPath := filepath.Join(
		repository,
		filepath.FromSlash(runtimeidentity.IdentityRelativePath),
	)
	identityBefore, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}

	legacyDirectory := filepath.Join(
		repository,
		runtimeidentity.LegacyControlDirectory,
	)
	if err := os.Mkdir(legacyDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	legacySentinel := filepath.Join(legacyDirectory, "sentinel")
	if err := os.WriteFile(
		legacySentinel,
		[]byte("do not rewrite"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	databasePath := filepath.Join(
		repository,
		runtimeidentity.ControlDirectory,
		"kernel.db",
	)
	socketDirectory := filepath.Join(repository, "socket-must-not-exist")
	socketPath := filepath.Join(socketDirectory, "gatemoled.sock")
	err = Run(ctx, Config{
		DatabasePath:    databasePath,
		SocketPath:      socketPath,
		RepositoryRoot:  repository,
		TransactionRoot: filepath.Join(t.TempDir(), "transactions"),
		RuntimeEngine:   filepath.Join(repository, "missing-oci-engine"),
	})
	if err == nil ||
		!strings.Contains(err.Error(), "legacy Vouch control state") {
		t.Fatalf("daemon did not reject legacy state: %v", err)
	}
	if _, statErr := os.Lstat(databasePath); !errors.Is(
		statErr,
		os.ErrNotExist,
	) {
		t.Fatalf("legacy rejection created a ledger: %v", statErr)
	}
	if _, statErr := os.Lstat(socketDirectory); !errors.Is(
		statErr,
		os.ErrNotExist,
	) {
		t.Fatalf("legacy rejection created a socket directory: %v", statErr)
	}
	identityAfter, err := os.ReadFile(identityPath)
	if err != nil || !bytes.Equal(identityBefore, identityAfter) {
		t.Fatalf("legacy rejection changed Runtime identity: err=%v", err)
	}
	legacyAfter, err := os.ReadFile(legacySentinel)
	if err != nil || string(legacyAfter) != "do not rewrite" {
		t.Fatalf("legacy rejection changed legacy state: data=%q err=%v", legacyAfter, err)
	}
}

func daemonSQLiteArtifactsSnapshot(
	t *testing.T,
	databasePath string,
) map[string][]byte {
	t.Helper()
	databaseDirectory := filepath.Dir(databasePath)
	databaseName := filepath.Base(databasePath)
	entries, err := os.ReadDir(databaseDirectory)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := make(map[string][]byte)
	for _, entry := range entries {
		name := entry.Name()
		if name == databaseName+".lock" ||
			(name != databaseName &&
				!strings.HasPrefix(name, databaseName+"-")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(databaseDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		snapshot[name] = data
	}
	return snapshot
}

func equalDaemonSQLiteArtifactSnapshots(
	left map[string][]byte,
	right map[string][]byte,
) bool {
	if len(left) != len(right) {
		return false
	}
	for name, leftData := range left {
		rightData, exists := right[name]
		if !exists || !bytes.Equal(leftData, rightData) {
			return false
		}
	}
	return true
}

func TestRunRefusesAndPreservesExistingSocketPath(t *testing.T) {
	dir := t.TempDir()
	initializeDaemonRuntime(t, dir)
	socket := filepath.Join(dir, "gatemoled.sock")
	if err := os.WriteFile(socket, []byte("owned by another process"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Config{
		DatabasePath:    filepath.Join(dir, "kernel.db"),
		SocketPath:      socket,
		RepositoryRoot:  dir,
		TransactionRoot: filepath.Join(t.TempDir(), "transactions"),
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

func TestPeerAuthenticatedUnixListenerAcceptsOnlyDaemonUID(t *testing.T) {
	socket := shortSocketPath(t, "peer-auth.sock")
	listener, err := net.ListenUnix(
		"unix",
		&net.UnixAddr{Name: socket, Net: "unix"},
	)
	if err != nil {
		t.Skipf("sandbox does not permit Unix sockets: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	authenticated := &peerAuthenticatedUnixListener{
		UnixListener: listener,
		expectedUID:  uint32(os.Geteuid()),
	}
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		connection, err := authenticated.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- connection
	}()

	client, err := net.DialUnix(
		"unix",
		nil,
		&net.UnixAddr{Name: socket, Net: "unix"},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	select {
	case connection := <-accepted:
		_ = connection.Close()
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("same-UID Unix client was not accepted")
	}

	rejectSocket := shortSocketPath(t, "peer-reject.sock")
	rejectListener, err := net.ListenUnix(
		"unix",
		&net.UnixAddr{Name: rejectSocket, Net: "unix"},
	)
	if err != nil {
		t.Skipf("sandbox does not permit Unix sockets: %v", err)
	}
	t.Cleanup(func() { _ = rejectListener.Close() })
	if err := rejectListener.SetDeadline(
		time.Now().Add(250 * time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	rejecting := &peerAuthenticatedUnixListener{
		UnixListener: rejectListener,
		expectedUID:  uint32(os.Geteuid()) + 1,
	}
	rejected := make(chan error, 1)
	go func() {
		connection, err := rejecting.Accept()
		if connection != nil {
			_ = connection.Close()
			rejected <- errors.New("wrong-UID client reached the HTTP listener")
			return
		}
		rejected <- err
	}()
	wrongClient, err := net.DialUnix(
		"unix",
		nil,
		&net.UnixAddr{Name: rejectSocket, Net: "unix"},
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = wrongClient.Close()
	if err := <-rejected; err == nil {
		t.Fatal("wrong-UID Unix client was accepted")
	} else {
		var networkErr net.Error
		if !errors.As(err, &networkErr) || !networkErr.Timeout() {
			t.Fatalf("wrong-UID rejection ended unexpectedly: %v", err)
		}
	}
}

func TestEnsurePrivateSocketDirectoryRejectsUnsafePaths(t *testing.T) {
	safeDirectory := filepath.Join(t.TempDir(), "safe")
	if err := ensurePrivateSocketDirectory(
		filepath.Join(safeDirectory, "gatemoled.sock"),
	); err != nil {
		t.Fatalf("private socket directory was rejected: %v", err)
	}
	info, err := os.Lstat(safeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		t.Fatalf("created socket directory is unsafe: %v", info.Mode())
	}

	writableDirectory := filepath.Join(t.TempDir(), "writable")
	if err := os.Mkdir(writableDirectory, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writableDirectory, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateSocketDirectory(
		filepath.Join(writableDirectory, "gatemoled.sock"),
	); err == nil || !strings.Contains(err.Error(), "group- or world-writable") {
		t.Fatalf("writable socket directory was accepted: %v", err)
	}

	target := t.TempDir()
	symlinkDirectory := filepath.Join(t.TempDir(), "socket-link")
	if err := os.Symlink(target, symlinkDirectory); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateSocketDirectory(
		filepath.Join(symlinkDirectory, "gatemoled.sock"),
	); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink socket directory was accepted: %v", err)
	}
}

func initializeDaemonRuntime(t *testing.T, repository string) {
	t.Helper()
	command := exec.Command("git", "-C", repository, "init", "--quiet")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	excludePath := filepath.Join(repository, ".git", "info", "exclude")
	file, err := os.OpenFile(excludePath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n/.gatemole/runtime.json\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtimeidentity.CreateOrLoad(
		context.Background(),
		repository,
	); err != nil {
		t.Fatalf("create Runtime identity: %v", err)
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
	dir, err := os.MkdirTemp("/tmp", "gatemole-daemon-")
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

func TestProductionConfigSnapshotsRejectSymlinkAndWritableTrust(t *testing.T) {
	dir := t.TempDir()
	writable := filepath.Join(dir, "writable.json")
	if err := os.WriteFile(writable, []byte("{}"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigFileSnapshot(
		writable,
		"trust",
		true,
	); err == nil {
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
	if _, err := readConfigFileSnapshot(link, "trust", true); err == nil {
		t.Fatal("production accepted a symlinked trust file")
	}
	if _, err := readConfigFileSnapshot(
		secure,
		"trust",
		true,
	); err != nil {
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
		SocketPath:               filepath.Join(dir, "gatemoled.sock"),
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
		SocketPath:      filepath.Join(dir, "gatemoled.sock"),
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
		SocketPath:        filepath.Join(dir, "gatemoled.sock"),
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
		Audiences:               []string{"gatemole-production"},
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
		SocketPath:        filepath.Join(dir, "gatemoled.sock"),
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

func TestRuntimeLockRejectsSameRuntimeAcrossDifferentLedgers(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".gatemole"), 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeID := "runtime:" + strings.Repeat("8", 64)
	first, err := acquireRuntimeLock(repository, runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	// Environment-dependent process state and separate database paths must not
	// change the single lock identity of this repository Runtime.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	firstDatabaseLock, err := acquireProcessLock(
		filepath.Join(t.TempDir(), "first.db"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstDatabaseLock.Close() })
	secondDatabaseLock, err := acquireProcessLock(
		filepath.Join(t.TempDir(), "second.db"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondDatabaseLock.Close() })

	if _, err := acquireRuntimeLock(repository, runtimeID); err == nil ||
		!strings.Contains(err.Error(), "already owned") {
		t.Fatalf("second ledger reused a live Runtime identity: %v", err)
	}
	info, err := os.Lstat(filepath.Join(repository, ".gatemole", "runtime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("repository Runtime lock is unsafe: %v", info.Mode())
	}
	other, err := acquireRuntimeLock(
		repository,
		"runtime:"+strings.Repeat("9", 64),
	)
	if err == nil || !strings.Contains(err.Error(), "already owned") {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf(
			"same repository acquired a second Runtime identity lock: %v",
			err,
		)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := acquireRuntimeLock(repository, runtimeID)
	if err != nil {
		t.Fatalf("released Runtime identity could not restart: %v", err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeLockDirectoryRejectsUnsafeOwnershipAndPermissions(t *testing.T) {
	directory := t.TempDir()
	info, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeLockDirectory(
		info,
		uint32(os.Geteuid()),
	); err != nil {
		t.Fatalf("daemon-owned Runtime lock directory was rejected: %v", err)
	}
	otherUID := uint32(os.Geteuid()) + 1
	if err := validateRuntimeLockDirectory(info, otherUID); err == nil ||
		!strings.Contains(err.Error(), "not owned") {
		t.Fatalf("foreign-owned Runtime lock directory was accepted: %v", err)
	}

	if err := os.Chmod(directory, 0o770); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeLockDirectory(
		info,
		uint32(os.Geteuid()),
	); err == nil || !strings.Contains(err.Error(), "group- or world-writable") {
		t.Fatalf("group-writable Runtime lock directory was accepted: %v", err)
	}

	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeLockDirectory(
		info,
		uint32(os.Geteuid()),
	); err == nil || !strings.Contains(err.Error(), "writable and searchable") {
		t.Fatalf("unwritable Runtime lock directory was accepted: %v", err)
	}

	regularFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(regularFile, []byte("not a lock directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(regularFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeLockDirectory(
		info,
		uint32(os.Geteuid()),
	); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("regular file Runtime lock directory was accepted: %v", err)
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
