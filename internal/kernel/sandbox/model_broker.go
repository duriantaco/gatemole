package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ModelBrokerConfig struct {
	EnginePath          string
	Image               string
	PolicyPath          string
	PolicyDigest        string
	ReceiptDirectory    string
	TransactionID       string
	RunID               string
	AgentToken          string
	ProviderBearerToken string
	UID                 int
	GID                 int
	MemoryBytes         int64
	CPUMillis           int64
	PIDsLimit           int64
	TmpfsBytes          int64
}

type ModelBrokerSession struct {
	NetworkName      string
	ContainerName    string
	AgentToken       string
	ImageDigest      string
	PolicyDigest     string
	ReceiptDirectory string
	ReceiptPath      string
}

func (config ModelBrokerConfig) Validate() error {
	if !filepath.IsAbs(config.EnginePath) || strings.ContainsRune(config.EnginePath, 0) {
		return errors.New("model broker OCI engine path must be absolute")
	}
	if _, err := ImageDigest(config.Image); err != nil {
		return fmt.Errorf("model broker image: %w", err)
	}
	for name, value := range map[string]string{
		"policy path":       config.PolicyPath,
		"receipt directory": config.ReceiptDirectory,
	} {
		if !filepath.IsAbs(value) || strings.ContainsAny(value, ",\x00") {
			return fmt.Errorf("model broker %s must be an absolute bind-safe path", name)
		}
	}
	if !digestPattern.MatchString(config.PolicyDigest) {
		return errors.New("model broker policy digest must be sha256")
	}
	if config.TransactionID == "" || config.RunID == "" {
		return errors.New("model broker transaction and run IDs are required")
	}
	if len(config.AgentToken) < 32 ||
		strings.ContainsAny(config.AgentToken, "\r\n\x00") ||
		strings.TrimSpace(config.ProviderBearerToken) == "" ||
		strings.ContainsAny(config.ProviderBearerToken, "\r\n\x00") {
		return errors.New("model broker credentials are invalid")
	}
	resourceConfig := OCIConfig{
		EnginePath: config.EnginePath, Image: config.Image,
		Workspace: config.ReceiptDirectory, TransactionID: config.TransactionID,
		RunID: config.RunID, Command: []string{"broker"}, UID: config.UID, GID: config.GID,
		MemoryBytes: config.MemoryBytes, CPUMillis: config.CPUMillis,
		PIDsLimit: config.PIDsLimit, TmpfsBytes: config.TmpfsBytes,
		ContainerName: ModelBrokerContainerName(config.TransactionID, config.RunID),
		Role:          "agent", WorkspaceMode: "transaction_rw",
	}
	if err := resourceConfig.Validate(); err != nil {
		return fmt.Errorf("model broker resource boundary: %w", err)
	}
	return nil
}

func StartModelBroker(ctx context.Context, config ModelBrokerConfig) (ModelBrokerSession, error) {
	if err := config.Validate(); err != nil {
		return ModelBrokerSession{}, err
	}
	session := ModelBrokerSession{
		NetworkName:   ModelBrokerNetworkName(config.TransactionID, config.RunID),
		ContainerName: ModelBrokerContainerName(config.TransactionID, config.RunID),
		AgentToken:    config.AgentToken, PolicyDigest: config.PolicyDigest,
		ReceiptDirectory: config.ReceiptDirectory,
		ReceiptPath:      filepath.Join(config.ReceiptDirectory, "model-calls.jsonl"),
	}
	imageDigest, _ := ImageDigest(config.Image)
	session.ImageDigest = imageDigest
	if err := os.MkdirAll(config.ReceiptDirectory, 0o770); err != nil {
		return ModelBrokerSession{}, fmt.Errorf("create model broker receipt directory: %w", err)
	}
	if err := os.Chmod(config.ReceiptDirectory, 0o770); err != nil {
		return ModelBrokerSession{}, fmt.Errorf("set model broker receipt permissions: %w", err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(config.ReceiptDirectory, config.UID, config.GID); err != nil {
			return ModelBrokerSession{}, fmt.Errorf("assign model broker receipt ownership: %w", err)
		}
	}
	policyData, err := os.ReadFile(config.PolicyPath)
	if err != nil {
		return ModelBrokerSession{}, fmt.Errorf("read model broker policy: %w", err)
	}
	if len(policyData) > 2<<20 || digestValue(policyData) != config.PolicyDigest {
		return ModelBrokerSession{}, errors.New("model broker policy changed after its digest was approved")
	}
	runtimePolicyPath := filepath.Join(config.ReceiptDirectory, "model-policy.json")
	if existing, err := os.ReadFile(runtimePolicyPath); err == nil {
		if digestValue(existing) != config.PolicyDigest {
			return ModelBrokerSession{}, errors.New("frozen model broker policy conflicts with the requested policy")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(runtimePolicyPath, policyData, 0o444); err != nil {
			return ModelBrokerSession{}, fmt.Errorf("freeze model broker policy: %w", err)
		}
	} else {
		return ModelBrokerSession{}, fmt.Errorf("inspect frozen model broker policy: %w", err)
	}
	readyPath := filepath.Join(config.ReceiptDirectory, "ready")
	_ = os.Remove(readyPath)
	envPath := filepath.Join(config.ReceiptDirectory, "broker.env")
	envData := []byte(
		"VOUCH_MODEL_BROKER_TOKEN=" + config.AgentToken + "\n" +
			"VOUCH_PROVIDER_BEARER_TOKEN=" + config.ProviderBearerToken + "\n",
	)
	if err := os.WriteFile(envPath, envData, 0o600); err != nil {
		return ModelBrokerSession{}, fmt.Errorf("write model broker secret environment: %w", err)
	}
	if err := os.Chmod(envPath, 0o600); err != nil {
		return ModelBrokerSession{}, fmt.Errorf("restrict model broker secret environment: %w", err)
	}
	defer os.Remove(envPath)
	if err := RemoveModelBroker(ctx, config.EnginePath, session); err != nil {
		return ModelBrokerSession{}, fmt.Errorf("remove stale model broker resources: %w", err)
	}
	labels := []string{
		"--label=vouch.managed=true",
		"--label=vouch.role=model-broker",
		"--label=vouch.transaction=" + shortResourceLabel(config.TransactionID),
	}
	networkArguments := append([]string{
		"network", "create", "--internal",
	}, labels...)
	networkArguments = append(networkArguments, session.NetworkName)
	if _, err := runEngine(ctx, config.EnginePath, networkArguments...); err != nil {
		return ModelBrokerSession{}, fmt.Errorf("create internal model broker network: %w", err)
	}
	cpus := strconv.FormatFloat(float64(config.CPUMillis)/1000, 'f', 3, 64)
	runArguments := []string{
		"run", "--detach", "--rm",
		"--name=" + session.ContainerName,
		"--hostname=vouch-model-broker",
		"--network=bridge",
		"--pull=never",
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges:true",
		"--pids-limit=" + strconv.FormatInt(config.PIDsLimit, 10),
		"--memory=" + strconv.FormatInt(config.MemoryBytes, 10),
		"--cpus=" + cpus,
		"--user=" + strconv.Itoa(config.UID) + ":" + strconv.Itoa(config.GID),
		"--env-file=" + envPath,
		"--tmpfs=/tmp:rw,nosuid,nodev,size=" + strconv.FormatInt(config.TmpfsBytes, 10) +
			",uid=" + strconv.Itoa(config.UID) + ",gid=" + strconv.Itoa(config.GID) + ",mode=1777",
		"--mount=type=bind,src=" + runtimePolicyPath + ",dst=/run/vouch/model-policy.json,readonly",
		"--mount=type=bind,src=" + config.ReceiptDirectory + ",dst=/var/lib/vouch",
	}
	runArguments = append(runArguments, labels...)
	runArguments = append(
		runArguments,
		config.Image,
		"--listen", "0.0.0.0:8080",
		"--policy", "/run/vouch/model-policy.json",
		"--receipts", "/var/lib/vouch/model-calls.jsonl",
		"--transaction", config.TransactionID,
		"--run", config.RunID,
		"--ready-file", "/var/lib/vouch/ready",
		"--production=true",
	)
	if _, err := runEngine(ctx, config.EnginePath, runArguments...); err != nil {
		_ = RemoveModelBroker(context.Background(), config.EnginePath, session)
		return ModelBrokerSession{}, fmt.Errorf("start model broker sidecar: %w", err)
	}
	connectArguments := []string{
		"network", "connect", "--alias", "vouch-model-broker",
		session.NetworkName, session.ContainerName,
	}
	if _, err := runEngine(ctx, config.EnginePath, connectArguments...); err != nil {
		_ = RemoveModelBroker(context.Background(), config.EnginePath, session)
		return ModelBrokerSession{}, fmt.Errorf("connect model broker to internal network: %w", err)
	}
	if err := waitForBrokerReady(ctx, readyPath); err != nil {
		_ = RemoveModelBroker(context.Background(), config.EnginePath, session)
		return ModelBrokerSession{}, err
	}
	return session, nil
}

func RemoveModelBroker(
	ctx context.Context,
	enginePath string,
	session ModelBrokerSession,
) error {
	var result error
	if session.ContainerName != "" {
		if err := RemoveContainer(ctx, enginePath, session.ContainerName); err != nil {
			result = errors.Join(result, err)
		}
	}
	if session.NetworkName != "" {
		output, err := runEngine(ctx, enginePath, "network", "rm", session.NetworkName)
		if err != nil && !resourceDoesNotExist(output) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func ModelBrokerNetworkName(transactionID, runID string) string {
	return "vouch-net-" + resourceSuffix(transactionID, runID)
}

func ModelBrokerContainerName(transactionID, runID string) string {
	return "vouch-broker-" + resourceSuffix(transactionID, runID)
}

func resourceSuffix(transactionID, runID string) string {
	sum := sha256.Sum256([]byte(transactionID + "\x00" + runID))
	return hex.EncodeToString(sum[:12])
}

func shortResourceLabel(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func runEngine(ctx context.Context, enginePath string, arguments ...string) ([]byte, error) {
	output, err := runBoundedEngineCommand(
		ctx,
		enginePath,
		64<<10,
		arguments...,
	)
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return output, fmt.Errorf("%s: %w", message, err)
	}
	return output, nil
}

func resourceDoesNotExist(output []byte) bool {
	message := strings.ToLower(string(output))
	return strings.Contains(message, "not found") ||
		strings.Contains(message, "no such network") ||
		strings.Contains(message, "does not exist")
}

func waitForBrokerReady(ctx context.Context, readyPath string) error {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(readyPath); err == nil && string(data) == "ready\n" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for model broker readiness: %w", ctx.Err())
		case <-deadline.C:
			return errors.New("model broker did not become ready within 10 seconds")
		case <-ticker.C:
		}
	}
}

func digestValue(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
