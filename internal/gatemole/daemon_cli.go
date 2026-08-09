package gatemole

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

	"github.com/duriantaco/gatemole/internal/kernel/daemon"
)

type daemonFlagValues struct {
	databasePath             string
	socketPath               string
	transactionRoot          string
	runtimeProfile           string
	allowedImages            string
	approvalTrust            string
	allowedGitRefs           string
	runtimeEngine            string
	verifierUID              int
	verifierGID              int
	modelBrokerImage         string
	modelBrokerPolicy        string
	modelTokenEnv            string
	identityTrust            string
	verifierProfiles         string
	allowUnsafeHostExecution bool
}

func newDaemonFlagSet(repo string, output io.Writer) (*flag.FlagSet, *daemonFlagValues) {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	flags.SetOutput(output)
	values := &daemonFlagValues{}
	flags.StringVar(&values.databasePath, "db", filepath.Join(repo, ".gatemole", "kernel.db"), "SQLite kernel database path")
	flags.StringVar(&values.socketPath, "socket", defaultKernelSocket(repo), "Unix socket path")
	flags.StringVar(&values.transactionRoot, "transaction-root", "", "isolated transaction worktree root (defaults to a repository-scoped per-user directory)")
	flags.StringVar(&values.runtimeProfile, "runtime-profile", "development", "execution policy: development or production")
	flags.StringVar(&values.allowedImages, "allowed-images", "", "comma-separated digest-pinned OCI images allowed in production")
	flags.StringVar(&values.approvalTrust, "approval-trust", "", "JSON file containing trusted approval public keys")
	flags.StringVar(&values.allowedGitRefs, "allowed-git-refs", "", "comma-separated Git branch ref patterns allowed for release")
	flags.StringVar(&values.runtimeEngine, "runtime-engine", "docker", "daemon-owned OCI engine executable")
	flags.IntVar(&values.verifierUID, "verifier-uid", os.Getuid(), "non-root UID for daemon-run agent, verifier, and broker workloads")
	flags.IntVar(&values.verifierGID, "verifier-gid", os.Getgid(), "non-root GID for daemon-run agent, verifier, and broker workloads")
	flags.StringVar(&values.modelBrokerImage, "model-broker-image", "", "digest-pinned Gatemole model broker OCI image")
	flags.StringVar(&values.modelBrokerPolicy, "model-broker-policy", "", "model broker policy JSON")
	flags.StringVar(&values.modelTokenEnv, "model-provider-token-env", "OPENAI_API_KEY", "daemon environment containing the provider bearer credential")
	flags.StringVar(&values.identityTrust, "identity-trust", "", "OIDC issuer/JWKS trust document")
	flags.StringVar(&values.verifierProfiles, "verifier-profiles", "", "strict daemon-owned verifier profile JSON")
	flags.BoolVar(
		&values.allowUnsafeHostExecution,
		"allow-unsafe-host-execution",
		false,
		"development only: allow the CLI to supervise an unenforced host process",
	)
	return flags, values
}

func daemonCommand(repo string, args []string, stdout io.Writer, stderr io.Writer) int {
	flags, values := newDaemonFlagSet(repo, stderr)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "daemon: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if !filepath.IsAbs(values.databasePath) {
		values.databasePath = filepath.Join(repo, values.databasePath)
	}
	if !filepath.IsAbs(values.socketPath) {
		values.socketPath = filepath.Join(repo, values.socketPath)
	}
	var err error
	values.transactionRoot, err = daemonTransactionRoot(
		repo,
		values.transactionRoot,
		daemonFlagWasSet(flags, "transaction-root"),
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if values.approvalTrust != "" && !filepath.IsAbs(values.approvalTrust) {
		values.approvalTrust = filepath.Join(repo, values.approvalTrust)
	}
	if values.modelBrokerPolicy != "" && !filepath.IsAbs(values.modelBrokerPolicy) {
		values.modelBrokerPolicy = filepath.Join(repo, values.modelBrokerPolicy)
	}
	if values.identityTrust != "" && !filepath.IsAbs(values.identityTrust) {
		values.identityTrust = filepath.Join(repo, values.identityTrust)
	}
	if values.verifierProfiles != "" && !filepath.IsAbs(values.verifierProfiles) {
		values.verifierProfiles = filepath.Join(repo, values.verifierProfiles)
	}
	values.runtimeEngine = repositoryExecutablePath(repo, values.runtimeEngine)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, daemon.Config{
		DatabasePath:             values.databasePath,
		SocketPath:               values.socketPath,
		RepositoryRoot:           repo,
		TransactionRoot:          values.transactionRoot,
		RuntimeProfile:           values.runtimeProfile,
		AllowedImages:            splitCommaValues(values.allowedImages),
		ApprovalTrustFile:        values.approvalTrust,
		AllowedGitRefs:           splitCommaValues(values.allowedGitRefs),
		RuntimeEngine:            values.runtimeEngine,
		VerifierUID:              values.verifierUID,
		VerifierGID:              values.verifierGID,
		ModelBrokerImage:         values.modelBrokerImage,
		ModelBrokerPolicy:        values.modelBrokerPolicy,
		ModelTokenEnv:            values.modelTokenEnv,
		IdentityTrustFile:        values.identityTrust,
		VerifierProfilesFile:     values.verifierProfiles,
		AllowUnsafeHostExecution: values.allowUnsafeHostExecution,
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
