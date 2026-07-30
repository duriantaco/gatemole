package verification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/sandbox"
)

const CaptureLimit = int64(16 << 20)

type ProcessReceipt struct {
	Version             string                     `json:"version"`
	Name                string                     `json:"name"`
	Status              model.AgentExecutionStatus `json:"status"`
	ExitCode            *int                       `json:"exit_code,omitempty"`
	CommandDigest       string                     `json:"command_digest"`
	RuntimeConfigDigest string                     `json:"runtime_config_digest"`
	ImageDigest         string                     `json:"image_digest"`
	StdoutDigest        string                     `json:"stdout_digest"`
	StderrDigest        string                     `json:"stderr_digest"`
	StdoutTruncated     bool                       `json:"stdout_truncated"`
	StderrTruncated     bool                       `json:"stderr_truncated"`
	CompletedAt         time.Time                  `json:"completed_at"`
}

type Outcome struct {
	Receipt           ProcessReceipt      `json:"process"`
	Evidence          []model.ArtifactRef `json:"evidence"`
	EvidenceDirectory string              `json:"evidence_directory"`
}

type Runner struct {
	EvidenceRoot string
	CaptureLimit int64
}

type CleanupError struct {
	ContainerName string
	Cause         error
}

func (err *CleanupError) Error() string {
	return fmt.Sprintf("remove interrupted OCI container %s: %v", err.ContainerName, err.Cause)
}

func (err *CleanupError) Unwrap() error {
	return err.Cause
}

func IsCleanupError(err error) bool {
	var cleanupError *CleanupError
	return errors.As(err, &cleanupError)
}

func (runner Runner) Run(
	ctx context.Context,
	config sandbox.OCIConfig,
	name string,
) (Outcome, error) {
	invocation, err := config.Invocation()
	if err != nil {
		return Outcome{}, err
	}
	runtimeDigest, err := config.RuntimeConfigDigest()
	if err != nil {
		return Outcome{}, err
	}
	imageDigest, err := sandbox.ImageDigest(config.Image)
	if err != nil {
		return Outcome{}, err
	}
	commandDigest, err := digestCommand(config.Command)
	if err != nil {
		return Outcome{}, err
	}
	directory, stdoutFile, stderrFile, err := runner.createEvidenceFiles(config.TransactionID, name)
	if err != nil {
		return Outcome{}, err
	}
	filesClosed := false
	defer func() {
		if !filesClosed {
			_ = stdoutFile.Close()
			_ = stderrFile.Close()
		}
	}()
	captureLimit := runner.CaptureLimit
	if captureLimit <= 0 {
		captureLimit = CaptureLimit
	}
	processContext, cancelProcess := context.WithCancel(ctx)
	defer cancelProcess()
	var outputExceeded atomic.Bool
	var cancelOutput sync.Once
	cancelOnLimit := func() {
		outputExceeded.Store(true)
		cancelOutput.Do(cancelProcess)
	}
	stdoutHash := sha256.New()
	stderrHash := sha256.New()
	stdoutCapture := &limitedWriter{
		writer:    io.MultiWriter(stdoutHash, stdoutFile),
		remaining: captureLimit,
		onLimit:   cancelOnLimit,
	}
	stderrCapture := &limitedWriter{
		writer:    io.MultiWriter(stderrHash, stderrFile),
		remaining: captureLimit,
		onLimit:   cancelOnLimit,
	}
	command := exec.CommandContext(
		processContext, invocation.Executable, invocation.Arguments...,
	)
	command.Stdin = nil
	command.Stdout = stdoutCapture
	command.Stderr = stderrCapture
	status := model.AgentExecutionStartFailed
	var exitCode *int
	started := false
	if startErr := command.Start(); startErr == nil {
		started = true
		waitErr := command.Wait()
		switch {
		case outputExceeded.Load():
			status = model.AgentExecutionFailed
		case ctx.Err() != nil:
			status = model.AgentExecutionInterrupted
		case waitErr == nil:
			value := 0
			exitCode = &value
			status = model.AgentExecutionSucceeded
		default:
			var exitError *exec.ExitError
			if errors.As(waitErr, &exitError) {
				value := exitError.ExitCode()
				exitCode = &value
				status = model.AgentExecutionFailed
			} else {
				status = model.AgentExecutionInterrupted
			}
		}
	}
	var cleanupErr error
	if started {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cleanupErr = sandbox.RemoveContainer(
			cleanupContext,
			invocation.Executable,
			invocation.ContainerName,
		)
		cancel()
	}
	if err := errors.Join(stdoutFile.Close(), stderrFile.Close()); err != nil {
		return Outcome{}, fmt.Errorf("close verification evidence: %w", err)
	}
	filesClosed = true
	receipt := ProcessReceipt{
		Version:             "gatemole.verification_process_receipt.v0",
		Name:                name,
		Status:              status,
		ExitCode:            exitCode,
		CommandDigest:       commandDigest,
		RuntimeConfigDigest: runtimeDigest,
		ImageDigest:         imageDigest,
		StdoutDigest:        encodedHash(stdoutHash),
		StderrDigest:        encodedHash(stderrHash),
		StdoutTruncated:     stdoutCapture.truncated,
		StderrTruncated:     stderrCapture.truncated,
		CompletedAt:         time.Now().UTC(),
	}
	outcome := Outcome{
		Receipt: receipt, EvidenceDirectory: directory,
	}
	evidence, err := finalizeEvidence(directory, config.TransactionID, name, receipt)
	if err != nil {
		return outcome, err
	}
	outcome.Evidence = evidence
	if cleanupErr != nil {
		return outcome, &CleanupError{
			ContainerName: invocation.ContainerName,
			Cause:         cleanupErr,
		}
	}
	return outcome, nil
}

func (runner Runner) createEvidenceFiles(transactionID, name string) (string, *os.File, *os.File, error) {
	if runner.EvidenceRoot == "" {
		return "", nil, nil, errors.New("verification evidence root is required")
	}
	if err := os.MkdirAll(runner.EvidenceRoot, 0o700); err != nil {
		return "", nil, nil, fmt.Errorf("create verifier evidence root: %w", err)
	}
	prefix := sha256.Sum256([]byte(transactionID + "\x00" + name))
	directory, err := os.MkdirTemp(
		runner.EvidenceRoot, "verification-"+hex.EncodeToString(prefix[:6])+"-",
	)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create verifier evidence directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", nil, nil, err
	}
	stdoutFile, err := os.OpenFile(filepath.Join(directory, "stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, nil, err
	}
	stderrFile, err := os.OpenFile(filepath.Join(directory, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = stdoutFile.Close()
		return "", nil, nil, err
	}
	return directory, stdoutFile, stderrFile, nil
}

func finalizeEvidence(
	directory, transactionID, name string,
	receipt ProcessReceipt,
) ([]model.ArtifactRef, error) {
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	receiptPath := filepath.Join(directory, "receipt.json")
	if err := os.WriteFile(receiptPath, data, 0o600); err != nil {
		return nil, err
	}
	values := []struct {
		name      string
		path      string
		mediaType string
	}{
		{"receipt", receiptPath, "application/json"},
		{"stdout", filepath.Join(directory, "stdout.log"), "text/plain"},
		{"stderr", filepath.Join(directory, "stderr.log"), "text/plain"},
	}
	result := make([]model.ArtifactRef, 0, len(values))
	for _, value := range values {
		digest, err := fileDigest(value.path)
		if err != nil {
			return nil, err
		}
		result = append(result, model.ArtifactRef{
			URI:    "evidence://" + transactionID + "/" + name + "/" + value.name,
			Digest: digest, MediaType: value.mediaType,
		})
	}
	return result, nil
}

func digestCommand(command []string) (string, error) {
	data, err := json.Marshal(command)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return encodedHash(sum), nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
	truncated bool
	onLimit   func()
}

func (writer *limitedWriter) Write(data []byte) (int, error) {
	if writer.remaining <= 0 {
		writer.truncated = writer.truncated || len(data) > 0
		if len(data) > 0 && writer.onLimit != nil {
			writer.onLimit()
		}
		return len(data), nil
	}
	value := data
	if int64(len(value)) > writer.remaining {
		value = value[:writer.remaining]
		writer.truncated = true
		if writer.onLimit != nil {
			defer writer.onLimit()
		}
	}
	written, err := writer.writer.Write(value)
	writer.remaining -= int64(written)
	if err != nil {
		return written, err
	}
	if written != len(value) {
		return written, io.ErrShortWrite
	}
	return len(data), nil
}

func encodedHash(value hash.Hash) string {
	return "sha256:" + hex.EncodeToString(value.Sum(nil))
}
