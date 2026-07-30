package modelbroker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
)

const PolicyVersion = "vouch.model_broker_policy.v0"

const maxPolicyBytes = 2 << 20

type Policy struct {
	Version                   string            `json:"version"`
	Provider                  string            `json:"provider"`
	UpstreamBaseURL           string            `json:"upstream_base_url"`
	AllowedModels             []string          `json:"allowed_models"`
	AllowedToolTypes          []string          `json:"allowed_tool_types"`
	ProviderHeaders           map[string]string `json:"provider_headers,omitempty"`
	MaxRequests               int64             `json:"max_requests"`
	MaxRequestBytes           int64             `json:"max_request_bytes"`
	MaxResponseBytes          int64             `json:"max_response_bytes"`
	MaxOutputTokensPerRequest int64             `json:"max_output_tokens_per_request"`
	MaxTotalInputTokens       int64             `json:"max_total_input_tokens"`
	MaxTotalOutputTokens      int64             `json:"max_total_output_tokens"`
	RequestTimeoutSeconds     int64             `json:"request_timeout_seconds"`
	ForceStoreFalse           bool              `json:"force_store_false"`
	AllowStatefulRequests     bool              `json:"allow_stateful_requests"`
}

func (policy Policy) Validate(production bool) error {
	if policy.Version != PolicyVersion {
		return fmt.Errorf("model broker policy version must be %q", PolicyVersion)
	}
	if !identifier(policy.Provider) {
		return errors.New("model broker provider must be an identifier")
	}
	upstream, err := url.Parse(policy.UpstreamBaseURL)
	if err != nil || upstream.Host == "" || upstream.User != nil ||
		upstream.RawQuery != "" || upstream.Fragment != "" ||
		(upstream.Scheme != "https" && (!(!production && upstream.Scheme == "http"))) {
		return errors.New("model broker upstream must be an HTTPS origin")
	}
	if upstream.Path != "" && upstream.Path != "/" {
		return errors.New("model broker upstream must not contain a path")
	}
	if len(policy.AllowedModels) == 0 {
		return errors.New("model broker requires at least one allowed model")
	}
	for _, pattern := range policy.AllowedModels {
		if strings.TrimSpace(pattern) != pattern || pattern == "" {
			return errors.New("model broker model patterns cannot be empty or padded")
		}
		if _, err := path.Match(pattern, "model-probe"); err != nil {
			return fmt.Errorf("invalid model pattern %q: %w", pattern, err)
		}
	}
	seenTools := make(map[string]struct{}, len(policy.AllowedToolTypes))
	for _, toolType := range policy.AllowedToolTypes {
		if !identifier(toolType) {
			return fmt.Errorf("invalid allowed tool type %q", toolType)
		}
		if _, duplicate := seenTools[toolType]; duplicate {
			return fmt.Errorf("duplicate allowed tool type %q", toolType)
		}
		seenTools[toolType] = struct{}{}
	}
	for name, value := range policy.ProviderHeaders {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower != "openai-organization" && lower != "openai-project" {
			return fmt.Errorf("provider header %q is not allowed", name)
		}
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("provider header %q has an invalid value", name)
		}
	}
	if policy.MaxRequests < 1 || policy.MaxRequests > 10000 {
		return errors.New("model broker max_requests must be between 1 and 10000")
	}
	if policy.MaxRequestBytes < 1024 || policy.MaxRequestBytes > 64<<20 {
		return errors.New("model broker max_request_bytes must be between 1 KiB and 64 MiB")
	}
	if policy.MaxResponseBytes < 1024 || policy.MaxResponseBytes > 256<<20 {
		return errors.New("model broker max_response_bytes must be between 1 KiB and 256 MiB")
	}
	if policy.MaxOutputTokensPerRequest < 1 ||
		policy.MaxTotalOutputTokens < policy.MaxOutputTokensPerRequest ||
		policy.MaxTotalInputTokens < 1 {
		return errors.New("model broker token budgets are invalid")
	}
	if policy.RequestTimeoutSeconds < 1 || policy.RequestTimeoutSeconds > 3600 {
		return errors.New("model broker request timeout must be between 1 and 3600 seconds")
	}
	if production && !policy.ForceStoreFalse {
		return errors.New("production model broker must force store=false")
	}
	return nil
}

func (policy Policy) AllowsModel(model string) bool {
	for _, pattern := range policy.AllowedModels {
		if matched, err := path.Match(pattern, model); err == nil && matched {
			return true
		}
	}
	return false
}

func (policy Policy) AllowsTool(toolType string) bool {
	for _, allowed := range policy.AllowedToolTypes {
		if allowed == toolType {
			return true
		}
	}
	return false
}

func LoadPolicy(path string, production bool) (Policy, error) {
	file, err := os.Open(path)
	if err != nil {
		return Policy{}, fmt.Errorf("open model broker policy: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Policy{}, fmt.Errorf("stat model broker policy: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxPolicyBytes {
		return Policy{}, errors.New("model broker policy must be a regular file no larger than 2 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPolicyBytes+1))
	if err != nil {
		return Policy{}, fmt.Errorf("read model broker policy: %w", err)
	}
	if len(data) > maxPolicyBytes {
		return Policy{}, errors.New("model broker policy must be a regular file no larger than 2 MiB")
	}
	policy, err := ParsePolicy(data, production)
	if err != nil {
		return Policy{}, fmt.Errorf("decode model broker policy: %w", err)
	}
	return policy, nil
}

// ParsePolicy validates one already-bounded model-broker policy snapshot.
// Callers that also record a digest must hash these exact bytes.
func ParsePolicy(data []byte, production bool) (Policy, error) {
	if len(data) > maxPolicyBytes {
		return Policy{}, errors.New("model broker policy must be no larger than 2 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy Policy
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Policy{}, errors.New("model broker policy contains trailing JSON")
	}
	if err := policy.Validate(production); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func identifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for i, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			(i > 0 && (character == '_' || character == '.' || character == ':' || character == '-')) {
			continue
		}
		return false
	}
	return true
}
