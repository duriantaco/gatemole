package driver

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
)

func TestFilesystemWritesAndReadsInsideWorkspace(t *testing.T) {
	repo, run, grant, filesystem := filesystemFixture(t)
	content := []byte("governed output\n")
	write := actionRequest(t, run.ID, FilesystemWrite, "workspace/hello.txt", content)
	result, err := filesystem.Execute(run, grant, write, content)
	if err != nil {
		t.Fatal(err)
	}
	if result.Digest != DigestBytes(content) || result.Bytes != int64(len(content)) {
		t.Fatalf("unexpected write result: %#v", result)
	}
	stored, err := os.ReadFile(filepath.Join(repo, "workspace", "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(content) {
		t.Fatalf("stored content=%q", stored)
	}

	read := actionRequest(t, run.ID, FilesystemRead, "workspace/hello.txt", nil)
	result, err = filesystem.Execute(run, grant, read, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != string(content) || result.Digest != DigestBytes(content) {
		t.Fatalf("unexpected read result: %#v", result)
	}
}

func TestFilesystemDeniesLexicalAndSymlinkEscapes(t *testing.T) {
	repo, run, grant, filesystem := filesystemFixture(t)
	outside := filepath.Join(repo, "outside")
	if err := os.Mkdir(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(repo, "workspace", "link")); err != nil {
		t.Fatal(err)
	}

	unsafeArguments, _ := json.Marshal(FilesystemWriteArguments{
		Path:          "workspace/../outside/escaped.txt",
		ContentDigest: DigestBytes([]byte("escape")),
	})
	unsafe := baseAction(run.ID, FilesystemWrite, "workspace/../outside/escaped.txt", unsafeArguments)
	_, err := filesystem.Execute(run, grant, unsafe, []byte("escape"))
	assertKernelCode(t, err, model.ErrorSchemaInvalid)

	symlink := actionRequest(t, run.ID, FilesystemWrite, "workspace/link/escaped.txt", []byte("escape"))
	_, err = filesystem.Execute(run, grant, symlink, []byte("escape"))
	if err == nil {
		t.Fatal("symlink escape was allowed")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "escaped.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("outside file exists or could not be checked: %v", statErr)
	}
}

func TestFilesystemPermanentlyDeniesGitAndVouchControlState(t *testing.T) {
	_, run, grant, filesystem := filesystemFixture(t)
	content := []byte("forged control state")
	for _, logicalPath := range []string{
		"workspace/.git/config",
		"workspace/.GiT/refs/heads/main",
		"workspace/.git./config",
		"workspace/.git::$DATA/config",
		"workspace/.vouch/kernel.db",
		"workspace/.VOUCH./vouchd.sock",
	} {
		request := actionRequest(
			t,
			run.ID,
			FilesystemWrite,
			logicalPath,
			content,
		)
		_, err := filesystem.Execute(run, grant, request, content)
		assertKernelCode(t, err, model.ErrorCapabilityDenied)
	}
}

func TestFilesystemDeniesSymlinkAliasesToControlState(t *testing.T) {
	repo, run, grant, filesystem := filesystemFixture(t)
	gitDirectory := filepath.Join(repo, "workspace", ".git")
	if err := os.Mkdir(gitDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(gitDirectory, "config"),
		[]byte("protected\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		".git",
		filepath.Join(repo, "workspace", "metadata"),
	); err != nil {
		t.Fatal(err)
	}

	request := actionRequest(
		t,
		run.ID,
		FilesystemRead,
		"workspace/metadata/config",
		nil,
	)
	_, err := filesystem.Execute(run, grant, request, nil)
	assertKernelCode(t, err, model.ErrorCapabilityDenied)

	rootGitDirectory := filepath.Join(repo, ".git")
	if err := os.Mkdir(rootGitDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(rootGitDirectory, "config"),
		[]byte("protected\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git", filepath.Join(repo, "workspace-alias")); err != nil {
		t.Fatal(err)
	}
	run.Workspace = "workspace-alias"
	grant.Resource.Pattern = "workspace-alias/**"
	grant.Conditions.WorkspaceRoot = "workspace-alias"
	request = actionRequest(
		t,
		run.ID,
		FilesystemRead,
		"workspace-alias/config",
		nil,
	)
	_, err = filesystem.Execute(run, grant, request, nil)
	assertKernelCode(t, err, model.ErrorCapabilityDenied)
}

func TestFilesystemEnforcesOutputLimitAndArgumentDigest(t *testing.T) {
	_, run, grant, filesystem := filesystemFixture(t)
	limit := int64(3)
	grant.Conditions.MaxOutputBytes = &limit
	request := actionRequest(t, run.ID, FilesystemWrite, "workspace/large.txt", []byte("four"))
	_, err := filesystem.Execute(run, grant, request, []byte("four"))
	assertKernelCode(t, err, model.ErrorCapabilityDenied)

	request = actionRequest(t, run.ID, FilesystemWrite, "workspace/tampered.txt", []byte("safe"))
	request.ArgumentsDigest = DigestBytes([]byte("different arguments"))
	_, err = filesystem.Execute(run, grant, request, []byte("safe"))
	assertKernelCode(t, err, model.ErrorSchemaInvalid)
}

func filesystemFixture(t *testing.T) (string, model.AgentRun, model.CapabilityGrant, *Filesystem) {
	t.Helper()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, "workspace"), 0o750); err != nil {
		t.Fatal(err)
	}
	filesystem, err := NewFilesystem(repo)
	if err != nil {
		t.Fatal(err)
	}
	run := model.AgentRun{
		ID:        "run:filesystem-test",
		Workspace: "workspace",
	}
	limit := int64(1 << 20)
	grant := model.CapabilityGrant{
		ID:           "cap:filesystem-test",
		SubjectRunID: run.ID,
		Resource: model.ResourceSelector{
			Kind:    "filesystem",
			Pattern: "workspace/**",
		},
		Operations: []string{FilesystemRead, FilesystemWrite},
		Conditions: model.CapabilityConditions{
			WorkspaceRoot:  "workspace",
			MaxOutputBytes: &limit,
		},
	}
	return repo, run, grant, filesystem
}

func actionRequest(t *testing.T, runID, operation, logicalPath string, content []byte) model.ActionRequest {
	t.Helper()
	var raw []byte
	var err error
	if operation == FilesystemWrite {
		raw, err = json.Marshal(FilesystemWriteArguments{Path: logicalPath, ContentDigest: DigestBytes(content)})
	} else {
		raw, err = json.Marshal(FilesystemReadArguments{Path: logicalPath})
	}
	if err != nil {
		t.Fatal(err)
	}
	return baseAction(runID, operation, logicalPath, raw)
}

func baseAction(runID, operation, logicalPath string, raw json.RawMessage) model.ActionRequest {
	_, _, normalized, _ := NormalizeArguments(operation, raw)
	return model.ActionRequest{
		Version:         model.ActionRequestVersion,
		ID:              "action:filesystem-test",
		RunID:           runID,
		Operation:       operation,
		Resource:        model.ResourceSelector{Kind: "filesystem", Pattern: logicalPath},
		Arguments:       raw,
		ArgumentsDigest: DigestBytes(normalized),
		IdempotencyKey:  "idem:filesystem-test",
		Intent:          "exercise the mediated filesystem driver",
		RequestedAt:     time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		Attempt:         1,
	}
}

func assertKernelCode(t *testing.T, err error, want model.ErrorCode) {
	t.Helper()
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) || kernelErr.Code != want {
		t.Fatalf("error=%v, want %s", err, want)
	}
}
