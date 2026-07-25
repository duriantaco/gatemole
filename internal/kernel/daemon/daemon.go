package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	kernelapi "github.com/duriantaco/vouch/internal/kernel/api"
	"github.com/duriantaco/vouch/internal/kernel/approval"
	"github.com/duriantaco/vouch/internal/kernel/broker"
	"github.com/duriantaco/vouch/internal/kernel/identity"
	"github.com/duriantaco/vouch/internal/kernel/modelbroker"
	"github.com/duriantaco/vouch/internal/kernel/sandbox"
	"github.com/duriantaco/vouch/internal/kernel/store"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
	"github.com/duriantaco/vouch/internal/kernel/transaction/gitstage"
	"github.com/duriantaco/vouch/internal/kernel/verification"
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
		return errors.New("vouchd: database path is required")
	}
	if strings.TrimSpace(config.SocketPath) == "" {
		return errors.New("vouchd: socket path is required")
	}
	if strings.TrimSpace(config.TransactionRoot) == "" {
		return errors.New("vouchd: transaction staging root is required")
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
		return errors.New("vouchd: production profile forbids client-supervised host execution")
	}
	runtimeEngine := strings.TrimSpace(config.RuntimeEngine)
	if runtimeEngine == "" {
		runtimeEngine = "docker"
	}
	enginePath, engineErr := exec.LookPath(runtimeEngine)
	if engineErr != nil && config.RuntimeProfile == "production" {
		return fmt.Errorf("vouchd: find production OCI engine %q: %w", runtimeEngine, engineErr)
	}
	if engineErr == nil {
		enginePath, err = filepath.Abs(enginePath)
		if err != nil {
			return fmt.Errorf("vouchd: resolve OCI engine: %w", err)
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
	if config.RuntimeProfile == "production" &&
		strings.TrimSpace(config.ApprovalTrustFile) != "" {
		if err := validateProductionConfigFile(
			config.ApprovalTrustFile, "approval trust",
		); err != nil {
			return err
		}
	}
	approvalTrust, err := approval.LoadTrustFile(config.ApprovalTrustFile)
	if err != nil {
		return fmt.Errorf("vouchd: %w", err)
	}
	if config.RuntimeProfile == "production" && approvalTrust.Len() == 0 {
		return errors.New("vouchd: production profile requires at least one trusted approval key")
	}
	if strings.TrimSpace(config.ApprovalTrustFile) != "" {
		executionPolicy.ApprovalTrustDigest, err = digestConfigFile(
			config.ApprovalTrustFile,
			"approval trust",
		)
		if err != nil {
			return err
		}
	}
	var identityVerifier *identity.Verifier
	if strings.TrimSpace(config.IdentityTrustFile) != "" {
		if config.RuntimeProfile == "production" {
			if err := validateProductionConfigFile(
				config.IdentityTrustFile, "OIDC identity trust",
			); err != nil {
				return err
			}
		}
		identityVerifier, err = identity.LoadTrustFile(config.IdentityTrustFile)
		if err != nil {
			return fmt.Errorf("vouchd: %w", err)
		}
		executionPolicy.IdentityTrustDigest, err = digestConfigFile(
			config.IdentityTrustFile,
			"OIDC identity trust",
		)
		if err != nil {
			return err
		}
	}
	if config.RuntimeProfile == "production" && identityVerifier == nil {
		return errors.New("vouchd: production profile requires an OIDC identity trust document")
	}
	releasePolicy, err := releaseRuntimePolicy(config.RuntimeProfile, config.AllowedGitRefs)
	if err != nil {
		return err
	}
	var verifierProfiles *verification.ProfileSet
	if strings.TrimSpace(config.VerifierProfilesFile) != "" {
		if config.RuntimeProfile == "production" {
			if err := validateProductionConfigFile(
				config.VerifierProfilesFile, "verifier profiles",
			); err != nil {
				return err
			}
		}
		verifierProfiles, err = verification.LoadProfiles(
			config.VerifierProfilesFile,
		)
		if err != nil {
			return fmt.Errorf("vouchd: %w", err)
		}
	}
	if config.RuntimeProfile == "production" && verifierProfiles == nil {
		return errors.New("vouchd: production profile requires daemon-owned verifier profiles")
	}
	if verifierProfiles != nil {
		for _, profile := range verifierProfiles.Profiles() {
			if profile.TimeoutSeconds > executionPolicy.MaxVerificationTimeoutSecs {
				return fmt.Errorf(
					"vouchd: verifier profile %q timeout exceeds daemon maximum",
					profile.Name,
				)
			}
		}
	}
	executionPolicy.VerifierProfiles = verifierProfiles
	executionPolicy.RequireVerifierProfiles = config.RuntimeProfile == "production"
	stdout := config.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	if err := os.MkdirAll(filepath.Dir(config.SocketPath), 0o750); err != nil {
		return fmt.Errorf("vouchd: create socket directory: %w", err)
	}
	daemonLock, err := acquireProcessLock(config.DatabasePath)
	if err != nil {
		return err
	}
	defer daemonLock.Close()
	if err := prepareSocketPath(config.SocketPath); err != nil {
		return err
	}
	listener, err := net.ListenUnix(
		"unix",
		&net.UnixAddr{Name: config.SocketPath, Net: "unix"},
	)
	if err != nil {
		return fmt.Errorf("vouchd: listen: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	ownedSocket, err := os.Lstat(config.SocketPath)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("vouchd: inspect created socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		_ = removeOwnedSocket(config.SocketPath, ownedSocket)
	}()
	if err := os.Chmod(config.SocketPath, 0o600); err != nil {
		return fmt.Errorf("vouchd: restrict socket permissions: %w", err)
	}
	kernelStore, err := store.OpenSQLite(config.DatabasePath)
	if err != nil {
		return err
	}
	defer kernelStore.Close()
	if err := kernelStore.BindEnforcementProfile(
		ctx, enforcementProfile,
	); err != nil {
		return fmt.Errorf(
			"vouchd: bind ledger enforcement profile: %w",
			err,
		)
	}
	recoveredExecutions, err := recoverAgentExecutions(
		ctx,
		kernelStore,
		engineExecutionCleanup(executionPolicy.EnginePath),
		time.Now,
	)
	if err != nil {
		return fmt.Errorf("vouchd: recover interrupted agent executions: %w", err)
	}
	if recoveredExecutions > 0 {
		fmt.Fprintf(stdout, "vouchd marked %d interrupted agent execution(s)\n", recoveredExecutions)
	}
	repositoryRoot := config.RepositoryRoot
	if repositoryRoot == "" {
		repositoryRoot = "."
	}
	actionBroker, err := broker.New(kernelStore, repositoryRoot)
	if err != nil {
		return fmt.Errorf("vouchd: configure action broker: %w", err)
	}
	recovered, err := actionBroker.Recover(ctx)
	if err != nil {
		return fmt.Errorf("vouchd: recover interrupted actions: %w", err)
	}
	if recovered > 0 {
		fmt.Fprintf(stdout, "vouchd marked %d interrupted action(s) for reconciliation\n", recovered)
	}
	transactionRoot := config.TransactionRoot
	transactionManager, err := gitstage.New()
	if err != nil {
		return fmt.Errorf("vouchd: configure transaction staging: %w", err)
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
	fmt.Fprintf(stdout, "vouchd listening on %s\n", config.SocketPath)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("vouchd: serve: %w", err)
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
		return fmt.Errorf("vouchd: inspect socket path: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("vouchd: socket path exists and is not a socket: %s", socketPath)
	}
	connection, dialErr := dial(
		"unix", socketPath, 250*time.Millisecond,
	)
	if dialErr == nil {
		_ = connection.Close()
		return fmt.Errorf("vouchd: socket already has an active listener: %s", socketPath)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) &&
		!errors.Is(dialErr, syscall.ENOENT) {
		return fmt.Errorf(
			"vouchd: cannot prove Unix socket is stale; preserving %s: %w",
			socketPath,
			dialErr,
		)
	}
	current, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("vouchd: re-inspect stale socket path: %w", err)
	}
	if current.Mode()&os.ModeSocket == 0 ||
		!os.SameFile(info, current) {
		return fmt.Errorf(
			"vouchd: socket path changed during stale-socket recovery; preserving %s",
			socketPath,
		)
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("vouchd: remove stale socket: %w", err)
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
		return errors.New("vouchd: production profile requires non-root workload UID and GID")
	}
	// A non-root daemon cannot safely chown bind-mounted worktrees or evidence.
	// Requiring the same numeric identity keeps the OCI process writable only
	// where the daemon deliberately mounted daemon-owned transaction state.
	if daemonUID != 0 && (uid != daemonUID || gid != daemonGID) {
		return fmt.Errorf(
			"vouchd: production workload UID:GID %d:%d must match non-root daemon UID:GID %d:%d for bind-mount ownership",
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
		return errors.New("vouchd: model broker image and policy must be configured together")
	}
	if _, err := sandbox.ImageDigest(image); err != nil {
		return fmt.Errorf("vouchd: invalid model broker image: %w", err)
	}
	if !filepath.IsAbs(policyPath) {
		absolute, err := filepath.Abs(policyPath)
		if err != nil {
			return fmt.Errorf("vouchd: resolve model broker policy path: %w", err)
		}
		policyPath = absolute
	}
	production := config.RuntimeProfile == "production"
	if production {
		if err := validateProductionConfigFile(policyPath, "model broker policy"); err != nil {
			return err
		}
	}
	policy, err := modelbroker.LoadPolicy(policyPath, production)
	if err != nil {
		return fmt.Errorf("vouchd: %w", err)
	}
	policyData, err := os.ReadFile(policyPath)
	if err != nil {
		return fmt.Errorf("vouchd: read model broker policy for digest: %w", err)
	}
	if len(policyData) > 2<<20 {
		return errors.New("vouchd: model broker policy exceeds 2 MiB")
	}
	sum := sha256.Sum256(policyData)
	policyDigest := "sha256:" + hex.EncodeToString(sum[:])
	tokenEnvironment := strings.TrimSpace(config.ModelTokenEnv)
	if tokenEnvironment == "" {
		tokenEnvironment = "OPENAI_API_KEY"
	}
	providerToken, exists := os.LookupEnv(tokenEnvironment)
	if !exists || strings.TrimSpace(providerToken) == "" {
		return fmt.Errorf("vouchd: model provider credential environment %s is not set", tokenEnvironment)
	}
	executionPolicy.ModelBroker = &kernelapi.ModelBrokerRuntimePolicy{
		Image: image, PolicyPath: policyPath, PolicyDigest: policyDigest,
		Policy: policy, ProviderBearerToken: providerToken,
	}
	return nil
}

func validateProductionConfigFile(filePath, label string) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return fmt.Errorf("vouchd: inspect production %s file: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("vouchd: production %s must be a regular non-symlink file", label)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("vouchd: production %s must not be group- or world-writable", label)
	}
	return nil
}

func digestConfigFile(filePath, label string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("vouchd: read %s for digest: %w", label, err)
	}
	if len(data) > 2<<20 {
		return "", fmt.Errorf("vouchd: %s exceeds 2 MiB", label)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func releaseRuntimePolicy(profile string, allowedGitRefs []string) (kernelapi.ReleasePolicy, error) {
	if profile == "" {
		profile = "development"
	}
	if profile == "production" && len(allowedGitRefs) == 0 {
		return kernelapi.ReleasePolicy{}, errors.New("vouchd: production profile requires at least one allowed Git ref pattern")
	}
	patterns := make([]string, 0, len(allowedGitRefs))
	for _, raw := range allowedGitRefs {
		pattern := strings.TrimSpace(raw)
		if !strings.HasPrefix(pattern, "refs/heads/") {
			return kernelapi.ReleasePolicy{}, fmt.Errorf("vouchd: allowed Git ref pattern must start with refs/heads/: %q", raw)
		}
		if _, err := path.Match(pattern, "refs/heads/probe"); err != nil {
			return kernelapi.ReleasePolicy{}, fmt.Errorf("vouchd: invalid Git ref pattern %q: %w", raw, err)
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
		return kernelapi.ExecutionRuntimePolicy{}, errors.New("vouchd: runtime profile must be development or production")
	}
	allowed := make(map[string]struct{}, len(allowedImages))
	fullImages := make([]string, 0, len(allowedImages))
	seenImages := make(map[string]struct{}, len(allowedImages))
	for _, image := range allowedImages {
		image = strings.TrimSpace(image)
		digest, err := sandbox.ImageDigest(image)
		if err != nil {
			return kernelapi.ExecutionRuntimePolicy{}, fmt.Errorf("vouchd: invalid allowed image %q: %w", image, err)
		}
		allowed[digest] = struct{}{}
		if _, seen := seenImages[image]; !seen {
			seenImages[image] = struct{}{}
			fullImages = append(fullImages, image)
		}
	}
	if profile == "production" && len(allowed) == 0 {
		return kernelapi.ExecutionRuntimePolicy{}, errors.New("vouchd: production profile requires at least one digest-pinned allowed image")
	}
	return kernelapi.ExecutionRuntimePolicy{
		AllowHost:                profile == "development",
		AllowedAgentImageDigests: allowed,
		AllowedAgentImages:       fullImages,
		AllowedImageDigests:      allowed,
	}, nil
}
