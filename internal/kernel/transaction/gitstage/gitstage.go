// Package gitstage creates isolated Git worktrees and turns their exact diff
// into normalized, task-scoped transaction effects.
package gitstage

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/transaction"
)

const (
	DefaultMaxChangedFiles            = 10_000
	DefaultMaxFileBytes         int64 = 64 << 20
	DefaultMaxTotalChangedBytes int64 = 512 << 20
	DefaultMaxCapturedDiffBytes int64 = 128 << 20
	DefaultMaxTreeEntries             = 100_000
	DefaultMaxTreeBlobBytes     int64 = 256 << 20
	DefaultMaxTreeBytes         int64 = 1 << 30

	minimumChangeListCaptureBytes int64 = 1 << 20
	maximumChangeListCaptureBytes int64 = 64 << 20
	estimatedBytesPerChangedPath  int64 = 4 << 10
	maximumGitErrorBytes          int64 = 64 << 10
)

// Limits bounds the resources consumed while turning a Git worktree into a
// transaction snapshot. MaxTotalChangedBytes counts both before-state and
// after-state file payloads inspected during a snapshot.
type Limits struct {
	MaxChangedFiles      int   `json:"max_changed_files"`
	MaxFileBytes         int64 `json:"max_file_bytes"`
	MaxTotalChangedBytes int64 `json:"max_total_changed_bytes"`
	MaxCapturedDiffBytes int64 `json:"max_captured_diff_bytes"`
	MaxTreeEntries       int   `json:"max_tree_entries"`
	MaxTreeBlobBytes     int64 `json:"max_tree_blob_bytes"`
	MaxTreeBytes         int64 `json:"max_tree_bytes"`
}

type Manager struct {
	gitPath string
	limits  Limits
}

type Workspace struct {
	TransactionID  string    `json:"transaction_id"`
	RepositoryRoot string    `json:"repository_root"`
	GitCommonDir   string    `json:"git_common_dir"`
	Path           string    `json:"path"`
	BaseRevision   string    `json:"base_revision"`
	CreatedAt      time.Time `json:"created_at"`
}

type Snapshot struct {
	Workspace         Workspace      `json:"workspace"`
	TreeRevision      string         `json:"tree_revision"`
	Effects           []model.Effect `json:"effects"`
	EffectSetDigest   string         `json:"effect_set_digest"`
	StagedStateDigest string         `json:"staged_state_digest"`
	PatchDigest       string         `json:"patch_digest"`
	InspectedAt       time.Time      `json:"inspected_at"`
}

const (
	DiffMetadataVersion = "gatemole.git_transaction_diff.v1"
	DiffMetadataHeader  = "Gatemole-Transaction-Diff"
)

// DiffMetadata binds a rendered patch to the exact transaction projection and
// immutable Git tree from which the daemon produced it.
type DiffMetadata struct {
	Version           string `json:"version"`
	Namespace         string `json:"namespace"`
	TransactionID     string `json:"transaction_id"`
	Attempt           int64  `json:"attempt"`
	EventSequence     int64  `json:"event_sequence"`
	StagedStateDigest string `json:"staged_state_digest"`
	EffectSetDigest   string `json:"effect_set_digest"`
	PatchDigest       string `json:"patch_digest"`
	BaseRevision      string `json:"base_revision"`
	TreeRevision      string `json:"tree_revision"`
}

type PreparedCommit struct {
	TargetRef        string `json:"target_ref"`
	ExpectedRevision string `json:"expected_revision"`
	TreeRevision     string `json:"tree_revision"`
	CommitRevision   string `json:"commit_revision"`
	MessageDigest    string `json:"message_digest"`
}

type PublishStatus string

const (
	PublishApplied    PublishStatus = "applied"
	PublishReconciled PublishStatus = "reconciled"
	PublishConflict   PublishStatus = "conflict"
	PublishUnknown    PublishStatus = "unknown"
)

type PublishResult struct {
	Status          PublishStatus `json:"status"`
	CurrentRevision string        `json:"current_revision,omitempty"`
}

type FileEffectArguments struct {
	Path         string `json:"path"`
	Change       string `json:"change"`
	BeforeDigest string `json:"before_digest,omitempty"`
	AfterDigest  string `json:"after_digest,omitempty"`
}

func (workspace Workspace) Binding() model.StageBinding {
	return model.StageBinding{
		ID:           stageBindingID(workspace.TransactionID),
		Kind:         "git_worktree",
		Resource:     model.ResourceSelector{Kind: "git_repository", Pattern: workspace.RepositoryRoot},
		Location:     workspace.Path,
		BaseRevision: workspace.BaseRevision,
		CreatedAt:    workspace.CreatedAt,
	}
}

func (manager *Manager) FromBinding(ctx context.Context, transactionID string, binding model.StageBinding) (Workspace, error) {
	if binding.Kind != "git_worktree" || binding.Resource.Kind != "git_repository" {
		return Workspace{}, errors.New("stage binding is not a Git worktree")
	}
	commonDir, err := manager.gitCommonDir(ctx, binding.Resource.Pattern)
	if err != nil {
		return Workspace{}, err
	}
	workspace := Workspace{
		TransactionID:  transactionID,
		RepositoryRoot: binding.Resource.Pattern,
		GitCommonDir:   commonDir,
		Path:           binding.Location,
		BaseRevision:   binding.BaseRevision,
		CreatedAt:      binding.CreatedAt,
	}
	if err := manager.validateWorkspace(ctx, workspace); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

type change struct {
	path       string
	operation  string
	beforeSize int64
}

// DefaultLimits returns production-safe defaults while leaving enough room for
// large source changes in normal repositories.
func DefaultLimits() Limits {
	return Limits{
		MaxChangedFiles:      DefaultMaxChangedFiles,
		MaxFileBytes:         DefaultMaxFileBytes,
		MaxTotalChangedBytes: DefaultMaxTotalChangedBytes,
		MaxCapturedDiffBytes: DefaultMaxCapturedDiffBytes,
		MaxTreeEntries:       DefaultMaxTreeEntries,
		MaxTreeBlobBytes:     DefaultMaxTreeBlobBytes,
		MaxTreeBytes:         DefaultMaxTreeBytes,
	}
}

func New() (*Manager, error) {
	return NewWithLimits(DefaultLimits())
}

// NewWithLimits creates a manager with explicit snapshot resource limits.
func NewWithLimits(limits Limits) (*Manager, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("find git: %w", err)
	}
	return &Manager{gitPath: gitPath, limits: limits}, nil
}

func (limits Limits) validate() error {
	fields := []struct {
		name  string
		valid bool
	}{
		{name: "max_changed_files", valid: limits.MaxChangedFiles > 0},
		{name: "max_file_bytes", valid: limits.MaxFileBytes > 0},
		{name: "max_total_changed_bytes", valid: limits.MaxTotalChangedBytes > 0},
		{name: "max_captured_diff_bytes", valid: limits.MaxCapturedDiffBytes > 0},
		{name: "max_tree_entries", valid: limits.MaxTreeEntries > 0},
		{name: "max_tree_blob_bytes", valid: limits.MaxTreeBlobBytes > 0},
		{name: "max_tree_bytes", valid: limits.MaxTreeBytes > 0},
	}
	for _, field := range fields {
		if !field.valid {
			return &model.KernelError{
				Code:      model.ErrorSchemaInvalid,
				Operation: "configure_git_stage",
				Field:     field.name,
				Message:   "resource limit must be greater than zero",
			}
		}
	}
	return nil
}

type inspectionBudget struct {
	limits        Limits
	transactionID string
	changedBytes  int64
}

type treeScanBudget struct {
	limits        Limits
	transactionID string
	scannedBytes  int64
}

func (budget *treeScanBudget) consumeFile(path string, size int64) error {
	if size < 0 {
		return fmt.Errorf("tree path %q reported a negative size", path)
	}
	if size > budget.limits.MaxTreeBlobBytes {
		return resourceLimitError(
			budget.transactionID,
			"tree_blob_bytes",
			fmt.Sprintf("tracked tree payload for %q exceeds the configured per-blob byte limit", path),
		)
	}
	if size > budget.limits.MaxTreeBytes-budget.scannedBytes {
		return resourceLimitError(
			budget.transactionID,
			"tree_bytes",
			"tracked tree payloads exceed the configured aggregate byte limit",
		)
	}
	budget.scannedBytes += size
	return nil
}

func (budget *inspectionBudget) consumeFile(path, state string, size int64) error {
	if size < 0 {
		return fmt.Errorf("%s reported a negative size for %q", state, path)
	}
	if size > budget.limits.MaxFileBytes {
		return resourceLimitError(
			budget.transactionID,
			"file_bytes",
			fmt.Sprintf("%s payload for %q exceeds the configured per-file byte limit", state, path),
		)
	}
	if size > budget.limits.MaxTotalChangedBytes-budget.changedBytes {
		return resourceLimitError(
			budget.transactionID,
			"total_changed_bytes",
			"changed file payloads exceed the configured aggregate byte limit",
		)
	}
	budget.changedBytes += size
	return nil
}

func resourceLimitError(transactionID, field, message string) *model.KernelError {
	return &model.KernelError{
		Code:      model.ErrorBudgetExceeded,
		Operation: "inspect_git_stage",
		Resource:  transactionID,
		Field:     field,
		Message:   message,
	}
}

// Create makes a detached transaction worktree at revision. The caller's
// active worktree and branch are never switched or modified.
func (manager *Manager) Create(
	ctx context.Context,
	repositoryRoot string,
	stagingRoot string,
	transactionID string,
	revision string,
	now time.Time,
) (Workspace, error) {
	if strings.TrimSpace(transactionID) == "" || strings.ContainsRune(transactionID, 0) {
		return Workspace{}, errors.New("transaction ID is required")
	}
	if revision == "" {
		revision = "HEAD"
	}
	repository, err := manager.repositoryRoot(ctx, repositoryRoot)
	if err != nil {
		return Workspace{}, err
	}
	baseRevision, err := manager.outputText(ctx, repository, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve base revision: %w", err)
	}
	stagingAbsolute, err := filepath.Abs(stagingRoot)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve staging root: %w", err)
	}
	if err := os.MkdirAll(stagingAbsolute, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("create staging root: %w", err)
	}
	stagingAbsolute, err = filepath.EvalSymlinks(stagingAbsolute)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve staging root links: %w", err)
	}
	target := filepath.Join(stagingAbsolute, workspaceName(transactionID))
	if pathWithin(repository, target) {
		return Workspace{}, errors.New("transaction staging root must be outside the source repository")
	}
	if _, err := os.Lstat(target); err == nil {
		return Workspace{}, fmt.Errorf("transaction workspace already exists: %s", target)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Workspace{}, fmt.Errorf("inspect transaction workspace: %w", err)
	}
	if _, err := manager.run(
		ctx,
		repository,
		"worktree", "add", "--detach", "--no-checkout", target, baseRevision,
	); err != nil {
		return Workspace{}, fmt.Errorf("create detached transaction worktree: %w", err)
	}
	commonDir, err := manager.gitCommonDir(ctx, repository)
	if err != nil {
		_, cleanupErr := manager.run(ctx, repository, "worktree", "remove", "--force", target)
		return Workspace{}, errors.Join(err, cleanupErr)
	}
	workspace := Workspace{
		TransactionID:  transactionID,
		RepositoryRoot: repository,
		GitCommonDir:   commonDir,
		Path:           target,
		BaseRevision:   baseRevision,
		CreatedAt:      now.UTC(),
	}
	if err := manager.materializeTreeFiles(ctx, workspace, baseRevision, target); err != nil {
		_, cleanupErr := manager.run(ctx, repository, "worktree", "remove", "--force", target)
		return Workspace{}, errors.Join(
			fmt.Errorf("materialize detached transaction worktree without Git filters: %w", err),
			cleanupErr,
		)
	}
	return workspace, nil
}

func (manager *Manager) preflightWorktree(
	ctx context.Context,
	workspace Workspace,
) (changes []change, returnErr error) {
	baseFiles, err := manager.treeFiles(ctx, workspace, workspace.BaseRevision)
	if err != nil {
		return nil, err
	}
	indexDirectory, err := privateTemporaryDirectory(
		workspace,
		".gatemole-gitstage-preflight-index-",
	)
	if err != nil {
		return nil, fmt.Errorf("create private Git preflight index directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(indexDirectory); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove private Git preflight index directory: %w", err))
		}
	}()
	indexEnvironment := []string{"GIT_INDEX_FILE=" + filepath.Join(indexDirectory, "index")}
	if _, err := manager.runInput(
		ctx,
		workspace.Path,
		"",
		indexEnvironment,
		"read-tree", workspace.BaseRevision,
	); err != nil {
		return nil, fmt.Errorf("initialize private Git preflight index: %w", err)
	}

	changes, err = manager.rawWorktreeChanges(ctx, workspace, baseFiles, indexEnvironment)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(workspace.Path)
	if err != nil {
		return nil, fmt.Errorf("open transaction worktree for resource preflight: %w", err)
	}
	defer root.Close()

	budget := &inspectionBudget{
		limits:        manager.limits,
		transactionID: workspace.TransactionID,
	}
	for _, candidate := range changes {
		if candidate.beforeSize > 0 {
			if err := budget.consumeFile(candidate.path, "before-state", candidate.beforeSize); err != nil {
				return nil, err
			}
		}
		if candidate.operation == "delete" {
			continue
		}
		size, err := rootedFileSize(root, candidate.path)
		if err != nil {
			return nil, fmt.Errorf("preflight staged path %q: %w", candidate.path, err)
		}
		if err := budget.consumeFile(candidate.path, "after-state", size); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

func rootedFileSize(root *os.Root, path string) (int64, error) {
	native := filepath.FromSlash(path)
	info, err := root.Lstat(native)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(native)
		if err != nil {
			return 0, err
		}
		return int64(len(target)), nil
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("only regular files and symlinks can become Git effects")
	}
	return info.Size(), nil
}

func privateTemporaryDirectory(workspace Workspace, prefix string) (string, error) {
	parent, err := filepath.Abs(filepath.Dir(workspace.Path))
	if err != nil {
		return "", fmt.Errorf("resolve transaction staging directory: %w", err)
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve transaction staging directory links: %w", err)
	}
	if pathWithin(workspace.RepositoryRoot, parent) {
		return "", errors.New("private Git state must remain outside the source repository")
	}
	directory, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return "", err
	}
	if !pathWithin(parent, directory) {
		return "", errors.Join(
			errors.New("private Git state escaped the transaction staging directory"),
			os.RemoveAll(directory),
		)
	}
	return directory, nil
}

func (manager *Manager) materializeTree(
	ctx context.Context,
	workspace Workspace,
	changes []change,
) (treeRevision string, returnErr error) {
	indexDirectory, err := privateTemporaryDirectory(workspace, ".gatemole-gitstage-index-")
	if err != nil {
		return "", fmt.Errorf("create private Git index directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(indexDirectory); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove private Git index directory: %w", err))
		}
	}()

	indexPath := filepath.Join(indexDirectory, "index")
	indexEnvironment := []string{"GIT_INDEX_FILE=" + indexPath}
	if _, err := manager.runInput(
		ctx,
		workspace.Path,
		"",
		indexEnvironment,
		"read-tree", workspace.BaseRevision,
	); err != nil {
		return "", fmt.Errorf("initialize private Git index: %w", err)
	}
	if err := manager.updatePrivateIndex(ctx, workspace, indexEnvironment, changes); err != nil {
		return "", err
	}
	treeRevision, err = manager.runInput(
		ctx,
		workspace.Path,
		"",
		indexEnvironment,
		"write-tree",
	)
	if err != nil {
		return "", fmt.Errorf("write immutable transaction tree: %w", err)
	}
	treeRevision = strings.TrimSpace(treeRevision)
	if !validObjectID(treeRevision) {
		return "", errors.New("Git returned an invalid transaction tree revision")
	}
	return treeRevision, nil
}

func (manager *Manager) updatePrivateIndex(
	ctx context.Context,
	workspace Workspace,
	indexEnvironment []string,
	changes []change,
) error {
	objectFormat, err := manager.outputText(ctx, workspace.Path, "rev-parse", "--show-object-format")
	if err != nil {
		return fmt.Errorf("resolve Git object format: %w", err)
	}
	zeroObjectID := ""
	switch objectFormat {
	case "sha1":
		zeroObjectID = strings.Repeat("0", 40)
	case "sha256":
		zeroObjectID = strings.Repeat("0", 64)
	default:
		return fmt.Errorf("unsupported Git object format %q", objectFormat)
	}

	root, err := os.OpenRoot(workspace.Path)
	if err != nil {
		return fmt.Errorf("open transaction worktree for raw staging: %w", err)
	}
	defer root.Close()

	budget := &inspectionBudget{
		limits:        manager.limits,
		transactionID: workspace.TransactionID,
	}
	for _, candidate := range changes {
		if candidate.beforeSize > 0 {
			if err := budget.consumeFile(candidate.path, "before-state", candidate.beforeSize); err != nil {
				return err
			}
		}
	}

	// Removals are intentionally emitted first so file/directory type changes
	// cannot leave stale index entries that conflict with their replacements.
	var indexInfo bytes.Buffer
	for _, candidate := range changes {
		if candidate.operation != "delete" {
			continue
		}
		fmt.Fprintf(&indexInfo, "0 %s\t%s%c", zeroObjectID, candidate.path, byte(0))
	}
	for _, candidate := range changes {
		if candidate.operation == "delete" {
			continue
		}
		content, mode, err := manager.readRootedContent(root, workspace, candidate.path, budget)
		if err != nil {
			return fmt.Errorf("stage raw worktree path %q: %w", candidate.path, err)
		}
		objectOutput, exceeded, err := manager.runInputBytesLimited(
			ctx,
			workspace.Path,
			content,
			128,
			nil,
			"hash-object", "-w", "--stdin", "--no-filters",
		)
		if exceeded {
			return errors.New("Git returned an oversized object ID while staging raw content")
		}
		if err != nil {
			return fmt.Errorf("write raw Git blob for %q: %w", candidate.path, err)
		}
		objectID := strings.TrimSpace(string(objectOutput))
		if !validObjectID(objectID) {
			return fmt.Errorf("Git returned an invalid object ID for %q", candidate.path)
		}
		expectedObjectID, err := gitBlobObjectID(
			objectFormat,
			int64(len(content)),
			bytes.NewReader(content),
		)
		if err != nil {
			return fmt.Errorf("verify raw Git blob for %q: %w", candidate.path, err)
		}
		if objectID != expectedObjectID {
			return fmt.Errorf("Git hashed filtered or unexpected content for %q", candidate.path)
		}
		fmt.Fprintf(&indexInfo, "%s %s\t%s%c", mode, objectID, candidate.path, byte(0))
	}

	if _, exceeded, err := manager.runInputBytesLimited(
		ctx,
		workspace.Path,
		indexInfo.Bytes(),
		maximumGitErrorBytes,
		indexEnvironment,
		"update-index", "-z", "--index-info",
	); exceeded {
		return errors.New("Git returned oversized output while updating the private index")
	} else if err != nil {
		return fmt.Errorf("update private Git index without filters: %w", err)
	}
	return nil
}

func (manager *Manager) readRootedContent(
	root *os.Root,
	workspace Workspace,
	path string,
	budget *inspectionBudget,
) ([]byte, string, error) {
	native := filepath.FromSlash(path)
	info, err := root.Lstat(native)
	if err != nil {
		return nil, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(native)
		if err != nil {
			return nil, "", err
		}
		content := []byte(target)
		if err := budget.consumeFile(path, "after-state", int64(len(content))); err != nil {
			return nil, "", err
		}
		return content, "120000", nil
	}
	if !info.Mode().IsRegular() {
		return nil, "", errors.New("only regular files and symlinks can become Git effects")
	}
	if info.Size() > manager.limits.MaxFileBytes {
		return nil, "", resourceLimitError(
			workspace.TransactionID,
			"file_bytes",
			fmt.Sprintf("after-state payload for %q exceeds the configured per-file byte limit", path),
		)
	}
	file, err := root.Open(native)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	openInfo, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !openInfo.Mode().IsRegular() {
		return nil, "", errors.New("path changed type while its raw content was staged")
	}
	content, err := io.ReadAll(io.LimitReader(file, manager.limits.MaxFileBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(content)) > manager.limits.MaxFileBytes {
		return nil, "", resourceLimitError(
			workspace.TransactionID,
			"file_bytes",
			fmt.Sprintf("after-state payload for %q exceeds the configured per-file byte limit", path),
		)
	}
	if err := budget.consumeFile(path, "after-state", int64(len(content))); err != nil {
		return nil, "", err
	}
	mode := "100644"
	if openInfo.Mode().Perm()&0o111 != 0 {
		mode = "100755"
	}
	return content, mode, nil
}

// Inspect freezes the current worktree view into deterministic effects. It
// first materializes a private Git index as an immutable tree, then derives
// every effect and digest from that tree rather than the mutable worktree.
func (manager *Manager) Inspect(ctx context.Context, workspace Workspace, now time.Time) (Snapshot, error) {
	if err := manager.validateWorkspace(ctx, workspace); err != nil {
		return Snapshot{}, err
	}
	worktreeChanges, err := manager.preflightWorktree(ctx, workspace)
	if err != nil {
		return Snapshot{}, err
	}
	treeRevision, err := manager.materializeTree(ctx, workspace, worktreeChanges)
	if err != nil {
		return Snapshot{}, err
	}
	changes, err := manager.changes(ctx, workspace, treeRevision)
	if err != nil {
		return Snapshot{}, err
	}

	effects := make([]model.Effect, 0, len(changes))
	budget := &inspectionBudget{
		limits:        manager.limits,
		transactionID: workspace.TransactionID,
	}
	for i, candidate := range changes {
		effect, err := manager.effectForChange(
			ctx,
			workspace,
			treeRevision,
			candidate,
			int64(i+1),
			now.UTC(),
			budget,
		)
		if err != nil {
			return Snapshot{}, err
		}
		effects = append(effects, effect)
	}
	effectSetDigest, err := transaction.ComputeEffectSetDigest(effects)
	if err != nil {
		return Snapshot{}, err
	}
	patch, exceeded, err := manager.runLimited(
		ctx,
		workspace.Path,
		manager.limits.MaxCapturedDiffBytes,
		"diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", "--no-renames",
		workspace.BaseRevision, treeRevision, "--",
	)
	if exceeded {
		return Snapshot{}, resourceLimitError(
			workspace.TransactionID,
			"captured_diff_bytes",
			"Git diff exceeds the configured captured byte limit",
		)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read staged Git diff: %w", err)
	}
	patchDigest := digestBytes(patch)
	state := struct {
		Version      string `json:"version"`
		BaseRevision string `json:"base_revision"`
		TreeRevision string `json:"tree_revision"`
		PatchDigest  string `json:"patch_digest"`
	}{
		Version:      "gatemole.git_stage_state.v1",
		BaseRevision: workspace.BaseRevision,
		TreeRevision: treeRevision,
		PatchDigest:  patchDigest,
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode staged state: %w", err)
	}
	return Snapshot{
		Workspace:         workspace,
		TreeRevision:      treeRevision,
		Effects:           effects,
		EffectSetDigest:   effectSetDigest,
		StagedStateDigest: digestBytes(encoded),
		PatchDigest:       patchDigest,
		InspectedAt:       now.UTC(),
	}, nil
}

// CapturePatch renders the exact base-to-tree patch already bound by a
// snapshot. The tree is immutable in Git's object database; recomputing and
// checking the digest prevents the review surface from drifting from the
// snapshot that policy and approval consumed.
func (manager *Manager) CapturePatch(ctx context.Context, snapshot Snapshot) ([]byte, error) {
	if err := manager.validateWorkspace(ctx, snapshot.Workspace); err != nil {
		return nil, err
	}
	if !validObjectID(snapshot.TreeRevision) || snapshot.PatchDigest == "" {
		return nil, errors.New("Git snapshot is missing an immutable tree or patch digest")
	}
	patch, exceeded, err := manager.runLimited(
		ctx,
		snapshot.Workspace.Path,
		manager.limits.MaxCapturedDiffBytes,
		"diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", "--no-renames",
		snapshot.Workspace.BaseRevision, snapshot.TreeRevision, "--",
	)
	if exceeded {
		return nil, resourceLimitError(
			snapshot.Workspace.TransactionID,
			"captured_diff_bytes",
			"Git diff exceeds the configured captured byte limit",
		)
	}
	if err != nil {
		return nil, fmt.Errorf("read frozen Git diff: %w", err)
	}
	if digestBytes(patch) != snapshot.PatchDigest {
		return nil, &model.KernelError{
			Code:      model.ErrorTransactionConflict,
			Operation: "capture_git_diff",
			Resource:  snapshot.Workspace.TransactionID,
			Message:   "rendered Git diff does not match the frozen patch digest",
		}
	}
	return patch, nil
}

// Verify re-inspects the worktree and fails if anything changed after the
// snapshot that policy, verification, or approval consumed.
func (manager *Manager) Verify(ctx context.Context, snapshot Snapshot, now time.Time) error {
	current, err := manager.Inspect(ctx, snapshot.Workspace, now)
	if err != nil {
		return err
	}
	if current.TreeRevision != snapshot.TreeRevision ||
		current.EffectSetDigest != snapshot.EffectSetDigest ||
		current.StagedStateDigest != snapshot.StagedStateDigest {
		return &model.KernelError{
			Code:      model.ErrorTransactionConflict,
			Operation: "verify_git_stage",
			Resource:  snapshot.Workspace.TransactionID,
			Message:   "transaction worktree changed after its snapshot was frozen",
		}
	}
	return nil
}

// MaterializeSnapshot checks out exactly snapshot.TreeRevision into a new
// read-only directory. The returned cleanup function is idempotent and removes
// only that private materialization.
func (manager *Manager) MaterializeSnapshot(
	ctx context.Context,
	snapshot Snapshot,
	materializationRoot string,
) (string, func() error, error) {
	if err := manager.validateWorkspace(ctx, snapshot.Workspace); err != nil {
		return "", nil, err
	}
	if !validObjectID(snapshot.TreeRevision) {
		return "", nil, errors.New("snapshot has an invalid Git tree revision")
	}
	if _, err := manager.run(
		ctx,
		snapshot.Workspace.Path,
		"cat-file", "-e", snapshot.TreeRevision+"^{tree}",
	); err != nil {
		return "", nil, fmt.Errorf("resolve snapshot Git tree: %w", err)
	}
	if strings.TrimSpace(materializationRoot) == "" {
		return "", nil, errors.New("snapshot materialization root is required")
	}
	root, err := filepath.Abs(materializationRoot)
	if err != nil {
		return "", nil, fmt.Errorf("resolve snapshot materialization root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, fmt.Errorf("create snapshot materialization root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, fmt.Errorf("resolve snapshot materialization root links: %w", err)
	}
	if pathWithin(snapshot.Workspace.RepositoryRoot, root) ||
		pathWithin(snapshot.Workspace.Path, root) {
		return "", nil, errors.New("snapshot materialization root must be outside the source repository and transaction worktree")
	}
	directory, err := os.MkdirTemp(root, "tree-"+snapshot.TreeRevision[:12]+"-")
	if err != nil {
		return "", nil, fmt.Errorf("create snapshot materialization: %w", err)
	}
	cleanup := func() error {
		return removeMaterializedTree(directory)
	}
	fail := func(cause error) (string, func() error, error) {
		return "", nil, errors.Join(cause, cleanup())
	}
	if err := manager.materializeTreeFiles(ctx, snapshot.Workspace, snapshot.TreeRevision, directory); err != nil {
		return fail(err)
	}
	if err := hardenMaterializedTree(directory); err != nil {
		return fail(err)
	}
	return directory, cleanup, nil
}

func (manager *Manager) materializeTreeFiles(
	ctx context.Context,
	workspace Workspace,
	treeRevision string,
	destination string,
) error {
	files, err := manager.treeFiles(ctx, workspace, treeRevision)
	if err != nil {
		return err
	}

	for _, file := range files {
		target := filepath.Join(destination, filepath.FromSlash(file.path))
		if !pathWithin(destination, target) {
			return fmt.Errorf("unsafe materialized Git path %q", file.path)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("create materialized parent for %q: %w", file.path, err)
		}
	}
	// Materialize non-links before links so no subsequent path operation can
	// traverse a symlink sourced from the Git tree.
	for _, file := range files {
		if file.mode == "120000" {
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(file.path))
		if err := manager.writeGitBlob(ctx, workspace, file, target); err != nil {
			return err
		}
	}
	for _, file := range files {
		if file.mode != "120000" {
			continue
		}
		if file.size > manager.limits.MaxFileBytes {
			return resourceLimitError(
				workspace.TransactionID,
				"file_bytes",
				fmt.Sprintf("snapshot symlink payload for %q exceeds the configured per-file byte limit", file.path),
			)
		}
		content, exceeded, err := manager.runLimited(
			ctx,
			workspace.Path,
			file.size,
			"cat-file", "blob", file.objectID,
		)
		if exceeded {
			return fmt.Errorf("Git returned oversized symlink content for %q", file.path)
		}
		if err != nil {
			return fmt.Errorf("read snapshot symlink %q: %w", file.path, err)
		}
		if int64(len(content)) != file.size {
			return fmt.Errorf("Git returned an unexpected snapshot size for %q", file.path)
		}
		target := filepath.Join(destination, filepath.FromSlash(file.path))
		if err := os.Symlink(string(content), target); err != nil {
			return fmt.Errorf("create materialized symlink for %q: %w", file.path, err)
		}
	}
	return nil
}

func (manager *Manager) writeGitBlob(
	ctx context.Context,
	workspace Workspace,
	file gitTreeFile,
	target string,
) error {
	mode := fs.FileMode(0o600)
	if file.mode == "100755" {
		mode = 0o700
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create materialized file %q: %w", file.path, err)
	}
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandCtx, manager.gitPath, "cat-file", "blob", file.objectID)
	command.Dir = workspace.Path
	command.Env = gitEnvironment(nil)
	writer := &boundedWriter{writer: output, limit: file.size, cancel: cancel}
	stderr := boundedBuffer{limit: maximumGitErrorBytes, cancel: cancel}
	command.Stdout = writer
	command.Stderr = &stderr
	runErr := command.Run()
	closeErr := output.Close()
	if writer.exceeded {
		return errors.Join(
			fmt.Errorf("Git returned oversized snapshot content for %q", file.path),
			closeErr,
		)
	}
	if runErr != nil {
		message := strings.TrimSpace(string(stderr.bytes()))
		if message == "" {
			message = runErr.Error()
		} else if stderr.exceeded {
			message += " (truncated)"
		}
		return errors.Join(fmt.Errorf("read snapshot file %q: %s", file.path, message), closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close materialized file %q: %w", file.path, closeErr)
	}
	if writer.written != file.size {
		return fmt.Errorf("Git returned an unexpected snapshot size for %q", file.path)
	}
	return nil
}

func hardenMaterializedTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			if err := os.Chmod(path, 0o555); err != nil {
				return fmt.Errorf("make materialized directory read-only: %w", err)
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported materialized Git object at %q", path)
		}
		mode := fs.FileMode(0o444)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o555
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("make materialized file read-only: %w", err)
		}
		return nil
	})
}

func removeMaterializedTree(root string) error {
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("make materialized tree removable: %w", err)
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove materialized tree: %w", err)
	}
	return nil
}

// PrepareCommit re-inspects the worktree into an immutable tree, verifies that
// exact tree against the plan's tree-bound staged-state digest, and commits only
// that tree. Effect-set authority is validated by the transaction layer, where
// API-attached run metadata is available.
func (manager *Manager) PrepareCommit(
	ctx context.Context,
	workspace Workspace,
	plan model.CommitPlan,
) (PreparedCommit, error) {
	if err := manager.validateWorkspace(ctx, workspace); err != nil {
		return PreparedCommit{}, err
	}
	if plan.TransactionID != workspace.TransactionID ||
		plan.Connector != "git" ||
		plan.Target.Kind != "git_ref" ||
		plan.ExpectedResourceVersion != workspace.BaseRevision {
		return PreparedCommit{}, errors.New("commit plan does not bind to this Git workspace")
	}
	if err := manager.ValidateTargetRef(ctx, workspace.RepositoryRoot, plan.Target.Pattern); err != nil {
		return PreparedCommit{}, err
	}
	snapshot, err := manager.Inspect(ctx, workspace, plan.CreatedAt.UTC())
	if err != nil {
		return PreparedCommit{}, err
	}
	if snapshot.StagedStateDigest != plan.StagedStateDigest {
		return PreparedCommit{}, &model.KernelError{
			Code:      model.ErrorTransactionConflict,
			Operation: "prepare_git_commit",
			Resource:  workspace.TransactionID,
			Message:   "transaction worktree changed after its commit plan was frozen",
		}
	}
	treeRevision := snapshot.TreeRevision
	message := fmt.Sprintf(
		"Gatemole transaction %s\n\nIntent-Digest: %s\nEffect-Set-Digest: %s\nStaged-State-Digest: %s\nPolicy-Digest: %s\nCommit-Plan-Digest: %s\n",
		plan.TransactionID,
		plan.IntentDigest,
		plan.EffectSetDigest,
		plan.StagedStateDigest,
		plan.PolicyDigest,
		plan.Digest,
	)
	commitRevision, err := manager.runInput(
		ctx,
		workspace.Path,
		message,
		[]string{
			"GIT_AUTHOR_NAME=Gatemole Transaction OS",
			"GIT_AUTHOR_EMAIL=gatemole@localhost",
			"GIT_COMMITTER_NAME=Gatemole Transaction OS",
			"GIT_COMMITTER_EMAIL=gatemole@localhost",
			"GIT_AUTHOR_DATE=" + plan.CreatedAt.UTC().Format(time.RFC3339),
			"GIT_COMMITTER_DATE=" + plan.CreatedAt.UTC().Format(time.RFC3339),
		},
		"commit-tree", treeRevision, "-p", workspace.BaseRevision,
	)
	if err != nil {
		return PreparedCommit{}, fmt.Errorf("create transaction commit: %w", err)
	}
	commitRevision = strings.TrimSpace(commitRevision)
	if !validObjectID(commitRevision) || !validObjectID(treeRevision) {
		return PreparedCommit{}, errors.New("Git returned an invalid prepared object revision")
	}
	return PreparedCommit{
		TargetRef:        plan.Target.Pattern,
		ExpectedRevision: workspace.BaseRevision,
		TreeRevision:     treeRevision,
		CommitRevision:   commitRevision,
		MessageDigest:    digestBytes([]byte(message)),
	}, nil
}

// PublishCommit performs an atomic compare-and-swap ref update and reads the
// ref back. Retrying the same prepared commit after a daemon crash is safe.
func (manager *Manager) PublishCommit(
	ctx context.Context,
	workspace Workspace,
	prepared PreparedCommit,
) (PublishResult, error) {
	if err := manager.validateWorkspace(ctx, workspace); err != nil {
		return PublishResult{Status: PublishUnknown}, err
	}
	if err := manager.ValidateTargetRef(ctx, workspace.RepositoryRoot, prepared.TargetRef); err != nil {
		return PublishResult{Status: PublishUnknown}, err
	}
	if prepared.ExpectedRevision != workspace.BaseRevision {
		return PublishResult{Status: PublishUnknown}, errors.New("prepared commit expected revision does not match workspace")
	}
	if err := manager.ensureTargetNotCheckedOut(ctx, workspace.RepositoryRoot, prepared.TargetRef); err != nil {
		return PublishResult{Status: PublishConflict}, err
	}
	current, err := manager.referenceRevision(ctx, workspace.RepositoryRoot, prepared.TargetRef)
	if err != nil {
		return PublishResult{Status: PublishUnknown}, err
	}
	if current == prepared.CommitRevision {
		return PublishResult{Status: PublishReconciled, CurrentRevision: current}, nil
	}
	if current != prepared.ExpectedRevision {
		return PublishResult{Status: PublishConflict, CurrentRevision: current}, nil
	}
	_, updateErr := manager.run(
		ctx, workspace.RepositoryRoot,
		"update-ref", "--no-deref",
		prepared.TargetRef, prepared.CommitRevision, prepared.ExpectedRevision,
	)
	current, readErr := manager.referenceRevision(ctx, workspace.RepositoryRoot, prepared.TargetRef)
	if readErr != nil {
		return PublishResult{Status: PublishUnknown}, errors.Join(updateErr, readErr)
	}
	if current == prepared.CommitRevision {
		status := PublishApplied
		if updateErr != nil {
			status = PublishReconciled
		}
		return PublishResult{Status: status, CurrentRevision: current}, nil
	}
	if current == prepared.ExpectedRevision {
		return PublishResult{Status: PublishConflict, CurrentRevision: current}, updateErr
	}
	return PublishResult{Status: PublishUnknown, CurrentRevision: current}, updateErr
}

func (manager *Manager) ValidateTargetRef(ctx context.Context, repository, target string) error {
	if !strings.HasPrefix(target, "refs/heads/") {
		return errors.New("Git release target must be a full refs/heads/* branch ref")
	}
	if _, err := manager.run(ctx, repository, "check-ref-format", target); err != nil {
		return fmt.Errorf("invalid Git release target %q: %w", target, err)
	}
	if _, err := manager.referenceRevision(ctx, repository, target); err != nil {
		return err
	}
	return nil
}

// Discard removes only the exact registered detached worktree. Callers must
// treat this as an explicit transaction-abort operation.
func (manager *Manager) Discard(ctx context.Context, workspace Workspace) error {
	if err := manager.validateWorkspace(ctx, workspace); err != nil {
		return err
	}
	if _, err := manager.run(ctx, workspace.RepositoryRoot, "worktree", "remove", "--force", workspace.Path); err != nil {
		return fmt.Errorf("discard transaction worktree: %w", err)
	}
	return nil
}

func (manager *Manager) effectForChange(
	ctx context.Context,
	workspace Workspace,
	treeRevision string,
	candidate change,
	sequence int64,
	now time.Time,
	budget *inspectionBudget,
) (model.Effect, error) {
	arguments := FileEffectArguments{Path: candidate.path, Change: candidate.operation}
	var beforeRef *model.ArtifactRef
	if candidate.operation != "add" {
		content, objectID, _, err := manager.treeContent(
			ctx,
			workspace,
			workspace.BaseRevision,
			candidate.path,
			"before-state",
			budget,
		)
		if err != nil {
			return model.Effect{}, err
		}
		arguments.BeforeDigest = digestBytes(content)
		beforeRef = &model.ArtifactRef{
			URI:       "git-object://" + objectID,
			Digest:    arguments.BeforeDigest,
			MediaType: "application/octet-stream",
		}
	}

	var stagedContent []byte
	mode := "deleted"
	if candidate.operation == "delete" {
		var err error
		stagedContent, err = json.Marshal(struct {
			Path    string `json:"path"`
			Deleted bool   `json:"deleted"`
		}{Path: candidate.path, Deleted: true})
		if err != nil {
			return model.Effect{}, fmt.Errorf("encode deletion tombstone: %w", err)
		}
	} else {
		var gitMode string
		var err error
		stagedContent, _, gitMode, err = manager.treeContent(
			ctx,
			workspace,
			treeRevision,
			candidate.path,
			"after-state",
			budget,
		)
		if err != nil {
			return model.Effect{}, err
		}
		mode, err = effectMode(gitMode)
		if err != nil {
			return model.Effect{}, fmt.Errorf("read staged path %q: %w", candidate.path, err)
		}
		arguments.AfterDigest = digestBytes(stagedContent)
	}
	stageDigest := digestBytes(stagedContent)
	argumentsJSON, err := json.Marshal(arguments)
	if err != nil {
		return model.Effect{}, fmt.Errorf("encode file effect: %w", err)
	}
	objects := int64(1)
	bytesChanged := int64(len(stagedContent))
	effectID := effectID(workspace.TransactionID, candidate.path, candidate.operation)
	stageURI := "git-tree://" + treeRevision + "/" + url.PathEscape(candidate.path)
	effect := model.Effect{
		Version:            model.EffectVersion,
		ID:                 effectID,
		TransactionID:      workspace.TransactionID,
		Sequence:           sequence,
		System:             "git",
		Resource:           model.ResourceSelector{Kind: "file", Pattern: candidate.path},
		Operation:          candidate.operation,
		Arguments:          argumentsJSON,
		ArgumentsDigest:    digestBytes(argumentsJSON),
		Dependencies:       []string{},
		EstimatedScope:     model.EffectScope{Bytes: &bytesChanged, Objects: &objects},
		DataClassification: "source-code",
		RecoveryClass:      model.RecoveryStageable,
		Status:             model.EffectStaged,
		IdempotencyKey:     idempotencyKey(workspace.TransactionID, effectID),
		StageRef: &model.ArtifactRef{
			URI: stageURI, Digest: stageDigest, MediaType: mediaType(mode),
		},
		BeforeStateRef: beforeRef,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := effect.Validate(); err != nil {
		return model.Effect{}, fmt.Errorf("validate staged effect for %q: %w", candidate.path, err)
	}
	return effect, nil
}

func (manager *Manager) changes(
	ctx context.Context,
	workspace Workspace,
	treeRevision string,
) ([]change, error) {
	listCaptureLimit := changeListCaptureLimit(manager.limits.MaxChangedFiles)
	trackedOutput, exceeded, err := manager.runLimited(
		ctx,
		workspace.Path,
		listCaptureLimit,
		"diff", "--name-status", "-z", "--no-renames",
		workspace.BaseRevision, treeRevision, "--",
	)
	if exceeded {
		return nil, resourceLimitError(
			workspace.TransactionID,
			"changed_files",
			"Git change listing exceeds the configured changed-file limit",
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list tracked Git changes: %w", err)
	}
	if bytes.Count(trackedOutput, []byte{0})/2 > manager.limits.MaxChangedFiles {
		return nil, resourceLimitError(
			workspace.TransactionID,
			"changed_files",
			"worktree exceeds the configured changed-file count",
		)
	}
	tracked, err := parseNameStatus(trackedOutput)
	if err != nil {
		return nil, err
	}
	if len(tracked) > manager.limits.MaxChangedFiles {
		return nil, resourceLimitError(
			workspace.TransactionID,
			"changed_files",
			"worktree exceeds the configured changed-file count",
		)
	}
	sort.Slice(tracked, func(i, j int) bool {
		return tracked[i].path < tracked[j].path
	})
	return tracked, nil
}

func (manager *Manager) rawWorktreeChanges(
	ctx context.Context,
	workspace Workspace,
	baseFiles []gitTreeFile,
	indexEnvironment []string,
) ([]change, error) {
	objectFormat, err := manager.outputText(ctx, workspace.Path, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, fmt.Errorf("resolve Git object format: %w", err)
	}
	root, err := os.OpenRoot(workspace.Path)
	if err != nil {
		return nil, fmt.Errorf("open transaction worktree for raw comparison: %w", err)
	}
	defer root.Close()
	scanBudget := &treeScanBudget{
		limits:        manager.limits,
		transactionID: workspace.TransactionID,
	}

	byPath := make(map[string]change)
	for _, baseFile := range baseFiles {
		current, exists, err := rootedFileIdentity(
			root,
			baseFile.path,
			objectFormat,
			scanBudget,
		)
		if err != nil {
			return nil, fmt.Errorf("compare tracked path %q without filters: %w", baseFile.path, err)
		}
		if !exists {
			byPath[baseFile.path] = change{
				path:       baseFile.path,
				operation:  "delete",
				beforeSize: baseFile.size,
			}
		} else if current.objectID != baseFile.objectID || current.mode != baseFile.mode {
			byPath[baseFile.path] = change{
				path:       baseFile.path,
				operation:  "modify",
				beforeSize: baseFile.size,
			}
		}
		if len(byPath) > manager.limits.MaxChangedFiles {
			return nil, resourceLimitError(
				workspace.TransactionID,
				"changed_files",
				"worktree exceeds the configured changed-file count",
			)
		}
	}

	listCaptureLimit := changeListCaptureLimit(manager.limits.MaxChangedFiles)
	untrackedOutput, exceeded, err := manager.runLimitedEnvironment(
		ctx,
		workspace.Path,
		listCaptureLimit,
		indexEnvironment,
		"ls-files", "--others", "--exclude-standard", "-z",
	)
	if exceeded {
		return nil, resourceLimitError(
			workspace.TransactionID,
			"changed_files",
			"Git change listing exceeds the configured changed-file limit",
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list untracked Git changes for resource preflight: %w", err)
	}
	if bytes.Count(untrackedOutput, []byte{0}) > manager.limits.MaxChangedFiles-len(byPath) {
		return nil, resourceLimitError(
			workspace.TransactionID,
			"changed_files",
			"worktree exceeds the configured changed-file count",
		)
	}

	for _, path := range splitNUL(untrackedOutput) {
		if err := validateGitPath(path); err != nil {
			return nil, err
		}
		if _, exists := byPath[path]; !exists {
			byPath[path] = change{path: path, operation: "add"}
			if len(byPath) > manager.limits.MaxChangedFiles {
				return nil, resourceLimitError(
					workspace.TransactionID,
					"changed_files",
					"worktree exceeds the configured changed-file count",
				)
			}
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	result := make([]change, len(paths))
	for i, path := range paths {
		result[i] = byPath[path]
	}
	return result, nil
}

type gitTreeFile struct {
	path     string
	objectID string
	mode     string
	size     int64
}

func (manager *Manager) treeFiles(
	ctx context.Context,
	workspace Workspace,
	revision string,
) ([]gitTreeFile, error) {
	output, exceeded, err := manager.runLimited(
		ctx,
		workspace.Path,
		maximumChangeListCaptureBytes,
		"ls-tree", "-r", "-l", "-z", "--full-tree", revision,
	)
	if exceeded {
		return nil, resourceLimitError(
			workspace.TransactionID,
			"tree_entries",
			"Git tree listing exceeds the bounded capture limit",
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list Git tree files: %w", err)
	}
	entries := splitNUL(output)
	files := make([]gitTreeFile, 0, len(entries))
	treeBytes := int64(0)
	for _, entry := range entries {
		if len(files) >= manager.limits.MaxTreeEntries {
			return nil, resourceLimitError(
				workspace.TransactionID,
				"tree_entries",
				"Git tree exceeds the configured entry limit",
			)
		}
		parts := strings.SplitN(entry, "\t", 2)
		if len(parts) != 2 {
			return nil, errors.New("invalid Git tree listing")
		}
		fields := strings.Fields(parts[0])
		if len(fields) != 4 || !validObjectID(fields[2]) {
			return nil, errors.New("invalid Git tree file entry")
		}
		path := parts[1]
		if err := validateGitPath(path); err != nil {
			return nil, err
		}
		size := int64(0)
		switch {
		case fields[1] == "blob":
			if _, err := effectMode(fields[0]); err != nil {
				return nil, fmt.Errorf("unsupported Git object for %q: %w", path, err)
			}
			size, err = strconv.ParseInt(fields[3], 10, 64)
			if err != nil || size < 0 {
				return nil, fmt.Errorf("invalid Git blob size for %q", path)
			}
			if size > manager.limits.MaxTreeBlobBytes {
				return nil, resourceLimitError(
					workspace.TransactionID,
					"tree_blob_bytes",
					fmt.Sprintf("Git tree payload for %q exceeds the configured per-blob byte limit", path),
				)
			}
			if size > manager.limits.MaxTreeBytes-treeBytes {
				return nil, resourceLimitError(
					workspace.TransactionID,
					"tree_bytes",
					"Git tree payloads exceed the configured aggregate byte limit",
				)
			}
			treeBytes += size
		case fields[0] == "160000" && fields[1] == "commit":
			return nil, fmt.Errorf(
				"unsupported Git gitlink %q: exact snapshot materialization requires a fully pinned file tree",
				path,
			)
		default:
			return nil, fmt.Errorf("unsupported Git tree entry for %q", path)
		}
		files = append(files, gitTreeFile{
			path: path, objectID: fields[2], mode: fields[0], size: size,
		})
	}
	return files, nil
}

func rootedFileIdentity(
	root *os.Root,
	path string,
	objectFormat string,
	budget *treeScanBudget,
) (gitTreeFile, bool, error) {
	native := filepath.FromSlash(path)
	info, err := root.Lstat(native)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return gitTreeFile{}, false, nil
	}
	if err != nil {
		return gitTreeFile{}, false, err
	}
	if info.IsDir() {
		return gitTreeFile{}, false, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(native)
		if err != nil {
			return gitTreeFile{}, false, err
		}
		if err := budget.consumeFile(path, int64(len(target))); err != nil {
			return gitTreeFile{}, false, err
		}
		objectID, err := gitBlobObjectID(objectFormat, int64(len(target)), strings.NewReader(target))
		if err != nil {
			return gitTreeFile{}, false, err
		}
		return gitTreeFile{
			path: path, objectID: objectID, mode: "120000", size: int64(len(target)),
		}, true, nil
	}
	if !info.Mode().IsRegular() {
		return gitTreeFile{}, false, errors.New("only regular files and symlinks can become Git effects")
	}
	file, err := root.Open(native)
	if err != nil {
		return gitTreeFile{}, false, err
	}
	defer file.Close()
	openInfo, err := file.Stat()
	if err != nil {
		return gitTreeFile{}, false, err
	}
	if !openInfo.Mode().IsRegular() {
		return gitTreeFile{}, false, errors.New("tracked path changed type while it was inspected")
	}
	if err := budget.consumeFile(path, openInfo.Size()); err != nil {
		return gitTreeFile{}, false, err
	}
	objectID, err := gitBlobObjectID(objectFormat, openInfo.Size(), file)
	if err != nil {
		return gitTreeFile{}, false, err
	}
	mode := "100644"
	if openInfo.Mode().Perm()&0o111 != 0 {
		mode = "100755"
	}
	return gitTreeFile{
		path: path, objectID: objectID, mode: mode, size: openInfo.Size(),
	}, true, nil
}

func gitBlobObjectID(objectFormat string, size int64, content io.Reader) (string, error) {
	var digest hash.Hash
	switch objectFormat {
	case "sha1":
		digest = sha1.New()
	case "sha256":
		digest = sha256.New()
	default:
		return "", fmt.Errorf("unsupported Git object format %q", objectFormat)
	}
	if _, err := fmt.Fprintf(digest, "blob %d%c", size, byte(0)); err != nil {
		return "", err
	}
	written, err := io.CopyN(digest, content, size)
	if err != nil {
		return "", fmt.Errorf("hash Git blob content: copied %d of %d bytes: %w", written, size, err)
	}
	var extra [1]byte
	if count, err := content.Read(extra[:]); count != 0 {
		return "", errors.New("file grew while its Git blob identity was computed")
	} else if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("finish hashing Git blob content: %w", err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (manager *Manager) treeFile(
	ctx context.Context,
	workspace Workspace,
	revision string,
	path string,
	state string,
) (gitTreeFile, error) {
	output, exceeded, err := manager.runLimited(
		ctx,
		workspace.Path,
		minimumChangeListCaptureBytes,
		"ls-tree", "-z", revision, "--", path,
	)
	if exceeded {
		return gitTreeFile{}, resourceLimitError(
			workspace.TransactionID,
			"changed_files",
			"Git tree entry exceeds the bounded change-list capture",
		)
	}
	if err != nil {
		return gitTreeFile{}, fmt.Errorf("resolve %s for %q: %w", state, path, err)
	}
	if len(output) == 0 {
		return gitTreeFile{}, fmt.Errorf("%s is missing for %q", state, path)
	}
	entry := bytes.TrimSuffix(output, []byte{0})
	parts := bytes.SplitN(entry, []byte{'\t'}, 2)
	if len(parts) != 2 || string(parts[1]) != path {
		return gitTreeFile{}, fmt.Errorf("invalid Git tree entry for %q", path)
	}
	fields := strings.Fields(string(parts[0]))
	if len(fields) != 3 || fields[1] != "blob" {
		return gitTreeFile{}, fmt.Errorf("unsupported Git object for %q", path)
	}
	if _, err := effectMode(fields[0]); err != nil {
		return gitTreeFile{}, fmt.Errorf("unsupported Git object for %q: %w", path, err)
	}
	if !validObjectID(fields[2]) {
		return gitTreeFile{}, fmt.Errorf("invalid Git object ID for %q", path)
	}
	sizeOutput, exceeded, err := manager.runLimited(
		ctx,
		workspace.Path,
		64,
		"cat-file", "-s", fields[2],
	)
	if exceeded {
		return gitTreeFile{}, fmt.Errorf("Git returned an invalid %s size for %q", state, path)
	}
	if err != nil {
		return gitTreeFile{}, fmt.Errorf("read %s size for %q: %w", state, path, err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeOutput)), 10, 64)
	if err != nil || size < 0 {
		return gitTreeFile{}, fmt.Errorf("Git returned an invalid %s size for %q", state, path)
	}
	return gitTreeFile{objectID: fields[2], mode: fields[0], size: size}, nil
}

func (manager *Manager) treeContent(
	ctx context.Context,
	workspace Workspace,
	revision string,
	path string,
	state string,
	budget *inspectionBudget,
) ([]byte, string, string, error) {
	file, err := manager.treeFile(ctx, workspace, revision, path, state)
	if err != nil {
		return nil, "", "", err
	}
	if err := budget.consumeFile(path, state, file.size); err != nil {
		return nil, "", "", err
	}
	content, exceeded, err := manager.runLimited(
		ctx,
		workspace.Path,
		manager.limits.MaxFileBytes,
		"cat-file", "blob", file.objectID,
	)
	if exceeded {
		return nil, "", "", resourceLimitError(
			workspace.TransactionID,
			"file_bytes",
			fmt.Sprintf("%s payload for %q exceeds the configured per-file byte limit", state, path),
		)
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("read %s for %q: %w", state, path, err)
	}
	if int64(len(content)) != file.size {
		return nil, "", "", fmt.Errorf("Git returned an unexpected %s size for %q", state, path)
	}
	return content, file.objectID, file.mode, nil
}

func effectMode(gitMode string) (string, error) {
	switch gitMode {
	case "100644", "100755":
		return "regular", nil
	case "120000":
		return "symlink", nil
	default:
		return "", fmt.Errorf("unsupported Git file mode %q", gitMode)
	}
}

func (manager *Manager) validateWorkspace(ctx context.Context, workspace Workspace) error {
	if workspace.TransactionID == "" || workspace.RepositoryRoot == "" || workspace.GitCommonDir == "" || workspace.Path == "" || workspace.BaseRevision == "" {
		return errors.New("incomplete transaction workspace")
	}
	repository, err := manager.repositoryRoot(ctx, workspace.RepositoryRoot)
	if err != nil {
		return err
	}
	if repository != workspace.RepositoryRoot {
		return errors.New("transaction repository root changed")
	}
	commonDir, err := manager.gitCommonDir(ctx, workspace.Path)
	if err != nil {
		return err
	}
	if commonDir != workspace.GitCommonDir {
		return errors.New("transaction worktree is no longer registered to the source repository")
	}
	actualRevision, err := manager.outputText(ctx, workspace.Path, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("inspect transaction worktree revision: %w", err)
	}
	if actualRevision != workspace.BaseRevision {
		return &model.KernelError{
			Code: model.ErrorTransactionConflict, Operation: "validate_git_stage", Resource: workspace.TransactionID,
			Message: "transaction worktree HEAD changed from its frozen base revision",
		}
	}
	return nil
}

func (manager *Manager) gitCommonDir(ctx context.Context, directory string) (string, error) {
	value, err := manager.outputText(ctx, directory, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory: %w", err)
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(directory, value)
	}
	value, err = filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory path: %w", err)
	}
	return filepath.Clean(value), nil
}

func (manager *Manager) repositoryRoot(ctx context.Context, path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	root, err := manager.outputText(ctx, absolute, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve Git repository root: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve reported Git root: %w", err)
	}
	return root, nil
}

func (manager *Manager) outputText(ctx context.Context, directory string, arguments ...string) (string, error) {
	output, err := manager.run(ctx, directory, arguments...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func (manager *Manager) referenceRevision(ctx context.Context, repository, target string) (string, error) {
	output, exceeded, err := manager.runLimited(
		ctx,
		repository,
		1024,
		"for-each-ref",
		"--count=2",
		"--format=%(refname)%00%(objectname)%00%(objecttype)%00%(symref)",
		target,
	)
	if exceeded {
		return "", fmt.Errorf("Git returned oversized metadata for release target %q", target)
	}
	if err != nil {
		return "", fmt.Errorf("resolve Git release target %q: %w", target, err)
	}
	output = bytes.TrimSuffix(output, []byte{'\n'})
	fields := bytes.Split(output, []byte{0})
	if len(fields) != 4 || string(fields[0]) != target {
		return "", fmt.Errorf("Git release target %q does not resolve to one direct ref", target)
	}
	if len(fields[3]) != 0 {
		return "", fmt.Errorf("Git release target %q must be a direct ref, not a symbolic ref", target)
	}
	revision := string(fields[1])
	if string(fields[2]) != "commit" || !validObjectID(revision) {
		return "", fmt.Errorf("Git release target %q does not directly reference a commit", target)
	}
	return revision, nil
}

func (manager *Manager) ensureTargetNotCheckedOut(ctx context.Context, repository, target string) error {
	output, err := manager.outputText(ctx, repository, "worktree", "list", "--porcelain")
	if err != nil {
		return fmt.Errorf("list Git worktrees: %w", err)
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "branch "+target {
			return fmt.Errorf("Git release target %q is checked out in a worktree", target)
		}
	}
	return nil
}

func (manager *Manager) run(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	output, err := manager.runInput(ctx, directory, "", nil, arguments...)
	return []byte(output), err
}

func (manager *Manager) runLimited(
	ctx context.Context,
	directory string,
	maxOutputBytes int64,
	arguments ...string,
) ([]byte, bool, error) {
	return manager.runLimitedEnvironment(ctx, directory, maxOutputBytes, nil, arguments...)
}

func (manager *Manager) runLimitedEnvironment(
	ctx context.Context,
	directory string,
	maxOutputBytes int64,
	extraEnvironment []string,
	arguments ...string,
) ([]byte, bool, error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandCtx, manager.gitPath, arguments...)
	command.Dir = directory
	command.Env = gitEnvironment(extraEnvironment)
	stdout := boundedBuffer{limit: maxOutputBytes, cancel: cancel}
	stderr := boundedBuffer{limit: maximumGitErrorBytes, cancel: cancel}
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(string(stderr.bytes()))
		if message == "" {
			message = err.Error()
		} else if stderr.exceeded {
			message += " (truncated)"
		}
		return stdout.bytes(), stdout.exceeded, errors.New(message)
	}
	return stdout.bytes(), stdout.exceeded, nil
}

func (manager *Manager) runInputBytesLimited(
	ctx context.Context,
	directory string,
	input []byte,
	maxOutputBytes int64,
	extraEnvironment []string,
	arguments ...string,
) ([]byte, bool, error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandCtx, manager.gitPath, arguments...)
	command.Dir = directory
	command.Env = gitEnvironment(extraEnvironment)
	command.Stdin = bytes.NewReader(input)
	stdout := boundedBuffer{limit: maxOutputBytes, cancel: cancel}
	stderr := boundedBuffer{limit: maximumGitErrorBytes, cancel: cancel}
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(string(stderr.bytes()))
		if message == "" {
			message = err.Error()
		} else if stderr.exceeded {
			message += " (truncated)"
		}
		return stdout.bytes(), stdout.exceeded, errors.New(message)
	}
	return stdout.bytes(), stdout.exceeded, nil
}

func (manager *Manager) runInput(
	ctx context.Context,
	directory string,
	input string,
	extraEnvironment []string,
	arguments ...string,
) (string, error) {
	command := exec.CommandContext(ctx, manager.gitPath, arguments...)
	command.Dir = directory
	command.Env = gitEnvironment(extraEnvironment)
	if input != "" {
		command.Stdin = strings.NewReader(input)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", errors.New(message)
	}
	return stdout.String(), nil
}

func gitEnvironment(extra []string) []string {
	overrides := map[string]struct{}{
		"GIT_CONFIG_NOSYSTEM": {},
		"GIT_CONFIG_GLOBAL":   {},
		"GIT_CONFIG_COUNT":    {},
		"GIT_CONFIG_KEY_0":    {},
		"GIT_CONFIG_VALUE_0":  {},
		"GIT_CONFIG_KEY_1":    {},
		"GIT_CONFIG_VALUE_1":  {},
		"GIT_ATTR_NOSYSTEM":   {},
		"GIT_TERMINAL_PROMPT": {},
		"GIT_INDEX_FILE":      {},
		"GIT_WORK_TREE":       {},
		"GIT_DIR":             {},
	}
	for _, value := range extra {
		if separator := strings.IndexByte(value, '='); separator > 0 {
			overrides[value[:separator]] = struct{}{}
		}
	}
	environment := make([]string, 0, len(os.Environ())+len(extra)+2)
	for _, value := range os.Environ() {
		separator := strings.IndexByte(value, '=')
		if separator <= 0 {
			continue
		}
		if _, overridden := overrides[value[:separator]]; !overridden {
			environment = append(environment, value)
		}
	}
	environment = append(
		environment,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0="+os.DevNull,
		"GIT_CONFIG_KEY_1=core.fsmonitor",
		"GIT_CONFIG_VALUE_1=false",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	return append(environment, extra...)
}

type boundedWriter struct {
	writer   io.Writer
	limit    int64
	written  int64
	exceeded bool
	cancel   context.CancelFunc
}

func (writer *boundedWriter) Write(data []byte) (int, error) {
	dataLength := len(data)
	remaining := writer.limit - writer.written
	if remaining > 0 {
		writeLength := dataLength
		if int64(writeLength) > remaining {
			writeLength = int(remaining)
		}
		written, err := writer.writer.Write(data[:writeLength])
		writer.written += int64(written)
		if err != nil {
			return written, err
		}
		if written != writeLength {
			return written, io.ErrShortWrite
		}
	}
	if int64(dataLength) > remaining {
		writer.exceeded = true
		if writer.cancel != nil {
			writer.cancel()
		}
	}
	return dataLength, nil
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	exceeded bool
	cancel   context.CancelFunc
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	dataLength := len(data)
	remaining := buffer.limit - int64(buffer.buffer.Len())
	if remaining > 0 {
		writeLength := dataLength
		if int64(writeLength) > remaining {
			writeLength = int(remaining)
		}
		_, _ = buffer.buffer.Write(data[:writeLength])
	}
	if int64(dataLength) > remaining {
		buffer.exceeded = true
		if buffer.cancel != nil {
			buffer.cancel()
		}
	}
	return dataLength, nil
}

func (buffer *boundedBuffer) bytes() []byte {
	return buffer.buffer.Bytes()
}

func changeListCaptureLimit(maxChangedFiles int) int64 {
	if maxChangedFiles > int(maximumChangeListCaptureBytes/estimatedBytesPerChangedPath) {
		return maximumChangeListCaptureBytes
	}
	limit := int64(maxChangedFiles) * estimatedBytesPerChangedPath
	if limit < minimumChangeListCaptureBytes {
		return minimumChangeListCaptureBytes
	}
	if limit > maximumChangeListCaptureBytes {
		return maximumChangeListCaptureBytes
	}
	return limit
}

func parseNameStatus(data []byte) ([]change, error) {
	fields := splitNUL(data)
	if len(fields)%2 != 0 {
		return nil, errors.New("invalid NUL-delimited Git name-status output")
	}
	result := make([]change, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		status := fields[i]
		path := fields[i+1]
		if err := validateGitPath(path); err != nil {
			return nil, err
		}
		operation := ""
		switch {
		case strings.HasPrefix(status, "A"):
			operation = "add"
		case strings.HasPrefix(status, "M"), strings.HasPrefix(status, "T"):
			operation = "modify"
		case strings.HasPrefix(status, "D"):
			operation = "delete"
		default:
			return nil, fmt.Errorf("unsupported or unresolved Git status %q for %q", status, path)
		}
		result = append(result, change{path: path, operation: operation})
	}
	return result, nil
}

func splitNUL(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	data = bytes.TrimSuffix(data, []byte{0})
	parts := bytes.Split(data, []byte{0})
	result := make([]string, len(parts))
	for i, part := range parts {
		result[i] = string(part)
	}
	return result
}

func validateGitPath(path string) error {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(filepath.FromSlash(path)) != filepath.FromSlash(path) || path == "." {
		return fmt.Errorf("unsafe Git path %q", path)
	}
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("unsafe Git path %q", path)
		}
		if strings.EqualFold(component, ".git") {
			return fmt.Errorf("Git administrative path %q is not stageable", path)
		}
	}
	return nil
}

func workspaceName(transactionID string) string {
	sum := sha256.Sum256([]byte(transactionID))
	return "tx-" + hex.EncodeToString(sum[:12])
}

func effectID(transactionID, path, operation string) string {
	sum := sha256.Sum256([]byte(transactionID + "\x00" + path + "\x00" + operation))
	return "effect:" + hex.EncodeToString(sum[:16])
}

func idempotencyKey(transactionID, effectID string) string {
	sum := sha256.Sum256([]byte(transactionID + "\x00" + effectID))
	return "idempotency:" + hex.EncodeToString(sum[:16])
}

func stageBindingID(transactionID string) string {
	sum := sha256.Sum256([]byte(transactionID))
	return "stage:git:" + hex.EncodeToString(sum[:16])
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func mediaType(mode string) string {
	if mode == "symlink" {
		return "application/vnd.gatemole.symlink"
	}
	if mode == "deleted" {
		return "application/vnd.gatemole.tombstone+json"
	}
	return "application/octet-stream"
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
