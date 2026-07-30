package approval

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

func TestSignedApprovalBindsPackagePrincipalClassAndTime(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	principal := model.Principal{ID: "human:alice", Kind: model.PrincipalHuman}
	store, err := NewTrustStore([]TrustedKey{{
		KeyID: "key:alice", PublicKey: publicKey, Principal: principal,
		ApprovalClasses: []string{"security-reviewer"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pkg := approvalPackageFixture(now)
	decision := approvalDecisionFixture(pkg, principal, now)
	if err := Sign(&decision, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(decision, pkg, now.Add(time.Minute)); err != nil {
		t.Fatalf("verify signed decision: %v", err)
	}

	tampered := decision
	tampered.Decision = model.ApprovalReject
	tampered.Reason = "changed after signing"
	if err := store.Verify(tampered, pkg, now.Add(time.Minute)); err == nil {
		t.Fatal("tampered approval decision was accepted")
	}

	wrongPackage := pkg
	wrongPackage.Digest = digest("e")
	if err := store.Verify(decision, wrongPackage, now.Add(time.Minute)); err == nil {
		t.Fatal("decision was replayed against another package")
	}

	if err := store.Verify(decision, pkg, decision.ExpiresAt); err == nil {
		t.Fatal("expired decision was accepted")
	}
}

func approvalDecisionFixture(pkg model.ApprovalPackage, principal model.Principal, now time.Time) model.ApprovalDecision {
	return model.ApprovalDecision{
		Version: model.ApprovalDecisionVersion, ID: "approval-decision:1",
		TransactionID: pkg.TransactionID, ApprovalID: "approval:security:1",
		PackageDigest: pkg.Digest, ApprovalClass: "security-reviewer",
		Decision: model.ApprovalApprove, Approver: principal, KeyID: "key:alice",
		IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), Nonce: "nonce:1",
	}
}

func approvalPackageFixture(now time.Time) model.ApprovalPackage {
	return model.ApprovalPackage{
		Version: model.ApprovalPackageVersion, ID: "approval-package:1",
		TransactionID: "tx:1", IntentDigest: digest("a"), EffectSetDigest: digest("b"),
		StagedStateDigest: digest("c"), PolicyDigest: digest("d"), CommitPlanDigest: digest("e"),
		VerificationResultIDs: []string{"verification:1"}, RiskFindings: []model.RiskFinding{},
		RequiredApprovalClasses: []string{"security-reviewer"}, Summary: "review required",
		Digest: digest("f"), CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}
}

func digest(character string) string {
	value := ""
	for range 64 {
		value += character
	}
	return "sha256:" + value
}
