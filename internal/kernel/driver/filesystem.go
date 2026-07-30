package driver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/duriantaco/vouch/internal/kernel/capability"
	"github.com/duriantaco/vouch/internal/kernel/model"
)

const (
	FilesystemRead  = "filesystem.read"
	FilesystemWrite = "filesystem.write"
	defaultMaxBytes = int64(1 << 20)
)

type FilesystemReadArguments struct {
	Path string `json:"path"`
}

type FilesystemWriteArguments struct {
	Path          string `json:"path"`
	ContentDigest string `json:"content_digest"`
}

type Result struct {
	Digest string
	Bytes  int64
	Output []byte
}

// ExecutionError distinguishes an ordinary failed effect from an ambiguous
// outcome that must never be retried blindly.
type ExecutionError struct {
	Unknown bool
	Err     error
}

func (e *ExecutionError) Error() string { return e.Err.Error() }
func (e *ExecutionError) Unwrap() error { return e.Err }

type Filesystem struct {
	repositoryRoot string
}

func NewFilesystem(repositoryRoot string) (*Filesystem, error) {
	absolute, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect repository root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("repository root is not a directory: %s", absolute)
	}
	return &Filesystem{repositoryRoot: absolute}, nil
}

func (filesystem *Filesystem) Execute(
	run model.AgentRun,
	grant model.CapabilityGrant,
	request model.ActionRequest,
	input []byte,
) (Result, error) {
	if grant.SubjectRunID != run.ID || request.RunID != run.ID {
		return Result{}, denied(request.ID, "run does not own the filesystem capability or action")
	}
	if grant.Resource.Kind != "filesystem" || request.Resource.Kind != "filesystem" {
		return Result{}, denied(request.ID, "filesystem driver requires filesystem resources")
	}
	if !slices.Contains(grant.Operations, request.Operation) {
		return Result{}, denied(request.ID, "capability does not include the requested operation")
	}
	logicalPath, contentDigest, normalized, err := NormalizeArguments(request.Operation, request.Arguments)
	if err != nil {
		return Result{}, err
	}
	if request.Resource.Pattern != logicalPath {
		return Result{}, denied(request.ID, "action resource does not match its typed path argument")
	}
	if !capability.MatchResource(grant.Resource.Pattern, logicalPath) {
		return Result{}, denied(request.ID, "filesystem path is outside the capability selector")
	}
	if request.ArgumentsDigest != DigestBytes(normalized) {
		return Result{}, &model.KernelError{
			Code: model.ErrorSchemaInvalid, Operation: "filesystem_action", Resource: request.ID,
			Message: "arguments_digest does not match normalized typed arguments",
		}
	}
	workspace, valid := capability.CleanLogicalPath(run.Workspace)
	if !valid || grant.Conditions.WorkspaceRoot != workspace {
		return Result{}, denied(request.ID, "run workspace and capability workspace_root do not match")
	}
	if containsControlMetadataPath(workspace) || containsControlMetadataPath(logicalPath) {
		return Result{}, denied(request.ID, "Git, Gatemole, and legacy Vouch control-state paths are never available to the filesystem driver")
	}
	relative, valid := capability.WorkspaceRelative(workspace, logicalPath)
	if !valid {
		return Result{}, denied(request.ID, "filesystem path is outside the run workspace")
	}
	repository, err := os.OpenRoot(filesystem.repositoryRoot)
	if err != nil {
		return Result{}, fmt.Errorf("open repository root: %w", err)
	}
	defer repository.Close()
	if err := rejectSymlinkComponents(repository, workspace, request.ID, true); err != nil {
		return Result{}, err
	}
	workspaceRoot, err := repository.OpenRoot(filepath.FromSlash(workspace))
	if err != nil {
		return Result{}, fmt.Errorf("open run workspace: %w", err)
	}
	defer workspaceRoot.Close()

	limit := defaultMaxBytes
	if grant.Conditions.MaxOutputBytes != nil {
		limit = *grant.Conditions.MaxOutputBytes
	}
	switch request.Operation {
	case FilesystemRead:
		if len(input) != 0 {
			return Result{}, schemaError(request.ID, "filesystem.read does not accept input content")
		}
		if err := rejectSymlinkComponents(workspaceRoot, relative, request.ID, true); err != nil {
			return Result{}, err
		}
		return readFile(workspaceRoot, filepath.FromSlash(relative), limit)
	case FilesystemWrite:
		if int64(len(input)) > limit {
			return Result{}, denied(request.ID, fmt.Sprintf("write input exceeds capability limit of %d bytes", limit))
		}
		if DigestBytes(input) != contentDigest {
			return Result{}, schemaError(request.ID, "write content does not match content_digest")
		}
		if err := rejectSymlinkComponents(workspaceRoot, relative, request.ID, false); err != nil {
			return Result{}, err
		}
		return writeFile(workspaceRoot, filepath.FromSlash(relative), input)
	default:
		return Result{}, schemaError(request.ID, fmt.Sprintf("unsupported filesystem operation %q", request.Operation))
	}
}

func containsControlMetadataPath(logicalPath string) bool {
	for _, component := range strings.Split(logicalPath, "/") {
		// EqualFold covers case-insensitive filesystems. Trimming trailing dots
		// and spaces also rejects Win32 aliases such as ".git." and ".git ".
		normalized := strings.TrimRight(component, ". ")
		if stream := strings.IndexByte(normalized, ':'); stream >= 0 {
			normalized = normalized[:stream]
		}
		if strings.EqualFold(normalized, ".git") ||
			strings.EqualFold(normalized, ".gatemole") ||
			strings.EqualFold(normalized, ".vouch") {
			return true
		}
	}
	return false
}

func rejectSymlinkComponents(
	root *os.Root,
	logicalPath string,
	resource string,
	includeLeaf bool,
) error {
	components := strings.Split(filepath.ToSlash(logicalPath), "/")
	limit := len(components)
	if !includeLeaf {
		limit--
	}
	current := ""
	for _, component := range components[:limit] {
		current = path.Join(current, component)
		info, err := root.Lstat(filepath.FromSlash(current))
		if errors.Is(err, os.ErrNotExist) {
			// The eventual operation will report the missing parent or leaf.
			// No later component can exist beneath a missing component.
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return denied(resource, "filesystem paths cannot traverse symbolic links")
		}
	}
	return nil
}

func NormalizeArguments(operation string, raw json.RawMessage) (logicalPath, contentDigest string, normalized []byte, err error) {
	switch operation {
	case FilesystemRead:
		arguments, decodeErr := model.DecodeStrict[FilesystemReadArguments](raw)
		if decodeErr != nil {
			return "", "", nil, decodeErr
		}
		if _, valid := capability.CleanLogicalPath(arguments.Path); !valid {
			return "", "", nil, schemaError("", "read path must be a clean relative logical path")
		}
		normalized, err = json.Marshal(arguments)
		return arguments.Path, "", normalized, err
	case FilesystemWrite:
		arguments, decodeErr := model.DecodeStrict[FilesystemWriteArguments](raw)
		if decodeErr != nil {
			return "", "", nil, decodeErr
		}
		if _, valid := capability.CleanLogicalPath(arguments.Path); !valid {
			return "", "", nil, schemaError("", "write path must be a clean relative logical path")
		}
		if !validDigest(arguments.ContentDigest) {
			return "", "", nil, schemaError("", "content_digest must be a lowercase sha256 digest")
		}
		normalized, err = json.Marshal(arguments)
		return arguments.Path, arguments.ContentDigest, normalized, err
	default:
		return "", "", nil, schemaError("", fmt.Sprintf("unsupported filesystem operation %q", operation))
	}
}

func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func readFile(root *os.Root, relative string, limit int64) (Result, error) {
	file, err := root.Open(relative)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Result{}, err
	}
	if !info.Mode().IsRegular() {
		return Result{}, errors.New("filesystem.read only supports regular files")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return Result{}, err
	}
	if int64(len(data)) > limit {
		return Result{}, fmt.Errorf("read output exceeds capability limit of %d bytes", limit)
	}
	return Result{Digest: DigestBytes(data), Bytes: int64(len(data)), Output: data}, nil
}

func writeFile(root *os.Root, relative string, data []byte) (Result, error) {
	directory := path.Dir(filepath.ToSlash(relative))
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return Result{}, fmt.Errorf("generate temporary file name: %w", err)
	}
	temporary := path.Join(directory, ".vouch-write-"+hex.EncodeToString(random))
	temporary = filepath.FromSlash(temporary)
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Result{}, err
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = root.Remove(temporary)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return Result{}, err
	}
	if err := file.Sync(); err != nil {
		return Result{}, err
	}
	if err := file.Close(); err != nil {
		return Result{}, err
	}
	if err := root.Rename(temporary, relative); err != nil {
		return Result{}, err
	}
	cleanup = false
	directoryFile, err := root.Open(filepath.FromSlash(directory))
	if err != nil {
		return Result{}, &ExecutionError{Unknown: true, Err: fmt.Errorf("sync committed write directory: %w", err)}
	}
	defer directoryFile.Close()
	if err := directoryFile.Sync(); err != nil {
		return Result{}, &ExecutionError{Unknown: true, Err: fmt.Errorf("sync committed write directory: %w", err)}
	}
	return Result{Digest: DigestBytes(data), Bytes: int64(len(data))}, nil
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && strings.ToLower(value) == value
}

func denied(resource, message string) *model.KernelError {
	return &model.KernelError{Code: model.ErrorCapabilityDenied, Operation: "filesystem_action", Resource: resource, Message: message}
}

func schemaError(resource, message string) *model.KernelError {
	return &model.KernelError{Code: model.ErrorSchemaInvalid, Operation: "filesystem_action", Resource: resource, Message: message}
}
