package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestConfigSnapshotBindsOriginalBytesAfterPathReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "trust.json")
	original := []byte(`{"version":"a"}`)
	replacement := []byte(`{"version":"b"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, err := readConfigFileSnapshot(path, "trust", true)
	if err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(directory, "replacement.json")
	if err := os.WriteFile(replacementPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, path); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(snapshot.Data, original) {
		t.Fatalf("snapshot data=%s, want original=%s", snapshot.Data, original)
	}
	originalSum := sha256.Sum256(original)
	wantDigest := "sha256:" + hex.EncodeToString(originalSum[:])
	if snapshot.Digest != wantDigest {
		t.Fatalf("snapshot digest=%q, want %q", snapshot.Digest, wantDigest)
	}
	replacementSum := sha256.Sum256(replacement)
	if snapshot.Digest == "sha256:"+hex.EncodeToString(replacementSum[:]) {
		t.Fatal("snapshot digest followed the replacement path")
	}
}

func TestConfigSnapshotRejectsAtoBReplacementWhileOpening(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "trust.json")
	if err := os.WriteFile(path, []byte(`{"version":"a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(directory, "replacement.json")
	if err := os.WriteFile(
		replacementPath,
		[]byte(`{"version":"b"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	_, err := readConfigFileSnapshotWithOpen(
		path,
		"trust",
		true,
		func(openPath string) (*os.File, error) {
			if renameErr := os.Rename(replacementPath, path); renameErr != nil {
				return nil, renameErr
			}
			return os.Open(openPath)
		},
	)
	if err == nil || !strings.Contains(err.Error(), "changed while it was opened") {
		t.Fatalf("replacement error=%v", err)
	}
}

func TestConfigureModelBrokerRetainsParsedAndHashedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-policy.json")
	policyA := []byte(`{
		"version":"gatemole.model_broker_policy.v0",
		"provider":"openai",
		"upstream_base_url":"http://provider-a.invalid",
		"allowed_models":["model-a"],
		"allowed_tool_types":[],
		"max_requests":1,
		"max_request_bytes":1024,
		"max_response_bytes":1024,
		"max_output_tokens_per_request":1,
		"max_total_input_tokens":1,
		"max_total_output_tokens":1,
		"request_timeout_seconds":1,
		"force_store_false":false,
		"allow_stateful_requests":false
	}`)
	policyB := bytes.ReplaceAll(
		policyA,
		[]byte("provider-a.invalid"),
		[]byte("provider-b.invalid"),
	)
	policyB = bytes.ReplaceAll(policyB, []byte("model-a"), []byte("model-b"))
	if err := os.WriteFile(path, policyA, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEMOLE_SNAPSHOT_PROVIDER_TOKEN", "provider-token")

	executionPolicy, err := executionRuntimePolicy("development", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = configureModelBroker(Config{
		RuntimeProfile:    "development",
		ModelBrokerImage:  "broker@sha256:" + strings.Repeat("a", 64),
		ModelBrokerPolicy: path,
		ModelTokenEnv:     "GATEMOLE_SNAPSHOT_PROVIDER_TOKEN",
	}, &executionPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, policyB, 0o600); err != nil {
		t.Fatal(err)
	}

	broker := executionPolicy.ModelBroker
	if broker == nil {
		t.Fatal("model broker policy was not configured")
	}
	if !bytes.Equal(broker.PolicyData, policyA) {
		t.Fatal("retained model policy followed the replaced source path")
	}
	sum := sha256.Sum256(policyA)
	if broker.PolicyDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatal("model policy digest does not bind the retained snapshot")
	}
	if !broker.Policy.AllowsModel("model-a") ||
		broker.Policy.AllowsModel("model-b") {
		t.Fatalf("parsed model policy followed replacement: %#v", broker.Policy)
	}
}

func TestProductionConfigSnapshotRejectsOtherOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	otherUID := uint32(os.Geteuid() + 1)
	if otherUID == 0 {
		otherUID = 1
	}
	err = validateConfigSnapshotInfo(
		fileInfoWithOwner{FileInfo: info, uid: otherUID},
		"trust",
		true,
	)
	if err == nil || !strings.Contains(err.Error(), "root or the daemon user") {
		t.Fatalf("other-owner error=%v", err)
	}
}

type fileInfoWithOwner struct {
	os.FileInfo
	uid uint32
}

func (info fileInfoWithOwner) Sys() any {
	return &syscall.Stat_t{Uid: info.uid}
}
