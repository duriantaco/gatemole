package gatemole

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
	kernelverification "github.com/duriantaco/gatemole/internal/kernel/verification"
)

type transactionVerifyResult struct {
	Projection        transactionreducer.Projection `json:"projection"`
	Verification      model.VerificationResult      `json:"verification"`
	EvidenceDirectory string                        `json:"evidence_directory"`
	Process           verificationReceipt           `json:"process"`
}

type verificationReceipt = kernelverification.ProcessReceipt

func transactionVerify(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
	newClient transactionClientFactory,
) int {
	flags := flag.NewFlagSet("tx verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "transaction namespace")
	id := flags.String("id", "", "transaction ID")
	name := flags.String("name", "", "stable verifier name")
	image := flags.String("image", "", "digest-pinned verifier OCI image")
	timeout := flags.Duration("timeout", 15*time.Minute, "maximum verifier duration")
	socket := flags.String("socket", defaultKernelSocket(repo), "gatemoled Unix socket")
	actorID := flags.String("actor", "operator:local", "principal ID requesting verification")
	actorKind := flags.String("actor-kind", string(model.PrincipalOperator), "requesting principal kind")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	command := flags.Args()
	if *namespace == "" || *id == "" || *name == "" || *image == "" ||
		len(command) == 0 || *timeout < time.Second {
		fmt.Fprintln(stderr, "tx verify requires --namespace, --id, --name, --image, a positive --timeout, and a command after --")
		return 2
	}
	if !model.IsIdentifier(*name) {
		fmt.Fprintln(stderr, "tx verify --name must be a kernel identifier")
		return 2
	}
	client := newClient(*socket)
	projection, err := client.GetTransaction(context.Background(), *namespace, *id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if (projection.Transaction.State != model.TransactionValidating &&
		projection.Transaction.State != model.TransactionValidationFailed) ||
		len(projection.Transaction.StageBindings) != 1 {
		fmt.Fprintln(stderr, "tx verify requires a validating or verification-failed transaction with exactly one staged workspace")
		return 1
	}
	processContext, cancel := context.WithTimeout(
		context.Background(), *timeout+30*time.Second,
	)
	defer cancel()
	actor := model.Principal{ID: *actorID, Kind: model.PrincipalKind(*actorKind)}
	recorded, err := client.RunTransactionVerification(
		processContext,
		*namespace,
		*id,
		projection.Transaction.EventSequence,
		*name,
		*image,
		command,
		int64(*timeout/time.Second),
		actor,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result := transactionVerifyResult{
		Projection:        recorded.Projection,
		Verification:      recorded.Result,
		EvidenceDirectory: recorded.EvidenceDirectory,
		Process:           recorded.Process,
	}
	if jsonOut {
		if code := renderCommandJSON(result, stdout, stderr); code != 0 {
			return code
		}
	} else {
		fmt.Fprintf(
			stdout,
			"Verification %s: %s\nTransaction state: %s\nEvidence: %s\n",
			recorded.Result.Name,
			recorded.Result.Status,
			recorded.Projection.Transaction.State,
			recorded.EvidenceDirectory,
		)
	}
	if recorded.Result.Status != model.VerificationPassed {
		return supervisedProcessExitCode(model.AgentExecution{
			Status: recorded.Process.Status, ExitCode: recorded.Process.ExitCode,
		})
	}
	return 0
}
