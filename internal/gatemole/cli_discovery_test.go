package gatemole

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelpIsSuccessfulAndDoesNotRequireRepositoryState(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "does-not-exist")
	for _, args := range [][]string{
		{"--repo", repo, "--help"},
		{"--repo", repo, "help"},
		{"--repo", repo, "help", "run"},
		{"--repo", repo, "daemon", "--help"},
		{"--repo", repo, "run", "--help"},
		{"--repo", repo, "runtime", "--help"},
		{"--repo", repo, "doctor", "-h"},
		{"--repo", repo, "review", "--help"},
		{"--repo", repo, "diff", "--help"},
		{"--repo", repo, "apply", "--help"},
		{"--repo", repo, "reject", "--help"},
		{"--repo", repo, "tx", "effects", "--help"},
	} {
		t.Run(strings.Join(args[2:], "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Main(args, &stdout, &stderr); code != 0 {
				t.Fatalf("help returned %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "usage: gatemole") {
				t.Fatalf("help did not write usage to stdout: %q", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("help wrote stderr: %q", stderr.String())
			}
		})
	}
}

func TestDaemonHelpIncludesProductionControls(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "does-not-exist")
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"--repo", repo, "daemon", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("daemon help returned %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, flagName := range []string{
		"-allowed-images",
		"-approval-trust",
		"-identity-trust",
		"-model-broker-policy",
		"-verifier-profiles",
	} {
		if !strings.Contains(stdout.String(), flagName) {
			t.Errorf("daemon help omitted %s: %q", flagName, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("daemon help wrote stderr: %q", stderr.String())
	}
}

func TestHelpAfterCommandSeparatorRemainsAnAgentArgument(t *testing.T) {
	if helpFlagBeforeSeparator([]string{"--", "--help"}) {
		t.Fatal("agent --help after the command separator was treated as CLI help")
	}
	if !helpFlagBeforeSeparator([]string{"--intent", "test", "--help", "--", "agent"}) {
		t.Fatal("CLI --help before the command separator was not detected")
	}
}

func TestVersionIsStateIndependentAndMachineReadable(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "does-not-exist")
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"--repo", repo, "--json", "version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version returned %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var info cliVersionInfo
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		t.Fatalf("decode version JSON: %v; output=%q", err, stdout.String())
	}
	if info.Schema != cliVersionSchema || info.Version == "" || info.GoVersion == "" {
		t.Fatalf("incomplete version information: %+v", info)
	}
	if stderr.Len() != 0 {
		t.Fatalf("version wrote stderr: %q", stderr.String())
	}
}

func TestConventionalVersionAliasesAreSupported(t *testing.T) {
	for _, argument := range []string{"--version", "-V"} {
		var stdout, stderr bytes.Buffer
		if code := Main([]string{argument}, &stdout, &stderr); code != 0 {
			t.Fatalf("%s returned %d; stdout=%q stderr=%q", argument, code, stdout.String(), stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "gatemole ") || stderr.Len() != 0 {
			t.Fatalf("%s output: stdout=%q stderr=%q", argument, stdout.String(), stderr.String())
		}
	}
}

func TestVersionRejectsUnexpectedArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"version", "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("version returned %d, want 2", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "unexpected argument") {
		t.Fatalf("unexpected output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
