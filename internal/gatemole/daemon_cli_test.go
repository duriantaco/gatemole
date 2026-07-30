package gatemole

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryExecutablePathPreservesNamesAndResolvesPaths(
	t *testing.T,
) {
	t.Parallel()
	repository := filepath.Join(string(filepath.Separator), "srv", "gatemole-repo")
	absolute := filepath.Join(
		string(filepath.Separator),
		"opt",
		"bin",
		"docker",
	)
	tests := map[string]struct {
		input string
		want  string
	}{
		"PATH lookup": {
			input: "docker",
			want:  "docker",
		},
		"repository relative": {
			input: filepath.Join(".", "bin", "runtime"),
			want:  filepath.Join(repository, "bin", "runtime"),
		},
		"absolute": {
			input: absolute,
			want:  absolute,
		},
		"empty": {
			input: "",
			want:  "",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := repositoryExecutablePath(
				repository,
				test.input,
			); got != test.want {
				t.Fatalf(
					"repositoryExecutablePath(%q)=%q, want %q",
					test.input,
					got,
					test.want,
				)
			}
		})
	}
}

func TestDaemonTransactionRootUsesSecureDefaultAndRepositoryRelativeCustomPath(
	t *testing.T,
) {
	runtimeDirectory := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDirectory)
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}

	root, err := daemonTransactionRoot(repository, "", false)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRuntimeDirectory, err := filepath.EvalSymlinks(runtimeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(root) != filepath.Join(
		canonicalRuntimeDirectory,
		"gatemole",
		"transactions",
	) {
		t.Fatalf("default transaction root is not per-user scoped: %q", root)
	}

	custom, err := daemonTransactionRoot(
		repository,
		filepath.Join("state", "transactions"),
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(repository, "state", "transactions"); custom != want {
		t.Fatalf("custom transaction root=%q, want %q", custom, want)
	}
}
