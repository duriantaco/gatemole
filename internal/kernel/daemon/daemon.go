package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	kernelapi "github.com/duriantaco/gatemole/internal/kernel/api"
	"github.com/duriantaco/gatemole/internal/kernel/approval"
	"github.com/duriantaco/gatemole/internal/kernel/broker"
	"github.com/duriantaco/gatemole/internal/kernel/identity"
	"github.com/duriantaco/gatemole/internal/kernel/modelbroker"
	"github.com/duriantaco/gatemole/internal/kernel/peercred"
	"github.com/duriantaco/gatemole/internal/kernel/runtimeidentity"
	"github.com/duriantaco/gatemole/internal/kernel/sandbox"
	"github.com/duriantaco/gatemole/internal/kernel/store"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
	"github.com/duriantaco/gatemole/internal/kernel/transaction/gitstage"
	"github.com/duriantaco/gatemole/internal/kernel/verification"
)

type Config struct {
	DatabasePath             string
	SocketPath               string
	RepositoryRoot           string
	TransactionRoot          string
	RuntimeProfile           string
	AllowedImages            []string
	ApprovalTrustFile        string
	AllowedGitRefs           []string
	RuntimeEngine            string
	VerifierUID              int
	VerifierGID              int
	ModelBrokerImage         string
	ModelBrokerPolicy        string
	ModelTokenEnv            string
	IdentityTrustFile        string
	VerifierProfilesFile     string
	AllowUnsafeHostExecution bool
	Stdout                   io.Writer
}

// Run serves the local kernel until the context is cancelled. After acquiring
// the ledger process lock it recovers a stale Unix socket, preserves active or
// non-socket paths, and only removes the socket it owns.
func Run(ctx context.Context, config Config) error {
	if strings.TrimSpace(config.DatabasePath) == "" {
		return errors.New("gatemoled: database path is required")
	}
	if strings.TrimSpace(config.SocketPath) == "" {
		return errors.New("gatemoled: socket path is required")
	}
	if strings.TrimSpace(config.TransactionRoot) == "" {
		return errors.New("gatemoled: transaction staging root is required")
	}
	executionPolicy, err := executionRuntimePolicy(config.RuntimeProfile, config.AllowedImages)
	if err != nil {
		return err
	}
	enforcementProfile := config.RuntimeProfile
	if enforcementProfile == "" {
		enforcementProfile = "development"
	}
	executionPolicy.EnforcementProfile = enforcementProfile
	if config.AllowUnsafeHostExecution && config.RuntimeProfile == "production" {
		return errors.New("gatemoled: production profile forbids client-supervised host execution")
	}
	runtimeEngine := strings.TrimSpace(config.RuntimeEngine)
	if runtimeEngine == "" {
		runtimeEngine = "docker"
	}
	enginePath, engineErr := exec.LookPath(runtimeEngine)
	if engineErr != nil && config.RuntimeProfile == "production" {
		return fmt.Errorf("gatemoled: find production OCI engine %q: %w", runtimeEngine, engineErr)
	}
	if engineErr == nil {
		enginePath, err = filepath.Abs(enginePath)
		if err != nil {
			return fmt.Errorf("gatemoled: resolve OCI engine: %w", err)
		}
		executionPolicy.EnginePath = enginePath
	}
	verifierUID := config.VerifierUID
	verifierGID := config.VerifierGID
	if verifierUID == 0 && config.RuntimeProfile != "production" {
		verifierUID = os.Getuid()
	}
	if verifierGID == 0 && config.RuntimeProfile != "production" {
		verifierGID = os.Getgid()
	}
	if err := validateWorkloadIdentity(
		config.RuntimeProfile,
		verifierUID,
		verifierGID,
		os.Geteuid(),
		os.Getegid(),
	); err != nil {
		return err
	}
	executionPolicy.AllowExternalVerification = false
	executionPolicy.AllowExternalExecution = config.AllowUnsafeHostExecution
	executionPolicy.VerifierUID = verifierUID
	executionPolicy.VerifierGID = verifierGID
	executionPolicy.VerifierMemoryBytes = 4 << 30
	executionPolicy.VerifierCPUMillis = 2000
	executionPolicy.VerifierPIDsLimit = 256
	executionPolicy.VerifierTmpfsBytes = 1 << 30
	executionPolicy.MaxVerificationTimeoutSecs = 15 * 60
	executionPolicy.MaxAgentTimeoutSecs = 30 * 60
	executionPolicy.MaxConcurrentWorkloads = 2
	if err := configureModelBroker(config, &executionPolicy); err != nil {
		return err
	}
	production := config.RuntimeProfile == "production"
	approvalTrust := approval.EmptyTrustStore()
	if approvalTrustPath := strings.TrimSpace(
		config.ApprovalTrustFile,
	); approvalTrustPath != "" {
		snapshot, snapshotErr := readConfigFileSnapshot(
			approvalTrustPath,
			"approval trust",
			production,
		)
		if snapshotErr != nil {
			return snapshotErr
		}
		approvalTrust, err = approval.ParseTrustDocument(snapshot.Data)
		if err != nil {
			return fmt.Errorf("gatemoled: decode approval trust: %w", err)
		}
		executionPolicy.ApprovalTrustDigest = snapshot.Digest
	}
	if production && approvalTrust.Len() == 0 {
		return errors.New("gatemoled: production profile requires at least one trusted approval key")
	}
	var identityVerifier *identity.Verifier
	if identityTrustPath := strings.TrimSpace(
		config.IdentityTrustFile,
	); identityTrustPath != "" {
		snapshot, snapshotErr := readConfigFileSnapshot(
			identityTrustPath,
			"OIDC identity trust",
			production,
		)
		if snapshotErr != nil {
			return snapshotErr
		}
		identityVerifier, err = identity.ParseTrustDocument(snapshot.Data)
		if err != nil {
			return fmt.Errorf("gatemoled: decode OIDC identity trust: %w", err)
		}
		executionPolicy.IdentityTrustDigest = snapshot.Digest
	}
	if production && identityVerifier == nil {
		return errors.New("gatemoled: production profile requires an OIDC identity trust document")
	}
	releasePolicy, err := releaseRuntimePolicy(config.RuntimeProfile, config.AllowedGitRefs)
	if err != nil {
		return err
	}
	var verifierProfiles *verification.ProfileSet
	if profilesPath := strings.TrimSpace(
		config.VerifierProfilesFile,
	); profilesPath != "" {
		snapshot, snapshotErr := readConfigFileSnapshot(
			profilesPath,
			"verifier profiles",
			production,
		)
		if snapshotErr != nil {
			return snapshotErr
		}
		verifierProfiles, err = verification.ParseProfiles(snapshot.Data)
		if err != nil {
			return fmt.Errorf("gatemoled: decode verifier profiles: %w", err)
		}
		executionPolicy.VerifierProfilesSourceDigest = snapshot.Digest
	}
	if production && verifierProfiles == nil {
		return errors.New("gatemoled: production profile requires daemon-owned verifier profiles")
	}
	if verifierProfiles != nil {
		for _, profile := range verifierProfiles.Profiles() {
			if profile.TimeoutSeconds > executionPolicy.MaxVerificationTimeoutSecs {
				return fmt.Errorf(
					"gatemoled: verifier profile %q timeout exceeds daemon maximum",
					profile.Name,
				)
			}
		}
	}
	executionPolicy.VerifierProfiles = verifierProfiles
	executionPolicy.RequireVerifierProfiles = production
	stdout := config.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	repositoryRoot := config.RepositoryRoot
	if repositoryRoot == "" {
		repositoryRoot = "."
	}
	runtimeIdentity, err := runtimeidentity.Load(ctx, repositoryRoot)
	if err != nil {
		return fmt.Errorf(
			"gatemoled: load Runtime identity; run `gatemole runtime init` first: %w",
			err,
		)
	}
	repositoryRoot, err = filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return fmt.Errorf("gatemoled: canonicalize repository root: %w", err)
	}
	repositoryRoot, err = filepath.Abs(repositoryRoot)
	if err != nil {
		return fmt.Errorf("gatemoled: resolve repository root: %w", err)
	}
	runtimeLock, err := acquireRuntimeLock(
		repositoryRoot,
		runtimeIdentity.RuntimeID,
	)
	if err != nil {
		return err
	}
	defer runtimeLock.Close()
	transactionRoot, err := prepareTransactionRootForRepository(
		config.TransactionRoot,
		repositoryRoot,
	)
	if err != nil {
		return err
	}
	daemonLock, err := acquireProcessLock(config.DatabasePath)
	if err != nil {
		return err
	}
	defer daemonLock.Close()
	kernelStore, err := store.OpenSQLiteForRuntime(
		ctx,
		config.DatabasePath,
		runtimeIdentity.RuntimeID,
		enforcementProfile,
	)
	if err != nil {
		return fmt.Errorf(
			"gatemoled: open Runtime-bound ledger: %w",
			err,
		)
	}
	defer kernelStore.Close()
	recoveredExecutions, err := recoverAgentExecutions(
		ctx,
		kernelStore,
		engineExecutionCleanup(executionPolicy.EnginePath),
		recoverModelExecutionFrom(transactionRoot),
		time.Now,
	)
	if err != nil {
		return fmt.Errorf("gatemoled: recover interrupted agent executions: %w", err)
	}
	if recoveredExecutions > 0 {
		fmt.Fprintf(stdout, "gatemoled marked %d interrupted agent execution(s)\n", recoveredExecutions)
	}
	actionBroker, err := broker.New(kernelStore, repositoryRoot)
	if err != nil {
		return fmt.Errorf("gatemoled: configure action broker: %w", err)
	}
	recovered, err := actionBroker.Recover(ctx)
	if err != nil {
		return fmt.Errorf("gatemoled: recover interrupted actions: %w", err)
	}
	if recovered > 0 {
		fmt.Fprintf(stdout, "gatemoled marked %d interrupted action(s) for reconciliation\n", recovered)
	}
	transactionManager, err := gitstage.New()
	if err != nil {
		return fmt.Errorf("gatemoled: configure transaction staging: %w", err)
	}
	if err := ensurePrivateSocketDirectory(config.SocketPath); err != nil {
		return err
	}
	if err := prepareSocketPath(config.SocketPath); err != nil {
		return err
	}
	listener, err := net.ListenUnix(
		"unix",
		&net.UnixAddr{Name: config.SocketPath, Net: "unix"},
	)
	if err != nil {
		return fmt.Errorf("gatemoled: listen: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	ownedSocket, err := os.Lstat(config.SocketPath)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("gatemoled: inspect created socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		_ = removeOwnedSocket(config.SocketPath, ownedSocket)
	}()
	if err := os.Chmod(config.SocketPath, 0o600); err != nil {
		return fmt.Errorf("gatemoled: restrict socket permissions: %w", err)
	}

	server := &http.Server{
		Handler: kernelapi.NewServer(
			kernelStore,
			kernelapi.WithBroker(actionBroker),
			kernelapi.WithTransactionRuntime(
				transactionManager,
				repositoryRoot,
				transactionRoot,
				transactionreducer.BaselinePolicy{},
			),
			kernelapi.WithRuntimeIdentity(runtimeIdentity.RuntimeID),
			kernelapi.WithExecutionRuntimePolicy(executionPolicy),
			kernelapi.WithApprovalTrustStore(approvalTrust),
			kernelapi.WithReleasePolicy(releasePolicy),
			kernelapi.WithIdentityVerifier(identityVerifier),
		).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	shutdownCtx, stopShutdown := context.WithCancel(ctx)
	defer stopShutdown()
	go func() {
		<-shutdownCtx.Done()
		deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(deadline); err != nil {
			_ = server.Close()
		}
	}()
	fmt.Fprintf(stdout, "gatemoled listening on %s\n", config.SocketPath)
	authenticatedListener := &peerAuthenticatedUnixListener{
		UnixListener: listener,
		expectedUID:  uint32(os.Geteuid()),
	}
	if err := server.Serve(authenticatedListener); err != nil &&
		!errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("gatemoled: serve: %w", err)
	}
	return nil
}

type peerAuthenticatedUnixListener struct {
	*net.UnixListener
	expectedUID uint32
}

func (listener *peerAuthenticatedUnixListener) Accept() (net.Conn, error) {
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, credentialErr := peercred.UID(connection)
		if credentialErr != nil {
			_ = connection.Close()
			return nil, fmt.Errorf(
				"gatemoled: authenticate Unix client: %w",
				credentialErr,
			)
		}
		if uid != listener.expectedUID {
			_ = connection.Close()
			continue
		}
		return connection, nil
	}
}

func ensurePrivateSocketDirectory(socketPath string) error {
	directory := filepath.Dir(socketPath)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("gatemoled: create socket directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("gatemoled: inspect socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New(
			"gatemoled: socket directory must be a real directory",
		)
	}
	ownerUID, err := peercred.FileOwnerUID(info)
	if err != nil {
		return fmt.Errorf("gatemoled: inspect socket directory owner: %w", err)
	}
	if ownerUID != uint32(os.Geteuid()) {
		return errors.New(
			"gatemoled: socket directory must be owned by the daemon user",
		)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New(
			"gatemoled: socket directory must not be group- or world-writable",
		)
	}
	return nil
}

func prepareSocketPath(socketPath string) error {
	return prepareSocketPathWithDial(
		socketPath,
		net.DialTimeout,
	)
}

func prepareSocketPathWithDial(
	socketPath string,
	dial func(string, string, time.Duration) (net.Conn, error),
) error {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gatemoled: inspect socket path: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("gatemoled: socket path exists and is not a socket: %s", socketPath)
	}
	connection, dialErr := dial(
		"unix", socketPath, 250*time.Millisecond,
	)
	if dialErr == nil {
		_ = connection.Close()
		return fmt.Errorf("gatemoled: socket already has an active listener: %s", socketPath)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) &&
		!errors.Is(dialErr, syscall.ENOENT) {
		return fmt.Errorf(
			"gatemoled: cannot prove Unix socket is stale; preserving %s: %w",
			socketPath,
			dialErr,
		)
	}
	current, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gatemoled: re-inspect stale socket path: %w", err)
	}
	if current.Mode()&os.ModeSocket == 0 ||
		!os.SameFile(info, current) {
		return fmt.Errorf(
			"gatemoled: socket path changed during stale-socket recovery; preserving %s",
			socketPath,
		)
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("gatemoled: remove stale socket: %w", err)
	}
	return nil
}

func removeOwnedSocket(socketPath string, owned os.FileInfo) error {
	current, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSocket == 0 ||
		!os.SameFile(owned, current) {
		return nil
	}
	return os.Remove(socketPath)
}

func validateWorkloadIdentity(profile string, uid, gid, daemonUID, daemonGID int) error {
	if profile != "production" {
		return nil
	}
	if uid <= 0 || gid <= 0 {
		return errors.New("gatemoled: production profile requires non-root workload UID and GID")
	}
	// A non-root daemon cannot safely chown bind-mounted worktrees or evidence.
	// Requiring the same numeric identity keeps the OCI process writable only
	// where the daemon deliberately mounted daemon-owned transaction state.
	if daemonUID != 0 && (uid != daemonUID || gid != daemonGID) {
		return fmt.Errorf(
			"gatemoled: production workload UID:GID %d:%d must match non-root daemon UID:GID %d:%d for bind-mount ownership",
			uid, gid, daemonUID, daemonGID,
		)
	}
	return nil
}

func configureModelBroker(config Config, executionPolicy *kernelapi.ExecutionRuntimePolicy) error {
	image := strings.TrimSpace(config.ModelBrokerImage)
	policyPath := strings.TrimSpace(config.ModelBrokerPolicy)
	if image == "" && policyPath == "" {
		return nil
	}
	if image == "" || policyPath == "" {
		return errors.New("gatemoled: model broker image and policy must be configured together")
	}
	if _, err := sandbox.ImageDigest(image); err != nil {
		return fmt.Errorf("gatemoled: invalid model broker image: %w", err)
	}
	if !filepath.IsAbs(policyPath) {
		absolute, err := filepath.Abs(policyPath)
		if err != nil {
			return fmt.Errorf("gatemoled: resolve model broker policy path: %w", err)
		}
		policyPath = absolute
	}
	production := config.RuntimeProfile == "production"
	snapshot, err := readConfigFileSnapshot(
		policyPath,
		"model broker policy",
		production,
	)
	if err != nil {
		return err
	}
	policy, err := modelbroker.ParsePolicy(snapshot.Data, production)
	if err != nil {
		return fmt.Errorf("gatemoled: decode model broker policy: %w", err)
	}
	tokenEnvironment := strings.TrimSpace(config.ModelTokenEnv)
	if tokenEnvironment == "" {
		tokenEnvironment = "OPENAI_API_KEY"
	}
	providerToken, exists := os.LookupEnv(tokenEnvironment)
	if !exists || strings.TrimSpace(providerToken) == "" {
		return fmt.Errorf("gatemoled: model provider credential environment %s is not set", tokenEnvironment)
	}
	executionPolicy.ModelBroker = &kernelapi.ModelBrokerRuntimePolicy{
		Image: image, PolicyData: cloneConfigBytes(snapshot.Data),
		PolicyDigest: snapshot.Digest, Policy: policy,
		ProviderBearerToken: providerToken,
	}
	return nil
}

func releaseRuntimePolicy(profile string, allowedGitRefs []string) (kernelapi.ReleasePolicy, error) {
	if profile == "" {
		profile = "development"
	}
	if profile == "production" && len(allowedGitRefs) == 0 {
		return kernelapi.ReleasePolicy{}, errors.New("gatemoled: production profile requires at least one allowed Git ref pattern")
	}
	patterns := make([]string, 0, len(allowedGitRefs))
	for _, raw := range allowedGitRefs {
		pattern := strings.TrimSpace(raw)
		if !strings.HasPrefix(pattern, "refs/heads/") {
			return kernelapi.ReleasePolicy{}, fmt.Errorf("gatemoled: allowed Git ref pattern must start with refs/heads/: %q", raw)
		}
		if _, err := path.Match(pattern, "refs/heads/probe"); err != nil {
			return kernelapi.ReleasePolicy{}, fmt.Errorf("gatemoled: invalid Git ref pattern %q: %w", raw, err)
		}
		patterns = append(patterns, pattern)
	}
	return kernelapi.ReleasePolicy{AllowedGitRefs: patterns}, nil
}

func executionRuntimePolicy(profile string, allowedImages []string) (kernelapi.ExecutionRuntimePolicy, error) {
	if profile == "" {
		profile = "development"
	}
	if profile != "development" && profile != "production" {
		return kernelapi.ExecutionRuntimePolicy{}, errors.New("gatemoled: runtime profile must be development or production")
	}
	allowed := make(map[string]struct{}, len(allowedImages))
	fullImages := make([]string, 0, len(allowedImages))
	seenImages := make(map[string]struct{}, len(allowedImages))
	for _, image := range allowedImages {
		image = strings.TrimSpace(image)
		digest, err := sandbox.ImageDigest(image)
		if err != nil {
			return kernelapi.ExecutionRuntimePolicy{}, fmt.Errorf("gatemoled: invalid allowed image %q: %w", image, err)
		}
		allowed[digest] = struct{}{}
		if _, seen := seenImages[image]; !seen {
			seenImages[image] = struct{}{}
			fullImages = append(fullImages, image)
		}
	}
	if profile == "production" && len(allowed) == 0 {
		return kernelapi.ExecutionRuntimePolicy{}, errors.New("gatemoled: production profile requires at least one digest-pinned allowed image")
	}
	return kernelapi.ExecutionRuntimePolicy{
		AllowHost:                profile == "development",
		AllowedAgentImageDigests: allowed,
		AllowedAgentImages:       fullImages,
		AllowedImageDigests:      allowed,
	}, nil
}
