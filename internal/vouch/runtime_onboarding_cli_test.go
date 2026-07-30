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
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/runtimeidentity"
	"github.com/duriantaco/vouch/internal/kernel/runtimepreflight"
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
	for _, entry := range []string{
		"kernel.db\n",
		"kernel.db-journal\n",
		"kernel.db-shm\n",
		"kernel.db-wal\n",
		"kernel.db.lock\n",
		"runtime.lock\n",
		"gatemoled.sock\n",
		"runtime.json\n",
	} {
		if !bytes.Contains(ignore, []byte(entry)) {
			t.Fatalf("runtime ignore is missing %q: %s", entry, ignore)
		}
	}
	if bytes.Contains(ignore, []byte("agent-profiles.json\n")) {
		t.Fatal("runtime ignore hides the repo-owned agent profile")
	}
	originalIgnore := append([]byte(nil), ignore...)
	identity, err := runtimeidentity.Load(context.Background(), repo)
	if err != nil {
		t.Fatalf("generated Runtime identity is invalid: %v", err)
	}
	if !runtimeidentity.IsRuntimeID(identity.RuntimeID) {
		t.Fatalf("generated Runtime ID is invalid: %q", identity.RuntimeID)
	}
	identityPath := filepath.Join(
		repo,
		filepath.FromSlash(runtimeidentity.IdentityRelativePath),
	)
	originalIdentity, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := trackedRuntimeState(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracked) != 0 {
		t.Fatalf("Runtime init left local state tracked by Git: %q", tracked)
	}

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
	afterIdentity, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(originalIdentity, afterIdentity) {
		t.Fatal("identical init rewrote the Runtime identity")
	}

	different := runtimeInitArgsForTest(repo, "/opt/agent", "different")
	stdout.Reset()
	stderr.Reset()
	if code := Main(different, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "different content") {
		t.Fatalf("different init was not refused: code=%d stderr=%s", code, stderr.String())
	}

	mergeRepo := runtimeGitRepoForTest(t)
	if err := os.Mkdir(filepath.Join(mergeRepo, ".gatemole"), 0o700); err != nil {
		t.Fatal(err)
	}
	mergeIgnore := filepath.Join(mergeRepo, runtimeIgnoreFile)
	if err := os.WriteFile(
		mergeIgnore,
		[]byte(
			"# Local Vouch Runtime state. Keep agent-profiles.json under version control.\n"+
				"custom-entry\n",
		),
		0o600,
	); err != nil {
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
	if bytes.Count(
		merged,
		[]byte("# Local Vouch Runtime state."),
	) != 1 {
		t.Fatalf("Runtime ignore marker was duplicated: %s", merged)
	}
}

func TestRuntimeInitIdentityOnlyPreservesExistingProfiles(t *testing.T) {
	repo := runtimeGitRepoForTest(t)
	if err := os.Mkdir(filepath.Join(repo, ".gatemole"), 0o700); err != nil {
		t.Fatal(err)
	}
	first := validAgentProfileForTest()
	second := validAgentProfileForTest()
	second.Name = "review-agent"
	second.OCIImage = "registry.example.invalid/reviewer@" +
		"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	second.Descriptor.ID = "image.review-agent.v1"
	second.Descriptor.Digest =
		"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	second.Descriptor.SourceDigest =
		"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	profilesPath := filepath.Join(repo, defaultAgentProfiles)
	profileData, err := json.MarshalIndent(agentProfileDocument{
		Version:  agentProfilesVersion,
		Profiles: []agentProfileEntry{first, second},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	profileData = append(profileData, '\n')
	if err := os.WriteFile(profilesPath, profileData, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Main(
		[]string{"--repo", repo, "runtime", "init"},
		&stdout,
		&stderr,
	); code != 0 {
		t.Fatalf(
			"identity-only init code=%d stderr=%s",
			code,
			stderr.String(),
		)
	}
	if strings.Contains(stdout.String(), "Agent profile:") {
		t.Fatalf(
			"identity-only init claimed to register a profile: %s",
			stdout.String(),
		)
	}
	after, err := os.ReadFile(profilesPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(profileData, after) {
		t.Fatal("identity-only init rewrote existing agent profiles")
	}
	identity, err := runtimeidentity.Load(context.Background(), repo)
	if err != nil {
		t.Fatalf("identity-only init did not create a valid identity: %v", err)
	}
	beforeIdentity := identity

	stdout.Reset()
	stderr.Reset()
	if code := Main(
		[]string{"--repo", repo, "runtime", "init"},
		&stdout,
		&stderr,
	); code != 0 {
		t.Fatalf(
			"identity-only retry code=%d stderr=%s",
			code,
			stderr.String(),
		)
	}
	afterIdentity, err := runtimeidentity.Load(
		context.Background(),
		repo,
	)
	if err != nil {
		t.Fatal(err)
	}
	if afterIdentity != beforeIdentity {
		t.Fatal("identity-only retry changed Runtime identity")
	}
}

func TestRuntimeInitRejectsPartialProfileRegistration(t *testing.T) {
	repo := runtimeGitRepoForTest(t)
	var stdout, stderr bytes.Buffer
	code := Main(
		[]string{
			"--repo", repo,
			"runtime", "init",
			"--agent", "incomplete-agent",
		},
		&stdout,
		&stderr,
	)
	if code != 2 ||
		!strings.Contains(
			stderr.String(),
			"profile registration requires",
		) {
		t.Fatalf(
			"partial registration code=%d stderr=%s",
			code,
			stderr.String(),
		)
	}
	if _, err := os.Lstat(filepath.Join(repo, ".gatemole")); !errors.Is(
		err,
		os.ErrNotExist,
	) {
		t.Fatalf("partial registration wrote Runtime state: %v", err)
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
	if _, err := os.Stat(filepath.Join(repo, ".gatemole", "config.json")); err != nil {
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

func TestRuntimeInitRejectsUnsafeOrMalformedControlStateBeforeWrites(
	t *testing.T,
) {
	t.Run("group-writable directory", func(t *testing.T) {
		repo := runtimeGitRepoForTest(t)
		directory := filepath.Join(repo, ".gatemole")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o770); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := Main(
			runtimeInitArgsForTest(repo, "/opt/agent"),
			&stdout,
			&stderr,
		)
		if code != 1 ||
			!strings.Contains(stderr.String(), "group- or world-writable") {
			t.Fatalf(
				"unsafe directory code=%d stderr=%s",
				code,
				stderr.String(),
			)
		}
		for _, path := range []string{
			filepath.Join(repo, defaultAgentProfiles),
			filepath.Join(repo, runtimeIgnoreFile),
		} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe directory initialization wrote %s: %v", path, err)
			}
		}
	})

	t.Run("malformed existing identity", func(t *testing.T) {
		repo := runtimeGitRepoForTest(t)
		directory := filepath.Join(repo, ".gatemole")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		ignorePath := filepath.Join(repo, runtimeIgnoreFile)
		originalIgnore := []byte("runtime.json\n")
		if err := os.WriteFile(ignorePath, originalIgnore, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(
				repo,
				filepath.FromSlash(runtimeidentity.IdentityRelativePath),
			),
			[]byte(`{"version":"invalid"}`),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := Main(
			runtimeInitArgsForTest(repo, "/opt/agent"),
			&stdout,
			&stderr,
		)
		if code != 1 ||
			!strings.Contains(stderr.String(), "validate existing Runtime identity") {
			t.Fatalf(
				"malformed identity code=%d stderr=%s",
				code,
				stderr.String(),
			)
		}
		if _, err := os.Lstat(
			filepath.Join(repo, defaultAgentProfiles),
		); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("malformed identity created profile: %v", err)
		}
		afterIgnore, err := os.ReadFile(ignorePath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(afterIgnore, originalIgnore) {
			t.Fatalf("malformed identity changed ignore: %q", afterIgnore)
		}
	})
}

func TestAppendRuntimeIgnoreEntriesRejectsPathReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, ".gitignore")
	original := []byte("custom-entry\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	originalPath := path + ".original"
	if err := os.Rename(path, originalPath); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "target")
	targetData := []byte("do-not-change\n")
	if err := os.WriteFile(target, targetData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	err = appendRuntimeIgnoreEntries(
		path,
		expected,
		original,
		[]string{"runtime.json"},
	)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("replaced Runtime ignore was accepted: %v", err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, targetData) {
		t.Fatalf("replacement target was modified: %q", after)
	}
}

func TestRuntimeDoctorDistinguishesWarningsFailuresAndSelectedImages(t *testing.T) {
	repo := runtimeGitRepoForTest(t)
	initializeRuntimeIdentityForDoctorTest(t, repo)
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
		daemonPreflight: func(
			context.Context,
			string,
			string,
			runtimepreflight.Request,
		) (runtimepreflight.Result, error) {
			t.Fatal("missing daemon socket should not be probed")
			return runtimepreflight.Result{}, nil
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

	stdout.Reset()
	stderr.Reset()
	code = runtimeDoctorCommandWithProbes(
		repo,
		[]string{
			"--agent", first.Name,
			"--agent-profiles", profilesPath,
			"--require-enforcement-profile", "production",
		},
		true,
		&stdout,
		&stderr,
		probes,
	)
	if code != 1 {
		t.Fatalf(
			"offline doctor claimed required production profile: code=%d stderr=%s",
			code,
			stderr.String(),
		)
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

	if err := os.WriteFile(profilesPath, []byte(`{"version":"gatemole.agent_profiles.v0","profiles":[]}`), 0o600); err != nil {
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

func TestRuntimeDoctorUsesDaemonPreflightAsAuthority(t *testing.T) {
	repo := runtimeGitRepoForTest(t)
	identity := initializeRuntimeIdentityForDoctorTest(t, repo)
	profile := validAgentProfileForTest()
	profilesPath := writeAgentProfilesForTest(t, repo, profile)
	socketPath := filepath.Join(repo, ".gatemole", "gatemoled.sock")

	preflightCalls := 0
	probes := runtimeDoctorProbes{
		gitRoot: func(context.Context, string) (string, error) {
			return repo, nil
		},
		ociEngine: func(context.Context, string) (string, error) {
			t.Fatal("secure daemon socket must make the daemon engine authoritative")
			return "", nil
		},
		ociImage: func(context.Context, string, string) error {
			t.Fatal("secure daemon socket must make daemon image inspection authoritative")
			return nil
		},
		socketInfo: func(gotPath string) (os.FileInfo, error) {
			if gotPath != socketPath {
				t.Fatalf("socket path=%q, want %q", gotPath, socketPath)
			}
			return runtimeSocketInfoForTest{}, nil
		},
		daemonPreflight: func(
			_ context.Context,
			gotSocket string,
			namespace string,
			request runtimepreflight.Request,
		) (runtimepreflight.Result, error) {
			preflightCalls++
			if gotSocket != socketPath ||
				namespace != "payments" ||
				request.ExpectedRuntimeID != identity.RuntimeID ||
				request.RequiredEnforcementProfile != "production" ||
				request.Agent == nil ||
				request.Agent.Profile.ID != profile.Name ||
				request.Agent.OCIImage != profile.OCIImage {
				t.Fatalf(
					"unexpected daemon preflight: socket=%q namespace=%q request=%#v",
					gotSocket,
					namespace,
					request,
				)
			}
			return runtimepreflight.Result{
				Version:            runtimepreflight.ResultVersion,
				RuntimeID:          identity.RuntimeID,
				EnforcementProfile: "production",
				Ready:              true,
				AgentProfileID:     profile.Name,
				ImageDigest:        profile.Descriptor.Digest,
			}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := runtimeDoctorCommandWithProbes(
		repo,
		[]string{
			"--namespace", "payments",
			"--require-enforcement-profile", "production",
			"--agent", profile.Name,
			"--agent-profiles", profilesPath,
			"--socket", socketPath,
		},
		true,
		&stdout,
		&stderr,
		probes,
	)
	if code != 0 {
		t.Fatalf("daemon-authoritative doctor failed: code=%d stderr=%s", code, stderr.String())
	}
	if preflightCalls != 1 {
		t.Fatalf("daemon preflight calls=%d, want 1", preflightCalls)
	}
	var result runtimeDoctorResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"runtime_identity",
		"runtime_state",
		"enforcement_profile",
		"oci_engine",
		"agent_images",
		"daemon",
	} {
		if status := doctorCheckStatusForTest(result, name); status != "pass" {
			t.Fatalf("%s status=%q result=%#v", name, status, result)
		}
	}
}

func TestRuntimeDoctorClassifiesProfileRejectionBeforeEngine(
	t *testing.T,
) {
	repo := runtimeGitRepoForTest(t)
	identity := initializeRuntimeIdentityForDoctorTest(t, repo)
	socketPath := filepath.Join(repo, ".gatemole", "gatemoled.sock")
	probes := runtimeDoctorProbes{
		gitRoot: func(context.Context, string) (string, error) {
			return repo, nil
		},
		ociEngine: func(context.Context, string) (string, error) {
			t.Fatal("daemon preflight must remain authoritative")
			return "", nil
		},
		ociImage: func(context.Context, string, string) error {
			t.Fatal("daemon preflight must remain authoritative")
			return nil
		},
		socketInfo: func(string) (os.FileInfo, error) {
			return runtimeSocketInfoForTest{}, nil
		},
		daemonPreflight: func(
			_ context.Context,
			_ string,
			_ string,
			request runtimepreflight.Request,
		) (runtimepreflight.Result, error) {
			if request.ExpectedRuntimeID != identity.RuntimeID ||
				request.RequiredEnforcementProfile != "production" {
				t.Fatalf("unexpected preflight request: %#v", request)
			}
			return runtimepreflight.Result{}, &model.KernelError{
				Code:      model.ErrorCapabilityDenied,
				Operation: "preflight_runtime",
				Message:   "daemon enforcement profile does not satisfy the request",
			}
		},
	}

	var stdout, stderr bytes.Buffer
	code := runtimeDoctorCommandWithProbes(
		repo,
		[]string{
			"--socket", socketPath,
			"--require-enforcement-profile", "production",
		},
		true,
		&stdout,
		&stderr,
		probes,
	)
	if code != 1 {
		t.Fatalf("profile downgrade doctor code=%d stderr=%s", code, stderr.String())
	}
	var result runtimeDoctorResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if doctorCheckStatusForTest(result, "enforcement_profile") != "fail" ||
		doctorCheckStatusForTest(result, "oci_engine") != "warn" ||
		doctorCheckStatusForTest(result, "daemon") != "fail" {
		t.Fatalf("profile rejection was misclassified: %#v", result)
	}
}

func TestRuntimeInitAndDoctorRejectTrackedLocalState(t *testing.T) {
	repo := runtimeGitRepoForTest(t)
	var stdout, stderr bytes.Buffer
	args := runtimeInitArgsForTest(repo, "/opt/agent")
	if code := Main(args, &stdout, &stderr); code != 0 {
		t.Fatalf("runtime init code=%d stderr=%s", code, stderr.String())
	}

	trackedPath := filepath.Join(repo, ".gatemole", "kernel.db")
	if err := os.WriteFile(trackedPath, []byte("not-a-ledger"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		"git",
		"-C",
		repo,
		"add",
		"--force",
		"--",
		".gatemole/kernel.db",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("track local Runtime state: %v: %s", err, output)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Main(args, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "must not be tracked by Git") {
		t.Fatalf(
			"runtime init accepted tracked state: code=%d stderr=%s",
			code,
			stderr.String(),
		)
	}

	probes := runtimeDoctorProbes{
		gitRoot: func(context.Context, string) (string, error) {
			return repo, nil
		},
		ociEngine: func(context.Context, string) (string, error) {
			return "/fake/docker", nil
		},
		ociImage: func(context.Context, string, string) error {
			return nil
		},
		daemonPreflight: func(
			context.Context,
			string,
			string,
			runtimepreflight.Request,
		) (runtimepreflight.Result, error) {
			t.Fatal("missing daemon socket should not be probed")
			return runtimepreflight.Result{}, nil
		},
	}
	stdout.Reset()
	stderr.Reset()
	code := runtimeDoctorCommandWithProbes(
		repo,
		[]string{"--agent", "coding-agent"},
		true,
		&stdout,
		&stderr,
		probes,
	)
	if code != 1 {
		t.Fatalf("doctor accepted tracked Runtime state: code=%d", code)
	}
	var result runtimeDoctorResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if status := doctorCheckStatusForTest(result, "runtime_state"); status != "fail" {
		t.Fatalf("runtime_state status=%q result=%#v", status, result)
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
		"-c", "user.email=gatemole-test@example.invalid",
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

func initializeRuntimeIdentityForDoctorTest(
	t *testing.T,
	repo string,
) runtimeidentity.Identity {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ".gatemole"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(repo, runtimeIgnoreFile),
		runtimeStateIgnore,
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	identity, _, err := runtimeidentity.CreateOrLoad(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func doctorCheckStatusForTest(result runtimeDoctorResult, name string) string {
	for _, check := range result.Checks {
		if check.Name == name {
			return check.Status
		}
	}
	return ""
}

type runtimeSocketInfoForTest struct{}

func (runtimeSocketInfoForTest) Name() string       { return "gatemoled.sock" }
func (runtimeSocketInfoForTest) Size() int64        { return 0 }
func (runtimeSocketInfoForTest) Mode() os.FileMode  { return os.ModeSocket | 0o600 }
func (runtimeSocketInfoForTest) ModTime() time.Time { return time.Time{} }
func (runtimeSocketInfoForTest) IsDir() bool        { return false }
func (runtimeSocketInfoForTest) Sys() any           { return nil }
