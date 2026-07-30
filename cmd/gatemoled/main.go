package main

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

func main() {
	os.Exit(run())
}

func run() int {
	config, exitCode := parseDaemonConfig(os.Args[1:], os.Stderr)
	if exitCode != 0 {
		return exitCode
	}
	config.Stdout = os.Stdout

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func parseDaemonConfig(args []string, stderr io.Writer) (daemon.Config, int) {
	flags := flag.NewFlagSet("gatemoled", flag.ContinueOnError)
	flags.SetOutput(stderr)
	databasePath := flags.String("db", ".gatemole/kernel.db", "SQLite kernel database path")
	socketPath := flags.String("socket", ".gatemole/gatemoled.sock", "Unix socket path")
	repositoryRoot := flags.String("repo", ".", "repository root for mediated workspaces")
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
		"development only: allow clients to supervise unenforced host processes",
	)
	if err := flags.Parse(args); err != nil {
		return daemon.Config{}, 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "gatemoled: unexpected argument %q\n", flags.Arg(0))
		return daemon.Config{}, 2
	}

	repository, err := canonicalRepositoryRoot(*repositoryRoot)
	if err != nil {
		fmt.Fprintf(stderr, "gatemoled: resolve repository root: %v\n", err)
		return daemon.Config{}, 1
	}
	*databasePath = repositoryPath(repository, *databasePath)
	*socketPath = repositoryPath(repository, *socketPath)
	*approvalTrust = repositoryPath(repository, *approvalTrust)
	*identityTrust = repositoryPath(repository, *identityTrust)
	*verifierProfiles = repositoryPath(repository, *verifierProfiles)
	*modelBrokerPolicy = repositoryPath(repository, *modelBrokerPolicy)
	*runtimeEngine = repositoryExecutable(repository, *runtimeEngine)
	if flagWasSet(flags, "transaction-root") {
		*transactionRoot = repositoryPath(repository, *transactionRoot)
	} else {
		*transactionRoot, err = daemon.DefaultTransactionRoot(repository)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return daemon.Config{}, 1
		}
	}

	return daemon.Config{
		DatabasePath:             *databasePath,
		SocketPath:               *socketPath,
		RepositoryRoot:           repository,
		TransactionRoot:          *transactionRoot,
		RuntimeProfile:           *runtimeProfile,
		AllowedImages:            commaValues(*allowedImages),
		ApprovalTrustFile:        *approvalTrust,
		AllowedGitRefs:           commaValues(*allowedGitRefs),
		RuntimeEngine:            *runtimeEngine,
		VerifierUID:              *verifierUID,
		VerifierGID:              *verifierGID,
		ModelBrokerImage:         *modelBrokerImage,
		ModelBrokerPolicy:        *modelBrokerPolicy,
		ModelTokenEnv:            *modelTokenEnv,
		IdentityTrustFile:        *identityTrust,
		VerifierProfilesFile:     *verifierProfiles,
		AllowUnsafeHostExecution: *allowUnsafeHostExecution,
	}, 0
}

func canonicalRepositoryRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func repositoryPath(repository, path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(repository, path))
}

func repositoryExecutable(repository, executable string) string {
	if executable == "" ||
		filepath.IsAbs(executable) ||
		!strings.ContainsRune(executable, filepath.Separator) {
		return executable
	}
	return repositoryPath(repository, executable)
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	wasSet := false
	flags.Visit(func(current *flag.Flag) {
		if current.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func commaValues(value string) []string {
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
