package vouch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeInitCreatesStrictProfileAndIsIdempotent(t *testing.T) {
	repo := runtimeGitRepoForTest(t)
	args := runtimeInitArgsForTest(
		repo, "/opt/agent", "run", "--repo", "/agent/workspace", "--json",
	)
	var stdout, stderr bytes.Buffer
	if code := Main(args, &stdout, &stderr); code != 0 {
		t.Fatalf("runtime init code=%d stderr=%s", code, stderr.String())
	}

	profilesPath := filepath.Join(repo, defaultAgentProfiles)
	data, err := readAgentProfilesFile(profilesPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := decodeAgentProfileDocument(data)
	if err != nil {
		t.Fatalf("generated profile is not strict and valid: %v", err)
	}
	if len(document.Profiles) != 1 ||
		document.Profiles[0].Name != "coding-agent" ||
		strings.Join(document.Profiles[0].Descriptor.Runtime.Entrypoint, " ") !=
			"/opt/agent run --repo /agent/workspace --json" {
		t.Fatalf("unexpected generated profile: %#v", document)
	}
	original := append([]byte(nil), data...)
	ignore, err := os.ReadFile(filepath.Join(repo, runtimeIgnoreFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"kernel.db\n", "kernel.db-shm\n", "kernel.db-wal\n", "kernel.db.lock\n", "vouchd.sock\n"} {
		if !bytes.Contains(ignore, []byte(entry)) {
			t.Fatalf("runtime ignore is missing %q: %s", entry, ignore)
		}
	}
	if bytes.Contains(ignore, []byte("agent-profiles.json\n")) {
		t.Fatal("runtime ignore hides the repo-owned agent profile")
	}
	originalIgnore := append([]byte(nil), ignore...)

	stdout.Reset()
	stderr.Reset()
	if code := Main(args, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "already initialized") {
		t.Fatalf("identical init was not idempotent: code=%d stderr=%s", code, stderr.String())
	}
	after, err := os.ReadFile(profilesPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("identical init rewrote the profile")
	}
	afterIgnore, err := os.ReadFile(filepath.Join(repo, runtimeIgnoreFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(originalIgnore, afterIgnore) {
		t.Fatal("identical init rewrote the complete Runtime ignore")
	}

	different := runtimeInitArgsForTest(repo, "/opt/agent", "different")
	stdout.Reset()
	stderr.Reset()
	if code := Main(different, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "different content") {
		t.Fatalf("different init was not refused: code=%d stderr=%s", code, stderr.String())
	}

	mergeRepo := runtimeGitRepoForTest(t)
	if err := os.Mkdir(filepath.Join(mergeRepo, ".vouch"), 0o700); err != nil {
		t.Fatal(err)
	}
	mergeIgnore := filepath.Join(mergeRepo, runtimeIgnoreFile)
	if err := os.WriteFile(mergeIgnore, []byte("custom-entry\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main(runtimeInitArgsForTest(mergeRepo, "/opt/agent"), &stdout, &stderr); code != 0 {
		t.Fatalf("init did not merge Runtime ignore rules: code=%d stderr=%s", code, stderr.String())
	}
	merged, err := os.ReadFile(mergeIgnore)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(merged, []byte("custom-entry\n")) ||
		len(missingRuntimeIgnoreEntries(merged)) != 0 {
		t.Fatalf("existing Runtime ignore content was not safely completed: %s", merged)
	}
}

func TestRuntimeInitPreservesContractsInitAndRefusesSymlink(t *testing.T) {
	repo := runtimeGitRepoForTest(t)
	var stdout, stderr bytes.Buffer
	if code := Main(
		[]string{"--repo", repo, "init", "--profile", "generic"},
		&stdout, &stderr,
	); code != 0 {
		t.Fatalf("legacy Contracts init failed: code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(repo, ".vouch", "config.json")); err != nil {
		t.Fatalf("legacy Contracts config is missing: %v", err)
	}

	target := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(repo, defaultAgentProfiles)); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main(runtimeInitArgsForTest(repo, "/opt/agent"), &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "not a regular file") {
		t.Fatalf("symlink profile was not refused: code=%d stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("symlink target changed: data=%q err=%v", data, err)
	}
}

func TestRuntimeDoctorDistinguishesWarningsFailuresAndSelectedImages(t *testing.T) {
	repo := runtimeGitRepoForTest(t)
	first := validAgentProfileForTest()
	second := validAgentProfileForTest()
	second.Name = "other-agent"
	second.OCIImage = "registry.example.invalid/other@" +
		"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	second.Descriptor.ID = "image.other-agent.v1"
	second.Descriptor.Digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	second.Descriptor.SourceDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	profilesPath := writeAgentProfilesForTest(t, repo, first, second)

	probes := runtimeDoctorProbes{
		gitRoot: func(context.Context, string) (string, error) { return repo, nil },
		ociEngine: func(context.Context, string) (string, error) {
			return "/fake/docker", nil
		},
		ociImage: func(_ context.Context, _ string, image string) error {
			if image == second.OCIImage {
				return errors.New("not found")
			}
			return nil
		},
		daemonReady: func(context.Context, string) error {
			t.Fatal("missing daemon socket should not be probed")
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := runtimeDoctorCommandWithProbes(
		repo,
		[]string{"--agent", first.Name, "--agent-profiles", profilesPath},
		true,
		&stdout, &stderr, probes,
	)
	if code != 0 {
		t.Fatalf("selected available image should pass: code=%d stderr=%s", code, stderr.String())
	}
	var result runtimeDoctorResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Ready ||
		doctorCheckStatusForTest(result, "agent_images") != "pass" ||
		doctorCheckStatusForTest(result, "daemon") != "warn" {
		t.Fatalf("unexpected warning result: %#v", result)
	}

	mismatchedRoot := probes
	mismatchedRoot.gitRoot = func(context.Context, string) (string, error) {
		return filepath.Dir(repo), nil
	}
	stdout.Reset()
	stderr.Reset()
	code = runtimeDoctorCommandWithProbes(
		repo,
		[]string{"--agent", first.Name, "--agent-profiles", profilesPath},
		true,
		&stdout, &stderr, mismatchedRoot,
	)
	if code != 1 {
		t.Fatalf("non-root --repo did not fail doctor: code=%d", code)
	}

	stdout.Reset()
	stderr.Reset()
	code = runtimeDoctorCommandWithProbes(
		repo,
		[]string{"--agent", second.Name, "--agent-profiles", profilesPath},
		true,
		&stdout, &stderr, probes,
	)
	if code != 1 {
		t.Fatalf("selected missing image did not fail: code=%d", code)
	}

	if err := os.WriteFile(profilesPath, []byte(`{"version":"vouch.agent_profiles.v0","profiles":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = runtimeDoctorCommandWithProbes(
		repo, []string{"--agent-profiles", profilesPath}, true,
		&stdout, &stderr, probes,
	)
	if code != 1 {
		t.Fatalf("invalid existing profiles did not fail: code=%d", code)
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if doctorCheckStatusForTest(result, "agent_profiles") != "fail" {
		t.Fatalf("invalid profile result: %#v", result)
	}
}

func TestCommonArgumentsStopAtAgentCommandSeparator(t *testing.T) {
	for _, args := range [][]string{
		{
			"--repo", "/real/repo", "runtime", "init",
			"--agent", "coding-agent", "--",
			"/opt/agent", "--repo", "/agent/workspace", "--json",
		},
		{
			"--repo", "/real/repo", "run",
			"--intent", "fix auth", "--image", "image@sha256:digest", "--",
			"/opt/agent", "--manifest", "/agent/manifest", "--json=false",
		},
	} {
		common, rest, err := parseCommonArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		if common.repo != "/real/repo" || common.json || common.manifest != "" {
			t.Fatalf("agent arguments mutated common options: %#v", common)
		}
		separator := -1
		for index, value := range rest {
			if value == "--" {
				separator = index
				break
			}
		}
		if separator < 0 || separator+1 >= len(rest) {
			t.Fatalf("command separator or agent command was lost: %q", rest)
		}
		joined := strings.Join(rest[separator+1:], "\x00")
		for _, expected := range []string{"/opt/agent", "--json"} {
			if !strings.Contains(joined, expected) {
				t.Fatalf("agent argument %q was lost from %q", expected, rest)
			}
		}
	}
}

func runtimeGitRepoForTest(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	command := exec.Command("git", "-C", repo, "init", "--quiet")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	command = exec.Command(
		"git", "-C", repo,
		"-c", "user.name=Vouch Test",
		"-c", "user.email=vouch-test@example.invalid",
		"commit", "--allow-empty", "--quiet", "-m", "initial",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	return repo
}

func runtimeInitArgsForTest(repo string, command ...string) []string {
	args := []string{
		"--repo", repo,
		"runtime", "init",
		"--agent", "coding-agent",
		"--image", "registry.example.invalid/coding-agent@" + testAgentImageDigest,
		"--source-digest", testAgentSourceDigest,
		"--",
	}
	return append(args, command...)
}

func doctorCheckStatusForTest(result runtimeDoctorResult, name string) string {
	for _, check := range result.Checks {
		if check.Name == name {
			return check.Status
		}
	}
	return ""
}
