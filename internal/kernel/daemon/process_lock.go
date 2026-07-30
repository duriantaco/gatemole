package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/duriantaco/vouch/internal/kernel/model"
)

type processLock struct {
	file *os.File
	path string
}

func acquireProcessLock(databasePath string) (*processLock, error) {
	lockPath := databasePath + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o750); err != nil {
		return nil, fmt.Errorf("vouchd: create lock directory: %w", err)
	}
	return acquireFileLock(
		lockPath,
		"database is already owned by another daemon: "+databasePath,
	)
}

func acquireRuntimeLock(
	repositoryRoot string,
	runtimeID string,
) (*processLock, error) {
	if !model.IsRuntimeID(runtimeID) {
		return nil, errors.New("vouchd: Runtime lock requires a canonical Runtime ID")
	}
	if strings.TrimSpace(repositoryRoot) == "" ||
		!filepath.IsAbs(repositoryRoot) {
		return nil, errors.New(
			"vouchd: Runtime lock requires an absolute repository root",
		)
	}
	directory := filepath.Join(repositoryRoot, ".gatemole")
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf(
			"vouchd: inspect private Runtime directory: %w",
			err,
		)
	}
	if err := validateRuntimeLockDirectory(
		info,
		uint32(os.Geteuid()),
	); err != nil {
		return nil, err
	}
	return acquireFileLock(
		filepath.Join(directory, "runtime.lock"),
		"Runtime instance is already owned by another daemon: "+runtimeID,
	)
}

func validateRuntimeLockDirectory(
	info os.FileInfo,
	expectedUID uint32,
) error {
	if info == nil ||
		!info.IsDir() ||
		info.Mode()&os.ModeSymlink != 0 {
		return errors.New(
			"vouchd: Runtime lock directory must be a real directory",
		)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != expectedUID {
		return errors.New(
			"vouchd: Runtime lock directory is not owned by the daemon user",
		)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New(
			"vouchd: Runtime lock directory must not be group- or world-writable",
		)
	}
	if info.Mode().Perm()&0o300 != 0o300 {
		return errors.New(
			"vouchd: Runtime lock directory must be writable and searchable by the daemon user",
		)
	}
	return nil
}

func acquireFileLock(
	lockPath string,
	alreadyOwnedMessage string,
) (*processLock, error) {
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vouchd: open process lock %s: %w", lockPath, err)
	}
	if err := os.Chmod(lockPath, 0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("vouchd: restrict process lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.New("vouchd: " + alreadyOwnedMessage)
		}
		return nil, fmt.Errorf("vouchd: acquire process lock %s: %w", lockPath, err)
	}
	return &processLock{file: file, path: lockPath}, nil
}

func (lock *processLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}
