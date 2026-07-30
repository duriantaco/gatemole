package vouch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/sandbox"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

const (
	agentProfilesVersion  = "gatemole.agent_profiles.v0"
	defaultAgentProfiles  = ".vouch/agent-profiles.json"
	maxAgentProfilesBytes = 1 << 20
	maxAgentProfiles      = 128
)

// agentProfileDocument is a strict, repo-owned selection layer around the
// portable AgentImage descriptor. OCIImage supplies the complete pullable
// reference that AgentImage intentionally does not encode.
type agentProfileDocument struct {
	Version  string              `json:"version"`
	Profiles []agentProfileEntry `json:"profiles"`
}

type agentProfileEntry struct {
	Name       string           `json:"name"`
	OCIImage   string           `json:"oci_image"`
	Descriptor model.AgentImage `json:"descriptor"`
}

type resolvedAgentProfile struct {
	ID           string
	Digest       string
	RuntimeClass string
	OCIImage     string
	ImageDigest  string
	Entrypoint   string
	Command      []string
}

func resolveNamedAgentProfile(
	repo, profilesPath, name string,
	additionalArguments []string,
) (resolvedAgentProfile, error) {
	if profilesPath == "" {
		profilesPath = defaultAgentProfiles
	}
	if !filepath.IsAbs(profilesPath) {
		profilesPath = filepath.Join(repo, profilesPath)
	}
	data, err := readAgentProfilesFile(profilesPath)
	if err != nil {
		return resolvedAgentProfile{}, err
	}
	document, err := decodeAgentProfileDocument(data)
	if err != nil {
		return resolvedAgentProfile{}, err
	}
	var selected *agentProfileEntry
	for index := range document.Profiles {
		profile := &document.Profiles[index]
		if profile.Name == name {
			selected = profile
		}
	}
	if selected == nil {
		return resolvedAgentProfile{}, fmt.Errorf(
			"agent profile %q was not found in %s", name, profilesPath,
		)
	}
	digest, err := canonicalAgentProfileDigest(*selected)
	if err != nil {
		return resolvedAgentProfile{}, err
	}
	imageDigest, err := sandbox.ImageDigest(selected.OCIImage)
	if err != nil {
		return resolvedAgentProfile{}, err
	}
	command := append([]string(nil), selected.Descriptor.Runtime.Entrypoint...)
	command = append(command, additionalArguments...)
	return resolvedAgentProfile{
		ID:           selected.Name,
		Digest:       digest,
		RuntimeClass: "oci",
		OCIImage:     selected.OCIImage,
		ImageDigest:  imageDigest,
		Entrypoint:   selected.Descriptor.Runtime.Entrypoint[0],
		Command:      command,
	}, nil
}

func decodeAgentProfileDocument(data []byte) (agentProfileDocument, error) {
	document, err := model.DecodeStrict[agentProfileDocument](data)
	if err != nil {
		return agentProfileDocument{}, fmt.Errorf("decode agent profiles: %w", err)
	}
	if err := validateAgentProfileDocument(document); err != nil {
		return agentProfileDocument{}, err
	}
	return document, nil
}

func validateAgentProfileDocument(document agentProfileDocument) error {
	if document.Version != agentProfilesVersion {
		return fmt.Errorf("agent profiles version must be %q", agentProfilesVersion)
	}
	if len(document.Profiles) == 0 || len(document.Profiles) > maxAgentProfiles {
		return fmt.Errorf(
			"agent profiles must contain between 1 and %d profiles", maxAgentProfiles,
		)
	}
	seen := make(map[string]struct{}, len(document.Profiles))
	for index, profile := range document.Profiles {
		if !model.IsIdentifier(profile.Name) {
			return fmt.Errorf("agent profile %d has an invalid name", index)
		}
		if _, exists := seen[profile.Name]; exists {
			return fmt.Errorf("agent profile name %q is duplicated", profile.Name)
		}
		seen[profile.Name] = struct{}{}
		if err := validateAgentProfile(profile); err != nil {
			return fmt.Errorf("agent profile %q: %w", profile.Name, err)
		}
	}
	return nil
}

func validateAgentProfile(profile agentProfileEntry) error {
	if err := profile.Descriptor.Validate(); err != nil {
		return fmt.Errorf("invalid AgentImage descriptor: %w", err)
	}
	if profile.Descriptor.Runtime.Adapter != "oci" {
		return fmt.Errorf("AgentImage runtime adapter must be %q", "oci")
	}
	if !strings.Contains(profile.OCIImage, "@") {
		return errors.New("oci_image must be a complete digest-pinned reference")
	}
	imageDigest, err := sandbox.ImageDigest(profile.OCIImage)
	if err != nil {
		return fmt.Errorf("invalid oci_image: %w", err)
	}
	if imageDigest != profile.Descriptor.Digest {
		return errors.New("oci_image digest does not match AgentImage descriptor digest")
	}
	return nil
}

func canonicalAgentProfileDigest(profile agentProfileEntry) (string, error) {
	data, err := json.Marshal(profile)
	if err != nil {
		return "", fmt.Errorf("encode canonical agent profile: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func resolveAdHocAgentProfile(
	runtimeClass, image string,
	command []string,
) (resolvedAgentProfile, error) {
	commandDigest, err := transactionreducer.ComputeCommandDigest(command)
	if err != nil {
		return resolvedAgentProfile{}, err
	}
	imageDigest := ""
	if runtimeClass == "oci" {
		imageDigest, err = sandbox.ImageDigest(image)
		if err != nil {
			return resolvedAgentProfile{}, err
		}
	}
	input := struct {
		Version       string   `json:"version"`
		RuntimeClass  string   `json:"runtime_class"`
		OCIImage      string   `json:"oci_image,omitempty"`
		ImageDigest   string   `json:"image_digest,omitempty"`
		Command       []string `json:"command"`
		CommandDigest string   `json:"command_digest"`
	}{
		Version:       "gatemole.ad_hoc_agent_profile.v0",
		RuntimeClass:  runtimeClass,
		OCIImage:      image,
		ImageDigest:   imageDigest,
		Command:       append([]string(nil), command...),
		CommandDigest: commandDigest,
	}
	data, err := json.Marshal(input)
	if err != nil {
		return resolvedAgentProfile{}, fmt.Errorf("encode ad-hoc agent profile: %w", err)
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	return resolvedAgentProfile{
		ID:           "agent-profile:adhoc:" + hex.EncodeToString(sum[:12]),
		Digest:       digest,
		RuntimeClass: runtimeClass,
		OCIImage:     image,
		ImageDigest:  imageDigest,
		Command:      append([]string(nil), command...),
	}, nil
}

func (profile resolvedAgentProfile) binding() (model.AgentTaskProfileBinding, error) {
	commandDigest, err := transactionreducer.ComputeCommandDigest(profile.Command)
	if err != nil {
		return model.AgentTaskProfileBinding{}, err
	}
	return model.AgentTaskProfileBinding{
		ID:            profile.ID,
		Digest:        profile.Digest,
		RuntimeClass:  profile.RuntimeClass,
		ImageDigest:   profile.ImageDigest,
		Entrypoint:    profile.Entrypoint,
		CommandDigest: commandDigest,
	}, nil
}

func readAgentProfilesFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("open agent profiles: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("agent profiles must be a regular file: %s", path)
	}
	if info.Size() > maxAgentProfilesBytes {
		return nil, fmt.Errorf(
			"agent profiles exceed %d bytes", maxAgentProfilesBytes,
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open agent profiles: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAgentProfilesBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read agent profiles: %w", err)
	}
	if len(data) > maxAgentProfilesBytes {
		return nil, fmt.Errorf(
			"agent profiles exceed %d bytes", maxAgentProfilesBytes,
		)
	}
	return data, nil
}
