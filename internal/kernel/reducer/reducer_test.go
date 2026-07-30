package reducer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

func TestTransitionTableMatchesDocumentedLifecycle(t *testing.T) {
	t.Parallel()
	states := []model.RunState{
		model.RunCreated,
		model.RunAdmitted,
		model.RunRunning,
		model.RunWaitingForEvent,
		model.RunWaitingForAgent,
		model.RunWaitingForApproval,
		model.RunBlocked,
		model.RunFailed,
		model.RunCancelled,
		model.RunCompleted,
	}
	allowed := map[model.RunState][]model.RunState{
		model.RunCreated:            {model.RunAdmitted, model.RunFailed, model.RunCancelled},
		model.RunAdmitted:           {model.RunRunning, model.RunBlocked, model.RunFailed, model.RunCancelled},
		model.RunRunning:            {model.RunWaitingForEvent, model.RunWaitingForAgent, model.RunWaitingForApproval, model.RunBlocked, model.RunFailed, model.RunCancelled, model.RunCompleted},
		model.RunWaitingForEvent:    {model.RunRunning, model.RunBlocked, model.RunFailed, model.RunCancelled},
		model.RunWaitingForAgent:    {model.RunRunning, model.RunBlocked, model.RunFailed, model.RunCancelled},
		model.RunWaitingForApproval: {model.RunRunning, model.RunBlocked, model.RunFailed, model.RunCancelled},
		model.RunBlocked:            {model.RunAdmitted, model.RunRunning, model.RunFailed, model.RunCancelled},
	}
	for _, from := range states {
		for _, to := range states {
			want := containsState(allowed[from], to)
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%q, %q) = %t, want %t", from, to, got, want)
			}
		}
	}
}

func TestGoldenTransitionFixtures(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		fixture := readTransitionFixture(t, "valid.json")
		state := fixture.Initial
		for i, transition := range fixture.Transitions {
			if transition.From != state {
				t.Fatalf("transition %d starts at %q, current %q", i, transition.From, state)
			}
			if err := ValidateTransition(transition.From, transition.To, reasonFor(transition.To)); err != nil {
				t.Fatalf("transition %d: %v", i, err)
			}
			state = transition.To
		}
		if state != fixture.Final {
			t.Fatalf("final state %q, want %q", state, fixture.Final)
		}
	})

	t.Run("terminal cannot reopen", func(t *testing.T) {
		t.Parallel()
		fixture := readTransitionFixture(t, "invalid-terminal-reopen.json")
		state := fixture.Initial
		var gotErr error
		for _, transition := range fixture.Transitions {
			if transition.From != state {
				t.Fatalf("fixture transition starts at %q, current %q", transition.From, state)
			}
			gotErr = ValidateTransition(transition.From, transition.To, reasonFor(transition.To))
			if gotErr != nil {
				break
			}
			state = transition.To
		}
		assertKernelCode(t, gotErr, model.ErrorCode(fixture.ErrorCode))
	})
}

func TestReplayBuildsDeterministicProjection(t *testing.T) {
	t.Parallel()
	created := readCreationEvent(t)
	events := []model.RunEvent{created}
	projection, err := Apply(nil, created)
	if err != nil {
		t.Fatalf("apply creation: %v", err)
	}

	transitions := []struct {
		to     model.RunState
		reason string
	}{
		{model.RunAdmitted, ""},
		{model.RunRunning, ""},
		{model.RunWaitingForApproval, ""},
		{model.RunRunning, ""},
		{model.RunCompleted, ""},
	}
	for i, transition := range transitions {
		event := stateEvent(t, projection, transition.to, transition.reason, i+2)
		events = append(events, event)
		projection, err = Apply(&projection, event)
		if err != nil {
			t.Fatalf("apply transition %d: %v", i, err)
		}
	}
	if projection.Run.State != model.RunCompleted {
		t.Fatalf("state = %q, want completed", projection.Run.State)
	}
	if projection.Run.CompletedAt == nil {
		t.Fatal("completed projection needs completed_at")
	}
	if projection.Run.EventSequence != int64(len(events)) {
		t.Fatalf("sequence = %d, want %d", projection.Run.EventSequence, len(events))
	}

	replayedA, err := Replay(events)
	if err != nil {
		t.Fatalf("first replay: %v", err)
	}
	replayedB, err := Replay(events)
	if err != nil {
		t.Fatalf("second replay: %v", err)
	}
	jsonA, err := json.Marshal(replayedA)
	if err != nil {
		t.Fatal(err)
	}
	jsonB, err := json.Marshal(replayedB)
	if err != nil {
		t.Fatal(err)
	}
	if string(jsonA) != string(jsonB) {
		t.Fatalf("replay is not byte-equivalent:\n%s\n%s", jsonA, jsonB)
	}
}

func TestReducerRejectsSequenceAndDigestBreaks(t *testing.T) {
	t.Parallel()
	created := readCreationEvent(t)
	projection, err := Apply(nil, created)
	if err != nil {
		t.Fatal(err)
	}
	event := stateEvent(t, projection, model.RunAdmitted, "", 2)

	wrongSequence := event
	wrongSequence.Sequence = 3
	assertKernelCode(t, applyError(projection, wrongSequence), model.ErrorEventSequence)

	wrongDigest := event
	wrongDigest.PreviousDigest = digest("9")
	assertKernelCode(t, applyError(projection, wrongDigest), model.ErrorEventChain)
}

func TestReducerRejectsStateMismatchAndTerminalReopen(t *testing.T) {
	t.Parallel()
	projection, err := Apply(nil, readCreationEvent(t))
	if err != nil {
		t.Fatal(err)
	}
	mismatch := stateEvent(t, projection, model.RunAdmitted, "", 2)
	payload := model.RunStateChangedPayload{From: model.RunRunning, To: model.RunCompleted}
	mismatch.Payload = mustJSON(t, payload)
	assertKernelCode(t, applyError(projection, mismatch), model.ErrorTransitionInvalid)

	for i, to := range []model.RunState{model.RunAdmitted, model.RunRunning, model.RunCompleted} {
		event := stateEvent(t, projection, to, "", i+2)
		projection, err = Apply(&projection, event)
		if err != nil {
			t.Fatal(err)
		}
	}
	reopen := stateEvent(t, projection, model.RunRunning, "", 5)
	assertKernelCode(t, applyError(projection, reopen), model.ErrorTransitionInvalid)
}

func TestTransitionsIntoFailureStatesRequireReason(t *testing.T) {
	t.Parallel()
	for _, state := range []model.RunState{model.RunBlocked, model.RunFailed, model.RunCancelled} {
		err := ValidateTransition(model.RunRunning, state, "")
		assertKernelCode(t, err, model.ErrorTransitionInvalid)
	}
}

func TestNonStateEventsAdvanceCursorWithoutChangingState(t *testing.T) {
	t.Parallel()
	projection, err := Apply(nil, readCreationEvent(t))
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range []model.RunState{model.RunAdmitted, model.RunRunning} {
		projection, err = Apply(&projection, stateEvent(t, projection, state, "", i+2))
		if err != nil {
			t.Fatal(err)
		}
	}
	actionData, err := os.ReadFile(filepath.Join(schemaRoot(t), "fixtures", "kernel", "valid", "action_request.json"))
	if err != nil {
		t.Fatal(err)
	}
	action, err := model.DecodeStrict[model.ActionRequest](actionData)
	if err != nil {
		t.Fatal(err)
	}
	event := model.RunEvent{
		Version:        model.RunEventVersion,
		ID:             "event:demo-001:4",
		RunID:          projection.Run.ID,
		Sequence:       4,
		Type:           EventActionRequested,
		Actor:          projection.Run.Principal,
		OccurredAt:     projection.Run.UpdatedAt.Add(time.Second),
		Payload:        mustJSON(t, model.ActionRequestedPayload{Request: action}),
		PreviousDigest: projection.LastEventDigest,
		Digest:         digest("4"),
	}
	next, err := Apply(&projection, event)
	if err != nil {
		t.Fatal(err)
	}
	if next.Run.State != model.RunRunning {
		t.Fatalf("state changed to %q", next.Run.State)
	}
	if next.Run.EventSequence != 4 || next.LastEventDigest != event.Digest {
		t.Fatalf("event cursor did not advance: %#v", next)
	}
}

type transitionFixture struct {
	Initial     model.RunState `json:"initial"`
	Transitions []struct {
		From model.RunState `json:"from"`
		To   model.RunState `json:"to"`
	} `json:"transitions"`
	Final     model.RunState `json:"final"`
	ErrorCode string         `json:"error_code"`
}

func readTransitionFixture(t *testing.T, name string) transitionFixture {
	t.Helper()
	path := filepath.Join(schemaRoot(t), "fixtures", "kernel", "transitions", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture transitionFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func readCreationEvent(t *testing.T) model.RunEvent {
	t.Helper()
	path := filepath.Join(schemaRoot(t), "fixtures", "kernel", "valid", "run_event.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	event, err := model.DecodeStrict[model.RunEvent](data)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func stateEvent(t *testing.T, projection Projection, to model.RunState, reason string, sequence int) model.RunEvent {
	t.Helper()
	payload := model.RunStateChangedPayload{
		From:   projection.Run.State,
		To:     to,
		Reason: reason,
	}
	return model.RunEvent{
		Version:        model.RunEventVersion,
		ID:             fmt.Sprintf("event:demo-001:%d", sequence),
		RunID:          projection.Run.ID,
		Sequence:       int64(sequence),
		Type:           EventRunStateChanged,
		Actor:          model.Principal{ID: "service:gatemoled", Kind: model.PrincipalService},
		OccurredAt:     projection.Run.UpdatedAt.Add(time.Second),
		Payload:        mustJSON(t, payload),
		PreviousDigest: projection.LastEventDigest,
		Digest:         digest(fmt.Sprintf("%x", sequence%16)),
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func digest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}

func applyError(projection Projection, event model.RunEvent) error {
	_, err := Apply(&projection, event)
	return err
}

func assertKernelCode(t *testing.T, err error, want model.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected kernel error %s", want)
	}
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) {
		t.Fatalf("expected KernelError, got %T: %v", err, err)
	}
	if kernelErr.Code != want {
		t.Fatalf("code = %s, want %s: %v", kernelErr.Code, want, err)
	}
}

func containsState(values []model.RunState, want model.RunState) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func reasonFor(state model.RunState) string {
	if state == model.RunBlocked || state == model.RunFailed || state == model.RunCancelled {
		return "fixture reason"
	}
	return ""
}

func schemaRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "schemas"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
