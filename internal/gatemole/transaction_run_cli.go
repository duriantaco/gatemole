package gatemole

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/admission"
	kernelclient "github.com/duriantaco/gatemole/internal/kernel/client"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/runtimeidentity"
	"github.com/duriantaco/gatemole/internal/kernel/runtimepreflight"
	"github.com/duriantaco/gatemole/internal/kernel/sandbox"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
)

type transactionRunResult struct {
	Projection transactionreducer.Projection        `json:"projection"`
	Workspace  string                               `json:"workspace"`
	Execution  model.AgentExecution                 `json:"execution"`
	Decision   *transactionreducer.SequenceDecision `json:"decision,omitempty"`
}

type supervisedProcessResult struct {
	Status       model.AgentExecutionStatus
	ExitCode     *int
	StdoutDigest string
	StderrDigest string
}

type supervisedInvocation struct {
	Executable    string
	Arguments     []string
	Directory     string
	RuntimeClass  string
	RuntimeDigest string
	ImageDigest   string
	Cleanup       func()
}

func transactionRun(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
	newClient transactionClientFactory,
) int {
	return transactionRunNamed("tx run", repo, args, jsonOut, stdout, stderr, newClient)
}

func runtimeRunCommand(repo string, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		runtimeRunUsage(stderr)
		return 2
	}
	transactionFactory, err := runtimeBoundTransactionClientFactory(repo)
	if err != nil {
		fmt.Fprintf(stderr, "run: %v\n", err)
		return 1
	}
	kernelFactory, err := runtimeBoundKernelClientFactory(repo)
	if err != nil {
		fmt.Fprintf(stderr, "run: %v\n", err)
		return 1
	}
	return runtimeRunCommandWithFactories(
		repo,
		args,
		jsonOut,
		stdout,
		stderr,
		transactionFactory,
		kernelFactory,
	)
}

func runtimeRunCommandWithFactories(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
	newTransactionClient transactionClientFactory,
	newKernelClient kernelClientFactory,
) int {
	if len(args) == 0 {
		runtimeRunUsage(stderr)
		return 2
	}
	if isLegacyRunSubcommand(args[0]) {
		// Keep the original gatemole run lifecycle callable for existing scripts
		// while making it explicit under `gatemole kernel run` for new users.
		return runCommandWithFactory(
			repo, args, jsonOut, stdout, stderr, newKernelClient,
		)
	}
	return transactionRunNamed(
		"run", repo, args, jsonOut, stdout, stderr, newTransactionClient,
	)
}

func isLegacyRunSubcommand(value string) bool {
	switch value {
	case "create", "get", "list", "events", "transition", "pause", "resume", "cancel", "grant":
		return true
	default:
		return false
	}
}

func transactionRunNamed(
	commandName, repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
	newClient transactionClientFactory,
) int {
	flags := flag.NewFlagSet(commandName, flag.ContinueOnError)
	flags.SetOutput(stderr)
	id := flags.String("id", "", "transaction ID; generated when omitted")
	namespace := flags.String("namespace", "local", "transaction namespace")
	intent := flags.String("intent", "", "human-owned task intent")
	intentFile := flags.String("intent-file", "", "file containing task intent")
	sponsorID := flags.String("sponsor", "human:local", "human or service sponsor ID")
	sponsorKind := flags.String("sponsor-kind", string(model.PrincipalHuman), "sponsor principal kind")
	runID := flags.String("run", "", "participating agent run ID; generated when omitted")
	revision := flags.String("revision", "HEAD", "base Git revision")
	timeout := flags.Duration("timeout", 30*time.Minute, "maximum agent execution duration")
	modelProvider := flags.String(
		"model-provider",
		"",
		"allow model egress only through the daemon broker for this provider",
	)
	runtimeClass := flags.String("runtime", "oci", "execution runtime: oci or host")
	image := flags.String("image", "", "digest-pinned OCI image")
	agent := flags.String("agent", "", "named agent profile from the strict repo profile document")
	agentProfiles := flags.String("agent-profiles", "", "agent profile document (default .gatemole/agent-profiles.json)")
	unsafeHost := flags.Bool("unsafe-host", false, "acknowledge that host execution is not a security boundary")
	requiredEnforcementProfile := flags.String(
		"require-enforcement-profile",
		"",
		"require daemon enforcement profile: development or production",
	)
	socket := flags.String("socket", defaultKernelSocket(repo), "gatemoled Unix socket")
	actorID := flags.String("actor", "operator:local", "principal ID supervising the transaction")
	actorKind := flags.String("actor-kind", string(model.PrincipalOperator), "supervisor principal kind")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	command := flags.Args()
	if (*intent == "") == (*intentFile == "") || *namespace == "" || *timeout < time.Second {
		fmt.Fprintf(stderr, "%s requires exactly one of --intent or --intent-file and a timeout of at least one second\n", commandName)
		return 2
	}
	if *runtimeClass != "oci" && *runtimeClass != "host" {
		fmt.Fprintf(stderr, "%s --runtime must be oci or host\n", commandName)
		return 2
	}
	if *requiredEnforcementProfile != "" &&
		!model.IsEnforcementProfile(*requiredEnforcementProfile) {
		fmt.Fprintf(
			stderr,
			"%s --require-enforcement-profile must be development or production\n",
			commandName,
		)
		return 2
	}
	if *runtimeClass == "host" &&
		*requiredEnforcementProfile == "production" {
		fmt.Fprintf(
			stderr,
			"%s host execution cannot require the production enforcement profile\n",
			commandName,
		)
		return 2
	}
	if *modelProvider != "" && !model.IsIdentifier(*modelProvider) {
		fmt.Fprintf(stderr, "%s --model-provider must be an identifier\n", commandName)
		return 2
	}
	if *modelProvider != "" && *runtimeClass != "oci" {
		fmt.Fprintf(stderr, "%s --model-provider requires --runtime oci\n", commandName)
		return 2
	}
	if *agent == "" && len(command) == 0 {
		fmt.Fprintf(stderr, "%s requires --agent or a raw command after --\n", commandName)
		return 2
	}
	if *agent == "" && *agentProfiles != "" {
		fmt.Fprintf(stderr, "%s --agent-profiles requires --agent\n", commandName)
		return 2
	}
	if *agent != "" && *image != "" {
		fmt.Fprintf(stderr, "%s --agent and --image cannot be combined\n", commandName)
		return 2
	}
	if *agent != "" && *runtimeClass != "oci" {
		fmt.Fprintf(stderr, "%s named agent profiles require --runtime oci\n", commandName)
		return 2
	}
	if *agent != "" && *unsafeHost {
		fmt.Fprintf(stderr, "%s named agent profiles do not accept --unsafe-host\n", commandName)
		return 2
	}
	if *agent == "" && *runtimeClass == "host" && !*unsafeHost {
		fmt.Fprintf(stderr, "%s host execution requires --unsafe-host; use --runtime oci for an enforced boundary\n", commandName)
		return 2
	}
	if *agent == "" && *runtimeClass == "oci" && *image == "" {
		fmt.Fprintf(stderr, "%s OCI execution requires --image pinned by sha256 digest\n", commandName)
		return 2
	}
	if *agent == "" && *runtimeClass == "host" && *image != "" {
		fmt.Fprintf(stderr, "%s host execution does not accept --image\n", commandName)
		return 2
	}
	var selectedProfile resolvedAgentProfile
	var err error
	if *agent != "" {
		selectedProfile, err = resolveNamedAgentProfile(
			repo, *agentProfiles, *agent, command,
		)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		*runtimeClass = selectedProfile.RuntimeClass
		*image = selectedProfile.OCIImage
		command = selectedProfile.Command
	} else {
		selectedProfile, err = resolveAdHocAgentProfile(
			*runtimeClass, *image, command,
		)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	runtimeIdentity, err := runtimeidentity.Load(
		context.Background(),
		repo,
	)
	if err != nil {
		fmt.Fprintf(
			stderr,
			"%s: load Runtime identity; run `gatemole runtime init` first: %v\n",
			commandName,
			err,
		)
		return 1
	}
	intentBytes, err := transactionRunIntent(repo, *intent, *intentFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *id == "" {
		*id, err = generateTransactionID(time.Now().UTC())
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if *runID == "" {
		*runID = "run:" + *id
	}
	actor := model.Principal{ID: *actorID, Kind: model.PrincipalKind(*actorKind)}
	profileBinding, err := selectedProfile.binding()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	maxWallTimeSeconds := int64(*timeout / time.Second)
	resources := []model.ContractResource{{
		ID: "workspace",
		Selector: model.ResourceSelector{
			Kind:    "filesystem",
			Pattern: "workspace/**",
		},
		Operations: []string{"filesystem.read", "filesystem.write"},
		Conditions: model.CapabilityConditions{
			WorkspaceRoot: "workspace",
		},
	}}
	if *modelProvider != "" {
		resources = append(resources, model.ContractResource{
			ID: "model-egress",
			Selector: model.ResourceSelector{
				Kind:    "model",
				Pattern: *modelProvider + "/*",
			},
			Operations: []string{"model.invoke"},
		})
	}
	expectedEnforcementProfile := *requiredEnforcementProfile
	if expectedEnforcementProfile == "" {
		expectedEnforcementProfile = "development"
	}
	admissionRequest := admission.Request{
		Version:                    admission.RequestVersion,
		ExpectedRuntimeID:          runtimeIdentity.RuntimeID,
		ExpectedEnforcementProfile: expectedEnforcementProfile,
		IdempotencyKey:             *id,
		TransactionID:              *id,
		RunID:                      *runID,
		Intent:                     string(intentBytes),
		AgentProfile:               profileBinding,
		Sponsor: model.Principal{
			ID:   *sponsorID,
			Kind: model.PrincipalKind(*sponsorKind),
		},
		Actor: actor,
		Contract: admission.ContractSpec{
			Risk:      "high",
			Resources: resources,
			Budgets: model.BudgetLimits{
				MaxWallTimeSeconds: &maxWallTimeSeconds,
			},
		},
	}
	client := newClient(*socket)
	if kernelClient, ok := client.(*kernelclient.Client); ok {
		client = kernelClient.WithExpectedRuntimeID(
			runtimeIdentity.RuntimeID,
		)
	}
	if *runtimeClass == "oci" {
		preflight, err := client.PreflightRuntime(
			context.Background(),
			*namespace,
			runtimepreflight.Request{
				Version:                    runtimepreflight.RequestVersion,
				ExpectedRuntimeID:          runtimeIdentity.RuntimeID,
				RequiredEnforcementProfile: *requiredEnforcementProfile,
				Agent: &runtimepreflight.AgentSelection{
					Profile:  profileBinding,
					OCIImage: *image,
				},
			},
		)
		if err != nil {
			fmt.Fprintf(stderr, "%s: Runtime preflight failed: %v\n", commandName, err)
			return 1
		}
		admissionRequest.ExpectedEnforcementProfile =
			preflight.EnforcementProfile
	}
	admitted, err := client.AdmitTask(
		context.Background(),
		*namespace,
		admissionRequest,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	projection, err := client.GetTransaction(
		context.Background(),
		*namespace,
		admitted.Transaction.Transaction.ID,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	workspacePath := ""
	if len(projection.Transaction.StageBindings) == 1 {
		workspacePath = projection.Transaction.StageBindings[0].Location
	}
	var execution model.AgentExecution
	if current, ok := latestTransactionAttemptExecution(projection); ok {
		execution = current
	}
	if projection.Transaction.State == model.TransactionStaged {
		validated, validateErr := client.ValidateTransaction(
			context.Background(),
			*namespace,
			*id,
			projection.Transaction.EventSequence,
			actor,
		)
		if validateErr != nil {
			fmt.Fprintln(stderr, validateErr)
			return 1
		}
		return renderTransactionRunResult(transactionRunResult{
			Projection: validated.Projection,
			Workspace:  workspacePath,
			Execution:  execution,
			Decision:   &validated.Decision,
		}, jsonOut, stdout, stderr)
	}
	switch projection.Transaction.State {
	case model.TransactionCreated:
		projection, err = client.StartTransaction(
			context.Background(), *namespace, *id,
			projection.Transaction.EventSequence, actor,
		)
	case model.TransactionValidationFailed, model.TransactionReviseRequired:
		projection, err = client.StartTransaction(
			context.Background(), *namespace, *id,
			projection.Transaction.EventSequence, actor,
		)
	case model.TransactionRunning:
		// Resume below from the latest durable execution receipt.
	default:
		code := renderTransactionRunResult(transactionRunResult{
			Projection: projection,
			Workspace:  workspacePath,
			Execution:  execution,
		}, jsonOut, stdout, stderr)
		if code != 0 || transactionRunStateIsHealthy(projection.Transaction.State) {
			return code
		}
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(projection.Transaction.StageBindings) == 0 {
		worktree, worktreeErr := client.CreateTransactionWorktree(
			context.Background(), *namespace, *id,
			projection.Transaction.EventSequence, *revision, actor,
		)
		if worktreeErr != nil {
			fmt.Fprintln(stderr, worktreeErr)
			return 1
		}
		projection = worktree.Projection
		workspacePath = worktree.Workspace.Path
	} else if len(projection.Transaction.StageBindings) == 1 {
		workspacePath = projection.Transaction.StageBindings[0].Location
	} else {
		fmt.Fprintln(stderr, "transaction has more than one stage boundary")
		return 1
	}
	if !jsonOut {
		fmt.Fprintf(stderr, "Gatemole transaction: %s\nWorkspace: %s\n", *id, workspacePath)
	}
	execution, hasExecution := latestTransactionAttemptExecution(projection)
	if hasExecution && execution.Status == model.AgentExecutionRunning {
		fmt.Fprintln(stderr, "transaction has an active execution; wait for daemon recovery before continuing")
		return 1
	}
	if !hasExecution || execution.Status != model.AgentExecutionSucceeded {
		if *runtimeClass == "oci" {
			signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			processContext, cancel := context.WithTimeout(signalContext, *timeout+30*time.Second)
			defer cancel()
			executed, executeErr := client.RunTransactionAgent(
				processContext,
				*namespace, *id,
				projection.Transaction.EventSequence,
				*image, command, 0, actor,
			)
			if executeErr != nil {
				fmt.Fprintln(stderr, executeErr)
				return 1
			}
			projection = executed.Projection
			execution = executed.Execution
			if !jsonOut {
				fmt.Fprintf(stderr, "Agent evidence: %s\n", executed.EvidenceDirectory)
			}
		} else {
			commandDigest, digestErr := digestCommand(command)
			if digestErr != nil {
				fmt.Fprintln(stderr, digestErr)
				return 1
			}
			invocation, invocationErr := buildSupervisedInvocation(
				"host", "", "", workspacePath, *id, *runID,
				command, commandDigest, 0, 0, 0, 0, 0, 0,
			)
			if invocationErr != nil {
				fmt.Fprintln(stderr, invocationErr)
				return 1
			}
			projection, err = client.StartAgentExecution(
				context.Background(), *namespace, *id,
				projection.Transaction.EventSequence,
				*runID, filepath.Base(command[0]), commandDigest,
				invocation.RuntimeClass, invocation.RuntimeDigest, invocation.ImageDigest,
				actor,
			)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			execution = projection.Executions[len(projection.Executions)-1]
			childStdout := stdout
			if jsonOut {
				childStdout = stderr
			}
			processContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			processContext, cancel := context.WithTimeout(processContext, *timeout)
			defer cancel()
			processResult := superviseTransactionProcess(
				processContext, invocation,
				os.Stdin, childStdout, stderr,
			)
			projection, err = client.FinishAgentExecution(
				context.Background(), *namespace, *id,
				projection.Transaction.EventSequence,
				execution.ID, processResult.Status, processResult.ExitCode,
				processResult.StdoutDigest, processResult.StderrDigest, actor,
			)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			execution = projection.Executions[len(projection.Executions)-1]
		}
	}
	result := transactionRunResult{
		Projection: projection,
		Workspace:  workspacePath,
		Execution:  execution,
	}
	if execution.Status != model.AgentExecutionSucceeded {
		if code := renderTransactionRunResult(result, jsonOut, stdout, stderr); code != 0 {
			return code
		}
		return supervisedProcessExitCode(execution)
	}

	staged, err := client.StageTransaction(
		context.Background(), *namespace, *id,
		projection.Transaction.EventSequence, actor,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if staged.Projection.Transaction.State == model.TransactionCompletedNoEffect {
		result.Projection = staged.Projection
		return renderTransactionRunResult(result, jsonOut, stdout, stderr)
	}
	validated, err := client.ValidateTransaction(
		context.Background(), *namespace, *id,
		staged.Projection.Transaction.EventSequence, actor,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result.Projection = validated.Projection
	result.Decision = &validated.Decision
	return renderTransactionRunResult(result, jsonOut, stdout, stderr)
}

func transactionRunIntent(repo, inline, path string) ([]byte, error) {
	if path == "" {
		return []byte(inline), nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read transaction intent: %w", err)
	}
	return data, nil
}

func generateTransactionID(now time.Time) (string, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate transaction ID: %w", err)
	}
	return fmt.Sprintf(
		"tx:%s-%s",
		now.UTC().Format("20060102T150405Z"),
		hex.EncodeToString(random),
	), nil
}

func digestCommand(command []string) (string, error) {
	data, err := json.Marshal(command)
	if err != nil {
		return "", fmt.Errorf("encode supervised command: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func runtimeRunUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: gatemole [--repo DIR] [--json] run [--namespace NS] [--require-enforcement-profile development|production] [options] -- [AGENT_ARG...]")
	fmt.Fprintln(out, "  run (--intent TEXT | --intent-file FILE) --agent NAME [--agent-profiles FILE] [-- AGENT_ARG...]")
	fmt.Fprintln(out, "  run (--intent TEXT | --intent-file FILE) --image IMAGE -- COMMAND [ARG...]")
	fmt.Fprintln(out, "  run (--intent TEXT | --intent-file FILE) --runtime host --unsafe-host -- COMMAND [ARG...]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "production callers should pass --require-enforcement-profile production")
	fmt.Fprintln(out, "advanced lifecycle: gatemole tx <command>")
	fmt.Fprintln(out, "low-level run records: gatemole kernel run <command>")
}

func buildSupervisedInvocation(
	runtimeClass, containerRuntime, image, workspace, transactionID, runID string,
	command []string,
	commandDigest string,
	uid, gid int,
	memoryBytes, cpuMillis, pidsLimit, tmpfsBytes int64,
) (supervisedInvocation, error) {
	if runtimeClass == "host" {
		runtimeDigest, err := digestRuntimeConfiguration(struct {
			Version       string `json:"version"`
			CommandDigest string `json:"command_digest"`
			Authority     string `json:"authority"`
		}{
			Version:       "gatemole.host_runtime_config.v0",
			CommandDigest: commandDigest,
			Authority:     "ambient_host",
		})
		if err != nil {
			return supervisedInvocation{}, err
		}
		return supervisedInvocation{
			Executable:    command[0],
			Arguments:     append([]string(nil), command[1:]...),
			Directory:     workspace,
			RuntimeClass:  "host",
			RuntimeDigest: runtimeDigest,
		}, nil
	}
	enginePath, err := exec.LookPath(containerRuntime)
	if err != nil {
		return supervisedInvocation{}, fmt.Errorf("find OCI runtime %q: %w", containerRuntime, err)
	}
	enginePath, err = filepath.Abs(enginePath)
	if err != nil {
		return supervisedInvocation{}, fmt.Errorf("resolve OCI runtime path: %w", err)
	}
	config := sandbox.OCIConfig{
		EnginePath:    enginePath,
		Image:         image,
		Workspace:     workspace,
		TransactionID: transactionID,
		RunID:         runID,
		Command:       append([]string(nil), command...),
		UID:           uid,
		GID:           gid,
		MemoryBytes:   memoryBytes,
		CPUMillis:     cpuMillis,
		PIDsLimit:     pidsLimit,
		TmpfsBytes:    tmpfsBytes,
		ContainerName: sandbox.ContainerName(transactionID, runID),
		Role:          "agent",
		WorkspaceMode: "transaction_rw",
	}
	ociInvocation, err := config.Invocation()
	if err != nil {
		return supervisedInvocation{}, err
	}
	runtimeDigest, err := config.RuntimeConfigDigest()
	if err != nil {
		return supervisedInvocation{}, err
	}
	imageDigest, err := sandbox.ImageDigest(image)
	if err != nil {
		return supervisedInvocation{}, err
	}
	return supervisedInvocation{
		Executable:    ociInvocation.Executable,
		Arguments:     ociInvocation.Arguments,
		RuntimeClass:  "oci",
		RuntimeDigest: runtimeDigest,
		ImageDigest:   imageDigest,
		Cleanup: func() {
			cleanupOCIContainer(ociInvocation.Executable, ociInvocation.ContainerName)
		},
	}, nil
}

func digestRuntimeConfiguration(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode runtime configuration: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func superviseTransactionProcess(
	ctx context.Context,
	invocation supervisedInvocation,
	stdin io.Reader,
	stdout, stderr io.Writer,
) supervisedProcessResult {
	stdoutHash := sha256.New()
	stderrHash := sha256.New()
	cmd := exec.CommandContext(ctx, invocation.Executable, invocation.Arguments...)
	cmd.Dir = invocation.Directory
	cmd.Stdin = stdin
	cmd.Stdout = digestingWriter(stdout, stdoutHash)
	cmd.Stderr = digestingWriter(stderr, stderrHash)
	result := supervisedProcessResult{}
	if err := cmd.Start(); err != nil {
		result.Status = model.AgentExecutionStartFailed
		result.StdoutDigest = encodedHash(stdoutHash)
		result.StderrDigest = encodedHash(stderrHash)
		return result
	}
	err := cmd.Wait()
	if ctx.Err() != nil && invocation.Cleanup != nil {
		invocation.Cleanup()
	}
	switch {
	case ctx.Err() != nil:
		result.Status = model.AgentExecutionInterrupted
	case err == nil:
		code := 0
		result.Status = model.AgentExecutionSucceeded
		result.ExitCode = &code
	default:
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			code := exitError.ExitCode()
			result.Status = model.AgentExecutionFailed
			result.ExitCode = &code
		} else {
			result.Status = model.AgentExecutionInterrupted
		}
	}
	result.StdoutDigest = encodedHash(stdoutHash)
	result.StderrDigest = encodedHash(stderrHash)
	return result
}

func cleanupOCIContainer(enginePath, containerName string) {
	arguments, err := sandbox.CleanupArguments(containerName)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, enginePath, arguments...)
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	_ = command.Run()
}

func digestingWriter(writer io.Writer, digest hash.Hash) io.Writer {
	if writer == nil {
		writer = io.Discard
	}
	return io.MultiWriter(writer, digest)
}

func encodedHash(digest hash.Hash) string {
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func supervisedProcessExitCode(execution model.AgentExecution) int {
	if execution.ExitCode != nil && *execution.ExitCode > 0 && *execution.ExitCode < 256 {
		return *execution.ExitCode
	}
	if execution.Status == model.AgentExecutionInterrupted {
		return 130
	}
	return 1
}

func latestTransactionAttemptExecution(
	projection transactionreducer.Projection,
) (model.AgentExecution, bool) {
	for index := len(projection.Executions) - 1; index >= 0; index-- {
		if projection.Executions[index].Attempt == projection.Transaction.Attempt {
			return projection.Executions[index], true
		}
	}
	return model.AgentExecution{}, false
}

func transactionRunStateIsHealthy(state model.TransactionState) bool {
	switch state {
	case model.TransactionValidating,
		model.TransactionPendingApproval,
		model.TransactionReadyToCommit,
		model.TransactionCommitting,
		model.TransactionCommitted,
		model.TransactionCompletedNoEffect:
		return true
	default:
		return false
	}
}

func renderTransactionRunResult(
	result transactionRunResult,
	jsonOut bool,
	stdout, stderr io.Writer,
) int {
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprintf(
		stdout,
		"Agent execution: %s\nTransaction state: %s\nAttempt: %d\nEffects: %d\n",
		result.Execution.Status,
		result.Projection.Transaction.State,
		result.Projection.Transaction.Attempt,
		len(result.Projection.Effects),
	)
	if result.Decision != nil {
		fmt.Fprintf(stdout, "Sequence decision: %s\n", result.Decision.Outcome)
		for _, finding := range result.Decision.Findings {
			fmt.Fprintf(stdout, "- %s: %s\n", finding.RuleID, finding.Summary)
		}
	}
	return 0
}
