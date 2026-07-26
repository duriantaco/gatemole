package main

import (
	"context"
	"flag"
	"fmt"
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
	flags := flag.NewFlagSet("vouchd", flag.ContinueOnError)
	databasePath := flags.String("db", ".vouch/kernel.db", "SQLite kernel database path")
	socketPath := flags.String("socket", ".vouch/vouchd.sock", "Unix socket path")
	repositoryRoot := flags.String("repo", ".", "repository root for mediated workspaces")
	transactionRoot := flags.String("transaction-root", filepath.Join(os.TempDir(), "vouch-transactions"), "isolated transaction worktree root")
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
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "vouchd: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, daemon.Config{
		DatabasePath:             *databasePath,
		SocketPath:               *socketPath,
		RepositoryRoot:           *repositoryRoot,
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
		Stdout:                   os.Stdout,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
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
