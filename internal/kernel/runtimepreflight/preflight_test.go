package runtimepreflight

import (
	"strings"
	"testing"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

func TestRequestValidatesExactRuntimeAndAgentBinding(t *testing.T) {
	t.Parallel()
	request := validRequest()
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	request.Agent.OCIImage =
		"registry.example.invalid/other@" + testDigest("b")
	if err := request.Validate(); err != nil {
		t.Fatalf("equivalent full image reference was rejected: %v", err)
	}
	request.Agent.OCIImage =
		"registry.example.invalid/agent@" + testDigest("c")
	if err := request.Validate(); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched image digest was accepted: %v", err)
	}
}

func TestRequestRejectsMalformedInputs(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*Request){
		"version": func(request *Request) {
			request.Version = "gatemole.runtime_preflight_request.v1"
		},
		"runtime": func(request *Request) {
			request.ExpectedRuntimeID = "runtime:short"
		},
		"enforcement profile": func(request *Request) {
			request.RequiredEnforcementProfile = "Production"
		},
		"profile": func(request *Request) {
			request.Agent.Profile.ID = "not valid!"
		},
		"runtime class": func(request *Request) {
			request.Agent.Profile.RuntimeClass = "host"
		},
		"mutable image": func(request *Request) {
			request.Agent.OCIImage = "registry.example.invalid/agent:latest"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := validRequest()
			mutate(&request)
			if err := request.Validate(); err == nil {
				t.Fatal("malformed preflight request was accepted")
			}
		})
	}
}

func TestIdentityOnlyPreflightRequestIsValid(t *testing.T) {
	t.Parallel()
	request := validRequest()
	request.Agent = nil
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
}

func validRequest() Request {
	imageDigest := testDigest("b")
	return Request{
		Version:                    RequestVersion,
		ExpectedRuntimeID:          "runtime:" + strings.Repeat("a", 64),
		RequiredEnforcementProfile: "development",
		Agent: &AgentSelection{
			Profile: model.AgentTaskProfileBinding{
				ID:            "agent-profile:test",
				Digest:        testDigest("a"),
				RuntimeClass:  "oci",
				ImageDigest:   imageDigest,
				Entrypoint:    "/opt/agent",
				CommandDigest: testDigest("c"),
			},
			OCIImage: "registry.example.invalid/agent@" + imageDigest,
		},
	}
}

func testDigest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}
