package authority_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/admission"
	"github.com/duriantaco/gatemole/internal/kernel/authority"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
)

func TestCompileDerivesContentBoundOCIPlan(t *testing.T) {
	input := validInput(t, workspaceResource(
		"filesystem.read", "filesystem.write",
	), model.BudgetLimits{})
	timeout := int64(120)
	input.CallerTimeoutSeconds = &timeout

	plan, err := authority.Compile(input)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.Namespace != input.Task.Namespace ||
		plan.TransactionID != input.Transaction.ID ||
		plan.StageBindingID != input.Transaction.StageBindings[0].ID ||
		plan.TaskDigest != input.Task.Digest ||
		plan.ContractDigest != input.Contract.Digest ||
		plan.RunID != input.Run.ID {
		t.Fatalf("plan identity is not derived from admission: %#v", plan)
	}
	if plan.ImageReference != input.ImageReference ||
		plan.ImageDigest != input.Task.AgentProfile.ImageDigest ||
		plan.CommandDigest != input.Task.AgentProfile.CommandDigest ||
		plan.Entrypoint != "agent" ||
		len(plan.Command) != 1 || plan.Command[0] != "--task" {
		t.Fatalf("plan executable binding = %#v", plan)
	}
	if plan.WorkspaceAccess != authority.WorkspaceReadWrite ||
		plan.NetworkAccess != authority.NetworkNone ||
		plan.ModelBroker != nil {
		t.Fatalf("plan isolation = %#v", plan)
	}
	if plan.TimeoutSeconds != timeout ||
		!plan.NotAfter.Equal(input.Now.Add(time.Duration(timeout)*time.Second)) {
		t.Fatalf("plan timeout = %d / %s", plan.TimeoutSeconds, plan.NotAfter)
	}
	input.Command[1] = "--mutated"
	input.Run.CapabilityIDs[0] = "cap:mutated"
	if plan.Command[0] != "--task" ||
		plan.CapabilityIDs[0] == "cap:mutated" {
		t.Fatal("plan aliases caller-owned slices")
	}
}

func TestCompileEnablesOnlyExplicitNarrowModelEgress(t *testing.T) {
	maxCalls, maxInput, maxOutput := int64(10), int64(2_000), int64(1_000)
	input := validInput(
		t,
		[]model.ContractResource{
			workspaceResource("filesystem.read", "filesystem.write")[0],
			{
				ID:         "model-openai",
				Selector:   model.ResourceSelector{Kind: authority.ModelResourceKind, Pattern: "openai/*"},
				Operations: []string{authority.ModelInvoke},
			},
		},
		model.BudgetLimits{
			MaxModelCalls:   &maxCalls,
			MaxInputTokens:  &maxInput,
			MaxOutputTokens: &maxOutput,
		},
	)
	input.Ceilings.ModelBroker = &authority.ModelBrokerCeiling{
		Provider: "openai", AllowedModels: []string{"gpt-4.1"},
		ImageDigest: testDigest("d"), PolicyDigest: testDigest("e"),
		MaxModelCalls: 5, MaxInputTokens: 1_000, MaxOutputTokens: 500,
		MaxOutputTokensPerRequest: 100,
	}

	plan, err := authority.Compile(input)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if plan.NetworkAccess != authority.NetworkModelBroker ||
		plan.ModelBroker == nil ||
		plan.ModelBroker.Provider != "openai" ||
		len(plan.ModelBroker.AllowedModels) != 1 ||
		plan.ModelBroker.AllowedModels[0] != "gpt-4.1" ||
		plan.ModelBroker.MaxModelCalls != 5 {
		t.Fatalf("model plan = %#v", plan.ModelBroker)
	}

	withoutGrant := validInput(
		t, workspaceResource("filesystem.read", "filesystem.write"), model.BudgetLimits{},
	)
	withoutGrant.Ceilings.ModelBroker = input.Ceilings.ModelBroker
	noEgress, err := authority.Compile(withoutGrant)
	if err != nil {
		t.Fatalf("Compile(no model grant) error = %v", err)
	}
	if noEgress.NetworkAccess != authority.NetworkNone || noEgress.ModelBroker != nil {
		t.Fatalf("model broker enabled without a grant: %#v", noEgress)
	}
}

func TestCompileRejectsModelAuthorityTheStaticBrokerCannotEnforce(t *testing.T) {
	modelResource := model.ContractResource{
		ID:         "model-openai",
		Selector:   model.ResourceSelector{Kind: authority.ModelResourceKind, Pattern: "openai/*"},
		Operations: []string{authority.ModelInvoke},
	}
	tests := []struct {
		name   string
		mutate func(*authority.CompileInput)
		code   model.ErrorCode
	}{
		{
			name: "broker unavailable",
			mutate: func(input *authority.CompileInput) {
				input.Ceilings.ModelBroker = nil
			},
			code: model.ErrorCapabilityDenied,
		},
		{
			name: "static call budget broader than contract",
			mutate: func(input *authority.CompileInput) {
				input.Ceilings.ModelBroker.MaxModelCalls = 6
			},
			code: model.ErrorCapabilityDenied,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			maxCalls := int64(5)
			input := validInput(
				t, []model.ContractResource{
					workspaceResource("filesystem.read", "filesystem.write")[0],
					modelResource,
				},
				model.BudgetLimits{MaxModelCalls: &maxCalls},
			)
			input.Ceilings.ModelBroker = &authority.ModelBrokerCeiling{
				Provider: "openai", AllowedModels: []string{"gpt-4.1"},
				ImageDigest: testDigest("d"), PolicyDigest: testDigest("e"),
				MaxModelCalls: 5, MaxInputTokens: 1_000,
				MaxOutputTokens: 500, MaxOutputTokensPerRequest: 100,
			}
			test.mutate(&input)
			_, err := authority.Compile(input)
			requireKernelCode(t, err, test.code)
		})
	}

	narrowed := validInput(
		t,
		[]model.ContractResource{
			workspaceResource("filesystem.read", "filesystem.write")[0],
			{
				ID: "model-openai",
				Selector: model.ResourceSelector{
					Kind: authority.ModelResourceKind, Pattern: "openai/gpt-4.1",
				},
				Operations: []string{authority.ModelInvoke},
			},
		},
		model.BudgetLimits{},
	)
	narrowed.Ceilings.ModelBroker = &authority.ModelBrokerCeiling{
		Provider: "openai", AllowedModels: []string{"gpt-4.1"},
		ImageDigest: testDigest("d"), PolicyDigest: testDigest("e"),
		MaxModelCalls: 5, MaxInputTokens: 1_000,
		MaxOutputTokens: 500, MaxOutputTokensPerRequest: 100,
	}
	_, err := authority.Compile(narrowed)
	requireKernelCode(t, err, model.ErrorCapabilityDenied)
}

func TestCompileRejectsUntrustedExecutableWidening(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*authority.CompileInput)
	}{
		{"different image digest", func(input *authority.CompileInput) {
			input.ImageReference = "registry.example/agent@" + testDigest("f")
		}},
		{"mutable image", func(input *authority.CompileInput) {
			input.ImageReference = "registry.example/agent:latest"
		}},
		{"different command", func(input *authority.CompileInput) {
			input.Command = []string{"agent", "--other"}
		}},
		{"different entrypoint", func(input *authority.CompileInput) {
			input.Command = []string{"other", "--task"}
		}},
		{"image outside daemon ceiling", func(input *authority.CompileInput) {
			input.Ceilings.AllowedImageDigests = []string{testDigest("f")}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validInput(t, workspaceResource("filesystem.read", "filesystem.write"), model.BudgetLimits{})
			test.mutate(&input)
			if _, err := authority.Compile(input); err == nil {
				t.Fatal("Compile() accepted widened executable material")
			}
		})
	}
}

func TestCompileRejectsBrokenLiveBindingsAndLifecycle(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*authority.CompileInput)
		code   model.ErrorCode
	}{
		{"contract content digest", func(input *authority.CompileInput) {
			input.Contract.Goal = "changed"
		}, model.ErrorEventChain},
		{"delegation lineage", func(input *authority.CompileInput) {
			input.Run.DelegationChain[0] = model.Principal{
				ID: "human:other", Kind: model.PrincipalHuman,
			}
		}, model.ErrorIdentityInvalid},
		{"run not admitted", func(input *authority.CompileInput) {
			input.Run.State = model.RunRunning
		}, model.ErrorTransitionInvalid},
		{"run has active execution", func(input *authority.CompileInput) {
			input.Run.State = model.RunRunning
			input.Run.ActiveExecutionID = "execution:active"
		}, model.ErrorTransitionInvalid},
		{"run already leased", func(input *authority.CompileInput) {
			input.Run.Runtime = &model.RuntimeBinding{
				Adapter: "oci", AdapterVersion: "1",
			}
		}, model.ErrorTransitionInvalid},
		{"unsupported parent run", func(input *authority.CompileInput) {
			input.Run.ParentRunID = "run:parent"
		}, model.ErrorTransitionInvalid},
		{"transaction not running", func(input *authority.CompileInput) {
			input.Transaction.State = model.TransactionCreated
		}, model.ErrorTransitionInvalid},
		{"transaction missing stage", func(input *authority.CompileInput) {
			input.Transaction.StageBindings = nil
		}, model.ErrorTransitionInvalid},
		{"prior execution", func(input *authority.CompileInput) {
			input.Executions = []model.AgentExecution{{ID: "execution:prior"}}
		}, model.ErrorTransitionInvalid},
		{"run deadline changed", func(input *authority.CompileInput) {
			changed := input.Run.Deadline.Add(-time.Second)
			input.Run.Deadline = &changed
		}, model.ErrorEventChain},
		{"grant resource changed", func(input *authority.CompileInput) {
			input.Grants[0].Resource.Pattern = "workspace/private/**"
		}, model.ErrorEventChain},
		{"grant subject changed", func(input *authority.CompileInput) {
			input.Grants[0].SubjectRunID = "run:other"
		}, model.ErrorEventChain},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validInput(
				t, workspaceResource("filesystem.read", "filesystem.write"), model.BudgetLimits{},
			)
			test.mutate(&input)
			_, err := authority.Compile(input)
			requireKernelCode(t, err, test.code)
		})
	}
}

func TestCompileAllowsRetryAfterTerminalExecutionReceipt(t *testing.T) {
	input := validInput(
		t,
		workspaceResource("filesystem.read", "filesystem.write"),
		model.BudgetLimits{},
	)
	exitCode := 7
	completedAt := input.Now.Add(-time.Second)
	input.Executions = []model.AgentExecution{{
		Version:             model.AgentExecutionVersion,
		ID:                  "execution:prior",
		TransactionID:       input.Transaction.ID,
		Attempt:             input.Transaction.Attempt,
		RunID:               input.Run.ID,
		StageBindingID:      input.Transaction.StageBindings[0].ID,
		Program:             "agent",
		CommandDigest:       input.Task.AgentProfile.CommandDigest,
		RuntimeClass:        "oci",
		RuntimeConfigDigest: testDigest("c"),
		ImageDigest:         input.Task.AgentProfile.ImageDigest,
		TaskDigest:          input.Task.Digest,
		Status:              model.AgentExecutionFailed,
		ExitCode:            &exitCode,
		StdoutDigest:        testDigest("d"),
		StderrDigest:        testDigest("e"),
		StartedAt:           input.Now.Add(-2 * time.Second),
		CompletedAt:         &completedAt,
	}}
	for _, state := range []model.RunState{
		model.RunWaitingForAgent,
		model.RunWaitingForEvent,
	} {
		candidate := input
		candidate.Run.State = state
		if _, err := authority.Compile(candidate); err != nil {
			t.Fatalf("terminal execution prevented retry from %s: %v", state, err)
		}
	}
}

func TestCompileRejectsInactiveGrantsAndTimeoutWidening(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*authority.CompileInput)
		code   model.ErrorCode
	}{
		{"revoked", func(input *authority.CompileInput) {
			revoked := input.Now
			input.Grants[0].RevokedAt = &revoked
		}, model.ErrorCapabilityRevoked},
		{"expired", func(input *authority.CompileInput) {
			input.Now = input.Grants[0].ExpiresAt
		}, model.ErrorCapabilityExpired},
		{"not active yet", func(input *authority.CompileInput) {
			input.Now = input.Grants[0].IssuedAt.Add(-time.Second)
		}, model.ErrorCapabilityDenied},
		{"caller widens timeout", func(input *authority.CompileInput) {
			timeout := int64(301)
			input.CallerTimeoutSeconds = &timeout
		}, model.ErrorCapabilityDenied},
		{"zero caller timeout", func(input *authority.CompileInput) {
			timeout := int64(0)
			input.CallerTimeoutSeconds = &timeout
		}, model.ErrorSchemaInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validInput(
				t, workspaceResource("filesystem.read", "filesystem.write"), model.BudgetLimits{},
			)
			test.mutate(&input)
			_, err := authority.Compile(input)
			requireKernelCode(t, err, test.code)
		})
	}
}

func TestCompileRejectsUnsupportedContractSurface(t *testing.T) {
	tests := []struct {
		name      string
		resources []model.ContractResource
		budgets   model.BudgetLimits
		mutate    func(*authority.CompileInput)
	}{
		{"write only", workspaceResource("filesystem.write"), model.BudgetLimits{}, nil},
		{"narrow subpath", []model.ContractResource{{
			ID: "workspace", Selector: model.ResourceSelector{
				Kind: "filesystem", Pattern: "workspace/private/**",
			}, Operations: []string{"filesystem.read"},
			Conditions: model.CapabilityConditions{WorkspaceRoot: "workspace"},
		}}, model.BudgetLimits{}, nil},
		{"data classes", []model.ContractResource{{
			ID: "workspace", Selector: model.ResourceSelector{
				Kind: "filesystem", Pattern: "workspace/**",
			}, Operations: []string{"filesystem.read"},
			Conditions: model.CapabilityConditions{
				WorkspaceRoot: "workspace", DataClassifications: []string{"secret"},
			},
		}}, model.BudgetLimits{}, nil},
		{"unsupported resource", []model.ContractResource{{
			ID: "email", Selector: model.ResourceSelector{
				Kind: "email", Pattern: "mailbox/**",
			}, Operations: []string{"email.send"},
		}}, model.BudgetLimits{}, nil},
		{"tool budget", workspaceResource("filesystem.read", "filesystem.write"), model.BudgetLimits{
			MaxToolCalls: int64Pointer(5),
		}, nil},
		{"cost budget", workspaceResource("filesystem.read", "filesystem.write"), model.BudgetLimits{
			MaxCostMicros: int64Pointer(5),
		}, nil},
		{"model budget without model grant", workspaceResource("filesystem.read", "filesystem.write"), model.BudgetLimits{
			MaxModelCalls: int64Pointer(5),
		}, nil},
		{"child delegation", workspaceResource("filesystem.read", "filesystem.write"), model.BudgetLimits{}, func(input *authority.CompileInput) {
			input.Contract.MaxChildren = int64Pointer(1)
			input.Contract.Digest = mustContractDigest(t, input.Contract)
			input.Run.ContractDigest = input.Contract.Digest
			input.Transaction.Admission.ContractDigest = input.Contract.Digest
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validInput(t, test.resources, test.budgets)
			if test.mutate != nil {
				test.mutate(&input)
			}
			_, err := authority.Compile(input)
			requireKernelCode(t, err, model.ErrorCapabilityDenied)
		})
	}
}

func validInput(
	t *testing.T,
	resources []model.ContractResource,
	budgets model.BudgetLimits,
) authority.CompileInput {
	t.Helper()
	now := time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC)
	command := []string{"agent", "--task"}
	commandDigest, err := transactionreducer.ComputeCommandDigest(command)
	if err != nil {
		t.Fatal(err)
	}
	maxWall := int64(600)
	budgets.MaxWallTimeSeconds = &maxWall
	prepared, err := admission.Prepare("team", admission.Request{
		Version:        admission.LegacyRequestVersion,
		IdempotencyKey: "admission:test",
		TransactionID:  "tx:test",
		RunID:          "run:test",
		Intent:         "perform the admitted task",
		AgentProfile: model.AgentTaskProfileBinding{
			ID: "agent:test", Digest: testDigest("a"), RuntimeClass: "oci",
			ImageDigest: testDigest("b"), Entrypoint: "agent",
			CommandDigest: commandDigest,
		},
		Sponsor: model.Principal{
			ID: "human:sponsor", Kind: model.PrincipalHuman,
		},
		Actor: model.Principal{
			ID: "operator:supervisor", Kind: model.PrincipalOperator,
		},
		Contract: admission.ContractSpec{
			Risk: "high", Resources: resources, Budgets: budgets,
		},
	}, now)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	transaction := prepared.Result.Transaction.Transaction
	transaction.State = model.TransactionRunning
	transaction.StageBindings = []model.StageBinding{{
		ID: "stage:test", Kind: "git_worktree",
		Resource: model.ResourceSelector{
			Kind: "git_repository", Pattern: "/repository",
		},
		Location: "/staging/tx-test", BaseRevision: "revision:base",
		CreatedAt: now,
	}}
	transaction.EventSequence = 3
	transaction.UpdatedAt = now.Add(time.Second)
	if err := transaction.Validate(); err != nil {
		t.Fatalf("transaction fixture invalid: %v", err)
	}
	return authority.CompileInput{
		Task: prepared.Result.Task, Contract: prepared.Result.Contract,
		Run: prepared.Result.Run.Run, Grants: prepared.Result.Grants,
		Transaction:    transaction,
		ImageReference: "registry.example/agent@" + testDigest("b"),
		Command:        command,
		Ceilings: authority.DaemonCeilings{
			MaxWallTimeSeconds:  300,
			AllowedImageDigests: []string{testDigest("b")},
		},
		Now: now.Add(10 * time.Second),
	}
}

func workspaceResource(operations ...string) []model.ContractResource {
	return []model.ContractResource{{
		ID: "workspace",
		Selector: model.ResourceSelector{
			Kind: "filesystem", Pattern: "workspace/**",
		},
		Operations: operations,
		Conditions: model.CapabilityConditions{WorkspaceRoot: "workspace"},
	}}
}

func testDigest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}

func int64Pointer(value int64) *int64 {
	return &value
}

func mustContractDigest(t *testing.T, contract model.ExecutionContract) string {
	t.Helper()
	digest, err := model.ComputeExecutionContractDigest(contract)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func requireKernelCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", code)
	}
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) {
		t.Fatalf("error = %T %v, want *model.KernelError", err, err)
	}
	if kernelErr.Code != code {
		t.Fatalf("error code = %s, want %s: %v", kernelErr.Code, code, err)
	}
}
