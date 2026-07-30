package vouch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/broker"
	kernelclient "github.com/duriantaco/vouch/internal/kernel/client"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
	"github.com/duriantaco/vouch/internal/kernel/runtimeidentity"
)

type kernelRunClient interface {
	CreateRun(context.Context, model.RunEvent) (reducer.Projection, error)
	GetRun(context.Context, string, string) (reducer.Projection, error)
	ListRuns(context.Context, string) ([]model.AgentRun, error)
	Events(context.Context, string, string, int64) ([]model.RunEvent, error)
	AppendEvent(context.Context, string, string, int64, model.RunEvent) (reducer.Projection, error)
	InstallCapabilities(context.Context, string, string, int64, model.ExecutionContract, model.Principal) (kernelclient.CapabilityInstallResult, error)
	ExecuteAction(context.Context, string, string, broker.ExecuteRequest) (broker.Outcome, error)
}

type kernelClientFactory func(string) kernelRunClient

func kernelCommand(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		kernelUsage(stderr)
		return 2
	}
	factory, err := runtimeBoundKernelClientFactory(repo)
	if err != nil {
		fmt.Fprintf(stderr, "kernel: %v\n", err)
		return 1
	}
	return kernelCommandWithFactory(
		repo, args, jsonOut, stdout, stderr, factory,
	)
}

func kernelCommandWithFactory(
	repo string,
	args []string,
	jsonOut bool,
	stdout io.Writer,
	stderr io.Writer,
	newClient kernelClientFactory,
) int {
	if len(args) == 0 {
		kernelUsage(stderr)
		return 2
	}
	switch args[0] {
	case "run":
		return runCommandWithFactory(repo, args[1:], jsonOut, stdout, stderr, newClient)
	default:
		fmt.Fprintf(stderr, "kernel: unknown command %q\n", args[0])
		kernelUsage(stderr)
		return 2
	}
}

func runCommand(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		runUsage(stderr)
		return 2
	}
	factory, err := runtimeBoundKernelClientFactory(repo)
	if err != nil {
		fmt.Fprintf(stderr, "run: %v\n", err)
		return 1
	}
	return runCommandWithFactory(
		repo, args, jsonOut, stdout, stderr, factory,
	)
}

func runtimeBoundKernelClientFactory(
	repo string,
) (kernelClientFactory, error) {
	identity, err := runtimeidentity.Load(context.Background(), repo)
	if err != nil {
		return nil, fmt.Errorf(
			"load Runtime identity; run `vouch runtime init` first: %w",
			err,
		)
	}
	return func(socket string) kernelRunClient {
		return kernelclient.New(socket).
			WithExpectedRuntimeID(identity.RuntimeID)
	}, nil
}

func runCommandWithFactory(
	repo string,
	args []string,
	jsonOut bool,
	stdout io.Writer,
	stderr io.Writer,
	newClient kernelClientFactory,
) int {
	if len(args) == 0 {
		runUsage(stderr)
		return 2
	}
	switch args[0] {
	case "create":
		return runCreate(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "get":
		return runGet(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "list":
		return runList(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "events":
		return runEvents(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "transition":
		return runTransition(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "pause":
		return runTransitionAlias(repo, args[1:], jsonOut, stdout, stderr, newClient, model.RunBlocked, "paused by operator")
	case "resume":
		return runTransitionAlias(repo, args[1:], jsonOut, stdout, stderr, newClient, model.RunRunning, "")
	case "cancel":
		return runTransitionAlias(repo, args[1:], jsonOut, stdout, stderr, newClient, model.RunCancelled, "cancelled by operator")
	case "grant":
		return runGrant(repo, args[1:], jsonOut, stdout, stderr, newClient)
	default:
		fmt.Fprintf(stderr, "run: unknown command %q\n", args[0])
		runUsage(stderr)
		return 2
	}
}

func runCreate(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer, newClient kernelClientFactory) int {
	flags := flag.NewFlagSet("run create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	file := flags.String("file", "", "AgentRun JSON file")
	socket := flags.String("socket", defaultKernelSocket(repo), "vouchd Unix socket")
	actorID := flags.String("actor", "operator:local", "principal ID creating the run")
	actorKind := flags.String("actor-kind", string(model.PrincipalOperator), "principal kind creating the run")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *file == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "run create requires --file and accepts no positional arguments")
		return 2
	}
	run, err := readRunFile(repo, *file)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	now := time.Now().UTC()
	run.State = model.RunCreated
	run.StateReason = ""
	run.EventSequence = 1
	run.CreatedAt = now
	run.UpdatedAt = now
	run.CompletedAt = nil
	payload, err := json.Marshal(model.RunCreatedPayload{Run: run})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	event := model.RunEvent{
		Version:    model.RunEventVersion,
		ID:         kernelEventID(run.ID, 1),
		RunID:      run.ID,
		Sequence:   1,
		Type:       reducer.EventRunCreated,
		Actor:      model.Principal{ID: *actorID, Kind: model.PrincipalKind(*actorKind)},
		OccurredAt: now,
		Payload:    payload,
	}
	event.Digest, err = model.ComputeEventDigest(event)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	projection, err := newClient(*socket).CreateRun(context.Background(), event)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return renderRunProjection(projection, jsonOut, "Created", stdout, stderr)
}

func runGet(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer, newClient kernelClientFactory) int {
	flags := flag.NewFlagSet("run get", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "run namespace")
	runID := flags.String("id", "", "run ID")
	socket := flags.String("socket", defaultKernelSocket(repo), "vouchd Unix socket")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || *runID == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "run get requires --namespace and --id")
		return 2
	}
	projection, err := newClient(*socket).GetRun(context.Background(), *namespace, *runID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return renderRunProjection(projection, jsonOut, "Run", stdout, stderr)
}

func runList(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer, newClient kernelClientFactory) int {
	flags := flag.NewFlagSet("run list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "run namespace")
	socket := flags.String("socket", defaultKernelSocket(repo), "vouchd Unix socket")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "run list requires --namespace")
		return 2
	}
	runs, err := newClient(*socket).ListRuns(context.Background(), *namespace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(runs, stdout, stderr)
	}
	if len(runs) == 0 {
		fmt.Fprintf(stdout, "No runs in namespace %s.\n", *namespace)
		return 0
	}
	for _, run := range runs {
		fmt.Fprintf(stdout, "%s\t%s\tsequence=%d\tupdated=%s\n", run.ID, run.State, run.EventSequence, run.UpdatedAt.Format(time.RFC3339))
	}
	return 0
}

func runEvents(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer, newClient kernelClientFactory) int {
	flags := flag.NewFlagSet("run events", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "run namespace")
	runID := flags.String("id", "", "run ID")
	after := flags.Int64("after", 0, "only events after this sequence")
	socket := flags.String("socket", defaultKernelSocket(repo), "vouchd Unix socket")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || *runID == "" || *after < 0 || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "run events requires --namespace and --id; --after must be non-negative")
		return 2
	}
	events, err := newClient(*socket).Events(context.Background(), *namespace, *runID, *after)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(events, stdout, stderr)
	}
	for _, event := range events {
		fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\n", event.Sequence, event.Type, event.Actor.ID, event.OccurredAt.Format(time.RFC3339Nano))
	}
	return 0
}

func runTransition(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer, newClient kernelClientFactory) int {
	flags := flag.NewFlagSet("run transition", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "run namespace")
	runID := flags.String("id", "", "run ID")
	to := flags.String("to", "", "target run state")
	reason := flags.String("reason", "", "transition reason")
	socket := flags.String("socket", defaultKernelSocket(repo), "vouchd Unix socket")
	actorID := flags.String("actor", "operator:local", "principal ID requesting the transition")
	actorKind := flags.String("actor-kind", string(model.PrincipalOperator), "principal kind requesting the transition")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || *runID == "" || *to == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "run transition requires --namespace, --id, and --to")
		return 2
	}
	client := newClient(*socket)
	current, err := client.GetRun(context.Background(), *namespace, *runID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	now := time.Now().UTC()
	if now.Before(current.Run.UpdatedAt) {
		now = current.Run.UpdatedAt
	}
	payload, err := json.Marshal(model.RunStateChangedPayload{
		From:   current.Run.State,
		To:     model.RunState(*to),
		Reason: *reason,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	sequence := current.Run.EventSequence + 1
	event := model.RunEvent{
		Version:        model.RunEventVersion,
		ID:             kernelEventID(current.Run.ID, sequence),
		RunID:          current.Run.ID,
		Sequence:       sequence,
		Type:           reducer.EventRunStateChanged,
		Actor:          model.Principal{ID: *actorID, Kind: model.PrincipalKind(*actorKind)},
		OccurredAt:     now,
		Payload:        payload,
		PreviousDigest: current.LastEventDigest,
	}
	event.Digest, err = model.ComputeEventDigest(event)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	next, err := client.AppendEvent(context.Background(), *namespace, *runID, current.Run.EventSequence, event)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return renderRunProjection(next, jsonOut, "Transitioned", stdout, stderr)
}

func runTransitionAlias(
	repo string,
	args []string,
	jsonOut bool,
	stdout io.Writer,
	stderr io.Writer,
	newClient kernelClientFactory,
	target model.RunState,
	defaultReason string,
) int {
	transitionArgs := append([]string{}, args...)
	transitionArgs = append(transitionArgs, "--to", string(target))
	if defaultReason != "" && !hasFlag(args, "--reason") {
		transitionArgs = append(transitionArgs, "--reason", defaultReason)
	}
	return runTransition(repo, transitionArgs, jsonOut, stdout, stderr, newClient)
}

func runGrant(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer, newClient kernelClientFactory) int {
	flags := flag.NewFlagSet("run grant", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "run namespace")
	runID := flags.String("id", "", "run ID")
	contractPath := flags.String("contract", "", "ExecutionContract JSON file")
	socket := flags.String("socket", defaultKernelSocket(repo), "vouchd Unix socket")
	actorID := flags.String("actor", "operator:local", "principal ID installing the grants")
	actorKind := flags.String("actor-kind", string(model.PrincipalOperator), "principal kind installing the grants")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || *runID == "" || *contractPath == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "run grant requires --namespace, --id, and --contract")
		return 2
	}
	contract, err := readKernelFile[model.ExecutionContract](repo, *contractPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	client := newClient(*socket)
	projection, err := client.GetRun(context.Background(), *namespace, *runID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result, err := client.InstallCapabilities(
		context.Background(),
		*namespace,
		*runID,
		projection.Run.EventSequence,
		contract,
		model.Principal{ID: *actorID, Kind: model.PrincipalKind(*actorKind)},
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Installed %d capabilities on %s at sequence %d\n", len(result.Grants), result.Projection.Run.ID, result.Projection.Run.EventSequence)
	return 0
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func readRunFile(repo, path string) (model.AgentRun, error) {
	return readKernelFile[model.AgentRun](repo, path)
}

func readKernelFile[T any](repo, path string) (T, error) {
	var zero T
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return zero, fmt.Errorf("read kernel resource: %w", err)
	}
	value, err := model.DecodeStrict[T](data)
	if err != nil {
		return zero, err
	}
	return value, nil
}

func defaultKernelSocket(repo string) string {
	return filepath.Join(repo, ".vouch", "vouchd.sock")
}

func kernelEventID(runID string, sequence int64) string {
	sum := sha256.Sum256([]byte(runID))
	return fmt.Sprintf("event:%s:%d", hex.EncodeToString(sum[:16]), sequence)
}

func renderRunProjection(
	projection reducer.Projection,
	jsonOut bool,
	verb string,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if jsonOut {
		return renderCommandJSON(projection, stdout, stderr)
	}
	fmt.Fprintf(
		stdout,
		"%s %s in %s: state=%s sequence=%d\n",
		verb,
		projection.Run.ID,
		projection.Run.Namespace,
		projection.Run.State,
		projection.Run.EventSequence,
	)
	return 0
}

func runUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: vouch [--repo DIR] [--json] kernel run <command>")
	fmt.Fprintln(out, "  run get --namespace NS --id ID [--socket FILE]")
	fmt.Fprintln(out, "  run list --namespace NS [--socket FILE]")
	fmt.Fprintln(out, "  run events --namespace NS --id ID [--after N] [--socket FILE]")
	fmt.Fprintln(out, "  run transition --namespace NS --id ID --to STATE [--reason TEXT] [--socket FILE]")
	fmt.Fprintln(out, "  run pause|resume|cancel --namespace NS --id ID [--reason TEXT] [--socket FILE]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "embedded/unbound compatibility only; configured vouchd rejects raw authority creation:")
	fmt.Fprintln(out, "  run create --file FILE [--socket FILE] [--actor ID] [--actor-kind KIND]")
	fmt.Fprintln(out, "  run grant --namespace NS --id ID --contract FILE [--socket FILE]")
}

func kernelUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: vouch [--repo DIR] [--json] kernel run <command>")
	fmt.Fprintln(out, "  kernel run manages low-level AgentRun inspection and lifecycle transitions")
	fmt.Fprintln(out, "  raw run creation and grants are embedded/unbound compatibility operations")
	fmt.Fprintln(out, "  use 'vouch kernel run' for command details")
}
