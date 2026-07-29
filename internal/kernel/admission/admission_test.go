package admission

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
)

func TestComputeRequestDigestIsDeterministicAndIgnoresActorClaimsDigest(t *testing.T) {
	t.Parallel()
	request := validAdmissionRequest()
	first, err := ComputeRequestDigest("engineering", request)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := ComputeRequestDigest("engineering", request)
	if err != nil {
		t.Fatal(err)
	}
	if repeated != first {
		t.Fatalf("request digest changed across identical calls: %q != %q", repeated, first)
	}

	changedClaims := request
	changedClaims.Actor.ClaimsDigest = testDigest("f")
	withoutClaims := request
	withoutClaims.Actor.ClaimsDigest = ""
	for _, equivalent := range []Request{changedClaims, withoutClaims} {
		digest, err := ComputeRequestDigest("engineering", equivalent)
		if err != nil {
			t.Fatal(err)
		}
		if digest != first {
			t.Fatal("short-lived authenticated actor claims changed the request digest")
		}
	}

	changedActor := request
	changedActor.Actor.ID = "operator:different"
	digest, err := ComputeRequestDigest("engineering", changedActor)
	if err != nil {
		t.Fatal(err)
	}
	if digest == first {
		t.Fatal("material actor identity did not change the request digest")
	}
}

func TestPrepareAuthorsAdmittedRunEventChain(t *testing.T) {
	t.Parallel()
	request := validAdmissionRequest()
	prepared := mustPrepareAdmission(t, request)
	if len(prepared.RunEvents) != 3 {
		t.Fatalf("run event count = %d, want 3", len(prepared.RunEvents))
	}
	wantTypes := []string{
		reducer.EventRunCreated,
		reducer.EventCapabilitiesGranted,
		reducer.EventRunStateChanged,
	}
	daemon := daemonPrincipal()
	for index, event := range prepared.RunEvents {
		if event.Type != wantTypes[index] {
			t.Fatalf("event %d type = %q, want %q", index, event.Type, wantTypes[index])
		}
		if event.Sequence != int64(index+1) {
			t.Fatalf("event %d sequence = %d", index, event.Sequence)
		}
		if index == 0 {
			if event.Actor != request.Actor {
				t.Fatalf("creation actor = %#v, want %#v", event.Actor, request.Actor)
			}
		} else if event.Actor != daemon {
			t.Fatalf("daemon event %d actor = %#v, want %#v", index, event.Actor, daemon)
		}
		valid, err := model.VerifyEventDigest(event)
		if err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Fatalf("event %d digest was not daemon-authored from its content", index)
		}
		if index > 0 && event.PreviousDigest != prepared.RunEvents[index-1].Digest {
			t.Fatalf("event %d does not extend the prior digest", index)
		}
	}

	granted, err := model.DecodePayloadStrict[model.CapabilitiesGrantedPayload](
		prepared.RunEvents[1],
	)
	if err != nil {
		t.Fatal(err)
	}
	if granted.ContractDigest != prepared.Result.Contract.Digest ||
		!reflect.DeepEqual(granted.Grants, prepared.Result.Grants) {
		t.Fatal("capability event does not contain the admitted contract grants")
	}
	admitted, err := model.DecodePayloadStrict[model.RunStateChangedPayload](
		prepared.RunEvents[2],
	)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.From != model.RunCreated || admitted.To != model.RunAdmitted {
		t.Fatalf("admission transition = %s -> %s", admitted.From, admitted.To)
	}
	replayed, err := reducer.Replay(prepared.RunEvents)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, prepared.Result.Run) {
		t.Fatal("daemon-authored events did not replay to the returned run")
	}
	if replayed.Run.State != model.RunAdmitted || replayed.Run.EventSequence != 3 {
		t.Fatalf("run ended at state=%s sequence=%d", replayed.Run.State, replayed.Run.EventSequence)
	}
}

func TestPrepareBindsContractTaskRunAndTransaction(t *testing.T) {
	t.Parallel()
	request := validAdmissionRequest()
	prepared := mustPrepareAdmission(t, request)
	result := prepared.Result

	contractDigest, err := model.ComputeExecutionContractDigest(result.Contract)
	if err != nil {
		t.Fatal(err)
	}
	if contractDigest != result.Contract.Digest {
		t.Fatal("admitted contract digest does not bind its content")
	}
	changedContract := result.Contract
	changedContract.Goal += " Also alter the release policy."
	changedDigest, err := model.ComputeExecutionContractDigest(changedContract)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == contractDigest {
		t.Fatal("material contract mutation retained the admitted digest")
	}

	transaction := result.Transaction.Transaction
	binding := transaction.Admission
	if binding == nil {
		t.Fatal("transaction omitted its admission binding")
	}
	if transaction.Task == nil || !reflect.DeepEqual(*transaction.Task, result.Task) {
		t.Fatal("transaction does not retain the exact admitted task")
	}
	if result.Task.TransactionID != transaction.ID ||
		result.Task.RunID != result.Run.Run.ID ||
		len(transaction.AgentRunIDs) != 1 ||
		transaction.AgentRunIDs[0] != result.Run.Run.ID {
		t.Fatal("task, run, and transaction identities are not cross-bound")
	}
	if binding.TaskDigest != result.Task.Digest ||
		binding.RunID != result.Run.Run.ID ||
		binding.ContractDigest != result.Contract.Digest ||
		result.Run.Run.ContractDigest != result.Contract.Digest {
		t.Fatal("task, run, transaction, and contract digests are not cross-bound")
	}
	if len(result.Grants) == 0 ||
		len(result.Run.Run.CapabilityIDs) != len(result.Grants) {
		t.Fatal("admitted run does not retain its initial grants")
	}
	for index, grant := range result.Grants {
		if grant.SubjectRunID != result.Run.Run.ID ||
			result.Run.Run.CapabilityIDs[index] != grant.ID {
			t.Fatalf("grant %q is not bound to the admitted run", grant.ID)
		}
	}
}

func TestPreparedValidateRejectsAdmissionMutations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*Prepared)
	}{
		{
			name: "contract content",
			edit: func(prepared *Prepared) {
				prepared.Result.Contract.Goal += " Mutated."
			},
		},
		{
			name: "task intent",
			edit: func(prepared *Prepared) {
				prepared.Result.Task.Intent += " Mutated."
			},
		},
		{
			name: "run contract binding",
			edit: func(prepared *Prepared) {
				prepared.Result.Run.Run.ContractDigest = testDigest("e")
			},
		},
		{
			name: "grant subject",
			edit: func(prepared *Prepared) {
				prepared.Result.Grants[0].SubjectRunID = "run:different"
			},
		},
		{
			name: "transaction admission binding",
			edit: func(prepared *Prepared) {
				prepared.Result.Transaction.Transaction.Admission.ContractDigest =
					testDigest("e")
			},
		},
		{
			name: "run event digest",
			edit: func(prepared *Prepared) {
				prepared.RunEvents[1].Digest = testDigest("e")
			},
		},
		{
			name: "run event content",
			edit: func(prepared *Prepared) {
				prepared.RunEvents[2].Actor.ID = "service:tampered"
			},
		},
		{
			name: "transaction event digest",
			edit: func(prepared *Prepared) {
				prepared.TransactionEvent.Digest = testDigest("e")
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			prepared := mustPrepareAdmission(t, validAdmissionRequest())
			test.edit(&prepared)
			if err := prepared.Validate(); err == nil {
				t.Fatal("mutated admission passed validation")
			}
		})
	}
}

func mustPrepareAdmission(t *testing.T, request Request) Prepared {
	t.Helper()
	prepared, err := Prepare(
		"engineering",
		request,
		time.Date(2026, 7, 27, 8, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Validate(); err != nil {
		t.Fatalf("prepared admission did not validate: %v", err)
	}
	return prepared
}

func validAdmissionRequest() Request {
	maxToolCalls := int64(20)
	deadline := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	return Request{
		Version:        RequestVersion,
		IdempotencyKey: "admission:auth-fix",
		TransactionID:  "tx:auth-fix",
		Intent:         "Fix authentication without changing public behavior.",
		AgentProfile: model.AgentTaskProfileBinding{
			ID:            "agent-profile:secure-coder",
			Digest:        testDigest("a"),
			RuntimeClass:  "oci",
			ImageDigest:   testDigest("b"),
			Entrypoint:    "/opt/vouch-agent",
			CommandDigest: testDigest("c"),
		},
		Sponsor: model.Principal{
			ID: "human:sponsor", Kind: model.PrincipalHuman,
			Issuer: "https://identity.example.invalid",
		},
		Actor: model.Principal{
			ID: "operator:creator", Kind: model.PrincipalOperator,
			Issuer: "https://identity.example.invalid", ClaimsDigest: testDigest("d"),
		},
		Contract: ContractSpec{
			Risk: "high",
			Resources: []model.ContractResource{{
				ID: "workspace-files",
				Selector: model.ResourceSelector{
					Kind: "filesystem", Pattern: "workspace/**",
				},
				Operations: []string{"filesystem.read", "filesystem.write"},
				Conditions: model.CapabilityConditions{
					WorkspaceRoot: "workspace",
				},
			}},
			Budgets:  model.BudgetLimits{MaxToolCalls: &maxToolCalls},
			Deadline: &deadline,
		},
	}
}

func testDigest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}
