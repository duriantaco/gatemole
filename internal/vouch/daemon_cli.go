package vouch

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/duriantaco/vouch/internal/kernel/daemon"
)

func daemonCommand(repo string, args []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	flags.SetOutput(stderr)
	databasePath := flags.String("db", filepath.Join(repo, ".vouch", "kernel.db"), "SQLite kernel database path")
	socketPath := flags.String("socket", defaultKernelSocket(repo), "Unix socket path")
	transactionRoot := flags.String("transaction-root", "", "isolated transaction worktree root (defaults to a repository-scoped per-user directory)")
	runtimeProfile := flags.String("runtime-profile", "development", "execution policy: development or production")
	allowedImages := flags.String("allowed-images", "", "comma-separated digest-pinned OCI images allowed in production")
	approvalTrust := flags.String("approval-trust", "", "JSON file containing trusted approval public keys")
	allowedGitRefs := flags.String("allowed-git-refs", "", "comma-separated Git branch ref patterns allowed for release")
	runtimeEngine := flags.String("runtime-engine", "docker", "daemon-owned OCI engine executable")
	verifierUID := flags.Int("verifier-uid", os.Getuid(), "non-root UID for daemon-run agent, verifier, and broker workloads")
	verifierGID := flags.Int("verifier-gid", os.Getgid(), "non-root GID for daemon-run agent, verifier, and broker workloads")
	modelBrokerImage := flags.String("model-broker-image", "", "digest-pinned Vouch model broker OCI image")
	modelBrokerPolicy := flags.String("model-broker-policy", "", "model broker policy JSON")
	modelTokenEnv := flags.String("model-provider-token-env", "OPENAI_API_KEY", "daemon environment containing the provider bearer credential")
	identityTrust := flags.String("identity-trust", "", "OIDC issuer/JWKS trust document")
	verifierProfiles := flags.String(
		"verifier-profiles",
		"",
		"strict daemon-owned verifier profile JSON",
	)
	allowUnsafeHostExecution := flags.Bool(
		"allow-unsafe-host-execution",
		false,
		"development only: allow the CLI to supervise an unenforced host process",
	)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "daemon: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if !filepath.IsAbs(*databasePath) {
		*databasePath = filepath.Join(repo, *databasePath)
	}
	if !filepath.IsAbs(*socketPath) {
		*socketPath = filepath.Join(repo, *socketPath)
	}
	var err error
	*transactionRoot, err = daemonTransactionRoot(
		repo,
		*transactionRoot,
		daemonFlagWasSet(flags, "transaction-root"),
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *approvalTrust != "" && !filepath.IsAbs(*approvalTrust) {
		*approvalTrust = filepath.Join(repo, *approvalTrust)
	}
	if *modelBrokerPolicy != "" && !filepath.IsAbs(*modelBrokerPolicy) {
		*modelBrokerPolicy = filepath.Join(repo, *modelBrokerPolicy)
	}
	if *identityTrust != "" && !filepath.IsAbs(*identityTrust) {
		*identityTrust = filepath.Join(repo, *identityTrust)
	}
	if *verifierProfiles != "" && !filepath.IsAbs(*verifierProfiles) {
		*verifierProfiles = filepath.Join(repo, *verifierProfiles)
	}
	*runtimeEngine = repositoryExecutablePath(repo, *runtimeEngine)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, daemon.Config{
		DatabasePath:             *databasePath,
		SocketPath:               *socketPath,
		RepositoryRoot:           repo,
		TransactionRoot:          *transactionRoot,
		RuntimeProfile:           *runtimeProfile,
		AllowedImages:            splitCommaValues(*allowedImages),
		ApprovalTrustFile:        *approvalTrust,
		AllowedGitRefs:           splitCommaValues(*allowedGitRefs),
		RuntimeEngine:            *runtimeEngine,
		VerifierUID:              *verifierUID,
		VerifierGID:              *verifierGID,
		ModelBrokerImage:         *modelBrokerImage,
		ModelBrokerPolicy:        *modelBrokerPolicy,
		ModelTokenEnv:            *modelTokenEnv,
		IdentityTrustFile:        *identityTrust,
		VerifierProfilesFile:     *verifierProfiles,
		AllowUnsafeHostExecution: *allowUnsafeHostExecution,
		Stdout:                   stdout,
	}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func repositoryExecutablePath(repo, executable string) string {
	if executable == "" ||
		filepath.IsAbs(executable) ||
		!strings.ContainsRune(executable, filepath.Separator) {
		return executable
	}
	return filepath.Clean(filepath.Join(repo, executable))
}

func splitCommaValues(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func daemonFlagWasSet(flags *flag.FlagSet, name string) bool {
	wasSet := false
	flags.Visit(func(current *flag.Flag) {
		if current.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func daemonTransactionRoot(
	repo string,
	configured string,
	wasConfigured bool,
) (string, error) {
	if !wasConfigured {
		return daemon.DefaultTransactionRoot(repo)
	}
	if filepath.IsAbs(configured) {
		return filepath.Clean(configured), nil
	}
	return filepath.Clean(filepath.Join(repo, configured)), nil
}
