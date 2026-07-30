package vouch

import (
	"os"
	"path/filepath"
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
