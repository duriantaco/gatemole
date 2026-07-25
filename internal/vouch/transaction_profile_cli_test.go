package vouch

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duriantaco/vouch/internal/kernel/model"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

const (
	testAgentImageDigest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testAgentSourceDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestResolveNamedAgentProfileBindsDescriptorAndFinalCommand(t *testing.T) {
	repo := t.TempDir()
	path := writeAgentProfilesForTest(t, repo, validAgentProfileForTest())

	resolved, err := resolveNamedAgentProfile(
		repo, path, "coding-agent", []string{"--task-mode", "safe"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != "coding-agent" ||
		resolved.RuntimeClass != "oci" ||
		resolved.OCIImage != "registry.example.invalid/coding-agent@"+testAgentImageDigest ||
		resolved.ImageDigest != testAgentImageDigest {
		t.Fatalf("unexpected resolved profile: %#v", resolved)
	}
	wantCommand := []string{"/opt/vouch-agent", "serve", "--task-mode", "safe"}
	if strings.Join(resolved.Command, "\x00") != strings.Join(wantCommand, "\x00") {
		t.Fatalf("command=%q, want %q", resolved.Command, wantCommand)
	}
	binding, err := resolved.binding()
	if err != nil {
		t.Fatal(err)
	}
	wantCommandDigest, err := transactionreducer.ComputeCommandDigest(wantCommand)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ID != "coding-agent" ||
		binding.Digest == "" ||
		binding.ImageDigest != testAgentImageDigest ||
		binding.Entrypoint != "/opt/vouch-agent" ||
		binding.CommandDigest != wantCommandDigest {
		t.Fatalf("unexpected task binding: %#v", binding)
	}
}

func TestPublishedAgentProfileFixtureResolves(t *testing.T) {
	path, err := filepath.Abs(filepath.Join(
		"..", "..", "schemas", "fixtures", "runtime", "valid", "agent_profiles.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveNamedAgentProfile(
		t.TempDir(), path, "coding-agent", nil,
	)
	if err != nil {
		t.Fatalf("published agent profile fixture is not loadable: %v", err)
	}
	if resolved.ImageDigest != testAgentImageDigest ||
		len(resolved.Command) != 2 {
		t.Fatalf("unexpected published profile: %#v", resolved)
	}
}

func TestResolveNamedAgentProfileRejectsUnknownFieldsAndDigestDrift(t *testing.T) {
	repo := t.TempDir()
	unknownPath := filepath.Join(repo, "unknown.json")
	if err := os.WriteFile(
		unknownPath,
		[]byte(`{"version":"vouch.agent_profiles.v0","profiles":[],"unexpected":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveNamedAgentProfile(repo, unknownPath, "coding-agent", nil); err == nil ||
		!strings.Contains(err.Error(), string(model.ErrorUnknownField)) {
		t.Fatalf("unknown field was not rejected: %v", err)
	}

	profile := validAgentProfileForTest()
	profile.Descriptor.Digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	driftPath := writeAgentProfilesForTest(t, repo, profile)
	if _, err := resolveNamedAgentProfile(repo, driftPath, "coding-agent", nil); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("image digest drift was not rejected: %v", err)
	}

	profile = validAgentProfileForTest()
	profile.OCIImage = "registry.example.invalid/coding-agent@junk@" + testAgentImageDigest
	malformedPath := writeAgentProfilesForTest(t, repo, profile)
	if _, err := resolveNamedAgentProfile(repo, malformedPath, "coding-agent", nil); err == nil ||
		!strings.Contains(err.Error(), "invalid") {
		t.Fatalf("malformed multi-@ image reference was not rejected: %v", err)
	}
}

func TestAdHocAgentProfileIsDeterministicAndCommandBound(t *testing.T) {
	first, err := resolveAdHocAgentProfile(
		"host", "", []string{"/bin/sh", "-c", "exit 0"},
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolveAdHocAgentProfile(
		"host", "", []string{"/bin/sh", "-c", "exit 0"},
	)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := resolveAdHocAgentProfile(
		"host", "", []string{"/bin/sh", "-c", "exit 1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Digest != second.Digest {
		t.Fatalf("ad-hoc profile was not deterministic: %#v %#v", first, second)
	}
	if first.ID == changed.ID || first.Digest == changed.Digest {
		t.Fatal("ad-hoc profile did not bind the final command")
	}
}

func TestRuntimeRunRoutesTransactionAndLegacyRunSurfaces(t *testing.T) {
	repo := t.TempDir()
	var stdout, stderr bytes.Buffer
	transactionFactory := func(string) transactionClient {
		t.Fatal("invalid primary command reached the transaction client")
		return nil
	}
	kernelFactory := func(string) kernelRunClient {
		t.Fatal("invalid legacy command reached the kernel client")
		return nil
	}

	code := runtimeRunCommandWithFactories(
		repo,
		[]string{"--intent", "change auth"},
		false,
		&stdout,
		&stderr,
		transactionFactory,
		kernelFactory,
	)
	if code != 2 || !strings.Contains(stderr.String(), "run requires --agent or a raw command") {
		t.Fatalf("primary run was not transaction-shaped: code=%d stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = runtimeRunCommandWithFactories(
		repo,
		[]string{"get"},
		false,
		&stdout,
		&stderr,
		transactionFactory,
		kernelFactory,
	)
	if code != 2 || !strings.Contains(stderr.String(), "run get requires --namespace and --id") {
		t.Fatalf("legacy run compatibility did not route: code=%d stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = kernelCommandWithFactory(
		repo,
		[]string{"run", "get"},
		false,
		&stdout,
		&stderr,
		kernelFactory,
	)
	if code != 2 || !strings.Contains(stderr.String(), "run get requires --namespace and --id") {
		t.Fatalf("explicit kernel run did not route: code=%d stderr=%s", code, stderr.String())
	}
}

func TestTopLevelTransactionAliasesRemainThin(t *testing.T) {
	repo := t.TempDir()
	newClient := func(string) transactionClient {
		t.Fatal("invalid alias reached the transaction client")
		return nil
	}
	for _, alias := range []string{"status", "approve", "release"} {
		t.Run(alias, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := transactionAliasCommandWithFactory(
				alias, repo, nil, false, &stdout, &stderr, newClient,
			)
			if code != 2 {
				t.Fatalf("%s code=%d stderr=%s", alias, code, stderr.String())
			}
		})
	}
}

func TestTopLevelTransactionAliasesAcceptPositionalIDAndDefaultLocalNamespace(t *testing.T) {
	normalized, err := normalizeTopLevelTransactionArgs([]string{
		"tx:auth-fix",
		"--socket", "/tmp/vouchd.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(normalized, "\x00")
	for _, required := range []string{
		"--namespace\x00local",
		"--id\x00tx:auth-fix",
		"--socket\x00/tmp/vouchd.sock",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("normalized args %q do not contain %q", normalized, required)
		}
	}

	normalized, err = normalizeTopLevelTransactionArgs([]string{
		"--namespace", "payments",
		"--id", "tx:explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.Join(normalized, "\x00"), "--namespace") != 1 {
		t.Fatalf("explicit namespace was duplicated: %q", normalized)
	}

	if _, err := normalizeTopLevelTransactionArgs([]string{
		"tx:positional", "--id", "tx:flag",
	}); err == nil {
		t.Fatal("positional and flag transaction IDs were both accepted")
	}
}

func validAgentProfileForTest() agentProfileEntry {
	return agentProfileEntry{
		Name:     "coding-agent",
		OCIImage: "registry.example.invalid/coding-agent@" + testAgentImageDigest,
		Descriptor: model.AgentImage{
			Version: model.AgentImageVersion,
			ID:      "image.coding-agent.v1",
			Digest:  testAgentImageDigest,
			Runtime: model.AgentRuntime{
				Adapter:        "oci",
				AdapterVersion: "v1",
				Entrypoint:     []string{"/opt/vouch-agent", "serve"},
			},
			SourceDigest: testAgentSourceDigest,
			Publisher: model.Principal{
				ID: "service:agent-publisher", Kind: model.PrincipalService,
				Issuer: "https://publisher.example.invalid",
			},
		},
	}
}

func writeAgentProfilesForTest(t *testing.T, repo string, profiles ...agentProfileEntry) string {
	t.Helper()
	path := filepath.Join(repo, "agent-profiles.json")
	data, err := json.Marshal(agentProfileDocument{
		Version: agentProfilesVersion, Profiles: profiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
