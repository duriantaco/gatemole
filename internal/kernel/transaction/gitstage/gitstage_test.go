package gitstage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
)

func TestLimitedCommandIsCancelledWhenOutputLimitIsExceeded(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is unavailable")
	}
	manager := &Manager{gitPath: shell}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	started := time.Now()
	output, exceeded, err := manager.runLimited(
		ctx,
		"",
		1024,
		"-c",
		"while :; do printf '0123456789abcdef'; done",
	)
	if !exceeded {
		t.Fatalf("output limit was not reported: len=%d err=%v", len(output), err)
	}
	if len(output) != 1024 {
		t.Fatalf("captured output=%d bytes, want 1024", len(output))
	}
	if time.Since(started) >= 2*time.Second {
		t.Fatal("output-producing command was not cancelled promptly")
	}
}

func TestWorktreeStagesExactEffectsWithoutMutatingSource(t *testing.T) {
	repository := createRepository(t)
	stagingRoot := t.TempDir()
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	workspace, err := manager.Create(context.Background(), repository, stagingRoot, "tx:git-demo", "HEAD", now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, statErr := os.Stat(workspace.Path); statErr == nil {
			if discardErr := manager.Discard(context.Background(), workspace); discardErr != nil {
				t.Errorf("discard worktree: %v", discardErr)
			}
		}
	})

	mustWrite(t, filepath.Join(workspace.Path, "internal", "auth", "middleware.go"), []byte("package auth\n\nfunc Allowed() bool { return true }\n"))
	if err := os.Remove(filepath.Join(workspace.Path, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(workspace.Path, "new.txt"), []byte("new staged data\n"))
	externalSecret := filepath.Join(t.TempDir(), "secret.txt")
	mustWrite(t, externalSecret, []byte("must-not-be-read"))
	if err := os.Symlink(externalSecret, filepath.Join(workspace.Path, "external-link")); err != nil {
		t.Fatal(err)
	}

	snapshot, err := manager.Inspect(context.Background(), workspace, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Effects) != 4 {
		t.Fatalf("effects = %d, want 4: %#v", len(snapshot.Effects), snapshot.Effects)
	}
	want := []struct {
		path      string
		operation string
	}{
		{"delete.txt", "delete"},
		{"external-link", "add"},
		{"internal/auth/middleware.go", "modify"},
		{"new.txt", "add"},
	}
	for i, expected := range want {
		effect := snapshot.Effects[i]
		if effect.Sequence != int64(i+1) || effect.Resource.Pattern != expected.path || effect.Operation != expected.operation {
			t.Fatalf("effect %d = %#v, want %s %s", i, effect, expected.operation, expected.path)
		}
		if effect.Status != model.EffectStaged || effect.RecoveryClass != model.RecoveryStageable || effect.StageRef == nil {
			t.Fatalf("effect is not a stageable staged change: %#v", effect)
		}
		if err := effect.Validate(); err != nil {
			t.Fatalf("effect %d invalid: %v", i, err)
		}
	}

	symlink := snapshot.Effects[1]
	if symlink.StageRef.Digest != digestBytes([]byte(externalSecret)) {
		t.Fatalf("symlink digest followed external content: %s", symlink.StageRef.Digest)
	}
	if symlink.StageRef.Digest == digestBytes([]byte("must-not-be-read")) {
		t.Fatal("stage inspection leaked the external symlink target content")
	}

	if err := manager.Verify(context.Background(), snapshot, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("unchanged snapshot failed verification: %v", err)
	}
	mustWrite(t, filepath.Join(workspace.Path, "new.txt"), []byte("changed after approval\n"))
	err = manager.Verify(context.Background(), snapshot, now.Add(3*time.Minute))
	assertCode(t, err, model.ErrorTransactionConflict)

	sourceAuth, err := os.ReadFile(filepath.Join(repository, "internal", "auth", "middleware.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(sourceAuth) != "package auth\n\nfunc Allowed() bool { return false }\n" {
		t.Fatalf("source worktree was mutated: %q", sourceAuth)
	}
	if _, err := os.Stat(filepath.Join(repository, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("untracked staged file escaped into source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repository, "delete.txt")); err != nil {
		t.Fatalf("source deletion escaped staging: %v", err)
	}
}

func TestCreateRejectsWorkspaceInsideSourceRepository(t *testing.T) {
	repository := createRepository(t)
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Create(
		context.Background(),
		repository,
		filepath.Join(repository, ".gatemole", "transactions"),
		"tx:inside",
		"HEAD",
		time.Now(),
	)
	if err == nil {
		t.Fatal("expected staging inside source repository to fail")
	}
}

func TestInspectIsDeterministicAndDuplicateWorkspaceFails(t *testing.T) {
	repository := createRepository(t)
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	stagingRoot := t.TempDir()
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	workspace, err := manager.Create(context.Background(), repository, stagingRoot, "tx:deterministic", "HEAD", now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })
	if _, err := manager.Create(context.Background(), repository, stagingRoot, "tx:deterministic", "HEAD", now); err == nil {
		t.Fatal("duplicate transaction workspace was accepted")
	}
	mustWrite(t, filepath.Join(workspace.Path, "z.txt"), []byte("z"))
	mustWrite(t, filepath.Join(workspace.Path, "a.txt"), []byte("a"))
	first, err := manager.Inspect(context.Background(), workspace, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Inspect(context.Background(), workspace, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.EffectSetDigest != second.EffectSetDigest || first.StagedStateDigest != second.StagedStateDigest {
		t.Fatalf("stable worktree produced different digests: %#v %#v", first, second)
	}
	if first.Effects[0].Resource.Pattern != "a.txt" || first.Effects[1].Resource.Pattern != "z.txt" {
		t.Fatalf("effects are not deterministically sorted: %#v", first.Effects)
	}
}

func TestParseNameStatusFailsClosed(t *testing.T) {
	if _, err := parseNameStatus([]byte("R100\x00old\x00new\x00")); err == nil {
		t.Fatal("rename-shaped status should be rejected when renames are disabled")
	}
	if _, err := parseNameStatus([]byte("M\x00../escape\x00")); err == nil {
		t.Fatal("unsafe path should be rejected")
	}
}

func TestDefaultLimitsAndConfigurationValidation(t *testing.T) {
	want := Limits{
		MaxChangedFiles:      10_000,
		MaxFileBytes:         64 << 20,
		MaxTotalChangedBytes: 512 << 20,
		MaxCapturedDiffBytes: 128 << 20,
		MaxTreeEntries:       100_000,
		MaxTreeBlobBytes:     256 << 20,
		MaxTreeBytes:         1 << 30,
	}
	if got := DefaultLimits(); got != want {
		t.Fatalf("DefaultLimits() = %#v, want %#v", got, want)
	}
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if manager.limits != want {
		t.Fatalf("New() limits = %#v, want %#v", manager.limits, want)
	}

	tests := []struct {
		name   string
		change func(*Limits)
		field  string
	}{
		{
			name:   "changed files",
			change: func(limits *Limits) { limits.MaxChangedFiles = 0 },
			field:  "max_changed_files",
		},
		{
			name:   "file bytes",
			change: func(limits *Limits) { limits.MaxFileBytes = 0 },
			field:  "max_file_bytes",
		},
		{
			name:   "total changed bytes",
			change: func(limits *Limits) { limits.MaxTotalChangedBytes = 0 },
			field:  "max_total_changed_bytes",
		},
		{
			name:   "captured diff bytes",
			change: func(limits *Limits) { limits.MaxCapturedDiffBytes = 0 },
			field:  "max_captured_diff_bytes",
		},
		{
			name:   "tree entries",
			change: func(limits *Limits) { limits.MaxTreeEntries = 0 },
			field:  "max_tree_entries",
		},
		{
			name:   "tree blob bytes",
			change: func(limits *Limits) { limits.MaxTreeBlobBytes = 0 },
			field:  "max_tree_blob_bytes",
		},
		{
			name:   "tree bytes",
			change: func(limits *Limits) { limits.MaxTreeBytes = 0 },
			field:  "max_tree_bytes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limits := DefaultLimits()
			test.change(&limits)
			_, err := NewWithLimits(limits)
			assertKernelError(t, err, model.ErrorSchemaInvalid, "configure_git_stage", "", test.field)
		})
	}
}

func TestInspectEnforcesResourceLimits(t *testing.T) {
	baseLimits := DefaultLimits()
	baseLimits.MaxChangedFiles = 10
	baseLimits.MaxFileBytes = 1 << 10
	baseLimits.MaxTotalChangedBytes = 2 << 10
	baseLimits.MaxCapturedDiffBytes = 1 << 20
	tests := []struct {
		name   string
		limits func() Limits
		mutate func(*testing.T, Workspace)
		field  string
	}{
		{
			name: "changed file count",
			limits: func() Limits {
				limits := baseLimits
				limits.MaxChangedFiles = 1
				return limits
			},
			mutate: func(t *testing.T, workspace Workspace) {
				mustWrite(t, filepath.Join(workspace.Path, "one.txt"), []byte("1"))
				mustWrite(t, filepath.Join(workspace.Path, "two.txt"), []byte("2"))
			},
			field: "changed_files",
		},
		{
			name: "after-state file bytes",
			limits: func() Limits {
				limits := baseLimits
				limits.MaxFileBytes = 4
				return limits
			},
			mutate: func(t *testing.T, workspace Workspace) {
				mustWrite(t, filepath.Join(workspace.Path, "large.txt"), []byte("12345"))
			},
			field: "file_bytes",
		},
		{
			name: "before-state file bytes",
			limits: func() Limits {
				limits := baseLimits
				limits.MaxFileBytes = 8
				return limits
			},
			mutate: func(t *testing.T, workspace Workspace) {
				if err := os.Remove(filepath.Join(workspace.Path, "delete.txt")); err != nil {
					t.Fatal(err)
				}
			},
			field: "file_bytes",
		},
		{
			name: "total changed bytes",
			limits: func() Limits {
				limits := baseLimits
				limits.MaxFileBytes = 64
				limits.MaxTotalChangedBytes = 53
				return limits
			},
			mutate: func(t *testing.T, workspace Workspace) {
				mustWrite(t, filepath.Join(workspace.Path, "delete.txt"), bytes.Repeat([]byte("x"), 27))
			},
			field: "total_changed_bytes",
		},
		{
			name: "captured diff bytes",
			limits: func() Limits {
				limits := baseLimits
				limits.MaxCapturedDiffBytes = 32
				return limits
			},
			mutate: func(t *testing.T, workspace Workspace) {
				mustWrite(t, filepath.Join(workspace.Path, "delete.txt"), []byte("replacement\n"))
			},
			field: "captured_diff_bytes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := createRepository(t)
			manager, err := NewWithLimits(test.limits())
			if err != nil {
				t.Fatal(err)
			}
			workspace, err := manager.Create(
				context.Background(),
				repository,
				t.TempDir(),
				"tx:limits:"+strings.ReplaceAll(test.name, " ", "-"),
				"HEAD",
				time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC),
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })
			test.mutate(t, workspace)

			_, err = manager.Inspect(context.Background(), workspace, time.Now())
			assertKernelError(
				t,
				err,
				model.ErrorBudgetExceeded,
				"inspect_git_stage",
				workspace.TransactionID,
				test.field,
			)
		})
	}
}

func TestInspectRejectsOversizedFileBeforeWritingGitObject(t *testing.T) {
	repository := createRepository(t)
	limits := DefaultLimits()
	limits.MaxChangedFiles = 10
	limits.MaxFileBytes = 8
	limits.MaxTotalChangedBytes = 1 << 10
	limits.MaxCapturedDiffBytes = 1 << 20
	manager, err := NewWithLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	workspace, err := manager.Create(
		context.Background(),
		repository,
		t.TempDir(),
		"tx:oversized-preflight",
		"HEAD",
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })

	content := []byte("oversized-preflight-object-that-must-not-be-written\n")
	objectID := runGitWithInput(t, repository, content, "hash-object", "--stdin")
	if gitObjectExists(t, repository, objectID) {
		t.Fatalf("test object %s unexpectedly existed before inspection", objectID)
	}
	mustWrite(t, filepath.Join(workspace.Path, "oversized.txt"), content)

	_, err = manager.Inspect(context.Background(), workspace, now)
	assertKernelError(
		t,
		err,
		model.ErrorBudgetExceeded,
		"inspect_git_stage",
		workspace.TransactionID,
		"file_bytes",
	)
	if gitObjectExists(t, repository, objectID) {
		t.Fatalf("oversized blob %s was written before its limit rejection", objectID)
	}
}

func TestInspectAllowsResourceUsageAtExactLimits(t *testing.T) {
	repository := createRepository(t)
	limits := DefaultLimits()
	limits.MaxChangedFiles = 1
	limits.MaxFileBytes = 4
	limits.MaxTotalChangedBytes = 4
	limits.MaxCapturedDiffBytes = 1 << 20
	manager, err := NewWithLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := manager.Create(
		context.Background(),
		repository,
		t.TempDir(),
		"tx:exact-limits",
		"HEAD",
		time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })
	mustWrite(t, filepath.Join(workspace.Path, "four.txt"), []byte("1234"))

	snapshot, err := manager.Inspect(context.Background(), workspace, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Effects) != 1 {
		t.Fatalf("effects = %d, want 1", len(snapshot.Effects))
	}
}

func TestTrackedTreeScanRejectsOversizedFileFromMetadataBeforeHashing(t *testing.T) {
	repository := createRepository(t)
	limits := DefaultLimits()
	limits.MaxTreeBlobBytes = 128
	manager, err := NewWithLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := manager.Create(
		context.Background(),
		repository,
		t.TempDir(),
		"tx:tracked-scan-limit",
		"HEAD",
		time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })
	if err := os.Truncate(filepath.Join(workspace.Path, "delete.txt"), 1<<30); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err = manager.Inspect(context.Background(), workspace, time.Now())
	assertKernelError(
		t,
		err,
		model.ErrorBudgetExceeded,
		"inspect_git_stage",
		workspace.TransactionID,
		"tree_blob_bytes",
	)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("oversized sparse file was read instead of rejected from fstat metadata: %s", elapsed)
	}
}

func TestRawGitContentDriversNeverExecuteAndMaterializationIsExact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("content-driver regression uses a POSIX shell script")
	}
	repository := createRepository(t)
	mustWrite(
		t,
		filepath.Join(repository, ".gitattributes"),
		[]byte("*.txt filter=gatemole-evil diff=gatemole-evil\n"),
	)
	mustWrite(t, filepath.Join(repository, ".gitignore"), []byte("ignored.secret\n"))
	mustWrite(t, filepath.Join(repository, "payload.txt"), []byte("base raw payload\n"))
	runGit(
		t,
		repository,
		"add", "--", ".gitattributes", ".gitignore", "payload.txt",
	)
	runGit(
		t,
		repository,
		"-c", "user.name=Test",
		"-c", "user.email=test@example.invalid",
		"commit", "-m", "add content driver fixture",
	)

	marker := filepath.Join(t.TempDir(), "driver-invocations")
	driver := filepath.Join(t.TempDir(), "content-driver")
	mustWrite(t, driver, []byte(
		"#!/bin/sh\n"+
			"printf 'invoked\\n' >> \"$GATEMOLE_TEST_FILTER_MARKER\"\n"+
			"if [ \"$#\" -gt 0 ]; then cat \"$1\"; else cat; fi\n",
	))
	if err := os.Chmod(driver, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEMOLE_TEST_FILTER_MARKER", marker)
	runGit(t, repository, "config", "filter.gatemole-evil.clean", driver)
	runGit(t, repository, "config", "filter.gatemole-evil.smudge", driver)
	runGit(t, repository, "config", "diff.gatemole-evil.textconv", driver)

	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	stagingRoot := t.TempDir()
	workspace, err := manager.Create(
		context.Background(),
		repository,
		stagingRoot,
		"tx:raw-content-drivers",
		"HEAD",
		time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree creation executed a configured smudge driver: %v", err)
	}

	approved := []byte("approved raw payload\n")
	mustWrite(t, filepath.Join(workspace.Path, "payload.txt"), approved)
	mustWrite(t, filepath.Join(workspace.Path, "ignored.secret"), []byte("malicious ignored input\n"))
	snapshot, err := manager.Inspect(context.Background(), workspace, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection executed a clean or textconv driver: %v", err)
	}
	if content := runGit(t, repository, "show", snapshot.TreeRevision+":payload.txt"); content != strings.TrimSpace(string(approved)) {
		t.Fatalf("snapshot tree contains filtered payload %q", content)
	}

	materializationRoot := t.TempDir()
	materialized, cleanup, err := manager.MaterializeSnapshot(
		context.Background(),
		snapshot,
		materializationRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot materialization executed a smudge driver: %v", err)
	}
	materializedContent, err := os.ReadFile(filepath.Join(materialized, "payload.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(materializedContent, approved) {
		t.Fatalf("materialized payload = %q, want raw %q", materializedContent, approved)
	}
	if _, err := os.Lstat(filepath.Join(materialized, "ignored.secret")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ignored malicious file reached verifier materialization: %v", err)
	}
	info, err := os.Stat(filepath.Join(materialized, "payload.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("materialized verifier input is writable: mode=%#o", info.Mode().Perm())
	}
	mustWrite(t, filepath.Join(workspace.Path, "payload.txt"), []byte("live mutation\n"))
	materializedContent, err = os.ReadFile(filepath.Join(materialized, "payload.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(materializedContent, approved) {
		t.Fatalf("live worktree mutation changed immutable verifier input: %q", materializedContent)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("materialization cleanup is not idempotent: %v", err)
	}
	if _, err := os.Stat(materialized); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materialized verifier input survived cleanup: %v", err)
	}
}

func TestMaterializeSnapshotEnforcesWholeTreeQuotas(t *testing.T) {
	repository := createRepository(t)
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := manager.Create(
		context.Background(),
		repository,
		t.TempDir(),
		"tx:materialize-quotas",
		"HEAD",
		time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })
	snapshot, err := manager.Inspect(context.Background(), workspace, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		limits func() Limits
		field  string
	}{
		{
			name: "entries",
			limits: func() Limits {
				limits := DefaultLimits()
				limits.MaxTreeEntries = 1
				return limits
			},
			field: "tree_entries",
		},
		{
			name: "aggregate bytes",
			limits: func() Limits {
				limits := DefaultLimits()
				limits.MaxTreeBytes = 1
				return limits
			},
			field: "tree_bytes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restricted, err := NewWithLimits(test.limits())
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			_, _, err = restricted.MaterializeSnapshot(context.Background(), snapshot, root)
			assertKernelError(
				t,
				err,
				model.ErrorBudgetExceeded,
				"inspect_git_stage",
				workspace.TransactionID,
				test.field,
			)
			entries, readErr := os.ReadDir(root)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("failed materialization left quota-consuming files: %#v", entries)
			}
		})
	}
}

func TestExactSnapshotProfileRejectsGitlinks(t *testing.T) {
	repository := createRepository(t)
	base := runGit(t, repository, "rev-parse", "HEAD")
	runGit(
		t,
		repository,
		"update-index", "--add", "--cacheinfo", "160000", base, "vendor/submodule",
	)
	tree := runGit(t, repository, "write-tree")
	commit := runGitWithInput(
		t,
		repository,
		[]byte("gitlink fixture\n"),
		"-c", "user.name=Test",
		"-c", "user.email=test@example.invalid",
		"commit-tree", tree, "-p", base, "-F", "-",
	)
	runGit(t, repository, "update-ref", "refs/heads/main", commit, base)

	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	stagingRoot := t.TempDir()
	_, err = manager.Create(
		context.Background(),
		repository,
		stagingRoot,
		"tx:gitlink-rejected",
		commit,
		time.Now(),
	)
	if err == nil || !strings.Contains(err.Error(), "unsupported Git gitlink") {
		t.Fatalf("exact snapshot profile accepted a gitlink: %v", err)
	}
	entries, readErr := os.ReadDir(stagingRoot)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed gitlink workspace creation leaked staging state: %#v", entries)
	}
}

func TestPrepareCommitUsesTreeBoundStateAndRejectsWorktreeMutation(t *testing.T) {
	repository := createRepository(t)
	runGit(t, repository, "branch", "release/prepare-conflict", "HEAD")
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	workspace, err := manager.Create(
		context.Background(),
		repository,
		t.TempDir(),
		"tx:prepare-conflict",
		"HEAD",
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })
	path := filepath.Join(workspace.Path, "approved.txt")
	mustWrite(t, path, []byte("approved\n"))
	snapshot, err := manager.Inspect(context.Background(), workspace, now)
	if err != nil {
		t.Fatal(err)
	}
	plan := testGitCommitPlan(
		workspace.TransactionID,
		"refs/heads/release/prepare-conflict",
		workspace.BaseRevision,
		snapshot,
		now,
	)
	// The transaction API attaches RunID metadata after Git inspection, so its
	// authoritative effect digest intentionally differs from the raw Git one.
	plan.EffectSetDigest = testDigest("9")
	prepared, err := manager.PrepareCommit(context.Background(), workspace, plan)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.TreeRevision != snapshot.TreeRevision {
		t.Fatalf("prepared tree = %s, want %s", prepared.TreeRevision, snapshot.TreeRevision)
	}
	t.Run("staged state digest", func(t *testing.T) {
		mismatched := plan
		mismatched.StagedStateDigest = testDigest("8")
		_, err := manager.PrepareCommit(context.Background(), workspace, mismatched)
		assertKernelError(
			t,
			err,
			model.ErrorTransactionConflict,
			"prepare_git_commit",
			workspace.TransactionID,
			"",
		)
	})

	mustWrite(t, path, []byte("mutated after approval\n"))
	_, err = manager.PrepareCommit(context.Background(), workspace, plan)
	assertKernelError(
		t,
		err,
		model.ErrorTransactionConflict,
		"prepare_git_commit",
		workspace.TransactionID,
		"",
	)
}

func TestPrepareCommitUsesApprovedTreeWhenWorktreeMutatesBeforeCommitTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test Git command shim requires a POSIX shell")
	}
	repository := createRepository(t)
	runGit(t, repository, "branch", "release/immutable-tree", "HEAD")
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	realGit := manager.gitPath
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	workspace, err := manager.Create(
		context.Background(),
		repository,
		t.TempDir(),
		"tx:immutable-tree",
		"HEAD",
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })

	approvedPath := filepath.Join(workspace.Path, "approved.txt")
	outsidePath := filepath.Join(workspace.Path, "outside.txt")
	mustWrite(t, approvedPath, []byte("approved\n"))
	indexBefore := runGit(t, workspace.Path, "write-tree")
	snapshot, err := manager.Inspect(context.Background(), workspace, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TreeRevision == "" {
		t.Fatal("inspection did not expose its immutable tree revision")
	}
	if indexAfter := runGit(t, workspace.Path, "write-tree"); indexAfter != indexBefore {
		t.Fatalf("inspection mutated the worktree index: %s != %s", indexAfter, indexBefore)
	}
	plan := testGitCommitPlan(
		workspace.TransactionID,
		"refs/heads/release/immutable-tree",
		workspace.BaseRevision,
		snapshot,
		now,
	)

	shim := filepath.Join(t.TempDir(), "git")
	mustWrite(t, shim, []byte(
		"#!/bin/sh\n"+
			"if [ \"$1\" = \"commit-tree\" ]; then\n"+
			"  printf 'unapproved\\n' > \"$GATEMOLE_TEST_APPROVED_PATH\"\n"+
			"  printf 'outside\\n' > \"$GATEMOLE_TEST_OUTSIDE_PATH\"\n"+
			"fi\n"+
			"exec \"$GATEMOLE_TEST_REAL_GIT\" \"$@\"\n",
	))
	if err := os.Chmod(shim, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEMOLE_TEST_REAL_GIT", realGit)
	t.Setenv("GATEMOLE_TEST_APPROVED_PATH", approvedPath)
	t.Setenv("GATEMOLE_TEST_OUTSIDE_PATH", outsidePath)
	manager.gitPath = shim

	prepared, err := manager.PrepareCommit(context.Background(), workspace, plan)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.TreeRevision != snapshot.TreeRevision {
		t.Fatalf(
			"prepared tree = %s, want approved tree %s",
			prepared.TreeRevision,
			snapshot.TreeRevision,
		)
	}
	if commitTree := runGit(t, repository, "rev-parse", prepared.CommitRevision+"^{tree}"); commitTree != snapshot.TreeRevision {
		t.Fatalf("commit tree = %s, want approved tree %s", commitTree, snapshot.TreeRevision)
	}
	if content := runGit(t, repository, "show", prepared.CommitRevision+":approved.txt"); content != "approved" {
		t.Fatalf("prepared commit contains %q, want approved content", content)
	}
	command := exec.Command(realGit, "cat-file", "-e", prepared.CommitRevision+":outside.txt")
	command.Dir = repository
	if err := command.Run(); err == nil {
		t.Fatal("prepared commit included a file created after immutable inspection")
	}
	if content, err := os.ReadFile(approvedPath); err != nil || string(content) != "unapproved\n" {
		t.Fatalf("commit shim did not mutate approved path: content=%q err=%v", content, err)
	}
	if content, err := os.ReadFile(outsidePath); err != nil || string(content) != "outside\n" {
		t.Fatalf("commit shim did not create outside path: content=%q err=%v", content, err)
	}
}

func TestPreparedCommitPublishesWithCASAndReconcilesRetry(t *testing.T) {
	repository := createRepository(t)
	base := runGit(t, repository, "rev-parse", "HEAD")
	runGit(t, repository, "branch", "release/tx-test", base)
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	workspace, err := manager.Create(
		context.Background(), repository, t.TempDir(), "tx:release", base, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })
	mustWrite(
		t,
		filepath.Join(workspace.Path, "internal", "auth", "middleware.go"),
		[]byte("package auth\n\nfunc Allowed() bool { return true }\n"),
	)
	snapshot, err := manager.Inspect(context.Background(), workspace, now)
	if err != nil {
		t.Fatal(err)
	}
	plan := model.CommitPlan{
		Version: model.CommitPlanVersion, ID: "commit-plan:release",
		TransactionID: "tx:release", IntentDigest: testDigest("a"),
		EffectSetDigest: snapshot.EffectSetDigest, StagedStateDigest: snapshot.StagedStateDigest,
		PolicyDigest: testDigest("d"), Connector: "git",
		Target: model.ResourceSelector{
			Kind: "git_ref", Pattern: "refs/heads/release/tx-test",
		},
		ExpectedResourceVersion: base,
		Digest:                  testDigest("e"),
		CreatedAt:               now,
	}
	first, err := manager.PrepareCommit(context.Background(), workspace, plan)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.PrepareCommit(context.Background(), workspace, plan)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("prepared commit is not deterministic: %#v != %#v", first, second)
	}
	published, err := manager.PublishCommit(context.Background(), workspace, first)
	if err != nil {
		t.Fatal(err)
	}
	if published.Status != PublishApplied ||
		published.CurrentRevision != first.CommitRevision {
		t.Fatalf("unexpected publish result: %#v", published)
	}
	reconciled, err := manager.PublishCommit(context.Background(), workspace, first)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Status != PublishReconciled {
		t.Fatalf("retry was not reconciled: %#v", reconciled)
	}
	if current := runGit(t, repository, "rev-parse", "refs/heads/main"); current != base {
		t.Fatalf("source branch moved: %s != %s", current, base)
	}
	if current := runGit(t, repository, "rev-parse", "refs/heads/release/tx-test"); current != first.CommitRevision {
		t.Fatalf("release branch=%s, want %s", current, first.CommitRevision)
	}
}

func TestPublishRejectsCheckedOutTarget(t *testing.T) {
	repository := createRepository(t)
	base := runGit(t, repository, "rev-parse", "HEAD")
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := manager.Create(
		context.Background(), repository, t.TempDir(), "tx:checked-out", base, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })
	result, err := manager.PublishCommit(context.Background(), workspace, PreparedCommit{
		TargetRef: "refs/heads/main", ExpectedRevision: base,
		CommitRevision: base,
	})
	if err == nil || result.Status != PublishConflict {
		t.Fatalf("checked-out target was accepted: result=%#v err=%v", result, err)
	}
}

func TestPublishRejectsSymbolicReleaseTargetWithoutMovingReferent(t *testing.T) {
	repository := createRepository(t)
	base := runGit(t, repository, "rev-parse", "HEAD")
	runGit(
		t,
		repository,
		"symbolic-ref", "refs/heads/release/alias", "refs/heads/main",
	)
	tree := runGit(t, repository, "rev-parse", base+"^{tree}")
	candidate := runGitWithInput(
		t,
		repository,
		[]byte("candidate\n"),
		"-c", "user.name=Test",
		"-c", "user.email=test@example.invalid",
		"commit-tree", tree, "-p", base, "-F", "-",
	)
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := manager.Create(
		context.Background(),
		repository,
		t.TempDir(),
		"tx:symbolic-release",
		base,
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })

	result, err := manager.PublishCommit(context.Background(), workspace, PreparedCommit{
		TargetRef:        "refs/heads/release/alias",
		ExpectedRevision: base,
		CommitRevision:   candidate,
	})
	if err == nil || result.Status != PublishUnknown {
		t.Fatalf("symbolic release target was accepted: result=%#v err=%v", result, err)
	}
	if current := runGit(t, repository, "rev-parse", "refs/heads/main"); current != base {
		t.Fatalf("symbolic release target moved checked-out main: %s != %s", current, base)
	}
	if target := runGit(t, repository, "symbolic-ref", "refs/heads/release/alias"); target != "refs/heads/main" {
		t.Fatalf("symbolic release alias was mutated: %q", target)
	}
}

func TestPublishNoDerefCASCannotMoveReferentDuringSymbolicRefRace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("race regression uses a POSIX Git command shim")
	}
	repository := createRepository(t)
	base := runGit(t, repository, "rev-parse", "HEAD")
	runGit(t, repository, "branch", "release/race", base)
	tree := runGit(t, repository, "rev-parse", base+"^{tree}")
	candidate := runGitWithInput(
		t,
		repository,
		[]byte("candidate\n"),
		"-c", "user.name=Test",
		"-c", "user.email=test@example.invalid",
		"commit-tree", tree, "-p", base, "-F", "-",
	)
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := manager.Create(
		context.Background(),
		repository,
		t.TempDir(),
		"tx:symbolic-release-race",
		base,
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	realGit := manager.gitPath
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })
	marker := filepath.Join(t.TempDir(), "update-ref-intercepted")
	shim := filepath.Join(t.TempDir(), "git")
	mustWrite(t, shim, []byte(
		"#!/bin/sh\n"+
			"if [ \"$1\" = \"update-ref\" ]; then\n"+
			"  if [ \"$2\" != \"--no-deref\" ]; then\n"+
			"    printf 'missing --no-deref\\n' >&2\n"+
			"    exit 97\n"+
			"  fi\n"+
			"  \"$GATEMOLE_TEST_REAL_GIT\" symbolic-ref \"$GATEMOLE_TEST_TARGET_REF\" refs/heads/main || exit $?\n"+
			"  : > \"$GATEMOLE_TEST_UPDATE_MARKER\"\n"+
			"fi\n"+
			"exec \"$GATEMOLE_TEST_REAL_GIT\" \"$@\"\n",
	))
	if err := os.Chmod(shim, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEMOLE_TEST_REAL_GIT", realGit)
	t.Setenv("GATEMOLE_TEST_TARGET_REF", "refs/heads/release/race")
	t.Setenv("GATEMOLE_TEST_UPDATE_MARKER", marker)
	manager.gitPath = shim

	result, err := manager.PublishCommit(context.Background(), workspace, PreparedCommit{
		TargetRef:        "refs/heads/release/race",
		ExpectedRevision: base,
		CommitRevision:   candidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != PublishApplied || result.CurrentRevision != candidate {
		t.Fatalf("unexpected race-safe publish result: %#v", result)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("update-ref race shim did not run: %v", err)
	}
	if current := runGit(t, repository, "rev-parse", "refs/heads/main"); current != base {
		t.Fatalf("--no-deref CAS moved symbolic referent main: %s != %s", current, base)
	}
	if current := runGit(t, repository, "rev-parse", "refs/heads/release/race"); current != candidate {
		t.Fatalf("no-deref CAS did not update exact target ref: %s != %s", current, candidate)
	}
}

func TestPublishCASRejectsConcurrentBranchMove(t *testing.T) {
	repository := createRepository(t)
	base := runGit(t, repository, "rev-parse", "HEAD")
	runGit(t, repository, "branch", "release/conflict", base)
	manager, err := New()
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := manager.Create(
		context.Background(), repository, t.TempDir(), "tx:conflict", base,
		time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Discard(context.Background(), workspace) })
	mustWrite(t, filepath.Join(workspace.Path, "new.txt"), []byte("transaction\n"))
	snapshot, err := manager.Inspect(
		context.Background(),
		workspace,
		time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	plan := model.CommitPlan{
		Connector: "git",
		Target: model.ResourceSelector{
			Kind: "git_ref", Pattern: "refs/heads/release/conflict",
		},
		ExpectedResourceVersion: base,
		TransactionID:           "tx:conflict",
		IntentDigest:            testDigest("a"),
		EffectSetDigest:         snapshot.EffectSetDigest,
		StagedStateDigest:       snapshot.StagedStateDigest,
		PolicyDigest:            testDigest("c"),
		Digest:                  testDigest("d"),
		CreatedAt:               time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC),
	}
	prepared, err := manager.PrepareCommit(context.Background(), workspace, plan)
	if err != nil {
		t.Fatal(err)
	}
	tree := runGit(t, repository, "rev-parse", base+"^{tree}")
	concurrent := runGitWithInput(
		t, repository, []byte("concurrent\n"),
		"-c", "user.name=Concurrent",
		"-c", "user.email=concurrent@example.invalid",
		"commit-tree", tree, "-p", base, "-F", "-",
	)
	runGit(t, repository, "update-ref", "refs/heads/release/conflict", concurrent, base)
	result, err := manager.PublishCommit(context.Background(), workspace, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != PublishConflict || result.CurrentRevision != concurrent {
		t.Fatalf("concurrent move was not reported as conflict: %#v", result)
	}
	if current := runGit(t, repository, "rev-parse", "refs/heads/release/conflict"); current != concurrent {
		t.Fatalf("CAS overwrote concurrent ref: %s != %s", current, concurrent)
	}
}

func createRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	runGit(t, repository, "init", "--initial-branch=main")
	mustWrite(t, filepath.Join(repository, "internal", "auth", "middleware.go"), []byte("package auth\n\nfunc Allowed() bool { return false }\n"))
	mustWrite(t, filepath.Join(repository, "delete.txt"), []byte("tracked deletion candidate\n"))
	runGit(t, repository, "add", "--", "internal/auth/middleware.go", "delete.txt")
	tree := runGit(t, repository, "write-tree")
	commit := runGitWithInput(t, repository, []byte("base\n"),
		"-c", "user.name=Vouch Test", "-c", "user.email=gatemole-test@example.invalid",
		"commit-tree", tree, "-F", "-")
	runGit(t, repository, "update-ref", "refs/heads/main", commit)
	return repository
}

func mustWrite(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	return runGitWithInput(t, directory, nil, arguments...)
}

func runGitWithInput(t *testing.T, directory string, input []byte, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func gitObjectExists(t *testing.T, directory, objectID string) bool {
	t.Helper()
	command := exec.Command("git", "cat-file", "-e", objectID)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false
	}
	t.Fatalf("inspect Git object %s: %v: %s", objectID, err, output)
	return false
}

func assertCode(t *testing.T, err error, want model.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s", want)
	}
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) || kernelErr.Code != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}

func assertKernelError(
	t *testing.T,
	err error,
	code model.ErrorCode,
	operation string,
	resource string,
	field string,
) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s", code)
	}
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) {
		t.Fatalf("error = %v, want *model.KernelError", err)
	}
	if kernelErr.Code != code ||
		kernelErr.Operation != operation ||
		kernelErr.Resource != resource ||
		kernelErr.Field != field {
		t.Fatalf(
			"kernel error = %#v, want code=%s operation=%q resource=%q field=%q",
			kernelErr,
			code,
			operation,
			resource,
			field,
		)
	}
}

func decodeArguments(t *testing.T, effect model.Effect) FileEffectArguments {
	t.Helper()
	var arguments FileEffectArguments
	if err := json.Unmarshal(effect.Arguments, &arguments); err != nil {
		t.Fatal(err)
	}
	return arguments
}

func testGitCommitPlan(
	transactionID string,
	target string,
	baseRevision string,
	snapshot Snapshot,
	now time.Time,
) model.CommitPlan {
	return model.CommitPlan{
		Version:           model.CommitPlanVersion,
		ID:                "commit-plan:" + strings.TrimPrefix(transactionID, "tx:"),
		TransactionID:     transactionID,
		IntentDigest:      testDigest("1"),
		EffectSetDigest:   snapshot.EffectSetDigest,
		StagedStateDigest: snapshot.StagedStateDigest,
		PolicyDigest:      testDigest("2"),
		Connector:         "git",
		Target: model.ResourceSelector{
			Kind:    "git_ref",
			Pattern: target,
		},
		ExpectedResourceVersion: baseRevision,
		Digest:                  testDigest("3"),
		CreatedAt:               now,
	}
}

func testDigest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}
