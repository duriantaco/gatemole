package runtimeidentity

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
)

func TestHTTPHeaderUsesGatemoleNamespace(t *testing.T) {
	if HTTPHeader != "Gatemole-Runtime-ID" {
		t.Fatalf("HTTPHeader=%q, want Gatemole-Runtime-ID", HTTPHeader)
	}
}

func TestCreateOrLoadIsStrictPrivateAndIdempotent(t *testing.T) {
	repository := newGitRepository(t, filepath.Join(t.TempDir(), "repository"))
	first, created, err := CreateOrLoad(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first identity was not created")
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("created identity is invalid: %v", err)
	}
	if !strings.HasPrefix(first.RuntimeID, runtimeIDPrefix) ||
		len(first.RuntimeID) != len(runtimeIDPrefix)+runtimeIDBytes*2 {
		t.Fatalf("unexpected Runtime ID: %q", first.RuntimeID)
	}
	if first.CreatedAt.IsZero() {
		t.Fatal("created identity has no creation time")
	}
	path := filepath.Join(repository, filepath.FromSlash(IdentityRelativePath))
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode=%v, want regular 0600", info.Mode())
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(before, []byte(repository)) {
		t.Fatal("identity document discloses its repository path")
	}

	second, created, err := CreateOrLoad(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if created || second != first {
		t.Fatalf("idempotent load returned %#v created=%t, want %#v", second, created, first)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("idempotent create rewrote the identity document")
	}

	loaded, err := Load(context.Background(), repository)
	if err != nil || loaded != first {
		t.Fatalf("load returned %#v err=%v, want %#v", loaded, err, first)
	}
}

func TestLegacyVouchControlStateFailsClosedWithoutMutation(t *testing.T) {
	tests := []struct {
		name       string
		create     func(*testing.T, string)
		currentDir bool
	}{
		{
			name: "directory",
			create: func(t *testing.T, repository string) {
				t.Helper()
				legacy := filepath.Join(repository, LegacyControlDirectory)
				if err := os.Mkdir(legacy, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					filepath.Join(legacy, "sentinel"),
					[]byte("do not rewrite"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "file",
			create: func(t *testing.T, repository string) {
				t.Helper()
				if err := os.WriteFile(
					filepath.Join(repository, LegacyControlDirectory),
					[]byte("do not rewrite"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			create: func(t *testing.T, repository string) {
				t.Helper()
				if runtime.GOOS == "windows" {
					t.Skip("symlink control-state shape is a Unix boundary")
				}
				target := filepath.Join(t.TempDir(), "legacy-target")
				if err := os.WriteFile(target, []byte("do not rewrite"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(
					target,
					filepath.Join(repository, LegacyControlDirectory),
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "both current and legacy roots",
			currentDir: true,
			create: func(t *testing.T, repository string) {
				t.Helper()
				if err := os.Mkdir(
					filepath.Join(repository, LegacyControlDirectory),
					0o700,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := newGitRepository(
				t,
				filepath.Join(t.TempDir(), "repository"),
			)
			if test.currentDir {
				if err := os.Mkdir(
					filepath.Join(repository, ControlDirectory),
					0o700,
				); err != nil {
					t.Fatal(err)
				}
			}
			test.create(t, repository)

			operations := []struct {
				name string
				run  func() error
			}{
				{
					name: "create or load",
					run: func() error {
						_, _, err := CreateOrLoad(context.Background(), repository)
						return err
					},
				},
				{
					name: "load",
					run: func() error {
						_, err := Load(context.Background(), repository)
						return err
					},
				},
				{
					name: "ensure private directory",
					run: func() error {
						return EnsurePrivateDirectory(context.Background(), repository)
					},
				},
			}
			for _, operation := range operations {
				err := operation.run()
				if err == nil ||
					!strings.Contains(err.Error(), "legacy Vouch control state") ||
					!strings.Contains(err.Error(), LegacyControlDirectory) {
					t.Errorf("%s did not fail closed: %v", operation.name, err)
				}
			}

			if _, err := os.Lstat(
				filepath.Join(repository, LegacyControlDirectory),
			); err != nil {
				t.Fatalf("legacy state was changed or removed: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(
				repository,
				filepath.FromSlash(IdentityRelativePath),
			)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Gatemole identity was created after rejection: %v", err)
			}
			if !test.currentDir {
				if _, err := os.Lstat(
					filepath.Join(repository, ControlDirectory),
				); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Gatemole control state was created after rejection: %v", err)
				}
			}
		})
	}
}

func TestRuntimeIDsUseCryptographicRandomness(t *testing.T) {
	parent := t.TempDir()
	first, _, err := CreateOrLoad(
		context.Background(),
		newGitRepository(t, filepath.Join(parent, "first")),
	)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := CreateOrLoad(
		context.Background(),
		newGitRepository(t, filepath.Join(parent, "second")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.RuntimeID == second.RuntimeID {
		t.Fatal("independent Runtime identities reused a Runtime ID")
	}

	repository := newGitRepository(t, filepath.Join(parent, "failed-random"))
	_, created, err := createOrLoad(
		context.Background(),
		repository,
		errorReader{},
		time.Now,
	)
	if err == nil || created {
		t.Fatalf("failed random source created an identity: created=%t err=%v", created, err)
	}
	if _, statErr := os.Lstat(filepath.Join(
		repository, filepath.FromSlash(IdentityRelativePath),
	)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("random failure left identity state: %v", statErr)
	}
}

func TestIdentityRemainsStableWhenRepositoryMoves(t *testing.T) {
	parent := t.TempDir()
	original := newGitRepository(t, filepath.Join(parent, "original"))
	identity, _, err := CreateOrLoad(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(
		original, filepath.FromSlash(IdentityRelativePath),
	))
	if err != nil {
		t.Fatal(err)
	}

	moved := filepath.Join(parent, "moved")
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(context.Background(), moved)
	if err != nil {
		t.Fatalf("moved identity did not load: %v", err)
	}
	if loaded != identity {
		t.Fatalf("moved identity changed: got %#v want %#v", loaded, identity)
	}
	again, created, err := CreateOrLoad(context.Background(), moved)
	if err != nil || created || again != identity {
		t.Fatalf(
			"moved identity was not idempotent: identity=%#v created=%t err=%v",
			again, created, err,
		)
	}

	// The file store intentionally has no path binding. Copying this ignored
	// local identity deliberately clones the trust target; local artifacts
	// cannot distinguish that from moving/restoring the same Runtime. Normal
	// Git clones do not copy the file and therefore receive distinct IDs.
	copied := newGitRepository(t, filepath.Join(parent, "copied"))
	if err := os.Mkdir(filepath.Join(copied, ".gatemole"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(copied, filepath.FromSlash(IdentityRelativePath)),
		data,
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	copiedIdentity, err := Load(context.Background(), copied)
	if err != nil || copiedIdentity != identity {
		t.Fatalf(
			"repository-neutral identity document changed: identity=%#v err=%v",
			copiedIdentity, err,
		)
	}
}

func TestLoadRejectsUnsafeFileAndDirectoryShapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes and symlinks are Runtime requirements")
	}
	t.Run("permissive file", func(t *testing.T) {
		repository, path := createdIdentityPath(t)
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(context.Background(), repository); err == nil ||
			!strings.Contains(err.Error(), "exactly 0600") {
			t.Fatalf("permissive identity was accepted: %v", err)
		}
	})
	t.Run("symlink file", func(t *testing.T) {
		repository, path := createdIdentityPath(t)
		target := path + ".target"
		if err := os.Rename(path, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(context.Background(), repository); err == nil ||
			!strings.Contains(err.Error(), "regular file") {
			t.Fatalf("symlink identity was accepted: %v", err)
		}
	})
	t.Run("nonregular file", func(t *testing.T) {
		repository := newGitRepository(t, filepath.Join(t.TempDir(), "repository"))
		if err := os.MkdirAll(
			filepath.Join(repository, filepath.FromSlash(IdentityRelativePath)),
			0o700,
		); err != nil {
			t.Fatal(err)
		}
		if _, _, err := CreateOrLoad(
			context.Background(), repository,
		); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("directory identity was accepted: %v", err)
		}
	})
	t.Run("symlink gatemole directory", func(t *testing.T) {
		parent := t.TempDir()
		source := newGitRepository(t, filepath.Join(parent, "source"))
		if _, _, err := CreateOrLoad(context.Background(), source); err != nil {
			t.Fatal(err)
		}
		repository := newGitRepository(t, filepath.Join(parent, "repository"))
		if err := os.Symlink(
			filepath.Join(source, ".gatemole"),
			filepath.Join(repository, ".gatemole"),
		); err != nil {
			t.Fatal(err)
		}
		if _, _, err := CreateOrLoad(
			context.Background(), repository,
		); err == nil || !strings.Contains(err.Error(), "real directory") {
			t.Fatalf("symlink .gatemole directory was accepted: %v", err)
		}
		if _, err := Load(
			context.Background(), repository,
		); err == nil || !strings.Contains(err.Error(), "real directory") {
			t.Fatalf("Load accepted a symlink .gatemole directory: %v", err)
		}
	})
	t.Run("writable gatemole directory", func(t *testing.T) {
		repository, _ := createdIdentityPath(t)
		if err := os.Chmod(filepath.Join(repository, ".gatemole"), 0o770); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(
			context.Background(), repository,
		); err == nil || !strings.Contains(err.Error(), "group- or world-writable") {
			t.Fatalf("writable .gatemole directory was accepted: %v", err)
		}
	})
}

func TestLoadRejectsMalformedIdentityDocuments(t *testing.T) {
	repository, path := createdIdentityPath(t)
	valid, err := Load(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"unknown field": `{
			"version":"gatemole.runtime_identity.v0",
			"runtime_id":"` + valid.RuntimeID + `",
			"created_at":"` + valid.CreatedAt.Format(time.RFC3339Nano) + `",
			"unexpected":true
		}`,
		"wrong version": `{
			"version":"gatemole.runtime_identity.v1",
			"runtime_id":"` + valid.RuntimeID + `",
			"created_at":"` + valid.CreatedAt.Format(time.RFC3339Nano) + `"
		}`,
		"invalid Runtime ID": `{
			"version":"gatemole.runtime_identity.v0",
			"runtime_id":"runtime:not-random",
			"created_at":"` + valid.CreatedAt.Format(time.RFC3339Nano) + `"
		}`,
		"invalid creation time": `{
			"version":"gatemole.runtime_identity.v0",
			"runtime_id":"` + valid.RuntimeID + `",
			"created_at":"0001-01-01T00:00:00Z"
		}`,
		"trailing JSON": `{
			"version":"gatemole.runtime_identity.v0",
			"runtime_id":"` + valid.RuntimeID + `",
			"created_at":"` + valid.CreatedAt.Format(time.RFC3339Nano) + `"
		} {}`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(context.Background(), repository); err == nil {
				t.Fatal("malformed identity was accepted")
			}
		})
	}
}

func TestIdentityMustRemainGitIgnoredAndUntracked(t *testing.T) {
	parent := t.TempDir()
	unignored := newGitRepository(t, filepath.Join(parent, "unignored"))
	excludePath := filepath.Join(unignored, ".git", "info", "exclude")
	if err := os.WriteFile(excludePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateOrLoad(
		context.Background(), unignored,
	); err == nil || !strings.Contains(err.Error(), "Git-ignored") {
		t.Fatalf("unignored identity was created: %v", err)
	}

	tracked := newGitRepository(t, filepath.Join(parent, "tracked"))
	if _, _, err := CreateOrLoad(context.Background(), tracked); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		"git", "-C", tracked, "add", "--force", "--", IdentityRelativePath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git add identity: %v: %s", err, output)
	}
	if _, err := Load(
		context.Background(), tracked,
	); err == nil || !strings.Contains(err.Error(), "must not be tracked") {
		t.Fatalf("tracked identity was loaded: %v", err)
	}
}

func TestRepositoryMustBeExactRealGitRoot(t *testing.T) {
	repository := newGitRepository(t, filepath.Join(t.TempDir(), "repository"))
	nested := filepath.Join(repository, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateOrLoad(
		context.Background(), nested,
	); err == nil || !strings.Contains(err.Error(), "exact Git worktree root") {
		t.Fatalf("nested repository path was accepted: %v", err)
	}

	if runtime.GOOS != "windows" {
		alias := filepath.Join(t.TempDir(), "repository-link")
		if err := os.Symlink(repository, alias); err != nil {
			t.Fatal(err)
		}
		if _, _, err := CreateOrLoad(
			context.Background(), alias,
		); err == nil || !strings.Contains(err.Error(), "real directory") {
			t.Fatalf("symlink repository root was accepted: %v", err)
		}
	}

	nonGit := t.TempDir()
	if _, _, err := CreateOrLoad(
		context.Background(), nonGit,
	); err == nil || !strings.Contains(err.Error(), "not a Git worktree") {
		t.Fatalf("non-Git root was accepted: %v", err)
	}
}

func createdIdentityPath(t *testing.T) (string, string) {
	t.Helper()
	repository := newGitRepository(t, filepath.Join(t.TempDir(), "repository"))
	if _, _, err := CreateOrLoad(context.Background(), repository); err != nil {
		t.Fatal(err)
	}
	return repository, filepath.Join(
		repository, filepath.FromSlash(IdentityRelativePath),
	)
}

func newGitRepository(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "-C", path, "init", "--quiet")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	excludePath := filepath.Join(path, ".git", "info", "exclude")
	file, err := os.OpenFile(excludePath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n/.gatemole/runtime.json\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("random unavailable")
}

func TestIdentityJSONContainsOnlyNonPathFields(t *testing.T) {
	identity := Identity{
		Version:   IdentityVersion,
		RuntimeID: runtimeIDPrefix + strings.Repeat("a", runtimeIDBytes*2),
		CreatedAt: time.Date(2026, time.July, 29, 0, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 ||
		fields["version"] == nil ||
		fields["runtime_id"] == nil ||
		fields["created_at"] == nil {
		t.Fatalf("identity JSON fields changed: %s", data)
	}
}
