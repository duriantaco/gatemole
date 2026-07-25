package capability

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
)

func TestCompileCreatesStableBoundedGrants(t *testing.T) {
	contract := readFixture[model.ExecutionContract](t, "execution_contract.json")
	run := readFixture[model.AgentRun](t, "agent_run.json")
	issuedAt := time.Date(2026, 7, 23, 0, 10, 0, 0, time.UTC)
	first, err := Compile(contract, run, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compile(contract, run, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != len(first) {
		t.Fatalf("grant counts = %d and %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID || first[i].PolicyDecisionID != second[i].PolicyDecisionID {
			t.Fatalf("grant identity is unstable: %#v %#v", first[i], second[i])
		}
		if !first[i].ExpiresAt.Equal(issuedAt.Add(defaultGrantTTL)) {
			t.Fatalf("expiry=%s, want default TTL boundary %s", first[i].ExpiresAt, issuedAt.Add(defaultGrantTTL))
		}
		if first[i].SubjectRunID != run.ID {
			t.Fatalf("subject=%s, want %s", first[i].SubjectRunID, run.ID)
		}
	}
}

func TestCompileRejectsDigestMismatchExpiredAndEscapingScope(t *testing.T) {
	issuedAt := time.Date(2026, 7, 23, 0, 10, 0, 0, time.UTC)
	tests := []struct {
		name string
		edit func(*model.ExecutionContract, *model.AgentRun)
		code model.ErrorCode
	}{
		{
			name: "digest mismatch",
			edit: func(_ *model.ExecutionContract, run *model.AgentRun) {
				run.ContractDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			code: model.ErrorCapabilityDenied,
		},
		{
			name: "expired",
			edit: func(contract *model.ExecutionContract, _ *model.AgentRun) {
				deadline := issuedAt
				contract.Deadline = &deadline
			},
			code: model.ErrorCapabilityExpired,
		},
		{
			name: "escaping workspace",
			edit: func(contract *model.ExecutionContract, _ *model.AgentRun) {
				contract.Resources[0].Conditions.WorkspaceRoot = "../workspace"
			},
			code: model.ErrorSchemaInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contract := readFixture[model.ExecutionContract](t, "execution_contract.json")
			run := readFixture[model.AgentRun](t, "agent_run.json")
			test.edit(&contract, &run)
			_, err := Compile(contract, run, issuedAt)
			assertCode(t, err, test.code)
		})
	}
}

func TestResourceMatchingAndWorkspaceRelative(t *testing.T) {
	for _, target := range []string{"workspace/file.txt", "workspace/sub/file.txt"} {
		if !MatchResource("workspace/**", target) {
			t.Fatalf("workspace selector did not match %q", target)
		}
	}
	for _, target := range []string{"outside/file.txt", "workspace/../outside", "/tmp/file", `workspace\file`} {
		if MatchResource("workspace/**", target) {
			t.Fatalf("workspace selector matched unsafe target %q", target)
		}
	}
	if relative, ok := WorkspaceRelative("workspace", "workspace/sub/file.txt"); !ok || relative != "sub/file.txt" {
		t.Fatalf("relative=%q ok=%v", relative, ok)
	}
}

func TestGrantsFromEventsCountsAuthorizations(t *testing.T) {
	contract := readFixture[model.ExecutionContract](t, "execution_contract.json")
	run := readFixture[model.AgentRun](t, "agent_run.json")
	grants, err := Compile(contract, run, time.Date(2026, 7, 23, 0, 10, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	grantPayload, _ := json.Marshal(model.CapabilitiesGrantedPayload{ContractDigest: contract.Digest, Grants: grants})
	decisionPayload, _ := json.Marshal(model.ActionDecisionPayload{
		ActionID:     "action:test",
		Decision:     model.DecisionAllow,
		CapabilityID: grants[0].ID,
		Reason:       "matched",
	})
	events := []model.RunEvent{
		{ID: "event:grants", Type: reducer.EventCapabilitiesGranted, Payload: grantPayload},
		{ID: "event:authorized", Type: reducer.EventActionAuthorized, Payload: decisionPayload},
	}
	replayed, uses, err := GrantsFromEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 2 || uses[grants[0].ID] != 1 {
		t.Fatalf("grants=%d uses=%v", len(replayed), uses)
	}
}

func readFixture[T any](t *testing.T, name string) T {
	t.Helper()
	path := filepath.Join("..", "..", "..", "schemas", "fixtures", "kernel", "valid", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value, err := model.DecodeStrict[T](data)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func assertCode(t *testing.T, err error, want model.ErrorCode) {
	t.Helper()
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) || kernelErr.Code != want {
		t.Fatalf("error=%v, want %s", err, want)
	}
}
