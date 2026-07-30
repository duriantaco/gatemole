package vouch

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/admission"
	kernelclient "github.com/duriantaco/vouch/internal/kernel/client"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/runtimeidentity"
	"github.com/duriantaco/vouch/internal/kernel/runtimepreflight"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

type transactionClient interface {
	PreflightRuntime(
		context.Context,
		string,
		runtimepreflight.Request,
	) (runtimepreflight.Result, error)
	AdmitTask(context.Context, string, admission.Request) (admission.Result, error)
	GetTransaction(context.Context, string, string) (transactionreducer.Projection, error)
	ListTransactions(context.Context, string) ([]model.AgentTransaction, error)
	TransactionEvents(context.Context, string, string, int64) ([]model.TransactionEvent, error)
	StartTransaction(context.Context, string, string, int64, model.Principal) (transactionreducer.Projection, error)
	CreateTransactionWorktree(context.Context, string, string, int64, string, model.Principal) (kernelclient.TransactionWorktreeResult, error)
	StartAgentExecution(context.Context, string, string, int64, string, string, string, string, string, string, model.Principal) (transactionreducer.Projection, error)
	FinishAgentExecution(context.Context, string, string, int64, string, model.AgentExecutionStatus, *int, string, string, model.Principal) (transactionreducer.Projection, error)
	RunTransactionAgent(context.Context, string, string, int64, string, []string, int64, model.Principal) (kernelclient.TransactionAgentRunResult, error)
	StageTransaction(context.Context, string, string, int64, model.Principal) (kernelclient.TransactionStageResult, error)
	ValidateTransaction(context.Context, string, string, int64, model.Principal) (kernelclient.TransactionValidationResult, error)
	RecordTransactionVerification(context.Context, string, string, int64, string, model.VerificationStatus, string, string, string, []model.ArtifactRef, string, model.Principal) (kernelclient.TransactionVerificationRecordResult, error)
	RunTransactionVerification(context.Context, string, string, int64, string, string, []string, int64, model.Principal) (kernelclient.TransactionVerificationRunResult, error)
	PrepareTransaction(context.Context, string, string, int64, string, model.Principal) (kernelclient.TransactionPrepareResult, error)
	ResolveTransactionApproval(context.Context, string, string, int64, model.ApprovalDecision) (transactionreducer.Projection, error)
	ReleaseTransaction(context.Context, string, string, int64, model.Principal) (kernelclient.TransactionReleaseResult, error)
	AbortTransaction(context.Context, string, string, int64, model.Principal) (kernelclient.TransactionAbortResult, error)
}

type transactionClientFactory func(string) transactionClient

func transactionCommand(repo string, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	factory, err := runtimeBoundTransactionClientFactory(repo)
	if err != nil {
		fmt.Fprintf(stderr, "tx: %v\n", err)
		return 1
	}
	return transactionCommandWithFactory(
		repo, args, jsonOut, stdout, stderr, factory,
	)
}

func transactionStatusCommand(repo string, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	factory, err := runtimeBoundTransactionClientFactory(repo)
	if err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}
	return transactionAliasCommandWithFactory(
		"status", repo, args, jsonOut, stdout, stderr, factory,
	)
}

func transactionApproveCommand(repo string, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	factory, err := runtimeBoundTransactionClientFactory(repo)
	if err != nil {
		fmt.Fprintf(stderr, "approve: %v\n", err)
		return 1
	}
	return transactionAliasCommandWithFactory(
		"approve", repo, args, jsonOut, stdout, stderr, factory,
	)
}

func transactionReleaseCommand(repo string, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	factory, err := runtimeBoundTransactionClientFactory(repo)
	if err != nil {
		fmt.Fprintf(stderr, "release: %v\n", err)
		return 1
	}
	return transactionAliasCommandWithFactory(
		"release", repo, args, jsonOut, stdout, stderr, factory,
	)
}

func runtimeBoundTransactionClientFactory(
	repo string,
) (transactionClientFactory, error) {
	identity, err := runtimeidentity.Load(context.Background(), repo)
	if err != nil {
		return nil, fmt.Errorf(
			"load Runtime identity; run `vouch runtime init` first: %w",
			err,
		)
	}
	return func(socket string) transactionClient {
		return kernelclient.New(socket).
			WithExpectedRuntimeID(identity.RuntimeID)
	}, nil
}

// transactionAliasCommandWithFactory keeps the public runtime verbs as thin
// aliases over the same transaction operations used by the advanced tx
// surface. There is deliberately no second lifecycle or hidden automation.
func transactionAliasCommandWithFactory(
	alias, repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
	newClient transactionClientFactory,
) int {
	normalized, err := normalizeTopLevelTransactionArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", alias, err)
		return 2
	}
	switch alias {
	case "status":
		return transactionGet(repo, normalized, jsonOut, stdout, stderr, newClient)
	case "approve":
		return transactionApprove(repo, normalized, jsonOut, stdout, stderr, newClient)
	case "release":
		return transactionRelease(repo, normalized, jsonOut, stdout, stderr, newClient)
	default:
		fmt.Fprintf(stderr, "unknown transaction alias %q\n", alias)
		return 2
	}
}

// normalizeTopLevelTransactionArgs makes the product-facing commands concise:
// `vouch status TX_ID` targets the local namespace by default. The advanced
// `vouch tx` surface retains its explicit --namespace/--id contract.
func normalizeTopLevelTransactionArgs(args []string) ([]string, error) {
	normalized := append([]string(nil), args...)
	if len(normalized) > 0 && !strings.HasPrefix(normalized[0], "-") {
		if hasCommandFlag(normalized[1:], "id") {
			return nil, errors.New("transaction ID cannot be both positional and supplied with --id")
		}
		normalized = append([]string{"--id", normalized[0]}, normalized[1:]...)
	}
	if !hasCommandFlag(normalized, "namespace") {
		normalized = append([]string{"--namespace", "local"}, normalized...)
	}
	return normalized, nil
}

func hasCommandFlag(args []string, name string) bool {
	for _, argument := range args {
		if argument == "-"+name || argument == "--"+name ||
			strings.HasPrefix(argument, "-"+name+"=") ||
			strings.HasPrefix(argument, "--"+name+"=") {
			return true
		}
	}
	return false
}

func transactionCommandWithFactory(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
	newClient transactionClientFactory,
) int {
	if len(args) == 0 {
		transactionUsage(stderr)
		return 2
	}
	switch args[0] {
	case "run":
		return transactionRun(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "create":
		return transactionCreate(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "start":
		return transactionStart(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "worktree":
		return transactionWorktree(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "stage":
		return transactionStage(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "validate":
		return transactionValidate(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "verify":
		return transactionVerify(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "prepare":
		return transactionPrepare(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "approve":
		return transactionApprove(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "release":
		return transactionRelease(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "get":
		return transactionGet(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "list":
		return transactionList(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "effects":
		return transactionEffects(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "events":
		return transactionEvents(repo, args[1:], jsonOut, stdout, stderr, newClient)
	case "abort":
		return transactionAbort(repo, args[1:], jsonOut, stdout, stderr, newClient)
	default:
		fmt.Fprintf(stderr, "tx: unknown command %q\n", args[0])
		transactionUsage(stderr)
		return 2
	}
}

func transactionCreate(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	flags := flag.NewFlagSet("tx create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	id := flags.String("id", "", "transaction ID")
	namespace := flags.String("namespace", "", "transaction namespace")
	intent := flags.String("intent", "", "human-owned task intent")
	intentFile := flags.String("intent-file", "", "file containing task intent")
	sponsorID := flags.String("sponsor", "human:local", "human or service sponsor ID")
	sponsorKind := flags.String("sponsor-kind", string(model.PrincipalHuman), "sponsor principal kind")
	runID := flags.String("run", "", "participating agent run ID")
	socket := flags.String("socket", defaultKernelSocket(repo), "gatemoled Unix socket")
	actorID := flags.String("actor", "operator:local", "principal ID creating the transaction")
	actorKind := flags.String("actor-kind", string(model.PrincipalOperator), "creator principal kind")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *id == "" || *namespace == "" || (*intent == "") == (*intentFile == "") || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "tx create requires --id, --namespace, and exactly one of --intent or --intent-file")
		return 2
	}
	intentBytes := []byte(*intent)
	if *intentFile != "" {
		path := *intentFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(repo, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		intentBytes = data
	}
	runtimeIdentity, err := runtimeidentity.Load(context.Background(), repo)
	if err != nil {
		fmt.Fprintf(
			stderr,
			"tx create: load Runtime identity; run `vouch runtime init` first: %v\n",
			err,
		)
		return 1
	}
	if *runID == "" {
		*runID = "run:" + *id
	}
	manualProfile, err := resolveAdHocAgentProfile(
		"host",
		"",
		[]string{"gatemole-manual-transaction"},
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	profileBinding, err := manualProfile.binding()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	request := admission.Request{
		Version:                    admission.RequestVersion,
		ExpectedRuntimeID:          runtimeIdentity.RuntimeID,
		ExpectedEnforcementProfile: "development",
		IdempotencyKey:             *id,
		TransactionID:              *id,
		RunID:                      *runID,
		Intent:                     string(intentBytes),
		AgentProfile:               profileBinding,
		Sponsor: model.Principal{
			ID:   *sponsorID,
			Kind: model.PrincipalKind(*sponsorKind),
		},
		Actor: model.Principal{
			ID:   *actorID,
			Kind: model.PrincipalKind(*actorKind),
		},
		Contract: admission.ContractSpec{
			Risk: "high",
			Resources: []model.ContractResource{{
				ID: "workspace",
				Selector: model.ResourceSelector{
					Kind:    "filesystem",
					Pattern: "workspace/**",
				},
				Operations: []string{
					"filesystem.read",
					"filesystem.write",
				},
				Conditions: model.CapabilityConditions{
					WorkspaceRoot: "workspace",
				},
			}},
		},
	}
	client := newClient(*socket)
	if kernelClient, ok := client.(*kernelclient.Client); ok {
		client = kernelClient.WithExpectedRuntimeID(
			runtimeIdentity.RuntimeID,
		)
	}
	admitted, err := client.AdmitTask(
		context.Background(),
		*namespace,
		request,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return renderTransactionProjection(
		admitted.Transaction,
		jsonOut,
		"Created",
		stdout,
		stderr,
	)
}

func transactionStart(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	values, ok := parseTransactionTarget("tx start", repo, args, stderr)
	if !ok {
		return 2
	}
	client := newClient(values.socket)
	projection, err := client.GetTransaction(context.Background(), values.namespace, values.id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	next, err := client.StartTransaction(context.Background(), values.namespace, values.id, projection.Transaction.EventSequence, values.actor)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return renderTransactionProjection(next, jsonOut, "Started", stdout, stderr)
}

func transactionWorktree(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	flags := flag.NewFlagSet("tx worktree", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "transaction namespace")
	id := flags.String("id", "", "transaction ID")
	revision := flags.String("revision", "HEAD", "base Git revision")
	socket := flags.String("socket", defaultKernelSocket(repo), "gatemoled Unix socket")
	actorID := flags.String("actor", "operator:local", "requesting principal ID")
	actorKind := flags.String("actor-kind", string(model.PrincipalOperator), "requesting principal kind")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || *id == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "tx worktree requires --namespace and --id")
		return 2
	}
	client := newClient(*socket)
	projection, err := client.GetTransaction(context.Background(), *namespace, *id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result, err := client.CreateTransactionWorktree(
		context.Background(), *namespace, *id, projection.Transaction.EventSequence, *revision,
		model.Principal{ID: *actorID, Kind: model.PrincipalKind(*actorKind)},
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Transaction worktree: %s\nBase revision: %s\n", result.Workspace.Path, result.Workspace.BaseRevision)
	return 0
}

func transactionStage(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	values, ok := parseTransactionTarget("tx stage", repo, args, stderr)
	if !ok {
		return 2
	}
	client := newClient(values.socket)
	projection, err := client.GetTransaction(context.Background(), values.namespace, values.id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result, err := client.StageTransaction(context.Background(), values.namespace, values.id, projection.Transaction.EventSequence, values.actor)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Staged %d effects for %s\nEffect set: %s\nStaged state: %s\n", len(result.Projection.Effects), values.id, result.Projection.Transaction.EffectSetDigest, result.Projection.Transaction.StagedStateDigest)
	return 0
}

func transactionValidate(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	values, ok := parseTransactionTarget("tx validate", repo, args, stderr)
	if !ok {
		return 2
	}
	client := newClient(values.socket)
	projection, err := client.GetTransaction(context.Background(), values.namespace, values.id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result, err := client.ValidateTransaction(context.Background(), values.namespace, values.id, projection.Transaction.EventSequence, values.actor)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Sequence decision for %s: %s\n", values.id, result.Decision.Outcome)
	for _, finding := range result.Decision.Findings {
		fmt.Fprintf(stdout, "- %s: %s\n", finding.RuleID, finding.Summary)
	}
	fmt.Fprintf(stdout, "Transaction state: %s\n", result.Projection.Transaction.State)
	return 0
}

func transactionPrepare(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	flags := flag.NewFlagSet("tx prepare", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "transaction namespace")
	id := flags.String("id", "", "transaction ID")
	gitRef := flags.String("git-ref", "", "full Git branch ref to bind into the commit plan")
	socket := flags.String("socket", defaultKernelSocket(repo), "gatemoled Unix socket")
	actorID := flags.String("actor", "operator:local", "requesting principal ID")
	actorKind := flags.String("actor-kind", string(model.PrincipalOperator), "requesting principal kind")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || *id == "" || *gitRef == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "tx prepare requires --namespace, --id, and --git-ref")
		return 2
	}
	actor := model.Principal{ID: *actorID, Kind: model.PrincipalKind(*actorKind)}
	client := newClient(*socket)
	projection, err := client.GetTransaction(context.Background(), *namespace, *id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result, err := client.PrepareTransaction(
		context.Background(), *namespace, *id,
		projection.Transaction.EventSequence, *gitRef, actor,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprintf(
		stdout,
		"Prepared %s: state=%s plan=%s approval_package=%s\n",
		*id,
		result.Projection.Transaction.State,
		result.Projection.Transaction.CommitPlanDigest,
		result.Projection.Transaction.ApprovalPackageDigest,
	)
	return 0
}

func transactionGet(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	values, ok := parseTransactionTarget("tx get", repo, args, stderr)
	if !ok {
		return 2
	}
	projection, err := newClient(values.socket).GetTransaction(context.Background(), values.namespace, values.id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return renderTransactionProjection(projection, jsonOut, "Transaction", stdout, stderr)
}

func transactionRelease(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	values, ok := parseTransactionTarget("tx release", repo, args, stderr)
	if !ok {
		return 2
	}
	client := newClient(values.socket)
	projection, err := client.GetTransaction(context.Background(), values.namespace, values.id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result, err := client.ReleaseTransaction(
		context.Background(), values.namespace, values.id,
		projection.Transaction.EventSequence, values.actor,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		if code := renderCommandJSON(result, stdout, stderr); code != 0 {
			return code
		}
	} else {
		fmt.Fprintf(
			stdout,
			"Git release %s: %s -> %s (%s); transaction=%s\n",
			result.Prepared.TargetRef,
			result.Prepared.ExpectedRevision,
			result.Prepared.CommitRevision,
			result.Publish.Status,
			result.Projection.Transaction.State,
		)
		if result.Message != "" {
			fmt.Fprintf(stderr, "release: %s\n", result.Message)
		}
	}
	if result.Projection.Transaction.State != model.TransactionCommitted {
		return 1
	}
	return 0
}

func transactionList(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	flags := flag.NewFlagSet("tx list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "transaction namespace")
	socket := flags.String("socket", defaultKernelSocket(repo), "gatemoled Unix socket")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "tx list requires --namespace")
		return 2
	}
	transactions, err := newClient(*socket).ListTransactions(context.Background(), *namespace)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(transactions, stdout, stderr)
	}
	for _, transaction := range transactions {
		fmt.Fprintf(stdout, "%s\t%s\teffects=%d\tsequence=%d\n", transaction.ID, transaction.State, len(transaction.EffectIDs), transaction.EventSequence)
	}
	return 0
}

func transactionEffects(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	values, ok := parseTransactionTarget("tx effects", repo, args, stderr)
	if !ok {
		return 2
	}
	projection, err := newClient(values.socket).GetTransaction(context.Background(), values.namespace, values.id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(projection.Effects, stdout, stderr)
	}
	for _, effect := range projection.Effects {
		fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\t%s\n", effect.Sequence, effect.System, effect.Operation, effect.Resource.Pattern, effect.RecoveryClass)
	}
	return 0
}

func transactionEvents(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	flags := flag.NewFlagSet("tx events", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "transaction namespace")
	id := flags.String("id", "", "transaction ID")
	after := flags.Int64("after", 0, "only events after this sequence")
	socket := flags.String("socket", defaultKernelSocket(repo), "gatemoled Unix socket")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || *id == "" || *after < 0 || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "tx events requires --namespace and --id; --after must be non-negative")
		return 2
	}
	events, err := newClient(*socket).TransactionEvents(context.Background(), *namespace, *id, *after)
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

func transactionAbort(repo string, args []string, jsonOut bool, stdout, stderr io.Writer, newClient transactionClientFactory) int {
	values, ok := parseTransactionTarget("tx abort", repo, args, stderr)
	if !ok {
		return 2
	}
	client := newClient(values.socket)
	projection, err := client.GetTransaction(context.Background(), values.namespace, values.id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result, err := client.AbortTransaction(context.Background(), values.namespace, values.id, projection.Transaction.EventSequence, values.actor)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Aborted %s; workspace_removed=%t\n", values.id, result.WorkspaceRemoved)
	if result.CleanupWarning != "" {
		fmt.Fprintf(stderr, "warning: %s\n", result.CleanupWarning)
	}
	return 0
}

type transactionTarget struct {
	namespace string
	id        string
	socket    string
	actor     model.Principal
}

func parseTransactionTarget(command, repo string, args []string, stderr io.Writer) (transactionTarget, bool) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "transaction namespace")
	id := flags.String("id", "", "transaction ID")
	socket := flags.String("socket", defaultKernelSocket(repo), "gatemoled Unix socket")
	actorID := flags.String("actor", "operator:local", "requesting principal ID")
	actorKind := flags.String("actor-kind", string(model.PrincipalOperator), "requesting principal kind")
	if err := flags.Parse(args); err != nil {
		return transactionTarget{}, false
	}
	if *namespace == "" || *id == "" || flags.NArg() != 0 {
		fmt.Fprintf(stderr, "%s requires --namespace and --id\n", command)
		return transactionTarget{}, false
	}
	return transactionTarget{
		namespace: *namespace,
		id:        *id,
		socket:    *socket,
		actor:     model.Principal{ID: *actorID, Kind: model.PrincipalKind(*actorKind)},
	}, true
}

func renderTransactionProjection(
	projection transactionreducer.Projection,
	jsonOut bool,
	verb string,
	stdout, stderr io.Writer,
) int {
	if jsonOut {
		return renderCommandJSON(projection, stdout, stderr)
	}
	fmt.Fprintf(
		stdout,
		"%s %s in %s: state=%s effects=%d sequence=%d\n",
		verb,
		projection.Transaction.ID,
		projection.Transaction.Namespace,
		projection.Transaction.State,
		len(projection.Effects),
		projection.Transaction.EventSequence,
	)
	return 0
}

func transactionUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: vouch [--repo DIR] [--json] tx <command>")
	fmt.Fprintln(out, "  tx run [--id ID] [--namespace NS] [--require-enforcement-profile development|production] (--intent TEXT | --intent-file FILE) [--agent NAME | --image IMAGE] [--run RUN] -- [COMMAND_OR_AGENT_ARG...]")
	fmt.Fprintln(out, "  tx create --id ID --namespace NS (--intent TEXT | --intent-file FILE) [--run RUN] (development-only manual Runtime-bound v1 admission)")
	fmt.Fprintln(out, "  tx start --namespace NS --id ID")
	fmt.Fprintln(out, "  tx worktree --namespace NS --id ID [--revision REV]")
	fmt.Fprintln(out, "  tx stage --namespace NS --id ID")
	fmt.Fprintln(out, "  tx validate --namespace NS --id ID")
	fmt.Fprintln(out, "  tx verify --namespace NS --id ID --name NAME --image IMAGE -- COMMAND [ARG...]")
	fmt.Fprintln(out, "  tx prepare --namespace NS --id ID --git-ref refs/heads/BRANCH")
	fmt.Fprintln(out, "  tx approve --namespace NS --id ID --key FILE --key-id ID --approver ID --class CLASS [--decision approve|reject|revise]")
	fmt.Fprintln(out, "  tx release --namespace NS --id ID")
	fmt.Fprintln(out, "  tx get|effects|events|abort --namespace NS --id ID")
	fmt.Fprintln(out, "  tx list --namespace NS")
}
