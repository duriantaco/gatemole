package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ComputeEventDigest provides the deterministic local v0 event digest. It is
// not yet a cross-implementation canonical JSON signature format.
func ComputeEventDigest(event RunEvent) (string, error) {
	event.Digest = ""
	data, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func VerifyEventDigest(event RunEvent) (bool, error) {
	want, err := ComputeEventDigest(event)
	if err != nil {
		return false, err
	}
	return event.Digest == want, nil
}

func ComputeTransactionEventDigest(event TransactionEvent) (string, error) {
	event.Digest = ""
	data, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func VerifyTransactionEventDigest(event TransactionEvent) (bool, error) {
	want, err := ComputeTransactionEventDigest(event)
	if err != nil {
		return false, err
	}
	return event.Digest == want, nil
}
