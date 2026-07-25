package verification

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	profileImageA = "registry.example/verifier@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	profileImageB = "registry.example/verifier@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestLoadProfilesBuildsDeterministicImmutableLookup(t *testing.T) {
	first := loadProfilesJSON(t, `{
  "version": "vouch.verifier_profiles.v0",
  "profiles": [
    {
      "name": "zeta",
      "image": "`+profileImageB+`",
      "command": ["scan", "--mode", "strict"],
      "timeout_seconds": 600
    },
    {
      "name": "alpha",
      "image": "`+profileImageA+`",
      "command": ["go", "test", "./..."],
      "timeout_seconds": 300
    }
  ]
}`)
	second := loadProfilesJSON(t, `{"profiles":[
  {"timeout_seconds":300,"command":["go","test","./..."],"image":"`+profileImageA+`","name":"alpha"},
  {"command":["scan","--mode","strict"],"name":"zeta","timeout_seconds":600,"image":"`+profileImageB+`"}
],"version":"vouch.verifier_profiles.v0"}`)

	if first.Len() != 2 {
		t.Fatalf("Len()=%d, want 2", first.Len())
	}
	if first.Digest() == "" || !strings.HasPrefix(first.Digest(), "sha256:") {
		t.Fatalf("Digest()=%q, want sha256 digest", first.Digest())
	}
	if first.Digest() != "sha256:177a5fd132ecf3b7ed05601c8f4539cb2dd3ce56998853e1c3a68c166af5f4bc" {
		t.Fatalf("Digest()=%q, deterministic v0 digest changed", first.Digest())
	}
	if first.Digest() != second.Digest() {
		t.Fatalf(
			"set digest changed with JSON formatting or profile order: %q != %q",
			first.Digest(),
			second.Digest(),
		)
	}

	profiles := first.Profiles()
	if profiles[0].Name != "alpha" || profiles[1].Name != "zeta" {
		t.Fatalf("Profiles() order=%q,%q, want alpha,zeta", profiles[0].Name, profiles[1].Name)
	}
	alpha, found := first.Lookup("alpha")
	if !found {
		t.Fatal("Lookup(alpha) did not find the profile")
	}
	if alpha.Image != profileImageA ||
		alpha.ImageDigest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		alpha.TimeoutSeconds != 300 ||
		strings.Join(alpha.Command, "\x00") != "go\x00test\x00./..." ||
		alpha.Digest != "sha256:caf027da50b0027c9c7e3fa71c08acce6626f3e94429b8abec53bffb3365f2f6" {
		t.Fatalf("Lookup(alpha) returned unexpected profile: %#v", alpha)
	}
	secondAlpha, found := second.Lookup("alpha")
	if !found || secondAlpha.Digest != alpha.Digest {
		t.Fatalf("profile digest is not deterministic: %#v != %#v", alpha, secondAlpha)
	}
	if _, found := first.Lookup("missing"); found {
		t.Fatal("Lookup(missing) unexpectedly found a profile")
	}

	// Neither lookup surface may expose the set's internal command slices.
	alpha.Command[0] = "mutated"
	profiles[0].Command[0] = "also-mutated"
	alpha, _ = first.Lookup("alpha")
	if alpha.Command[0] != "go" {
		t.Fatalf("profile set was mutated through a returned command: %#v", alpha.Command)
	}
}

func TestProfileAndSetDigestsBindEveryProfileField(t *testing.T) {
	baseline := loadProfilesJSON(t, singleProfileDocument(
		"alpha",
		profileImageA,
		`["go","test","./..."]`,
		"300",
	))
	baselineProfile, _ := baseline.Lookup("alpha")

	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "name",
			content: singleProfileDocument("beta", profileImageA, `["go","test","./..."]`, "300"),
		},
		{
			name:    "image",
			content: singleProfileDocument("alpha", profileImageB, `["go","test","./..."]`, "300"),
		},
		{
			name:    "command",
			content: singleProfileDocument("alpha", profileImageA, `["go","test","./short"]`, "300"),
		},
		{
			name:    "timeout",
			content: singleProfileDocument("alpha", profileImageA, `["go","test","./..."]`, "301"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := loadProfilesJSON(t, test.content)
			profiles := changed.Profiles()
			if len(profiles) != 1 {
				t.Fatalf("Profiles()=%#v, want one profile", profiles)
			}
			if profiles[0].Digest == baselineProfile.Digest {
				t.Fatalf("%s change did not change profile digest %q", test.name, profiles[0].Digest)
			}
			if changed.Digest() == baseline.Digest() {
				t.Fatalf("%s change did not change set digest %q", test.name, changed.Digest())
			}
		})
	}
}

func TestLoadProfilesRejectsInvalidDocuments(t *testing.T) {
	validProfile := `{
  "name": "alpha",
  "image": "` + profileImageA + `",
  "command": ["go", "test", "./..."],
  "timeout_seconds": 300
}`
	document := func(profile string) string {
		return `{"version":"vouch.verifier_profiles.v0","profiles":[` + profile + `]}`
	}
	tests := []struct {
		name    string
		content string
	}{
		{name: "malformed", content: `{"version":`},
		{name: "invalid utf8", content: document(validProfile) + string([]byte{0xff})},
		{name: "unknown document field", content: `{"version":"vouch.verifier_profiles.v0","profiles":[` + validProfile + `],"extra":true}`},
		{name: "wrong document field case", content: `{"Version":"vouch.verifier_profiles.v0","profiles":[` + validProfile + `]}`},
		{name: "unknown profile field", content: document(`{
			"name":"alpha","image":"` + profileImageA + `","command":["go"],"timeout_seconds":300,"extra":true
		}`)},
		{name: "wrong profile field case", content: document(`{
			"Name":"alpha","image":"` + profileImageA + `","command":["go"],"timeout_seconds":300
		}`)},
		{name: "trailing value", content: document(validProfile) + `{}`},
		{name: "duplicate document key", content: `{
			"version":"vouch.verifier_profiles.v0",
			"version":"vouch.verifier_profiles.v0",
			"profiles":[` + validProfile + `]
		}`},
		{name: "duplicate profile key", content: document(`{
			"name":"alpha","name":"beta","image":"` + profileImageA + `","command":["go"],"timeout_seconds":300
		}`)},
		{name: "wrong version", content: `{"version":"vouch.verifier_profiles.v1","profiles":[` + validProfile + `]}`},
		{name: "empty profiles", content: `{"version":"vouch.verifier_profiles.v0","profiles":[]}`},
		{name: "missing profiles", content: `{"version":"vouch.verifier_profiles.v0"}`},
		{name: "invalid name", content: document(`{
			"name":"bad name","image":"` + profileImageA + `","command":["go"],"timeout_seconds":300
		}`)},
		{name: "duplicate name", content: `{
			"version":"vouch.verifier_profiles.v0",
			"profiles":[` + validProfile + `,` + validProfile + `]
		}`},
		{name: "tagged image", content: document(`{
			"name":"alpha","image":"registry.example/verifier:latest","command":["go"],"timeout_seconds":300
		}`)},
		{name: "empty command", content: document(`{
			"name":"alpha","image":"` + profileImageA + `","command":[],"timeout_seconds":300
		}`)},
		{name: "empty command element", content: document(`{
			"name":"alpha","image":"` + profileImageA + `","command":["go",""],"timeout_seconds":300
		}`)},
		{name: "nul command element", content: document(`{
			"name":"alpha","image":"` + profileImageA + `","command":["go","\u0000"],"timeout_seconds":300
		}`)},
		{name: "timeout below bound", content: document(`{
			"name":"alpha","image":"` + profileImageA + `","command":["go"],"timeout_seconds":0
		}`)},
		{name: "timeout above bound", content: document(`{
			"name":"alpha","image":"` + profileImageA + `","command":["go"],"timeout_seconds":3601
		}`)},
		{name: "fractional timeout", content: document(`{
			"name":"alpha","image":"` + profileImageA + `","command":["go"],"timeout_seconds":1.5
		}`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeProfilesFile(t, test.content)
			if profiles, err := LoadProfiles(path); err == nil {
				t.Fatalf("LoadProfiles()=%#v, want error", profiles)
			}
		})
	}
}

func TestLoadProfilesAcceptsTimeoutBounds(t *testing.T) {
	for _, timeout := range []string{"1", "3600"} {
		t.Run(timeout, func(t *testing.T) {
			profiles := loadProfilesJSON(t, singleProfileDocument(
				"alpha",
				profileImageA,
				`["go"]`,
				timeout,
			))
			if profiles.Len() != 1 {
				t.Fatalf("Len()=%d, want 1", profiles.Len())
			}
		})
	}
}

func TestLoadProfilesRejectsNonRegularAndOversizedFiles(t *testing.T) {
	if profiles, err := LoadProfiles(t.TempDir()); err == nil {
		t.Fatalf("LoadProfiles(directory)=%#v, want error", profiles)
	}

	path := filepath.Join(t.TempDir(), "profiles.json")
	oversized := strings.Repeat(" ", int(MaxVerifierProfilesFileBytes)+1)
	if err := os.WriteFile(path, []byte(oversized), 0o600); err != nil {
		t.Fatal(err)
	}
	if profiles, err := LoadProfiles(path); err == nil {
		t.Fatalf("LoadProfiles(oversized)=%#v, want error", profiles)
	}
}

func singleProfileDocument(name, image, command, timeout string) string {
	return `{
  "version": "vouch.verifier_profiles.v0",
  "profiles": [{
    "name": "` + name + `",
    "image": "` + image + `",
    "command": ` + command + `,
    "timeout_seconds": ` + timeout + `
  }]
}`
}

func loadProfilesJSON(t *testing.T, content string) *ProfileSet {
	t.Helper()
	profiles, err := LoadProfiles(writeProfilesFile(t, content))
	if err != nil {
		t.Fatalf("LoadProfiles(): %v", err)
	}
	return profiles
}

func writeProfilesFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
