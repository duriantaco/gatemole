package model

import (
	"strings"
	"testing"
)

func TestComputeExecutionContractDigestExcludesDigestAndBindsContent(t *testing.T) {
	t.Parallel()
	data := readFixture(t, "valid", "execution_contract.json")
	contract, err := DecodeStrict[ExecutionContract](data)
	if err != nil {
		t.Fatal(err)
	}
	originalDigest := contract.Digest
	computed, err := ComputeExecutionContractDigest(contract)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(computed, "sha256:") || len(computed) != len("sha256:")+64 {
		t.Fatalf("computed digest has invalid form: %q", computed)
	}
	if contract.Digest != originalDigest {
		t.Fatal("digest computation mutated its input")
	}

	contract.Digest = "sha256:" + strings.Repeat("0", 64)
	withDifferentClaim, err := ComputeExecutionContractDigest(contract)
	if err != nil {
		t.Fatal(err)
	}
	if withDifferentClaim != computed {
		t.Fatal("existing digest claim changed the computed content digest")
	}

	contract.Goal += " Preserve all audit evidence."
	withChangedContent, err := ComputeExecutionContractDigest(contract)
	if err != nil {
		t.Fatal(err)
	}
	if withChangedContent == computed {
		t.Fatal("material contract content did not change the computed digest")
	}
}
