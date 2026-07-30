package verification

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/sandbox"
)

const (
	VerifierProfilesVersion = "gatemole.verifier_profiles.v0"

	MinVerifierProfileTimeoutSeconds int64 = 1
	MaxVerifierProfileTimeoutSeconds int64 = 3600
	MaxVerifierProfilesFileBytes     int64 = 2 << 20

	verifierProfileDigestVersion    = "gatemole.verifier_profile_digest.v0"
	verifierProfileSetDigestVersion = "gatemole.verifier_profile_set_digest.v0"
)

// Profile is one daemon-owned OCI verifier invocation. Command is an exact
// argv vector: it is never joined, split, or interpreted by a shell.
type Profile struct {
	Name           string
	Image          string
	ImageDigest    string
	Command        []string
	TimeoutSeconds int64
	Digest         string
}

// ProfileSet is an immutable, name-indexed collection loaded from one strict
// v0 profile document. Its digest is independent of document whitespace and
// profile order.
type ProfileSet struct {
	digest   string
	profiles []Profile
	byName   map[string]int
}

type profileDocument struct {
	Version  string         `json:"version"`
	Profiles []profileEntry `json:"profiles"`
}

type profileEntry struct {
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	Command        []string `json:"command"`
	TimeoutSeconds int64    `json:"timeout_seconds"`
}

type profileDigestInput struct {
	Version        string   `json:"version"`
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	ImageDigest    string   `json:"image_digest"`
	Command        []string `json:"command"`
	TimeoutSeconds int64    `json:"timeout_seconds"`
}

type profileSetDigestEntry struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

type profileSetDigestInput struct {
	Version  string                  `json:"version"`
	Profiles []profileSetDigestEntry `json:"profiles"`
}

// LoadProfiles loads, validates, and digests a production verifier profile
// document. Unknown fields, duplicate object keys, and trailing JSON values
// are rejected.
func LoadProfiles(path string) (*ProfileSet, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("verifier profiles path is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open verifier profiles: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat verifier profiles: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("verifier profiles must be a regular file")
	}
	if info.Size() > MaxVerifierProfilesFileBytes {
		return nil, fmt.Errorf(
			"verifier profiles must be no larger than %d bytes",
			MaxVerifierProfilesFileBytes,
		)
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxVerifierProfilesFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read verifier profiles: %w", err)
	}
	if int64(len(data)) > MaxVerifierProfilesFileBytes {
		return nil, fmt.Errorf(
			"verifier profiles must be no larger than %d bytes",
			MaxVerifierProfilesFileBytes,
		)
	}
	profiles, err := ParseProfiles(data)
	if err != nil {
		return nil, fmt.Errorf("decode verifier profiles: %w", err)
	}
	return profiles, nil
}

// ParseProfiles validates and digests one already-bounded verifier-profile
// snapshot. The returned set digest is derived from these exact parsed bytes.
func ParseProfiles(data []byte) (*ProfileSet, error) {
	if int64(len(data)) > MaxVerifierProfilesFileBytes {
		return nil, fmt.Errorf(
			"verifier profiles must be no larger than %d bytes",
			MaxVerifierProfilesFileBytes,
		)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("JSON must be valid UTF-8")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, err
	}

	var document profileDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return nil, err
	}
	return newProfileSet(document)
}

// Digest returns the deterministic digest of the complete profile set.
func (set *ProfileSet) Digest() string {
	if set == nil {
		return ""
	}
	return set.digest
}

// Len returns the number of verifier profiles in the set.
func (set *ProfileSet) Len() int {
	if set == nil {
		return 0
	}
	return len(set.profiles)
}

// Lookup returns a defensive copy of the named profile.
func (set *ProfileSet) Lookup(name string) (Profile, bool) {
	if set == nil {
		return Profile{}, false
	}
	index, exists := set.byName[name]
	if !exists {
		return Profile{}, false
	}
	return cloneProfile(set.profiles[index]), true
}

// Profiles returns defensive copies sorted by profile name.
func (set *ProfileSet) Profiles() []Profile {
	if set == nil {
		return nil
	}
	profiles := make([]Profile, len(set.profiles))
	for index := range set.profiles {
		profiles[index] = cloneProfile(set.profiles[index])
	}
	return profiles
}

func newProfileSet(document profileDocument) (*ProfileSet, error) {
	if document.Version != VerifierProfilesVersion {
		return nil, fmt.Errorf(
			"verifier profiles version must be %q",
			VerifierProfilesVersion,
		)
	}
	if len(document.Profiles) == 0 {
		return nil, errors.New("verifier profiles must contain at least one profile")
	}

	profiles := make([]Profile, 0, len(document.Profiles))
	seen := make(map[string]struct{}, len(document.Profiles))
	for index, entry := range document.Profiles {
		if !model.IsIdentifier(entry.Name) {
			return nil, fmt.Errorf(
				"verifier profile %d has invalid name %q",
				index,
				entry.Name,
			)
		}
		if _, duplicate := seen[entry.Name]; duplicate {
			return nil, fmt.Errorf("duplicate verifier profile name %q", entry.Name)
		}
		seen[entry.Name] = struct{}{}

		imageDigest, err := sandbox.ImageDigest(entry.Image)
		if err != nil {
			return nil, fmt.Errorf(
				"verifier profile %q image is invalid: %w",
				entry.Name,
				err,
			)
		}
		if err := validateProfileCommand(entry.Command); err != nil {
			return nil, fmt.Errorf(
				"verifier profile %q command is invalid: %w",
				entry.Name,
				err,
			)
		}
		if entry.TimeoutSeconds < MinVerifierProfileTimeoutSeconds ||
			entry.TimeoutSeconds > MaxVerifierProfileTimeoutSeconds {
			return nil, fmt.Errorf(
				"verifier profile %q timeout_seconds must be between %d and %d",
				entry.Name,
				MinVerifierProfileTimeoutSeconds,
				MaxVerifierProfileTimeoutSeconds,
			)
		}

		profile := Profile{
			Name:           entry.Name,
			Image:          entry.Image,
			ImageDigest:    imageDigest,
			Command:        append([]string(nil), entry.Command...),
			TimeoutSeconds: entry.TimeoutSeconds,
		}
		profile.Digest, err = computeProfileDigest(profile)
		if err != nil {
			return nil, fmt.Errorf(
				"digest verifier profile %q: %w",
				entry.Name,
				err,
			)
		}
		profiles = append(profiles, profile)
	}

	sort.Slice(profiles, func(left, right int) bool {
		return profiles[left].Name < profiles[right].Name
	})
	digest, err := computeProfileSetDigest(profiles)
	if err != nil {
		return nil, fmt.Errorf("digest verifier profile set: %w", err)
	}
	byName := make(map[string]int, len(profiles))
	for index, profile := range profiles {
		byName[profile.Name] = index
	}
	return &ProfileSet{
		digest:   digest,
		profiles: profiles,
		byName:   byName,
	}, nil
}

func validateProfileCommand(command []string) error {
	if len(command) == 0 {
		return errors.New("at least one exact argv element is required")
	}
	for index, argument := range command {
		if strings.TrimSpace(argument) == "" {
			return fmt.Errorf("argv element %d cannot be empty", index)
		}
		if strings.ContainsRune(argument, 0) {
			return fmt.Errorf("argv element %d cannot contain NUL", index)
		}
	}
	return nil
}

func computeProfileDigest(profile Profile) (string, error) {
	return digestProfileJSON(profileDigestInput{
		Version:        verifierProfileDigestVersion,
		Name:           profile.Name,
		Image:          profile.Image,
		ImageDigest:    profile.ImageDigest,
		Command:        append([]string(nil), profile.Command...),
		TimeoutSeconds: profile.TimeoutSeconds,
	})
}

func computeProfileSetDigest(profiles []Profile) (string, error) {
	entries := make([]profileSetDigestEntry, len(profiles))
	for index, profile := range profiles {
		entries[index] = profileSetDigestEntry{
			Name:   profile.Name,
			Digest: profile.Digest,
		}
	}
	return digestProfileJSON(profileSetDigestInput{
		Version:  verifierProfileSetDigestVersion,
		Profiles: entries,
	})
}

func digestProfileJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func cloneProfile(profile Profile) Profile {
	profile.Command = append([]string(nil), profile.Command...)
	return profile
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeUniqueJSONValue(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func consumeUniqueJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key at %s is not a string", path)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object key %q at %s", key, path)
			}
			if !allowedProfileDocumentKey(path, key) {
				return fmt.Errorf("unknown object key %q at %s", key, path)
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		if token != json.Delim('}') {
			return fmt.Errorf("object at %s is not closed", path)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := consumeUniqueJSONValue(
				decoder,
				fmt.Sprintf("%s[%d]", path, index),
			); err != nil {
				return err
			}
			index++
		}
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		if token != json.Delim(']') {
			return fmt.Errorf("array at %s is not closed", path)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, path)
	}
	return nil
}

func allowedProfileDocumentKey(path, key string) bool {
	if path == "$" {
		return key == "version" || key == "profiles"
	}
	if strings.HasPrefix(path, "$.profiles[") && strings.HasSuffix(path, "]") {
		switch key {
		case "name", "image", "command", "timeout_seconds":
			return true
		default:
			return false
		}
	}
	return true
}
