package vouch

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/broker"
	"github.com/duriantaco/vouch/internal/kernel/driver"
	"github.com/duriantaco/vouch/internal/kernel/model"
)

func actionCommand(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		actionUsage(stderr)
		return 2
	}
	factory, err := runtimeBoundKernelClientFactory(repo)
	if err != nil {
		fmt.Fprintf(stderr, "action: %v\n", err)
		return 1
	}
	return actionCommandWithFactory(
		repo, args, jsonOut, stdout, stderr, factory,
	)
}

func actionCommandWithFactory(
	repo string,
	args []string,
	jsonOut bool,
	stdout io.Writer,
	stderr io.Writer,
	newClient kernelClientFactory,
) int {
	if len(args) == 0 {
		actionUsage(stderr)
		return 2
	}
	switch args[0] {
	case "fs-write":
		return filesystemActionCommand(repo, args[1:], jsonOut, stdout, stderr, newClient, driver.FilesystemWrite)
	case "fs-read":
		return filesystemActionCommand(repo, args[1:], jsonOut, stdout, stderr, newClient, driver.FilesystemRead)
	default:
		fmt.Fprintf(stderr, "action: unknown command %q\n", args[0])
		actionUsage(stderr)
		return 2
	}
}

func filesystemActionCommand(
	repo string,
	args []string,
	jsonOut bool,
	stdout io.Writer,
	stderr io.Writer,
	newClient kernelClientFactory,
	operation string,
) int {
	flags := flag.NewFlagSet("action "+operation, flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "run namespace")
	runID := flags.String("run", "", "run ID")
	logicalPath := flags.String("path", "", "logical path including the workspace root")
	inputPath := flags.String("input", "", "input content file for fs-write")
	intent := flags.String("intent", "perform a governed filesystem action", "declared action intent")
	actionID := flags.String("action-id", "", "stable action ID (generated when omitted)")
	idempotencyKey := flags.String("idempotency-key", "", "idempotency key (generated when omitted)")
	capabilityHint := flags.String("capability", "", "specific capability grant ID")
	socket := flags.String("socket", defaultKernelSocket(repo), "gatemoled Unix socket")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || *runID == "" || *logicalPath == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "filesystem actions require --namespace, --run, and --path")
		return 2
	}
	if operation == driver.FilesystemWrite && *inputPath == "" {
		fmt.Fprintln(stderr, "action fs-write requires --input")
		return 2
	}
	if operation == driver.FilesystemRead && *inputPath != "" {
		fmt.Fprintln(stderr, "action fs-read does not accept --input")
		return 2
	}
	if *actionID == "" {
		*actionID = randomKernelID("action")
	}
	if *idempotencyKey == "" {
		*idempotencyKey = randomKernelID("idem")
	}

	var content []byte
	var arguments json.RawMessage
	var err error
	if operation == driver.FilesystemWrite {
		path := *inputPath
		if !filepath.IsAbs(path) {
			path = filepath.Join(repo, path)
		}
		content, err = os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		arguments, err = json.Marshal(driver.FilesystemWriteArguments{
			Path:          *logicalPath,
			ContentDigest: driver.DigestBytes(content),
		})
	} else {
		arguments, err = json.Marshal(driver.FilesystemReadArguments{Path: *logicalPath})
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_, _, normalized, err := driver.NormalizeArguments(operation, arguments)
	if err != nil {
		// Send structurally valid but unsafe paths to the broker so the denied
		// attempt is part of the authoritative audit history.
		normalized = arguments
	}
	client := newClient(*socket)
	projection, err := client.GetRun(context.Background(), *namespace, *runID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	request := model.ActionRequest{
		Version:         model.ActionRequestVersion,
		ID:              *actionID,
		RunID:           *runID,
		Operation:       operation,
		Resource:        model.ResourceSelector{Kind: "filesystem", Pattern: *logicalPath},
		Arguments:       arguments,
		ArgumentsDigest: driver.DigestBytes(normalized),
		IdempotencyKey:  *idempotencyKey,
		Intent:          *intent,
		CapabilityHint:  *capabilityHint,
		RequestedAt:     time.Now().UTC(),
		Attempt:         1,
	}
	outcome, err := client.ExecuteAction(context.Background(), *namespace, *runID, broker.ExecuteRequest{
		ExpectedSequence: projection.Run.EventSequence,
		Action:           request,
		InputBase64:      base64.StdEncoding.EncodeToString(content),
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		if code := renderCommandJSON(outcome, stdout, stderr); code != 0 {
			return code
		}
		if outcome.Status != "committed" {
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Action %s: %s (%d bytes, %s)\n", outcome.ActionID, outcome.Status, outcome.OutputBytes, outcome.ResultDigest)
	if operation == driver.FilesystemRead && outcome.OutputBase64 != "" {
		output, decodeErr := base64.StdEncoding.DecodeString(outcome.OutputBase64)
		if decodeErr != nil {
			fmt.Fprintln(stderr, decodeErr)
			return 1
		}
		if _, err := stdout.Write(output); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if outcome.Status != "committed" {
		return 1
	}
	return 0
}

func randomKernelID(prefix string) string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		// crypto/rand failure is extraordinary; the timestamp preserves a valid
		// identifier and the store still rejects any collision.
		return fmt.Sprintf("%s:%d", prefix, time.Now().UTC().UnixNano())
	}
	return prefix + ":" + hex.EncodeToString(value)
}

func actionUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: vouch [--repo DIR] [--json] action <command>")
	fmt.Fprintln(out, "  action fs-write --namespace NS --run ID --path WORKSPACE/PATH --input FILE [--capability ID]")
	fmt.Fprintln(out, "  action fs-read --namespace NS --run ID --path WORKSPACE/PATH [--capability ID]")
}
