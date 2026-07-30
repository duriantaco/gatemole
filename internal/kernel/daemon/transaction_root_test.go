package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPrepareTransactionRootCreatesPrivateRealDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gatemole", "transactions", "repo")
	prepared, err := prepareTransactionRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if prepared != canonicalRoot {
		t.Fatalf("prepared root=%q, want %q", prepared, canonicalRoot)
	}
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() ||
		info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0o022 != 0 {
		t.Fatalf("created transaction root is unsafe: %v", info.Mode())
	}
	if _, err := prepareTransactionRoot(root); err != nil {
		t.Fatalf("secure existing transaction root was rejected: %v", err)
	}
}

func TestPrepareTransactionRootRejectsRelativeSymlinkAndWritablePaths(
	t *testing.T,
) {
	t.Run("relative", func(t *testing.T) {
		if _, err := prepareTransactionRoot("transactions"); err == nil ||
			!strings.Contains(err.Error(), "absolute") {
			t.Fatalf("relative transaction root was accepted: %v", err)
		}
	})

	t.Run("symlink root", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("create directory symlink: %v", err)
		}
		if _, err := prepareTransactionRoot(link); err == nil ||
			!strings.Contains(err.Error(), "real directory") {
			t.Fatalf("symlinked transaction root was accepted: %v", err)
		}
	})

	t.Run("non-sticky writable parent", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "shared")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareTransactionRoot(
			filepath.Join(parent, "transactions"),
		); err == nil || !strings.Contains(
			err.Error(),
			"unless it has the sticky bit",
		) {
			t.Fatalf("writable transaction parent was accepted: %v", err)
		}
	})

	t.Run("writable leaf", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "transactions")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0o770); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareTransactionRoot(root); err == nil ||
			!strings.Contains(err.Error(), "group- or world-writable") {
			t.Fatalf("writable transaction root was accepted: %v", err)
		}
	})
}

func TestPrepareTransactionRootAcceptsProtectedChildOfStickyParent(
	t *testing.T,
) {
	parent := filepath.Join(t.TempDir(), "sticky")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "owned", "transactions")
	if _, err := prepareTransactionRoot(root); err != nil {
		t.Fatalf("private child under sticky parent was rejected: %v", err)
	}
}

func TestPrepareTransactionRootRejectsRepositoryOverlapBeforeCreation(
	t *testing.T,
) {
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(repository, ".transactions")
	if _, err := prepareTransactionRootForRepository(
		inside,
		repository,
	); err == nil || !strings.Contains(err.Error(), "disjoint") {
		t.Fatalf("in-repository transaction root was accepted: %v", err)
	}
	if _, err := os.Lstat(inside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected in-repository root was created: %v", err)
	}

	ancestor := filepath.Dir(repository)
	if _, err := prepareTransactionRootForRepository(
		ancestor,
		repository,
	); err == nil || !strings.Contains(err.Error(), "disjoint") {
		t.Fatalf("repository-containing transaction root was accepted: %v", err)
	}
}

func TestTransactionRootRevalidationDetectsReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "transactions")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(parent, "replacement")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, root); err != nil {
		t.Skipf("atomically replace transaction root: %v", err)
	}
	err = revalidateTransactionDirectorySnapshots(
		[]transactionDirectorySnapshot{{path: root, info: info}},
		root,
		uint32(os.Geteuid()),
	)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("replacement transaction root was accepted: %v", err)
	}
}

func TestTransactionRootRejectsForeignOwnerMetadata(t *testing.T) {
	directory := t.TempDir()
	info, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("Unix owner metadata is unavailable")
	}
	foreign := *stat
	foreign.Uid = uint32(os.Geteuid()) + 1
	err = validateTransactionDirectoryInfo(
		directory,
		foreignOwnerFileInfo{FileInfo: info, stat: &foreign},
		uint32(os.Geteuid()),
		true,
	)
	if err == nil || !strings.Contains(err.Error(), "owned by daemon UID") {
		t.Fatalf("foreign-owned transaction root was accepted: %v", err)
	}
}

func TestDefaultTransactionRootUsesSecurePerUserRepositoryScope(
	t *testing.T,
) {
	runtimeDirectory := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDirectory)
	repositoryA := filepath.Join(t.TempDir(), "repo-a")
	repositoryB := filepath.Join(t.TempDir(), "repo-b")
	for _, repository := range []string{repositoryA, repositoryB} {
		if err := os.Mkdir(repository, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	rootA, err := DefaultTransactionRoot(repositoryA)
	if err != nil {
		t.Fatal(err)
	}
	rootB, err := DefaultTransactionRoot(repositoryB)
	if err != nil {
		t.Fatal(err)
	}
	if rootA == rootB {
		t.Fatalf("repositories share transaction root %q", rootA)
	}
	canonicalRuntimeDirectory, err := filepath.EvalSymlinks(runtimeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	wantParent := filepath.Join(
		canonicalRuntimeDirectory,
		"gatemole",
		"transactions",
	)
	if filepath.Dir(rootA) != wantParent {
		t.Fatalf(
			"default transaction root parent=%q, want %q",
			filepath.Dir(rootA),
			wantParent,
		)
	}
	if _, err := os.Lstat(rootA); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default selection unexpectedly created the root: %v", err)
	}
}

type foreignOwnerFileInfo struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (info foreignOwnerFileInfo) Sys() any {
	return info.stat
}
