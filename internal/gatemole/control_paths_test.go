package gatemole

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoSuggestionsSkipControlStateNamespaces(t *testing.T) {
	repository := t.TempDir()
	for _, directory := range []string{
		"src",
		".gatemole/forged",
		".vouch/legacy",
	} {
		path := filepath.Join(repository, directory)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(path, "main.go"),
			[]byte("package example\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
	}

	suggestions := goSuggestions(repository)
	if len(suggestions) != 1 {
		t.Fatalf("suggestions=%#v, want only source directory", suggestions)
	}
	if len(suggestions[0].OwnedPaths) != 1 ||
		suggestions[0].OwnedPaths[0] != "src/**" {
		t.Fatalf("suggestion=%#v, want src/**", suggestions[0])
	}
}

func TestMainRejectsLegacyVouchControlStateBeforeWritingGatemoleState(t *testing.T) {
	for _, command := range [][]string{
		{"init"},
		{"runtime", "init"},
	} {
		t.Run(strings.Join(command, "_"), func(t *testing.T) {
			repository := t.TempDir()
			legacy := filepath.Join(repository, ".vouch")
			if err := os.Mkdir(legacy, 0o700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(legacy, "sentinel")
			if err := os.WriteFile(
				sentinel,
				[]byte("do not rewrite"),
				0o600,
			); err != nil {
				t.Fatal(err)
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			args := append([]string{"--repo", repository}, command...)
			code := Main(args, &stdout, &stderr)
			if code != 1 ||
				!strings.Contains(stderr.String(), "legacy Vouch control state") {
				t.Fatalf(
					"legacy state was not rejected: code=%d stdout=%q stderr=%q",
					code,
					stdout.String(),
					stderr.String(),
				)
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "do not rewrite" {
				t.Fatalf("legacy sentinel changed: data=%q err=%v", data, err)
			}
			if _, err := os.Lstat(
				filepath.Join(repository, ".gatemole"),
			); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Gatemole state was created after rejection: %v", err)
			}
		})
	}
}
