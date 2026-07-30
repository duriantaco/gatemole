// Package runtimeidentity owns the local identity of one Vouch Runtime
// installation. The identity document contains no repository path or derived
// path digest and therefore remains stable when its Git worktree is moved.
package runtimeidentity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
)

const (
	// IdentityVersion identifies the strict local Runtime identity document.
	IdentityVersion = "gatemole.runtime_identity.v0"
	// IdentityRelativePath is repository-relative and must never be resolved
	// against an agent-controlled working directory.
	IdentityRelativePath = ".vouch/runtime.json"
	// HTTPHeader carries a client's expected Runtime identity on requests that
	// do not otherwise have a versioned Runtime binding in their body.
	HTTPHeader = "Vouch-Runtime-ID"

	maxIdentityBytes = 4 << 10
	runtimeIDPrefix  = "runtime:"
	runtimeIDBytes   = 32
)

// Identity contains no repository path or other repository-derived material.
// Runtime-to-repository admission is bound separately in the kernel ledger.
type Identity struct {
	Version   string    `json:"version"`
	RuntimeID string    `json:"runtime_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Validate checks the portable document shape.
func (identity Identity) Validate() error {
	if identity.Version != IdentityVersion {
		return fmt.Errorf("runtime identity version must be %q", IdentityVersion)
	}
	if !validRuntimeID(identity.RuntimeID) {
		return errors.New("runtime identity has an invalid Runtime ID")
	}
	if identity.CreatedAt.IsZero() {
		return errors.New("runtime identity has an invalid creation time")
	}
	_, offset := identity.CreatedAt.Zone()
	if offset != 0 {
		return errors.New("runtime identity creation time must be UTC")
	}
	return nil
}

// CreateOrLoad creates a new local identity when absent or loads the existing
// valid identity. It never replaces malformed, tracked, permissive, or
// non-regular state. The boolean reports whether this call created the file.
func CreateOrLoad(
	ctx context.Context,
	repositoryRoot string,
) (Identity, bool, error) {
	return createOrLoad(ctx, repositoryRoot, rand.Reader, time.Now)
}

func createOrLoad(
	ctx context.Context,
	repositoryRoot string,
	random io.Reader,
	now func() time.Time,
) (Identity, bool, error) {
	root, err := resolveRepository(ctx, repositoryRoot)
	if err != nil {
		return Identity{}, false, err
	}
	if random == nil {
		return Identity{}, false, errors.New("create runtime identity: random source is required")
	}
	if now == nil {
		return Identity{}, false, errors.New("create runtime identity: clock is required")
	}
	if err := ensureIdentityDirectory(root); err != nil {
		return Identity{}, false, err
	}
	if err := requireLocalOnlyIdentity(ctx, root); err != nil {
		return Identity{}, false, err
	}

	path := filepath.Join(root, filepath.FromSlash(IdentityRelativePath))
	if _, err := os.Lstat(path); err == nil {
		identity, loadErr := loadResolved(path)
		return identity, false, loadErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, false, fmt.Errorf("inspect %s: %w", IdentityRelativePath, err)
	}

	randomID, err := newRuntimeID(random)
	if err != nil {
		return Identity{}, false, err
	}
	identity := Identity{
		Version:   IdentityVersion,
		RuntimeID: randomID,
		CreatedAt: now().UTC(),
	}
	if err := identity.Validate(); err != nil {
		return Identity{}, false, err
	}
	data, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return Identity{}, false, fmt.Errorf("encode runtime identity: %w", err)
	}
	data = append(data, '\n')

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		loaded, loadErr := loadResolved(path)
		return loaded, false, loadErr
	}
	if err != nil {
		return Identity{}, false, fmt.Errorf("create %s: %w", IdentityRelativePath, err)
	}
	removeIncomplete := true
	defer func() {
		_ = file.Close()
		if removeIncomplete {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return Identity{}, false, fmt.Errorf("restrict %s: %w", IdentityRelativePath, err)
	}
	if _, err := file.Write(data); err != nil {
		return Identity{}, false, fmt.Errorf("write %s: %w", IdentityRelativePath, err)
	}
	if err := file.Sync(); err != nil {
		return Identity{}, false, fmt.Errorf("sync %s: %w", IdentityRelativePath, err)
	}
	if err := file.Close(); err != nil {
		return Identity{}, false, fmt.Errorf("close %s: %w", IdentityRelativePath, err)
	}
	removeIncomplete = false

	loaded, err := loadResolved(path)
	if err != nil {
		return Identity{}, false, fmt.Errorf("verify created runtime identity: %w", err)
	}
	return loaded, true, nil
}

// Load reads the existing local-only identity from an exact Git worktree root.
// The Runtime ID remains stable when that worktree is moved.
func Load(ctx context.Context, repositoryRoot string) (Identity, error) {
	root, err := resolveRepository(ctx, repositoryRoot)
	if err != nil {
		return Identity{}, err
	}
	if err := validateIdentityDirectory(root); err != nil {
		return Identity{}, err
	}
	if err := requireLocalOnlyIdentity(ctx, root); err != nil {
		return Identity{}, err
	}
	return loadResolved(filepath.Join(
		root, filepath.FromSlash(IdentityRelativePath),
	))
}

// EnsurePrivateDirectory creates or validates the repository-local .vouch
// directory before onboarding writes any profile or ignore file into it.
func EnsurePrivateDirectory(
	ctx context.Context,
	repositoryRoot string,
) error {
	root, err := resolveRepository(ctx, repositoryRoot)
	if err != nil {
		return err
	}
	return ensureIdentityDirectory(root)
}

func loadResolved(path string) (Identity, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return Identity{}, fmt.Errorf("inspect %s: %w", IdentityRelativePath, err)
	}
	if err := validateIdentityFileInfo(before); err != nil {
		return Identity{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Identity{}, fmt.Errorf("open %s: %w", IdentityRelativePath, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return Identity{}, fmt.Errorf("inspect opened %s: %w", IdentityRelativePath, err)
	}
	if err := validateIdentityFileInfo(opened); err != nil {
		return Identity{}, err
	}
	if !os.SameFile(before, opened) {
		return Identity{}, errors.New("runtime identity changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxIdentityBytes+1))
	if err != nil {
		return Identity{}, fmt.Errorf("read %s: %w", IdentityRelativePath, err)
	}
	if len(data) > maxIdentityBytes {
		return Identity{}, fmt.Errorf("runtime identity exceeds %d bytes", maxIdentityBytes)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return Identity{}, fmt.Errorf("reinspect %s: %w", IdentityRelativePath, err)
	}
	if err := validateIdentityFileInfo(after); err != nil {
		return Identity{}, err
	}
	if !os.SameFile(opened, after) {
		return Identity{}, errors.New("runtime identity changed while it was read")
	}

	identity, err := model.DecodeStrict[Identity](data)
	if err != nil {
		return Identity{}, fmt.Errorf("decode runtime identity: %w", err)
	}
	if err := identity.Validate(); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func validateIdentityFileInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("runtime identity must be a regular file, not a symlink")
	}
	if info.Mode().Perm() != 0o600 {
		return errors.New("runtime identity permissions must be exactly 0600")
	}
	if !ownedByCurrentUser(info) {
		return errors.New("runtime identity must be owned by the current user")
	}
	if info.Size() < 1 || info.Size() > maxIdentityBytes {
		return fmt.Errorf("runtime identity size must be between 1 and %d bytes", maxIdentityBytes)
	}
	return nil
}

func ensureIdentityDirectory(root string) error {
	path := filepath.Join(root, ".vouch")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil &&
			!errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create .vouch directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	return validateIdentityDirectoryInfo(info, err)
}

func validateIdentityDirectory(root string) error {
	info, err := os.Lstat(filepath.Join(root, ".vouch"))
	return validateIdentityDirectoryInfo(info, err)
}

func validateIdentityDirectoryInfo(info os.FileInfo, err error) error {
	if err != nil {
		return fmt.Errorf("inspect .vouch directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New(".vouch must be a real directory, not a symlink")
	}
	if !ownedByCurrentUser(info) {
		return errors.New(".vouch must be owned by the current user")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New(".vouch must not be group- or world-writable")
	}
	if info.Mode().Perm()&0o300 != 0o300 {
		return errors.New(".vouch must be writable and searchable by the current user")
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

func resolveRepository(
	ctx context.Context,
	repositoryRoot string,
) (string, error) {
	if ctx == nil {
		return "", errors.New("resolve repository: context is required")
	}
	if strings.TrimSpace(repositoryRoot) == "" ||
		strings.TrimSpace(repositoryRoot) != repositoryRoot {
		return "", errors.New("repository root is required without surrounding whitespace")
	}
	absolute, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect repository root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("repository root must be a real directory, not a symlink")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("canonicalize repository root: %w", err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("resolve canonical repository root: %w", err)
	}

	command := exec.CommandContext(
		ctx, "git", "-C", canonical, "rev-parse", "--show-toplevel",
	)
	output, err := command.Output()
	if err != nil {
		return "", errors.New("repository root is not a Git worktree")
	}
	gitRoot := strings.TrimSpace(string(output))
	if gitRoot == "" {
		return "", errors.New("Git returned an empty repository root")
	}
	gitRoot, err = filepath.EvalSymlinks(gitRoot)
	if err != nil {
		return "", fmt.Errorf("canonicalize Git repository root: %w", err)
	}
	gitRoot, err = filepath.Abs(gitRoot)
	if err != nil {
		return "", fmt.Errorf("resolve Git repository root: %w", err)
	}
	if filepath.Clean(canonical) != filepath.Clean(gitRoot) {
		return "", errors.New(
			"repository root must be the exact Git worktree root",
		)
	}
	return canonical, nil
}

func requireLocalOnlyIdentity(ctx context.Context, root string) error {
	tracked := exec.CommandContext(
		ctx, "git", "-C", root,
		"ls-files", "--cached", "--", IdentityRelativePath,
	)
	trackedOutput, err := tracked.Output()
	if err != nil {
		return fmt.Errorf("check tracked Runtime identity state: %w", err)
	}
	if strings.TrimSpace(string(trackedOutput)) != "" {
		return errors.New("runtime identity must not be tracked by Git")
	}
	ignored := exec.CommandContext(
		ctx, "git", "-C", root,
		"check-ignore", "--quiet", "--no-index", "--", IdentityRelativePath,
	)
	if err := ignored.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("check Runtime identity ignore state: %w", ctx.Err())
		}
		return errors.New(
			"runtime identity must be Git-ignored before it can be created or loaded",
		)
	}
	return nil
}

func newRuntimeID(random io.Reader) (string, error) {
	value := make([]byte, runtimeIDBytes)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", fmt.Errorf("generate Runtime ID: %w", err)
	}
	return runtimeIDPrefix + hex.EncodeToString(value), nil
}

func validRuntimeID(value string) bool {
	return model.IsRuntimeID(value)
}

// IsRuntimeID reports whether value is a canonical opaque Runtime instance ID.
func IsRuntimeID(value string) bool {
	return validRuntimeID(value)
}
