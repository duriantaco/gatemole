package gatemole

import (
	"context"
	"fmt"
	"io"
	"strings"

	kernelclient "github.com/duriantaco/gatemole/internal/kernel/client"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
)

const transactionReviewVersion = "gatemole.transaction_review.v1"

type transactionReviewResult struct {
	Version         string                              `json:"version"`
	Projection      transactionreducer.Projection       `json:"projection"`
	Diff            *kernelclient.TransactionDiffResult `json:"diff,omitempty"`
	DiffUnavailable string                              `json:"diff_unavailable,omitempty"`
}

func transactionReview(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
	newClient transactionClientFactory,
) int {
	values, ok := parseTransactionTarget("tx review", repo, args, stderr)
	if !ok {
		return 2
	}
	client := newClient(values.socket)
	projection, err := client.GetTransaction(
		context.Background(), values.namespace, values.id,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result := transactionReviewResult{
		Version:    transactionReviewVersion,
		Projection: projection,
	}
	if reason := transactionDiffUnavailable(projection); reason != "" {
		result.DiffUnavailable = reason
	} else {
		diff, err := client.GetTransactionDiff(
			context.Background(), values.namespace, values.id,
		)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		current, err := client.GetTransaction(
			context.Background(), values.namespace, values.id,
		)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if !diffMatchesProjection(diff, current) {
			fmt.Fprintln(stderr, "transaction changed while its review was assembled; retry the command")
			return 1
		}
		result.Projection = current
		result.Diff = &diff
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	return renderTransactionReview(result, stdout, stderr)
}

func transactionDiff(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
	newClient transactionClientFactory,
) int {
	values, ok := parseTransactionTarget("tx diff", repo, args, stderr)
	if !ok {
		return 2
	}
	diff, err := newClient(values.socket).GetTransactionDiff(
		context.Background(), values.namespace, values.id,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(diff, stdout, stderr)
	}
	if _, err := stdout.Write(diff.Patch); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func transactionDiffUnavailable(projection transactionreducer.Projection) string {
	switch {
	case projection.Transaction.State == model.TransactionAborted:
		return "the transaction was rejected and its isolated worktree was discarded"
	case len(projection.Effects) == 0:
		return "the transaction has no frozen Git effects"
	case projection.Transaction.StagedStateDigest == "" ||
		projection.Transaction.EffectSetDigest == "":
		return "the transaction has not frozen a Git effect set"
	default:
		return ""
	}
}

func diffMatchesProjection(
	diff kernelclient.TransactionDiffResult,
	projection transactionreducer.Projection,
) bool {
	metadata := diff.Metadata
	transaction := projection.Transaction
	return metadata.Namespace == transaction.Namespace &&
		metadata.TransactionID == transaction.ID &&
		metadata.Attempt == transaction.Attempt &&
		metadata.EventSequence == transaction.EventSequence &&
		metadata.StagedStateDigest == transaction.StagedStateDigest &&
		metadata.EffectSetDigest == transaction.EffectSetDigest
}

func renderTransactionReview(
	review transactionReviewResult,
	stdout, stderr io.Writer,
) int {
	projection := review.Projection
	transaction := projection.Transaction
	fmt.Fprintf(
		stdout,
		"Transaction %s in %s\nState: %s (attempt %d, sequence %d)\n",
		transaction.ID,
		transaction.Namespace,
		transaction.State,
		transaction.Attempt,
		transaction.EventSequence,
	)
	if transaction.Task != nil {
		fmt.Fprintf(stdout, "Intent: %q\n", transaction.Task.Intent)
	}
	fmt.Fprintf(
		stdout,
		"Effects: %d\nStaged state: %s\nEffect set: %s\n",
		len(projection.Effects),
		valueOrNone(transaction.StagedStateDigest),
		valueOrNone(transaction.EffectSetDigest),
	)
	for _, effect := range projection.Effects {
		fmt.Fprintf(
			stdout,
			"  - #%d %s [%s] %s %q (%s; %s)\n",
			effect.Sequence,
			effect.ID,
			effect.Status,
			effect.Operation,
			effect.Resource.Pattern,
			effect.System,
			effect.RecoveryClass,
		)
	}
	fmt.Fprintf(stdout, "Verifications: %d\n", len(projection.Verifications))
	for _, verification := range projection.Verifications {
		fmt.Fprintf(
			stdout,
			"  - %s [%s] %s: %q\n",
			verification.ID,
			verification.Status,
			verification.Name,
			verification.Summary,
		)
		for _, evidence := range verification.Evidence {
			fmt.Fprintf(
				stdout,
				"    evidence: %q (%s)\n",
				evidence.URI,
				evidence.Digest,
			)
		}
	}
	fmt.Fprintf(
		stdout,
		"Approval package: %s\nApproval decisions: %s\nOutstanding approvals: %s\n",
		valueOrNone(transaction.ApprovalPackageDigest),
		listOrNone(projection.ApprovalDigests),
		listOrNone(transaction.OutstandingApprovalIDs),
	)
	if review.Diff == nil {
		fmt.Fprintf(stdout, "Exact diff: unavailable (%s)\n", review.DiffUnavailable)
	} else {
		metadata := review.Diff.Metadata
		fmt.Fprintf(
			stdout,
			"Exact diff: %s\nBase: %s\nTree: %s\n\n",
			metadata.PatchDigest,
			metadata.BaseRevision,
			metadata.TreeRevision,
		)
		if _, err := stdout.Write(review.Diff.Patch); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if len(review.Diff.Patch) > 0 && review.Diff.Patch[len(review.Diff.Patch)-1] != '\n' {
			fmt.Fprintln(stdout)
		}
	}
	if next := transactionReviewNextStep(transaction); next != "" {
		fmt.Fprintf(stdout, "\nNext: %s\n", next)
	}
	return 0
}

func transactionReviewNextStep(transaction model.AgentTransaction) string {
	switch transaction.State {
	case model.TransactionPendingApproval:
		return "approve the exact approval package, then run `gatemole apply " + transaction.ID + "`"
	case model.TransactionReadyToCommit:
		return "run `gatemole apply " + transaction.ID + "` or `gatemole reject " + transaction.ID + "`"
	case model.TransactionCommitted:
		return "the approved Git ref update has already been applied"
	case model.TransactionAborted:
		return "the transaction has been rejected"
	case model.TransactionCompletedNoEffect:
		return "there are no effects to apply"
	case model.TransactionValidating, model.TransactionValidationFailed:
		return "complete the required verification and prepare an approval package"
	case model.TransactionStaged:
		return "validate the frozen effect sequence"
	default:
		return "wait for the transaction to reach a reviewable staged state"
	}
}

func valueOrNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func listOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}
