package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ComputeExecutionContractDigest returns the deterministic local v0 digest of
// an execution contract. The existing Digest value is excluded so callers can
// verify a persisted contract or author its digest from the same function.
func ComputeExecutionContractDigest(contract ExecutionContract) (string, error) {
	contract.Digest = ""
	data, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("encode execution contract digest input: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
