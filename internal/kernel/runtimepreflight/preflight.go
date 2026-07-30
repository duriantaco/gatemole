// Package runtimepreflight defines the versioned, non-authoritative Runtime
// diagnostic contract shared by vouchd and local clients. Successful preflight
// never grants authority; admission and launch independently recheck policy.
package runtimepreflight

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/runtimeidentity"
	"github.com/duriantaco/vouch/internal/kernel/sandbox"
)

const (
	RequestVersion = "vouch.runtime_preflight_request.v0"
	ResultVersion  = "vouch.runtime_preflight.v0"
)

type AgentSelection struct {
	Profile  model.AgentTaskProfileBinding `json:"profile"`
	OCIImage string                        `json:"oci_image"`
}

type Request struct {
	Version                    string          `json:"version"`
	ExpectedRuntimeID          string          `json:"expected_runtime_id"`
	RequiredEnforcementProfile string          `json:"required_enforcement_profile,omitempty"`
	Agent                      *AgentSelection `json:"agent,omitempty"`
}

type Result struct {
	Version            string `json:"version"`
	RuntimeID          string `json:"runtime_id"`
	EnforcementProfile string `json:"enforcement_profile"`
	Ready              bool   `json:"ready"`
	AgentProfileID     string `json:"agent_profile_id,omitempty"`
	ImageDigest        string `json:"image_digest,omitempty"`
}

func (request Request) Validate() error {
	if request.Version != RequestVersion {
		return fmt.Errorf(
			"Runtime preflight version must be %q",
			RequestVersion,
		)
	}
	if !runtimeidentity.IsRuntimeID(request.ExpectedRuntimeID) {
		return errors.New("Runtime preflight expected_runtime_id is invalid")
	}
	if request.RequiredEnforcementProfile != "" &&
		!model.IsEnforcementProfile(
			request.RequiredEnforcementProfile,
		) {
		return errors.New(
			"Runtime preflight required_enforcement_profile is invalid",
		)
	}
	if request.Agent == nil {
		return nil
	}
	return request.Agent.Validate()
}

func (selection AgentSelection) Validate() error {
	profile := selection.Profile
	if !model.IsIdentifier(profile.ID) {
		return errors.New("Runtime preflight agent profile ID is invalid")
	}
	for label, digest := range map[string]string{
		"profile digest": profile.Digest,
		"command digest": profile.CommandDigest,
		"image digest":   profile.ImageDigest,
	} {
		if !model.IsSHA256Digest(digest) {
			return fmt.Errorf("Runtime preflight %s is invalid", label)
		}
	}
	if profile.RuntimeClass != "oci" {
		return errors.New("Runtime preflight supports only OCI agent profiles")
	}
	if profile.Entrypoint != "" &&
		(!utf8.ValidString(profile.Entrypoint) ||
			strings.TrimSpace(profile.Entrypoint) == "" ||
			strings.ContainsRune(profile.Entrypoint, 0)) {
		return errors.New("Runtime preflight agent entrypoint is invalid")
	}
	imageDigest, err := sandbox.ImageDigest(selection.OCIImage)
	if err != nil {
		return fmt.Errorf(
			"Runtime preflight OCI image must be digest pinned: %w",
			err,
		)
	}
	if imageDigest != profile.ImageDigest {
		return errors.New(
			"Runtime preflight OCI image digest does not match the agent profile",
		)
	}
	return nil
}

func (result Result) Validate() error {
	if result.Version != ResultVersion ||
		!runtimeidentity.IsRuntimeID(result.RuntimeID) ||
		(result.EnforcementProfile != "development" &&
			result.EnforcementProfile != "production") ||
		!result.Ready {
		return errors.New("Runtime preflight result is invalid")
	}
	if result.AgentProfileID == "" && result.ImageDigest == "" {
		return nil
	}
	if !model.IsIdentifier(result.AgentProfileID) ||
		!model.IsSHA256Digest(result.ImageDigest) {
		return errors.New("Runtime preflight agent result is invalid")
	}
	return nil
}
