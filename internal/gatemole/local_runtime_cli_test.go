package gatemole

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductCommandsAutomaticallyHostLocalDevelopmentRuntime(t *testing.T) {
	repo, err := os.MkdirTemp("/tmp", "gm-auto-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repo) })
	repo = transactionRunRepositoryAt(t, repo)
	runtimeBase := t.TempDir()
	if err := os.Chmod(runtimeBase, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)
	fakeRuntime := writeFakeOCIRuntime(t)
	fakeRuntimeDirectory := t.TempDir()
	if err := os.Symlink(
		fakeRuntime,
		filepath.Join(fakeRuntimeDirectory, "docker"),
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeRuntimeDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	code := Main(
		runtimeInitArgsForTest(
			repo,
			"/bin/sh", "-c",
			"printf 'hello from the agent\\n' > agent-note.txt",
		),
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("agent registration code=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{
		"--repo", repo,
		"run",
		"--id", "tx:auto-local-runtime",
		"--intent", "Add a note through the local Runtime",
		"--agent", "coding-agent",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("automatic local run code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Local Runtime: started temporarily") ||
		!strings.Contains(stdout.String(), "Transaction: tx:auto-local-runtime") ||
		!strings.Contains(stdout.String(), "Next: `gatemole review tx:auto-local-runtime`") {
		t.Fatalf(
			"run was not self-guiding:\nstdout=%s\nstderr=%s",
			stdout.String(),
			stderr.String(),
		)
	}
	assertAutomaticRuntimeStopped(t, repo)

	stdout.Reset()
	stderr.Reset()
	code = Main(
		[]string{"--repo", repo, "review", "tx:auto-local-runtime"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("automatic local review code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "hello from the agent") ||
		!strings.Contains(stdout.String(), "Exact diff: sha256:") {
		t.Fatalf("review did not expose the frozen patch:\n%s", stdout.String())
	}
	assertAutomaticRuntimeStopped(t, repo)

	stdout.Reset()
	stderr.Reset()
	code = Main(
		[]string{"--repo", repo, "reject", "tx:auto-local-runtime"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("automatic local reject code=%d stderr=%s", code, stderr.String())
	}
	assertAutomaticRuntimeStopped(t, repo)
}

func TestAutomaticLocalRuntimeDoesNotReplaceExplicitDaemonSelection(t *testing.T) {
	repo := transactionRunRepository(t)
	customSocket := filepath.Join(t.TempDir(), "configured.sock")
	var stdout, stderr bytes.Buffer
	code := Main([]string{
		"--repo", repo,
		"run",
		"--id", "tx:external-runtime",
		"--intent", "Use the explicitly configured Runtime",
		"--runtime", "host",
		"--unsafe-host",
		"--socket", customSocket,
		"--",
		"/bin/true",
	}, &stdout, &stderr)
	if code != 1 ||
		!strings.Contains(stderr.String(), "an explicit daemon socket was selected") ||
		strings.Contains(stderr.String(), "Local Runtime: started temporarily") {
		t.Fatalf(
			"explicit daemon selection was not preserved: code=%d stderr=%s",
			code,
			stderr.String(),
		)
	}
	if _, err := os.Lstat(defaultKernelSocket(repo)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default Runtime socket was created: %v", err)
	}
}

func assertAutomaticRuntimeStopped(t *testing.T, repo string) {
	t.Helper()
	if _, err := os.Lstat(defaultKernelSocket(repo)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary Runtime socket remains after command: %v", err)
	}
}
