package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type transactionDirectorySnapshot struct {
	path string
	info os.FileInfo
}

// DefaultTransactionRoot returns a repository-scoped path beneath a secure
// per-user runtime or cache directory. Run still creates and revalidates the
// complete path before the transaction manager can use it.
func DefaultTransactionRoot(repository string) (string, error) {
	if !filepath.IsAbs(repository) {
		return "", errors.New(
			"gatemoled: repository root must be absolute before selecting a transaction root",
		)
	}
	canonicalRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		return "", fmt.Errorf(
			"gatemoled: canonicalize repository for transaction root: %w",
			err,
		)
	}
	canonicalRepository = filepath.Clean(canonicalRepository)
	expectedUID := uint32(os.Geteuid())

	base := ""
	if runtimeDirectory := os.Getenv("XDG_RUNTIME_DIR"); runtimeDirectory != "" {
		if canonicalBase, err := validateDefaultTransactionBase(
			runtimeDirectory,
			expectedUID,
			true,
		); err == nil {
			base = canonicalBase
		}
	}
	if base == "" {
		cacheDirectory, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf(
				"gatemoled: resolve secure per-user transaction base: %w",
				err,
			)
		}
		canonicalBase, err := validateDefaultTransactionBase(
			cacheDirectory,
			expectedUID,
			false,
		)
		if err != nil {
			return "", fmt.Errorf(
				"gatemoled: secure per-user transaction base is unavailable: %w",
				err,
			)
		}
		base = canonicalBase
	}

	sum := sha256.Sum256([]byte(canonicalRepository))
	return filepath.Join(
		base,
		"gatemole",
		"transactions",
		hex.EncodeToString(sum[:12]),
	), nil
}

func validateDefaultTransactionBase(
	path string,
	expectedUID uint32,
	mustExist bool,
) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("per-user directory must be absolute")
	}
	canonical, err := canonicalTransactionPath(path)
	if err != nil {
		return "", err
	}
	snapshots, complete, err := inspectTransactionDirectoryPath(
		canonical,
		expectedUID,
		false,
	)
	if err != nil {
		return "", err
	}
	if mustExist && !complete {
		return "", errors.New("per-user runtime directory does not exist")
	}
	if complete && len(snapshots) > 0 {
		leaf := snapshots[len(snapshots)-1]
		if err := validateTransactionDirectoryInfo(
			leaf.path,
			leaf.info,
			expectedUID,
			true,
		); err != nil {
			return "", err
		}
	}
	return canonical, nil
}

// prepareTransactionRoot creates one missing path component at a time beneath
// already-validated parents, rejects symlinks and foreign-owned components,
// then rechecks every inode. Other UIDs therefore cannot pre-create or replace
// the root through a shared non-sticky parent.
func prepareTransactionRoot(path string) (string, error) {
	return prepareTransactionRootForRepository(path, "")
}

func prepareTransactionRootForRepository(
	path string,
	repositoryRoot string,
) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New(
			"gatemoled: transaction staging root must be an absolute path",
		)
	}
	if info, err := os.Lstat(path); err == nil &&
		info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New(
			"gatemoled: transaction staging root must be a real directory, not a symlink",
		)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf(
			"gatemoled: inspect transaction staging root: %w",
			err,
		)
	}
	cleaned, err := canonicalTransactionPath(path)
	if err != nil {
		return "", fmt.Errorf(
			"gatemoled: canonicalize transaction staging root: %w",
			err,
		)
	}
	if cleaned == filepath.Clean(string(filepath.Separator)) {
		return "", errors.New(
			"gatemoled: transaction staging root cannot be the filesystem root",
		)
	}
	if repositoryRoot != "" {
		if !filepath.IsAbs(repositoryRoot) {
			return "", errors.New(
				"gatemoled: repository root must be absolute before validating transaction staging",
			)
		}
		canonicalRepository, err := canonicalTransactionPath(
			repositoryRoot,
		)
		if err != nil {
			return "", fmt.Errorf(
				"gatemoled: canonicalize repository before validating transaction staging: %w",
				err,
			)
		}
		if pathsOverlap(
			canonicalRepository,
			cleaned,
		) {
			return "", errors.New(
				"gatemoled: transaction staging root must be outside and disjoint from the source repository",
			)
		}
	}
	_, complete, err := inspectTransactionDirectoryPath(
		cleaned,
		uint32(os.Geteuid()),
		true,
	)
	if err != nil {
		return "", fmt.Errorf(
			"gatemoled: secure transaction staging root %s: %w",
			cleaned,
			err,
		)
	}
	if !complete {
		return "", errors.New(
			"gatemoled: transaction staging root was not created completely",
		)
	}
	return cleaned, nil
}

func pathsOverlap(first string, second string) bool {
	return pathContains(first, second) || pathContains(second, first)
}

func pathContains(parent string, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." ||
		(relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// canonicalTransactionPath resolves every existing component, then appends
// only the still-missing suffix. Returning the canonical path means a trusted
// system alias such as macOS /var cannot later redirect transaction access.
func canonicalTransactionPath(path string) (string, error) {
	current := filepath.Clean(path)
	missing := make([]string, 0, 4)
	for {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	canonical, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for index := len(missing) - 1; index >= 0; index-- {
		canonical = filepath.Join(canonical, missing[index])
	}
	return filepath.Clean(canonical), nil
}

func inspectTransactionDirectoryPath(
	path string,
	expectedUID uint32,
	create bool,
) ([]transactionDirectorySnapshot, bool, error) {
	components := transactionPathComponents(path)
	snapshots := make([]transactionDirectorySnapshot, 0, len(components))
	for index, component := range components {
		info, err := os.Lstat(component)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				return snapshots, false, nil
			}
			if err := os.Mkdir(component, 0o700); err != nil &&
				!errors.Is(err, os.ErrExist) {
				return nil, false, fmt.Errorf(
					"create directory %s: %w",
					component,
					err,
				)
			}
			info, err = os.Lstat(component)
		}
		if err != nil {
			return nil, false, fmt.Errorf(
				"inspect directory %s: %w",
				component,
				err,
			)
		}
		leaf := index == len(components)-1
		if err := validateTransactionDirectoryInfo(
			component,
			info,
			expectedUID,
			leaf,
		); err != nil {
			return nil, false, err
		}
		snapshots = append(snapshots, transactionDirectorySnapshot{
			path: component,
			info: info,
		})
	}

	if err := revalidateTransactionDirectorySnapshots(
		snapshots,
		path,
		expectedUID,
	); err != nil {
		return nil, false, err
	}
	return snapshots, true, nil
}

func revalidateTransactionDirectorySnapshots(
	snapshots []transactionDirectorySnapshot,
	leafPath string,
	expectedUID uint32,
) error {
	for _, snapshot := range snapshots {
		current, err := os.Lstat(snapshot.path)
		if err != nil {
			return fmt.Errorf(
				"reinspect directory %s: %w",
				snapshot.path,
				err,
			)
		}
		if !os.SameFile(snapshot.info, current) {
			return fmt.Errorf(
				"directory %s changed while the transaction root was validated",
				snapshot.path,
			)
		}
		leaf := snapshot.path == leafPath
		if err := validateTransactionDirectoryInfo(
			snapshot.path,
			current,
			expectedUID,
			leaf,
		); err != nil {
			return err
		}
	}
	return nil
}

func transactionPathComponents(path string) []string {
	current := filepath.Clean(path)
	reversed := make([]string, 0, 8)
	for {
		reversed = append(reversed, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	components := make([]string, len(reversed))
	for index := range reversed {
		components[index] = reversed[len(reversed)-1-index]
	}
	return components
}

func validateTransactionDirectoryInfo(
	path string,
	info os.FileInfo,
	expectedUID uint32,
	leaf bool,
) error {
	if info == nil ||
		!info.IsDir() ||
		info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"transaction path component %s must be a real directory, not a symlink",
			path,
		)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf(
			"transaction path component %s has no Unix owner metadata",
			path,
		)
	}
	if leaf {
		if stat.Uid != expectedUID {
			return fmt.Errorf(
				"transaction staging root %s must be owned by daemon UID %d",
				path,
				expectedUID,
			)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf(
				"transaction staging root %s must not be group- or world-writable",
				path,
			)
		}
		if info.Mode().Perm()&0o300 != 0o300 {
			return fmt.Errorf(
				"transaction staging root %s must be writable and searchable by the daemon user",
				path,
			)
		}
		return nil
	}

	if stat.Uid != 0 && stat.Uid != expectedUID {
		return fmt.Errorf(
			"transaction path parent %s must be owned by root or daemon UID %d",
			path,
			expectedUID,
		)
	}
	if info.Mode().Perm()&0o022 != 0 &&
		info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf(
			"transaction path parent %s must not be group- or world-writable unless it has the sticky bit",
			path,
		)
	}
	return nil
}
