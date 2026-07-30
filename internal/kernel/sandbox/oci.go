// Package sandbox constructs fail-closed process isolation boundaries for
// untrusted agent and verifier workloads.
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

const (
	MinimumMemoryBytes       = int64(256 << 20)
	MinimumTmpfsBytes        = int64(16 << 20)
	MaximumTaskEnvelopeBytes = int64(80 << 10)
)

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type OCIConfig struct {
	EnginePath    string
	Image         string
	Workspace     string
	TransactionID string
	RunID         string
	Entrypoint    string
	Command       []string
	UID           int
	GID           int
	MemoryBytes   int64
	CPUMillis     int64
	PIDsLimit     int64
	TmpfsBytes    int64
	ContainerName string
	Role          string
	WorkspaceMode string
	TaskDirectory string
	TaskDigest    string
	NetworkName   string
	ModelBroker   *ModelBrokerBinding
}

type ModelBrokerBinding struct {
	URL               string
	Token             string
	ImageDigest       string
	PolicyDigest      string
	TokenDigest       string
	ReceiptLedgerHint string
}

type Invocation struct {
	Executable    string
	Arguments     []string
	ContainerName string
}

type runtimeDigestInput struct {
	Version         string   `json:"version"`
	ImageDigest     string   `json:"image_digest"`
	WorkspaceMode   string   `json:"workspace_mode"`
	Role            string   `json:"role"`
	Entrypoint      string   `json:"entrypoint,omitempty"`
	Command         []string `json:"command"`
	Network         string   `json:"network"`
	ReadOnlyRootFS  bool     `json:"read_only_rootfs"`
	Capabilities    string   `json:"capabilities"`
	NoNewPrivileges bool     `json:"no_new_privileges"`
	UID             int      `json:"uid"`
	GID             int      `json:"gid"`
	MemoryBytes     int64    `json:"memory_bytes"`
	CPUMillis       int64    `json:"cpu_millis"`
	PIDsLimit       int64    `json:"pids_limit"`
	TmpfsBytes      int64    `json:"tmpfs_bytes"`
	TaskDigest      string   `json:"task_digest,omitempty"`
	ModelBroker     *struct {
		URL          string `json:"url"`
		ImageDigest  string `json:"image_digest"`
		PolicyDigest string `json:"policy_digest"`
		TokenDigest  string `json:"token_digest"`
	} `json:"model_broker,omitempty"`
}

func (config OCIConfig) Validate() error {
	if !filepath.IsAbs(config.EnginePath) {
		return errors.New("OCI engine path must be absolute")
	}
	if strings.ContainsRune(config.EnginePath, 0) {
		return errors.New("OCI engine path cannot contain NUL")
	}
	if _, err := ImageDigest(config.Image); err != nil {
		return err
	}
	if !filepath.IsAbs(config.Workspace) {
		return errors.New("OCI workspace must be absolute")
	}
	if strings.ContainsAny(config.Workspace, ",\x00") {
		return errors.New("OCI workspace cannot contain comma or NUL")
	}
	if config.Entrypoint != "" &&
		(strings.TrimSpace(config.Entrypoint) == "" ||
			strings.ContainsRune(config.Entrypoint, 0)) {
		return errors.New("OCI entrypoint must be non-empty and cannot contain NUL")
	}
	if config.Entrypoint == "" &&
		(len(config.Command) == 0 || strings.TrimSpace(config.Command[0]) == "") {
		return errors.New("OCI command is required")
	}
	for _, argument := range config.Command {
		if strings.ContainsRune(argument, 0) {
			return errors.New("OCI command arguments cannot contain NUL")
		}
	}
	if config.TransactionID == "" || config.RunID == "" {
		return errors.New("OCI transaction and run IDs are required")
	}
	if config.Role != "agent" && config.Role != "verifier" {
		return errors.New("OCI role must be agent or verifier")
	}
	if config.WorkspaceMode != "transaction_rw" && config.WorkspaceMode != "staged_ro" {
		return errors.New("OCI workspace mode must be transaction_rw or staged_ro")
	}
	if (config.Role == "agent" && config.WorkspaceMode != "transaction_rw") ||
		(config.Role == "verifier" && config.WorkspaceMode != "staged_ro") {
		return errors.New("OCI role and workspace mode are incompatible")
	}
	if err := config.validateTaskMount(); err != nil {
		return err
	}
	if config.ModelBroker == nil {
		if config.NetworkName != "" {
			return errors.New("OCI custom network requires a model broker binding")
		}
	} else {
		if config.Role != "agent" || !validContainerName(config.NetworkName) ||
			config.ModelBroker.URL != "http://gatemole-model-broker:8080/v1" ||
			len(config.ModelBroker.Token) < 32 ||
			!digestPattern.MatchString(config.ModelBroker.ImageDigest) ||
			!digestPattern.MatchString(config.ModelBroker.PolicyDigest) ||
			!digestPattern.MatchString(config.ModelBroker.TokenDigest) ||
			digestValue([]byte(config.ModelBroker.Token)) != config.ModelBroker.TokenDigest {
			return errors.New("OCI model broker binding is invalid")
		}
	}
	if config.UID <= 0 || config.GID <= 0 {
		return errors.New("OCI workload must run as a non-root numeric UID and GID")
	}
	if config.MemoryBytes < MinimumMemoryBytes {
		return fmt.Errorf("OCI memory limit must be at least %d bytes", MinimumMemoryBytes)
	}
	if config.CPUMillis < 100 || config.CPUMillis > 64000 {
		return errors.New("OCI CPU limit must be between 100 and 64000 millicpus")
	}
	if config.PIDsLimit < 16 || config.PIDsLimit > 4096 {
		return errors.New("OCI PID limit must be between 16 and 4096")
	}
	if config.TmpfsBytes < MinimumTmpfsBytes || config.TmpfsBytes > config.MemoryBytes {
		return errors.New("OCI tmpfs size must be at least 16 MiB and no larger than the memory limit")
	}
	if !validContainerName(config.ContainerName) {
		return errors.New("OCI container name is invalid")
	}
	return nil
}

func (config OCIConfig) Invocation() (Invocation, error) {
	if err := config.Validate(); err != nil {
		return Invocation{}, err
	}
	cpus := strconv.FormatFloat(float64(config.CPUMillis)/1000, 'f', 3, 64)
	mountMode := ""
	if config.WorkspaceMode == "staged_ro" {
		mountMode = ",readonly"
	}
	network := "none"
	if config.ModelBroker != nil {
		network = config.NetworkName
	}
	arguments := []string{
		"run",
		"--rm",
		"--interactive",
		"--pull=never",
		"--name=" + config.ContainerName,
		"--hostname=gatemole-agent",
		"--network=" + network,
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges:true",
		"--pids-limit=" + strconv.FormatInt(config.PIDsLimit, 10),
		"--memory=" + strconv.FormatInt(config.MemoryBytes, 10),
		"--cpus=" + cpus,
		"--user=" + strconv.Itoa(config.UID) + ":" + strconv.Itoa(config.GID),
		"--workdir=/workspace",
		"--env=HOME=/tmp",
		"--env=GATEMOLE_TRANSACTION_ID=" + config.TransactionID,
		"--env=GATEMOLE_RUN_ID=" + config.RunID,
		"--env=GATEMOLE_RUNTIME_ROLE=" + config.Role,
		"--tmpfs=/tmp:rw,nosuid,nodev,size=" + strconv.FormatInt(config.TmpfsBytes, 10) +
			",uid=" + strconv.Itoa(config.UID) + ",gid=" + strconv.Itoa(config.GID) + ",mode=1777",
		"--mount=type=bind,src=" + config.Workspace + ",dst=/workspace" + mountMode,
	}
	if config.TaskDirectory != "" {
		arguments = append(arguments,
			"--env=GATEMOLE_TASK_PATH=/gatemole/task.json",
			"--env=GATEMOLE_TASK_DIGEST="+config.TaskDigest,
			"--mount=type=bind,src="+config.TaskDirectory+",dst=/gatemole,readonly",
		)
	}
	if config.Entrypoint != "" {
		arguments = append(arguments, "--entrypoint="+config.Entrypoint)
	}
	arguments = append(arguments, config.Image)
	if config.ModelBroker != nil {
		brokerEnvironment := []string{
			"--env=GATEMOLE_MODEL_BROKER_URL=" + config.ModelBroker.URL,
			"--env=GATEMOLE_MODEL_BROKER_TOKEN=" + config.ModelBroker.Token,
			"--env=OPENAI_BASE_URL=" + config.ModelBroker.URL,
			"--env=OPENAI_API_KEY=" + config.ModelBroker.Token,
		}
		imageIndex := len(arguments) - 1
		arguments = append(arguments[:imageIndex], append(brokerEnvironment, arguments[imageIndex:]...)...)
	}
	arguments = append(arguments, config.Command...)
	return Invocation{
		Executable:    config.EnginePath,
		Arguments:     arguments,
		ContainerName: config.ContainerName,
	}, nil
}

func (config OCIConfig) RuntimeConfigDigest() (string, error) {
	if err := config.Validate(); err != nil {
		return "", err
	}
	imageDigest, err := ImageDigest(config.Image)
	if err != nil {
		return "", err
	}
	var brokerDigest *struct {
		URL          string `json:"url"`
		ImageDigest  string `json:"image_digest"`
		PolicyDigest string `json:"policy_digest"`
		TokenDigest  string `json:"token_digest"`
	}
	if config.ModelBroker != nil {
		brokerDigest = &struct {
			URL          string `json:"url"`
			ImageDigest  string `json:"image_digest"`
			PolicyDigest string `json:"policy_digest"`
			TokenDigest  string `json:"token_digest"`
		}{
			URL: config.ModelBroker.URL, ImageDigest: config.ModelBroker.ImageDigest,
			PolicyDigest: config.ModelBroker.PolicyDigest,
			TokenDigest:  config.ModelBroker.TokenDigest,
		}
	}
	network := "none"
	if brokerDigest != nil {
		network = "internal_model_broker"
	}
	data, err := json.Marshal(runtimeDigestInput{
		Version:         "gatemole.oci_runtime_config.v0",
		ImageDigest:     imageDigest,
		WorkspaceMode:   config.WorkspaceMode,
		Role:            config.Role,
		Entrypoint:      config.Entrypoint,
		Command:         append([]string(nil), config.Command...),
		Network:         network,
		ReadOnlyRootFS:  true,
		Capabilities:    "none",
		NoNewPrivileges: true,
		UID:             config.UID,
		GID:             config.GID,
		MemoryBytes:     config.MemoryBytes,
		CPUMillis:       config.CPUMillis,
		PIDsLimit:       config.PIDsLimit,
		TmpfsBytes:      config.TmpfsBytes,
		TaskDigest:      config.TaskDigest,
		ModelBroker:     brokerDigest,
	})
	if err != nil {
		return "", fmt.Errorf("encode OCI runtime configuration: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (config OCIConfig) validateTaskMount() error {
	if config.TaskDirectory == "" && config.TaskDigest == "" {
		return nil
	}
	if config.TaskDirectory == "" || config.TaskDigest == "" ||
		config.Role != "agent" ||
		!digestPattern.MatchString(config.TaskDigest) {
		return errors.New("OCI task mount requires an agent role, directory, and sha256 digest")
	}
	if !filepath.IsAbs(config.TaskDirectory) ||
		strings.ContainsAny(config.TaskDirectory, ",\x00") {
		return errors.New("OCI task directory must be an absolute path without comma or NUL")
	}
	if pathsOverlap(config.Workspace, config.TaskDirectory) {
		return errors.New("OCI task directory must be outside the writable workspace")
	}
	directory, err := os.Lstat(config.TaskDirectory)
	if err != nil {
		return fmt.Errorf("inspect OCI task directory: %w", err)
	}
	if !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 ||
		directory.Mode().Perm()&0o022 != 0 {
		return errors.New("OCI task directory must be a non-writable real directory")
	}
	taskPath := filepath.Join(config.TaskDirectory, "task.json")
	task, err := os.Lstat(taskPath)
	if err != nil {
		return fmt.Errorf("inspect OCI task envelope: %w", err)
	}
	if !task.Mode().IsRegular() || task.Mode()&os.ModeSymlink != 0 ||
		task.Mode().Perm()&0o022 != 0 ||
		task.Size() < 1 || task.Size() > MaximumTaskEnvelopeBytes {
		return errors.New("OCI task envelope must be a bounded, non-writable regular file")
	}
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return fmt.Errorf("read OCI task envelope: %w", err)
	}
	envelope, err := model.DecodeStrict[model.AgentTask](data)
	if err != nil {
		return fmt.Errorf("decode OCI task envelope: %w", err)
	}
	if err := envelope.Validate(); err != nil {
		return fmt.Errorf("validate OCI task envelope: %w", err)
	}
	if envelope.Digest != config.TaskDigest {
		return errors.New("OCI task digest does not match the mounted task envelope")
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	relative, err := filepath.Rel(left, right)
	if err == nil && relative != ".." &&
		relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return true
	}
	relative, err = filepath.Rel(right, left)
	return err == nil && relative != ".." &&
		(relative == "." || !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func ImageDigest(reference string) (string, error) {
	if strings.TrimSpace(reference) != reference || reference == "" ||
		strings.ContainsAny(reference, " \t\r\n\x00") || strings.HasPrefix(reference, "-") {
		return "", errors.New("OCI image reference is invalid")
	}
	digest := reference
	if strings.Count(reference, "@") > 1 {
		return "", errors.New("OCI image reference is invalid")
	}
	if separator := strings.Index(reference, "@"); separator >= 0 {
		if separator == 0 || separator == len(reference)-1 {
			return "", errors.New("OCI image reference is invalid")
		}
		digest = reference[separator+1:]
	}
	if !digestPattern.MatchString(digest) {
		return "", errors.New("OCI image must be pinned by sha256 digest")
	}
	return digest, nil
}

// PrepareWorkspaceOwnership makes a daemon-created bind mount writable by a
// configured non-root workload when gatemoled itself is running as root. A
// non-root production daemon is required to use its own UID/GID, so no
// ownership mutation is needed in that deployment.
func PrepareWorkspaceOwnership(workspace string, uid, gid int) error {
	if !filepath.IsAbs(workspace) || strings.ContainsAny(workspace, ",\x00") {
		return errors.New("OCI workspace ownership path is invalid")
	}
	if uid <= 0 || gid <= 0 {
		return errors.New("OCI workspace ownership requires non-root UID and GID")
	}
	if os.Geteuid() != 0 {
		return nil
	}
	return filepath.WalkDir(workspace, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return fmt.Errorf("chown OCI workspace path %s: %w", path, err)
		}
		return nil
	})
}

func ContainerName(transactionID, runID string) string {
	sum := sha256.Sum256([]byte(transactionID + "\x00" + runID))
	return "gatemole-" + hex.EncodeToString(sum[:12])
}

func CleanupArguments(containerName string) ([]string, error) {
	if !validContainerName(containerName) {
		return nil, errors.New("OCI container name is invalid")
	}
	return []string{"rm", "--force", containerName}, nil
}

func RemoveContainer(ctx context.Context, enginePath, containerName string) error {
	if !filepath.IsAbs(enginePath) || strings.ContainsRune(enginePath, 0) {
		return errors.New("OCI engine path must be absolute and cannot contain NUL")
	}
	arguments, err := CleanupArguments(containerName)
	if err != nil {
		return err
	}
	output, err := runBoundedEngineCommand(
		ctx,
		enginePath,
		64<<10,
		arguments...,
	)
	if err == nil || containerDoesNotExist(output) {
		return nil
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		message = err.Error()
	}
	return fmt.Errorf("%s: %w", message, err)
}

func containerDoesNotExist(output []byte) bool {
	message := strings.ToLower(string(output))
	return strings.Contains(message, "no such container") ||
		strings.Contains(message, "no container with name or id") ||
		strings.Contains(message, "container not found")
}

func validContainerName(value string) bool {
	if len(value) < 2 || len(value) > 128 {
		return false
	}
	for i, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			(i > 0 && (character == '_' || character == '.' || character == '-')) {
			continue
		}
		return false
	}
	return true
}
