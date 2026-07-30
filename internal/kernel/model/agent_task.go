package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	AgentTaskVersion        = "gatemole.agent_task.v0"
	MaxAgentTaskIntentBytes = 64 << 10
)

// AgentTaskProfileBinding records the immutable identity and executable
// boundary selected from a daemon/client-owned agent profile. The profile
// document remains independently versioned; its digest binds every field that
// was used to select this executable.
type AgentTaskProfileBinding struct {
	ID            string `json:"id"`
	Digest        string `json:"digest"`
	RuntimeClass  string `json:"runtime_class"`
	ImageDigest   string `json:"image_digest,omitempty"`
	Entrypoint    string `json:"entrypoint,omitempty"`
	CommandDigest string `json:"command_digest"`
}

// AgentTask is the durable product-level input to one participating agent run.
// Intent is deliberately retained, rather than represented only by a digest,
// so a supervised agent can consume exactly the human-owned task. Callers must
// not place credentials or other secrets in Intent.
type AgentTask struct {
	Version       string                  `json:"version"`
	ID            string                  `json:"id"`
	TransactionID string                  `json:"transaction_id"`
	Namespace     string                  `json:"namespace"`
	RunID         string                  `json:"run_id"`
	Intent        string                  `json:"intent"`
	IntentDigest  string                  `json:"intent_digest"`
	AgentProfile  AgentTaskProfileBinding `json:"agent_profile"`
	CreatedAt     time.Time               `json:"created_at"`
	Digest        string                  `json:"digest"`
}

func NewAgentTask(
	id, transactionID, namespace, runID, intent string,
	profile AgentTaskProfileBinding,
	createdAt time.Time,
) (AgentTask, error) {
	task := AgentTask{
		Version:       AgentTaskVersion,
		ID:            id,
		TransactionID: transactionID,
		Namespace:     namespace,
		RunID:         runID,
		Intent:        intent,
		IntentDigest:  ComputeAgentTaskIntentDigest(intent),
		AgentProfile:  profile,
		CreatedAt:     createdAt.UTC(),
	}
	digest, err := ComputeAgentTaskDigest(task)
	if err != nil {
		return AgentTask{}, err
	}
	task.Digest = digest
	if err := task.Validate(); err != nil {
		return AgentTask{}, err
	}
	return task, nil
}

func ComputeAgentTaskIntentDigest(intent string) string {
	sum := sha256.Sum256([]byte(intent))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ComputeAgentTaskDigest(task AgentTask) (string, error) {
	task.Digest = ""
	data, err := json.Marshal(task)
	if err != nil {
		return "", fmt.Errorf("encode agent task digest input: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (task AgentTask) Validate() error {
	const resource = "AgentTask"
	if err := validateVersion(resource, task.Version, AgentTaskVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", task.ID},
		{"transaction_id", task.TransactionID},
		{"namespace", task.Namespace},
		{"run_id", task.RunID},
		{"agent_profile.id", task.AgentProfile.ID},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if !utf8.ValidString(task.Intent) ||
		strings.TrimSpace(task.Intent) == "" ||
		strings.ContainsRune(task.Intent, 0) {
		return invalid(resource, "intent", "intent must be non-empty UTF-8 and cannot contain NUL")
	}
	if len(task.Intent) > MaxAgentTaskIntentBytes {
		return invalid(resource, "intent", fmt.Sprintf("intent cannot exceed %d bytes", MaxAgentTaskIntentBytes))
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"intent_digest", task.IntentDigest},
		{"agent_profile.digest", task.AgentProfile.Digest},
		{"agent_profile.command_digest", task.AgentProfile.CommandDigest},
		{"digest", task.Digest},
	} {
		if err := validateDigest(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if task.IntentDigest != ComputeAgentTaskIntentDigest(task.Intent) {
		return invalid(resource, "intent_digest", "intent digest does not match the retained intent")
	}
	switch task.AgentProfile.RuntimeClass {
	case "oci":
		if err := validateDigest(resource, "agent_profile.image_digest", task.AgentProfile.ImageDigest); err != nil {
			return err
		}
		if task.AgentProfile.Entrypoint != "" &&
			(!utf8.ValidString(task.AgentProfile.Entrypoint) ||
				strings.TrimSpace(task.AgentProfile.Entrypoint) == "" ||
				strings.ContainsRune(task.AgentProfile.Entrypoint, 0)) {
			return invalid(resource, "agent_profile.entrypoint", "entrypoint must be non-empty UTF-8 and cannot contain NUL")
		}
	case "host":
		if task.AgentProfile.ImageDigest != "" {
			return invalid(resource, "agent_profile.image_digest", "host profile cannot claim an OCI image digest")
		}
		if task.AgentProfile.Entrypoint != "" {
			return invalid(resource, "agent_profile.entrypoint", "host profile cannot claim an OCI entrypoint")
		}
	default:
		return invalid(resource, "agent_profile.runtime_class", "runtime class must be oci or host")
	}
	if task.CreatedAt.IsZero() {
		return invalid(resource, "created_at", "created_at is required")
	}
	computed, err := ComputeAgentTaskDigest(task)
	if err != nil {
		return invalid(resource, "digest", "compute task digest")
	}
	if task.Digest != computed {
		return invalid(resource, "digest", "task digest does not match the task envelope")
	}
	return nil
}
