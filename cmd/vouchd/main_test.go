package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonConfigDefaultsStayWithRepositoryOutsideWorkingDirectory(t *testing.T) {
	base := t.TempDir()
	repositoryA := filepath.Join(base, "repo-a")
	repositoryB := filepath.Join(base, "repo-b")
	workingDirectory := filepath.Join(base, "caller")
	runtimeDirectory := filepath.Join(base, "runtime")
	for _, directory := range []string{
		repositoryA,
		repositoryB,
		workingDirectory,
		runtimeDirectory,
	} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
	}
	if err := os.Chmod(runtimeDirectory, 0o700); err != nil {
		t.Fatalf("restrict runtime directory: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDirectory)

	previousWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(workingDirectory); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousWorkingDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	configA, exitCode := parseDaemonConfig([]string{"--repo", repositoryA}, io.Discard)
	if exitCode != 0 {
		t.Fatalf("parse repo A config: exit code %d", exitCode)
	}
	canonicalA, err := filepath.EvalSymlinks(repositoryA)
	if err != nil {
		t.Fatalf("canonicalize repo A: %v", err)
	}
	if configA.RepositoryRoot != canonicalA {
		t.Fatalf("repository root = %q, want %q", configA.RepositoryRoot, canonicalA)
	}
	assertPathEqual(t, "database", configA.DatabasePath, filepath.Join(canonicalA, ".gatemole", "kernel.db"))
	assertPathEqual(t, "socket", configA.SocketPath, filepath.Join(canonicalA, ".gatemole", "vouchd.sock"))
	if configA.DatabasePath == filepath.Join(workingDirectory, ".gatemole", "kernel.db") {
		t.Fatalf("database path was redirected to caller working directory: %q", configA.DatabasePath)
	}
	if configA.SocketPath == filepath.Join(workingDirectory, ".gatemole", "vouchd.sock") {
		t.Fatalf("socket path was redirected to caller working directory: %q", configA.SocketPath)
	}

	configB, exitCode := parseDaemonConfig([]string{"--repo", repositoryB}, io.Discard)
	if exitCode != 0 {
		t.Fatalf("parse repo B config: exit code %d", exitCode)
	}
	if configA.TransactionRoot == configB.TransactionRoot {
		t.Fatalf("repositories share transaction root %q", configA.TransactionRoot)
	}
	canonicalRuntimeDirectory, err := filepath.EvalSymlinks(runtimeDirectory)
	if err != nil {
		t.Fatalf("canonicalize runtime directory: %v", err)
	}
	assertPathEqual(
		t,
		"transaction root parent",
		filepath.Dir(configA.TransactionRoot),
		filepath.Join(canonicalRuntimeDirectory, "vouch", "transactions"),
	)
}

func TestDaemonConfigResolvesRelativePathsAgainstCanonicalRepository(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	repositoryLink := filepath.Join(base, "repository-link")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	if err := os.Symlink(repository, repositoryLink); err != nil {
		t.Skipf("create repository symlink: %v", err)
	}

	config, exitCode := parseDaemonConfig([]string{
		"--repo", repositoryLink,
		"--db", filepath.Join("state", "kernel.db"),
		"--socket", filepath.Join("run", "vouchd.sock"),
		"--transaction-root", "transactions",
		"--approval-trust", filepath.Join("config", "approval-trust.json"),
		"--identity-trust", filepath.Join("config", "identity-trust.json"),
		"--verifier-profiles", filepath.Join("config", "verifier-profiles.json"),
		"--model-broker-policy", filepath.Join("config", "model-policy.json"),
		"--runtime-engine", filepath.Join("bin", "docker"),
	}, io.Discard)
	if exitCode != 0 {
		t.Fatalf("parse daemon config: exit code %d", exitCode)
	}

	canonical, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatalf("canonicalize repository: %v", err)
	}
	assertPathEqual(t, "repository", config.RepositoryRoot, canonical)
	assertPathEqual(t, "database", config.DatabasePath, filepath.Join(canonical, "state", "kernel.db"))
	assertPathEqual(t, "socket", config.SocketPath, filepath.Join(canonical, "run", "vouchd.sock"))
	assertPathEqual(t, "transaction root", config.TransactionRoot, filepath.Join(canonical, "transactions"))
	assertPathEqual(t, "approval trust", config.ApprovalTrustFile, filepath.Join(canonical, "config", "approval-trust.json"))
	assertPathEqual(t, "identity trust", config.IdentityTrustFile, filepath.Join(canonical, "config", "identity-trust.json"))
	assertPathEqual(t, "verifier profiles", config.VerifierProfilesFile, filepath.Join(canonical, "config", "verifier-profiles.json"))
	assertPathEqual(t, "model broker policy", config.ModelBrokerPolicy, filepath.Join(canonical, "config", "model-policy.json"))
	assertPathEqual(t, "runtime engine", config.RuntimeEngine, filepath.Join(canonical, "bin", "docker"))
}

func TestDaemonConfigPreservesAbsoluteConfigPaths(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	configDirectory := filepath.Join(base, "operator-config")
	paths := map[string]string{
		"approval trust":    filepath.Join(configDirectory, "approval-trust.json"),
		"identity trust":    filepath.Join(configDirectory, "identity-trust.json"),
		"verifier profiles": filepath.Join(configDirectory, "verifier-profiles.json"),
		"model policy":      filepath.Join(configDirectory, "model-policy.json"),
	}

	config, exitCode := parseDaemonConfig([]string{
		"--repo", repository,
		"--approval-trust", paths["approval trust"],
		"--identity-trust", paths["identity trust"],
		"--verifier-profiles", paths["verifier profiles"],
		"--model-broker-policy", paths["model policy"],
		"--runtime-engine", "/usr/local/bin/docker",
	}, io.Discard)
	if exitCode != 0 {
		t.Fatalf("parse daemon config: exit code %d", exitCode)
	}

	assertPathEqual(t, "approval trust", config.ApprovalTrustFile, paths["approval trust"])
	assertPathEqual(t, "identity trust", config.IdentityTrustFile, paths["identity trust"])
	assertPathEqual(t, "verifier profiles", config.VerifierProfilesFile, paths["verifier profiles"])
	assertPathEqual(t, "model broker policy", config.ModelBrokerPolicy, paths["model policy"])
	assertPathEqual(t, "runtime engine", config.RuntimeEngine, "/usr/local/bin/docker")
}

func TestDaemonConfigLeavesPathLookedUpRuntimeEngineUnchanged(t *testing.T) {
	repository := t.TempDir()
	config, exitCode := parseDaemonConfig([]string{
		"--repo", repository,
		"--runtime-engine", "podman",
	}, io.Discard)
	if exitCode != 0 {
		t.Fatalf("parse daemon config: exit code %d", exitCode)
	}
	if config.RuntimeEngine != "podman" {
		t.Fatalf("runtime engine = %q, want PATH lookup name podman", config.RuntimeEngine)
	}
}

func assertPathEqual(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s path = %q, want %q", name, got, want)
	}
}
