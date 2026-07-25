package vouch

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kernelapi "github.com/duriantaco/vouch/internal/kernel/api"
	"github.com/duriantaco/vouch/internal/kernel/broker"
	kernelclient "github.com/duriantaco/vouch/internal/kernel/client"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
	"github.com/duriantaco/vouch/internal/kernel/store"
)

func TestRunCLIThroughKernelAPI(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, "workspace"), 0o750); err != nil {
		t.Fatal(err)
	}
	kernelStore, err := store.OpenSQLite(filepath.Join(repo, "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	now := time.Now().UTC()
	actionBroker, err := broker.New(kernelStore, repo, broker.WithClock(func() time.Time { return now.Add(time.Minute) }))
	if err != nil {
		t.Fatal(err)
	}
	handler := kernelapi.NewServer(
		kernelStore,
		kernelapi.WithBroker(actionBroker),
		kernelapi.WithClock(func() time.Time { return now.Add(time.Minute) }),
	).Handler()
	newClient := func(string) kernelRunClient {
		return kernelclient.NewWithTransport(handlerTransport{handler: handler})
	}

	fixture, contractFixture := writeLiveKernelFixtures(t, repo, now.Add(2*time.Hour))
	stdout, stderr, code := invokeRunCLI(repo, newClient, true,
		"create", "--file", fixture,
	)
	if code != 0 {
		t.Fatalf("create code=%d stderr=%s", code, stderr)
	}
	var created reducer.Projection
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("decode create output: %v\n%s", err, stdout)
	}
	if created.Run.State != model.RunCreated || created.Run.EventSequence != 1 {
		t.Fatalf("unexpected creation projection: %#v", created)
	}

	stdout, stderr, code = invokeRunCLI(repo, newClient, true,
		"list", "--namespace", created.Run.Namespace,
	)
	if code != 0 || !strings.Contains(stdout, created.Run.ID) {
		t.Fatalf("list code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}

	stdout, stderr, code = invokeRunCLI(repo, newClient, true,
		"transition",
		"--namespace", created.Run.Namespace,
		"--id", created.Run.ID,
		"--to", string(model.RunAdmitted),
	)
	if code != 0 {
		t.Fatalf("transition code=%d stderr=%s", code, stderr)
	}
	var admitted reducer.Projection
	if err := json.Unmarshal([]byte(stdout), &admitted); err != nil {
		t.Fatal(err)
	}
	if admitted.Run.State != model.RunAdmitted || admitted.Run.EventSequence != 2 {
		t.Fatalf("unexpected transition projection: %#v", admitted)
	}

	stdout, stderr, code = invokeRunCLI(repo, newClient, true,
		"pause",
		"--namespace", admitted.Run.Namespace,
		"--id", admitted.Run.ID,
	)
	if code != 0 {
		t.Fatalf("pause code=%d stderr=%s", code, stderr)
	}
	var paused reducer.Projection
	if err := json.Unmarshal([]byte(stdout), &paused); err != nil {
		t.Fatal(err)
	}
	if paused.Run.State != model.RunBlocked || paused.Run.StateReason != "paused by operator" {
		t.Fatalf("unexpected paused projection: %#v", paused)
	}

	stdout, stderr, code = invokeRunCLI(repo, newClient, true,
		"resume",
		"--namespace", paused.Run.Namespace,
		"--id", paused.Run.ID,
	)
	if code != 0 {
		t.Fatalf("resume code=%d stderr=%s", code, stderr)
	}
	var resumed reducer.Projection
	if err := json.Unmarshal([]byte(stdout), &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.Run.State != model.RunRunning || resumed.Run.EventSequence != 4 {
		t.Fatalf("unexpected resumed projection: %#v", resumed)
	}

	stdout, stderr, code = invokeRunCLI(repo, newClient, true,
		"grant",
		"--namespace", resumed.Run.Namespace,
		"--id", resumed.Run.ID,
		"--contract", contractFixture,
	)
	if code != 0 {
		t.Fatalf("grant code=%d stderr=%s", code, stderr)
	}
	var installed kernelclient.CapabilityInstallResult
	if err := json.Unmarshal([]byte(stdout), &installed); err != nil {
		t.Fatal(err)
	}
	if len(installed.Grants) != 2 || installed.Projection.Run.EventSequence != 5 {
		t.Fatalf("unexpected capability install: %#v", installed)
	}

	inputPath := filepath.Join(repo, "input.txt")
	if err := os.WriteFile(inputPath, []byte("governed CLI write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = invokeActionCLI(repo, newClient, true,
		"fs-write",
		"--namespace", resumed.Run.Namespace,
		"--run", resumed.Run.ID,
		"--path", "workspace/cli.txt",
		"--input", inputPath,
		"--action-id", "action:cli-write",
		"--idempotency-key", "idem:cli-write",
	)
	if code != 0 {
		t.Fatalf("fs-write code=%d stderr=%s", code, stderr)
	}
	var written broker.Outcome
	if err := json.Unmarshal([]byte(stdout), &written); err != nil {
		t.Fatal(err)
	}
	if written.Status != "committed" || written.Projection.Run.EventSequence != 9 {
		t.Fatalf("unexpected write outcome: %#v", written)
	}

	stdout, stderr, code = invokeActionCLI(repo, newClient, true,
		"fs-read",
		"--namespace", resumed.Run.Namespace,
		"--run", resumed.Run.ID,
		"--path", "workspace/cli.txt",
		"--action-id", "action:cli-read",
		"--idempotency-key", "idem:cli-read",
	)
	if code != 0 {
		t.Fatalf("fs-read code=%d stderr=%s", code, stderr)
	}
	var read broker.Outcome
	if err := json.Unmarshal([]byte(stdout), &read); err != nil {
		t.Fatal(err)
	}
	if read.Status != "committed" || read.Projection.Run.EventSequence != 13 || read.OutputBase64 == "" {
		t.Fatalf("unexpected read outcome: %#v", read)
	}

	_, stderr, code = invokeActionCLI(repo, newClient, true,
		"fs-write",
		"--namespace", resumed.Run.Namespace,
		"--run", resumed.Run.ID,
		"--path", "workspace/../escaped.txt",
		"--input", inputPath,
		"--action-id", "action:cli-escape",
		"--idempotency-key", "idem:cli-escape",
	)
	if code != 1 || !strings.Contains(stderr, string(model.ErrorCapabilityDenied)) {
		t.Fatalf("escape code=%d stderr=%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(repo, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied escape changed filesystem: %v", err)
	}

	_, stderr, code = invokeRunCLI(repo, newClient, false,
		"transition",
		"--namespace", resumed.Run.Namespace,
		"--id", resumed.Run.ID,
		"--to", string(model.RunAdmitted),
	)
	if code != 1 || !strings.Contains(stderr, string(model.ErrorTransitionInvalid)) {
		t.Fatalf("illegal transition code=%d stderr=%s", code, stderr)
	}

	stdout, stderr, code = invokeRunCLI(repo, newClient, true,
		"cancel",
		"--namespace", resumed.Run.Namespace,
		"--id", resumed.Run.ID,
	)
	if code != 0 {
		t.Fatalf("cancel code=%d stderr=%s", code, stderr)
	}
	var cancelled reducer.Projection
	if err := json.Unmarshal([]byte(stdout), &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.Run.State != model.RunCancelled || cancelled.Run.EventSequence != 16 {
		t.Fatalf("unexpected cancelled projection: %#v", cancelled)
	}

	stdout, stderr, code = invokeRunCLI(repo, newClient, true,
		"get",
		"--namespace", cancelled.Run.Namespace,
		"--id", cancelled.Run.ID,
	)
	if code != 0 {
		t.Fatalf("get code=%d stderr=%s", code, stderr)
	}
	var fetched reducer.Projection
	if err := json.Unmarshal([]byte(stdout), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.Run.State != model.RunCancelled || fetched.Run.EventSequence != 16 {
		t.Fatalf("unexpected fetched run: %#v", fetched)
	}

	stdout, stderr, code = invokeRunCLI(repo, newClient, true,
		"events",
		"--namespace", cancelled.Run.Namespace,
		"--id", cancelled.Run.ID,
		"--after", "1",
	)
	if code != 0 {
		t.Fatalf("events code=%d stderr=%s", code, stderr)
	}
	var events []model.RunEvent
	if err := json.Unmarshal([]byte(stdout), &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 15 || events[0].Sequence != 2 || events[14].Sequence != 16 {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func invokeRunCLI(repo string, newClient kernelClientFactory, jsonOut bool, args ...string) (string, string, int) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runCommandWithFactory(repo, args, jsonOut, &stdout, &stderr, newClient)
	return stdout.String(), stderr.String(), code
}

func invokeActionCLI(repo string, newClient kernelClientFactory, jsonOut bool, args ...string) (string, string, int) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := actionCommandWithFactory(repo, args, jsonOut, &stdout, &stderr, newClient)
	return stdout.String(), stderr.String(), code
}

func writeLiveKernelFixtures(t *testing.T, repo string, deadline time.Time) (string, string) {
	t.Helper()
	fixtureRoot, err := filepath.Abs(filepath.Join("..", "..", "schemas", "fixtures", "kernel", "valid"))
	if err != nil {
		t.Fatal(err)
	}
	runData, err := os.ReadFile(filepath.Join(fixtureRoot, "agent_run.json"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := model.DecodeStrict[model.AgentRun](runData)
	if err != nil {
		t.Fatal(err)
	}
	run.Deadline = &deadline
	runPath := filepath.Join(repo, "agent_run.json")
	writeKernelFixture(t, runPath, run)

	contractData, err := os.ReadFile(filepath.Join(fixtureRoot, "execution_contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	contract, err := model.DecodeStrict[model.ExecutionContract](contractData)
	if err != nil {
		t.Fatal(err)
	}
	contract.Deadline = &deadline
	contractPath := filepath.Join(repo, "execution_contract.json")
	writeKernelFixture(t, contractPath, contract)
	return runPath, contractPath
}

func writeKernelFixture(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

type handlerTransport struct {
	handler http.Handler
}

func (transport handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}
