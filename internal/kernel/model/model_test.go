package model

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestValidKernelFixturesDecodeStrictlyAndValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		file     string
		validate func([]byte) error
	}{
		{"agent_image.json", decodeAndValidate[AgentImage]},
		{"execution_contract.json", decodeAndValidate[ExecutionContract]},
		{"agent_run.json", decodeAndValidate[AgentRun]},
		{"capability_grant.json", decodeAndValidate[CapabilityGrant]},
		{"action_request.json", decodeAndValidate[ActionRequest]},
		{"run_event.json", decodeAndValidate[RunEvent]},
		{"checkpoint.json", decodeAndValidate[Checkpoint]},
		{"policy_decision.json", decodeAndValidate[PolicyDecision]},
		{"agent_task.json", decodeAndValidate[AgentTask]},
		{"agent_transaction.json", decodeAndValidate[AgentTransaction]},
		{"effect.json", decodeAndValidate[Effect]},
		{"verification_result.json", decodeAndValidate[VerificationResult]},
		{"approval_package.json", decodeAndValidate[ApprovalPackage]},
		{"approval_decision.json", decodeAndValidate[ApprovalDecision]},
		{"commit_plan.json", decodeAndValidate[CommitPlan]},
		{"transaction_event.json", decodeAndValidate[TransactionEvent]},
		{"agent_execution.json", decodeAndValidate[AgentExecution]},
	}
	for _, test := range tests {
		test := test
		t.Run(test.file, func(t *testing.T) {
			t.Parallel()
			data := readFixture(t, "valid", test.file)
			if err := test.validate(data); err != nil {
				t.Fatalf("fixture did not validate: %v", err)
			}
		})
	}
}

func TestStrictDecodeRejectsUnknownKernelFields(t *testing.T) {
	t.Parallel()
	data := readFixture(t, "invalid", "agent_run_unknown_field.json")
	_, err := DecodeStrict[AgentRun](data)
	assertKernelCode(t, err, ErrorUnknownField)
}

func TestStrictDecodeRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	data := append(readFixture(t, "valid", "agent_run.json"), []byte(` {}`)...)
	_, err := DecodeStrict[AgentRun](data)
	assertKernelCode(t, err, ErrorSchemaInvalid)
}

func TestRunEventRequiresPreviousDigestAfterCreation(t *testing.T) {
	t.Parallel()
	data := readFixture(t, "invalid", "run_event_missing_previous_digest.json")
	event, err := DecodeStrict[RunEvent](data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	err = event.Validate()
	assertKernelCode(t, err, ErrorEventChain)
}

func TestAgentRunBudgetLimitsFailClosed(t *testing.T) {
	t.Parallel()
	data := readFixture(t, "valid", "agent_run.json")
	run, err := DecodeStrict[AgentRun](data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	limit := int64(1)
	run.BudgetLimits.MaxToolCalls = &limit
	run.BudgetUsage.ToolCalls = 2
	err = run.Validate()
	assertKernelCode(t, err, ErrorBudgetExceeded)
}

func TestTerminalRunRequiresCompletionTimestamp(t *testing.T) {
	t.Parallel()
	data := readFixture(t, "valid", "agent_run.json")
	run, err := DecodeStrict[AgentRun](data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	run.State = RunCompleted
	err = run.Validate()
	assertKernelCode(t, err, ErrorSchemaInvalid)
}

func TestCapabilityGrantRejectsExhaustedOveruseAndInvalidTime(t *testing.T) {
	t.Parallel()
	data := readFixture(t, "valid", "capability_grant.json")
	grant, err := DecodeStrict[CapabilityGrant](data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	maxUses := int64(1)
	grant.MaxUses = &maxUses
	grant.Uses = 2
	assertKernelCode(t, grant.Validate(), ErrorSchemaInvalid)

	grant.Uses = 0
	grant.ExpiresAt = grant.IssuedAt
	assertKernelCode(t, grant.Validate(), ErrorSchemaInvalid)
}

func TestDriverDefinedObjectsMustStillBeSingleJSONObjects(t *testing.T) {
	t.Parallel()
	data := readFixture(t, "valid", "action_request.json")
	request, err := DecodeStrict[ActionRequest](data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	request.Arguments = json.RawMessage(`[]`)
	assertKernelCode(t, request.Validate(), ErrorSchemaInvalid)

	request.Arguments = json.RawMessage(`{} {}`)
	assertKernelCode(t, request.Validate(), ErrorSchemaInvalid)

	request.Arguments = json.RawMessage(`{} trailing`)
	assertKernelCode(t, request.Validate(), ErrorSchemaInvalid)
}

func TestKernelResourceSchemasDeclareStrictVersionedObjects(t *testing.T) {
	t.Parallel()
	expected := map[string]string{
		"gatemole.agent_image.v0.schema.json":         AgentImageVersion,
		"gatemole.execution_contract.v0.schema.json":  ExecutionContractVersion,
		"gatemole.agent_run.v0.schema.json":           AgentRunVersion,
		"gatemole.capability_grant.v0.schema.json":    CapabilityGrantVersion,
		"gatemole.action_request.v0.schema.json":      ActionRequestVersion,
		"gatemole.run_event.v0.schema.json":           RunEventVersion,
		"gatemole.checkpoint.v0.schema.json":          CheckpointVersion,
		"gatemole.policy_decision.v0.schema.json":     PolicyDecisionVersion,
		"gatemole.agent_task.v0.schema.json":          AgentTaskVersion,
		"gatemole.agent_transaction.v0.schema.json":   AgentTransactionVersion,
		"gatemole.effect.v0.schema.json":              EffectVersion,
		"gatemole.verification_result.v0.schema.json": VerificationResultVersion,
		"gatemole.approval_package.v0.schema.json":    ApprovalPackageVersion,
		"gatemole.approval_decision.v0.schema.json":   ApprovalDecisionVersion,
		"gatemole.commit_plan.v0.schema.json":         CommitPlanVersion,
		"gatemole.transaction_event.v0.schema.json":   TransactionEventVersion,
		"gatemole.agent_execution.v0.schema.json":     AgentExecutionVersion,
	}
	files, err := filepath.Glob(filepath.Join(schemaRoot(t), "gatemole.*.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	seen := map[string]bool{}
	for _, file := range files {
		base := filepath.Base(file)
		wantVersion, isResource := expected[base]
		if !isResource {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("parse %s: %v", base, err)
		}
		if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s: wrong JSON Schema dialect", base)
		}
		if schema["additionalProperties"] != false {
			t.Errorf("%s: public resource must reject unknown fields", base)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s: missing properties", base)
			continue
		}
		version, ok := properties["version"].(map[string]any)
		if !ok || version["const"] != wantVersion {
			t.Errorf("%s: version const does not match %q", base, wantVersion)
		}
		seen[base] = true
	}
	for file := range expected {
		if !seen[file] {
			t.Errorf("missing resource schema %s", file)
		}
	}
}

type validatable interface {
	Validate() error
}

func decodeAndValidate[T validatable](data []byte) error {
	value, err := DecodeStrict[T](data)
	if err != nil {
		return err
	}
	return value.Validate()
}

func readFixture(t *testing.T, group, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(schemaRoot(t), "fixtures", "kernel", group, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func schemaRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "schemas"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func assertKernelCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected kernel error %s", want)
	}
	var kernelErr *KernelError
	if !errors.As(err, &kernelErr) {
		t.Fatalf("expected KernelError, got %T: %v", err, err)
	}
	if kernelErr.Code != want {
		t.Fatalf("expected code %s, got %s: %v", want, kernelErr.Code, err)
	}
	if strings.TrimSpace(kernelErr.Message) == "" {
		t.Fatal("kernel error message must not be empty")
	}
}
